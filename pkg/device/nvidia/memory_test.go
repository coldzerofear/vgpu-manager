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

package nvidia

import (
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/docker/go-units"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveDeviceMemory(t *testing.T) {
	const gib = 1024 * units.MiB

	tests := map[string]struct {
		ret         nvml.Return
		reported    uint64
		overrideMB  uint64
		wantTotal   uint64
		wantUnified bool
		wantErr     bool
	}{
		"a device with its own memory": {
			ret: nvml.SUCCESS, reported: 24 * gib,
			wantTotal: 24 * gib,
		},
		// The override describes hardware NVML cannot describe; a device that
		// can speak for itself always wins, so a stale config cannot resize a
		// normal node.
		"an override is ignored when the device reports a size": {
			ret: nvml.SUCCESS, reported: 24 * gib, overrideMB: 1024,
			wantTotal: 24 * gib,
		},
		"unified memory with an override": {
			ret: nvml.ERROR_NOT_SUPPORTED, overrideMB: 65536,
			wantTotal: 64 * gib, wantUnified: true,
		},
		"unified memory without an override": {
			ret:       nvml.ERROR_NOT_SUPPORTED,
			wantTotal: 0, wantUnified: true,
		},
		// Some drivers answer the call and report nothing rather than refusing
		// it; that is the same hardware.
		"a zero total is unified memory too": {
			ret: nvml.SUCCESS, reported: 0, overrideMB: 65536,
			wantTotal: 64 * gib, wantUnified: true,
		},
		"a zero total without an override": {
			ret: nvml.SUCCESS, reported: 0,
			wantTotal: 0, wantUnified: true,
		},
		// Anything else is a real failure and stays one: an override must not
		// paper over a broken driver.
		"another NVML error is still an error": {
			ret: nvml.ERROR_GPU_IS_LOST, overrideMB: 65536,
			wantErr: true,
		},
		"an uninitialised NVML is still an error": {
			ret:     nvml.ERROR_UNINITIALIZED,
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			total, unified, err := resolveDeviceMemory(tc.ret, tc.reported, tc.overrideMB)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantTotal, total)
			assert.Equal(t, tc.wantUnified, unified)
			// Logging the outcome must never panic, whatever the combination.
			logDeviceMemory(0, total, unified, tc.overrideMB)
		})
	}
}

// A negative override reaches the library through the monitor's flag, which is
// not validated the way the node config is, and must not wrap around.
func TestWithMemoryOverrideMBRejectsNonPositive(t *testing.T) {
	for _, mb := range []int{-1, 0} {
		lib := &DeviceLib{}
		WithMemoryOverrideMB(mb)(lib)
		assert.Zero(t, lib.memoryOverrideMB, "override %d", mb)
	}

	lib := &DeviceLib{}
	WithMemoryOverrideMB(65536)(lib)
	assert.Equal(t, uint64(65536), lib.memoryOverrideMB)
}
