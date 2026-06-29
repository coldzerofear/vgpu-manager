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

func Test_GangAnchorComponent_ControllerOwner(t *testing.T) {
	ctrlTrue := true
	ownerA := &metav1.OwnerReference{UID: types.UID("rsA")}
	self := sets.New(types.UID("self"))
	// mkPod: a pod with NO gang label but a controller owner (like a
	// Deployment/Job replica) referencing ownerUID.
	mkPod := func(uid, ownerUID, claim string) *corev1.Pod {
		p := gangPod(uid, "", "node1", claim)
		p.OwnerReferences = []metav1.OwnerReference{
			{Kind: "ReplicaSet", Name: "rs", UID: types.UID(ownerUID), Controller: &ctrlTrue},
		}
		return p
	}

	// Same controller owner → anchor to the sibling's component.
	n := twoComponentNode(mkPod("sib", "rsA", claimText("gpu2")))
	if root, ok := n.GangAnchorComponent("", ownerA, self); !ok || root != 2 {
		t.Fatalf("same-controller sibling: (%d,%v), want (2,true)", root, ok)
	}
	// Different controller owner → ignored.
	n = twoComponentNode(mkPod("sib", "rsB", claimText("gpu2")))
	if root, ok := n.GangAnchorComponent("", ownerA, self); ok || root != -1 {
		t.Fatalf("different controller: (%d,%v), want (-1,false)", root, ok)
	}
	// No gang name AND no owner → never anchors.
	n = twoComponentNode(mkPod("sib", "rsA", claimText("gpu2")))
	if root, ok := n.GangAnchorComponent("", nil, self); ok || root != -1 {
		t.Fatalf("nil owner + empty gang: (%d,%v), want (-1,false)", root, ok)
	}
	// gangName takes precedence over owner: a controller-only sibling (no gang
	// label) is NOT matched when a gangName is given.
	if root, ok := n.GangAnchorComponent("gangA", ownerA, self); ok || root != -1 {
		t.Fatalf("gangName set, controller-only sibling: (%d,%v), want (-1,false)", root, ok)
	}
}

func Test_GangAnchorComponent_TieBreakByOrdinal(t *testing.T) {
	// Two NVLink components: root 5 has ordinal 0 ("first rail"), root 0 has
	// ordinal 1. The LOWER root (0) has the HIGHER ordinal, so a root-based and an
	// ordinal-based tie-break disagree — this asserts we use the ordinal.
	n := &NodeInfo{
		name:                   "node1",
		nvlinkComponentByUUID:  map[string]int{"gpuX": 5, "gpuY": 0},
		nvlinkComponentToUUIDs: map[int][]string{5: {"gpuX"}, 0: {"gpuY"}},
		nvlinkComponentOrdinal: map[int]int{5: 0, 0: 1},
		nodePods: []*corev1.Pod{
			gangPod("sibA", "gangA", "node1", claimText("gpuX")), // 1 vote → root 5 (ord 0)
			gangPod("sibB", "gangA", "node1", claimText("gpuY")), // 1 vote → root 0 (ord 1)
		},
	}
	root, ok := n.GangAnchorComponent("gangA", nil, sets.New(types.UID("self")))
	if !ok || root != 5 {
		t.Fatalf("tie-break = root %d (ok=%v), want root 5 (lower ordinal), not lower root 0", root, ok)
	}
}

func Test_GangAnchorComponent_UnknownUUIDIgnored(t *testing.T) {
	// A sibling pre-allocated a card the node doesn't know about (UUID not in
	// nvlinkComponentByUUID) contributes no vote and must not anchor.
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

func Test_buildComponentDomains_OrdinalFallback(t *testing.T) {
	// No rail map → positional "ord:N" signatures. Two components: root 7 =
	// {gpu4,gpu5}, root 0 = {gpu0,gpu1}. Ranked by min index: root 0 → ord 0,
	// root 7 → ord 1.
	componentToUUIDs := map[int][]string{
		7: {"gpu4", "gpu5"},
		0: {"gpu0", "gpu1"},
	}
	deviceIndexMap := map[string]int{"gpu0": 0, "gpu1": 1, "gpu4": 4, "gpu5": 5}
	componentOrdinal, componentDomain, rootByDomain := buildComponentDomains(componentToUUIDs, deviceIndexMap, nil)
	if componentOrdinal[0] != 0 || componentOrdinal[7] != 1 {
		t.Fatalf("componentOrdinal = %v, want {0:0, 7:1}", componentOrdinal)
	}
	if componentDomain[0] != "ord:0" || componentDomain[7] != "ord:1" {
		t.Fatalf("componentDomain = %v, want {0:ord:0, 7:ord:1}", componentDomain)
	}
	if rootByDomain["ord:0"] != 0 || rootByDomain["ord:1"] != 7 {
		t.Fatalf("rootByDomain = %v, want {ord:0:0, ord:1:7}", rootByDomain)
	}
}

func Test_buildComponentDomains_RailKeyed(t *testing.T) {
	// With a full rail map, signatures are rail-sets — NOT positional. This is the
	// heterogeneity-correct path: the SAME rail-set yields the SAME signature
	// regardless of which index range it occupies.
	componentToUUIDs := map[int][]string{
		7: {"gpu4", "gpu5"}, // rails rA,rB
		0: {"gpu0", "gpu1"}, // rails rC,rD
	}
	deviceIndexMap := map[string]int{"gpu0": 0, "gpu1": 1, "gpu4": 4, "gpu5": 5}
	rail := map[string]string{"gpu0": "rC", "gpu1": "rD", "gpu4": "rA", "gpu5": "rB"}
	_, componentDomain, rootByDomain := buildComponentDomains(componentToUUIDs, deviceIndexMap, rail)
	if componentDomain[7] != "rail:rA,rB" || componentDomain[0] != "rail:rC,rD" {
		t.Fatalf("componentDomain = %v, want rail-set signatures", componentDomain)
	}
	if rootByDomain["rail:rA,rB"] != 7 || rootByDomain["rail:rC,rD"] != 0 {
		t.Fatalf("rootByDomain = %v, want rail→root", rootByDomain)
	}
	// Partial rail map (gpu5 missing) → all-or-nothing falls back to ordinals.
	partial := map[string]string{"gpu0": "rC", "gpu1": "rD", "gpu4": "rA"}
	_, dom2, _ := buildComponentDomains(componentToUUIDs, deviceIndexMap, partial)
	if dom2[0] != "ord:0" || dom2[7] != "ord:1" {
		t.Fatalf("partial rail map must fall back to ordinals, got %v", dom2)
	}
}

func Test_buildComponentDomains_UnresolvableSortsLast(t *testing.T) {
	// Defensive: a component whose UUIDs are absent from deviceIndexMap (min stays
	// MaxInt) must sort LAST, not steal ordinal 0 from a real component.
	componentToUUIDs := map[int][]string{
		5: {"ghostA", "ghostB"}, // no index → MaxInt → last
		0: {"gpu0", "gpu1"},     // min 0 → ordinal 0
	}
	deviceIndexMap := map[string]int{"gpu0": 0, "gpu1": 1}
	componentOrdinal, _, rootByDomain := buildComponentDomains(componentToUUIDs, deviceIndexMap, nil)
	if rootByDomain["ord:0"] != 0 {
		t.Fatalf("ord:0 = root %d, want real component root 0", rootByDomain["ord:0"])
	}
	if componentOrdinal[5] != 1 {
		t.Fatalf("unresolvable component root 5 = ordinal %d, want last (1)", componentOrdinal[5])
	}
}

func Test_LinkTopologyFitness(t *testing.T) {
	// Island node: NVLink islands of 4; the two islands are only cross-CPU
	// reachable, so the node fits 8 GPUs at the any-P2P tier only.
	island := &NodeInfo{gpuTopology: true, maxNVLinkComponentSize: 4,
		maxSwitchComponentSize: 4, maxNUMAComponentSize: 4, maxLinkComponentSize: 8}
	// Same islands, but bridged within one NUMA node (HostBridge) → NUMA tier of 8.
	numaIsland := &NodeInfo{gpuTopology: true, maxNVLinkComponentSize: 4,
		maxSwitchComponentSize: 4, maxNUMAComponentSize: 8, maxLinkComponentSize: 8}
	// Same islands, but bridged across PCIe switches → switch tier of 8.
	switchIsland := &NodeInfo{gpuTopology: true, maxNVLinkComponentSize: 4,
		maxSwitchComponentSize: 8, maxNUMAComponentSize: 8, maxLinkComponentSize: 8}
	// Full-NVSwitch node: one NVLink fabric of 8.
	nvswitch := &NodeInfo{gpuTopology: true, maxNVLinkComponentSize: 8,
		maxSwitchComponentSize: 8, maxNUMAComponentSize: 8, maxLinkComponentSize: 8}
	noTopo := &NodeInfo{gpuTopology: false}

	for _, tc := range []struct {
		name string
		n    *NodeInfo
		need int
		want int
	}{
		{"island fits NVLink island", island, 4, 5},
		{"island needs 8 -> cross-CPU only", island, 8, 2},
		{"island needs 9 -> cannot fit", island, 9, 1},
		{"numaIsland needs 8 -> NUMA tier", numaIsland, 8, 3},
		{"switchIsland needs 8 -> switch tier", switchIsland, 8, 4},
		{"nvswitch fits 8 NVLink", nvswitch, 8, 5},
		{"no topology", noTopo, 4, 0},
	} {
		if got := tc.n.LinkTopologyFitness(tc.need); got != tc.want {
			t.Fatalf("%s: LinkTopologyFitness(%d) = %d, want %d", tc.name, tc.need, got, tc.want)
		}
	}
	// Gradient invariant for an 8-card group: NVLink > switch > NUMA > cross-CPU.
	if !(nvswitch.LinkTopologyFitness(8) > switchIsland.LinkTopologyFitness(8) &&
		switchIsland.LinkTopologyFitness(8) > numaIsland.LinkTopologyFitness(8) &&
		numaIsland.LinkTopologyFitness(8) > island.LinkTopologyFitness(8)) {
		t.Fatalf("fitness must strictly decrease NVLink > switch > NUMA > cross-CPU")
	}
	// Preserved invariant: any topology-capable node beats a non-topology node.
	if !(island.LinkTopologyFitness(4) > noTopo.LinkTopologyFitness(4)) {
		t.Fatalf("topology node must rank above non-topology node")
	}
}

func Test_computeTieredComponents(t *testing.T) {
	// 4 GPUs with deliberately distinct tier links so every tier differs:
	//   0-1 NVLink(7), 1-2 HostBridge(3), 2-3 CrossCPU(1).
	// NVLink island {0,1}; NUMA adds 2 via host bridge; any adds 3 via cross-CPU.
	dl := make(gpuallocator.DeviceList, 4)
	for i := 0; i < 4; i++ {
		dl[i] = gpuallocator.NewDevice(i, fmt.Sprintf("gpu%d", i), "")
	}
	addBoth := func(a, b int, t links.P2PLinkType) {
		dl.AddLink(a, b, t)
		dl.AddLink(b, a, t)
	}
	addBoth(0, 1, links.SingleNVLINKLink)
	addBoth(1, 2, links.P2PLinkHostBridge)
	addBoth(2, 3, links.P2PLinkCrossCPU)

	byNV, maxNV, maxSw, maxNUMA, maxAny := computeTieredComponents(dl)
	if maxNV != 2 || maxSw != 2 || maxNUMA != 3 || maxAny != 4 {
		t.Fatalf("sizes = NVLink %d, switch %d, NUMA %d, any %d; want 2,2,3,4",
			maxNV, maxSw, maxNUMA, maxAny)
	}
	// NVLink map: gpu0/gpu1 share the island; gpu2/gpu3 are their own singletons.
	if byNV["gpu0"] != byNV["gpu1"] {
		t.Fatalf("nvlink: gpu0/gpu1 must share an island")
	}
	if byNV["gpu0"] == byNV["gpu2"] || byNV["gpu2"] == byNV["gpu3"] {
		t.Fatalf("nvlink: gpu2 and gpu3 must each be their own singleton")
	}
}

func Test_buildComponentDomains_Deterministic(t *testing.T) {
	// Equal min is impossible for disjoint components, but the root tiebreak must
	// keep ordinals deterministic across runs regardless of map iteration order.
	componentToUUIDs := map[int][]string{2: {"a"}, 9: {"b"}, 4: {"c"}}
	deviceIndexMap := map[string]int{"a": 10, "b": 30, "c": 20}
	for i := 0; i < 8; i++ {
		_, _, rbd := buildComponentDomains(componentToUUIDs, deviceIndexMap, nil)
		if rbd["ord:0"] != 2 || rbd["ord:1"] != 4 || rbd["ord:2"] != 9 {
			t.Fatalf("non-deterministic domains: %v", rbd)
		}
	}
}

// twoDomainNode builds a NodeInfo with stable domains: ord:0 = component root 0
// (gpu0/gpu1, ids 0/1), ord:1 = component root 2 (gpu2/gpu3, ids 2/3).
func twoDomainNode() *NodeInfo {
	n := twoComponentNode()
	n.deviceIndexMap = map[string]int{"gpu0": 0, "gpu1": 1, "gpu2": 2, "gpu3": 3}
	n.nvlinkComponentOrdinal, n.nvlinkComponentDomain, n.nvlinkRootByDomain =
		buildComponentDomains(n.nvlinkComponentToUUIDs, n.deviceIndexMap, nil)
	return n
}

func Test_ComponentByDomain(t *testing.T) {
	n := twoDomainNode()
	if root, ok := n.ComponentByDomain("ord:0"); !ok || root != 0 {
		t.Fatalf("ComponentByDomain(ord:0) = (%d,%v), want (0,true)", root, ok)
	}
	if root, ok := n.ComponentByDomain("ord:1"); !ok || root != 2 {
		t.Fatalf("ComponentByDomain(ord:1) = (%d,%v), want (2,true)", root, ok)
	}
	if _, ok := n.ComponentByDomain("ord:5"); ok {
		t.Fatalf("ComponentByDomain(ord:5) should be absent")
	}
	if _, ok := n.ComponentByDomain(""); ok {
		t.Fatalf("ComponentByDomain(empty) should be absent")
	}
}

func Test_DomainOfUUIDs(t *testing.T) {
	n := twoDomainNode()
	// Sibling's UUIDs {gpu2,gpu3} live in component root 2 → ord:1.
	if dom, ok := n.DomainOfUUIDs([]string{"gpu2", "gpu3"}); !ok || dom != "ord:1" {
		t.Fatalf("DomainOfUUIDs([gpu2,gpu3]) = (%q,%v), want (ord:1,true)", dom, ok)
	}
	// {gpu0,gpu1} → ord:0.
	if dom, ok := n.DomainOfUUIDs([]string{"gpu0", "gpu1"}); !ok || dom != "ord:0" {
		t.Fatalf("DomainOfUUIDs([gpu0,gpu1]) = (%q,%v), want (ord:0,true)", dom, ok)
	}
	// Duplicate UUIDs (multi-container sharing a card) collapse, don't skew.
	if dom, ok := n.DomainOfUUIDs([]string{"gpu2", "gpu2", "gpu3"}); !ok || dom != "ord:1" {
		t.Fatalf("DomainOfUUIDs(dup) = (%q,%v), want (ord:1,true)", dom, ok)
	}
	// Majority wins when UUIDs span domains (degraded sibling): 1×ord:0 + 2×ord:1.
	if dom, ok := n.DomainOfUUIDs([]string{"gpu0", "gpu2", "gpu3"}); !ok || dom != "ord:1" {
		t.Fatalf("DomainOfUUIDs(spanning) = (%q,%v), want (ord:1,true)", dom, ok)
	}
	// Empty / unknown UUIDs → no domain.
	if _, ok := n.DomainOfUUIDs(nil); ok {
		t.Fatalf("DomainOfUUIDs(nil) should be false")
	}
	if _, ok := n.DomainOfUUIDs([]string{"ghost"}); ok {
		t.Fatalf("DomainOfUUIDs(unknown) should be false")
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
