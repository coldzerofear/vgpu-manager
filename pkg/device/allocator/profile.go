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

// NewRequestProfile builds the profile from a pod's container requests.
// Deliberately NODE-INDEPENDENT — no candidate node, no mem-per-card, no
// memoryFactor reaches this function. Earlier iterations passed a
// "representative node" for memory normalisation, which was wrong:
//
//   - Heterogeneous clusters can mix 16 / 24 / 80 GB cards, so any single
//     reference node biases the cross-node ranking toward that node's
//     hardware view of "what's heavy".
//   - The natural reference (nodeInfoList[0]) is non-deterministic for a
//     given pod — it shifts whenever upstream filtering or kube-scheduler
//     reorders the candidate list.
//   - memoryFactor is per-node too (device-plugin --device-memory-factor
//     flag); reading it from any single node has the same bias.
//
// Trade-off: explicit-vgpu-memory weights are now in the user-typed unit
// rather than card-fractions. Within one cluster admins typically pick a
// single factor cluster-wide so users consistently type one unit and the
// weights stay coherent. The implicit-full-card branches below cover the
// dominant "give me a card" case and are unit-independent — a single-line
// "vgpu-number: 1" pod gets (1/3, 1/3, 1/3) regardless of cluster shape.
//
// Implicit-fill rules mirrored from allocator.resolveContainerNeeds and
// allocator.buildClaims:
//
//   - vgpu-memory == 0 → reqMemory becomes the whole card's memory at
//     claim-construction time. Profile records this as cNum "cards'
//     worth" of memory (the unit cancels, so no node info needed).
//
//   - vgpu-cores == 0 AND vgpu-memory == 0 → needCores is promoted to
//     util.HundredCore. Profile records cNum "cards' worth" of cores.
//     When the user explicitly typed vgpu-memory but left cores at 0
//     ("memory-only" pod), cores correctly stays 0 and drops out of the
//     weights — matching the allocator's per-claim Cores=0 behaviour.
//
// Per-container values multiply by cNum because buildClaims writes the
// same (needCores, reqMemory) into EACH of the cNum claims it produces,
// so per-container consumption is cNum * per-vGPU.
func NewRequestProfile(pod *corev1.Pod) RequestProfile {
	var rNum, rMem, rCore float64
	// Weights are a ranking heuristic over the pod's overall vGPU shape, so
	// init containers are folded in alongside app containers (a sum across
	// both groups). For a pod without init containers this is identical to
	// the historical app-only weighting.
	addContainer := func(c *corev1.Container) {
		cNum := util.GetResourceOfContainer(c, util.VGPUNumberResourceName)
		if cNum <= 0 {
			return
		}
		cCore := util.GetResourceOfContainer(c, util.VGPUCoreResourceName)
		cMem := util.GetResourceOfContainer(c, util.VGPUMemoryResourceName)

		rNum += float64(cNum)

		// Memory: implicit-full-card collapses to "cNum cards' worth";
		// explicit uses raw user-typed value (no factor, no per-card
		// normalisation — see the trade-off discussion above).
		if cMem == 0 {
			rMem += float64(cNum)
		} else {
			rMem += float64(cNum) * float64(cMem)
		}

		// Cores: implicit-full-cores only fires when BOTH cores AND memory
		// are unset — the allocator's "if needCores == 0 && needMemory == 0"
		// test reads user-typed values, so we test the same. Explicit-cores
		// uses cCore's well-defined "100 = full card" convention.
		switch {
		case cCore == 0 && cMem == 0:
			rCore += float64(cNum)
		case cCore > 0:
			rCore += float64(cNum) * float64(cCore) / float64(util.HundredCore)
		}
	}
	for i := range pod.Spec.InitContainers {
		addContainer(&pod.Spec.InitContainers[i])
	}
	for i := range pod.Spec.Containers {
		addContainer(&pod.Spec.Containers[i])
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
		Num:  1 - util.SafeDiv(availNum, totalNum),
		Mem:  1 - util.SafeDiv(availMem, totalMem),
		Core: 1 - util.SafeDiv(availCore, totalCore),
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
func Score(u ResourceUtilization, p RequestProfile, mode util.SchedulerPolicy) float64 {
	switch mode {
	case util.BinpackPolicy:
		return p.NumWeight*u.Num + p.MemWeight*u.Mem + p.CoreWeight*u.Core
	case util.SpreadPolicy:
		return p.NumWeight*(1-u.Num) + p.MemWeight*(1-u.Mem) + p.CoreWeight*(1-u.Core)
	}
	return 0
}
