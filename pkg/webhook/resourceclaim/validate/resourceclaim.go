package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/claimresolve"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/common"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

const Path = "/resourceclaim/validate"

func NewValidateWebhook(client client.Client, options *options.Options,
	reader resourcereader.ResourceAPIReader, recorder events.EventRecorderLogger,
) (http.Handler, error) {
	if !options.DefaultConvertToDRA {
		return nil, nil
	}
	return &resourceClaimWebhook{
		options:  options,
		scheme:   client.Scheme(),
		logger:   klog.NewKlogr(),
		reader:   reader,
		recorder: recorder,
		codecs:   serializer.NewCodecFactory(client.Scheme()),
	}, nil
}

type resourceClaimWebhook struct {
	options  *options.Options
	scheme   *runtime.Scheme
	logger   logr.Logger
	reader   resourcereader.ResourceAPIReader
	recorder events.EventRecorderLogger
	codecs   serializer.CodecFactory
}

// serve handles the http portion of a request prior to handing to an admit
// function.
func (rw *resourceClaimWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := rw.logger
	var body []byte
	if r.Body != nil {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			logger.Error(err, "failed to read body")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body = data
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		msg := fmt.Sprintf("contentType=%s, expected application/json", contentType)
		logger.Error(nil, msg)
		http.Error(w, msg, http.StatusUnsupportedMediaType)
		return
	}

	logger.V(5).Info(fmt.Sprintf("handling request: %s", body))

	requestedAdmissionReview, err := rw.readAdmissionReview(body)
	if err != nil {
		msg := fmt.Sprintf("failed to read AdmissionReview from request body: %v", err)
		logger.Error(nil, msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	logger = admission.DefaultLogConstructor(logger, &admission.Request{
		AdmissionRequest: *requestedAdmissionReview.Request,
	})
	logger = logger.WithValues("operation", requestedAdmissionReview.Request.Operation)
	logger.V(4).Info("into resourceClaim validate handle")
	ctx := log.IntoContext(r.Context(), logger)

	responseAdmissionReview := &admissionv1.AdmissionReview{}
	responseAdmissionReview.SetGroupVersionKind(requestedAdmissionReview.GroupVersionKind())
	responseAdmissionReview.Response = rw.admitResourceClaimParameters(ctx, *requestedAdmissionReview)
	responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID

	logger.V(5).Info(fmt.Sprintf("sending response: %v", responseAdmissionReview))
	respBytes, err := json.Marshal(responseAdmissionReview)
	if err != nil {
		logger.Error(err, "responseAdmissionReview marshal failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		logger.Error(err, "Write response body failed")
	}
}

var (
	admissionReviewV1GVK      = admissionv1.SchemeGroupVersion.WithKind("AdmissionReview")
	admissionReviewV1beta1GVK = admissionv1beta1.SchemeGroupVersion.WithKind("AdmissionReview")
)

func (rw *resourceClaimWebhook) readAdmissionReview(data []byte) (*admissionv1.AdmissionReview, error) {
	deserializer := rw.codecs.UniversalDeserializer()
	obj, gvk, err := deserializer.Decode(data, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("request could not be decoded: %w", err)
	}

	var requestedAdmissionReview *admissionv1.AdmissionReview

	switch *gvk {
	case admissionReviewV1GVK:
		admissionReview, ok := obj.(*admissionv1.AdmissionReview)
		if !ok {
			return nil, fmt.Errorf("expected v1.AdmissionReview but got: %T", obj)
		}
		requestedAdmissionReview = admissionReview
	case admissionReviewV1beta1GVK:
		admissionReview, ok := obj.(*admissionv1beta1.AdmissionReview)
		if !ok {
			return nil, fmt.Errorf("expected v1.AdmissionReview but got: %T", obj)
		}
		requestedAdmissionReview = &admissionv1.AdmissionReview{}
		requestedAdmissionReview.TypeMeta = admissionReview.TypeMeta
		// Both v1 and v1beta1 AdmissionReview types are exactly the same, so directly convert the pointer to v1.
		if admissionReview.Request != nil {
			marshal, err := json.Marshal(admissionReview.Request)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal AdmissionReview request: %v", err)
			}
			requestedAdmissionReview.Request = &admissionv1.AdmissionRequest{}
			err = json.Unmarshal(marshal, &requestedAdmissionReview.Request)
			if err != nil {
				return nil, fmt.Errorf("failed to unmarshal AdmissionReview request: %v", err)
			}
		}
		if admissionReview.Response != nil {
			marshal, err := json.Marshal(admissionReview.Response)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal AdmissionReview response: %v", err)
			}
			requestedAdmissionReview.Response = &admissionv1.AdmissionResponse{}
			err = json.Unmarshal(marshal, &requestedAdmissionReview.Response)
			if err != nil {
				return nil, fmt.Errorf("failed to unmarshal AdmissionReview response: %v", err)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported group version kind: %v", gvk)
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

func (rw *resourceClaimWebhook) isVGPUDeviceRequest(ctx context.Context, matchedDriver bool, req resourceapi.DeviceRequest) bool {
	return common.DeviceRequestLooksLikeVGPU(ctx, rw.reader, req, matchedDriver, rw.options.VGPUDeviceClassName)
}

func (rw *resourceClaimWebhook) isVGPUSubRequest(ctx context.Context, matchedDriver bool, req resourceapi.DeviceSubRequest) bool {
	return common.SubRequestLooksLikeVGPU(ctx, rw.reader, req, matchedDriver, rw.options.VGPUDeviceClassName)
}

func (rw *resourceClaimWebhook) getAllocatedVGPURequests(ctx context.Context, claim *resourceapi.ResourceClaim) sets.Set[string] {
	return claimresolve.GetAllocatedVGPURequests(ctx, claim, util.DRADriverName,
		func(ctx context.Context, request resourceapi.DeviceRequest) bool {
			return rw.isVGPUDeviceRequest(ctx, true, request)
		}, func(ctx context.Context, request resourceapi.DeviceSubRequest) bool {
			return rw.isVGPUSubRequest(ctx, true, request)
		})
}

type actualRequestUsage struct {
	Pods           sets.Set[string]
	InitContainers sets.Set[string] // "<ns>/<pod>/<container>"
	AppContainers  sets.Set[string] // "<ns>/<pod>/<container>"
}

// validateOneReservedPodAgainstAllocatedClaim make the final decision on a reserved pod for the current claim.
// It does two things:
// 1. Bottom line check: The actual number of vGPU claims hit by a container is less than or equal to 1
//   - Used to verify mixed FirstAvailable that cannot be determined in advance during the Pod webhook phase
//
// 2. Only for the current claim:
//   - app-app cannot hit the same mainRequest
//   - init-init cannot hit the same mainRequest
//   - init-app allow hitting the same mainRequest
//   - cross Pod sharing of the same mainRequest is not allowed
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

			// Accumulate the VGPU claims hit by this container
			actualHitReqs := claimresolve.ResolveActualAllocatedRequestsForClaimRef(claimRef, actualAllocatedVGPUReqs)
			if len(actualHitReqs) > 0 {
				containerActualVGPUClaims.Insert(string(actualClaim.UID))
			}

			// Only accumulate the usage of the current claim
			if actualClaim.UID == currentClaim.UID {
				for _, mainReq := range actualHitReqs {
					containerCurrentClaimReqs.Insert(mainReq)
				}
			}
		}

		// A container can only hit a maximum of 1 "claim that has actually been allocated to vGPU"
		if containerActualVGPUClaims.Len() > 1 {
			return fmt.Errorf(
				"pod %s/%s %s %q uses multiple allocated vgpu claims %v; one container can use at most one vgpu claim",
				pod.Namespace, pod.Name, c.Kind, c.Name, sets.List(containerActualVGPUClaims),
			)
		}

		// Perform final usage accumulation for each mainRequest of the current claim
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

			// Cross Pod sharing of the same mainRequest is strictly prohibited
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
	objKey := client.ObjectKey{
		Namespace: namespace,
		Name:      name,
	}
	if obj, ok := cache[objKey.String()]; ok {
		return obj, nil
	}

	var rc resourceapi.ResourceClaim
	if err := rw.reader.GetResourceClaim(ctx, objKey, &rc); err != nil {
		return nil, err
	}

	cache[objKey.String()] = &rc
	return &rc, nil
}

// validateAllocatedVGPUSharing The entrance to the claim/status webhook.
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
	claimKey := client.ObjectKey{
		Namespace: claim.Namespace,
		Name:      claim.Name,
	}
	claimCache := map[string]*resourceapi.ResourceClaim{
		claimKey.String(): claim,
	}

	for _, pod := range reservedPods {
		if err = rw.validateOneReservedPodAgainstAllocatedClaim(ctx, pod, claim, usages, claimCache); err != nil {
			return err
		}
	}

	return nil
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
			logger.Error(err, "extractResourceClaim failed")
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
					Reason:  metav1.StatusReasonBadRequest,
				},
			}
		}

		if ar.Request.Operation == admissionv1.Update && ar.Request.SubResource == "status" {
			if err = rw.validateAllocatedVGPUSharing(ctx, claim); err != nil {
				if rw.recorder != nil {
					rw.recorder.Eventf(claim, nil, corev1.EventTypeWarning, "AdmissionFailed", "Conflict", err.Error())
				}
				logger.Error(err, "validateAllocatedVGPUSharing failed")
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
			logger.Error(err, "extractResourceClaimTemplate failed")
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
