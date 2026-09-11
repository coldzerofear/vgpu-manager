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
	"context"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	draclient "k8s.io/dynamic-resource-allocation/client"
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

// GetDraDriverPodInformer returns the pod informer the DRA collector reads.
// With remoteGPU on, the cache must also hold consumer pods on OTHER nodes
// (they hold this node's remote devices), so the node field selector is
// dropped — a cluster-wide pod watch, acceptable on the expected cluster
// sizes but worth revisiting at scale.
func GetDraDriverPodInformer(factory informers.SharedInformerFactory, nodeName string, remoteGPU bool) (cache.SharedIndexInformer, error) {
	var informer cache.SharedIndexInformer
	if remoteGPU {
		informer = factory.Core().V1().Pods().Informer()
	} else {
		informer = factory.InformerFor(&corev1.Pod{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
			fieldSelector := fields.OneTermEqualSelector("spec.nodeName", nodeName)
			watcher := cache.NewListWatchFromClient(k.CoreV1().RESTClient(), "pods", corev1.NamespaceAll, fieldSelector)
			return cache.NewSharedIndexInformer(watcher, &corev1.Pod{}, d, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		})
	}
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

// draListerWatcher builds a ListerWatcher that goes through the DRA client's
// own API-version negotiation, so these informers keep working on a cluster
// that still serves resource.k8s.io as v1beta1/v1beta2: the client converts
// either of them to the v1 types the caches, listers and collectors use.
//
// Building this on draclient.RESTClient() (or on k.ResourceV1().RESTClient(),
// which is what it returns) would pin v1 instead — that method hands back the
// v1 REST client unconditionally, bypassing the negotiation the typed calls
// do, so the informer would retry a 404 forever on such a cluster while every
// other call in the process worked. Monitoring is observability, so it is
// worth being tolerant here; components on the data path pin v1 deliberately
// and check for it at startup (see client.DRAAPIRequirement).
func draListerWatcher[T runtime.Object](
	client *draclient.Client, fieldSelector string,
	list func(context.Context, metav1.ListOptions) (T, error),
	watchFn func(context.Context, metav1.ListOptions) (watch.Interface, error),
) cache.ListerWatcher {
	listWatch := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = fieldSelector
			return list(ctx, options)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = fieldSelector
			return watchFn(ctx, options)
		},
	}
	// The reflector asks the ListerWatcher (not the client) whether
	// watch-list streaming may be used; delegate so the answer stays the
	// client library's own.
	return cache.ToListWatcherWithWatchListSemantics(listWatch, client)
}

// GetResourceSliceInformer watches only the slices this node's driver published.
//
// The driver names its single pool after the node, so spec.pool.name is the
// authoritative ownership key. spec.nodeName is NOT usable: a node with
// RemoteGPUSupport publishes its pool with a nodeSelector (cluster-visible)
// and leaves spec.nodeName empty (design v2.x, D23).
func GetResourceSliceInformer(factory informers.SharedInformerFactory, nodeName string) (cache.SharedIndexInformer, error) {
	return factory.InformerFor(&resourcev1.ResourceSlice{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
		client := draclient.New(k)
		watcher := draListerWatcher(client, fields.AndSelectors(
			// TODO I0901 13:22:40.722465 3051183 reflector.go:490] "Data couldn't be fetched in watchlist mode. Falling back to regular list. This is expected if watchlist is not supported or disabled in kube-apiserver." err="field label not supported for resource.k8s.io/v1, Kind=ResourceSlice: spec.pool.name"
			// E0901 13:22:40.724960 3051183 reflector.go:227] "Failed to watch" err="failed to list *v1.ResourceSlice: field label not supported for resource.k8s.io/v1, Kind=ResourceSlice: spec.pool.name" reflector="pkg/mod/k8s.io/client-go@v0.37.0-rc.0/tools/cache/reflector.go:343" type="*v1.ResourceSlice"
			//fields.OneTermEqualSelector(resourcev1.ResourceSliceSelectorPoolName, nodeName),
			fields.OneTermEqualSelector(resourcev1.ResourceSliceSelectorDriver, util.DRADriverName),
		).String(), client.ResourceSlices().List, client.ResourceSlices().Watch)
		return cache.NewSharedIndexInformer(watcher, &resourcev1.ResourceSlice{}, d, cache.Indexers{})
	}), nil
}

// GetResourceClaimInformer watches every claim in the cluster: the collector
// resolves a pod's claims by name, and a remote consumer's claim lives in the
// consumer pod's namespace, not this node's.
//
// It replaces factory.Resource().V1().ResourceClaims() — same object type, so
// the same factory slot and the same v1 lister — purely to get the API-version
// tolerance described on draListerWatcher.
func GetResourceClaimInformer(factory informers.SharedInformerFactory) (cache.SharedIndexInformer, error) {
	return factory.InformerFor(&resourcev1.ResourceClaim{}, func(k kubernetes.Interface, d time.Duration) cache.SharedIndexInformer {
		client := draclient.New(k)
		claims := client.ResourceClaims(corev1.NamespaceAll)
		watcher := draListerWatcher(client, "", claims.List, claims.Watch)
		// NamespaceIndex is what the generated informer registers, and what
		// the namespaced lister's List path uses to avoid a full scan.
		return cache.NewSharedIndexInformer(watcher, &resourcev1.ResourceClaim{}, d,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	}), nil
}
