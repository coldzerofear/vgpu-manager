package metrics

import (
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

func GetNodeInformer(factory informers.SharedInformerFactory, nodeName string) (cache.SharedIndexInformer, error) {
	return factory.InformerFor(&corev1.Node{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
		watcher := cache.NewListWatchFromClient(k.CoreV1().RESTClient(), "nodes",
			corev1.NamespaceAll, fields.OneTermEqualSelector("metadata.name", nodeName))
		return cache.NewSharedIndexInformer(watcher, &corev1.Node{}, d, cache.Indexers{})
	}), nil
}

const (
	IndexerKeyPodPlanSchedulingNode        = "pod.planSchedulingNode"
	IndexerKeyPodDeviceAllocationCountable = "pod.device.allocation.countable"
)

func GetPodInformer(factory informers.SharedInformerFactory, nodeName string) (cache.SharedIndexInformer, error) {
	informer := factory.InformerFor(&corev1.Pod{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
		watcher := cache.NewFilteredListWatchFromClient(k.CoreV1().RESTClient(),
			"pods", corev1.NamespaceAll, func(options *metav1.ListOptions) {
				options.LabelSelector = labels.Set{util.PodMetricsNodeLabel: nodeName}.String()
			})
		indexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
		return cache.NewSharedIndexInformer(watcher, &corev1.Pod{}, d, indexers)
	})
	return informer, informer.AddIndexers(map[string]cache.IndexFunc{
		IndexerKeyPodPlanSchedulingNode: func(obj interface{}) ([]string, error) {
			var indexerValues []string
			if pod, ok := obj.(*corev1.Pod); ok {
				indexerValues = []string{util.PodPlanSchedulingNode(pod)}
			}
			return indexerValues, nil
		},
		IndexerKeyPodDeviceAllocationCountable: func(obj interface{}) ([]string, error) {
			indexerValue := "false"
			if pod, ok := obj.(*corev1.Pod); ok {
				if device.ShouldCountPodDeviceAllocation(pod) {
					indexerValue = "true"
				}
			}
			return []string{indexerValue}, nil
		},
	})
}
