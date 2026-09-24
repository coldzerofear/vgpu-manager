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

package remoteagent

// Pod-owned sessions (device-plugin path). The agent watches the remote pods
// the scheduler placed on this node's GPUs and this node's own object; it
// never touches the DRA API, so it also runs on clusters without it.

import (
	"context"
	"math"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
)

const podUIDIndex = "pod-uid"

func (a *Agent) podResourceEventHandler() cache.ResourceEventHandler {
	return &cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if pod, ok := obj.(*corev1.Pod); ok {
				a.sweepPod(pod)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if pod, ok := newObj.(*corev1.Pod); ok {
				a.sweepPod(pod)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			if pod, ok := obj.(*corev1.Pod); ok {
				a.store.Sweep(string(pod.UID), nil, math.MaxInt64)
			}
		},
	}
}

func (a *Agent) nodeResourceEventHandler() cache.ResourceEventHandler {
	return &cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { a.refreshNodeDevicesFromNode(obj) },
		UpdateFunc: func(_, newObj interface{}) { a.refreshNodeDevicesFromNode(newObj) },
	}
}

// startPodInformers watches the pods whose GPUs are on this node -- the
// scheduler labels them metrics-node=<node> -- and the node object the device
// plugin publishes its device registry on. Blocks until both caches are synced.
func (a *Agent) startPodInformers(ctx context.Context) error {
	nodeRegistration, err := a.nodeInformer.AddEventHandler(a.nodeResourceEventHandler())
	if err != nil {
		return err
	}
	podFactory := informers.NewSharedInformerFactoryWithOptions(a.cfg.ClientSets.Core,
		10*time.Hour, informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = labels.Set{util.PodMetricsNodeLabel: a.cfg.NodeName}.String()
		}))
	a.podInformer = podFactory.Core().V1().Pods().Informer()
	if err = a.podInformer.AddIndexers(podIndexers()); err != nil {
		return err
	}
	if err = a.podInformer.SetTransform(crcache.TransformStripManagedFields()); err != nil {
		return err
	}
	podRegistration, err := a.podInformer.AddEventHandler(a.podResourceEventHandler())
	if err != nil {
		return err
	}

	a.podCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(),
		a.podInformer.GetStore(),
		a.podInformer.GetIndexer(),
		time.Minute, true,
	)

	a.addReady(
		a.podInformer.HasSynced,
		a.nodeInformer.HasSynced,
		podRegistration.HasSynced,
		nodeRegistration.HasSynced,
	)
	a.wg.Go(func() { a.podInformer.RunWithContext(ctx) })
	a.wg.Go(func() { a.nodeInformer.RunWithContext(ctx) })

	a.healthSynced = append(a.healthSynced, a.podInformer.HasSynced)

	return nil
}

// podIndexers looks pods up by UID, which is what a session request carries.
func podIndexers() cache.Indexers {
	return cache.Indexers{
		podUIDIndex: func(obj interface{}) ([]string, error) {
			if pod, ok := obj.(*corev1.Pod); ok {
				return []string{string(pod.UID)}, nil
			}
			return nil, nil
		},
	}
}

func (a *Agent) GetPodByUID(uid string) (*corev1.Pod, error) {
	objs, err := a.podCache.ByIndex(podUIDIndex, uid)
	if err != nil {
		return nil, err
	}
	if len(objs) == 0 {
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), uid)
	}
	return objs[0].(*corev1.Pod), nil
}

// refreshNodeDevicesFromNode rebuilds the pod-session device snapshot from
// what the device plugin publishes on this node.
func (a *Agent) refreshNodeDevicesFromNode(obj interface{}) {
	node, ok := obj.(*corev1.Node)
	if !ok || node.Name != a.cfg.NodeName {
		return
	}
	nd, err := NodeDevicesFromNode(node)
	if err != nil {
		klog.Warningf("node device snapshot: %v", err)
		return
	}
	a.podDevices.Store(nd)
	klog.V(4).Infof("Node device snapshot from registry: %d device(s), CUDA %q", len(nd.Devices), nd.CudaVersionString())
}

// podUsesNodeDevices reports whether the pod is a live remote pod whose
// devices the scheduler took from this node.
func podUsesNodeDevices(pod *corev1.Pod, nodeName string) bool {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}
	if mode, _ := util.PodVGPUAccessMode(pod); mode != util.AccessModeRemote {
		return false
	}
	return util.PodPlanSchedulingNode(pod) == nodeName
}

// podForSession returns the pod an EnsureSession request may build its session
// from, and the container the token belongs to. The token is derived from the
// pod UID and a container name, so authorization is: the pod still uses this
// node's GPUs, and one of the containers the scheduler pre-allocated devices
// to has exactly this token. A cache that is behind is not an error -- the
// pod is then read from the API.
func (a *Agent) podForSession(ctx context.Context, session, uid, namespace, name, resourceVersion string) (*corev1.Pod, string, error) {
	if uid == "" {
		return nil, "", status.Error(codes.InvalidArgument, "pod uid is required")
	}
	wantRV := objectRV(resourceVersion)
	pod, err := a.GetPodByUID(uid)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, "", status.Errorf(codes.Unavailable, "get pod failed: %v", err)
	}
	if pod == nil || objectRV(pod.ResourceVersion) < wantRV {
		pod, err = a.cfg.ClientSets.Core.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || (err == nil && string(pod.UID) != uid) {
			return nil, "", status.Errorf(codes.NotFound, "pod %s not found", uid)
		}
		if err != nil {
			return nil, "", status.Errorf(codes.Unavailable, "get pod failed: %v", err)
		}
	}
	a.podCache.Mutation(pod)
	if !podUsesNodeDevices(pod, a.cfg.NodeName) {
		return nil, "", status.Errorf(codes.PermissionDenied,
			"pod %s does not use the GPUs of node %s", klog.KObj(pod), a.cfg.NodeName)
	}
	container, ok := PodSessionContainer(pod, session)
	if !ok {
		return nil, "", status.Errorf(codes.PermissionDenied,
			"session %s is not a session of pod %s", session, klog.KObj(pod))
	}
	return pod, container, nil
}

// ensurePodSession materializes the session of one container of a remote pod.
func (a *Agent) ensurePodSession(ctx context.Context, req *remoteagent.EnsureSessionRequest, nd *NodeDevices) error {
	pod, container, err := a.podForSession(ctx, req.Session, req.ClaimUid, req.ClaimNamespace, req.ClaimName, req.ClaimResourceVersion)
	if err != nil {
		return err
	}
	spec, err := PodSessionSpec(pod, container, nd)
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	node, _ := a.nodeLister.Get(a.cfg.NodeName)
	policy := vgpu.GetDefaultComputePolicy(pod, node)
	if err = a.store.Materialize(req.Session, spec, nd, policy); err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return nil
}

// sweepPod removes the sessions this version of the pod no longer uses. A pod
// that is only being deleted keeps them: its containers run until the object
// is gone, and the delete event sweeps the rest.
func (a *Agent) sweepPod(pod *corev1.Pod) {
	keep := sets.New[string]()
	if podUsesNodeDevices(pod, a.cfg.NodeName) {
		keep.Insert(PodSessionTokens(pod)...)
	}
	if removed := a.store.Sweep(string(pod.UID), keep, objectRV(pod.ResourceVersion)); removed > 0 {
		klog.V(2).Infof("Swept %d stale session(s) of pod %s (rv %s)", removed, klog.KObj(pod), pod.ResourceVersion)
	}
}

// gcPodSessions is the periodic backstop for one pod's sessions.
func (a *Agent) gcPodSessions(uid string) {
	pod, err := a.GetPodByUID(uid)
	if apierrors.IsNotFound(err) {
		a.store.Sweep(uid, nil, math.MaxInt64)
		return
	}
	if err != nil {
		klog.Warningf("gc sessions of pod %s: %v", uid, err)
		return
	}
	a.sweepPod(pod)
}
