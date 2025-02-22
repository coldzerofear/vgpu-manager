package device

import (
	"sort"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

type NumaDevices map[int][]*DeviceInfo

func NewNumaDevices(devices []*DeviceInfo) NumaDevices {
	numaDevices := make(NumaDevices, 0)
	for i, device := range devices {
		numaDevices[device.GetNUMA()] = append(numaDevices[device.GetNUMA()], devices[i])
	}
	return numaDevices
}

func (n NumaDevices) MaxDeviceNumberForNumaNode() int {
	maxNum := 0
	for _, devices := range n {
		maxNum = util.Max(maxNum, len(devices))
	}
	return maxNum
}

func (n NumaDevices) sortScoreAsc() []int {
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

type Callback func(numaNode int, devices []*DeviceInfo) (done bool)

func (n NumaDevices) NumaScoreBinpackCallback(callback Callback) {
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

func (n NumaDevices) NumaScoreSpreadCallback(callback Callback) {
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
func GetDeviceScore(info *DeviceInfo) float64 {
	var multiplier = float64(100)
	numPercentage := safeDiv(float64(info.AllocatableNumber()), float64(info.GetTotalNumber()))
	memPercentage := safeDiv(float64(info.AllocatableMemory()), float64(info.GetTotalMemory()))
	corePercentage := safeDiv(float64(info.AllocatableCores()), float64(info.GetTotalCores()))
	score := multiplier * (numPercentage + memPercentage + corePercentage)
	return score
}

// GetNumaNodeScore Calculate the average score of all devices on the numa node
func GetNumaNodeScore(devices []*DeviceInfo) float64 {
	score := float64(0)
	for _, device := range devices {
		score += GetDeviceScore(device)
	}
	return safeDiv(score, float64(len(devices)))
}

func CanNotCrossNumaNode(gpuNumber int, devices []*DeviceInfo) (NumaDevices, bool) {
	if gpuNumber > 1 {
		numaDevices := NewNumaDevices(devices)
		if gpuNumber <= numaDevices.MaxDeviceNumberForNumaNode() {
			return numaDevices, true
		}
	}
	return nil, false
}
