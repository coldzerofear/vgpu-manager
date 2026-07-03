package allocator

import (
	"math"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

// RequestProfile is the normalized resource-demand profile of a pod. Its
// three weights (Num / Mem / Core) sum to 1.0 and tell the scoring layer
// how much each dimension should contribute to a binpack/spread score.
//
// Why this exists: the legacy GetBinpackNodeScore / GetSpreadNodeScore
// formulas treat the three dimensions as equally important — a pod
// requesting only memory still ranks nodes by their (num + mem + core)/3
// average, which means a node with low memory utilization but high vGPU
// slot utilization could lose to a node with the opposite profile, even
// though "low memory" is the only thing that actually matters for the
// requesting pod. The weights here re-bias the score toward whatever the
// pod actually consumes so binpack/spread reflect user intent.
//
// Dimensions the pod doesn't ask for collapse to weight 0 — they then
// drop out of the score entirely (a node's utilization in that dimension
// becomes irrelevant). When a vGPU pod somehow requests nothing (should
// be impossible since vGPUNumber > 0 is enforced upstream, but defended
// against here so Score never returns NaN), weights fall back to a
// uniform 1/3 each so behavior matches the legacy formula.
type RequestProfile struct {
	NumWeight  float64
	MemWeight  float64
	CoreWeight float64
}

// NewRequestProfile builds the pod's request-weighted scoring profile from
// the pod-wide aggregate req.Total (Σ over containers of Number, Number×cores,
// Number×memory — the same peak-demand figure the capacity gate uses). It is
// deliberately NODE-INDEPENDENT — the raw card capacity and per-node
// MemoryFactor never reach it — so every candidate node is ranked with the
// SAME weights (a per-node profile would compare scores computed from
// different weight vectors, which is ill-defined for a cross-node sort).
//
// The three terms are put on a comparable "cards' worth" scale so none
// dominates purely because of its unit:
//
//   - rNum  = Total.Number.
//   - rCore = Total.Cores / HundredCore. Total.Cores is already Σ(per-vGPU
//     cores × Number) and HundredCore == one whole card's cores, so this is
//     the pod's total card-equivalents of compute. A whole-card request
//     (per-vGPU cores resolved to HundredCore) yields exactly Total.Number,
//     matching rNum; a memory-only pod (Total.Cores == 0) drops out.
//   - rMem  = whole-card (Total.Memory == 0) → Total.Number (balanced with
//     num/core, so a plain "give me N cards" pod scores 1/3 each); explicit
//     memory → single-card GB units (ceil(Total.Memory/1024)), floored at
//     Total.Number so a multi-card pod never weighs memory below the cards
//     it spans. GB units (not raw MB) keep an 8 GB request from swamping the
//     num/core dimensions.
//
// The card capacity would let memory be a true fraction of a card, but that
// is node-specific; keeping the profile node-independent is the deliberate
// trade-off for a coherent cross-node ranking.
func NewRequestProfile(req *AllocationRequest) RequestProfile {
	if req == nil {
		return UniformProfile
	}

	var rNum, rMem, rCore float64

	number := float64(req.Total.Number)
	if number <= 0 {
		return UniformProfile
	}
	rNum += number

	// req.Total already sums (per-vGPU value × Number) across the pod's
	// containers, so cores/memory here are POD-WIDE magnitudes — do NOT
	// multiply by Number again (that would make the term grow with Number²).
	cores := float64(req.Total.Cores)
	memory := float64(req.Total.Memory)

	// Cores in "cards' worth": HundredCore == one whole card's cores, so
	// Total.Cores / HundredCore is the pod's total card-equivalents of
	// compute. A whole-card request (per-vGPU cores resolved to HundredCore)
	// yields exactly `number`, matching rNum; a memory-only pod
	// (Total.Cores == 0) contributes nothing and drops out of the weights.
	if cores > 0 {
		rCore += cores / float64(util.HundredCore)
	}

	// Memory: a whole-card request (Total.Memory == 0) counts as `number`
	// cards' worth, keeping it balanced with num/core (a plain "give me N
	// cards" pod scores 1/3 each). Explicit memory uses single-card GB units
	// — the profile is node-independent, so the real card capacity isn't
	// available here — floored at `number` so a multi-card pod never weighs
	// memory below the cards it spans.
	if memory <= 0 {
		rMem += number
	} else {
		rMem += max(math.Ceil(memory/float64(1024)), number)
	}

	sum := rNum + rMem + rCore
	if sum == 0 {
		return UniformProfile
	}
	return RequestProfile{
		NumWeight:  rNum / sum,
		MemWeight:  rMem / sum,
		CoreWeight: rCore / sum,
	}
}

// UniformProfile is the fallback profile used when no pod is available to
// derive weights from — every dimension weighted equally, reproducing the
// legacy "(num + mem + core) / 3" behaviour. Test fixtures that exercise
// scoring without a pod (e.g. the NUMA callback tests) pass this so the
// expected ordering matches the pre-Phase-B math.
var UniformProfile = RequestProfile{
	NumWeight:  1.0 / float64(3),
	MemWeight:  1.0 / float64(3),
	CoreWeight: 1.0 / float64(3),
}

// ResourceUtilization is the 0..1 used-fraction of each of the three vGPU
// dimensions on some resource container (a node, a device, or a NUMA
// group). It's the input to Score; building it is the only thing each
// resource kind needs to provide.
type ResourceUtilization struct {
	Num  float64
	Mem  float64
	Core float64
}

// NodeUtilization measures how much of a node's aggregate capacity is
// currently in use. Mirrors GetBinpackNodeScore's input math but returns
// the raw used-fraction instead of pre-collapsing to a single number, so
// the same utilization snapshot can feed either a binpack or a spread
// score depending on what the caller asks for.
func NodeUtilization(info *device.NodeInfo, req *AllocationRequest) ResourceUtilization {
	availNum := float64(info.GetAvailableNumber())
	availMem := float64(info.GetAvailableMemory())
	availCore := float64(info.GetAvailableCores())
	if req != nil {
		reqNumber := float64(req.Total.Number)
		reqCores := float64(req.Total.Cores)
		reqMemory := float64(req.Total.Memory)
		// A node that cannot fit the pod must never rank first. A single
		// utilization value can't say "worst" for both policies (binpack
		// prefers high utilization, spread prefers low), so report per
		// policy: spread sees "fully used" (score 0), binpack sees "fully
		// empty" (score 0). Unreachable for candidates that passed the
		// deviceFilter capacity gate; defensive.
		if availNum < reqNumber || availCore < reqCores || availMem < reqMemory {
			if req.NodePolicy == util.SpreadPolicy {
				return ResourceUtilization{Num: 1, Core: 1, Mem: 1}
			}
			return ResourceUtilization{}
		}
		// Binpack scores by POST-placement fullness: subtract this pod's
		// peak demand so a node that would become fuller ranks higher.
		// Spread keeps current (pre-placement) freeness.
		if req.NodePolicy == util.BinpackPolicy {
			availNum -= reqNumber
			availMem -= reqMemory
			availCore -= reqCores
		}
	}
	return ResourceUtilization{
		Num:  1 - util.SafeDiv(availNum, float64(info.GetTotalNumber())),
		Mem:  1 - util.SafeDiv(availMem, float64(info.GetTotalMemory())),
		Core: 1 - util.SafeDiv(availCore, float64(info.GetTotalCores())),
	}
}

// DeviceUtilization mirrors NodeUtilization for a single GPU. Replaces
// the inline calculation that allocator.deviceUsedRatio used to do — same
// arithmetic, exposed as a building block so the per-device Score path
// can share machinery with the per-node and per-NUMA paths.
func DeviceUtilization(d *device.Device) ResourceUtilization {
	totalNum := float64(d.GetTotalNumber())
	totalMem := float64(d.GetTotalMemory())
	totalCore := float64(d.GetTotalCores())
	return ResourceUtilization{
		Num:  1 - util.SafeDiv(float64(d.AllocatableNumber()), totalNum),
		Mem:  1 - util.SafeDiv(float64(d.AllocatableMemory()), totalMem),
		Core: 1 - util.SafeDiv(float64(d.AllocatableCores()), totalCore),
	}
}

// NumaUtilization averages per-device utilization across the devices in
// one NUMA group. Returns zero-utilization for an empty group (callers
// never feed empty groups in practice; defended so safeDiv stays the only
// place that handles zero-divisor).
func NumaUtilization(devices []*device.Device) ResourceUtilization {
	if len(devices) == 0 {
		return ResourceUtilization{}
	}
	var agg ResourceUtilization
	for _, d := range devices {
		u := DeviceUtilization(d)
		agg.Num += u.Num
		agg.Mem += u.Mem
		agg.Core += u.Core
	}
	n := float64(len(devices))
	return ResourceUtilization{
		Num:  agg.Num / n,
		Mem:  agg.Mem / n,
		Core: agg.Core / n,
	}
}

// Score collapses a utilization snapshot to a single ranking number,
// weighted by the request profile. Binpack returns the weighted USED
// fraction (higher = warmer = preferred); Spread returns the weighted
// FREE fraction (higher = colder = preferred). NonePolicy returns 0 — it
// means "don't rank by score", and callers that pass NonePolicy here
// should already be using a different LessFunc chain.
//
// The returned value is in [0, 1] before any callsite multiplier is
// applied; the legacy ×100 was just for log-readable percentages and the
// new code drops the multiplier since we're comparing scores, not
// reporting them.
func Score(u ResourceUtilization, p RequestProfile, policy util.SchedulerPolicy) float64 {
	switch policy {
	case util.BinpackPolicy:
		return p.NumWeight*u.Num + p.MemWeight*u.Mem + p.CoreWeight*u.Core
	case util.SpreadPolicy:
		return p.NumWeight*(1-u.Num) + p.MemWeight*(1-u.Mem) + p.CoreWeight*(1-u.Core)
	}
	return 0
}
