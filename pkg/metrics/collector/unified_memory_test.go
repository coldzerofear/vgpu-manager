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

package collector

import (
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/stretchr/testify/assert"
)

func procs(used ...uint64) procInfoList {
	list := make(procInfoList, len(used))
	for i, u := range used {
		list[uint32(i+1)] = nvml.ProcessInfo_v1{Pid: uint32(i + 1), UsedGpuMemory: u}
	}
	return list
}

func TestDeviceMemoryUsage(t *testing.T) {
	const gib = uint64(1024 * 1024 * 1024)

	tests := map[string]struct {
		unified bool
		total   uint64
		used    uint64
		procs   procInfoList
		want    uint64
	}{
		// A device with its own framebuffer: NVML knows, the process list is
		// not consulted even when it disagrees.
		"discrete device": {
			total: 24 * gib, used: 3 * gib, procs: procs(gib), want: 3 * gib,
		},
		"discrete device, idle": {
			total: 24 * gib, used: 0, procs: procs(gib), want: 0,
		},
		// Unified memory with an override configured: NVML reports no usage, so
		// the per-process numbers already collected stand in for it.
		"unified device, summed from processes": {
			unified: true, total: 64 * gib, used: 0, procs: procs(2*gib, 3*gib), want: 5 * gib,
		},
		"unified device, no processes": {
			unified: true, total: 64 * gib, used: 0, procs: nil, want: 0,
		},
		// The pool is shared with the CPU and the override is only a
		// bookkeeping ceiling, so the sum can exceed it.
		"unified device, more than the ceiling": {
			unified: true, total: 8 * gib, used: 0, procs: procs(6*gib, 5*gib), want: 8 * gib,
		},
		// Nothing to be a fraction of, and no ceiling to cap against.
		"unified device without an override": {
			unified: true, total: 0, used: 0, procs: procs(2 * gib), want: 0,
		},
		// If a future driver does report usage on such a part, it wins.
		"unified device that reports its usage": {
			unified: true, total: 64 * gib, used: 7 * gib, procs: procs(2 * gib), want: 7 * gib,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gpuInfo := &nvidia.GpuInfo{
				UnifiedMemory: tc.unified,
				Memory:        nvml.Memory{Total: tc.total, Used: tc.used},
			}

			got := deviceMemoryUsage(gpuInfo, tc.procs)

			assert.Equal(t, tc.want, got)
			// Whatever the inputs, the usage never exceeds the reported total,
			// so the utilization rate derived from it cannot pass 100%.
			if tc.total > 0 {
				assert.LessOrEqual(t, got, tc.total)
			}
		})
	}
}
