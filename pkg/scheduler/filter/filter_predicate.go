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
	"k8s.io/kube-scheduler/framework"
	framework2 "k8s.io/kubernetes/pkg/scheduler/framework"
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
type filterFunc func(context.Context, *allocator.AllocationRequest, []corev1.Node, CycleState) ([]corev1.Node, map[string]*reason.FilterReason, error)

func (f *gpuFilter) IsReady(ctx context.Context) bool {
	return f.hasSyncFunc(ctx)
}

type CycleState interface {
	Read(key framework.StateKey) (framework.StateData, error)
	Write(key framework.StateKey, val framework.StateData)
	Delete(key framework.StateKey)
}

func (f *gpuFilter) Filter(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
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
	// Parse pod-wide scheduling inputs ONCE — req feeds both the node-
	// ranking comparators here and the per-node allocator below, so they
	// share annotation-parse cost and never disagree about what the pod
	// asked for.
	req := allocator.BuildAllocationRequest(pod)
	if len(req.Containers) == 0 {
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
		nodeReasons map[string]*reason.FilterReason
	)
	switch {
	case args.NodeNames != nil && len(*args.NodeNames) > 0:
		nodeCache = true
		filteredNodes, nodeReasons = f.getNodesOnCache(*args.NodeNames...)
	case args.Nodes != nil && len(args.Nodes.Items) > 0:
		filteredNodes = args.Nodes.Items
		nodeReasons = make(map[string]*reason.FilterReason, len(filteredNodes))
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
	state := framework2.NewCycleState()
	for i, filter := range filters {
		if len(filteredNodes) == 0 {
			break
		}
		passedNodes, stageReasons, err := filter(ctx, req, filteredNodes, state)
		if err != nil {
			klog.Errorf("Filter %d (%T) call failed: %v", i, filter, err)
			return &extenderv1.ExtenderFilterResult{Error: err.Error()}
		}
		// Change the latest node filtering list for the next round of filtering.
		filteredNodes = passedNodes
		maps.Copy(nodeReasons, stageReasons)
	}
	var (
		nodes     *corev1.NodeList
		nodeNames *[]string
	)
	if nodeCache {
		names := make([]string, len(filteredNodes))
		for i, node := range filteredNodes {
			names[i] = node.GetName()
		}
		nodeNames = &names
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
		f.recorder.Event(pod, corev1.EventTypeWarning, reason.EventFilteringFailed, msg)
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
func (f *gpuFilter) nodeFilter(ctx context.Context, req *allocator.AllocationRequest, nodes []corev1.Node, state CycleState) ([]corev1.Node, map[string]*reason.FilterReason, error) {
	var (
		filteredNodes = make([]corev1.Node, 0, len(nodes))
		failed        = make(map[string]*reason.FilterReason, len(nodes))
	)
	memoryPolicyFunc := GetMemoryPolicyFunc(req.Pod)
	for i, node := range nodes {
		var nodeConfig *device.NodeConfigInfo
		if r := CheckNode(&node, memoryPolicyFunc, func(node *corev1.Node, info *device.NodeConfigInfo) *reason.FilterReason {
			nodeConfig = info
			return nil
		}); r != nil {
			failed[node.Name] = r
		} else {
			state.Write(framework.StateKey(node.Name), nodeConfig)
			filteredNodes = append(filteredNodes, nodes[i])
		}
	}
	return filteredNodes, failed, nil
}

func (f *gpuFilter) CheckDeviceRequest(req *allocator.AllocationRequest) error {
	for _, container := range req.Containers {
		for _, fn := range []func(allocator.ContainerNeed) error{checkCoreRequest, checkNumberRequest} {
			if err := fn(container); err != nil {
				f.recorder.Event(req.Pod, corev1.EventTypeWarning, reason.EventResourceInvalid, err.Error())
				return err
			}
		}
	}
	return nil
}

func checkNumberRequest(container allocator.ContainerNeed) error {
	if container.Number > vgpu.MaxDeviceCount {
		return fmt.Errorf("container %s requests vGPU number exceeding limit", container.Name)
	}
	return nil
}

func checkCoreRequest(container allocator.ContainerNeed) error {
	if container.Cores > util.HundredCore {
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
func (f *gpuFilter) deviceFilter(ctx context.Context, req *allocator.AllocationRequest, nodes []corev1.Node, state CycleState) ([]corev1.Node, map[string]*reason.FilterReason, error) {
	var (
		pod           = req.Pod
		filteredNodes = make([]corev1.Node, 0, 1)
		failed        = make(map[string]*reason.FilterReason, len(nodes))
		success       bool
	)

	if err := f.CheckDeviceRequest(req); err != nil {
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

	// Ensure that the context has not timed out
	if err := ctx.Err(); err != nil {
		return filteredNodes, failed, err
	}

	nodePodsMap, err := f.podLister.NodeMapByIndexValue(IndexerKeyPodRequestVGPU, "true")
	if err != nil {
		klog.ErrorS(err, "PodLister list all vGPU Pods failed")
		return filteredNodes, failed, err
	}

	var (
		mutex                = sync.Mutex{}
		nodeInfoList         = make([]*device.NodeInfo, 0, len(nodes))
		nodeOriginalPosition = make(map[string]int, len(nodes))
	)

	maxGoroutines := runtime.GOMAXPROCS(0) * 2
	batchSize := (len(nodes) + maxGoroutines - 1) / maxGoroutines
	parallel := watcher.NewBatchParallel(len(nodes), batchSize)
	parallel.Execute(func(_ int, config watcher.BatchConfig) {
		startIndex, endIndex, count := config.StartIndex, config.EndIndex, config.Count
		batchNodeInfos := make([]*device.NodeInfo, 0, count)
		batchFailed := make(map[string]*reason.FilterReason, count)
		batchNodeOrigPosition := make(map[string]int, count)
		for index := startIndex; index <= endIndex; index++ {
			node := &nodes[index]
			batchNodeOrigPosition[node.Name] = index

			opts := []device.NodeInfoOptionFn{
				device.WithNodePods(nodePodsMap[node.Name]...),
				device.WithExcludedPods(pod.UID),
			}
			read, _ := state.Read(framework.StateKey(node.Name))
			if read != nil {
				if nodeConfig, ok := read.(*device.NodeConfigInfo); ok {
					opts = append(opts, device.WithNodeConfig(nodeConfig))
				}
			}
			nodeInfo, err := device.NewNodeInfo(node, opts...)
			if err != nil {
				klog.V(3).ErrorS(err, "new NodeInfo failed, skipping node", "node", node.Name)
				batchFailed[node.Name] = reason.New(reason.NodeInfoBuildFailed).WithDetail("%v", err)
				continue
			}
			// Pre-allocator capacity gate: reject nodes that obviously
			// can't fit the pod BEFORE letting them into the sorted
			// candidate list. NodeInfo is already built (annotation
			// decode is the dominant cost there and we needed it for
			// the GetAvailableNumber call anyway); what we save is the
			// downstream allocator pass — sort comparators,
			// pickDeviceClaims, topology dispatch, per-container
			// Allocate — which would otherwise iterate every node in
			// nodeInfoList. On saturated clusters this is the
			// difference between scanning 5000 NodeInfos or just the
			// 50 that still have room.
			//
			// Two checks, in order from cheapest/strictest to broadest:
			//
			//   maxNumber > GetDeviceCount() — the largest single
			//   container needs more cards than the node has TOTAL,
			//   even ignoring current usage. Each container's vGPUs
			//   must land on distinct cards, so this is a hard
			//   structural reject (allocator would reach the same
			//   verdict via allocateOne but only after parsing the
			//   per-container loop).
			//
			//   req.Total.Number > GetAvailableNumber() — sum of all
			//   containers' vGPU slots exceeds what's free on this
			//   node right now. Caught later anyway, but allocator's
			//   per-container greedy may waste cycles before
			//   discovering it.
			if req.Max.Number > nodeInfo.GetMaxAvailableDevices() {
				batchFailed[node.Name] = reason.New(reason.InsufficientGPUCards).
					WithDetail("max %d devices, node available %d", req.Max.Number, nodeInfo.GetMaxAvailableDevices())
				continue
			}
			if req.Max.Cores > nodeInfo.GetMaxAvailableCores() {
				batchFailed[node.Name] = reason.New(reason.InsufficientVGPUCore).
					WithDetail("max %d cores, max available %d", req.Max.Number, nodeInfo.GetMaxAvailableCores())
				continue
			}
			if req.Max.Memory > nodeInfo.GetMaxAvailableMemory() {
				batchFailed[node.Name] = reason.New(reason.InsufficientVGPUMemory).
					WithDetail("max %d memory, max available %d", req.Max.Number, nodeInfo.GetMaxAvailableMemory())
				continue
			}
			if req.Total.Number > nodeInfo.GetAvailableNumber() {
				batchFailed[node.Name] = reason.New(reason.InsufficientGPUResources).
					WithDetail("need %d number, available %d", req.Total.Number, nodeInfo.GetAvailableNumber())
				continue
			}
			if req.Total.Cores > nodeInfo.GetAvailableCores() {
				batchFailed[node.Name] = reason.New(reason.InsufficientGPUResources).
					WithDetail("need %d cores, available %d", req.Total.Cores, nodeInfo.GetAvailableCores())
			}
			if req.Total.Memory > nodeInfo.GetAvailableMemory() {
				batchFailed[node.Name] = reason.New(reason.InsufficientGPUResources).
					WithDetail("need %d memory, available %d", req.Total.Cores, nodeInfo.GetAvailableCores())
			}
			batchNodeInfos = append(batchNodeInfos, nodeInfo)
		}

		mutex.Lock()
		maps.Copy(failed, batchFailed)
		for index := range batchNodeInfos {
			nodeInfoList = append(nodeInfoList, batchNodeInfos[index])
		}
		maps.Copy(nodeOriginalPosition, batchNodeOrigPosition)
		mutex.Unlock()
	})
	parallel.WaitDone()

	// Quickly return results
	if len(nodeInfoList) == 0 {
		return filteredNodes, failed, nil
	}

	switch req.NodePolicy {
	case util.BinpackPolicy, util.SpreadPolicy:
		klog.V(4).Infof("Pod <%s> use <%s> node scheduling policy", klog.KObj(pod), req.NodePolicy)
		allocator.NewNodePolicyPriority(*req).Sort(nodeInfoList)
	default:
		if req.RawNodePolicy() != "" && req.RawNodePolicy() != string(util.NonePolicy) {
			klog.V(4).Infof("Pod <%s> not supported node scheduling policy: %s", klog.KObj(pod), req.RawNodePolicy())
			f.recorder.Eventf(pod, corev1.EventTypeWarning, reason.EventPolicyInvalid,
				"unsupported node scheduling policy %q", req.RawNodePolicy())
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
		// Ensure that the context has not timed out
		if err := ctx.Err(); err != nil {
			return filteredNodes, failed, err
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
		f.recorder.Eventf(pod, corev1.EventTypeNormal, reason.EventFilteringSucceed,
			"Successfully matched node %q", filteredNodes[0].Name)
	}
	return filteredNodes, failed, nil
}
