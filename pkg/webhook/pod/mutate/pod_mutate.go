package mutate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/controller/reschedule"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/pod/common"
	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const Path = "/pods/mutate"

func NewMutateWebhook(client client.Client, options *options.Options) (*admission.Webhook, error) {
	return &admission.Webhook{
		Handler: &mutateHandle{
			decoder: admission.NewDecoder(client.Scheme()),
			options: options,
			client:  client,
		},
		RecoverPanic: ptr.To[bool](true),
	}, nil
}

type mutateHandle struct {
	decoder admission.Decoder
	options *options.Options
	client  client.Client
}

func setDefaultSchedulerName(pod *corev1.Pod, options *options.Options, logger logr.Logger) {
	if len(options.SchedulerName) > 0 && (pod.Spec.SchedulerName == "" || pod.Spec.SchedulerName == corev1.DefaultSchedulerName) {
		pod.Spec.SchedulerName = options.SchedulerName
		logger.V(4).Info("Successfully set schedulerName", "schedulerName", options.SchedulerName)
	}
	if len(options.SchedulerName) > 0 && pod.Spec.SchedulerName != options.SchedulerName {
		logger.Info("Pod already has different scheduler assigned", "schedulerName", pod.Spec.SchedulerName)
	}
}

func setDefaultNodeSchedulerPolicy(pod *corev1.Pod, options *options.Options, logger logr.Logger) {
	if _, ok := util.HasAnnotation(pod, util.NodeSchedulerPolicyAnnotation); !ok {
		setPolicy := false
		defaultNodePolicy := strings.ToLower(options.DefaultNodePolicy)
		switch defaultNodePolicy {
		case string(util.BinpackPolicy):
			setPolicy = true
			util.InsertAnnotation(pod, util.NodeSchedulerPolicyAnnotation, string(util.BinpackPolicy))
		case string(util.SpreadPolicy):
			setPolicy = true
			util.InsertAnnotation(pod, util.NodeSchedulerPolicyAnnotation, string(util.SpreadPolicy))
		}
		if setPolicy {
			logger.V(4).Info("Successfully set default node scheduler policy", "NodeSchedulerPolicy", defaultNodePolicy)
		}
	}
}

func setDefaultDeviceSchedulerPolicy(pod *corev1.Pod, options *options.Options, logger logr.Logger) {
	if _, ok := util.HasAnnotation(pod, util.DeviceSchedulerPolicyAnnotation); !ok {
		setPolicy := false
		defaultDevicePolicy := strings.ToLower(options.DefaultDevicePolicy)
		switch defaultDevicePolicy {
		case string(util.BinpackPolicy):
			setPolicy = true
			util.InsertAnnotation(pod, util.DeviceSchedulerPolicyAnnotation, string(util.BinpackPolicy))
		case string(util.SpreadPolicy):
			setPolicy = true
			util.InsertAnnotation(pod, util.DeviceSchedulerPolicyAnnotation, string(util.SpreadPolicy))
		}
		if setPolicy {
			logger.V(4).Info("Successfully set default device scheduler policy", "DeviceSchedulerPolicy", defaultDevicePolicy)
		}
	}
}

func setDefaultDeviceTopologyMode(pod *corev1.Pod, options *options.Options, logger logr.Logger) {
	if _, ok := util.HasAnnotation(pod, util.DeviceTopologyModeAnnotation); !ok {
		setTopoMode := false
		defaultTopologyMode := strings.ToLower(options.DefaultTopologyMode)
		switch defaultTopologyMode {
		case string(util.NUMATopology):
			setTopoMode = true
			util.InsertAnnotation(pod, util.DeviceTopologyModeAnnotation, string(util.NUMATopology))
		case string(util.LinkTopology):
			setTopoMode = true
			util.InsertAnnotation(pod, util.DeviceTopologyModeAnnotation, string(util.LinkTopology))
		}
		if setTopoMode {
			logger.V(4).Info("Successfully set default device topology mode", "DeviceTopologyMode", defaultTopologyMode)
		}
	}
}

func setDefaultRuntimeClassName(pod *corev1.Pod, options *options.Options, logger logr.Logger) {
	if len(options.DefaultRuntimeClass) > 0 && (pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName == "") {
		pod.Spec.RuntimeClassName = ptr.To[string](options.DefaultRuntimeClass)
		logger.V(4).Info("Successfully set default runtimeClassName", "runtimeClassName", options.DefaultRuntimeClass)
	}
}

// fixSpecifiedNodeName fix using nodeSelector to specify scheduling nodes for pod.
func fixSpecifiedNodeName(pod *corev1.Pod, logger logr.Logger) {
	if pod.Spec.NodeName != "" {
		if pod.Spec.NodeSelector == nil {
			pod.Spec.NodeSelector = map[string]string{}
		}
		pod.Spec.NodeSelector[corev1.LabelHostname] = pod.Spec.NodeName
		logger.Info("Successfully fix specified nodeName", "spec.nodeName", pod.Spec.NodeName)
		pod.Spec.NodeName = ""
	}
}

func cleanupSchedulerPolicyAnnotation(pod *corev1.Pod) {
	if _, ok := util.HasAnnotation(pod, util.NodeSchedulerPolicyAnnotation); ok {
		delete(pod.Annotations, util.NodeSchedulerPolicyAnnotation)
	}
	if _, ok := util.HasAnnotation(pod, util.DeviceSchedulerPolicyAnnotation); ok {
		delete(pod.Annotations, util.DeviceSchedulerPolicyAnnotation)
	}
}

func cleanupTopologyModeAnnotation(pod *corev1.Pod) {
	if _, ok := util.HasAnnotation(pod, util.DeviceTopologyModeAnnotation); ok {
		delete(pod.Annotations, util.DeviceTopologyModeAnnotation)
	}
}

func (h *mutateHandle) MutateCreate(ctx context.Context, pod *corev1.Pod) error {
	logger := log.FromContext(ctx)

	isVGPUPod := false
	isMultiGPUs := false
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		number := util.GetResourceOfContainer(container, util.VGPUNumberResourceName)
		cores := util.GetResourceOfContainer(container, util.VGPUCoreResourceName)
		memory := util.GetResourceOfContainer(container, util.VGPUMemoryResourceName)
		if number == 0 && (cores > 0 || memory > 0) {
			number = 1
			quantity := resource.MustParse(fmt.Sprintf("%d", number))
			container.Resources.Limits[corev1.ResourceName(util.VGPUNumberResourceName)] = quantity
			logger.V(4).Info("Successfully set 1 vGPU number", "containerName", container.Name)
		}

		if number > 0 && cores == 0 && memory == 0 {
			cores = util.HundredCore
			quantity := resource.MustParse(fmt.Sprintf("%d", cores))
			container.Resources.Limits[corev1.ResourceName(util.VGPUCoreResourceName)] = quantity
			logger.V(4).Info("Successfully set 100 vGPU cores", "containerName", container.Name)
		}

		if number > 0 {
			isVGPUPod = true
		}
		if number > 1 {
			isMultiGPUs = true
		}
	}
	// Cleaning metadata to prevent impact on scheduling.
	reschedule.CleanupMetadata(pod)
	if isVGPUPod {
		setDefaultSchedulerName(pod, h.options, logger)
		setDefaultNodeSchedulerPolicy(pod, h.options, logger)
		setDefaultDeviceSchedulerPolicy(pod, h.options, logger)
		setDefaultRuntimeClassName(pod, h.options, logger)
		fixSpecifiedNodeName(pod, logger)
	} else {
		// Clean up invalid scheduling policy annotations.
		cleanupSchedulerPolicyAnnotation(pod)
	}
	if isMultiGPUs {
		// Setting topology mode only makes sense when requesting multiple GPUs.
		setDefaultDeviceTopologyMode(pod, h.options, logger)
	} else {
		// Clean up invalid topology mode annotations.
		cleanupTopologyModeAnnotation(pod)
	}

	delete(pod.Annotations, util.DRAOriResAnnotation)
	if h.options.DefaultConvertToDRA {
		return h.convertDRARequest(ctx, pod)
	}
	return nil
}

// convertDRARequest Convert pod's extended resource requests into DRA requests
func (h *mutateHandle) convertDRARequest(ctx context.Context, pod *corev1.Pod) error {
	logger := log.FromContext(ctx)
	resourceInfos := make(common.ResourceInfos, 0)
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if !util.IsVGPURequiredContainer(container) {
			continue
		}

		deviceCount := util.GetResourceOfContainer(container, util.VGPUNumberResourceName)
		deviceCores := util.GetResourceOfContainer(container, util.VGPUCoreResourceName)
		deviceMemory := util.GetResourceOfContainer(container, util.VGPUMemoryResourceName)

		resourceInfo := common.ResourceInfo{
			Name: container.Name,
			Resources: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceName(util.VGPUNumberResourceName): *resource.NewQuantity(deviceCount, resource.DecimalSI),
				corev1.ResourceName(util.VGPUCoreResourceName):   *resource.NewQuantity(deviceCores, resource.DecimalSI),
				corev1.ResourceName(util.VGPUMemoryResourceName): *resource.NewQuantity(deviceMemory, resource.DecimalSI),
			},
		}
		resourceInfos = append(resourceInfos, resourceInfo)
		// Convert container resource requests into DRA requests.
		util.DelResourceOfContainer(container, util.VGPUNumberResourceName)
		util.DelResourceOfContainer(container, util.VGPUCoreResourceName)
		util.DelResourceOfContainer(container, util.VGPUMemoryResourceName)
		resourceClaimName := util.GenerateK8sSafeResourceName(pod.Name, container.Name)
		container.Resources.Claims = append(container.Resources.Claims, corev1.ResourceClaim{
			Name: resourceClaimName,
		})
		pod.Spec.ResourceClaims = append(pod.Spec.ResourceClaims, corev1.PodResourceClaim{
			Name:              resourceClaimName,
			ResourceClaimName: &resourceClaimName,
		})
		logger.V(2).Info("Successfully convert vGPU requests to resourceClaims", "container", container.Name,
			"vGPUNumber", deviceCount, "vGPUCores", deviceCores, "vGPUMemory", deviceMemory)
	}
	if len(resourceInfos) > 0 {
		encode, err := resourceInfos.Encode()
		if err != nil {
			logger.Error(err, "Encoding original resource information failed")
			return apierrors.NewBadRequest(fmt.Sprintf("Encoding original resource information failed: %v", err))
		}
		util.InsertAnnotation(pod, util.DRAOriResAnnotation, encode)
		logger.Info("Successfully convert all vGPU requests to resourceClaims")
	}
	return nil
}

func (h *mutateHandle) updateDRAClaims(ctx context.Context, pod *corev1.Pod) error {
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

	updatedInfos := make(common.ResourceInfos, 0, len(infos))
	for i, info := range infos {
		index := slices.IndexFunc(pod.Spec.Containers, func(container corev1.Container) bool {
			return container.Name == info.Name
		})
		if index < 0 {
			logger.V(1).Info("Container not found, skip ResourceClaim update", "container", info.Name)
			continue
		}
		container := &pod.Spec.Containers[index]
		resourceClaimName := util.GenerateK8sSafeResourceName(pod.Name, container.Name)
		if !slices.ContainsFunc(pod.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
			return claim.ResourceClaimName != nil && *claim.ResourceClaimName == resourceClaimName
		}) {
			logger.V(1).Info("ResourceClaimName for container not found, skip ResourceClaim update",
				"container", info.Name, "resourceClaimName", resourceClaimName)
			continue
		}
		resourceClaimKey := types.NamespacedName{
			Name:      resourceClaimName,
			Namespace: pod.Namespace,
		}
		if err := h.updateResourceOwner(ctx, pod, resourceClaimKey); err != nil {
			updatedInfos = append(updatedInfos, infos[i])
		}
	}

	if len(updatedInfos) > 0 {
		encode, err := updatedInfos.Encode()
		if err != nil {
			logger.Error(err, "Encoding original resource information failed")
			return apierrors.NewBadRequest(fmt.Sprintf("Encoding original resource information failed: %v", err))
		}
		util.InsertAnnotation(pod, util.DRAOriResAnnotation, encode)
	} else {
		delete(pod.Annotations, util.DRAOriResAnnotation)
		logger.Info("Successfully updated the ownership of all resourceClaims")
	}
	return nil
}

func (h *mutateHandle) MutateUpdate(ctx context.Context, pod *corev1.Pod) error {
	if h.options.DefaultConvertToDRA {
		return h.updateDRAClaims(ctx, pod)
	}
	return nil
}

func (h *mutateHandle) updateResourceOwner(ctx context.Context, owner metav1.Object, resourceKey types.NamespacedName) error {
	logger := log.FromContext(ctx).WithValues("ResourceClaim", resourceKey.String())
	claim := &resourceapi.ResourceClaim{}
	if err := h.client.Get(ctx, resourceKey, claim); err != nil {
		logger.Error(err, "get resourceClaim failed")
		return client.IgnoreNotFound(err)
	}
	if !controllerutil.HasControllerReference(claim) {
		if err := controllerutil.SetControllerReference(claim, owner, h.client.Scheme()); err != nil {
			logger.Error(err, "SetControllerReference failed")
			return err
		}
		if err := h.client.Update(ctx, claim); err != nil {
			logger.Error(err, "update resourceClaim failed")
			return err
		}
	} else {
		logger.V(3).Info("resourceClaim already has a controller reference, skip updating")
	}
	return nil
}

func (h *mutateHandle) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("operation", req.Operation)
	logger.V(5).Info("into pod mutate handle")

	var err error
	pod := &corev1.Pod{}
	ctx = log.IntoContext(ctx, logger)
	switch req.Operation {
	case admissionv1.Create:
		if err = h.decoder.Decode(req, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.MutateCreate(ctx, pod)
	case admissionv1.Update:
		if err = h.decoder.Decode(req, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.MutateUpdate(ctx, pod)
	default:
		// Always skip when a DELETE or UPDATE operation received in custom mutation handler.
		return admission.ValidationResponse(true, "")
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
		return admission.Denied(err.Error())
	}

	// Create the patch
	marshalled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshalled)
}
