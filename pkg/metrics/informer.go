package metrics

import (
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
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
	IndexerKeyPodPlanSchedulingNode  = "pod.planSchedulingNode"
	IndexerKeyPodStatusUnschedulable = "pod.status.unschedulable"
)

func GetPodInformer(factory informers.SharedInformerFactory, nodeName string) (cache.SharedIndexInformer, error) {
	informer := factory.Core().V1().Pods().Informer()
	return informer, informer.AddIndexers(map[string]cache.IndexFunc{
		IndexerKeyPodPlanSchedulingNode: func(obj interface{}) ([]string, error) {
			indexerValue := ""
			if pod, ok := obj.(*corev1.Pod); ok {
				indexerValue = util.PodPlanSchedulingNode(pod)
			}
			return []string{indexerValue}, nil
		},
		IndexerKeyPodStatusUnschedulable: func(obj interface{}) ([]string, error) {
			indexerValue := "false"
			if pod, ok := obj.(*corev1.Pod); ok {
				if device.PodStatusUnschedulable(pod) {
					indexerValue = "true"
				}
			}
			return []string{indexerValue}, nil
		},
	})
}
