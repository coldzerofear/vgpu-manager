package predicate

import (
	"context"

	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

type FilterPredicate interface {
	// Name returns the name of this predictor.
	Name() string
	// Filter returns the filter result of predictor, this will tell the suitable nodes to running pod.
	Filter(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult
	// IsReady Return whether the current plugin is ready.
	IsReady(ctx context.Context) bool
}

type BindPredicate interface {
	// Name returns the name of this predictor.
	Name() string
	// Bind Finally bind the pod to a specific node.
	Bind(ctx context.Context, args extenderv1.ExtenderBindingArgs) *extenderv1.ExtenderBindingResult
	// IsReady Return whether the current plugin is ready.
	IsReady(ctx context.Context) bool
}

// PreemptPredicate refines the victim set proposed by kube-scheduler's
// default preemption against vGPU resource constraints that in-tree filter
// plugins cannot evaluate (per-device packing, UUID / type / NUMA topology).
//
// Per kube-scheduler 1.32 source, callExtenders runs only AFTER the in-tree
// dry-run produces at least one candidate node with a non-empty victim list.
// This plugin therefore cannot rescue nodes that in-tree dropped, but it can
// add/remove victims on the candidates in-tree did propose so the actual
// eviction respects our resource view.
type PreemptPredicate interface {
	Name() string
	Preempt(ctx context.Context, args extenderv1.ExtenderPreemptionArgs) *extenderv1.ExtenderPreemptionResult
	IsReady(ctx context.Context) bool
}
