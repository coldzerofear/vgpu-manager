package vgpu

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/coldzerofear/vgpu-manager/pkg/device/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/base"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/checkpoint"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	client2 "sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/client-go/util/retry"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type vNumberDevicePlugin struct {
	pluginapi.UnimplementedDevicePluginServer
	baseServer  base.PluginServer
	kubeClient  *kubernetes.Clientset
	podResource *client.PodResource
	server      *registry.DeviceRegistryServerImpl
	cache       cache.Cache
}

var _ base.DevicePlugin = &vNumberDevicePlugin{}

// NewVNumberDevicePlugin returns an initialized vNumberDevicePlugin.
func NewVNumberDevicePlugin(resourceName, socket string, manager *manager.DeviceManager,
	kubeClient *kubernetes.Clientset, cache cache.Cache) base.DevicePlugin {
	server := registry.NewDeviceRegistryServer(cache, ContManagerDirectoryPath)
	podResource := client.NewPodResource(client.WithCallTimeoutSecond(5))
	return &vNumberDevicePlugin{
		baseServer:  base.NewBasePluginServer(resourceName, socket, manager),
		podResource: podResource,
		kubeClient:  kubeClient,
		server:      server,
		cache:       cache,
	}
}

func (m *vNumberDevicePlugin) Name() string {
	return "vnumber-plugin"
}

// Start starts the gRPC server, registers the device plugin with the Kubelet.
func (m *vNumberDevicePlugin) Start() error {
	err := m.baseServer.Start(m.Name(), m)
	if err == nil {
		m.baseServer.GetDeviceManager().AddRegistryFunc(m.Name(), m.registryDevices)
		m.baseServer.GetDeviceManager().AddCleanupRegistryFunc(m.Name(), m.cleanupRegistry)
		if m.baseServer.GetDeviceManager().GetFeatureGate().Enabled(util.ClientMode) {
			if err = m.server.Start(); err != nil {
				klog.ErrorS(err, "DeviceRegistryServer failed to start")
			}
		}
	}
	return err
}

// Stop stops the gRPC server.
func (m *vNumberDevicePlugin) Stop() error {
	err := m.baseServer.Stop(m.Name())
	m.baseServer.GetDeviceManager().RemoveRegistryFunc(m.Name())
	m.baseServer.GetDeviceManager().RemoveCleanupRegistryFunc(m.Name())
	if m.baseServer.GetDeviceManager().GetFeatureGate().Enabled(util.ClientMode) {
		m.server.Stop()
	}
	return err
}

var (
	encodeNodeConfigInfo   string
	encodeNodeTopologyInfo string
)

func (m *vNumberDevicePlugin) getEncodeNodeTopologyInfo() (string, error) {
	if encodeNodeTopologyInfo == "" {
		nodeTopologyInfo := m.baseServer.GetDeviceManager().GetNodeTopologyInfo()
		info, err := nodeTopologyInfo.Encode()
		if err != nil {
			return "", fmt.Errorf("encoding node topology information failed: %v", err)
		}
		klog.V(3).Infof("node GPU topology information: %s", info)
		encodeNodeTopologyInfo = info
	}
	return encodeNodeTopologyInfo, nil
}

func (m *vNumberDevicePlugin) getDecodeNodeConfigInfo() (string, error) {
	if encodeNodeConfigInfo == "" {
		nodeConfigInfo := device.NodeConfigInfo{
			DeviceSplit:   m.baseServer.GetDeviceManager().GetNodeConfig().GetDeviceSplitCount(),
			CoresScaling:  m.baseServer.GetDeviceManager().GetNodeConfig().GetDeviceCoresScaling(),
			MemoryFactor:  m.baseServer.GetDeviceManager().GetNodeConfig().GetDeviceMemoryFactor(),
			MemoryScaling: m.baseServer.GetDeviceManager().GetNodeConfig().GetDeviceMemoryScaling(),
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

func (m *vNumberDevicePlugin) registryDevices(featureGate featuregate.FeatureGate) (*client.PatchMetadata, error) {
	registryGPUs, err := m.baseServer.GetDeviceManager().GetNodeDeviceInfo().Encode()
	if err != nil {
		return nil, fmt.Errorf("encoding node device information failed: %v", err)
	}
	var registryGPUTopology *string
	if featureGate.Enabled(util.GPUTopology) {
		gpuTopology, err := m.getEncodeNodeTopologyInfo()
		if err != nil {
			return nil, err
		}
		registryGPUTopology = &gpuTopology
	}
	nodeConfigEncode, err := m.getDecodeNodeConfigInfo()
	if err != nil {
		return nil, err
	}
	driverVersion := m.baseServer.GetDeviceManager().GetDriverVersion().DriverVersion
	cudaDriverVersion := strconv.Itoa(int(m.baseServer.GetDeviceManager().GetDriverVersion().CudaDriverVersion))
	metadata := client.PatchMetadata{
		Annotations: map[string]*string{
			util.NodeConfigInfoAnnotation:     pointer.String(nodeConfigEncode),
			util.NodeDeviceRegisterAnnotation: pointer.String(registryGPUs),
			util.NodeDeviceTopologyAnnotation: registryGPUTopology,
		},
		Labels: map[string]*string{
			util.NodeNvidiaDriverVersionLabel: pointer.String(driverVersion),
			util.NodeNvidiaCudaVersionLabel:   pointer.String(cudaDriverVersion),
		},
	}
	return &metadata, nil
}

func (m *vNumberDevicePlugin) cleanupRegistry(_ featuregate.FeatureGate) (*client.PatchMetadata, error) {
	metadata := client.PatchMetadata{
		Annotations: map[string]*string{
			// TODO Reserved for cleaning up after upgrading
			util.NodeDeviceHeartbeatAnnotation: nil,
			util.NodeDeviceRegisterAnnotation:  nil,
			util.NodeDeviceTopologyAnnotation:  nil,
			util.NodeConfigInfoAnnotation:      nil,
		},
		Labels: map[string]*string{
			util.NodeNvidiaDriverVersionLabel: nil,
			util.NodeNvidiaCudaVersionLabel:   nil,
		},
	}
	return &metadata, nil
}

// GetDevicePluginOptions returns options to be communicated with Device Manager.
func (m *vNumberDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	klog.V(4).InfoS("GetDevicePluginOptions", "pluginName", m.Name())
	return &pluginapi.DevicePluginOptions{PreStartRequired: true}, nil
}

// ListAndWatch returns a stream of List of Devices, Whenever a Device state change or a Device disappears,
// ListAndWatch returns the new list.
func (m *vNumberDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	klog.V(4).InfoS("ListAndWatch", "pluginName", m.Name(), "server", s)
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
		klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
	}
	stopCh := m.baseServer.GetStopCh()
	for {
		select {
		case d := <-m.baseServer.GetDeviceCh():
			if d.GPU != nil && !d.GPU.MigEnabled {
				klog.Infof("'%s' device marked unhealthy: %s", m.baseServer.GetResourceName(), d.GPU.UUID)
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
func (m *vNumberDevicePlugin) GetPreferredAllocation(_ context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	klog.V(4).InfoS("GetPreferredAllocation", "pluginName", m.Name(), "request", req.GetContainerRequests())
	return &pluginapi.PreferredAllocationResponse{}, nil
}

const (
	HostProcDirectoryPath    = "/proc"
	ContManagerDirectoryPath = util.ManagerRootPath
	ContConfigDirectoryPath  = ContManagerDirectoryPath + "/" + util.Config
	ContProcDirectoryPath    = ContManagerDirectoryPath + "/.host_proc"
	ContWatcherDirectoryPath = ContManagerDirectoryPath + "/" + util.Watcher
	ContDeviceRegistryPath   = ContManagerDirectoryPath + "/" + util.Registry

	VGPULockDirName     = "vgpu_lock"
	ContVGPULockPath    = "/tmp/." + VGPULockDirName
	ContVMemoryNodePath = "/tmp/." + util.VMemNode

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

	fakeDeviceUUID = "GPU-00000000-0000-0000-0000-000000000000"
)

var (
	HostManagerDirectoryPath = os.Getenv("HOST_MANAGER_DIR")
	HostPreLoadFilePath      = HostManagerDirectoryPath + "/" + LdPreLoadFileName
	HostVGPUControlFilePath  = HostManagerDirectoryPath + "/" + VGPUControlFileName
	HostWatcherDirectoryPath = HostManagerDirectoryPath + "/" + util.Watcher
	HostDeviceRegistryPath   = HostManagerDirectoryPath + "/" + util.Registry
)

func PassDeviceSpecs(devices []manager.Device, imexChannels imex.Channels) []*pluginapi.DeviceSpec {
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

func UpdateResponseForNodeConfig(response *pluginapi.ContainerAllocateResponse, devManager *manager.DeviceManager, deviceIDs ...string) {
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
func (m *vNumberDevicePlugin) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (resp *pluginapi.AllocateResponse, err error) {
	klog.V(4).InfoS("Allocate", "pluginName", m.Name(), "request", req.GetContainerRequests())
	var (
		activePods []corev1.Pod
		currentPod *corev1.Pod
		assignDevs *device.ContainerDevices
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

	nodeConfig := m.baseServer.GetDeviceManager().GetNodeConfig()
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
	deviceMap := m.baseServer.GetDeviceManager().GetGPUDeviceMap()
	memoryRatio := nodeConfig.GetDeviceMemoryScaling()
	imexChannels := m.baseServer.GetDeviceManager().GetImexChannels()
	enabledSMWatcher := m.baseServer.GetDeviceManager().GetFeatureGate().Enabled(util.SMWatcher)
	enabledClientMode := m.baseServer.GetDeviceManager().GetFeatureGate().Enabled(util.ClientMode)
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
			deviceIds   []string
			gpuDevices  []manager.Device
			deviceUuids = make([]string, vgpu.MaxDeviceCount)
			response    = &pluginapi.ContainerAllocateResponse{
				Envs: make(map[string]string),
			}
		)
		for idx := 0; idx < vgpu.MaxDeviceCount; idx++ {
			deviceUuids[idx] = fakeDeviceUUID // Fill in fake uuids for placeholder purposes
		}
		response.Envs[util.PodNameEnv] = currentPod.Name
		response.Envs[util.PodNamespaceEnv] = currentPod.Namespace
		response.Envs[util.PodUIDEnv] = string(currentPod.UID)
		response.Envs[util.ContNameEnv] = assignDevs.Name
		response.Envs[util.CudaMemoryRatioEnv] = fmt.Sprintf("%.2f", memoryRatio)
		sort.Slice(assignDevs.Devices, func(i, j int) bool {
			return assignDevs.Devices[i].Id < assignDevs.Devices[j].Id
		})
		for _, dev := range assignDevs.Devices {
			gpuDevice, exists := deviceMap[dev.Uuid]
			if !exists {
				err = fmt.Errorf("GPU device %s does not exist", dev.Uuid)
				klog.V(3).ErrorS(err, "", "pod", klog.KObj(currentPod))
				return resp, err
			}
			deviceUuids[gpuDevice.Index] = dev.Uuid
			deviceIds = append(deviceIds, dev.Uuid)
			gpuDevices = append(gpuDevices, manager.Device{GPU: &gpuDevice})
			memoryLimitEnv := fmt.Sprintf("%s_%d", util.CudaMemoryLimitEnv, gpuDevice.Index)
			response.Envs[memoryLimitEnv] = fmt.Sprintf("%dm", dev.Memory)
			if dev.Cores > 0 && dev.Cores < util.HundredCore {
				coreLimitEnv := fmt.Sprintf("%s_%d", util.CudaCoreLimitEnv, gpuDevice.Index)
				response.Envs[coreLimitEnv] = strconv.FormatInt(dev.Cores, 10)
			}
		}
		response.Envs[util.ManagerVisibleDevices] = strings.Join(deviceUuids, ",")
		UpdateResponseForNodeConfig(response, m.baseServer.GetDeviceManager(), deviceIds...)
		response.Devices = append(response.Devices, PassDeviceSpecs(gpuDevices, imexChannels)...)

		if enabledClientMode {
			// mount /etc/vgpu-manager/registry dir
			response.Mounts = append(response.Mounts, &pluginapi.Mount{
				ContainerPath: ContDeviceRegistryPath,
				HostPath:      HostDeviceRegistryPath,
				ReadOnly:      true,
			})
		} else {
			// mount /etc/vgpu-manager/.host_proc dir
			response.Mounts = append(response.Mounts, &pluginapi.Mount{
				ContainerPath: ContProcDirectoryPath,
				HostPath:      HostProcDirectoryPath,
				ReadOnly:      true,
			})
		}
		if enabledSMWatcher {
			// mount /etc/vgpu-manager/watcher dir
			response.Mounts = append(response.Mounts, &pluginapi.Mount{
				ContainerPath: ContWatcherDirectoryPath,
				HostPath:      HostWatcherDirectoryPath,
				ReadOnly:      true,
			})
		}
		// /etc/vgpu-manager/<pod-uid>_<cont-name>
		contManagerDirectory := util.GetPodContainerManagerPath(ContManagerDirectoryPath,
			currentPod.UID, assignDevs.Name)
		_ = os.MkdirAll(contManagerDirectory, 0777)
		_ = os.Chmod(contManagerDirectory, 0777)
		// /etc/vgpu-manager/<pod-uid>_<cont-name>/devices.json
		filePath := filepath.Join(contManagerDirectory, DeviceListFileName)
		jsonBytes, _ := json.Marshal(containerRequest.GetDevicesIds())
		if err = os.WriteFile(filePath, jsonBytes, 0664); err != nil {
			msg := fmt.Sprintf("writing to %s failed", DeviceListFileName)
			klog.V(3).ErrorS(err, msg, "pod", klog.KObj(currentPod))
			err = fmt.Errorf("%s: %v", msg, err)
			return resp, err
		}
		// /etc/vgpu-manager/<pod-uid>_<cont-name>/vgpu_lock
		contVGPULockPath := filepath.Join(contManagerDirectory, VGPULockDirName)
		_ = os.MkdirAll(contVGPULockPath, 0777)
		_ = os.Chmod(contVGPULockPath, 0777)

		// /etc/vgpu-manager/<pod-uid>_<cont-name>/vmem_node
		contVMemoryNodePath := filepath.Join(contManagerDirectory, util.VMemNode)
		_ = os.MkdirAll(contVMemoryNodePath, 0777)
		_ = os.Chmod(contVMemoryNodePath, 0777)

		// <host_manager_dir>/<pod-uid>_<cont-name>
		hostManagerDirectory := util.GetPodContainerManagerPath(HostManagerDirectoryPath,
			currentPod.UID, assignDevs.Name)
		// <host_manager_dir>/<pod-uid>_<cont-name>/config
		hostVGPUConfigPath := filepath.Join(hostManagerDirectory, util.Config)
		// <host_manager_dir>/<pod-uid>_<cont-name>/vgpu_lock
		hostVGPULockPath := filepath.Join(hostManagerDirectory, VGPULockDirName)
		// <host_manager_dir>/<pod-uid>_<cont-name>/vmem_node
		hostVMemNodePath := filepath.Join(hostManagerDirectory, util.VMemNode)

		response.Mounts = append(response.Mounts, &pluginapi.Mount{
			// mount libvgpu-control.so file
			ContainerPath: ContVGPUControlFilePath,
			HostPath:      HostVGPUControlFilePath,
			ReadOnly:      true,
		}, &pluginapi.Mount{ // mount vgpu.config file
			ContainerPath: ContConfigDirectoryPath,
			HostPath:      hostVGPUConfigPath,
			ReadOnly:      true,
		}, &pluginapi.Mount{ // mount vgpu_lock dir
			ContainerPath: ContVGPULockPath,
			HostPath:      hostVGPULockPath,
			ReadOnly:      false,
		}, &pluginapi.Mount{ // mount vmem_node dir
			ContainerPath: ContVMemoryNodePath,
			HostPath:      hostVMemNodePath,
			ReadOnly:      false,
		})

		if !util.VGPUControlDisabled(currentPod, assignDevs.Name) {
			//response.Envs[util.LdPreloadEnv] = ContVGPUControlFilePath
			response.Mounts = append(response.Mounts, &pluginapi.Mount{ // mount ld_preload file
				ContainerPath: ContPreLoadFilePath,
				HostPath:      HostPreLoadFilePath,
				ReadOnly:      true,
			})
		}

		podDevices := device.PodDevices{}
		if realAlloc, ok := util.HasAnnotation(currentPod, util.PodVGPURealAllocAnnotation); ok {
			_ = podDevices.UnmarshalText(realAlloc)
		}
		podDevices = append(podDevices, *assignDevs)
		var realAllocated string
		if realAllocated, err = podDevices.MarshalText(); err != nil {
			msg := "encoding the device for real allocation failed"
			klog.V(3).ErrorS(err, msg, "pod", klog.KObj(currentPod))
			err = fmt.Errorf("%s: %v", msg, err)
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

// GetPodInfoByCheckpoint find relevant pod information for devicesIDs in kubelet checkpoint
func (m *vNumberDevicePlugin) GetPodInfoByCheckpoint(ctx context.Context, devicesIDs []string) (*client.PodInfo, error) {
	klog.V(3).Infoln("Attempt to retrieve pod information from the device plugin checkpoint")
	devicePluginPath := m.baseServer.GetDeviceManager().GetNodeConfig().GetDevicePluginPath()
	checkpointData, err := checkpoint.GetDevicePluginCheckpointData(devicePluginPath)
	if err != nil {
		return nil, err
	}
	devSet := sets.NewString(devicesIDs...)
	nodeName := m.baseServer.GetDeviceManager().GetNodeConfig().GetNodeName()
	for _, entry := range checkpointData.PodDeviceEntries {
		if entry.ResourceName != util.VGPUNumberResourceName || !devSet.HasAll(entry.DeviceIDs...) {
			continue
		}
		podList := corev1.PodList{}
		if err = m.cache.List(ctx, &podList,
			client2.MatchingFields{"metadata.uid": entry.PodUID},
			client2.UnsafeDisableDeepCopyOption(true)); err != nil {
			return nil, err
		}
		for _, pod := range podList.Items {
			if pod.Spec.NodeName != nodeName ||
				util.PodIsTerminated(&pod) ||
				!util.IsVGPUResourcePod(&pod) {
				continue
			}
			return &client.PodInfo{
				PodName:       pod.Name,
				PodNamespace:  pod.Namespace,
				ContainerName: entry.ContainerName,
			}, nil
		}
		break
	}
	return nil, fmt.Errorf("pod info not found")
}

func (m *vNumberDevicePlugin) GetPodInfoByDeviceIDs(ctx context.Context, devicesIDs ...string) (*client.PodInfo, error) {
	if len(devicesIDs) == 0 {
		return nil, errors.New("deviceIDs cannot be empty")
	}
	resp, err := m.podResource.ListPodResource(ctx)
	if err != nil {
		klog.ErrorS(err, "ListPodResource failed")
		return m.GetPodInfoByCheckpoint(ctx, devicesIDs)
	}
	deviceSet := sets.NewString(devicesIDs...)
	podInfo, err := m.podResource.GetPodInfoByMatchFunc(resp, func(devices *v1alpha1.ContainerDevices) bool {
		return devices.GetResourceName() == util.VGPUNumberResourceName && deviceSet.HasAll(devices.GetDeviceIds()...)
	})
	if err != nil {
		klog.ErrorS(err, "GetPodInfoByMatchFunc failed")
		return m.GetPodInfoByCheckpoint(ctx, devicesIDs)
	}
	return podInfo, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as resetting the device before making devices available to the container.
func (m *vNumberDevicePlugin) PreStartContainer(ctx context.Context, req *pluginapi.PreStartContainerRequest) (resp *pluginapi.PreStartContainerResponse, err error) {
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
		nodeName  = m.baseServer.GetDeviceManager().GetNodeConfig().GetNodeName()
	)
	err = retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		// Node does not require timeliness, search from API server cache.
		opts := metav1.GetOptions{ResourceVersion: "0"}
		node, err = m.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, opts)
		return err
	})
	if err != nil {
		klog.ErrorS(err, "get node failed", "node", nodeName)
		return resp, err
	}
	podInfo, err := m.GetPodInfoByDeviceIDs(ctx, req.GetDevicesIds()...)
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
	contManagerDirectory := util.GetPodContainerManagerPath(ContManagerDirectoryPath,
		pod.UID, podInfo.ContainerName)
	// /etc/vgpu-manager/<pod-uid>_<cont-name>/devices.json
	devicesFilePath := filepath.Join(contManagerDirectory, DeviceListFileName)
	if deviBytes, err = os.ReadFile(devicesFilePath); err != nil {
		msg := fmt.Sprintf("failed to read %s file", DeviceListFileName)
		klog.V(3).ErrorS(err, msg, "pod", klog.KObj(pod))
		err = fmt.Errorf("%s: %v", msg, err)
		return resp, err
	}
	if err = json.Unmarshal(deviBytes, &deviceIDs); err != nil {
		msg := fmt.Sprintf("unmarshal %s failed", DeviceListFileName)
		klog.V(3).ErrorS(err, msg, "pod", klog.KObj(pod))
		err = fmt.Errorf("%s: %v", msg, err)
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
	contVGPUConfigPath := filepath.Join(contManagerDirectory, util.Config)
	_ = os.MkdirAll(contVGPUConfigPath, 0777)
	_ = os.Chmod(contVGPUConfigPath, 0777)
	// /etc/vgpu-manager/<pod-uid>_<cont-name>/config/vgpu.config
	vgpuConfigFilePath := filepath.Join(contVGPUConfigPath, VGPUConfigFileName)
	klog.V(4).Infof("Pod <%s/%s> container <%s> vgpu config path is <%s>",
		pod.Namespace, pod.Name, podInfo.ContainerName, vgpuConfigFilePath)
	realAlloc, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	realDevices := device.PodDevices{}
	if err = realDevices.UnmarshalText(realAlloc); err != nil {
		msg := "parse pod assign devices failed"
		klog.V(3).ErrorS(err, msg, "pod", klog.KObj(pod))
		err = fmt.Errorf("%s: %v", msg, err)
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
	err = vgpu.WriteVGPUConfigFile(vgpuConfigFilePath, m.baseServer.GetDeviceManager(), pod, realDevices[index], oversold, node)
	if err != nil {
		klog.V(3).ErrorS(err, "Writing vGPU config failed", "pod", klog.KObj(pod))
		return resp, err
	}
	// Extra check the size of the VGPU configuration file.
	// When a version upgrade causes a change in the configuration structure,
	// the controller can reschedule these pods that cannot be started
	if err = vgpu.CheckResourceDataSize(vgpuConfigFilePath); err != nil {
		klog.ErrorS(err, "CheckResourceDataSize failed", "filePath", vgpuConfigFilePath)
		return resp, err
	}
	return resp, nil
}

func (m *vNumberDevicePlugin) Devices() []*pluginapi.Device {
	var devices []*pluginapi.Device
	for _, gpuDevice := range m.baseServer.GetDeviceManager().GetNodeDeviceInfo() {
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
