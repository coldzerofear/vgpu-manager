package device

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type allocator struct {
	nodeInfo *NodeInfo
}

func NewAllocator(n *NodeInfo) *allocator {
	return &allocator{
		nodeInfo: n,
	}
}

// Allocate tries to find a suitable GPU device for containers
// and records some data in pod's annotation
func (alloc *allocator) Allocate(pod *corev1.Pod) (*corev1.Pod, error) {
	klog.V(4).Infof("Attempt to allocate pod <%s/%s> on node <%s>",
		pod.Namespace, pod.Name, alloc.nodeInfo.GetName())
	newPod := pod.DeepCopy()
	var podAssignDevices PodDevices
	for i := range newPod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		// Skip containers that do not request vGPU.
		if !util.IsVGPURequiredContainer(container) {
			continue
		}
		assignDevices, err := alloc.allocateOne(newPod, container)
		if err != nil {
			klog.V(4).Infof("Container <%s> device allocation failed: %v", container.Name, err)
			return nil, err
		}
		podAssignDevices = append(podAssignDevices, *assignDevices)
	}
	preAlloc, err := podAssignDevices.MarshalText()
	if err != nil {
		return nil, fmt.Errorf("pod assign devices encoding failed: %v", err)
	}
	util.InsertAnnotation(newPod, util.PodVGPUPreAllocAnnotation, preAlloc)
	util.InsertAnnotation(newPod, util.PodPredicateNodeAnnotation, alloc.nodeInfo.GetName())
	return newPod, nil
}

func (alloc *allocator) allocateOne(pod *corev1.Pod, container *corev1.Container) (*ContainerDevices, error) {
	klog.V(4).Infof("Attempt to allocate container <%s> on node <%s>",
		container.Name, alloc.nodeInfo.GetName())
	node := alloc.nodeInfo.GetNode()
	needNumber := util.GetResourceOfContainer(container, util.VGPUNumberResourceName)
	needCores := util.GetResourceOfContainer(container, util.VGPUCoreResourceName)
	needMemory := util.GetResourceOfContainer(container, util.VGPUMemoryResourceName)
	tmpStore := make([]*DeviceInfo, 0, alloc.nodeInfo.GetDeviceCount())
	if needNumber > alloc.nodeInfo.GetDeviceCount() {
		return nil, fmt.Errorf("no enough GPU number on node %s", node.GetName())
	}
	// Calculate the actual requested memory size based on the node memory factor.
	if needMemory > 0 {
		memFactor := node.Annotations[util.DeviceMemoryFactorAnnotation]
		factor, _ := strconv.Atoi(memFactor)
		needMemory *= factor
	}
	for i := range alloc.nodeInfo.GetDeviceMap() {
		deviceInfo := alloc.nodeInfo.GetDeviceMap()[i]
		// Filter unhealthy device.
		if !deviceInfo.Healthy() {
			klog.V(4).Infof("Filter unhealthy device %d", i)
			continue
		}
		// Filter MIG enabled device.
		if deviceInfo.IsMIG() {
			klog.V(4).Infof("Filter MIG or Mig's parent device %d", i)
			continue
		}
		tmpStore = append(tmpStore, deviceInfo)
	}

	var (
		claimDevices []ClaimDevice
		err          error
	)

	// Sort the devices according to the device scheduling strategy.
	devPolicy, _ := util.HasAnnotation(pod, util.DeviceSchedulerPolicyAnnotation)
	switch strings.ToLower(devPolicy) {
	case string(util.BinpackPolicy):
		klog.V(4).Infof("Pod <%s/%s> use <%s> node scheduling strategy", pod.Namespace, pod.Name, devPolicy)
		if numaDevices, ok := CanNotCrossNumaNode(needNumber, tmpStore); ok {
			klog.V(3).Infoln("Try using NUMA allocation mode")
			numaDevices.NumaScoreBinpackCallback(func(numaNode int, devices []*DeviceInfo) bool {
				// Filter numa nodes with insufficient number of devices.
				if needNumber > len(devices) {
					return false
				}
				klog.V(4).Infof("Currently allocating devices on numa node %d", numaNode)
				NewDeviceBinpackPriority().Sort(devices)
				claimDevices, err = allocateDevices(devices, pod, node.GetName(), needNumber, needCores, needMemory)
				return err == nil
			})
			if err == nil {
				goto DONE
			}
			// When there is only one numa, quickly return an error
			if len(numaDevices) == 1 {
				return nil, err
			}
			klog.Warningf("NUMA node allocation failed, fallback to normal resource allocation mode")
		}
		NewDeviceBinpackPriority().Sort(tmpStore)
	case string(util.SpreadPolicy):
		klog.V(4).Infof("Pod <%s/%s> use <%s> node scheduling strategy", pod.Namespace, pod.Name, devPolicy)
		if numaDevices, ok := CanNotCrossNumaNode(needNumber, tmpStore); ok {
			klog.V(3).Infoln("Try using NUMA allocation mode")
			numaDevices.NumaScoreSpreadCallback(func(numaNode int, devices []*DeviceInfo) bool {
				// Filter numa nodes with insufficient number of devices.
				if needNumber > len(devices) {
					return false
				}
				klog.V(4).Infof("Currently allocating devices on numa node %d", numaNode)
				NewDeviceSpreadPriority().Sort(devices)
				claimDevices, err = allocateDevices(devices, pod, node.GetName(), needNumber, needCores, needMemory)
				return err == nil
			})
			if err == nil {
				goto DONE
			}
			// When there is only one numa, quickly return an error
			if len(numaDevices) == 1 {
				return nil, err
			}
			klog.Warningf("NUMA node allocation failed, fallback to normal resource allocation mode")
		}
		NewDeviceSpreadPriority().Sort(tmpStore)
	default:
		klog.V(4).Infof("Pod <%s/%s> no device scheduling strategy", pod.Namespace, pod.Name)
		NewSortPriority(ByNumaAsc, ByDeviceIdAsc).Sort(tmpStore)
	}
	claimDevices, err = allocateDevices(tmpStore, pod, node.GetName(), needNumber, needCores, needMemory)
	if err != nil {
		return nil, err
	}

DONE:
	if len(claimDevices) == 0 {
		return nil, fmt.Errorf("insufficient GPU on node %s", node.GetName())
	}
	assignDevice := &ContainerDevices{Name: container.Name, Devices: claimDevices}
	sort.Slice(assignDevice.Devices, func(i, j int) bool {
		devA := assignDevice.Devices[i]
		devB := assignDevice.Devices[j]
		return devA.Id < devB.Id
	})
	return assignDevice, nil
}

func allocateDevices(tmpStore []*DeviceInfo, pod *corev1.Pod, nodeName string, needNumber, needCores, needMemory int) ([]ClaimDevice, error) {
	var devices []ClaimDevice
	for i, deviceInfo := range tmpStore {
		if needNumber == 0 {
			break
		}
		// Filter for insufficient number of virtual devices.
		if deviceInfo.AllocatableNumber() == 0 {
			klog.V(4).Infof("current gpu device %d insufficient available number, skip allocation", i)
			continue
		}
		var reqMemory = needMemory
		// When there is no defined request for memory,
		// it occupies the entire card memory.
		if reqMemory == 0 {
			reqMemory = deviceInfo.GetTotalMemory()
		}
		if reqMemory > deviceInfo.AllocatableMemory() {
			klog.V(4).Infof("current gpu device %d insufficient available memory, skip allocation", i)
			continue
		}
		if needCores > deviceInfo.AllocatableCores() {
			klog.V(4).Infof("current gpu device %d insufficient available core, skip allocation", i)
			continue
		}
		if needCores == util.HundredCore && deviceInfo.AllocatableCores() < util.HundredCore {
			klog.V(4).Infof("current gpu device %d insufficient available core, skip allocation", i)
			continue
		}
		// Filter device type.
		if !util.CheckDeviceType(pod.Annotations, deviceInfo.GetType()) {
			klog.V(4).Infof("current gpu device <%d> type <%s> non compliant annotation[%s], skip allocation", i,
				deviceInfo.GetType(), fmt.Sprintf("'%s' or '%s'", util.PodIncludeGpuTypeAnnotation, util.PodExcludeGpuTypeAnnotation))
			continue
		}
		// Filter device uuid.
		if !util.CheckDeviceUuid(pod.Annotations, deviceInfo.GetUUID()) {
			klog.V(4).Infof("current gpu device <%d> type <%s> non compliant annotation[%s], skip allocation", i,
				deviceInfo.GetType(), fmt.Sprintf("'%s' or '%s'", util.PodIncludeGPUUUIDAnnotation, util.PodExcludeGPUUUIDAnnotation))
			continue
		}
		devices = append(devices, ClaimDevice{
			Id:     deviceInfo.GetID(),
			Uuid:   deviceInfo.GetUUID(),
			Cores:  needCores,
			Memory: reqMemory,
		})
		needNumber--
	}
	if needNumber > 0 {
		return nil, fmt.Errorf("insufficient GPU on node %s", nodeName)
	}
	return devices, nil
}
