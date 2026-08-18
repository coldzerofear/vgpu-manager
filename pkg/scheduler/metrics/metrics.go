/*
Copyright 2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package metrics exposes the scheduler extender's Prometheus instrumentation.
//
// # Counting units
//
// Every metric here declares ONE unambiguous unit, stated in its Help string,
// because the extender has several very different natural units and conflating
// them produces numbers that look meaningful and are not:
//
//   - PER CALL — emitted once per extender HTTP verb invocation. These answer
//     "is the extender healthy and fast?".
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
// # The verb dimension
//
// The extender serves four verbs, and every call-scoped metric carries the same
// `verb` label so one query shape works across all of them. Verbs do NOT share
// a result vocabulary — "fit" means nothing for a bind — so each verb documents
// its own, see the Result* constants:
//
//	filter         fit | no_fit | error
//	filter_dryrun  fit | no_fit | error
//	bind           success | no_node | pod_not_found | uid_mismatch |
//	               node_mismatch | prealloc_expired | patch_failed | bind_failed
//	preempt        victims | no_victims | passthrough
//
// The verb label is also what keeps read-only simulation traffic legible.
// Autoscaler probes are unbounded — every candidate node group on every loop —
// so they are separated by verb rather than folded into live scheduling.
//
// # What is deliberately NOT counted
//
// Preemption re-runs the allocator as a DRY RUN for every victim set it tests,
// and so does the dry-run filter. Those simulations place nothing, so the
// placement- and allocator-scoped series are suppressed at the source (see
// allocator.NewSimulationAllocator) rather than being subtracted later. Without
// that, a single preemption would inflate placement counts several times over.
package metrics

import (
	"net/http"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
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

// Extender verbs. Every call-scoped metric carries this label.
const (
	VerbFilter       = "filter"
	VerbFilterDryRun = "filter_dryrun"
	VerbBind         = "bind"
	VerbPreempt      = "preempt"
)

// Call stages. Every verb reports StageTotal; the filter additionally splits
// its work, because LockWait is a queue, not work: folded into the total, a
// contention spike reads as "allocation got slower". For the filter,
// StageLockWait is contained INSIDE StageDevice, so device work is
// device minus lock_wait in PromQL.
const (
	StageTotal    = "total"
	StageNode     = "node"
	StageDevice   = "device"
	StageLockWait = "lock_wait"
)

// Filter and dry-run filter results.
const (
	ResultFit   = "fit"    // at least one candidate can host the pod
	ResultNoFit = "no_fit" // every candidate was rejected
	ResultError = "error"  // the call itself failed
)

// Bind results. Everything except ResultBindSuccess is a failure, and they are
// kept apart because they demand different responses: ResultBindNodeMismatch
// and ResultBindPreAllocExpired mean the filter's optimistic pre-allocation did
// not survive until bind (retune --stuck-grace-period), whereas the rest are
// ordinary API-level failures.
const (
	ResultBindSuccess         = "success"
	ResultBindNoNode          = "no_node"          // caller sent no target node
	ResultBindPodNotFound     = "pod_not_found"    // pod vanished between filter and bind
	ResultBindUIDMismatch     = "uid_mismatch"     // pod was recreated under the same name
	ResultBindNodeMismatch    = "node_mismatch"    // bound node is not the predicated one
	ResultBindPreAllocExpired = "prealloc_expired" // pre-allocation went stale before bind
	ResultBindPatchFailed     = "patch_failed"     // could not stamp allocation metadata
	ResultBindFailed          = "bind_failed"      // the API server rejected the binding
)

// Preempt results.
const (
	ResultPreemptVictims     = "victims"     // at least one node survived with a victim set
	ResultPreemptNoVictims   = "no_victims"  // every candidate was vetoed after victim removal
	ResultPreemptPassthrough = "passthrough" // not our pod, or we could not judge: input returned as-is
)

// Why an in-tree-proposed victim was refused. These explain a preemption that
// came up short: kube-scheduler picked a victim we will not evict.
const (
	ProtectedTerminating = "terminating"  // already being deleted or finished
	ProtectedCritical    = "critical"     // system-critical priority class
	ProtectedDaemonSet   = "daemonset"    // recreated on the same node, evicting achieves nothing
	ProtectedBinding     = "binding"      // inside its own filter/bind window
	ProtectedGangSibling = "gang_sibling" // same gang as the preemptor
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

	// NodeRejectTotal buckets per-node rejections by structured reason. The
	// filter already computes this breakdown for its log line; preempt reports
	// the node gates it applies before considering victims. Split by verb so
	// unbounded simulation traffic cannot drown live scheduling.
	NodeRejectTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "node_reject_total",
		Help:      "PER NODE EVALUATION. Nodes rejected during a verb, by structured reason code.",
	}, []string{"verb", "code"})

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

	// VerbTotal is the extender's headline health signal: every call, by verb
	// and how it ended. Result vocabularies are per-verb — see the package doc.
	VerbTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "verb_total",
		Help:      "PER CALL. Extender verb invocations, by verb and outcome.",
	}, []string{"verb", "result"})

	// VerbDuration measures how long a call spent, end to end and — for the
	// filter — per stage. See the Stage* constants for how the filter's stages
	// nest.
	VerbDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "verb_duration_seconds",
		Help:      "PER CALL. Time spent serving an extender verb, by stage.",
		// 100µs .. ~13s: the normal budget is single-digit ms, but a saturated
		// cluster under the serial filter lock can queue for far longer.
		Buckets: prometheus.ExponentialBuckets(0.0001, 3, 12),
	}, []string{"verb", "stage"})

	// PreemptVictimsAdded answers "how much MORE disruption did we cause than
	// kube-scheduler planned?". In-tree cannot see per-device constraints, so it
	// routinely under-selects and we append victims until the pod fits. Zero is
	// the healthy bucket: in-tree's proposal was already enough.
	PreemptVictimsAdded = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "preempt_victims_added",
		Help:      "PER ACCEPTED NODE. Victims appended beyond the set kube-scheduler proposed.",
		Buckets:   []float64{0, 1, 2, 3, 4, 6, 8, 16},
	})

	// PreemptProtectedTotal counts victims we refuse to evict. A preemption that
	// finds no viable node while this climbs means kube-scheduler keeps
	// proposing victims that are off-limits to us.
	PreemptProtectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "preempt_protected_total",
		Help:      "PER VICTIM. Proposed victims refused by preemption, by why they are protected.",
	}, []string{"reason"})
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
		VerbTotal,
		VerbDuration,
		PreemptVictimsAdded,
		PreemptProtectedTotal,
	)
}

// Handler serves this registry, for mounting on the extender's existing router.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// Registry exposes the registry to tests that need to gather series.
func Registry() *prometheus.Registry { return registry }

// ObserveVerb records one completed extender call. Intended as a deferred
// closure so the result is whatever the call actually returned.
func ObserveVerb(verb, result string, start time.Time) {
	VerbTotal.WithLabelValues(verb, result).Inc()
	ObserveStage(verb, StageTotal, start)
}

// ObserveStage records one stage of a call. Intended as
// `defer metrics.ObserveStage(metrics.VerbBind, metrics.StageLockWait, time.Now())`.
func ObserveStage(verb, stage string, start time.Time) {
	VerbDuration.WithLabelValues(verb, stage).Observe(time.Since(start).Seconds())
}

// RecordNodeReject buckets one rejected node under the verb that rejected it.
// An unlabelled rejection is dropped rather than minting a code="" series.
func RecordNodeReject(verb, code string) {
	if code == "" {
		return
	}
	NodeRejectTotal.WithLabelValues(verb, code).Inc()
}

// ObserveLinkSearch records one executed in-component search.
func ObserveLinkSearch(algo string, candidates int) {
	LinkSearchTotal.WithLabelValues(algo).Inc()
	LinkSearchCandidates.WithLabelValues(algo).Observe(float64(candidates))
}

// LabelOther is the bucket every unrecognised policy / topology value collapses
// into.
const LabelOther = "other"

// PolicyLabel and TopologyLabel map a parsed annotation value onto the CLOSED
// set of label values these metrics are allowed to emit.
//
// They exist because the parsers deliberately pass unknown values through
// verbatim — parseSchedulerPolicy returns SchedulerPolicy(raw) and
// TopologyMode.BaseTopology returns the mode unchanged in their default
// branches, which util's own tests pin ("bogus" must stay "bogus"). That is the
// right behaviour for the scheduler: an unrecognised policy simply does not
// match any comparator and the pod schedules with default ordering.
//
// It is NOT safe as a metric label. The value comes straight from a pod
// annotation, so without this whitelist any tenant able to create a pod could
// mint an unbounded number of Prometheus series inside the scheduler process
// just by varying `nvidia.com/node-scheduler-policy` — and client-side metric
// maps are never evicted, so the memory is held for the process lifetime.
// Bucketing to LabelOther keeps "someone is passing a typo'd policy" visible
// without letting the cardinality follow user input.
func PolicyLabel(p util.SchedulerPolicy) string {
	switch p {
	case util.BinpackPolicy, util.SpreadPolicy, util.NonePolicy:
		return string(p)
	case "":
		return string(util.NonePolicy)
	default:
		return LabelOther
	}
}

// TopologyLabel is PolicyLabel for topology modes. It expects the BASE mode
// (strictness is a separate dimension and is not a label here).
func TopologyLabel(m util.TopologyMode) string {
	switch m {
	case util.NUMATopology, util.LinkTopology, util.NoneTopology:
		return string(m)
	case "":
		return string(util.NoneTopology)
	default:
		return LabelOther
	}
}

// CounterValue sums one counter across the label pairs given, ignoring series
// that do not carry them. Exists for tests: instrumentation nobody asserts on
// silently rots when the code it watches is refactored.
func CounterValue(name string, labels map[string]string) float64 {
	families, err := registry.Gather()
	if err != nil {
		return 0
	}
	total := 0.0
	for _, family := range families {
		if family.GetName() != namespace+"_"+name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := 0
			for _, pair := range metric.GetLabel() {
				if want, ok := labels[pair.GetName()]; ok && want == pair.GetValue() {
					matched++
				}
			}
			if matched == len(labels) {
				total += metric.GetCounter().GetValue()
			}
		}
	}
	return total
}
