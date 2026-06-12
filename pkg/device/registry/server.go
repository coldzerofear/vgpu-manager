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

// TargetCandidate is one (pod, container) pair the server may try to attribute
// a register request to. The DRA path can return multiple candidates per
// register UUID — the server iterates them and accepts the first one whose
// cgroup yields live PIDs.
type TargetCandidate struct {
	Pod           *corev1.Pod
	ContainerName string
}

// Target carries the resolution result for a register request. Candidates are
// tried in order. ConfigDir is the same for every candidate of the same
// request because Prepare creates one directory per partition annotation.
type Target struct {
	Candidates []TargetCandidate

	// ConfigDir is the absolute path of the directory that will hold
	// pids.config. The directory is created on demand if it doesn't exist.
	ConfigDir string
}

// GetPodByUIDFunc resolves a pod by its UID. Used by the legacy device-plugin
// path where the calling library already knows its own (pod, container) pair.
type GetPodByUIDFunc func(ctx context.Context, uid string) (*corev1.Pod, error)

// GetTargetByUUIDFunc resolves a register UUID minted by the DRA driver into
// the set of (pod, container) candidates that could be the caller plus the
// shared on-disk directory for pids.config. Returning multiple candidates is
// the contract that lets the server tolerate stale informer views and
// fallback partition keys baked into claim annotations at Prepare time.
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
//     uuid resolver, which returns every (pod, container) the UUID could
//     legitimately refer to plus the partition directory. The server then
//     tries each candidate and writes pids.config under the directory for the
//     first one whose cgroup is alive.
//   - pod_uid + container_name set: legacy device-plugin path. The library
//     tells us its identity directly; we look up the pod and write to the
//     classic per-(pod, container) manager directory.
//
// In either case the call retries transient failures (informer not yet synced,
// container not yet Running, cgroup still ramping up) until the request
// context is canceled.
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

	configDir, pids, err := s.resolveTarget(ctx, req)
	if err != nil {
		return resp, err
	}
	if err = s.persistPids(configDir, pids); err != nil {
		return resp, err
	}
	return resp, nil
}

// resolveTarget loops until either (a) some candidate resolves to a running
// container whose cgroup yields a non-empty PID set, (b) a hard error
// (terminated pod in legacy mode, vGPU policy violation, uuid not minted by
// us) surfaces, or (c) the context is canceled.
//
// The cgroup PID readback is the authoritative cross-check: the kernel cannot
// lie about whether a container has live processes, so an "informer says
// Running but cgroup is empty" state is treated as a transient stale view and
// we keep polling until a fresher snapshot lands.
//
// On the DRA path the resolver may return multiple candidates per UUID (to
// handle stale partition keys baked into annotations at Prepare time, and
// init+app pairs sharing a partition). The candidate list is tried in order;
// the first viable one wins. If none are viable we wait one backoff tick and
// re-query the resolver — by then either the informer has caught up or the
// runtime has progressed enough for a different candidate to be Running.
func (s *DeviceRegistryServerImpl) resolveTarget(ctx context.Context, req *registry.ContainerDeviceRequest) (string, []int, error) {
	useUUID := req.GetRegisterUuid() != ""
	cgroupResolver := cgroupFullPathResolver()

	var (
		configDir string
		winPids   []int
		lastErr   error
	)
	// TODO When malicious requests are constantly being sent within a container,
	// such as non-existent UUIDs or container names, the retry logic here may continuously
	// retry and consume resources during the timeout period. How to identify and respond quickly?
	pollErr := wait.PollUntilContextCancel(ctx, resolveBackoff, true, func(ctx context.Context) (bool, error) {
		target, err := s.lookupTarget(ctx, req)
		if err != nil {
			// "Not minted by us / never patched" is unrecoverable — bail out
			// instead of looping forever (also forecloses a cheap DoS vector
			// where junk UUIDs would otherwise pin a goroutine indefinitely).
			if apierrors.IsNotFound(err) {
				return false, err
			}
			lastErr = err
			klog.V(5).InfoS("register target lookup failed; retrying", "err", err)
			return false, nil
		}
		if len(target.Candidates) == 0 {
			lastErr = errors.New("resolver returned no candidates")
			return false, nil
		}

		// Legacy device-plugin path has exactly one candidate; validation
		// failures are hard errors (no other candidate could rescue them).
		// DRA path iterates candidates, accepting the first viable one and
		// continuing on per-candidate soft failures.
		for i, cand := range target.Candidates {
			if !useUUID {
				if util.PodIsTerminated(cand.Pod) {
					return false, fmt.Errorf("pod %s is terminated", klog.KObj(cand.Pod))
				}
			}
			pids, ok := isCandidateAlive(cand, cgroupResolver, &lastErr)
			if !ok {
				continue
			}
			configDir = target.ConfigDir
			winPids = pids
			klog.V(5).InfoS("register candidate accepted",
				"pod", klog.KObj(cand.Pod),
				"containerName", cand.ContainerName,
				"candidateIndex", i,
				"pids", len(pids))
			return true, nil
		}
		return false, nil
	})

	if pollErr != nil {
		if errors.Is(pollErr, context.Canceled) || errors.Is(pollErr, context.DeadlineExceeded) {
			if lastErr != nil {
				return "", nil, fmt.Errorf("%w: last attempt: %v", pollErr, lastErr)
			}
		}
		return "", nil, pollErr
	}
	return configDir, winPids, nil
}

// isCandidateAlive runs the per-candidate viability check. Returns (pids,
// true) when the candidate is in Running state and its cgroup has live
// processes; otherwise (nil, false) with *lastErr updated for the eventual
// context-cancel error message.
func isCandidateAlive(cand TargetCandidate, cgroupResolver func(string) string, lastErr *error) ([]int, bool) {
	if util.PodIsTerminated(cand.Pod) {
		return nil, false
	}
	status, ok := cgroup.GetContainerStatus(cand.Pod, cand.ContainerName)
	if !ok || status.State.Running == nil {
		*lastErr = fmt.Errorf("candidate container %q in pod %s not yet running",
			cand.ContainerName, klog.KObj(cand.Pod))
		return nil, false
	}
	pids := cgroup.GetContainerPidsFunc(cand.Pod, cand.ContainerName, cgroupResolver)
	if len(pids) == 0 {
		*lastErr = fmt.Errorf("candidate container %q in pod %s reports Running but its cgroup has no live PIDs (likely stale informer)",
			cand.ContainerName, klog.KObj(cand.Pod))
		return nil, false
	}
	return pids, true
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
// the right resolver based on the request shape and synthesizes a Target. For
// the legacy path the Target carries exactly one candidate.
func (s *DeviceRegistryServerImpl) lookupTarget(ctx context.Context, req *registry.ContainerDeviceRequest) (*Target, error) {
	if uuid := req.GetRegisterUuid(); uuid != "" {
		if s.getTargetByUUIDFn == nil {
			return nil, errors.New("uuid registration is not supported by this server")
		}
		t, err := s.getTargetByUUIDFn(ctx, uuid)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, fmt.Errorf("uuid %s did not resolve to a target", uuid)
		}
		if len(t.Candidates) == 0 {
			return nil, fmt.Errorf("uuid %s resolver returned no candidates", uuid)
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
		return nil, fmt.Errorf("pod %q not found", podUID)
	}
	return &Target{
		Candidates: []TargetCandidate{{
			Pod:           pod,
			ContainerName: containerName,
		}},
		ConfigDir: filepath.Join(
			util.GetPodContainerManagerPath(s.contPath, pod.UID, containerName),
			util.Config,
		),
	}, nil
}

// persistPids writes the sorted PID list to <configDir>/pids.config atomically
// (flock + truncate+write) via the in-tree cgo helper.
func (s *DeviceRegistryServerImpl) persistPids(configDir string, pids []int) error {
	_ = util.EnsureDir(configDir, 0o777)
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
	// TODO How to prevent DDoS attacks from interrupting our services through a series of
	// effective flow limiting strategies when a large number of malicious requests suddenly appear
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

	// Immediately close the listener and reject new connections
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}

	// Elegantly stop existing requests
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

	s.running = false
}
