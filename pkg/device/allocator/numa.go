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

// sortByScoreDesc orders NUMA group ids by the chosen policy's score in
// DESCENDING order — Score already encodes the policy direction (binpack
// returns weighted USED, spread returns weighted FREE), so the highest
// score is always the most-preferred group regardless of mode. Replaces
// the legacy sortScoreAsc which encoded the direction by reading the same
// "free fraction" both ways and reversing iteration in SpreadCallback.
func (n NumaNodeDevice) sortByScoreDesc(profile RequestProfile, mode util.SchedulerPolicy) []int {
	numaNodes := make([]int, 0, len(n))
	for numaNode := range n {
		numaNodes = append(numaNodes, numaNode)
	}
	sort.Slice(numaNodes, func(i, j int) bool {
		sA := Score(NumaUtilization(n[numaNodes[i]]), profile, mode)
		sB := Score(NumaUtilization(n[numaNodes[j]]), profile, mode)
		return sA > sB
	})
	return numaNodes
}

type Callback func(numaNode int, devices []*device.Device) (done bool)

func (n NumaNodeDevice) SchedulerPolicyCallback(profile RequestProfile, policy util.SchedulerPolicy, callback Callback) {
	switch policy {
	case util.BinpackPolicy:
		n.BinpackCallback(profile, callback)
	case util.SpreadPolicy:
		n.SpreadCallback(profile, callback)
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

func (n NumaNodeDevice) BinpackCallback(profile RequestProfile, callback Callback) {
	if callback == nil {
		return
	}
	for _, numaNode := range n.sortByScoreDesc(profile, util.BinpackPolicy) {
		if callback(numaNode, n[numaNode]) {
			return
		}
	}
}

func (n NumaNodeDevice) SpreadCallback(profile RequestProfile, callback Callback) {
	if callback == nil {
		return
	}
	for _, numaNode := range n.sortByScoreDesc(profile, util.SpreadPolicy) {
		if callback(numaNode, n[numaNode]) {
			return
		}
	}
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
