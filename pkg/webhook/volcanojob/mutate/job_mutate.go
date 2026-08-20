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
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/common"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	vcv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

const Path = "/volcano-jobs/mutate"

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

func (h *mutateHandle) MutateCreate(ctx context.Context, job *vcv1alpha1.Job, dryRun bool) error {
	if h.options.DefaultConvertToDRA {
		//reschedule.CleanupDRAMetadata(job)

		logger := log.FromContext(ctx)
		resourceInfoMap := make(map[string]common.ResourceInfos)
		for i := range job.Spec.Tasks {
			task := &job.Spec.Tasks[i]
			if !util.IsVGPUResourceVcTask(task) {
				continue
			}
			resourceName := job.Name
			if job.GenerateName != "" {
				resourceName = fmt.Sprintf("%s-%s-%s", strings.TrimSuffix(job.GenerateName, "-"),
					task.Name, common.GenerateRandomString(5))
			} else if h.options.CombinedResourceClaim {
				resourceName = fmt.Sprintf("%s-%s-%s", resourceName,
					task.Name, common.GenerateRandomString(5))
			}

			subLogger := logger.WithValues("taskName", task.Name)
			if infos := h.convertDRARequest(log.IntoContext(ctx, subLogger), task, resourceName); len(infos) > 0 {
				resourceInfoMap[task.Name] = infos
			}
		}

		if len(resourceInfoMap) > 0 {
			marshal, err := json.Marshal(resourceInfoMap)
			if err != nil {
				logger.Error(err, "Encoding original resource information failed")
				return apierrors.NewBadRequest(fmt.Sprintf("Encoding original resource information failed: %v", err))
			}
			util.InsertAnnotation(job, util.DRAOriResAnnotation, string(marshal))
		}
	}
	return nil
}

// convertDRARequest Convert pod's extended resource requests into DRA requests
func (h *mutateHandle) convertDRARequest(ctx context.Context, task *vcv1alpha1.TaskSpec, resourceName string) common.ResourceInfos {
	logger := log.FromContext(ctx)

	resourceInfos := make(common.ResourceInfos, 0)
	for i := range task.Template.Spec.InitContainers {
		info := common.ConvertDRAContainerRequest(ctx, resourceName, &task.Template.Spec.InitContainers[i], h.options)
		if info != nil {
			resourceInfos = append(resourceInfos, *info)
		}
	}
	for i := range task.Template.Spec.Containers {
		info := common.ConvertDRAContainerRequest(ctx, resourceName, &task.Template.Spec.Containers[i], h.options)
		if info != nil {
			resourceInfos = append(resourceInfos, *info)
		}
	}

	// Due to compressing all container resource requests into one resource claim, only the first resource claim is inserted.
	if resourceInfos.CombinedResourceClaim() {
		task.Template.Spec.ResourceClaims = append(task.Template.Spec.ResourceClaims, corev1.PodResourceClaim{
			Name:                      resourceInfos[0].ClaimName,
			ResourceClaimTemplateName: &resourceInfos[0].ClaimName,
		})
	} else {
		for _, info := range resourceInfos {
			task.Template.Spec.ResourceClaims = append(task.Template.Spec.ResourceClaims, corev1.PodResourceClaim{
				Name:                      info.ClaimName,
				ResourceClaimTemplateName: &info.ClaimName,
			})
		}
	}

	if len(resourceInfos) > 0 {
		logger.Info("Successfully convert all vGPU requests to resourceInfos")
	}
	return resourceInfos
}

func (h *mutateHandle) MutateUpdate(ctx context.Context, job *vcv1alpha1.Job, dryRun bool) error {
	if h.options.DefaultConvertToDRA {
		return h.updateResourceClaimTemplates(ctx, job, dryRun)
	}
	return nil
}

func (h *mutateHandle) updateResourceClaimTemplates(ctx context.Context, job *vcv1alpha1.Job, dryRun bool) error {
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

	var deleteTaskNames []string
	for taskName, infos := range infoMap {
		subLogger := logger.WithValues("taskName", taskName)
		index := slices.IndexFunc(job.Spec.Tasks, func(spec vcv1alpha1.TaskSpec) bool {
			return spec.Name == taskName
		})
		if index < 0 {
			subLogger.V(2).Info("TaskSpec not found, skip ResourceClaimTemplate update")
			continue
		}
		task := &job.Spec.Tasks[index]
		subCtx := log.IntoContext(ctx, subLogger)

		if infos.CombinedResourceClaim() {
			if err := h.updateCombinedResourceClaimTemplate(subCtx, job, task, infos, dryRun); err == nil {
				deleteTaskNames = append(deleteTaskNames, taskName)
			}
		} else {
			if remaining := h.updateMultiResourceClaimTemplates(subCtx, job, task, infos, dryRun); len(remaining) == 0 {
				deleteTaskNames = append(deleteTaskNames, taskName)
			} else {
				infoMap[taskName] = remaining
			}
		}
	}

	for _, taskName := range deleteTaskNames {
		delete(infoMap, taskName)
	}

	if len(infoMap) > 0 {
		marshal, err := json.Marshal(infoMap)
		if err != nil {
			logger.Error(err, "Encoding original resource information failed")
			return apierrors.NewBadRequest(fmt.Sprintf("Encoding original resource information failed: %v", err))
		}
		util.InsertAnnotation(job, util.DRAOriResAnnotation, string(marshal))
	} else {
		delete(job.Annotations, util.DRAOriResAnnotation)
	}

	return nil
}

func (h *mutateHandle) updateCombinedResourceClaimTemplate(
	ctx context.Context, job *vcv1alpha1.Job, task *vcv1alpha1.TaskSpec, infos common.ResourceInfos, dryRun bool,
) error {
	logger := log.FromContext(ctx)

	resourceClaimName := infos[0].ClaimName
	claimTemplateKey := types.NamespacedName{Name: resourceClaimName, Namespace: job.Namespace}
	if !slices.ContainsFunc(task.Template.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
		return claim.ResourceClaimTemplateName != nil && *claim.ResourceClaimTemplateName == resourceClaimName
	}) {
		logger.V(1).Info("ResourceClaimTemplateName not found, skip update",
			"ResourceClaimTemplate", claimTemplateKey.String())
	} else if !dryRun {
		if err := h.updateResourceClaimTemplateOwner(ctx, job, claimTemplateKey); err != nil {
			return err
		}
	}

	logger.Info("Successfully updated the ownership of combined resourceClaimTemplate",
		"ResourceClaimTemplate", claimTemplateKey.String())

	return nil
}

func (h *mutateHandle) updateMultiResourceClaimTemplates(
	ctx context.Context, job *vcv1alpha1.Job, task *vcv1alpha1.TaskSpec, infos common.ResourceInfos, dryRun bool,
) common.ResourceInfos {
	logger := log.FromContext(ctx)

	var updatedInfos common.ResourceInfos
	for i, info := range infos {
		claimTemplateKey := types.NamespacedName{Name: info.ClaimName, Namespace: job.Namespace}
		if !slices.ContainsFunc(task.Template.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
			return claim.ResourceClaimTemplateName != nil && *claim.ResourceClaimTemplateName == info.ClaimName
		}) {
			logger.V(1).Info("ResourceClaimTemplateName for container not found, skip update",
				"container", info.Name, "ResourceClaimTemplate", claimTemplateKey)
		} else if !dryRun {
			if err := h.updateResourceClaimTemplateOwner(ctx, job, claimTemplateKey); err != nil {
				updatedInfos = append(updatedInfos, infos[i])
			}
		}
	}
	if len(updatedInfos) == 0 {
		logger.Info("Successfully updated the ownership of all resourceClaimTemplates")
	}

	return updatedInfos
}

func (h *mutateHandle) getResourceClaimTemplate(ctx context.Context, key client.ObjectKey, obj *resourceapi.ResourceClaimTemplate) error {
	if h.reader == nil {
		return h.client.Get(ctx, key, obj)
	}
	return h.reader.GetResourceClaimTemplate(ctx, key, obj)
}

func (h *mutateHandle) mutation(obj client.Object) {
	if h.reader == nil {
		return
	}
	h.reader.Mutation(obj)
}

func (h *mutateHandle) updateResourceClaimTemplateOwner(ctx context.Context, owner metav1.Object, claimKey types.NamespacedName) error {
	logger := log.FromContext(ctx).WithValues("ResourceClaimTemplate", claimKey.String())
	claimTemplate := &resourceapi.ResourceClaimTemplate{}

	if err := h.getResourceClaimTemplate(ctx, claimKey, claimTemplate); err != nil {
		logger.Error(err, "get resourceClaim failed")
		return client.IgnoreNotFound(err)
	}

	if !controllerutil.HasControllerReference(claimTemplate) {
		if err := controllerutil.SetControllerReference(owner, claimTemplate, h.client.Scheme()); err != nil {
			logger.Error(err, "SetControllerReference failed")
			return err
		}
		if err := h.client.Update(ctx, claimTemplate); err != nil {
			logger.Error(err, "update resourceClaimTemplate ownerReference failed")
			return err
		}
		h.mutation(claimTemplate)
	} else {
		logger.V(3).Info("resourceClaimTemplate already has a controller reference, skip updating")
	}
	return nil
}

func (h *mutateHandle) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("operation", req.Operation)
	logger.V(4).Info("into volcano job mutate handle")

	dryrun := req.DryRun != nil && *req.DryRun
	if dryrun {
		logger = logger.WithValues("dryRun", true)
	}
	var err error
	job := &vcv1alpha1.Job{}
	ctx = log.IntoContext(ctx, logger)
	switch req.Operation {
	case admissionv1.Create:
		if err = h.decoder.Decode(req, job); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.MutateCreate(ctx, job, dryrun)
	case admissionv1.Update:
		if err = h.decoder.Decode(req, job); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.MutateUpdate(ctx, job, dryrun)
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
	marshalled, err := json.Marshal(job)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshalled)
}
