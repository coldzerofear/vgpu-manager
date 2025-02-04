package deviceplugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type NumberDevicePlugin struct {
	manager      *manager.DeviceManager
	resourceName string
	socket       string
	kubeClient   *kubernetes.Clientset
	podResource  *client.PodResource
	cache        cache.Cache

	server *grpc.Server
	health chan *pluginapi.Device
	stop   chan struct{}
}

var _ DevicePlugin = &NumberDevicePlugin{}

// NewNumberDevicePlugin returns an initialized NumberDevicePlugin
func NewNumberDevicePlugin(resourceName string, manager *manager.DeviceManager,
	socket string, kubeClient *kubernetes.Clientset, cache cache.Cache) DevicePlugin {
	return &NumberDevicePlugin{
		manager:      manager,
		resourceName: resourceName,
		socket:       socket,
		kubeClient:   kubeClient,
		podResource:  client.NewPodResource(),
		cache:        cache,

		// These will be reinitialized every
		// time the plugin server is restarted.
		server: nil,
		health: nil,
		stop:   nil,
	}
}

func (m *NumberDevicePlugin) Name() string {
	return "number-plugin"
}

func (m *NumberDevicePlugin) initialize() {
	m.server = grpc.NewServer([]grpc.ServerOption{}...)
	m.health = make(chan *pluginapi.Device)
	m.stop = make(chan struct{})
}

func (m *NumberDevicePlugin) cleanup() {
	close(m.stop)
	m.server = nil
	m.health = nil
	m.stop = nil
}

// Start starts the gRPC server, registers the device plugin with the Kubelet,
// and starts the device healthchecks.
func (m *NumberDevicePlugin) Start() error {
	m.initialize()

	if err := m.serve(); err != nil {
		klog.Infof("Could not start device plugin for '%s': %s", m.resourceName, err)
		m.cleanup()
		return err
	}

	klog.Infof("Starting to serve '%s' on %s", m.resourceName, m.socket)

	if err := m.register(); err != nil {
		klog.Infof("Could not register device plugin: %v", err)
		_ = m.Stop()
		return err
	}

	klog.Infof("Registered device plugin for '%s' with Kubelet", m.resourceName)

	m.manager.AddNotifyChannel(m.Name(), m.health)

	return nil
}

// Stop stops the gRPC server.
func (m *NumberDevicePlugin) Stop() error {
	if m == nil || m.server == nil {
		return nil
	}
	klog.Infof("Stopping to serve '%s' on %s", m.resourceName, m.socket)

	m.manager.RemoveNotifyChannel(m.Name())

	m.server.Stop()
	err := os.Remove(m.socket)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	m.cleanup()
	return nil
}

// serve starts the gRPC server of the device plugin.
func (m *NumberDevicePlugin) serve() error {
	_ = os.Remove(m.socket)
	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return err
	}

	pluginapi.RegisterDevicePluginServer(m.server, m)

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			klog.Infof("Starting GRPC server for '%s'", m.resourceName)
			if err = m.server.Serve(sock); err == nil {
				break
			}
			klog.Errorf("GRPC server for '%s' crashed with error: %v", m.resourceName, err)

			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				klog.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", m.resourceName)
			}

			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				// it has been one hour since the last crash.. reset the count
				// to reflect on the frequency
				restartCount = 1
			} else {
				restartCount++
			}
		}
	}()

	// Wait for server to start by launching a blocking connexion
	conn, err := dial(m.socket, 5*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()

	return nil
}

// register the device plugin for the given resourceName with Kubelet.
func (m *NumberDevicePlugin) register() error {
	conn, err := dial(pluginapi.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	cli := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: m.resourceName,
		Options:      &pluginapi.DevicePluginOptions{},
	}

	_, err = cli.Register(context.Background(), reqt)
	return err
}

// GetDevicePluginOptions returns options to be communicated with Device Manager.
func (m *NumberDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired: true,
	}, nil
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears,
// ListAndWatch returns the new list.
func (m *NumberDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
		klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
	}
	stopCh := m.stop
	for {
		select {
		case d := <-m.health:
			klog.Infof("'%s' device marked unhealthy: %s", m.resourceName, d.ID)
			if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
				klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
			}
		case <-stopCh:
			return nil
		}
	}
}

// GetPreferredAllocation returns the preferred allocation from the set of devices specified in the request.
func (m *NumberDevicePlugin) GetPreferredAllocation(context.Context, *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

const (
	HostProcPath             = "/proc"
	ManagerDirectoryPath     = "/etc/vgpu-manager"
	ContainerConfigPath      = ManagerDirectoryPath + "/config"
	ContainerProcPath        = ManagerDirectoryPath + "/host_proc"
	ContainerCgroupPath      = ManagerDirectoryPath + "/host_cgroup"
	LdPreLoadFileName        = "ld.so.preload"
	ContainerPreloadPath     = "/etc/" + LdPreLoadFileName
	HostPreloadPath          = ManagerDirectoryPath + "/" + LdPreLoadFileName
	VGPUControlFileName      = "libvgpu-control.so"
	ContainerVGPUControlPath = ManagerDirectoryPath + "/driver/" + VGPUControlFileName
	HostVGPUControlPath      = ManagerDirectoryPath + "/" + VGPUControlFileName
	VGPUConfigFileName       = "vgpu.config"

	NvidiaDeviceFilePrefix = "/dev/nvidia"
	NvidiaCTLFilePath      = "/dev/nvidiactl"
	NvidiaUVMFilePath      = "/dev/nvidia-uvm"
	NvidiaUVMToolsFilePath = "/dev/nvidia-uvm-tools"
)

func GetHostManagerDirectoryPath(podUID types.UID, containerName string) string {
	return fmt.Sprintf("%s/%s_%s", ManagerDirectoryPath, string(podUID), containerName)
}

func GetDeviceMinorMap(gpus []manager.GPUDevice) map[string]int {
	minorMap := make(map[string]int)
	for _, gpuDevice := range gpus {
		minorMap[gpuDevice.Uuid] = gpuDevice.MinorNumber
	}
	return minorMap
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container.
func (m *NumberDevicePlugin) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (resp *pluginapi.AllocateResponse, err error) {
	klog.V(4).Infoln("Allocate", req.GetContainerRequests())
	resp = &pluginapi.AllocateResponse{}
	var (
		activePods    []corev1.Pod
		currentPod    *corev1.Pod
		assignDevs    *device.ContainerDevices
		podCgroupPath string
	)
	// When an error occurs, return a fixed format error message
	// and patch the failed metadata allocation.
	defer func() {
		if err == nil {
			return
		}
		err = fmt.Errorf("%s: %s", util.AllocateCheckErrMsg, err.Error())
		klog.Errorln(err.Error())
		if currentPod == nil {
			return
		}
		patchErr := client.PatchPodAllocationFailed(m.kubeClient, currentPod)
		if patchErr != nil {
			klog.Warningf("PatchPodAllocationFailed error: %v", patchErr)
		}
	}()

	activePods, err = client.GetActivePodsOnNode(ctx, m.kubeClient, m.manager.GetNodeConfig().NodeName())
	if err != nil {
		klog.Errorf("failed to retrieve the active pods of the current node: %v", err)
		return resp, err
	}
	allocatingPods := util.FilterAllocatingPods(activePods)
	currentPod, err = util.GetCurrentPodByAllocatingPods(allocatingPods)
	if err != nil {
		klog.Errorln(err.Error())
		return resp, err
	}

	responses := make([]*pluginapi.ContainerAllocateResponse, len(req.ContainerRequests))
	devMinorMap := GetDeviceMinorMap(m.manager.GetDevices())
	for i, containerRequest := range req.ContainerRequests {
		number := len(containerRequest.GetDevicesIDs())
		assignDevs, err = device.GetCurrentPreAllocateContainerDevice(currentPod)
		if err != nil {
			klog.Errorln(err.Error())
			return resp, err
		}
		if number != len(assignDevs.Devices) {
			err = fmt.Errorf("requested number of devices does not match")
			klog.Errorln(err.Error())
			return resp, err
		}
		var (
			deviceIds []string
			envMap    = make(map[string]string)
			mounts    []*pluginapi.Mount
			devices   []*pluginapi.DeviceSpec
		)
		envMap[util.PodNameEnv] = currentPod.Name
		envMap[util.PodNamespaceEnv] = currentPod.Namespace
		envMap[util.PodUIDEnv] = string(currentPod.UID)
		envMap[util.ContNameEnv] = assignDevs.Name
		sort.Slice(assignDevs.Devices, func(i, j int) bool {
			return assignDevs.Devices[i].Id < assignDevs.Devices[j].Id
		})
		for idx, dev := range assignDevs.Devices {
			memoryLimitEnv := fmt.Sprintf("%s_%d", util.CudaMemoryLimitEnv, idx)
			envMap[memoryLimitEnv] = fmt.Sprintf("%dm", dev.Memory)
			if dev.Core > 0 && dev.Core < util.HundredCore {
				coreLimitEnv := fmt.Sprintf("%s_%d", util.CudaCoreLimitEnv, idx)
				envMap[coreLimitEnv] = strconv.Itoa(dev.Core)
			}
			deviceIds = append(deviceIds, dev.Uuid)
			nvidiaDeviceFile := fmt.Sprintf("%s%d",
				NvidiaDeviceFilePrefix, devMinorMap[dev.Uuid])
			devices = append(devices, &pluginapi.DeviceSpec{
				ContainerPath: nvidiaDeviceFile,
				HostPath:      nvidiaDeviceFile,
				Permissions:   "rw",
			})
		}
		deviceIdStr := strings.Join(deviceIds, ",")
		envMap[util.GPUDeviceUuidEnv] = deviceIdStr
		envMap[util.NvidiaVisibleDevicesEnv] = deviceIdStr
		devices = append(devices, &pluginapi.DeviceSpec{
			ContainerPath: NvidiaCTLFilePath,
			HostPath:      NvidiaCTLFilePath,
			Permissions:   "rw",
		}, &pluginapi.DeviceSpec{
			ContainerPath: NvidiaUVMFilePath,
			HostPath:      NvidiaUVMFilePath,
			Permissions:   "rw",
		}, &pluginapi.DeviceSpec{
			ContainerPath: NvidiaUVMToolsFilePath,
			HostPath:      NvidiaUVMToolsFilePath,
			Permissions:   "rw",
		})
		mounts = append(mounts, &pluginapi.Mount{ // mount /proc dir
			ContainerPath: ContainerProcPath,
			HostPath:      HostProcPath,
			ReadOnly:      true,
		}, &pluginapi.Mount{ // mount ld_preload file
			ContainerPath: ContainerPreloadPath,
			HostPath:      HostPreloadPath,
			ReadOnly:      true,
		}, &pluginapi.Mount{ // mount libvgpu-control.so file
			ContainerPath: ContainerVGPUControlPath,
			HostPath:      HostVGPUControlPath,
			ReadOnly:      true,
		})
		// The cgroup path that requires additional pod mounting in the cgroupv2 container environment.
		if cgroups.IsCgroup2UnifiedMode() {
			podCgroupPath, err = util.GetK8sPodCGroupPath(currentPod)
			if err != nil {
				klog.Errorln(err.Error())
				return resp, err
			}
			cgroupFullPath := util.GetK8sPodCGroupFullPath(podCgroupPath)
			baseCgroupPath := util.SplitK8sCGroupBasePath(cgroupFullPath)
			if util.PathIsNotExist(baseCgroupPath) {
				err = fmt.Errorf("unable to find k8s cgroup path")
				klog.Errorln(err.Error())
				return resp, err
			}
			mounts = append(mounts, &pluginapi.Mount{
				ContainerPath: ContainerCgroupPath,
				HostPath:      cgroupFullPath,
				ReadOnly:      true,
			})
		}
		// /etc/vgpu-manager/<pod-uid>_<cont-name>
		hostManagerDirectory := GetHostManagerDirectoryPath(currentPod.UID, assignDevs.Name)
		_ = os.MkdirAll(hostManagerDirectory, 0777)
		_ = os.Chmod(hostManagerDirectory, 0777)
		mounts = append(mounts, &pluginapi.Mount{ // mount vgpu.config file
			ContainerPath: ContainerConfigPath,
			HostPath:      hostManagerDirectory,
			ReadOnly:      true,
		})
		podDevices := device.PodDevices{}
		if realAlloc, ok := util.HasAnnotation(currentPod, util.PodVGPURealAllocAnnotation); ok {
			_ = podDevices.UnmarshalText(realAlloc)
		}
		podDevices = append(podDevices, *assignDevs)
		var realAllocated string
		if realAllocated, err = podDevices.MarshalText(); err != nil {
			err = fmt.Errorf("real allocated of encoding device failed: %v", err)
			klog.Errorln(err.Error())
			return resp, err
		}
		currentPod.Annotations[util.PodVGPURealAllocAnnotation] = realAllocated
		responses[i] = &pluginapi.ContainerAllocateResponse{
			Envs:    envMap,
			Mounts:  mounts,
			Devices: devices,
		}
	}
	resp.ContainerResponses = responses
	patchErr := client.PatchPodAllocationSucceed(m.kubeClient, currentPod)
	if patchErr != nil {
		klog.Warningf("PatchPodAllocationSucceed error: %v", patchErr)
	}
	return resp, nil
}

// GetActiveVGPUPodsOnNode Get the vgpu pods on the node
func (m *NumberDevicePlugin) GetActiveVGPUPodsOnNode() map[string]*corev1.Pod {
	podList := corev1.PodList{}
	err := m.cache.List(context.Background(), &podList)
	if err != nil {
		klog.ErrorS(err, "GetActiveVGPUPodsOnNode failed")
	}
	activePods := make(map[string]*corev1.Pod)
	nodeName := m.manager.GetNodeConfig().NodeName()
	for i, pod := range podList.Items {
		if pod.Spec.NodeName != nodeName {
			continue
		}
		if !util.IsVGPUResourcePod(&pod) || util.PodIsTerminated(&pod) {
			continue
		}
		activePods[string(pod.UID)] = &podList.Items[i]
	}
	return activePods
}

// getCurrentPodInfoByCheckpoint find relevant pod information for devicesIDs in kubelet checkpoint
func (m *NumberDevicePlugin) getCurrentPodInfoByCheckpoint(devicesIDs []string) (*client.PodInfo, error) {
	klog.V(3).Infoln("Try get DevicePlugin checkpoint data")
	pluginPath := m.manager.GetNodeConfig().DevicePluginPath()
	cp, err := GetCheckpointData(pluginPath)
	if err != nil {
		return nil, err
	}
	devSet := sets.NewString(devicesIDs...)
	for _, entry := range cp.PodDeviceEntries {
		if entry.ResourceName != util.VGPUNumberResourceName {
			continue
		}
		if !devSet.HasAll(entry.DeviceIDs...) {
			continue
		}
		if pod, ok := m.GetActiveVGPUPodsOnNode()[entry.PodUID]; ok {
			return &client.PodInfo{
				PodName:       pod.Name,
				PodNamespace:  pod.Namespace,
				ContainerName: entry.ContainerName,
			}, nil
		}
		break
	}
	return nil, fmt.Errorf("pod not found")
}

func (m *NumberDevicePlugin) getCurrentPodInfo(devicesIDs []string) (*client.PodInfo, error) {
	podResources, err := m.podResource.ListPodResource()
	if err != nil {
		klog.Errorf(err.Error())
		return m.getCurrentPodInfoByCheckpoint(devicesIDs)
	}
	podInfo, err := m.podResource.GetPodInfoByResourceDeviceIDs(
		podResources, util.VGPUNumberResourceName, devicesIDs)
	if err != nil {
		klog.Errorf(err.Error())
		return m.getCurrentPodInfoByCheckpoint(devicesIDs)
	}
	return podInfo, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as resetting the device before making devices available to the container.
func (m *NumberDevicePlugin) PreStartContainer(ctx context.Context, req *pluginapi.PreStartContainerRequest) (resp *pluginapi.PreStartContainerResponse, err error) {
	klog.V(4).Infoln("PreStartContainer", req.GetDevicesIDs())
	resp = &pluginapi.PreStartContainerResponse{}
	defer func() {
		if err != nil {
			err = fmt.Errorf("%s: %s", util.PreStartContainerCheckErrMsg, err.Error())
			klog.Errorln(err.Error())
		}
	}()

	var (
		node     *corev1.Node
		pod      *corev1.Pod
		nodeName = m.manager.GetNodeConfig().NodeName()
	)
	err = retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		// Node does not require timeliness, search from API server cache.
		options := metav1.GetOptions{ResourceVersion: "0"}
		node, err = m.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, options)
		return err
	})
	if err != nil {
		klog.Errorf("failed to get current node <%s>: %v", nodeName, err)
		return resp, err
	}
	podInfo, err := m.getCurrentPodInfo(req.GetDevicesIDs())
	if err != nil {
		klog.Errorln(err.Error())
		return resp, err
	}

	err = retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		// Pod ensures timeliness, query from etcd.
		pod, err = m.kubeClient.CoreV1().Pods(podInfo.PodNamespace).Get(ctx, podInfo.PodName, metav1.GetOptions{})
		return err
	})
	if err != nil {
		klog.Errorf("failed to get current pod <%s/%s>: %v", podInfo.PodNamespace, podInfo.PodName, err)
		return resp, err
	}
	managerDirectory := GetHostManagerDirectoryPath(pod.UID, podInfo.ContainerName)
	vgpuConfigPath := filepath.Join(managerDirectory, VGPUConfigFileName)
	klog.V(4).Infof("Pod <%s/%s> container <%s> vgpu config path is <%s>",
		pod.Namespace, pod.Name, podInfo.ContainerName, vgpuConfigPath)
	realAlloc, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	realDevices := device.PodDevices{}
	if err = realDevices.UnmarshalText(realAlloc); err != nil {
		err = fmt.Errorf("parse pod assign devices failed: %v", err)
		klog.Errorln(err.Error())
		return resp, err
	}
	index := slices.IndexFunc(realDevices, func(contDevs device.ContainerDevices) bool {
		return contDevs.Name == podInfo.ContainerName
	})
	if index < 0 {
		err = fmt.Errorf("unable to find allocated devices for container <%s>", podInfo.ContainerName)
		klog.Errorln(err.Error())
		return resp, err
	}
	oversold := slices.ContainsFunc(pod.Spec.Containers, func(container corev1.Container) bool {
		return container.Name == podInfo.ContainerName && slices.ContainsFunc(container.Env, func(env corev1.EnvVar) bool {
			return env.Name == util.CudaMemoryOversoldEnv && strings.ToUpper(env.Value) == "TRUE"
		})
	})
	err = vgpu.WriteVGPUConfigFile(vgpuConfigPath, m.manager, pod, realDevices[index], oversold, node)
	if err != nil {
		klog.Errorln(err.Error())
	}
	return resp, err
}

func (m *NumberDevicePlugin) Devices() []*pluginapi.Device {
	var devices []*pluginapi.Device
	for _, gpuDevice := range m.manager.GetDevices() {
		if gpuDevice.Mig { // skip MIG device
			continue
		}
		for i := 0; i < gpuDevice.Number; i++ {
			devId := fmt.Sprintf("%d:%s:%d", gpuDevice.Id, gpuDevice.Uuid, i)
			health := pluginapi.Healthy
			if !gpuDevice.Healthy {
				health = pluginapi.Unhealthy
			}
			devices = append(devices, &pluginapi.Device{
				ID:       devId,
				Health:   health,
				Topology: nil,
			})
		}
	}
	return devices
}
