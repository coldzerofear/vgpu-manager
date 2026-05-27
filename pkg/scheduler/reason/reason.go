// Package reason centralises the failure-cause vocabulary used by the
// vgpu-manager scheduler when it rejects a node, a pod request, or a pod
// preemption. The goal is twofold:
//
//   - Per-node messages written into the kube-scheduler extender's
//     FailedNodesMap match the upstream FailedScheduling phrasing (short
//     noun phrases like "Insufficient vGPU memory"), so the synthesised
//     "0/N nodes are available: ..." line reads natively in
//     `kubectl describe pod`.
//
//   - Per-pod summary events emitted by vgpu-manager (FilteringFailed,
//     PreemptionFailed) carry both the aggregated counts AND the list of
//     offending node names — the diagnostic detail HAMi provides — but in
//     the upstream "<count> <phrase>" format rather than HAMi's "n/total
//     phrase" style. One event message per Filter call, comma-separated
//     reason clauses, deterministic ordering.
//
// Failure causes flow up the stack as *FilterReason values instead of
// bare error strings: filterDevices builds per-device counts, allocateOne
// promotes them to a node-level reason, and the filter / preempt extenders
// aggregate node-level reasons across the candidate set.
package reason

import (
	"fmt"
	"sort"
	"strings"
)

// Code is a stable machine-readable failure category. Values are kept
// short and CamelCase so they can serve double duty as klog tag values
// (`"reason", code`) and Event message keys without quoting.
//
// IMPORTANT: these strings are part of the public diagnostic contract —
// downstream operators may grep / alert on them. Adding a new code is
// fine; renaming or removing one is a breaking change to that contract.
type Code string

// Node-level codes — produced by nodeFilter before the allocator runs.
// One node can only hit one of these; if the node is missing config at
// all there's nothing the allocator could do anyway.
const (
	NodeNotVGPUEnabled     Code = "NodeNotVGPUEnabled"
	NodeNoVGPURegister     Code = "NodeNoVGPURegister"
	NodeNoGPUConfig        Code = "NodeNoGPUConfig"
	NodeBadGPUConfig       Code = "NodeBadGPUConfig"
	NodeBadMemoryFactor    Code = "NodeBadMemoryFactor"
	NodeMemoryTypeMismatch Code = "NodeMemoryTypeMismatch"
	NodeCacheMiss          Code = "NodeCacheMiss"
	NodeInfoBuildFailed    Code = "NodeInfoBuildFailed"
)

// Device-level codes — filterDevices walks the node's devices and
// increments one of these per rejected device. The eventual node-level
// FilterReason carries a map of Code → count, so callers can show "3/8
// Insufficient vGPU memory" in detailed views while emitting the plain
// "3 Insufficient vGPU memory" in the k8s-style event message.
const (
	DeviceUnhealthy        Code = "DeviceUnhealthy"
	DeviceMIGEnabled       Code = "DeviceMIGEnabled"
	InsufficientVGPUSlot   Code = "InsufficientVGPUSlot"
	InsufficientVGPUMemory Code = "InsufficientVGPUMemory"
	InsufficientVGPUCore   Code = "InsufficientVGPUCore"
	DeviceTypeMismatch     Code = "DeviceTypeMismatch"
	DeviceUUIDMismatch     Code = "DeviceUUIDMismatch"
)

// Container / topology-level codes — produced by allocateOne or
// allocateByTopologyMode after the per-device filter pass.
const (
	InsufficientGPUCards      Code = "InsufficientGPUCards"
	InsufficientGPUResources  Code = "InsufficientGPUResources"
	LinkTopologyUnsatisfied   Code = "LinkTopologyUnsatisfied"
	NUMATopologyUnsatisfied   Code = "NUMATopologyUnsatisfied"
	AlreadyScheduledElsewhere Code = "AlreadyScheduledElsewhere"
)

// phrase is the short, k8s-style noun phrase rendered into Event
// messages and FailedNodesMap entries. Style rules: leading capital, no
// trailing punctuation, no embedded node name (the caller appends it).
//
// Keep these short — kube-scheduler joins them with commas into one
// "0/N nodes are available: ..." line, and long phrases push the
// message past the readable width.
var phrase = map[Code]string{
	NodeNotVGPUEnabled:     "node not vGPU-enabled",
	NodeNoVGPURegister:     "node has no vGPU registered",
	NodeNoGPUConfig:        "node has no GPU configuration",
	NodeBadGPUConfig:       "node GPU configuration invalid",
	NodeBadMemoryFactor:    "node memory factor invalid",
	NodeMemoryTypeMismatch: "node memory type mismatch",
	NodeCacheMiss:          "node missing from cache",
	NodeInfoBuildFailed:    "node info build failed",

	DeviceUnhealthy:        "GPU unhealthy",
	DeviceMIGEnabled:       "GPU has MIG enabled",
	InsufficientVGPUSlot:   "Insufficient vGPU slots",
	InsufficientVGPUMemory: "Insufficient vGPU memory",
	InsufficientVGPUCore:   "Insufficient vGPU cores",
	DeviceTypeMismatch:     "GPU type mismatch",
	DeviceUUIDMismatch:     "GPU UUID mismatch",

	InsufficientGPUCards:      "Insufficient GPU cards",
	InsufficientGPUResources:  "Insufficient GPU resources",
	LinkTopologyUnsatisfied:   "Link topology unsatisfied",
	NUMATopologyUnsatisfied:   "NUMA topology unsatisfied",
	AlreadyScheduledElsewhere: "pod already scheduled to another node",
}

// Phrase returns the human-readable short form for a Code. Unknown
// codes return the Code string verbatim so unrecognised values surface
// instead of getting masked by an empty string.
func Phrase(c Code) string {
	if p, ok := phrase[c]; ok {
		return p
	}
	return string(c)
}

// FilterReason captures why one candidate node was rejected. It carries
// enough structure for three different consumers:
//
//   - The per-node FailedNodesMap (kube-scheduler's view) takes
//     Reason.Short().
//   - The vgpu-manager FilteringFailed event aggregator buckets nodes
//     by Primary across the whole candidate set.
//   - klog at V(5) gets Reason.Detailed(), which includes the per-Code
//     counts and any Detail string.
//
// Counts is the per-device breakdown produced by filterDevices, e.g.
// {InsufficientVGPUMemory: 3, DeviceTypeMismatch: 2} for an 8-GPU node
// where 3 cards lacked memory and 2 had the wrong type. For node-level
// failures (the node itself doesn't qualify) Counts is empty and
// Primary alone tells the story.
//
// TotalDevices is the candidate's physical GPU count, used purely for
// detailed rendering ("3/8 ..."); the aggregate event drops the
// denominator because k8s style is "3 <phrase>" not "3/N <phrase>".
//
// Detail is an optional free-form clause appended to the verbose form
// for klog only — never inserted into the event message, which must
// stay deterministic across pods.
type FilterReason struct {
	Primary      Code
	Counts       map[Code]int
	TotalDevices int
	Detail       string
}

// New builds a node-level reason with no per-device counts. Use this for
// nodeFilter rejections and for "the whole node failed before we got to
// individual devices" situations.
func New(c Code) *FilterReason {
	return &FilterReason{Primary: c}
}

// FromCounts picks the most frequent device-level Code as Primary and
// keeps the full counts map for verbose rendering. Returns nil when
// counts is empty (the caller should not have called us at all).
//
// Ties are broken by Code lexical order, so two pods with identical
// failure shapes produce identical messages — handy for log grepping
// and for tests.
func FromCounts(counts map[Code]int, totalDevices int) *FilterReason {
	if len(counts) == 0 {
		return nil
	}
	primary := pickPrimary(counts)
	return &FilterReason{
		Primary:      primary,
		Counts:       counts,
		TotalDevices: totalDevices,
	}
}

// WithDetail returns a copy with Detail attached (chainable on New /
// FromCounts). Used for "needs %d MB" style parameter info that only
// belongs in klog.
func (r *FilterReason) WithDetail(format string, args ...any) *FilterReason {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Detail = fmt.Sprintf(format, args...)
	return &cp
}

// Short returns the phrase that goes into FailedNodesMap (and thence
// into kube-scheduler's FailedScheduling line). No counts, no node
// name, no punctuation — just the noun phrase.
func (r *FilterReason) Short() string {
	if r == nil {
		return ""
	}
	return Phrase(r.Primary)
}

// Detailed returns the verbose form for klog, including the per-Code
// counts and any Detail. Format:
//
//	"<Primary phrase> (3/8 InsufficientVGPUMemory, 2/8 DeviceTypeMismatch)
//	 [needs 8192 MB]"
//
// Codes inside the parentheses are sorted by descending count, then by
// Code lexicographically, so the output is deterministic.
func (r *FilterReason) Detailed() string {
	if r == nil {
		return ""
	}
	out := Phrase(r.Primary)
	if len(r.Counts) > 0 {
		out += " (" + renderCounts(r.Counts, r.TotalDevices) + ")"
	}
	if r.Detail != "" {
		out += " [" + r.Detail + "]"
	}
	return out
}

// FormatAggregate produces the k8s-style "0/N nodes are available: ..."
// line that vgpu-manager emits as the FilteringFailed event. perNode
// maps nodeName → its rejection reason; the function buckets nodes by
// Primary, sorts within each bucket, and formats one comma-separated
// summary suitable for a single Event message.
//
// Style mirrors kube-scheduler's own FailedScheduling phrasing:
//
//	"0/12 nodes are available: 4 Link topology unsatisfied (a,b,c,d),
//	 3 Insufficient vGPU memory (e,f,g), 5 node has no vGPU registered."
//
// Buckets are ordered by descending count so the dominant failure
// surfaces first; equal counts fall back to Code lexical order to keep
// output stable across runs.
//
// nodesPerBucketLimit caps how many node names appear inside each
// "(...)" — anything past the limit collapses into "(... and K more)"
// to keep the message readable on large clusters. 0 means "include all"
// (suitable for tests).
func FormatAggregate(totalNodes int, perNode map[string]*FilterReason, nodesPerBucketLimit int) string {
	if totalNodes <= 0 || len(perNode) == 0 {
		// Defensive: shouldn't be called in either case but produce a
		// sane fallback rather than an empty Event message.
		return fmt.Sprintf("0/%d nodes are available", totalNodes)
	}
	buckets := make(map[Code][]string, len(perNode))
	for nodeName, r := range perNode {
		if r == nil {
			continue
		}
		buckets[r.Primary] = append(buckets[r.Primary], nodeName)
	}
	for _, ns := range buckets {
		sort.Strings(ns)
	}

	type entry struct {
		code  Code
		nodes []string
	}
	entries := make([]entry, 0, len(buckets))
	for c, ns := range buckets {
		entries = append(entries, entry{c, ns})
	}
	sort.Slice(entries, func(i, j int) bool {
		if len(entries[i].nodes) != len(entries[j].nodes) {
			return len(entries[i].nodes) > len(entries[j].nodes)
		}
		return entries[i].code < entries[j].code
	})

	clauses := make([]string, 0, len(entries))
	for _, e := range entries {
		clauses = append(clauses, formatClause(e.code, e.nodes, nodesPerBucketLimit))
	}
	return fmt.Sprintf("0/%d nodes are available: %s.", totalNodes, strings.Join(clauses, ", "))
}

// --- internals -------------------------------------------------------

func pickPrimary(counts map[Code]int) Code {
	type kv struct {
		c Code
		n int
	}
	all := make([]kv, 0, len(counts))
	for c, n := range counts {
		all = append(all, kv{c, n})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].c < all[j].c
	})
	return all[0].c
}

func renderCounts(counts map[Code]int, total int) string {
	type kv struct {
		c Code
		n int
	}
	all := make([]kv, 0, len(counts))
	for c, n := range counts {
		all = append(all, kv{c, n})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].c < all[j].c
	})
	parts := make([]string, 0, len(all))
	for _, e := range all {
		if total > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d %s", e.n, total, e.c))
		} else {
			parts = append(parts, fmt.Sprintf("%d %s", e.n, e.c))
		}
	}
	return strings.Join(parts, ", ")
}

func formatClause(c Code, nodes []string, limit int) string {
	count := len(nodes)
	p := Phrase(c)
	if count == 0 {
		return p
	}
	if limit > 0 && count > limit {
		return fmt.Sprintf("%d %s (%s and %d more)",
			count, p, strings.Join(nodes[:limit], ","), count-limit)
	}
	return fmt.Sprintf("%d %s (%s)", count, p, strings.Join(nodes, ","))
}
