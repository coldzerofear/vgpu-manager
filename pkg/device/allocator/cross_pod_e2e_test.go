package allocator

import (
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

	// Resolve the sibling's rail ordinal ON ITS OWN NODE (what the filter does).
	ordinal, ok := node2Info.OrdinalOfUUIDs([]string{"N2-4", "N2-5"})
	require.True(t, ok, "sibling ordinal must resolve on node2")
	require.Equal(t, 1, ordinal, "island B (min index 4) is ordinal 1")
	t.Logf("sibling on node2 island B resolved to ordinal %d", ordinal)

	// node1: a DIFFERENT node (distinct UUIDs), no same-node sibling.
	node1, devs1 := crossPodNode(t, "node1", "N1")
	node1Info, err := device.NewNodeInfo(node1,
		device.WithGPUTopologyEnabled(true),
		device.WithNodeDevice(&devs1))
	require.NoError(t, err)

	// Build the request and inject the cross-node ordinal the filter resolved.
	req := BuildAllocationRequest(crossPodGangPod("main", "gangX", true, 2))
	req.GangLinkOrdinal = ordinal
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
