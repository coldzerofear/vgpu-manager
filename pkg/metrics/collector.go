package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/filter"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

// nodeGPUCollector implements the Collector interface.
type nodeGPUCollector struct {
	nodeName   string
	nodeLister listerv1.NodeLister
	podLister  listerv1.PodLister
	contLister *ContainerLister
}

var _ prometheus.Collector = &nodeGPUCollector{}

func NewNodeGPUCollector(nodeName string, nodeLister listerv1.NodeLister,
	podLister listerv1.PodLister, contLister *ContainerLister) *nodeGPUCollector {
	return &nodeGPUCollector{
		nodeName:   nodeName,
		nodeLister: nodeLister,
		podLister:  podLister,
		contLister: contLister,
	}
}

// Descriptors used by the nodeGPUCollector below.
var (
	physicalGPUTotalMemory = prometheus.NewDesc(
		"physical_gpu_device_total_memory_in_bytes",
		"Physical GPU device total memory (bytes)",
		[]string{"nodename", "deviceidx", "deviceuuid"}, nil,
	)
	physicalGPUMemoryUsage = prometheus.NewDesc(
		"physical_gpu_device_memory_usage_in_bytes",
		"Physical GPU device memory usage (bytes)",
		[]string{"nodename", "deviceidx", "deviceuuid"}, nil,
	)
	physicalGPUCoreUtil = prometheus.NewDesc(
		"physical_gpu_device_core_utilization",
		"Physical GPU device core utilization (percentage)",
		[]string{"nodename", "deviceidx", "deviceuuid"}, nil,
	)
	nodeVGPUTotalMemory = prometheus.NewDesc(
		"node_vgpu_total_memory_in_bytes",
		"Node vGPU total memory (bytes)",
		[]string{"nodename"}, nil,
	)
	nodeVGPUAssignedMemory = prometheus.NewDesc(
		"node_vgpu_assigned_memory_in_bytes",
		"Node vGPU assigned memory (bytes)",
		[]string{"nodename"}, nil,
	)
	vGPUTotalMemory = prometheus.NewDesc(
		"vgpu_device_total_memory_in_bytes",
		"vGPU device total memory (bytes)",
		[]string{"nodename", "deviceidx", "deviceuuid", "healthy"}, nil,
	)
	vGPUAssignedMemory = prometheus.NewDesc(
		"vgpu_device_assigned_memory_in_bytes",
		"vGPU device assigned memory (bytes)",
		[]string{"nodename", "deviceidx", "deviceuuid", "healthy"}, nil,
	)
	ctrVGPUMemUsage = prometheus.NewDesc(
		"container_vgpu_device_memory_usage_in_bytes",
		"Container vGPU device memory usage (bytes)",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid", "ctrid", "ctrpids", "nodename"}, nil,
	)
	ctrVGPUMemLimit = prometheus.NewDesc(
		"container_vgpu_device_memory_limit_in_bytes",
		"Container vGPU device memory limit (bytes)",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid", "ctrid", "ctrpids", "nodename"}, nil,
	)
	ctrVGPUUtil = prometheus.NewDesc(
		"container_vgpu_device_utilization",
		"Container vGPU device utilization (percentage)",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid", "ctrid", "ctrpids", "nodename"}, nil,
	)
)

// Describe is implemented with DescribeByCollect. That's possible because the
// Collect method will always return the same two metrics with the same two
// descriptors.
func (c nodeGPUCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- physicalGPUTotalMemory
	ch <- physicalGPUMemoryUsage
	ch <- physicalGPUCoreUtil
	ch <- nodeVGPUTotalMemory
	ch <- nodeVGPUAssignedMemory
	ch <- vGPUTotalMemory
	ch <- vGPUAssignedMemory
	ch <- ctrVGPUMemUsage
	ch <- ctrVGPUMemLimit
	ch <- ctrVGPUUtil
}

type processInfoList map[uint32]nvml.ProcessInfo_v1

type processUtilList map[uint32]nvml.ProcessUtilizationSample

func ContainerPidsFunc(pod *corev1.Pod, containerName string, fullPath func(string) string, f func(pid int)) {
	cgroupFullPath, err := util.GetK8sPodContainerCGroupFullPath(pod, containerName, fullPath)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}
	klog.Infof("Detected Pod <%s/%s> container <%s> CGroup path: %s", pod.Namespace, pod.Name, containerName, cgroupFullPath)
	pids, err := cgroups.GetAllPids(cgroupFullPath)
	if err != nil {
		klog.Errorf("Get Pod <%s/%s> container <%s> CGroup pids error: %v", pod.Namespace, pod.Name, containerName, err)
		return
	}
	klog.V(4).Infof("Pod <%s/%s> container <%s>  CGroup path <%s> pids: %+v", pod.Namespace, pod.Name, containerName, cgroupFullPath, pids)
	for _, pid := range pids {
		f(pid)
	}
}

func ContainerDeviceProcessFunc(processes processInfoList,
	containerPids []uint32, f func(nvml.ProcessInfo_v1)) {
	if processes == nil {
		return
	}
	for _, contPid := range containerPids {
		if process, ok := processes[contPid]; ok {
			f(process)
		}
	}
}

func ContainerDeviceUtilFunc(processes processUtilList,
	containerPids []uint32, f func(nvml.ProcessUtilizationSample)) {
	if processes == nil {
		return
	}
	for _, contPid := range containerPids {
		if process, ok := processes[contPid]; ok {
			f(process)
		}
	}
}

// Collect device indicators
func (c nodeGPUCollector) Collect(ch chan<- prometheus.Metric) {
	klog.V(4).Infof("Starting to collect metrics for vGPU on node <%s>", c.nodeName)
	var (
		devCount       int
		devIndexMap    = make(map[string]int)
		devProcessMap  = make(map[string]processInfoList)
		devProcUtilMap = make(map[string]processUtilList)
	)

	rt := nvml.Init()
	if rt != nvml.SUCCESS {
		klog.Errorf("nvml Init error: %s", nvml.ErrorString(rt))
		goto skip
	}
	defer nvml.Shutdown()

	devCount, rt = nvml.DeviceGetCount()
	if rt != nvml.SUCCESS {
		klog.Errorf("nvml DeviceGetCount error: %s", nvml.ErrorString(rt))
		goto skip
	}
	for devIdx := 0; devIdx < devCount; devIdx++ {
		hdev, rt := nvml.DeviceGetHandleByIndex(devIdx)
		if rt != nvml.SUCCESS {
			klog.Errorf("nvml DeviceGetHandleByIndex %d error: %s", devIdx, nvml.ErrorString(rt))
			continue
		}
		memoryInfo, rt := hdev.GetMemoryInfo()
		if rt != nvml.SUCCESS {
			klog.Errorf("nvml DeviceGetMemoryInfo %d error: %s", devIdx, nvml.ErrorString(rt))
			continue
		}
		deviceUUID, rt := hdev.GetUUID()
		if rt != nvml.SUCCESS {
			klog.Errorf("nvml DeviceGetUUID %d error: %s", devIdx, nvml.ErrorString(rt))
			continue
		}
		deviceUtil, rt := hdev.GetUtilizationRates()
		if rt != nvml.SUCCESS {
			klog.Errorf("nvml DeviceGetUtilizationRates %d error: %s", devIdx, nvml.ErrorString(rt))
			continue
		}

		deviceIndex := strconv.Itoa(devIdx)
		ch <- prometheus.MustNewConstMetric(
			physicalGPUTotalMemory,
			prometheus.GaugeValue,
			float64(memoryInfo.Total),
			c.nodeName, deviceIndex, deviceUUID)

		ch <- prometheus.MustNewConstMetric(
			physicalGPUMemoryUsage,
			prometheus.GaugeValue,
			float64(memoryInfo.Used),
			c.nodeName, deviceIndex, deviceUUID)

		ch <- prometheus.MustNewConstMetric(
			physicalGPUCoreUtil,
			prometheus.GaugeValue,
			float64(deviceUtil.Gpu),
			c.nodeName, deviceIndex, deviceUUID)

		// Aggregate GPU processes.
		var processes []nvml.ProcessInfo
		if procs, rt := hdev.GetGraphicsRunningProcesses(); rt == nvml.SUCCESS {
			processes = append(processes, procs...)
		}
		if procs, rt := hdev.GetComputeRunningProcesses(); rt == nvml.SUCCESS {
			processes = append(processes, procs...)
		}
		processInfos := make(processInfoList)
		for _, process := range processes {
			if info, ok := processInfos[process.Pid]; ok {
				info.UsedGpuMemory += process.UsedGpuMemory
			} else {
				processInfos[process.Pid] = nvml.ProcessInfo_v1{
					Pid:           process.Pid,
					UsedGpuMemory: process.UsedGpuMemory,
				}
			}
		}
		devIndexMap[deviceUUID] = devIdx
		devProcessMap[deviceUUID] = processInfos
		lastTs := time.Now().Add(-1 * time.Second).UnixMicro()
		utilization, rt := hdev.GetProcessUtilization(uint64(lastTs))
		if rt != nvml.SUCCESS {
			klog.V(5).Infof("nvml DeviceGetProcessUtilization %d error: %s", devIdx, nvml.ErrorString(rt))
			continue
		}
		procUtils := make(processUtilList)
		for _, sample := range utilization {
			procUtils[sample.Pid] = sample
		}
		devProcUtilMap[deviceUUID] = procUtils
	}

skip:
	var (
		vGpuHealthMap      = make(map[string]bool)
		vGpuTotalMemMap    = make(map[string]uint64)
		vGpuAssignedMemMap = make(map[string]uint64)
	)

	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		klog.Errorf("node lister get node <%s> error: %v", c.nodeName, err)
		return
	}

	nodeTotaMemoryBytes := uint64(0)
	registryNode, _ := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
	deviceInfos, _ := device.ParseNodeDeviceInfos(registryNode)
	for _, info := range deviceInfos {
		vGpuHealthMap[info.Uuid] = info.Healthy
		vGpuAssignedMemMap[info.Uuid] = 0
		vGpuTotalMemBytes := uint64(info.Memory) << 20
		vGpuTotalMemMap[info.Uuid] = vGpuTotalMemBytes
		nodeTotaMemoryBytes += vGpuTotalMemBytes
	}
	ch <- prometheus.MustNewConstMetric(
		nodeVGPUTotalMemory,
		prometheus.GaugeValue,
		float64(nodeTotaMemoryBytes),
		c.nodeName,
	)
	// get current node.
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("pod lister list error: %v", err)
		return
	}
	// Filter out some useless pods.
	pods = filter.CollectPodsOnNode(pods, node)
	nodeAssignedMemBytes := uint64(0)
	for _, pod := range pods {
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
			key := GetContainerKey(pod.UID, container.Name)
			resData, ok := c.contLister.GetResourceData(key)
			if !ok {
				continue
			}

			klog.V(4).Infoln("Container matching: using resource data", "ContainerName", container.Name)
			var getFullPath func(string) string
			if cgroups.IsCgroup2UnifiedMode() {
				getFullPath = util.GetK8sPodCGroupFullPath
			} else {
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
					deviceMemUsage = uint64(0)
					deviceSMUtil   = uint32(0)
				)
				var tmpPids []string
				ContainerDeviceProcessFunc(devProcessMap[deviceUUID], containerPids, func(process nvml.ProcessInfo_v1) {
					tmpPids = append(tmpPids, strconv.Itoa(int(process.Pid)))
					deviceMemUsage += process.UsedGpuMemory
				})
				containerGPUPids := strings.Join(tmpPids, ",")
				ContainerDeviceUtilFunc(devProcUtilMap[deviceUUID], containerPids, func(sample nvml.ProcessUtilizationSample) {
					deviceSMUtil += sample.SmUtil
				})

				ch <- prometheus.MustNewConstMetric(
					ctrVGPUMemLimit,
					prometheus.GaugeValue,
					float64(deviceMemLimit),
					pod.Namespace, pod.Name, container.Name, vDevIndex,
					deviceUUID, containerId, containerGPUPids, c.nodeName)
				ch <- prometheus.MustNewConstMetric(
					ctrVGPUMemUsage,
					prometheus.GaugeValue,
					float64(deviceMemUsage),
					pod.Namespace, pod.Name, container.Name, vDevIndex,
					deviceUUID, containerId, containerGPUPids, c.nodeName)
				ch <- prometheus.MustNewConstMetric(
					ctrVGPUUtil,
					prometheus.GaugeValue,
					float64(deviceSMUtil),
					pod.Namespace, pod.Name, container.Name, vDevIndex,
					deviceUUID, containerId, containerGPUPids, c.nodeName)
			}
		}
	}

	ch <- prometheus.MustNewConstMetric(
		nodeVGPUAssignedMemory,
		prometheus.GaugeValue,
		float64(nodeAssignedMemBytes),
		c.nodeName)

	for uuid, totalMem := range vGpuTotalMemMap {
		ch <- prometheus.MustNewConstMetric(
			vGPUTotalMemory,
			prometheus.GaugeValue,
			float64(totalMem),
			c.nodeName, strconv.Itoa(devIndexMap[uuid]), uuid,
			fmt.Sprint(vGpuHealthMap[uuid]))
	}
	for uuid, assignedMem := range vGpuAssignedMemMap {
		ch <- prometheus.MustNewConstMetric(
			vGPUAssignedMemory,
			prometheus.GaugeValue,
			float64(assignedMem),
			c.nodeName, strconv.Itoa(devIndexMap[uuid]), uuid,
			fmt.Sprint(vGpuHealthMap[uuid]))
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