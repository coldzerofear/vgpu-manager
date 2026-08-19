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

package common

import (
	"context"
	"fmt"
	"strings"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
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

// ConvertDRARequest Convert pod's extended resource requests into DRA requests
func ConvertDRARequest(ctx context.Context, metadata *metav1.ObjectMeta, podSpec *corev1.PodSpec, resourceName string, options *options.Options) error {
	logger := log.FromContext(ctx)

	resourceInfos := make(ResourceInfos, 0)
	convertContainerRequest := func(podSpec *corev1.PodSpec, container *corev1.Container) {
		if !util.IsVGPURequiredContainer(container) {
			return
		}

		var resourceClaimName, resourceRequestName string
		// Convert container resource requests into DRA requests.
		if options.CombinedResourceClaim {
			resourceClaimName = util.GenerateK8sSafeResourceName(resourceName)
			resourceRequestName = util.GenerateK8sSafeResourceName(container.Name, kubeletplugin.VGpuDeviceType)
			resourceClaim := corev1.ResourceClaim{Name: resourceClaimName, Request: resourceRequestName}
			container.Resources.Claims = append(container.Resources.Claims, resourceClaim)
		} else {
			resourceClaimName = util.GenerateK8sSafeResourceName(resourceName, container.Name)
			resourceRequestName = kubeletplugin.VGpuDeviceType
			resourceClaim := corev1.ResourceClaim{Name: resourceClaimName, Request: resourceRequestName}
			container.Resources.Claims = append(container.Resources.Claims, resourceClaim)
		}

		deviceCount := util.GetResourceOfContainer(container, util.VGPUNumberResourceName)
		deviceCores := util.GetResourceOfContainer(container, util.VGPUCoreResourceName)
		deviceMemory := util.GetResourceOfContainer(container, util.VGPUMemoryResourceName)

		resourceInfo := ResourceInfo{
			Name:        container.Name,
			ClaimName:   resourceClaimName,
			RequestName: resourceRequestName,
			Resources: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceName(util.VGPUNumberResourceName): *resource.NewQuantity(deviceCount, resource.DecimalSI),
				corev1.ResourceName(util.VGPUCoreResourceName):   *resource.NewQuantity(deviceCores, resource.DecimalSI),
				corev1.ResourceName(util.VGPUMemoryResourceName): *resource.NewQuantity(deviceMemory, resource.DecimalSI),
			},
		}

		util.DelResourceOfContainer(container, util.VGPUNumberResourceName)
		util.DelResourceOfContainer(container, util.VGPUCoreResourceName)
		util.DelResourceOfContainer(container, util.VGPUMemoryResourceName)
		resourceInfos = append(resourceInfos, resourceInfo)

		// Due to compressing all container resource requests into one resource claim, only the first resource claim is inserted.
		if !(options.CombinedResourceClaim && len(resourceInfos) == 1) {
			podSpec.ResourceClaims = append(podSpec.ResourceClaims, corev1.PodResourceClaim{
				Name:              resourceClaimName,
				ResourceClaimName: &resourceClaimName,
			})
		}
		logger.V(2).Info("Successfully convert vGPU requests to resourceClaims", "container",
			container.Name, "vGPUNumber", deviceCount, "vGPUCores", deviceCores, "vGPUMemory", deviceMemory)
	}
	for i := range podSpec.InitContainers {
		convertContainerRequest(podSpec, &podSpec.InitContainers[i])
	}
	for i := range podSpec.Containers {
		convertContainerRequest(podSpec, &podSpec.Containers[i])
	}

	if len(resourceInfos) > 0 {
		encode, err := resourceInfos.Encode()
		if err != nil {
			logger.Error(err, "Encoding original resource information failed")
			return apierrors.NewBadRequest(fmt.Sprintf("Encoding original resource information failed: %v", err))
		}
		util.InsertAnnotation(metadata, util.DRAOriResAnnotation, encode)
		logger.Info("Successfully convert all vGPU requests to resourceClaims")
	}
	return nil
}
