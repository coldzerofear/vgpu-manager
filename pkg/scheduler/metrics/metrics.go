// Package metrics exposes the scheduler extender's Prometheus instrumentation.
//
// # Counting units
//
// Every metric here declares ONE unambiguous unit, stated in its Help string,
// because the extender has two very different natural units and conflating them
// produces numbers that look meaningful and are not:
//
//   - PER POD — emitted once when a pod is actually placed. These answer
//     outcome questions: "are my link pods getting NVLink?", "what do users
//     actually request?".
//   - PER NODE EVALUATION — emitted once per node the allocator examined. These
//     answer pressure and cost questions: "how many nodes does a strict
//     contract refuse?", "how often does the combinatorial search run?".
//
// A pod may be evaluated against many nodes before one accepts it, so a
// per-node counter is NOT a pod count and must never be read as one.
//
// # What is deliberately NOT counted
//
// Preemption re-runs the allocator as a DRY RUN for every victim set it tests.
// Those simulations do not place anything, so they are excluded at the source
// (see allocator.NewSimulationAllocator) rather than being subtracted later.
// Without that, a single preemption would inflate placement counts several
// times over.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "vgpu_scheduler"

// Topology placement results, i.e. the connectivity a placement ACHIEVED.
//
// For link mode these mirror the interconnect tier ladder; for NUMA mode only
// ResultNUMA (satisfied) and ResultCrossNUMA (degraded) occur. ResultNone means
// the node published no topology data at all and the request fell through to
// plain resource-ordered allocation.
const (
	ResultNVLink    = "nvlink"
	ResultSwitch    = "switch"
	ResultNUMA      = "numa"
	ResultAny       = "any"
	ResultSpanned   = "spanned"
	ResultCrossNUMA = "cross-numa"
	ResultNone      = "none"
)

// Link search algorithms. The search only runs inside a NON-UNIFORM component;
// on uniform fabrics (NVSwitch, and every tier of a bridged or pure-PCIe node)
// selection is linear and nothing is recorded here at all.
const (
	AlgoExhaustive = "exhaustive"
	AlgoGreedy     = "greedy"
)

// Cross-pod alignment outcomes, i.e. which key actually steered the placement.
const (
	AlignRail      = "rail"      // finest: same rail set as the sibling
	AlignComponent = "component" // same NVLink component
	AlignNone      = "none"      // opted in, but nothing to align to here
)

// Filter stages. LockWait is split out because SerializedNodeFilter is on by
// default: folded together, a queueing spike reads as "allocation got slower".
const (
	StageNode     = "node"
	StageLockWait = "device_lock_wait"
	StageDevice   = "device_work"
)

var (
	// TopologyPlacementTotal is the headline outcome metric: for each pod
	// placed WITH a topology mode, what connectivity did it actually get?
	//
	// `mode` is what the pod asked for, `result` is what it received, so
	// result != the satisfying value is exactly the set of silent downgrades.
	TopologyPlacementTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "topology_placement_total",
		Help:      "PER POD. Topology-requesting pods placed, by requested mode and the connectivity actually achieved.",
	}, []string{"mode", "result"})

	// PodPolicyTotal records what users actually request. Without it there is
	// no way to tell whether topology work matters to this cluster at all.
	PodPolicyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "pod_policy_total",
		Help:      "PER POD. Successfully placed vGPU pods, by the scheduling policies they requested.",
	}, []string{"node_policy", "device_policy", "topology_mode"})

	// CrossPodAlignmentTotal reports which alignment key steered a cross-pod
	// placement — per POD, so the ratio is not diluted by how many nodes were
	// examined on the way.
	CrossPodAlignmentTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "crosspod_alignment_total",
		Help:      "PER POD. Cross-pod topology placements, by which alignment key was used.",
	}, []string{"result"})

	// TopologyStrictRejectTotal counts NODES refused by a strict contract. A
	// high value against a low placement rate means the contract is
	// over-constrained for the fleet.
	TopologyStrictRejectTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "topology_strict_reject_total",
		Help:      "PER NODE EVALUATION. Nodes rejected because a strict topology contract could not be met.",
	}, []string{"mode"})

	// NodeRejectTotal buckets per-node filter rejections by structured reason.
	// The filter already computes this breakdown for its log line.
	NodeRejectTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "node_reject_total",
		Help:      "PER NODE EVALUATION. Nodes rejected during filtering, by structured reason code.",
	}, []string{"code"})

	// LinkSearchTotal counts how often the in-component combinatorial search
	// ran. On the hardware this design targets it should be near zero: uniform
	// fabrics skip it entirely. A non-trivial rate means non-uniform machines
	// (DGX-1 class) are in the fleet — which is exactly the population that
	// needs the search to be correct.
	LinkSearchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "link_search_total",
		Help:      "PER SEARCH. In-component link searches actually executed, by algorithm.",
	}, []string{"algo"})

	// LinkSearchCandidates records the component size each search examined,
	// which is what bounds its cost. Recorded only when a search ran.
	LinkSearchCandidates = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "link_search_candidates",
		Help:      "PER SEARCH. Candidate count handed to an in-component link search.",
		Buckets:   []float64{2, 4, 6, 8, 12, 16, 24, 32, 64},
	}, []string{"algo"})

	// FilterDuration measures the extender's Filter verb. The lock-wait stage
	// is separate on purpose — see StageLockWait.
	FilterDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "filter_duration_seconds",
		Help:      "PER FILTER CALL. Time spent in each stage of the Filter verb.",
		// 100µs .. ~13s: the normal budget is single-digit ms, but a saturated
		// cluster under the serial filter lock can queue for far longer.
		Buckets: prometheus.ExponentialBuckets(0.0001, 3, 12),
	}, []string{"stage"})
)

// registry holds exactly the extender's own series plus the runtime collectors.
// Package-private so nothing can be registered onto it from elsewhere by
// accident.
var registry = prometheus.NewRegistry()

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		TopologyPlacementTotal,
		PodPolicyTotal,
		CrossPodAlignmentTotal,
		TopologyStrictRejectTotal,
		NodeRejectTotal,
		LinkSearchTotal,
		LinkSearchCandidates,
		FilterDuration,
	)
}

// Handler serves this registry, for mounting on the extender's existing router.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// Registry exposes the registry to tests that need to gather series.
func Registry() *prometheus.Registry { return registry }

// ObserveFilterStage records one Filter stage duration. Intended as
// `defer metrics.ObserveFilterStage(metrics.StageDevice, time.Now())`.
func ObserveFilterStage(stage string, start time.Time) {
	FilterDuration.WithLabelValues(stage).Observe(time.Since(start).Seconds())
}

// ObserveLinkSearch records one executed in-component search.
func ObserveLinkSearch(algo string, candidates int) {
	LinkSearchTotal.WithLabelValues(algo).Inc()
	LinkSearchCandidates.WithLabelValues(algo).Observe(float64(candidates))
}
