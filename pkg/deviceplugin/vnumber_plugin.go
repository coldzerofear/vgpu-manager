package deviceplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"k8s.io/client-go/util/retry"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type vnumberDevicePlugin struct {
	base        *baseDevicePlugin
	kubeClient  *kubernetes.Clientset
	podResource *client.PodResource
	cache       cache.Cache
}

var _ DevicePlugin = &vnumberDevicePlugin{}

// NewVNumberDevicePlugin returns an initialized vnumberDevicePlugin
func NewVNumberDevicePlugin(resourceName, socket string, manager *manager.DeviceManager,
	kubeClient *kubernetes.Clientset, cache cache.Cache) DevicePlugin {

	return &vnumberDevicePlugin{
		base:        newBaseDevicePlugin(resourceName, socket, manager),
		kubeClient:  kubeClient,
		podResource: client.NewPodResource(),
		cache:       cache,
	}
}

func (m *vnumberDevicePlugin) Name() string {
	return "vnumber-plugin"
}

// Start starts the gRPC server, registers the device plugin with the Kubelet.
func (m *vnumberDevicePlugin) Start() error {
	return m.base.Start(m.Name(), m)
}

// Stop stops the gRPC server.
func (m *vnumberDevicePlugin) Stop() error {
	return m.base.Stop(m.Name())
}

// GetDevicePluginOptions returns options to be communicated with Device Manager.
func (m *vnumberDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{PreStartRequired: true}, nil
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears,
// ListAndWatch returns the new list.
func (m *vnumberDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
		klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
	}
	stopCh := m.base.stop
	for {
		select {
		case d := <-m.base.health:
			if d.GPU != nil {
				klog.Infof("'%s' device marked unhealthy: %s", m.base.resourceName, d.GPU.UUID)
				if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
					klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
				}
			}
		case <-stopCh:
			return nil
		}
	}
}

// GetPreferredAllocation returns the preferred allocation from the set of devices specified in the request.
func (m *vnumberDevicePlugin) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

const (
	HostProcDirectoryPath    = "/proc"
	ContManagerDirectoryPath = "/etc/vgpu-manager"
	ContConfigDirectoryPath  = ContManagerDirectoryPath + "/config"
	ContProcDirectoryPath    = ContManagerDirectoryPath + "/host_proc"
	ContCGroupDirectoryPath  = ContManagerDirectoryPath + "/host_cgroup"

	LdPreLoadFileName       = "ld.so.preload"
	ContPreLoadFilePath     = "/etc/" + LdPreLoadFileName
	VGPUControlFileName     = "libvgpu-control.so"
	ContVGPUControlFilePath = ContManagerDirectoryPath + "/driver/" + VGPUControlFileName

	VGPUConfigFileName = "vgpu.config"
	DeviceListFileName = "devices.json"

	//NvidiaDeviceFilePrefix = "/dev/nvidia"
	NvidiaCTLFilePath      = "/dev/nvidiactl"
	NvidiaUVMFilePath      = "/dev/nvidia-uvm"
	NvidiaUVMToolsFilePath = "/dev/nvidia-uvm-tools"
	NvidiaModeSetFilePath  = "/dev/nvidia-modeset"
)

var (
	HostManagerDirectoryPath = os.Getenv("HOST_MANAGER_DIR")
	HostPreLoadFilePath      = HostManagerDirectoryPath + "/" + LdPreLoadFileName
	HostVGPUControlFilePath  = HostManagerDirectoryPath + "/" + VGPUControlFileName
	deviceFileMountConfig    = map[string]bool{
		NvidiaCTLFilePath:      true,
		NvidiaUVMFilePath:      true,
		NvidiaUVMToolsFilePath: true,
		NvidiaModeSetFilePath:  true,
	}
)

func GetHostManagerDirectoryPath(podUID types.UID, containerName string) string {
	return fmt.Sprintf("%s/%s_%s", HostManagerDirectoryPath, string(podUID), containerName)
}

func GetContManagerDirectoryPath(podUID types.UID, containerName string) string {
	return fmt.Sprintf("%s/%s_%s", ContManagerDirectoryPath, string(podUID), containerName)
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container.
func (m *vnumberDevicePlugin) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (resp *pluginapi.AllocateResponse, err error) {
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
		klog.V(4).ErrorS(err, util.AllocateCheckErrMsg)
		err = fmt.Errorf("%s: %s", util.AllocateCheckErrMsg, err.Error())
		if currentPod == nil {
			return
		}
		patchErr := client.PatchPodAllocationFailed(m.kubeClient, currentPod)
		if patchErr != nil {
			klog.Warningf("Pod <%s/%s> PatchPodAllocationFailed error: %v",
				currentPod.Namespace, currentPod.Name, patchErr)
		}
	}()

	activePods, err = client.GetActivePodsOnNode(ctx, m.kubeClient, m.base.manager.GetNodeConfig().NodeName())
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
	klog.V(4).Infof("Current allocated Pod <%s/%s> Uid <%s>",
		currentPod.Namespace, currentPod.Name, currentPod.UID)
	responses := make([]*pluginapi.ContainerAllocateResponse, len(req.ContainerRequests))
	deviceMap := m.base.manager.GetGPUDeviceMap()
	memoryRatio := m.base.manager.GetNodeConfig().DeviceMemoryScaling()
	for i, containerRequest := range req.ContainerRequests {
		number := len(containerRequest.GetDevicesIDs())
		assignDevs, err = device.GetCurrentPreAllocateContainerDevice(currentPod)
		if err != nil {
			klog.V(3).ErrorS(err, "", "pod",
				fmt.Sprintf("%s/%s", currentPod.Namespace, currentPod.Name))
			return resp, err
		}
		if number != len(assignDevs.Devices) {
			err = fmt.Errorf("requested number of devices does not match")
			klog.V(3).ErrorS(err, "", "pod",
				fmt.Sprintf("%s/%s", currentPod.Namespace, currentPod.Name))
			return resp, err
		}
		klog.V(4).Infof("Current Pod <%s/%s> allocated container is <%s>",
			currentPod.Namespace, currentPod.Name, assignDevs.Name)
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
		envMap[util.CudaMemoryRatioEnv] = fmt.Sprintf("%.2f", memoryRatio)
		sort.Slice(assignDevs.Devices, func(i, j int) bool {
			return assignDevs.Devices[i].Id < assignDevs.Devices[j].Id
		})
		for idx, dev := range assignDevs.Devices {
			memoryLimitEnv := fmt.Sprintf("%s_%d", util.CudaMemoryLimitEnv, idx)
			envMap[memoryLimitEnv] = fmt.Sprintf("%dm", dev.Memory)
			deviceIds = append(deviceIds, dev.Uuid)
			gpuDevice, exist := deviceMap[dev.Uuid]
			if !exist {
				err = fmt.Errorf("no device uuid  %s exists", dev.Uuid)
				klog.V(3).ErrorS(err, "", "pod",
					fmt.Sprintf("%s/%s", currentPod.Namespace, currentPod.Name))
				return resp, err
			}
			if dev.Cores > 0 && dev.Cores < util.HundredCore {
				coreLimitEnv := fmt.Sprintf("%s_%d", util.CudaCoreLimitEnv, idx)
				envMap[coreLimitEnv] = strconv.Itoa(dev.Cores)
			}
			for _, deviceFilePath := range gpuDevice.Paths {
				devices = append(devices, &pluginapi.DeviceSpec{
					ContainerPath: deviceFilePath,
					HostPath:      deviceFilePath,
					Permissions:   "rw",
				})
			}
		}
		deviceIdStr := strings.Join(deviceIds, ",")
		envMap[util.GPUDeviceUuidEnv] = deviceIdStr
		envMap[util.NvidiaVisibleDevicesEnv] = deviceIdStr
		for devFilePath, ok := range deviceFileMountConfig {
			if !ok || util.PathIsNotExist(devFilePath) {
				continue
			}
			devices = append(devices, &pluginapi.DeviceSpec{
				ContainerPath: devFilePath,
				HostPath:      devFilePath,
				Permissions:   "rw",
			})
		}
		mounts = append(mounts, &pluginapi.Mount{ // mount /proc dir
			ContainerPath: ContProcDirectoryPath,
			HostPath:      HostProcDirectoryPath,
			ReadOnly:      true,
		}, &pluginapi.Mount{ // mount ld_preload file
			ContainerPath: ContPreLoadFilePath,
			HostPath:      HostPreLoadFilePath,
			ReadOnly:      true,
		}, &pluginapi.Mount{ // mount libvgpu-control.so file
			ContainerPath: ContVGPUControlFilePath,
			HostPath:      HostVGPUControlFilePath,
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
				klog.V(3).ErrorS(err, "", "pod",
					fmt.Sprintf("%s/%s", currentPod.Namespace, currentPod.Name))
				return resp, err
			}
			mounts = append(mounts, &pluginapi.Mount{
				ContainerPath: ContCGroupDirectoryPath,
				HostPath:      cgroupFullPath,
				ReadOnly:      true,
			})
		}
		// /etc/vgpu-manager/<pod-uid>_<cont-name>
		contManagerDirectory := GetContManagerDirectoryPath(currentPod.UID, assignDevs.Name)
		_ = os.MkdirAll(contManagerDirectory, 0777)
		_ = os.Chmod(contManagerDirectory, 0777)
		jsonBytes, _ := json.Marshal(containerRequest.GetDevicesIDs())
		filePath := filepath.Join(contManagerDirectory, DeviceListFileName)
		if err = os.WriteFile(filePath, jsonBytes, 0664); err != nil {
			err = fmt.Errorf("failed to write %s file: %v", DeviceListFileName, err)
			klog.V(3).ErrorS(err, "", "pod",
				fmt.Sprintf("%s/%s", currentPod.Namespace, currentPod.Name))
			return resp, err
		}
		// <host_manager_dir>/<pod-uid>_<cont-name>
		hostManagerDirectory := GetHostManagerDirectoryPath(currentPod.UID, assignDevs.Name)
		mounts = append(mounts, &pluginapi.Mount{ // mount vgpu.config file
			ContainerPath: ContConfigDirectoryPath,
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
			klog.V(3).ErrorS(err, "", "pod",
				fmt.Sprintf("%s/%s", currentPod.Namespace, currentPod.Name))
			return resp, err
		}
		currentPod.Annotations[util.PodVGPURealAllocAnnotation] = realAllocated
		if m.base.manager.GetNodeConfig().GDSEnabled() {
			envMap["NVIDIA_GDS"] = "enabled"
		}
		if m.base.manager.GetNodeConfig().MOFEDEnabled() {
			envMap["NVIDIA_MOFED"] = "enabled"
		}
		responses[i] = &pluginapi.ContainerAllocateResponse{
			Envs:    envMap,
			Mounts:  mounts,
			Devices: devices,
		}
	}
	resp.ContainerResponses = responses
	patchErr := client.PatchPodAllocationSucceed(m.kubeClient, currentPod)
	if patchErr != nil {
		klog.Warningf("Pod <%s/%s> PatchPodAllocationSucceed error: %v",
			currentPod.Namespace, currentPod.Name, patchErr)
	}
	return resp, nil
}

// GetActiveVGPUPodsOnNode Get the vgpu pods on the node
func (m *vnumberDevicePlugin) GetActiveVGPUPodsOnNode() map[string]*corev1.Pod {
	podList := corev1.PodList{}
	err := m.cache.List(context.Background(), &podList)
	if err != nil {
		klog.ErrorS(err, "Failed to query the pod list in the cache")
	}
	activePods := make(map[string]*corev1.Pod)
	nodeName := m.base.manager.GetNodeConfig().NodeName()
	for i, pod := range podList.Items {
		if pod.Spec.NodeName != nodeName ||
			!util.IsVGPUResourcePod(&pod) ||
			util.PodIsTerminated(&pod) {
			continue
		}
		activePods[string(pod.UID)] = &podList.Items[i]
	}
	return activePods
}

// getCurrentPodInfoByCheckpoint find relevant pod information for devicesIDs in kubelet checkpoint
func (m *vnumberDevicePlugin) getCurrentPodInfoByCheckpoint(devicesIDs []string) (*client.PodInfo, error) {
	klog.V(3).Infoln("Try get DevicePlugin checkpoint data")
	pluginPath := m.base.manager.GetNodeConfig().DevicePluginPath()
	cp, err := GetCheckpointData(pluginPath)
	if err != nil {
		return nil, err
	}
	devSet := sets.NewString(devicesIDs...)
	for _, entry := range cp.PodDeviceEntries {
		if entry.ResourceName != util.VGPUNumberResourceName ||
			!devSet.HasAll(entry.DeviceIDs...) {
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

func (m *vnumberDevicePlugin) getCurrentPodInfo(devicesIDs []string) (*client.PodInfo, error) {
	podResources, err := m.podResource.ListPodResource()
	if err != nil {
		klog.ErrorS(err, "ListPodResource failed")
		return m.getCurrentPodInfoByCheckpoint(devicesIDs)
	}
	podInfo, err := m.podResource.GetPodInfoByResourceDeviceIDs(
		podResources, util.VGPUNumberResourceName, devicesIDs)
	if err != nil {
		klog.ErrorS(err, "GetPodInfoByResourceDeviceIDs failed")
		return m.getCurrentPodInfoByCheckpoint(devicesIDs)
	}
	return podInfo, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as resetting the device before making devices available to the container.
func (m *vnumberDevicePlugin) PreStartContainer(ctx context.Context, req *pluginapi.PreStartContainerRequest) (resp *pluginapi.PreStartContainerResponse, err error) {
	klog.V(4).Infoln("PreStartContainer", req.GetDevicesIDs())
	resp = &pluginapi.PreStartContainerResponse{}
	defer func() {
		if err != nil {
			klog.V(4).ErrorS(err, util.PreStartContainerCheckErrMsg)
			err = fmt.Errorf("%s: %s", util.PreStartContainerCheckErrMsg, err.Error())
		}
	}()

	var (
		node      *corev1.Node
		pod       *corev1.Pod
		deviBytes []byte
		deviceIDs []string
		nodeName  = m.base.manager.GetNodeConfig().NodeName()
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
	contManagerDirectory := GetContManagerDirectoryPath(pod.UID, podInfo.ContainerName)
	devicesFilePath := filepath.Join(contManagerDirectory, DeviceListFileName)
	if deviBytes, err = os.ReadFile(devicesFilePath); err != nil {
		err = fmt.Errorf("failed to read %s file: %v", DeviceListFileName, err)
		klog.V(3).ErrorS(err, "", "pod",
			fmt.Sprintf("%s/%s", podInfo.PodNamespace, podInfo.PodName))
		return resp, err
	}
	if err = json.Unmarshal(deviBytes, &deviceIDs); err != nil {
		err = fmt.Errorf("failed to read %s file: %v", DeviceListFileName, err)
		klog.V(3).ErrorS(err, "", "pod",
			fmt.Sprintf("%s/%s", podInfo.PodNamespace, podInfo.PodName))
		return resp, err
	}
	// Verify if there are any errors in the allocation of container equipment.
	if len(deviceIDs) != len(req.GetDevicesIDs()) ||
		!sets.NewString(req.GetDevicesIDs()...).HasAll(deviceIDs...) {
		err = fmt.Errorf("inconsistent allocation results of container equipment")
		klog.V(3).ErrorS(err, "", "pod",
			fmt.Sprintf("%s/%s", podInfo.PodNamespace, podInfo.PodName))
		return resp, err
	}

	vgpuConfigPath := filepath.Join(contManagerDirectory, VGPUConfigFileName)
	klog.V(4).Infof("Pod <%s/%s> container <%s> vgpu config path is <%s>",
		pod.Namespace, pod.Name, podInfo.ContainerName, vgpuConfigPath)
	realAlloc, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	realDevices := device.PodDevices{}
	if err = realDevices.UnmarshalText(realAlloc); err != nil {
		err = fmt.Errorf("parse pod assign devices failed: %v", err)
		klog.V(3).ErrorS(err, "", "pod",
			fmt.Sprintf("%s/%s", podInfo.PodNamespace, podInfo.PodName))
		return resp, err
	}
	index := slices.IndexFunc(realDevices, func(contDevs device.ContainerDevices) bool {
		return contDevs.Name == podInfo.ContainerName
	})
	if index < 0 {
		err = fmt.Errorf("unable to find allocated devices for container <%s>", podInfo.ContainerName)
		klog.V(3).ErrorS(err, "", "pod",
			fmt.Sprintf("%s/%s", podInfo.PodNamespace, podInfo.PodName))
		return resp, err
	}
	oversold := slices.ContainsFunc(pod.Spec.Containers, func(container corev1.Container) bool {
		return container.Name == podInfo.ContainerName && slices.ContainsFunc(container.Env, func(env corev1.EnvVar) bool {
			return env.Name == util.CudaMemoryOversoldEnv && strings.ToUpper(env.Value) == "TRUE"
		})
	})
	err = vgpu.WriteVGPUConfigFile(vgpuConfigPath, m.base.manager, pod, realDevices[index], oversold, node)
	if err != nil {
		klog.V(3).ErrorS(err, "", "pod",
			fmt.Sprintf("%s/%s", podInfo.PodNamespace, podInfo.PodName))
	}
	return resp, err
}

func (m *vnumberDevicePlugin) Devices() []*pluginapi.Device {
	var devices []*pluginapi.Device
	for _, gpuDevice := range m.base.manager.GetNodeDeviceInfo() {
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
				ID:     devId,
				Health: health,
				Topology: &pluginapi.TopologyInfo{
					Nodes: []*pluginapi.NUMANode{{
						ID: int64(gpuDevice.Numa),
					}},
				},
			})
		}
	}
	return devices
}
