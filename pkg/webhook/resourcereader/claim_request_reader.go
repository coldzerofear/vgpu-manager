package resourcereader

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kcache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ClaimRequestReader interface {
	GetDeviceRequestsForPodClaim(ctx context.Context, namespace string, podClaim *corev1.PodResourceClaim) ([]resourceapi.DeviceRequest, error)
	Mutation(obj client.Object)
}

type claimRequestReader struct {
	cachedClient client.Client
	liveClient   client.Client

	claimMutationCache    kcache.MutationCache
	templateMutationCache kcache.MutationCache
}

func NewClaimRequestReader(cachedClient, liveClient client.Client, claimIndexer, templateIndexer kcache.Indexer, ttl time.Duration) ClaimRequestReader {
	return &claimRequestReader{
		cachedClient: cachedClient,
		liveClient:   liveClient,
		claimMutationCache: kcache.NewIntegerResourceVersionMutationCache(
			klog.Background(), claimIndexer, claimIndexer, ttl, true,
		),
		templateMutationCache: kcache.NewIntegerResourceVersionMutationCache(
			klog.Background(), templateIndexer, templateIndexer, ttl, true,
		),
	}
}

func (r *claimRequestReader) GetDeviceRequestsForPodClaim(ctx context.Context, namespace string, podClaim *corev1.PodResourceClaim) ([]resourceapi.DeviceRequest, error) {
	if podClaim == nil {
		return nil, fmt.Errorf("podClaim is nil")
	}

	if podClaim.ResourceClaimName != nil && *podClaim.ResourceClaimName != "" {
		claim, err := r.getResourceClaim(ctx, types.NamespacedName{Namespace: namespace, Name: *podClaim.ResourceClaimName})
		if err != nil {
			return nil, fmt.Errorf("get ResourceClaim %q failed: %w", *podClaim.ResourceClaimName, err)
		}
		return claim.Spec.Devices.Requests, nil
	}

	if podClaim.ResourceClaimTemplateName != nil && *podClaim.ResourceClaimTemplateName != "" {
		tpl, err := r.getResourceClaimTemplate(ctx, types.NamespacedName{Namespace: namespace, Name: *podClaim.ResourceClaimTemplateName})
		if err != nil {
			return nil, fmt.Errorf("get ResourceClaimTemplate %q failed: %w", *podClaim.ResourceClaimTemplateName, err)
		}
		return tpl.Spec.Spec.Devices.Requests, nil
	}

	return nil, fmt.Errorf("pod resourceClaim %q must specify one of resourceClaimName or resourceClaimTemplateName", podClaim.Name)
}

func (r *claimRequestReader) Mutation(obj client.Object) {
	if obj == nil {
		return
	}
	switch typed := obj.(type) {
	case *resourceapi.ResourceClaim:
		r.claimMutationCache.Mutation(typed.DeepCopy())
	case *resourceapi.ResourceClaimTemplate:
		r.templateMutationCache.Mutation(typed.DeepCopy())
	}
}

func (r *claimRequestReader) getResourceClaim(ctx context.Context, key types.NamespacedName) (*resourceapi.ResourceClaim, error) {
	if obj, exists, err := r.claimMutationCache.GetByKey(key.String()); err != nil {
		return nil, err
	} else if exists {
		claim, ok := obj.(*resourceapi.ResourceClaim)
		if !ok {
			return nil, fmt.Errorf("unexpected object type %T for ResourceClaim %q", obj, key.String())
		}
		return claim.DeepCopy(), nil
	}

	claim := &resourceapi.ResourceClaim{}
	if r.cachedClient != nil {
		if err := r.cachedClient.Get(ctx, key, claim); err == nil {
			r.Mutation(claim)
			return claim, nil
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	if r.liveClient == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: resourceapi.GroupName, Resource: "resourceclaims"}, key.String())
	}

	if err := r.liveClient.Get(ctx, key, claim); err != nil {
		return nil, err
	}
	r.Mutation(claim)
	return claim, nil
}

func (r *claimRequestReader) getResourceClaimTemplate(ctx context.Context, key types.NamespacedName) (*resourceapi.ResourceClaimTemplate, error) {
	if obj, exists, err := r.templateMutationCache.GetByKey(key.String()); err != nil {
		return nil, err
	} else if exists {
		tpl, ok := obj.(*resourceapi.ResourceClaimTemplate)
		if !ok {
			return nil, fmt.Errorf("unexpected object type %T for ResourceClaimTemplate %q", obj, key.String())
		}
		return tpl.DeepCopy(), nil
	}

	tpl := &resourceapi.ResourceClaimTemplate{}
	if r.cachedClient != nil {
		if err := r.cachedClient.Get(ctx, key, tpl); err == nil {
			r.Mutation(tpl)
			return tpl, nil
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	if r.liveClient == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: resourceapi.GroupName, Resource: "resourceclaimtemplates"}, key.String())
	}

	if err := r.liveClient.Get(ctx, key, tpl); err != nil {
		return nil, err
	}
	r.Mutation(tpl)
	return tpl, nil
}
