package claimresolve

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Reader interface {
	GetPod(ctx context.Context, key client.ObjectKey, obj *corev1.Pod) error
	GetResourceClaim(ctx context.Context, key client.ObjectKey, obj *resourceapi.ResourceClaim) error
}

type DeviceRequestClassifier func(context.Context, resourceapi.DeviceRequest) bool

type SubRequestClassifier func(context.Context, resourceapi.DeviceSubRequest) bool

type AllocatedResultMeta struct {
	MainRequest string
	IsVGPU      bool
}

func BuildAllocatedResultIndex(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
	isVGPUDeviceRequest DeviceRequestClassifier,
	isVGPUSubRequest SubRequestClassifier,
) map[string]AllocatedResultMeta {
	index := map[string]AllocatedResultMeta{}
	if claim == nil {
		return index
	}

	for _, req := range claim.Spec.Devices.Requests {
		if req.Exactly != nil {
			index[req.Name] = AllocatedResultMeta{
				MainRequest: req.Name,
				IsVGPU:      isVGPUDeviceRequest != nil && isVGPUDeviceRequest(ctx, req),
			}
			continue
		}

		for _, subReq := range req.FirstAvailable {
			index[req.Name+"/"+subReq.Name] = AllocatedResultMeta{
				MainRequest: req.Name,
				IsVGPU:      isVGPUSubRequest != nil && isVGPUSubRequest(ctx, subReq),
			}
		}
	}

	return index
}

func GetAllocatedVGPURequests(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
	driverName string,
	isVGPUDeviceRequest DeviceRequestClassifier,
	isVGPUSubRequest SubRequestClassifier,
) sets.Set[string] {
	result := sets.New[string]()
	if claim == nil || claim.Status.Allocation == nil {
		return result
	}

	index := BuildAllocatedResultIndex(ctx, claim, isVGPUDeviceRequest, isVGPUSubRequest)
	for _, allocResult := range claim.Status.Allocation.Devices.Results {
		if driverName != "" && allocResult.Driver != driverName {
			continue
		}
		meta, ok := index[allocResult.Request]
		if !ok || !meta.IsVGPU {
			continue
		}
		result.Insert(meta.MainRequest)
	}

	return result
}

func ResolveActualAllocatedRequestsForClaimRef(
	claimRef corev1.ResourceClaim,
	allocatedVGPUReqs sets.Set[string],
) []string {
	if allocatedVGPUReqs.Len() == 0 {
		return nil
	}

	if claimRef.Request != "" {
		if allocatedVGPUReqs.Has(claimRef.Request) {
			return []string{claimRef.Request}
		}
		return nil
	}

	result := sets.List(allocatedVGPUReqs)
	return result
}

func ResolveActualClaimNameForPodClaim(pod *corev1.Pod, podClaimName string) (string, bool, error) {
	podClaim, err := FindPodResourceClaim(pod, podClaimName)
	if err != nil {
		return "", false, err
	}

	if podClaim.ResourceClaimName != nil && *podClaim.ResourceClaimName != "" {
		return *podClaim.ResourceClaimName, true, nil
	}

	if podClaim.ResourceClaimTemplateName != nil && *podClaim.ResourceClaimTemplateName != "" {
		status := GetPodResourceClaimStatus(pod, podClaimName)
		if status == nil || status.ResourceClaimName == nil || *status.ResourceClaimName == "" {
			return "", false, nil
		}
		return *status.ResourceClaimName, true, nil
	}

	return "", false, fmt.Errorf(
		"pod %s/%s resourceClaim %q must specify one of resourceClaimName or resourceClaimTemplateName",
		pod.Namespace, pod.Name, podClaimName,
	)
}

func GetPodResourceClaimStatus(pod *corev1.Pod, podClaimName string) *corev1.PodResourceClaimStatus {
	for i := range pod.Status.ResourceClaimStatuses {
		if pod.Status.ResourceClaimStatuses[i].Name == podClaimName {
			return &pod.Status.ResourceClaimStatuses[i]
		}
	}
	return nil
}

func GetReservedPods(ctx context.Context, reader Reader, claim *resourceapi.ResourceClaim) ([]*corev1.Pod, error) {
	if reader == nil || claim == nil {
		return nil, nil
	}

	var result []*corev1.Pod
	for _, ref := range claim.Status.ReservedFor {
		if ref.APIGroup != "" || ref.Resource != "pods" {
			continue
		}

		var pod corev1.Pod
		err := reader.GetPod(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: ref.Name}, &pod)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}

		if ref.UID != "" && pod.UID != ref.UID {
			continue
		}

		result = append(result, &pod)
	}

	return result, nil
}

func FindPodResourceClaim(pod *corev1.Pod, podClaimName string) (*corev1.PodResourceClaim, error) {
	if pod == nil {
		return nil, fmt.Errorf("pod is nil")
	}
	for i := range pod.Spec.ResourceClaims {
		if pod.Spec.ResourceClaims[i].Name == podClaimName {
			return &pod.Spec.ResourceClaims[i], nil
		}
	}
	return nil, fmt.Errorf("pod resourceClaim %q not found", podClaimName)
}
