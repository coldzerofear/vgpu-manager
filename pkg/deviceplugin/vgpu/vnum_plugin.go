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
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/imex"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/base"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/cdi"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/checkpoint"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/preempt"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	cache2 "k8s.io/client-go/tools/cache"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	"k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

type vNumberDevicePlugin struct {
	pluginapi.UnimplementedDevicePluginServer
	mutex       sync.Mutex
	baseServer  base.PluginServer
	kubeClient  *kubernetes.Clientset
	podResource *client.PodResource
	server      *registry.DeviceRegistryServerImpl
	kubeCache   cache.Cache
	podCache    cache2.MutationCache
	cdiHandler  cdi.Handler
}

var _ base.DevicePlugin = &vNumberDevicePlugin{}

// NewVNumberDevicePlugin returns an initialized vNumberDevicePlugin.
func NewVNumberDevicePlugin(resourceName, socket string, manager *manager.DeviceManager,
	kubeClient *kubernetes.Clientset, kubeCache cache.Cache, cdiHandler cdi.Handler) (base.DevicePlugin, error) {
	podInformer, err := kubeCache.GetInformer(context.TODO(), &corev1.Pod{}, cache.BlockUntilSynced(false))
	if err != nil {
		return nil, fmt.Errorf("get pod informer failed: %v", err)
	}
	podIndexer, _, err := util.NewMirrorIndexer(podInformer)
	if err != nil {
		return nil, fmt.Errorf("create pod mirror indexer failed: %v", err)
	}
	if err = podIndexer.AddIndexers(preempt.PodIndexers); err != nil {
		return nil, fmt.Errorf("add pod indexers failed: %v", err)
	}
	podCache := cache2.NewIntegerResourceVersionMutationCache(
		klog.Background(), podIndexer, podIndexer, time.Minute, true,
	)
	podResource := client.NewPodResource(client.WithCallTimeoutSecond(5))
	plugin := &vNumberDevicePlugin{
		baseServer:  base.NewBasePluginServer(resourceName, socket, manager),
		podResource: podResource,
		kubeClient:  kubeClient,
		kubeCache:   kubeCache,
		podCache:    podCache,
		cdiHandler:  cdiHandler,
	}

	fn := func(ctx context.Context, uid string) (*corev1.Pod, error) {
		return plugin.getPodByUid(ctx, uid, false)
	}
	plugin.server = registry.NewDeviceRegistryServer(ContManagerDirectoryPath, fn, nil)
	return plugin, nil
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
	featureGate := m.baseServer.GetDeviceManager().GetFeatureGate()
	return &pluginapi.DevicePluginOptions{
		PreStartRequired:                true,
		GetPreferredAllocationAvailable: featureGate.Enabled(util.HonorPreAllocatedDeviceIDs),
	}, nil
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

func defaultAllocateDeviceIDs(request *pluginapi.ContainerPreferredAllocationRequest, allocated sets.Set[string]) []string {
	allocSize := int(request.GetAllocationSize())
	mustInclude := request.GetMustIncludeDeviceIDs()
	available := request.GetAvailableDeviceIDs()
	deviceIDs := make([]string, 0, allocSize)
	for _, id := range mustInclude {
		if len(deviceIDs) == allocSize {
			break
		}
		if allocated.Has(id) {
			continue
		}
		allocated.Insert(id)
		deviceIDs = append(deviceIDs, id)
	}
	for _, id := range available {
		if len(deviceIDs) == allocSize {
			break
		}
		if allocated.Has(id) {
			continue
		}
		allocated.Insert(id)
		deviceIDs = append(deviceIDs, id)
	}

	return deviceIDs
}

func buildDefaultAllocationResponses(
	requests []*pluginapi.ContainerPreferredAllocationRequest,
) ([]*pluginapi.ContainerPreferredAllocationResponse, error) {
	resps := make([]*pluginapi.ContainerPreferredAllocationResponse, len(requests))
	allocated := sets.New[string]()

	for i, req := range requests {
		deviceIDs := defaultAllocateDeviceIDs(req, allocated)
		if len(deviceIDs) != int(req.GetAllocationSize()) {
			return nil, fmt.Errorf(
				"default preferred allocation failed for request[%d]: requested=%d allocated=%d",
				i, req.GetAllocationSize(), len(deviceIDs),
			)
		}
		resps[i] = &pluginapi.ContainerPreferredAllocationResponse{
			DeviceIDs: deviceIDs,
		}
	}
	return resps, nil
}

type preAllocContext struct {
	pod          *corev1.Pod
	claims       []*device.ContainerDeviceClaim
	availableMap []map[string][]string
}

func buildAvailableDeviceMap(availableDeviceIDs []string) map[string][]string {
	m := make(map[string][]string, len(availableDeviceIDs))
	for _, id := range availableDeviceIDs {
		uuid, _, _ := strings.Cut(id, "::")
		m[uuid] = append(m[uuid], id)
	}
	return m
}

func (m *vNumberDevicePlugin) buildPreAllocContext(
	ctx context.Context,
	requests []*pluginapi.ContainerPreferredAllocationRequest,
) (*preAllocContext, error) {
	currentPod, err := m.getCurrentPod(ctx)
	if err != nil {
		return nil, err
	}

	claims := make([]*device.ContainerDeviceClaim, len(requests))
	availableMap := make([]map[string][]string, len(requests))

	for i, req := range requests {
		claim, err := device.GetCurrentPreAllocateContainerDevice(currentPod)
		if err != nil {
			return nil, fmt.Errorf("get pre-allocate claim for request[%d]: %w", i, err)
		}

		if int(req.GetAllocationSize()) != len(claim.DeviceClaims) {
			return nil, fmt.Errorf(
				"request[%d] allocation size mismatch: requested=%d claims=%d",
				i, req.GetAllocationSize(), len(claim.DeviceClaims),
			)
		}

		if err = device.UpdatePodRealContainerDeviceClaim(currentPod, *claim); err != nil {
			return nil, fmt.Errorf("update pod real container device claim for request[%d]: %w", i, err)
		}

		claims[i] = claim
		availableMap[i] = buildAvailableDeviceMap(req.GetAvailableDeviceIDs())
	}

	return &preAllocContext{
		pod:          currentPod,
		claims:       claims,
		availableMap: availableMap,
	}, nil
}

func allocateFromClaim(
	claim *device.ContainerDeviceClaim,
	availableMap map[string][]string,
	allocated sets.Set[string],
) ([]string, error) {
	if claim == nil {
		return nil, fmt.Errorf("nil container claim")
	}

	deviceIDs := make([]string, 0, len(claim.DeviceClaims))

	for _, deviceClaim := range claim.DeviceClaims {
		candidates, ok := availableMap[deviceClaim.Uuid]
		if !ok {
			return nil, fmt.Errorf("claim uuid %q not found in available device map", deviceClaim.Uuid)
		}

		selected := ""
		for _, id := range candidates {
			if allocated.Has(id) {
				continue
			}
			selected = id
			break
		}
		if selected == "" {
			return nil, fmt.Errorf("no allocatable device left for claim uuid %q", deviceClaim.Uuid)
		}

		allocated.Insert(selected)
		deviceIDs = append(deviceIDs, selected)
	}

	return deviceIDs, nil
}

func buildPreferredAllocationResponsesFromClaims(
	requests []*pluginapi.ContainerPreferredAllocationRequest,
	preCtx *preAllocContext,
) ([]*pluginapi.ContainerPreferredAllocationResponse, error) {
	resps := make([]*pluginapi.ContainerPreferredAllocationResponse, len(requests))
	allocated := sets.New[string]()

	for i, req := range requests {
		deviceIDs, err := allocateFromClaim(preCtx.claims[i], preCtx.availableMap[i], allocated)
		if err != nil {
			return nil, fmt.Errorf("claim-based allocation failed for request[%d]: %w", i, err)
		}

		if len(deviceIDs) != int(req.GetAllocationSize()) {
			return nil, fmt.Errorf(
				"claim-based allocation size mismatch for request[%d]: requested=%d allocated=%d",
				i, req.GetAllocationSize(), len(deviceIDs),
			)
		}

		resps[i] = &pluginapi.ContainerPreferredAllocationResponse{
			DeviceIDs: deviceIDs,
		}
	}

	return resps, nil
}

// GetPreferredAllocation returns the preferred allocation from the set of devices specified in the request.
func (m *vNumberDevicePlugin) GetPreferredAllocation(ctx context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	klog.V(4).InfoS("GetPreferredAllocation", "pluginName", m.Name(), "request", req.GetContainerRequests())

	requests := req.GetContainerRequests()
	defaultResps, err := buildDefaultAllocationResponses(requests)
	if err != nil {
		return nil, err
	}
	preCtx, err := m.buildPreAllocContext(ctx, requests)
	if err != nil {
		klog.V(3).ErrorS(err, "failed to build pre-allocation context, fallback to default allocation")
		return &pluginapi.PreferredAllocationResponse{
			ContainerResponses: defaultResps,
		}, nil
	}
	claimResps, err := buildPreferredAllocationResponsesFromClaims(requests, preCtx)
	if err != nil {
		klog.V(3).ErrorS(err, "failed to build claim-based preferred allocation, fallback to default allocation",
			"pod", klog.KObj(preCtx.pod))
		return &pluginapi.PreferredAllocationResponse{
			ContainerResponses: defaultResps,
		}, nil
	}
	return &pluginapi.PreferredAllocationResponse{
		ContainerResponses: claimResps,
	}, nil
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
	HostPreLoadFilePath      = filepath.Join(HostManagerDirectoryPath, LdPreLoadFileName)
	HostVGPUControlFilePath  = fmt.Sprintf("%s.%s", filepath.Join(HostManagerDirectoryPath, VGPUControlFileName), version.Get().Version)
	HostWatcherDirectoryPath = filepath.Join(HostManagerDirectoryPath, util.Watcher)
	HostDeviceRegistryPath   = filepath.Join(HostManagerDirectoryPath, util.Registry)
)

var deviceMountOptional = map[string]bool{
	NvidiaCTLFilePath:      true,
	NvidiaUVMFilePath:      true,
	NvidiaUVMToolsFilePath: true,
	NvidiaModeSetFilePath:  true,
}

func PassDeviceSpecs(devices []manager.Device, imexChannels imex.Channels) []*pluginapi.DeviceSpec {
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
	strategies := devManager.GetNodeConfig().GetDeviceListStrategy()
	if strategies.Includes(util.DeviceListStrategyEnvvar) {
		response.Envs[deviceListEnvVar] = strings.Join(deviceIDs, ",")
		var channelIDs []string
		for _, channel := range devManager.GetImexChannels() {
			channelIDs = append(channelIDs, channel.ID)
		}
		if len(channelIDs) > 0 {
			response.Envs[imex.ImexChannelEnvVar] = strings.Join(channelIDs, ",")
		}
	}
	if strategies.Includes(util.DeviceListStrategyVolumeMounts) {
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
	if devManager.GetNodeConfig().GetGDRCopyEnabled() {
		response.Envs["NVIDIA_GDRCOPY"] = "enabled"
	}
}

// UpdateResponseForCDI injects CDI device references into the allocate response
// according to the configured CDI strategies. cdiClass is the CDI device class
// (e.g. "gpu") and deviceIDs are the device UUIDs to expose. It is a no-op when
// no CDI strategy is enabled.
func UpdateResponseForCDI(response *pluginapi.ContainerAllocateResponse,
	strategies util.DeviceListStrategies, handler cdi.Handler, cdiClass string, deviceIDs ...string) error {
	if !strategies.AnyCDIEnabled() {
		return nil
	}
	qualifiedNames := make([]string, 0, len(deviceIDs))
	for _, id := range deviceIDs {
		qualifiedNames = append(qualifiedNames, handler.QualifiedName(cdiClass, id))
	}
	if strategies.Includes(util.DeviceListStrategyCDIAnnotations) {
		annotations, err := handler.GetDeviceAnnotations(uuid.New().String(), qualifiedNames)
		if err != nil {
			return err
		}
		if response.Annotations == nil {
			response.Annotations = make(map[string]string, len(annotations))
		}
		for k, v := range annotations {
			response.Annotations[k] = v
		}
	}
	if strategies.Includes(util.DeviceListStrategyCDICRI) {
		for _, name := range qualifiedNames {
			response.CdiDevices = append(response.CdiDevices, &pluginapi.CDIDevice{Name: name})
		}
	}
	return nil
}

func (m *vNumberDevicePlugin) getCurrentPod(ctx context.Context) (*corev1.Pod, error) {
	nodeConfig := m.baseServer.GetDeviceManager().GetNodeConfig()
	pods, err := client.GetActivePodsOnNode(ctx, m.kubeClient, nodeConfig.GetNodeName())
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve the active pods of the current node: %v", err)
	}
	return util.GetCurrentPodByAllocatingPods(util.FilterAllocatingPods(pods))
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container.
func (m *vNumberDevicePlugin) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (resp *pluginapi.AllocateResponse, err error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	klog.V(4).InfoS("Allocate", "pluginName", m.Name(), "request", req.GetContainerRequests())

	var currentPod *corev1.Pod
	resp = &pluginapi.AllocateResponse{}
	// When an error occurs, return a fixed format error message
	// and patch the failed metadata allocation.
	defer func() {
		switch {
		case err == nil:
			// Last update of cache to ensure timeliness
			if currentPod != nil {
				m.podCache.Mutation(currentPod)
			}
			return
		case currentPod == nil:
			klog.V(4).ErrorS(err, util.AllocateCheckErrMsg)
		default:
			klog.V(4).ErrorS(err, util.AllocateCheckErrMsg, "pod", klog.KObj(currentPod))
			if patchErr := client.PatchPodAllocationFailed(m.kubeClient, currentPod); patchErr != nil {
				klog.ErrorS(patchErr, "Error calling PatchPodAllocationFailed", "pod", klog.KObj(currentPod))
			}
			// Last update of cache to ensure timeliness
			m.podCache.Mutation(currentPod)
		}
		err = fmt.Errorf("%s: %s", util.AllocateCheckErrMsg, err.Error())
	}()

	// Real time query of pod results from apiserver
	if currentPod, err = m.getCurrentPod(ctx); err != nil {
		return resp, err
	}
	// Update cache immediately
	m.podCache.Mutation(currentPod)

	// Retrieve the current node from the cache
	currentNode, err := m.getCurrentNode(ctx)
	if err != nil {
		return resp, err
	}

	klog.V(4).InfoS("Equipment allocation in progress", "pod", klog.KObj(currentPod), "uid", currentPod.UID)

	var contClaim *device.ContainerDeviceClaim
	responses := make([]*pluginapi.ContainerAllocateResponse, len(req.ContainerRequests))
	deviceMap := m.baseServer.GetDeviceManager().GetGPUDeviceMap()
	imexChannels := m.baseServer.GetDeviceManager().GetImexChannels()
	memoryRatio := m.baseServer.GetDeviceManager().GetNodeConfig().GetDeviceMemoryScaling()
	enabledSMWatcher := m.baseServer.GetDeviceManager().GetFeatureGate().Enabled(util.SMWatcher)
	enabledClientMode := m.baseServer.GetDeviceManager().GetFeatureGate().Enabled(util.ClientMode)

	for i, containerRequest := range req.ContainerRequests {
		contClaim, err = device.GetCurrentPreAllocateContainerDevice(currentPod)
		if err != nil {
			klog.V(3).ErrorS(err, "get pod pre-allocate device claim failed", "pod",
				klog.KObj(currentPod), "reqIndex", i, "deviceIDs", containerRequest.GetDevicesIds())
			return resp, err
		}
		if len(containerRequest.GetDevicesIds()) != len(contClaim.DeviceClaims) {
			klog.V(3).ErrorS(nil, "requested number of devices does not match", "pod",
				klog.KObj(currentPod), "container", contClaim.Name, "reqIndex", i, "deviceIDs", containerRequest.GetDevicesIds())
			return resp, fmt.Errorf("requested number of devices does not match")
		}

		klog.V(4).InfoS("Current pod allocated container devices", "pod", klog.KObj(currentPod),
			"container", contClaim.Name, "reqIndex", i, "deviceIDs", containerRequest.GetDevicesIds())

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
		response.Envs[util.ContNameEnv] = contClaim.Name
		response.Envs[util.CudaMemoryRatioEnv] = fmt.Sprintf("%.2f", memoryRatio)
		response.Envs[util.ExternalSmWatcherEnabled] = "false"
		response.Envs[util.CudaCoreLimitEnv] = ""
		response.Envs[util.CudaSoftCoreLimitEnv] = ""
		response.Envs[util.ManagerClientRegisterUuid] = ""
		mode := vgpu.GetCompatibilityMode(m.baseServer.GetDeviceManager())
		response.Envs[util.ManagerCompatibilityMode] = fmt.Sprintf("%v", mode)
		sort.Slice(contClaim.DeviceClaims, func(i, j int) bool {
			return contClaim.DeviceClaims[i].Id < contClaim.DeviceClaims[j].Id
		})
		policy := vgpu.GetDefaultComputePolicy(currentPod, currentNode)
		for _, deviceClaim := range contClaim.DeviceClaims {
			gpuDevice, exists := deviceMap[deviceClaim.Uuid]
			if !exists {
				klog.V(3).ErrorS(nil, "GPU device does not exist", "pod",
					klog.KObj(currentPod), "container", contClaim.Name, "gpuUuid", deviceClaim.Uuid)
				return resp, fmt.Errorf("GPU device %s does not exist", deviceClaim.Uuid)
			}
			deviceUuids[gpuDevice.Index] = deviceClaim.Uuid
			deviceIds = append(deviceIds, deviceClaim.Uuid)
			gpuDevices = append(gpuDevices, manager.Device{GPU: &gpuDevice})
			memoryLimitEnv := fmt.Sprintf("%s_%d", util.CudaMemoryLimitEnv, gpuDevice.Index)
			response.Envs[memoryLimitEnv] = fmt.Sprintf("%dm", deviceClaim.Memory)

			coreLimitEnv := fmt.Sprintf("%s_%d", util.CudaCoreLimitEnv, gpuDevice.Index)
			softCoreLimitEnv := fmt.Sprintf("%s_%d", util.CudaSoftCoreLimitEnv, gpuDevice.Index)
			response.Envs[coreLimitEnv] = ""
			response.Envs[softCoreLimitEnv] = ""
			switch policy {
			case util.BalanceComputePolicy:
				if deviceClaim.Cores > 0 && deviceClaim.Cores < util.HundredCore {
					response.Envs[coreLimitEnv] = strconv.FormatInt(deviceClaim.Cores, 10)
					response.Envs[softCoreLimitEnv] = fmt.Sprintf("%v", util.HundredCore)
				}
			case util.FixedComputePolicy:
				if deviceClaim.Cores > 0 && deviceClaim.Cores < util.HundredCore {
					response.Envs[coreLimitEnv] = strconv.FormatInt(deviceClaim.Cores, 10)
					response.Envs[softCoreLimitEnv] = response.Envs[coreLimitEnv]
				}
			}
		}

		response.Envs[util.ManagerVisibleDevices] = strings.Join(deviceUuids, ",")
		UpdateResponseForNodeConfig(response, m.baseServer.GetDeviceManager(), deviceIds...)
		response.Devices = append(response.Devices, PassDeviceSpecs(gpuDevices, imexChannels)...)
		if err = UpdateResponseForCDI(response, m.baseServer.GetDeviceManager().GetNodeConfig().GetDeviceListStrategy(),
			m.cdiHandler, util.CDIClass, deviceIds...); err != nil {
			klog.V(3).ErrorS(err, "failed to update allocate response for CDI", "pod",
				klog.KObj(currentPod), "container", contClaim.Name, "reqIndex", i)
			return resp, err
		}

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
			response.Envs[util.ExternalSmWatcherEnabled] = "true"
			// mount /etc/vgpu-manager/watcher dir
			response.Mounts = append(response.Mounts, &pluginapi.Mount{
				ContainerPath: ContWatcherDirectoryPath,
				HostPath:      HostWatcherDirectoryPath,
				ReadOnly:      true,
			})
		}
		// /etc/vgpu-manager/<pod-uid>_<cont-name>
		// <host_manager_dir>/<pod-uid>_<cont-name>
		contDir, hostDir := getContainerManagerPaths(currentPod.GetUID(), contClaim.Name)
		_ = util.EnsureDir(contDir, 0o777)
		devicesJsonFilePath := filepath.Join(contDir, DeviceListFileName)
		if err = writeJSONFile(devicesJsonFilePath, containerRequest.GetDevicesIds(), 0o664); err != nil {
			klog.V(3).ErrorS(err, fmt.Sprintf("write %s failed", DeviceListFileName),
				"pod", klog.KObj(currentPod), "filePath", devicesJsonFilePath, "container",
				contClaim.Name, "reqIndex", i, "deviceIDs", containerRequest.GetDevicesIds())
			return resp, fmt.Errorf("write %s failed: %w", DeviceListFileName, err)
		}

		// /etc/vgpu-manager/<pod-uid>_<cont-name>/vgpu_lock
		contVGPULockPath := filepath.Join(contDir, VGPULockDirName)
		_ = util.EnsureDir(contVGPULockPath, 0o777)

		// /etc/vgpu-manager/<pod-uid>_<cont-name>/vmem_node
		contVMemoryNodePath := filepath.Join(contDir, util.VMemNode)
		_ = util.EnsureDir(contVMemoryNodePath, 0o777)

		// <host_manager_dir>/<pod-uid>_<cont-name>/config
		hostVGPUConfigPath := filepath.Join(hostDir, util.Config)
		// <host_manager_dir>/<pod-uid>_<cont-name>/vgpu_lock
		hostVGPULockPath := filepath.Join(hostDir, VGPULockDirName)
		// <host_manager_dir>/<pod-uid>_<cont-name>/vmem_node
		hostVMemNodePath := filepath.Join(hostDir, util.VMemNode)

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

		if !util.PodContainerEnvEnabled(currentPod, contClaim.Name, util.DisableVGPUEnv) {
			//response.Envs[util.LdPreloadEnv] = ContVGPUControlFilePath
			response.Mounts = append(response.Mounts, &pluginapi.Mount{ // mount ld_preload file
				ContainerPath: ContPreLoadFilePath,
				HostPath:      HostPreLoadFilePath,
				ReadOnly:      true,
			})
		}

		configDirPath := filepath.Join(contDir, util.Config)
		configFilePath := filepath.Join(configDirPath, VGPUConfigFileName)
		// Clean up invalid configuration files that may have been written to the wrong location
		_ = os.RemoveAll(configFilePath)
		_ = util.EnsureDir(configDirPath, 0o777)
		klog.V(4).InfoS(
			"vGPU config path resolved",
			"pod", klog.KObj(currentPod),
			"container", contClaim.Name,
			"path", configFilePath,
		)

		// Attempt to write the vgpu configuration file during the Allocate phase,
		// and if unsuccessful, retry during the PreStartContainer phase
		oversold := util.PodContainerEnvEnabled(currentPod, contClaim.Name, util.CudaMemoryOversoldEnv)
		err = vgpu.WriteVGPUConfigFile(configFilePath, m.baseServer.GetDeviceManager(), currentPod, *contClaim, oversold, currentNode)
		if err != nil {
			klog.V(3).ErrorS(err, "write vGPU config failed, fallback to the PreStartContainer stage and retry",
				"pod", klog.KObj(currentPod), "container", contClaim.Name)
		}

		if err = device.UpdatePodRealContainerDeviceClaim(currentPod, *contClaim); err != nil {
			klog.V(3).ErrorS(err, "update pod real-allocate device claim failed", "pod",
				klog.KObj(currentPod), "container", contClaim.Name, "reqIndex", i, "deviceIDs", containerRequest.GetDevicesIds())
			return resp, err
		}

		responses[i] = response
	}

	resp.ContainerResponses = responses
	if patchErr := client.PatchPodAllocationSucceed(m.kubeClient, currentPod); patchErr != nil {
		klog.ErrorS(patchErr, "Error calling PatchPodAllocationSucceed", "pod", klog.KObj(currentPod))
	}
	return resp, nil
}

func (m *vNumberDevicePlugin) getPodByUid(_ context.Context, uid string, deepCopy bool) (*corev1.Pod, error) {
	objs, err := m.podCache.ByIndex(preempt.IndexerKeyPodMetadataUid, uid)
	if err != nil {
		return nil, err
	}
	if len(objs) != 1 {
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), "uid "+uid)
	}
	pod := objs[0].(*corev1.Pod)
	if deepCopy {
		return pod.DeepCopy(), nil
	}
	return pod, nil
}

// GetPodByCheckpoint find relevant pod information for devicesIDs in kubelet checkpoint
func (m *vNumberDevicePlugin) GetPodByCheckpoint(ctx context.Context, devicesIDs []string) (*corev1.Pod, string, error) {
	klog.V(3).Infoln("Attempt to retrieve pod information from the device plugin checkpoint")
	devicePluginPath := m.baseServer.GetDeviceManager().GetNodeConfig().GetDevicePluginPath()
	checkpointData, err := checkpoint.GetDevicePluginCheckpointData(devicePluginPath)
	if err != nil {
		return nil, "", err
	}
	deviceSet := sets.NewString(devicesIDs...)
	nodeName := m.baseServer.GetDeviceManager().GetNodeConfig().GetNodeName()
	for _, entry := range checkpointData.PodDeviceEntries {
		if entry.ResourceName != util.VGPUNumberResourceName ||
			deviceSet.Len() != len(entry.DeviceIDs) || !deviceSet.HasAll(entry.DeviceIDs...) {
			continue
		}
		pod, err := m.getPodByUid(ctx, entry.PodUID, false)
		if err != nil {
			return nil, "", fmt.Errorf("GetPodByCheckpoint failed: %v", err)
		}
		if pod.Spec.NodeName != nodeName || util.PodIsTerminated(pod) {
			continue
		}
		return pod, entry.ContainerName, nil
	}
	return nil, "", fmt.Errorf("pod not found")
}

func (m *vNumberDevicePlugin) GetPodByDeviceIDs(ctx context.Context, devicesIDs ...string) (*corev1.Pod, string, error) {
	if len(devicesIDs) == 0 {
		return nil, "", errors.New("deviceIDs cannot be empty")
	}
	resp, err := m.podResource.ListPodResource(ctx)
	if err != nil {
		klog.ErrorS(err, "ListPodResource failed, fallback to checkpoint")
		return m.GetPodByCheckpoint(ctx, devicesIDs)
	}
	deviceSet := sets.NewString(devicesIDs...)
	podInfo, err := m.podResource.GetPodInfoByMatchFunc(resp, func(devices *v1alpha1.ContainerDevices) bool {
		return devices.GetResourceName() == util.VGPUNumberResourceName &&
			deviceSet.Len() == len(devices.GetDeviceIds()) && deviceSet.HasAll(devices.GetDeviceIds()...)
	})
	if err != nil {
		klog.ErrorS(err, "GetPodInfoByMatchFunc failed, fallback to checkpoint")
		return m.GetPodByCheckpoint(ctx, devicesIDs)
	}
	podKey := cache2.ObjectName{Name: podInfo.PodName, Namespace: podInfo.PodNamespace}
	obj, exist, err := m.podCache.GetByKey(podKey.String())
	if err != nil {
		klog.ErrorS(err, "podCache.GetByKey failed, fallback to checkpoint")
		return m.GetPodByCheckpoint(ctx, devicesIDs)
	}
	if !exist {
		return nil, "", apierrors.NewNotFound(corev1.Resource("pods"), podKey.String())
	}
	return obj.(*corev1.Pod), podInfo.ContainerName, nil
}

func (m *vNumberDevicePlugin) getCurrentNode(ctx context.Context) (*corev1.Node, error) {
	node := &corev1.Node{}
	nodeName := m.baseServer.GetDeviceManager().GetNodeConfig().GetNodeName()
	if err := m.kubeCache.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return nil, fmt.Errorf("get node %q by cache failed: %v", nodeName, err)
	}
	return node, nil
}

func writeJSONFile(path string, v any, perm os.FileMode) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

func readJSONFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func getContainerManagerPaths(podUID types.UID, contName string) (contDir, hostDir string) {
	contDir = util.GetPodContainerManagerPath(ContManagerDirectoryPath, podUID, contName)
	hostDir = util.GetPodContainerManagerPath(HostManagerDirectoryPath, podUID, contName)
	return
}

func getRealContainerDeviceClaim(pod *corev1.Pod, containerName string) (*device.ContainerDeviceClaim, error) {
	realAlloc, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)

	var realDevices device.PodDeviceClaim
	if err := realDevices.UnmarshalText(realAlloc); err != nil {
		return nil, fmt.Errorf("parse pod assigned devices failed: %w", err)
	}

	idx := slices.IndexFunc(realDevices, func(contDevs device.ContainerDeviceClaim) bool {
		return contDevs.Name == containerName
	})
	if idx < 0 {
		return nil, fmt.Errorf("unable to find allocated devices for container %q", containerName)
	}

	return ptr.To(realDevices[idx]), nil
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

	pod, containerName, err := m.GetPodByDeviceIDs(ctx, req.GetDevicesIds()...)
	if err != nil {
		klog.ErrorS(err, "get pod by devices failed", "deviceIDs", req.GetDevicesIds())
		return resp, err
	}

	realClaim, err := getRealContainerDeviceClaim(pod, containerName)
	if err != nil {
		klog.ErrorS(err, "get container real-allocate device claim failed", klog.KObj(pod), "container", containerName)
		return resp, err
	}

	contDir, _ := getContainerManagerPaths(pod.GetUID(), containerName)
	devicesJsonFilePath := filepath.Join(contDir, DeviceListFileName)

	var allocatedDeviceIDs []string
	if err = readJSONFile(devicesJsonFilePath, &allocatedDeviceIDs); err != nil {
		klog.V(3).ErrorS(err, fmt.Sprintf("read %s failed", DeviceListFileName),
			"pod", klog.KObj(pod), "filePath", devicesJsonFilePath)
		return resp, fmt.Errorf("read %s failed: %w", DeviceListFileName, err)
	}
	// Verify if there are any errors in the allocation of container equipment.
	if !sets.NewString(allocatedDeviceIDs...).Equal(sets.NewString(req.GetDevicesIds()...)) {
		klog.ErrorS(nil, "inconsistent allocation results of container equipment", "pod", klog.KObj(pod),
			"container", containerName, "reqDeviceIDs", req.GetDevicesIds(), "allocatedDeviceIDs", allocatedDeviceIDs)
		return resp, fmt.Errorf("inconsistent allocation results of container equipment")
	}

	// Retrieve the current node from the cache
	node, err := m.getCurrentNode(ctx)
	if err != nil {
		klog.ErrorS(err, "get current node failed")
		return resp, err
	}

	configDirPath := filepath.Join(contDir, util.Config)
	_ = util.EnsureDir(configDirPath, 0o777)
	configFilePath := filepath.Join(configDirPath, VGPUConfigFileName)
	klog.V(4).InfoS("vGPU config path resolved",
		"pod", klog.KObj(pod),
		"container", containerName,
		"path", configFilePath,
	)

	oversold := util.PodContainerEnvEnabled(pod, containerName, util.CudaMemoryOversoldEnv)
	err = vgpu.WriteVGPUConfigFile(configFilePath, m.baseServer.GetDeviceManager(), pod, *realClaim, oversold, node)
	if err != nil {
		klog.V(3).ErrorS(err, "write vGPU config failed", "pod", klog.KObj(pod), "container", containerName)
		return resp, fmt.Errorf("write vGPU config failed: %w", err)
	}

	if m.baseServer.GetDeviceManager().GetFeatureGate().Enabled(util.Reschedule) {
		// Extra check the size of the vGPU configuration file.
		// When a version upgrade causes a change in the configuration structure,
		// the controller can reschedule these pods that cannot be started
		if err = vgpu.CheckResourceDataSize(configFilePath); err != nil {
			klog.ErrorS(err, "check resource data size failed", "pod",
				klog.KObj(pod), "container", containerName, "filePath", configFilePath)
			return resp, fmt.Errorf("check resource data size failed: %w", err)
		}
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
			health := pluginapi.Healthy
			if !gpuDevice.Healthy {
				health = pluginapi.Unhealthy
			}
			devices = append(devices, &pluginapi.Device{
				ID:       fmt.Sprintf("%s::%d", gpuDevice.Uuid, i),
				Health:   health,
				Topology: topologyInfo,
			})
		}
	}
	return devices
}
