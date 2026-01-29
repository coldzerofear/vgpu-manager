package collector

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/mig"
	"github.com/coldzerofear/vgpu-manager/pkg/metrics/lister"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/util/cgroup"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
)

// nodeGPUCollector implements the Collector interface.
type nodeGPUCollector struct {
	*nvidia.DeviceLib
	nodeName    string
	nodeLister  listerv1.NodeLister
	podLister   listerv1.PodLister
	contLister  *lister.ContainerLister
	podResource *client.PodResource
	featureGate featuregate.FeatureGate
}

func NewNodeGPUCollector(nodeName string, nodeLister listerv1.NodeLister, podLister listerv1.PodLister,
	contLister *lister.ContainerLister, featureGate featuregate.FeatureGate) (prometheus.Collector, error) {
	deviceLib, err := nvidia.InitDeviceLib("/")
	if err != nil {
		return nil, err
	}
	return &nodeGPUCollector{
		DeviceLib:   deviceLib,
		nodeName:    nodeName,
		nodeLister:  nodeLister,
		podLister:   podLister,
		contLister:  contLister,
		featureGate: featureGate,
		podResource: client.NewPodResource(
			client.WithCallTimeoutSecond(5)),
	}, nil
}

// Descriptors used by the nodeGPUCollector below.
var (
	physicalGPUTotalMemory = prometheus.NewDesc(
		"physical_gpu_device_total_memory_in_bytes",
		"Physical GPU device total memory (bytes)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "pci_bus_id", "minor_num", "mig_enabled", "capability", "numa_node"}, nil,
	)
	physicalGPUMemoryUsage = prometheus.NewDesc(
		"physical_gpu_device_memory_usage_in_bytes",
		"Physical GPU device memory usage (bytes)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "pci_bus_id", "minor_num", "mig_enabled", "capability", "numa_node"}, nil,
	)
	physicalGPUMemoryUtilRate = prometheus.NewDesc(
		"physical_gpu_device_memory_utilization_percent",
		"Physical GPU device memory utilization percentage (0-100)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "pci_bus_id", "minor_num", "mig_enabled", "capability", "numa_node"}, nil,
	)
	physicalGPUCoreUtilRate = prometheus.NewDesc(
		"physical_gpu_device_core_utilization_percent",
		"Physical GPU device core utilization percentage (0-100)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "pci_bus_id", "minor_num", "mig_enabled", "capability", "numa_node"}, nil,
	)
	physicalGPUHealthStatus = prometheus.NewDesc(
		"physical_gpu_device_health_status",
		"Physical GPU device health status (1 for healthy, 0 for unhealthy)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "pci_bus_id", "minor_num", "mig_enabled", "capability", "numa_node"}, nil,
	)

	nodeGPUConfigInfo = prometheus.NewDesc(
		"node_gpu_device_configuration_info",
		"Configuration information of GPU devices node",
		[]string{"node", "device_split", "cores_scaling", "memory_scaling", "memory_factor"}, nil,
	)
	nodeGPUDriverVersionInfo = prometheus.NewDesc(
		"node_gpu_device_driver_version_info",
		"Driver version information for GPU devices node",
		[]string{"node", "driver_version", "cuda_version", "nvml_version"}, nil,
	)

	nodeVGPUTotalMemory = prometheus.NewDesc(
		"node_vgpu_device_total_memory_in_bytes",
		"Node virtual GPU devices total memory (sum of physical GPU memory + unified memory)",
		[]string{"node"}, nil,
	)
	nodeVGPUTotalPhysicalMemory = prometheus.NewDesc(
		"node_vgpu_device_total_physical_memory_in_bytes",
		"Node virtual GPU devices total physical memory (sum of physical GPU memory)",
		[]string{"node"}, nil,
	)
	nodeVGPUAssignedMemory = prometheus.NewDesc(
		"node_vgpu_device_assigned_memory_in_bytes",
		"Node virtual GPU devices assigned memory (sum of physical GPU memory + unified memory)",
		[]string{"node"}, nil,
	)
	nodeVGPUAssignedPhysicalMemory = prometheus.NewDesc(
		"node_vgpu_device_assigned_physical_memory_in_bytes",
		"Node virtual GPU devices assigned physical memory (sum of physical GPU memory)",
		[]string{"node"}, nil,
	)

	vGPUTotalCoresNumber = prometheus.NewDesc(
		"vgpu_device_total_cores_number",
		"Virtual GPU device total cores number",
		[]string{"node", "device_idx", "device_uuid", "device_type"}, nil,
	)
	vGPUAssignedCoresNumber = prometheus.NewDesc(
		"vgpu_device_assigned_cores_number",
		"Virtual GPU device assigned cores number",
		[]string{"node", "device_idx", "device_uuid", "device_type"}, nil,
	)
	vGPUSharedContainersNumber = prometheus.NewDesc(
		"vgpu_device_shared_containers_number",
		"Virtual GPU device shared containers number",
		[]string{"node", "device_idx", "device_uuid", "device_type"}, nil,
	)
	vGPUTotalMemory = prometheus.NewDesc(
		"vgpu_device_total_memory_in_bytes",
		"Virtual GPU device total memory (sum of physical GPU memory + unified memory)",
		[]string{"node", "device_idx", "device_uuid", "device_type"}, nil,
	)
	vGPUTotalPhysicalMemory = prometheus.NewDesc(
		"vgpu_device_total_physical_memory_in_bytes",
		"Virtual GPU device total physical memory (only physical GPU memory)",
		[]string{"node", "device_idx", "device_uuid", "device_type"}, nil,
	)
	vGPUAssignedMemory = prometheus.NewDesc(
		"vgpu_device_assigned_memory_in_bytes",
		"Virtual GPU device assigned memory (sum of physical GPU memory + unified memory)",
		[]string{"node", "device_idx", "device_uuid", "device_type"}, nil,
	)
	vGPUAssignedPhysicalMemory = prometheus.NewDesc(
		"vgpu_device_assigned_physical_memory_in_bytes",
		"Virtual GPU device assigned physical memory (only physical GPU memory)",
		[]string{"node", "device_idx", "device_uuid", "device_type"}, nil,
	)

	containerVGPUMemoryLimit = prometheus.NewDesc(
		"container_vgpu_device_memory_limit_in_bytes",
		"Container's virtual GPU device total memory limit (sum of physical GPU memory + unified memory)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node"}, nil,
	)
	containerVGPUPhysicalMemoryLimit = prometheus.NewDesc(
		"container_vgpu_device_physical_memory_limit_in_bytes",
		"Container's virtual GPU device physical memory limit (only physical GPU memory)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node"}, nil,
	)

	containerVGPUMemoryUsage = prometheus.NewDesc(
		"container_vgpu_device_memory_usage_in_bytes",
		"Container's virtual GPU device memory usage (sum of physical GPU memory + unified memory)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node"}, nil,
	)
	containerVGPUPhysicalMemoryUsage = prometheus.NewDesc(
		"container_vgpu_device_physical_memory_usage_in_bytes",
		"Container's virtual GPU device physical memory usage (only physical GPU memory)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node"}, nil,
	)

	containerVGPUMemoryUtilRate = prometheus.NewDesc(
		"container_vgpu_device_memory_utilization_percent",
		"Container's virtual GPU device memory utilization percentage (0-100)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node"}, nil,
	)
	containerVGPUCoreUtilRate = prometheus.NewDesc(
		"container_vgpu_device_core_utilization_percent",
		"Container's virtual GPU device core utilization percentage (0-100)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node"}, nil,
	)

	migDeviceTotalMemory = prometheus.NewDesc(
		"mig_device_total_memory_in_bytes",
		"MIG device total memory (bytes)",
		[]string{"node", "device_idx", "device_uuid", "parent_uuid", "ci_id", "gi_id", "profile"}, nil,
	)
	migDeviceMemoryUsage = prometheus.NewDesc(
		"mig_device_memory_usage_in_bytes",
		"MIG device memory usage (bytes)",
		[]string{"node", "device_idx", "device_uuid", "parent_uuid", "ci_id", "gi_id", "profile"}, nil,
	)
	migDeviceMemoryUtilRate = prometheus.NewDesc(
		"mig_device_memory_utilization_percent",
		"MIG device memory utilization percentage (0-100)",
		[]string{"node", "device_idx", "device_uuid", "parent_uuid", "ci_id", "gi_id", "profile"}, nil,
	)
	containerMIGAllocationInfo = prometheus.NewDesc(
		"container_mig_device_allocation_info",
		"Container's MIG device allocation information",
		[]string{"node", "device_idx", "device_uuid", "parent_uuid", "pod_namespace", "pod_name", "container_name"}, nil,
	)
)

// Describe is implemented with DescribeByCollect. That's possible because the
// Collect method will always return the same two metrics with the same two
// descriptors.
func (c nodeGPUCollector) Describe(ch chan<- *prometheus.Desc) {
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
	ch <- vGPUSharedContainersNumber
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
	ch <- migDeviceTotalMemory
	ch <- migDeviceMemoryUsage
	ch <- migDeviceMemoryUtilRate
	ch <- containerMIGAllocationInfo
}

type procInfoList map[uint32]nvml.ProcessInfo_v1

type procUtilList map[uint32]nvml.ProcessUtilizationSample

func ContainerDeviceProcInfoEach(procInfos procInfoList,
	containerPids []uint32, fn func(nvml.ProcessInfo_v1)) {
	if procInfos == nil || fn == nil {
		return
	}
	for _, contPid := range containerPids {
		if process, ok := procInfos[contPid]; ok {
			fn(process)
		}
	}
}

func ContainerDeviceProcUtilEach(procUtils procUtilList,
	containerPids []uint32, fn func(nvml.ProcessUtilizationSample)) {
	if procUtils == nil || fn == nil {
		return
	}
	for _, contPid := range containerPids {
		if process, ok := procUtils[contPid]; ok {
			fn(process)
		}
	}
}

var smFilePath = filepath.Join(util.ManagerRootPath, util.Watcher, util.SMUtilFile)

// Collect device indicators
func (c nodeGPUCollector) Collect(ch chan<- prometheus.Metric) {
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
		deviceUtil     *watcher.DeviceUtil
	)
	err := c.NvmlInit()
	if err != nil {
		klog.Errorln(err)
		goto skipNvml
	}
	defer func() {
		c.NvmlShutdown()
		_ = deviceUtil.Munmap(false)
	}()

	func() {
		driverVersion, ret := c.SystemGetDriverVersion()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting driver version: %s", nvml.ErrorString(ret))
			driverVersion = "N/A"
		}
		cudaVersion := ""
		version, ret := c.SystemGetCudaDriverVersion()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting CUDA driver version: %s", nvml.ErrorString(ret))
			cudaVersion = "N/A"
		} else {
			cudaVersion = strconv.Itoa(version)
		}
		nvmlVersion, ret := c.SystemGetNVMLVersion()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting NVML driver version: %s", nvml.ErrorString(ret))
			nvmlVersion = "N/A"
		}
		ch <- prometheus.MustNewConstMetric(
			nodeGPUDriverVersionInfo,
			prometheus.GaugeValue,
			float64(1),
			c.nodeName, driverVersion, cudaVersion, nvmlVersion)
	}()

	if c.featureGate.Enabled(util.SMWatcher) {
		if deviceUtil, err = watcher.NewDeviceUtil(smFilePath); err != nil {
			klog.V(3).ErrorS(err, "Failed to read manager SM util file")
		}
	}

	err = c.VisitDevices(func(index int, hdev nvdev.Device) error {
		gpuInfo, err := c.GetGpuInfo(index, hdev)
		if err != nil {
			klog.Errorf("error getting info for GPU %d: %v", index, err)
			return nil
		}
		devHealthMap[gpuInfo.UUID]++
		devIndexMap[gpuInfo.UUID] = index
		devTypeMap[gpuInfo.UUID] = gpuInfo.ProductName
		devMemInfoMap[gpuInfo.UUID] = gpuInfo.Memory
		busId := links.PciInfo(gpuInfo.PciInfo).BusID()
		migEnabled := fmt.Sprint(gpuInfo.MigEnabled)
		var numaNode string
		if numa := links.PciInfo(gpuInfo.PciInfo).NumaNode(); numa >= 0 {
			numaNode = strconv.Itoa(int(numa))
		}
		deviceIndex := strconv.Itoa(index)
		minorNumber := strconv.Itoa(gpuInfo.Minor)
		devHealthLvs[gpuInfo.UUID] = []string{
			c.nodeName, deviceIndex, gpuInfo.UUID, gpuInfo.ProductName, busId,
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

		migInfos, err := c.GetMigInfos(gpuInfo)
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

		CollectorDeviceProcesses(deviceUtil, index, hdev, devProcInfoMap, devProcUtilMap)
		return nil
	})
	if err != nil {
		klog.Errorln(err.Error())
	}

skipNvml:
	var (
		//vGpuHealthMap      = make(map[string]bool)
		vGpuTotalMemMap    = make(map[string]uint64)
		vGpuAssignedMemMap = make(map[string]uint64)
		vGPUTotalCoresMap  = make(map[string]int64)
		vGPUTotalNumberMap = make(map[string]int)
	)
	// Get current node.
	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		klog.Errorf("node lister get node <%s> error: %v", c.nodeName, err)
		return
	}

	nodeVGPUTotalMemBytes, nodeGPUTotalMemBytes := uint64(0), uint64(0)
	registryNode, _ := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
	nodeDevInfo, _ := device.ParseNodeDeviceInfo(registryNode)
	for _, devInfo := range nodeDevInfo {
		// Label unhealthy devices.
		if _, ok := devHealthMap[devInfo.Uuid]; !ok || !devInfo.Healthy {
			devHealthMap[devInfo.Uuid] = 0
		}
		// Skip the statistics of MIG device.
		if devInfo.Mig {
			continue
		}
		vGPUTotalCoresMap[devInfo.Uuid] = devInfo.Core
		vGPUTotalNumberMap[devInfo.Uuid] = devInfo.Number
		//vGpuHealthMap[devInfo.Uuid] = devInfo.Healthy
		vGpuTotalMemBytes := uint64(devInfo.Memory) << 20
		vGpuTotalMemMap[devInfo.Uuid] = vGpuTotalMemBytes
		nodeVGPUTotalMemBytes += vGpuTotalMemBytes
		if memory, exists := devMemInfoMap[devInfo.Uuid]; exists {
			nodeGPUTotalMemBytes += memory.Total
		} else {
			nodeGPUTotalMemBytes += vGpuTotalMemBytes
		}
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

	configInfoStr, _ := util.HasAnnotation(node, util.NodeConfigInfoAnnotation)
	nodeConfigInfo := device.NodeConfigInfo{}
	if err = nodeConfigInfo.Decode(configInfoStr); err == nil {
		ch <- prometheus.MustNewConstMetric(
			nodeGPUConfigInfo,
			prometheus.GaugeValue,
			float64(1), c.nodeName,
			strconv.Itoa(nodeConfigInfo.DeviceSplit),
			strconv.FormatFloat(nodeConfigInfo.CoresScaling, 'f', 2, 64),
			strconv.FormatFloat(nodeConfigInfo.MemoryScaling, 'f', 2, 64),
			strconv.Itoa(nodeConfigInfo.MemoryFactor))
	}

	// Get all pods.
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("pod lister list error: %v", err)
		return
	}
	nodeVGpuAssignedMemBytes := uint64(0)
	vGpuAssignedCoresMap := make(map[string]int64)
	vGpuAssignedNumberMap := make(map[string]int)
	sharedContainersMap := make(map[string]int)
	// Filter out some useless pods.
	util.PodsOnNodeCallback(pods, node, func(pod *corev1.Pod) {
		// Aggregate the allocated memory size on the node.
		podDeviceClaim := device.GetPodDeviceClaim(pod)
		devContainersMap := make(map[string]sets.Set[string])
		FlattenDevicesEach(podDeviceClaim, func(ctrName string, claimDevice device.DeviceClaim) {
			if ctrNameSet, ok := devContainersMap[claimDevice.Uuid]; ok {
				ctrNameSet.Insert(ctrName)
			} else {
				devContainersMap[claimDevice.Uuid] = sets.New[string](ctrName)
			}
			vGpuAssignedNumberMap[claimDevice.Uuid]++
			vGpuAssignedCoresMap[claimDevice.Uuid] += claimDevice.Cores
			memoryBytes := uint64(claimDevice.Memory) << 20
			nodeVGpuAssignedMemBytes += memoryBytes
			vGpuAssignedMemMap[claimDevice.Uuid] += memoryBytes
		})
		for uuid, crtNameSet := range devContainersMap {
			sharedContainersMap[uuid] += crtNameSet.Len()
		}
		for _, container := range pod.Spec.Containers {
			contKey := lister.GetContainerKey(pod.UID, container.Name)
			resData, exist := c.contLister.GetResourceDataT(contKey)
			if !exist {
				continue
			}

			klog.V(4).Infoln("Container matching: using resource data", "ContainerName", container.Name)
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
			//_, containerId := cgroup.GetContainerRuntime(pod, container.Name)

			deviceCount := 0
			for i := int32(0); i < vgpu.MaxDeviceCount; i++ {
				containerDevice := resData.Devices[i]
				if containerDevice.Activate == 0 {
					continue
				}
				deviceUUID := string(containerDevice.UUID[0:40])
				vHostIndex, exists := devIndexMap[deviceUUID]
				if !exists {
					continue
				}

				var (
					deviceMemLimit  = containerDevice.TotalMemory
					realMemBytes    = containerDevice.RealMemory
					vDevIndex       = strconv.Itoa(deviceCount)
					deviceMemUsage  = uint64(0)
					deviceVMemUsage = uint64(0)
					deviceSMUtil    = uint32(0)
					contGPUPids     []string
				)
				deviceCount++

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

				// TODO handler Virtual Memory Cache node.
				if c.featureGate.Enabled(util.VMemoryNode) {
					// Calculate virtual memory, if any.
					func() {
						// TODO Prevent gpu task from exiting unexpectedly, and fail to clean up the virtual cache in time.
						if len(contGPUPids) == 0 {
							return
						}
						vMemory, exists := c.contLister.GetResourceVMem(contKey)
						if !exists {
							return
						}
						if err = vMemory.RLock(vHostIndex); err != nil {
							klog.V(3).ErrorS(err, "virtual memory RLock failed", "vHostIndex", vHostIndex)
							return
						}
						defer func() { _ = vMemory.Unlock(vHostIndex) }()
						for index := uint32(0); index < vMemory.GetVMem().Devices[vHostIndex].ProcessesSize; index++ {
							deviceVMemUsage += vMemory.GetVMem().Devices[vHostIndex].Processes[index].Used
						}
					}()
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
				if deviceMemUsage >= deviceMemLimit {
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
	for uuid, totalMemoryBytes := range vGpuTotalMemMap {
		totalPhyMemoryBytes := totalMemoryBytes
		if memory, exists := devMemInfoMap[uuid]; exists {
			totalPhyMemoryBytes = memory.Total
		}
		memoryRatio := float64(totalMemoryBytes) / float64(totalPhyMemoryBytes)
		deviceIndex := strconv.Itoa(devIndexMap[uuid])
		//healthy := fmt.Sprint(vGpuHealthMap[uuid])
		ch <- prometheus.MustNewConstMetric(
			vGPUTotalMemory,
			prometheus.GaugeValue,
			float64(totalMemoryBytes), c.nodeName,
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

		//ch <- prometheus.MustNewConstMetric(
		//	virtGPUTotalSplitsNumber,
		//	prometheus.GaugeValue,
		//	float64(vGPUTotalNumberMap[uuid]),
		//	c.nodeName, deviceIndex, uuid,
		//	devTypeMap[uuid])
		//ch <- prometheus.MustNewConstMetric(
		//	virtGPUAssignedSplitsNum,
		//	prometheus.GaugeValue,
		//	float64(vGpuAssignedNumberMap[uuid]),
		//	c.nodeName, deviceIndex, uuid,
		//	devTypeMap[uuid])

		ch <- prometheus.MustNewConstMetric(
			vGPUTotalCoresNumber,
			prometheus.GaugeValue,
			float64(vGPUTotalCoresMap[uuid]),
			c.nodeName, deviceIndex, uuid,
			devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(
			vGPUAssignedCoresNumber,
			prometheus.GaugeValue,
			float64(vGpuAssignedCoresMap[uuid]),
			c.nodeName, deviceIndex, uuid,
			devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(
			vGPUSharedContainersNumber,
			prometheus.GaugeValue,
			float64(sharedContainersMap[uuid]),
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

	var (
		listResourceOnce        sync.Once
		podResourcesResp        *v1alpha1.ListPodResourcesResponse
		listMigPodResourcesFunc = func() *v1alpha1.ListPodResourcesResponse {
			listResourceOnce.Do(func() {
				resource, err := c.podResource.ListPodResource(context.Background(),
					func(devices *v1alpha1.ContainerDevices) bool {
						return len(devices.GetDeviceIds()) > 0 &&
							strings.HasPrefix(devices.GetResourceName(), util.MIGDeviceResourceNamePrefix)
					})
				if err != nil {
					klog.ErrorS(err, "ListPodResource failed")
				} else {
					podResourcesResp = resource
				}
			})
			return podResourcesResp
		}
	)

	FlattenMigInfosMapEach(devMigInfosMap, func(parentUUID string, migInfo *nvidia.MigInfo) {
		migIdx := strconv.Itoa(migInfo.Index)
		ciId := fmt.Sprintf("%d", migInfo.CiInfo.Id)
		giId := fmt.Sprintf("%d", migInfo.GiInfo.Id)
		//isHealthy := fmt.Sprint(vGpuHealthMap[parentUUID])
		podResourcesResp = listMigPodResourcesFunc()
		podInfoP, _ := c.podResource.GetPodInfoByMatchFunc(podResourcesResp, func(devices *v1alpha1.ContainerDevices) bool {
			return devices.GetResourceName() == mig.GetMigResourceName(migInfo) &&
				slices.Contains(devices.GetDeviceIds(), migInfo.UUID)
		})
		if podInfoP != nil {
			ch <- prometheus.MustNewConstMetric(
				containerMIGAllocationInfo,
				prometheus.GaugeValue,
				float64(1),
				c.nodeName, migIdx, migInfo.UUID, parentUUID,
				podInfoP.PodNamespace, podInfoP.PodName, podInfoP.ContainerName)
		}
		ch <- prometheus.MustNewConstMetric(
			migDeviceTotalMemory,
			prometheus.GaugeValue,
			float64(migInfo.Memory.Total),
			c.nodeName, migIdx, migInfo.UUID,
			parentUUID, ciId, giId, migInfo.Profile)
		ch <- prometheus.MustNewConstMetric(
			migDeviceMemoryUsage,
			prometheus.GaugeValue,
			float64(migInfo.Memory.Used),
			c.nodeName, migIdx, migInfo.UUID,
			parentUUID, ciId, giId, migInfo.Profile)
		memoryUtilRate := int64(0)
		if migInfo.Memory.Total > 0 {
			memoryUtilRate = int64(float64(migInfo.Memory.Used) / float64(migInfo.Memory.Total) * 100)
			if memoryUtilRate > 100 {
				memoryUtilRate = 100
			}
		}
		ch <- prometheus.MustNewConstMetric(
			migDeviceMemoryUtilRate,
			prometheus.GaugeValue,
			float64(memoryUtilRate),
			c.nodeName, migIdx, migInfo.UUID,
			parentUUID, ciId, giId, migInfo.Profile)
	})

}

func CollectorDeviceProcesses(deviceUtil *watcher.DeviceUtil, index int, hdev nvml.Device, devProcInfoMap map[string]procInfoList, devProcUtilMap map[string]procUtilList) {
	uuid, rt := hdev.GetUUID()
	if rt != nvml.SUCCESS {
		err := fmt.Errorf("error getting pci info for device %d: %v", index, rt)
		klog.ErrorS(err, "Skip the device collection process")
		return
	}
	// Aggregate GPU processes.
	var (
		processInfos              []nvml.ProcessInfo
		processUtilizationSamples []nvml.ProcessUtilizationSample
	)

	nvmlProcessInfoFunc := func() {
		// In MIG mode, if device handle is provided, the API returns aggregate information, only if the caller has appropriate privileges.
		// Per-instance information can be queried by using specific MIG device handles.
		// Querying per-instance information using MIG device handles is not supported if the device is in vGPU Host virtualization mode.
		if procs, rt := hdev.GetGraphicsRunningProcesses(); rt == nvml.SUCCESS {
			processInfos = append(processInfos, procs...)
		}
		if procs, rt := hdev.GetComputeRunningProcesses(); rt == nvml.SUCCESS {
			processInfos = append(processInfos, procs...)
		}
	}

	if deviceUtil != nil {
		deviceUtilWrap := deviceUtil.GetWrap()
		klog.V(4).InfoS("collector device processes from sm watcher", "device", index)
		if err := deviceUtilWrap.RLock(index); err == nil {
			micro := time.UnixMicro(int64(deviceUtilWrap.GetUtil().Devices[index].LastSeenTimeStamp))
			if time.Now().Sub(micro) > 5*time.Second {
				_ = deviceUtilWrap.Unlock(index)
				klog.V(3).InfoS("Process utilization time window timeout detected, rollback using nvml driver to obtain utilization", "device", index)
				nvmlProcessInfoFunc()
				goto nvmlProcessUtil
			}
			if deviceUtilWrap.GetUtil().Devices[index].ComputeProcessesSize > 0 {
				processInfos = append(processInfos, deviceUtilWrap.GetUtil().Devices[index].ComputeProcesses[:deviceUtilWrap.GetUtil().Devices[index].ComputeProcessesSize]...)
			}
			if deviceUtilWrap.GetUtil().Devices[index].GraphicsProcessesSize > 0 {
				processInfos = append(processInfos, deviceUtilWrap.GetUtil().Devices[index].GraphicsProcesses[:deviceUtilWrap.GetUtil().Devices[index].GraphicsProcessesSize]...)
			}
			if len(processInfos) == 0 {
				nvmlProcessInfoFunc()
			}
			if deviceUtilWrap.GetUtil().Devices[index].ProcessUtilSamplesSize > 0 {
				processUtilizationSamples = append(processUtilizationSamples, deviceUtilWrap.GetUtil().Devices[index].ProcessUtilSamples[:deviceUtilWrap.GetUtil().Devices[index].ProcessUtilSamplesSize]...)
			}
			_ = deviceUtilWrap.Unlock(index)
			goto collecProcessInfo
		} else {
			klog.V(3).ErrorS(err, "SM Watcher lock failed, fallback to nvml driver call", "device", index)
		}
	}

	nvmlProcessInfoFunc()

nvmlProcessUtil:
	// On MIG-enabled GPUs, querying process utilization is not currently supported.
	processUtilizationSamples, rt = hdev.GetProcessUtilization(uint64(time.Now().Add(-1 * time.Second).UnixMicro()))
	if rt != nvml.SUCCESS {
		klog.V(4).Infof("error getting process utilization for device %d: %s", index, nvml.ErrorString(rt))
		processUtilizationSamples = nil
	}

collecProcessInfo:
	processInfoList := make(procInfoList, len(processInfos))
	for _, processInfo := range processInfos {
		procInfo, ok := processInfoList[processInfo.Pid]
		if ok {
			procInfo.UsedGpuMemory += processInfo.UsedGpuMemory
		} else {
			procInfo = nvml.ProcessInfo_v1{
				Pid:           processInfo.Pid,
				UsedGpuMemory: processInfo.UsedGpuMemory,
			}
		}
		processInfoList[processInfo.Pid] = procInfo
	}
	devProcInfoMap[uuid] = processInfoList

	processUtilList := make(procUtilList, len(processUtilizationSamples))
	for _, procUtilSample := range processUtilizationSamples {
		processUtilList[procUtilSample.Pid] = procUtilSample
	}
	devProcUtilMap[uuid] = processUtilList
}

func FlattenMigInfosMapEach(migInfosMap map[string][]*nvidia.MigInfo,
	fn func(parentUuid string, mig *nvidia.MigInfo)) {
	if fn == nil {
		return
	}
	for parentUUID, migInfos := range migInfosMap {
		for _, migInfo := range migInfos {
			if migInfo == nil {
				continue
			}
			fn(parentUUID, migInfo)
		}
	}
}

func FlattenDevicesEach(podDeviceClaim device.PodDeviceClaim,
	fn func(ctrName string, claim device.DeviceClaim)) {
	if fn == nil {
		return
	}
	for _, containerClaim := range podDeviceClaim {
		for _, claim := range containerClaim.DeviceClaims {
			fn(containerClaim.Name, claim)
		}
	}
}
