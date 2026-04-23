package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/claimresolve"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/common"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

const Path = "/resourceclaim/validate"

func NewWebhookHandler(client client.Client, options *options.Options, reader resourcereader.ResourceAPIReader) (http.Handler, error) {
	if !options.DefaultConvertToDRA {
		return nil, nil
	}
	return &resourceClaimWebhook{
		options: options,
		scheme:  client.Scheme(),
		logger:  klog.NewKlogr(),
		reader:  reader,
		codecs:  serializer.NewCodecFactory(client.Scheme()),
	}, nil
}

type resourceClaimWebhook struct {
	options *options.Options
	scheme  *runtime.Scheme
	logger  logr.Logger
	reader  resourcereader.ResourceAPIReader
	codecs  serializer.CodecFactory
}

// serve handles the http portion of a request prior to handing to an admit
// function.
func (rw *resourceClaimWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			klog.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body = data
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		msg := fmt.Sprintf("contentType=%s, expected application/json", contentType)
		klog.Error(msg)
		http.Error(w, msg, http.StatusUnsupportedMediaType)
		return
	}

	klog.V(2).Infof("handling request: %s", body)

	requestedAdmissionReview, err := rw.readAdmissionReview(body)
	if err != nil {
		msg := fmt.Sprintf("failed to read AdmissionReview from request body: %v", err)
		klog.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	logger := admission.DefaultLogConstructor(rw.logger, &admission.Request{
		AdmissionRequest: *requestedAdmissionReview.Request},
	)
	logger = logger.WithValues("operation", requestedAdmissionReview.Request.Operation)
	logger.V(5).Info("into resourceClaim validate handle")
	ctx := log.IntoContext(r.Context(), logger)

	responseAdmissionReview := &admissionv1.AdmissionReview{}
	responseAdmissionReview.SetGroupVersionKind(requestedAdmissionReview.GroupVersionKind())
	responseAdmissionReview.Response = rw.admitResourceClaimParameters(ctx, *requestedAdmissionReview)
	responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID

	klog.V(2).Infof("sending response: %v", responseAdmissionReview)
	respBytes, err := json.Marshal(responseAdmissionReview)
	if err != nil {
		klog.Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		klog.Error(err)
	}
}

func (rw *resourceClaimWebhook) readAdmissionReview(data []byte) (*admissionv1.AdmissionReview, error) {
	deserializer := rw.codecs.UniversalDeserializer()
	obj, gvk, err := deserializer.Decode(data, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("request could not be decoded: %w", err)
	}

	if *gvk != admissionv1.SchemeGroupVersion.WithKind("AdmissionReview") {
		return nil, fmt.Errorf("unsupported group version kind: %v", gvk)
	}

	requestedAdmissionReview, ok := obj.(*admissionv1.AdmissionReview)
	if !ok {
		return nil, fmt.Errorf("expected v1.AdmissionReview but got: %T", obj)
	}

	return requestedAdmissionReview, nil
}

func buildClaimScopedRequestKey(claimUID, mainRequest string) string {
	return claimUID + "/" + mainRequest
}

func podID(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

func containerID(pod *corev1.Pod, containerName string) string {
	return pod.Namespace + "/" + pod.Name + "/" + containerName
}

func (rw *resourceClaimWebhook) isVGPUDeviceRequest(ctx context.Context, req resourceapi.DeviceRequest) bool {
	return common.DeviceRequestLooksLikeVGPU(ctx, rw.reader, req, rw.options.VGPUDeviceClassName)
}

func (rw *resourceClaimWebhook) isVGPUSubRequest(ctx context.Context, req resourceapi.DeviceSubRequest) bool {
	return common.SubRequestLooksLikeVGPU(ctx, rw.reader, req, rw.options.VGPUDeviceClassName)
}

func (rw *resourceClaimWebhook) getAllocatedVGPURequests(ctx context.Context, claim *resourceapi.ResourceClaim) sets.Set[string] {
	return claimresolve.GetAllocatedVGPURequests(ctx, claim, util.DRADriverName, rw.isVGPUDeviceRequest, rw.isVGPUSubRequest)
}

type actualRequestUsage struct {
	Pods           sets.Set[string]
	InitContainers sets.Set[string] // "<ns>/<pod>/<container>"
	AppContainers  sets.Set[string] // "<ns>/<pod>/<container>"
}

// resolveActualAllocatedRequestsForClaimRef：把一个容器对某个 claimRef 的引用，
// 展开成“当前这个实际 claim 已分配出来的 vgpu mainRequest”。
//
// 规则：
// - claimRef.Request != "": 如果该 mainRequest 实际已落成 vgpu，则只返回它
// - claimRef.Request == "": 返回该 claim 当前已分配出来的全部 vgpu mainRequest
// validateOneReservedPodAgainstAllocatedClaim：
// 对当前 claim 的一个 reserved pod 做最终裁决。
//
// 它做两件事：
// 1. 兜底检查“一个 container 最终实际命中的 vgpu claim 数 <= 1”
//   - 这是用来兜住 Pod webhook 阶段无法提前判定的 mixed FirstAvailable
//
// 2. 仅针对 currentClaim：
//   - app-app 不允许命中同一个 mainRequest
//   - init-init 不允许命中同一个 mainRequest
//   - init-app 允许
//   - 跨 Pod 不允许共享同一个 mainRequest
func (rw *resourceClaimWebhook) validateOneReservedPodAgainstAllocatedClaim(
	ctx context.Context,
	pod *corev1.Pod,
	currentClaim *resourceapi.ResourceClaim,
	usages map[string]actualRequestUsage,
	claimCache map[string]*resourceapi.ResourceClaim,
) error {
	allContainers := common.GetAllPodContainers(pod)

	for _, c := range allContainers {
		containerActualVGPUClaims := sets.New[string]()
		containerCurrentClaimReqs := sets.New[string]()

		for _, claimRef := range c.Claims {
			actualClaimName, ok, err := claimresolve.ResolveActualClaimNameForPodClaim(pod, claimRef.Name)
			if err != nil {
				return fmt.Errorf("resolve actual claim for pod %s/%s claimRef %q failed: %w",
					pod.Namespace, pod.Name, claimRef.Name, err)
			}
			if !ok {
				continue
			}

			actualClaim, err := rw.getClaimCached(ctx, pod.Namespace, actualClaimName, claimCache)
			if err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("get actual claim %s/%s failed: %w", pod.Namespace, actualClaimName, err)
			}

			actualAllocatedVGPUReqs := rw.getAllocatedVGPURequests(ctx, actualClaim)
			if actualAllocatedVGPUReqs.Len() == 0 {
				continue
			}

			// 这个 container 最终命中了哪个 claim 的 vGPU
			actualHitReqs := claimresolve.ResolveActualAllocatedRequestsForClaimRef(claimRef, actualAllocatedVGPUReqs)
			if len(actualHitReqs) > 0 {
				containerActualVGPUClaims.Insert(string(actualClaim.UID))
			}

			// 只累计当前 claim 的 usage
			if actualClaim.UID == currentClaim.UID {
				for _, mainReq := range actualHitReqs {
					containerCurrentClaimReqs.Insert(mainReq)
				}
			}
		}

		// 最终兜底：一个容器最多只能命中 1 个“实际已分配成 vGPU 的 claim”
		if containerActualVGPUClaims.Len() > 1 {
			return fmt.Errorf(
				"pod %s/%s %s %q uses multiple allocated vgpu claims %v; one container can use at most one vgpu claim",
				pod.Namespace, pod.Name, c.Kind, c.Name, sets.List(containerActualVGPUClaims),
			)
		}

		// 对当前 claim 的每个 mainRequest 做最终 usage 累积
		for _, mainReq := range sets.List(containerCurrentClaimReqs) {
			key := buildClaimScopedRequestKey(string(currentClaim.UID), mainReq)

			usage := usages[key]
			if usage.Pods == nil {
				usage.Pods = sets.New[string]()
			}
			if usage.InitContainers == nil {
				usage.InitContainers = sets.New[string]()
			}
			if usage.AppContainers == nil {
				usage.AppContainers = sets.New[string]()
			}

			thisPodID := podID(pod)
			thisContainerID := containerID(pod, c.Name)

			usage.Pods.Insert(thisPodID)
			switch c.Kind {
			case common.ContainerKindInit:
				usage.InitContainers.Insert(thisContainerID)
				if usage.InitContainers.Len() > 1 {
					return fmt.Errorf(
						"allocated vgpu request %q in claim %s/%s is referenced by multiple init containers %v",
						mainReq, currentClaim.Namespace, currentClaim.Name, sets.List(usage.InitContainers),
					)
				}
			case common.ContainerKindApp:
				usage.AppContainers.Insert(thisContainerID)
				if usage.AppContainers.Len() > 1 {
					return fmt.Errorf(
						"allocated vgpu request %q in claim %s/%s is referenced by multiple app containers %v",
						mainReq, currentClaim.Namespace, currentClaim.Name, sets.List(usage.AppContainers),
					)
				}
			default:
				return fmt.Errorf("unknown container kind %q for container %q", c.Kind, c.Name)
			}

			// 跨 Pod 一律不允许共享同一个 mainRequest
			if usage.Pods.Len() > 1 {
				return fmt.Errorf(
					"allocated vgpu request %q in claim %s/%s is shared by multiple pods %v",
					mainReq, currentClaim.Namespace, currentClaim.Name, sets.List(usage.Pods),
				)
			}

			usages[key] = usage
		}
	}

	return nil
}

func (rw *resourceClaimWebhook) getClaimCached(
	ctx context.Context,
	namespace, name string,
	cache map[string]*resourceapi.ResourceClaim,
) (*resourceapi.ResourceClaim, error) {
	cacheKey := namespace + "/" + name
	if obj, ok := cache[cacheKey]; ok {
		return obj, nil
	}

	var rc resourceapi.ResourceClaim
	if err := rw.reader.GetResourceClaim(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      name,
	}, &rc); err != nil {
		return nil, err
	}

	cache[cacheKey] = &rc
	return &rc, nil
}

// validateAllocatedVGPUSharing：claim/status webhook 的最终入口。
// 建议在 ResourceClaim /status UPDATE 校验里调用它。
func (rw *resourceClaimWebhook) validateAllocatedVGPUSharing(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
) error {
	if claim == nil || claim.Status.Allocation == nil {
		return nil
	}

	allocatedVGPUReqs := rw.getAllocatedVGPURequests(ctx, claim)
	if allocatedVGPUReqs.Len() == 0 {
		return nil
	}

	reservedPods, err := claimresolve.GetReservedPods(ctx, rw.reader, claim)
	if err != nil {
		return err
	}

	usages := map[string]actualRequestUsage{}
	// "ns/name" -> claim
	claimCache := map[string]*resourceapi.ResourceClaim{
		claim.Namespace + "/" + claim.Name: claim,
	}

	for _, pod := range reservedPods {
		if err = rw.validateOneReservedPodAgainstAllocatedClaim(ctx, pod, claim, usages, claimCache); err != nil {
			return err
		}
	}

	return nil
}

func (rw *resourceClaimWebhook) isVGPUDeviceResult(
	ctx context.Context, claim *resourceapi.ResourceClaim,
	result resourceapi.DeviceRequestAllocationResult,
) bool {
	if result.Driver != util.DRADriverName {
		return false
	}
	index := claimresolve.BuildAllocatedResultIndex(ctx, claim, rw.isVGPUDeviceRequest, rw.isVGPUSubRequest)
	meta, ok := index[result.Request]
	return ok && meta.IsVGPU
}

// admitResourceClaimParameters accepts both ResourceClaims and ResourceClaimTemplates and validates their
// opaque device configuration parameters for this driver.
func (rw *resourceClaimWebhook) admitResourceClaimParameters(ctx context.Context, ar admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	logger := log.FromContext(ctx)
	logger.V(2).Info("admitting resource claim parameters")

	var deviceConfigs []resourceapi.DeviceClaimConfiguration
	var specPath string

	switch ar.Request.Resource {
	case resourceClaimResourceV1, resourceClaimResourceV1Beta1, resourceClaimResourceV1Beta2:
		claim, err := rw.extractResourceClaim(ar, ar.Request.Object.Raw)
		if err != nil {
			logger.Error(err, "")
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
					Reason:  metav1.StatusReasonBadRequest,
				},
			}
		}

		if ar.Request.Operation == admissionv1.Update && ar.Request.SubResource == "status" {
			err = rw.validateAllocatedVGPUSharing(ctx, claim)
			if err != nil {
				logger.Error(err, "")
				return &admissionv1.AdmissionResponse{
					Result: &metav1.Status{
						Message: err.Error(),
						Reason:  metav1.StatusReasonInvalid,
					},
				}
			}
		}

		deviceConfigs = claim.Spec.Devices.Config
		specPath = "spec"
	case resourceClaimTemplateResourceV1, resourceClaimTemplateResourceV1Beta1, resourceClaimTemplateResourceV1Beta2:
		claimTemplate, err := rw.extractResourceClaimTemplate(ar)
		if err != nil {
			klog.Error(err)
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
					Reason:  metav1.StatusReasonBadRequest,
				},
			}
		}
		deviceConfigs = claimTemplate.Spec.Spec.Devices.Config
		specPath = "spec.spec"
	default:
		msg := fmt.Sprintf("expected resource to be one of the supported versions for resourceclaims or resourceclaimtemplates, got %s", ar.Request.Resource)
		logger.Error(nil, msg)
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: msg,
				Reason:  metav1.StatusReasonBadRequest,
			},
		}
	}

	var errs []error
	for configIndex, config := range deviceConfigs {
		if config.Opaque == nil || config.Opaque.Driver != util.DRADriverName {
			continue
		}

		fieldPath := fmt.Sprintf("%s.devices.config[%d].opaque.parameters", specPath, configIndex)
		// Strict-decode: do not allow for users to provide unknown fields.
		decodedConfig, err := runtime.Decode(nvapi.StrictDecoder, config.Opaque.Parameters.Raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("error decoding object at %s: %w", fieldPath, err))
			continue
		}

		// Cast the opaque config to a nvapi.Interface type and validate it
		var configInterface nvapi.Interface
		switch castConfig := decodedConfig.(type) {
		case *nvapi.GpuConfig:
			configInterface = castConfig
		case *nvapi.MigDeviceConfig:
			configInterface = castConfig
		case *nvapi.ComputeDomainChannelConfig:
			configInterface = castConfig
		case *nvapi.ComputeDomainDaemonConfig:
			configInterface = castConfig
		default:
			errs = append(errs, fmt.Errorf("expected a recognized configuration type at %s but got: %T", fieldPath, decodedConfig))
			continue
		}

		// Normalize the config to set any implied defaults
		if err := configInterface.Normalize(); err != nil {
			errs = append(errs, fmt.Errorf("error normalizing config at %s: %w", fieldPath, err))
			continue
		}

		// Validate the config to ensure its integrity
		if err := configInterface.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("object at %s is invalid: %w", fieldPath, err))
		}
	}

	if len(errs) > 0 {
		var errMsgs []string
		for _, err := range errs {
			errMsgs = append(errMsgs, err.Error())
		}
		msg := fmt.Sprintf("%d configs failed to validate: %s", len(errs), strings.Join(errMsgs, "; "))
		logger.Error(nil, msg)
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: msg,
				Reason:  metav1.StatusReasonInvalid,
			},
		}
	}

	return &admissionv1.AdmissionResponse{
		Allowed: true,
	}
}
