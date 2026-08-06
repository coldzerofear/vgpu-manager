package allocator

import (
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Degenerate and adversarial inputs must degrade, never panic: this code runs
// inside the scheduler's Filter path, where a panic takes down scheduling for
// every pod, not just the offending one.
func Test_EdgeCases_NoPanic(t *testing.T) {
	empty := device.NewFakeNodeInfo(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "empty"}}, true)
	one := fixtureNode("one", topoDevices(1))
	pair := fixtureNode("pair", topoDevices(2),
		device.FakeLink{A: 0, B: 1, Type: links.SingleNVLINKLink})
	req := BuildAllocationRequest(linkPod(2, false, "binpack"))

	t.Run("zero and negative needNumber", func(t *testing.T) {
		for _, n := range []int{0, -1} {
			assert.NotPanics(t, func() {
				NewAllocator(pair, nil).allocateTiered(req, topoDevices(2), n, nil)
			})
		}
	})
	t.Run("node with no devices", func(t *testing.T) {
		assert.NotPanics(t, func() {
			assert.Nil(t, NewAllocator(empty, nil).allocateTiered(req, nil, 2, nil))
		})
	})
	t.Run("more needed than available", func(t *testing.T) {
		assert.Nil(t, NewAllocator(one, nil).allocateTiered(req, topoDevices(1), 4, nil))
	})
	t.Run("empty restrict window", func(t *testing.T) {
		assert.Nil(t, NewAllocator(pair, nil).allocateTiered(req, topoDevices(2), 2, sets.New[string]()))
	})
	t.Run("restrict window naming unknown devices", func(t *testing.T) {
		assert.Nil(t, NewAllocator(pair, nil).allocateTiered(req, topoDevices(2), 2, sets.New("ghost")))
	})
	t.Run("nil devices inside the store", func(t *testing.T) {
		// filterDevices never emits nils, but allocateTiered is the contract
		// boundary: it normalises the candidate set so every helper below can
		// dereference freely. Exercised through the entry point rather than a
		// helper, because "helpers tolerate nil" is deliberately NOT the
		// contract — "the entry point strips them" is.
		devs := topoDevices(2)
		withNils := []*device.Device{nil, devs[0], nil, devs[1], nil}
		var plan *linkPlan
		assert.NotPanics(t, func() {
			plan = NewAllocator(pair, nil).allocateTiered(req, withNils, 2, nil)
		})
		require.NotNil(t, plan, "the two real devices must still be allocatable")
		require.Len(t, plan.Devices, 2)
		for _, d := range plan.Devices {
			assert.NotNil(t, d)
		}
	})
	t.Run("empty policy runs", func(t *testing.T) {
		assert.Nil(t, policyRuns(req, nil))
	})
	t.Run("subset search degenerate sizes", func(t *testing.T) {
		devs := []*gpuallocator.Device{gpuallocator.NewDevice(0, "a", "")}
		for _, n := range []int{0, -1, 5} {
			score, best := searchBestSubset(devs, n)
			assert.Equal(t, 0, score)
			assert.Nil(t, best)
		}
	})
	t.Run("combination enumeration terminates on degenerate input", func(t *testing.T) {
		for _, tc := range [][2]int{{0, 0}, {3, 0}, {3, -1}, {3, 5}, {0, 2}} {
			count := 0
			forEachCombination(tc[0], tc[1], func([]int) { count++ })
			assert.Zero(t, count, "n=%d k=%d must enumerate nothing", tc[0], tc[1])
		}
	})
}

// A node whose GPUs have NO links at all: every card is its own component at
// every tier, so no component can host a 2-card request and the request is
// satisfied by SPANNING singletons at the loosest tier.
//
// The result is deliberately main-compatible. There, bestEffort scored every
// partition at zero, kept the first, and returned deviceStore[:2] as a success
// with no TopologyFallback event. Spanning produces the same cards and the same
// signal, so link-mode pods on link-less nodes behave exactly as before.
//
// Spanned is what keeps strict honest: an unconnected set can never satisfy
// link-strict, and acceptable() rejects it on that flag alone.
func Test_LinklessNode_SpansAtLoosestTier(t *testing.T) {
	n := fixtureNode("linkless", topoDevices(4)) // topology enabled, zero edges
	req := BuildAllocationRequest(linkPod(2, false, ""))
	store := topoDevices(4)

	plan := NewAllocator(n, nil).allocateTiered(req, store, 2, nil)
	require.NotNil(t, plan)
	assert.True(t, plan.Spanned, "unconnected cards must be flagged as spanned")
	assert.Equal(t, device.TierAny, plan.Tier)
	require.Len(t, plan.Devices, 2)
	assert.Equal(t, store[0].GetUUID(), plan.Devices[0].GetUUID(), "same cards main would have picked")
	assert.Equal(t, store[1].GetUUID(), plan.Devices[1].GetUUID())

	// link-strict must refuse the very same plan.
	strictReq := BuildAllocationRequest(linkPod(2, true, ""))
	_, rsn, err := NewAllocator(fixtureNode("linkless", topoDevices(4)), nil).Allocate(strictReq)
	require.NoError(t, err)
	require.NotNil(t, rsn, "strict cannot accept a set with no connectivity")
}
