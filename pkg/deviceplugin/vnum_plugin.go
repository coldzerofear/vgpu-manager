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
	"github.com/coldzerofear/vgpu-manager/pkg/device/imex"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"k8s.io/client-go/util/retry"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type vnumberDevicePlugin struct {
	pluginapi.UnimplementedDevicePluginServer
	base        *baseDevicePlugin
	kubeClient  *kubernetes.Clientset
	podResource *client.PodResource
	cache       cache.Cache
}

var _ DevicePlugin = &vnumberDevicePlugin{}

// NewVNumberDevicePlugin returns an initialized vnumberDevicePlugin.
func NewVNumberDevicePlugin(resourceName, socket string, manager *manager.DeviceManager,
	kubeClient *kubernetes.Clientset, cache cache.Cache) DevicePlugin {

	return &vnumberDevicePlugin{
		base:        newBaseDevicePlugin(resourceName, socket, manager),
		podResource: client.NewPodResource(),
		kubeClient:  kubeClient,
		cache:       cache,
	}
}

func (m *vnumberDevicePlugin) Name() string {
	return "vnumber-plugin"
}

// Start starts the gRPC server, registers the device plugin with the Kubelet.
func (m *vnumberDevicePlugin) Start() error {
	err := m.base.Start(m.Name(), m)
	if err == nil {
		m.base.manager.AddRegistryFunc(m.Name(), m.registryDevices)
		m.base.manager.AddCleanupRegistryFunc(m.Name(), m.cleanupRegistry)
	}
	return err
}

// Stop stops the gRPC server.
func (m *vnumberDevicePlugin) Stop() error {
	err := m.base.Stop(m.Name())
	m.base.manager.RemoveRegistryFunc(m.Name())
	m.base.manager.RemoveCleanupRegistryFunc(m.Name())
	return err
}

var (
	encodeNodeConfigInfo   string
	encodeNodeTopologyInfo string
)

func (m *vnumberDevicePlugin) getEncodeNodeTopologyInfo() (string, error) {
	if encodeNodeTopologyInfo == "" {
		nodeTopologyInfo := m.base.manager.GetNodeTopologyInfo()
		info, err := nodeTopologyInfo.Encode()
		if err != nil {
			return "", fmt.Errorf("encoding node topology information failed: %v", err)
		}
		klog.V(3).Infof("node GPU topology information: %s", info)
		encodeNodeTopologyInfo = info
	}
	return encodeNodeTopologyInfo, nil
}

func (m *vnumberDevicePlugin) getDecodeNodeConfigInfo() (string, error) {
	if encodeNodeConfigInfo == "" {
		nodeConfigInfo := device.NodeConfigInfo{
			DeviceSplit:   m.base.manager.GetNodeConfig().GetDeviceSplitCount(),
			CoresScaling:  m.base.manager.GetNodeConfig().GetDeviceCoresScaling(),
			MemoryFactor:  m.base.manager.GetNodeConfig().GetDeviceMemoryFactor(),
			MemoryScaling: m.base.manager.GetNodeConfig().GetDeviceMemoryScaling(),
		}
		info, err := nodeConfigInfo.Encode()
		if err != nil {
			return "", fmt.Errorf("encoding node configuration information failed: %v", err)
		}
		klog.V(3).Infof("node GPU configuration information: %s", info)
		encodeNodeConfigInfo = info
	}
	return encodeNodeConfigInfo, nil
}

func (m *vnumberDevicePlugin) registryDevices(featureGate featuregate.FeatureGate) (*client.PatchMetadata, error) {
	registryGPUs, err := m.base.manager.GetNodeDeviceInfo().Encode()
	if err != nil {
		return nil, fmt.Errorf("encoding node device information failed: %v", err)
	}
	heartbeatTime, err := metav1.NowMicro().MarshalText()
	if err != nil {
		return nil, fmt.Errorf("failed to generate node heartbeat timestamp: %v", err)
	}
	var registryGPUTopology string
	if featureGate.Enabled(util.GPUTopology) {
		if registryGPUTopology, err = m.getEncodeNodeTopologyInfo(); err != nil {
			return nil, err
		}
	}
	nodeConfigEncode, err := m.getDecodeNodeConfigInfo()
	if err != nil {
		return nil, err
	}
	metadata := client.PatchMetadata{
		Annotations: map[string]string{
			util.NodeDeviceRegisterAnnotation:  registryGPUs,
			util.NodeDeviceHeartbeatAnnotation: string(heartbeatTime),
			util.NodeDeviceTopologyAnnotation:  registryGPUTopology,
			util.NodeConfigInfoAnnotation:      nodeConfigEncode,
		},
		Labels: map[string]string{
			util.NodeNvidiaDriverVersionLabel: m.base.manager.GetDriverVersion().DriverVersion,
			util.NodeNvidiaCudaVersionLabel:   strconv.Itoa(int(m.base.manager.GetDriverVersion().CudaDriverVersion)),
		},
	}
	return &metadata, nil
}

func (m *vnumberDevicePlugin) cleanupRegistry(_ featuregate.FeatureGate) (*client.PatchMetadata, error) {
	metadata := client.PatchMetadata{
		Annotations: map[string]string{
			util.NodeDeviceHeartbeatAnnotation: "",
			util.NodeDeviceRegisterAnnotation:  "",
			util.NodeDeviceTopologyAnnotation:  "",
			util.NodeConfigInfoAnnotation:      "",
		},
	}
	return &metadata, nil
}

// GetDevicePluginOptions returns options to be communicated with Device Manager.
func (m *vnumberDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	klog.V(4).InfoS("GetDevicePluginOptions", "pluginName", m.Name())
	return &pluginapi.DevicePluginOptions{PreStartRequired: true}, nil
}

// ListAndWatch returns a stream of List of Devices,
// Whenever a Device state change or a Device disappears,
// ListAndWatch returns the new list.
func (m *vnumberDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	klog.V(4).InfoS("ListAndWatch", "pluginName", m.Name(), "server", s)
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
		klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
	}
	stopCh := m.base.stop
	for {
		select {
		case d := <-m.base.health:
			if d.GPU != nil && !d.GPU.MigEnabled {
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
func (m *vnumberDevicePlugin) GetPreferredAllocation(_ context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	klog.V(4).InfoS("GetPreferredAllocation", "pluginName", m.Name(), "request", req.GetContainerRequests())
	return &pluginapi.PreferredAllocationResponse{}, nil
}

const (
	VGPUConfigDirName        = "config"
	HostProcDirectoryPath    = "/proc"
	ContManagerDirectoryPath = util.ManagerRootPath
	ContConfigDirectoryPath  = ContManagerDirectoryPath + "/" + VGPUConfigDirName
	ContProcDirectoryPath    = ContManagerDirectoryPath + "/.host_proc"
	ContCGroupDirectoryPath  = ContManagerDirectoryPath + "/.host_cgroup"
	ContWatcherDirectoryPath = ContManagerDirectoryPath + "/" + util.Watcher

	VGPULockDirName  = "vgpu_lock"
	ContVGPULockPath = "/tmp/." + VGPULockDirName

	LdPreLoadFileName       = "ld.so.preload"
	ContPreLoadFilePath     = "/etc/" + LdPreLoadFileName
	VGPUControlFileName     = "libvgpu-control.so"
	ContVGPUControlFilePath = ContManagerDirectoryPath + "/driver/" + VGPUControlFileName

	VGPUConfigFileName = "vgpu.config"
	DeviceListFileName = "devices.json"

	NvidiaCTLFilePath      = "/dev/nvidiactl"
	NvidiaUVMFilePath      = "/dev/nvidia-uvm"
	NvidiaUVMToolsFilePath = "/dev/nvidia-uvm-tools"
	NvidiaModeSetFilePath  = "/dev/nvidia-modeset"

	deviceListEnvVar                          = "NVIDIA_VISIBLE_DEVICES"
	deviceListAsVolumeMountsHostPath          = "/dev/null"
	deviceListAsVolumeMountsContainerPathRoot = "/var/run/nvidia-container-devices"
)

var (
	HostManagerDirectoryPath = os.Getenv("HOST_MANAGER_DIR")
	HostPreLoadFilePath      = HostManagerDirectoryPath + "/" + LdPreLoadFileName
	HostVGPUControlFilePath  = HostManagerDirectoryPath + "/" + VGPUControlFileName
	HostWatcherDirectoryPath = HostManagerDirectoryPath + "/" + util.Watcher
)

func GetHostManagerDirectoryPath(podUID types.UID, containerName string) string {
	return fmt.Sprintf("%s/%s_%s", HostManagerDirectoryPath, string(podUID), containerName)
}

func GetContManagerDirectoryPath(podUID types.UID, containerName string) string {
	return fmt.Sprintf("%s/%s_%s", ContManagerDirectoryPath, string(podUID), containerName)
}

func passDeviceSpecs(devices []manager.Device, imexChannels imex.Channels) []*pluginapi.DeviceSpec {
	deviceMountOptional := map[string]bool{
		NvidiaCTLFilePath:      true,
		NvidiaUVMFilePath:      true,
		NvidiaUVMToolsFilePath: true,
		NvidiaModeSetFilePath:  true,
	}
	devPaths := sets.NewString()
	for _, dev := range devices {
		if dev.GPU != nil {
			devPaths.Insert(dev.GPU.Paths...)
		}
		if dev.MIG != nil {
			devPaths.Insert(dev.MIG.Paths...)
		}
	}
	var specs []*pluginapi.DeviceSpec
	for devPath := range devPaths {
		specs = append(specs, &pluginapi.DeviceSpec{
			ContainerPath: devPath,
			HostPath:      devPath,
			Permissions:   "rw",
		})
	}
	for devPath, enabled := range deviceMountOptional {
		if !enabled || util.PathIsNotExist(devPath) {
			continue
		}
		specs = append(specs, &pluginapi.DeviceSpec{
			ContainerPath: devPath,
			HostPath:      devPath,
			Permissions:   "rw",
		})
	}
	for _, channel := range imexChannels {
		spec := &pluginapi.DeviceSpec{
			ContainerPath: channel.Path,
			// TODO: The HostPath property for a channel is not the correct value to use here.
			// The `devRoot` there represents the devRoot in the current container when discovering devices
			// and is set to "{{ .*config.Flags.Plugin.ContainerDriverRoot }}/dev".
			// The devRoot in this context is the {{ .config.Flags.NvidiaDevRoot }} and defines the
			// root for device nodes on the host. This is usually / or /run/nvidia/driver when the
			// driver container is used.
			HostPath:    channel.HostPath,
			Permissions: "rw",
		}
		specs = append(specs, spec)
	}
	return specs
}

func updateResponseForNodeConfig(response *pluginapi.ContainerAllocateResponse, devManager *manager.DeviceManager, deviceIDs ...string) {
	switch devManager.GetNodeConfig().GetDeviceListStrategy() {
	case util.DeviceListStrategyEnvvar:
		response.Envs[deviceListEnvVar] = strings.Join(deviceIDs, ",")
		var channelIDs []string
		for _, channel := range devManager.GetImexChannels() {
			channelIDs = append(channelIDs, channel.ID)
		}
		if len(channelIDs) > 0 {
			response.Envs[imex.ImexChannelEnvVar] = strings.Join(channelIDs, ",")
		}
	case util.DeviceListStrategyVolumeMounts:
		response.Envs[deviceListEnvVar] = deviceListAsVolumeMountsContainerPathRoot
		for _, id := range deviceIDs {
			mount := &pluginapi.Mount{
				HostPath:      deviceListAsVolumeMountsHostPath,
				ContainerPath: filepath.Join(deviceListAsVolumeMountsContainerPathRoot, id),
			}
			response.Mounts = append(response.Mounts, mount)
		}
		for _, channel := range devManager.GetImexChannels() {
			mount := &pluginapi.Mount{
				HostPath:      deviceListAsVolumeMountsHostPath,
				ContainerPath: filepath.Join(deviceListAsVolumeMountsContainerPathRoot, "imex", channel.ID),
			}
			response.Mounts = append(response.Mounts, mount)
		}
	}
	if devManager.GetNodeConfig().GetGDSEnabled() {
		response.Envs["NVIDIA_GDS"] = "enabled"
	}
	if devManager.GetNodeConfig().GetMOFEDEnabled() {
		response.Envs["NVIDIA_MOFED"] = "enabled"
	}
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container.
func (m *vnumberDevicePlugin) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (resp *pluginapi.AllocateResponse, err error) {
	klog.V(4).InfoS("Allocate", "pluginName", m.Name(), "request", req.GetContainerRequests())
	var (
		activePods    []corev1.Pod
		currentPod    *corev1.Pod
		assignDevs    *device.ContainerDevices
		podCgroupPath string
	)
	resp = &pluginapi.AllocateResponse{}
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
			klog.Warningf("Pod <%s> PatchPodAllocationFailed error: %v", klog.KObj(currentPod), patchErr)
		}
	}()

	nodeConfig := m.base.manager.GetNodeConfig()
	activePods, err = client.GetActivePodsOnNode(ctx, m.kubeClient, nodeConfig.GetNodeName())
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
	klog.V(4).InfoS("Equipment allocation in progress", "pod", klog.KObj(currentPod), "uid", currentPod.UID)

	responses := make([]*pluginapi.ContainerAllocateResponse, len(req.ContainerRequests))
	deviceMap := m.base.manager.GetGPUDeviceMap()
	memoryRatio := nodeConfig.GetDeviceMemoryScaling()
	imexChannels := m.base.manager.GetImexChannels()
	enabledSMWatcher := m.base.manager.GetFeatureGate().Enabled(util.SMWatcher)
	for i, containerRequest := range req.ContainerRequests {
		number := len(containerRequest.GetDevicesIds())
		assignDevs, err = device.GetCurrentPreAllocateContainerDevice(currentPod)
		if err != nil {
			klog.V(3).ErrorS(err, "", "pod", klog.KObj(currentPod))
			return resp, err
		}
		if number != len(assignDevs.Devices) {
			err = fmt.Errorf("requested number of devices does not match")
			klog.V(3).ErrorS(err, "", "pod", klog.KObj(currentPod))
			return resp, err
		}
		klog.V(4).Infof("Current Pod <%s> allocated container is <%s>", klog.KObj(currentPod), assignDevs.Name)
		var (
			deviceIds  []string
			gpuDevices []manager.Device
			response   = &pluginapi.ContainerAllocateResponse{
				Envs: make(map[string]string),
			}
		)
		response.Envs[util.PodNameEnv] = currentPod.Name
		response.Envs[util.PodNamespaceEnv] = currentPod.Namespace
		response.Envs[util.PodUIDEnv] = string(currentPod.UID)
		response.Envs[util.ContNameEnv] = assignDevs.Name
		response.Envs[util.CudaMemoryRatioEnv] = fmt.Sprintf("%.2f", memoryRatio)
		sort.Slice(assignDevs.Devices, func(i, j int) bool {
			return assignDevs.Devices[i].Id < assignDevs.Devices[j].Id
		})
		for idx, dev := range assignDevs.Devices {
			memoryLimitEnv := fmt.Sprintf("%s_%d", util.CudaMemoryLimitEnv, idx)
			response.Envs[memoryLimitEnv] = fmt.Sprintf("%dm", dev.Memory)
			deviceIds = append(deviceIds, dev.Uuid)
			gpuDevice, exists := deviceMap[dev.Uuid]
			if !exists {
				err = fmt.Errorf("GPU device %s does not exist", dev.Uuid)
				klog.V(3).ErrorS(err, "", "pod", klog.KObj(currentPod))
				return resp, err
			}
			gpuDevices = append(gpuDevices, manager.Device{GPU: &gpuDevice})
			if dev.Cores > 0 && dev.Cores < util.HundredCore {
				coreLimitEnv := fmt.Sprintf("%s_%d", util.CudaCoreLimitEnv, idx)
				response.Envs[coreLimitEnv] = strconv.Itoa(dev.Cores)
			}
		}
		response.Envs[util.GPUDevicesUuidEnv] = strings.Join(deviceIds, ",")
		updateResponseForNodeConfig(response, m.base.manager, deviceIds...)
		response.Devices = append(response.Devices, passDeviceSpecs(gpuDevices, imexChannels)...)
		response.Mounts = append(response.Mounts, &pluginapi.Mount{ // mount /proc dir
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
		if enabledSMWatcher {
			response.Mounts = append(response.Mounts, &pluginapi.Mount{ // mount /proc dir
				ContainerPath: ContWatcherDirectoryPath,
				HostPath:      HostWatcherDirectoryPath,
				ReadOnly:      true,
			})
		}
		podCgroupPath, err = util.GetK8sPodCGroupPath(currentPod)
		if err != nil {
			klog.Errorln(err.Error())
			return resp, err
		}
		var hostCGroupFullPath string
		switch {
		case cgroups.IsCgroup2UnifiedMode(): // cgroupv2
			hostCGroupFullPath = util.GetK8sPodCGroupFullPath(podCgroupPath)
		case cgroups.IsCgroup2HybridMode():
			hostCGroupFullPath = util.GetK8sPodDeviceCGroupFullPath(podCgroupPath)
			baseCgroupPath := util.SplitK8sCGroupBasePath(hostCGroupFullPath)
			// If the device controller does not exist, use the path of cgroupv2.
			if util.PathIsNotExist(baseCgroupPath) {
				klog.V(3).Infof("try find k8s cgroup path failed: %s", baseCgroupPath)
				hostCGroupFullPath = util.GetK8sPodCGroupFullPath(podCgroupPath)
			}
		default: // cgroupv1
			hostCGroupFullPath = util.GetK8sPodDeviceCGroupFullPath(podCgroupPath)
		}
		baseCgroupPath := util.SplitK8sCGroupBasePath(hostCGroupFullPath)
		if util.PathIsNotExist(baseCgroupPath) {
			err = fmt.Errorf("unable to find k8s cgroup path: %s", baseCgroupPath)
			klog.V(3).ErrorS(err, "", "pod", klog.KObj(currentPod))
			return resp, err
		}
		response.Mounts = append(response.Mounts, &pluginapi.Mount{
			ContainerPath: ContCGroupDirectoryPath,
			HostPath:      hostCGroupFullPath,
			ReadOnly:      true,
		})
		// /etc/vgpu-manager/<pod-uid>_<cont-name>
		contManagerDirectory := GetContManagerDirectoryPath(currentPod.UID, assignDevs.Name)
		_ = os.MkdirAll(contManagerDirectory, 0777)
		_ = os.Chmod(contManagerDirectory, 0777)
		// /etc/vgpu-manager/<pod-uid>_<cont-name>/devices.json
		filePath := filepath.Join(contManagerDirectory, DeviceListFileName)
		jsonBytes, _ := json.Marshal(containerRequest.GetDevicesIds())
		if err = os.WriteFile(filePath, jsonBytes, 0664); err != nil {
			err = fmt.Errorf("failed to write %s file: %v", DeviceListFileName, err)
			klog.V(3).ErrorS(err, "", "pod", klog.KObj(currentPod))
			return resp, err
		}
		// /etc/vgpu-manager/<pod-uid>_<cont-name>/vgpu_lock
		contVGPULockPath := filepath.Join(contManagerDirectory, VGPULockDirName)
		_ = os.MkdirAll(contVGPULockPath, 0777)
		_ = os.Chmod(contVGPULockPath, 0777)
		// <host_manager_dir>/<pod-uid>_<cont-name>
		hostManagerDirectory := GetHostManagerDirectoryPath(currentPod.UID, assignDevs.Name)
		// <host_manager_dir>/<pod-uid>_<cont-name>/config
		hostVGPUConfigPath := filepath.Join(hostManagerDirectory, VGPUConfigDirName)
		// <host_manager_dir>/<pod-uid>_<cont-name>/vgpu_lock
		hostVGPULockPath := filepath.Join(hostManagerDirectory, VGPULockDirName)
		response.Mounts = append(response.Mounts, &pluginapi.Mount{ // mount vgpu.config file
			ContainerPath: ContConfigDirectoryPath,
			HostPath:      hostVGPUConfigPath,
			ReadOnly:      true,
		}, &pluginapi.Mount{ // mount vgpu_lock dir
			ContainerPath: ContVGPULockPath,
			HostPath:      hostVGPULockPath,
			ReadOnly:      false,
		})
		podDevices := device.PodDevices{}
		if realAlloc, ok := util.HasAnnotation(currentPod, util.PodVGPURealAllocAnnotation); ok {
			_ = podDevices.UnmarshalText(realAlloc)
		}
		podDevices = append(podDevices, *assignDevs)
		var realAllocated string
		if realAllocated, err = podDevices.MarshalText(); err != nil {
			err = fmt.Errorf("real allocated of encoding device failed: %v", err)
			klog.V(3).ErrorS(err, "", "pod", klog.KObj(currentPod))
			return resp, err
		}
		currentPod.Annotations[util.PodVGPURealAllocAnnotation] = realAllocated

		responses[i] = response
	}
	resp.ContainerResponses = responses
	patchErr := client.PatchPodAllocationSucceed(m.kubeClient, currentPod)
	if patchErr != nil {
		klog.Warningf("Pod <%s> PatchPodAllocationSucceed error: %v", klog.KObj(currentPod), patchErr)
	}
	return resp, nil
}

// GetActiveVGPUPodsOnNode Get the vgpu pods on the node.
func (m *vnumberDevicePlugin) GetActiveVGPUPodsOnNode() map[string]*corev1.Pod {
	podList := corev1.PodList{}
	err := m.cache.List(context.Background(), &podList)
	if err != nil {
		klog.ErrorS(err, "Failed to query the pod list in the cache")
	}
	activePods := make(map[string]*corev1.Pod)
	nodeName := m.base.manager.GetNodeConfig().GetNodeName()
	for i, pod := range podList.Items {
		if pod.Spec.NodeName != nodeName ||
			util.PodIsTerminated(&pod) ||
			!util.IsVGPUResourcePod(&pod) {
			continue
		}
		activePods[string(pod.UID)] = &podList.Items[i]
	}
	return activePods
}

// getCurrentPodInfoByCheckpoint find relevant pod information for devicesIDs in kubelet checkpoint
func (m *vnumberDevicePlugin) getCurrentPodInfoByCheckpoint(devicesIDs []string) (*client.PodInfo, error) {
	klog.V(3).Infoln("Try get DevicePlugin checkpoint data")
	devicePluginPath := m.base.manager.GetNodeConfig().GetDevicePluginPath()
	cp, err := GetCheckpointData(devicePluginPath)
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
	resp, err := m.podResource.ListPodResource()
	if err != nil {
		klog.ErrorS(err, "ListPodResource failed")
		return m.getCurrentPodInfoByCheckpoint(devicesIDs)
	}
	deviceSet := sets.NewString(devicesIDs...)
	podInfo, err := m.podResource.GetPodInfoByMatchFunc(resp, func(devices *v1alpha1.ContainerDevices) bool {
		return devices.GetResourceName() == util.VGPUNumberResourceName &&
			deviceSet.HasAll(devices.GetDeviceIds()...)
	})
	if err != nil {
		klog.ErrorS(err, "GetPodInfoByMatchFunc failed")
		return m.getCurrentPodInfoByCheckpoint(devicesIDs)
	}
	return podInfo, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as resetting the device before making devices available to the container.
func (m *vnumberDevicePlugin) PreStartContainer(ctx context.Context, req *pluginapi.PreStartContainerRequest) (resp *pluginapi.PreStartContainerResponse, err error) {
	klog.V(4).InfoS("PreStartContainer", "pluginName", m.Name(), "request", req.GetDevicesIds())
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
		nodeName  = m.base.manager.GetNodeConfig().GetNodeName()
	)
	err = retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		// Node does not require timeliness, search from API server cache.
		opts := metav1.GetOptions{ResourceVersion: "0"}
		node, err = m.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, opts)
		return err
	})
	if err != nil {
		klog.Errorf("failed to get node <%s>: %v", nodeName, err)
		return resp, err
	}
	podInfo, err := m.getCurrentPodInfo(req.GetDevicesIds())
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
		klog.Errorf("failed to get pod <%s/%s>: %v", podInfo.PodNamespace, podInfo.PodName, err)
		return resp, err
	}
	// /etc/vgpu-manager/<pod-uid>_<cont-name>
	contManagerDirectory := GetContManagerDirectoryPath(pod.UID, podInfo.ContainerName)
	// /etc/vgpu-manager/<pod-uid>_<cont-name>/devices.json
	devicesFilePath := filepath.Join(contManagerDirectory, DeviceListFileName)
	if deviBytes, err = os.ReadFile(devicesFilePath); err != nil {
		err = fmt.Errorf("failed to read %s file: %v", DeviceListFileName, err)
		klog.V(3).ErrorS(err, "", "pod", klog.KObj(pod))
		return resp, err
	}
	if err = json.Unmarshal(deviBytes, &deviceIDs); err != nil {
		err = fmt.Errorf("failed to read %s file: %v", DeviceListFileName, err)
		klog.V(3).ErrorS(err, "", "pod", klog.KObj(pod))
		return resp, err
	}
	// Verify if there are any errors in the allocation of container equipment.
	if len(deviceIDs) != len(req.GetDevicesIds()) ||
		!sets.NewString(req.GetDevicesIds()...).HasAll(deviceIDs...) {
		err = fmt.Errorf("inconsistent allocation results of container equipment")
		klog.V(3).ErrorS(err, "", "pod", klog.KObj(pod))
		return resp, err
	}
	// /etc/vgpu-manager/<pod-uid>_<cont-name>/config
	contVGPUConfigPath := filepath.Join(contManagerDirectory, VGPUConfigDirName)
	_ = os.MkdirAll(contVGPUConfigPath, 0777)
	_ = os.Chmod(contVGPUConfigPath, 0777)
	// /etc/vgpu-manager/<pod-uid>_<cont-name>/config/vgpu.config
	vgpuConfigFilePath := filepath.Join(contVGPUConfigPath, VGPUConfigFileName)
	klog.V(4).Infof("Pod <%s/%s> container <%s> vgpu config path is <%s>",
		pod.Namespace, pod.Name, podInfo.ContainerName, vgpuConfigFilePath)
	realAlloc, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	realDevices := device.PodDevices{}
	if err = realDevices.UnmarshalText(realAlloc); err != nil {
		err = fmt.Errorf("parse pod assign devices failed: %v", err)
		klog.V(3).ErrorS(err, "", "pod", klog.KObj(pod))
		return resp, err
	}
	index := slices.IndexFunc(realDevices, func(contDevs device.ContainerDevices) bool {
		return contDevs.Name == podInfo.ContainerName
	})
	if index < 0 {
		err = fmt.Errorf("unable to find allocated devices for container <%s>", podInfo.ContainerName)
		klog.V(3).ErrorS(err, "", "pod", klog.KObj(pod))
		return resp, err
	}
	oversold := slices.ContainsFunc(pod.Spec.Containers, func(cont corev1.Container) bool {
		return cont.Name == podInfo.ContainerName && slices.ContainsFunc(cont.Env, func(env corev1.EnvVar) bool {
			return env.Name == util.CudaMemoryOversoldEnv && strings.ToUpper(env.Value) == "TRUE"
		})
	})
	err = vgpu.WriteVGPUConfigFile(vgpuConfigFilePath, m.base.manager, pod, realDevices[index], oversold, node)
	if err != nil {
		klog.V(3).ErrorS(err, "", "pod", klog.KObj(pod))
	}
	return resp, err
}

func (m *vnumberDevicePlugin) Devices() []*pluginapi.Device {
	var devices []*pluginapi.Device
	for _, gpuDevice := range m.base.manager.GetNodeDeviceInfo() {
		if gpuDevice.Mig { // skip MIG device
			continue
		}
		var topologyInfo *pluginapi.TopologyInfo
		if gpuDevice.Numa >= 0 {
			topologyInfo = &pluginapi.TopologyInfo{
				Nodes: []*pluginapi.NUMANode{
					{ID: int64(gpuDevice.Numa)},
				},
			}
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
				Topology: topologyInfo,
			})
		}
	}
	return devices
}
