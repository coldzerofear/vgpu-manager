/*
Copyright 2026 coldzerofear

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

package kubeletplugin

import (
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/docker/go-units"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// unifiedTestGpuInfo is a GPU whose attributes pass the device-attribute
// conversion (the version fields are parsed with semver.MustParse).
func unifiedTestGpuInfo(totalMemory uint64) *nvidia.GpuInfo {
	return &nvidia.GpuInfo{
		Index: 0, Minor: 0, UUID: "GPU-ABC",
		ProductName:           "NVIDIA Test GPU",
		Brand:                 "NVIDIA",
		Architecture:          "Test",
		CudaComputeCapability: "12.1",
		DriverVersion: nvidia.DriverVersion{
			DriverVersion:     "580.0.0",
			CudaDriverVersion: 13000,
		},
		Memory: nvml.Memory{Total: totalMemory},
	}
}

// assertConsumableCapacityValid re-checks what the API server checks for a
// consumable capacity (pkg/apis/resource/validation: validateRequestPolicyRange).
// A slice that fails any of these is rejected as a whole, so the driver stops
// publishing devices entirely - which is what a zero memory capacity used to do
// on a unified-memory node.
func assertConsumableCapacityValid(t *testing.T, name resourceapi.QualifiedName, c resourceapi.DeviceCapacity) {
	t.Helper()
	if c.RequestPolicy == nil {
		return
	}
	capacity := c.Value.Value()
	policy := c.RequestPolicy
	require.NotNil(t, policy.Default, "%s: default is required when a policy is set", name)
	require.NotNil(t, policy.ValidRange, "%s: this driver only publishes validRange policies", name)

	minQ, maxQ, stepQ := policy.ValidRange.Min, policy.ValidRange.Max, policy.ValidRange.Step
	def := policy.Default.Value()
	require.NotNil(t, minQ, "%s: min is required when validRange is defined", name)
	assert.LessOrEqual(t, minQ.Value(), capacity, "%s: min is larger than the capacity value", name)
	assert.GreaterOrEqual(t, def, minQ.Value(), "%s: default is less than min", name)
	if maxQ != nil {
		assert.LessOrEqual(t, minQ.Value(), maxQ.Value(), "%s: min is larger than max", name)
		assert.LessOrEqual(t, maxQ.Value(), capacity, "%s: max is larger than the capacity value", name)
		assert.LessOrEqual(t, def, maxQ.Value(), "%s: default is more than max", name)
	}
	if stepQ != nil {
		assert.LessOrEqual(t, minQ.Value()+stepQ.Value(), capacity,
			"%s: one step past min is larger than the capacity value", name)
		assert.Zero(t, (def-minQ.Value())%stepQ.Value(), "%s: default is not a multiple of step from min", name)
		if maxQ != nil {
			assert.Zero(t, (maxQ.Value()-minQ.Value())%stepQ.Value(), "%s: max is not a multiple of step from min", name)
		}
	}
}

func TestVGpuDevicePublishesAValidPolicy(t *testing.T) {
	tests := map[string]struct {
		totalMemory uint64
		memoryRatio uint
		coresRatio  uint
	}{
		"discrete memory":           {totalMemory: 8 * units.GiB, memoryRatio: 100, coresRatio: 100},
		"discrete memory, oversold": {totalMemory: 8 * units.GiB, memoryRatio: 200, coresRatio: 200},
		"discrete memory, one MiB":  {totalMemory: units.MiB, memoryRatio: 100, coresRatio: 100},
		// A ratio that does not divide the device size leaves a capacity that is
		// not a whole number of MiB steps from the minimum.
		"discrete memory, indivisible ratio": {totalMemory: 24 * units.GiB, memoryRatio: 33, coresRatio: 100},
		"unified memory, no override":        {totalMemory: 0, memoryRatio: 100, coresRatio: 100},
		"unified memory, ratio irrelevant":   {totalMemory: 0, memoryRatio: 200, coresRatio: 100},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			d := &VGpuDeviceInfo{
				GpuDeviceInfo:     &GpuDeviceInfo{GpuInfo: unifiedTestGpuInfo(tc.totalMemory)},
				deviceCoresRatio:  tc.coresRatio,
				deviceMemoryRatio: tc.memoryRatio,
			}

			device := d.GetDevice()

			require.Contains(t, device.Capacity, MemoryResourceName)
			for capName, capacity := range device.Capacity {
				assertConsumableCapacityValid(t, capName, capacity)
			}
			// A device that cannot report its size still publishes the capacity,
			// so the vGPU stays a consumable device; it just consumes nothing.
			if tc.totalMemory == 0 {
				mem := device.Capacity[MemoryResourceName]
				assert.Zero(t, mem.Value.Value())
				assert.Nil(t, mem.RequestPolicy.ValidRange.Step,
					"a zero capacity cannot carry a step: min+step would exceed it")
			}
		})
	}
}

// The memory quota reaches the container as CUDA_MEM_LIMIT_<idx>. An empty
// value means "no quota"; "0m" means a zero-byte quota, which bricks the
// container (see get_mem_limit in library/src/util.c).
func TestVGpuAllocationMemoryEnvNeverRequestsZeroBytes(t *testing.T) {
	tests := map[string]struct {
		totalMemory uint64
		consumed    int64
		want        string
	}{
		"a slice of the device":        {totalMemory: 8 * units.GiB, consumed: 4 * units.GiB, want: "4096m"},
		"the whole device":             {totalMemory: 8 * units.GiB, consumed: 8 * units.GiB, want: ""},
		"more than the device":         {totalMemory: 8 * units.GiB, consumed: 16 * units.GiB, want: ""},
		"nothing of a discrete device": {totalMemory: 8 * units.GiB, consumed: 0, want: ""},
		"nothing of a unified device":  {totalMemory: 0, consumed: 0, want: ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			manager := &VGPUManager{deviceCoresRatio: 100, deviceMemoryRatio: 100}
			claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-a")}}
			result := &resourceapi.DeviceRequestAllocationResult{
				ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{
					MemoryResourceName: *resource.NewQuantity(tc.consumed, resource.BinarySI),
				},
			}
			device := &AllocatableDevice{VGpu: &VGpuDeviceInfo{
				GpuDeviceInfo: &GpuDeviceInfo{GpuInfo: unifiedTestGpuInfo(tc.totalMemory)},
			}}

			edits := manager.GetAllocationEnvContainerEdits(claim, result, device)
			require.NotNil(t, edits)

			prefix := fmt.Sprintf("%s_0=", util.CudaMemoryLimitEnv)
			var got string
			var found bool
			for _, env := range edits.ContainerEdits.Env {
				assert.NotEqual(t, prefix+"0m", env, "a zero-byte quota is never a valid limit")
				if strings.HasPrefix(env, prefix) {
					got, found = strings.TrimPrefix(env, prefix), true
				}
			}
			require.True(t, found, "the memory limit env must always be set, even when empty")
			assert.Equal(t, tc.want, got)
		})
	}
}
