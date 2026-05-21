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
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	listerv1 "k8s.io/client-go/listers/core/v1"
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
	recorder    record.EventRecorder
	hasSyncFunc func(ctx context.Context) bool
}

var (
	_           predicate.PreemptPredicate = &vgpuPreempt{}
	podIndexers                            = cache.Indexers{
		IndexerKeyPodMetadataUid: func(obj interface{}) ([]string, error) {
			var indexerValues []string
			if accessor, err := meta.Accessor(obj); err == nil {
				indexerValues = append(indexerValues, string(accessor.GetUID()))
			}
			return indexerValues, nil
		},
	}
)

// New wires the preempt plugin to the same informer-backed pod lister that
// the filter plugin uses, so per-node pod lookups go through the indexer that
// already filters by vGPU resource requests.
func New(factory informers.SharedInformerFactory, recorder record.EventRecorder,
	podLister client.PodLister) (*vgpuPreempt, error) {
	podInformer := factory.Core().V1().Pods().Informer()
	nodeInformer := factory.Core().V1().Nodes().Informer()
	if err := podInformer.AddIndexers(podIndexers); err != nil {
		return nil, err
	}
	nodeLister := listerv1.NewNodeLister(nodeInformer.GetIndexer())
	hasSyncFunc := func(ctx context.Context) bool {
		return cache.WaitForCacheSync(
			ctx.Done(),
			podInformer.HasSynced,
			nodeInformer.HasSynced,
		)
	}
	return &vgpuPreempt{
		nodeLister:  nodeLister,
		podLister:   podLister,
		recorder:    recorder,
		hasSyncFunc: hasSyncFunc,
	}, nil
}

func (p *vgpuPreempt) Name() string {
	return Name
}

func (p *vgpuPreempt) IsReady(ctx context.Context) bool {
	return p.hasSyncFunc(ctx)
}

// Preempt iterates the candidate nodes in-tree proposed and, for each, asks
// our allocator whether the pending pod can fit after the proposed victims
// are removed. If yes, we keep the set (modulo protected pods). If not, we
// search for additional lower-priority victims on that node until the
// allocator accepts; if no such set exists, we drop the node.
func (p *vgpuPreempt) Preempt(ctx context.Context, args extenderv1.ExtenderPreemptionArgs) *extenderv1.ExtenderPreemptionResult {
	klog.V(4).InfoS("PreemptPod", "ExtenderPreemptionArgs", args)
	pod := args.Pod
	if pod == nil {
		klog.V(4).InfoS("Preempt called with nil pod, passing input through")
		return passthrough(args)
	}
	// Non-vGPU pods are out of our scope; let the in-tree decision stand.
	if !util.IsVGPUResourcePod(pod) {
		klog.V(5).InfoS("Preempt: pod is not a vGPU pod, passing input through",
			"pod", klog.KObj(pod))
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

	allVGPUPods, err := p.podLister.ListByIndexValue(filter.IndexerKeyPodRequestVGPU, "true")
	if err != nil {
		klog.ErrorS(err, "PodLister list vGPU pods failed in preempt")
		return passthrough(args)
	}
	mu := sync.Mutex{}
	wg := sync.WaitGroup{}

	keys := maps.Keys(victimsMap)
	maxGoroutines := runtime.NumCPU() * 2
	batchSize := (len(keys) + maxGoroutines - 1) / maxGoroutines
	batches := watcher.BalanceBatches(len(keys), batchSize)
	for _, batch := range batches {
		wg.Add(1)
		go func(victimKeys []string) {
			defer wg.Done()

			for _, nodeName := range victimKeys {
				victims := victimsMap[nodeName]
				if victims == nil {
					continue
				}
				refined, pdbViolations, ok := p.refineForNode(pod, nodeName, victims, allVGPUPods)
				if !ok {
					klog.V(3).InfoS("Preempt: node cannot fit pod even after preemption, dropping",
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
				for _, vp := range refined {
					meta.Pods = append(meta.Pods, &extenderv1.MetaPod{UID: string(vp.UID)})
				}

				mu.Lock()
				result.NodeNameToMetaVictims[nodeName] = meta
				mu.Unlock()
			}
		}(keys[batch.StartIndex : batch.EndIndex+1])
	}
	wg.Wait()
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
func (p *vgpuPreempt) refineForNode(pod *corev1.Pod, nodeName string,
	victims *extenderv1.Victims, allVGPUPods []*corev1.Pod) ([]*corev1.Pod, int64, bool) {

	node, err := p.nodeLister.Get(nodeName)
	if err != nil {
		klog.V(3).ErrorS(err, "Preempt: get node failed", "node", nodeName)
		return nil, 0, false
	}

	// check node fast return
	if err = filter.CheckNode(node, filter.GetMemoryPolicyFunc(pod)); err != nil {
		klog.V(3).ErrorS(err, "Preempt: check node failed", "node", nodeName)
		return nil, 0, false
	}

	// Strip protected pods from in-tree's proposal — never evict them.
	keep := make([]*corev1.Pod, 0, len(victims.Pods))
	excluded := make(map[k8stypes.UID]struct{}, len(victims.Pods))
	for _, v := range victims.Pods {
		if v == nil {
			continue
		}
		if isProtectedFromPreemption(v) {
			klog.V(4).InfoS("Preempt: refusing to evict protected pod proposed by in-tree",
				"pod", klog.KObj(v), "node", nodeName)
			continue
		}
		keep = append(keep, v)
		excluded[v.UID] = struct{}{}
	}
	// Snapshot the post-protection length so pdbViolationsUpperBound can
	// distinguish "carried over from input" from "newly appended below".
	keptFromInput := len(keep)

	// First pass: does the pending pod fit after removing the kept victims?
	if p.canAllocate(pod, node, allVGPUPods, excluded) {
		return keep, pdbViolationsUpperBound(victims.NumPDBViolations, keptFromInput, 0), true
	}

	// Second pass: in-tree under-selected (likely because per-device or
	// annotation constraints invisible to it require more victims). Walk the
	// remaining lower-priority pods on this node and greedily add until fit.
	additional := p.findAdditionalVictims(pod, node, allVGPUPods, excluded)
	for _, cand := range additional {
		excluded[cand.UID] = struct{}{}
		keep = append(keep, cand)
		if p.canAllocate(pod, node, allVGPUPods, excluded) {
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
func (p *vgpuPreempt) canAllocate(pod *corev1.Pod, node *corev1.Node,
	allVGPUPods []*corev1.Pod, excluded map[k8stypes.UID]struct{}) bool {

	uids := maps.Keys(excluded)
	nodeInfo, err := device.NewNodeInfo(node, allVGPUPods, uids...)
	if err != nil {
		klog.V(3).ErrorS(err, "Preempt: NewNodeInfo failed", "node", node.Name)
		return false
	}
	if _, err := allocator.NewAllocator(nodeInfo, nil).Allocate(pod); err != nil {
		klog.V(5).InfoS("Preempt: allocator rejects pod after excluding victims",
			"pod", klog.KObj(pod), "node", node.Name, "reason", err)
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
func (p *vgpuPreempt) findAdditionalVictims(pod *corev1.Pod, node *corev1.Node,
	allVGPUPods []*corev1.Pod, excluded map[k8stypes.UID]struct{}) []*corev1.Pod {

	preemptorPriority := corev1helpers.PodPriority(pod)
	out := make([]*corev1.Pod, 0)
	for _, candidate := range allVGPUPods {
		if candidate.UID == pod.UID {
			continue
		}
		if _, dup := excluded[candidate.UID]; dup {
			continue
		}
		// Must be actually bound to this node — see function doc.
		if candidate.Spec.NodeName != node.Name {
			continue
		}
		if corev1helpers.PodPriority(candidate) >= preemptorPriority {
			continue
		}
		if isProtectedFromPreemption(candidate) {
			continue
		}
		out = append(out, candidate)
	}
	sortVictimsByPreference(out)
	return out
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
func isProtectedFromPreemption(pod *corev1.Pod) bool {
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
		aGang, bGang := util.PodIsGangMember(a), util.PodIsGangMember(b)
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
