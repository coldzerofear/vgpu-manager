package gpuallocator

import "sync/atomic"

// DefaultBestEffortMaxGPUs is the candidate-count threshold above which the
// link-topology allocator switches from the exhaustive bestEffort policy to
// a greedy fallback. Wired from the device-scheduler --best-effort-max-gpus
// flag via SetBestEffortMaxGPUs; defaults are kept conservative so existing
// deployments behave unchanged on typical 8-GPU nodes.
const DefaultBestEffortMaxGPUs = 12

// bestEffortMaxGPUs is mutated at most once at startup and read on every
// allocation. atomic.Int32 keeps the lock-free read path while letting flag
// parsing on main() install the operator-tuned value.
var bestEffortMaxGPUs atomic.Int32

func init() {
	bestEffortMaxGPUs.Store(DefaultBestEffortMaxGPUs)
}

// SetBestEffortMaxGPUs overrides the runtime threshold. Negative or zero
// values are ignored to prevent accidentally disabling exhaustive search
// entirely (callers wanting "always greedy" should pass 1).
func SetBestEffortMaxGPUs(n int) {
	if n <= 0 {
		return
	}
	bestEffortMaxGPUs.Store(int32(n))
}

// GetBestEffortMaxGPUs returns the current threshold (for tests / logging).
func GetBestEffortMaxGPUs() int {
	return int(bestEffortMaxGPUs.Load())
}

// AllocateLink picks `size` devices using either the exhaustive bestEffort
// algorithm or the greedy fallback, chosen by the candidate count vs the
// configured threshold. Callers should prefer this over invoking the policies
// directly so that the threshold gate is always applied.
func AllocateLink(devices []*Device, size int) []*Device {
	if size <= 0 || size > len(devices) {
		return nil
	}
	if len(devices) > GetBestEffortMaxGPUs() {
		return greedyLinkAllocate(devices, size)
	}
	return NewBestEffortPolicy().Allocate(devices, nil, size)
}

// AllocateLinkTopK returns up to k topology-best candidate sets. Above the
// bestEffortMaxGPUs threshold only the single greedy result is returned
// (wrapped in a 1-element slice) — top-K exhaustive enumeration would
// reintroduce the combinatorial cost we are trying to avoid.
//
// Callers that want link+binpack/spread composition should request a small k
// (3-5 is typical) so the secondary policy can break ties among
// link-equivalent sets without paying for full enumeration.
func AllocateLinkTopK(devices []*Device, size, k int) [][]*Device {
	if size <= 0 || size > len(devices) {
		return nil
	}
	if k <= 0 {
		k = 1
	}
	if len(devices) > GetBestEffortMaxGPUs() {
		// Greedy path doesn't enumerate alternatives. Returning the single
		// best set is a degraded but safe behaviour: the caller's secondary
		// ranking simply has one candidate to choose from.
		got := greedyLinkAllocate(devices, size)
		if len(got) != size {
			return nil
		}
		return [][]*Device{got}
	}
	return bestEffortPolicyInstance.AllocateTopK(devices, nil, size, k)
}

// bestEffortPolicyInstance is a process-wide pointer reused across calls so
// we don't reallocate on every Filter pass. The policy itself is stateless.
var bestEffortPolicyInstance = &bestEffortPolicy{}

// greedyLinkAllocate is an O(n²·k) replacement for the exhaustive
// bestEffortPolicy when the candidate device count exceeds a configurable
// threshold (see DefaultBestEffortMaxGPUs). The exhaustive algorithm
// enumerates ALL partitions of `available` into sets of `size` and picks the
// best — that grows combinatorially and becomes prohibitive on 16+ GPU nodes
// (e.g. partitioning 16 GPUs into 8 sets of 2 yields ~2M partitions).
//
// Algorithm:
//  1. Compute each candidate GPU's "neighbour link sum" — total pair-score
//     against every other candidate. Highest neighbour sum becomes the seed,
//     i.e. the GPU best connected on average.
//  2. Iteratively add the GPU that maximises the cumulative pair score
//     against the currently-selected set, until size is reached.
//
// Properties:
//   - O(n²) seed computation, O(n·k) for the additions → O(n²·k) overall.
//   - Per-step greedy: not globally optimal, but consistently picks a
//     well-connected cluster because each addition maximises against the
//     entire chosen-so-far set (not just one neighbour).
//   - Deterministic: ties broken by lower GPU index (iteration order).
//
// Returns nil if size is invalid or insufficient devices are available.
// Matches bestEffort's "empty slice on failure" contract.
func greedyLinkAllocate(devices []*Device, size int) []*Device {
	if size <= 0 || size > len(devices) {
		return nil
	}
	if size == len(devices) {
		// Whole set requested — no need to score, just return it.
		out := make([]*Device, len(devices))
		copy(out, devices)
		return out
	}

	// Step 1: rank each candidate by its total pair score against the rest.
	rank := make([]int, len(devices))
	for i := range devices {
		for j := range devices {
			if i == j {
				continue
			}
			rank[i] += calculateGPUPairScore(devices[i], devices[j])
		}
	}
	seed := 0
	for i := 1; i < len(devices); i++ {
		if rank[i] > rank[seed] {
			seed = i
		}
	}

	selected := make([]*Device, 0, size)
	selected = append(selected, devices[seed])
	picked := make([]bool, len(devices))
	picked[seed] = true

	// Step 2: greedy expansion. At each step pick the still-unselected GPU
	// that yields the highest cumulative pair score against the current set.
	for len(selected) < size {
		bestIdx := -1
		bestScore := -1
		for i, cand := range devices {
			if picked[i] {
				continue
			}
			score := 0
			for _, s := range selected {
				score += calculateGPUPairScore(s, cand)
			}
			if score > bestScore {
				bestScore = score
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			// Should be unreachable given the upfront size check, but guard
			// defensively rather than risk an out-of-bounds panic.
			break
		}
		selected = append(selected, devices[bestIdx])
		picked[bestIdx] = true
	}
	return selected
}
