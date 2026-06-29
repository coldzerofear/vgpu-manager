package allocator

import (
	"sort"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/klog/v2"
)

// LessFunc represents function to compare two DeviceInfo or NodeInfo
type LessFunc[T any] func(p1, p2 T) bool

var (
	// ByDeviceScoreAsc Compare the device scores of two devices in ascending order
	ByDeviceScoreAsc = func(p1, p2 *device.Device) bool {
		return p1.Score() < p2.Score()
	}
	// ByDeviceScoreDes Compare the device scores of two devices in descending order
	ByDeviceScoreDes = func(p1, p2 *device.Device) bool {
		return p1.Score() > p2.Score()
	}
	// ByDeviceIdAsc Compare the device id of two devices in ascending order
	ByDeviceIdAsc = func(p1, p2 *device.Device) bool {
		return p1.GetID() < p2.GetID()
	}
	ByNuma = func(p1, p2 *device.Device) bool {
		switch {
		case p1.GetNUMA() < 0 && p2.GetNUMA() < 0:
			return false
		case p1.GetNUMA() < 0:
			return false
		case p2.GetNUMA() < 0:
			return true
		default:
			return p1.GetNUMA() < p2.GetNUMA()
		}
	}
	ByNodeNameAsc = func(p1, p2 *device.NodeInfo) bool {
		return p1.GetName() < p2.GetName()
	}
)

// ByNodeGPUTopologyFitness ranks nodes by their actual ability to satisfy a
// link-topology group of needNumber GPUs, preferring the best NCCL performance.
// It delegates to NodeInfo.LinkTopologyFitness, whose tiers are (high→low):
// NVLink fabric (5) > PCIe switch fabric (4) > one NUMA (3) > cross-CPU
// reachable (2) > has-topology-can't-fit (1) > no-topology (0).
//
// On a homogeneous NVSwitch cluster every candidate is tier 5 (== the old
// uniform "fits" tier), so the downstream binpack/spread order is unchanged; the
// finer tiers only bite on mixed/island clusters or when N exceeds a single
// NVLink island, pulling tighter-coupled placements to the front.
func ByNodeGPUTopologyFitness(needNumber int) LessFunc[*device.NodeInfo] {
	return func(p1, p2 *device.NodeInfo) bool {
		return gpuTopologyFitness(p1, needNumber) > gpuTopologyFitness(p2, needNumber)
	}
}

func gpuTopologyFitness(n *device.NodeInfo, needNumber int) int {
	return n.LinkTopologyFitness(needNumber)
}

// ByNodeNUMATopologyFitness is the NUMA-aware counterpart to
// ByNodeGPUTopologyFitness: it ranks higher the nodes that have a single
// NUMA domain large enough to host the request, avoiding nodes that publish
// NUMA info but force cross-NUMA fallback.
func ByNodeNUMATopologyFitness(needNumber int) LessFunc[*device.NodeInfo] {
	return func(p1, p2 *device.NodeInfo) bool {
		return numaTopologyFitness(p1, needNumber) > numaTopologyFitness(p2, needNumber)
	}
}

func numaTopologyFitness(n *device.NodeInfo, needNumber int) int {
	switch {
	case !n.HasNUMATopology():
		return 0
	case n.MaxNUMAGroupSize() >= needNumber:
		return 2
	default:
		return 1
	}
}

type sortPriority[T any] struct {
	data []T
	less []LessFunc[T]
}

func NewSortPriority[T any](less ...LessFunc[T]) *sortPriority[T] {
	return &sortPriority[T]{
		less: less,
	}
}

func (sp *sortPriority[T]) Sort(data []T) {
	sp.data = data
	sort.Sort(sp)
}

func (sp *sortPriority[T]) Len() int {
	return len(sp.data)
}

func (sp *sortPriority[T]) Swap(i, j int) {
	sp.data[i], sp.data[j] = sp.data[j], sp.data[i]
}

func (sp *sortPriority[T]) Less(i, j int) bool {
	var k int

	for k = 0; k < len(sp.less)-1; k++ {
		less := sp.less[k]
		switch {
		case less(sp.data[i], sp.data[j]):
			return true
		case less(sp.data[j], sp.data[i]):
			return false
		}
	}

	return sp.less[k](sp.data[i], sp.data[j])
}

// WeightedNodeLess returns a comparator that ranks nodes by their
// request-weighted score under the given policy mode. Binpack ranks
// higher-utilisation nodes first; Spread ranks lower-utilisation nodes
// first (Score encodes the direction — higher score is always more
// preferred regardless of mode).
//
// The closure captures a per-name cache so the O(n log n) comparisons
// in one sort pass each evaluate Score at most once per node — keeps
// the total work O(n) and the cache lives only for the lifetime of the
// returned LessFunc (confined to a single filter goroutine, so no
// mutex is needed).
func WeightedNodeLess(profile RequestProfile, mode util.SchedulerPolicy) LessFunc[*device.NodeInfo] {
	cache := make(map[string]float64)
	return func(p1, p2 *device.NodeInfo) bool {
		return cachedNodeScore(cache, p1, profile, mode) >
			cachedNodeScore(cache, p2, profile, mode)
	}
}

func cachedNodeScore(
	cache map[string]float64, info *device.NodeInfo,
	profile RequestProfile, mode util.SchedulerPolicy,
) float64 {
	name := info.GetName()
	if s, ok := cache[name]; ok {
		return s
	}
	s := Score(NodeUtilization(info), profile, mode) * util.HundredCore
	klog.V(5).Infof("Policy %s node <%s> resource score is <%.2f>", mode, info.GetName(), s)
	cache[name] = s
	return s
}

// ApplyTopologyMode prepends a topology-fitness comparator at the highest
// priority of the sort chain when the pod requested a topology mode. The
// fitness comparator returns true when the candidate node can ACTUALLY host
// the requested topology group (max link-component or max NUMA group ≥
// PodTopologyNeedNumber(req.Pod)) — strictly stronger than a bare "just
// has topology metadata" check.
//
// req carries both inputs the fitness comparator needs: req.Topology
// selects link- vs NUMA-aware ranking, and req.Pod is consulted via
// PodTopologyNeedNumber to size the required group. For none-topology
// requests or unrecognised modes, the input slice is returned unchanged.
//
// Both strict and non-strict topology variants get the same prepended
// comparator — strictness only changes ALLOCATION fallback behaviour
// (handled inside allocateByTopologyMode), not node ranking.
func ApplyTopologyMode(req AllocationRequest, less ...LessFunc[*device.NodeInfo]) []LessFunc[*device.NodeInfo] {
	var fitness LessFunc[*device.NodeInfo]
	switch req.Topology.BaseTopology() {
	case util.LinkTopology:
		fitness = ByNodeGPUTopologyFitness(req.Max.Number)
	case util.NUMATopology:
		fitness = ByNodeNUMATopologyFitness(req.Max.Number)
	default:
		return less
	}
	return append([]LessFunc[*device.NodeInfo]{fitness}, less...)
}

// NewNodePolicyPriority builds the node-level ranking chain for a pod:
// request-weighted Score under req.NodePolicy first, node name as the
// deterministic tiebreaker, and a topology-fitness comparator prepended
// when req.Topology asks for one.
//
// req.NodePolicy drives the score direction (Binpack ranks high-use
// first; Spread ranks low-use first); req.Profile carries the
// per-dimension weights; req.Topology + req.Pod feed ApplyTopologyMode.
// Unrecognised NodePolicy values were normalised to NonePolicy by
// BuildAllocationRequest, in which case Score returns 0 for every
// candidate and the comparator collapses to "all equal", letting
// ByNodeNameAsc decide.
func NewNodePolicyPriority(req AllocationRequest) *sortPriority[*device.NodeInfo] {
	less := []LessFunc[*device.NodeInfo]{
		WeightedNodeLess(req.Profile, req.NodePolicy),
		ByNodeNameAsc,
	}
	return &sortPriority[*device.NodeInfo]{
		less: ApplyTopologyMode(req, less...),
	}
}

func NewDevicePolicyPriority(req AllocationRequest) *sortPriority[*device.Device] {
	switch req.DevicePolicy {
	case util.BinpackPolicy:
		return NewSortPriority[*device.Device](ByDeviceScoreAsc, ByDeviceIdAsc)
	case util.SpreadPolicy:
		return NewSortPriority[*device.Device](ByDeviceScoreDes, ByDeviceIdAsc)
	default:
		return NewSortPriority[*device.Device](ByNuma, ByDeviceIdAsc)
	}
}
