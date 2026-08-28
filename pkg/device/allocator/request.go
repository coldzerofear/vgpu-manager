/*
Copyright 2026 coldzerofear

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

package allocator

import (
	"slices"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AllocationRequest captures everything the allocator needs to decide a
// pod's GPU placement, parsed once from the pod's containers and
// annotations. Without this abstraction the allocator would re-read
// scattered annotations across allocateOne / allocateByTopologyMode /
// filterDevices — each container iteration paid for another
// util.HasAnnotation lookup and the filter / preempt / allocator paths
// were free to disagree on what the pod asked for.
//
// Centralising the parse here gives:
//
//   - One source of truth for "what did this pod ask for". The filter
//     uses req for node ranking; the allocator (per node) uses the same
//     req for per-container device selection. Both see the same
//     normalised NodePolicy / DevicePolicy / Topology / Profile.
//   - A predictable hot path: per-container loops just read fields off
//     req instead of re-parsing strings.
type AllocationRequest struct {
	// Pod is the input pod. Kept on the request because allocator
	// internals still need it for annotation writes (PodVGPUPreAllocAnnotation),
	// type/UUID filter checks against pod.Annotations, and event recording.
	Pod *corev1.Pod

	ControllerOwner *metav1.OwnerReference

	// Containers is the per-container vGPU need list, in declaration order
	// from pod.Spec.Containers, filtered to vGPU-requesting containers
	// only. Index i here does NOT correspond to index i in pod.Spec.Containers
	// when non-vGPU containers are interleaved.
	Containers []ContainerNeed

	// Total is the pod-wide PEAK demand across its lifecycle:
	// sidecarAgg + max(regularAgg, initMaxAgg) per dimension, where each
	// aggregate is Σ/max of (per-container Number, Number × per-vGPU
	// cores/memory). Init and app containers never run concurrently, so this
	// is the K8s-style effective request (not a naive sum across phases); for
	// a pod without init containers it collapses to the historical plain sum.
	//
	// It is filled TWICE. BuildAllocationRequest fills it from RAW user values
	// (memory un-scaled, whole-card memory == 0) — this build-time value only
	// feeds the node-independent Profile. Before the deviceFilter capacity
	// gate reads it, ResetStatistics(nodeInfo) RECOMPUTES it PER NODE, applying
	// that node's MemoryFactor and expanding whole-card memory to the card
	// capacity on same-capacity nodes. So at gate time (req.Total vs
	// GetAvailable*) it is node-aware and does not over-estimate, hence never
	// false-rejects; it stays a NECESSARY condition only because a node passing
	// it may still fail on per-card fragmentation (the allocator re-verifies
	// exactly). On heterogeneous nodes whole-card memory stays 0 — a looser but
	// still non-false-rejecting bound.
	//
	// Total.Name is unset (no container owns the aggregate) and
	// ContainerNeed consumers should not read it.
	Total ContainerNeed

	// Max is the per-field MAXIMUM across Containers (the largest single
	// container's Number, Cores, Memory — each field maxed independently).
	// Used by the deviceFilter per-single-device structural gate (req.Max
	// vs GetSchedulableDeviceCount / GetMaxDevice*) and by the topology
	// fitness comparator (Max.Number = minimum link/NUMA group size the
	// node must host). Like Total, Max is recomputed per node by
	// ResetStatistics (scaled memory, whole-card → card capacity).
	//
	// Max.Name is unset (no container owns the aggregate) and
	// ContainerNeed consumers should not read it.
	Max ContainerNeed

	// NodePolicy / DevicePolicy are the two binpack/spread choices the
	// user expressed via annotations. NonePolicy means "use the default
	// ordering"; the dispatch in deviceFilter / allocateOne reads these
	// directly so the strings.ToLower normalisation and unknown-policy
	// event happen once at parse time, not on every comparator call.
	NodePolicy   util.SchedulerPolicy
	DevicePolicy util.SchedulerPolicy

	// Topology + TopologyStrict are the parsed topology mode. Topology
	// holds the BASE mode (link / numa / none) regardless of whether the
	// user wrote the -strict suffix; TopologyStrict captures the suffix
	// separately so both ranking (which doesn't care) and allocation
	// fallback (which does) can read what they need without re-parsing.
	Topology       util.TopologyMode
	TopologyStrict bool

	// GangName is the gang/pod-group identity this pod belongs to, parsed once
	// via util.PodGangKey (which understands coscheduling / Volcano /
	// Koordinator / native dialects). It is NAMESPACE-QUALIFIED
	// ("<namespace>/<name>") because a PodGroup is a namespaced object, so the
	// bare name does not identify a gang cluster-wide -- see PodGangKey. Every
	// value it is compared against (the IndexerKeyPodGangName index, the pods
	// GangAnchorComponent walks, preemption's brother-pod check) must be
	// produced by the same helper.
	//
	// Empty for non-gang pods. Node-independent (same for every candidate node),
	// so it lives on the shared request rather than per-node context. Used by
	// cross-pod NVLink allocation to resolve the component a gang's sibling pods
	// already occupy on a node; non-gang pods (empty value) never enter the
	// anchor path, so their behaviour is unchanged.
	GangName string

	// CrossPodTopology opts this pod into cross-pod topology affinity (parsed
	// from CrossPodTopologyAnnotation). When true AND the pod is in a gang AND
	// topology mode is link, the allocator keeps the pod's GPUs in the same
	// NVLink component as same-node gang siblings, and aligns to the same
	// component ordinal as cross-node siblings (rail alignment). Replaces the
	// former cluster-wide feature gate with a per-pod, webhook-defaultable switch.
	// False (default) = unchanged single-pod behaviour.
	CrossPodTopology bool

	// GangDomainKey is the cross-node-STABLE sub-domain SIGNATURE that an
	// already-placed gang sibling occupies, computed ONCE by the filter by
	// resolving the sibling's UUIDs on the sibling's OWN NodeInfo (so it does not
	// depend on the possibly-stale Device.Index in the annotation). It is a single
	// node-independent string (the gang's chosen rail-set, or "ord:N" fallback);
	// each candidate node maps it back to its OWN component via
	// NodeInfo.ComponentByDomain — that is where the per-node specialization
	// happens. "" (default) = no cross-node sibling resolved yet (first pod,
	// sibling node not a candidate, or cross-pod off).
	GangDomainKey string

	// GangRailKey is the FINER cross-node alignment key: the sorted set of
	// per-GPU rail keys an already-placed gang sibling occupies, resolved on the
	// sibling's OWN node (rail map when published, device index otherwise).
	//
	// It exists because GangDomainKey cannot express single-card alignment: on a
	// fully connected node every GPU is in one NVLink component, so the
	// component signature is identical on every node. A GPU's own rail is what
	// distinguishes "GPU 3 on node A" from "GPU 5 on node B" — and on a
	// rail-optimized fabric that distinction is the difference between one leaf
	// hop and a trip through the spine.
	//
	// Applied as a PREFERENCE that degrades to GangDomainKey, never as a hard
	// filter, so an over-constrained alignment cannot make a pod unschedulable.
	GangRailKey string

	// topoOutcome accumulates what the topology decisions on THIS node actually
	// achieved, so the filter can report the pod's real outcome once it knows
	// which node won. It is per-node because the filter hands each candidate its
	// own request snapshot (GetSnapshot), and per-pod-worst across containers:
	// a pod whose second container had to drop to cross-NUMA did not get what it
	// asked for, however well its first container fared.
	topoOutcome TopologyOutcome

	// Profile is the pod's request-weighted scoring profile. Captured
	// here so the filter and the allocator score with identical weights
	// for the same pod — see profile.go for the rationale.
	Profile RequestProfile

	// Check if the pod requires additional verification of the device's uuid or type
	CheckDeviceUuid bool
	CheckDeviceType bool

	// HasSequentialInit Refers to a pod having at least one initialization container that is not a sidecar container
	HasSequentialInit bool
}

// ContainerNeed is one container's vGPU-resource request, copied verbatim
// from the container's resources.limits. The values here are USER-TYPED;
// the memoryFactor multiplication and "if cores==0 && mem==0 promote to
// HundredCore" / "if mem==0 use whole card" implicit-fill rules still
// happen at allocation time (allocateOne), not at parse time — keeping
// the raw values lets the per-container allocator path apply the rules
// against the right node's MemoryFactor.
type ContainerNeed struct {
	Name string
	// Kind is init or app; Restartable marks a sidecar (restartPolicy: Always)
	// init container. Together they drive the allocator's lifecycle-aware
	// placement — sequential (non-restartable) init containers run before and
	// never overlap the app phase, so they reuse the app phase's GPUs, while
	// sidecars run concurrently with the app containers. Empty/false on the
	// aggregate Total/Max (no container owns those).
	Kind        util.ContainerKind
	Restartable bool
	Number      int
	Cores       int64
	Memory      int64
}

// TopologyOutcome is what a topology-requesting placement actually achieved on
// one node. Zero value means no topology decision was made (the pod requested
// none, or never reached the topology branch).
type TopologyOutcome struct {
	// Result is a metrics.Result* value: the connectivity received.
	Result string
	// Alignment is a metrics.Align* value, empty when the pod did not opt into
	// cross-pod topology.
	Alignment string
}

// resultSeverity ranks outcomes worst-first so recordTopologyOutcome can keep
// the worst across a pod's containers. Unknown values rank worst, which is the
// safe direction: an unclassified outcome should never be reported as success.
func resultSeverity(result string) int {
	switch result {
	case metrics.ResultNVLink:
		return 0
	case metrics.ResultSwitch:
		return 1
	case metrics.ResultNUMA:
		return 2
	case metrics.ResultAny:
		return 3
	case metrics.ResultCrossNUMA, metrics.ResultSpanned:
		return 4
	case metrics.ResultNone:
		return 5
	default:
		return 6
	}
}

// recordTopologyOutcome keeps the WORST result seen across this pod's
// containers on this node, and the FIRST alignment (all containers of a pod
// share one gang identity, so they cannot disagree).
func (req *AllocationRequest) recordTopologyOutcome(result, alignment string) {
	if req.topoOutcome.Result == "" || resultSeverity(result) > resultSeverity(req.topoOutcome.Result) {
		req.topoOutcome.Result = result
	}
	if req.topoOutcome.Alignment == "" {
		req.topoOutcome.Alignment = alignment
	}
}

// TopologyOutcome reports what the topology decisions achieved during the LAST
// Allocate call on this request. Allocate clears it on entry, so it always
// describes exactly one node — the filter reuses one request across every
// candidate. Only meaningful after a SUCCESSFUL Allocate; a simulation records
// nothing at all.
func (req *AllocationRequest) TopologyOutcome() TopologyOutcome {
	return req.topoOutcome
}

func (req *AllocationRequest) GetSnapshot() *AllocationRequest {
	if req == nil {
		return nil
	}
	allocationRequest := *req
	allocationRequest.Containers = slices.Clone(req.Containers)
	return &allocationRequest
}

// ResetStatistics Reset the statistics of max and total based on node device information
func (req *AllocationRequest) ResetStatistics(nodeInfo *device.NodeInfo) *AllocationRequest {
	if req == nil || nodeInfo == nil {
		return req
	}
	req.Max.Number, req.Max.Cores, req.Max.Memory = 0, 0, 0

	// Aggregate demand bucketed by lifecycle group so Total reflects the
	// pod's PEAK concurrent demand (not a naive sum across non-overlapping
	// phases). Concurrent group = regular app + sidecars (sum); sequential
	// init containers each take the per-dimension max. cores/memory are
	// per-vGPU, so aggregates multiply by Number. Mirrors device.ReducePodFootprint
	// at the node-aggregate level.
	var sidecarAgg, regularAgg, initMaxAgg ContainerNeed
	for i := range req.Containers {
		need := &req.Containers[i]

		number := int64(need.Number)
		// cores/memory are PER-VGPU (each of need.Number vGPUs lands on a
		// distinct card and consumes this much). Max tracks the single largest
		// device requirement across ALL containers (init included), so it does
		// NOT multiply by Number; the per-group aggregates below do.
		cores, memory := resolveContainerNeeds(*need, nodeInfo.MemoryFactor,
			nodeInfo.HasSameCapacity(), nodeInfo.GetMaxDeviceMemory())

		req.Max.Number = max(req.Max.Number, need.Number)
		req.Max.Cores = max(req.Max.Cores, cores)
		req.Max.Memory = max(req.Max.Memory, memory)

		aggCores, aggMemory := cores*number, memory*number
		switch {
		case need.Kind == util.ContainerKindInit && !need.Restartable:
			// Sequential init: runs alone, take the per-dimension max.
			initMaxAgg.Number = max(initMaxAgg.Number, need.Number)
			initMaxAgg.Cores = max(initMaxAgg.Cores, aggCores)
			initMaxAgg.Memory = max(initMaxAgg.Memory, aggMemory)
		case need.Restartable:
			// Sidecar: runs throughout, sum into the concurrent group.
			sidecarAgg.Number += need.Number
			sidecarAgg.Cores += aggCores
			sidecarAgg.Memory += aggMemory
		default:
			// Regular app container: sum into the concurrent group.
			regularAgg.Number += need.Number
			regularAgg.Cores += aggCores
			regularAgg.Memory += aggMemory
		}
	}

	// Effective peak demand: sidecars run for the whole pod life (constant
	// addend present in both the app phase and every sequential-init phase);
	// the variable part is whichever peaks higher — the app phase (regularAgg)
	// or the heaviest single sequential init phase (initMaxAgg). For a pod
	// without init containers this collapses to the historical plain sum.
	req.Total.Number = sidecarAgg.Number + max(regularAgg.Number, initMaxAgg.Number)
	req.Total.Cores = sidecarAgg.Cores + max(regularAgg.Cores, initMaxAgg.Cores)
	req.Total.Memory = sidecarAgg.Memory + max(regularAgg.Memory, initMaxAgg.Memory)

	return req
}

// BuildAllocationRequest parses one pod into an AllocationRequest, doing
// every annotation lookup the allocator pipeline needs in one pass. The
// node is intentionally NOT a parameter — profile weights are node-
// independent (see profile.go) and the per-container resource resolution
// against memoryFactor stays inside allocateOne where the relevant node
// is unambiguous.
func BuildAllocationRequest(pod *corev1.Pod) *AllocationRequest {
	req := &AllocationRequest{
		Pod:             pod,
		ControllerOwner: metav1.GetControllerOf(pod),
		// GangDomainKey defaults to "" (no cross-node sibling resolved yet).
	}

	// Aggregate demand bucketed by lifecycle group so Total reflects the
	// pod's PEAK concurrent demand (not a naive sum across non-overlapping
	// phases). Concurrent group = regular app + sidecars (sum); sequential
	// init containers each take the per-dimension max. cores/memory are
	// per-vGPU, so aggregates multiply by Number. Mirrors device.ReducePodFootprint
	// at the node-aggregate level.
	var sidecarAgg, regularAgg, initMaxAgg ContainerNeed

	addContainer := func(c *corev1.Container, kind util.ContainerKind, restartable bool) {
		number := util.GetResourceOfContainer(c, util.VGPUNumberResourceName)
		if number <= 0 {
			return
		}
		need := ContainerNeed{
			Name:        c.Name,
			Kind:        kind,
			Restartable: restartable,
			Number:      int(number),
			Cores:       util.GetResourceOfContainer(c, util.VGPUCoreResourceName),
			Memory:      util.GetResourceOfContainer(c, util.VGPUMemoryResourceName),
		}
		req.Containers = append(req.Containers, need)

		// cores/memory are PER-VGPU (each of need.Number vGPUs lands on a
		// distinct card and consumes this much). Max tracks the single largest
		// device requirement across ALL containers (init included), so it does
		// NOT multiply by Number; the per-group aggregates below do.
		cores, memory := resolveContainerNeeds(need, 0, false, 0)
		req.Max.Number = max(req.Max.Number, need.Number)
		req.Max.Cores = max(req.Max.Cores, cores)
		req.Max.Memory = max(req.Max.Memory, memory)

		aggCores, aggMemory := cores*number, memory*number
		switch {
		case kind == util.ContainerKindInit && !restartable:
			// Sequential init: runs alone, take the per-dimension max.
			initMaxAgg.Number = max(initMaxAgg.Number, need.Number)
			initMaxAgg.Cores = max(initMaxAgg.Cores, aggCores)
			initMaxAgg.Memory = max(initMaxAgg.Memory, aggMemory)
			req.HasSequentialInit = true
		case restartable:
			// Sidecar: runs throughout, sum into the concurrent group.
			sidecarAgg.Number += need.Number
			sidecarAgg.Cores += aggCores
			sidecarAgg.Memory += aggMemory
		default:
			// Regular app container: sum into the concurrent group.
			regularAgg.Number += need.Number
			regularAgg.Cores += aggCores
			regularAgg.Memory += aggMemory
		}
	}

	// init containers first (matches kubelet's Allocate call order and the
	// device-plugin PreAlloc cursor), then regular app containers.
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		addContainer(c, util.ContainerKindInit, util.IsRestartableInitContainer(c))
	}
	for i := range pod.Spec.Containers {
		addContainer(&pod.Spec.Containers[i], util.ContainerKindApp, false)
	}

	// Effective peak demand: sidecars run for the whole pod life (constant
	// addend present in both the app phase and every sequential-init phase);
	// the variable part is whichever peaks higher — the app phase (regularAgg)
	// or the heaviest single sequential init phase (initMaxAgg). For a pod
	// without init containers this collapses to the historical plain sum.
	req.Total.Number = sidecarAgg.Number + max(regularAgg.Number, initMaxAgg.Number)
	req.Total.Cores = sidecarAgg.Cores + max(regularAgg.Cores, initMaxAgg.Cores)
	req.Total.Memory = sidecarAgg.Memory + max(regularAgg.Memory, initMaxAgg.Memory)

	if len(req.Containers) > 0 {
		req.NodePolicy = parseSchedulerPolicy(pod, util.NodeSchedulerPolicyAnnotation)
		req.DevicePolicy = parseSchedulerPolicy(pod, util.DeviceSchedulerPolicyAnnotation)
		req.Topology, req.TopologyStrict = ParsePodTopologyMode(pod)
		req.GangName, _ = util.PodGangKey(pod)
		if v, ok := util.HasAnnotation(pod, util.CrossPodTopologyAnnotation); ok {
			req.CrossPodTopology = strings.EqualFold(v, "true")
		}

		_, ok1 := util.HasAnnotation(pod, util.PodIncludeGPUUUIDAnnotation)
		_, ok2 := util.HasAnnotation(pod, util.PodExcludeGPUUUIDAnnotation)
		req.CheckDeviceUuid = ok1 || ok2

		_, ok1 = util.HasAnnotation(pod, util.PodIncludeGpuTypeAnnotation)
		_, ok2 = util.HasAnnotation(pod, util.PodExcludeGpuTypeAnnotation)
		req.CheckDeviceType = ok1 || ok2

		//req.Profile = UniformProfile
		req.Profile = NewRequestProfile(req)
	}

	return req
}

// parseSchedulerPolicy reads a SchedulerPolicy annotation and returns
// both the recognised enum value and the raw lowercased string.
// Unrecognised input (including empty and "none") maps to NonePolicy so
// downstream switches only have to handle the three known cases; the
// raw string is preserved for diagnostic events.
func parseSchedulerPolicy(pod *corev1.Pod, annotation string) util.SchedulerPolicy {
	raw, _ := util.HasAnnotation(pod, annotation)
	lower := strings.ToLower(raw)
	switch util.SchedulerPolicy(lower) {
	case util.BinpackPolicy:
		return util.BinpackPolicy
	case util.SpreadPolicy:
		return util.SpreadPolicy
	case util.NonePolicy, "":
		return util.NonePolicy
	default:
		return util.SchedulerPolicy(lower)
	}
}

// ParsePodTopologyMode reads DeviceTopologyModeAnnotation and returns the
// BASE mode (numa / link / none) plus a strict flag derived from the
// "-strict" suffix variants. Moved here from allocator.go so the parse
// happens once at BuildAllocationRequest time, alongside the other
// annotation reads.
func ParsePodTopologyMode(pod *corev1.Pod) (mode util.TopologyMode, strict bool) {
	raw, _ := util.HasAnnotation(pod, util.DeviceTopologyModeAnnotation)
	tm := util.TopologyMode(strings.ToLower(raw))
	return tm, tm.IsStrictTopology()
}
