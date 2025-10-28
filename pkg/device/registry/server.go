package registry

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/coldzerofear/vgpu-manager/pkg/api/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/util/cgroup"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
)

func NewDeviceRegistryServer(cache cache.Cache, containerPath string) *DeviceRegistryServerImpl {
	runtime.Must(cache.IndexField(context.Background(), &corev1.Pod{},
		"metadata.uid", func(obj client.Object) []string {
			return []string{string(obj.GetUID())}
		}))
	return &DeviceRegistryServerImpl{
		contPath: containerPath,
		cache:    cache,
	}
}

type DeviceRegistryServerImpl struct {
	registry.UnimplementedVDeviceRegistryServer
	mutex    sync.Mutex
	contPath string
	cache    cache.Cache
	server   *grpc.Server
	listener net.Listener
	running  bool
}

func (s *DeviceRegistryServerImpl) IsRunning() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.running
}

func (s *DeviceRegistryServerImpl) RegisterContainerDevice(ctx context.Context, req *registry.ContainerDeviceRequest) (resp *registry.ContainerDeviceResponse, err error) {
	klog.V(4).InfoS("RegisterContainerDevice", "podUid", req.GetPodUid(), "containerName", req.GetContainerName())
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			klog.ErrorS(fmt.Errorf("Unexpected panic in handler: %v\n%s", r, stack), "RegisterContainerDevice panicked",
				"podUid", req.GetPodUid(), "containerName", req.GetContainerName())
			err = fmt.Errorf("internal exception: %v", r)
		}
		if err != nil {
			klog.V(4).ErrorS(err, "RegisterContainerDevice failed", "podUid", req.GetPodUid(), "containerName", req.GetContainerName())
		}
	}()

	resp = &registry.ContainerDeviceResponse{}
	if len(req.PodUid) == 0 {
		err = fmt.Errorf("pod uid cannot be empty")
		return resp, err
	}
	if len(req.ContainerName) == 0 {
		err = fmt.Errorf("container name cannot be empty")
		return resp, err
	}
	podList := corev1.PodList{}
	if err = s.cache.List(ctx, &podList,
		client.MatchingFields{"metadata.uid": req.PodUid},
		client.UnsafeDisableDeepCopyOption(true)); err != nil {
		return resp, err
	}
	if len(podList.Items) != 1 {
		err = fmt.Errorf("unable to find pod %s", req.PodUid)
		return resp, err
	}
	pod := &podList.Items[0]
	if util.PodIsTerminated(pod) {
		err = fmt.Errorf("terminated pod %s", klog.KObj(pod).String())
		return resp, err
	}
	contIndex := slices.IndexFunc(pod.Spec.Containers, func(container corev1.Container) bool {
		return container.Name == req.ContainerName
	})
	if contIndex < 0 {
		err = fmt.Errorf("unable to find container %s in pod", req.ContainerName)
		return resp, err
	}
	if !util.IsVGPURequiredContainer(&pod.Spec.Containers[contIndex]) {
		err = fmt.Errorf("container %s does not have vGPU", req.ContainerName)
		return resp, err
	}

	var getFullPath func(string) string
	switch {
	case cgroups.IsCgroup2UnifiedMode(): // cgroupv2
		getFullPath = cgroup.GetK8sPodCGroupFullPath
	case cgroups.IsCgroup2HybridMode():
		// If the device controller does not exist, use the path of cgroupv2.
		getFullPath = cgroup.GetK8sPodDeviceCGroupFullPath
		if util.PathIsNotExist(cgroup.CGroupDevicePath) {
			getFullPath = cgroup.GetK8sPodCGroupFullPath
		}
	default: // cgroupv1
		getFullPath = cgroup.GetK8sPodDeviceCGroupFullPath
	}
	contPids := cgroup.GetContainerPidsFunc(pod, req.ContainerName, getFullPath)
	if len(contPids) == 0 {
		err = fmt.Errorf("unable to find the process ID of the container %s", req.ContainerName)
		return resp, err
	}

	sort.Ints(contPids)
	var buf bytes.Buffer
	for _, pid := range contPids {
		buf.WriteString(strconv.Itoa(pid))
		buf.WriteByte('\n')
	}
	containerFilePath := util.GetPodContainerManagerPath(s.contPath, pod.UID, req.ContainerName)
	cFileName := C.CString(filepath.Join(containerFilePath, util.Config, PidsConfig))
	cData := C.CString(buf.String())
	defer func() {
		C.free(unsafe.Pointer(cFileName))
		C.free(unsafe.Pointer(cData))
	}()
	if C.write_to_disk(cFileName, cData) != 0 {
		return resp, fmt.Errorf("can't sink pids file")
	}
	return resp, nil
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
	s.server = grpc.NewServer(grpc.MaxConcurrentStreams(1000))

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
