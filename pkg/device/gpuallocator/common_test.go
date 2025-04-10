// Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.

package gpuallocator

import (
	"sort"
	"testing"

	nvml "github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
)

const pad = ^int(0)

type AllocTest struct {
	size   int
	result []int
}

type PolicyAllocTest struct {
	description string
	devices     []*Device
	available   []int
	required    []int
	size        int
	result      []int
}

func GetDevicesFromIndices(allDevices []*Device, indices []int) []*Device {
	var input []*Device
	for _, i := range indices {
		for _, device := range allDevices {
			if i == pad {
				input = append(input, nil)
				break
			}
			if i == device.Index {
				input = append(input, device)
				break
			}
		}
	}
	return input
}

func sortGPUSet(set []*Device) {
	sort.Slice(set, func(i, j int) bool {
		if set[i] == nil {
			return true
		}
		if set[j] == nil {
			return false
		}
		return set[i].Index < set[j].Index
	})
}

func RunAllocTests(t *testing.T, allocator *Allocator, tests []AllocTest) {
	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			result := GetDevicesFromIndices(allocator.GPUs, tc.result)

			allocated := allocator.Allocate(tc.size)
			if len(allocated) != len(tc.result) {
				t.Errorf("got %v, want %v", allocated, tc.result)
				return
			}

			sortGPUSet(allocated)
			sortGPUSet(result)

			for _, device := range allocated {
				if !NewDeviceSet(result...).Contains(device) {
					t.Errorf("got %v, want %v", allocated, tc.result)
					break
				}
			}
		})
	}
}

func RunPolicyAllocTests(t *testing.T, policy Policy, tests []PolicyAllocTest) {
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			available := GetDevicesFromIndices(tc.devices, tc.available)
			required := GetDevicesFromIndices(tc.devices, tc.required)
			result := GetDevicesFromIndices(tc.devices, tc.result)

			allocated := policy.Allocate(available, required, tc.size)
			if len(allocated) != len(tc.result) {
				t.Errorf("got %v, want %v", allocated, tc.result)
				return
			}

			sortGPUSet(allocated)
			sortGPUSet(result)

			for _, device := range allocated {
				if !NewDeviceSet(result...).Contains(device) {
					t.Errorf("got %v, want %v", allocated, tc.result)
					break
				}
			}
		})
	}
}

func New4xRTX8000Node() DeviceList {
	node := DeviceList{
		NewDevice(0, "GPU-0", "GPU-0"),
		NewDevice(1, "GPU-1", "GPU-1"),
		NewDevice(2, "GPU-2", "GPU-2"),
		NewDevice(3, "GPU-3", "GPU-3"),
	}

	// NVLinks
	node.AddLink(0, 3, nvml.TwoNVLINKLinks)
	node.AddLink(1, 2, nvml.TwoNVLINKLinks)
	node.AddLink(2, 1, nvml.TwoNVLINKLinks)
	node.AddLink(3, 0, nvml.TwoNVLINKLinks)

	// P2PLinks
	node.AddLink(0, 1, nvml.P2PLinkSameCPU)
	node.AddLink(0, 2, nvml.P2PLinkCrossCPU)
	node.AddLink(1, 0, nvml.P2PLinkSameCPU)
	node.AddLink(1, 3, nvml.P2PLinkCrossCPU)
	node.AddLink(2, 0, nvml.P2PLinkCrossCPU)
	node.AddLink(2, 3, nvml.P2PLinkSameCPU)
	node.AddLink(3, 1, nvml.P2PLinkCrossCPU)
	node.AddLink(3, 2, nvml.P2PLinkSameCPU)

	return node
}

func NewDGX1PascalNode() DeviceList {
	node := DeviceList{
		NewDevice(0, "GPU-0", "GPU-0"),
		NewDevice(1, "GPU-1", "GPU-1"),
		NewDevice(2, "GPU-2", "GPU-2"),
		NewDevice(3, "GPU-3", "GPU-3"),
		NewDevice(4, "GPU-4", "GPU-4"),
		NewDevice(5, "GPU-5", "GPU-5"),
		NewDevice(6, "GPU-6", "GPU-6"),
		NewDevice(7, "GPU-7", "GPU-7"),
	}

	// NVLinks
	node.AddLink(0, 1, nvml.SingleNVLINKLink)
	node.AddLink(0, 2, nvml.SingleNVLINKLink)
	node.AddLink(0, 3, nvml.SingleNVLINKLink)
	node.AddLink(0, 4, nvml.SingleNVLINKLink)

	node.AddLink(1, 0, nvml.SingleNVLINKLink)
	node.AddLink(1, 2, nvml.SingleNVLINKLink)
	node.AddLink(1, 3, nvml.SingleNVLINKLink)
	node.AddLink(1, 5, nvml.SingleNVLINKLink)

	node.AddLink(2, 0, nvml.SingleNVLINKLink)
	node.AddLink(2, 1, nvml.SingleNVLINKLink)
	node.AddLink(2, 3, nvml.SingleNVLINKLink)
	node.AddLink(2, 6, nvml.SingleNVLINKLink)

	node.AddLink(3, 0, nvml.SingleNVLINKLink)
	node.AddLink(3, 1, nvml.SingleNVLINKLink)
	node.AddLink(3, 2, nvml.SingleNVLINKLink)
	node.AddLink(3, 7, nvml.SingleNVLINKLink)

	node.AddLink(4, 0, nvml.SingleNVLINKLink)
	node.AddLink(4, 5, nvml.SingleNVLINKLink)
	node.AddLink(4, 6, nvml.SingleNVLINKLink)
	node.AddLink(4, 7, nvml.SingleNVLINKLink)

	node.AddLink(5, 1, nvml.SingleNVLINKLink)
	node.AddLink(5, 4, nvml.SingleNVLINKLink)
	node.AddLink(5, 6, nvml.SingleNVLINKLink)
	node.AddLink(5, 7, nvml.SingleNVLINKLink)

	node.AddLink(6, 2, nvml.SingleNVLINKLink)
	node.AddLink(6, 4, nvml.SingleNVLINKLink)
	node.AddLink(6, 5, nvml.SingleNVLINKLink)
	node.AddLink(6, 7, nvml.SingleNVLINKLink)

	node.AddLink(7, 3, nvml.SingleNVLINKLink)
	node.AddLink(7, 4, nvml.SingleNVLINKLink)
	node.AddLink(7, 5, nvml.SingleNVLINKLink)
	node.AddLink(7, 6, nvml.SingleNVLINKLink)

	// P2PLinks
	node.AddLink(0, 1, nvml.P2PLinkHostBridge)
	node.AddLink(0, 2, nvml.P2PLinkSingleSwitch)
	node.AddLink(0, 3, nvml.P2PLinkHostBridge)
	node.AddLink(0, 4, nvml.P2PLinkCrossCPU)
	node.AddLink(0, 5, nvml.P2PLinkCrossCPU)
	node.AddLink(0, 6, nvml.P2PLinkCrossCPU)
	node.AddLink(0, 7, nvml.P2PLinkCrossCPU)

	node.AddLink(1, 0, nvml.P2PLinkHostBridge)
	node.AddLink(1, 2, nvml.P2PLinkHostBridge)
	node.AddLink(1, 3, nvml.P2PLinkSingleSwitch)
	node.AddLink(1, 4, nvml.P2PLinkCrossCPU)
	node.AddLink(1, 5, nvml.P2PLinkCrossCPU)
	node.AddLink(1, 6, nvml.P2PLinkCrossCPU)
	node.AddLink(1, 7, nvml.P2PLinkCrossCPU)

	node.AddLink(2, 0, nvml.P2PLinkSingleSwitch)
	node.AddLink(2, 1, nvml.P2PLinkHostBridge)
	node.AddLink(2, 3, nvml.P2PLinkHostBridge)
	node.AddLink(2, 4, nvml.P2PLinkCrossCPU)
	node.AddLink(2, 5, nvml.P2PLinkCrossCPU)
	node.AddLink(2, 6, nvml.P2PLinkCrossCPU)
	node.AddLink(2, 7, nvml.P2PLinkCrossCPU)

	node.AddLink(3, 0, nvml.P2PLinkHostBridge)
	node.AddLink(3, 1, nvml.P2PLinkSingleSwitch)
	node.AddLink(3, 2, nvml.P2PLinkHostBridge)
	node.AddLink(3, 4, nvml.P2PLinkCrossCPU)
	node.AddLink(3, 5, nvml.P2PLinkCrossCPU)
	node.AddLink(3, 6, nvml.P2PLinkCrossCPU)
	node.AddLink(3, 7, nvml.P2PLinkCrossCPU)

	node.AddLink(4, 0, nvml.P2PLinkCrossCPU)
	node.AddLink(4, 1, nvml.P2PLinkCrossCPU)
	node.AddLink(4, 2, nvml.P2PLinkCrossCPU)
	node.AddLink(4, 3, nvml.P2PLinkCrossCPU)
	node.AddLink(4, 5, nvml.P2PLinkHostBridge)
	node.AddLink(4, 6, nvml.P2PLinkSingleSwitch)
	node.AddLink(4, 7, nvml.P2PLinkHostBridge)

	node.AddLink(5, 0, nvml.P2PLinkCrossCPU)
	node.AddLink(5, 1, nvml.P2PLinkCrossCPU)
	node.AddLink(5, 2, nvml.P2PLinkCrossCPU)
	node.AddLink(5, 3, nvml.P2PLinkCrossCPU)
	node.AddLink(5, 4, nvml.P2PLinkHostBridge)
	node.AddLink(5, 6, nvml.P2PLinkHostBridge)
	node.AddLink(5, 7, nvml.P2PLinkSingleSwitch)

	node.AddLink(6, 0, nvml.P2PLinkCrossCPU)
	node.AddLink(6, 1, nvml.P2PLinkCrossCPU)
	node.AddLink(6, 2, nvml.P2PLinkCrossCPU)
	node.AddLink(6, 3, nvml.P2PLinkCrossCPU)
	node.AddLink(6, 4, nvml.P2PLinkSingleSwitch)
	node.AddLink(6, 5, nvml.P2PLinkHostBridge)
	node.AddLink(6, 7, nvml.P2PLinkHostBridge)

	node.AddLink(7, 0, nvml.P2PLinkCrossCPU)
	node.AddLink(7, 1, nvml.P2PLinkCrossCPU)
	node.AddLink(7, 2, nvml.P2PLinkCrossCPU)
	node.AddLink(7, 3, nvml.P2PLinkCrossCPU)
	node.AddLink(7, 4, nvml.P2PLinkHostBridge)
	node.AddLink(7, 5, nvml.P2PLinkSingleSwitch)
	node.AddLink(7, 6, nvml.P2PLinkHostBridge)

	return node
}

func NewDGX1VoltaNode() DeviceList {
	node := DeviceList{
		NewDevice(0, "GPU-0", "GPU-0"),
		NewDevice(1, "GPU-1", "GPU-1"),
		NewDevice(2, "GPU-2", "GPU-2"),
		NewDevice(3, "GPU-3", "GPU-3"),
		NewDevice(4, "GPU-4", "GPU-4"),
		NewDevice(5, "GPU-5", "GPU-5"),
		NewDevice(6, "GPU-6", "GPU-6"),
		NewDevice(7, "GPU-7", "GPU-7"),
	}

	// NVLinks
	node.AddLink(0, 1, nvml.SingleNVLINKLink)
	node.AddLink(0, 2, nvml.SingleNVLINKLink)
	node.AddLink(0, 3, nvml.TwoNVLINKLinks)
	node.AddLink(0, 4, nvml.TwoNVLINKLinks)

	node.AddLink(1, 0, nvml.SingleNVLINKLink)
	node.AddLink(1, 2, nvml.TwoNVLINKLinks)
	node.AddLink(1, 3, nvml.SingleNVLINKLink)
	node.AddLink(1, 5, nvml.TwoNVLINKLinks)

	node.AddLink(2, 0, nvml.SingleNVLINKLink)
	node.AddLink(2, 1, nvml.TwoNVLINKLinks)
	node.AddLink(2, 3, nvml.TwoNVLINKLinks)
	node.AddLink(2, 6, nvml.SingleNVLINKLink)

	node.AddLink(3, 0, nvml.TwoNVLINKLinks)
	node.AddLink(3, 1, nvml.SingleNVLINKLink)
	node.AddLink(3, 2, nvml.TwoNVLINKLinks)
	node.AddLink(3, 7, nvml.SingleNVLINKLink)

	node.AddLink(4, 0, nvml.TwoNVLINKLinks)
	node.AddLink(4, 5, nvml.SingleNVLINKLink)
	node.AddLink(4, 6, nvml.SingleNVLINKLink)
	node.AddLink(4, 7, nvml.TwoNVLINKLinks)

	node.AddLink(5, 1, nvml.TwoNVLINKLinks)
	node.AddLink(5, 4, nvml.SingleNVLINKLink)
	node.AddLink(5, 6, nvml.TwoNVLINKLinks)
	node.AddLink(5, 7, nvml.SingleNVLINKLink)

	node.AddLink(6, 2, nvml.SingleNVLINKLink)
	node.AddLink(6, 4, nvml.SingleNVLINKLink)
	node.AddLink(6, 5, nvml.TwoNVLINKLinks)
	node.AddLink(6, 7, nvml.TwoNVLINKLinks)

	node.AddLink(7, 3, nvml.SingleNVLINKLink)
	node.AddLink(7, 4, nvml.TwoNVLINKLinks)
	node.AddLink(7, 5, nvml.SingleNVLINKLink)
	node.AddLink(7, 6, nvml.TwoNVLINKLinks)

	// P2PLinks
	node.AddLink(0, 1, nvml.P2PLinkSingleSwitch)
	node.AddLink(0, 2, nvml.P2PLinkHostBridge)
	node.AddLink(0, 3, nvml.P2PLinkHostBridge)
	node.AddLink(0, 4, nvml.P2PLinkCrossCPU)
	node.AddLink(0, 5, nvml.P2PLinkCrossCPU)
	node.AddLink(0, 6, nvml.P2PLinkCrossCPU)
	node.AddLink(0, 7, nvml.P2PLinkCrossCPU)

	node.AddLink(1, 0, nvml.P2PLinkSingleSwitch)
	node.AddLink(1, 2, nvml.P2PLinkHostBridge)
	node.AddLink(1, 3, nvml.P2PLinkHostBridge)
	node.AddLink(1, 4, nvml.P2PLinkCrossCPU)
	node.AddLink(1, 5, nvml.P2PLinkCrossCPU)
	node.AddLink(1, 6, nvml.P2PLinkCrossCPU)
	node.AddLink(1, 7, nvml.P2PLinkCrossCPU)

	node.AddLink(2, 0, nvml.P2PLinkHostBridge)
	node.AddLink(2, 1, nvml.P2PLinkHostBridge)
	node.AddLink(2, 3, nvml.P2PLinkSingleSwitch)
	node.AddLink(2, 4, nvml.P2PLinkCrossCPU)
	node.AddLink(2, 5, nvml.P2PLinkCrossCPU)
	node.AddLink(2, 6, nvml.P2PLinkCrossCPU)
	node.AddLink(2, 7, nvml.P2PLinkCrossCPU)

	node.AddLink(3, 0, nvml.P2PLinkHostBridge)
	node.AddLink(3, 1, nvml.P2PLinkHostBridge)
	node.AddLink(3, 2, nvml.P2PLinkSingleSwitch)
	node.AddLink(3, 4, nvml.P2PLinkCrossCPU)
	node.AddLink(3, 5, nvml.P2PLinkCrossCPU)
	node.AddLink(3, 6, nvml.P2PLinkCrossCPU)
	node.AddLink(3, 7, nvml.P2PLinkCrossCPU)

	node.AddLink(4, 0, nvml.P2PLinkCrossCPU)
	node.AddLink(4, 1, nvml.P2PLinkCrossCPU)
	node.AddLink(4, 2, nvml.P2PLinkCrossCPU)
	node.AddLink(4, 3, nvml.P2PLinkCrossCPU)
	node.AddLink(4, 5, nvml.P2PLinkSingleSwitch)
	node.AddLink(4, 6, nvml.P2PLinkHostBridge)
	node.AddLink(4, 7, nvml.P2PLinkHostBridge)

	node.AddLink(5, 0, nvml.P2PLinkCrossCPU)
	node.AddLink(5, 1, nvml.P2PLinkCrossCPU)
	node.AddLink(5, 2, nvml.P2PLinkCrossCPU)
	node.AddLink(5, 3, nvml.P2PLinkCrossCPU)
	node.AddLink(5, 4, nvml.P2PLinkSingleSwitch)
	node.AddLink(5, 6, nvml.P2PLinkHostBridge)
	node.AddLink(5, 7, nvml.P2PLinkHostBridge)

	node.AddLink(6, 0, nvml.P2PLinkCrossCPU)
	node.AddLink(6, 1, nvml.P2PLinkCrossCPU)
	node.AddLink(6, 2, nvml.P2PLinkCrossCPU)
	node.AddLink(6, 3, nvml.P2PLinkCrossCPU)
	node.AddLink(6, 4, nvml.P2PLinkHostBridge)
	node.AddLink(6, 5, nvml.P2PLinkHostBridge)
	node.AddLink(6, 7, nvml.P2PLinkSingleSwitch)

	node.AddLink(7, 0, nvml.P2PLinkCrossCPU)
	node.AddLink(7, 1, nvml.P2PLinkCrossCPU)
	node.AddLink(7, 2, nvml.P2PLinkCrossCPU)
	node.AddLink(7, 3, nvml.P2PLinkCrossCPU)
	node.AddLink(7, 4, nvml.P2PLinkHostBridge)
	node.AddLink(7, 5, nvml.P2PLinkHostBridge)
	node.AddLink(7, 6, nvml.P2PLinkSingleSwitch)

	return node
}
