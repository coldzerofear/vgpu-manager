package device

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

type allocator struct {
	kubeClient kubernetes.Interface
	nodeInfo   *NodeInfo
}

func NewAllocator(k kubernetes.Interface, n *NodeInfo) *allocator {
	return &allocator{
		kubeClient: k,
		nodeInfo:   n,
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
		err = fmt.Errorf("pod assign devices encoding failed: %v", err)
		klog.Errorln(err)
		return nil, err
	}
	// patch vGPU annotations.
	patchData := client.PatchMetadata{Annotations: map[string]string{}}
	patchData.Annotations[util.PodPredicateNodeAnnotation] = alloc.nodeInfo.GetName()
	patchData.Annotations[util.PodVGPUPreAllocAnnotation] = preAlloc
	err = retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return client.PatchPodMetadata(alloc.kubeClient, newPod, patchData)
	})
	if err != nil {
		err = fmt.Errorf("patch pod vgpu metadata failed: %v", err)
		klog.Errorln(err)
		return nil, err
	}
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
		return nil, fmt.Errorf("no enough gpu number on node %s", node.GetName())
	}
	if needMemory > 0 {
		memFactor := node.Annotations[util.NodeDeviceMemoryFactor]
		factor, _ := strconv.Atoi(memFactor)
		needMemory *= factor
	}
	for i, _ := range alloc.nodeInfo.GetDeviceMap() {
		tmpStore = append(tmpStore, alloc.nodeInfo.GetDeviceMap()[i])
	}
	// Sort the devices according to the device scheduling strategy.
	devPolicy, _ := util.HasAnnotation(pod, util.DeviceSchedulerPolicyAnnotation)
	switch devPolicy {
	case string(util.BinpackPolicy):
		klog.V(4).Infof("Pod <%s/%s> use <%s> node scheduling strategy", pod.Namespace, pod.Name, devPolicy)
		NewDeviceBinpackPriority(needNumber).Sort(tmpStore)
	case string(util.SpreadPolicy):
		klog.V(4).Infof("Pod <%s/%s> use <%s> node scheduling strategy", pod.Namespace, pod.Name, devPolicy)
		NewDeviceSpreadPriority(needNumber).Sort(tmpStore)
	default:
		klog.V(4).Infof("Pod <%s/%s> no device scheduling strategy", pod.Namespace, pod.Name)
		NewSortPriority(ByDeviceIdAsc).Sort(tmpStore)
	}

	assignDevice := &ContainerDevices{Name: container.Name}
	for i, deviceInfo := range tmpStore {
		if needNumber == 0 {
			break
		}
		// TODO 跳过不健康设备的分配
		if !deviceInfo.Healthy() {
			klog.V(4).Infof("current gpu device %d it's unhealthy, skip allocation", i)
			continue
		}
		// TODO 过滤掉开启了mig的设备
		if deviceInfo.Mig() {
			klog.V(4).Infof("current gpu device %d enabled mig mode, skip allocation", i)
			continue
		}
		// TODO 过滤数量不足的设备
		if deviceInfo.AllocatableNumber() == 0 {
			klog.V(4).Infof("current gpu device %d insufficient available number, skip allocation", i)
			continue
		}
		// TODO 没有定义memory大小时默认占用GPU所有内存
		var reqMemory = needMemory
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
		// TODO 设备类型过滤
		if !util.CheckDeviceType(pod.Annotations, deviceInfo.GetType()) {
			klog.V(4).Infof("current gpu device <%d> type <%s> non compliant annotation[%s], skip allocation", i,
				deviceInfo.GetType(), fmt.Sprintf("'%s' or '%s'", util.PodIncludeGpuTypeAnnotation, util.PodExcludeGpuTypeAnnotation))
			continue
		}
		// TODO 设备uuid过滤
		if !util.CheckDeviceUuid(pod.Annotations, deviceInfo.GetUUID()) {
			klog.V(4).Infof("current gpu device <%d> type <%s> non compliant annotation[%s], skip allocation", i,
				deviceInfo.GetType(), fmt.Sprintf("'%s' or '%s'", util.PodIncludeGPUUUIDAnnotation, util.PodExcludeGPUUUIDAnnotation))
			continue
		}
		assignDevice.Devices = append(assignDevice.Devices, ClaimDevice{
			Id:     deviceInfo.GetID(),
			Uuid:   deviceInfo.GetUUID(),
			Core:   needCores,
			Memory: reqMemory,
		})
		needNumber--
	}
	if needNumber > 0 {
		return nil, fmt.Errorf("insufficient gpu on node %s", node.GetName())
	}
	sort.Slice(assignDevice.Devices, func(i, j int) bool {
		devA := assignDevice.Devices[i]
		devB := assignDevice.Devices[j]
		return devA.Id < devB.Id
	})
	return assignDevice, nil
}
