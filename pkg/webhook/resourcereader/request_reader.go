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

type ResourceAPIReader interface {
	GetDeviceRequestsForPodClaim(ctx context.Context, namespace string, podClaim *corev1.PodResourceClaim) ([]resourceapi.DeviceRequest, error)
	GetResourceClaim(ctx context.Context, key client.ObjectKey, obj *resourceapi.ResourceClaim) error
	GetResourceClaimTemplate(ctx context.Context, key client.ObjectKey, obj *resourceapi.ResourceClaimTemplate) error
	GetDeviceClass(ctx context.Context, key client.ObjectKey, obj *resourceapi.DeviceClass) error
	GetPod(ctx context.Context, key client.ObjectKey, obj *corev1.Pod) error
	Mutation(obj client.Object)
}

type resourceAPIReader struct {
	liveClient client.Client

	claimMutationCache    kcache.MutationCache
	templateMutationCache kcache.MutationCache
	classMutationCache    kcache.MutationCache
	podMutationCache      kcache.MutationCache
}

func NewResourceAPIReader(
	liveClient client.Client, claimIndexer, templateIndexer,
	classIndexer, podIndexer kcache.Indexer, ttl time.Duration,
) ResourceAPIReader {
	return &resourceAPIReader{
		liveClient: liveClient,
		claimMutationCache: kcache.NewIntegerResourceVersionMutationCache(
			klog.Background(), claimIndexer, claimIndexer, ttl, true,
		),
		templateMutationCache: kcache.NewIntegerResourceVersionMutationCache(
			klog.Background(), templateIndexer, templateIndexer, ttl, true,
		),
		classMutationCache: kcache.NewIntegerResourceVersionMutationCache(
			klog.Background(), classIndexer, classIndexer, ttl, true,
		),
		podMutationCache: kcache.NewIntegerResourceVersionMutationCache(
			klog.Background(), podIndexer, podIndexer, ttl, true,
		),
	}
}

func (r *resourceAPIReader) GetDeviceRequestsForPodClaim(ctx context.Context, namespace string, podClaim *corev1.PodResourceClaim) ([]resourceapi.DeviceRequest, error) {
	if podClaim == nil {
		return nil, fmt.Errorf("podClaim is nil")
	}
	objKey := types.NamespacedName{Namespace: namespace}
	if podClaim.ResourceClaimName != nil && *podClaim.ResourceClaimName != "" {
		objKey.Name = *podClaim.ResourceClaimName
		claim, err := r.getResourceClaim(ctx, objKey, true)
		if err != nil {
			return nil, err
		}
		return claim.Spec.Devices.Requests, nil
	}

	if podClaim.ResourceClaimTemplateName != nil && *podClaim.ResourceClaimTemplateName != "" {
		objKey.Name = *podClaim.ResourceClaimTemplateName
		tpl, err := r.getResourceClaimTemplate(ctx, objKey, true)
		if err != nil {
			return nil, err
		}
		return tpl.Spec.Spec.Devices.Requests, nil
	}

	return nil, fmt.Errorf("pod resourceClaim %q must specify one of resourceClaimName or resourceClaimTemplateName", podClaim.Name)
}

func (r *resourceAPIReader) GetResourceClaim(ctx context.Context, key client.ObjectKey, obj *resourceapi.ResourceClaim) error {
	if obj == nil {
		return fmt.Errorf("obj is nil")
	}
	if claim, err := r.getResourceClaim(ctx, key, false); err != nil {
		return err
	} else {
		claim.DeepCopyInto(obj)
	}
	return nil
}

func (r *resourceAPIReader) GetResourceClaimTemplate(ctx context.Context, key client.ObjectKey, obj *resourceapi.ResourceClaimTemplate) error {
	if obj == nil {
		return fmt.Errorf("obj is nil")
	}
	if claim, err := r.getResourceClaimTemplate(ctx, key, false); err != nil {
		return err
	} else {
		claim.DeepCopyInto(obj)
	}
	return nil
}

func (r *resourceAPIReader) GetDeviceClass(ctx context.Context, key client.ObjectKey, obj *resourceapi.DeviceClass) error {
	if obj == nil {
		return fmt.Errorf("obj is nil")
	}
	if class, err := r.getDeviceClass(ctx, key, false); err != nil {
		return err
	} else {
		class.DeepCopyInto(obj)
	}
	return nil
}

func (r *resourceAPIReader) GetPod(ctx context.Context, key client.ObjectKey, obj *corev1.Pod) error {
	if obj == nil {
		return fmt.Errorf("obj is nil")
	}
	if pod, err := r.getPod(ctx, key, false); err != nil {
		return err
	} else {
		pod.DeepCopyInto(obj)
	}
	return nil
}

func (r *resourceAPIReader) Mutation(obj client.Object) {
	if obj == nil {
		return
	}
	switch typed := obj.(type) {
	case *resourceapi.ResourceClaim:
		r.claimMutationCache.Mutation(typed.DeepCopy())
	case *resourceapi.ResourceClaimTemplate:
		r.templateMutationCache.Mutation(typed.DeepCopy())
	case *resourceapi.DeviceClass:
		r.classMutationCache.Mutation(typed.DeepCopy())
	case *corev1.Pod:
		r.podMutationCache.Mutation(typed.DeepCopy())
	}
}

func (r *resourceAPIReader) getResourceClaim(ctx context.Context, key types.NamespacedName, deepCopy bool) (*resourceapi.ResourceClaim, error) {
	if obj, exists, err := r.claimMutationCache.GetByKey(key.String()); err != nil {
		return nil, err
	} else if exists {
		claim, ok := obj.(*resourceapi.ResourceClaim)
		if !ok {
			return nil, fmt.Errorf("unexpected object type %T for ResourceClaim %q", obj, key.String())
		}
		if !deepCopy {
			return claim, nil
		}
		return claim.DeepCopy(), nil
	}

	if r.liveClient == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: resourceapi.GroupName, Resource: "resourceclaims"}, key.String())
	}

	claim := &resourceapi.ResourceClaim{}
	if err := r.liveClient.Get(ctx, key, claim); err != nil {
		return nil, err
	}

	r.Mutation(claim)
	if !deepCopy {
		return claim, nil
	}
	return claim.DeepCopy(), nil
}

func (r *resourceAPIReader) getResourceClaimTemplate(ctx context.Context, key types.NamespacedName, deepCopy bool) (*resourceapi.ResourceClaimTemplate, error) {
	if obj, exists, err := r.templateMutationCache.GetByKey(key.String()); err != nil {
		return nil, err
	} else if exists {
		tpl, ok := obj.(*resourceapi.ResourceClaimTemplate)
		if !ok {
			return nil, fmt.Errorf("unexpected object type %T for ResourceClaimTemplate %q", obj, key.String())
		}
		if !deepCopy {
			return tpl, nil
		}
		return tpl.DeepCopy(), nil
	}

	if r.liveClient == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: resourceapi.GroupName, Resource: "resourceclaimtemplates"}, key.String())
	}

	tpl := &resourceapi.ResourceClaimTemplate{}
	if err := r.liveClient.Get(ctx, key, tpl, client.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}

	r.Mutation(tpl)
	if !deepCopy {
		return tpl, nil
	}
	return tpl.DeepCopy(), nil
}

func (r *resourceAPIReader) getDeviceClass(ctx context.Context, key types.NamespacedName, deepCopy bool) (*resourceapi.DeviceClass, error) {
	if obj, exists, err := r.classMutationCache.GetByKey(key.String()); err != nil {
		return nil, err
	} else if exists {
		class, ok := obj.(*resourceapi.DeviceClass)
		if !ok {
			return nil, fmt.Errorf("unexpected object type %T for DeviceClass %q", obj, key.String())
		}
		if !deepCopy {
			return class, nil
		}
		return class.DeepCopy(), nil
	}

	if r.liveClient == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: resourceapi.GroupName, Resource: "deviceclasses"}, key.String())
	}

	class := &resourceapi.DeviceClass{}
	if err := r.liveClient.Get(ctx, key, class, client.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}

	r.Mutation(class)
	if !deepCopy {
		return class, nil
	}
	return class.DeepCopy(), nil
}

func (r *resourceAPIReader) getPod(ctx context.Context, key types.NamespacedName, deepCopy bool) (*corev1.Pod, error) {
	if obj, exists, err := r.podMutationCache.GetByKey(key.String()); err != nil {
		return nil, err
	} else if exists {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return nil, fmt.Errorf("unexpected object type %T for DeviceClass %q", obj, key.String())
		}
		if !deepCopy {
			return pod, nil
		}
		return pod.DeepCopy(), nil
	}

	if r.liveClient == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: corev1.GroupName, Resource: "pods"}, key.String())
	}

	pod := &corev1.Pod{}
	if err := r.liveClient.Get(ctx, key, pod, client.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}

	r.Mutation(pod)
	if !deepCopy {
		return pod, nil
	}
	return pod.DeepCopy(), nil
}
