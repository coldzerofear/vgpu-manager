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
