package allocator

import (
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
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

	// Containers is the per-container vGPU need list, in declaration order
	// from pod.Spec.Containers, filtered to vGPU-requesting containers
	// only. Index i here does NOT correspond to index i in pod.Spec.Containers
	// when non-vGPU containers are interleaved.
	Containers []ContainerNeed

	// Total aggregates Number / Cores / Memory across Containers. Carried
	// for completeness — current callers (per-container Allocate loop)
	// don't consult it, but downstream code that needs a pod-wide GPU
	// count (e.g. capacity probes, future pod-level paths) can read it
	// without re-walking pod.Spec.Containers.
	//
	// Total.Name is unset (no container owns the aggregate) and
	// ContainerNeed consumers should not read it.
	Total ContainerNeed

	// Maximum aggregation Number / Cores / Memory for a single container.
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

	// Profile is the pod's request-weighted scoring profile. Captured
	// here so the filter and the allocator score with identical weights
	// for the same pod — see profile.go for the rationale.
	Profile RequestProfile

	// rawDevicePolicy is the unrecognised device policy string (if any),
	// preserved so allocateOne can emit the "Unsupported device scheduling
	// policy" event with the same wording as the pre-refactor code. Empty
	// when DevicePolicy was recognised (or unset) cleanly.
	rawDevicePolicy string
	// rawNodePolicy is the analogue for the node policy.
	rawNodePolicy string
}

// ContainerNeed is one container's vGPU-resource request, copied verbatim
// from the container's resources.limits. The values here are USER-TYPED;
// the memoryFactor multiplication and "if cores==0 && mem==0 promote to
// HundredCore" / "if mem==0 use whole card" implicit-fill rules still
// happen at allocation time (allocateOne), not at parse time — keeping
// the raw values lets the per-container allocator path apply the rules
// against the right node's MemoryFactor.
type ContainerNeed struct {
	Name   string
	Number int
	Cores  int64
	Memory int64
}

// BuildAllocationRequest parses one pod into an AllocationRequest, doing
// every annotation lookup the allocator pipeline needs in one pass. The
// node is intentionally NOT a parameter — profile weights are node-
// independent (see profile.go) and the per-container resource resolution
// against memoryFactor stays inside allocateOne where the relevant node
// is unambiguous.
func BuildAllocationRequest(pod *corev1.Pod) *AllocationRequest {
	req := &AllocationRequest{Pod: pod}

	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		number := util.GetResourceOfContainer(c, util.VGPUNumberResourceName)
		if number <= 0 {
			continue
		}
		need := ContainerNeed{
			Name:   c.Name,
			Number: int(number),
			Cores:  util.GetResourceOfContainer(c, util.VGPUCoreResourceName),
			Memory: util.GetResourceOfContainer(c, util.VGPUMemoryResourceName),
		}
		req.Containers = append(req.Containers, need)

		cores, memory := resolveContainerNeeds(need, 0)
		req.Total.Number += need.Number
		req.Total.Cores += cores
		req.Total.Memory += memory

		req.Max.Number = max(req.Max.Number, need.Number)
		req.Max.Cores = max(req.Max.Cores, cores)
		req.Max.Memory = max(req.Max.Memory, memory)
	}

	if len(req.Containers) > 0 {
		req.NodePolicy, req.rawNodePolicy = parseSchedulerPolicy(pod, util.NodeSchedulerPolicyAnnotation)
		req.DevicePolicy, req.rawDevicePolicy = parseSchedulerPolicy(pod, util.DeviceSchedulerPolicyAnnotation)
		req.Topology, req.TopologyStrict = parsePodTopologyMode(pod)
		req.Profile = NewRequestProfile(pod)
	}

	return req
}

// RawNodePolicy returns the user-typed node-scheduler-policy string. Used
// by the filter to emit the "Unsupported node scheduling policy" event
// with the unrecognised value verbatim — the parsed NodePolicy collapses
// unknown values to NonePolicy, which would lose the original string.
func (r *AllocationRequest) RawNodePolicy() string {
	return r.rawNodePolicy
}

// RawDevicePolicy is the device-scheduler-policy analogue of RawNodePolicy.
func (r *AllocationRequest) RawDevicePolicy() string {
	return r.rawDevicePolicy
}

// parseSchedulerPolicy reads a SchedulerPolicy annotation and returns
// both the recognised enum value and the raw lowercased string.
// Unrecognised input (including empty and "none") maps to NonePolicy so
// downstream switches only have to handle the three known cases; the
// raw string is preserved for diagnostic events.
func parseSchedulerPolicy(pod *corev1.Pod, annotation string) (util.SchedulerPolicy, string) {
	raw, _ := util.HasAnnotation(pod, annotation)
	lower := strings.ToLower(raw)
	switch util.SchedulerPolicy(lower) {
	case util.BinpackPolicy:
		return util.BinpackPolicy, lower
	case util.SpreadPolicy:
		return util.SpreadPolicy, lower
	default:
		return util.NonePolicy, lower
	}
}

// parsePodTopologyMode reads DeviceTopologyModeAnnotation and returns the
// BASE mode (numa / link / none) plus a strict flag derived from the
// "-strict" suffix variants. Moved here from allocator.go so the parse
// happens once at BuildAllocationRequest time, alongside the other
// annotation reads.
func parsePodTopologyMode(pod *corev1.Pod) (mode util.TopologyMode, strict bool) {
	raw, _ := util.HasAnnotation(pod, util.DeviceTopologyModeAnnotation)
	tm := util.TopologyMode(strings.ToLower(raw))
	return tm.BaseTopology(), tm.IsStrictTopology()
}
