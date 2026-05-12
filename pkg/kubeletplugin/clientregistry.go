/*
 * Copyright (c) 2024-2026, vgpu-manager authors. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License").
 */

package kubeletplugin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/claimresolve"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/sets"
	listerv1 "k8s.io/client-go/listers/core/v1"
	resourcev1 "k8s.io/client-go/listers/resource/v1"
	kcache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ClaimUUIDIndex names the indexer on the ResourceClaim informer that maps a
// register UUID — embedded in the claim annotation "<driver>/<uuid>" — back
// to its owning claim.
const ClaimUUIDIndex = "manager.register.uuid"

// MakeClaimUUIDIndexFunc returns an IndexFunc that emits every register UUID
// previously assigned by the driver. The annotation prefix is the driver name
// followed by '/'; anything matching that shape is treated as a UUID entry.
func MakeClaimUUIDIndexFunc(driverName string) kcache.IndexFunc {
	prefix := driverName + "/"
	return func(obj interface{}) ([]string, error) {
		accessor, err := meta.Accessor(obj)
		if err != nil {
			return nil, fmt.Errorf("object has no meta: %w", err)
		}
		annotations := accessor.GetAnnotations()
		out := make([]string, 0, len(annotations))
		for key := range annotations {
			if uuid, found := strings.CutPrefix(key, prefix); found && uuid != "" {
				out = append(out, uuid)
			}
		}
		return out, nil
	}
}

// AllocatedVGPURequestsFunc derives the set of mainRequest names in a claim
// that have been allocated to a vGPU device owned by this driver. It must be
// a pure function over the claim plus driver-local device inventory so that
// it can be safely called from the hot register-resolve path.
type AllocatedVGPURequestsFunc func(claim *resourceapi.ResourceClaim) sets.Set[string]

// ClientRegisterResolver answers two questions for the DRA registry server:
//
//  1. Which pod owns this register UUID and which container in that pod is
//     currently the one running?
//  2. Where on disk should pids.config be written so that the user-container's
//     vGPU library can read it?
//
// The translation walks:
//
//	uuid → claim          (via the UUID index on the claim informer)
//	     → partition key  (via the claim annotation "<driver>/<uuid>")
//	     → partition      (recomputed via claimresolve from spec + reservedFor)
//	     → running container (init or app; chosen by container status)
//
// Init and app containers in the same partition are mutually exclusive at
// runtime per the K8s init/app lifecycle, so picking the unique container in
// Running state unambiguously identifies the caller.
type ClientRegisterResolver struct {
	podLister             client.PodLister
	claimIndexer          kcache.Indexer
	contPath              string
	driverName            string
	allocatedVGPURequests AllocatedVGPURequestsFunc

	reader claimresolve.Reader
}

// NewClientRegisterResolver builds a resolver bound to the supplied informer
// caches. contPath is the directory whose `claims/` subtree mirrors the
// partition layout written during prepare.
func NewClientRegisterResolver(
	podLister client.PodLister,
	claimIndexer kcache.Indexer,
	contPath, driverName string,
	allocatedVGPURequests AllocatedVGPURequestsFunc,
) *ClientRegisterResolver {
	r := &ClientRegisterResolver{
		podLister:             podLister,
		claimIndexer:          claimIndexer,
		contPath:              contPath,
		driverName:            driverName,
		allocatedVGPURequests: allocatedVGPURequests,
	}
	claimLister := resourcev1.NewResourceClaimLister(claimIndexer)
	r.reader = &informerCacheReader{
		podLister:   podLister,
		claimLister: claimLister,
	}
	return r
}

// PodByUID is the legacy device-plugin lookup: the calling library already
// knows its pod UID and we just hand back the cached pod object.
func (r *ClientRegisterResolver) PodByUID(_ context.Context, uid string) (*corev1.Pod, error) {
	pods, err := r.podLister.ListByIndexValue("metadata.uid", uid)
	if err != nil {
		return nil, err
	}
	if len(pods) != 1 {
		if len(pods) > 1 {
			klog.ErrorS(nil, "find multiple pods matching UID", "uid", uid, "pods", util.ObjectKeys(pods...))
		}
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), "uid "+uid)
	}
	return pods[0], nil
}

// TargetByUUID implements registry.GetTargetByUUIDFunc.
func (r *ClientRegisterResolver) TargetByUUID(ctx context.Context, uuid string) (*registry.Target, error) {
	claim, partitionKey, err := r.lookupClaim(uuid)
	if err != nil {
		return nil, err
	}

	allocatedRequests := r.allocatedVGPURequests(claim)
	if allocatedRequests.Len() == 0 {
		return nil, fmt.Errorf("claim %s/%s has no vGPU allocated requests",
			claim.Namespace, claim.Name)
	}

	info, err := claimresolve.ResolveClaimVGPUPartitionsFromAllocatedRequests(ctx, r.reader, claim, allocatedRequests)
	if err != nil {
		return nil, fmt.Errorf("resolve partitions for claim %s/%s: %w",
			claim.Namespace, claim.Name, err)
	}
	detail, ok := info.Partitions[partitionKey]
	if !ok {
		return nil, fmt.Errorf("partition %q not present in claim %s/%s (have %d partitions)",
			partitionKey, claim.Namespace, claim.Name, len(info.Partitions))
	}

	pod, containerName, err := r.pickRunningContainer(detail)
	if err != nil {
		return nil, fmt.Errorf("partition %q in claim %s/%s: %w",
			partitionKey, claim.Namespace, claim.Name, err)
	}

	return &registry.Target{
		Pod:           pod,
		ContainerName: containerName,
		ConfigDir: filepath.Join(
			r.contPath, util.Claims, string(claim.UID), partitionKey, util.Config,
		),
	}, nil
}

// lookupClaim resolves a UUID to its single owning claim plus the partition
// key recorded for that UUID.
func (r *ClientRegisterResolver) lookupClaim(uuid string) (*resourceapi.ResourceClaim, string, error) {
	objs, err := r.claimIndexer.ByIndex(ClaimUUIDIndex, uuid)
	if err != nil {
		return nil, "", fmt.Errorf("index lookup for uuid %s: %w", uuid, err)
	}
	if len(objs) != 1 {
		if len(objs) > 1 {
			klog.ErrorS(nil, "find multiple claims matching uuid", "uuid", uuid, "claims", objs)
		}
		return nil, "", apierrors.NewNotFound(resourceapi.Resource("resourceclaims"), "register uuid "+uuid)
	}
	claim, ok := objs[0].(*resourceapi.ResourceClaim)
	if !ok {
		return nil, "", fmt.Errorf("unexpected indexer entry type %T", objs[0])
	}
	annotationKey := r.driverName + "/" + uuid
	partitionKey, ok := claim.Annotations[annotationKey]
	if !ok || partitionKey == "" {
		return nil, "", fmt.Errorf("claim %s/%s missing annotation %q",
			claim.Namespace, claim.Name, annotationKey)
	}
	return claim, partitionKey, nil
}

// pickRunningContainer walks the partition's container list (entries of the
// form "<podUID>/<kind>/<containerName>" produced by claimresolve) and returns
// the first one whose container is in the Running state.
//
// Pod admission rules guarantee a partition contains at most one init and one
// app container; K8s lifecycle guarantees they don't run concurrently. So the
// "first Running" container is well-defined whenever a register call is in
// flight.
func (r *ClientRegisterResolver) pickRunningContainer(detail claimresolve.PartitionDetail) (*corev1.Pod, string, error) {
	if len(detail.Containers) == 0 {
		return nil, "", fmt.Errorf("partition contains no containers")
	}
	var seen []string
	for _, key := range detail.Containers {
		parts := strings.SplitN(key, "/", 3)
		if len(parts) != 3 {
			return nil, "", fmt.Errorf("malformed container key %q", key)
		}
		podUID, _, containerName := parts[0], parts[1], parts[2]
		seen = append(seen, podUID+":"+containerName)

		pod, err := r.PodByUID(context.Background(), podUID)
		if err != nil {
			continue // pod might not be in the cache yet; try next candidate
		}
		if util.PodIsTerminated(pod) {
			continue
		}
		status, ok := findContainerStatus(pod, containerName)
		if !ok {
			continue
		}
		if status.State.Running != nil {
			return pod, containerName, nil
		}
	}

	return nil, "", fmt.Errorf("no candidate container is currently running (candidates: %v)", seen)
}

// findContainerStatus searches both InitContainerStatuses and
// ContainerStatuses for the named container.
func findContainerStatus(pod *corev1.Pod, name string) (*corev1.ContainerStatus, bool) {
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == name {
			return &pod.Status.InitContainerStatuses[i], true
		}
	}
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i], true
		}
	}
	return nil, false
}

// informerCacheReader adapts the resolver's pod lister and claim indexer to
// the claimresolve.Reader interface. It avoids API calls in the resolve path
// by serving from the local informer caches.
type informerCacheReader struct {
	podLister   listerv1.PodLister
	claimLister resourcev1.ResourceClaimLister
}

func (r *informerCacheReader) GetPod(_ context.Context, key ctrlclient.ObjectKey, out *corev1.Pod) error {
	pod, err := r.podLister.Pods(key.Namespace).Get(key.Name)
	if err != nil {
		return err
	}
	pod.DeepCopyInto(out)
	return nil
}

func (r *informerCacheReader) GetResourceClaim(_ context.Context, key ctrlclient.ObjectKey, out *resourceapi.ResourceClaim) error {
	claim, err := r.claimLister.ResourceClaims(key.Namespace).Get(key.Name)
	if err != nil {
		return err
	}
	claim.DeepCopyInto(out)
	return nil
}
