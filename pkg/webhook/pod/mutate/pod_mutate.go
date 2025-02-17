package mutate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/coldzerofear/vgpu-manager/cmd/webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/controller/reschedule"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const Path = "/pods/mutate"

func NewMutateWebhook(scheme *runtime.Scheme, options *options.Options) *admission.Webhook {
	return &admission.Webhook{
		Handler: &mutateHandle{
			decoder: admission.NewDecoder(scheme),
			options: options,
		},
		RecoverPanic: ptr.To[bool](true),
	}
}

type mutateHandle struct {
	decoder admission.Decoder
	options *options.Options
}

func needVGPUNumber(container *corev1.Container) bool {
	return util.GetResourceOfContainer(container, util.VGPUCoreResourceName) > 0 ||
		util.GetResourceOfContainer(container, util.VGPUMemoryResourceName) > 0
}

func (h *mutateHandle) MutateCreate(ctx context.Context, pod *corev1.Pod) error {
	logger := log.FromContext(ctx)
	for i, container := range pod.Spec.Containers {
		if util.IsVGPURequiredContainer(&container) {
			continue
		}
		// default 1 gpu
		if needVGPUNumber(&container) {
			pod.Spec.Containers[i].Resources.Limits[util.VGPUNumberResourceName] = resource.MustParse("1")
			logger.V(4).Info("Successfully set 1 vGPU number", "containerName", container.Name)
		}
	}

	if util.IsVGPUResourcePod(pod) {
		// Cleaning metadata to prevent impact on scheduling
		reschedule.CleanupMetadata(pod)
		if len(h.options.SchedulerName) > 0 &&
			(pod.Spec.SchedulerName == "" || pod.Spec.SchedulerName == "default-scheduler") {
			pod.Spec.SchedulerName = h.options.SchedulerName
			logger.V(4).Info("Successfully set schedulerName", "schedulerName", h.options.SchedulerName)
		}
	}
	return nil
}

func (h *mutateHandle) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx)
	logger.WithValues("operation", req.Operation)
	logger.V(5).Info("into pod mutate handle")
	ctx = log.IntoContext(ctx, logger)
	pod := &corev1.Pod{}
	switch req.Operation {
	case admissionv1.Create:
		if err := h.decoder.Decode(req, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		// Default the object
		if err := h.MutateCreate(ctx, pod); err != nil {
			var apiStatus apierrors.APIStatus
			if errors.As(err, &apiStatus) {
				return admission.Response{
					AdmissionResponse: admissionv1.AdmissionResponse{
						Allowed: false,
						Result:  ptr.To[metav1.Status](apiStatus.Status()),
					}}
			}
			return admission.Denied(err.Error())
		}
	default:
		// Always skip when a DELETE operation received in custom mutation handler.
		return admission.Response{
			AdmissionResponse: admissionv1.AdmissionResponse{
				Allowed: true,
				Result:  &metav1.Status{Code: http.StatusOK},
			}}
	}

	// Create the patch
	marshalled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshalled)
}
