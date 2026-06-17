package device

import (
	"fmt"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

// gangPod builds a pod that PodHasGangName recognises (via the coscheduling
// pod-group label), with NodeName set so ShouldCountPodDeviceAllocation counts
// its pre-allocation, and a PodVGPUPreAllocAnnotation referencing the given
// device IDs/UUIDs.
func gangPod(uid, gang, node string, claim string) *corev1.Pod {
	labels := map[string]string{}
	if gang != "" {
		labels[util.CoschedulingPodGroupLabel] = gang
	}
	anns := map[string]string{}
	if claim != "" {
		anns[util.PodVGPUPreAllocAnnotation] = claim
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:         types.UID(uid),
			Name:        uid,
			Labels:      labels,
			Annotations: anns,
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

// claimText renders a one-container pre-alloc annotation for the given UUIDs.
func claimText(uuids ...string) string {
	out := "cont1["
	for i, u := range uuids {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("%d_%s_10_1024", i, u)
	}
	return out + "]"
}

// twoComponentNode is a NodeInfo with cards gpu0/gpu1 in NVLink component 0 and
// gpu2/gpu3 in NVLink component 2 — directly constructed so the test controls the
// NVLink components precisely (NewFakeNodeInfo rebuilds the device list without
// links, which would make every card its own singleton component). Cross-pod
// methods read the nvlink* fields.
func twoComponentNode(pods ...*corev1.Pod) *NodeInfo {
	return &NodeInfo{
		name: "node1",
		nvlinkComponentByUUID: map[string]int{
			"gpu0": 0, "gpu1": 0,
			"gpu2": 2, "gpu3": 2,
		},
		nvlinkComponentToUUIDs: map[int][]string{
			0: {"gpu0", "gpu1"},
			2: {"gpu2", "gpu3"},
		},
		nodePods: pods,
	}
}

func Test_GangAnchorComponent(t *testing.T) {
	self := types.UID("self")
	tests := []struct {
		name     string
		gang     string
		pods     []*corev1.Pod
		wantRoot int
		wantOK   bool
	}{
		{
			name:     "sibling in component 2 anchors there",
			gang:     "gangA",
			pods:     []*corev1.Pod{gangPod("sib", "gangA", "node1", claimText("gpu2"))},
			wantRoot: 2,
			wantOK:   true,
		},
		{
			name:     "self pod is ignored",
			gang:     "gangA",
			pods:     []*corev1.Pod{gangPod("self", "gangA", "node1", claimText("gpu2"))},
			wantRoot: -1,
			wantOK:   false,
		},
		{
			name:     "different gang is ignored",
			gang:     "gangA",
			pods:     []*corev1.Pod{gangPod("sib", "gangB", "node1", claimText("gpu2"))},
			wantRoot: -1,
			wantOK:   false,
		},
		{
			name:     "non-gang pod is ignored",
			gang:     "gangA",
			pods:     []*corev1.Pod{gangPod("sib", "", "node1", claimText("gpu2"))},
			wantRoot: -1,
			wantOK:   false,
		},
		{
			name:     "empty gang name never anchors",
			gang:     "",
			pods:     []*corev1.Pod{gangPod("sib", "gangA", "node1", claimText("gpu2"))},
			wantRoot: -1,
			wantOK:   false,
		},
		{
			name:     "no pods, no anchor",
			gang:     "gangA",
			pods:     nil,
			wantRoot: -1,
			wantOK:   false,
		},
		{
			name: "split anchor picks majority component",
			gang: "gangA",
			pods: []*corev1.Pod{
				gangPod("sibA", "gangA", "node1", claimText("gpu0")),         // 1 vote → comp 0
				gangPod("sibB", "gangA", "node1", claimText("gpu2", "gpu3")), // 2 votes → comp 2
			},
			wantRoot: 2,
			wantOK:   true,
		},
		{
			name:     "stale sibling (no NodeName, no condition) still counts when scheduled elsewhere is false",
			gang:     "gangA",
			pods:     []*corev1.Pod{gangPod("sib", "gangA", "node1", claimText("gpu0"))},
			wantRoot: 0,
			wantOK:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := twoComponentNode(tc.pods...)
			root, ok := n.GangAnchorComponent(tc.gang, nil, sets.New(self))
			if ok != tc.wantOK || root != tc.wantRoot {
				t.Fatalf("GangAnchorComponent(%q) = (%d, %v), want (%d, %v)",
					tc.gang, root, ok, tc.wantRoot, tc.wantOK)
			}
		})
	}
}

func Test_GangAnchorComponent_UnknownUUIDIgnored(t *testing.T) {
	// A sibling pre-allocated a card the node doesn't know about (UUID not in
	// linkComponentByUUID) contributes no vote and must not anchor.
	n := twoComponentNode(gangPod("sib", "gangA", "node1", claimText("ghost")))
	if root, ok := n.GangAnchorComponent("gangA", nil, sets.New(types.UID("self"))); ok || root != -1 {
		t.Fatalf("unknown-uuid sibling: got (%d, %v), want (-1, false)", root, ok)
	}
}

func Test_ComponentUUIDs(t *testing.T) {
	n := twoComponentNode()
	if got := n.ComponentUUIDs(2); len(got) != 2 {
		t.Fatalf("ComponentUUIDs(2) = %v, want 2 members", got)
	}
	if got := n.ComponentUUIDs(99); got != nil {
		t.Fatalf("ComponentUUIDs(unknown) = %v, want nil", got)
	}
}

func Test_buildComponentIndex(t *testing.T) {
	if got := buildComponentIndex(nil); len(got) != 0 {
		t.Fatalf("buildComponentIndex(nil) = %v, want empty", got)
	}
	idx := buildComponentIndex(map[string]int{"a": 1, "b": 1, "c": 3})
	if len(idx[1]) != 2 || len(idx[3]) != 1 {
		t.Fatalf("buildComponentIndex produced %v", idx)
	}
}

func Test_buildComponentOrdinals(t *testing.T) {
	// Two components: root 7 = {gpu4(id4),gpu5(id5)}, root 0 = {gpu0(id0),gpu1(id1)}.
	// Ranked by min Device.Index: root 0 (min 0) → ordinal 0; root 7 (min 4) → ordinal 1.
	componentToUUIDs := map[int][]string{
		7: {"gpu4", "gpu5"},
		0: {"gpu0", "gpu1"},
	}
	deviceIndexMap := map[string]int{"gpu0": 0, "gpu1": 1, "gpu4": 4, "gpu5": 5}
	rootByOrdinal, componentOrdinal := buildComponentOrdinals(componentToUUIDs, deviceIndexMap)
	if rootByOrdinal[0] != 0 || rootByOrdinal[1] != 7 {
		t.Fatalf("rootByOrdinal = %v, want {0:0, 1:7}", rootByOrdinal)
	}
	// componentOrdinal is the inverse: root 0 → ordinal 0, root 7 → ordinal 1.
	if componentOrdinal[0] != 0 || componentOrdinal[7] != 1 {
		t.Fatalf("componentOrdinal = %v, want {0:0, 7:1}", componentOrdinal)
	}
}

func Test_buildComponentOrdinals_UnresolvableSortsLast(t *testing.T) {
	// Defensive: a component whose UUIDs are absent from deviceIndexMap (min stays
	// MaxInt) must sort LAST, not steal ordinal 0 from a real component.
	componentToUUIDs := map[int][]string{
		5: {"ghostA", "ghostB"}, // no index → MaxInt → last
		0: {"gpu0", "gpu1"},     // min 0 → ordinal 0
	}
	deviceIndexMap := map[string]int{"gpu0": 0, "gpu1": 1}
	rootByOrdinal, componentOrdinal := buildComponentOrdinals(componentToUUIDs, deviceIndexMap)
	if rootByOrdinal[0] != 0 {
		t.Fatalf("ordinal 0 = root %d, want real component root 0", rootByOrdinal[0])
	}
	if componentOrdinal[5] != 1 {
		t.Fatalf("unresolvable component root 5 = ordinal %d, want last (1)", componentOrdinal[5])
	}
}

func Test_LinkTopologyFitness(t *testing.T) {
	// Island node: NVLink islands of 4, whole node (P2P) of 8.
	island := &NodeInfo{gpuTopology: true, maxNVLinkComponentSize: 4, maxLinkComponentSize: 8}
	// Full-NVSwitch node: one NVLink fabric of 8.
	nvswitch := &NodeInfo{gpuTopology: true, maxNVLinkComponentSize: 8, maxLinkComponentSize: 8}
	noTopo := &NodeInfo{gpuTopology: false}

	for _, tc := range []struct {
		name string
		n    *NodeInfo
		need int
		want int
	}{
		{"island fits NVLink island", island, 4, 3},
		{"island needs 8 -> only P2P", island, 8, 2},
		{"island needs 9 -> cannot fit", island, 9, 1},
		{"nvswitch fits 8 NVLink", nvswitch, 8, 3},
		{"no topology", noTopo, 4, 0},
	} {
		if got := tc.n.LinkTopologyFitness(tc.need); got != tc.want {
			t.Fatalf("%s: LinkTopologyFitness(%d) = %d, want %d", tc.name, tc.need, got, tc.want)
		}
	}
	// Ranking invariant: NVLink-fit (8 cards) beats P2P-only on a mixed cluster.
	if !(nvswitch.LinkTopologyFitness(8) > island.LinkTopologyFitness(8)) {
		t.Fatalf("NVSwitch node must rank above island node for an 8-card group")
	}
	// Preserved invariant: any topology-capable node beats a non-topology node.
	if !(island.LinkTopologyFitness(4) > noTopo.LinkTopologyFitness(4)) {
		t.Fatalf("topology node must rank above non-topology node")
	}
}

func Test_computeLinkComponents_NVLinkVsAnyP2P(t *testing.T) {
	// 4 GPUs: 0-1 and 2-3 are NVLinked islands, but ALL pairs are at least PCIe
	// (cross-CPU) connected — the normal case for a multi-GPU node. The fix hinges
	// on these two views differing.
	dl := make(gpuallocator.DeviceList, 4)
	for i := 0; i < 4; i++ {
		dl[i] = gpuallocator.NewDevice(i, fmt.Sprintf("gpu%d", i), "")
	}
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			if i != j {
				dl.AddLink(i, j, links.P2PLinkCrossCPU) // PCIe between every pair
			}
		}
	}
	dl.AddLink(0, 1, links.SingleNVLINKLink)
	dl.AddLink(1, 0, links.SingleNVLINKLink)
	dl.AddLink(2, 3, links.SingleNVLINKLink)
	dl.AddLink(3, 2, links.SingleNVLINKLink)

	// any-P2P: the whole node is ONE reachability component (PCIe connects all).
	maxAny, byAny := computeLinkComponents(dl, anyLinkEdge)
	if maxAny != 4 || byAny["gpu0"] != byAny["gpu3"] {
		t.Fatalf("anyP2P: max=%d, want one component of 4 connecting all GPUs", maxAny)
	}
	// NVLink-only: two islands {0,1} and {2,3} — what cross-pod affinity needs.
	maxNV, byNV := computeLinkComponents(dl, nvlinkEdge)
	if maxNV != 2 {
		t.Fatalf("nvlink: max=%d, want islands of size 2", maxNV)
	}
	if byNV["gpu0"] != byNV["gpu1"] || byNV["gpu2"] != byNV["gpu3"] {
		t.Fatalf("nvlink: gpu0/1 and gpu2/3 must each share an island")
	}
	if byNV["gpu0"] == byNV["gpu2"] {
		t.Fatalf("nvlink: gpu0 and gpu2 must be in DIFFERENT islands")
	}
}

func Test_buildComponentOrdinals_Deterministic(t *testing.T) {
	// Equal min is impossible for disjoint components, but the root tiebreak must
	// keep ordinals deterministic across runs regardless of map iteration order.
	componentToUUIDs := map[int][]string{2: {"a"}, 9: {"b"}, 4: {"c"}}
	deviceIndexMap := map[string]int{"a": 10, "b": 30, "c": 20}
	for i := 0; i < 8; i++ {
		rbo, _ := buildComponentOrdinals(componentToUUIDs, deviceIndexMap)
		if rbo[0] != 2 || rbo[1] != 4 || rbo[2] != 9 {
			t.Fatalf("non-deterministic ordinals: %v", rbo)
		}
	}
}

// twoOrdinalNode builds a NodeInfo with stable ordinals: ordinal 0 = component
// root 0 (gpu0/gpu1, ids 0/1), ordinal 1 = component root 2 (gpu2/gpu3, ids 2/3).
func twoOrdinalNode() *NodeInfo {
	n := twoComponentNode()
	n.deviceIndexMap = map[string]int{"gpu0": 0, "gpu1": 1, "gpu2": 2, "gpu3": 3}
	n.nvlinkRootByOrdinal, n.nvlinkComponentOrdinal = buildComponentOrdinals(n.nvlinkComponentToUUIDs, n.deviceIndexMap)
	return n
}

func Test_ComponentByOrdinal(t *testing.T) {
	n := twoOrdinalNode()
	if root, ok := n.ComponentByOrdinal(0); !ok || root != 0 {
		t.Fatalf("ComponentByOrdinal(0) = (%d,%v), want (0,true)", root, ok)
	}
	if root, ok := n.ComponentByOrdinal(1); !ok || root != 2 {
		t.Fatalf("ComponentByOrdinal(1) = (%d,%v), want (2,true)", root, ok)
	}
	if _, ok := n.ComponentByOrdinal(5); ok {
		t.Fatalf("ComponentByOrdinal(5) should be absent")
	}
}

func Test_OrdinalOfUUIDs(t *testing.T) {
	n := twoOrdinalNode()
	// Sibling's UUIDs {gpu2,gpu3} live in component root 2 → ordinal 1.
	if ord, ok := n.OrdinalOfUUIDs([]string{"gpu2", "gpu3"}); !ok || ord != 1 {
		t.Fatalf("OrdinalOfUUIDs([gpu2,gpu3]) = (%d,%v), want (1,true)", ord, ok)
	}
	// {gpu0,gpu1} → ordinal 0.
	if ord, ok := n.OrdinalOfUUIDs([]string{"gpu0", "gpu1"}); !ok || ord != 0 {
		t.Fatalf("OrdinalOfUUIDs([gpu0,gpu1]) = (%d,%v), want (0,true)", ord, ok)
	}
	// Duplicate UUIDs (multi-container sharing a card) collapse, don't skew.
	if ord, ok := n.OrdinalOfUUIDs([]string{"gpu2", "gpu2", "gpu3"}); !ok || ord != 1 {
		t.Fatalf("OrdinalOfUUIDs(dup) = (%d,%v), want (1,true)", ord, ok)
	}
	// Majority wins when UUIDs span ordinals (degraded sibling): 1×ord0 + 2×ord1.
	if ord, ok := n.OrdinalOfUUIDs([]string{"gpu0", "gpu2", "gpu3"}); !ok || ord != 1 {
		t.Fatalf("OrdinalOfUUIDs(spanning) = (%d,%v), want (1,true)", ord, ok)
	}
	// Empty / unknown UUIDs → no ordinal.
	if _, ok := n.OrdinalOfUUIDs(nil); ok {
		t.Fatalf("OrdinalOfUUIDs(nil) should be false")
	}
	if _, ok := n.OrdinalOfUUIDs([]string{"ghost"}); ok {
		t.Fatalf("OrdinalOfUUIDs(unknown) should be false")
	}
}

func Test_PodPreAllocatedUUIDs(t *testing.T) {
	p := gangPod("sib", "gangA", "node1", claimText("gpu2", "gpu3"))
	uuids := PodPreAllocatedUUIDs(p)
	if len(uuids) != 2 || uuids[0] != "gpu2" || uuids[1] != "gpu3" {
		t.Fatalf("PodPreAllocatedUUIDs = %v, want [gpu2 gpu3]", uuids)
	}
	// Multi-container sharing the same card → UUID deduplicated.
	dup := &corev1.Pod{}
	dup.Annotations = map[string]string{
		util.PodVGPUPreAllocAnnotation: "cont1[0_gpu2_10_1024];cont2[0_gpu2_10_1024]",
	}
	if got := PodPreAllocatedUUIDs(dup); len(got) != 1 || got[0] != "gpu2" {
		t.Fatalf("dedup PodPreAllocatedUUIDs = %v, want [gpu2]", got)
	}
	if got := PodPreAllocatedUUIDs(&corev1.Pod{}); got != nil {
		t.Fatalf("no-annotation pod = %v, want nil", got)
	}
}
