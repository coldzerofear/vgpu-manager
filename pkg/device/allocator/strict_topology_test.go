package allocator

import (
	"fmt"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file drives the allocator through device.NewNodeInfo and REAL node
// annotations rather than the in-memory fixtures the rest of the suite uses.
// Both bugs it pins were reported from a live cluster and neither was reachable
// through the fixture helpers: one depended on how the register annotation
// encodes an absent NUMA node (-1), the other on the topology annotation being
// present-but-weak, which the fixtures cannot express because they take link
// lists directly.

func annotatedNode(t *testing.T, devicesJSON, topologyJSON string) *device.NodeInfo {
	t.Helper()
	ann := map[string]string{
		util.NodeConfigInfoAnnotation:     `{"deviceSplit":10,"coresScaling":2,"memoryFactor":1,"memoryScaling":2}`,
		util.NodeDeviceRegisterAnnotation: devicesJSON,
	}
	if topologyJSON != "" {
		ann[util.NodeDeviceTopologyAnnotation] = topologyJSON
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "annotated", Annotations: ann},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("40")},
			Allocatable: corev1.ResourceList{
				corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("40")},
		},
	}
	n, err := device.NewNodeInfo(node, device.WithGPUTopologyEnabled(true))
	require.NoError(t, err)
	return n
}

func registerJSON(numa ...int) string {
	out := "["
	for i, na := range numa {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf(`{"id":%d,"type":"T","uuid":"GPU-%d","core":200,"memory":8192,`+
			`"number":10,"numa":%d,"mig":false,"busId":"0000:0%d:00.0","capability":6.1,"healthy":true}`,
			i, i, na, i+1)
	}
	return out + "]"
}

func topologyPod(mode util.TopologyMode, number int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default",
			Annotations: map[string]string{util.DeviceTopologyModeAnnotation: string(mode)}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "c1",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", number)),
				corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse("50"),
				corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse("1024"),
			}},
		}}},
	}
}

func scheduled(t *testing.T, n *device.NodeInfo, pod *corev1.Pod) bool {
	t.Helper()
	_, rsn, err := NewAllocator(n, nil).Allocate(BuildAllocationRequest(pod))
	require.NoError(t, err)
	return rsn == nil
}

// Test_Strict_SingleCandidateDevice reproduces the reported failure: a node with
// exactly ONE GPU (numa: -1, topology annotation present but with an empty links
// map) accepted both link-strict and numa-strict 1-GPU pods.
//
// The cause was not in the topology algorithms — neither ran. pickDeviceClaims
// carried a `needNumber == len(deviceStore) && (... || needNumber <= 1)` fast
// path that returned before dispatching on the topology mode at all, so strict
// was never evaluated. The trigger is not "single-GPU node" but "exactly one
// CANDIDATE device survives filtering", which a large node also hits once all
// but one of its cards are full.
func Test_Strict_SingleCandidateDevice(t *testing.T) {
	const emptyLinks = `[{"index":0,"links":{}}]`

	t.Run("link-strict rejects", func(t *testing.T) {
		n := annotatedNode(t, registerJSON(-1), emptyLinks)
		assert.False(t, scheduled(t, n, topologyPod(util.LinkTopologyStrict, 1)))
	})

	t.Run("numa-strict rejects", func(t *testing.T) {
		n := annotatedNode(t, registerJSON(-1), emptyLinks)
		assert.False(t, scheduled(t, n, topologyPod(util.NUMATopologyStrict, 1)))
	})

	// Guards against over-correcting. Rejecting more than the contract requires
	// would be just as wrong, and harder to notice.
	t.Run("numa-strict still accepts a node that HAS numa", func(t *testing.T) {
		n := annotatedNode(t, registerJSON(0), "")
		assert.True(t, scheduled(t, n, topologyPod(util.NUMATopologyStrict, 1)))
	})

	t.Run("non-strict link still places", func(t *testing.T) {
		n := annotatedNode(t, registerJSON(-1), emptyLinks)
		assert.True(t, scheduled(t, n, topologyPod(util.LinkTopology, 1)))
	})

	t.Run("none mode unaffected", func(t *testing.T) {
		n := annotatedNode(t, registerJSON(-1), "")
		assert.True(t, scheduled(t, n, topologyPod(util.NoneTopology, 1)))
	})
}

// Test_Strict_TopologyPresentButNoNVLink covers the follow-up case: the topology
// annotation is present AND non-empty, but every link is PCIe-class.
//
// This is materially different from the empty-links node above. EnabledGPUTopology
// is set by the presence of ANY link, so here HasGPUTopology() is true and the
// tier walk really does run — it just has nothing at NVLink strength to find.
// Multi-card strict already rejected correctly; the one-card case did not,
// because a component of one device has no pairs and so reads as connected at
// every tier including NVLink.
func Test_Strict_TopologyPresentButNoNVLink(t *testing.T) {
	// P2PLinkCrossCPU=2 and P2PLinkSingleSwitch=4 are PCIe-class;
	// SingleNVLINKLink=6 upward is NVLink.
	pcieOnly := `[{"index":0,"links":{"1":[4],"2":[2],"3":[2]}},` +
		`{"index":1,"links":{"0":[4],"2":[2],"3":[2]}},` +
		`{"index":2,"links":{"0":[2],"1":[2],"3":[4]}},` +
		`{"index":3,"links":{"0":[2],"1":[2],"2":[4]}}]`
	withNVLink := `[{"index":0,"links":{"1":[7],"2":[2],"3":[2]}},` +
		`{"index":1,"links":{"0":[7],"2":[2],"3":[2]}},` +
		`{"index":2,"links":{"0":[2],"1":[2],"3":[4]}},` +
		`{"index":3,"links":{"0":[2],"1":[2],"2":[4]}}]`
	reg := registerJSON(0, 0, 1, 1)

	t.Run("pcie-only rejects 1 card", func(t *testing.T) {
		n := annotatedNode(t, reg, pcieOnly)
		require.True(t, n.HasGPUTopology(), "premise: non-empty links means topology IS enabled")
		assert.False(t, scheduled(t, n, topologyPod(util.LinkTopologyStrict, 1)))
	})

	t.Run("pcie-only rejects 2 cards", func(t *testing.T) {
		n := annotatedNode(t, reg, pcieOnly)
		assert.False(t, scheduled(t, n, topologyPod(util.LinkTopologyStrict, 2)))
	})

	t.Run("real nvlink accepts 1 card", func(t *testing.T) {
		n := annotatedNode(t, reg, withNVLink)
		assert.True(t, scheduled(t, n, topologyPod(util.LinkTopologyStrict, 1)))
	})

	t.Run("real nvlink accepts 2 cards", func(t *testing.T) {
		n := annotatedNode(t, reg, withNVLink)
		assert.True(t, scheduled(t, n, topologyPod(util.LinkTopologyStrict, 2)))
	})

	t.Run("non-strict places on pcie-only and reports the degradation", func(t *testing.T) {
		n := annotatedNode(t, reg, pcieOnly)
		req := BuildAllocationRequest(topologyPod(util.LinkTopology, 2))
		_, rsn, err := NewAllocator(n, nil).Allocate(req)
		require.NoError(t, err)
		require.Nil(t, rsn, "non-strict must still place")
		assert.NotEmpty(t, req.TopologyOutcome().Result)
		assert.NotEqual(t, "nvlink", req.TopologyOutcome().Result,
			"there is no NVLink here, so reporting nvlink would be a false success")
	})
}
