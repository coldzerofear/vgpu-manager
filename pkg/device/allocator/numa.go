/*
Copyright 2025-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package allocator

import (
	"sort"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

type NumaNodeDevice map[int][]*device.Device

// NewNumaNodeDevice buckets devices by their NUMA node.
//
// Devices reporting a NEGATIVE NUMA id are excluded. -1 is what the device
// plugin writes when it could not determine the affinity (common on consumer
// boards and on GPUs behind a bus the driver cannot map), so it means "unknown",
// not "NUMA node -1". Bucketing them together would invent a NUMA node out of
// exactly the cards whose NUMA node is not known, and numa-strict — whose whole
// contract is "these GPUs share one NUMA node" — would then be satisfied by a
// group that shares nothing.
//
// On a node where EVERY device is -1 this returns an empty grouping, so
// MaxDeviceNumberForNumaNode is 0 and CanNotCrossNumaNode refuses any request.
// That agrees with NodeInfo.HasNUMATopology, which is likewise false there; the
// two layers previously disagreed, and only the NodeInfo one was consulted.
func NewNumaNodeDevice(devices []*device.Device) NumaNodeDevice {
	numaNode := make(NumaNodeDevice, 0)
	for i, dev := range devices {
		if dev == nil || dev.GetNUMA() < 0 {
			continue
		}
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

// DefaultCallback is the no-device-policy path: with no preference to express,
// NUMA nodes are visited in ascending id order.
//
// The ordering is not cosmetic. Ranging over the map directly made the choice
// depend on Go's randomised map iteration, so the same pod on the same node
// could land on a different NUMA node from one Filter call to the next — and,
// because Filter runs again during preemption and re-scheduling, the decision
// was not even stable for a single pod. Measured on a two-NUMA node, the
// selection flipped in roughly one run out of six.
func (n NumaNodeDevice) DefaultCallback(callback Callback) {
	if callback == nil {
		return
	}
	numaNodes := make([]int, 0, len(n))
	for numaNode := range n {
		numaNodes = append(numaNodes, numaNode)
	}
	sort.Ints(numaNodes)
	for _, numaNode := range numaNodes {
		if callback(numaNode, n[numaNode]) {
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

// CanNotCrossNumaNode reports whether the request can be satisfied WITHOUT
// crossing a NUMA boundary, returning the NUMA grouping when it can.
//
// There is deliberately no special case for a single card. The question is
// vacuous for one GPU — it is always inside exactly one NUMA node — and the
// general path answers it correctly: the largest NUMA group holds at least one
// device whenever the node has any, so gpuNumber == 1 succeeds and the caller
// still gets the grouping, letting the device policy choose WHICH NUMA node to
// consume. That matters: consolidating a single card onto the NUMA node already
// in use leaves the other intact for a later multi-card request, which is the
// same reasoning the link path applies when it picks a component.
//
// A `gpuNumber > 1` guard used to sit here, harmless only because single-card
// requests were short-circuited before ever reaching the topology branch. Once
// they stopped being short-circuited it reported a false "unsatisfiable", and
// numa-strict turned that into a rejection of every node in the cluster.
func CanNotCrossNumaNode(gpuNumber int, devices []*device.Device) (NumaNodeDevice, bool) {
	numaDevices := NewNumaNodeDevice(devices)
	if gpuNumber <= numaDevices.MaxDeviceNumberForNumaNode() {
		return numaDevices, true
	}
	return nil, false
}
