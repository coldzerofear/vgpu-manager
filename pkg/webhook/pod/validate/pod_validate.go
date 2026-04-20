package validate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/filter"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/pod/common"
	"github.com/docker/go-units"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const Path = "/pods/validate"

func NewValidateWebhook(client client.Client, options *options.Options) (*admission.Webhook, error) {
	return &admission.Webhook{
		Handler: &validateHandle{
			decoder: admission.NewDecoder(client.Scheme()),
			options: options,
			client:  client,
		},
		RecoverPanic: ptr.To[bool](true),
	}, nil
}

type validateHandle struct {
	decoder admission.Decoder
	options *options.Options
	client  client.Client
}

func (h *validateHandle) ValidateCreate(ctx context.Context, pod *corev1.Pod) error {
	if h.options.DefaultConvertToDRA {
		keySet := sets.New[string]()
		var repeat []corev1.ResourceClaim
		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			for _, claim := range container.Resources.Claims {
				if keySet.Has(claim.Name + claim.Request) {
					repeat = append(repeat, claim)
				} else {
					keySet.Insert(claim.Name + claim.Request)
				}
			}
		}
		for _, claim := range repeat {
			index := slices.IndexFunc(pod.Spec.ResourceClaims, func(item corev1.PodResourceClaim) bool {
				return item.Name == claim.Name
			})
			if index < 0 {
				continue
			}
			resourceClaim := &pod.Spec.ResourceClaims[index]
			if resourceClaim.ResourceClaimName != nil && *resourceClaim.ResourceClaimName != "" {
				// TODO check vgpu
				continue
			}
			if resourceClaim.ResourceClaimTemplateName != nil && *resourceClaim.ResourceClaimTemplateName != "" {
				// TODO check vgpu
				continue
			}
		}

		return h.createResourceClaims(ctx, pod)
	}
	return nil
}

// buildResourceClaim Build vGPU resource claims based on container requests.
func (h *validateHandle) buildResourceClaim(pod *corev1.Pod, requests []resourceapi.DeviceRequest, resourceClaimName, ownerPod, timestamp string) *resourceapi.ResourceClaim {
	var deviceConstraints []resourceapi.DeviceConstraint

	for _, request := range requests {
		// Device uuids are mutually exclusive, ensuring that each physical device is only assigned once.
		if request.Exactly.Count > 1 {
			deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
				Requests:          []string{request.Name},
				DistinctAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/uuid"),
			})
		}

		switch filter.PodUsedGPUTopologyMode(pod) {
		case util.LinkTopology:
			deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
				Requests:       []string{request.Name},
				MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](resourceapi.FullyQualifiedName(deviceattribute.StandardDeviceAttributePCIeRoot)),
			})
		case util.NUMATopology:
			deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
				Requests:       []string{request.Name},
				MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/numaNode"),
			})
		}

	}

	var annotations map[string]string
	if val, ok := util.HasAnnotation(pod, util.VGPUComputePolicyAnnotation); ok {
		annotations = map[string]string{util.VGPUComputePolicyAnnotation: val}
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				util.DRAOwnerPodLabel:   ownerPod,
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

func buildDeviceRequest(pod *corev1.Pod, requestName, deviceClassName string, info common.ResourceInfo) resourceapi.DeviceRequest {
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

	deviceSelectors := []resourceapi.DeviceSelector{{
		CEL: &resourceapi.CELDeviceSelector{
			Expression: fmt.Sprintf(`device.attributes["%s"].type == "%s"`,
				util.DRADriverName, kubeletplugin.VGpuDeviceType),
		},
	}}
	if uuids, _ := util.HasAnnotation(pod, util.PodIncludeGPUUUIDAnnotation); len(uuids) > 0 {
		split := strings.Split(strings.ToLower(uuids), ",")
		includeUuids := make([]string, 0, len(split))
		for _, uuid := range split {
			if uuid = strings.TrimSpace(uuid); uuid != "" {
				includeUuids = append(includeUuids, uuid)
			}
		}
		if len(includeUuids) > 0 {
			deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
				CEL: &resourceapi.CELDeviceSelector{
					Expression: fmt.Sprintf(`device.attributes["%s"].uuid in ["%s"]`,
						util.DRADriverName, strings.Join(includeUuids, `","`)),
				},
			})
		}
	}
	if uuids, _ := util.HasAnnotation(pod, util.PodExcludeGPUUUIDAnnotation); len(uuids) > 0 {
		split := strings.Split(strings.ToLower(uuids), ",")
		excludeUuids := make([]string, 0, len(split))
		for _, uuid := range split {
			if uuid = strings.TrimSpace(uuid); uuid != "" {
				excludeUuids = append(excludeUuids, uuid)
			}
		}
		if len(excludeUuids) > 0 {
			deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
				CEL: &resourceapi.CELDeviceSelector{
					Expression: fmt.Sprintf(`device.attributes["%s"].uuid not in ["%s"]`,
						util.DRADriverName, strings.Join(excludeUuids, `","`)),
				},
			})
		}
	}
	if types, _ := util.HasAnnotation(pod, util.PodIncludeGpuTypeAnnotation); len(types) > 0 {
		split := strings.Split(strings.ToUpper(types), ",")
		includeTypes := make([]string, 0, len(split))
		for _, name := range split {
			if name = strings.TrimSpace(name); name != "" {
				includeTypes = append(includeTypes, name)
			}
		}
		if len(includeTypes) > 0 {
			deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
				CEL: &resourceapi.CELDeviceSelector{
					Expression: fmt.Sprintf(`device.attributes["%s"].productName in ["%s"]`,
						util.DRADriverName, strings.Join(includeTypes, `","`)),
				},
			})
		}
	}
	if types, _ := util.HasAnnotation(pod, util.PodExcludeGpuTypeAnnotation); len(types) > 0 {
		split := strings.Split(strings.ToUpper(types), ",")
		excludeTypes := make([]string, 0, len(split))
		for _, name := range split {
			if name = strings.TrimSpace(name); name != "" {
				excludeTypes = append(excludeTypes, name)
			}
		}
		if len(excludeTypes) > 0 {
			deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
				CEL: &resourceapi.CELDeviceSelector{
					Expression: fmt.Sprintf(`device.attributes["%s"].productName not in ["%s"]`,
						util.DRADriverName, strings.Join(excludeTypes, `","`)),
				},
			})
		}
	}
	policy, _ := util.HasAnnotation(pod, util.MemorySchedulerPolicyAnnotation)
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == util.VirtualMemoryPolicy.String() || strings.HasPrefix(policy, "virt") {
		deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
			CEL: &resourceapi.CELDeviceSelector{
				Expression: fmt.Sprintf(`device.attributes["%s"].memoryRatio > 100`, util.DRADriverName),
			},
		})
	} else if policy == util.PhysicalMemoryPolicy.String() || strings.HasPrefix(policy, "phy") {
		deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
			CEL: &resourceapi.CELDeviceSelector{
				Expression: fmt.Sprintf(`device.attributes["%s"].memoryRatio <= 100`, util.DRADriverName),
			},
		})
	}

	return resourceapi.DeviceRequest{
		Name: requestName,
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

func CheckResourceName(pod *corev1.Pod, resourceName string) error {
	if resourceName == "" {
		return apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
			field.Invalid(field.NewPath("metadata").Child("annotations").Child(util.DRAGenNameAnnotation),
				resourceName, "generate name cannot be empty")})
	}
	return nil
}

func (h *validateHandle) createCombinedResourceClaim(ctx context.Context, pod *corev1.Pod, resourceInfos common.ResourceInfos) error {
	logger := log.FromContext(ctx)

	resourceName, _ := util.HasAnnotation(pod, util.DRAGenNameAnnotation)
	if err := CheckResourceName(pod, resourceName); err != nil {
		return err
	}

	resourceRequests := make([]resourceapi.DeviceRequest, len(resourceInfos))
	for i, info := range resourceInfos {
		containerIndex, err := checkResourceInfo(pod, i, info)
		if err != nil {
			logger.V(3).Error(err, "")
			return err
		}
		container := &pod.Spec.Containers[containerIndex]
		resourceRequestName := util.GenerateK8sSafeResourceName(container.Name, "vgpu")

		request := buildDeviceRequest(pod, resourceRequestName, util.VGPUDeviceClassName, info)
		resourceRequests[i] = request
	}

	ownerPod := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	createTimestamp := fmt.Sprintf("%v", time.Now().UnixMilli())
	resourceClaimName := util.GenerateK8sSafeResourceName(resourceName)
	resourceClaim := h.buildResourceClaim(pod, resourceRequests, resourceClaimName, ownerPod, createTimestamp)

	if err := h.client.Create(ctx, resourceClaim); err != nil {
		logger.Error(err, "Failed to create combined vGPU resourceClaim")
		return err
	}

	logger.Info("Successfully created combined vGPU resourceClaim", "resourceClaim", klog.KObj(resourceClaim))

	return nil
}

func checkResourceInfo(pod *corev1.Pod, infoIndex int, resourceInfo common.ResourceInfo) (containerIndex int, err error) {
	containerIndex = slices.IndexFunc(pod.Spec.Containers, func(container corev1.Container) bool {
		return container.Name == resourceInfo.Name
	})
	if containerIndex < 0 {
		err = apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
			field.Invalid(field.NewPath("metadata").Child("annotations").
				Child(util.DRAOriResAnnotation).Index(infoIndex).Child("containerName"),
				resourceInfo.Name, "container not found")})
		return containerIndex, err
	}

	quantity, ok := resourceInfo.Resources[corev1.ResourceName(util.VGPUCoreResourceName)]
	if ok && quantity.Value() > util.HundredCore {
		msg := fmt.Sprintf("container %s requests vGPU core exceeding limit", resourceInfo.Name)
		err = apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
			field.Invalid(field.NewPath("spec").Child("containers").Index(containerIndex).Child("resources").
				Child("limits").Key(util.VGPUCoreResourceName), quantity.Value(), msg)})
		return containerIndex, err
	}
	return containerIndex, nil
}

func (h *validateHandle) createMultiResourceClaims(ctx context.Context, pod *corev1.Pod, resourceInfos common.ResourceInfos) (err error) {
	logger := log.FromContext(ctx)
	ownerPod := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	createTimestamp := fmt.Sprintf("%v", time.Now().UnixMilli())
	resourceClaimKeys := make([]types.NamespacedName, 0, len(resourceInfos))

	resourceName := pod.Name
	if pod.GenerateName != "" {
		resourceName, _ = util.HasAnnotation(pod, util.DRAGenNameAnnotation)
		if err = CheckResourceName(pod, resourceName); err != nil {
			return err
		}
	}

	defer func() {
		// Clean up the created resources when an error occurs,
		// using timestamp label to prevent accidental deletion of resources during batch deletion.
		if err != nil && len(resourceClaimKeys) > 0 {
			if delErr := h.client.DeleteAllOf(
				context.Background(),
				&resourceapi.ResourceClaim{},
				client.InNamespace(pod.Namespace),
				client.MatchingLabels{
					util.DRAOwnerPodLabel:   ownerPod,
					util.DRACreateTimeLabel: createTimestamp,
				},
			); delErr != nil {
				logger.Error(delErr, "Failed to clear vGPU resourceClaims", "resourceClaims", resourceClaimKeys)
			}
		}
	}()

	for i, info := range resourceInfos {
		containerIndex, err := checkResourceInfo(pod, i, info)
		if err != nil {
			logger.V(3).Error(err, "")
			return err
		}

		container := &pod.Spec.Containers[containerIndex]
		resourceClaimName := util.GenerateK8sSafeResourceName(resourceName, container.Name)
		if !slices.ContainsFunc(pod.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
			return claim.ResourceClaimName != nil && *claim.ResourceClaimName == resourceClaimName
		}) {
			err = apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
				field.Invalid(field.NewPath("spec").Child("resourceClaims"),
					pod.Spec.ResourceClaims, fmt.Sprintf("resource claim %s not found", resourceClaimName))})
			logger.V(5).Error(err, "")
			return err
		}

		// Create container resource claim
		request := buildDeviceRequest(pod, kubeletplugin.VGpuDeviceType, util.VGPUDeviceClassName, info)
		resourceClaim := h.buildResourceClaim(pod, []resourceapi.DeviceRequest{request}, resourceClaimName, ownerPod, createTimestamp)
		if err = h.client.Create(ctx, resourceClaim); err != nil {
			logger.Error(err, "Failed to create vGPU resourceClaim", "container", container.Name)
			return err
		}
		resourceClaimKeys = append(resourceClaimKeys, client.ObjectKeyFromObject(resourceClaim))
		logger.V(2).Info("Successfully created resourceClaim", "resourceClaim",
			klog.KObj(resourceClaim), "container", container.Name)
	}

	if len(resourceClaimKeys) > 0 {
		logger.Info("Successfully created all vGPU resourceClaims", "resourceClaims", resourceClaimKeys)
	}
	return nil
}

func (h *validateHandle) createResourceClaims(ctx context.Context, pod *corev1.Pod) (err error) {
	logger := log.FromContext(ctx)
	val, ok := util.HasAnnotation(pod, util.DRAOriResAnnotation)
	if !ok || len(val) == 0 {
		return nil
	}
	if len(val) > util.PodAnnotationMaxLength {
		err = apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
			field.Invalid(field.NewPath("metadata").Child("annotations").Child(util.DRAOriResAnnotation),
				field.OmitValueType{}, "recorded value is too long")})
		logger.V(5).Error(err, "")
		return err
	}
	infos := common.ResourceInfos{}
	if err = infos.Decode(val); err != nil {
		logger.V(2).Error(err, "Decoding original resource information failed")
		return apierrors.NewBadRequest(fmt.Sprintf("Decoding original resource information failed: %v", err))
	}

	// fast return
	if len(infos) == 0 {
		return nil
	}

	if h.options.CombinedResourceClaim {
		if err = h.createCombinedResourceClaim(ctx, pod, infos); err != nil {
			return err
		}
	} else {
		if err = h.createMultiResourceClaims(ctx, pod, infos); err != nil {
			return err
		}
	}
	return nil
}

func (h *validateHandle) ValidateUpdate(ctx context.Context, oldPod, newPod *corev1.Pod) error {
	return nil
}

func (h *validateHandle) ValidateDelete(ctx context.Context, pod *corev1.Pod) error {
	if h.options.DefaultConvertToDRA {
		return h.deleteResourceClaims(ctx, pod)
	}
	return nil
}

func (h *validateHandle) deleteResourceClaims(ctx context.Context, pod *corev1.Pod) error {
	logger := log.FromContext(ctx)
	val, ok := util.HasAnnotation(pod, util.DRAOriResAnnotation)
	if !ok || len(val) == 0 {
		return nil
	}
	infos := common.ResourceInfos{}
	if err := infos.Decode(val); err != nil {
		logger.V(2).Error(err, "Decoding original resource information failed")
		return nil
	}
	// fast return
	if len(infos) == 0 {
		return nil
	}

	resourceName := pod.Name

	// try delete CombinedResourceClaim
	if h.options.CombinedResourceClaim {
		resourceName, _ = util.HasAnnotation(pod, util.DRAGenNameAnnotation)
		resourceClaimName := util.GenerateK8sSafeResourceName(resourceName)
		if slices.ContainsFunc(pod.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
			return claim.ResourceClaimName != nil && *claim.ResourceClaimName == resourceClaimName
		}) {
			logger.V(1).Info("ResourceClaimName not found, skip ResourceClaim delete",
				"resourceClaimName", resourceClaimName)
		} else {
			resourceClaim := &resourceapi.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceClaimName,
					Namespace: pod.Namespace,
				},
			}
			if err := h.client.Delete(ctx, resourceClaim); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "Failed to delete ResourceClaim", "resourceClaim", klog.KObj(resourceClaim))
			}
		}
	}

	if pod.GenerateName != "" {
		resourceName, _ = util.HasAnnotation(pod, util.DRAGenNameAnnotation)
	}

	// try delete MultiResourceClaims
	for _, info := range infos {

		containerIndex := slices.IndexFunc(pod.Spec.Containers, func(container corev1.Container) bool {
			return container.Name == info.Name
		})
		if containerIndex < 0 {
			logger.V(1).Info("Container not found, skip ResourceClaim delete", "container", info.Name)
			continue
		}

		container := &pod.Spec.Containers[containerIndex]
		resourceClaimName := util.GenerateK8sSafeResourceName(resourceName, container.Name)
		if !slices.ContainsFunc(pod.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
			return claim.ResourceClaimName != nil && *claim.ResourceClaimName == resourceClaimName
		}) {
			logger.V(1).Info("ResourceClaimName for container not found, skip ResourceClaim delete",
				"container", info.Name, "resourceClaimName", resourceClaimName)
			continue
		}
		resourceClaim := &resourceapi.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resourceClaimName,
				Namespace: pod.Namespace,
			},
		}
		if err := h.client.Delete(ctx, resourceClaim); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to delete ResourceClaim", "resourceClaim", klog.KObj(resourceClaim))
		}
	}
	return nil
}

func (h *validateHandle) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("operation", req.Operation)
	logger.V(5).Info("into pod validate handle")

	var err error
	var warnings []string
	ctx = log.IntoContext(ctx, logger)
	switch req.Operation {
	case admissionv1.Create:
		pod := &corev1.Pod{}
		if err = h.decoder.Decode(req, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.ValidateCreate(ctx, pod)
	case admissionv1.Update:
		oldPod, newPod := &corev1.Pod{}, &corev1.Pod{}
		if err = h.decoder.Decode(req, newPod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if err = h.decoder.DecodeRaw(req.OldObject, oldPod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.ValidateUpdate(ctx, oldPod, newPod)
	case admissionv1.Delete:
		pod := &corev1.Pod{}
		if err = h.decoder.DecodeRaw(req.OldObject, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.ValidateDelete(ctx, pod)
	default:
		// Always skip when a DELETE or UPDATE operation received in custom mutation handler.
		return admission.Allowed("").WithWarnings(warnings...)
	}

	// Check the error message first.
	if err != nil {
		var apiStatus apierrors.APIStatus
		if errors.As(err, &apiStatus) {
			return admission.Response{AdmissionResponse: admissionv1.AdmissionResponse{
				Allowed: false,
				Result:  ptr.To[metav1.Status](apiStatus.Status()),
			}}
		}
		return admission.Denied(err.Error()).WithWarnings(warnings...)
	}
	// Return allowed if everything succeeded.
	return admission.Allowed("").WithWarnings(warnings...)
}
