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
	"strings"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/common"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	vcv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

const Path = "/volcano-jobs/mutate"

func NewMutateWebhook(
	client client.Client, options *options.Options,
	_ resourcereader.ResourceAPIReader,
	_ events.EventRecorderLogger,
) (http.Handler, error) {
	return &admission.Webhook{
		Handler: &mutateHandle{
			decoder: admission.NewDecoder(client.Scheme()),
			options: options,
		},
		RecoverPanic: ptr.To[bool](true),
	}, nil
}

type mutateHandle struct {
	decoder admission.Decoder
	options *options.Options
}

func (h *mutateHandle) MutateCreate(ctx context.Context, job *vcv1alpha1.Job) error {
	logger := log.FromContext(ctx)
	for i := range job.Spec.Tasks {
		task := &job.Spec.Tasks[i]
		logger = logger.WithValues("taskName", task.Name)

		resourceName := job.Name
		if job.GenerateName != "" {
			resourceName = fmt.Sprintf("%s-%s-%s",
				strings.TrimSuffix(job.GenerateName, "-"),
				task.Name, common.GenerateRandomString(5))
		} else if h.options.CombinedResourceClaim {
			resourceName = fmt.Sprintf("%s-%s-%s", resourceName,
				task.Name, common.GenerateRandomString(5))
		}

		if err := common.ConvertDRARequest(
			log.IntoContext(ctx, logger),
			&task.Template.ObjectMeta,
			&task.Template.Spec,
			resourceName, h.options); err != nil {
			return err
		}
	}
	return nil
}

func (h *mutateHandle) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("operation", req.Operation)
	logger.V(4).Info("into volcano job mutate handle")

	var err error
	job := &vcv1alpha1.Job{}
	ctx = log.IntoContext(ctx, logger)
	switch req.Operation {
	case admissionv1.Create:
		if err = h.decoder.Decode(req, job); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.MutateCreate(ctx, job)
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
