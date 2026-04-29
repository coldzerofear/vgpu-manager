package kubeletplugin

import (
	"context"

	"github.com/coldzerofear/vgpu-manager/pkg/claimresolve"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type kubeClaimResolveReader struct {
	state *DeviceState
}

func (r *kubeClaimResolveReader) GetPod(ctx context.Context, key client.ObjectKey, obj *corev1.Pod) error {
	pod, err := r.state.config.Core.CoreV1().Pods(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	pod.DeepCopyInto(obj)
	return nil
}

func (r *kubeClaimResolveReader) GetResourceClaim(ctx context.Context, key client.ObjectKey, obj *resourceapi.ResourceClaim) error {
	claim, err := r.state.config.Resource.ResourceClaims(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	claim.DeepCopyInto(obj)
	return nil
}

func (s *DeviceState) resolveVGPUClaimPartitions(ctx context.Context, claim *resourceapi.ResourceClaim) (*claimresolve.PartitionInfo, error) {
	allocatedRequests := sets.New[string]()
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != util.DRADriverName {
			continue
		}
		allocatableDevice := s.perGPUAllocatable.GetAllocatableDevice(result.Device)
		if allocatableDevice == nil || allocatableDevice.Type() != VGpuDeviceType {
			continue
		}
		mainRequest := resolveMainRequestName(claim, result.Request)
		if mainRequest != "" {
			allocatedRequests.Insert(mainRequest)
		}
	}
	return claimresolve.ResolveClaimVGPUPartitionsFromAllocatedRequests(ctx, &kubeClaimResolveReader{state: s}, claim, allocatedRequests)
}

func resolveMainRequestName(claim *resourceapi.ResourceClaim, requestName string) string {
	if claim == nil {
		return ""
	}
	for _, req := range claim.Spec.Devices.Requests {
		if req.Exactly != nil && req.Name == requestName {
			return req.Name
		}
		for _, subReq := range req.FirstAvailable {
			if req.Name+"/"+subReq.Name == requestName {
				return req.Name
			}
		}
	}
	return ""
}
