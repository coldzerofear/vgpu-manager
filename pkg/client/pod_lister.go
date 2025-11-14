package client

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// PodLister helps list Pods.
// All objects returned here must be treated as read-only.
type PodLister interface {
	listerv1.PodLister
	ListByIndexFiled(fieldSet fields.Set) ([]*corev1.Pod, error)
}

// podLister implements the PodLister interface.
type podLister struct {
	listerv1.PodLister
	indexer cache.Indexer
}

// NewPodLister returns a new PodLister.
func NewPodLister(indexer cache.Indexer) PodLister {
	lister := listerv1.NewPodLister(indexer)
	return &podLister{
		PodLister: lister,
		indexer:   indexer,
	}
}

func (s *podLister) ListByIndexFiled(fieldSet fields.Set) ([]*corev1.Pod, error) {
	requirements := fieldSet.AsSelector().Requirements()
	if len(requirements) == 0 {
		return s.List(labels.Everything())
	}
	// Obtain stable indexer conditional filtering through sorting.
	sort.Slice(requirements, func(i, j int) bool {
		return requirements[i].Field < requirements[j].Field
	})
	return byIndexes[*corev1.Pod](s.indexer, requirements)
}

func byIndexes[T runtime.Object](indexer cache.Indexer, requires fields.Requirements) ([]T, error) {
	var (
		err      error
		objs     []interface{}
		objects  []T
		indexers = indexer.GetIndexers()
	)
	for idx, req := range requires {
		indexName := req.Field
		indexedValue := req.Value
		if idx == 0 {
			// we use first require to get snapshot data
			// TODO(halfcrazy): use complicated index when client-go provides byIndexes
			// https://github.com/kubernetes/kubernetes/issues/109329
			objs, err = indexer.ByIndex(indexName, indexedValue)
			if err != nil {
				return nil, err
			}
		}
		if idx == len(requires)-1 {
			objects, err = filterObjects[T](indexers, indexName, indexedValue, objs)
			if err != nil {
				return nil, err
			}
			if len(objects) == 0 {
				return nil, nil
			}
		} else {
			objs, err = filterObjects[interface{}](indexers, indexName, indexedValue, objs)
			if err != nil {
				return nil, err
			}
			if len(objs) == 0 {
				return nil, nil
			}
		}
	}
	return objects, nil
}

func filterObjects[T interface{}](indexers cache.Indexers, indexName, indexedValue string, objs []interface{}) ([]T, error) {
	if len(objs) == 0 {
		return nil, nil
	}
	fn, exist := indexers[indexName]
	if !exist {
		return nil, fmt.Errorf("index with name %s does not exist", indexName)
	}
	filteredObjects := make([]T, 0, len(objs))
	for _, obj := range objs {
		vals, err := fn(obj)
		if err != nil {
			return nil, err
		}
		for _, val := range vals {
			if val == indexedValue {
				filteredObjects = append(filteredObjects, obj.(T))
				break
			}
		}
	}
	return filteredObjects, nil
}
