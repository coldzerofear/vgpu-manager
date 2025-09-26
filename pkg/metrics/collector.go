package metrics

import (
	"fmt"
	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
	"k8s.io/component-base/featuregate"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
)

type CollectorService interface {
	prometheus.Collector
	Registry() *prometheus.Registry
}

// nodeGPUCollector implements the Collector interface.
type nodeGPUCollector struct {
	*nvidia.DeviceLib
	nodeName    string
	nodeLister  listerv1.NodeLister
	podLister   listerv1.PodLister
	contLister  *ContainerLister
	podResource *client.PodResource
	featureGate featuregate.FeatureGate
}

var _ CollectorService = &nodeGPUCollector{}

func NewNodeGPUCollector(nodeName string, nodeLister listerv1.NodeLister, podLister listerv1.PodLister,
	contLister *ContainerLister, featureGate featuregate.FeatureGate) (CollectorService, error) {
	deviceLib, err := nvidia.NewDeviceLib("/")
	if err != nil {
		klog.Error("If this is a GPU node, did you configure the NVIDIA Container Toolkit?")
		klog.Error("You can check the prerequisites at: https://github.com/NVIDIA/k8s-device-plugin#prerequisites")
		klog.Error("You can learn how to set the runtime at: https://github.com/NVIDIA/k8s-device-plugin#quick-start")
		klog.Error("If this is not a GPU node, you should set up a toleration or nodeSelector to only deploy this plugin on GPU nodes")
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

// Registry return to Prometheus registry.
func (c *nodeGPUCollector) Registry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	labels := prometheus.Labels{"zone": "vGPU"}
	prometheus.WrapRegistererWith(labels, registry).MustRegister(c)
	return registry
}

// Descriptors used by the nodeGPUCollector below.
var (
	physicalGPUTotalMemory = prometheus.NewDesc(
		"physical_gpu_device_total_memory_in_bytes",
		"Physical GPU device total memory (bytes)",
		[]string{"nodename", "deviceidx", "deviceuuid", "devicetype", "pcibusid", "minornum", "migenabled", "capability"}, nil,
	)
	physicalGPUMemoryUsage = prometheus.NewDesc(
		"physical_gpu_device_memory_usage_in_bytes",
		"Physical GPU device memory usage (bytes)",
		[]string{"nodename", "deviceidx", "deviceuuid", "devicetype", "pcibusid", "minornum", "migenabled", "capability"}, nil,
	)
	physicalGPUMemoryUtilRate = prometheus.NewDesc(
		"physical_gpu_device_memory_utilization_rate",
		"Physical GPU device memory utilization rate (percentage)",
		[]string{"nodename", "deviceidx", "deviceuuid", "devicetype", "pcibusid", "minornum", "migenabled", "capability"}, nil,
	)
	physicalGPUCoreUtilRate = prometheus.NewDesc(
		"physical_gpu_device_core_utilization_rate",
		"Physical GPU device core utilization rate (percentage)",
		[]string{"nodename", "deviceidx", "deviceuuid", "devicetype", "pcibusid", "minornum", "migenabled", "capability"}, nil,
	)

	nodeGPUDriverVersionInfo = prometheus.NewDesc(
		"node_gpu_driver_version_info",
		"Driver information for GPU node",
		[]string{"nodename", "driverversion", "cudaversion", "nvmlversion"}, nil,
	)
	nodePhysicalGPUCount = prometheus.NewDesc(
		"node_physical_gpu_total_count",
		"Total count of physical GPUs on the node",
		[]string{"nodename"}, nil,
	)
	nodeVGPUTotalMemory = prometheus.NewDesc(
		"node_vgpu_total_memory_in_bytes",
		"Node virtual GPU total memory (bytes)",
		[]string{"nodename"}, nil,
	)
	nodeVGPUAssignedMemory = prometheus.NewDesc(
		"node_vgpu_assigned_memory_in_bytes",
		"Node virtual GPU assigned memory (bytes)",
		[]string{"nodename"}, nil,
	)

	virtGPUTotalMemory = prometheus.NewDesc(
		"vgpu_device_total_memory_in_bytes",
		"Virtual GPU device total memory (bytes)",
		[]string{"nodename", "deviceidx", "deviceuuid", "phymembytes", "healthy"}, nil,
	)
	virtGPUAssignedMemory = prometheus.NewDesc(
		"vgpu_device_assigned_memory_in_bytes",
		"Virtual GPU device assigned memory (bytes)",
		[]string{"nodename", "deviceidx", "deviceuuid", "phymembytes", "healthy"}, nil,
	)

	containerVGPUMemoryLimit = prometheus.NewDesc(
		"container_vgpu_device_memory_limit_in_bytes",
		"Container virtual GPU device memory limit (bytes)",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid", "ctrid", "ctrpids", "nodename", "phymembytes"}, nil,
	)
	containerVGPUMemoryUsage = prometheus.NewDesc(
		"container_vgpu_device_memory_usage_in_bytes",
		"Container virtual GPU device memory usage (bytes)",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid", "ctrid", "ctrpids", "nodename"}, nil,
	)
	containerVGPUMemoryUtilRate = prometheus.NewDesc(
		"container_vgpu_device_memory_utilization_rate",
		"Container virtual GPU device memory utilization rate (percentage)",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid", "ctrid", "ctrpids", "nodename"}, nil,
	)
	containerVGPUCoreUtilRate = prometheus.NewDesc(
		"container_vgpu_device_core_utilization_rate",
		"Container virtual GPU device core utilization rate (percentage)",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid", "ctrid", "ctrpids", "nodename"}, nil,
	)

	migDeviceTotalMemory = prometheus.NewDesc(
		"mig_device_total_memory_in_bytes",
		"MIG device total memory (bytes)",
		[]string{"nodename", "deviceidx", "deviceuuid", "parentuuid", "ciid", "giid", "profile", "healthy", "podnamespace", "podname", "ctrname"}, nil,
	)
	migDeviceMemoryUsage = prometheus.NewDesc(
		"mig_device_memory_usage_in_bytes",
		"MIG device memory usage (bytes)",
		[]string{"nodename", "deviceidx", "deviceuuid", "parentuuid", "ciid", "giid", "profile", "healthy", "podnamespace", "podname", "ctrname"}, nil,
	)
	migDeviceMemoryUtilRate = prometheus.NewDesc(
		"mig_device_memory_utilization_rate",
		"MIG device memory utilization rate (percentage)",
		[]string{"nodename", "deviceidx", "deviceuuid", "parentuuid", "ciid", "giid", "profile", "healthy", "podnamespace", "podname", "ctrname"}, nil,
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
	ch <- nodeGPUDriverVersionInfo
	ch <- nodePhysicalGPUCount
	ch <- nodeVGPUTotalMemory
	ch <- nodeVGPUAssignedMemory
	ch <- virtGPUTotalMemory
	ch <- virtGPUAssignedMemory
	ch <- containerVGPUMemoryUsage
	ch <- containerVGPUMemoryLimit
	ch <- containerVGPUMemoryUtilRate
	ch <- containerVGPUCoreUtilRate
	ch <- migDeviceTotalMemory
	ch <- migDeviceMemoryUsage
	ch <- migDeviceMemoryUtilRate
}

type procInfoList map[uint32]nvml.ProcessInfo_v1

type procUtilList map[uint32]nvml.ProcessUtilizationSample

func ContainerPidsFunc(pod *corev1.Pod, containerName string, fullPath func(string) string, f func(pid int)) {
	cgroupFullPath, err := util.GetK8sPodContainerCGroupFullPath(pod, containerName, fullPath)
	if err != nil {
		klog.Errorln(err)
		return
	}
	klog.V(3).InfoS("Detected pod container cgroup path", "pod",
		klog.KObj(pod), "container", containerName, "cgroupPath", cgroupFullPath)
	pids, err := cgroups.GetAllPids(cgroupFullPath)
	if err != nil {
		klog.ErrorS(err, "Failed to retrieve container pids",
			"pod", klog.KObj(pod), "container", containerName)
		return
	}
	klog.V(4).Infof("Pod <%s/%s> container <%s>  CGroup path <%s> pids: %+v",
		pod.Namespace, pod.Name, containerName, cgroupFullPath, pids)
	for _, pid := range pids {
		f(pid)
	}
}

func ContainerDeviceProcInfoFunc(procInfos procInfoList,
	containerPids []uint32, f func(nvml.ProcessInfo_v1)) {
	if procInfos == nil {
		return
	}
	for _, contPid := range containerPids {
		if process, ok := procInfos[contPid]; ok {
			f(process)
		}
	}
}

func ContainerDeviceProcUtilFunc(procUtils procUtilList,
	containerPids []uint32, f func(nvml.ProcessUtilizationSample)) {
	if procUtils == nil {
		return
	}
	for _, contPid := range containerPids {
		if process, ok := procUtils[contPid]; ok {
			f(process)
		}
	}
}

var smFilePath = filepath.Join(util.ManagerRootPath, util.Watcher, util.SMUtilFile)

// Collect device indicators
func (c nodeGPUCollector) Collect(ch chan<- prometheus.Metric) {
	klog.V(4).Infof("Starting to collect metrics for vGPU on node <%s>", c.nodeName)
	var (
		devIndexMap    = make(map[string]int)
		devMemInfoMap  = make(map[string]nvml.Memory)
		devProcInfoMap = make(map[string]procInfoList)
		devProcUtilMap = make(map[string]procUtilList)
		devMigInfosMap = make(map[string][]*nvidia.MigInfo)

		deviceUtil     *watcher.DeviceUtilT
		deviceUtilData []byte
	)
	err := c.NvmlInit()
	if err != nil {
		klog.Errorln(err)
		goto skipNvml
	}
	defer func() {
		c.NvmlShutdown()
		if deviceUtil != nil && deviceUtilData != nil {
			_ = syscall.Munmap(deviceUtilData)
		}
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

		count, ret := c.DeviceGetCount()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting device count: %s", nvml.ErrorString(ret))
		} else {
			ch <- prometheus.MustNewConstMetric(
				nodePhysicalGPUCount,
				prometheus.GaugeValue,
				float64(count),
				c.nodeName)
		}
	}()

	if c.featureGate.Enabled(util.SMWatcher) {
		deviceUtil, deviceUtilData, err = watcher.MmapDeviceUtilT(smFilePath)
		if err != nil {
			klog.V(3).ErrorS(err, "Failed to read manager SM util file")
		}
	}

	err = c.VisitDevices(func(index int, hdev nvdev.Device) error {
		gpuInfo, err := c.GetGpuInfo(index, hdev)
		if err != nil {
			klog.Errorf("error getting info for GPU %d: %v", index, err)
			return nil
		}
		devIndexMap[gpuInfo.UUID] = index
		devMemInfoMap[gpuInfo.UUID] = gpuInfo.Memory
		busId := links.PciInfo(gpuInfo.PciInfo).BusID()
		migEnabled := fmt.Sprint(gpuInfo.MigEnabled)
		deviceIndex := strconv.Itoa(index)
		minorNumber := strconv.Itoa(gpuInfo.Minor)
		ch <- prometheus.MustNewConstMetric(
			physicalGPUTotalMemory,
			prometheus.GaugeValue,
			float64(gpuInfo.Memory.Total),
			c.nodeName, deviceIndex, gpuInfo.UUID, gpuInfo.ProductName,
			busId, minorNumber, migEnabled, gpuInfo.CudaComputeCapability)

		ch <- prometheus.MustNewConstMetric(
			physicalGPUMemoryUsage,
			prometheus.GaugeValue,
			float64(gpuInfo.Memory.Used),
			c.nodeName, deviceIndex, gpuInfo.UUID, gpuInfo.ProductName,
			busId, minorNumber, migEnabled, gpuInfo.CudaComputeCapability)

		memoryUtilRate := int64(0)
		if gpuInfo.Memory.Total > 0 {
			memoryUtilRate = int64(float64(gpuInfo.Memory.Used) / float64(gpuInfo.Memory.Total) * 100)
		}
		ch <- prometheus.MustNewConstMetric(
			physicalGPUMemoryUtilRate,
			prometheus.GaugeValue,
			float64(memoryUtilRate),
			c.nodeName, deviceIndex, gpuInfo.UUID, gpuInfo.ProductName,
			busId, minorNumber, migEnabled, gpuInfo.CudaComputeCapability)

		migInfos, err := c.GetMigInfos(gpuInfo)
		if err != nil {
			klog.Errorf("error getting MIG infos for GPU %d: %v", index, err)
		}
		if len(migInfos) > 0 {
			devMigInfosMap[gpuInfo.UUID] = maps.Values[map[string]*nvidia.MigInfo](migInfos)
		}

		// Skip unsupported operations after enabling Mig.
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
				c.nodeName, deviceIndex, gpuInfo.UUID, gpuInfo.ProductName,
				busId, minorNumber, migEnabled, gpuInfo.CudaComputeCapability)
		}

		CollectorDeviceProcesses(deviceUtil, index, hdev, devProcInfoMap, devProcUtilMap)
		return nil
	})
	if err != nil {
		klog.Errorln(err.Error())
	}

skipNvml:
	var (
		vGpuHealthMap      = make(map[string]bool)
		vGpuTotalMemMap    = make(map[string]uint64)
		vGpuAssignedMemMap = make(map[string]uint64)
	)
	// Get current node.
	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		klog.Errorf("node lister get node <%s> error: %v", c.nodeName, err)
		return
	}

	nodeVGPUTotalMemBytes := uint64(0)
	registryNode, _ := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
	nodeDevInfo, _ := device.ParseNodeDeviceInfo(registryNode)
	for _, devInfo := range nodeDevInfo {
		// Skip the statistics of MIG device.
		if devInfo.Mig {
			continue
		}
		vGpuHealthMap[devInfo.Uuid] = devInfo.Healthy
		vGpuAssignedMemMap[devInfo.Uuid] = 0
		vGpuTotalMemBytes := uint64(devInfo.Memory) << 20
		vGpuTotalMemMap[devInfo.Uuid] = vGpuTotalMemBytes
		nodeVGPUTotalMemBytes += vGpuTotalMemBytes
	}
	ch <- prometheus.MustNewConstMetric(
		nodeVGPUTotalMemory,
		prometheus.GaugeValue,
		float64(nodeVGPUTotalMemBytes),
		c.nodeName,
	)
	// Get all pods.
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("pod lister list error: %v", err)
		return
	}

	nodeAssignedMemBytes := uint64(0)
	// Filter out some useless pods.
	util.PodsOnNode(pods, node, func(pod *corev1.Pod) {
		// Aggregate the allocated memory size on the node.
		podDevices := device.GetPodAssignDevices(pod)
		FlattenDevicesFunc(podDevices, func(claimDevice device.ClaimDevice) {
			memoryBytes := uint64(claimDevice.Memory) << 20
			nodeAssignedMemBytes += memoryBytes
			if _, ok := vGpuAssignedMemMap[claimDevice.Uuid]; ok {
				vGpuAssignedMemMap[claimDevice.Uuid] += memoryBytes
			}
		})

		for _, container := range pod.Spec.Containers {
			contKey := GetContainerKey(pod.UID, container.Name)
			resData, exist := c.contLister.GetResourceData(contKey)
			if !exist {
				continue
			}

			klog.V(4).Infoln("Container matching: using resource data", "ContainerName", container.Name)
			var getFullPath func(string) string
			switch {
			case cgroups.IsCgroup2UnifiedMode(): // cgroupv2
				getFullPath = util.GetK8sPodCGroupFullPath
			case cgroups.IsCgroup2HybridMode():
				// If the device controller does not exist, use the path of cgroupv2.
				getFullPath = util.GetK8sPodDeviceCGroupFullPath
				if util.PathIsNotExist(util.CGroupDevicePath) {
					getFullPath = util.GetK8sPodCGroupFullPath
				}
			default: // cgroupv1
				getFullPath = util.GetK8sPodDeviceCGroupFullPath
			}
			var containerPids []uint32
			ContainerPidsFunc(pod, container.Name, getFullPath, func(pid int) {
				containerPids = append(containerPids, uint32(pid))
			})
			_, containerId := util.GetContainerRuntime(pod, container.Name)

			for i := int32(0); i < resData.DeviceCount; i++ {
				var (
					vDevIndex      = strconv.Itoa(int(i))
					deviceUUID     = string(resData.Devices[i].UUID[0:40])
					deviceMemLimit = resData.Devices[i].TotalMemory
					realMemBytes   = resData.Devices[i].RealMemory
					deviceMemUsage = uint64(0)
					deviceSMUtil   = uint32(0)
					tmpPids        []string
				)
				ContainerDeviceProcInfoFunc(devProcInfoMap[deviceUUID], containerPids,
					func(process nvml.ProcessInfo_v1) {
						tmpPids = append(tmpPids, strconv.Itoa(int(process.Pid)))
						deviceMemUsage += process.UsedGpuMemory
					})
				containerGPUPids := strings.Join(tmpPids, ",")
				ContainerDeviceProcUtilFunc(devProcUtilMap[deviceUUID], containerPids,
					func(sample nvml.ProcessUtilizationSample) {
						smUtil := GetValidValue(sample.SmUtil)
						codecUtil := GetValidValue(sample.EncUtil) +
							GetValidValue(sample.DecUtil)
						codecUtil = CodecNormalize(codecUtil)
						deviceSMUtil += smUtil + codecUtil
					})

				ch <- prometheus.MustNewConstMetric(
					containerVGPUMemoryLimit,
					prometheus.GaugeValue,
					float64(deviceMemLimit),
					pod.Namespace, pod.Name, container.Name, vDevIndex,
					deviceUUID, containerId, containerGPUPids, c.nodeName,
					strconv.FormatUint(realMemBytes, 10))
				// TODO Unable to calculate the usage of virtual video memory
				ch <- prometheus.MustNewConstMetric(
					containerVGPUMemoryUsage,
					prometheus.GaugeValue,
					float64(deviceMemUsage),
					pod.Namespace, pod.Name, container.Name, vDevIndex,
					deviceUUID, containerId, containerGPUPids, c.nodeName)
				memoryUtilRate := int64(0)
				if deviceMemLimit > 0 {
					memoryUtilRate = int64(float64(deviceMemUsage) / float64(deviceMemLimit) * 100)
					if memoryUtilRate > 100 {
						memoryUtilRate = 100
					}
				}
				// TODO Unable to calculate the usage of virtual video memory
				ch <- prometheus.MustNewConstMetric(
					containerVGPUMemoryUtilRate,
					prometheus.GaugeValue,
					float64(memoryUtilRate),
					pod.Namespace, pod.Name, container.Name, vDevIndex,
					deviceUUID, containerId, containerGPUPids, c.nodeName)
				ch <- prometheus.MustNewConstMetric(
					containerVGPUCoreUtilRate,
					prometheus.GaugeValue,
					float64(GetPercentageValue(deviceSMUtil)),
					pod.Namespace, pod.Name, container.Name, vDevIndex,
					deviceUUID, containerId, containerGPUPids, c.nodeName)
			}
		}
	})

	ch <- prometheus.MustNewConstMetric(
		nodeVGPUAssignedMemory,
		prometheus.GaugeValue,
		float64(nodeAssignedMemBytes),
		c.nodeName)

	var (
		listResourceOnce        sync.Once
		podResourcesResp        *v1alpha1.ListPodResourcesResponse
		listMigPodResourcesFunc = func() *v1alpha1.ListPodResourcesResponse {
			listResourceOnce.Do(func() {
				resource, err := c.podResource.ListPodResource(func(devices *v1alpha1.ContainerDevices) bool {
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

	FlattenMigInfosMapFunc(devMigInfosMap, func(parentUUID string, migInfo *nvidia.MigInfo) {
		migInx := strconv.Itoa(migInfo.Index)
		ciId := fmt.Sprintf("%d", migInfo.CiInfo.Id)
		giId := fmt.Sprintf("%d", migInfo.GiInfo.Id)
		isHealthy := fmt.Sprint(vGpuHealthMap[parentUUID])
		podResourcesResp = listMigPodResourcesFunc()
		podInfo := client.PodInfo{}
		podInfoP, _ := c.podResource.GetPodInfoByMatchFunc(podResourcesResp, func(devices *v1alpha1.ContainerDevices) bool {
			return devices.GetResourceName() == deviceplugin.GetMigResourceName(migInfo) &&
				slices.Contains(devices.GetDeviceIds(), migInfo.UUID)
		})
		if podInfoP != nil {
			podInfo = *podInfoP
		}
		ch <- prometheus.MustNewConstMetric(
			migDeviceTotalMemory,
			prometheus.GaugeValue,
			float64(migInfo.Memory.Total),
			c.nodeName, migInx, migInfo.UUID, parentUUID, ciId, giId, migInfo.Profile,
			isHealthy, podInfo.PodNamespace, podInfo.PodName, podInfo.ContainerName)
		ch <- prometheus.MustNewConstMetric(
			migDeviceMemoryUsage,
			prometheus.GaugeValue,
			float64(migInfo.Memory.Used),
			c.nodeName, migInx, migInfo.UUID, parentUUID, ciId, giId, migInfo.Profile,
			isHealthy, podInfo.PodNamespace, podInfo.PodName, podInfo.ContainerName)
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
			c.nodeName, migInx, migInfo.UUID, parentUUID, ciId, giId, migInfo.Profile,
			isHealthy, podInfo.PodNamespace, podInfo.PodName, podInfo.ContainerName)
	})

	devMemRatioMap := make(map[string]float64, len(vGpuTotalMemMap))
	for uuid, totalMem := range vGpuTotalMemMap {
		phyTotalMemory := totalMem
		if memory, exists := devMemInfoMap[uuid]; exists {
			phyTotalMemory = memory.Total
		}
		devMemRatioMap[uuid] = float64(totalMem) / float64(phyTotalMemory)
		ch <- prometheus.MustNewConstMetric(
			virtGPUTotalMemory,
			prometheus.GaugeValue,
			float64(totalMem),
			c.nodeName, strconv.Itoa(devIndexMap[uuid]), uuid,
			strconv.FormatUint(phyTotalMemory, 10), fmt.Sprint(vGpuHealthMap[uuid]))
	}

	for uuid, assignedMem := range vGpuAssignedMemMap {
		phyAssignedMemory := assignedMem
		if memRatio := devMemRatioMap[uuid]; memRatio > 1 {
			phyAssignedMemory = uint64(float64(phyAssignedMemory) / memRatio)
		}
		ch <- prometheus.MustNewConstMetric(
			virtGPUAssignedMemory,
			prometheus.GaugeValue,
			float64(assignedMem),
			c.nodeName, strconv.Itoa(devIndexMap[uuid]), uuid,
			strconv.FormatUint(phyAssignedMemory, 10), fmt.Sprint(vGpuHealthMap[uuid]))
	}
}

func CollectorDeviceProcesses(deviceUtil *watcher.DeviceUtilT, index int, hdev nvml.Device, devProcInfoMap map[string]procInfoList, devProcUtilMap map[string]procUtilList) {
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
		klog.V(4).InfoS("collector device processes from sm watcher", "device", index)
		fd, err := watcher.DeviceUtilRLock(index, smFilePath)
		if err == nil {
			micro := time.UnixMicro(int64(deviceUtil.Devices[index].LastSeenTimeStamp))
			if time.Now().Sub(micro) > 5*time.Second {
				watcher.DeviceUtilUnlock(fd, index)
				klog.V(3).InfoS("Process utilization time window timeout detected, rollback using nvml driver to obtain utilization", "device", index)
				nvmlProcessInfoFunc()
				goto nvmlProcessUtil
			}
			if deviceUtil.Devices[index].ComputeProcessesSize > 0 {
				processInfos = append(processInfos, deviceUtil.Devices[index].ComputeProcesses[:deviceUtil.Devices[index].ComputeProcessesSize]...)
			}
			if deviceUtil.Devices[index].GraphicsProcessesSize > 0 {
				processInfos = append(processInfos, deviceUtil.Devices[index].GraphicsProcesses[:deviceUtil.Devices[index].GraphicsProcessesSize]...)
			}
			if len(processInfos) == 0 {
				nvmlProcessInfoFunc()
			}
			if deviceUtil.Devices[index].ProcessUtilSamplesSize > 0 {
				processUtilizationSamples = append(processUtilizationSamples, deviceUtil.Devices[index].ProcessUtilSamples[:deviceUtil.Devices[index].ProcessUtilSamplesSize]...)
			}
			watcher.DeviceUtilUnlock(fd, index)
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

func FlattenMigInfosMapFunc(migInfosMap map[string][]*nvidia.MigInfo, f func(parentUUID string, migInfo *nvidia.MigInfo)) {
	if f == nil {
		return
	}
	for parentUUID, migInfos := range migInfosMap {
		for _, migInfo := range migInfos {
			if migInfo == nil {
				continue
			}
			f(parentUUID, migInfo)
		}
	}
}

func FlattenDevicesFunc(podDevices device.PodDevices, f func(claimDevice device.ClaimDevice)) {
	if f == nil {
		return
	}
	for _, contDevices := range podDevices {
		for _, dev := range contDevices.Devices {
			f(dev)
		}
	}
}
