/*
Copyright 2025-2026 coldzerofear

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
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/common"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const Path = "/pods/mutate"

func NewMutateWebhook(
	client client.Client, options *options.Options,
	reader resourcereader.ResourceAPIReader,
	_ events.EventRecorderLogger,
) (http.Handler, error) {
	return &admission.Webhook{
		Handler: &mutateHandle{
			decoder: admission.NewDecoder(client.Scheme()),
			options: options,
			client:  client,
			reader:  reader,
		},
		RecoverPanic: ptr.To[bool](true),
	}, nil
}

type mutateHandle struct {
	decoder admission.Decoder
	options *options.Options
	client  client.Client
	reader  resourcereader.ResourceAPIReader
}

func (h *mutateHandle) getResourceClaim(ctx context.Context, key client.ObjectKey, obj *resourceapi.ResourceClaim) error {
	if h.reader == nil {
		return h.client.Get(ctx, key, obj)
	}
	return h.reader.GetResourceClaim(ctx, key, obj)
}

func (h *mutateHandle) mutation(obj client.Object) {
	if h.reader == nil {
		return
	}
	h.reader.Mutation(obj)
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
		case string(util.NUMATopology), string(util.NUMATopologyStrict):
			setTopoMode = true
			util.InsertAnnotation(pod, util.DeviceTopologyModeAnnotation, defaultTopologyMode)
		case string(util.LinkTopology), string(util.LinkTopologyStrict):
			setTopoMode = true
			util.InsertAnnotation(pod, util.DeviceTopologyModeAnnotation, defaultTopologyMode)
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

func cleanupInvalidSchedulerAnnotation(pod *corev1.Pod) {
	if _, ok := util.HasAnnotation(pod, util.NodeSchedulerPolicyAnnotation); ok {
		delete(pod.Annotations, util.NodeSchedulerPolicyAnnotation)
	}
	if _, ok := util.HasAnnotation(pod, util.DeviceSchedulerPolicyAnnotation); ok {
		delete(pod.Annotations, util.DeviceSchedulerPolicyAnnotation)
	}
	if _, ok := util.HasAnnotation(pod, util.MemorySchedulerPolicyAnnotation); ok {
		delete(pod.Annotations, util.MemorySchedulerPolicyAnnotation)
	}
}

func (h *mutateHandle) MutateCreate(ctx context.Context, pod *corev1.Pod, dryRun bool) error {
	logger := log.FromContext(ctx)

	isVGPUPod := false
	isMultiGPUs := false
	setDefaultResource := func(container *corev1.Container) {
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
	for i := range pod.Spec.InitContainers {
		setDefaultResource(&pod.Spec.InitContainers[i])
	}
	for i := range pod.Spec.Containers {
		setDefaultResource(&pod.Spec.Containers[i])
	}
	// Cleaning metadata to prevent impact on scheduling.
	reschedule.CleanupMetadata(pod)
	if isVGPUPod {
		setDefaultSchedulerName(pod, h.options, logger)
		setDefaultNodeSchedulerPolicy(pod, h.options, logger)
		setDefaultDeviceSchedulerPolicy(pod, h.options, logger)
		setDefaultRuntimeClassName(pod, h.options, logger)
	} else {
		cleanupInvalidSchedulerAnnotation(pod)
	}
	if isMultiGPUs {
		// Setting topology mode only makes sense when requesting multiple GPUs.
		setDefaultDeviceTopologyMode(pod, h.options, logger)
	}
	// When a pod requests vGPU, resource claims, or extends resources,
	// the scheduler may need to collaborate to complete device allocation.
	// Therefore, here we will modify the specified node to a node selector to make the scheduler effective,
	// so that possible device allocation failure issues can be fixed.
	if isVGPUPod || util.HasDRARequests(pod) || util.HasExtendedResource(pod) {
		fixSpecifiedNodeName(pod, logger)
	}

	if h.options.DefaultConvertToDRA && isVGPUPod {
		reschedule.CleanupDRAMetadata(pod)
		return h.convertDRARequest(ctx, pod)
	}
	return nil
}

// convertDRARequest Convert pod's extended resource requests into DRA requests
func (h *mutateHandle) convertDRARequest(ctx context.Context, pod *corev1.Pod) error {
	logger := log.FromContext(ctx)
	resourceName := pod.Name
	if pod.GenerateName != "" {
		resourceName = fmt.Sprintf("%s-%s", strings.TrimSuffix(pod.GenerateName, "-"),
			common.GenerateRandomString(5))
	} else if h.options.CombinedResourceClaim {
		resourceName = fmt.Sprintf("%s-%s", pod.Name, common.GenerateRandomString(5))
	}

	resourceInfos := make(common.ResourceInfos, 0)
	for i := range pod.Spec.InitContainers {
		info := common.ConvertDRAContainerRequest(ctx, resourceName, &pod.Spec.InitContainers[i], h.options)
		if info != nil {
			resourceInfos = append(resourceInfos, *info)
		}
	}
	for i := range pod.Spec.Containers {
		info := common.ConvertDRAContainerRequest(ctx, resourceName, &pod.Spec.Containers[i], h.options)
		if info != nil {
			resourceInfos = append(resourceInfos, *info)
		}
	}

	// Due to compressing all container resource requests into one resource claim, only the first resource claim is inserted.
	if resourceInfos.CombinedResourceClaim() {
		pod.Spec.ResourceClaims = append(pod.Spec.ResourceClaims, corev1.PodResourceClaim{
			Name:              resourceInfos[0].ClaimName,
			ResourceClaimName: &resourceInfos[0].ClaimName,
		})
	} else {
		for _, info := range resourceInfos {
			pod.Spec.ResourceClaims = append(pod.Spec.ResourceClaims, corev1.PodResourceClaim{
				Name:              info.ClaimName,
				ResourceClaimName: &info.ClaimName,
			})
		}
	}

	if len(resourceInfos) > 0 {
		encode, err := resourceInfos.Encode()
		if err != nil {
			logger.Error(err, "Encoding original resource information failed")
			return apierrors.NewBadRequest(fmt.Sprintf("Encoding original resource information failed: %v", err))
		}
		util.InsertAnnotation(pod, util.DRAOriResAnnotation, encode)
		logger.Info("Successfully convert all vGPU requests to resourceInfos")
	}
	return nil
}

func (h *mutateHandle) MutateUpdate(ctx context.Context, pod *corev1.Pod, dryRun bool) error {
	if h.options.DefaultConvertToDRA {
		return h.updateResourceClaims(ctx, pod, dryRun)
	}
	return nil
}

func (h *mutateHandle) updateCombinedResourceClaim(ctx context.Context, pod *corev1.Pod, infos common.ResourceInfos, dryRun bool) error {
	logger := log.FromContext(ctx)

	resourceClaimName := infos[0].ClaimName
	claimKey := types.NamespacedName{Name: resourceClaimName, Namespace: pod.Namespace}
	if !slices.ContainsFunc(pod.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
		return claim.ResourceClaimName != nil && *claim.ResourceClaimName == resourceClaimName
	}) {
		logger.V(1).Info("ResourceClaimName not found, skip update", "ResourceClaim", claimKey.String())
	} else if !dryRun {
		if err := h.updateResourceClaimOwner(ctx, pod, claimKey); err != nil {
			return err
		}
	}

	delete(pod.Annotations, util.DRAOriResAnnotation)
	logger.Info("Successfully updated the ownership of combined resourceClaim", "ResourceClaim", claimKey.String())
	return nil
}

func (h *mutateHandle) updateMultiResourceClaims(ctx context.Context, pod *corev1.Pod, infos common.ResourceInfos, dryRun bool) error {
	logger := log.FromContext(ctx)

	updatedInfos := make(common.ResourceInfos, 0, len(infos))
	for i, info := range infos {
		resourceClaimName := info.ClaimName
		claimKey := types.NamespacedName{Name: resourceClaimName, Namespace: pod.Namespace}

		if !slices.ContainsFunc(pod.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
			return claim.ResourceClaimName != nil && *claim.ResourceClaimName == resourceClaimName
		}) {
			logger.V(1).Info("ResourceClaimName for container not found, skip update",
				"container", info.Name, "ResourceClaim", claimKey.String())
		} else if !dryRun {
			if err := h.updateResourceClaimOwner(ctx, pod, claimKey); err != nil {
				updatedInfos = append(updatedInfos, infos[i])
			}
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

func (h *mutateHandle) updateResourceClaims(ctx context.Context, pod *corev1.Pod, dryRun bool) error {
	logger := log.FromContext(ctx)
	val, ok := util.HasAnnotation(pod, util.DRAOriResAnnotation)
	if !ok || len(val) == 0 {
		return nil
	}
	infos := common.ResourceInfos{}
	if err := infos.Decode(val); err != nil {
		logger.V(2).Error(err, "Decoding original resource information failed")
		return nil
	} else if len(infos) == 0 { // fast return
		return nil
	}

	if infos.CombinedResourceClaim() {
		if err := h.updateCombinedResourceClaim(ctx, pod, infos, dryRun); err != nil {
			return err
		}
	} else {
		if err := h.updateMultiResourceClaims(ctx, pod, infos, dryRun); err != nil {
			return err
		}
	}

	return nil
}

func (h *mutateHandle) updateResourceClaimOwner(ctx context.Context, owner metav1.Object, claimKey types.NamespacedName) error {
	logger := log.FromContext(ctx).WithValues("ResourceClaim", claimKey.String())
	claim := &resourceapi.ResourceClaim{}
	if err := h.getResourceClaim(ctx, claimKey, claim); err != nil {
		logger.Error(err, "get resourceClaim failed")
		return client.IgnoreNotFound(err)
	}
	if !controllerutil.HasControllerReference(claim) {
		if err := controllerutil.SetControllerReference(owner, claim, h.client.Scheme()); err != nil {
			logger.Error(err, "SetControllerReference failed")
			return err
		}
		if err := h.client.Update(ctx, claim); err != nil {
			logger.Error(err, "update resourceClaim ownerReference failed")
			return err
		}
		h.mutation(claim)
	} else {
		logger.V(3).Info("resourceClaim already has a controller reference, skip updating")
	}
	return nil
}

func (h *mutateHandle) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("operation", req.Operation)
	logger.V(4).Info("into pod mutate handle")

	dryrun := req.DryRun != nil && *req.DryRun
	if dryrun {
		logger = logger.WithValues("dryRun", true)
	}
	var err error
	pod := &corev1.Pod{}
	ctx = log.IntoContext(ctx, logger)
	switch req.Operation {
	case admissionv1.Create:
		if err = h.decoder.Decode(req, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.MutateCreate(ctx, pod, dryrun)
	case admissionv1.Update:
		if err = h.decoder.Decode(req, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.MutateUpdate(ctx, pod, dryrun)
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
