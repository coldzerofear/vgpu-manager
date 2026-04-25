package common

import (
	"context"
	"fmt"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
)

func FindPodResourceClaim(pod *corev1.Pod, podClaimName string) (*corev1.PodResourceClaim, error) {
	for i := range pod.Spec.ResourceClaims {
		if pod.Spec.ResourceClaims[i].Name == podClaimName {
			return &pod.Spec.ResourceClaims[i], nil
		}
	}
	return nil, fmt.Errorf("pod resourceClaim %q not found", podClaimName)
}

func SubRequestLooksLikeVGPU(ctx context.Context, reader resourcereader.ResourceAPIReader, req resourceapi.DeviceSubRequest, matchedDriver bool, vgpuClassName string) bool {
	// VGPUDeviceClassName hit represents the vgpu type
	if req.DeviceClassName == vgpuClassName {
		return true
	}

	matchDriver := matchedDriver
	matchDevice := false
	if reader != nil {
		dc := resourceapi.DeviceClass{}
		_ = reader.GetDeviceClass(ctx, types.NamespacedName{Name: req.DeviceClassName}, &dc)
		for _, selector := range dc.Spec.Selectors {
			if selector.CEL == nil {
				continue
			}
			expr := selector.CEL.Expression
			if !matchDriver {
				matchDriver = strings.Contains(expr, util.DRADriverName)
			}
			if !matchDevice {
				matchDevice = strings.Contains(expr, kubeletplugin.VGpuDeviceType)
			}
			if matchDriver && matchDevice {
				return true
			}
		}
	}

	for _, selector := range req.Selectors {
		if selector.CEL == nil {
			continue
		}
		expr := selector.CEL.Expression
		if !matchDriver {
			matchDriver = strings.Contains(expr, util.DRADriverName)
		}
		if !matchDevice {
			matchDevice = strings.Contains(expr, kubeletplugin.VGpuDeviceType)
		}
		if matchDriver && matchDevice {
			return true
		}
	}
	return matchDriver && matchDevice
}

func ExactLooksLikeVGPU(ctx context.Context, reader resourcereader.ResourceAPIReader, req *resourceapi.ExactDeviceRequest, matchedDriver bool, vgpuClassName string) bool {
	if req == nil {
		return false
	}
	// VGPUDeviceClassName hit represents the vgpu type
	if req.DeviceClassName == vgpuClassName {
		return true
	}

	matchDriver := matchedDriver
	matchDevice := false

	if reader != nil {
		dc := resourceapi.DeviceClass{}
		_ = reader.GetDeviceClass(ctx, types.NamespacedName{Name: req.DeviceClassName}, &dc)
		for _, selector := range dc.Spec.Selectors {
			if selector.CEL == nil {
				continue
			}
			expr := selector.CEL.Expression
			if !matchDriver {
				matchDriver = strings.Contains(expr, util.DRADriverName)
			}
			if !matchDevice {
				matchDevice = strings.Contains(expr, kubeletplugin.VGpuDeviceType)
			}
			if matchDriver && matchDevice {
				return true
			}
		}
	}

	for _, selector := range req.Selectors {
		if selector.CEL == nil {
			continue
		}
		expr := selector.CEL.Expression
		if !matchDriver {
			matchDriver = strings.Contains(expr, util.DRADriverName)
		}
		if !matchDevice {
			matchDevice = strings.Contains(expr, kubeletplugin.VGpuDeviceType)
		}
		if matchDriver && matchDevice {
			return true
		}
	}
	return matchDriver && matchDevice
}

func DeviceRequestLooksLikeVGPU(ctx context.Context, reader resourcereader.ResourceAPIReader, req resourceapi.DeviceRequest, matchedDriver bool, vgpuClassName string) bool {
	switch {
	case req.Exactly != nil:
		return ExactLooksLikeVGPU(ctx, reader, req.Exactly, matchedDriver, vgpuClassName)
	case len(req.FirstAvailable) > 0:
		// This is just a 'whether to include vgpu candidates', not a definitive judgment in the Pod stage
		for _, sub := range req.FirstAvailable {
			if SubRequestLooksLikeVGPU(ctx, reader, sub, matchedDriver, vgpuClassName) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func GetAllPodContainers(pod *corev1.Pod) []ContainerRef {
	all := make([]ContainerRef, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for _, c := range pod.Spec.InitContainers {
		all = append(all, ContainerRef{
			Name:   c.Name,
			Claims: c.Resources.Claims,
			Kind:   ContainerKindInit,
		})
	}
	for _, c := range pod.Spec.Containers {
		all = append(all, ContainerRef{
			Name:   c.Name,
			Claims: c.Resources.Claims,
			Kind:   ContainerKindApp,
		})
	}
	return all
}
