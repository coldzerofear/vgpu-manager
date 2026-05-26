package allocator

import (
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
)

// AllocationRequest captures everything the allocator needs to decide a
// pod's GPU placement, parsed once from the pod's containers and
// annotations. Before Phase C the allocator re-read these scattered
// across allocateOne / allocateByTopologyMode / filterDevices — each
// container iteration paid for another util.HasAnnotation lookup, and
// pod-level decisions were impossible because the parsing happened mid-
// per-container-loop.
//
// Centralising the parse here gives two things:
//
//   - Per-container hot loops just read fields (req.Topology vs a fresh
//     annotation lookup), so the per-container path's CPU profile drops
//     a little.
//   - Multi-container topology pods become expressible: req.Total has
//     the pod-wide GPU count, and req.Containers preserves the per-
//     container split, so allocatePodLevel can pick one topology domain
//     covering all containers and distributeToContainers slices it back.
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

	// Total aggregates Number / Cores / Memory across Containers. Used by
	// the pod-level dispatch gate (Allocate) to decide between the
	// per-container and pod-level paths, and by selectByTopology to size
	// the single allocation that covers the whole pod.
	//
	// Total.Name is unset (no container owns the aggregate) and ContainerNeed
	// consumers should not read it.
	Total ContainerNeed

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
		if !util.IsVGPURequiredContainer(c) {
			continue
		}
		need := ContainerNeed{
			Name:   c.Name,
			Number: int(util.GetResourceOfContainer(c, util.VGPUNumberResourceName)),
			Cores:  util.GetResourceOfContainer(c, util.VGPUCoreResourceName),
			Memory: util.GetResourceOfContainer(c, util.VGPUMemoryResourceName),
		}
		req.Containers = append(req.Containers, need)
		req.Total.Number += need.Number
		req.Total.Cores += need.Cores
		req.Total.Memory += need.Memory
	}

	req.NodePolicy, req.rawNodePolicy = parseSchedulerPolicy(pod, util.NodeSchedulerPolicyAnnotation)
	req.DevicePolicy, req.rawDevicePolicy = parseSchedulerPolicy(pod, util.DeviceSchedulerPolicyAnnotation)
	req.Topology, req.TopologyStrict = parsePodTopologyMode(pod)
	req.Profile = NewRequestProfile(pod)

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
