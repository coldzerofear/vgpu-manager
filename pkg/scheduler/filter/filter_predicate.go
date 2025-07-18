package filter

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/allocator"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/predicate"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

type gpuFilter struct {
	kubeClient kubernetes.Interface
	nodeLister listerv1.NodeLister
	podLister  listerv1.PodLister
	recorder   record.EventRecorder
}

const (
	Name = "FilterPredicate"
)

var _ predicate.FilterPredicate = &gpuFilter{}

func New(client kubernetes.Interface, factory informers.SharedInformerFactory, recorder record.EventRecorder) (*gpuFilter, error) {
	podInformer := factory.Core().V1().Pods().Informer()
	nodeInformer := factory.Core().V1().Nodes().Informer()
	podLister := listerv1.NewPodLister(podInformer.GetIndexer())
	nodeLister := listerv1.NewNodeLister(nodeInformer.GetIndexer())

	return &gpuFilter{
		kubeClient: client,
		nodeLister: nodeLister,
		podLister:  podLister,
		recorder:   recorder,
	}, nil
}

func (f *gpuFilter) Name() string {
	return Name
}

type filterFunc func(*corev1.Pod, []corev1.Node) ([]corev1.Node, extenderv1.FailedNodesMap, error)

func (f *gpuFilter) Filter(_ context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
	klog.V(4).InfoS("FilterNode", "ExtenderArgs", args)
	if args.Pod == nil {
		return &extenderv1.ExtenderFilterResult{
			Error: "Parameter Pod is empty",
		}
	}
	if !util.IsVGPUResourcePod(args.Pod) {
		klog.V(5).InfoS("Skip pods that do not request vGPU", "pod", klog.KObj(args.Pod))
		return &extenderv1.ExtenderFilterResult{
			Nodes:     args.Nodes,
			NodeNames: args.NodeNames,
		}
	}

	var (
		nodeCache      bool
		filteredNodes  []corev1.Node
		failedNodesMap extenderv1.FailedNodesMap
	)
	switch {
	case args.NodeNames != nil && len(*args.NodeNames) > 0:
		nodeCache = true
		filteredNodes, failedNodesMap = f.getNodesOnCache(*args.NodeNames...)
	case args.Nodes != nil && len(args.Nodes.Items) > 0:
		filteredNodes = args.Nodes.Items
		failedNodesMap = make(extenderv1.FailedNodesMap)
	default:
		return &extenderv1.ExtenderFilterResult{
			Nodes:     args.Nodes,
			NodeNames: args.NodeNames,
			Error:     "No schedulable nodes",
		}
	}

	filters := []filterFunc{
		f.nodeFilter,
		f.deviceFilter,
	}

	for i, filter := range filters {
		passedNodes, failedNodes, err := filter(args.Pod, filteredNodes)
		if err != nil {
			klog.Errorf("Filter %d (%T) call failed: %v", i, filter, err)
			return &extenderv1.ExtenderFilterResult{Error: err.Error()}
		}
		// Change the latest node filtering list for the next round of filtering.
		filteredNodes = passedNodes
		for name, reason := range failedNodes {
			failedNodesMap[name] = reason
		}
	}
	var (
		nodes     *corev1.NodeList
		nodeNames *[]string
	)
	if nodeCache {
		temp := make([]string, len(filteredNodes))
		for i, node := range filteredNodes {
			temp[i] = node.Name
		}
		nodeNames = &temp
	} else {
		nodes = &corev1.NodeList{Items: filteredNodes}
	}

	return &extenderv1.ExtenderFilterResult{
		Nodes:       nodes,
		NodeNames:   nodeNames,
		FailedNodes: failedNodesMap,
	}
}

func (f *gpuFilter) getNodesOnCache(nodeNames ...string) ([]corev1.Node, extenderv1.FailedNodesMap) {
	failedNodesMap := make(extenderv1.FailedNodesMap)
	filteredNodes := make([]corev1.Node, 0, len(nodeNames))
	for _, nodeName := range nodeNames {
		if node, err := f.nodeLister.Get(nodeName); err != nil {
			errMsg := "get node cache failed"
			klog.ErrorS(err, errMsg, "node", nodeName)
			failedNodesMap[nodeName] = errMsg
		} else {
			filteredNodes = append(filteredNodes, *node)
		}
	}
	return filteredNodes, failedNodesMap
}

// TODO nodeFilter Filter nodes with heartbeat timeout and certain configuration errors
func (f *gpuFilter) nodeFilter(pod *corev1.Pod, nodes []corev1.Node) ([]corev1.Node, extenderv1.FailedNodesMap, error) {
	var (
		filteredNodes    = make([]corev1.Node, 0, len(nodes)) // Successful nodes
		failedNodesMap   = make(extenderv1.FailedNodesMap)    // Failed nodes
		memoryPolicyFunc = func(info *device.NodeConfigInfo) error {
			return nil
		}
	)

	policy, _ := util.HasAnnotation(pod, util.MemorySchedulerPolicyAnnotation)
	switch strings.ToLower(policy) {
	case string(util.VirtualMemoryPolicy), "virt":
		klog.V(4).Infof("Pod <%s> use <%s> memory scheduling policy", klog.KObj(pod), util.VirtualMemoryPolicy)
		memoryPolicyFunc = func(info *device.NodeConfigInfo) error {
			if info.MemoryScaling <= 1 {
				return fmt.Errorf("node GPU use physical memory")
			}
			return nil
		}
	case string(util.PhysicalMemoryPolicy), "phy":
		klog.V(4).Infof("Pod <%s> use <%s> memory scheduling policy", klog.KObj(pod), util.PhysicalMemoryPolicy)
		memoryPolicyFunc = func(info *device.NodeConfigInfo) error {
			if info.MemoryScaling > 1 {
				return fmt.Errorf("node GPU use virtual memory")
			}
			return nil
		}
	}
	for i, node := range nodes {
		heartbeat, ok := util.HasAnnotation(&node, util.NodeDeviceHeartbeatAnnotation)
		if !ok || len(heartbeat) == 0 {
			failedNodesMap[node.Name] = "node without device heartbeat"
			continue
		}
		heartbeatTime := metav1.MicroTime{}
		if err := heartbeatTime.UnmarshalText([]byte(heartbeat)); err != nil {
			failedNodesMap[node.Name] = "node with incorrect heartbeat timestamp"
			continue
		}
		if time.Since(heartbeatTime.Local()) > 2*time.Minute {
			failedNodesMap[node.Name] = "node with heartbeat timeout"
			continue
		}
		configInfoStr, ok := util.HasAnnotation(&node, util.NodeConfigInfoAnnotation)
		if !ok || len(configInfoStr) == 0 {
			failedNodesMap[node.Name] = "node with empty configuration information"
			continue
		}
		nodeConfigInfo := device.NodeConfigInfo{}
		if err := nodeConfigInfo.Decode(configInfoStr); err != nil {
			klog.V(3).ErrorS(err, "decoding node configuration information failed", "node", node.Name)
			failedNodesMap[node.Name] = "node with incorrect configuration information"
			continue
		}
		if nodeConfigInfo.DeviceSplit <= 0 {
			failedNodesMap[node.Name] = "node without GPU device"
			continue
		}
		if nodeConfigInfo.MemoryFactor <= 0 {
			failedNodesMap[node.Name] = "node with incorrect GPU memory factor"
			continue
		}
		if err := memoryPolicyFunc(&nodeConfigInfo); err != nil {
			failedNodesMap[node.Name] = err.Error()
			continue
		}
		filteredNodes = append(filteredNodes, nodes[i])
	}
	return filteredNodes, failedNodesMap, nil
}

func (f *gpuFilter) CheckDeviceRequest(pod *corev1.Pod) error {
	for _, container := range pod.Spec.Containers {
		if err := checkCoreRequest(&container); err != nil {
			f.recorder.Event(pod, corev1.EventTypeWarning, "ResourceError", err.Error())
			return err
		}
		if err := checkNumberRequest(&container); err != nil {
			f.recorder.Event(pod, corev1.EventTypeWarning, "ResourceError", err.Error())
			return err
		}
	}
	return nil
}

func checkNumberRequest(container *corev1.Container) error {
	if util.GetResourceOfContainer(container, util.VGPUNumberResourceName) > util.MaxDeviceNumber {
		return fmt.Errorf("container %s requests vGPU number exceeding limit", container.Name)
	}
	return nil
}

func checkCoreRequest(container *corev1.Container) error {
	if util.GetResourceOfContainer(container, util.VGPUCoreResourceName) > util.HundredCore {
		return fmt.Errorf("container %s requests vGPU core exceeding limit", container.Name)
	}
	return nil
}

func IsScheduled(pod *corev1.Pod) bool {
	nodeName, ok := util.HasAnnotation(pod, util.PodPredicateNodeAnnotation)
	if !ok || len(nodeName) == 0 {
		return false
	}
	preAlloc, ok := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	if !ok || len(preAlloc) == 0 {
		return false
	}
	podDevices := device.PodDevices{}
	err := podDevices.UnmarshalText(preAlloc)
	return err == nil
}

// deviceFilter will choose one and only one node fullfil the request,
// so it should always be the last filter of gpuFilter
func (f *gpuFilter) deviceFilter(pod *corev1.Pod, nodes []corev1.Node) ([]corev1.Node, extenderv1.FailedNodesMap, error) {
	var (
		mu             sync.Mutex
		filteredNodes  = make([]corev1.Node, 0, 1)       // Successful nodes
		failedNodesMap = make(extenderv1.FailedNodesMap) // Failed nodes
		nodeInfoList   []*device.NodeInfo
		success        bool
	)
	// Skip pods that have already been scheduled.
	if IsScheduled(pod) {
		return filteredNodes, failedNodesMap, fmt.Errorf("pod %s had been predicated", pod.UID)
	}

	if err := f.CheckDeviceRequest(pod); err != nil {
		klog.ErrorS(err, "Check device request failed", "pod", klog.KObj(pod))
		return filteredNodes, failedNodesMap, err
	}

	pods, err := f.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("PodLister get all pod failed: %v", err)
		return filteredNodes, failedNodesMap, err
	}
	nodeOriginalPosition := map[string]int{}
	wait := sync.WaitGroup{}
	for index := range nodes {
		node := &nodes[index]
		wait.Add(1)
		go func(node *corev1.Node, pods []*corev1.Pod) {
			defer wait.Done()
			_, ok := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
			if !ok || !util.IsVGPUEnabledNode(node) {
				klog.V(3).InfoS("node has not registered any GPU devices, skipping it", "node", node.Name)
				mu.Lock()
				failedNodesMap[node.Name] = "node without GPU device"
				mu.Unlock()
				return
			}
			// Pods on aggregation nodes.
			nodePods := CollectPodsOnNode(pods, node)
			nodeInfo, err := device.NewNodeInfo(node, nodePods)
			if err != nil {
				klog.ErrorS(err, "Create node info failed, skipping node", "node", node.Name)
				mu.Lock()
				failedNodesMap[node.Name] = err.Error()
				mu.Unlock()
				return
			}
			mu.Lock()
			nodeInfoList = append(nodeInfoList, nodeInfo)
			mu.Unlock()
		}(node, pods)
		nodeOriginalPosition[node.Name] = index
	}
	wait.Wait()

	// Sort nodes according to node scheduling strategy.
	nodePolicy, _ := util.HasAnnotation(pod, util.NodeSchedulerPolicyAnnotation)
	switch strings.ToLower(nodePolicy) {
	case string(util.BinpackPolicy):
		klog.V(4).Infof("Pod <%s> use <%s> node scheduling policy", klog.KObj(pod), nodePolicy)
		allocator.NewNodeBinpackPriority(needGPUTopology(pod)).Sort(nodeInfoList)
	case string(util.SpreadPolicy):
		klog.V(4).Infof("Pod <%s> use <%s> node scheduling policy", klog.KObj(pod), nodePolicy)
		allocator.NewNodeSpreadPriority(needGPUTopology(pod)).Sort(nodeInfoList)
	default:
		klog.V(4).Infof("Pod <%s> no node scheduling policy", klog.KObj(pod))
		less := []allocator.LessFunc[*device.NodeInfo]{func(p1, p2 *device.NodeInfo) bool {
			return nodeOriginalPosition[p1.GetName()] < nodeOriginalPosition[p2.GetName()]
		}}
		if needGPUTopology(pod) {
			less = slices.Insert[[]allocator.LessFunc[*device.NodeInfo],
				allocator.LessFunc[*device.NodeInfo]](less, 0, allocator.ByNodeGPUTopology)
		}
		allocator.NewSortPriority[*device.NodeInfo](less...).Sort(nodeInfoList)
	}

	for _, nodeInfo := range nodeInfoList {
		node := nodeInfo.GetNode()
		if success {
			failedNodesMap[node.Name] = fmt.Sprintf("pod %s has already been matched to another node", pod.UID)
			continue
		}
		// Attempt to allocate devices for pods on this node.
		newPod, err := allocator.NewAllocator(nodeInfo).Allocate(pod)
		if err != nil {
			klog.ErrorS(err, "node device allocate failed", "node", node.Name, "pod", klog.KObj(pod))
			failedNodesMap[node.Name] = err.Error()
			continue
		}
		if err = client.PatchPodVGPUAnnotation(f.kubeClient, newPod); err != nil {
			errMsg := fmt.Sprintf("patch vGPU metadata failed")
			klog.ErrorS(err, errMsg, "pod", klog.KObj(pod))
			failedNodesMap[node.Name] = errMsg
			continue
		}
		filteredNodes = append(filteredNodes, *node)
		success = true
	}

	return filteredNodes, failedNodesMap, nil
}

func needGPUTopology(pod *corev1.Pod) bool {
	if device.IsGPUTopologyEnabled() {
		topoMode, _ := util.HasAnnotation(pod, util.DeviceTopologyModeAnnotation)
		return strings.EqualFold(topoMode, string(util.LinkTopology)) && util.IsSingleContainerMultiGPUs(pod)
	}
	return false
}

func CollectPodsOnNode(pods []*corev1.Pod, node *corev1.Node) []*corev1.Pod {
	klog.V(5).Infof("Collect pods on node <%s>", node.Name)
	var ret []*corev1.Pod
	for _, pod := range pods {
		var predicateNode string
		if pod.Spec.NodeName == "" {
			predicateNode, _ = util.HasAnnotation(pod, util.PodPredicateNodeAnnotation)
		}
		if (pod.Spec.NodeName == node.Name || predicateNode == node.Name) &&
			pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			ret = append(ret, pod)
			klog.V(5).Infof("Append Pod <%s> on node <%s>", klog.KObj(pod), node.Name)
		}
	}
	return ret
}
