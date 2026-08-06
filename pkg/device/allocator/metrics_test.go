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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// numaFixture is a 4-GPU node split 2+2 across NUMA nodes, with no link
// topology (numa mode reads Device.Numa, a different data source).
func numaFixture() *device.NodeInfo {
	return device.NewFakeNodeInfo(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "numa-node"}}, false,
		device.NewFakeDeviceWithUUID("GPU-0", 0, 0, 1, 0, 100, 0, 1024, 0),
		device.NewFakeDeviceWithUUID("GPU-1", 1, 0, 1, 0, 100, 0, 1024, 0),
		device.NewFakeDeviceWithUUID("GPU-2", 2, 0, 1, 0, 100, 0, 1024, 1),
		device.NewFakeDeviceWithUUID("GPU-3", 3, 0, 1, 0, 100, 0, 1024, 1),
	)
}

func numaPod(number int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Annotations: map[string]string{
			util.DeviceTopologyModeAnnotation: string(util.NUMATopology)}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{vgpuContainer("c", number, 100, 1024)}},
	}
}

// linkSearchCount reads the live value of the search counter across all algos.
func linkSearchCount(t *testing.T) float64 {
	t.Helper()
	families, err := metrics.Registry().Gather()
	require.NoError(t, err)
	total := 0.0
	for _, f := range families {
		if f.GetName() != "vgpu_scheduler_link_search_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// The topology outcome recorded on the request IS what the filter turns into
// the per-pod metric, so its accuracy is the metric's accuracy.
func Test_TopologyOutcome_RecordsAchievedConnectivity(t *testing.T) {
	t.Run("nvswitch reaches nvlink", func(t *testing.T) {
		n, _ := nvswitchNode(t)
		req := BuildAllocationRequest(linkPod(4, false, ""))
		_, rsn, err := NewAllocator(n, nil).Allocate(req)
		require.NoError(t, err)
		require.Nil(t, rsn)
		assert.Equal(t, metrics.ResultNVLink, req.TopologyOutcome().Result)
	})

	t.Run("bridged board cannot give 4 NVLink cards", func(t *testing.T) {
		n, _ := bridgeNode(t)
		req := BuildAllocationRequest(linkPod(4, false, ""))
		_, rsn, err := NewAllocator(n, nil).Allocate(req)
		require.NoError(t, err)
		require.Nil(t, rsn)
		got := req.TopologyOutcome().Result
		assert.NotEqual(t, metrics.ResultNVLink, got)
		assert.Contains(t, []string{metrics.ResultSwitch, metrics.ResultNUMA, metrics.ResultAny}, got)
	})

	t.Run("link-less node reports spanned, not success", func(t *testing.T) {
		n := fixtureNode("linkless", topoDevices(4))
		req := BuildAllocationRequest(linkPod(2, false, ""))
		_, rsn, err := NewAllocator(n, nil).Allocate(req)
		require.NoError(t, err)
		require.Nil(t, rsn)
		assert.Equal(t, metrics.ResultSpanned, req.TopologyOutcome().Result)
	})

	t.Run("node without topology data reports none", func(t *testing.T) {
		n, _ := fakeNode(4, 1, 100, 1024)
		req := BuildAllocationRequest(linkPod(2, false, ""))
		_, rsn, err := NewAllocator(n, nil).Allocate(req)
		require.NoError(t, err)
		require.Nil(t, rsn)
		assert.Equal(t, metrics.ResultNone, req.TopologyOutcome().Result)
	})

	t.Run("numa satisfied", func(t *testing.T) {
		req := BuildAllocationRequest(numaPod(2))
		_, rsn, err := NewAllocator(numaFixture(), nil).Allocate(req)
		require.NoError(t, err)
		require.Nil(t, rsn)
		assert.Equal(t, metrics.ResultNUMA, req.TopologyOutcome().Result)
	})

	t.Run("numa degraded to cross-numa", func(t *testing.T) {
		// The node is 2+2, so 3 cards cannot stay inside one NUMA node.
		req := BuildAllocationRequest(numaPod(3))
		_, rsn, err := NewAllocator(numaFixture(), nil).Allocate(req)
		require.NoError(t, err)
		require.Nil(t, rsn)
		assert.Equal(t, metrics.ResultCrossNUMA, req.TopologyOutcome().Result)
	})
}

// A pod is only as well placed as its unluckiest container, so the reported
// outcome must be the WORST across them — otherwise a pod with one good and one
// degraded container reads as fully satisfied.
func Test_TopologyOutcome_KeepsWorstAcrossContainers(t *testing.T) {
	req := &AllocationRequest{}
	req.recordTopologyOutcome(metrics.ResultNVLink, "")
	assert.Equal(t, metrics.ResultNVLink, req.TopologyOutcome().Result)

	req.recordTopologyOutcome(metrics.ResultSwitch, "")
	assert.Equal(t, metrics.ResultSwitch, req.TopologyOutcome().Result, "worse must win")

	req.recordTopologyOutcome(metrics.ResultNVLink, "")
	assert.Equal(t, metrics.ResultSwitch, req.TopologyOutcome().Result, "better must not overwrite")
}

// Preemption re-runs allocation once per victim set it tests. Counting those
// dry runs would report a single pod as many placements, so they must record
// nothing at all.
func Test_Simulation_RecordsNothing(t *testing.T) {
	n, _ := dgx1Node(t) // non-uniform, so a search WOULD be counted
	before := linkSearchCount(t)

	req := BuildAllocationRequest(linkPod(4, false, ""))
	_, rsn, err := NewSimulationAllocator(n).Allocate(req)
	require.NoError(t, err)
	require.Nil(t, rsn)

	assert.Empty(t, req.TopologyOutcome().Result, "a simulation places nothing")
	assert.Equal(t, before, linkSearchCount(t), "a simulation must not count searches")
}

// The search counter answers "is the combinatorial path ever taken on this
// fleet?". That only works if uniform fabrics contribute zero.
func Test_LinkSearch_CountedOnlyWhenExecuted(t *testing.T) {
	before := linkSearchCount(t)

	nv, _ := nvswitchNode(t)
	_, _, err := NewAllocator(nv, nil).Allocate(BuildAllocationRequest(linkPod(4, false, "")))
	require.NoError(t, err)
	require.Equal(t, before, linkSearchCount(t), "a uniform fabric runs no search")

	dgx, _ := dgx1Node(t)
	_, _, err = NewAllocator(dgx, nil).Allocate(BuildAllocationRequest(linkPod(4, false, "")))
	require.NoError(t, err)
	assert.Greater(t, linkSearchCount(t), before, "a non-uniform component runs one")
}

func Test_LinkResult_MapsEveryTier(t *testing.T) {
	for tier, want := range map[device.LinkTier]string{
		device.TierNVLink: metrics.ResultNVLink,
		device.TierSwitch: metrics.ResultSwitch,
		device.TierNUMA:   metrics.ResultNUMA,
		device.TierAny:    metrics.ResultAny,
	} {
		assert.Equal(t, want, linkResult(&linkPlan{Tier: tier}), fmt.Sprint(tier))
		assert.Equal(t, metrics.ResultSpanned, linkResult(&linkPlan{Tier: tier, Spanned: true}),
			"spanning outranks the tier: there is no single connectivity level to report")
	}
}
