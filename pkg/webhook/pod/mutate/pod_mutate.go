package mutate

import (
	"context"
	"net/http"
	"strings"

	"github.com/coldzerofear/vgpu-manager/cmd/webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/controller/reschedule"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/json"
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

func setDefaultSchedulerName(pod *corev1.Pod, options *options.Options, logger logr.Logger) {
	if len(options.SchedulerName) > 0 && (pod.Spec.SchedulerName == "" || pod.Spec.SchedulerName == "default-scheduler") {
		pod.Spec.SchedulerName = options.SchedulerName
		logger.V(4).Info("Successfully set schedulerName", "schedulerName", options.SchedulerName)
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
	// Setting topology mode only makes sense when requesting multiple GPUs.
	if IsSingleContainerMultiGPUs(pod) {
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

func cleanupMetadata(pod *corev1.Pod) {
	// Cleaning metadata to prevent impact on scheduling.
	reschedule.CleanupMetadata(pod)
	// Clean up invalid scheduling policy annotations.
	if !util.IsVGPUResourcePod(pod) {
		if _, ok := util.HasAnnotation(pod, util.NodeSchedulerPolicyAnnotation); ok {
			delete(pod.Annotations, util.NodeSchedulerPolicyAnnotation)
		}
		if _, ok := util.HasAnnotation(pod, util.DeviceSchedulerPolicyAnnotation); ok {
			delete(pod.Annotations, util.DeviceSchedulerPolicyAnnotation)
		}
	}
	// Clean up invalid topology mode annotations.
	if !IsSingleContainerMultiGPUs(pod) {
		if _, ok := util.HasAnnotation(pod, util.DeviceTopologyModeAnnotation); ok {
			delete(pod.Annotations, util.DeviceTopologyModeAnnotation)
		}
	}
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
	// Clean up some useless metadata.
	cleanupMetadata(pod)
	if util.IsVGPUResourcePod(pod) {
		setDefaultSchedulerName(pod, h.options, logger)
		setDefaultNodeSchedulerPolicy(pod, h.options, logger)
		setDefaultDeviceSchedulerPolicy(pod, h.options, logger)
		setDefaultDeviceTopologyMode(pod, h.options, logger)
		setDefaultRuntimeClassName(pod, h.options, logger)
		fixSpecifiedNodeName(pod, logger)
	}
	return nil
}

func IsSingleContainerMultiGPUs(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.Containers {
		if util.GetResourceOfContainer(&container, util.VGPUNumberResourceName) > 1 {
			return true
		}
	}
	return false
}

func (h *mutateHandle) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("operation", req.Operation)
	logger.V(5).Info("into pod mutate handle")
	ctx = log.IntoContext(ctx, logger)
	switch req.Operation {
	case admissionv1.Create:
		pod := &corev1.Pod{}
		if err := h.decoder.Decode(req, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		// Default the object
		if err := h.MutateCreate(ctx, pod); err != nil {
			//var apiStatus apierrors.APIStatus
			//if errors.As(err, &apiStatus) {
			//	return admission.Response{AdmissionResponse: admissionv1.AdmissionResponse{
			//		Allowed: false,
			//		Result:  ptr.To[metav1.Status](apiStatus.Status()),
			//	}}
			//}
			return admission.Denied(err.Error())
		}
		// Create the patch
		marshalled, err := json.Marshal(pod)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		return admission.PatchResponseFromRaw(req.Object.Raw, marshalled)
	default:
		// Always skip when a DELETE or UPDATE operation received in custom mutation handler.
		return admission.ValidationResponse(true, "")
	}
}
