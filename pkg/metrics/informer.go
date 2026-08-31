/*
Copyright 2024-2026 coldzerofear

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

package metrics

import (
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
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
	IndexerKeyPodNodeName                  = "pod.spec.nodeName"
	IndexerKeyPodPlanSchedulingNode        = "pod.planSchedulingNode"
	IndexerKeyPodDeviceAllocationCountable = "pod.device.allocation.countable"
)

func GetDraDriverPodInformer(factory informers.SharedInformerFactory, nodeName string) (cache.SharedIndexInformer, error) {
	informer := factory.InformerFor(&corev1.Pod{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
		watcher := cache.NewListWatchFromClient(k.CoreV1().RESTClient(), "pods",
			corev1.NamespaceAll, fields.OneTermEqualSelector("spec.nodeName", nodeName))
		indexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
		return cache.NewSharedIndexInformer(watcher, &corev1.Pod{}, d, indexers)
	})
	return informer, informer.AddIndexers(map[string]cache.IndexFunc{
		IndexerKeyPodNodeName: func(obj interface{}) ([]string, error) {
			var indexerValues []string
			if pod, ok := obj.(*corev1.Pod); ok && pod.Spec.NodeName != "" {
				indexerValues = []string{pod.Spec.NodeName}
			}
			return indexerValues, nil
		},
	})
}

func GetDevicePluginPodInformer(factory informers.SharedInformerFactory, nodeName string) (cache.SharedIndexInformer, error) {
	informer := factory.InformerFor(&corev1.Pod{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
		watcher := cache.NewFilteredListWatchFromClient(k.CoreV1().RESTClient(), "pods",
			corev1.NamespaceAll, func(options *metav1.ListOptions) {
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

// GetResourceSliceInformer watches only the slices this node's driver published.
//
// The driver names its single pool after the node, so spec.pool.name is the
// authoritative ownership key. spec.nodeName is NOT usable: a node with
// RemoteGPUSupport publishes its pool with a nodeSelector (cluster-visible)
// and leaves spec.nodeName empty (design v2.x, D23).
func GetResourceSliceInformer(factory informers.SharedInformerFactory, nodeName string) (cache.SharedIndexInformer, error) {
	return factory.InformerFor(&resourcev1.ResourceSlice{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
		watcher := cache.NewListWatchFromClient(k.ResourceV1().RESTClient(), "resourceslices",
			corev1.NamespaceAll, fields.AndSelectors(
				fields.OneTermEqualSelector(resourcev1.ResourceSliceSelectorPoolName, nodeName),
				fields.OneTermEqualSelector(resourcev1.ResourceSliceSelectorDriver, util.DRADriverName),
			))
		return cache.NewSharedIndexInformer(watcher, &resourcev1.ResourceSlice{}, d, cache.Indexers{})
	}), nil
}
