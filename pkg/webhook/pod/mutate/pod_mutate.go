package mutate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/controller/reschedule"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/filter"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/docker/go-units"
	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	return nil
}

// buildResourceClaim Build vGPU resource claims based on container requests.
func (h *mutateHandle) buildResourceClaim(pod *corev1.Pod, container *corev1.Container) *resourceapi.ResourceClaim {
	deviceCount := util.GetResourceOfContainer(container, util.VGPUNumberResourceName)
	capacityRequest := make(map[resourceapi.QualifiedName]resource.Quantity)
	if deviceCores := util.GetResourceOfContainer(container, util.VGPUCoreResourceName); deviceCores > 0 {
		capacityRequest[kubeletplugin.CoresResourceName] = *resource.NewQuantity(deviceCores, resource.DecimalSI)
	}
	if deviceMemory := util.GetResourceOfContainer(container, util.VGPUMemoryResourceName); deviceMemory > 0 {
		deviceMemory = deviceMemory * units.MiB
		capacityRequest[kubeletplugin.MemoryResourceName] = *resource.NewQuantity(deviceMemory, resource.BinarySI)
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
	deviceConstraints := []resourceapi.DeviceConstraint{{
		Requests:          []string{kubeletplugin.VGpuDeviceType},
		DistinctAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/uuid"),
	}}

	switch filter.PodUsedGPUTopologyMode(pod) {
	case util.LinkTopology:
	case util.NUMATopology:
		deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
			Requests:       []string{kubeletplugin.VGpuDeviceType},
			MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/numaNode"),
		})
	}

	resourceClaim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"vgpu-manager.io/owner-pod": pod.Name,
			},
			GenerateName: fmt.Sprintf("%s-%s-", pod.Name, container.Name),
			Namespace:    pod.Namespace,
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Constraints: deviceConstraints,
				Requests: []resourceapi.DeviceRequest{
					{
						Name: kubeletplugin.VGpuDeviceType,
						Exactly: &resourceapi.ExactDeviceRequest{
							DeviceClassName: util.ComponentName,
							AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
							Count:           deviceCount,
							Capacity: &resourceapi.CapacityRequirements{
								Requests: capacityRequest,
							},
							Selectors: deviceSelectors,
						},
					},
				},
			},
		},
	}

	return resourceClaim
}

// MutateToDRA Convert pod's extended resource requests into DRA requests
func (h *mutateHandle) MutateToDRA(ctx context.Context, pod *corev1.Pod) (err error) {
	logger := log.FromContext(ctx)
	var resourceClaims []types.NamespacedName
	defer func() {
		if err != nil && len(resourceClaims) > 0 {
			if delErr := h.client.DeleteAllOf(
				context.Background(),
				&resourceapi.ResourceClaim{},
				client.InNamespace(pod.Namespace),
				client.MatchingLabels{"vgpu-manager.io/owner-pod": pod.Name},
			); delErr != nil {
				logger.Error(delErr, "Failed to clear vGPU resourceClaims", "resourceClaims", resourceClaims)
			}
		}
	}()

	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if util.GetResourceOfContainer(container, util.VGPUNumberResourceName) == 0 {
			continue
		}
		// Create container resource claim
		resourceClaim := h.buildResourceClaim(pod, container)
		if err = h.client.Create(ctx, resourceClaim); err != nil {
			logger.Error(err, "Failed to create vGPU resourceClaim", "container", container.Name)
			return err
		}
		resourceClaims = append(resourceClaims, client.ObjectKeyFromObject(resourceClaim))
		logger.Info("Successfully created resourceClaim", "resourceClaim",
			klog.KObj(resourceClaim), "container", container.Name)
		// Convert container resource requests into DRA requests.
		util.DelResourceOfContainer(container, util.VGPUNumberResourceName)
		util.DelResourceOfContainer(container, util.VGPUCoreResourceName)
		util.DelResourceOfContainer(container, util.VGPUMemoryResourceName)
		container.Resources.Claims = append(container.Resources.Claims, corev1.ResourceClaim{
			Name: resourceClaim.Name,
		})
		pod.Spec.ResourceClaims = append(pod.Spec.ResourceClaims, corev1.PodResourceClaim{
			Name:              resourceClaim.Name,
			ResourceClaimName: &resourceClaim.Name,
		})
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
		if err == nil && h.options.DefaultConvertToDRA {
			err = h.MutateToDRA(ctx, pod)
		}
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
