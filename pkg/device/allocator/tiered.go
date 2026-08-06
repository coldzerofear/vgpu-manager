package allocator

import (
	"sort"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/klog/v2"
)

// maxCombinationSearch bounds the in-component combinatorial search.
//
// It is an INTERNAL safety valve, not a tunable: on every known machine type
// the search either does not run at all (uniform fabrics — NVSwitch, and every
// tier of a bridged or pure-PCIe node) or runs on a component of at most 8
// cards, where C(8,4)=70 is the worst case. The bound exists only so that some
// hypothetical future non-uniform 32-GPU fabric degrades to a greedy pick
// instead of stalling the scheduler.
//
// This replaces the former --best-effort-max-gpus flag, which had to be a user
// knob because the old algorithm enumerated PARTITIONS of the whole node
// (2.6M for 16-choose-4) rather than combinations within one component.
const maxCombinationSearch = 50000

// tierLadder is walked tightest → loosest.
var tierLadder = [...]device.LinkTier{
	device.TierNVLink, device.TierSwitch, device.TierNUMA, device.TierAny,
}

// linkPlan is the outcome of a tiered link allocation.
type linkPlan struct {
	// Devices are the chosen cards, ALWAYS a subsequence of the input
	// deviceStore (invariant I2 — the topology layer filters, it never sorts).
	Devices []*device.Device
	// Tier is the interconnect level the set is connected at. strict-link
	// requires TierNVLink.
	Tier device.LinkTier
	// Spanned is true when no single component could host the request and the
	// set had to span several components at Tier.
	Spanned bool
}

// allocateTiered picks needNumber devices from deviceStore using the node's
// tiered connectivity view.
//
// The whole design in three lines:
//
//	tier walk   — find the TIGHTEST interconnect level that can host the request
//	group pick  — among components at that level, device policy first, link
//	              quality as the tie-break
//	member pick — within the component, walk policy-equal runs in deviceStore
//	              order; only the run that overflows needs a link-quality search
//
// Returns nil when the node cannot host the request at any tier, which the
// caller turns into a strict rejection or a non-topology fallback.
//
// deviceStore is READ-ONLY here (invariant I1): every candidate list is a fresh
// slice, so the caller's policy ordering survives intact for the fallback path.
func (alloc *allocator) allocateTiered(
	req *AllocationRequest, deviceStore []*device.Device, needNumber int,
	restrictUUIDs map[string]struct{},
) *linkPlan {

	if needNumber <= 0 || !alloc.nodeInfo.HasGPUTopology() {
		return nil
	}
	candidates := deviceStore
	if restrictUUIDs != nil {
		candidates = filterToUUIDs(deviceStore, restrictUUIDs)
	}
	if len(candidates) < needNumber {
		return nil
	}

	for _, tier := range tierLadder {
		groups := groupByComponent(alloc.nodeInfo, tier, candidates)
		if len(groups) == 0 {
			continue
		}
		// Prefer a single component: it is by definition better connected than
		// any set spanning several at the same tier.
		if fitting := groupsWithAtLeast(groups, needNumber); len(fitting) > 0 {
			best := alloc.pickGroup(req, fitting, tier, needNumber)
			picked := alloc.pickMembers(req, best.devices, tier, needNumber)
			if len(picked) == needNumber {
				return &linkPlan{Devices: picked, Tier: tier}
			}
		}
		// No single component fits: span components at this tier, taking from
		// the largest first. Proven optimal on a tier hierarchy — see the design
		// doc §4.2. Only meaningful at the LOOSEST tier in practice, because a
		// tighter tier's leftovers are always reachable at a looser one.
		if tier == device.TierAny {
			if picked := alloc.spanGroups(req, groups, tier, needNumber); len(picked) == needNumber {
				return &linkPlan{Devices: picked, Tier: tier, Spanned: true}
			}
		}
	}
	return nil
}

// componentGroup is one connectivity component's candidate members, in
// deviceStore order.
type componentGroup struct {
	root    int
	devices []*device.Device
}

// groupByComponent buckets candidates by their component at the given tier,
// PRESERVING deviceStore order inside each bucket (invariant I2). Groups are
// returned in ascending root order so selection is deterministic.
func groupByComponent(n *device.NodeInfo, tier device.LinkTier, candidates []*device.Device) []componentGroup {
	byRoot := make(map[int][]*device.Device, len(candidates))
	for _, d := range candidates {
		root, ok := n.LinkComponentOf(tier, d.GetUUID())
		if !ok {
			continue
		}
		byRoot[root] = append(byRoot[root], d)
	}
	groups := make([]componentGroup, 0, len(byRoot))
	for root, devices := range byRoot {
		groups = append(groups, componentGroup{root: root, devices: devices})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].root < groups[j].root })
	return groups
}

func groupsWithAtLeast(groups []componentGroup, n int) []componentGroup {
	out := make([]componentGroup, 0, len(groups))
	for _, g := range groups {
		if len(g.devices) >= n {
			out = append(out, g)
		}
	}
	return out
}

// pickGroup chooses among components that can all host the request.
//
// Sort keys, in order:
//  1. device policy score of the group (descending — Score already encodes the
//     binpack/spread direction, so higher is always more preferred)
//  2. best achievable link score for needNumber members (descending)
//  3. root (ascending) for determinism
//
// Key 2 is what makes this correct on a FRESH node: every group then has
// identical utilisation, so key 1 ties and the tie is the common case, not an
// edge case. Without key 2 the choice would fall through to an arbitrary root
// and all topology information would be discarded exactly when the node is
// empty enough for it to matter most.
//
// Under NonePolicy Score() is 0 for every group, so key 1 ties universally and
// link quality decides everything — matching the historical behaviour without a
// separate code path.
func (alloc *allocator) pickGroup(
	req *AllocationRequest, groups []componentGroup, tier device.LinkTier, needNumber int,
) componentGroup {
	if len(groups) == 1 {
		return groups[0]
	}
	type scored struct {
		group       componentGroup
		policyScore float64
		linkScore   int
	}
	all := make([]scored, len(groups))
	for i, g := range groups {
		all[i] = scored{
			group:       g,
			policyScore: Score(NumaUtilization(g.devices), req.Profile, req.DevicePolicy),
			linkScore:   alloc.bestLinkScore(g.devices, tier, needNumber),
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].policyScore != all[j].policyScore {
			return all[i].policyScore > all[j].policyScore
		}
		if all[i].linkScore != all[j].linkScore {
			return all[i].linkScore > all[j].linkScore
		}
		return all[i].group.root < all[j].group.root
	})
	return all[0].group
}

// bestLinkScore returns the highest achievable pair-score sum for needNumber
// members of the group.
//
// On a uniform tier this is closed-form — every pair inside a component has the
// same score, so any needNumber members give C(k,2) × pairScore and no search
// is needed. That closed form is what keeps large NVSwitch nodes free of
// combinatorics entirely.
func (alloc *allocator) bestLinkScore(devices []*device.Device, tier device.LinkTier, needNumber int) int {
	if len(devices) < needNumber || needNumber < 2 {
		return 0
	}
	linked := alloc.toLinkDevices(devices)
	if alloc.nodeInfo.LinkTierIsUniform(tier) {
		pair := gpuallocator.PairScore(linked[0], linked[1])
		return pair * needNumber * (needNumber - 1) / 2
	}
	best, _ := searchBestSubset(linked, needNumber)
	return best
}

// pickMembers chooses needNumber devices from one component.
//
// The rule is "device policy first, link quality as the tie-break", applied via
// POLICY-EQUAL RUNS. deviceStore is already sorted by policy, so devices with
// equal policy score are adjacent; whole runs are taken in order and only the
// run that would overflow needs a decision. That decision is where link quality
// applies.
//
// Two important degenerate cases fall out of the same code:
//   - NonePolicy: every device has policy score 0, so the component is ONE run
//     and link quality decides the entire selection — the historical behaviour.
//   - Fresh node under binpack/spread: all cards equally free, so again one run
//     and link quality decides.
func (alloc *allocator) pickMembers(
	req *AllocationRequest, devices []*device.Device, tier device.LinkTier, needNumber int,
) []*device.Device {

	if len(devices) < needNumber {
		return nil
	}
	picked := make([]*device.Device, 0, needNumber)
	for _, run := range policyRuns(req, devices) {
		remaining := needNumber - len(picked)
		if remaining == 0 {
			break
		}
		if len(run) <= remaining {
			// Whole run fits: policy strictly prefers all of it over anything
			// later, so link quality never enters.
			picked = append(picked, run...)
			continue
		}
		// This run overflows — pick `remaining` of it by link quality.
		picked = append(picked, alloc.pickWithinRun(run, tier, remaining, picked)...)
		break
	}
	if len(picked) != needNumber {
		return nil
	}
	// Restore deviceStore order: pickWithinRun may return members out of order,
	// and invariant I2 requires the result to be a SUBSEQUENCE of deviceStore.
	return inStoreOrder(picked, devices)
}

// pickWithinRun selects `need` devices from a policy-equal run, maximising link
// quality against the already-picked set plus each other.
//
// On a uniform tier all members are interchangeable, so the deviceStore-order
// prefix is taken directly — no search. Otherwise a bounded combination search
// runs; the bound is never reached on any known hardware (see
// maxCombinationSearch).
func (alloc *allocator) pickWithinRun(
	run []*device.Device, tier device.LinkTier, need int, alreadyPicked []*device.Device,
) []*device.Device {

	if need >= len(run) {
		return run
	}
	if alloc.nodeInfo.LinkTierIsUniform(tier) {
		return run[:need]
	}
	linkedRun := alloc.toLinkDevices(run)
	linkedFixed := alloc.toLinkDevices(alreadyPicked)
	if combinationCount(len(run), need) > maxCombinationSearch {
		klog.V(4).InfoS("Link selection exceeded combination budget, using greedy",
			"node", alloc.nodeInfo.GetName(), "candidates", len(run), "need", need)
		return fromLinkDevices(greedyPick(linkedRun, linkedFixed, need), run)
	}
	_, best := searchBestSubsetWithFixed(linkedRun, linkedFixed, need)
	return fromLinkDevices(best, run)
}

// spanGroups takes members across several components when none can host the
// request alone.
//
// Greedy by descending capacity is OPTIMAL on a tier hierarchy: taking aᵢ from
// component i yields Σ internal(aᵢ) + parentScore × (C(k,2) − Σ C(aᵢ,2)), and
// since internal(aᵢ) ≥ parentScore × C(aᵢ,2), concentrating in fewer components
// always scores at least as high. See the design doc §4.2.
func (alloc *allocator) spanGroups(
	req *AllocationRequest, groups []componentGroup, tier device.LinkTier, needNumber int,
) []*device.Device {

	ordered := make([]componentGroup, len(groups))
	copy(ordered, groups)
	// Descending capacity; ties broken by the group ordering rule so the choice
	// stays deterministic and policy-aware.
	sort.SliceStable(ordered, func(i, j int) bool {
		if len(ordered[i].devices) != len(ordered[j].devices) {
			return len(ordered[i].devices) > len(ordered[j].devices)
		}
		si := Score(NumaUtilization(ordered[i].devices), req.Profile, req.DevicePolicy)
		sj := Score(NumaUtilization(ordered[j].devices), req.Profile, req.DevicePolicy)
		if si != sj {
			return si > sj
		}
		return ordered[i].root < ordered[j].root
	})
	var picked []*device.Device
	all := make([]*device.Device, 0, needNumber)
	for _, g := range ordered {
		remaining := needNumber - len(all)
		if remaining <= 0 {
			break
		}
		take := remaining
		if len(g.devices) < take {
			take = len(g.devices)
		}
		all = append(all, alloc.pickMembers(req, g.devices, tier, take)...)
	}
	if len(all) != needNumber {
		return nil
	}
	picked = all
	return picked
}

// policyRuns splits devices into maximal groups of EQUAL device-policy score,
// preserving deviceStore order.
//
// NonePolicy is handled explicitly rather than derived: its comparator chain is
// [ByNuma, ByDeviceIdAsc] with no score involved, so reading equality off the
// sort keys would produce one run per device and silently disable the
// link-quality tie-break. Semantically NonePolicy means "no preference", which
// is exactly one run covering everything.
func policyRuns(req *AllocationRequest, devices []*device.Device) [][]*device.Device {
	if len(devices) == 0 {
		return nil
	}
	if req.DevicePolicy != util.BinpackPolicy && req.DevicePolicy != util.SpreadPolicy {
		return [][]*device.Device{devices}
	}
	var runs [][]*device.Device
	start := 0
	cur := Score(DeviceUtilization(devices[0]), req.Profile, req.DevicePolicy)
	for i := 1; i < len(devices); i++ {
		s := Score(DeviceUtilization(devices[i]), req.Profile, req.DevicePolicy)
		if s != cur {
			runs = append(runs, devices[start:i])
			start, cur = i, s
		}
	}
	return append(runs, devices[start:])
}

// inStoreOrder re-emits `picked` in the order they appear in `store`, so the
// result is a subsequence of the caller's policy-sorted list (invariant I2).
func inStoreOrder(picked, store []*device.Device) []*device.Device {
	want := make(map[string]struct{}, len(picked))
	for _, d := range picked {
		want[d.GetUUID()] = struct{}{}
	}
	out := make([]*device.Device, 0, len(picked))
	for _, d := range store {
		if _, ok := want[d.GetUUID()]; ok {
			out = append(out, d)
		}
	}
	return out
}

// filterToUUIDs keeps only the devices in the allowed set, preserving order.
func filterToUUIDs(devices []*device.Device, allowed map[string]struct{}) []*device.Device {
	out := make([]*device.Device, 0, len(devices))
	for _, d := range devices {
		if _, ok := allowed[d.GetUUID()]; ok {
			out = append(out, d)
		}
	}
	return out
}

// toLinkDevices maps allocator devices to the link-aware shape, preserving
// order. Unknown UUIDs are skipped defensively.
func (alloc *allocator) toLinkDevices(devices []*device.Device) []*gpuallocator.Device {
	list := alloc.nodeInfo.GetDeviceList()
	byUUID := make(map[string]*gpuallocator.Device, len(list))
	for _, d := range list {
		if d != nil {
			byUUID[d.UUID] = d
		}
	}
	out := make([]*gpuallocator.Device, 0, len(devices))
	for _, d := range devices {
		if l, ok := byUUID[d.GetUUID()]; ok {
			out = append(out, l)
		}
	}
	return out
}

// fromLinkDevices maps back, preserving the source slice's order.
func fromLinkDevices(picked []*gpuallocator.Device, store []*device.Device) []*device.Device {
	want := make(map[string]struct{}, len(picked))
	for _, d := range picked {
		want[d.UUID] = struct{}{}
	}
	out := make([]*device.Device, 0, len(picked))
	for _, d := range store {
		if _, ok := want[d.GetUUID()]; ok {
			out = append(out, d)
		}
	}
	return out
}

// combinationCount returns C(n, k), saturating at maxCombinationSearch+1 so a
// large n cannot overflow on the way to a comparison.
func combinationCount(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	if k > n-k {
		k = n - k
	}
	result := 1
	for i := 1; i <= k; i++ {
		result = result * (n - k + i) / i
		if result > maxCombinationSearch {
			return maxCombinationSearch + 1
		}
	}
	return result
}
