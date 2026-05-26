package allocator

import (
	"sort"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"golang.org/x/exp/slices"
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
	// ByNodeGPUTopology Nodes with GPU Link topology information will be
	// ranked first. Coarse "has topology?" comparator kept for backward
	// compatibility; new call sites should prefer ByNodeGPUTopologyFitness
	// which additionally checks whether the topology can actually host the
	// requested group size (max connected component ≥ needNumber).
	ByNodeGPUTopology = func(p1, p2 *device.NodeInfo) bool {
		hasTopo1 := p1.HasGPUTopology()
		hasTopo2 := p2.HasGPUTopology()
		switch {
		case hasTopo1 && !hasTopo2:
			return true // p1 has topology, p2 does not → p1 ranks first
		case !hasTopo1 && hasTopo2:
			return false // p2 has topology, p1 does not → p2 ranks first
		default:
			return false // both are the same: continue to compare in the future
		}
	}
	// ByNodeNUMATopology Nodes with GPU NUMA topology information will be
	// ranked first. See ByNodeGPUTopology comment — prefer the fitness-aware
	// ByNodeNUMATopologyFitness for new call sites.
	ByNodeNUMATopology = func(p1, p2 *device.NodeInfo) bool {
		hasTopo1 := p1.HasNUMATopology()
		hasTopo2 := p2.HasNUMATopology()
		switch {
		case hasTopo1 && !hasTopo2:
			return true // p1 has topology, p2 does not → p1 ranks first
		case !hasTopo1 && hasTopo2:
			return false // p2 has topology, p1 does not → p2 ranks first
		default:
			return false // both are the same: continue to compare in the future
		}
	}
)

// ByNodeGPUTopologyFitness ranks nodes by their actual ability to satisfy a
// link-topology group of needNumber GPUs:
//
//	fitness 2 = has topology AND max connected component >= needNumber
//	fitness 1 = has topology BUT max component too small (will fall back)
//	fitness 0 = no topology info
//
// Without this fitness check the old ByNodeGPUTopology just orders by
// "has-topology vs has-not", so a node that publishes topology info but
// physically can't host the request can still beat a node that would
// actually allocate fine via non-topology fallback.
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

// WeightedBinpackNodeLess returns a comparator that ranks nodes higher
// when their request-weighted USED fraction is higher (the binpack rule).
// The profile is captured once; the per-name cache reuses the computed
// score across the O(n log n) comparisons in a single sort pass, so each
// node's Score is evaluated at most once per priority instantiation.
//
// Why a closure-captured cache: NodeUtilization is cheap but Score with a
// weighted profile is invoked O(n log n) times during sort.Sort; caching
// keeps the cost O(n) without exposing a mutex (each priority instance is
// confined to one filter goroutine).
func WeightedBinpackNodeLess(profile RequestProfile) LessFunc[*device.NodeInfo] {
	cache := make(map[string]float64)
	return func(p1, p2 *device.NodeInfo) bool {
		return cachedNodeScore(cache, p1, profile, util.BinpackPolicy) >
			cachedNodeScore(cache, p2, profile, util.BinpackPolicy)
	}
}

// WeightedSpreadNodeLess mirrors WeightedBinpackNodeLess but for spread —
// ranks higher the nodes whose request-weighted FREE fraction is higher.
func WeightedSpreadNodeLess(profile RequestProfile) LessFunc[*device.NodeInfo] {
	cache := make(map[string]float64)
	return func(p1, p2 *device.NodeInfo) bool {
		return cachedNodeScore(cache, p1, profile, util.SpreadPolicy) >
			cachedNodeScore(cache, p2, profile, util.SpreadPolicy)
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
// group ≥ needNumber), strictly stronger than the prior "just has topology
// metadata" check.
//
// needNumber is the total per-pod GPU count (single-container multi-GPU pods
// pass the container's vgpu-number; multi-container pods pass the sum). For
// none-topology or single-GPU requests this is a no-op.
//
// Both strict and non-strict topology variants get the same prepended
// comparator — strictness only changes ALLOCATION fallback behaviour
// (handled inside allocateByTopologyMode), not node ranking.
func ApplyTopologyMode(mode util.TopologyMode, needNumber int,
	less []LessFunc[*device.NodeInfo]) []LessFunc[*device.NodeInfo] {
	switch mode.BaseTopology() {
	case util.LinkTopology:
		less = slices.Insert[[]LessFunc[*device.NodeInfo],
			LessFunc[*device.NodeInfo]](less, 0, ByNodeGPUTopologyFitness(needNumber))
	case util.NUMATopology:
		less = slices.Insert[[]LessFunc[*device.NodeInfo],
			LessFunc[*device.NodeInfo]](less, 0, ByNodeNUMATopologyFitness(needNumber))
	}
	return less
}

func NewNodeBinpackPriority(profile RequestProfile, mode util.TopologyMode, needNumber int) *sortPriority[*device.NodeInfo] {
	less := []LessFunc[*device.NodeInfo]{
		WeightedBinpackNodeLess(profile),
		ByNodeNameAsc,
	}
	return &sortPriority[*device.NodeInfo]{
		less: ApplyTopologyMode(mode, needNumber, less),
	}
}

func NewNodeSpreadPriority(profile RequestProfile, mode util.TopologyMode, needNumber int) *sortPriority[*device.NodeInfo] {
	less := []LessFunc[*device.NodeInfo]{
		WeightedSpreadNodeLess(profile),
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
