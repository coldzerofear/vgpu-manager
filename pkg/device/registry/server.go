package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/coldzerofear/vgpu-manager/pkg/api/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/util/cgroup"
	"github.com/opencontainers/cgroups"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

//#include <stdio.h>
//#include <stdlib.h>
//#include <string.h>
//#include <unistd.h>
//#include <fcntl.h>
//#include <sys/file.h>
//#include <time.h>
//
//int write_to_disk(const char* filename, const char* data) {
//  int fd = 0;
//  int wsize = 0;
//  struct timespec wait = {
//	  .tv_sec = 0, .tv_nsec = 100 * 1000 * 1000,
//  };
//  int ret = 0;
//  size_t data_len = 0;
//  data_len = strlen(data);
//  fd = open(filename, O_CREAT | O_TRUNC | O_WRONLY, 00777);
//  if (fd == -1) {
//    return 1;
//  }
//  while (flock(fd, LOCK_EX)) {
//    nanosleep(&wait, NULL);
//  }
//  wsize = (int)write(fd, (void*)data, data_len);
//  if (wsize != (int)data_len) {
//	  ret = 2;
//    goto DONE;
//  }
//DONE:
//  flock(fd, LOCK_UN);
//  close(fd);
//  return ret;
//}
import "C"

const (
	SocketFile = "socket.sock"
	PidsConfig = "pids.config"

	// resolveBackoff is the per-attempt poll interval used while waiting for
	// the lookup state (informer cache, container status) to become
	// consistent enough to satisfy a register request.
	resolveBackoff = 40 * time.Millisecond
)

// Target describes the container that a register request resolves to and the
// directory the server should drop pids.config into.
type Target struct {
	Pod           *corev1.Pod
	ContainerName string

	// ConfigDir is the absolute path of the directory that will hold
	// pids.config. The directory is created on demand if it doesn't exist.
	ConfigDir string
}

// GetPodByUIDFunc resolves a pod by its UID. Used by the legacy device-plugin
// path where the calling library already knows its own (pod, container) pair.
type GetPodByUIDFunc func(ctx context.Context, uid string) (*corev1.Pod, error)

// GetTargetByUUIDFunc resolves a register UUID minted by the DRA driver into a
// concrete Target. It encapsulates the partition-aware lookup that maps
// uuid → claim → partition → currently-running container.
type GetTargetByUUIDFunc func(ctx context.Context, uuid string) (*Target, error)

func NewDeviceRegistryServer(containerPath string, getPodByUIDFn GetPodByUIDFunc, getTargetByUUIDFn GetTargetByUUIDFunc) *DeviceRegistryServerImpl {
	return &DeviceRegistryServerImpl{
		contPath:          containerPath,
		getPodByUIDFn:     getPodByUIDFn,
		getTargetByUUIDFn: getTargetByUUIDFn,
	}
}

type DeviceRegistryServerImpl struct {
	registry.UnimplementedVDeviceRegistryServer
	mutex             sync.Mutex
	contPath          string
	getPodByUIDFn     GetPodByUIDFunc
	getTargetByUUIDFn GetTargetByUUIDFunc
	server            *grpc.Server
	listener          net.Listener
	running           bool
}

func (s *DeviceRegistryServerImpl) IsRunning() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.running
}

// RegisterContainerDevice is the gRPC entry point invoked by the in-container
// vGPU library. It supports two request shapes:
//
//   - register_uuid set: DRA path. The server hands the UUID to the configured
//     uuid resolver, which walks the partition graph to find the running
//     container and the on-disk partition directory.
//   - pod_uid + container_name set: legacy device-plugin path. The library
//     tells us its identity directly; we look up the pod and write to the
//     classic per-(pod, container) manager directory.
//
// In either case the call retries transient failures (informer not yet synced,
// container not yet Running) until the request context is canceled.
func (s *DeviceRegistryServerImpl) RegisterContainerDevice(ctx context.Context, req *registry.ContainerDeviceRequest) (resp *registry.ContainerDeviceResponse, err error) {
	klog.V(4).InfoS("RegisterContainerDevice",
		"podUid", req.GetPodUid(),
		"containerName", req.GetContainerName(),
		"registerUuid", req.GetRegisterUuid())
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			klog.ErrorS(fmt.Errorf("unexpected panic in handler: %v\n%s", r, stack),
				"RegisterContainerDevice panicked",
				"podUid", req.GetPodUid(),
				"containerName", req.GetContainerName(),
				"registerUuid", req.GetRegisterUuid())
			err = fmt.Errorf("internal exception: %v", r)
		}
		if err != nil {
			klog.V(4).ErrorS(err, "RegisterContainerDevice failed",
				"podUid", req.GetPodUid(),
				"containerName", req.GetContainerName(),
				"registerUuid", req.GetRegisterUuid())
		}
	}()

	resp = &registry.ContainerDeviceResponse{}

	target, pids, err := s.resolveTarget(ctx, req)
	if err != nil {
		return resp, err
	}
	if err = s.persistPids(target.ConfigDir, pids); err != nil {
		return resp, err
	}
	return resp, nil
}

// resolveTarget loops until either (a) the request resolves to a running
// container whose cgroup yields a non-empty PID set, (b) a hard error
// (terminated pod, vGPU policy violation) surfaces, or (c) the context is
// canceled.
//
// Soft failures — informer hasn't caught up, container not yet Running, cgroup
// not yet populated, or a stale informer view that points at an already-exited
// container — all keep the poll going. The cgroup PID readback is the
// authoritative cross-check: the kernel cannot lie about whether a container
// has live processes, so an "informer says Running but cgroup is empty" state
// is treated as a transient stale view and we keep polling until a fresher
// snapshot lands.
func (s *DeviceRegistryServerImpl) resolveTarget(ctx context.Context, req *registry.ContainerDeviceRequest) (*Target, []int, error) {
	useUUID := req.GetRegisterUuid() != ""

	var (
		target  *Target
		pids    []int
		lastErr error
	)
	pollErr := wait.PollUntilContextCancel(ctx, resolveBackoff, true, func(ctx context.Context) (bool, error) {
		t, err := s.lookupTarget(ctx, req)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, err
			}
			lastErr = err
			klog.V(5).InfoS("register target lookup failed; retrying", "err", err)
			return false, nil
		}

		if util.PodIsTerminated(t.Pod) {
			return false, fmt.Errorf("pod %s is terminated", klog.KObj(t.Pod))
		}

		// Legacy device-plugin path applies the spec-level vGPU policy check.
		// DRA path's resolver has already vetted that the container belongs
		// to a vGPU partition.
		if !useUUID {
			if err := validateLegacyContainer(t.Pod, t.ContainerName); err != nil {
				return false, err
			}
		}

		status, ok := cgroup.GetContainerStatus(t.Pod, t.ContainerName)
		if !ok || status.State.Running == nil {
			lastErr = fmt.Errorf("container %q not yet running", t.ContainerName)
			klog.V(4).InfoS("waiting for container to enter Running state",
				"pod", klog.KObj(t.Pod),
				"containerName", t.ContainerName,
				"resourceVersion", t.Pod.ResourceVersion)
			return false, nil
		}

		// Cgroup cross-check. If the informer view is stale (e.g. an init
		// container was just replaced by an app container in the same
		// partition), the cgroup for the container the informer still calls
		// "Running" will be empty or gone, and we keep polling until the
		// informer catches up and the resolver picks the real running
		// container.
		candidatePids := cgroup.GetContainerPidsFunc(t.Pod, t.ContainerName, cgroupFullPathResolver())
		if len(candidatePids) == 0 {
			lastErr = fmt.Errorf("container %q reports Running but its cgroup has no live PIDs (likely stale informer)", t.ContainerName)
			klog.V(4).InfoS("cgroup cross-check failed; retrying",
				"pod", klog.KObj(t.Pod),
				"containerName", t.ContainerName,
				"resourceVersion", t.Pod.ResourceVersion)
			return false, nil
		}

		target = t
		pids = candidatePids
		return true, nil
	})

	if pollErr != nil {
		if errors.Is(pollErr, context.Canceled) || errors.Is(pollErr, context.DeadlineExceeded) {
			if lastErr != nil {
				return nil, nil, fmt.Errorf("%w: last attempt: %v", pollErr, lastErr)
			}
		}
		return nil, nil, pollErr
	}
	return target, pids, nil
}

// cgroupFullPathResolver picks the right cgroup hierarchy walker based on the
// host's cgroup driver / unified-vs-hybrid mode.
func cgroupFullPathResolver() func(string) string {
	switch {
	case cgroups.IsCgroup2UnifiedMode():
		return cgroup.GetK8sPodCGroupFullPath
	case cgroups.IsCgroup2HybridMode():
		if util.PathIsNotExist(cgroup.CGroupDevicePath) {
			return cgroup.GetK8sPodCGroupFullPath
		}
		return cgroup.GetK8sPodDeviceCGroupFullPath
	default:
		return cgroup.GetK8sPodDeviceCGroupFullPath
	}
}

// lookupTarget performs a single (non-retrying) resolution attempt. It picks
// the right resolver based on the request shape and synthesizes a Target.
func (s *DeviceRegistryServerImpl) lookupTarget(ctx context.Context, req *registry.ContainerDeviceRequest) (*Target, error) {
	if uuid := req.GetRegisterUuid(); uuid != "" {
		if s.getTargetByUUIDFn == nil {
			return nil, errors.New("uuid registration is not supported by this server")
		}
		t, err := s.getTargetByUUIDFn(ctx, uuid)
		if err != nil {
			return nil, err
		}
		if t == nil || t.Pod == nil {
			return nil, fmt.Errorf("uuid %s did not resolve to any pod", uuid)
		}
		if t.ContainerName == "" {
			return nil, fmt.Errorf("uuid %s resolver returned empty containerName", uuid)
		}
		if t.ConfigDir == "" {
			return nil, fmt.Errorf("uuid %s resolver returned empty ConfigDir", uuid)
		}
		return t, nil
	}

	if s.getPodByUIDFn == nil {
		return nil, errors.New("pod-uid registration is not supported by this server")
	}
	podUID := req.GetPodUid()
	containerName := req.GetContainerName()
	if podUID == "" {
		return nil, errors.New("pod_uid is empty")
	}
	if containerName == "" {
		return nil, errors.New("container_name is empty")
	}
	pod, err := s.getPodByUIDFn(ctx, podUID)
	if err != nil {
		return nil, err
	}
	if pod == nil {
		return nil, fmt.Errorf("pod %s not found", podUID)
	}
	return &Target{
		Pod:           pod,
		ContainerName: containerName,
		ConfigDir: filepath.Join(
			util.GetPodContainerManagerPath(s.contPath, pod.UID, containerName),
			util.Config,
		),
	}, nil
}

// validateLegacyContainer enforces the policy used by the device-plugin path:
// the container must exist in pod.Spec.Containers (init containers are not
// supported under device-plugin) and must request vGPU resources.
func validateLegacyContainer(pod *corev1.Pod, containerName string) error {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name != containerName {
			continue
		}
		if !util.IsVGPURequiredContainer(&pod.Spec.Containers[i]) {
			return fmt.Errorf("container %s does not have vGPU", containerName)
		}
		return nil
	}
	return fmt.Errorf("unable to find container %s in pod %s", containerName, klog.KObj(pod))
}

// persistPids writes the sorted PID list to <configDir>/pids.config atomically
// (flock + truncate+write) via the in-tree cgo helper.
func (s *DeviceRegistryServerImpl) persistPids(configDir string, pids []int) error {
	_ = os.MkdirAll(configDir, 0o777)
	var buf bytes.Buffer
	sort.Ints(pids)
	for _, pid := range pids {
		buf.WriteString(strconv.Itoa(pid))
		buf.WriteByte('\n')
	}

	cFileName := C.CString(filepath.Join(configDir, PidsConfig))
	cData := C.CString(buf.String())
	defer func() {
		C.free(unsafe.Pointer(cFileName))
		C.free(unsafe.Pointer(cData))
	}()
	if C.write_to_disk(cFileName, cData) != 0 {
		return fmt.Errorf("failed to write pids file at %s", filepath.Join(configDir, PidsConfig))
	}
	return nil
}

func (s *DeviceRegistryServerImpl) Start() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.running {
		return fmt.Errorf("DeviceRegistry server is already running")
	}
	registryPath := filepath.Join(s.contPath, util.Registry)
	_ = os.MkdirAll(registryPath, 0777)
	_ = os.Chmod(registryPath, 0777)
	socketFile := filepath.Join(registryPath, SocketFile)
	if err := syscall.Unlink(socketFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %v", err)
	}

	addr, err := net.ResolveUnixAddr("unix", socketFile)
	if err != nil {
		return fmt.Errorf("failed to resolve unix addr: %v", err)
	}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("failed to listen unix: %v", err)
	}

	if err = os.Chmod(socketFile, 0777); err != nil {
		_ = listener.Close()
		return fmt.Errorf("failed to set socket permissions: %v", err)
	}
	s.listener = listener
	s.server = grpc.NewServer(grpc.MaxConcurrentStreams(1024))

	registry.RegisterVDeviceRegistryServer(s.server, s)
	s.running = true

	go func() {
		if err = s.server.Serve(listener); err != nil && err != grpc.ErrServerStopped {
			klog.Errorf("DeviceRegistry gRPC server serve error: %v", err)
		}
		s.mutex.Lock()
		s.running = false
		s.mutex.Unlock()
	}()

	klog.V(3).Info("DeviceRegistry gRPC server started successfully")
	return nil
}

func (s *DeviceRegistryServerImpl) Stop() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.server != nil {
		stopped := make(chan struct{})
		go func() {
			s.server.GracefulStop()
			close(stopped)
		}()

		select {
		case <-stopped:
			klog.Info("DeviceRegistry gRPC server stopped gracefully")
		case <-time.After(10 * time.Second):
			klog.Warning("Force stopping gRPC server after timeout")
			s.server.Stop()
		}
		s.server = nil
	}

	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}

	s.running = false
}
