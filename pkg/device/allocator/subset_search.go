package allocator

import (
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
)

// searchBestSubset returns the highest pair-score sum achievable by choosing
// `need` of `devices`, and the winning subset.
//
// This is the ONLY combinatorial step left in link allocation, and it runs only
// inside a single NON-UNIFORM component — in practice a DGX-1-class hybrid cube
// mesh, where the component is 8 cards and the worst case is C(8,4)=70.
//
// Note what it does NOT do: the previous implementation enumerated PARTITIONS
// of the whole node into sets of the requested size (2.6M for 16-choose-4),
// because it used the partition score as a proxy for "don't wreck the
// remainder". That goal is now served structurally by walking the tier ladder
// tightest-first, so plain combinations suffice — which is what removes the
// need for a candidate-count threshold.
func searchBestSubset(devices []*gpuallocator.Device, need int) (int, []*gpuallocator.Device) {
	return searchBestSubsetWithFixed(devices, nil, need)
}

// searchBestSubsetWithFixed is searchBestSubset with a set of already-chosen
// devices that every candidate is scored against.
//
// Ties are broken toward the subset that leaves the better REMAINDER: with
// equal scores it prefers the choice whose complement can still form the
// highest-scoring group of the same size. Without it, "pick the globally best
// set" would be free to carve up an intact clique when an equally good
// alternative existed — the one behaviour the old partition enumeration got
// right and that a naive combination search would lose.
//
// Cost: the complement scan runs for any candidate scoring >= the best seen so
// far, not only on exact ties, so it is hit more often early in the enumeration
// and rarely once the maximum is found. It is a GREEDY estimate rather than a
// nested exhaustive search precisely to keep that bounded — on a DGX-1 quad the
// whole thing is a handful of pair lookups.
func searchBestSubsetWithFixed(
	devices, fixed []*gpuallocator.Device, need int,
) (int, []*gpuallocator.Device) {

	if need <= 0 || need > len(devices) {
		return 0, nil
	}
	var (
		bestScore     = -1
		bestRemainder = -1
		best          []*gpuallocator.Device
		current       = make([]*gpuallocator.Device, need)
	)
	forEachCombination(len(devices), need, func(idx []int) {
		for i, j := range idx {
			current[i] = devices[j]
		}
		score := setScoreWithFixed(current, fixed)
		if score < bestScore {
			return
		}
		remainder := complementScore(devices, idx, need)
		if score > bestScore || remainder > bestRemainder {
			bestScore, bestRemainder = score, remainder
			best = append(best[:0], current...)
		}
	})
	if best == nil {
		return 0, nil
	}
	return bestScore, best
}

// complementScore is the best same-size group score achievable from the devices
// NOT chosen. Returns 0 when the complement is too small to form one, which
// correctly makes "no remainder left" the least attractive tie-break.
func complementScore(devices []*gpuallocator.Device, chosen []int, need int) int {
	taken := make(map[int]struct{}, len(chosen))
	for _, i := range chosen {
		taken[i] = struct{}{}
	}
	rest := make([]*gpuallocator.Device, 0, len(devices)-len(chosen))
	for i, d := range devices {
		if _, ok := taken[i]; !ok {
			rest = append(rest, d)
		}
	}
	if len(rest) < need || need < 2 {
		return 0
	}
	// Greedy, not exhaustive: this is a TIE-BREAK, so an approximation is
	// enough and it keeps the nested search linear rather than quadratic in the
	// combination count.
	return setScore(greedyPick(rest, nil, need))
}

// setScore sums the pair scores within a set.
func setScore(set []*gpuallocator.Device) int {
	total := 0
	for i := 0; i < len(set); i++ {
		for j := i + 1; j < len(set); j++ {
			total += gpuallocator.PairScore(set[i], set[j])
		}
	}
	return total
}

// setScoreWithFixed scores a candidate set together with an already-chosen set:
// pairs inside the candidate, plus every candidate↔fixed pair. Pairs inside
// `fixed` are a constant across candidates and so are omitted.
func setScoreWithFixed(set, fixed []*gpuallocator.Device) int {
	total := setScore(set)
	for _, a := range set {
		for _, b := range fixed {
			total += gpuallocator.PairScore(a, b)
		}
	}
	return total
}

// greedyPick selects `need` devices by repeatedly taking the one that adds the
// most pair score against everything chosen so far (including `fixed`).
//
// Used for the complement tie-break and as the over-budget safety valve. O(n²k).
func greedyPick(devices, fixed []*gpuallocator.Device, need int) []*gpuallocator.Device {
	if need <= 0 || need > len(devices) {
		return nil
	}
	chosen := make([]*gpuallocator.Device, 0, need)
	chosen = append(chosen, fixed...)
	fixedLen := len(chosen)
	used := make([]bool, len(devices))
	for len(chosen)-fixedLen < need {
		bestIdx, bestGain := -1, -1
		for i, cand := range devices {
			if used[i] {
				continue
			}
			gain := 0
			for _, c := range chosen {
				gain += gpuallocator.PairScore(c, cand)
			}
			// Strictly-greater keeps the FIRST candidate on ties, which is the
			// deviceStore-order preference.
			if gain > bestGain {
				bestIdx, bestGain = i, gain
			}
		}
		if bestIdx < 0 {
			break
		}
		used[bestIdx] = true
		chosen = append(chosen, devices[bestIdx])
	}
	return chosen[fixedLen:]
}

// forEachCombination enumerates every k-subset of [0,n) as ascending index
// slices, iteratively (no recursion, no allocation per combination).
func forEachCombination(n, k int, fn func(idx []int)) {
	if k <= 0 || k > n {
		return
	}
	idx := make([]int, k)
	for i := range idx {
		idx[i] = i
	}
	for {
		fn(idx)
		// Advance to the next combination in lexicographic order.
		i := k - 1
		for i >= 0 && idx[i] == i+n-k {
			i--
		}
		if i < 0 {
			return
		}
		idx[i]++
		for j := i + 1; j < k; j++ {
			idx[j] = idx[j-1] + 1
		}
	}
}
