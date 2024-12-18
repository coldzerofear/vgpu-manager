package device

import (
	"sort"

	"k8s.io/klog/v2"
)

// LessFunc represents funcion to compare two DeviceInfo or NodeInfo
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
	//ByAllocatableNumberAsc = func(p1, p2 *DeviceInfo) bool {
	//	return p1.AllocatableNumber() < p2.AllocatableNumber()
	//}
	ByNodeScoreDes = func(p1, p2 *NodeInfo) bool {
		return GetNodeScore(p1) > GetNodeScore(p2)
	}
	ByNodeScoreAsc = func(p1, p2 *NodeInfo) bool {
		return GetNodeScore(p1) < GetNodeScore(p2)
	}
	ByNodeNameAsc = func(p1, p2 *NodeInfo) bool {
		return p1.GetName() < p2.GetName()
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

func NewNodeBinpackPriority() *sortPriority[*NodeInfo] {
	return &sortPriority[*NodeInfo]{
		less: []LessFunc[*NodeInfo]{
			ByNodeScoreAsc,
			ByNodeNameAsc,
		},
	}
}

// GetNodeScore 计算节点空闲资源得分：空闲资源 / 资源总量 = 可用资源百分比
func GetNodeScore(info *NodeInfo) float64 {
	var multiplier = float64(100)
	numPercentage := float64(info.GetAvailableNumber()) / float64(info.GetTotalNumber())
	memPercentage := float64(info.GetAvailableMemory()) / float64(info.GetTotalMemory())
	corePercentage := float64(info.GetAvailableCore()) / float64(info.GetTotalCore())
	score := multiplier * (numPercentage + memPercentage + corePercentage)
	klog.V(5).Infof("Current Node <%s> resource score is <%.2f>", info.name, score)
	return score
}

func NewNodeSpreadPriority() *sortPriority[*NodeInfo] {
	return &sortPriority[*NodeInfo]{
		less: []LessFunc[*NodeInfo]{
			ByNodeScoreDes,
			ByNodeNameAsc,
		},
	}
}

func NewDeviceBinpackPriority(number int) *sortPriority[*DeviceInfo] {
	var lessFunc = make([]LessFunc[*DeviceInfo], 0, 4)
	if number > 1 {
		lessFunc = append(lessFunc, ByNumaAsc)
	}
	lessFunc = append(lessFunc, ByAllocatableMemoryAsc,
		ByAllocatableCoresAsc, ByDeviceIdAsc)
	return &sortPriority[*DeviceInfo]{
		less: lessFunc,
	}
}

func NewDeviceSpreadPriority(number int) *sortPriority[*DeviceInfo] {
	var lessFunc = make([]LessFunc[*DeviceInfo], 0, 5)
	if number > 1 {
		lessFunc = append(lessFunc, ByNumaAsc)
	}
	lessFunc = append(lessFunc, ByAllocatableMemoryDes,
		ByAllocatableNumberDes, ByAllocatableCoresDes,
		ByDeviceIdAsc)
	return &sortPriority[*DeviceInfo]{
		less: lessFunc,
	}
}
