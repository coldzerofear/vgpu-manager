package device

import (
	"sort"

	"k8s.io/klog/v2"
)

// LessFunc represents function to compare two DeviceInfo or NodeInfo
type LessFunc[T any] func(p1, p2 T) bool

var (
	// ByAllocatableMemoryAsc Compare the assignable memory of two devices in ascending order
	ByAllocatableMemoryAsc = func(p1, p2 *DeviceInfo) bool {
		return p1.AllocatableMemory() < p2.AllocatableMemory()
	}
	// ByAllocatableMemoryDes Compare the assignable memory of two devices in descending order
	ByAllocatableMemoryDes = func(p1, p2 *DeviceInfo) bool {
		return p1.AllocatableMemory() > p2.AllocatableMemory()
	}
	// ByAllocatableCoresAsc Compare the assignable cores of two devices in ascending order
	ByAllocatableCoresAsc = func(p1, p2 *DeviceInfo) bool {
		return p1.AllocatableCores() < p2.AllocatableCores()
	}
	// ByAllocatableCoresDes Compare the assignable cores of two devices in descending order
	ByAllocatableCoresDes = func(p1, p2 *DeviceInfo) bool {
		return p1.AllocatableCores() > p2.AllocatableCores()
	}
	// ByDeviceIdAsc Compare the device id of two devices in ascending order
	ByDeviceIdAsc = func(p1, p2 *DeviceInfo) bool {
		return p1.GetID() < p2.GetID()
	}
	ByAllocatableNumberDes = func(p1, p2 *DeviceInfo) bool {
		return p1.AllocatableNumber() > p2.AllocatableNumber()
	}
	ByNumaAsc = func(p1, p2 *DeviceInfo) bool {
		return p1.GetNUMA() < p2.GetNUMA()
	}
	ByNodeNameAsc = func(p1, p2 *NodeInfo) bool {
		return p1.GetName() < p2.GetName()
	}
	// ByNodeScoreAsc Sort in ascending order based on node scores,
	// to avoid double counting scores, a score cache was used.
	ByNodeScoreAsc = func(nodeInfoList []*NodeInfo) func(p1, p2 *NodeInfo) bool {
		nodeScoreMap := map[string]float64{}
		for _, nodeInfo := range nodeInfoList {
			nodeScoreMap[nodeInfo.GetName()] = GetNodeScore(nodeInfo)
		}
		return func(p1, p2 *NodeInfo) bool {
			return nodeScoreMap[p1.GetName()] < nodeScoreMap[p2.GetName()]
		}
	}
	// ByNodeScoreDes Sort in descending order based on node scores,
	// to avoid double counting scores, a score cache was used.
	ByNodeScoreDes = func(nodeInfoList []*NodeInfo) func(p1, p2 *NodeInfo) bool {
		nodeScoreMap := map[string]float64{}
		for _, nodeInfo := range nodeInfoList {
			nodeScoreMap[nodeInfo.GetName()] = GetNodeScore(nodeInfo)
		}
		return func(p1, p2 *NodeInfo) bool {
			return nodeScoreMap[p1.GetName()] > nodeScoreMap[p2.GetName()]
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
func GetNodeScore(info *NodeInfo) float64 {
	var multiplier = float64(100)
	numPercentage := safeDiv(float64(info.GetAvailableNumber()), float64(info.GetTotalNumber()))
	memPercentage := safeDiv(float64(info.GetAvailableMemory()), float64(info.GetTotalMemory()))
	corePercentage := safeDiv(float64(info.GetAvailableCores()), float64(info.GetTotalCores()))
	score := multiplier * (numPercentage + memPercentage + corePercentage)
	klog.V(5).Infof("Current Node <%s> resource score is <%.2f>", info.name, score)
	return score
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func NewNodeBinpackPriority(nodeInfos []*NodeInfo) *sortPriority[*NodeInfo] {
	return &sortPriority[*NodeInfo]{
		less: []LessFunc[*NodeInfo]{
			ByNodeScoreAsc(nodeInfos),
			ByNodeNameAsc,
		},
	}
}

func NewNodeSpreadPriority(nodeInfos []*NodeInfo) *sortPriority[*NodeInfo] {
	return &sortPriority[*NodeInfo]{
		less: []LessFunc[*NodeInfo]{
			ByNodeScoreDes(nodeInfos),
			ByNodeNameAsc,
		},
	}
}

func NewDeviceBinpackPriority() *sortPriority[*DeviceInfo] {
	return &sortPriority[*DeviceInfo]{
		less: []LessFunc[*DeviceInfo]{
			ByAllocatableMemoryAsc,
			ByAllocatableCoresAsc,
			ByDeviceIdAsc,
		},
	}
}

func NewDeviceSpreadPriority() *sortPriority[*DeviceInfo] {
	return &sortPriority[*DeviceInfo]{
		less: []LessFunc[*DeviceInfo]{
			ByAllocatableMemoryDes,
			ByAllocatableNumberDes,
			ByAllocatableCoresDes,
			ByDeviceIdAsc,
		},
	}
}
