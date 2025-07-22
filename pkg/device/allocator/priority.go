package allocator

import (
	"sort"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"golang.org/x/exp/slices"
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
	ByNumaAsc = func(p1, p2 *device.Device) bool {
		return p1.GetNUMA() < p2.GetNUMA()
	}
	ByNodeNameAsc = func(p1, p2 *device.NodeInfo) bool {
		return p1.GetName() < p2.GetName()
	}
	// ByNodeScoreAsc Sort in ascending order based on node scores,
	// to avoid double counting scores, a score cache was used.
	ByNodeScoreAsc = func() func(p1, p2 *device.NodeInfo) bool {
		nodeScoreMap := map[string]float64{}
		return func(p1, p2 *device.NodeInfo) bool {
			p1Score, ok := nodeScoreMap[p1.GetName()]
			if !ok {
				p1Score = GetNodeScore(p1, util.HundredCore)
				nodeScoreMap[p1.GetName()] = p1Score
			}
			p2Score, ok := nodeScoreMap[p2.GetName()]
			if !ok {
				p2Score = GetNodeScore(p2, util.HundredCore)
				nodeScoreMap[p2.GetName()] = p2Score
			}
			return p1Score < p2Score
		}
	}
	// ByNodeScoreDes Sort in descending order based on node scores,
	// to avoid double counting scores, a score cache was used.
	ByNodeScoreDes = func() func(p1, p2 *device.NodeInfo) bool {
		nodeScoreMap := map[string]float64{}
		return func(p1, p2 *device.NodeInfo) bool {
			p1Score, ok := nodeScoreMap[p1.GetName()]
			if !ok {
				p1Score = GetNodeScore(p1, util.HundredCore)
				nodeScoreMap[p1.GetName()] = p1Score
			}
			p2Score, ok := nodeScoreMap[p2.GetName()]
			if !ok {
				p2Score = GetNodeScore(p2, util.HundredCore)
				nodeScoreMap[p2.GetName()] = p2Score
			}
			return p1Score > p2Score
		}
	}
	// ByNodeGPUTopology Nodes with GPU topology information will be ranked first.
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
)

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

// GetNodeScore Calculate node score: freeResource / totalResource = scorePercentage
func GetNodeScore(info *device.NodeInfo, multiplier float64) float64 {
	numPercentage := safeDiv(float64(info.GetAvailableNumber()), float64(info.GetTotalNumber()))
	memPercentage := safeDiv(float64(info.GetAvailableMemory()), float64(info.GetTotalMemory()))
	corePercentage := safeDiv(float64(info.GetAvailableCores()), float64(info.GetTotalCores()))
	score := multiplier * (numPercentage + memPercentage + corePercentage)
	klog.V(5).Infof("Current Node <%s> resource score is <%.2f>", info.GetName(), score)
	return score
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func NewNodeBinpackPriority(needGPUTopo bool) *sortPriority[*device.NodeInfo] {
	less := []LessFunc[*device.NodeInfo]{
		ByNodeScoreAsc(),
		ByNodeNameAsc,
	}
	if needGPUTopo {
		less = slices.Insert[[]LessFunc[*device.NodeInfo],
			LessFunc[*device.NodeInfo]](less, 0, ByNodeGPUTopology)
	}
	return &sortPriority[*device.NodeInfo]{
		less: less,
	}
}

func NewNodeSpreadPriority(needGPUTopo bool) *sortPriority[*device.NodeInfo] {
	less := []LessFunc[*device.NodeInfo]{
		ByNodeScoreDes(),
		ByNodeNameAsc,
	}
	if needGPUTopo {
		less = slices.Insert[[]LessFunc[*device.NodeInfo],
			LessFunc[*device.NodeInfo]](less, 0, ByNodeGPUTopology)
	}
	return &sortPriority[*device.NodeInfo]{
		less: less,
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
