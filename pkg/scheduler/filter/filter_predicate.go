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

package filter

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/allocator"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/predicate"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
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
	"k8s.io/kube-scheduler/framework"
	framework2 "k8s.io/kubernetes/pkg/scheduler/framework"
)

type gpuFilter struct {
	locker      *serial.Locker
	kubeClient  kubernetes.Interface
	nodeLister  listerv1.NodeLister
	podLister   client.PodLister
	recorder    record.EventRecorder
	gpuTopology bool
	hasSyncFunc func(ctx context.Context) bool
}

const (
	Name                      = "FilterPredicate"
	IndexerKeyPodRequestVGPU  = "pod.requestVGPU"
	IndexerKeyPodGangName     = "pod.gangName"
	IndexerKeyControlOwnerUID = "pod.controllerOwner.UID"

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
		// Indexed by the NAMESPACE-QUALIFIED gang key: this informer is
		// cluster-wide, so indexing by bare name would return another
		// namespace's identically-named gang as siblings of this one.
		IndexerKeyPodGangName: func(obj interface{}) ([]string, error) {
			var indexerValue []string
			if pod, ok := obj.(*corev1.Pod); ok {
				if key, ok := util.PodGangKey(pod); ok {
					indexerValue = []string{key}
				}
			}
			return indexerValue, nil
		},
		IndexerKeyControlOwnerUID: func(obj interface{}) ([]string, error) {
			var indexerValue []string
			if pod, ok := obj.(*corev1.Pod); ok {
				if owner := metav1.GetControllerOfNoCopy(pod); owner != nil {
					indexerValue = []string{string(owner.UID)}
				}
			}
			return indexerValue, nil
		},
	}
)

func New(kubeClient kubernetes.Interface, factory informers.SharedInformerFactory,
	recorder record.EventRecorder, serialFilterNode bool, gpuTopology bool) (*gpuFilter, error) {
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
		gpuTopology: gpuTopology,
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

func NodeNames(args extenderv1.ExtenderArgs) []string {
	var names []string
	if args.Nodes != nil {
		names = make([]string, len(args.Nodes.Items))
		for i, item := range args.Nodes.Items {
			names[i] = item.GetName()
		}
	} else if args.NodeNames != nil {
		names = *args.NodeNames
	}
	return names
}

// filterMode selects which of the two contracts a Filter call honours. The
// whole chain is shared: mode only decides whether the call may touch the
// cluster (pre-allocation, events, live metrics) and whether it stops at the
// first node that fits or reports every node that fits.
type filterMode uint8

const (
	// liveFilter serves kube-scheduler: it commits an optimistic
	// pre-allocation onto the winning node and returns that node alone.
	liveFilter filterMode = iota
	// dryRunFilter serves scale-up simulation: read-only, and it returns
	// EVERY feasible node because the caller — not us — picks.
	dryRunFilter
)

func (m filterMode) isDryRun() bool { return m == dryRunFilter }

// verb is the metrics label this mode reports under. Keeping the two paths on
// separate labels is what stops unbounded simulation traffic from drowning the
// live scheduling numbers.
func (m filterMode) verb() string {
	if m.isDryRun() {
		return metrics.VerbFilterDryRun
	}
	return metrics.VerbFilter
}

// Filter is the live scheduling verb: feasibility plus pre-allocation.
func (f *gpuFilter) Filter(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
	return f.filter(ctx, args, liveFilter)
}

// FilterDryRun is the read-only verb used by scale-up simulation. It shares
// every feasibility decision with Filter and commits nothing.
func (f *gpuFilter) FilterDryRun(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
	return f.filter(ctx, args, dryRunFilter)
}

func (f *gpuFilter) filter(ctx context.Context, args extenderv1.ExtenderArgs, mode filterMode) (result *extenderv1.ExtenderFilterResult) {
	klog.V(4).InfoS("FilterNode", "pod", klog.KObj(args.Pod), "nodeNames", NodeNames(args), "dryRun", mode.isDryRun())
	start := time.Now()
	defer func() { metrics.ObserveVerb(mode.verb(), filterResultLabel(result), start) }()

	req, filteredNodes, nodeReasons, result := f.preFilterRequestNodes(args)
	if result != nil {
		return result
	}

	// Snapshot the candidate count BEFORE the filter chain runs so the
	// "0/N nodes are available:" header reflects what kube-scheduler
	// asked us about, regardless of how many drop out at each stage.
	totalCandidates := len(filteredNodes) + len(nodeReasons)

	filters := []struct {
		stage string
		fn    filterFunc
	}{
		{metrics.StageNode, f.nodeFilter},
		{metrics.StageDevice, f.deviceFilterFunc(mode)},
	}
	state := framework2.NewCycleState()
	for i, filter := range filters {
		if len(filteredNodes) == 0 {
			break
		}
		stageStart := time.Now()
		passedNodes, stageReasons, err := filter.fn(ctx, req, filteredNodes, state)
		// Total time in the stage. deviceFilter separately records the time it
		// spent WAITING on the serial filter lock, so device work = device minus
		// lock_wait in PromQL. They are kept as two independent observations
		// rather than one subtracted value because Filter calls run
		// concurrently — carrying the wait on the shared gpuFilter to subtract
		// it here would be a data race.
		metrics.ObserveStage(mode.verb(), filter.stage, stageStart)
		if err != nil {
			klog.Errorf("Filter %d (%s) call failed: %v", i, filter.stage, err)
			return &extenderv1.ExtenderFilterResult{Error: err.Error()}
		}
		// Change the latest node filtering list for the next round of filtering.
		filteredNodes = passedNodes
		maps.Copy(nodeReasons, stageReasons)
	}
	recordNodeRejects(mode.verb(), nodeReasons)

	// If no node survived, emit the aggregate FilteringFailed event so
	// operators see a single k8s-native-style summary in
	// `kubectl describe pod` ALONGSIDE kube-scheduler's own
	// FailedScheduling line. The two are consistent because they read
	// the same per-node Short() phrases — ours is more detailed (carries
	// node names in parentheses) and is the place to look first for
	// scheduling debugging.
	if !mode.isDryRun() && len(filteredNodes) == 0 && totalCandidates > 0 && f.recorder != nil {
		msg := reason.FormatAggregate(totalCandidates, nodeReasons, aggregateBucketNodeLimit)
		f.recorder.Event(args.Pod, corev1.EventTypeWarning, reason.EventFilteringFailed, msg)
		klog.V(2).InfoS("FilteringFailed", "pod", klog.KObj(args.Pod),
			"totalCandidates", totalCandidates, "failedReasons", failureBreakdown(nodeReasons))
	}

	return buildFilterResult(args, filteredNodes, nodeReasons, mode)
}

func (f *gpuFilter) preFilterRequestNodes(args extenderv1.ExtenderArgs) (
	*allocator.AllocationRequest, []corev1.Node,
	map[string]*reason.FilterReason, *extenderv1.ExtenderFilterResult,
) {
	if args.Pod == nil {
		return nil, nil, nil, &extenderv1.ExtenderFilterResult{Error: "extenderArgs.Pod cannot be empty"}
	}

	if args.Pod.Spec.NodeName != "" {
		return nil, nil, nil, &extenderv1.ExtenderFilterResult{
			Error: fmt.Sprintf("pod has been bound to node %q", args.Pod.Spec.NodeName),
		}
	}
	// Parse pod-wide scheduling inputs ONCE — req feeds both the node-
	// ranking comparators here and the per-node allocator below, so they
	// share annotation-parse cost and never disagree about what the pod
	// asked for.
	req := allocator.BuildAllocationRequest(args.Pod)
	if len(req.Containers) == 0 {
		klog.V(5).InfoS("Skip pods that do not request vGPU", "pod", klog.KObj(args.Pod))
		return nil, nil, nil, &extenderv1.ExtenderFilterResult{
			Nodes:     args.Nodes,
			NodeNames: args.NodeNames,
		}
	}

	var (
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
		filteredNodes, nodeReasons = f.getNodesOnCache(*args.NodeNames...)
	case args.Nodes != nil && len(args.Nodes.Items) > 0:
		filteredNodes = args.Nodes.Items
		nodeReasons = make(map[string]*reason.FilterReason, len(filteredNodes))
	default:
		return nil, nil, nil, &extenderv1.ExtenderFilterResult{
			Nodes:     args.Nodes,
			NodeNames: args.NodeNames,
			Error:     "No schedulable nodes",
		}
	}
	return req, filteredNodes, nodeReasons, nil
}

// buildFilterResult shapes the response to mirror the request: a
// nodeCacheCapable caller sent NodeNames and expects NodeNames back, everyone
// else gets Nodes. Dry-run always takes the Nodes form (see
// preFilterRequestNodes).
func buildFilterResult(
	args extenderv1.ExtenderArgs, filteredNodes []corev1.Node,
	nodeReasons map[string]*reason.FilterReason, mode filterMode,
) *extenderv1.ExtenderFilterResult {
	var (
		nodes     *corev1.NodeList
		nodeNames *[]string
	)
	if args.NodeNames != nil && len(*args.NodeNames) > 0 {
		names := make([]string, len(filteredNodes))
		for i, node := range filteredNodes {
			names[i] = node.GetName()
		}
		nodeNames = &names
	} else {
		nodes = &corev1.NodeList{Items: filteredNodes}
	}
	return &extenderv1.ExtenderFilterResult{
		Nodes:       nodes,
		NodeNames:   nodeNames,
		FailedNodes: reasonsToFailedNodesMap(nodeReasons, mode),
	}
}

// filterResultLabel maps a finished filter response onto the closed metric
// vocabulary: did the call fail, keep something, or rule everything out?
func filterResultLabel(result *extenderv1.ExtenderFilterResult) string {
	switch {
	case result == nil || result.Error != "":
		return metrics.ResultError
	case len(NodeNamesOfResult(result)) > 0:
		return metrics.ResultFit
	default:
		return metrics.ResultNoFit
	}
}

// NodeNamesOfResult lists the nodes a filter result kept, whichever of the two
// response forms it used.
func NodeNamesOfResult(result *extenderv1.ExtenderFilterResult) []string {
	if result == nil {
		return nil
	}
	if result.NodeNames != nil {
		return *result.NodeNames
	}
	if result.Nodes == nil {
		return nil
	}
	names := make([]string, len(result.Nodes.Items))
	for i, node := range result.Nodes.Items {
		names[i] = node.GetName()
	}
	return names
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
//
// Dry-run callers get Detailed() instead: nothing downstream aggregates a
// simulation's reasons into an event, so "why can't a new node of this shape
// take the pod" is the only diagnostic the caller will ever see.
func reasonsToFailedNodesMap(reasons map[string]*reason.FilterReason, mode filterMode) extenderv1.FailedNodesMap {
	out := make(extenderv1.FailedNodesMap, len(reasons))
	for name, r := range reasons {
		if mode.isDryRun() {
			out[name] = r.Detailed()
			continue
		}
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
		return func(_ *corev1.Node, _ *device.NodeDeviceInfo, config *device.NodeConfigInfo) *reason.FilterReason {
			if config.MemoryScaling <= 1 {
				return reason.New(reason.NodeMemoryTypeMismatch).
					WithDetail("requires virtual memory but node memoryScaling=%v", config.MemoryScaling)
			}
			return nil
		}
	}
	if policy == util.PhysicalMemoryPolicy.String() || strings.HasPrefix(policy, "phy") {
		klog.V(4).Infof("Pod <%s> use <%s> memory scheduling policy", klog.KObj(pod), util.PhysicalMemoryPolicy)
		return func(_ *corev1.Node, _ *device.NodeDeviceInfo, config *device.NodeConfigInfo) *reason.FilterReason {
			if config.MemoryScaling > 1 {
				return reason.New(reason.NodeMemoryTypeMismatch).
					WithDetail("requires physical memory but node memoryScaling=%v", config.MemoryScaling)
			}
			return nil
		}
	}
	return func(_ *corev1.Node, _ *device.NodeDeviceInfo, _ *device.NodeConfigInfo) *reason.FilterReason {
		return nil
	}
}

// CheckNodeFunc is one node-level gate. Returning nil means the gate
// accepted the node; returning a non-nil *reason.FilterReason means the
// node fails the gate with the given structured cause.
type CheckNodeFunc func(node *corev1.Node, device *device.NodeDeviceInfo, config *device.NodeConfigInfo) *reason.FilterReason

// CheckNode runs the built-in node prerequisites plus any caller-
// supplied gates. Returns the first failing reason, or nil if every
// gate accepts the node.
func CheckNode(node *corev1.Node, checkNodeFuncs ...CheckNodeFunc) *reason.FilterReason {
	if !util.IsVGPUEnabledNode(node) {
		return reason.New(reason.NodeNotVGPUEnabled)
	}
	devRegister, ok := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
	if !ok || len(devRegister) == 0 {
		klog.V(3).InfoS("node has not registered any GPU devices", "node", node.Name)
		return reason.New(reason.NodeNoVGPURegister)
	}
	var nodeDeviceInfo device.NodeDeviceInfo
	if err := nodeDeviceInfo.Decode(devRegister); err != nil {
		klog.V(3).ErrorS(err, "decoding node device information failed", "node", node.Name)
		return reason.New(reason.NodeBadVGPURegister).WithDetail("%v", err)
	}
	devConfigInfo, ok := util.HasAnnotation(node, util.NodeConfigInfoAnnotation)
	if !ok || len(devConfigInfo) == 0 {
		return reason.New(reason.NodeNoVGPUConfig)
	}
	var nodeConfigInfo device.NodeConfigInfo
	if err := nodeConfigInfo.Decode(devConfigInfo); err != nil {
		klog.V(3).ErrorS(err, "decoding node configuration information failed", "node", node.Name)
		return reason.New(reason.NodeBadVGPUConfig).WithDetail("%v", err)
	}
	if nodeConfigInfo.DeviceSplit <= 0 {
		return reason.New(reason.NodeNotVGPUEnabled).WithDetail("deviceSplit=%d", nodeConfigInfo.DeviceSplit)
	}
	if nodeConfigInfo.MemoryFactor <= 0 {
		return reason.New(reason.NodeBadMemoryFactor).WithDetail("memoryFactor=%d", nodeConfigInfo.MemoryFactor)
	}
	for _, checkFunc := range checkNodeFuncs {
		if r := checkFunc(node, &nodeDeviceInfo, &nodeConfigInfo); r != nil {
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
		var nodeDevice *device.NodeDeviceInfo
		if r := CheckNode(&node, memoryPolicyFunc, func(
			node *corev1.Node, device *device.NodeDeviceInfo,
			config *device.NodeConfigInfo) *reason.FilterReason {
			nodeConfig, nodeDevice = config, device
			return nil
		}); r != nil {
			failed[node.Name] = r
		} else {
			state.Write(nodeDeviceKey(node.Name), nodeDevice)
			state.Write(nodeConfigKey(node.Name), nodeConfig)
			filteredNodes = append(filteredNodes, nodes[i])
		}
	}
	return filteredNodes, failed, nil
}

func nodeDeviceKey(nodeName string) framework.StateKey {
	return framework.StateKey(nodeName + "-device")
}

func nodeConfigKey(nodeName string) framework.StateKey {
	return framework.StateKey(nodeName + "-config")
}

func (f *gpuFilter) CheckDeviceRequest(req *allocator.AllocationRequest, mode filterMode) error {
	checkFuncs := []func(allocator.ContainerNeed) error{
		checkCoreRequest, checkNumberRequest,
	}
	for _, container := range req.Containers {
		for _, checkFn := range checkFuncs {
			if err := checkFn(container); err != nil {
				if !mode.isDryRun() {
					f.recorder.Event(req.Pod, corev1.EventTypeWarning, reason.EventResourceInvalid, err.Error())
				}
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

// FindGangSiblingDomain resolves the gang's cross-node-stable sub-domain (rail)
// SIGNATURE by tallying it across the gang's already-placed siblings and
// returning the majority. Each sibling's signature is resolved on the sibling's
// OWN node by UUID — identity-based, independent of the possibly-stale
// Device.Index recorded in the annotation. `pods` MUST come from the gang-name
// index (so gang membership is already guaranteed — not re-checked here). A
// sibling on a candidate node uses its prebuilt NodeInfo (free); otherwise the
// node is built on demand from nodeLister and CACHED so a node hosting several
// siblings is built at most once. Best-effort: alignment is an optimization,
// never a correctness gate.
// Returns TWO alignment keys, both resolved in the same pass:
//   - domain: the NVLink component signature (coarse; useless on a fully
//     connected node, where every GPU shares one component)
//   - rail: the sorted per-GPU rail keys (fine; the only key that can align
//     single-card gang members, and the one that matters on a rail-optimized
//     fabric)
//
// Either may be "" when nothing resolved.
func FindGangSiblingDomain(
	pods []*corev1.Pod, nodeInfoByName map[string]*allocator.NodeInfo,
	nodeLister listerv1.NodeLister, req *allocator.AllocationRequest,
) (domain string, rail string) {

	domainMap := make(map[string]int)
	railMap := make(map[string]int)
	// built caches NodeInfos constructed on demand for non-candidate sibling
	// nodes, so multiple siblings on one node trigger a single (expensive) build.
	var built map[string]*allocator.NodeInfo
	for _, p := range pods {
		// Gang membership is guaranteed by the IndexerKeyPodGangName query; only
		// self-exclusion and a live pre-allocation remain to be checked.
		if p == nil || p.UID == req.Pod.UID || !device.ShouldCountPodDeviceAllocation(p) {
			continue
		}
		// Resolve the chosen UUIDs first: a sibling without a live pre-allocation
		// contributes nothing, so skip it before paying for any NodeInfo build.
		uuids := device.PodPreAllocatedUUIDs(p)
		if len(uuids) == 0 {
			continue
		}
		nodeName := util.PodPlanSchedulingNode(p)
		nodeInfoW, ok := nodeInfoByName[nodeName]
		if !ok || nodeInfoW == nil {
			// Sibling on a NON-candidate node (the common cross-node case): build
			// its NodeInfo on demand and cache it. The resolved nodeInfo MUST still
			// fall through to the vote below — that is the whole point of the build.
			if built == nil {
				built = make(map[string]*allocator.NodeInfo)
			}
			if nodeInfoW, ok = built[nodeName]; !ok {
				node, err := nodeLister.Get(nodeName)
				if err != nil {
					continue // sibling node unknown → its UUIDs can't be resolved here
				}
				nodeInfo, err := device.NewNodeInfo(node, device.WithGPUTopologyEnabled(true))
				if err != nil {
					continue
				}
				nodeInfoW = &allocator.NodeInfo{NodeInfo: nodeInfo}
				built[nodeName] = nodeInfoW
			}
		}
		if domain, ok := nodeInfoW.DomainOfUUIDs(uuids); ok {
			domainMap[domain]++
		}
		if rail, ok := nodeInfoW.RailSignatureOfUUIDs(uuids); ok {
			railMap[rail]++
		}
	}
	return majorityKey(domainMap), majorityKey(railMap)
}

// majorityKey returns the most-voted key, breaking ties on the lower key for
// determinism. Returns "" when there are no votes.
func majorityKey(votes map[string]int) string {
	keys := maps.Keys(votes)
	switch len(keys) {
	case 0:
		return ""
	case 1:
		return keys[0]
	default:
		sort.Slice(keys, func(i, j int) bool {
			if ci, cj := votes[keys[i]], votes[keys[j]]; ci != cj {
				return ci > cj
			}
			return keys[i] < keys[j]
		})
		return keys[0]
	}
}

func (f *gpuFilter) preFilterNodeInfos(
	ctx context.Context, req *allocator.AllocationRequest, nodes []corev1.Node, state CycleState,
) ([]*allocator.NodeInfo, map[string]int, map[string]*reason.FilterReason, error) {

	nodePodsMap, err := f.podLister.NodeMapByIndexValue(IndexerKeyPodRequestVGPU, "true")
	if err != nil {
		klog.ErrorS(err, "PodLister list all vGPU pods failed")
		return nil, nil, nil, err
	}

	var (
		mutex                = sync.Mutex{}
		failed               = make(map[string]*reason.FilterReason, len(nodes))
		nodeInfoList         = make([]*allocator.NodeInfo, 0, len(nodes))
		nodeOriginalPosition = make(map[string]int, len(nodes))
		nodeInfoByName       map[string]*allocator.NodeInfo
		topologyEnabled      = f.gpuTopology && req.Topology.BaseTopology() == util.LinkTopology
		// nodeInfoByName is consumed only by the cross-pod gang ordinal lookup below.
		// Build and populate it solely when that path will run so the common
		// (non-gang / non-cross-pod) scheduling pays nothing for it.
		needGangOrdinal = req.CrossPodTopology && topologyEnabled && (req.GangName != "" || req.ControllerOwner != nil)
	)
	if needGangOrdinal {
		nodeInfoByName = make(map[string]*allocator.NodeInfo, len(nodes))
	}

	maxGoroutines := runtime.GOMAXPROCS(0) * 2
	batchSize := (len(nodes) + maxGoroutines - 1) / maxGoroutines
	parallel := watcher.NewBatchParallel(len(nodes), batchSize)
	parallel.Execute(func(_ int, config watcher.BatchConfig) {
		startIndex, endIndex, count := config.StartIndex, config.EndIndex, config.Count
		batchNodeInfos := make([]*allocator.NodeInfo, 0, count)
		batchFailed := make(map[string]*reason.FilterReason, count)
		batchNodeOrigPosition := make(map[string]int, count)
		for index := startIndex; index <= endIndex; index++ {
			node := &nodes[index]
			batchNodeOrigPosition[node.Name] = index

			opts := []device.NodeInfoOptionFn{
				device.WithNodePods(nodePodsMap[node.Name]...),
				device.WithExcludedPods(req.Pod.UID),
				device.WithGPUTopologyEnabled(topologyEnabled),
			}
			if read, _ := state.Read(nodeConfigKey(node.Name)); read != nil {
				if nodeConfig, ok := read.(*device.NodeConfigInfo); ok {
					opts = append(opts, device.WithNodeConfig(nodeConfig))
				}
			}
			if read, _ := state.Read(nodeDeviceKey(node.Name)); read != nil {
				if nodeDevice, ok := read.(*device.NodeDeviceInfo); ok {
					opts = append(opts, device.WithNodeDevice(nodeDevice))
				}
			}

			nodeInfo, err := device.NewNodeInfo(node, opts...)
			if err != nil {
				klog.V(3).ErrorS(err, "new NodeInfo failed, skipping node", "node", node.Name)
				batchFailed[node.Name] = reason.New(reason.NodeInfoBuildFailed).WithDetail("%v", err)
				continue
			}
			req := req.GetSnapshot().ResetStatistics(nodeInfo)
			nodeInfoW := &allocator.NodeInfo{
				NodeInfo:          nodeInfo,
				AllocationRequest: req,
			}

			// Pre-allocator capacity gate: reject nodes that obviously
			// can't fit the pod BEFORE letting them into the sorted
			// candidate list. NodeInfo is already built (annotation
			// decode is the dominant cost there and we needed it for the
			// GetAvailable* calls anyway); what we save is the downstream
			// allocator pass — sort comparators, pickDeviceClaims,
			// topology dispatch, per-container Allocate — which would
			// otherwise iterate every node in nodeInfoList. On saturated
			// clusters this is the difference between scanning 5000
			// NodeInfos or just the 50 that still have room.
			//
			// Every check below is a NECESSARY condition only (passing
			// the gate does NOT guarantee the allocator will succeed);
			// the allocator re-verifies exactly, so a too-loose gate just
			// costs wasted work, never a wrong placement. They run in two
			// tiers:
			//
			// Tier 1 — per-single-device CAPACITY (req.Max vs
			// GetMaxDevice* / GetSchedulableDeviceCount). The largest
			// single container needs req.Max.Number distinct cards, each
			// vGPU wanting req.Max.Cores / req.Max.Memory. If even the
			// biggest card on the node can't hold one such vGPU, or the
			// node has fewer schedulable cards than req.Max.Number, no
			// arrangement can ever work — hard structural reject.
			//
			// Tier 2 — node-wide REMAINING totals (req.Total vs
			// GetAvailable*). req.Total is the true pod-wide demand
			// (per-vGPU cores/memory already multiplied by each
			// container's Number), so this fires whenever the pod's total
			// ask exceeds the node's free pool. It stays a necessary
			// condition only because req.*.Memory is UN-scaled (node
			// MemoryFactor applied later) and whole-card memory requests
			// count as 0 — so it never false-rejects; the allocator
			// re-verifies exactly.
			if req.Max.Number > nodeInfo.GetSchedulableDeviceCount() {
				batchFailed[node.Name] = reason.New(reason.InsufficientGPUCards).
					WithDetail("max %d devices, node has %d schedulable", req.Max.Number, nodeInfo.GetSchedulableDeviceCount())
				continue
			}
			if req.Max.Cores > nodeInfo.GetMaxDeviceCores() {
				batchFailed[node.Name] = reason.New(reason.InsufficientVGPUCore).
					WithDetail("max %d cores, largest device has %d", req.Max.Cores, nodeInfo.GetMaxDeviceCores())
				continue
			}
			if req.Max.Memory > nodeInfo.GetMaxDeviceMemory() {
				batchFailed[node.Name] = reason.New(reason.InsufficientVGPUMemory).
					WithDetail("max %d memory, largest device has %d", req.Max.Memory, nodeInfo.GetMaxDeviceMemory())
				continue
			}
			if req.Total.Number > nodeInfo.GetAvailableNumber() {
				batchFailed[node.Name] = reason.New(reason.InsufficientGPUResources).
					WithDetail("need %d number, available %d", req.Total.Number, nodeInfo.GetAvailableNumber())
				continue
			}
			if req.Total.Cores > nodeInfo.GetAvailableCores() {
				batchFailed[node.Name] = reason.New(reason.InsufficientVGPUCore).
					WithDetail("need %d cores, available %d", req.Total.Cores, nodeInfo.GetAvailableCores())
				continue
			}
			if req.Total.Memory > nodeInfo.GetAvailableMemory() {
				batchFailed[node.Name] = reason.New(reason.InsufficientVGPUMemory).
					WithDetail("need %d memory, available %d", req.Total.Memory, nodeInfo.GetAvailableMemory())
				continue
			}

			// Reject nodes that can't satisfy the pod's include/exclude
			// GPU UUID / type constraints. CheckDeviceUuid/Type return
			// true when a device is ALLOWED by the annotations, so a node
			// is viable only if it has at least req.Max.Number devices
			// passing every requested check (the largest container needs
			// that many distinct allowed cards). Reject only when too few
			// qualify — NOT when any single device fails, since an
			// include filter naturally excludes most of a node's cards.
			// Necessary-condition pre-check; the allocator's filterDevices
			// re-verifies exactly.
			if req.CheckDeviceUuid || req.CheckDeviceType {
				matched := 0
				for _, dev := range nodeInfo.GetDeviceMap() {
					if req.CheckDeviceUuid && !util.CheckDeviceUuid(req.Pod.Annotations, dev.GetUUID()) {
						continue
					}
					if req.CheckDeviceType && !util.CheckDeviceType(req.Pod.Annotations, dev.GetType()) {
						continue
					}
					matched++
				}
				if matched < req.Max.Number {
					rc := reason.DeviceTypeMismatch
					if req.CheckDeviceUuid {
						rc = reason.DeviceUUIDMismatch
					}
					batchFailed[node.Name] = reason.New(rc).
						WithDetail("only %d of %d required devices match the requested GPU uuid/type", matched, req.Max.Number)
					continue
				}
			}

			batchNodeInfos = append(batchNodeInfos, nodeInfoW)
		}

		mutex.Lock()
		maps.Copy(failed, batchFailed)
		for _, nodeInfo := range batchNodeInfos {
			if needGangOrdinal {
				nodeInfoByName[nodeInfo.GetName()] = nodeInfo
			}
			nodeInfoList = append(nodeInfoList, nodeInfo)
		}
		maps.Copy(nodeOriginalPosition, batchNodeOrigPosition)
		mutex.Unlock()
	})
	parallel.WaitDone()

	// Quickly return results
	if len(nodeInfoList) == 0 {
		return nodeInfoList, nodeOriginalPosition, failed, nil
	}

	// Cross-node sub-domain (rail) alignment: when this pod opts into cross-pod
	// link topology and is in a gang, resolve the gang's chosen sub-domain
	// signature from any already-placed sibling and carry it (node-independent) on
	// req. Each node later maps it back to its own component via ComponentByDomain.
	// The signature is resolved on the SIBLING's own NodeInfo by UUID (identity-
	// based, dedup'd), so it does not depend on the possibly-stale Device.Index in
	// the annotation; we only need the sibling's node to be among the built
	// candidates (the common case under Kueue rack-pinning). Reuses nodePodsMap +
	// nodeInfoList (no extra List / NodeInfo build). Gang-only; others skip it.
	if needGangOrdinal {
		var gangPods []*corev1.Pod
		switch {
		case req.GangName != "":
			if gangPods, err = f.podLister.ListByIndexValue(IndexerKeyPodGangName, req.GangName); err != nil {
				klog.ErrorS(err, "PodLister list same gang pods failed", "gangName", req.GangName)
				return nodeInfoList, nodeOriginalPosition, failed, err
			}
		case req.ControllerOwner != nil:
			if gangPods, err = f.podLister.ListByIndexValue(IndexerKeyControlOwnerUID, string(req.ControllerOwner.UID)); err != nil {
				klog.ErrorS(err, "PodLister list same controller owner reference pods failed", "controllerOwner", *req.ControllerOwner)
				return nodeInfoList, nodeOriginalPosition, failed, err
			}
		}
		req.GangDomainKey, req.GangRailKey = FindGangSiblingDomain(gangPods, nodeInfoByName, f.nodeLister, req)
	}

	return nodeInfoList, nodeOriginalPosition, failed, nil
}

// deviceFilterFunc binds deviceFilter to a mode so it fits the filterFunc chain.
func (f *gpuFilter) deviceFilterFunc(mode filterMode) filterFunc {
	return func(ctx context.Context, req *allocator.AllocationRequest, nodes []corev1.Node, state CycleState) ([]corev1.Node, map[string]*reason.FilterReason, error) {
		return f.deviceFilter(ctx, req, nodes, state, mode)
	}
}

// deviceFilter runs the allocator against every candidate, and is always the
// last stage of the chain. In live mode it commits a pre-allocation on the
// first node that fits and stops there, so it returns one node; in dry-run mode
// it commits nothing and returns EVERY node that fits, because the caller is
// asking which node shapes could host the pod, not where to put it.
func (f *gpuFilter) deviceFilter(
	ctx context.Context, req *allocator.AllocationRequest, nodes []corev1.Node, state CycleState, mode filterMode,
) ([]corev1.Node, map[string]*reason.FilterReason, error) {
	if err := f.CheckDeviceRequest(req, mode); err != nil {
		klog.V(2).ErrorS(err, "Check device request failed", "pod", klog.KObj(req.Pod), "dryRun", mode.isDryRun())
		return nil, nil, err
	}

	var filteredNodes []corev1.Node
	if !mode.isDryRun() {
		// Skip pods that have already been scheduled.
		if nodeName, ok := IsScheduled(req.Pod); ok {
			if device.ShouldCountPodDeviceAllocation(req.Pod) {
				// Pre-allocation is current; steer the pod back to its predicated node.
				foundNode := false
				failed := make(map[string]*reason.FilterReason, len(nodes))
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
				return nil, nil, fmt.Errorf("pod %s had been predicated", req.Pod.UID)
			}
			// Pre-allocation is stale or stuck — re-trigger device pre-allocation.
			klog.V(3).InfoS("Re-triggering device pre allocation for pod", "pod", klog.KObj(req.Pod))
		}

		// Dry-run stays off this lock on purpose: it writes nothing, and a
		// simulation burst must never queue behind — or ahead of — live scheduling.
		lockStart := time.Now()
		f.locker.Lock()
		defer f.locker.Unlock()
		// Recorded separately from the stage total: SerializedNodeFilter is on by
		// default, so on a busy cluster this is queueing, not work, and folding the
		// two together makes contention look like slow allocation.
		metrics.ObserveStage(metrics.VerbFilter, metrics.StageLockWait, lockStart)

		// Ensure that the context has not timed out
		if err := ctx.Err(); err != nil {
			klog.V(3).ErrorS(err, "Context error", "pod", klog.KObj(req.Pod))
			return nil, nil, err
		}
	}

	nodeInfoList, nodeOriginalPosition, failed, err := f.preFilterNodeInfos(ctx, req, nodes, state)
	// failed carries every rejection the capacity pre-gate collected, so it must
	// be returned even when nothing survived — that is exactly the case the
	// caller most needs a reason for.
	if err != nil || len(nodeInfoList) == 0 {
		return nil, failed, err
	}
	f.sortNodeInfos(req, nodeInfoList, nodeOriginalPosition, mode)

	recorder := f.recorder
	for i, nodeInfo := range nodeInfoList {
		node := nodeInfo.GetNode()
		if !mode.isDryRun() && len(filteredNodes) > 0 {
			failed[node.Name] = reason.New(reason.AlreadyScheduledElsewhere).
				WithDetail("pod already matched to %s in this Filter pass", filteredNodes[0].Name)
			continue
		}
		if i > 0 { // Only send one event.
			recorder = nil
		}
		// Attempt to allocate devices for pods on this node. A dry-run
		// allocation is hypothetical, so it stays out of events and metrics.
		alloc := allocator.NewAllocator(nodeInfo.NodeInfo, recorder)
		if mode.isDryRun() {
			alloc = allocator.NewSimulationAllocator(nodeInfo.NodeInfo)
		}
		newPod, rsn, err := alloc.Allocate(req)
		if err != nil {
			// Internal/programmer error (annotation encoding, accounting
			// bug). Don't just skip the node — bubble up so the whole
			// Filter call returns Error and the operator notices.
			klog.ErrorS(err, "node device allocate internal error", "node", node.Name, "pod", klog.KObj(req.Pod))
			return filteredNodes, failed, err
		}
		if rsn != nil {
			klog.V(4).InfoS("node device allocate rejected", "node", node.Name, "pod", klog.KObj(req.Pod), "reason", rsn.Detailed())
			failed[node.Name] = rsn
			continue
		}
		if mode.isDryRun() {
			// Feasibility answered — nothing to commit, keep scanning so the
			// caller sees the whole feasible set.
			filteredNodes = append(filteredNodes, *node)
			continue
		}
		// Ensure that the context has not timed out
		if err := ctx.Err(); err != nil {
			klog.V(3).ErrorS(err, "Context error", "pod", klog.KObj(req.Pod))
			return filteredNodes, failed, err
		}
		if err = client.PatchPodPreAllocatedMetadata(f.kubeClient, newPod); err != nil {
			klog.ErrorS(err, "patch vGPU metadata failed", "pod", klog.KObj(req.Pod), "node", node.Name)
			return filteredNodes, failed, err
		}
		// Cache the patched Pod locally to bridge the informer watch lag.
		// Concurrent Filter calls on neighbouring pods would otherwise rebuild
		// NodeInfo from a stale informer view (without our pre-allocated
		// annotation) and miscount free GPU.
		f.podLister.Mutation(newPod)
		filteredNodes = append(filteredNodes, *node)
		// PER POD, emitted for the node that actually accepted it.
		recordPlacement(req)
	}
	if len(filteredNodes) > 0 {
		if mode.isDryRun() {
			klog.V(2).InfoS("DryRun filter found feasible nodes", "pod",
				klog.KObj(req.Pod), "feasibleNodes", len(filteredNodes), "failedNodes", len(failed))
		} else {
			f.recorder.Eventf(req.Pod, corev1.EventTypeNormal, reason.EventFilteringSucceed,
				"Successfully matched node %q", filteredNodes[0].Name)
		}
	}
	return filteredNodes, failed, nil
}

// sortNodeInfos orders candidates by the pod's node policy, falling back to the
// request order (with topology tie-breakers) when no policy applies. Dry-run
// returns every feasible node regardless, but keeping one ordering means both
// modes agree on which node they would prefer.
func (f *gpuFilter) sortNodeInfos(
	req *allocator.AllocationRequest, nodeInfoList []*allocator.NodeInfo,
	nodeOriginalPosition map[string]int, mode filterMode,
) {
	var defaultSortPriority bool
	switch req.NodePolicy {
	case util.BinpackPolicy, util.SpreadPolicy:
		klog.V(4).InfoS("Pod node scheduling policy", "pod", klog.KObj(req.Pod), "policy", req.NodePolicy, "dryRun", mode.isDryRun())
		allocator.NewNodePolicyPriority(*req).Sort(nodeInfoList)
	case util.NonePolicy:
		klog.V(4).InfoS("Pod node scheduling policy", "pod", klog.KObj(req.Pod), "policy", req.NodePolicy, "dryRun", mode.isDryRun())
		defaultSortPriority = true
	default:
		klog.V(4).InfoS("Pod not supported node scheduling policy", "pod", klog.KObj(req.Pod), "policy", req.NodePolicy, "dryRun", mode.isDryRun())
		defaultSortPriority = true
		if !mode.isDryRun() {
			f.recorder.Eventf(req.Pod, corev1.EventTypeWarning, reason.EventPolicyInvalid, "unsupported node scheduling policy %q", req.NodePolicy)
		}
	}
	if defaultSortPriority {
		less := allocator.ApplyTopologyMode(*req, func(p1, p2 *allocator.NodeInfo) bool {
			return nodeOriginalPosition[p1.GetName()] < nodeOriginalPosition[p2.GetName()]
		})
		allocator.NewSortPriority[*allocator.NodeInfo](less...).Sort(nodeInfoList)
	}
}

// recordNodeRejects publishes the per-Code rejection counts.
//
// AlreadyScheduledElsewhere is deliberately EXCLUDED: it is stamped on every
// remaining candidate once some node has accepted the pod, so counting it would
// add one bump per non-selected node on every SUCCESSFUL schedule and swamp the
// genuine rejection causes. Those nodes were not rejected — they were not
// needed.
func recordNodeRejects(verb string, reasons map[string]*reason.FilterReason) {
	for _, r := range reasons {
		if r == nil || r.Primary == reason.AlreadyScheduledElsewhere {
			continue
		}
		metrics.RecordNodeReject(verb, string(r.Primary))
	}
}

// recordPlacement publishes the PER POD metrics, once, for the node that
// actually accepted the pod.
//
// Emitted here rather than in the allocator because only the filter knows which
// candidate won: the allocator runs per node and would report a single pod as
// several placements. nodeReq is the winning node's request snapshot, carrying
// the topology outcome the allocator recorded on it.
func recordPlacement(req *allocator.AllocationRequest) {
	// Every annotation-derived value goes through the metrics package's
	// whitelist: the parsers pass unknown values through verbatim, which would
	// otherwise make label cardinality user-controlled.
	mode := metrics.TopologyLabel(req.Topology.BaseTopology())
	metrics.PodPolicyTotal.WithLabelValues(
		metrics.PolicyLabel(req.NodePolicy), metrics.PolicyLabel(req.DevicePolicy), mode,
	).Inc()

	// Read from the SAME request the allocator was handed: the per-node snapshot
	// on nodeInfo is copied before Allocate runs and never receives the outcome.
	outcome := req.TopologyOutcome()
	if outcome.Result != "" {
		metrics.TopologyPlacementTotal.WithLabelValues(mode, outcome.Result).Inc()
	}
	if outcome.Alignment != "" {
		metrics.CrossPodAlignmentTotal.WithLabelValues(outcome.Alignment).Inc()
	}
}
