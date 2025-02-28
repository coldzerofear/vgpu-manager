package allocator

import (
	"sort"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

type NumaNodeDevice map[int][]*device.Device

func NewNumaNodeDevice(devices []*device.Device) NumaNodeDevice {
	numaNode := make(NumaNodeDevice, 0)
	for i, dev := range devices {
		numaNode[dev.GetNUMA()] = append(numaNode[dev.GetNUMA()], devices[i])
	}
	return numaNode
}

func (n NumaNodeDevice) MaxDeviceNumberForNumaNode() int {
	maxNum := 0
	for _, devices := range n {
		maxNum = max(maxNum, len(devices))
	}
	return maxNum
}

func (n NumaNodeDevice) sortScoreAsc() []int {
	numaNodes := make([]int, 0, len(n))
	for numaNode := range n {
		numaNodes = append(numaNodes, numaNode)
	}
	sort.Slice(numaNodes, func(i, j int) bool {
		numaScoreA := GetNumaNodeScore(n[numaNodes[i]])
		numaScoreB := GetNumaNodeScore(n[numaNodes[j]])
		return numaScoreA < numaScoreB
	})
	return numaNodes
}

type Callback func(numaNode int, devices []*device.Device) (done bool)

func (n NumaNodeDevice) SchedulerPolicyCallback(policy util.SchedulerPolicy, callback Callback) {
	switch policy {
	case util.BinpackPolicy:
		n.BinpackCallback(callback)
	case util.SpreadPolicy:
		n.SpreadCallback(callback)
	default:
		n.DefaultCallback(callback)
	}
}

func (n NumaNodeDevice) DefaultCallback(callback Callback) {
	if callback == nil {
		return
	}
	for numaNode, devices := range n {
		if callback(numaNode, devices) {
			return
		}
	}
}

func (n NumaNodeDevice) BinpackCallback(callback Callback) {
	if callback == nil {
		return
	}
	numaNodes := n.sortScoreAsc()
	for _, numaNode := range numaNodes {
		if callback(numaNode, n[numaNode]) {
			return
		}
	}
}

func (n NumaNodeDevice) SpreadCallback(callback Callback) {
	if callback == nil {
		return
	}
	numaNodes := n.sortScoreAsc()
	for i := len(n) - 1; i >= 0; i-- {
		numaNode := numaNodes[i]
		if callback(numaNode, n[numaNode]) {
			return
		}
	}
}

// GetDeviceScore Calculate device score: assignableResource / totalResource = scorePercentage
func GetDeviceScore(info *device.Device) float64 {
	var multiplier = float64(100)
	numPercentage := safeDiv(float64(info.AllocatableNumber()), float64(info.GetTotalNumber()))
	memPercentage := safeDiv(float64(info.AllocatableMemory()), float64(info.GetTotalMemory()))
	corePercentage := safeDiv(float64(info.AllocatableCores()), float64(info.GetTotalCores()))
	score := multiplier * (numPercentage + memPercentage + corePercentage)
	return score
}

// GetNumaNodeScore Calculate the average score of all devices on the numa node
func GetNumaNodeScore(devices []*device.Device) float64 {
	score := float64(0)
	for _, dev := range devices {
		score += GetDeviceScore(dev)
	}
	return safeDiv(score, float64(len(devices)))
}

func CanNotCrossNumaNode(gpuNumber int, devices []*device.Device) (NumaNodeDevice, bool) {
	if gpuNumber > 1 {
		numaDevices := NewNumaNodeDevice(devices)
		if gpuNumber <= numaDevices.MaxDeviceNumberForNumaNode() {
			return numaDevices, true
		}
	}
	return nil, false
}
