package allocator

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// crossPodIslandTopo encodes an 8-GPU node with TWO NVLink islands:
//
//	island A = {0,1,2,3} — pairwise TwoNVLINKLinks (STRONGER link)
//	island B = {4,5,6,7} — pairwise SingleNVLINKLink (weaker link)
//	cross-island        — P2PLinkSingleSwitch (PCIe, NOT NVLink)
//
// So nvlinkComponentByUUID has exactly two components, and bestEffort's natural
// (no-anchor) choice for a 2-card group is island A because its link score is
// higher. That makes the cross-pod anchor's effect unambiguous: only affinity
// can pull the new pod onto the weaker island B where its gang sibling sits.
func crossPodIslandTopo(t *testing.T) string {
	two := []links.P2PLinkType{links.TwoNVLINKLinks}
	one := []links.P2PLinkType{links.SingleNVLINKLink}
	sw := []links.P2PLinkType{links.P2PLinkSingleSwitch}
	linkOf := func(i, j int) []links.P2PLinkType {
		switch {
		case i/4 != j/4:
			return sw
		case i/4 == 0:
			return two
		default:
			return one
		}
	}
	topo := make(device.NodeTopologyInfo, 8)
	for i := 0; i < 8; i++ {
		m := map[int][]links.P2PLinkType{}
		for j := 0; j < 8; j++ {
			if i != j {
				m[j] = linkOf(i, j) // both directions → symmetric graph
			}
		}
		topo[i] = device.TopologyInfo{Index: i, Links: m}
	}
	s, err := topo.Encode()
	require.NoError(t, err)
	return s
}

// crossPodNode builds an 8-GPU 2-island node named `name` whose card UUIDs are
// `prefix-0`..`prefix-7` (distinct UUIDs per node so cross-node alignment is
// genuinely identity-based, not coincidental).
func crossPodNode(t *testing.T, name, prefix string) (*corev1.Node, device.NodeDeviceInfo) {
	devs := make(device.NodeDeviceInfo, 8)
	for i := 0; i < 8; i++ {
		devs[i] = device.DeviceInfo{
			Id: i, Uuid: fmt.Sprintf("%s-%d", prefix, i), Type: "NVIDIA-A100",
			Core: util.HundredCore, Memory: 40960, Number: 10,
			Numa: -1, Capability: 8.0, Healthy: true,
		}
	}
	cfg, err := device.NodeConfigInfo{
		DeviceSplit: 10, CoresScaling: 1, MemoryFactor: 1, MemoryScaling: 1,
	}.Encode()
	require.NoError(t, err)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				util.NodeDeviceTopologyAnnotation: crossPodIslandTopo(t),
				util.NodeConfigInfoAnnotation:     cfg,
			},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("80"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("80"),
			},
		},
	}
	return node, devs
}

// withRails attaches a NodeGPUDomainAnnotation mapping <prefix>-i → rails[i],
// so the node's NVLink islands are keyed by physical rail-set instead of by
// positional ordinal.
func withRails(t *testing.T, node *corev1.Node, prefix string, rails [8]string) {
	m := map[string]string{}
	for i, r := range rails {
		m[fmt.Sprintf("%s-%d", prefix, i)] = r
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	node.Annotations[util.NodeGPUDomainAnnotation] = string(b)
}

// Test_CrossPod_NVLink_HeterogeneousRail is the crux of the rail-keyed design:
// node1's island A and node2's island B physically share the SAME rails (node2 is
// "reverse-wired"). Positional ordinal would mis-align (ordinal 0 → island A on
// both); rail keying resolves the sibling's rail-SET and lands the new pod on
// node2's island B — the truly rail-aligned island.
func Test_CrossPod_NVLink_HeterogeneousRail(t *testing.T) {
	// node1: island A (idx 0-3) = rails rA..rD; island B (idx 4-7) = rE..rH.
	node1, devs1 := crossPodNode(t, "node1", "N1")
	withRails(t, node1, "N1", [8]string{"rA", "rB", "rC", "rD", "rE", "rF", "rG", "rH"})
	// node2: REVERSED — island A (idx 0-3) = rE..rH; island B (idx 4-7) = rA..rD.
	node2, devs2 := crossPodNode(t, "node2", "N2")
	withRails(t, node2, "N2", [8]string{"rE", "rF", "rG", "rH", "rA", "rB", "rC", "rD"})

	// Sibling holds node1's island A (rails rA..rD).
	sibling := crossPodSibling("sib", "gangX", "node1", "N1-0", "N1-1")
	node1Info, err := device.NewNodeInfo(node1,
		device.WithGPUTopologyEnabled(true), device.WithNodeDevice(&devs1),
		device.WithNodePods(sibling))
	require.NoError(t, err)

	domain, ok := node1Info.DomainOfUUIDs([]string{"N1-0", "N1-1"})
	require.True(t, ok)
	require.Equal(t, "rail:rA,rB,rC,rD", domain, "sibling island keyed by its rail-set")

	// Allocate on node2 with the rail-set domain → must land on island B (rA..rD),
	// i.e. N2-4..7, NOT the positionally-equal island A.
	node2Info, err := device.NewNodeInfo(node2,
		device.WithGPUTopologyEnabled(true), device.WithNodeDevice(&devs2))
	require.NoError(t, err)
	req := BuildAllocationRequest(crossPodGangPod("main", "gangX", true, 2))
	req.GangDomainKey = domain
	newPod, rsn, err := NewAllocator(node2Info, nil).Allocate(req)
	require.NoError(t, err)
	require.Nil(t, rsn)
	claim := device.PodDeviceClaim{}
	pre, _ := util.HasAnnotation(newPod, util.PodVGPUPreAllocAnnotation)
	require.NoError(t, claim.UnmarshalText(pre))

	got := containerUUIDs(claim, "app")
	t.Logf("heterogeneous cross-node landed on: %v", keys(got))
	node2IslandB := map[string]bool{"N2-4": true, "N2-5": true, "N2-6": true, "N2-7": true}
	require.Len(t, got, 2)
	for u := range got {
		require.Truef(t, node2IslandB[u], "rail alignment must pick node2 island B (rA..rD), got %s", u)
	}
}

// crossPodGangPod builds a link-topology gang member requesting `number` cards.
func crossPodGangPod(uid, gang string, crossPod bool, number int64) *corev1.Pod {
	anns := map[string]string{
		util.DeviceTopologyModeAnnotation: string(util.LinkTopology),
	}
	if crossPod {
		anns[util.CrossPodTopologyAnnotation] = "true"
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:         types.UID(uid),
			Name:        uid,
			Labels:      map[string]string{util.CoschedulingPodGroupLabel: gang},
			Annotations: anns,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{vgpuContainer("app", number, 10, 1024)},
		},
	}
}

// crossPodSibling is a gang member already bound to `node` with a pre-allocated
// annotation pinning it to the given UUIDs (so the scheduler counts its usage
// AND GangAnchorComponent can read the NVLink island it occupies).
func crossPodSibling(uid, gang, node string, uuids ...string) *corev1.Pod {
	claim := "app["
	for i, u := range uuids {
		if i > 0 {
			claim += ","
		}
		claim += fmt.Sprintf("%d_%s_10_1024", i, u)
	}
	claim += "]"
	p := crossPodGangPod(uid, gang, true, int64(len(uuids)))
	p.Spec.NodeName = node
	p.Annotations[util.PodVGPUPreAllocAnnotation] = claim
	return p
}

// Test_CrossPod_NVLink_SameNodeAnchor drives the REAL allocator end-to-end and
// proves cross-pod NVLink affinity works: with the annotation off, the pod
// follows bestEffort onto the stronger island A; with it on, the pod converges
// onto the gang sibling's island B.
func Test_CrossPod_NVLink_SameNodeAnchor(t *testing.T) {
	node, devs := crossPodNode(t, "node1", "GPU")
	islandA := map[string]bool{"GPU-0": true, "GPU-1": true, "GPU-2": true, "GPU-3": true}
	islandB := map[string]bool{"GPU-4": true, "GPU-5": true, "GPU-6": true, "GPU-7": true}

	// Gang sibling already holds two cards in the weaker island B.
	sibling := crossPodSibling("sib", "gangX", "node1", "GPU-4", "GPU-5")

	build := func(pods ...*corev1.Pod) *device.NodeInfo {
		ni, err := device.NewNodeInfo(node,
			device.WithGPUTopologyEnabled(true),
			device.WithNodeDevice(&devs),
			device.WithNodePods(pods...))
		require.NoError(t, err)
		return ni
	}

	// Control: NO cross-pod affinity → bestEffort prefers the stronger island A.
	ctrl := containerUUIDs(runAllocate(t, build(sibling), crossPodGangPod("main", "gangX", false, 2)), "app")
	t.Logf("control  (cross-pod OFF) landed on: %v", keys(ctrl))
	for u := range ctrl {
		require.Truef(t, islandA[u], "control expected to follow bestEffort onto island A, got %s", u)
	}

	// Cross-pod ON → must converge onto the sibling's island B despite weaker link.
	got := containerUUIDs(runAllocate(t, build(sibling), crossPodGangPod("main", "gangX", true, 2)), "app")
	t.Logf("cross-pod (cross-pod ON ) landed on: %v", keys(got))
	require.Len(t, got, 2)
	for u := range got {
		require.Truef(t, islandB[u], "cross-pod pod landed on %s, expected sibling's island B {GPU-4..7}", u)
	}
}

// Test_CrossPod_NVLink_CrossNodeOrdinal drives the cross-NODE rail alignment:
// a gang sibling sits on node2's island B (ordinal 1). We resolve that ordinal
// on node2 (UUID-based, the real filter step), feed it to the request, and the
// new pod scheduled on node1 — which has NO same-node sibling — must converge
// onto node1's OWN ordinal-1 island. Distinct UUIDs per node prove the alignment
// is by structural ordinal (rail), not by shared identity.
func Test_CrossPod_NVLink_CrossNodeOrdinal(t *testing.T) {
	// node2: sibling occupies indices 4,5 → NVLink island B → ordinal 1.
	node2, devs2 := crossPodNode(t, "node2", "N2")
	sibling := crossPodSibling("sib", "gangX", "node2", "N2-4", "N2-5")
	node2Info, err := device.NewNodeInfo(node2,
		device.WithGPUTopologyEnabled(true),
		device.WithNodeDevice(&devs2),
		device.WithNodePods(sibling))
	require.NoError(t, err)

	// Resolve the sibling's rail domain ON ITS OWN NODE (what the filter does).
	// No rail map → positional "ord:1" (island B is the 2nd by min index).
	domain, ok := node2Info.DomainOfUUIDs([]string{"N2-4", "N2-5"})
	require.True(t, ok, "sibling domain must resolve on node2")
	require.Equal(t, "ord:1", domain, "island B (min index 4) is ordinal 1")
	t.Logf("sibling on node2 island B resolved to domain %q", domain)

	// node1: a DIFFERENT node (distinct UUIDs), no same-node sibling.
	node1, devs1 := crossPodNode(t, "node1", "N1")
	node1Info, err := device.NewNodeInfo(node1,
		device.WithGPUTopologyEnabled(true),
		device.WithNodeDevice(&devs1))
	require.NoError(t, err)

	// Build the request and inject the cross-node domain the filter resolved.
	req := BuildAllocationRequest(crossPodGangPod("main", "gangX", true, 2))
	req.GangDomainKey = domain
	newPod, rsn, err := NewAllocator(node1Info, nil).Allocate(req)
	require.NoError(t, err)
	require.Nil(t, rsn, "allocation unexpectedly rejected: %v", rsn)
	pre, has := util.HasAnnotation(newPod, util.PodVGPUPreAllocAnnotation)
	require.True(t, has)
	claim := device.PodDeviceClaim{}
	require.NoError(t, claim.UnmarshalText(pre))

	got := containerUUIDs(claim, "app")
	t.Logf("cross-node pod on node1 landed on: %v", keys(got))
	node1IslandB := map[string]bool{"N1-4": true, "N1-5": true, "N1-6": true, "N1-7": true}
	require.Len(t, got, 2)
	for u := range got {
		require.Truef(t, node1IslandB[u], "expected node1's ordinal-1 island {N1-4..7}, got %s", u)
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
