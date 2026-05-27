package filter

import (
	"context"
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
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
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

	// Event Reason vocabulary. Names are CamelCase verbs/states the
	// way kube-scheduler and other extenders do it (FailedScheduling,
	// FilteringSucceed, BindingFailed) — operators grep on these.
	EventReasonFilteringFailed  = "FilteringFailed"
	EventReasonFilteringSucceed = "FilteringSucceed"
	EventReasonResourceInvalid  = "ResourceInvalid"
	EventReasonPolicyInvalid    = "PolicyInvalid"
	EventReasonTopologyFallback = "TopologyFallback"

	// aggregateBucketNodeLimit caps how many node names appear inside
	// each "(...)" clause of the FilteringFailed aggregate event message.
	// On clusters with many nodes failing for the same reason the full
	// list pushes the Event past the typical 1024-char message budget;
	// truncating to a handful keeps the event readable while the full
	// list is still available in klog at V(5).
	aggregateBucketNodeLimit = 5
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

// filterFunc is one stage of the in-process filter chain. Stages return
// reasons as *reason.FilterReason (structured) rather than raw strings
// so the top-level Filter() can both:
//   - emit a single k8s-style "0/N nodes are available: ..." event
//     bucketing nodes by Primary code, and
//   - hand kube-scheduler a clean short-phrase FailedNodesMap for its
//     own FailedScheduling line.
type filterFunc func(*corev1.Pod, []corev1.Node) ([]corev1.Node, map[string]*reason.FilterReason, error)

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
		nodeCache     bool
		filteredNodes []corev1.Node
		// nodeReasons accumulates the structured rejection cause for each
		// node across BOTH the in-process filter chain (nodeFilter,
		// deviceFilter) and the initial cache-miss pass. We convert to
		// kube-scheduler's plain-string FailedNodesMap at the response
		// boundary; keeping the *FilterReason shape internally lets us
		// emit one aggregate FilteringFailed event with k8s-style
		// "0/N nodes are available: ..." text bucketed by Primary code.
		nodeReasons = make(map[string]*reason.FilterReason)
	)
	switch {
	case args.NodeNames != nil && len(*args.NodeNames) > 0:
		nodeCache = true
		filteredNodes, nodeReasons = f.getNodesOnCache(*args.NodeNames...)
	case args.Nodes != nil && len(args.Nodes.Items) > 0:
		filteredNodes = args.Nodes.Items
	default:
		return &extenderv1.ExtenderFilterResult{
			Nodes:     args.Nodes,
			NodeNames: args.NodeNames,
			Error:     "No schedulable nodes",
		}
	}

	// Snapshot the candidate count BEFORE the filter chain runs so the
	// "0/N nodes are available:" header reflects what kube-scheduler
	// asked us about, regardless of how many drop out at each stage.
	totalCandidates := len(filteredNodes) + len(nodeReasons)

	filters := []filterFunc{
		f.nodeFilter,
		f.deviceFilter,
	}

	for i, filter := range filters {
		passedNodes, stageReasons, err := filter(pod, filteredNodes)
		if err != nil {
			klog.Errorf("Filter %d (%T) call failed: %v", i, filter, err)
			return &extenderv1.ExtenderFilterResult{Error: err.Error()}
		}
		// Change the latest node filtering list for the next round of filtering.
		filteredNodes = passedNodes
		for name, r := range stageReasons {
			nodeReasons[name] = r
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

	// If no node survived, emit the aggregate FilteringFailed event so
	// operators see a single k8s-native-style summary in
	// `kubectl describe pod` ALONGSIDE kube-scheduler's own
	// FailedScheduling line. The two are consistent because they read
	// the same per-node Short() phrases — ours is more detailed (carries
	// node names in parentheses) and is the place to look first for
	// scheduling debugging.
	if len(filteredNodes) == 0 && totalCandidates > 0 && f.recorder != nil {
		msg := reason.FormatAggregate(totalCandidates, nodeReasons, aggregateBucketNodeLimit)
		f.recorder.Event(pod, corev1.EventTypeWarning, EventReasonFilteringFailed, msg)
		klog.V(2).InfoS("FilteringFailed",
			"pod", klog.KObj(pod), "totalCandidates", totalCandidates,
			"failedReasons", failureBreakdown(nodeReasons))
	}

	return &extenderv1.ExtenderFilterResult{
		Nodes:       nodes,
		NodeNames:   nodeNames,
		FailedNodes: reasonsToFailedNodesMap(nodeReasons),
	}
}

// failureBreakdown reduces per-node reasons to a Code → count map for
// klog. The full per-node detail lives in V(5) traces emitted by the
// filter functions themselves; this is the compact summary that pairs
// with the FilteringFailed event message.
func failureBreakdown(reasons map[string]*reason.FilterReason) map[reason.Code]int {
	counts := make(map[reason.Code]int, len(reasons))
	for _, r := range reasons {
		if r == nil {
			continue
		}
		counts[r.Primary]++
	}
	return counts
}

// reasonsToFailedNodesMap converts the in-process *FilterReason map to
// the plain-string FailedNodesMap that kube-scheduler's extender API
// requires. The Short() form is what feeds the synthesised
// "0/N nodes are available: <short>, ..." line in the upstream
// FailedScheduling event.
func reasonsToFailedNodesMap(reasons map[string]*reason.FilterReason) extenderv1.FailedNodesMap {
	out := make(extenderv1.FailedNodesMap, len(reasons))
	for name, r := range reasons {
		out[name] = r.Short()
	}
	return out
}

func (f *gpuFilter) getNodesOnCache(nodeNames ...string) ([]corev1.Node, map[string]*reason.FilterReason) {
	filteredNodes := make([]corev1.Node, 0, len(nodeNames))
	failed := make(map[string]*reason.FilterReason, len(nodeNames))
	for _, nodeName := range nodeNames {
		if node, err := f.nodeLister.Get(nodeName); err != nil {
			klog.ErrorS(err, "get node cache failed", "node", nodeName)
			failed[nodeName] = reason.New(reason.NodeCacheMiss).WithDetail("%v", err)
		} else {
			filteredNodes = append(filteredNodes, *node)
		}
	}
	return filteredNodes, failed
}

func GetMemoryPolicyFunc(pod *corev1.Pod) CheckNodeFunc {
	policy, _ := util.HasAnnotation(pod, util.MemorySchedulerPolicyAnnotation)
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == util.VirtualMemoryPolicy.String() || strings.HasPrefix(policy, "virt") {
		klog.V(4).Infof("Pod <%s> use <%s> memory scheduling policy", klog.KObj(pod), util.VirtualMemoryPolicy)
		return func(node *corev1.Node, info *device.NodeConfigInfo) *reason.FilterReason {
			if info.MemoryScaling <= 1 {
				return reason.New(reason.NodeMemoryTypeMismatch).
					WithDetail("requires virtual memory but node memoryScaling=%v", info.MemoryScaling)
			}
			return nil
		}
	}
	if policy == util.PhysicalMemoryPolicy.String() || strings.HasPrefix(policy, "phy") {
		klog.V(4).Infof("Pod <%s> use <%s> memory scheduling policy", klog.KObj(pod), util.PhysicalMemoryPolicy)
		return func(node *corev1.Node, info *device.NodeConfigInfo) *reason.FilterReason {
			if info.MemoryScaling > 1 {
				return reason.New(reason.NodeMemoryTypeMismatch).
					WithDetail("requires physical memory but node memoryScaling=%v", info.MemoryScaling)
			}
			return nil
		}
	}
	return func(node *corev1.Node, info *device.NodeConfigInfo) *reason.FilterReason {
		return nil
	}
}

// CheckNodeFunc is one node-level gate. Returning nil means the gate
// accepted the node; returning a non-nil *reason.FilterReason means the
// node fails the gate with the given structured cause.
type CheckNodeFunc func(node *corev1.Node, info *device.NodeConfigInfo) *reason.FilterReason

// CheckNode runs the built-in node prerequisites plus any caller-
// supplied gates. Returns the first failing reason, or nil if every
// gate accepts the node.
func CheckNode(node *corev1.Node, checkNodeFuncs ...CheckNodeFunc) *reason.FilterReason {
	if !util.IsVGPUEnabledNode(node) {
		return reason.New(reason.NodeNotVGPUEnabled)
	}
	if val, ok := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation); !ok || len(val) == 0 {
		klog.V(3).InfoS("node has not registered any GPU devices", "node", node.Name)
		return reason.New(reason.NodeNoVGPURegister)
	}
	devConfigInfo, ok := util.HasAnnotation(node, util.NodeConfigInfoAnnotation)
	if !ok || len(devConfigInfo) == 0 {
		return reason.New(reason.NodeNoGPUConfig)
	}
	nodeConfigInfo := device.NodeConfigInfo{}
	if err := nodeConfigInfo.Decode(devConfigInfo); err != nil {
		klog.V(3).ErrorS(err, "decoding node configuration information failed", "node", node.Name)
		return reason.New(reason.NodeBadGPUConfig).WithDetail("%v", err)
	}
	if nodeConfigInfo.DeviceSplit <= 0 {
		return reason.New(reason.NodeNotVGPUEnabled).WithDetail("deviceSplit=%d", nodeConfigInfo.DeviceSplit)
	}
	if nodeConfigInfo.MemoryFactor <= 0 {
		return reason.New(reason.NodeBadMemoryFactor).WithDetail("memoryFactor=%d", nodeConfigInfo.MemoryFactor)
	}
	for _, checkFunc := range checkNodeFuncs {
		if r := checkFunc(node, &nodeConfigInfo); r != nil {
			return r
		}
	}
	return nil
}

// nodeFilter rejects nodes that fail the node-level prerequisites (no
// GPU registered, bad config, wrong memory scaling for the requested
// policy). Per-node reasons feed both kube-scheduler's FailedNodesMap
// (via Short()) and vgpu-manager's own aggregate FilteringFailed event.
func (f *gpuFilter) nodeFilter(pod *corev1.Pod, nodes []corev1.Node) ([]corev1.Node, map[string]*reason.FilterReason, error) {
	var (
		filteredNodes = make([]corev1.Node, 0, len(nodes))
		failed        = make(map[string]*reason.FilterReason, len(nodes))
	)
	memoryPolicyFunc := GetMemoryPolicyFunc(pod)
	for i, node := range nodes {
		if r := CheckNode(&node, memoryPolicyFunc); r != nil {
			failed[node.Name] = r
		} else {
			filteredNodes = append(filteredNodes, nodes[i])
		}
	}
	return filteredNodes, failed, nil
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
func (f *gpuFilter) deviceFilter(pod *corev1.Pod, nodes []corev1.Node) ([]corev1.Node, map[string]*reason.FilterReason, error) {
	var (
		filteredNodes = make([]corev1.Node, 0, 1)
		failed        = make(map[string]*reason.FilterReason, len(nodes))
		success       bool
	)

	if err := f.CheckDeviceRequest(pod); err != nil {
		klog.V(2).ErrorS(err, "Check device request failed", "pod", klog.KObj(pod))
		return filteredNodes, failed, err
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
				failed[node.Name] = reason.New(reason.AlreadyScheduledElsewhere).
					WithDetail("pod already predicated on node %s", nodeName)
			}
			if foundNode {
				return filteredNodes, failed, nil
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
		return filteredNodes, failed, err
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
			batchFailed := make(map[string]*reason.FilterReason, count)
			batchNodeOrigPosition := make(map[string]int, count)
			for index := startIndex; index <= endIndex; index++ {
				node := &nodes[index]
				batchNodeOrigPosition[node.Name] = index
				nodeInfo, err := device.NewNodeInfo(node, pods, pod.UID)
				if err != nil {
					klog.V(3).ErrorS(err, "new NodeInfo failed, skipping node", "node", node.Name)
					batchFailed[node.Name] = reason.New(reason.NodeInfoBuildFailed).WithDetail("%v", err)
					continue
				}
				batchNodeInfos = append(batchNodeInfos, nodeInfo)
			}

			mutex.Lock()
			for name, r := range batchFailed {
				failed[name] = r
			}
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
		return filteredNodes, failed, nil
	}

	// Parse pod-wide scheduling inputs ONCE — req feeds both the node-
	// ranking comparators here and the per-node allocator below, so they
	// share annotation-parse cost and never disagree about what the pod
	// asked for.
	req := allocator.BuildAllocationRequest(pod)

	switch req.NodePolicy {
	case util.BinpackPolicy, util.SpreadPolicy:
		klog.V(4).Infof("Pod <%s> use <%s> node scheduling policy", klog.KObj(pod), req.NodePolicy)
		allocator.NewNodePolicyPriority(*req).Sort(nodeInfoList)
	default:
		if req.RawNodePolicy() != "" && req.RawNodePolicy() != string(util.NonePolicy) {
			klog.V(4).Infof("Pod <%s> not supported node scheduling policy: %s", klog.KObj(pod), req.RawNodePolicy())
			f.recorder.Eventf(pod, corev1.EventTypeWarning, "NodePolicy", "Unsupported node scheduling policy '%s'", req.RawNodePolicy())
		} else {
			klog.V(4).Infof("Pod <%s> no node scheduling policy", klog.KObj(pod))
		}
		less := []allocator.LessFunc[*device.NodeInfo]{func(p1, p2 *device.NodeInfo) bool {
			return nodeOriginalPosition[p1.GetName()] < nodeOriginalPosition[p2.GetName()]
		}}
		less = allocator.ApplyTopologyMode(*req, less)
		allocator.NewSortPriority[*device.NodeInfo](less...).Sort(nodeInfoList)
	}
	recorder := f.recorder
	for i, nodeInfo := range nodeInfoList {
		node := nodeInfo.GetNode()
		if success {
			failed[node.Name] = reason.New(reason.AlreadyScheduledElsewhere).
				WithDetail("pod already matched to %s in this Filter pass", filteredNodes[0].Name)
			continue
		}
		if i > 0 {
			// Only send one event.
			recorder = nil
		}
		// Attempt to allocate devices for pods on this node.
		newPod, rsn, err := allocator.NewAllocator(nodeInfo, recorder).Allocate(req)
		if err != nil {
			// Internal/programmer error (annotation encoding, accounting
			// bug). Don't just skip the node — bubble up so the whole
			// Filter call returns Error and the operator notices.
			klog.ErrorS(err, "node device allocate: internal error",
				"node", node.Name, "pod", klog.KObj(pod))
			return filteredNodes, failed, err
		}
		if rsn != nil {
			klog.V(4).InfoS("node device allocate rejected", "node", node.Name,
				"pod", klog.KObj(pod), "reason", rsn.Detailed())
			failed[node.Name] = rsn
			continue
		}
		if err = client.PatchPodPreAllocatedMetadata(f.kubeClient, newPod); err != nil {
			klog.ErrorS(err, "patch vGPU metadata failed", "pod", klog.KObj(pod), "node", node.Name)
			// Treat as a node-level rejection rather than an aborted Filter:
			// other nodes may still succeed. Use NodeInfoBuildFailed as the
			// closest existing code (it's "the node side broke") with the
			// underlying error in Detail for klog/debug.
			failed[node.Name] = reason.New(reason.NodeInfoBuildFailed).
				WithDetail("patch vGPU metadata failed: %v", err)
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
		f.recorder.Eventf(pod, corev1.EventTypeNormal, "FilteringSucceed",
			"Successfully matched node %q", filteredNodes[0].Name)
	}
	return filteredNodes, failed, nil
}
