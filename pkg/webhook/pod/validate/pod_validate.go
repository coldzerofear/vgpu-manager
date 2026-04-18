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
		return h.createDRAClaims(ctx, pod)
	}
	return nil
}

// buildResourceClaim Build vGPU resource claims based on container requests.
func (h *validateHandle) buildResourceClaim(pod *corev1.Pod, info common.ResourceInfo, resourceClaimName, ownerPod, timestamp string) *resourceapi.ResourceClaim {
	var (
		deviceCount     int64
		capacityRequest = make(map[resourceapi.QualifiedName]resource.Quantity)
	)
	if quantity, ok := info.Resources[corev1.ResourceName(util.VGPUNumberResourceName)]; ok {
		deviceCount = quantity.Value()
	}
	if quantity, ok := info.Resources[corev1.ResourceName(util.VGPUCoreResourceName)]; ok {
		capacityRequest[kubeletplugin.CoresResourceName] = *resource.NewQuantity(quantity.Value(), resource.DecimalSI)
	}
	if quantity, ok := info.Resources[corev1.ResourceName(util.VGPUMemoryResourceName)]; ok {
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
	deviceConstraints := []resourceapi.DeviceConstraint{{
		Requests:          []string{kubeletplugin.VGpuDeviceType},
		DistinctAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/uuid"),
	}}

	switch filter.PodUsedGPUTopologyMode(pod) {
	case util.LinkTopology:
		deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
			Requests:       []string{kubeletplugin.VGpuDeviceType},
			MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](resourceapi.FullyQualifiedName(deviceattribute.StandardDeviceAttributePCIeRoot)),
		})
	case util.NUMATopology:
		deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
			Requests:       []string{kubeletplugin.VGpuDeviceType},
			MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/numaNode"),
		})
	}
	var annotations map[string]string
	if val, ok := util.HasAnnotation(pod, util.VGPUComputePolicyAnnotation); ok {
		annotations = map[string]string{util.VGPUComputePolicyAnnotation: val}
	}
	resourceClaim := &resourceapi.ResourceClaim{
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
				Requests: []resourceapi.DeviceRequest{{
					Name: kubeletplugin.VGpuDeviceType,
					Exactly: &resourceapi.ExactDeviceRequest{
						DeviceClassName: util.VGPUDeviceClassName,
						AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
						Count:           deviceCount,
						Capacity: &resourceapi.CapacityRequirements{
							Requests: capacityRequest,
						},
						Selectors: deviceSelectors,
					},
				}},
			},
		},
	}

	return resourceClaim
}

func (h *validateHandle) createDRAClaims(ctx context.Context, pod *corev1.Pod) (err error) {
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

	ownerPod := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	createTimestamp := fmt.Sprintf("%v", time.Now().UnixMilli())
	resourceClaimKeys := make([]types.NamespacedName, 0, len(infos))
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

	resourceName := pod.Name
	if pod.GenerateName != "" {
		resourceName, _ = util.HasAnnotation(pod, util.DRAGenNameAnnotation)
	}
	for i, info := range infos {
		index := slices.IndexFunc(pod.Spec.Containers, func(container corev1.Container) bool {
			return container.Name == info.Name
		})
		if index < 0 {
			err = apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
				field.Invalid(field.NewPath("metadata").Child("annotations").
					Child(util.DRAOriResAnnotation).Index(i).Child("containerName"),
					info.Name, "container not found")})
			logger.V(5).Error(err, "")
			return err
		}
		container := &pod.Spec.Containers[index]
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
		resourceClaim := h.buildResourceClaim(pod, info, resourceClaimName, ownerPod, createTimestamp)
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

func (h *validateHandle) ValidateUpdate(ctx context.Context, oldPod, newPod *corev1.Pod) error {
	return nil
}

func (h *validateHandle) ValidateDelete(ctx context.Context, pod *corev1.Pod) error {
	if h.options.DefaultConvertToDRA {
		return h.deleteDRAClaims(ctx, pod)
	}
	return nil
}

func (h *validateHandle) deleteDRAClaims(ctx context.Context, pod *corev1.Pod) error {
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
	resourceName := pod.Name
	if pod.GenerateName != "" {
		resourceName, _ = util.HasAnnotation(pod, util.DRAGenNameAnnotation)
	}

	for _, info := range infos {
		index := slices.IndexFunc(pod.Spec.Containers, func(container corev1.Container) bool {
			return container.Name == info.Name
		})
		if index < 0 {
			logger.V(1).Info("Container not found, skip ResourceClaim delete", "container", info.Name)
			continue
		}
		container := &pod.Spec.Containers[index]
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
		if err := h.client.Delete(context.Background(), resourceClaim); err != nil && !apierrors.IsNotFound(err) {
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
