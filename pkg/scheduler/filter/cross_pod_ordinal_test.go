package filter

import (
	"fmt"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/allocator"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

// islandTopoAnn encodes an 8-GPU 2-NVLink-island layout: {0,1,2,3} and {4,5,6,7}
// are NVLink-connected within, PCIe-switch across. Island A (min index 0) is
// ordinal 0, island B (min index 4) is ordinal 1.
func islandTopoAnn(t *testing.T) string {
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
				m[j] = linkOf(i, j)
			}
		}
		topo[i] = device.TopologyInfo{Index: i, Links: m}
	}
	s, err := topo.Encode()
	require.NoError(t, err)
	return s
}

// islandNode builds a topology node whose cards are <name>-0..7, carrying the
// register/config/topology annotations NewNodeInfo parses on demand.
func islandNode(t *testing.T, name string) *corev1.Node {
	devs := make(device.NodeDeviceInfo, 8)
	for i := 0; i < 8; i++ {
		devs[i] = device.DeviceInfo{
			Id: i, Uuid: fmt.Sprintf("%s-%d", name, i), Type: "NVIDIA-A100",
			Core: util.HundredCore, Memory: 40960, Number: 10,
			Numa: -1, Capability: 8.0, Healthy: true,
		}
	}
	reg, err := devs.Encode()
	require.NoError(t, err)
	cfg, err := device.NodeConfigInfo{
		DeviceSplit: 10, CoresScaling: 1, MemoryFactor: 1, MemoryScaling: 1,
	}.Encode()
	require.NoError(t, err)
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				util.NodeDeviceRegisterAnnotation: reg,
				util.NodeConfigInfoAnnotation:     cfg,
				util.NodeDeviceTopologyAnnotation: islandTopoAnn(t),
			},
		},
	}
}

func ordinalSibling(uid, gang, node string, uuids ...string) *corev1.Pod {
	claim := "app["
	for i, u := range uuids {
		if i > 0 {
			claim += ","
		}
		claim += fmt.Sprintf("%d_%s_10_1024", i, u)
	}
	claim += "]"
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:         k8stypes.UID(uid),
			Name:        uid,
			Namespace:   namespace,
			Labels:      map[string]string{util.CoschedulingPodGroupLabel: gang},
			Annotations: map[string]string{util.PodVGPUPreAllocAnnotation: claim},
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

// Test_FindGangSiblingDomain covers both resolution paths, in particular the
// regression where a sibling on a NON-candidate node (built on demand) must still
// contribute its domain — the primary cross-node alignment scenario.
func Test_FindGangSiblingDomain(t *testing.T) {
	node := islandNode(t, "node2")
	candidateInfo, err := device.NewNodeInfo(node, device.WithGPUTopologyEnabled(true))
	require.NoError(t, err)

	// NodeLister knows the node, for the on-demand (non-candidate) build path.
	c := fake.NewClientset(node)
	f := informers.NewSharedInformerFactory(c, 0)
	nodeLister := f.Core().V1().Nodes().Lister()
	stop := make(chan struct{})
	defer close(stop)
	f.Start(stop)
	f.WaitForCacheSync(stop)

	req := &allocator.AllocationRequest{
		Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "self"}},
	}
	// Sibling holds two cards in island B (indices 4,5) → ordinal 1.
	sibling := ordinalSibling("sib", "gangX", "node2", "node2-4", "node2-5")

	t.Run("candidate-node sibling votes", func(t *testing.T) {
		dom, ok := FindGangSiblingDomain([]*corev1.Pod{sibling},
			map[string]*allocator.NodeInfo{"node2": {NodeInfo: candidateInfo}}, nodeLister, req)
		require.True(t, ok)
		require.Equal(t, "ord:1", dom)
	})

	t.Run("non-candidate sibling built on demand still votes (regression)", func(t *testing.T) {
		dom, ok := FindGangSiblingDomain([]*corev1.Pod{sibling},
			map[string]*allocator.NodeInfo{}, nodeLister, req) // empty candidate map
		require.True(t, ok, "sibling on a non-candidate node must contribute its domain")
		require.Equal(t, "ord:1", dom)
	})

	t.Run("self and no-prealloc are ignored", func(t *testing.T) {
		self := ordinalSibling("self", "gangX", "node2", "node2-4")
		noPre := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID:    "np",
				Labels: map[string]string{util.CoschedulingPodGroupLabel: "gangX"},
			},
			Spec: corev1.PodSpec{NodeName: "node2"},
		}
		_, ok := FindGangSiblingDomain([]*corev1.Pod{self, noPre},
			map[string]*allocator.NodeInfo{"node2": {NodeInfo: candidateInfo}}, nodeLister, req)
		require.False(t, ok)
	})
}
