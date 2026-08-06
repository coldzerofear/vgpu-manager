package allocator

import (
	"fmt"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Machine-type fixtures
//
// These reproduce REAL `nvidia-smi topo -m` matrices. They are the only thing
// standing between this allocator and a silent regression, because the tier
// walk's correctness depends entirely on what the link matrix actually looks
// like on each machine class — see docs/link_topology_tiered_allocation_design.md §3.
// ---------------------------------------------------------------------------

func topoDevices(n int, used ...int) []*device.Device {
	usedSet := make(map[int]bool, len(used))
	for _, u := range used {
		usedSet[u] = true
	}
	devs := make([]*device.Device, n)
	for i := 0; i < n; i++ {
		usedNum := 0
		var usedCore, usedMem int64
		if usedSet[i] {
			usedNum, usedCore, usedMem = 1, 100, 1024
		}
		devs[i] = device.NewFakeDeviceWithUUID(fmt.Sprintf("GPU-%d", i), i,
			usedNum, 1, usedCore, 100, usedMem, 1024, -1)
	}
	return devs
}

func fixtureNode(name string, devs []*device.Device, fl ...device.FakeLink) *device.NodeInfo {
	return device.NewFakeNodeInfoWithLinks(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, devs, fl...)
}

// nvswitchNode is a DGX H100 / HGX A100 class board: every GPU pair NV18.
func nvswitchNode(t *testing.T, used ...int) (*device.NodeInfo, []*device.Device) {
	t.Helper()
	devs := topoDevices(8, used...)
	var fl []device.FakeLink
	for i := 0; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			fl = append(fl, device.FakeLink{A: i, B: j, Type: links.EighteenNVLINKLinks})
		}
	}
	return fixtureNode("nvswitch", devs, fl...), devs
}

// dgx1Node reproduces the DGX-1 V100 hybrid cube mesh EXACTLY as reported by
// `nvidia-smi topo -m`:
//
//	NV1: 0-1 0-2 1-3 2-6 3-7 4-5 4-6 5-7
//	NV2: 0-3 0-4 1-2 1-5 2-3 4-7 5-6 6-7
//	everything else: SYS
//
// The defining property (verified in the design doc): union-find at the NV2
// threshold still leaves all 8 GPUs in ONE component, so the tier ladder cannot
// separate this machine and the in-component search is genuinely required.
func dgx1Node(t *testing.T, used ...int) (*device.NodeInfo, []*device.Device) {
	t.Helper()
	devs := topoDevices(8, used...)
	nv1 := [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 6}, {3, 7}, {4, 5}, {4, 6}, {5, 7}}
	nv2 := [][2]int{{0, 3}, {0, 4}, {1, 2}, {1, 5}, {2, 3}, {4, 7}, {5, 6}, {6, 7}}
	linked := map[[2]int]bool{}
	var fl []device.FakeLink
	for _, p := range nv1 {
		fl = append(fl, device.FakeLink{A: p[0], B: p[1], Type: links.SingleNVLINKLink})
		linked[p] = true
	}
	for _, p := range nv2 {
		fl = append(fl, device.FakeLink{A: p[0], B: p[1], Type: links.TwoNVLINKLinks})
		linked[p] = true
	}
	// All remaining pairs are SYS (cross-socket PCIe).
	for i := 0; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			if !linked[[2]int{i, j}] {
				fl = append(fl, device.FakeLink{A: i, B: j, Type: links.P2PLinkCrossCPU})
			}
		}
	}
	return fixtureNode("dgx1", devs, fl...), devs
}

// bridgeNode is an 8x PCIe board with NVLink bridges on adjacent pairs: bridged
// pairs report NV12, the rest of a socket half is PXB, across halves is SYS.
func bridgeNode(t *testing.T, used ...int) (*device.NodeInfo, []*device.Device) {
	t.Helper()
	devs := topoDevices(8, used...)
	var fl []device.FakeLink
	bridged := map[[2]int]bool{{0, 1}: true, {2, 3}: true, {4, 5}: true, {6, 7}: true}
	for i := 0; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			switch {
			case bridged[[2]int{i, j}]:
				fl = append(fl, device.FakeLink{A: i, B: j, Type: links.TwelveNVLINKLinks})
			case i/4 == j/4: // same socket half
				fl = append(fl, device.FakeLink{A: i, B: j, Type: links.P2PLinkMultiSwitch})
			default:
				fl = append(fl, device.FakeLink{A: i, B: j, Type: links.P2PLinkCrossCPU})
			}
		}
	}
	return fixtureNode("bridge", devs, fl...), devs
}

// pcieNode has no NVLink at all: PIX inside a switch pair, PXB across switches
// in a socket half, SYS across halves. The tier ladder is a genuine tree here.
func pcieNode(t *testing.T, used ...int) (*device.NodeInfo, []*device.Device) {
	t.Helper()
	devs := topoDevices(8, used...)
	var fl []device.FakeLink
	sameSwitch := map[[2]int]bool{{0, 1}: true, {2, 3}: true, {4, 5}: true, {6, 7}: true}
	for i := 0; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			switch {
			case sameSwitch[[2]int{i, j}]:
				fl = append(fl, device.FakeLink{A: i, B: j, Type: links.P2PLinkSingleSwitch})
			case i/4 == j/4:
				fl = append(fl, device.FakeLink{A: i, B: j, Type: links.P2PLinkMultiSwitch})
			default:
				fl = append(fl, device.FakeLink{A: i, B: j, Type: links.P2PLinkCrossCPU})
			}
		}
	}
	return fixtureNode("pcie", devs, fl...), devs
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func linkPod(number int64, strict bool, policy string) *corev1.Pod {
	mode := string(util.LinkTopology)
	if strict {
		mode = string(util.LinkTopologyStrict)
	}
	anns := map[string]string{util.DeviceTopologyModeAnnotation: mode}
	if policy != "" {
		anns[util.DeviceSchedulerPolicyAnnotation] = policy
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Annotations: anns},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{vgpuContainer("c", number, 100, 1024)}},
	}
}

// allocUUIDs runs a full allocation and returns the chosen UUIDs.
func allocUUIDs(t *testing.T, n *device.NodeInfo, pod *corev1.Pod) []string {
	t.Helper()
	claim := runAllocate(t, n, pod)
	require.Len(t, claim, 1)
	out := make([]string, 0, len(claim[0].DeviceClaims))
	for _, dc := range claim[0].DeviceClaims {
		out = append(out, dc.Uuid)
	}
	return out
}

// ---------------------------------------------------------------------------
// NVSwitch — the compatibility guarantee
// ---------------------------------------------------------------------------

func Test_NVSwitch_IsUniform(t *testing.T) {
	n, _ := nvswitchNode(t)
	assert.True(t, n.LinkTierIsUniform(device.TierNVLink),
		"an all-to-all equal-width fabric must be detected as uniform — this is what keeps large nodes free of combinatorial search")
	assert.Equal(t, 8, n.LinkTierMaxComponentSize(device.TierNVLink))
}

// Test_NVSwitch_ZeroBehaviourChange pins the headline compatibility property:
// on an NVSwitch board the tiered allocator returns exactly deviceStore[:N],
// which is what the previous bestEffort partition search also returned (all
// partitions score identically on a uniform fabric, so it kept the first one).
// This must hold for EVERY device policy.
func Test_NVSwitch_ZeroBehaviourChange(t *testing.T) {
	// Two cards are pre-used so binpack/spread actually differentiate; that
	// leaves 6 free, so requests stay at or below 6.
	const preUsedA, preUsedB = 1, 2
	for _, policy := range []string{"", "binpack", "spread"} {
		for _, need := range []int64{2, 4, 6} {
			t.Run(fmt.Sprintf("policy=%q need=%d", policy, need), func(t *testing.T) {
				n, _ := nvswitchNode(t, preUsedA, preUsedB)
				got := allocUUIDs(t, n, linkPod(need, false, policy))

				// Expectation is computed on a SEPARATE fixture: allocation
				// mutates device usage in place, so reusing the allocator's own
				// devices here would compare against post-allocation state.
				_, fresh := nvswitchNode(t, preUsedA, preUsedB)
				req := BuildAllocationRequest(linkPod(need, false, policy))
				free := make([]*device.Device, 0, len(fresh))
				for _, d := range fresh {
					if d.AllocatableNumber() > 0 { // as filterDevices would
						free = append(free, d)
					}
				}
				require.GreaterOrEqual(t, len(free), int(need))
				NewDevicePolicyPriority(*req).Sort(free)
				want := make([]string, 0, need)
				for _, d := range free[:need] {
					want = append(want, d.GetUUID())
				}
				assert.ElementsMatch(t, want, got,
					"uniform fabric must yield the policy-ordered prefix")
			})
		}
	}
}

// ---------------------------------------------------------------------------
// DGX-1 — the case that needs search
// ---------------------------------------------------------------------------

func Test_DGX1_LadderCannotSeparate(t *testing.T) {
	n, _ := dgx1Node(t)
	// The defining property: even the NV2 threshold leaves one component.
	assert.Equal(t, 8, n.LinkTierMaxComponentSize(device.TierNVLink),
		"DGX-1 hybrid cube mesh is fully connected over NVLink")
	assert.False(t, n.LinkTierIsUniform(device.TierNVLink),
		"mixed NV1/NV2 widths must be detected as non-uniform, otherwise selection would skip the search it needs")
}

func Test_DGX1_PicksBestQuad(t *testing.T) {
	n, _ := dgx1Node(t)
	got := allocUUIDs(t, n, linkPod(4, false, ""))
	// {0,1,2,3} scores 900 (3xNV2 + 3xNV1); any cross-socket mix drags in
	// SYS pairs and scores far lower ({0,3,4,7} = 720). {4,5,6,7} ties at 900
	// and the tie resolves to the lower deviceStore order.
	assert.ElementsMatch(t, []string{"GPU-0", "GPU-1", "GPU-2", "GPU-3"}, got)
}

func Test_DGX1_PicksDualLinkPair(t *testing.T) {
	n, _ := dgx1Node(t)
	got := allocUUIDs(t, n, linkPod(2, false, ""))
	require.Len(t, got, 2)
	// Must be one of the NV2 (dual-link, score 200) pairs, not an NV1 pair.
	nv2 := map[string]bool{
		"GPU-0|GPU-3": true, "GPU-0|GPU-4": true, "GPU-1|GPU-2": true, "GPU-1|GPU-5": true,
		"GPU-2|GPU-3": true, "GPU-4|GPU-7": true, "GPU-5|GPU-6": true, "GPU-6|GPU-7": true,
	}
	key := got[0] + "|" + got[1]
	assert.True(t, nv2[key], "expected a dual-NVLink pair, got %v", got)
}

// ---------------------------------------------------------------------------
// NVLink bridge — ladder separates it, no search needed
// ---------------------------------------------------------------------------

func Test_Bridge_PicksBridgedPair(t *testing.T) {
	n, _ := bridgeNode(t)
	assert.Equal(t, 2, n.LinkTierMaxComponentSize(device.TierNVLink),
		"bridges give 2-card NVLink components")
	got := allocUUIDs(t, n, linkPod(2, true, ""))
	require.Len(t, got, 2)
	bridged := map[string]bool{"GPU-0|GPU-1": true, "GPU-2|GPU-3": true, "GPU-4|GPU-5": true, "GPU-6|GPU-7": true}
	assert.True(t, bridged[got[0]+"|"+got[1]], "expected a bridged pair, got %v", got)
}

func Test_Bridge_FourCardsFallsBelowNVLink(t *testing.T) {
	n, _ := bridgeNode(t)
	// No NVLink component holds 4, so link-strict must reject.
	req := BuildAllocationRequest(linkPod(4, true, ""))
	_, rsn, err := NewAllocator(n, nil).Allocate(req)
	require.NoError(t, err)
	require.NotNil(t, rsn, "4 cards cannot be NVLink-connected on a bridged board")
	assert.Equal(t, reason.LinkTopologyUnsatisfied, rsn.Primary)

	// Non-strict places the pod anyway, within one socket half (switch tier).
	got := allocUUIDs(t, n, linkPod(4, false, ""))
	require.Len(t, got, 4)
	assert.ElementsMatch(t, []string{"GPU-0", "GPU-1", "GPU-2", "GPU-3"}, got,
		"should stay inside one PCIe switch fabric rather than span sockets")
}

// ---------------------------------------------------------------------------
// Pure PCIe — the tree case
// ---------------------------------------------------------------------------

func Test_PCIe_PrefersSameSwitchPair(t *testing.T) {
	n, _ := pcieNode(t)
	assert.Equal(t, 1, n.LinkTierMaxComponentSize(device.TierNVLink),
		"no NVLink at all → every card is its own NVLink singleton")
	got := allocUUIDs(t, n, linkPod(2, false, ""))
	require.Len(t, got, 2)
	sameSwitch := map[string]bool{"GPU-0|GPU-1": true, "GPU-2|GPU-3": true, "GPU-4|GPU-5": true, "GPU-6|GPU-7": true}
	assert.True(t, sameSwitch[got[0]+"|"+got[1]],
		"must prefer a same-PCIe-switch (PIX) pair over a cross-switch (PXB) one, got %v", got)
}

func Test_PCIe_StrictRejects(t *testing.T) {
	n, _ := pcieNode(t)
	// link-strict demands NVLink connectivity; a PCIe-only board can never
	// provide it. This is unchanged from the previous implementation.
	req := BuildAllocationRequest(linkPod(2, true, ""))
	_, rsn, err := NewAllocator(n, nil).Allocate(req)
	require.NoError(t, err)
	require.NotNil(t, rsn)
	assert.Equal(t, reason.LinkTopologyUnsatisfied, rsn.Primary)
}

func Test_PCIe_StaysInSocket(t *testing.T) {
	n, _ := pcieNode(t)
	got := allocUUIDs(t, n, linkPod(4, false, ""))
	assert.ElementsMatch(t, []string{"GPU-0", "GPU-1", "GPU-2", "GPU-3"}, got,
		"4 cards must stay within one socket half rather than span SYS")
}

// ---------------------------------------------------------------------------
// Invariants
// ---------------------------------------------------------------------------

// Test_Invariants_AcrossFixtures asserts I1 (deviceStore untouched) and I2
// (result is an order-preserving subsequence) for every machine type and every
// device policy. I2 is what makes "topology and binpack/spread are orthogonal"
// a structural property rather than something to be argued about.
func Test_Invariants_AcrossFixtures(t *testing.T) {
	fixtures := map[string]func(*testing.T, ...int) (*device.NodeInfo, []*device.Device){
		"nvswitch": nvswitchNode,
		"dgx1":     dgx1Node,
		"bridge":   bridgeNode,
		"pcie":     pcieNode,
	}
	for name, build := range fixtures {
		for _, policy := range []string{"", "binpack", "spread"} {
			for _, need := range []int{2, 4} {
				t.Run(fmt.Sprintf("%s/%q/%d", name, policy, need), func(t *testing.T) {
					n, devs := build(t, 1, 5)
					alloc := NewAllocator(n, nil)
					req := BuildAllocationRequest(linkPod(int64(need), false, policy))

					store := make([]*device.Device, 0, len(devs))
					for _, d := range devs {
						if d.AllocatableNumber() > 0 {
							store = append(store, d)
						}
					}
					NewDevicePolicyPriority(*req).Sort(store)
					before := append([]*device.Device(nil), store...)

					plan := alloc.allocateTiered(req, store, need, nil)
					require.NotNil(t, plan, "every fixture can host %d cards at some tier", need)
					require.Len(t, plan.Devices, need)

					// I1: the caller's slice is untouched, so the fallback path
					// still sees its policy ordering.
					assert.Equal(t, before, store, "I1: deviceStore must not be reordered")

					// I2: the result appears in deviceStore order.
					pos := map[string]int{}
					for i, d := range store {
						pos[d.GetUUID()] = i
					}
					last := -1
					for _, d := range plan.Devices {
						at, ok := pos[d.GetUUID()]
						require.True(t, ok, "I2: picked a device outside deviceStore")
						assert.Greater(t, at, last, "I2: result must be an order-preserving subsequence")
						last = at
					}
				})
			}
		}
	}
}

// Test_NoTopologyNode_Unchanged: a node without link topology must behave
// exactly as before — link mode fails, strict rejects, non-strict falls back.
func Test_NoTopologyNode_Unchanged(t *testing.T) {
	n, _ := fakeNode(4, 1, 100, 1024) // built without links

	req := BuildAllocationRequest(linkPod(2, true, ""))
	_, rsn, err := NewAllocator(n, nil).Allocate(req)
	require.NoError(t, err)
	require.NotNil(t, rsn, "strict must reject a node with no topology data")

	n, _ = fakeNode(4, 1, 100, 1024)
	got := allocUUIDs(t, n, linkPod(2, false, ""))
	assert.Len(t, got, 2, "non-strict must still place the pod")
}

// Test_PolicyRuns covers the run-splitting that makes "policy first, link
// quality as the tie-break" work — in particular that NonePolicy collapses to a
// SINGLE run so link quality decides everything, which is the historical
// behaviour and cannot be derived from the comparator chain.
func Test_PolicyRuns(t *testing.T) {
	devs := []*device.Device{
		device.NewFakeDeviceWithUUID("a", 0, 0, 10, 0, 100, 0, 1024, -1),    // free
		device.NewFakeDeviceWithUUID("b", 1, 0, 10, 0, 100, 0, 1024, -1),    // free
		device.NewFakeDeviceWithUUID("c", 2, 5, 10, 50, 100, 512, 1024, -1), // half used
	}

	t.Run("none collapses to one run", func(t *testing.T) {
		req := BuildAllocationRequest(linkPod(2, false, ""))
		runs := policyRuns(req, devs)
		require.Len(t, runs, 1, "NonePolicy means no preference → one run")
		assert.Len(t, runs[0], 3)
	})

	t.Run("binpack splits by equal policy score", func(t *testing.T) {
		req := BuildAllocationRequest(linkPod(2, false, "binpack"))
		// deviceStore order matters: sort first, as the allocator does.
		store := append([]*device.Device(nil), devs...)
		NewDevicePolicyPriority(*req).Sort(store)
		runs := policyRuns(req, store)
		require.Len(t, runs, 2, "one run for the half-used card, one for the two free ones")
		assert.Len(t, runs[0], 1)
		assert.Len(t, runs[1], 2)
	})

	t.Run("all-equal utilisation is one run under binpack too", func(t *testing.T) {
		// The fresh-node case: every card identical, so the policy has nothing
		// to say and link quality must decide.
		free := devs[:2]
		req := BuildAllocationRequest(linkPod(2, false, "binpack"))
		runs := policyRuns(req, free)
		require.Len(t, runs, 1)
	})
}

func Test_CombinationCount(t *testing.T) {
	assert.Equal(t, 70, combinationCount(8, 4))
	assert.Equal(t, 28, combinationCount(8, 2))
	assert.Equal(t, 1, combinationCount(8, 8))
	assert.Equal(t, 0, combinationCount(4, 5))
	// Saturates rather than overflowing on the way to the budget comparison.
	assert.Greater(t, combinationCount(64, 32), maxCombinationSearch)
}

func Test_ForEachCombination(t *testing.T) {
	var got [][]int
	forEachCombination(4, 2, func(idx []int) {
		got = append(got, append([]int(nil), idx...))
	})
	assert.Equal(t, [][]int{{0, 1}, {0, 2}, {0, 3}, {1, 2}, {1, 3}, {2, 3}}, got,
		"must enumerate every 2-subset exactly once, in lexicographic order")
}

// ---------------------------------------------------------------------------
// Single-card path and cross-pod rail alignment (Step 3)
// ---------------------------------------------------------------------------

// Test_SingleCard_EntersTopologyPath: removing the needNumber<=1 short-circuit
// must not change where a plain single-card pod lands, and must not emit a
// TopologyFallback event (there is nothing to fall back from).
func Test_SingleCard_EntersTopologyPath(t *testing.T) {
	for _, name := range []string{"nvswitch", "dgx1", "pcie"} {
		t.Run(name, func(t *testing.T) {
			var n *device.NodeInfo
			switch name {
			case "nvswitch":
				n, _ = nvswitchNode(t)
			case "dgx1":
				n, _ = dgx1Node(t)
			default:
				n, _ = pcieNode(t)
			}
			got := allocUUIDs(t, n, linkPod(1, false, ""))
			assert.Equal(t, []string{"GPU-0"}, got,
				"a single card must still take the deviceStore head")
		})
	}
}

// Test_SingleCard_StrictNotRejected: link-strict on a PCIe-only node rejects
// MULTI-card requests (no NVLink), but a single card is trivially connected and
// must still be placed.
func Test_SingleCard_StrictNotRejected(t *testing.T) {
	n, _ := pcieNode(t)
	got := allocUUIDs(t, n, linkPod(1, true, ""))
	assert.Len(t, got, 1, "one card is trivially NVLink-connected")
}

// Test_SingleCard_RailAlignment is the gap this step exists to close: on a
// fully connected node the NVLink component signature is identical everywhere,
// so component alignment cannot steer a 1-GPU-per-pod gang. The per-GPU rail
// key can.
func Test_SingleCard_RailAlignment(t *testing.T) {
	n, _ := nvswitchNode(t)
	require.Equal(t, 8, n.LinkTierMaxComponentSize(device.TierNVLink),
		"premise: one component, so the component signature is useless here")

	// Sibling landed on rail 3 (no rail map published → device index is the key).
	pod := linkPod(1, false, "")
	pod.Annotations[util.CrossPodTopologyAnnotation] = "true"
	pod.Labels = map[string]string{util.CoschedulingPodGroupLabel: "gangX"}
	req := BuildAllocationRequest(pod)
	req.GangRailKey = "idx:3"

	newPod, rsn, err := NewAllocator(n, nil).Allocate(req)
	require.NoError(t, err)
	require.Nil(t, rsn)
	pre, ok := util.HasAnnotation(newPod, util.PodVGPUPreAllocAnnotation)
	require.True(t, ok)
	claim := device.PodDeviceClaim{}
	require.NoError(t, claim.UnmarshalText(pre))
	require.Len(t, claim[0].DeviceClaims, 1)
	assert.Equal(t, "GPU-3", claim[0].DeviceClaims[0].Uuid,
		"single card must align to the sibling's rail, not take the deviceStore head")
}

// Test_RailAlignment_DegradesNotFails: an unsatisfiable rail window must widen
// rather than make the pod unschedulable.
func Test_RailAlignment_DegradesNotFails(t *testing.T) {
	// GPU-3 is fully consumed, so the requested rail is unavailable.
	n, _ := nvswitchNode(t, 3)
	pod := linkPod(1, false, "")
	pod.Annotations[util.CrossPodTopologyAnnotation] = "true"
	req := BuildAllocationRequest(pod)
	req.GangRailKey = "idx:3"

	newPod, rsn, err := NewAllocator(n, nil).Allocate(req)
	require.NoError(t, err)
	require.Nil(t, rsn, "an over-constrained alignment must degrade, never reject")
	pre, _ := util.HasAnnotation(newPod, util.PodVGPUPreAllocAnnotation)
	claim := device.PodDeviceClaim{}
	require.NoError(t, claim.UnmarshalText(pre))
	assert.Equal(t, "GPU-0", claim[0].DeviceClaims[0].Uuid)
}

func Test_RailSignature_PrefersPublishedRailOverIndex(t *testing.T) {
	n, _ := nvswitchNode(t)
	sig, ok := n.RailSignatureOfUUIDs([]string{"GPU-5", "GPU-2"})
	require.True(t, ok)
	assert.Equal(t, "idx:2,idx:5", sig, "sorted and de-duplicated, index fallback")

	matched := n.UUIDsMatchingRailSignature(sig)
	assert.ElementsMatch(t, []string{"GPU-2", "GPU-5"}, matched)

	assert.Empty(t, n.UUIDsMatchingRailSignature(""))
	assert.Empty(t, n.UUIDsMatchingRailSignature("rail:nope"))
}
