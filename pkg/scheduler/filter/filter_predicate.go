package filter

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

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
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

type gpuFilter struct {
	locker      *serial.Locker
	kubeClient  kubernetes.Interface
	nodeLister  listerv1.NodeLister
	podLister   client.PodLister
	recorder    record.EventRecorder
	hasSyncFunc func(ctx context.Context) bool
}

const (
	Name                     = "FilterPredicate"
	IndexerKeyPodRequestVGPU = "pod.requestVGPU"
)

var (
	_           predicate.FilterPredicate = &gpuFilter{}
	podIndexers                           = cache.Indexers{
		IndexerKeyPodRequestVGPU: func(obj interface{}) ([]string, error) {
			if pod, ok := obj.(*corev1.Pod); ok && util.IsVGPUResourcePod(pod) {
				return []string{"true"}, nil
			}
			return []string{"false"}, nil
		},
	}
)

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
	hasSyncFunc := func(ctx context.Context) bool {
		return cache.WaitForCacheSync(
			ctx.Done(),
			podInformer.HasSynced,
			nodeInformer.HasSynced,
		)
	}
	return &gpuFilter{
		locker:      locker,
		kubeClient:  kubeClient,
		nodeLister:  nodeLister,
		podLister:   podLister,
		recorder:    recorder,
		hasSyncFunc: hasSyncFunc,
	}, nil
}

func (f *gpuFilter) Name() string {
	return Name
}

func (f *gpuFilter) GetPodLister() client.PodLister {
	return f.podLister
}

type filterFunc func(*corev1.Pod, []corev1.Node) ([]corev1.Node, extenderv1.FailedNodesMap, error)

func (f *gpuFilter) IsReady(ctx context.Context) bool {
	return f.hasSyncFunc(ctx)
}

func (f *gpuFilter) Filter(_ context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
	klog.V(4).InfoS("FilterNode", "ExtenderArgs", args)
	pod := args.Pod
	if pod == nil {
		return &extenderv1.ExtenderFilterResult{
			Error: "ExtenderArgs.Pod cannot be empty",
		}
	}
	if pod.Spec.NodeName != "" {
		return &extenderv1.ExtenderFilterResult{
			Error: "Pod has specified nodes to " + pod.Spec.NodeName,
		}
	}
	if !util.IsVGPUResourcePod(pod) {
		klog.V(5).InfoS("Skip pods that do not request vGPU", "pod", klog.KObj(pod))
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
		passedNodes, failedNodes, err := filter(pod, filteredNodes)
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
			failedNodesMap[nodeName] = fmt.Sprintf("%s: %v", errMsg, err)
		} else {
			filteredNodes = append(filteredNodes, *node)
		}
	}
	return filteredNodes, failedNodesMap
}

func GetMemoryPolicyFunc(pod *corev1.Pod) CheckNodeFunc {
	policy, _ := util.HasAnnotation(pod, util.MemorySchedulerPolicyAnnotation)
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == util.VirtualMemoryPolicy.String() || strings.HasPrefix(policy, "virt") {
		klog.V(4).Infof("Pod <%s> use <%s> memory scheduling policy", klog.KObj(pod), util.VirtualMemoryPolicy)
		return func(node *corev1.Node, info *device.NodeConfigInfo) error {
			if info.MemoryScaling <= 1 {
				return errors.New(GPUMemTypeMismatch)
			}
			return nil
		}
	}
	if policy == util.PhysicalMemoryPolicy.String() || strings.HasPrefix(policy, "phy") {
		klog.V(4).Infof("Pod <%s> use <%s> memory scheduling policy", klog.KObj(pod), util.PhysicalMemoryPolicy)
		return func(node *corev1.Node, info *device.NodeConfigInfo) error {
			if info.MemoryScaling > 1 {
				return errors.New(GPUMemTypeMismatch)
			}
			return nil
		}
	}
	return func(node *corev1.Node, info *device.NodeConfigInfo) error {
		return nil
	}
}

const (
	NoGPUDevice            = "no GPU device"
	NoGPUConfigInfo        = "no GPU configuration info"
	IncorrectGPUConfigInfo = "incorrect GPU config info"
	IncorrectGPUMemFactory = "incorrect GPU memory factor"
	GPUMemTypeMismatch     = "GPU memory type mismatch"
	NoGPURegister          = "no GPU device registered"
)

type CheckNodeFunc func(node *corev1.Node, info *device.NodeConfigInfo) error

func CheckNode(node *corev1.Node, checkNodeFuncs ...CheckNodeFunc) error {
	if !util.IsVGPUEnabledNode(node) {
		return errors.New(NoGPUDevice)
	}
	if val, ok := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation); !ok || len(val) == 0 {
		klog.V(3).InfoS("node has not registered any GPU devices", "node", node.Name)
		return errors.New(NoGPURegister)
	}
	devConfigInfo, ok := util.HasAnnotation(node, util.NodeConfigInfoAnnotation)
	if !ok || len(devConfigInfo) == 0 {
		return errors.New(NoGPUConfigInfo)
	}
	nodeConfigInfo := device.NodeConfigInfo{}
	if err := nodeConfigInfo.Decode(devConfigInfo); err != nil {
		klog.V(3).ErrorS(err, "decoding node configuration information failed", "node", node.Name)
		return errors.New(IncorrectGPUConfigInfo)
	}
	if nodeConfigInfo.DeviceSplit <= 0 {
		return errors.New(NoGPUDevice)
	}
	if nodeConfigInfo.MemoryFactor <= 0 {
		return errors.New(IncorrectGPUMemFactory)
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
	podDevices := device.PodDeviceClaim{}
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

	if err := f.CheckDeviceRequest(pod); err != nil {
		klog.V(2).ErrorS(err, "Check device request failed", "pod", klog.KObj(pod))
		return filteredNodes, failedNodesMap, err
	}

	// Skip pods that have already been scheduled.
	if nodeName, ok := IsScheduled(pod); ok {
		if device.ShouldCountPodDeviceAllocation(pod) {
			// Pre-allocation is current; steer the pod back to its predicated node.
			foundNode := false
			for i, node := range nodes {
				if !foundNode && node.Name == nodeName {
					filteredNodes = append(filteredNodes, nodes[i])
					foundNode = true
					continue
				}
				failedNodesMap[node.Name] = fmt.Sprintf("pod has been scheduled to node %s", nodeName)
			}
			if foundNode {
				return filteredNodes, failedNodesMap, nil
			}
			return nil, nil, fmt.Errorf("pod %s had been predicated", pod.UID)
		}
		// Pre-allocation is stale or stuck — re-trigger device pre-allocation.
		klog.V(3).InfoS("Re-triggering device pre allocation for pod", "pod", klog.KObj(pod))
	}

	f.locker.Lock()
	defer f.locker.Unlock()

	pods, err := f.podLister.ListByIndexValue(IndexerKeyPodRequestVGPU, "true")
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
				nodeInfo, err := device.NewNodeInfo(node, pods, pod.UID)
				if err != nil {
					klog.V(3).ErrorS(err, "new NodeInfo failed, skipping node", "node", node.Name)
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

	// Sort nodes according to node scheduling strategy. The topology mode +
	// needNumber pair gates the fitness-aware comparator that ranks nodes by
	// whether they can ACTUALLY host the requested topology group (not just
	// whether they reported topology metadata).
	topoMode := PodUsedGPUTopologyMode(pod)
	topoNeedNumber := PodTopologyNeedNumber(pod)
	nodePolicy, _ := util.HasAnnotation(pod, util.NodeSchedulerPolicyAnnotation)
	switch policy := strings.ToLower(nodePolicy); policy {
	case string(util.BinpackPolicy):
		klog.V(4).Infof("Pod <%s> use <%s> node scheduling policy", klog.KObj(pod), policy)
		allocator.NewNodeBinpackPriority(topoMode, topoNeedNumber).Sort(nodeInfoList)
	case string(util.SpreadPolicy):
		klog.V(4).Infof("Pod <%s> use <%s> node scheduling policy", klog.KObj(pod), policy)
		allocator.NewNodeSpreadPriority(topoMode, topoNeedNumber).Sort(nodeInfoList)
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
		less = allocator.ApplyTopologyMode(topoMode, topoNeedNumber, less)
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
			klog.V(1).ErrorS(err, "node device allocate failed", "node", node.Name, "pod", klog.KObj(pod))
			failedNodesMap[node.Name] = err.Error()
			continue
		}
		if err = client.PatchPodPreAllocatedMetadata(f.kubeClient, newPod); err != nil {
			errMsg := fmt.Sprintf("patch vGPU metadata failed")
			klog.ErrorS(err, errMsg, "pod", klog.KObj(pod))
			failedNodesMap[node.Name] = errMsg
			continue
		}
		// Cache the patched Pod locally to bridge the informer watch lag.
		// Concurrent Filter calls on neighbouring pods would otherwise rebuild
		// NodeInfo from a stale informer view (without our pre-allocated
		// annotation) and miscount free GPU.
		f.podLister.Mutation(newPod)
		filteredNodes = append(filteredNodes, *node)
		success = true
	}
	if success {
		f.recorder.Eventf(pod, corev1.EventTypeNormal, "FilteringSucceed", "Successfully matched to node <%s>", filteredNodes[0].Name)
	}
	return filteredNodes, failedNodesMap, nil
}

// PodUsedGPUTopologyMode returns the EFFECTIVE topology mode for the given
// pod, taking into account both the user annotation and the runtime
// preconditions (GPUTopology feature gate for link mode; at least one
// container requesting >1 GPU). Both the regular and the *-strict variants
// of each mode are recognised; the strict suffix is preserved on the return
// value so downstream allocation paths can apply the no-fallback policy.
func PodUsedGPUTopologyMode(pod *corev1.Pod) util.TopologyMode {
	raw, _ := util.HasAnnotation(pod, util.DeviceTopologyModeAnnotation)
	mode := util.TopologyMode(strings.ToLower(raw))
	switch mode {
	case util.LinkTopology, util.LinkTopologyStrict:
		if device.IsGPUTopologyEnabled() && util.IsSingleContainerMultiGPUs(pod) {
			return mode
		}
	case util.NUMATopology, util.NUMATopologyStrict:
		if util.IsSingleContainerMultiGPUs(pod) {
			return mode
		}
	}
	return util.NoneTopology
}

// PodTopologyNeedNumber returns the largest single-container vGPU request in
// the pod. This is the value passed to ByNodeGPUTopologyFitness so the
// node-level sort knows the minimum group size the node has to be able to
// host topology-locally. Returns 0 when no container requests a vGPU group
// > 1, which makes the fitness comparator a no-op.
func PodTopologyNeedNumber(pod *corev1.Pod) int {
	if pod == nil {
		return 0
	}
	max := int64(0)
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if n := util.GetResourceOfContainer(c, util.VGPUNumberResourceName); n > max {
			max = n
		}
	}
	return int(max)
}
