package allocator

import (
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
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

// NewRequestProfile builds the profile from a pod's aggregated container
// requests. The weights MUST reflect what the pod ACTUALLY consumes at
// allocation time, not just what the user typed — vgpu-manager's allocator
// applies two implicit-fill rules that turn sparse user input into full-
// card consumption, and the profile mirrors both so a single-line
// "vgpu-number: 1" pod doesn't end up with weights that say "only the
// slot dimension matters".
//
// Implicit-fill rules being mirrored (see allocator.allocateOne and
// allocator.allocateByNumbers):
//
//   - vgpu-memory == 0 → reqMemory becomes the whole card's memory at
//     allocation time. Profile treats this as the pod consuming
//     'vgpu-number cards' worth' of memory.
//
//   - vgpu-cores == 0 AND vgpu-memory == 0 → needCores is promoted to
//     util.HundredCore. Profile treats this as 'vgpu-number cards' worth'
//     of cores. NB: when the user explicitly typed vgpu-memory but left
//     cores at 0 ("memory-only" pod), cores stays 0 and the dimension
//     correctly drops out of the weights.
//
// Unit handling: node-reported memory is in MB; the user-typed
// vgpu-memory value is also in MB but only AFTER multiplying by the
// node's memoryFactor — that's the same conversion allocator.allocateOne
// applies before any memory comparison. memPerCard arrives already in MB
// (via MemoryPerCard); we apply the factor to user input here so both
// sides of the rMem ratio are on the same scale.
//
// memPerCard should be derived from a representative candidate node (see
// MemoryPerCard); a single value is used across all candidates in one
// filter pass to keep the cross-node ranking consistent. In heterogeneous
// clusters this is an acknowledged approximation — different nodes have
// different mem/card, but a per-node profile would mean different
// dimension weights per node and the comparison would no longer be a
// well-defined ranking.
func NewRequestProfile(pod *corev1.Pod, memPerCard int64, memoryFactor int) RequestProfile {
	var rNum, rMem, rCore float64
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if !util.IsVGPURequiredContainer(c) {
			continue
		}
		cNum := util.GetResourceOfContainer(c, util.VGPUNumberResourceName)
		cCore := util.GetResourceOfContainer(c, util.VGPUCoreResourceName)
		cMem := util.GetResourceOfContainer(c, util.VGPUMemoryResourceName)

		rNum += float64(cNum)

		// allocateByNumbers writes the same (needCores, reqMemory) into EACH
		// of the cNum claims it produces, so per-container consumption is
		// cNum * per-vGPU. Without the cNum multiplier the profile would
		// under-count multi-vGPU containers.

		// Memory: implicit-full-memory wins when the user typed nothing.
		// Whole card per requested vGPU; ratio simplifies to cNum.
		if cMem == 0 {
			rMem += float64(cNum)
		} else if memPerCard > 0 {
			effMem := cMem
			if memoryFactor > 0 {
				effMem *= int64(memoryFactor)
			}
			rMem += float64(cNum) * float64(effMem) / float64(memPerCard)
		}

		// Cores: implicit-full-cores only fires when BOTH cores AND memory
		// are unset — the allocator's "if needCores == 0 && needMemory == 0"
		// test reads user-typed values, so we test the same. Explicit-cores
		// is independent; the "memory only" case (cMem>0, cCore==0) lets
		// cores drop out entirely, which is what the user asked for.
		switch {
		case cCore == 0 && cMem == 0:
			rCore += float64(cNum)
		case cCore > 0:
			rCore += float64(cNum) * float64(cCore) / float64(util.HundredCore)
		}
	}
	sum := rNum + rMem + rCore
	if sum == 0 {
		return RequestProfile{1.0 / 3, 1.0 / 3, 1.0 / 3}
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
var UniformProfile = RequestProfile{NumWeight: 1.0 / 3, MemWeight: 1.0 / 3, CoreWeight: 1.0 / 3}

// MemoryPerCard returns the average memory-per-GPU on a node, used as the
// normalization unit for RequestProfile.MemWeight. Falls back to 1 (i.e.
// every byte counts equally as one card) when device count is zero so
// callers never have to special-case empty nodes.
func MemoryPerCard(info *device.NodeInfo) int64 {
	total := info.GetTotalNumber()
	if total <= 0 {
		return 1
	}
	mem := info.GetTotalMemory() / int64(total)
	if mem <= 0 {
		return 1
	}
	return mem
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
func NodeUtilization(info *device.NodeInfo) ResourceUtilization {
	totalNum := float64(info.GetTotalNumber())
	totalMem := float64(info.GetTotalMemory())
	totalCore := float64(info.GetTotalCores())
	availNum := float64(info.GetAvailableNumber())
	availMem := float64(info.GetAvailableMemory())
	availCore := float64(info.GetAvailableCores())
	return ResourceUtilization{
		Num:  1 - safeDiv(availNum, totalNum),
		Mem:  1 - safeDiv(availMem, totalMem),
		Core: 1 - safeDiv(availCore, totalCore),
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
		Num:  1 - safeDiv(float64(d.AllocatableNumber()), totalNum),
		Mem:  1 - safeDiv(float64(d.AllocatableMemory()), totalMem),
		Core: 1 - safeDiv(float64(d.AllocatableCores()), totalCore),
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
func Score(u ResourceUtilization, p RequestProfile, mode util.SchedulerPolicy) float64 {
	switch mode {
	case util.BinpackPolicy:
		return p.NumWeight*u.Num + p.MemWeight*u.Mem + p.CoreWeight*u.Core
	case util.SpreadPolicy:
		return p.NumWeight*(1-u.Num) + p.MemWeight*(1-u.Mem) + p.CoreWeight*(1-u.Core)
	}
	return 0
}
