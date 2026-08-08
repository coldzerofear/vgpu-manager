package allocator

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Head-to-head comparison against the algorithm this replaced.
//
// The tiered selector is only worth merging if it does not LOSE interconnect
// quality relative to the exhaustive partition search it replaces. On uniform
// fabrics that is provable by inspection (every set of N is interchangeable);
// on NON-uniform ones it is an empirical question, and the answer decides how
// much the exactness of any single hand-built fixture actually matters.
//
// bestEffortPolicy.Allocate is still in the tree, so both algorithms can be run
// over the identical candidate list and their results scored on the identical
// scale.
// ---------------------------------------------------------------------------

// setScoreOf sums pairwise link scores — the same measure both algorithms
// optimise, so it is the fair basis for comparison.
func setScoreOf(n *device.NodeInfo, devs []*device.Device) int {
	list := n.GetDeviceList()
	byUUID := make(map[string]*gpuallocator.Device, len(list))
	for _, d := range list {
		if d != nil {
			byUUID[d.UUID] = d
		}
	}
	total := 0
	for i := 0; i < len(devs); i++ {
		for j := i + 1; j < len(devs); j++ {
			a, aok := byUUID[devs[i].GetUUID()]
			b, bok := byUUID[devs[j].GetUUID()]
			if aok && bok {
				total += gpuallocator.PairScore(a, b)
			}
		}
	}
	return total
}

// oldAlgorithmPick reproduces what main did for a NonePolicy link request:
// hand bestEffort the deviceStore-ordered candidate list and take its set.
func oldAlgorithmPick(n *device.NodeInfo, store []*device.Device, need int) []*device.Device {
	uuids := make([]string, len(store))
	for i, d := range store {
		uuids[i] = d.GetUUID()
	}
	available, err := n.GetDeviceList().Filter(uuids)
	if err != nil {
		return nil
	}
	got := gpuallocator.NewBestEffortPolicy().Allocate(available, nil, need)
	if len(got) != need {
		return nil
	}
	byUUID := make(map[string]*device.Device, len(store))
	for _, d := range store {
		byUUID[d.GetUUID()] = d
	}
	out := make([]*device.Device, 0, need)
	for _, g := range got {
		// A nil entry here is the old algorithm returning its internal PADDING
		// as a real result — see Test_OldAlgorithm_CanReturnPadding. main
		// dereferences these unguarded (resolveLinkDevices reads p.UUID), so
		// this is a scheduler panic, not a bad placement. The harness reports
		// it instead of crashing.
		if g == nil {
			return nil
		}
		if d, ok := byUUID[g.UUID]; ok {
			out = append(out, d)
		}
	}
	return out
}

// oldAlgorithmReturnsPadding reports whether bestEffort handed back a set with
// nil entries for this input.
func oldAlgorithmReturnsPadding(n *device.NodeInfo, store []*device.Device, need int) bool {
	uuids := make([]string, len(store))
	for i, d := range store {
		uuids[i] = d.GetUUID()
	}
	available, err := n.GetDeviceList().Filter(uuids)
	if err != nil {
		return false
	}
	got := gpuallocator.NewBestEffortPolicy().Allocate(available, nil, need)
	for _, g := range got {
		if g == nil {
			return true
		}
	}
	return false
}

// newAlgorithmPick runs the tiered selector over the same list.
func newAlgorithmPick(n *device.NodeInfo, store []*device.Device, need int, policy string) []*device.Device {
	req := BuildAllocationRequest(linkPod(int64(need), false, policy))
	sorted := append([]*device.Device(nil), store...)
	NewDevicePolicyPriority(*req).Sort(sorted)
	plan := NewAllocator(n, nil).allocateTiered(req, sorted, need, nil)
	if plan == nil {
		return nil
	}
	return plan.Devices
}

type comparison struct {
	cases, better, equal, worse int
	worstDeficitPct             float64
	worstCase                   string
}

func (c *comparison) record(t *testing.T, label string, oldScore, newScore int) {
	t.Helper()
	c.cases++
	switch {
	case newScore > oldScore:
		c.better++
	case newScore == oldScore:
		c.equal++
	default:
		c.worse++
		if oldScore > 0 {
			deficit := float64(oldScore-newScore) / float64(oldScore) * 100
			if deficit > c.worstDeficitPct {
				c.worstDeficitPct, c.worstCase = deficit, label
			}
		}
	}
}

func (c *comparison) report(t *testing.T, name string) {
	t.Helper()
	t.Logf("%s: %d cases — new better %d, equal %d, WORSE %d (worst deficit %.1f%% @ %s)",
		name, c.cases, c.better, c.equal, c.worse, c.worstDeficitPct, c.worstCase)
}

// ---------------------------------------------------------------------------
// Known machine types
// ---------------------------------------------------------------------------

// Test_Compare_KnownMachines quantifies the change on every machine class the
// design targets, at every plausible request size and occupancy.
func Test_Compare_KnownMachines(t *testing.T) {
	builders := map[string]func(*testing.T, ...int) (*device.NodeInfo, []*device.Device){
		"nvswitch": nvswitchNode,
		"dgx1":     dgx1Node,
		"bridge":   bridgeNode,
		"pcie":     pcieNode,
	}
	names := make([]string, 0, len(builders))
	for k := range builders {
		names = append(names, k)
	}
	sort.Strings(names)

	overall := &comparison{}
	for _, name := range names {
		per := &comparison{}
		for _, used := range [][]int{{}, {0}, {1, 5}, {0, 3, 6}} {
			for _, need := range []int{2, 3, 4} {
				n, devs := builders[name](t, used...)
				store := make([]*device.Device, 0, len(devs))
				for _, d := range devs {
					if d.AllocatableNumber() > 0 {
						store = append(store, d)
					}
				}
				if len(store) < need {
					continue
				}
				oldPick := oldAlgorithmPick(n, store, need)
				newPick := newAlgorithmPick(n, store, need, "")
				require.NotNil(t, newPick, "%s/used=%v/need=%d: new algorithm must place", name, used, need)
				require.NotNil(t, oldPick, "%s/used=%v/need=%d: old algorithm must place", name, used, need)

				label := fmt.Sprintf("%s used=%v need=%d", name, used, need)
				per.record(t, label, setScoreOf(n, oldPick), setScoreOf(n, newPick))
				overall.record(t, label, setScoreOf(n, oldPick), setScoreOf(n, newPick))
			}
		}
		per.report(t, name)
		assert.Zero(t, per.worse, "%s: the tiered selector must never lose link quality", name)
	}
	overall.report(t, "TOTAL")
}

// ---------------------------------------------------------------------------
// Randomised topologies
// ---------------------------------------------------------------------------

// randomTopology builds an arbitrary NON-uniform link matrix. Real hardware is
// far more structured than this, which is the point: if the tiered selector
// holds up on arbitrary graphs it certainly holds on the handful of real ones,
// and the exactness of any single hand-transcribed fixture stops being load
// bearing.
func randomTopology(t *testing.T, rng *rand.Rand, n int, used []int) (*device.NodeInfo, []*device.Device) {
	t.Helper()
	devs := topoDevices(n, used...)
	palette := []links.P2PLinkType{
		links.P2PLinkCrossCPU, links.P2PLinkSameCPU, links.P2PLinkHostBridge,
		links.P2PLinkMultiSwitch, links.P2PLinkSingleSwitch,
		links.SingleNVLINKLink, links.TwoNVLINKLinks, links.FourNVLINKLinks,
	}
	var fl []device.FakeLink
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			// Occasionally omit an edge entirely, which is what forces the
			// spanning path and transitive-only components.
			if rng.Intn(10) == 0 {
				continue
			}
			fl = append(fl, device.FakeLink{A: i, B: j, Type: palette[rng.Intn(len(palette))]})
		}
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "rand"}}
	return device.NewFakeNodeInfoWithLinks(node, devs, fl...), devs
}

func Test_Compare_RandomTopologies(t *testing.T) {
	rng := rand.New(rand.NewSource(20260806)) // fixed seed: reproducible
	c := &comparison{}
	bn := &comparison{}
	var oldPadding, oldFailed int
	// 8 GPUs keeps the OLD algorithm affordable (it enumerates partitions);
	// that ceiling is exactly why it needed a threshold flag.
	for iter := 0; iter < 400; iter++ {
		size := []int{4, 6, 8}[rng.Intn(3)]
		var used []int
		for i := 0; i < size; i++ {
			if rng.Intn(4) == 0 {
				used = append(used, i)
			}
		}
		n, devs := randomTopology(t, rng, size, used)
		store := make([]*device.Device, 0, len(devs))
		for _, d := range devs {
			if d.AllocatableNumber() > 0 {
				store = append(store, d)
			}
		}
		need := 2 + rng.Intn(3)
		if len(store) < need {
			continue
		}
		newPick := newAlgorithmPick(n, store, need, "")
		require.NotNil(t, newPick, "iter=%d: the tiered selector must always place", iter)

		if oldAlgorithmReturnsPadding(n, store, need) {
			oldPadding++
			continue // no valid old result to compare against
		}
		oldPick := oldAlgorithmPick(n, store, need)
		if oldPick == nil {
			oldFailed++
			continue
		}
		label := fmt.Sprintf("iter=%d size=%d need=%d", iter, size, need)
		c.record(t, label, setScoreOf(n, oldPick), setScoreOf(n, newPick))
		bn.record(t, label, bottleneckOf(n, oldPick), bottleneckOf(n, newPick))
	}
	c.report(t, "random SUM      ")
	bn.report(t, "random BOTTLENECK")
	t.Logf("random: old algorithm returned PADDING (would panic the scheduler) in %d cases, "+
		"failed to place in %d; the tiered selector placed in every case", oldPadding, oldFailed)
	require.Positive(t, c.cases, "the sweep must actually compare something")
	// Deliberately NOT asserting zero regressions here.
	//
	// These are ARBITRARY graphs, and the two algorithms optimise different
	// things: the tier walk maximises the weakest link (a bottleneck objective),
	// bestEffort maximises the sum. Neither is "the truth" — PairScore is a
	// heuristic rank, not bandwidth — so on graphs that correspond to no real
	// hardware they disagree in both directions, and tuning either to win here
	// is fitting to a synthetic benchmark.
	//
	// The claim that matters is asserted in Test_Compare_KnownMachines: on every
	// machine class this design targets, the tiered selector is never worse.
	// This sweep exists to BOUND the disagreement (a few percent of cases, in
	// both directions) and to exercise shapes the fixtures cannot reach.
	assert.Less(t, float64(c.worse)/float64(c.cases), 0.05,
		"disagreement on arbitrary graphs should stay marginal; a jump here means "+
			"the tier walk lost a structural property, not that a heuristic shifted")
}

// Test_OldAlgorithm_CanReturnPadding documents the latent defect the random
// sweep uncovered in the algorithm being replaced.
//
// bestEffortPolicy pads the candidate list so it divides evenly into sets of
// the requested size, then captures the best PARTITION. Both the partition
// accumulator and the per-set buffer are reused across the iteration, so the
// captured partition aliases memory that keeps being rewritten — and the set it
// finally hands back can contain the nil padding rather than real devices.
//
// main dereferences the result without a nil check (resolveLinkDevices reads
// p.UUID), so this is a scheduler-wide panic in the Filter path, not merely a
// poor placement. It is reachable only on topologies where scores tie at zero,
// which is why it survived: the fixtures in the tree are all well-connected.
//
// The tiered selector cannot express this failure — it returns devices drawn
// from the caller's own slice and never synthesises entries.
func Test_OldAlgorithm_CanReturnPadding(t *testing.T) {
	rng := rand.New(rand.NewSource(20260806))
	found := 0
	for iter := 0; iter < 400 && found == 0; iter++ {
		size := []int{4, 6, 8}[rng.Intn(3)]
		n, devs := randomTopology(t, rng, size, nil)
		for need := 2; need <= 4 && need < size; need++ {
			if oldAlgorithmReturnsPadding(n, devs, need) {
				found++
				t.Logf("reproduced: size=%d need=%d — bestEffort returned a set containing nil", size, need)
				// The replacement must handle the very same input cleanly.
				require.NotNil(t, newAlgorithmPick(n, devs, need, ""),
					"the tiered selector must still place on this input")
			}
		}
	}
	require.Positive(t, found,
		"expected the sweep to reproduce the padding defect; if this stops failing, "+
			"re-check whether bestEffortPolicy was fixed upstream")
}

// ---------------------------------------------------------------------------
// Where the new algorithm is BETTER
// ---------------------------------------------------------------------------

// Test_Compare_PolicyAdherence measures the thing the redesign set out to fix.
// The old path (main's `linkTopKCandidates = 5` window — the name is main's,
// not this branch's) applied binpack/spread only as a tie-break among the top 5
// link-equivalent sets, so a policy-optimal choice outside that window was
// silently ignored. The tiered selector makes the policy dominate inside the
// chosen tier.
func Test_Compare_PolicyAdherence(t *testing.T) {
	// A node where many sets are link-equivalent (uniform NVSwitch) but the
	// cards differ sharply in occupancy, so binpack has a clear preference.
	devs := make([]*device.Device, 8)
	for i := 0; i < 8; i++ {
		// GPU-7 is the most consumed → binpack's first choice; it sorts LAST by
		// index, which is where the old top-K window loses it.
		usedNum := 0
		var usedCore, usedMem int64
		if i >= 5 {
			usedNum, usedCore, usedMem = 1, 50, 512
		}
		devs[i] = device.NewFakeDeviceWithUUID(fmt.Sprintf("GPU-%d", i), i,
			usedNum, 4, usedCore, 100, usedMem, 1024, -1)
	}
	var fl []device.FakeLink
	for i := 0; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			fl = append(fl, device.FakeLink{A: i, B: j, Type: links.EighteenNVLINKLinks})
		}
	}
	n := fixtureNode("uniform-uneven", devs, fl...)

	req := BuildAllocationRequest(linkPod(2, false, string(util.BinpackPolicy)))
	sorted := append([]*device.Device(nil), devs...)
	NewDevicePolicyPriority(*req).Sort(sorted)

	plan := NewAllocator(n, nil).allocateTiered(req, sorted, 2, nil)
	require.NotNil(t, plan)

	// binpack wants the warmest cards; the tiered selector must return exactly
	// the policy-ordered head, since every pair is link-equivalent here.
	assert.Equal(t, sorted[0].GetUUID(), plan.Devices[0].GetUUID())
	assert.Equal(t, sorted[1].GetUUID(), plan.Devices[1].GetUUID())
	for _, d := range plan.Devices {
		assert.Greater(t, d.GetUsedNumber(), 0,
			"binpack must consolidate onto already-used cards, not take fresh ones")
	}
}

// Test_Compare_Cost contrasts the search effort. The old algorithm enumerated
// PARTITIONS of the whole node — the growth that forced --best-effort-max-gpus.
func Test_Compare_Cost(t *testing.T) {
	for _, tc := range []struct{ n, k int }{{8, 2}, {8, 4}, {12, 4}, {16, 4}, {16, 8}} {
		combinations := combinationCount(tc.n, tc.k)
		t.Logf("n=%2d k=%d: combinations within a component = %d (capped at %d)",
			tc.n, tc.k, combinations, maxCombinationSearch)
	}
	// And the case that matters: on a uniform fabric the count is irrelevant
	// because no search runs at all.
	n, _ := nvswitchNode(t)
	assert.True(t, n.LinkTierIsUniform(device.TierNVLink),
		"uniform detection is what removes the search entirely on modern hardware")
}

// bottleneckOf is the WEAKEST pair score in a set — the objective the tier walk
// actually optimises, and the one that governs collective communication: a ring
// is as fast as its slowest hop. bestEffort optimised the SUM instead, so the
// two can disagree, and a set can score lower in total while being strictly
// better connected pair-for-pair.
func bottleneckOf(n *device.NodeInfo, devs []*device.Device) int {
	list := n.GetDeviceList()
	byUUID := make(map[string]*gpuallocator.Device, len(list))
	for _, d := range list {
		if d != nil {
			byUUID[d.UUID] = d
		}
	}
	worst := -1
	for i := 0; i < len(devs); i++ {
		for j := i + 1; j < len(devs); j++ {
			a, aok := byUUID[devs[i].GetUUID()]
			b, bok := byUUID[devs[j].GetUUID()]
			if !aok || !bok {
				continue
			}
			if s := gpuallocator.PairScore(a, b); worst < 0 || s < worst {
				worst = s
			}
		}
	}
	if worst < 0 {
		return 0
	}
	return worst
}
