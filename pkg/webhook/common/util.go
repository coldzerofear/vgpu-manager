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
	"crypto/rand"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device/allocator"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	"github.com/docker/go-units"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	vcv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
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

func GenerateRandomString(length int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	const letterLen = len(letters)

	maxValid := byte(255 - (255 % letterLen))
	b := make([]byte, length)
	buffer := make([]byte, length*4)
	idx := 0
	for idx < length {
		n, err := rand.Read(buffer)
		if err != nil {
			panic("crypto/rand: " + err.Error())
		}
		for i := 0; i < n && idx < length; i++ {
			v := buffer[i]
			if v < maxValid {
				b[idx] = letters[int(v)%letterLen]
				idx++
			}
		}
	}
	return string(b)
}

func ConvertDRAContainerRequest(ctx context.Context, resourceName string, container *corev1.Container, options *options.Options) *ResourceInfo {
	if !util.IsVGPURequiredContainer(container) {
		return nil
	}
	logger := log.FromContext(ctx)

	var resourceClaimName, resourceRequestName string
	// Convert container resource requests into DRA requests.
	if options.CombinedResourceClaim {
		resourceClaimName = util.GenerateK8sSafeResourceName(resourceName)
		resourceRequestName = util.GenerateK8sSafeResourceName(container.Name, kubeletplugin.VGpuDeviceType)
	} else {
		resourceClaimName = util.GenerateK8sSafeResourceName(resourceName, container.Name)
		resourceRequestName = kubeletplugin.VGpuDeviceType
	}

	resourceClaim := corev1.ResourceClaim{Name: resourceClaimName, Request: resourceRequestName}
	container.Resources.Claims = append(container.Resources.Claims, resourceClaim)

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

	logger.V(2).Info("Successfully convert vGPU requests to resourceInfos", "container",
		container.Name, "vGPUNumber", deviceCount, "vGPUCores", deviceCores, "vGPUMemory", deviceMemory)

	return &resourceInfo
}

func BuildDeviceRequest(pod *corev1.Pod, deviceClassName string, info ResourceInfo) resourceapi.DeviceRequest {
	var (
		deviceCount     int64
		capacityRequest = make(map[resourceapi.QualifiedName]resource.Quantity)
	)
	if quantity, ok := info.Resources[corev1.ResourceName(util.VGPUNumberResourceName)]; ok {
		deviceCount = quantity.Value()
	}
	if quantity, ok := info.Resources[corev1.ResourceName(util.VGPUCoreResourceName)]; ok && quantity.Value() > 0 {
		capacityRequest[kubeletplugin.CoresResourceName] = *resource.NewQuantity(quantity.Value(), resource.DecimalSI)
	}
	if quantity, ok := info.Resources[corev1.ResourceName(util.VGPUMemoryResourceName)]; ok && quantity.Value() > 0 {
		capacityRequest[kubeletplugin.MemoryResourceName] = *resource.NewQuantity(quantity.Value()*units.MiB, resource.BinarySI)
	}

	// Access mode (remote GPU): the request pins accessMode so that pods
	// without the annotation keep getting local-only devices even when a
	// node publishes accessMode=remote. An invalid value falls back to
	// local here; the validating webhook rejects it before this runs.
	accessMode, _ := util.PodVGPUAccessMode(pod)

	deviceSelectors := []resourceapi.DeviceSelector{{
		CEL: &resourceapi.CELDeviceSelector{
			Expression: fmt.Sprintf(`device.attributes["%s"].type == "%s" && device.attributes["%s"].%s == "%s"`,
				util.DRADriverName, kubeletplugin.VGpuDeviceType, util.DRADriverName, util.AccessModeAttribute, accessMode),
		},
	}}
	if uuids, _ := util.HasAnnotation(pod, util.PodIncludeGPUUUIDAnnotation); len(uuids) > 0 {
		split := strings.Split(strings.ToLower(uuids), ",")
		includeUuids := make([]string, 0, len(split))
		for _, uuid := range split {
			if uuid = strings.TrimSpace(uuid); uuid != "" {
				includeUuids = append(includeUuids, strconv.Quote(uuid))
			}
		}
		if len(includeUuids) > 0 {
			deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
				CEL: &resourceapi.CELDeviceSelector{
					Expression: fmt.Sprintf(`device.attributes[%q].uuid in [%s]`,
						util.DRADriverName, strings.Join(includeUuids, ",")),
				},
			})
		}
	}
	if uuids, _ := util.HasAnnotation(pod, util.PodExcludeGPUUUIDAnnotation); len(uuids) > 0 {
		split := strings.Split(strings.ToLower(uuids), ",")
		excludeUuids := make([]string, 0, len(split))
		for _, uuid := range split {
			if uuid = strings.TrimSpace(uuid); uuid != "" {
				excludeUuids = append(excludeUuids, strconv.Quote(uuid))
			}
		}
		if len(excludeUuids) > 0 {
			deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
				CEL: &resourceapi.CELDeviceSelector{
					Expression: fmt.Sprintf(`!(device.attributes[%q].uuid in [%s])`,
						util.DRADriverName, strings.Join(excludeUuids, ",")),
				},
			})
		}
	}
	if types, _ := util.HasAnnotation(pod, util.PodIncludeGpuTypeAnnotation); len(types) > 0 {
		split := strings.Split(strings.ToUpper(types), ",")
		includeTypes := make([]string, 0, len(split))
		for _, name := range split {
			if name = strings.TrimSpace(name); name != "" {
				includeTypes = append(includeTypes, strconv.Quote(name))
			}
		}
		if len(includeTypes) > 0 {
			deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
				CEL: &resourceapi.CELDeviceSelector{
					Expression: fmt.Sprintf(`device.attributes[%q].productName in [%s]`,
						util.DRADriverName, strings.Join(includeTypes, ",")),
				},
			})
		}
	}
	if types, _ := util.HasAnnotation(pod, util.PodExcludeGpuTypeAnnotation); len(types) > 0 {
		split := strings.Split(strings.ToUpper(types), ",")
		excludeTypes := make([]string, 0, len(split))
		for _, name := range split {
			if name = strings.TrimSpace(name); name != "" {
				excludeTypes = append(excludeTypes, strconv.Quote(name))
			}
		}
		if len(excludeTypes) > 0 {
			deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
				CEL: &resourceapi.CELDeviceSelector{
					Expression: fmt.Sprintf(`!(device.attributes[%q].productName in [%s])`,
						util.DRADriverName, strings.Join(excludeTypes, ",")),
				},
			})
		}
	}
	policy, _ := util.HasAnnotation(pod, util.MemorySchedulerPolicyAnnotation)
	policy = strings.ToLower(strings.TrimSpace(policy))
	switch {
	case policy == util.VirtualMemoryPolicy.String(), strings.HasPrefix(policy, "virt"):
		deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
			CEL: &resourceapi.CELDeviceSelector{
				Expression: fmt.Sprintf(`device.attributes[%q].memoryRatio > 100`, util.DRADriverName),
			},
		})
	case policy == util.PhysicalMemoryPolicy.String(), strings.HasPrefix(policy, "phy"):
		deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
			CEL: &resourceapi.CELDeviceSelector{
				Expression: fmt.Sprintf(`device.attributes[%q].memoryRatio <= 100`, util.DRADriverName),
			},
		})
	}

	return resourceapi.DeviceRequest{
		Name: info.RequestName,
		Exactly: &resourceapi.ExactDeviceRequest{
			DeviceClassName: deviceClassName,
			AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
			Count:           deviceCount,
			Capacity: &resourceapi.CapacityRequirements{
				Requests: capacityRequest,
			},
			Selectors: deviceSelectors,
		},
	}
}

func BuildTaskDeviceRequest(task *vcv1alpha1.TaskSpec, deviceClassName string, info ResourceInfo) resourceapi.DeviceRequest {
	pod := &corev1.Pod{
		ObjectMeta: task.Template.ObjectMeta,
		Spec:       task.Template.Spec,
	}
	return BuildDeviceRequest(pod, deviceClassName, info)
}

func BuildTaskResourceClaimTemplate(task *vcv1alpha1.TaskSpec, requests []resourceapi.DeviceRequest, resourceClaimName, ownerKey, timestamp string) *resourceapi.ResourceClaimTemplate {
	pod := &corev1.Pod{
		ObjectMeta: task.Template.ObjectMeta,
		Spec:       task.Template.Spec,
	}
	resourceClaim := BuildResourceClaim(pod, requests, resourceClaimName, ownerKey, timestamp)
	return &resourceapi.ResourceClaimTemplate{
		ObjectMeta: resourceClaim.ObjectMeta,
		Spec: resourceapi.ResourceClaimTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      resourceClaim.ObjectMeta.GetLabels(),
				Annotations: resourceClaim.ObjectMeta.GetAnnotations(),
			},
			Spec: resourceClaim.Spec,
		},
	}
}

// BuildResourceClaim Build vGPU resource claims based on container requests.
func BuildResourceClaim(pod *corev1.Pod, requests []resourceapi.DeviceRequest, resourceClaimName, ownerKey, timestamp string) *resourceapi.ResourceClaim {
	var deviceConstraints []resourceapi.DeviceConstraint
	topologyMode, strict := allocator.ParsePodTopologyMode(pod)
	tiers := topologyTiers(topologyMode.BaseTopology())
	accessMode, _ := util.PodVGPUAccessMode(pod)
	for i := range requests {
		request := &requests[i]
		if request.Exactly == nil || !isMultiDeviceRequest(request.Exactly) {
			continue
		}
		// The uuids of multiple devices in a single request are mutually exclusive, ensuring that each physical device is only assigned once.
		deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
			Requests:          []string{request.Name},
			DistinctAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/uuid"),
		})
		if len(tiers) == 0 {
			// No topology asked for: the devices may come from anywhere, even
			// from several remote GPU servers for one container.
			continue
		}
		if accessMode == util.AccessModeRemote {
			// TODO Cross node topology allocation is temporarily not supported under remote access.
			// Pinned on the main request, so it holds for every subrequest
			// below, the unconstrained last one included. numaNode in
			// particular compares equal across nodes and would not keep the
			// devices together on its own.
			deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
				Requests:       []string{request.Name},
				MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/" + remote.AttrNodeName),
			})
		}
		deviceConstraints = append(deviceConstraints, applyTopologyTiers(request, tiers, strict)...)
	}

	var annotations map[string]string
	if val, ok := util.HasAnnotation(pod, util.VGPUComputePolicyAnnotation); ok {
		annotations = map[string]string{util.VGPUComputePolicyAnnotation: val}
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				util.DRAOwnerKeyLabel:   ownerKey,
				util.DRACreateTimeLabel: timestamp,
			},
			Annotations: annotations,
			Name:        resourceClaimName,
			Namespace:   pod.Namespace,
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Constraints: deviceConstraints,
				Requests:    requests,
			},
		},
	}
}

func requestConvertSubRequest(request resourceapi.ExactDeviceRequest, subName string) resourceapi.DeviceSubRequest {
	return resourceapi.DeviceSubRequest{
		Name:              subName,
		DeviceClassName:   request.DeviceClassName,
		Selectors:         request.Selectors,
		AllocationMode:    request.AllocationMode,
		Count:             request.Count,
		Tolerations:       request.Tolerations,
		Capacity:          request.Capacity,
		DerivedAttributes: request.DerivedAttributes,
	}
}

// topologyTier is one rung of a topology mode's fallback ladder: the
// subrequest it becomes and the attribute its devices must share.
type topologyTier struct {
	subRequest string
	attribute  resourceapi.FullyQualifiedName
}

// anyTierSubRequest names the last, unconstrained rung every non-strict
// ladder ends in, so the request can still be placed when no topology holds.
const anyTierSubRequest = "any"

// topologyTiers is a topology mode's ladder, tightest first. It mirrors the
// scheduler-extender's tier walk -- NVLink, then PCIe switch, then one NUMA
// node -- so a mode means the same thing, and degrades the same way, on both
// paths. nil for modes with no topology.
//
// The subrequest names are what the allocation result reports
// ("<request>/pcie"), so they say which tier the request actually got.
func topologyTiers(mode util.TopologyMode) []topologyTier {
	nvlink := topologyTier{"nvlink", resourceapi.FullyQualifiedName(util.DRADriverName + "/" + util.NVLinkDomainDeviceAttribute)}
	pcie := topologyTier{"pcie", resourceapi.FullyQualifiedName(util.DRADriverName + "/" + util.PCIeDomainDeviceAttribute)}
	numa := topologyTier{"numa", resourceapi.FullyQualifiedName(deviceattribute.StandardDeviceAttributeNUMANode)}
	switch mode {
	case util.LinkTopology:
		return []topologyTier{nvlink, pcie, numa}
	case util.PCIeTopology:
		return []topologyTier{pcie, numa}
	case util.NUMATopology:
		return []topologyTier{numa}
	default:
		return nil
	}
}

// applyTopologyTiers shapes request for its topology ladder and returns the
// constraints that go with it.
//
// strict keeps the request as it is and pins it to the first tier: anything
// looser must refuse the node. Otherwise the request becomes a firstAvailable
// list, one subrequest per tier plus the unconstrained "any", each constrained
// to its own tier's attribute. The allocator takes the first subrequest a
// node can satisfy, and the scheduler scores nodes higher the earlier that
// subrequest is, so the tightest available tier wins without ever making the
// pod unschedulable.
//
// nvlinkDomain and pcieDomain are published only with the kubelet plugin's
// TopologyDeviceAttributes gate; a node without it simply never satisfies
// those tiers and falls through to the next.
func applyTopologyTiers(request *resourceapi.DeviceRequest, tiers []topologyTier, strict bool) []resourceapi.DeviceConstraint {
	// A subrequest cannot carry adminAccess, so converting would silently drop it.
	if strict || ptr.Deref(request.Exactly.AdminAccess, false) {
		return []resourceapi.DeviceConstraint{{
			Requests:       []string{request.Name},
			MatchAttribute: ptr.To(tiers[0].attribute),
		}}
	}
	constraints := make([]resourceapi.DeviceConstraint, 0, len(tiers))
	subRequests := make([]resourceapi.DeviceSubRequest, 0, len(tiers)+1)
	for _, tier := range tiers {
		subRequests = append(subRequests, requestConvertSubRequest(*request.Exactly, tier.subRequest))
		constraints = append(constraints, resourceapi.DeviceConstraint{
			Requests:       []string{request.Name + "/" + tier.subRequest},
			MatchAttribute: ptr.To(tier.attribute),
		})
	}
	request.FirstAvailable = append(subRequests, requestConvertSubRequest(*request.Exactly, anyTierSubRequest))
	request.Exactly = nil
	return constraints
}

// CheckDeviceConstraintLimit refuses a claim spec with more device constraints
// than the API server accepts (resourceapi.DeviceConstraintsMaxSize).
//
// Only a combined claim can get there: every multi-device request adds a
// distinct-uuid constraint, one per topology tier, and a node pin for remote
// access, so a non-strict `link` request takes 4 (5 remote) and enough
// multi-GPU containers in one claim run out. The limit is reported as it is
// rather than worked around by quietly dropping tiers, which would make what a
// topology mode guarantees depend on how many containers share the claim; the
// message says which knobs move the boundary.
func CheckDeviceConstraintLimit(spec *resourceapi.ResourceClaimSpec, fldPath *field.Path) field.ErrorList {
	n := len(spec.Devices.Constraints)
	if n <= resourceapi.DeviceConstraintsMaxSize {
		return nil
	}
	return field.ErrorList{field.Invalid(fldPath, n, fmt.Sprintf(
		"the combined ResourceClaim would need %d device constraints, more than the %d Kubernetes allows: each "+
			"multi-GPU container takes one per topology tier plus one for uuid exclusivity (and one more for "+
			"remote access); use fewer multi-GPU containers, a \"-strict\" topology mode (one tier), or turn off "+
			"--combined-resource-claim so each container gets its own claim",
		n, resourceapi.DeviceConstraintsMaxSize))}
}

// isMultiDeviceRequest reports whether a request can hand out more than one
// device, which is when constraints between its devices mean anything.
func isMultiDeviceRequest(exact *resourceapi.ExactDeviceRequest) bool {
	switch exact.AllocationMode {
	case "", resourceapi.DeviceAllocationModeExactCount:
		return exact.Count > 1
	case resourceapi.DeviceAllocationModeAll:
		return true
	default:
		return false
	}
}

func validateContainerResources(resourceInfo ResourceInfo, containerPath *field.Path) field.ErrorList {
	var errs field.ErrorList

	quantity, ok := resourceInfo.Resources[corev1.ResourceName(util.VGPUCoreResourceName)]
	if ok && quantity.Value() > util.HundredCore {
		errs = append(errs, field.Invalid(
			containerPath.Child("resources").Child("limits").Key(util.VGPUCoreResourceName),
			quantity.Value(), fmt.Sprintf("request exceeds limit, maximum: %v", util.HundredCore)))
	}

	quantity, ok = resourceInfo.Resources[corev1.ResourceName(util.VGPUNumberResourceName)]
	if ok && quantity.Value() > vgpu.MaxDeviceCount {
		errs = append(errs, field.Invalid(
			containerPath.Child("resources").Child("limits").Key(util.VGPUNumberResourceName),
			quantity.Value(), fmt.Sprintf("request exceeds limit, maximum: %v", vgpu.MaxDeviceCount)))
	}
	return errs
}

func CheckTaskResourceInfo(taskPath *field.Path, task *vcv1alpha1.TaskSpec, infoIndex int, resourceInfo ResourceInfo) (*corev1.Container, field.ErrorList) {
	if initContainerIndex := slices.IndexFunc(task.Template.Spec.InitContainers, func(c corev1.Container) bool {
		return c.Name == resourceInfo.Name
	}); initContainerIndex >= 0 {
		container := &task.Template.Spec.InitContainers[initContainerIndex]
		basePath := taskPath.Child("template").Child("spec").Child("initContainers").Index(initContainerIndex)
		if errs := validateContainerResources(resourceInfo, basePath); len(errs) > 0 {
			return nil, errs
		}
		return container, nil
	}

	if containerIndex := slices.IndexFunc(task.Template.Spec.Containers, func(c corev1.Container) bool {
		return c.Name == resourceInfo.Name
	}); containerIndex >= 0 {
		container := &task.Template.Spec.Containers[containerIndex]
		basePath := taskPath.Child("template").Child("spec").Child("containers").Index(containerIndex)
		if errs := validateContainerResources(resourceInfo, basePath); len(errs) > 0 {
			return nil, errs
		}
		return container, nil
	}

	return nil, field.ErrorList{field.Invalid(taskPath.Child("template").Child("metadata").
		Child("annotations").Child(util.DRAOriResAnnotation).Index(infoIndex).Child("name"),
		resourceInfo.Name, "container not found"),
	}
}

func CheckResourceInfo(pod *corev1.Pod, infoIndex int, resourceInfo ResourceInfo) (*corev1.Container, error) {
	if initContainerIndex := slices.IndexFunc(pod.Spec.InitContainers, func(c corev1.Container) bool {
		return c.Name == resourceInfo.Name
	}); initContainerIndex >= 0 {
		container := &pod.Spec.InitContainers[initContainerIndex]
		basePath := field.NewPath("spec").Child("initContainers").Index(initContainerIndex)
		if errs := validateContainerResources(resourceInfo, basePath); len(errs) > 0 {
			return nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, errs)
		}
		return container, nil
	}

	if containerIndex := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool {
		return c.Name == resourceInfo.Name
	}); containerIndex >= 0 {
		container := &pod.Spec.Containers[containerIndex]
		basePath := field.NewPath("spec").Child("containers").Index(containerIndex)
		if errs := validateContainerResources(resourceInfo, basePath); len(errs) > 0 {
			return nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, errs)
		}
		return container, nil
	}
	return nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
		field.Invalid(field.NewPath("metadata").Child("annotations").Child(util.DRAOriResAnnotation).
			Index(infoIndex).Child("name"), resourceInfo.Name, "container not found")})
}
