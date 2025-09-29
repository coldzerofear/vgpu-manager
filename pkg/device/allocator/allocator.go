package allocator

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

type allocator struct {
	nodeInfo *device.NodeInfo
	recorder record.EventRecorder
}

func NewAllocator(nodeInfo *device.NodeInfo, recorder record.EventRecorder) *allocator {
	return &allocator{
		nodeInfo: nodeInfo,
		recorder: recorder,
	}
}

func (alloc *allocator) addAllocateOne(contDevices *device.ContainerDevices) error {
	for _, d := range contDevices.Devices {
		err := alloc.nodeInfo.AddUsedResources(d.Id, d.Cores, d.Memory)
		if err != nil {
			return err
		}
	}
	return nil
}

// Allocate tries to find a suitable GPU device for containers
// and records some data in pod's annotation
func (alloc *allocator) Allocate(pod *corev1.Pod) (*corev1.Pod, error) {
	klog.V(4).Infof("Attempt to allocate pod <%s> on node <%s>", klog.KObj(pod), alloc.nodeInfo.GetName())
	newPod := pod.DeepCopy()
	var podAssignDevices device.PodDevices
	for i := range newPod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		// Skip containers that do not request vGPU.
		if !util.IsVGPURequiredContainer(container) {
			continue
		}
		assignDevices, err := alloc.allocateOne(newPod, container)
		if err != nil {
			klog.V(4).InfoS(err.Error(), "node", alloc.nodeInfo.GetName(),
				"pod", klog.KObj(pod), "container", container.Name)
			return nil, err
		}
		if err = alloc.addAllocateOne(assignDevices); err != nil {
			klog.V(4).InfoS(err.Error(), "node", alloc.nodeInfo.GetName(),
				"pod", klog.KObj(pod), "container", container.Name)
			return nil, fmt.Errorf("internal device scheduling error")
		}
		podAssignDevices = append(podAssignDevices, *assignDevices)
	}
	preAlloc, err := podAssignDevices.MarshalText()
	if err != nil {
		klog.V(4).InfoS(fmt.Sprintf("assign devices encoding failed: %v", err),
			"node", alloc.nodeInfo.GetName(), "pod", klog.KObj(pod))
		return nil, fmt.Errorf("assign devices encoding failed")
	}
	util.InsertAnnotation(newPod, util.PodVGPUPreAllocAnnotation, preAlloc)
	util.InsertAnnotation(newPod, util.PodPredicateNodeAnnotation, alloc.nodeInfo.GetName())
	return newPod, nil
}

func getDeviceUUIDs(devices []*device.Device) []string {
	uuids := make([]string, len(devices))
	for i, d := range devices {
		uuids[i] = d.GetUUID()
	}
	return uuids
}

func (alloc *allocator) allocateOne(pod *corev1.Pod, container *corev1.Container) (*device.ContainerDevices, error) {
	klog.V(4).Infof("Attempt to allocate container <%s> on node <%s>", container.Name, alloc.nodeInfo.GetName())
	node := alloc.nodeInfo.GetNode()
	needNumber := int(util.GetResourceOfContainer(container, util.VGPUNumberResourceName))
	needCores := util.GetResourceOfContainer(container, util.VGPUCoreResourceName)
	needMemory := util.GetResourceOfContainer(container, util.VGPUMemoryResourceName)
	if needNumber > alloc.nodeInfo.GetDeviceCount() {
		return nil, fmt.Errorf("no enough GPU number on node")
	}
	// Calculate the actual requested memory size based on the node memory factor.
	if needMemory > 0 {
		nodeConfigInfo := device.NodeConfigInfo{}
		err := nodeConfigInfo.Decode(node.Annotations[util.NodeConfigInfoAnnotation])
		if err != nil {
			return nil, fmt.Errorf("decoding node configuration information failed: %v", err)
		}
		needMemory *= int64(nodeConfigInfo.MemoryFactor)
	}
	if needCores == 0 && needMemory == 0 {
		needCores = util.HundredCore
	}
	var (
		claimDevices []device.ClaimDevice
		devicePolicy string
	)
	deviceStore := filterDevices(alloc.nodeInfo.GetDeviceMap(), pod, node.GetName(), needCores, needMemory)
	if needNumber > len(deviceStore) {
		return nil, fmt.Errorf("insufficient GPU on node")
	} else if needNumber == len(deviceStore) {
		claimDevices = allocateByNumbers(deviceStore, needNumber, needCores, needMemory)
		goto DONE
	}
	// Sort the devices according to the device scheduling strategy.
	devicePolicy, _ = util.HasAnnotation(pod, util.DeviceSchedulerPolicyAnnotation)
	switch policy := strings.ToLower(devicePolicy); policy {
	case string(util.BinpackPolicy):
		klog.V(4).Infof("Pod <%s/%s> use <%s> node scheduling policy", pod.Namespace, pod.Name, policy)
		NewDeviceBinpackPriority().Sort(deviceStore)
		claimDevices = alloc.allocateByTopologyMode(pod, deviceStore, util.BinpackPolicy, needNumber, needCores, needMemory)
	case string(util.SpreadPolicy):
		klog.V(4).Infof("Pod <%s/%s> use <%s> node scheduling policy", pod.Namespace, pod.Name, policy)
		NewDeviceSpreadPriority().Sort(deviceStore)
		claimDevices = alloc.allocateByTopologyMode(pod, deviceStore, util.SpreadPolicy, needNumber, needCores, needMemory)
	default:
		if policy == "" || policy == string(util.NonePolicy) {
			klog.V(4).Infof("Pod <%s/%s> none device scheduling policy", pod.Namespace, pod.Name)
		} else {
			klog.V(4).Infof("Pod <%s/%s> not supported device scheduling policy: %s", pod.Namespace, pod.Name, devicePolicy)
			alloc.sendEventf(pod, corev1.EventTypeWarning, "DevicePolicy", "Unsupported device scheduling policy '%s'", devicePolicy)
		}
		NewSortPriority(ByNuma, ByDeviceIdAsc).Sort(deviceStore)
		claimDevices = alloc.allocateByTopologyMode(pod, deviceStore, util.NonePolicy, needNumber, needCores, needMemory)
	}
DONE:
	if len(claimDevices) != needNumber {
		return nil, fmt.Errorf("insufficient GPU on node")
	}
	assignDevice := &device.ContainerDevices{Name: container.Name, Devices: claimDevices}
	sort.Slice(assignDevice.Devices, func(i, j int) bool {
		return assignDevice.Devices[i].Id < assignDevice.Devices[j].Id
	})
	return assignDevice, nil
}

func (alloc *allocator) sendEventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	if alloc.recorder != nil {
		alloc.recorder.Eventf(object, eventtype, reason, messageFmt, args)
	}
}

func (alloc *allocator) allocateByTopologyMode(pod *corev1.Pod, deviceStore []*device.Device, policy util.SchedulerPolicy, needNumber int, needCores, needMemory int64) []device.ClaimDevice {
	if needNumber > 1 {
		topologyMode, _ := util.HasAnnotation(pod, util.DeviceTopologyModeAnnotation)
		switch strings.ToLower(topologyMode) {
		case string(util.LinkTopology):
			klog.V(4).Infof("Pod <%s/%s> use Links topology mode", pod.Namespace, pod.Name)
			if alloc.nodeInfo.HasGPUTopology() {
				devices, _ := alloc.nodeInfo.GetDeviceList().Filter(getDeviceUUIDs(deviceStore))
				devices = gpuallocator.NewBestEffortPolicy().Allocate(devices, nil, needNumber)
				if len(devices) == needNumber {
					return allocateByDevices(deviceStore, devices, needCores, needMemory)
				}
				klog.Warningf("LinkTopology allocation failed, fallback to normal allocation mode")
			} else {
				klog.V(3).InfoS("Current node does not have GPU topology information, fallback to normal allocation mode",
					"node", alloc.nodeInfo.GetName(), "pod", klog.KObj(pod))
			}
		case string(util.NUMATopology):
			klog.V(4).Infof("Pod <%s/%s> use NUMA topology mode", pod.Namespace, pod.Name)
			if alloc.nodeInfo.HasNUMATopology() {
				numaNode, notCrossNuma := CanNotCrossNumaNode(needNumber, deviceStore)
				if notCrossNuma {
					var claimDevices []device.ClaimDevice
					callbackFunc := func(_ int, devices []*device.Device) bool {
						// Filter numa nodes with insufficient number of devices.
						if needNumber > len(devices) {
							return false
						}
						claimDevices = allocateByNumbers(devices, needNumber, needCores, needMemory)
						return true
					}
					numaNode.SchedulerPolicyCallback(policy, callbackFunc)
					if len(claimDevices) == needNumber {
						return claimDevices
					}
					klog.Warningf("NUMA node allocation failed, fallback to normal resource allocation mode")
				} else {
					klog.Warningf("NUMA node does not meet the request, fallback to normal resource allocation mode")
				}
			} else {
				klog.V(3).InfoS("Current node does not have NUMA topology information, fallback to normal allocation mode",
					"node", alloc.nodeInfo.GetName(), "pod", klog.KObj(pod))
			}
		case "", string(util.NoneTopology):
			klog.V(4).Infof("Pod <%s/%s> none topology mode", pod.Namespace, pod.Name)
		default:
			klog.V(4).Infof("Pod <%s/%s> not supported topology mode: %s", pod.Namespace, pod.Name, topologyMode)
			alloc.sendEventf(pod, corev1.EventTypeWarning, "DeviceTopologyMode", "Unsupported device topology mode '%s'", topologyMode)
		}
	}
	return allocateByNumbers(deviceStore, needNumber, needCores, needMemory)
}

func allocateByDevices(deviceStore []*device.Device, devices []*gpuallocator.Device, needCores, needMemory int64) []device.ClaimDevice {
	claimDevices := make([]device.ClaimDevice, len(devices))
	for i, dev := range devices {
		reqMemory := needMemory
		// When there is no defined request for memory,
		// it occupies the entire card memory.
		if reqMemory == 0 {
			index := slices.IndexFunc(deviceStore, func(d *device.Device) bool {
				return d.GetUUID() == dev.UUID
			})
			reqMemory = deviceStore[index].GetTotalMemory()
		}
		claimDevices[i] = device.ClaimDevice{
			Id:     dev.Index,
			Uuid:   dev.UUID,
			Cores:  needCores,
			Memory: needMemory,
		}
	}
	return claimDevices
}

func allocateByNumbers(deviceStore []*device.Device, needNumber int, needCores, needMemory int64) []device.ClaimDevice {
	devices := make([]device.ClaimDevice, needNumber)
	for i, deviceInfo := range deviceStore[0:needNumber] {
		reqMemory := needMemory
		// When there is no defined request for memory,
		// it occupies the entire card memory.
		if reqMemory == 0 {
			reqMemory = deviceInfo.GetTotalMemory()
		}
		devices[i] = device.ClaimDevice{
			Id:     deviceInfo.GetID(),
			Uuid:   deviceInfo.GetUUID(),
			Cores:  needCores,
			Memory: reqMemory,
		}
	}
	return devices
}

func filterDevices(deviceMap map[int]*device.Device, pod *corev1.Pod, nodeName string, needCores, needMemory int64) []*device.Device {
	var devices []*device.Device
	for i := range deviceMap {
		deviceInfo := deviceMap[i]
		// Filter unhealthy device.
		if !deviceInfo.Healthy() {
			klog.V(4).Infof("Filter unhealthy device <%d> on the node <%s>", i, nodeName)
			continue
		}
		// Filter MIG enabled device.
		if deviceInfo.IsMIG() {
			klog.V(4).Infof("Filter MIG enabled device <%d> on the node <%s>", i, nodeName)
			continue
		}
		// Filter for insufficient number of virtual devices.
		if deviceInfo.AllocatableNumber() == 0 {
			klog.V(4).Infof("Filter device <%d> insufficient available number on the node <%s>", i, nodeName)
			continue
		}
		reqMemory := needMemory
		// When there is no defined request for memory,
		// it occupies the entire card memory.
		if reqMemory == 0 {
			reqMemory = deviceInfo.GetTotalMemory()
		}
		if reqMemory > deviceInfo.AllocatableMemory() {
			klog.V(4).Infof("Filter device <%d> with insufficient available memory on the node <%s>", i, nodeName)
			continue
		}
		if needCores > deviceInfo.AllocatableCores() || (needCores == util.HundredCore && deviceInfo.AllocatableCores() < util.HundredCore) {
			klog.V(4).Infof("Filter device <%d> with insufficient available cores on the node <%s>", i, nodeName)
			continue
		}
		// Filter device type.
		if !util.CheckDeviceType(pod.Annotations, deviceInfo.GetType()) {
			klog.V(4).Infof("Filter gpu device <%d> type <%s> non compliant annotation[%s]", i,
				deviceInfo.GetType(), fmt.Sprintf("'%s' or '%s'", util.PodIncludeGpuTypeAnnotation, util.PodExcludeGpuTypeAnnotation))
			continue
		}
		// Filter device uuid.
		if !util.CheckDeviceUuid(pod.Annotations, deviceInfo.GetUUID()) {
			klog.V(4).Infof("Filter gpu device <%d> uuid <%s> non compliant annotation[%s]", i,
				deviceInfo.GetUUID(), fmt.Sprintf("'%s' or '%s'", util.PodIncludeGPUUUIDAnnotation, util.PodExcludeGPUUUIDAnnotation))
			continue
		}
		devices = append(devices, deviceInfo)
	}
	return devices
}
