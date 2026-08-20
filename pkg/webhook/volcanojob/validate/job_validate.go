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

package validate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/common"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	vcv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

const Path = "/volcano-jobs/validate"

func NewValidateWebhook(
	client client.Client, options *options.Options,
	reader resourcereader.ResourceAPIReader,
	_ events.EventRecorderLogger,
) (http.Handler, error) {
	return &admission.Webhook{
		Handler: &validateHandle{
			decoder: admission.NewDecoder(client.Scheme()),
			options: options,
			client:  client,
			reader:  reader,
		},
		RecoverPanic: ptr.To[bool](true),
	}, nil
}

type validateHandle struct {
	decoder admission.Decoder
	options *options.Options
	client  client.Client
	reader  resourcereader.ResourceAPIReader
}

func (h *validateHandle) ValidateCreate(ctx context.Context, job *vcv1alpha1.Job, dryRun bool) error {
	if h.options.DefaultConvertToDRA {
		if err := h.createResourceClaimTempaltes(ctx, job, dryRun); err != nil {
			return err
		}
	}
	return nil
}

func (h *validateHandle) createResourceClaimTempaltes(ctx context.Context, job *vcv1alpha1.Job, dryRun bool) (err error) {
	logger := log.FromContext(ctx)
	val, ok := util.HasAnnotation(job, util.DRAOriResAnnotation)
	if !ok || len(val) == 0 {
		return nil
	} else if len(val) > util.PodAnnotationMaxLength {
		groupKind := vcv1alpha1.SchemeGroupVersion.WithKind("Job").GroupKind()
		err = apierrors.NewInvalid(groupKind, job.Name, field.ErrorList{
			field.Invalid(field.NewPath("metadata").Child("annotations").
				Child(util.DRAOriResAnnotation), field.OmitValueType{}, "recorded value is too long")})
		logger.V(5).Error(err, "")
		return err
	}

	var infoMap map[string]common.ResourceInfos
	if err = json.Unmarshal([]byte(val), &infoMap); err != nil {
		logger.V(2).Error(err, "Decoding original resource information failed")
		return apierrors.NewBadRequest(fmt.Sprintf("Decoding original resource information failed: %v", err))
	} else if len(infoMap) == 0 { // fast return
		return nil
	}
	var deleteTasks []string

	ownerKey := fmt.Sprintf("%s-%s", job.Namespace, job.Name)
	createTimestamp := fmt.Sprintf("%v", time.Now().UnixMilli())

	var claimTemplateKeys []client.ObjectKey
	defer func() {
		// Clean up the created resources when an error occurs,
		// using timestamp label to prevent accidental deletion of resources during batch deletion.
		if err != nil && len(claimTemplateKeys) > 0 && !dryRun {
			if delErr := h.client.DeleteAllOf(
				context.Background(), &resourceapi.ResourceClaimTemplate{},
				client.InNamespace(job.Namespace), client.MatchingLabels{
					util.DRAOwnerKeyLabel:   ownerKey,
					util.DRACreateTimeLabel: createTimestamp,
				},
			); delErr != nil {
				logger.Error(delErr, "Failed to clear vGPU resourceClaimTemplates", "resourceClaimTemplates", claimTemplateKeys)
			}
		}
	}()

	var templateKeys []client.ObjectKey
	for taskName, infos := range infoMap {
		subLogger := logger.WithValues("taskName", taskName)
		index := slices.IndexFunc(job.Spec.Tasks, func(spec vcv1alpha1.TaskSpec) bool {
			return spec.Name == taskName
		})
		if index < 0 {
			subLogger.V(2).Info("TaskSpec not found, skip ResourceClaimTemplate creation")
			deleteTasks = append(deleteTasks, taskName)
			continue
		}
		task := &job.Spec.Tasks[index]
		subCtx := log.IntoContext(ctx, subLogger)
		err = nil
		if infos.CombinedResourceClaim() {
			templateKeys, err = h.createCombinedResourceClaimTemplate(subCtx, job, index, task, infos, ownerKey, createTimestamp, dryRun)
		} else {
			templateKeys, err = h.createMultiResourceClaimTemplates(subCtx, job, index, task, infos, ownerKey, createTimestamp, dryRun)
		}
		if len(templateKeys) > 0 {
			claimTemplateKeys = append(claimTemplateKeys, templateKeys...)
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *validateHandle) createCombinedResourceClaimTemplate(
	ctx context.Context, job *vcv1alpha1.Job, index int, task *vcv1alpha1.TaskSpec,
	infos common.ResourceInfos, ownerKey, createTimestamp string, dryRun bool,
) ([]client.ObjectKey, error) {
	logger := log.FromContext(ctx)

	var resourceClaimName string
	resourceRequests := make([]resourceapi.DeviceRequest, len(infos))
	taskPath := field.NewPath("spec").Child("tasks").Index(index)
	for i, info := range infos {
		if _, errs := common.CheckTaskResourceInfo(taskPath, task, i, info); len(errs) > 0 {
			err := apierrors.NewInvalid(vcv1alpha1.SchemeGroupVersion.WithKind("Job").GroupKind(), job.Name, errs)
			logger.V(3).Error(err, "")
			return nil, err
		}
		if i == 0 {
			resourceClaimName = info.ClaimName
			if !slices.ContainsFunc(task.Template.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
				return claim.ResourceClaimTemplateName != nil && *claim.ResourceClaimTemplateName == resourceClaimName
			}) {
				err := apierrors.NewInvalid(vcv1alpha1.SchemeGroupVersion.WithKind("Job").GroupKind(), job.Name, field.ErrorList{
					field.Invalid(taskPath.Child("template").Child("spec").Child("resourceClaims"),
						task.Template.Spec.ResourceClaims, fmt.Sprintf("ResourceClaimTemplateName %q not found", resourceClaimName))})
				logger.V(5).Error(err, "")
				return nil, err
			}
		}

		deviceRequest := common.BuildTaskDeviceRequest(task, h.options.VGPUDeviceClassName, info)
		resourceRequests[i] = deviceRequest
	}

	resourceClaimTemplate := common.BuildTaskResourceClaimTemplate(task, resourceRequests, resourceClaimName, ownerKey, createTimestamp)

	if !dryRun {
		if err := h.client.Create(ctx, resourceClaimTemplate); err != nil {
			logger.Error(err, "Failed to create combined vGPU resourceClaimTemplate")
			return nil, err
		}
		h.mutation(resourceClaimTemplate)
	}

	logger.Info("Successfully created combined vGPU resourceClaimTemplate", "resourceClaimTemplate", klog.KObj(resourceClaimTemplate))

	return []client.ObjectKey{client.ObjectKeyFromObject(resourceClaimTemplate)}, nil
}

func (h *validateHandle) createMultiResourceClaimTemplates(
	ctx context.Context, job *vcv1alpha1.Job, index int, task *vcv1alpha1.TaskSpec,
	infos common.ResourceInfos, ownerKey, createTimestamp string, dryRun bool,
) ([]client.ObjectKey, error) {
	logger := log.FromContext(ctx)

	taskPath := field.NewPath("spec").Child("tasks").Index(index)
	resourceClaimTemplateKeys := make([]client.ObjectKey, 0, len(infos))
	for i, info := range infos {
		if _, errs := common.CheckTaskResourceInfo(taskPath, task, i, info); errs != nil {
			err := apierrors.NewInvalid(vcv1alpha1.SchemeGroupVersion.WithKind("Job").GroupKind(), job.Name, errs)
			logger.V(3).Error(err, "")
			return resourceClaimTemplateKeys, err
		}

		if !slices.ContainsFunc(task.Template.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
			return claim.ResourceClaimTemplateName != nil && *claim.ResourceClaimTemplateName == info.ClaimName
		}) {
			err := apierrors.NewInvalid(vcv1alpha1.SchemeGroupVersion.WithKind("Job").GroupKind(), job.Name, field.ErrorList{
				field.Invalid(taskPath.Child("template").Child("spec").Child("resourceClaims"),
					task.Template.Spec.ResourceClaims, fmt.Sprintf("ResourceClaimTemplateName %q not found", info.ClaimName))})
			logger.V(5).Error(err, "")
			return resourceClaimTemplateKeys, err
		}

		// Create container resource claim
		deviceRequest := common.BuildTaskDeviceRequest(task, h.options.VGPUDeviceClassName, info)
		resourceClaimTemplate := common.BuildTaskResourceClaimTemplate(task, []resourceapi.DeviceRequest{deviceRequest}, info.ClaimName, ownerKey, createTimestamp)

		if !dryRun {
			if err := h.client.Create(ctx, resourceClaimTemplate); err != nil {
				logger.Error(err, "Failed to create vGPU resourceClaimTemplate", "container", info.Name)
				return resourceClaimTemplateKeys, err
			}
			h.mutation(resourceClaimTemplate)
		}

		resourceClaimTemplateKeys = append(resourceClaimTemplateKeys, client.ObjectKeyFromObject(resourceClaimTemplate))
		logger.V(2).Info("Successfully created resourceClaimTemplate", "resourceClaimTemplate", klog.KObj(resourceClaimTemplate), "container", info.Name)
	}

	if len(resourceClaimTemplateKeys) > 0 {
		logger.Info("Successfully created all vGPU resourceClaimTemplates", "resourceClaimTemplates", resourceClaimTemplateKeys)
	}

	return resourceClaimTemplateKeys, nil
}

func (h *validateHandle) mutation(obj client.Object) {
	if h.reader == nil {
		return
	}
	h.reader.Mutation(obj)
}

//func (h *validateHandle) ValidateUpdate(ctx context.Context, oldJob, newJob *vcv1alpha1.Job) error {
//	return nil
//}

func (h *validateHandle) ValidateDelete(ctx context.Context, job *vcv1alpha1.Job, dryRun bool) error {
	if h.options.DefaultConvertToDRA && !dryRun {
		return h.deleteResourceClaimTemplates(ctx, job)
	}
	return nil
}

func (h *validateHandle) deleteResourceClaimTemplates(ctx context.Context, job *vcv1alpha1.Job) error {
	logger := log.FromContext(ctx)
	val, ok := util.HasAnnotation(job, util.DRAOriResAnnotation)
	if !ok || len(val) == 0 {
		return nil
	}
	var infoMap map[string]common.ResourceInfos
	if err := json.Unmarshal([]byte(val), &infoMap); err != nil {
		logger.V(2).Error(err, "Decoding original resource information failed")
		return nil
	} else if len(infoMap) == 0 { // fast return
		return nil
	}

	for taskName, infos := range infoMap {
		subLogger := logger.WithValues("taskName", taskName)
		index := slices.IndexFunc(job.Spec.Tasks, func(spec vcv1alpha1.TaskSpec) bool {
			return spec.Name == taskName
		})
		if index < 0 {
			subLogger.V(2).Info("TaskSpec not found, skip ResourceClaimTemplate deletion")
			continue
		}
		task := &job.Spec.Tasks[index]
		if infos.CombinedResourceClaim() {
			// try delete CombinedResourceClaim
			claimTemplateName := infos[0].ClaimName
			if !slices.ContainsFunc(task.Template.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
				return claim.ResourceClaimTemplateName != nil && *claim.ResourceClaimTemplateName == claimTemplateName
			}) {
				subLogger.V(1).Info("ResourceClaimTemplate not found, skip deletion",
					"resourceClaimTemplateName", claimTemplateName)
			} else {
				resourceClaimTemplate := &resourceapi.ResourceClaimTemplate{ObjectMeta: metav1.ObjectMeta{
					Name:      claimTemplateName,
					Namespace: job.Namespace,
				}}
				if err := h.client.Delete(ctx, resourceClaimTemplate); err != nil && !apierrors.IsNotFound(err) {
					subLogger.Error(err, "Failed to delete ResourceClaimTemplate", "resourceClaimTemplate", klog.KObj(resourceClaimTemplate))
				}
			}
		} else {
			// try delete MultiResourceClaims
			for _, info := range infos {
				claimTemplateName := info.ClaimName
				if !slices.ContainsFunc(task.Template.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
					return claim.ResourceClaimTemplateName != nil && *claim.ResourceClaimTemplateName == claimTemplateName
				}) {
					subLogger.V(1).Info("ResourceClaimTemplate for container not found, skip deletion",
						"container", info.Name, "resourceClaimTemplateName", claimTemplateName)
					continue
				}
				resourceClaimTemplate := &resourceapi.ResourceClaimTemplate{ObjectMeta: metav1.ObjectMeta{
					Name:      claimTemplateName,
					Namespace: job.Namespace,
				}}
				if err := h.client.Delete(ctx, resourceClaimTemplate); err != nil && !apierrors.IsNotFound(err) {
					subLogger.Error(err, "Failed to delete ResourceClaimTemplate", "resourceClaimTemplate", klog.KObj(resourceClaimTemplate))
				}
			}
		}
	}

	return nil
}

func (h *validateHandle) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("operation", req.Operation)
	logger.V(4).Info("into volcano job validate handle")

	dryrun := req.DryRun != nil && *req.DryRun
	if dryrun {
		logger = logger.WithValues("dryRun", true)
	}
	var err error
	var warnings []string
	ctx = log.IntoContext(ctx, logger)
	switch req.Operation {
	case admissionv1.Create:
		job := &vcv1alpha1.Job{}
		if err = h.decoder.Decode(req, job); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.ValidateCreate(ctx, job, dryrun)
	//case admissionv1.Update:
	//	oldJob, newJob := &vcv1alpha1.Job{}, &vcv1alpha1.Job{}
	//	if err = h.decoder.Decode(req, newJob); err != nil {
	//		return admission.Errored(http.StatusBadRequest, err)
	//	}
	//	if err = h.decoder.DecodeRaw(req.OldObject, oldJob); err != nil {
	//		return admission.Errored(http.StatusBadRequest, err)
	//	}
	//	err = h.ValidateUpdate(ctx, oldJob, newJob)
	case admissionv1.Delete:
		job := &vcv1alpha1.Job{}
		if err = h.decoder.DecodeRaw(req.OldObject, job); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.ValidateDelete(ctx, job, dryrun)
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
