package allocator

import (
	"sort"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
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
// link-topology group of needNumber GPUs:
//
//	fitness 2 = has topology AND max connected component >= needNumber
//	fitness 1 = has topology BUT max component too small (will fall back)
//	fitness 0 = no topology info
//
// The fitness check is strictly stronger than a bare "has topology?" test:
// a node that publishes link metadata but physically can't host the
// requested group size ranks BELOW a node that would actually allocate
// fine via the non-topology fallback.
func ByNodeGPUTopologyFitness(needNumber int) LessFunc[*device.NodeInfo] {
	return func(p1, p2 *device.NodeInfo) bool {
		return gpuTopologyFitness(p1, needNumber) > gpuTopologyFitness(p2, needNumber)
	}
}

func gpuTopologyFitness(n *device.NodeInfo, needNumber int) int {
	if !n.HasGPUTopology() {
		return 0
	}
	if n.MaxLinkComponentSize() >= needNumber {
		return 2
	}
	return 1
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
	if !n.HasNUMATopology() {
		return 0
	}
	if n.MaxNUMAGroupSize() >= needNumber {
		return 2
	}
	return 1
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
	s := Score(NodeUtilization(info), profile, mode)
	cache[name] = s
	return s
}

// ApplyTopologyMode prepends a topology-fitness comparator at the highest
// priority of the sort chain when the pod requested a topology mode. The
// fitness comparator returns true when the candidate node can ACTUALLY host
// the requested topology group of needNumber GPUs (max component / max NUMA
// group ≥ needNumber), strictly stronger than a bare "just has topology
// metadata" check.
//
// needNumber is the per-pod GPU count whose locality the user wants. For
// none-topology requests, single-GPU requests, or unrecognised modes, the
// input slice is returned unchanged.
//
// Both strict and non-strict topology variants get the same prepended
// comparator — strictness only changes ALLOCATION fallback behaviour
// (handled inside allocateByTopologyMode), not node ranking.
func ApplyTopologyMode(mode util.TopologyMode, needNumber int,
	less []LessFunc[*device.NodeInfo],
) []LessFunc[*device.NodeInfo] {
	var fitness LessFunc[*device.NodeInfo]
	switch mode.BaseTopology() {
	case util.LinkTopology:
		fitness = ByNodeGPUTopologyFitness(needNumber)
	case util.NUMATopology:
		fitness = ByNodeNUMATopologyFitness(needNumber)
	default:
		return less
	}
	return append([]LessFunc[*device.NodeInfo]{fitness}, less...)
}

// NewNodeBinpackPriority builds the node-level binpack ranking chain:
// request-weighted score first, name as deterministic tiebreaker, and a
// topology fitness comparator prepended when a topology mode applies.
func NewNodeBinpackPriority(profile RequestProfile, mode util.TopologyMode, needNumber int) *sortPriority[*device.NodeInfo] {
	return newNodePriority(profile, util.BinpackPolicy, mode, needNumber)
}

// NewNodeSpreadPriority is the spread-policy counterpart of
// NewNodeBinpackPriority — same shape, just inverts the score direction.
func NewNodeSpreadPriority(profile RequestProfile, mode util.TopologyMode, needNumber int) *sortPriority[*device.NodeInfo] {
	return newNodePriority(profile, util.SpreadPolicy, mode, needNumber)
}

func newNodePriority(profile RequestProfile, policy util.SchedulerPolicy,
	mode util.TopologyMode, needNumber int,
) *sortPriority[*device.NodeInfo] {
	less := []LessFunc[*device.NodeInfo]{
		WeightedNodeLess(profile, policy),
		ByNodeNameAsc,
	}
	return &sortPriority[*device.NodeInfo]{
		less: ApplyTopologyMode(mode, needNumber, less),
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
