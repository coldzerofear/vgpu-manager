/*
Copyright 2024-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collector

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/mig"
	"github.com/coldzerofear/vgpu-manager/pkg/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/metrics/lister"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/util/cgroup"
	"github.com/opencontainers/cgroups"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
)

// nodeGPUCollector implements the Collector interface.
type nodeGPUCollector struct {
	*nvidia.DeviceLib
	nodeName    string
	managerRoot string
	nodeLister  listerv1.NodeLister
	podLister   client.PodLister
	contLister  *lister.ContainerLister
	podResource *client.PodResource
	utilAdapter watcher.DeviceUtilInterface
	featureGate featuregate.FeatureGate
}

func NewNodeGPUCollector(
	config *node.NodeConfigSpec, nodeLister listerv1.NodeLister, podLister client.PodLister,
	contLister *lister.ContainerLister, managerRoot string, featureGate featuregate.FeatureGate,
) (prometheus.Collector, error) {
	driverRoot := config.GetDriverRoot()
	deviceLib, err := nvidia.DetectionDeviceLib(driverRoot)
	if err != nil {
		return nil, err
	}
	utilAdapter := watcher.NewDeviceUtilAdapter(
		watcher.WithExtendedInterface(deviceLib.Extensions()),
	)
	return &nodeGPUCollector{
		DeviceLib:   deviceLib,
		nodeName:    config.GetNodeName(),
		managerRoot: managerRoot,
		nodeLister:  nodeLister,
		podLister:   podLister,
		contLister:  contLister,
		featureGate: featureGate,
		utilAdapter: utilAdapter,
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
		[]string{"node", "device_idx", "device_uuid", "device_type", "access_mode"}, nil,
	)
	vGPUAssignedCoresNumber = prometheus.NewDesc(
		"vgpu_device_assigned_cores_number",
		"Virtual GPU device assigned cores number",
		[]string{"node", "device_idx", "device_uuid", "device_type", "access_mode"}, nil,
	)
	vGPUPeakSharedContainersNumber = prometheus.NewDesc(
		"vgpu_device_peak_shared_containers_number",
		"Peak number of containers that may concurrently share the vGPU device across the pods' lifecycle (reserved view; >= the current value)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "access_mode"}, nil,
	)
	vGPUCurrentSharedContainersNumber = prometheus.NewDesc(
		"vgpu_device_current_shared_containers_number",
		"Number of currently running containers sharing the vGPU device right now (a completed sequential init container is excluded; <= the peak value)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "access_mode"}, nil,
	)
	vGPUTotalMemory = prometheus.NewDesc(
		"vgpu_device_total_memory_in_bytes",
		"Virtual GPU device total memory (sum of physical GPU memory + unified memory)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "access_mode"}, nil,
	)
	vGPUTotalPhysicalMemory = prometheus.NewDesc(
		"vgpu_device_total_physical_memory_in_bytes",
		"Virtual GPU device total physical memory (only physical GPU memory)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "access_mode"}, nil,
	)
	vGPUAssignedMemory = prometheus.NewDesc(
		"vgpu_device_assigned_memory_in_bytes",
		"Virtual GPU device assigned memory (sum of physical GPU memory + unified memory)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "access_mode"}, nil,
	)
	vGPUAssignedPhysicalMemory = prometheus.NewDesc(
		"vgpu_device_assigned_physical_memory_in_bytes",
		"Virtual GPU device assigned physical memory (only physical GPU memory)",
		[]string{"node", "device_idx", "device_uuid", "device_type", "access_mode"}, nil,
	)

	containerVGPUMemoryLimit = prometheus.NewDesc(
		"container_vgpu_device_memory_limit_in_bytes",
		"Container's virtual GPU device total memory limit (sum of physical GPU memory + unified memory)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node", "access_mode", "pod_node"}, nil,
	)
	containerVGPUPhysicalMemoryLimit = prometheus.NewDesc(
		"container_vgpu_device_physical_memory_limit_in_bytes",
		"Container's virtual GPU device physical memory limit (only physical GPU memory)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node", "access_mode", "pod_node"}, nil,
	)

	containerVGPUMemoryUsage = prometheus.NewDesc(
		"container_vgpu_device_memory_usage_in_bytes",
		"Container's virtual GPU device memory usage (sum of physical GPU memory + unified memory)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node", "access_mode", "pod_node"}, nil,
	)
	containerVGPUPhysicalMemoryUsage = prometheus.NewDesc(
		"container_vgpu_device_physical_memory_usage_in_bytes",
		"Container's virtual GPU device physical memory usage (only physical GPU memory)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node", "access_mode", "pod_node"}, nil,
	)

	containerVGPUMemoryUtilRate = prometheus.NewDesc(
		"container_vgpu_device_memory_utilization_percent",
		"Container's virtual GPU device memory utilization percentage (0-100)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node", "access_mode", "pod_node"}, nil,
	)
	containerVGPUCoreUtilRate = prometheus.NewDesc(
		"container_vgpu_device_core_utilization_percent",
		"Container's virtual GPU device core utilization percentage (0-100)",
		[]string{"pod_namespace", "pod_name", "container_name", "vdevice_idx", "device_uuid", "node", "access_mode", "pod_node"}, nil,
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
	)

	CollectBasedOnNvml(ch, c.DeviceLib, c.nodeName, c.managerRoot, devTypeMap, devIndexMap, devHealthMap,
		devHealthLvs, devMemInfoMap, devProcInfoMap, devProcUtilMap, devMigInfosMap, c.utilAdapter, c.featureGate)

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

	defaultNodeConfigInfo := device.NodeConfigInfo{
		DeviceSplit: 10, CoresScaling: 1,
		MemoryScaling: 1, MemoryFactor: 1,
	}
	configInfoStr, _ := util.HasAnnotation(node, util.NodeConfigInfoAnnotation)
	if err = defaultNodeConfigInfo.Decode(configInfoStr); err != nil {
		klog.V(2).ErrorS(err, "parse node config information failed", "node", node.Name, "value", configInfoStr)
	}

	ch <- prometheus.MustNewConstMetric(nodeGPUConfigInfo,
		prometheus.GaugeValue, float64(1), c.nodeName,
		strconv.Itoa(defaultNodeConfigInfo.DeviceSplit),
		strconv.FormatFloat(defaultNodeConfigInfo.CoresScaling, 'f', 2, 64),
		strconv.FormatFloat(defaultNodeConfigInfo.MemoryScaling, 'f', 2, 64),
		strconv.Itoa(defaultNodeConfigInfo.MemoryFactor))

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
		} else if defaultNodeConfigInfo.MemoryScaling > 1 {
			nodeGPUTotalMemBytes += uint64(float64(vGpuTotalMemBytes) / defaultNodeConfigInfo.MemoryScaling)
		} else {
			nodeGPUTotalMemBytes += vGpuTotalMemBytes
		}
	}
	for uuid, status := range devHealthMap {
		labelValues, ok := devHealthLvs[uuid]
		if !ok {
			// The loop above adds an entry for every registered device, including
			// ones NVML never enumerated -- and when NvmlInit fails outright,
			// CollectBasedOnNvml returns with devHealthLvs still empty, so that is
			// ALL of them. Emitting with a nil label slice trips the
			// label-cardinality check inside MustNewConstMetric, and prometheus
			// runs Collect on its own goroutine without recovering, so the panic
			// would take the whole exporter down on every scrape.
			klog.V(4).InfoS("skip health metric for device with no NVML labels", "deviceUuid", uuid)
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			physicalGPUHealthStatus, prometheus.GaugeValue, float64(status), labelValues...)
	}
	ch <- prometheus.MustNewConstMetric(nodeVGPUTotalMemory,
		prometheus.GaugeValue, float64(nodeVGPUTotalMemBytes), c.nodeName)

	ch <- prometheus.MustNewConstMetric(nodeVGPUTotalPhysicalMemory,
		prometheus.GaugeValue, float64(nodeGPUTotalMemBytes), c.nodeName)

	// Get all pods on the current node whose device allocation should be counted.
	pods, err := c.podLister.ListByIndexFiledSet(fields.Set{
		metrics.IndexerKeyPodPlanSchedulingNode:        c.nodeName,
		metrics.IndexerKeyPodDeviceAllocationCountable: "true",
	})
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
	util.PodsOnNodeCallback(pods, node, func(pod *corev1.Pod) {
		// Fast return of pods allocated without counting devices.
		if !device.ShouldCountPodDeviceAllocation(pod) {
			return
		}
		// Aggregate the allocated memory size on the node, collapsing each
		// pod's claims to the per-GPU lifecycle peak so a sequential init
		// container reusing a regular container's GPU is not double-counted.
		// For a pod without a vGPU init container this is the plain per-GPU
		// sum, identical to the historical per-claim accounting.
		podDeviceClaim := device.GetPodDeviceClaim(pod)
		// Fast return of pods without device claims.
		if len(podDeviceClaim) == 0 {
			return
		}

		for _, fp := range device.ReducePodFootprint(pod, podDeviceClaim) {
			vGpuAssignedNumberMap[fp.Uuid] += fp.Number
			vGpuAssignedCoresMap[fp.Uuid] += fp.Cores
			memoryBytes := uint64(fp.Memory) << 20
			nodeVGpuAssignedMemBytes += memoryBytes
			vGpuAssignedMemMap[fp.Uuid] += memoryBytes
			// Peak (reserved) concurrent sharing: per-GPU lifecycle peak count.
			peakSharedContainersMap[fp.Uuid] += fp.Number
		}
		// Current sharing: only containers running right now, so a completed
		// sequential init container drops out (always <= the peak above).
		for uuid, n := range device.CurrentSharedContainers(pod, podDeviceClaim) {
			currentSharedContainersMap[uuid] += n
		}
		// Real-time per-container usage: regular containers, sidecars, and
		// currently-running sequential init containers (a completed init
		// container is excluded so its stale usage stops being reported).
		for _, containerName := range util.CollectableContainerNames(pod) {
			contKey := lister.GetContainerKey(pod.UID, containerName)
			resData, exist := c.contLister.GetResourceData(contKey)
			if !exist {
				continue
			}

			klog.V(4).Infoln("Container matching: using resource data", "pod", klog.KObj(pod), "container", containerName)
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
			_ = cgroup.GetContainerPidsFunc(pod, containerName, getFullPath, func(pid int) {
				containerPids = append(containerPids, uint32(pid))
			})
			//_, containerId := cgroup.GetContainerRuntime(pod, containerName)

			deviceCount := 0
			for i := int32(0); i < vgpu.MaxDeviceCount; i++ {
				containerDevice := resData.GetDeviceSnapshot(int(i))
				if containerDevice == nil || containerDevice.Activate == 0 {
					continue
				}
				deviceUUID := string(containerDevice.UUID[0:40])
				if !utf8.ValidString(deviceUUID) {
					klog.InfoS("Invalid UTF-8 device uuid, skip current device", "pod", klog.KObj(pod),
						"container", containerName, "deviceUuid", deviceUUID, "deviceIndex", i)
					continue
				}
				devHostIndex, exists := devIndexMap[deviceUUID]
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
						codecUtil := util.GetValidValue(sample.EncUtil) + util.GetValidValue(sample.DecUtil)
						codecUtil = util.CodecNormalize(codecUtil)
						deviceSMUtil += smUtil + codecUtil
					})

				ch <- prometheus.MustNewConstMetric(
					containerVGPUMemoryLimit, prometheus.GaugeValue, float64(deviceMemLimit), pod.Namespace, pod.Name,
					containerName, vDevIndex, deviceUUID, c.nodeName, util.AccessModeLocal, pod.Spec.NodeName)

				ch <- prometheus.MustNewConstMetric(
					containerVGPUPhysicalMemoryLimit, prometheus.GaugeValue, float64(realMemBytes), pod.Namespace,
					pod.Name, containerName, vDevIndex, deviceUUID, c.nodeName, util.AccessModeLocal, pod.Spec.NodeName)

				// TODO handler Virtual Memory Cache node.
				if c.featureGate.Enabled(util.VirtualMemoryTracking) {
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
						unlock, err := vMemory.RLock(devHostIndex)
						if err != nil {
							klog.V(3).ErrorS(err, "virtual memory RLock failed", "devHostIndex", devHostIndex)
							return
						}
						defer func() {
							if err := unlock(); err != nil {
								klog.ErrorS(err, "vMemory unlock failed", "devHostIndex", devHostIndex)
							}
						}()
						deviceUsed, err := vMemory.GetDeviceMemory(devHostIndex)
						if err != nil {
							klog.V(3).ErrorS(err, "get device vMemory failed", "devHostIndex", devHostIndex)
							return
						}
						deviceVMemUsage += deviceUsed.GetTotalUsed()
					}()
				}

				ch <- prometheus.MustNewConstMetric(containerVGPUMemoryUsage,
					prometheus.GaugeValue, float64(deviceMemUsage+deviceVMemUsage), pod.Namespace, pod.Name,
					containerName, vDevIndex, deviceUUID, c.nodeName, util.AccessModeLocal, pod.Spec.NodeName)
				ch <- prometheus.MustNewConstMetric(
					containerVGPUPhysicalMemoryUsage, prometheus.GaugeValue, float64(deviceMemUsage), pod.Namespace,
					pod.Name, containerName, vDevIndex, deviceUUID, c.nodeName, util.AccessModeLocal, pod.Spec.NodeName)

				deviceMemUsage += deviceVMemUsage
				memoryUtilRate := int64(0)
				if deviceMemUsage >= deviceMemLimit {
					memoryUtilRate = 100
				} else if deviceMemLimit > 0 {
					memoryUtilRate = int64(float64(deviceMemUsage) / float64(deviceMemLimit) * 100)
				}
				ch <- prometheus.MustNewConstMetric(containerVGPUMemoryUtilRate,
					prometheus.GaugeValue, float64(memoryUtilRate), pod.Namespace, pod.Name, containerName,
					vDevIndex, deviceUUID, c.nodeName, util.AccessModeLocal, pod.Spec.NodeName)

				ch <- prometheus.MustNewConstMetric(containerVGPUCoreUtilRate,
					prometheus.GaugeValue, float64(util.GetPercentageValue(deviceSMUtil)), pod.Namespace, pod.Name,
					containerName, vDevIndex, deviceUUID, c.nodeName, util.AccessModeLocal, pod.Spec.NodeName)
			}
		}
	})

	nodeGpuAssignedMemoryBytes := uint64(0)
	for uuid, totalMemoryBytes := range vGpuTotalMemMap {
		totalPhyMemoryBytes := totalMemoryBytes
		if memory, exists := devMemInfoMap[uuid]; exists {
			totalPhyMemoryBytes = memory.Total
		}
		memoryRatio := float64(totalMemoryBytes) / float64(totalPhyMemoryBytes)
		deviceIndex := strconv.Itoa(devIndexMap[uuid])
		//healthy := fmt.Sprint(vGpuHealthMap[uuid])
		ch <- prometheus.MustNewConstMetric(vGPUTotalMemory, prometheus.GaugeValue,
			float64(totalMemoryBytes), c.nodeName, deviceIndex, uuid, devTypeMap[uuid], util.AccessModeLocal)

		ch <- prometheus.MustNewConstMetric(vGPUTotalPhysicalMemory, prometheus.GaugeValue,
			float64(totalPhyMemoryBytes), c.nodeName, deviceIndex, uuid, devTypeMap[uuid], util.AccessModeLocal)

		assignedPhyMemoryBytes := vGpuAssignedMemMap[uuid]
		if memoryRatio > 1 {
			assignedPhyMemoryBytes = uint64(float64(assignedPhyMemoryBytes) / memoryRatio)
		}
		nodeGpuAssignedMemoryBytes += assignedPhyMemoryBytes
		ch <- prometheus.MustNewConstMetric(vGPUAssignedMemory, prometheus.GaugeValue,
			float64(vGpuAssignedMemMap[uuid]), c.nodeName, deviceIndex, uuid, devTypeMap[uuid], util.AccessModeLocal)

		ch <- prometheus.MustNewConstMetric(vGPUAssignedPhysicalMemory, prometheus.GaugeValue,
			float64(assignedPhyMemoryBytes), c.nodeName, deviceIndex, uuid, devTypeMap[uuid], util.AccessModeLocal)

		ch <- prometheus.MustNewConstMetric(vGPUTotalCoresNumber, prometheus.GaugeValue,
			float64(vGPUTotalCoresMap[uuid]), c.nodeName, deviceIndex, uuid, devTypeMap[uuid], util.AccessModeLocal)

		ch <- prometheus.MustNewConstMetric(vGPUAssignedCoresNumber, prometheus.GaugeValue,
			float64(vGpuAssignedCoresMap[uuid]), c.nodeName, deviceIndex, uuid, devTypeMap[uuid], util.AccessModeLocal)

		ch <- prometheus.MustNewConstMetric(vGPUPeakSharedContainersNumber, prometheus.GaugeValue,
			float64(peakSharedContainersMap[uuid]), c.nodeName, deviceIndex, uuid, devTypeMap[uuid], util.AccessModeLocal)

		ch <- prometheus.MustNewConstMetric(vGPUCurrentSharedContainersNumber, prometheus.GaugeValue,
			float64(currentSharedContainersMap[uuid]), c.nodeName, deviceIndex, uuid, devTypeMap[uuid], util.AccessModeLocal)
	}

	ch <- prometheus.MustNewConstMetric(
		nodeVGPUAssignedMemory, prometheus.GaugeValue, float64(nodeVGpuAssignedMemBytes), c.nodeName)

	ch <- prometheus.MustNewConstMetric(
		nodeVGPUAssignedPhysicalMemory, prometheus.GaugeValue, float64(nodeGpuAssignedMemoryBytes), c.nodeName)

	var (
		listResourceOnce        sync.Once
		podResourcesResp        *v1alpha1.ListPodResourcesResponse
		listMigPodResourcesFunc = func() *v1alpha1.ListPodResourcesResponse {
			listResourceOnce.Do(func() {
				if resource, err := c.podResource.ListPodResource(context.Background(), func(devices *v1alpha1.ContainerDevices) bool {
					return len(devices.GetDeviceIds()) > 0 && strings.HasPrefix(devices.GetResourceName(), util.MIGDeviceResourceNamePrefix)
				}); err != nil {
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
			return devices.GetResourceName() == mig.GetMigResourceName(migInfo) && slices.Contains(devices.GetDeviceIds(), migInfo.UUID)
		})
		if podInfoP != nil {
			ch <- prometheus.MustNewConstMetric(containerMIGAllocationInfo,
				prometheus.GaugeValue, float64(1), c.nodeName, migIdx, migInfo.UUID,
				parentUUID, podInfoP.PodNamespace, podInfoP.PodName, podInfoP.ContainerName)
		}
		ch <- prometheus.MustNewConstMetric(migDeviceTotalMemory,
			prometheus.GaugeValue, float64(migInfo.Memory.Total),
			c.nodeName, migIdx, migInfo.UUID, parentUUID, ciId, giId, migInfo.Profile)

		ch <- prometheus.MustNewConstMetric(migDeviceMemoryUsage,
			prometheus.GaugeValue, float64(migInfo.Memory.Used),
			c.nodeName, migIdx, migInfo.UUID, parentUUID, ciId, giId, migInfo.Profile)

		memoryUtilRate := int64(0)
		if migInfo.Memory.Total > 0 {
			memoryUtilRate = int64(float64(migInfo.Memory.Used) / float64(migInfo.Memory.Total) * 100)
			if memoryUtilRate > 100 {
				memoryUtilRate = 100
			}
		}

		ch <- prometheus.MustNewConstMetric(migDeviceMemoryUtilRate,
			prometheus.GaugeValue, float64(memoryUtilRate), c.nodeName,
			migIdx, migInfo.UUID, parentUUID, ciId, giId, migInfo.Profile)
	})

}

func collectorDeviceProcesses(
	utilAdapter watcher.DeviceUtilInterface,
	mmapUtil *watcher.MmapDeviceUtil,
	index int, hdev nvml.Device,
	devProcInfoMap map[string]procInfoList,
	devProcUtilMap map[string]procUtilList,
) {
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

	if mmapUtil != nil {
		klog.V(4).InfoS("collector device processes from sm watcher", "device", index)
		if unlock, err := mmapUtil.RLock(index); err != nil {
			klog.V(3).ErrorS(err, "SM Watcher lock failed, fallback to nvml driver call", "device", index)
		} else if devUtil, err := mmapUtil.GetDeviceUtil(index); err != nil {
			klog.V(3).ErrorS(err, "get device util failed", "device", index)
		} else {
			micro := time.UnixMicro(int64(devUtil.LastSeenTimeStamp))
			if time.Now().Sub(micro) > 5*time.Second {
				_ = unlock()
				klog.V(3).InfoS("Process utilization time window timeout detected, rollback using nvml driver to obtain utilization", "device", index)
				nvmlProcessInfoFunc()
				goto nvmlProcessUtil
			}
			if devUtil.ComputeProcessesSize > 0 {
				processInfos = append(processInfos, devUtil.ComputeProcesses[:devUtil.ComputeProcessesSize]...)
			}
			if devUtil.GraphicsProcessesSize > 0 {
				processInfos = append(processInfos, devUtil.GraphicsProcesses[:devUtil.GraphicsProcessesSize]...)
			}
			if len(processInfos) == 0 {
				nvmlProcessInfoFunc()
			}
			if devUtil.ProcessUtilSamplesSize > 0 {
				processUtilizationSamples = append(processUtilizationSamples, devUtil.ProcessUtilSamples[:devUtil.ProcessUtilSamplesSize]...)
			}
			_ = unlock()
			goto collecProcessInfo
		}
	}

	nvmlProcessInfoFunc()

nvmlProcessUtil:
	// On MIG-enabled GPUs, querying process utilization is not currently supported.
	processUtilizationSamples, _, rt = utilAdapter.DeviceGetEnhanceCompatibilityProcessUtilSamples(hdev)
	if rt != nvml.SUCCESS {
		klog.V(4).Infof("error getting process utilization for device %d: %s", index, nvml.ErrorString(rt))
	}

collecProcessInfo:
	// NVML's nvmlProcessInfo_t.UsedGpuMemory is the per-PROCESS total, not
	// per-context. A process that owns both compute (CUDA / OpenCL) and
	// graphics (Vulkan / OpenGL / DirectX) contexts shows up in both the
	// Compute and Graphics process lists with the SAME UsedGpuMemory value
	// (typical for Isaac Sim, Omniverse, UE5 + CUDA inference, etc.).
	// Skip the duplicate to avoid doubling the metric. This guards both
	// the SM-watcher path (in case the watcher dedup is bypassed) and the
	// direct nvmlProcessInfoFunc fallback. Same fix class as cuda_hook.c
	// commit 20e9519 and watcher.go in this commit.
	processInfoList := make(procInfoList, len(processInfos))
	for _, processInfo := range processInfos {
		processInfoList[processInfo.Pid] = nvml.ProcessInfo_v1{
			Pid:           processInfo.Pid,
			UsedGpuMemory: processInfo.UsedGpuMemory,
		}
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
