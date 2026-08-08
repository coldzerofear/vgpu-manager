package allocator

import (
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func setOf(uuids ...string) map[string]bool {
	m := make(map[string]bool, len(uuids))
	for _, u := range uuids {
		m[u] = true
	}
	return m
}

func withPolicy(pod *corev1.Pod, p util.SchedulerPolicy) *corev1.Pod {
	pod.Annotations[util.DeviceSchedulerPolicyAnnotation] = string(p)
	return pod
}

// assertExactSet fails unless got has exactly the UUIDs in want.
func assertExactSet(t *testing.T, got map[string]struct{}, want map[string]bool) {
	t.Helper()
	require.Len(t, got, len(want))
	for u := range got {
		require.Truef(t, want[u], "unexpected card %s (want %v)", u, want)
	}
}

// Test_CrossPod_SameNodeIntraIsland: podA holds island A cards GPU-0,GPU-1 (a,b).
// podB (same gang, cross-pod) must be ANCHORED into island A {GPU-0..3}; which of
// the four it picks depends on request size, free capacity, and device policy.
func Test_CrossPod_SameNodeIntraIsland(t *testing.T) {
	islandA := setOf("GPU-0", "GPU-1", "GPU-2", "GPU-3") // a,b,c,d
	ab := setOf("GPU-0", "GPU-1")                        // podA's cards
	cd := setOf("GPU-2", "GPU-3")                        // the free cards

	for _, tc := range []struct {
		name    string
		num     int64
		policy  util.SchedulerPolicy
		exact   map[string]bool // exact expected set (nil → only invariants)
		avoidAB bool            // must not overlap podA's a,b
	}{
		{"4 cards none -> whole island a,b,c,d", 4, util.NonePolicy, islandA, false},
		{"2 cards spread -> free c,d (avoids podA)", 2, util.SpreadPolicy, cd, true},
		{"2 cards binpack -> stacks on a,b", 2, util.BinpackPolicy, ab, false},
		{"2 cards none -> some pair within island", 2, util.NonePolicy, nil, false},
		{"3 cards none -> within island", 3, util.NonePolicy, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, devs := crossPodNode(t, "node1", "GPU")
			podA := crossPodSibling("podA", "gangX", "node1", "GPU-0", "GPU-1")
			ni, err := device.NewNodeInfo(node,
				device.WithGPUTopologyEnabled(true),
				device.WithNodeDevice(&devs),
				device.WithNodePods(podA))
			require.NoError(t, err)

			podB := crossPodGangPod("podB", "gangX", true, tc.num)
			if tc.policy != util.NonePolicy {
				withPolicy(podB, tc.policy)
			}
			got := containerUUIDs(runAllocate(t, ni, podB), "app")
			t.Logf("%s -> %v", tc.name, keys(got))

			// Invariant 1: anchored — every card is in island A.
			for u := range got {
				require.Truef(t, islandA[u], "%s left island A (anchor failed)", u)
			}
			require.Len(t, got, int(tc.num))
			// Invariant 2: spread must not co-locate on podA's cards.
			if tc.avoidAB {
				for u := range got {
					require.Falsef(t, ab[u], "spread should avoid podA's card %s", u)
				}
			}
			if tc.exact != nil {
				assertExactSet(t, got, tc.exact)
			}
		})
	}
}

// crossNodeLand drives the cross-node flow: a sibling occupies siblingCards on
// node1; resolve its sub-domain signature on node1; allocate podB (num cards) on
// node2 carrying that signature; return the cards podB landed on (node2 UUIDs).
// n1Rails/n2Rails nil → no rail map (positional ordinal); non-nil → rail keys.
func crossNodeLand(t *testing.T, n1Rails, n2Rails *[8]string, siblingCards []string, num int64) map[string]struct{} {
	t.Helper()
	node1, devs1 := crossPodNode(t, "node1", "N1")
	node2, devs2 := crossPodNode(t, "node2", "N2")
	if n1Rails != nil {
		withRails(t, node1, "N1", *n1Rails)
	}
	if n2Rails != nil {
		withRails(t, node2, "N2", *n2Rails)
	}
	sibling := crossPodSibling("sib", "gangX", "node1", siblingCards...)
	node1Info, err := device.NewNodeInfo(node1,
		device.WithGPUTopologyEnabled(true), device.WithNodeDevice(&devs1),
		device.WithNodePods(sibling))
	require.NoError(t, err)

	// Resolve the sibling's domain on ITS OWN node (what the filter does).
	domain, ok := node1Info.DomainOfUUIDs(siblingCards)
	require.True(t, ok, "sibling domain must resolve on node1")

	node2Info, err := device.NewNodeInfo(node2,
		device.WithGPUTopologyEnabled(true), device.WithNodeDevice(&devs2))
	require.NoError(t, err)
	req := BuildAllocationRequest(crossPodGangPod("main", "gangX", true, num))
	req.GangDomainKey = domain
	newPod, rsn, err := NewAllocator(node2Info, nil).Allocate(req)
	require.NoError(t, err)
	require.Nil(t, rsn, "allocation unexpectedly rejected: %v", rsn)
	claim := device.PodDeviceClaim{}
	pre, _ := util.HasAnnotation(newPod, util.PodVGPUPreAllocAnnotation)
	require.NoError(t, claim.UnmarshalText(pre))
	return containerUUIDs(claim, "app")
}

// Test_CrossPod_CrossNode_DomainMatching covers BOTH identification modes:
//   - homogeneous nodes, no rail map → positional ORDINAL ("ord:N") alignment.
//   - heterogeneous (node2 reverse-wired) + rail map → DOMAIN-KEY (rail-set)
//     alignment, which the ordinal scheme would get wrong.
//
// node1/node2 island A = idx 0-3, island B = idx 4-7. A "straight" rail map keys
// island A→{rA..rD}, B→{rE..rH}; "reversed" swaps them so node2's island B carries
// node1 island A's rails.
func Test_CrossPod_CrossNode_DomainMatching(t *testing.T) {
	straight := [8]string{"rA", "rB", "rC", "rD", "rE", "rF", "rG", "rH"}
	reversed := [8]string{"rE", "rF", "rG", "rH", "rA", "rB", "rC", "rD"}
	n2A := setOf("N2-0", "N2-1", "N2-2", "N2-3") // node2 island A
	n2B := setOf("N2-4", "N2-5", "N2-6", "N2-7") // node2 island B

	for _, tc := range []struct {
		name            string
		n1Rails         *[8]string
		n2Rails         *[8]string
		siblingCards    []string
		wantNode2Island map[string]bool
	}{
		// ── 同构节点:位置序号 ordinal 识别同域(无 rail 数据)──
		{"homogeneous ordinal: sibling island A (ord0) -> node2 island A",
			nil, nil, []string{"N1-0", "N1-1"}, n2A},
		{"homogeneous ordinal: sibling island B (ord1) -> node2 island B",
			nil, nil, []string{"N1-4", "N1-5"}, n2B},

		// ── 异构节点:域签名 domain key 识别同域(node2 反接 + rail 数据)──
		{"heterogeneous rail: sibling node1 A (rA-rD) -> node2 island B (rA-rD)",
			&straight, &reversed, []string{"N1-0", "N1-1"}, n2B},
		{"heterogeneous rail: sibling node1 B (rE-rH) -> node2 island A (rE-rH)",
			&straight, &reversed, []string{"N1-4", "N1-5"}, n2A},

		// ── 同构 + rail 数据:rail-set 与 ordinal 一致 ──
		{"homogeneous rail: sibling node1 A (rA-rD) -> node2 island A (rA-rD)",
			&straight, &straight, []string{"N1-0", "N1-1"}, n2A},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := crossNodeLand(t, tc.n1Rails, tc.n2Rails, tc.siblingCards, 4)
			t.Logf("%s -> %v", tc.name, keys(got))
			assertExactSet(t, got, tc.wantNode2Island)
		})
	}
}
