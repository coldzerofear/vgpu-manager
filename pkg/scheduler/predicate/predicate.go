package predicate

import (
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

type FilterPredicate interface {
	// Name returns the name of this predictor
	Name() string
	// Filter returns the filter result of predictor, this will tell the suitable nodes to running
	// pod
	Filter(args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult
}

type BindPredicate interface {
	// Name returns the name of this predictor
	Name() string
	// Bind Finally bind the pod to a specific node
	Bind(args extenderv1.ExtenderBindingArgs) *extenderv1.ExtenderBindingResult
}
