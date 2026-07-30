package collector

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/util/cgroup"
	"github.com/opencontainers/cgroups"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	resourcev1 "k8s.io/client-go/listers/resource/v1"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

// draGPUCollector implements the Collector interface.
type draGPUCollector struct {
	*nvidia.DeviceLib
	nodeName    string
	podLister   client.PodLister
	sliceLister resourcev1.ResourceSliceLister
	claimLister resourcev1.ResourceClaimLister
	podResource *client.PodResource
	utilAdapter watcher.DeviceUtilInterface
	featureGate featuregate.FeatureGate
}

func NewDRAGPUCollector(
	config *node.NodeConfigSpec, podLister client.PodLister,
	sliceLister resourcev1.ResourceSliceLister, claimLister resourcev1.ResourceClaimLister,
	featureGate featuregate.FeatureGate,
) (prometheus.Collector, error) {
	driverRoot := config.GetDriverRoot()
	deviceLib, err := nvidia.InitDeviceLib(driverRoot)
	if err != nil {
		return nil, err
	}
	adapter := watcher.NewDeviceUtilAdapter(
		watcher.WithExtendedInterface(deviceLib.Extensions()),
	)
	return &draGPUCollector{
		DeviceLib:   deviceLib,
		nodeName:    config.GetNodeName(),
		podLister:   podLister,
		sliceLister: sliceLister,
		claimLister: claimLister,
		featureGate: featureGate,
		utilAdapter: adapter,
		podResource: client.NewPodResource(
			client.WithCallTimeoutSecond(5)),
	}, nil
}

// Describe is implemented with DescribeByCollect. That's possible because the
// Collect method will always return the same two metrics with the same two
// descriptors.
func (c draGPUCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- physicalGPUTotalMemory
	ch <- physicalGPUMemoryUsage
	ch <- physicalGPUMemoryUtilRate
	ch <- physicalGPUCoreUtilRate
	ch <- physicalGPUHealthStatus
	ch <- nodeGPUConfigInfo
	ch <- nodeGPUDriverVersionInfo
	ch <- nodeVGPUTotalMemory
	ch <- nodeVGPUTotalPhysicalMemory
	ch <- nodeVGPUAssignedMemory
	ch <- nodeVGPUAssignedPhysicalMemory
	ch <- vGPUTotalCoresNumber
	ch <- vGPUAssignedCoresNumber
	ch <- vGPUPeakSharedContainersNumber
	ch <- vGPUCurrentSharedContainersNumber
	ch <- vGPUTotalMemory
	ch <- vGPUTotalPhysicalMemory
	ch <- vGPUAssignedMemory
	ch <- vGPUAssignedPhysicalMemory
	ch <- containerVGPUMemoryUsage
	ch <- containerVGPUPhysicalMemoryUsage
	ch <- containerVGPUMemoryLimit
	ch <- containerVGPUPhysicalMemoryLimit
	ch <- containerVGPUMemoryUtilRate
	ch <- containerVGPUCoreUtilRate
	//ch <- migDeviceTotalMemory
	//ch <- migDeviceMemoryUsage
	//ch <- migDeviceMemoryUtilRate
	//ch <- containerMIGAllocationInfo
}

func (c draGPUCollector) getManagerResourceSlice() (*v1.ResourceSlice, error) {
	sliceList, err := c.sliceLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, slice := range sliceList {
		if slice.Spec.NodeName != nil && *slice.Spec.NodeName != c.nodeName {
			continue
		}
		if slice.Spec.Driver == util.DRADriverName {
			return slice.DeepCopy(), nil
		}
	}
	return nil, apierrors.NewNotFound(v1.Resource("resourceslices"), util.DRADriverName)
}

func CollectBasedOnNvml(
	ch chan<- prometheus.Metric, lib *nvidia.DeviceLib, nodeName string,
	devTypeMap map[string]string, devIndexMap map[string]int, devHealthMap map[string]int,
	devHealthLvs map[string][]string, devMemInfoMap map[string]nvml.Memory,
	devProcInfoMap map[string]procInfoList, devProcUtilMap map[string]procUtilList,
	devMigInfosMap map[string][]*nvidia.MigInfo,
	utilAdapter watcher.DeviceUtilInterface, featureGate featuregate.FeatureGate,
) {
	err := lib.NvmlInit()
	if err != nil {
		klog.Errorln(err)
		return
	}
	var deviceUtil *watcher.MmapDeviceUtil
	defer func() {
		lib.NvmlShutdown()
		if deviceUtil != nil {
			_ = deviceUtil.Close()
		}
	}()

	func() {
		driverVersion, ret := lib.SystemGetDriverVersion()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting driver version: %s", nvml.ErrorString(ret))
			driverVersion = "N/A"
		}
		cudaVersion := ""
		version, ret := lib.SystemGetCudaDriverVersion()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting CUDA driver version: %s", nvml.ErrorString(ret))
			cudaVersion = "N/A"
		} else {
			cudaVersion = strconv.Itoa(version)
		}
		nvmlVersion, ret := lib.SystemGetNVMLVersion()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting NVML driver version: %s", nvml.ErrorString(ret))
			nvmlVersion = "N/A"
		}
		ch <- prometheus.MustNewConstMetric(
			nodeGPUDriverVersionInfo,
			prometheus.GaugeValue,
			float64(1),
			nodeName, driverVersion, cudaVersion, nvmlVersion)
	}()

	if featureGate.Enabled(util.SMWatcher) {
		if deviceUtil, err = watcher.NewMmapDeviceUtil(smFilePath); err != nil && !os.IsNotExist(err) {
			klog.V(3).ErrorS(err, "Failed to read manager SM util file")
		}
	}

	err = lib.VisitDevices(func(index int, hdev nvdev.Device) error {
		gpuInfo, err := lib.GetGpuInfo(index, hdev)
		if err != nil {
			klog.Errorf("error getting info for GPU %d: %v", index, err)
			return nil
		}
		devHealthMap[gpuInfo.UUID]++
		devIndexMap[gpuInfo.UUID] = index
		devTypeMap[gpuInfo.UUID] = gpuInfo.ProductName
		devMemInfoMap[gpuInfo.UUID] = gpuInfo.Memory
		migEnabled := fmt.Sprint(gpuInfo.MigEnabled)

		var numaNode string
		if numa := gpuInfo.GetNumaNode(); numa >= 0 {
			numaNode = strconv.Itoa(int(numa))
		}
		deviceIndex := strconv.Itoa(index)
		minorNumber := strconv.Itoa(gpuInfo.Minor)
		devHealthLvs[gpuInfo.UUID] = []string{
			nodeName, deviceIndex, gpuInfo.UUID, gpuInfo.ProductName, gpuInfo.PciBusID,
			minorNumber, migEnabled, gpuInfo.CudaComputeCapability, numaNode,
		}

		ch <- prometheus.MustNewConstMetric(
			physicalGPUTotalMemory,
			prometheus.GaugeValue,
			float64(gpuInfo.Memory.Total),
			devHealthLvs[gpuInfo.UUID]...)

		ch <- prometheus.MustNewConstMetric(
			physicalGPUMemoryUsage,
			prometheus.GaugeValue,
			float64(gpuInfo.Memory.Used),
			devHealthLvs[gpuInfo.UUID]...)

		memoryUtilRate := int64(0)
		if gpuInfo.Memory.Total > 0 {
			memoryUtilRate = int64(float64(gpuInfo.Memory.Used) / float64(gpuInfo.Memory.Total) * 100)
		}
		ch <- prometheus.MustNewConstMetric(
			physicalGPUMemoryUtilRate,
			prometheus.GaugeValue,
			float64(memoryUtilRate),
			devHealthLvs[gpuInfo.UUID]...)

		migInfos, err := lib.GetMigInfos(gpuInfo)
		if err != nil {
			klog.Errorf("error getting MIG infos for GPU %d: %v", index, err)
		}
		if len(migInfos) > 0 {
			devMigInfosMap[gpuInfo.UUID] = maps.Values[map[string]*nvidia.MigInfo](migInfos)
		}

		// Skip unsupported operations after enabling MIG.
		if gpuInfo.MigEnabled {
			return nil
		}

		// On MIG-enabled GPUs, querying device utilization rates is not currently supported.
		deviceUtilRates, rt := hdev.GetUtilizationRates()
		if rt != nvml.SUCCESS {
			klog.Errorf("error getting utilization rates for device %d: %s", index, nvml.ErrorString(rt))
		} else {
			ch <- prometheus.MustNewConstMetric(
				physicalGPUCoreUtilRate,
				prometheus.GaugeValue,
				float64(deviceUtilRates.Gpu),
				devHealthLvs[gpuInfo.UUID]...)
		}

		CollectorDeviceProcesses(utilAdapter, deviceUtil, index, hdev, devProcInfoMap, devProcUtilMap)
		return nil
	})
	if err != nil {
		klog.Errorln(err.Error())
	}
}

type DRADeviceInfo struct {
	name        string
	devType     string
	uuid        string
	coreRatio   int64
	memoryRatio int64
	cores       int64
	memory      int64
	healthy     bool
}

// Collect device indicators
func (c draGPUCollector) Collect(ch chan<- prometheus.Metric) {
	klog.V(4).Infof("Starting to collect metrics for vGPU on node <%s>", c.nodeName)
	var (
		devTypeMap     = make(map[string]string)
		devIndexMap    = make(map[string]int)
		devHealthMap   = make(map[string]int)
		devHealthLvs   = make(map[string][]string)
		devMemInfoMap  = make(map[string]nvml.Memory)
		devProcInfoMap = make(map[string]procInfoList)
		devProcUtilMap = make(map[string]procUtilList)
		devMigInfosMap = make(map[string][]*nvidia.MigInfo)
	)

	CollectBasedOnNvml(ch, c.DeviceLib, c.nodeName, devTypeMap, devIndexMap,
		devHealthMap, devHealthLvs, devMemInfoMap, devProcInfoMap, devProcUtilMap,
		devMigInfosMap, c.utilAdapter, c.featureGate)

	// Retrieve the vGPU resourceSlice of the current node
	resourceSlice, err := c.getManagerResourceSlice()
	if err != nil {
		klog.Errorf("resourceSlice get node %q error: %v", c.nodeName, err)
		return
	}

	var (
		coreRatio             = int64(util.HundredCore)
		memoryRatio           = int64(util.HundredCore)
		nodeGPUTotalMemBytes  uint64
		nodeVGPUTotalMemBytes uint64
		//vGpuHealthMap      = make(map[string]bool)
		//vGpuTotalMemMap    = make(map[string]uint64)
		vGpuAssignedMemMap = make(map[string]uint64)
		//vGPUTotalCoresMap  = make(map[string]int64)
		//vGPUTotalNumberMap = make(map[string]int)
		attribute      v1.DeviceAttribute
		devInfoUuidMap = make(map[string]*DRADeviceInfo, len(resourceSlice.Spec.Devices))
		devInfoNameMap = make(map[string]*DRADeviceInfo, len(resourceSlice.Spec.Devices))
	)

	for _, dev := range resourceSlice.Spec.Devices {
		if attribute = dev.Attributes["type"]; attribute.StringValue == nil ||
			(*attribute.StringValue != kubeletplugin.VGpuDeviceType && *attribute.StringValue != kubeletplugin.GpuDeviceType) {
			continue
		}
		devInfo := &DRADeviceInfo{
			name:        dev.Name,
			devType:     *attribute.StringValue,
			coreRatio:   util.HundredCore,
			memoryRatio: util.HundredCore,
			cores:       util.HundredCore,
		}
		if attribute = dev.Attributes["uuid"]; attribute.StringValue != nil {
			devInfo.uuid = *attribute.StringValue
			if idx := strings.Index(devInfo.uuid, "-"); idx >= 0 {
				devInfo.uuid = strings.ToUpper(devInfo.uuid[:idx]) + devInfo.uuid[idx:]
			}
		}
		if attribute = dev.Attributes["coreRatio"]; attribute.IntValue != nil {
			coreRatio = *attribute.IntValue
			devInfo.coreRatio = *attribute.IntValue
		}
		if attribute = dev.Attributes["memoryRatio"]; attribute.IntValue != nil {
			memoryRatio = *attribute.IntValue
			devInfo.memoryRatio = *attribute.IntValue
		}
		if devInfo.healthy = kubeletplugin.IsHealthy(dev.Taints); !devInfo.healthy {
			devHealthMap[devInfo.uuid] = 0
		}
		if dev.Capacity == nil {
			dev.Capacity = map[v1.QualifiedName]v1.DeviceCapacity{}
		}
		capacity := dev.Capacity[kubeletplugin.CoresResourceName]
		if val, ok := capacity.Value.AsInt64(); ok && val >= 0 {
			devInfo.cores = val
		}
		capacity = dev.Capacity[kubeletplugin.MemoryResourceName]
		if val, ok := capacity.Value.AsInt64(); ok && val >= 0 {
			devInfo.memory = val
			if devInfo.devType == kubeletplugin.VGpuDeviceType {
				nodeVGPUTotalMemBytes += uint64(val)
			}
		}
		if memory, exists := devMemInfoMap[devInfo.uuid]; exists {
			nodeGPUTotalMemBytes += memory.Total
		} else {
			nodeGPUTotalMemBytes += uint64(devInfo.memory)
		}
		devInfoUuidMap[devInfo.uuid] = devInfo
		devInfoNameMap[devInfo.name] = devInfo
	}

	for uuid, status := range devHealthMap {
		ch <- prometheus.MustNewConstMetric(
			physicalGPUHealthStatus,
			prometheus.GaugeValue,
			float64(status),
			devHealthLvs[uuid]...)
	}
	ch <- prometheus.MustNewConstMetric(
		nodeVGPUTotalMemory,
		prometheus.GaugeValue,
		float64(nodeVGPUTotalMemBytes),
		c.nodeName,
	)
	ch <- prometheus.MustNewConstMetric(
		nodeVGPUTotalPhysicalMemory,
		prometheus.GaugeValue,
		float64(nodeGPUTotalMemBytes),
		c.nodeName,
	)

	ch <- prometheus.MustNewConstMetric(
		nodeGPUConfigInfo,
		prometheus.GaugeValue,
		float64(1), c.nodeName, "",
		strconv.FormatFloat(float64(coreRatio)/float64(util.HundredCore), 'f', 2, 64),
		strconv.FormatFloat(float64(memoryRatio)/float64(util.HundredCore), 'f', 2, 64),
		"")

	// Get all pods on the current node
	pods, err := c.podLister.ListByIndexValue(metrics.IndexerKeyPodNodeName, c.nodeName)
	if err != nil {
		klog.Errorf("pod lister list error: %v", err)
		return
	}

	nodeVGpuAssignedMemBytes := uint64(0)
	vGpuAssignedCoresMap := make(map[string]int64)
	vGpuAssignedNumberMap := make(map[string]int)
	peakSharedContainersMap := make(map[string]int)
	currentSharedContainersMap := make(map[string]int)

	// Filter out some useless pods.
	util.PodsOnNodeCallback(pods, &corev1.Node{ObjectMeta: v12.ObjectMeta{Name: c.nodeName}}, func(pod *corev1.Pod) {
		if pod.Spec.NodeName != c.nodeName || util.HasDRARequests(pod) {
			return
		}
		requestContainersMap := make(map[string][]string)
		containerRefMap := make(map[string]util.ContainerRef, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
		for _, containerRef := range util.GetAllPodContainers(pod) {
			containerRefMap[containerRef.Name] = containerRef
			for _, claimRef := range containerRef.Container.Resources.Claims {
				key := claimRef.Name + "/" + claimRef.Request
				requestContainersMap[key] = append(requestContainersMap[key], containerRef.Name)
				requestContainersMap[key+"/"] = append(requestContainersMap[key+"/"], containerRef.Name)
			}
		}

		containerAllocResults := make(map[string][]v1.DeviceRequestAllocationResult, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
		// Collect actual cited resource claims
		for _, status := range pod.Status.ResourceClaimStatuses {
			if status.ResourceClaimName == nil {
				continue
			}
			claim, err := c.claimLister.ResourceClaims(pod.Namespace).Get(*status.ResourceClaimName)
			if err != nil {
				klog.V(4).ErrorS(err, "get resourceClaim failed", "resourceClaim",
					fmt.Sprintf("%s/%s", pod.Namespace, *status.ResourceClaimName))
				continue
			}
			// Filter resource claims actually referenced by current pod
			if !slices.ContainsFunc(claim.Status.ReservedFor, func(r v1.ResourceClaimConsumerReference) bool {
				return r.UID == pod.GetUID()
			}) {
				continue
			}
			if claim.Status.Allocation == nil {
				continue
			}
			// Filter out the allocation of DRA devices for each container
			for _, result := range claim.Status.Allocation.Devices.Results {
				if result.Driver != util.DRADriverName {
					continue
				}
				mainRequest, _, _ := strings.Cut(result.Request, "/")
				exactKey := status.Name + "/" + mainRequest + "/"
				for _, containerName := range requestContainersMap[exactKey] {
					containerAllocResults[containerName] = append(containerAllocResults[containerName], result)
				}
				wildcardKey := status.Name + "/"
				for _, containerName := range requestContainersMap[wildcardKey] {
					containerAllocResults[containerName] = append(containerAllocResults[containerName], result)
				}
			}
		}

		if pod.Status.ExtendedResourceClaimStatus != nil {
			claimName := pod.Status.ExtendedResourceClaimStatus.ResourceClaimName
			if claim, err := c.claimLister.ResourceClaims(pod.Namespace).Get(claimName); err == nil && claim.Status.Allocation != nil {
				for _, mapping := range pod.Status.ExtendedResourceClaimStatus.RequestMappings {
					if index := slices.IndexFunc(claim.Status.Allocation.Devices.Results, func(r v1.DeviceRequestAllocationResult) bool {
						return r.Request == mapping.RequestName
					}); index >= 0 {
						result := claim.Status.Allocation.Devices.Results[index]
						containerAllocResults[mapping.ContainerName] = append(containerAllocResults[mapping.ContainerName], result)
					}
				}
			} else if err != nil {
				klog.V(4).ErrorS(err, "get resourceClaim failed", "resourceClaim", fmt.Sprintf("%s/%s", pod.Namespace, claimName))
			}
		}

		for containerName, requestAllocationResult := range containerAllocResults {
			containerRef := containerRefMap[containerName]
			switch {
			case containerRef.Kind == util.ContainerKindInit && containerRef.Restartable:

			case containerRef.Kind == util.ContainerKindApp:

			}
			uuidNumber := make(map[string]int)
			for _, result := range requestAllocationResult {
				devInfo, ok := devInfoNameMap[result.Device]
				if !ok {
					continue
				}
				uuidNumber[devInfo.uuid]++
				vGpuAssignedNumberMap[devInfo.uuid]++
				vGpuAssignedCoresMap[devInfo.uuid] += devInfo.cores
				nodeVGpuAssignedMemBytes += uint64(devInfo.memory)
				vGpuAssignedMemMap[devInfo.uuid] += uint64(devInfo.memory)
			}
			running := util.IsContainerRunning(pod, containerName)
			for uuid, number := range uuidNumber {
				// Peak (reserved) concurrent sharing: per-GPU lifecycle peak count.
				currentSharedContainersMap[uuid] += number
				if running {
					currentSharedContainersMap[uuid] += number
				}
			}
		}

		// Real-time per-container usage: regular containers, sidecars, and
		// currently-running sequential init containers (a completed init
		// container is excluded so its stale usage stops being reported).
		for _, container := range util.CollectableContainers(pod) {
			results, ok := containerAllocResults[container.Name]
			if !ok || len(results) == 0 {
				continue
			}

			klog.V(4).Infoln("Container matching: using resource data", "pod", klog.KObj(pod), "container", container.Name)

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
			var containerPids []uint32
			_ = cgroup.GetContainerPidsFunc(pod, container.Name, getFullPath, func(pid int) {
				containerPids = append(containerPids, uint32(pid))
			})

			deviceCount := 0

			sort.Slice(results, func(i, j int) bool { return results[i].Device < results[j].Device })
			for _, result := range results {
				devInfo, ok := devInfoNameMap[result.Device]
				if !ok {
					continue
				}
				deviceUUID := devInfo.uuid
				//devHostIndex, exists := devIndexMap[deviceUUID]
				//if !exists {
				//	continue
				//}
				var (
					deviceMemLimit  = devInfo.memory
					realMemBytes    = devInfo.memory
					vDevIndex       = strconv.Itoa(deviceCount)
					deviceMemUsage  = uint64(0)
					deviceVMemUsage = uint64(0)
					deviceSMUtil    = uint32(0)
					contGPUPids     []string
				)
				deviceCount++

				if devInfo.devType == kubeletplugin.VGpuDeviceType {
					quantity := result.ConsumedCapacity[kubeletplugin.MemoryResourceName]
					if val, ok := quantity.AsInt64(); ok && val >= 0 {
						deviceMemLimit = val
					}
					if ratio := float64(devInfo.memoryRatio) / float64(util.HundredCore); ratio > 1 {
						realMemBytes = int64(float64(deviceMemLimit) / ratio)
					}
				}

				ContainerDeviceProcInfoEach(devProcInfoMap[deviceUUID], containerPids,
					func(process nvml.ProcessInfo_v1) {
						contGPUPids = append(contGPUPids, strconv.Itoa(int(process.Pid)))
						deviceMemUsage += process.UsedGpuMemory
					})
				ContainerDeviceProcUtilEach(devProcUtilMap[deviceUUID], containerPids,
					func(sample nvml.ProcessUtilizationSample) {
						smUtil := util.GetValidValue(sample.SmUtil)
						codecUtil := util.GetValidValue(sample.EncUtil) +
							util.GetValidValue(sample.DecUtil)
						codecUtil = util.CodecNormalize(codecUtil)
						deviceSMUtil += smUtil + codecUtil
					})

				ch <- prometheus.MustNewConstMetric(
					containerVGPUMemoryLimit,
					prometheus.GaugeValue,
					float64(deviceMemLimit),
					pod.Namespace, pod.Name, container.Name,
					vDevIndex, deviceUUID, c.nodeName)
				ch <- prometheus.MustNewConstMetric(
					containerVGPUPhysicalMemoryLimit,
					prometheus.GaugeValue,
					float64(realMemBytes),
					pod.Namespace, pod.Name, container.Name,
					vDevIndex, deviceUUID, c.nodeName)

				// TODO Unable to track virtual memory usage temporarily
				if c.featureGate.Enabled(util.VMemoryNode) {
					// Once there is a suitable plan in the future, it will be implemented deviceVMemUsage
				}

				ch <- prometheus.MustNewConstMetric(
					containerVGPUMemoryUsage,
					prometheus.GaugeValue,
					float64(deviceMemUsage+deviceVMemUsage),
					pod.Namespace, pod.Name, container.Name,
					vDevIndex, deviceUUID, c.nodeName)
				ch <- prometheus.MustNewConstMetric(
					containerVGPUPhysicalMemoryUsage,
					prometheus.GaugeValue,
					float64(deviceMemUsage),
					pod.Namespace, pod.Name, container.Name,
					vDevIndex, deviceUUID, c.nodeName)

				deviceMemUsage += deviceVMemUsage
				memoryUtilRate := int64(0)
				if deviceMemUsage >= uint64(deviceMemLimit) {
					memoryUtilRate = 100
				} else if deviceMemLimit > 0 {
					memoryUtilRate = int64(float64(deviceMemUsage) / float64(deviceMemLimit) * 100)
				}
				ch <- prometheus.MustNewConstMetric(
					containerVGPUMemoryUtilRate,
					prometheus.GaugeValue,
					float64(memoryUtilRate),
					pod.Namespace, pod.Name, container.Name,
					vDevIndex, deviceUUID, c.nodeName)
				ch <- prometheus.MustNewConstMetric(
					containerVGPUCoreUtilRate,
					prometheus.GaugeValue,
					float64(util.GetPercentageValue(deviceSMUtil)),
					pod.Namespace, pod.Name, container.Name,
					vDevIndex, deviceUUID, c.nodeName)
			}
		}
	})

	nodeGpuAssignedMemoryBytes := uint64(0)
	//devMemRatioMap := make(map[string]float64, len(vGpuTotalMemMap))
	for uuid, devInfo := range devInfoUuidMap {
		totalPhyMemoryBytes := devInfo.memory
		memoryRatio := float64(devInfo.memoryRatio) / util.HundredCore
		if memoryRatio > 1 {
			if memory, exists := devMemInfoMap[uuid]; exists {
				totalPhyMemoryBytes = int64(memory.Total)
			} else {
				totalPhyMemoryBytes = int64(float64(devInfo.memory) / memoryRatio)
			}
		}

		deviceIndex := strconv.Itoa(devIndexMap[uuid])
		//healthy := fmt.Sprint(vGpuHealthMap[uuid])
		ch <- prometheus.MustNewConstMetric(
			vGPUTotalMemory,
			prometheus.GaugeValue,
			float64(devInfo.memory), c.nodeName,
			deviceIndex, uuid, devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(
			vGPUTotalPhysicalMemory,
			prometheus.GaugeValue,
			float64(totalPhyMemoryBytes), c.nodeName,
			deviceIndex, uuid, devTypeMap[uuid])

		assignedPhyMemoryBytes := vGpuAssignedMemMap[uuid]
		if memoryRatio > 1 {
			assignedPhyMemoryBytes = uint64(float64(assignedPhyMemoryBytes) / memoryRatio)
		}
		nodeGpuAssignedMemoryBytes += assignedPhyMemoryBytes
		ch <- prometheus.MustNewConstMetric(
			vGPUAssignedMemory,
			prometheus.GaugeValue,
			float64(vGpuAssignedMemMap[uuid]), c.nodeName,
			deviceIndex, uuid, devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(
			vGPUAssignedPhysicalMemory,
			prometheus.GaugeValue,
			float64(assignedPhyMemoryBytes), c.nodeName,
			deviceIndex, uuid, devTypeMap[uuid])

		ch <- prometheus.MustNewConstMetric(
			vGPUTotalCoresNumber,
			prometheus.GaugeValue,
			float64(devInfo.cores),
			c.nodeName, deviceIndex, uuid,
			devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(
			vGPUAssignedCoresNumber,
			prometheus.GaugeValue,
			float64(vGpuAssignedCoresMap[uuid]),
			c.nodeName, deviceIndex, uuid,
			devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(
			vGPUPeakSharedContainersNumber,
			prometheus.GaugeValue,
			float64(peakSharedContainersMap[uuid]),
			c.nodeName, deviceIndex, uuid,
			devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(
			vGPUCurrentSharedContainersNumber,
			prometheus.GaugeValue,
			float64(currentSharedContainersMap[uuid]),
			c.nodeName, deviceIndex, uuid,
			devTypeMap[uuid])
	}

	ch <- prometheus.MustNewConstMetric(
		nodeVGPUAssignedMemory,
		prometheus.GaugeValue,
		float64(nodeVGpuAssignedMemBytes),
		c.nodeName,
	)
	ch <- prometheus.MustNewConstMetric(
		nodeVGPUAssignedPhysicalMemory,
		prometheus.GaugeValue,
		float64(nodeGpuAssignedMemoryBytes),
		c.nodeName,
	)

}
