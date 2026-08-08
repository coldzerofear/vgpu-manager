package allocator

import (
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
)

// Test_buildClaims covers the central claim-building helper that
// replaced the two near-duplicate allocateBy* functions. Most
// important: the implicit-full-memory rule (needMemory == 0 →
// device's whole card memory) MUST be applied — the pre-cleanup
// code had a copy-paste bug in the link-topology path that wrote
// Memory=0 instead of the resolved value, leaving pods that omit
// vgpu-memory with claims that didn't reserve any memory.
func Test_buildClaims(t *testing.T) {
	// d0: 12 GB card, d1: 24 GB card. NewFakeDeviceWithUUID so the
	// "Id and UUID come from the picked device" subtest can prove
	// the per-device routing isn't accidentally falling back to
	// slice-index lookups.
	d0 := device.NewFakeDeviceWithUUID("uuid-0", 0, 0, 10, 0, 100, 0, 12000, 0)
	d1 := device.NewFakeDeviceWithUUID("uuid-1", 1, 0, 10, 0, 100, 0, 24000, 0)

	t.Run("implicit-full memory: needMemory==0 expands to each device's total", func(t *testing.T) {
		// REGRESSION GUARD: this is the exact case the old
		// allocateByDevices got wrong — Memory ended up as needMemory
		// (0) instead of reqMemory (totalMemory). Heterogeneous cards
		// in the same slice must each get their own card capacity.
		claims := buildClaims([]*device.Device{d0, d1}, 50, 0)
		assert.Len(t, claims, 2)
		assert.Equal(t, int64(12000), claims[0].Memory, "d0 expands to its total")
		assert.Equal(t, int64(24000), claims[1].Memory, "d1 expands to its total (different card size)")
		assert.Equal(t, int64(50), claims[0].Cores)
		assert.Equal(t, int64(50), claims[1].Cores)
	})

	t.Run("explicit memory: same value written to every claim", func(t *testing.T) {
		claims := buildClaims([]*device.Device{d0, d1}, 25, 4096)
		assert.Equal(t, int64(4096), claims[0].Memory)
		assert.Equal(t, int64(4096), claims[1].Memory)
	})

	t.Run("zero devices → empty claims", func(t *testing.T) {
		assert.Empty(t, buildClaims(nil, 50, 1000))
		assert.Empty(t, buildClaims([]*device.Device{}, 50, 1000))
	})

	t.Run("Id and UUID come from the picked device, not the index in the input", func(t *testing.T) {
		// Pass devices out of id-order to confirm we don't accidentally
		// fall back to slice index.
		claims := buildClaims([]*device.Device{d1, d0}, 0, 100)
		assert.Equal(t, d1.GetID(), claims[0].Id)
		assert.Equal(t, d0.GetID(), claims[1].Id)
		assert.Equal(t, d1.GetUUID(), claims[0].Uuid)
		assert.Equal(t, d0.GetUUID(), claims[1].Uuid)
	})
}

// Test_resolveContainerNeeds locks down the implicit-fill rules that
// turn user-typed (cores, memory) into the values allocateOne actually
// reserves against device.AllocatableX. These rules are duplicated in
// concept across profile.go (weight derivation) and the per-container
// allocation; centralising the resolution here means any future
// rule change touches exactly one place.
func Test_resolveContainerNeeds(t *testing.T) {
	testCases := []struct {
		name                  string
		need                  ContainerNeed
		factor                int
		wantCores, wantMemory int64
	}{
		{
			name:      "implicit-everything → full cores, mem stays 0 for buildClaims to expand",
			need:      ContainerNeed{Number: 1},
			factor:    1024,
			wantCores: util.HundredCore, wantMemory: 0,
		},
		{
			name:      "explicit cores only → cores kept, mem stays 0 (implicit-full)",
			need:      ContainerNeed{Number: 1, Cores: 50},
			factor:    1024,
			wantCores: 50, wantMemory: 0,
		},
		{
			name:      "explicit memory only → mem * factor, cores stays 0 (memory-only pod)",
			need:      ContainerNeed{Number: 1, Memory: 4},
			factor:    1024,
			wantCores: 0, wantMemory: 4096,
		},
		{
			name:      "both explicit → both kept, mem * factor",
			need:      ContainerNeed{Number: 2, Cores: 50, Memory: 8},
			factor:    1024,
			wantCores: 50, wantMemory: 8192,
		},
		{
			name:      "memory typed but factor=0 → no multiplication, raw value through",
			need:      ContainerNeed{Number: 1, Memory: 4096},
			factor:    0,
			wantCores: 0, wantMemory: 4096,
		},
		{
			name:      "explicit cores=0 AND memory=0 → full-cores promotion",
			need:      ContainerNeed{Number: 1, Cores: 0, Memory: 0},
			factor:    1024,
			wantCores: util.HundredCore, wantMemory: 0,
		},
		{
			name:      "explicit cores=100 (full) + explicit memory → both kept, no promotion fires",
			need:      ContainerNeed{Number: 1, Cores: 100, Memory: 12},
			factor:    1024,
			wantCores: 100, wantMemory: 12 * 1024,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotCores, gotMemory := resolveContainerNeeds(tc.need, tc.factor, false, 0)
			assert.Equal(t, tc.wantCores, gotCores, "cores")
			assert.Equal(t, tc.wantMemory, gotMemory, "memory")
		})
	}
}
