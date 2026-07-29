package allocator

import (
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
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

// Test_resolveLinkDevices covers the cross-package join from
// gpuallocator.Device (returned by the topology selector) back to
// *device.Device (carrier of the allocatable accounting buildClaims
// needs). Picked entries that aren't in the store are dropped — a
// defensive guard since the selector is supposed to choose only from
// devices we passed in, but a stale entry shouldn't poison the whole
// claim list.
func Test_resolveLinkDevices(t *testing.T) {
	d0 := device.NewFakeDeviceWithUUID("uuid-0", 0, 0, 10, 0, 100, 0, 12000, 0)
	d1 := device.NewFakeDeviceWithUUID("uuid-1", 1, 0, 10, 0, 100, 0, 12000, 0)
	d2 := device.NewFakeDeviceWithUUID("uuid-2", 2, 0, 10, 0, 100, 0, 12000, 0)
	store := []*device.Device{d0, d1, d2}

	mkPicked := func(uuids ...string) []*gpuallocator.Device {
		out := make([]*gpuallocator.Device, len(uuids))
		for i, u := range uuids {
			out[i] = gpuallocator.NewDevice(i, u, "")
		}
		return out
	}

	t.Run("happy path: each picked UUID resolves, order preserved", func(t *testing.T) {
		got := resolveLinkDevices(mkPicked(d2.GetUUID(), d0.GetUUID()), store)
		assert.Equal(t, []*device.Device{d2, d0}, got)
	})

	t.Run("empty picked → empty result", func(t *testing.T) {
		assert.Empty(t, resolveLinkDevices(nil, store))
		assert.Empty(t, resolveLinkDevices([]*gpuallocator.Device{}, store))
	})

	t.Run("unknown UUID is silently skipped, known UUIDs still resolved", func(t *testing.T) {
		// The selector should never pick UUIDs we didn't supply, but if
		// it ever does we want graceful degradation over a panic / partial
		// build with nil entries. Known UUIDs come back; unknown ones drop.
		got := resolveLinkDevices(mkPicked(d0.GetUUID(), "ghost-uuid", d1.GetUUID()), store)
		assert.Equal(t, []*device.Device{d0, d1}, got)
	})
}

// Test_candidateSetScore makes sure the per-device-Score averaging used
// by link-topology + binpack/spread tie-breaking still returns a
// meaningful directional value after the single-pass-max refactor.
// Sets with no resolvable devices return 0 rather than NaN so the
// argmax in selectLinkCandidateByDevicePolicy stays well-defined.
func Test_candidateSetScore(t *testing.T) {
	// d0 has higher usage than d1 — binpack prefers d0 (more "warm").
	// UUIDs are explicit so the byUUID map keys actually differ; bare
	// NewFakeDevice leaves UUID as "" and collapses both into one entry.
	d0 := device.NewFakeDeviceWithUUID("uuid-warm", 0, 8, 10, 80, 100, 8000, 10000, 0) // 80% used
	d1 := device.NewFakeDeviceWithUUID("uuid-cold", 1, 2, 10, 20, 100, 2000, 10000, 0) // 20% used
	store := []*device.Device{d0, d1}

	byUUID := map[string]*device.Device{
		d0.GetUUID(): d0,
		d1.GetUUID(): d1,
	}
	setD0 := []*gpuallocator.Device{gpuallocator.NewDevice(0, d0.GetUUID(), "")}
	setD1 := []*gpuallocator.Device{gpuallocator.NewDevice(1, d1.GetUUID(), "")}

	t.Run("binpack: higher-used device gets the higher score", func(t *testing.T) {
		sD0 := candidateSetScore(setD0, byUUID, UniformProfile, util.BinpackPolicy)
		sD1 := candidateSetScore(setD1, byUUID, UniformProfile, util.BinpackPolicy)
		assert.Greater(t, sD0, sD1, "binpack: 80%%-used d0 should beat 20%%-used d1")
	})

	t.Run("spread: lower-used device gets the higher score", func(t *testing.T) {
		sD0 := candidateSetScore(setD0, byUUID, UniformProfile, util.SpreadPolicy)
		sD1 := candidateSetScore(setD1, byUUID, UniformProfile, util.SpreadPolicy)
		assert.Greater(t, sD1, sD0, "spread: 20%%-used d1 should beat 80%%-used d0")
	})

	t.Run("empty set → 0 score, not NaN", func(t *testing.T) {
		assert.Equal(t, 0.0, candidateSetScore(nil, byUUID, UniformProfile, util.BinpackPolicy))
	})

	t.Run("all UUIDs missing from byUUID → 0 score, not NaN", func(t *testing.T) {
		ghost := []*gpuallocator.Device{gpuallocator.NewDevice(0, "no-such-uuid", "")}
		assert.Equal(t, 0.0, candidateSetScore(ghost, byUUID, UniformProfile, util.BinpackPolicy))
	})

	// Cross-check that selectLinkCandidateByDevicePolicy's single-pass
	// argmax picks the same set the older O(n log n) sort would have.
	// Tie behaviour: ties resolve to lowest index, preserving the
	// link-score ordering established by AllocateLinkTopK.
	t.Run("single-pass argmax matches sorted-first behaviour", func(t *testing.T) {
		candidates := [][]*gpuallocator.Device{setD1, setD0} // d1 first, d0 second
		// binpack prefers warmer → d0 should win, picked over d1
		got := selectLinkCandidateByDevicePolicy(candidates, store, UniformProfile, util.BinpackPolicy)
		assert.Equal(t, setD0, got)

		// spread prefers colder → d1 wins
		got = selectLinkCandidateByDevicePolicy(candidates, store, UniformProfile, util.SpreadPolicy)
		assert.Equal(t, setD1, got)
	})

	t.Run("single-candidate fast path skips scoring entirely", func(t *testing.T) {
		got := selectLinkCandidateByDevicePolicy([][]*gpuallocator.Device{setD0}, store, UniformProfile, util.BinpackPolicy)
		assert.Equal(t, setD0, got)
	})
}
