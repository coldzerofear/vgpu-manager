package filter

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/allocator"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/predicate"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/serial"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

type gpuFilter struct {
	locker     *serial.Locker
	kubeClient kubernetes.Interface
	nodeLister listerv1.NodeLister
	podLister  client.PodLister
	recorder   record.EventRecorder
}

const (
	Name                     = "FilterPredicate"
	indexerKeyPodRequestVGPU = "pod.requestVGPU"
	//indexerKeyPodPlanSchedulingNode = "pod.planSchedulingNode"
)

var _ predicate.FilterPredicate = &gpuFilter{}
var podIndexers = cache.Indexers{
	indexerKeyPodRequestVGPU: func(obj interface{}) ([]string, error) {
		if pod, ok := obj.(*corev1.Pod); ok && util.IsVGPUResourcePod(pod) {
			return []string{"true"}, nil
		}
		return []string{"false"}, nil
	},
	//indexerKeyPodPlanSchedulingNode: func(obj interface{}) ([]string, error) {
	//	nodeName := ""
	//	if pod, ok := obj.(*corev1.Pod); ok {
	//		nodeName = util.PodPlanSchedulingNode(pod)
	//	}
	//	return []string{nodeName}, nil
	//},
}

func New(kubeClient kubernetes.Interface, factory informers.SharedInformerFactory,
	recorder record.EventRecorder, serialFilterNode bool) (*gpuFilter, error) {
	podInformer := factory.Core().V1().Pods().Informer()
	nodeInformer := factory.Core().V1().Nodes().Informer()
	if err := podInformer.AddIndexers(podIndexers); err != nil {
		return nil, err
	}
	podLister := client.NewPodLister(podInformer.GetIndexer())
	nodeLister := listerv1.NewNodeLister(nodeInformer.GetIndexer())
	locker := serial.NewLocker(serial.WithName(Name),
		serial.WithEnabled(serialFilterNode))
	return &gpuFilter{
		locker:     locker,
		kubeClient: kubeClient,
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
	filteredNodes := make([]corev1.Node, 0, len(nodeNames))
	failedNodesMap := make(extenderv1.FailedNodesMap, len(nodeNames))
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

func GetMemoryPolicyFunc(pod *corev1.Pod) CheckNodeFunc {
	memoryPolicyFunc := func(node *corev1.Node, info *device.NodeConfigInfo) error {
		return nil
	}
	policy, _ := util.HasAnnotation(pod, util.MemorySchedulerPolicyAnnotation)
	switch strings.ToLower(policy) {
	case string(util.VirtualMemoryPolicy), "virt":
		klog.V(4).Infof("Pod <%s> use <%s> memory scheduling policy", klog.KObj(pod), util.VirtualMemoryPolicy)
		memoryPolicyFunc = func(node *corev1.Node, info *device.NodeConfigInfo) error {
			if info.MemoryScaling <= 1 {
				return fmt.Errorf("node GPU use physical memory")
			}
			return nil
		}
	case string(util.PhysicalMemoryPolicy), "phy":
		klog.V(4).Infof("Pod <%s> use <%s> memory scheduling policy", klog.KObj(pod), util.PhysicalMemoryPolicy)
		memoryPolicyFunc = func(node *corev1.Node, info *device.NodeConfigInfo) error {
			if info.MemoryScaling > 1 {
				return fmt.Errorf("node GPU use virtual memory")
			}
			return nil
		}
	}
	return memoryPolicyFunc
}

type CheckNodeFunc func(node *corev1.Node, info *device.NodeConfigInfo) error

func CheckNode(node *corev1.Node, checkNodeFuncs ...CheckNodeFunc) error {
	heartbeat, ok := util.HasAnnotation(node, util.NodeDeviceHeartbeatAnnotation)
	if !ok || len(heartbeat) == 0 {
		return fmt.Errorf("node without device heartbeat")
	}
	heartbeatTime := metav1.MicroTime{}
	if err := heartbeatTime.UnmarshalText([]byte(heartbeat)); err != nil {
		return fmt.Errorf("node with incorrect heartbeat timestamp")
	}
	if time.Since(heartbeatTime.Local()) > 2*time.Minute {
		return fmt.Errorf("node with heartbeat timeout")
	}
	configInfoStr, ok := util.HasAnnotation(node, util.NodeConfigInfoAnnotation)
	if !ok || len(configInfoStr) == 0 {
		return fmt.Errorf("node with empty configuration information")
	}
	nodeConfigInfo := device.NodeConfigInfo{}
	if err := nodeConfigInfo.Decode(configInfoStr); err != nil {
		klog.V(3).ErrorS(err, "decoding node configuration information failed", "node", node.Name)
		return fmt.Errorf("node with incorrect configuration information")
	}
	if nodeConfigInfo.DeviceSplit <= 0 {
		return fmt.Errorf("node without GPU device")
	}
	if nodeConfigInfo.MemoryFactor <= 0 {
		return fmt.Errorf("node with incorrect GPU memory factor")
	}
	for _, checkFunc := range checkNodeFuncs {
		if err := checkFunc(node, &nodeConfigInfo); err != nil {
			return err
		}
	}
	return nil
}

// nodeFilter Filter nodes with heartbeat timeout and certain configuration errors
func (f *gpuFilter) nodeFilter(pod *corev1.Pod, nodes []corev1.Node) ([]corev1.Node, extenderv1.FailedNodesMap, error) {
	var (
		filteredNodes  = make([]corev1.Node, 0, len(nodes))          // Successful nodes
		failedNodesMap = make(extenderv1.FailedNodesMap, len(nodes)) // Failed nodes
	)
	memoryPolicyFunc := GetMemoryPolicyFunc(pod)
	for i, node := range nodes {
		if err := CheckNode(&node, memoryPolicyFunc); err != nil {
			failedNodesMap[node.Name] = err.Error()
		} else {
			filteredNodes = append(filteredNodes, nodes[i])
		}
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
	if util.GetResourceOfContainer(container, util.VGPUNumberResourceName) > vgpu.MaxDeviceCount {
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

func IsScheduled(pod *corev1.Pod) (string, bool) {
	nodeName, ok := util.HasAnnotation(pod, util.PodPredicateNodeAnnotation)
	if !ok || len(nodeName) == 0 {
		return "", false
	}
	preAlloc, ok := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	if !ok || len(preAlloc) == 0 {
		return "", false
	}
	podDevices := device.PodDevices{}
	err := podDevices.UnmarshalText(preAlloc)
	return nodeName, err == nil
}

// deviceFilter will choose one and only one node fullfil the request, so it should always be the last filter of gpuFilter
func (f *gpuFilter) deviceFilter(pod *corev1.Pod, nodes []corev1.Node) ([]corev1.Node, extenderv1.FailedNodesMap, error) {
	var (
		filteredNodes  = make([]corev1.Node, 0, 1)                   // Successful nodes
		failedNodesMap = make(extenderv1.FailedNodesMap, len(nodes)) // Failed nodes
		success        bool
	)
	// Skip pods that have already been scheduled.
	if nodeName, ok := IsScheduled(pod); ok {
		foundNode := false
		for i, node := range nodes {
			if !foundNode && node.Name == nodeName {
				filteredNodes = append(filteredNodes, nodes[i])
				foundNode = true
				continue
			}
			failedNodesMap[node.Name] = fmt.Sprintf("pod has been scheduled to node %s", node.Name)
		}
		if foundNode {
			return filteredNodes, failedNodesMap, nil
		}
		return nil, nil, fmt.Errorf("pod %s had been predicated", pod.UID)
	}

	if err := f.CheckDeviceRequest(pod); err != nil {
		klog.ErrorS(err, "Check device request failed", "pod", klog.KObj(pod))
		return filteredNodes, failedNodesMap, err
	}

	f.locker.Lock()
	defer f.locker.Unlock()

	pods, err := f.podLister.ListByIndexValue(indexerKeyPodRequestVGPU, "true")
	if err != nil {
		klog.ErrorS(err, "PodLister list all vGPU Pods failed")
		return filteredNodes, failedNodesMap, err
	}

	var (
		mutex                = sync.Mutex{}
		waitGroup            = sync.WaitGroup{}
		nodeInfoList         = make([]*device.NodeInfo, 0, len(nodes))
		nodeOriginalPosition = make(map[string]int, len(nodes))
	)
	maxGoroutines := runtime.NumCPU() * 2
	batchSize := (len(nodes) + maxGoroutines - 1) / maxGoroutines
	batches := watcher.BalanceBatches(len(nodes), batchSize)
	for _, batch := range batches {
		waitGroup.Add(1)
		go func(startIndex, endIndex, count int) {
			defer waitGroup.Done()
			batchNodeInfos := make([]*device.NodeInfo, 0, count)
			batchFailedNodes := make(map[string]string, count)
			batchNodeOrigPosition := make(map[string]int, count)
			for index := startIndex; index <= endIndex; index++ {
				node := &nodes[index]
				batchNodeOrigPosition[node.Name] = index
				_, ok := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
				if !ok || !util.IsVGPUEnabledNode(node) {
					klog.V(3).InfoS("node has not registered any GPU devices, skipping it", "node", node.Name)
					batchFailedNodes[node.Name] = "node without GPU device"
					continue
				}
				nodeInfo, err := device.NewNodeInfo(node, pods)
				if err != nil {
					klog.ErrorS(err, "Create node info failed, skipping node", "node", node.Name)
					batchFailedNodes[node.Name] = err.Error()
					continue
				}
				batchNodeInfos = append(batchNodeInfos, nodeInfo)
			}

			mutex.Lock()
			maps.Copy(failedNodesMap, batchFailedNodes)
			for index := range batchNodeInfos {
				nodeInfoList = append(nodeInfoList, batchNodeInfos[index])
			}
			maps.Copy(nodeOriginalPosition, batchNodeOrigPosition)
			mutex.Unlock()
		}(batch.StartIndex, batch.EndIndex, batch.Count)
	}
	waitGroup.Wait()
	// Quickly return results
	if len(nodeInfoList) == 0 {
		return filteredNodes, failedNodesMap, nil
	}

	// Sort nodes according to node scheduling strategy.
	nodePolicy, _ := util.HasAnnotation(pod, util.NodeSchedulerPolicyAnnotation)
	switch policy := strings.ToLower(nodePolicy); policy {
	case string(util.BinpackPolicy):
		klog.V(4).Infof("Pod <%s> use <%s> node scheduling policy", klog.KObj(pod), policy)
		allocator.NewNodeBinpackPriority(PodUsedGPUTopologyMode(pod)).Sort(nodeInfoList)
	case string(util.SpreadPolicy):
		klog.V(4).Infof("Pod <%s> use <%s> node scheduling policy", klog.KObj(pod), policy)
		allocator.NewNodeSpreadPriority(PodUsedGPUTopologyMode(pod)).Sort(nodeInfoList)
	default:
		if policy == "" || policy == string(util.NonePolicy) {
			klog.V(4).Infof("Pod <%s> no node scheduling policy", klog.KObj(pod))
		} else {
			klog.V(4).Infof("Pod <%s> not supported node scheduling policy: %s", klog.KObj(pod), nodePolicy)
			f.recorder.Eventf(pod, corev1.EventTypeWarning, "NodePolicy", "Unsupported node scheduling policy '%s'", nodePolicy)
		}
		less := []allocator.LessFunc[*device.NodeInfo]{func(p1, p2 *device.NodeInfo) bool {
			return nodeOriginalPosition[p1.GetName()] < nodeOriginalPosition[p2.GetName()]
		}}
		less = allocator.ApplyTopologyMode(PodUsedGPUTopologyMode(pod), less)
		allocator.NewSortPriority[*device.NodeInfo](less...).Sort(nodeInfoList)
	}
	recorder := f.recorder
	for i, nodeInfo := range nodeInfoList {
		node := nodeInfo.GetNode()
		if success {
			failedNodesMap[node.Name] = fmt.Sprintf("pod %s has already been matched to another node", pod.UID)
			continue
		}
		if i > 0 {
			// Only send one event.
			recorder = nil
		}
		// Attempt to allocate devices for pods on this node.
		newPod, err := allocator.NewAllocator(nodeInfo, recorder).Allocate(pod)
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
		cachePod, err := f.podLister.Pods(newPod.Namespace).Get(newPod.Name)
		if err == nil && util.CompareResourceVersion(cachePod, newPod) < 0 {
			cachePod.ObjectMeta = newPod.ObjectMeta
		}
		filteredNodes = append(filteredNodes, *node)
		success = true
	}

	return filteredNodes, failedNodesMap, nil
}

func PodUsedGPUTopologyMode(pod *corev1.Pod) util.TopologyMode {
	topoMode, _ := util.HasAnnotation(pod, util.DeviceTopologyModeAnnotation)
	switch {
	case topoMode == string(util.LinkTopology) && device.IsGPUTopologyEnabled() && util.IsSingleContainerMultiGPUs(pod):
		return util.LinkTopology
	case topoMode == string(util.NUMATopology) && util.IsSingleContainerMultiGPUs(pod):
		return util.NUMATopology
	default:
		return util.NoneTopology
	}
}
