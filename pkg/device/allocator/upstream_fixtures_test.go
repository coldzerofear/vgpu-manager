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

package allocator

// Fixtures transcribed MECHANICALLY from NVIDIA/go-gpuallocator's own
// gpuallocator/common_test.go (extracted, not retyped), so the link matrices
// are the vendor's rather than this project's reading of a topology dump.
//
// Two things make them stronger than the hand-built fixtures next door:
//
//   - they are the topologies NVIDIA validates its own allocator against, so
//     agreement here is agreement with the reference implementation's own
//     notion of a correct machine;
//   - every pair carries BOTH its NVLink and its PCIe edge, exactly as
//     nvidia-smi reports. PairScore sums them, so pair scores here are
//     materially different from a single-edge approximation — a class of
//     fidelity error the local fixtures could not have caught.
//
// The DGX-1 Volta NVLink matrix below matches the one transcribed by hand in
// tiered_test.go edge for edge, which independently confirms that reading.

import (
	"fmt"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upstreamRTX8000Node is 4x RTX 8000 — NVLink bridges on 0-3 and 1-2, PCIe elsewhere.
func upstreamRTX8000Node(t *testing.T, used ...int) (*device.NodeInfo, []*device.Device) {
	t.Helper()
	return fixtureNode("RTX8000", topoDevices(4, used...),
		device.FakeLink{A: 0, B: 3, Type: links.TwoNVLINKLinks},
		device.FakeLink{A: 1, B: 2, Type: links.TwoNVLINKLinks},
		device.FakeLink{A: 0, B: 1, Type: links.P2PLinkSameCPU},
		device.FakeLink{A: 0, B: 2, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 1, B: 3, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 2, B: 3, Type: links.P2PLinkSameCPU},
	), topoDevices(4, used...)
}

// upstreamDGX1PascalNode is DGX-1 Pascal — uniform single NVLinks in a hybrid cube mesh.
func upstreamDGX1PascalNode(t *testing.T, used ...int) (*device.NodeInfo, []*device.Device) {
	t.Helper()
	return fixtureNode("DGX1Pascal", topoDevices(8, used...),
		device.FakeLink{A: 0, B: 1, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 0, B: 2, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 0, B: 3, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 0, B: 4, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 1, B: 2, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 1, B: 3, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 1, B: 5, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 2, B: 3, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 2, B: 6, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 3, B: 7, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 4, B: 5, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 4, B: 6, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 4, B: 7, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 5, B: 6, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 5, B: 7, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 6, B: 7, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 0, B: 1, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 0, B: 2, Type: links.P2PLinkSingleSwitch},
		device.FakeLink{A: 0, B: 3, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 0, B: 4, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 0, B: 5, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 0, B: 6, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 0, B: 7, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 1, B: 2, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 1, B: 3, Type: links.P2PLinkSingleSwitch},
		device.FakeLink{A: 1, B: 4, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 1, B: 5, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 1, B: 6, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 1, B: 7, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 2, B: 3, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 2, B: 4, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 2, B: 5, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 2, B: 6, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 2, B: 7, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 3, B: 4, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 3, B: 5, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 3, B: 6, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 3, B: 7, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 4, B: 5, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 4, B: 6, Type: links.P2PLinkSingleSwitch},
		device.FakeLink{A: 4, B: 7, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 5, B: 6, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 5, B: 7, Type: links.P2PLinkSingleSwitch},
		device.FakeLink{A: 6, B: 7, Type: links.P2PLinkHostBridge},
	), topoDevices(8, used...)
}

// upstreamDGX1VoltaNode is DGX-1 Volta — hybrid cube mesh with mixed NV1/NV2 widths.
func upstreamDGX1VoltaNode(t *testing.T, used ...int) (*device.NodeInfo, []*device.Device) {
	t.Helper()
	return fixtureNode("DGX1Volta", topoDevices(8, used...),
		device.FakeLink{A: 0, B: 1, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 0, B: 2, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 0, B: 3, Type: links.TwoNVLINKLinks},
		device.FakeLink{A: 0, B: 4, Type: links.TwoNVLINKLinks},
		device.FakeLink{A: 1, B: 2, Type: links.TwoNVLINKLinks},
		device.FakeLink{A: 1, B: 3, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 1, B: 5, Type: links.TwoNVLINKLinks},
		device.FakeLink{A: 2, B: 3, Type: links.TwoNVLINKLinks},
		device.FakeLink{A: 2, B: 6, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 3, B: 7, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 4, B: 5, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 4, B: 6, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 4, B: 7, Type: links.TwoNVLINKLinks},
		device.FakeLink{A: 5, B: 6, Type: links.TwoNVLINKLinks},
		device.FakeLink{A: 5, B: 7, Type: links.SingleNVLINKLink},
		device.FakeLink{A: 6, B: 7, Type: links.TwoNVLINKLinks},
		device.FakeLink{A: 0, B: 1, Type: links.P2PLinkSingleSwitch},
		device.FakeLink{A: 0, B: 2, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 0, B: 3, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 0, B: 4, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 0, B: 5, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 0, B: 6, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 0, B: 7, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 1, B: 2, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 1, B: 3, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 1, B: 4, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 1, B: 5, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 1, B: 6, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 1, B: 7, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 2, B: 3, Type: links.P2PLinkSingleSwitch},
		device.FakeLink{A: 2, B: 4, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 2, B: 5, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 2, B: 6, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 2, B: 7, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 3, B: 4, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 3, B: 5, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 3, B: 6, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 3, B: 7, Type: links.P2PLinkCrossCPU},
		device.FakeLink{A: 4, B: 5, Type: links.P2PLinkSingleSwitch},
		device.FakeLink{A: 4, B: 6, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 4, B: 7, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 5, B: 6, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 5, B: 7, Type: links.P2PLinkHostBridge},
		device.FakeLink{A: 6, B: 7, Type: links.P2PLinkSingleSwitch},
	), topoDevices(8, used...)
}

// Test_Upstream_MatchesHandTranscribedDGX1 checks the local DGX-1 Volta fixture
// against NVIDIA's, edge for edge on the NVLink layer.
//
// The exactness of that hand transcription has been the single largest open
// risk in this work: the tier walk's behaviour on non-uniform hardware is
// validated almost entirely through it. Comparing against the vendor's own
// matrix retires that risk without needing physical hardware.
func Test_Upstream_MatchesHandTranscribedDGX1(t *testing.T) {
	up, _ := upstreamDGX1VoltaNode(t)
	local, _ := dgx1Node(t)

	for i := 0; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			a, b := fmt.Sprintf("GPU-%d", i), fmt.Sprintf("GPU-%d", j)
			assert.Equal(t,
				local.ConnectedAtTier(a, b, device.TierNVLink),
				up.ConnectedAtTier(a, b, device.TierNVLink),
				"NVLink edge %s-%s disagrees with NVIDIA's fixture", a, b)
		}
	}
	// The defining structural property must survive on the vendor matrix too:
	// the tier ladder cannot separate a hybrid cube mesh, which is exactly why
	// the in-component search has to exist.
	assert.Equal(t, 8, up.LinkTierMaxComponentSize(device.TierNVLink),
		"all 8 GPUs are one NVLink component")
	assert.False(t, up.LinkTierIsUniform(device.TierNVLink),
		"mixed NV1/NV2 widths must read as non-uniform")
}

// Test_Upstream_CompareAgainstOldAlgorithm is the headline check: on the
// vendor's own topologies, at every request size and occupancy, the tiered
// selector must never return a worse-connected set than the algorithm it
// replaces.
func Test_Upstream_CompareAgainstOldAlgorithm(t *testing.T) {
	builders := []struct {
		name  string
		build func(*testing.T, ...int) (*device.NodeInfo, []*device.Device)
		size  int
	}{
		{"4xRTX8000", upstreamRTX8000Node, 4},
		{"DGX1Pascal", upstreamDGX1PascalNode, 8},
		{"DGX1Volta", upstreamDGX1VoltaNode, 8},
	}
	overall := &comparison{}
	for _, b := range builders {
		per := &comparison{}
		for _, used := range [][]int{{}, {0}, {1, 5}, {0, 3, 6}, {2, 4}} {
			for need := 2; need <= b.size; need++ {
				n, devs := b.build(t, used...)
				store := make([]*device.Device, 0, len(devs))
				for _, d := range devs {
					if d.AllocatableNumber() > 0 {
						store = append(store, d)
					}
				}
				if len(store) < need {
					continue
				}
				newPick := newAlgorithmPick(n, store, need, "")
				require.NotNil(t, newPick, "%s used=%v need=%d: must place", b.name, used, need)

				if oldAlgorithmReturnsPadding(n, store, need) {
					// The reference implementation's own defect; nothing to
					// compare against, and the tiered selector placed anyway.
					continue
				}
				oldPick := oldAlgorithmPick(n, store, need)
				if oldPick == nil {
					continue
				}
				label := fmt.Sprintf("%s used=%v need=%d", b.name, used, need)
				per.record(t, label, setScoreOf(n, oldPick), setScoreOf(n, newPick))
				overall.record(t, label, setScoreOf(n, oldPick), setScoreOf(n, newPick))
			}
		}
		per.report(t, "upstream "+b.name)
		assert.Zero(t, per.worse,
			"%s: must never lose link quality against the reference implementation", b.name)
	}
	overall.report(t, "upstream TOTAL")
	assert.Zero(t, overall.worse)
}

// Test_Upstream_StrictStaysHonest: link-strict must accept exactly the sets
// that really are NVLink-connected on these machines, and reject the rest.
func Test_Upstream_StrictStaysHonest(t *testing.T) {
	t.Run("RTX8000 bridges hold 2 but not 3", func(t *testing.T) {
		n, _ := upstreamRTX8000Node(t)
		// 0-3 and 1-2 are the NVLink pairs; nothing links three of them.
		assert.Equal(t, 2, n.LinkTierMaxComponentSize(device.TierNVLink))

		got := allocUUIDs(t, n, linkPod(2, true, ""))
		require.Len(t, got, 2)
		pair := got[0] + "|" + got[1]
		assert.Contains(t, []string{"GPU-0|GPU-3", "GPU-1|GPU-2"}, pair,
			"strict must land on an actual bridge, got %v", got)

		req := BuildAllocationRequest(linkPod(3, true, ""))
		_, rsn, err := NewAllocator(n, nil).Allocate(req)
		require.NoError(t, err)
		require.NotNil(t, rsn, "no NVLink component holds 3 cards")
	})

	t.Run("DGX-1 holds all 8 at NVLink tier", func(t *testing.T) {
		for _, b := range []func(*testing.T, ...int) (*device.NodeInfo, []*device.Device){
			upstreamDGX1PascalNode, upstreamDGX1VoltaNode,
		} {
			for _, need := range []int64{2, 4, 8} {
				// A FRESH node per request: each card has a single vGPU slot,
				// so reusing one would have the previous allocation consume it.
				n, _ := b(t)
				got := allocUUIDs(t, n, linkPod(need, true, ""))
				assert.Len(t, got, int(need),
					"a hybrid cube mesh is NVLink-connected throughout")
			}
		}
	})
}
