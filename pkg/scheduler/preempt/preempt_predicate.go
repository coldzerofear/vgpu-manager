// Package preempt implements the scheduler extender PreemptVerb endpoint.
//
// kube-scheduler's default preemption proposes victim sets using only in-tree
// filter plugins (RunFilterPluginsWithNominatedPods does not call extenders).
// For pods that pass in-tree count checks but fail extender constraints
// (per-device packing, UUID / type / NUMA topology, oversold memory), the
// in-tree victim set is wrong: too few victims, the wrong victims, or none at
// all. This plugin re-validates each candidate node against the vGPU view and
// returns a corrected set.
//
// Hard limit (kube-scheduler 1.32 source): callExtenders runs only AFTER
// in-tree dry-run produces >=1 candidate node with >=1 victim. If in-tree's
// SelectVictimsOnNode reprieves every lower-priority pod (because its
// resource accounting says the pod fits without removing anyone), the node
// is dropped before reaching us. The Helm chart's managedResources entry for
// vgpu-number must use ignoredByScheduler=false so in-tree at least sees
// count pressure; memory/cores can remain ignored since per-device packing
// is what we validate here.
package preempt

import (
	"context"
	"runtime"
	"sort"
	"sync"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/allocator"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/filter"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/predicate"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	policyv1 "k8s.io/client-go/listers/policy/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
)

const (
	Name                     = "PreemptPredicate"
	IndexerKeyPodMetadataUid = "pod.metadata.uid"
)

type vgpuPreempt struct {
	nodeLister  listerv1.NodeLister
	podLister   client.PodLister
	pdbLister   policyv1.PodDisruptionBudgetLister
	recorder    record.EventRecorder
	gpuTopology bool
	hasSyncFunc func(ctx context.Context) bool
}

var (
	_           predicate.PreemptPredicate = &vgpuPreempt{}
	PodIndexers                            = cache.Indexers{
		IndexerKeyPodMetadataUid: func(obj interface{}) ([]string, error) {
			var indexerValues []string
			if accessor, err := meta.Accessor(obj); err == nil {
				indexerValues = []string{string(accessor.GetUID())}
			}
			return indexerValues, nil
		},
	}
)

// New wires the preempt plugin to the same informer-backed pod lister that
// the filter plugin uses, so per-node pod lookups go through the indexer that
// already filters by vGPU resource requests.
func New(kubeClient kubernetes.Interface, factory informers.SharedInformerFactory, recorder record.EventRecorder,
	podLister client.PodLister, gpuTopology bool) (*vgpuPreempt, error) {
	podInformer := factory.Core().V1().Pods().Informer()
	nodeInformer := factory.Core().V1().Nodes().Informer()
	if err := podInformer.AddIndexers(PodIndexers); err != nil {
		return nil, err
	}
	nodeLister := listerv1.NewNodeLister(nodeInformer.GetIndexer())
	pdbLister, pdbInformer, err := client.NewPDBLister(kubeClient, factory)
	if err != nil {
		return nil, err
	}
	hasSyncFunc := func(ctx context.Context) bool {
		return cache.WaitForCacheSync(
			ctx.Done(),
			podInformer.HasSynced,
			nodeInformer.HasSynced,
			pdbInformer.HasSynced,
		)
	}
	return &vgpuPreempt{
		nodeLister:  nodeLister,
		podLister:   podLister,
		pdbLister:   pdbLister,
		recorder:    recorder,
		gpuTopology: gpuTopology,
		hasSyncFunc: hasSyncFunc,
	}, nil
}

func (p *vgpuPreempt) Name() string {
	return Name
}

func (p *vgpuPreempt) IsReady(ctx context.Context) bool {
	return p.hasSyncFunc(ctx)
}

func NodeVictims(args extenderv1.ExtenderPreemptionArgs) map[string]*extenderv1.MetaVictims {
	if len(args.NodeNameToMetaVictims) > 0 {
		return args.NodeNameToMetaVictims
	}
	nodeVictims := make(map[string]*extenderv1.MetaVictims, len(args.NodeNameToVictims))
	for node, victims := range args.NodeNameToVictims {
		if victims == nil {
			continue
		}
		metaVictims := extenderv1.MetaVictims{
			Pods:             make([]*extenderv1.MetaPod, len(victims.Pods)),
			NumPDBViolations: victims.NumPDBViolations,
		}
		for i, pod := range victims.Pods {
			metaVictims.Pods[i] = &extenderv1.MetaPod{
				UID: string(pod.GetUID()),
			}
		}
		nodeVictims[node] = &metaVictims
	}
	return nodeVictims
}

// Preempt iterates the candidate nodes in-tree proposed and, for each, asks
// our allocator whether the pending pod can fit after the proposed victims
// are removed. If yes, we keep the set (modulo protected pods). If not, we
// search for additional lower-priority victims on that node until the
// allocator accepts; if no such set exists, we drop the node.
func (p *vgpuPreempt) Preempt(ctx context.Context, args extenderv1.ExtenderPreemptionArgs) *extenderv1.ExtenderPreemptionResult {
	klog.V(4).InfoS("PreemptPod", "pod", klog.KObj(args.Pod), "nodeVictims", NodeVictims(args))
	pod := args.Pod
	if pod == nil {
		klog.V(4).InfoS("Preempt called with nil pod, passing input through")
		return passthrough(args)
	}
	// Non-vGPU pods are out of our scope; let the in-tree decision stand.
	req := allocator.BuildAllocationRequest(pod)
	if len(req.Containers) == 0 {
		klog.V(5).InfoS("Preempt: pod is not a vGPU pod, passing input through", "pod", klog.KObj(pod))
		return passthrough(args)
	}

	victimsMap, err := p.resolveVictimsMap(args)
	if err != nil {
		klog.ErrorS(err, "Preempt: failed to resolve victims, passing input through")
		return passthrough(args)
	}

	result := &extenderv1.ExtenderPreemptionResult{
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{},
	}

	if len(victimsMap) == 0 {
		return result
	}

	nodePodsMap, err := p.podLister.NodeMapByIndexValue(filter.IndexerKeyPodRequestVGPU, "true")
	if err != nil {
		klog.ErrorS(err, "PodLister list vGPU pods failed in preempt")
		return passthrough(args)
	}

	mu := sync.Mutex{}
	gangNameSet := sets.Set[string]{}

	victimKeys := maps.Keys(victimsMap)
	maxGoroutines := runtime.GOMAXPROCS(0) * 2
	batchSize := (len(victimKeys) + maxGoroutines - 1) / maxGoroutines
	parallel := watcher.NewBatchParallel(len(victimKeys), batchSize)

	nodeInfoByName := make(map[string]*device.NodeInfo, len(victimsMap))
	nodeRequestMap := make(map[string]*allocator.AllocationRequest, len(victimsMap))
	topologyEnabled := p.gpuTopology && req.Topology.BaseTopology() == util.LinkTopology

	parallel.Execute(func(_ int, config watcher.BatchConfig) {
		for _, nodeName := range victimKeys[config.StartIndex : config.EndIndex+1] {
			node, err := p.nodeLister.Get(nodeName)
			if err != nil {
				klog.V(3).ErrorS(err, "Preempt: get node failed", "node", nodeName)
				continue
			}
			nodeInfo, err := device.NewNodeInfo(node, device.WithGPUTopologyEnabled(topologyEnabled))
			if err != nil {
				filterReason := reason.New(reason.NodeInfoBuildFailed).WithDetail("%v", err)
				klog.V(3).ErrorS(err, "Preempt: "+string(filterReason.Primary), "node", node.Name, "pod", klog.KObj(req.Pod), "reason", filterReason.Detailed())
				continue
			}
			snapshot := req.GetSnapshot()
			snapshot.ResetStatistics(nodeInfo)

			mu.Lock()
			nodeInfoByName[nodeName] = nodeInfo
			nodeRequestMap[nodeName] = snapshot
			mu.Unlock()
		}
	})
	parallel.WaitDone()

	if req.CrossPodTopology && topologyEnabled && (req.GangName != "" || req.ControllerOwner != nil) {
		var gangPods []*corev1.Pod
		switch {
		case req.GangName != "":
			gangPods, err = p.podLister.ListByIndexValue(filter.IndexerKeyPodGangName, req.GangName)
			if err != nil {
				klog.ErrorS(err, "PodLister list same gang pods failed", "gangName", req.GangName)
				return passthrough(args)
			}
		case req.ControllerOwner != nil:
			gangPods, err = p.podLister.ListByIndexValue(filter.IndexerKeyControlOwnerUID, string(req.ControllerOwner.UID))
			if err != nil {
				klog.ErrorS(err, "PodLister list same controller owner reference pods failed", "controllerOwner", *req.ControllerOwner)
				return passthrough(args)
			}
		}
		if domain, ok := filter.FindGangSiblingDomain(gangPods, nodeInfoByName, p.nodeLister, req); ok {
			req.GangDomainKey = domain
		}
	}

	parallel.Execute(func(_ int, config watcher.BatchConfig) {
		for _, nodeName := range victimKeys[config.StartIndex : config.EndIndex+1] {
			victims := victimsMap[nodeName]
			if victims == nil {
				continue
			}
			nodeInfo, ok := nodeInfoByName[nodeName]
			if !ok {
				continue
			}
			refined, pdbViolations, ok := p.refineForNode(nodeRequestMap[nodeName], nodeInfo, victims, nodePodsMap[nodeName])
			if !ok {
				klog.V(2).InfoS("Preempt: node cannot fit pod even after preemption, dropping",
					"pod", klog.KObj(pod), "node", nodeName)
				continue
			}
			if len(refined) == 0 {
				// Should not happen — in-tree gave us at least one and we never empty the list.
				continue
			}
			meta := &extenderv1.MetaVictims{
				Pods:             make([]*extenderv1.MetaPod, 0, len(refined)),
				NumPDBViolations: pdbViolations,
			}
			var gangNames []string
			for _, vp := range refined {
				if gangName, ok := util.PodHasGangName(vp); ok {
					gangNames = append(gangNames, gangName)
				}
				meta.Pods = append(meta.Pods, &extenderv1.MetaPod{UID: string(vp.UID)})
			}

			mu.Lock()
			gangNameSet.Insert(gangNames...)
			result.NodeNameToMetaVictims[nodeName] = meta
			mu.Unlock()
		}
	})
	parallel.WaitDone()

	if gangNameSet.Len() > 0 {
		p.recorder.Eventf(pod, corev1.EventTypeWarning, reason.EventGangDisrupted,
			"evicted as preemption victim; this may trigger rescheduling of these pod groups %v", sets.List(gangNameSet))
	}

	// If every candidate node was dropped (the allocator rejected the
	// pod even with the proposed victims removed), surface a
	// kube-scheduler-style "preemption: 0/N nodes are available" event
	// so operators see WHY preemption didn't help. Without this, the
	// user only sees the in-tree "no preemption victims found" message
	// and has no signal that vgpu-manager itself vetoed every option.
	if len(result.NodeNameToMetaVictims) == 0 && len(victimsMap) > 0 && p.recorder != nil {
		p.recorder.Eventf(pod, corev1.EventTypeWarning, reason.EventPreemptionFailed,
			"preemption: 0/%d nodes are available: vgpu-manager rejects allocation on every candidate after victim removal",
			len(victimsMap))
	}

	return result
}

// resolveVictimsMap normalizes the two input modes of ExtenderPreemptionArgs
// into a single map[string]*Victims with fully-populated *Pod objects.
//
// kube-scheduler sends one of two encodings depending on the extender's
// nodeCacheCapable setting:
//   - false: NodeNameToVictims carries full *Pod objects
//   - true:  NodeNameToMetaVictims carries only UIDs (smaller payload)
//
// Our chart sets nodeCacheCapable=true, so in production we get the second
// form and must look the pods back up from the informer cache. We still
// accept the first form for resilience against future config changes and for
// tests that craft the args directly.
//
// Victims may be non-vGPU pods (default preemption can propose any
// lower-priority pod as victim before reprieving via NodeResourcesFit), so
// the lookup runs against the full pod list rather than the vGPU index.
// UIDs that can no longer be resolved (pod already deleted) are silently
// dropped — they cannot be evicted anyway.
func (p *vgpuPreempt) resolveVictimsMap(args extenderv1.ExtenderPreemptionArgs) (map[string]*extenderv1.Victims, error) {
	if len(args.NodeNameToVictims) > 0 {
		return args.NodeNameToVictims, nil
	}
	if len(args.NodeNameToMetaVictims) == 0 {
		return nil, nil
	}
	resolved := make(map[string]*extenderv1.Victims, len(args.NodeNameToMetaVictims))
	for nodeName, meta := range args.NodeNameToMetaVictims {
		if meta == nil {
			continue
		}
		pods := make([]*corev1.Pod, 0, len(meta.Pods))
		for _, mp := range meta.Pods {
			if mp == nil {
				continue
			}
			objs, err := p.podLister.ListByIndexValue(IndexerKeyPodMetadataUid, mp.UID)
			if err != nil || len(objs) == 0 {
				klog.V(3).InfoS("Preempt: victim UID not found in cache, skipping",
					"uid", mp.UID, "node", nodeName, "err", err)
				continue
			}
			pods = append(pods, objs[0])
		}
		if len(pods) == 0 {
			// in-tree proposed victims but we lost track of them; drop the node
			continue
		}
		resolved[nodeName] = &extenderv1.Victims{
			Pods:             pods,
			NumPDBViolations: meta.NumPDBViolations,
		}
	}
	return resolved, nil
}

// refineForNode validates the proposed victim set against the vGPU allocator.
// Returns (victims, numPDBViolations, true) when the set works (possibly
// augmented with additional victims), or (nil, 0, false) when no achievable
// victim set on this node lets the pod fit.
//
// On PDB: numPDBViolations is a conservative upper bound computed by
// pdbViolationsUpperBound — we don't carry a PDB lister at this layer
// (kube-scheduler's pickOneNodeForPreemption uses it for ordering only;
// actual deletion goes through Pods().Delete() which bypasses PDB
// enforcement). Over-reporting is safer than under-reporting: it discourages
// the scheduler from picking us when better candidates exist, rather than
// luring it into inflicting more real disruption than necessary. If a
// workload must be protected from vGPU preemption, the only mechanism that
// works is giving it sufficient priority.
func (p *vgpuPreempt) refineForNode(req *allocator.AllocationRequest, nodeInfo *device.NodeInfo,
	victims *extenderv1.Victims, allVGPUPods []*corev1.Pod) ([]*corev1.Pod, int64, bool) {
	node := nodeInfo.GetNode()
	nodeName := nodeInfo.GetName()

	// Fast-reject: if the node itself doesn't meet vGPU prerequisites,
	// preempting any pod on it won't help.
	if r := filter.CheckNode(node, filter.GetMemoryPolicyFunc(req.Pod)); r != nil {
		klog.V(3).InfoS("Preempt: check node failed", "node", nodeName, "pod", klog.KObj(req.Pod), "reason", r.Detailed())
		return nil, 0, false
	}
	if req.Max.Number > nodeInfo.GetSchedulableDeviceCount() {
		filterReason := reason.New(reason.InsufficientGPUCards).
			WithDetail("max %d devices, node has %d schedulable", req.Max.Number, nodeInfo.GetSchedulableDeviceCount())
		klog.V(3).InfoS("Preempt: "+string(filterReason.Primary), "node", nodeName, "pod", klog.KObj(req.Pod), "reason", filterReason.Detailed())
		return nil, 0, false
	}
	if req.Max.Cores > nodeInfo.GetMaxDeviceCores() {
		filterReason := reason.New(reason.InsufficientVGPUCore).
			WithDetail("max %d cores, largest device has %d", req.Max.Cores, nodeInfo.GetMaxDeviceCores())
		klog.V(3).InfoS("Preempt: "+string(filterReason.Primary), "node", nodeName, "pod", klog.KObj(req.Pod), "reason", filterReason.Detailed())
		return nil, 0, false
	}
	if req.Max.Memory > nodeInfo.GetMaxDeviceMemory() {
		filterReason := reason.New(reason.InsufficientVGPUMemory).
			WithDetail("max %d memory, largest device has %d", req.Max.Memory, nodeInfo.GetMaxDeviceMemory())
		klog.V(3).InfoS("Preempt: "+string(filterReason.Primary), "node", nodeName, "pod", klog.KObj(req.Pod), "reason", filterReason.Detailed())
		return nil, 0, false
	}
	// CheckDeviceUuid/Type return true when a device is ALLOWED by the
	// pod's include/exclude annotations. The node can host the pod only if
	// at least req.Max.Number devices pass every requested check (the
	// largest container needs that many allowed cards). Reject only when
	// too few qualify — NOT on the first non-matching device, since an
	// include filter naturally excludes most of a node's cards.
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
			klog.V(3).InfoS("Preempt: "+string(rc), "node", nodeName, "pod", klog.KObj(req.Pod),
				"matched", matched, "need", req.Max.Number)
			return nil, 0, false
		}
	}

	// Strip protected pods from in-tree's proposal — never evict them.
	keep := make([]*corev1.Pod, 0, len(victims.Pods))
	excludedUidSet := sets.New[k8stypes.UID]()
	for _, v := range victims.Pods {
		if v == nil {
			continue
		}
		if isProtectedFromPreemption(v, req.GangName) {
			klog.V(4).InfoS("Preempt: refusing to evict protected pod proposed by in-tree", "pod", klog.KObj(v), "node", nodeName)
			continue
		}
		keep = append(keep, v)
		excludedUidSet.Insert(v.UID)
	}
	// Snapshot the post-protection length so pdbViolationsUpperBound can
	// distinguish "carried over from input" from "newly appended below".
	keptFromInput := len(keep)

	// First pass: does the pending pod fit after removing the kept victims?
	if p.canAllocate(req, nodeInfo, allVGPUPods, excludedUidSet) {
		return keep, pdbViolationsUpperBound(victims.NumPDBViolations, keptFromInput, 0), true
	}

	// Second pass: in-tree under-selected (likely because per-device or
	// annotation constraints invisible to it require more victims). Walk the
	// remaining lower-priority pods on this node and greedily add until fit.
	additional := p.findAdditionalVictims(req, node, allVGPUPods, excludedUidSet)
	for _, cand := range additional {
		excludedUidSet.Insert(cand.UID)
		keep = append(keep, cand)
		if p.canAllocate(req, nodeInfo, allVGPUPods, excludedUidSet) {
			added := len(keep) - keptFromInput
			return keep, pdbViolationsUpperBound(victims.NumPDBViolations, keptFromInput, added), true
		}
	}
	return nil, 0, false
}

// pdbViolationsUpperBound is a conservative estimate of how many PDB
// violators are in the refined victim set, computed WITHOUT a PDB lister.
//
// The exact answer would need PDB lookups matching every victim against
// every PDB selector (kube-scheduler does this in-tree via
// filterPodsWithPDBViolation). To stay lister-free in Phase 1, we instead
// use an upper bound:
//
//   - of the original `originalCount` violators in the input, at most
//     min(originalCount, keptLen) survived the protection filter
//     (the dropped victims may or may not have been violators — we
//     assume the worst case "none of the dropped were violators");
//   - of the `addedLen` victims we appended via findAdditionalVictims,
//     at most all of them are new violators.
//
// Why err on the high side? NumPDBViolations is the ordering signal in
// kube-scheduler's pickOneNodeForPreemption — fewer reported violations
// makes our node more attractive. Under-reporting would tempt the
// scheduler to pick our node and inflict more real disruption than it
// should. Over-reporting only costs us being chosen less often; correct
// preemption still happens, just maybe on a different node.
//
// The returned value is guaranteed ≤ keptLen + addedLen so the invariant
// NumPDBViolations ≤ len(Pods) holds for our response.
func pdbViolationsUpperBound(originalCount int64, keptLen, addedLen int) int64 {
	carriedOver := originalCount
	if int64(keptLen) < carriedOver {
		carriedOver = int64(keptLen)
	}
	return carriedOver + int64(addedLen)
}

// canAllocate constructs a NodeInfo with excluded pods removed and asks the
// allocator whether the pending pod fits. Reuses the same NewNodeInfo
// excludedPods mechanism already used during the filter re-allocation path.
func (p *vgpuPreempt) canAllocate(req *allocator.AllocationRequest, nodeInfo *device.NodeInfo,
	allVGPUPods []*corev1.Pod, excludedUidSet sets.Set[k8stypes.UID]) bool {
	nodeInfo.AddPodsUsedResources(allVGPUPods, device.WithExcludedUidSet(excludedUidSet),
		device.WithResetPods(true), device.WithResetUsed(true))

	// Preempt only cares about "would this pod fit?", not why it might
	// not. Both a structured reason (node would still reject) and a real
	// internal error answer the same question: "no". Log at V(5) for
	// debugging; the verb-level Preempt event captures the user-facing
	// outcome.
	_, rsn, err := allocator.NewAllocator(nodeInfo, nil).Allocate(req)
	switch {
	case err != nil:
		klog.V(3).ErrorS(err, "Preempt: allocator internal error",
			"pod", klog.KObj(req.Pod), "node", nodeInfo.GetName())
		return false
	case rsn != nil:
		klog.V(5).InfoS("Preempt: allocator rejects pod after excluding victims",
			"pod", klog.KObj(req.Pod), "node", nodeInfo.GetName(), "reason", rsn.Detailed())
		return false
	}
	return true
}

// findAdditionalVictims returns lower-priority vGPU pods on the given node
// (excluding those already chosen), sorted in preferred-to-evict order.
//
// Why we need this at all: kube-scheduler's args.NodeNameToVictims is NOT a
// complete list of preemption-eligible pods on the node — it is the minimal
// set in-tree reprieve picked using its own (in-tree-visible) filters. When
// our extender has constraints in-tree can't see (per-device packing,
// include-gpu-uuid / include-gpu-type / device-topology-mode), in-tree may
// reprieve the WRONG pods and propose a victim whose eviction doesn't satisfy
// us. Example: A occupies GPU-Y, B occupies GPU-X, preemptor needs GPU-X.
// If in-tree's reprieve order happens to keep B (still on GPU-X) and propose
// A as victim, our allocator rejects [A] — but we can succeed by appending B
// from the broader lower-priority pool that in-tree reprieved.
//
// Trade-off — over-eviction: this function only APPENDS to the proposed set,
// never replaces. In the example above we return [A, B] when [B] alone would
// suffice (A is reaped for nothing). A future Phase 2 optimization could
// perform a minimal-subset search over the pool of (in-tree's victims ∪
// other lower-priority pods on the node), but for now we accept the
// over-eviction to keep the algorithm linear and predictable.
//
// Candidate restriction: candidates must have Spec.NodeName == node.Name
// (i.e., actually bound). kube-scheduler's convertPodUIDToPod looks UIDs up
// in nodeInfo.Pods, which only contains bound pods. Returning a UID for a
// pod that only has the predicate-node annotation (pre-bind / stuck pre-
// bind) would fail UID resolution and the scheduler would reject the ENTIRE
// preemption response. Stuck pods that occupy predicate-node without ever
// binding cannot be evicted through this path — they must be reclaimed by a
// separate controller or by the existing fresh-window grace mechanism.
func (p *vgpuPreempt) findAdditionalVictims(req *allocator.AllocationRequest, node *corev1.Node,
	allVGPUPods []*corev1.Pod, excludedUidSet sets.Set[k8stypes.UID]) []*corev1.Pod {

	priority := corev1helpers.PodPriority(req.Pod)
	out := make([]*corev1.Pod, 0)
	for _, candidate := range allVGPUPods {
		if candidate.UID == req.Pod.UID {
			continue
		}
		if excludedUidSet.Has(candidate.UID) {
			continue
		}
		// Must be actually bound to this node — see function doc.
		if candidate.Spec.NodeName != node.Name {
			continue
		}
		if corev1helpers.PodPriority(candidate) >= priority {
			continue
		}
		if isProtectedFromPreemption(candidate, req.GangName) {
			continue
		}
		if p.violationOfPDBs(candidate) {
			continue
		}
		out = append(out, candidate)
	}
	sortVictimsByPreference(out)
	return out
}

// Check if the pod has been recorded as allowing interrupts
func isPodAlreadyDisrupted(pod *corev1.Pod, pdb *policy.PodDisruptionBudget) bool {
	if pod == nil || pdb == nil || pdb.Status.DisruptedPods == nil {
		return false
	}
	_, exists := pdb.Status.DisruptedPods[pod.Name]
	return exists
}

// violationOfPDBs Check if the pod violates PDBs constraints
func (p *vgpuPreempt) violationOfPDBs(pod *corev1.Pod) bool {
	if p.pdbLister == nil {
		return false
	}
	budgets, err := p.pdbLister.GetPodPodDisruptionBudgets(pod)
	if err != nil {
		klog.V(4).ErrorS(err, "Failed to list PDBs; assuming no PDB violation", "pod", klog.KObj(pod))
		return false
	}

	for _, pdb := range budgets {
		if pdb == nil || !pdb.DeletionTimestamp.IsZero() {
			continue
		}
		if isPodAlreadyDisrupted(pod, pdb) {
			klog.V(5).InfoS("Pod already disrupted, no PDB violation",
				"pod", klog.KObj(pod), "pdb", klog.KObj(pdb))
			continue
		}
		if pdb.Status.DisruptionsAllowed <= 0 {
			klog.V(4).InfoS("Preempt: pod matches PDB with zero disruptions allowed",
				"pod", klog.KObj(pod), "pdb", klog.KObj(pdb))
			return true
		}
	}
	return false
}

// passthrough returns the input victim map unchanged. Used when the pod is
// out of scope (nil / non-vGPU) or when we hit an error: the scheduler will
// proceed with in-tree's original choice rather than aborting preemption.
//
// ExtenderPreemptionResult only carries the MetaVictims form, so when the
// caller used NodeNameToVictims (NodeCacheCapable=false), we down-convert.
// Otherwise the scheduler would receive an empty result and cancel a valid
// in-tree preemption.
func passthrough(args extenderv1.ExtenderPreemptionArgs) *extenderv1.ExtenderPreemptionResult {
	out := &extenderv1.ExtenderPreemptionResult{
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{},
	}
	if len(args.NodeNameToMetaVictims) > 0 {
		for k, v := range args.NodeNameToMetaVictims {
			out.NodeNameToMetaVictims[k] = v
		}
		return out
	}
	for nodeName, v := range args.NodeNameToVictims {
		if v == nil {
			continue
		}
		meta := &extenderv1.MetaVictims{
			Pods:             make([]*extenderv1.MetaPod, 0, len(v.Pods)),
			NumPDBViolations: v.NumPDBViolations,
		}
		for _, pod := range v.Pods {
			if pod == nil {
				continue
			}
			meta.Pods = append(meta.Pods, &extenderv1.MetaPod{UID: string(pod.UID)})
		}
		out.NodeNameToMetaVictims[nodeName] = meta
	}
	return out
}

// isProtectedFromPreemption returns true when the pod must NOT be evicted.
//
// We intentionally do NOT consult pod.Spec.PreemptionPolicy here. In
// kube-scheduler 1.32 that field gates whether a pod can INITIATE preemption
// (via PodEligibleToPreemptOthers); it has zero effect on whether the pod
// can be CHOSEN as a victim. SelectVictimsOnNode reads only priority and PDB
// status. Mirroring K8s semantics, we apply the same: priority is the
// authoritative "don't preempt me" signal. If we add a vGPU-specific opt-out
// later it should be a separate annotation (e.g. vgpu-manager.io/no-preempt).
//
// Protection categories (more conservative than upstream default preemption,
// which only filters by priority):
//
//  1. Already terminating (DeletionTimestamp set): eviction is moot.
//  2. Already terminated (Phase Succeeded/Failed): containers are gone;
//     scheduler considers these as still occupying resources only because
//     pod objects haven't been garbage-collected. Evicting them surfaces
//     as confusing operator noise without freeing anything real.
//  3. Critical pods (kubelettypes.IsCriticalPod): covers
//     - Static pods (kubernetes.io/config.source != "api"),
//     - Mirror pods (kubernetes.io/config.mirror annotation present) —
//     kubelet immediately recreates them from local files after
//     deletion, so eviction is a no-op at best and noise at worst,
//     - system-cluster-critical / system-node-critical priority (≥ 2B)
//     which priority comparison already excludes in the common case,
//     but adds defense for the unusual "critical preemptor" scenario.
//     Reuses the same helper used by pkg/client/eviction.go and the
//     reschedule controller for project-wide consistency.
//  4. DaemonSet-owned pods: the DaemonSet controller recreates on the same
//     node after eviction. The new pod can't preempt our preemptor back
//     (we just took its slot), so it stays Pending forever — the
//     DaemonSet's desired state is broken AND the preempt cycle accomplished
//     nothing.
//  5. Inside the bind-window fresh allocation: NodeName empty but
//     ShouldCountPodDeviceAllocation==true means Filter just pre-allocated
//     and bind is in progress. findAdditionalVictims further filters to
//     Spec.NodeName == node.Name so pre-bind pods aren't selectable as
//     additional victims anyway; this check still defends against a race
//     where in-tree's proposed victims include a pod that just entered
//     bind state during our processing.
func isProtectedFromPreemption(pod *corev1.Pod, gangName string) bool {
	if pod.DeletionTimestamp != nil {
		return true
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true
	}
	if kubelettypes.IsCriticalPod(pod) {
		return true
	}
	if owner := metav1.GetControllerOf(pod); owner != nil && owner.Kind == "DaemonSet" {
		return true
	}
	if pod.Spec.NodeName == "" && device.ShouldCountPodDeviceAllocation(pod) {
		return true
	}
	// Avoid seizing resources from brother pods
	if gangName != "" {
		if name, _ := util.PodHasGangName(pod); gangName == name {
			return true
		}
	}
	return false
}

// sortVictimsByPreference orders candidates from "prefer to evict" to
// "prefer to keep". All candidates here are bound to the node (findAdditionalVictims
// filters by Spec.NodeName), so a "prefer-stuck-unbound-first" tiebreak that
// earlier drafts of this code carried would have been dead — pre-bind pods
// can't reach this list. Remaining tiebreakers, in order:
//  1. Lower priority first — standard preemption semantic.
//  2. Newer creation time first — minimize the amount of in-flight work lost.
func sortVictimsByPreference(pods []*corev1.Pod) {
	sort.SliceStable(pods, func(i, j int) bool {
		a, b := pods[i], pods[j]
		// Non gang members are preferred for selection.
		_, aGang := util.PodHasGangName(a)
		_, bGang := util.PodHasGangName(b)
		if aGang != bGang {
			return !aGang
		}
		ap, bp := corev1helpers.PodPriority(a), corev1helpers.PodPriority(b)
		if ap != bp {
			return ap < bp
		}
		return a.CreationTimestamp.After(b.CreationTimestamp.Time)
	})
}
