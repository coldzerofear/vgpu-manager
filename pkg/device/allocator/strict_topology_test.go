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

import (
	"fmt"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/metrics"
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

// Test_SingleCard_NeverReportsSpanned guards a label, not a placement.
//
// "Spanned" asserts that a set has members sitting in different components. One
// device is always in exactly one component, so a single-card plan can never be
// spanned — yet once single cards started walking the tier ladder, a card with
// no links at all fell through every tier into the spanning branch and came back
// Spanned=true. That surfaces as result="spanned" on topology_placement_total
// and as "any (spanning multiple components)" in the downgrade event, both
// describing something that cannot happen.
//
// The honest outcome for that card is ResultNone: topology placed nothing.
func Test_SingleCard_NeverReportsSpanned(t *testing.T) {
	n := fixtureNode("linkless", topoDevices(4))

	req := BuildAllocationRequest(linkPod(1, false, ""))
	_, rsn, err := NewAllocator(n, nil).Allocate(req)
	require.NoError(t, err)
	require.Nil(t, rsn, "non-strict must still place the card")
	assert.Equal(t, metrics.ResultNone, req.TopologyOutcome().Result,
		"a lone card on a linkless node was not placed by topology; it did not span anything")

	// Two cards on the same node genuinely DO span, so the label still works
	// where it applies — this fix must not blanket-disable spanning.
	req2 := BuildAllocationRequest(linkPod(2, false, ""))
	n2 := fixtureNode("linkless", topoDevices(4))
	_, rsn2, err := NewAllocator(n2, nil).Allocate(req2)
	require.NoError(t, err)
	require.Nil(t, rsn2)
	assert.Equal(t, metrics.ResultSpanned, req2.TopologyOutcome().Result)
}

// Test_NUMA_UnknownAffinityIsNotANumaNode covers a defect that predates the
// strict-mode work but is the same mistake in a different place: treating a
// sentinel as a real value.
//
// The device plugin writes numa: -1 when it cannot determine affinity. Grouping
// by the raw value put every such card into a shared "-1" bucket, which
// CanNotCrossNumaNode then reported as a NUMA node like any other — so
// numa-strict, whose contract is "these GPUs share one NUMA node", was satisfied
// by a group whose defining property is that nobody knows what node they are on.
//
// The sub-test below also pins DETERMINISM. DefaultCallback used to range over
// the grouping map, so which NUMA node won depended on Go's randomised map
// iteration: measured at ~1 run in 6 landing on the unknown-affinity card.
func Test_NUMA_UnknownAffinityIsNotANumaNode(t *testing.T) {
	t.Run("all cards unknown → numa-strict rejects", func(t *testing.T) {
		n := annotatedNode(t, registerJSON(-1, -1), "")
		assert.False(t, scheduled(t, n, topologyPod(util.NUMATopologyStrict, 1)))
	})

	t.Run("mixed → strict only ever uses the known-affinity card", func(t *testing.T) {
		// Repeated because the bug was probabilistic: a single run passed ~5/6
		// of the time even while broken.
		for i := 0; i < 200; i++ {
			n := annotatedNode(t, registerJSON(-1, 0), "")
			pod, rsn, err := NewAllocator(n, nil).Allocate(
				BuildAllocationRequest(topologyPod(util.NUMATopologyStrict, 1)))
			require.NoError(t, err)
			require.Nil(t, rsn)
			claim, ok := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
			require.True(t, ok)
			require.Contains(t, claim, "GPU-1",
				"iteration %d picked the numa=-1 card", i)
		}
	})

	t.Run("non-strict is unaffected and still places", func(t *testing.T) {
		n := annotatedNode(t, registerJSON(-1, -1), "")
		assert.True(t, scheduled(t, n, topologyPod(util.NUMATopology, 1)))
	})
}

// Test_NUMA_DefaultCallbackIsDeterministic pins the ordering of the
// no-device-policy NUMA path on a node with SEVERAL valid NUMA nodes.
//
// It needs its own fixture: on a mixed known/unknown node the -1 filter already
// leaves a single group, so iteration order becomes unobservable there and that
// test cannot see this bug. Here both NUMA nodes are real and equally eligible,
// which is exactly when ranging over the grouping map let Go's randomised
// iteration decide the placement.
//
// Identical inputs must produce an identical decision. Beyond surprising users,
// instability here is a correctness problem for the extender: Filter is re-run
// during preemption and rescheduling, so a pod could be validated against one
// NUMA node and then placed on another.
func Test_NUMA_DefaultCallbackIsDeterministic(t *testing.T) {
	// Two NUMA nodes, two cards each, no device policy → nothing expresses a
	// preference between them.
	first := ""
	for i := 0; i < 200; i++ {
		n := annotatedNode(t, registerJSON(0, 0, 1, 1), "")
		pod, rsn, err := NewAllocator(n, nil).Allocate(
			BuildAllocationRequest(topologyPod(util.NUMATopology, 2)))
		require.NoError(t, err)
		require.Nil(t, rsn)
		claim, ok := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
		require.True(t, ok)
		if first == "" {
			first = claim
			continue
		}
		require.Equal(t, first, claim, "iteration %d chose a different NUMA node", i)
	}
	t.Logf("stable choice across 200 runs: %s", first)
}
