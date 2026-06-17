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
	// ByAllocatableMemoryAsc Compare the assignable memory of two devices in ascending order
	ByAllocatableMemoryAsc = func(p1, p2 *device.Device) bool {
		return p1.AllocatableMemory() < p2.AllocatableMemory()
	}
	// ByAllocatableMemoryDes Compare the assignable memory of two devices in descending order
	ByAllocatableMemoryDes = func(p1, p2 *device.Device) bool {
		return p1.AllocatableMemory() > p2.AllocatableMemory()
	}
	// ByAllocatableCoresAsc Compare the assignable cores of two devices in ascending order
	ByAllocatableCoresAsc = func(p1, p2 *device.Device) bool {
		return p1.AllocatableCores() < p2.AllocatableCores()
	}
	// ByAllocatableCoresDes Compare the assignable cores of two devices in descending order
	ByAllocatableCoresDes = func(p1, p2 *device.Device) bool {
		return p1.AllocatableCores() > p2.AllocatableCores()
	}
	// ByDeviceIdAsc Compare the device id of two devices in ascending order
	ByDeviceIdAsc = func(p1, p2 *device.Device) bool {
		return p1.GetID() < p2.GetID()
	}
	ByAllocatableNumberDes = func(p1, p2 *device.Device) bool {
		return p1.AllocatableNumber() > p2.AllocatableNumber()
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
// link-topology group of needNumber GPUs, preferring the best NCCL performance:
//
//	fitness 3 = fits within ONE NVLink fabric (MaxNVLinkComponentSize >= N) — best
//	fitness 2 = fits within a P2P-reachable group but spans NVLink islands
//	            (MaxLinkComponentSize >= N > MaxNVLinkComponentSize) — works, PCIe
//	fitness 1 = has topology BUT can't fit even a P2P group (will fall back)
//	fitness 0 = no topology info
//
// Tiers 0/1/2 preserve the previous ordering (topology-capable nodes rank above
// non-topology ones); tier 3 is the new finer split that pulls fully
// NVLink-connectable nodes to the front. On a homogeneous NVSwitch cluster every
// candidate is tier 3 (== the old "all tier 2"), so the downstream binpack/
// spread order is unchanged; the new distinction only bites on mixed/island
// clusters or when N exceeds a single NVLink island.
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

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
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

func cachedNodeScore(cache map[string]float64, info *device.NodeInfo,
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
func ApplyTopologyMode(req AllocationRequest, less []LessFunc[*device.NodeInfo]) []LessFunc[*device.NodeInfo] {
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
		less: ApplyTopologyMode(req, less),
	}
}

func NewDeviceBinpackPriority() *sortPriority[*device.Device] {
	return &sortPriority[*device.Device]{
		less: []LessFunc[*device.Device]{
			ByAllocatableMemoryAsc,
			ByAllocatableCoresAsc,
			ByDeviceIdAsc,
		},
	}
}

func NewDeviceSpreadPriority() *sortPriority[*device.Device] {
	return &sortPriority[*device.Device]{
		less: []LessFunc[*device.Device]{
			ByAllocatableMemoryDes,
			ByAllocatableNumberDes,
			ByAllocatableCoresDes,
			ByDeviceIdAsc,
		},
	}
}
