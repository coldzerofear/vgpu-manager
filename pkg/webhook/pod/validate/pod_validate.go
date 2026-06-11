package validate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/claimresolve"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device/allocator"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/common"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	"github.com/docker/go-units"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/events"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const Path = "/pods/validate"

func NewValidateWebhook(client client.Client, options *options.Options,
	reader resourcereader.ResourceAPIReader, _ events.EventRecorderLogger,
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

func (h *validateHandle) ValidateCreate(ctx context.Context, pod *corev1.Pod) error {
	if h.options.DRAAdmissionEnabled {
		if err := h.checkResourceClaimRequests(ctx, pod); err != nil {
			return &apierrors.StatusError{
				ErrStatus: metav1.Status{
					Message: err.Error(),
					Reason:  metav1.StatusReasonInvalid,
				},
			}
		}
		if err := h.checkCrossPodsVGPURequestConflict(ctx, pod); err != nil {
			return &apierrors.StatusError{
				ErrStatus: metav1.Status{
					Message: err.Error(),
					Reason:  metav1.StatusReasonInvalid,
				},
			}
		}
	}
	if h.options.DefaultConvertToDRA {
		if err := h.createResourceClaims(ctx, pod); err != nil {
			return err
		}
	}
	return nil
}

// buildPodRequestIndex Establish a decidable index for Pod stage based on the 'main request'
// - Exactly: Directly determine whether it is defined as a vGPU request
// - FirstAvailable:
//   - All of them are vGPU -> definite vgpu
//   - None of them are vGPU -> non-vgpu
//   - Mixed with vGPU -> mixed-maybe-vgpu, No final conflict judgment is made during the Pod stage
func (h *validateHandle) buildPodRequestIndex(ctx context.Context, requests []resourceapi.DeviceRequest) map[string]podRequestMeta {
	index := make(map[string]podRequestMeta, len(requests))

	for _, req := range requests {
		switch {
		case req.Exactly != nil:
			class := common.MainRequestNonVGPU
			if h.isVGPUDeviceRequest(ctx, req) {
				class = common.MainRequestDefVGPU
			}
			index[req.Name] = podRequestMeta{
				MainRequest: req.Name,
				Class:       class,
			}

		case len(req.FirstAvailable) > 0:
			vgpuCount := 0
			for _, subReq := range req.FirstAvailable {
				if h.isVGPUSubRequest(ctx, subReq) {
					vgpuCount++
				}
			}

			class := common.MainRequestNonVGPU
			switch {
			case vgpuCount == 0:
				class = common.MainRequestNonVGPU
			case vgpuCount == len(req.FirstAvailable):
				class = common.MainRequestDefVGPU
			default:
				class = common.MainRequestMixedMaybe
			}

			index[req.Name] = podRequestMeta{
				MainRequest: req.Name,
				Class:       class,
			}
		}
	}

	return index
}

type requestUsage struct {
	InitContainers sets.Set[string]
	AppContainers  sets.Set[string]
}

type podRequestMeta struct {
	MainRequest string
	Class       common.MainRequestClass
}

type resourceInfoMap struct {
	contClaimReqMap map[string]map[string]sets.Set[string]
}

func newResourceInfoCache() *resourceInfoMap {
	return &resourceInfoMap{
		contClaimReqMap: make(map[string]map[string]sets.Set[string]),
	}
}

func (m *resourceInfoMap) GetReqSet(contName, claimName string) (sets.Set[string], bool) {
	claimReqMap, ok := m.contClaimReqMap[contName]
	if !ok {
		return nil, false
	}
	set, ok := claimReqMap[claimName]
	return set, ok
}

func (m *resourceInfoMap) Insert(contName, claimName, request string) {
	claimReqMap, ok := m.contClaimReqMap[contName]
	if !ok {
		claimReqMap = make(map[string]sets.Set[string])
		m.contClaimReqMap[contName] = claimReqMap
	}

	set, ok := claimReqMap[claimName]
	if !ok {
		set = sets.New[string]()
	}
	set.Insert(request)
	claimReqMap[claimName] = set
}

type deviceRequestCacheEntry struct {
	requests []resourceapi.DeviceRequest
	err      error
}

func (h *validateHandle) getConvertedContainerClaimsMap(pod *corev1.Pod) *resourceInfoMap {
	cache := newResourceInfoCache()
	val, ok := util.HasAnnotation(pod, util.DRAOriResAnnotation)
	if !ok || len(val) == 0 {
		return cache
	}
	infos := common.ResourceInfos{}
	if err := infos.Decode(val); err != nil {
		return cache
	}
	if len(infos) == 0 {
		return cache
	}

	resourceName := pod.Name
	if h.options.CombinedResourceClaim {
		resourceName, _ = util.HasAnnotation(pod, util.DRAGenNameAnnotation)
		resourceClaimName := util.GenerateK8sSafeResourceName(resourceName)
		for _, resourceInfo := range infos {
			resourceRequestName := util.GenerateK8sSafeResourceName(resourceInfo.Name, kubeletplugin.VGpuDeviceType)
			cache.Insert(resourceInfo.Name, resourceClaimName, resourceRequestName)
		}
	} else {
		if pod.GenerateName != "" {
			resourceName, _ = util.HasAnnotation(pod, util.DRAGenNameAnnotation)
		}
		for _, resourceInfo := range infos {
			resourceClaimName := util.GenerateK8sSafeResourceName(resourceName, resourceInfo.Name)
			cache.Insert(resourceInfo.Name, resourceClaimName, kubeletplugin.VGpuDeviceType)
		}
	}
	return cache
}

// checkResourceClaimRequests：Pod stage verification rules (within a single pod)
// 1. A container can only hit a maximum of 1 definite-vgpu claim
// 2. A container can hit multiple definite-vgpu requests under the same claim
// 3. init-init Do not allow overlapping with mainRequest
// 4. app-app Do not allow overlapping with mainRequest
// 5. init-app Allow overlap with mainRequest
// 6. mixed FirstAvailable Don't verify for now, leave it to the claim webhook
// 7. init-app cross-overlap: an init container may only overlap with at most one app container
//
// Cross-pod rule (enforced separately in checkCrossPodsVGPURequestConflict):
// 8. cross-pod: two pods must not use the same definite-vgpu mainRequest from a shared named claim
func (h *validateHandle) checkResourceClaimRequests(ctx context.Context, pod *corev1.Pod) error {
	// fast return
	if !util.HasDRARequests(pod) {
		return nil
	}

	allContainers := util.GetAllPodContainers(pod)
	claimsMap := h.getConvertedContainerClaimsMap(pod)

	// key format: "<podClaimName>/<mainRequest>"
	usages := map[string]requestUsage{}
	requestCache := map[string]deviceRequestCacheEntry{}

	for _, c := range allContainers {
		containerVGPUClaims := sets.New[string]()
		containerResolvedReqKeys := sets.New[string]()

		for _, claimRef := range c.Claims {
			var requestKeys []string

			// Special processing should be applied to the resource Claims requests converted from webhooks, as these resources have not yet been created at this time.
			// No need to determine the type of vGPU (it must be vGPU), just collect requestKeys.
			if requestSet, ok := claimsMap.GetReqSet(c.Name, claimRef.Name); ok {
				if claimRef.Request != "" {
					requestKeys = []string{buildVGPURequestKey(claimRef.Name, claimRef.Request)}
				} else {
					for request, _ := range requestSet {
						requestKeys = append(requestKeys, buildVGPURequestKey(claimRef.Name, request))
					}
				}
				slices.Sort(requestKeys)
			} else {
				var err error
				requestKeys, err = h.resolveDefiniteVGPURequestsFromContainerClaim(ctx, pod, claimRef, requestCache)
				if err != nil {
					return fmt.Errorf("%s container %q claim %q validation failed: %w", c.Kind, c.Name, claimRef.Name, err)
				}
			}

			if len(requestKeys) > 0 {
				containerVGPUClaims.Insert(claimRef.Name)
			}
			for _, reqKey := range requestKeys {
				containerResolvedReqKeys.Insert(reqKey)
			}
		}

		// A container can hit a maximum of 1 vGPU claim
		if containerVGPUClaims.Len() > 1 {
			return fmt.Errorf(
				"%s container %q references multiple vgpu claims %v; one container can use at most one vgpu claim",
				c.Kind, c.Name, sets.List(containerVGPUClaims),
			)
		}

		// Conflict Matrix
		for _, reqKey := range sets.List(containerResolvedReqKeys) {
			usage := usages[reqKey]
			if usage.InitContainers == nil {
				usage.InitContainers = sets.New[string]()
			}
			if usage.AppContainers == nil {
				usage.AppContainers = sets.New[string]()
			}

			// A sidecar (restartable init) runs concurrently with the app
			// containers, so for vGPU-request sharing it must be treated like an
			// app container: it does NOT get the init/app cross-phase reuse
			// allowance. Only a non-restartable init container, which completes
			// before the app phase, may share a request with an app container.
			kind := c.Kind
			if c.Restartable {
				kind = util.ContainerKindApp
			}
			switch kind {
			case util.ContainerKindInit:
				// Non-restartable init containers run strictly sequentially and
				// never overlap each other or the app phase, so any number of
				// them may share (sequentially reuse) the same vGPU request.
				usage.InitContainers.Insert(c.Name)
			case util.ContainerKindApp:
				usage.AppContainers.Insert(c.Name)
				if usage.AppContainers.Len() > 1 {
					return fmt.Errorf(
						"vgpu request %q is referenced by multiple concurrent containers %v; "+
							"app containers (and sidecars) run concurrently and cannot share a vgpu request",
						reqKey, sets.List(usage.AppContainers),
					)
				}
			default:
				return fmt.Errorf("unknown container kind %q for container %q", c.Kind, c.Name)
			}

			usages[reqKey] = usage
		}
	}

	// Rule 7: init-app cross-overlap prohibition.
	// For every request key that has both an init and an app container,
	// record which app containers each init container overlaps with.
	// If any single init container ends up overlapping with more than one
	// distinct app container, the pod is invalid.
	initAppOverlap := map[string]sets.Set[string]{}
	for _, usage := range usages {
		if usage.InitContainers.Len() == 0 || usage.AppContainers.Len() == 0 {
			continue
		}
		for _, initName := range sets.List(usage.InitContainers) {
			if initAppOverlap[initName] == nil {
				initAppOverlap[initName] = sets.New[string]()
			}
			initAppOverlap[initName].Insert(sets.List(usage.AppContainers)...)
		}
	}
	for initName, appConts := range initAppOverlap {
		if appConts.Len() > 1 {
			return fmt.Errorf(
				"init container %q overlaps vgpu requests with multiple app containers %v; "+
					"an init container may only share vgpu requests with at most one app container",
				initName, sets.List(appConts),
			)
		}
	}

	return nil
}

// resolveDefiniteVGPURequestsFromContainerClaim Only return vGPU main requests that can be confirmed during the Pod stage.
// rules：
// - claimRef.Request != "": Only check this main request
// - claimRef.Request == "": Return all vGPU main requests defined under this claim
// - mixed FirstAvailable: Do not check, leave it for resource claim webhook verification
func (h *validateHandle) resolveDefiniteVGPURequestsFromContainerClaim(
	ctx context.Context,
	pod *corev1.Pod,
	claimRef corev1.ResourceClaim,
	requestCache map[string]deviceRequestCacheEntry,
) ([]string, error) {
	podClaim, err := common.FindPodResourceClaim(pod, claimRef.Name)
	if err != nil {
		return nil, err
	}

	deviceRequests, err := h.getDeviceRequestsForPodClaimCached(ctx, pod.Namespace, podClaim, requestCache)
	if err != nil {
		// skip not found resource
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	index := h.buildPodRequestIndex(ctx, deviceRequests)

	// Explicitly specify request: only check this request
	if claimRef.Request != "" {
		meta, ok := index[claimRef.Request]
		if !ok {
			return nil, fmt.Errorf("request %q not found in pod resourceClaim %q", claimRef.Request, claimRef.Name)
		}
		if meta.Class != common.MainRequestDefVGPU {
			// Non vGPU or mixed, both left for claim webhook to check
			return nil, nil
		}
		return []string{buildVGPURequestKey(claimRef.Name, meta.MainRequest)}, nil
	}

	// claimRef.Request is empty: Expand all define vgpu main requests under this claim
	var result []string
	for _, meta := range index {
		if meta.Class == common.MainRequestDefVGPU {
			result = append(result, buildVGPURequestKey(claimRef.Name, meta.MainRequest))
		}
	}
	slices.Sort(result)
	return result, nil
}

func (h *validateHandle) getDeviceRequestsForPodClaimCached(
	ctx context.Context,
	namespace string,
	podClaim *corev1.PodResourceClaim,
	requestCache map[string]deviceRequestCacheEntry,
) ([]resourceapi.DeviceRequest, error) {
	cacheKey := namespace + "/" + podClaim.Name
	if cacheEntry, ok := requestCache[cacheKey]; ok {
		return cacheEntry.requests, cacheEntry.err
	}
	requests, err := h.getDeviceRequestsForPodClaim(ctx, namespace, podClaim)
	requestCache[cacheKey] = deviceRequestCacheEntry{requests: requests, err: err}
	return requests, err
}

// getDeviceRequestsForPodClaim loads device requests from either:
// - spec.resourceClaims[].resourceClaimName
// - spec.resourceClaims[].resourceClaimTemplateName
func (h *validateHandle) getDeviceRequestsForPodClaim(
	ctx context.Context,
	namespace string,
	podClaim *corev1.PodResourceClaim,
) ([]resourceapi.DeviceRequest, error) {
	if h.reader != nil {
		return h.reader.GetDeviceRequestsForPodClaim(ctx, namespace, podClaim)
	}

	if podClaim == nil {
		return nil, fmt.Errorf("podClaim is nil")
	}

	if podClaim.ResourceClaimName != nil && *podClaim.ResourceClaimName != "" {
		var rc resourceapi.ResourceClaim
		if err := h.client.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      *podClaim.ResourceClaimName,
		}, &rc); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, err
			}
			return nil, fmt.Errorf("get ResourceClaim %q failed: %w", *podClaim.ResourceClaimName, err)
		}
		return rc.Spec.Devices.Requests, nil
	}

	if podClaim.ResourceClaimTemplateName != nil && *podClaim.ResourceClaimTemplateName != "" {
		var tpl resourceapi.ResourceClaimTemplate
		if err := h.client.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      *podClaim.ResourceClaimTemplateName,
		}, &tpl); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, err
			}
			return nil, fmt.Errorf("get ResourceClaimTemplate %q failed: %w", *podClaim.ResourceClaimTemplateName, err)
		}
		return tpl.Spec.Spec.Devices.Requests, nil
	}

	return nil, fmt.Errorf(
		"pod resourceClaim %q must specify one of resourceClaimName or resourceClaimTemplateName",
		podClaim.Name,
	)
}

func (h *validateHandle) mutation(obj client.Object) {
	if h.reader == nil {
		return
	}
	h.reader.Mutation(obj)
}

func buildVGPURequestKey(podClaimName, mainRequest string) string {
	return podClaimName + "/" + mainRequest
}

// buildResourceClaim Build vGPU resource claims based on container requests.
func (h *validateHandle) buildResourceClaim(pod *corev1.Pod, requests []resourceapi.DeviceRequest, resourceClaimName, ownerPod, timestamp string) *resourceapi.ResourceClaim {
	var deviceConstraints []resourceapi.DeviceConstraint
	req := allocator.BuildAllocationRequest(pod)
	// Handling multiple request device allocation constraints
	//if len(requests) > 1 {
	//	// All requests are mutually exclusive by device UUID to ensure that multiple requests are not assigned the same device
	//	deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
	//		Requests:          []string{}, // match all requests
	//		DistinctAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/uuid"),
	//	})
	//
	//	switch req.Topology.BaseTopology() {
	//	case util.LinkTopology:
	//		deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
	//			Requests:       []string{}, // match all requests
	//			MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](resourceapi.FullyQualifiedName(deviceattribute.StandardDeviceAttributePCIeRoot)),
	//		})
	//	case util.NUMATopology:
	//		deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
	//			Requests:       []string{}, // match all requests
	//			MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/numa"),
	//		})
	//	}
	//}

	for _, request := range requests {
		// Handling multiple device allocation constraints
		if (request.Exactly.Count > 1 && (request.Exactly.AllocationMode == "" ||
			request.Exactly.AllocationMode == resourceapi.DeviceAllocationModeExactCount)) ||
			request.Exactly.AllocationMode == resourceapi.DeviceAllocationModeAll {

			// The uuids of multiple devices in a single request are mutually exclusive, ensuring that each physical device is only assigned once.
			deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
				Requests:          []string{request.Name},
				DistinctAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/uuid"),
			})

			// Multiple devices are matched and allocated according to defined topology patterns to ensure optimal performance.
			switch req.Topology.BaseTopology() {
			case util.LinkTopology:
				deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
					Requests:       []string{request.Name},
					MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](resourceapi.FullyQualifiedName(deviceattribute.StandardDeviceAttributePCIeRoot)),
				})
			case util.NUMATopology:
				deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
					Requests:       []string{request.Name},
					MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/numa"),
				})
			}
		}
	}

	var annotations map[string]string
	if val, ok := util.HasAnnotation(pod, util.VGPUComputePolicyAnnotation); ok {
		annotations = map[string]string{util.VGPUComputePolicyAnnotation: val}
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				util.DRAOwnerPodLabel:   ownerPod,
				util.DRACreateTimeLabel: timestamp,
			},
			Annotations: annotations,
			Name:        resourceClaimName,
			Namespace:   pod.Namespace,
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Constraints: deviceConstraints,
				Requests:    requests,
			},
		},
	}
}

func buildDeviceRequest(pod *corev1.Pod, requestName, deviceClassName string, info common.ResourceInfo) resourceapi.DeviceRequest {
	var (
		deviceCount     int64
		capacityRequest = make(map[resourceapi.QualifiedName]resource.Quantity)
	)
	if quantity, ok := info.Resources[corev1.ResourceName(util.VGPUNumberResourceName)]; ok {
		deviceCount = quantity.Value()
	}
	if quantity, ok := info.Resources[corev1.ResourceName(util.VGPUCoreResourceName)]; ok && quantity.Value() > 0 {
		capacityRequest[kubeletplugin.CoresResourceName] = *resource.NewQuantity(quantity.Value(), resource.DecimalSI)
	}
	if quantity, ok := info.Resources[corev1.ResourceName(util.VGPUMemoryResourceName)]; ok && quantity.Value() > 0 {
		capacityRequest[kubeletplugin.MemoryResourceName] = *resource.NewQuantity(quantity.Value()*units.MiB, resource.BinarySI)
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
	policy, _ := util.HasAnnotation(pod, util.MemorySchedulerPolicyAnnotation)
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == util.VirtualMemoryPolicy.String() || strings.HasPrefix(policy, "virt") {
		deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
			CEL: &resourceapi.CELDeviceSelector{
				Expression: fmt.Sprintf(`device.attributes["%s"].memoryRatio > 100`, util.DRADriverName),
			},
		})
	} else if policy == util.PhysicalMemoryPolicy.String() || strings.HasPrefix(policy, "phy") {
		deviceSelectors = append(deviceSelectors, resourceapi.DeviceSelector{
			CEL: &resourceapi.CELDeviceSelector{
				Expression: fmt.Sprintf(`device.attributes["%s"].memoryRatio <= 100`, util.DRADriverName),
			},
		})
	}

	return resourceapi.DeviceRequest{
		Name: requestName,
		Exactly: &resourceapi.ExactDeviceRequest{
			DeviceClassName: deviceClassName,
			AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
			Count:           deviceCount,
			Capacity: &resourceapi.CapacityRequirements{
				Requests: capacityRequest,
			},
			Selectors: deviceSelectors,
		},
	}
}

func (h *validateHandle) isVGPUDeviceRequest(ctx context.Context, req resourceapi.DeviceRequest) bool {
	return common.DeviceRequestLooksLikeVGPU(ctx, h.reader, req, false, h.options.VGPUDeviceClassName)
}

func (h *validateHandle) isVGPUSubRequest(ctx context.Context, req resourceapi.DeviceSubRequest) bool {
	return common.SubRequestLooksLikeVGPU(ctx, h.reader, req, false, h.options.VGPUDeviceClassName)
}

func CheckResourceName(pod *corev1.Pod, resourceName string) error {
	if resourceName == "" {
		return apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
			field.Invalid(field.NewPath("metadata").Child("annotations").Child(util.DRAGenNameAnnotation),
				resourceName, "generate name cannot be empty")})
	}
	return nil
}

func (h *validateHandle) createCombinedResourceClaim(ctx context.Context, pod *corev1.Pod, resourceInfos common.ResourceInfos) error {
	logger := log.FromContext(ctx)

	resourceName, _ := util.HasAnnotation(pod, util.DRAGenNameAnnotation)
	if err := CheckResourceName(pod, resourceName); err != nil {
		return err
	}

	resourceRequests := make([]resourceapi.DeviceRequest, len(resourceInfos))
	for i, info := range resourceInfos {
		container, err := checkResourceInfo(pod, i, info)
		if err != nil {
			logger.V(3).Error(err, "")
			return err
		}

		resourceRequestName := util.GenerateK8sSafeResourceName(container.Name, kubeletplugin.VGpuDeviceType)
		request := buildDeviceRequest(pod, resourceRequestName, h.options.VGPUDeviceClassName, info)
		resourceRequests[i] = request
	}

	ownerPod := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	createTimestamp := fmt.Sprintf("%v", time.Now().UnixMilli())
	resourceClaimName := util.GenerateK8sSafeResourceName(resourceName)
	resourceClaim := h.buildResourceClaim(pod, resourceRequests, resourceClaimName, ownerPod, createTimestamp)

	if err := h.client.Create(ctx, resourceClaim); err != nil {
		logger.Error(err, "Failed to create combined vGPU resourceClaim")
		return err
	}
	h.mutation(resourceClaim)

	logger.Info("Successfully created combined vGPU resourceClaim", "resourceClaim", klog.KObj(resourceClaim))

	return nil
}

func validateContainerResources(resourceInfo common.ResourceInfo, containerPath *field.Path) field.ErrorList {
	var errs field.ErrorList

	quantity, ok := resourceInfo.Resources[corev1.ResourceName(util.VGPUCoreResourceName)]
	if ok && quantity.Value() > util.HundredCore {
		errs = append(errs, field.Invalid(
			containerPath.Child("resources").Child("limits").Key(util.VGPUCoreResourceName),
			quantity.Value(), fmt.Sprintf("request exceeds limit, maximum: %v", util.HundredCore)))
	}

	quantity, ok = resourceInfo.Resources[corev1.ResourceName(util.VGPUNumberResourceName)]
	if ok && quantity.Value() > vgpu.MaxDeviceCount {
		errs = append(errs, field.Invalid(
			containerPath.Child("resources").Child("limits").Key(util.VGPUNumberResourceName),
			quantity.Value(), fmt.Sprintf("request exceeds limit, maximum: %v", vgpu.MaxDeviceCount)))
	}
	return errs
}

func checkResourceInfo(pod *corev1.Pod, infoIndex int, resourceInfo common.ResourceInfo) (*corev1.Container, error) {
	if initContainerIndex := slices.IndexFunc(pod.Spec.InitContainers, func(c corev1.Container) bool {
		return c.Name == resourceInfo.Name
	}); initContainerIndex >= 0 {
		container := &pod.Spec.InitContainers[initContainerIndex]
		basePath := field.NewPath("spec").Child("initContainers").Index(initContainerIndex)
		if errs := validateContainerResources(resourceInfo, basePath); len(errs) > 0 {
			return nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, errs)
		}
		return container, nil
	}

	if containerIndex := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool {
		return c.Name == resourceInfo.Name
	}); containerIndex >= 0 {
		container := &pod.Spec.Containers[containerIndex]
		basePath := field.NewPath("spec").Child("containers").Index(containerIndex)
		if errs := validateContainerResources(resourceInfo, basePath); len(errs) > 0 {
			return nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, errs)
		}
		return container, nil
	}
	return nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
		field.Invalid(field.NewPath("metadata").Child("annotations").
			Child(util.DRAOriResAnnotation).Index(infoIndex).Child("containerName"),
			resourceInfo.Name, "container not found")})
}

func (h *validateHandle) createMultiResourceClaims(ctx context.Context, pod *corev1.Pod, resourceInfos common.ResourceInfos) (err error) {
	logger := log.FromContext(ctx)
	ownerPod := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	createTimestamp := fmt.Sprintf("%v", time.Now().UnixMilli())
	resourceClaimKeys := make([]types.NamespacedName, 0, len(resourceInfos))

	resourceName := pod.Name
	if pod.GenerateName != "" {
		resourceName, _ = util.HasAnnotation(pod, util.DRAGenNameAnnotation)
		if err = CheckResourceName(pod, resourceName); err != nil {
			return err
		}
	}

	defer func() {
		// Clean up the created resources when an error occurs,
		// using timestamp label to prevent accidental deletion of resources during batch deletion.
		if err != nil && len(resourceClaimKeys) > 0 {
			if delErr := h.client.DeleteAllOf(
				context.Background(),
				&resourceapi.ResourceClaim{},
				client.InNamespace(pod.Namespace),
				client.MatchingLabels{
					util.DRAOwnerPodLabel:   ownerPod,
					util.DRACreateTimeLabel: createTimestamp,
				},
			); delErr != nil {
				logger.Error(delErr, "Failed to clear vGPU resourceClaims", "resourceClaims", resourceClaimKeys)
			}
		}
	}()

	for i, info := range resourceInfos {
		container, err := checkResourceInfo(pod, i, info)
		if err != nil {
			logger.V(3).Error(err, "")
			return err
		}

		resourceClaimName := util.GenerateK8sSafeResourceName(resourceName, container.Name)
		if !slices.ContainsFunc(pod.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
			return claim.ResourceClaimName != nil && *claim.ResourceClaimName == resourceClaimName
		}) {
			err = apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
				field.Invalid(field.NewPath("spec").Child("resourceClaims"),
					pod.Spec.ResourceClaims, fmt.Sprintf("resource claim %s not found", resourceClaimName))})
			logger.V(5).Error(err, "")
			return err
		}

		// Create container resource claim
		request := buildDeviceRequest(pod, kubeletplugin.VGpuDeviceType, h.options.VGPUDeviceClassName, info)
		resourceClaim := h.buildResourceClaim(pod, []resourceapi.DeviceRequest{request}, resourceClaimName, ownerPod, createTimestamp)
		if err = h.client.Create(ctx, resourceClaim); err != nil {
			logger.Error(err, "Failed to create vGPU resourceClaim", "container", container.Name)
			return err
		}
		h.mutation(resourceClaim)
		resourceClaimKeys = append(resourceClaimKeys, client.ObjectKeyFromObject(resourceClaim))
		logger.V(2).Info("Successfully created resourceClaim", "resourceClaim",
			klog.KObj(resourceClaim), "container", container.Name)
	}

	if len(resourceClaimKeys) > 0 {
		logger.Info("Successfully created all vGPU resourceClaims", "resourceClaims", resourceClaimKeys)
	}
	return nil
}

func (h *validateHandle) createResourceClaims(ctx context.Context, pod *corev1.Pod) (err error) {
	logger := log.FromContext(ctx)
	val, ok := util.HasAnnotation(pod, util.DRAOriResAnnotation)
	if !ok || len(val) == 0 {
		return nil
	}
	if len(val) > util.PodAnnotationMaxLength {
		err = apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
			field.Invalid(field.NewPath("metadata").Child("annotations").Child(util.DRAOriResAnnotation),
				field.OmitValueType{}, "recorded value is too long")})
		logger.V(5).Error(err, "")
		return err
	}
	infos := common.ResourceInfos{}
	if err = infos.Decode(val); err != nil {
		logger.V(2).Error(err, "Decoding original resource information failed")
		return apierrors.NewBadRequest(fmt.Sprintf("Decoding original resource information failed: %v", err))
	}

	// fast return
	if len(infos) == 0 {
		return nil
	}

	if h.options.CombinedResourceClaim {
		if err = h.createCombinedResourceClaim(ctx, pod, infos); err != nil {
			return err
		}
	} else {
		if err = h.createMultiResourceClaims(ctx, pod, infos); err != nil {
			return err
		}
	}
	return nil
}

// hasDefiniteVGPURequests reports whether the index contains at least one definite vGPU request.
func hasDefiniteVGPURequests(index map[string]podRequestMeta) bool {
	for _, meta := range index {
		if meta.Class == common.MainRequestDefVGPU {
			return true
		}
	}
	return false
}

// findPodClaimNameForActualClaim returns the pod-local claim name (spec.resourceClaims[].name)
// that resolves to actualClaimName, checking both direct resourceClaimName references and
// template-resolved names stored in status.resourceClaimStatuses.
func findPodClaimNameForActualClaim(pod *corev1.Pod, actualClaimName string) string {
	for _, pc := range pod.Spec.ResourceClaims {
		if pc.ResourceClaimName != nil && *pc.ResourceClaimName == actualClaimName {
			return pc.Name
		}
		if pc.ResourceClaimTemplateName != nil {
			if st := claimresolve.GetPodResourceClaimStatus(pod, pc.Name); st != nil &&
				st.ResourceClaimName != nil && *st.ResourceClaimName == actualClaimName {
				return pc.Name
			}
		}
	}
	return ""
}

// collectPodClaimVGPURefs returns the set of definite-vGPU mainRequest names that pod
// uses from the claim identified by podClaimName within that pod's spec.
func (h *validateHandle) collectPodClaimVGPURefs(
	pod *corev1.Pod,
	podClaimName string,
	requestIndex map[string]podRequestMeta,
) sets.Set[string] {
	result := sets.New[string]()
	for _, c := range util.GetAllPodContainers(pod) {
		for _, claimRef := range c.Claims {
			if claimRef.Name != podClaimName {
				continue
			}
			if claimRef.Request != "" {
				if meta, ok := requestIndex[claimRef.Request]; ok && meta.Class == common.MainRequestDefVGPU {
					result.Insert(meta.MainRequest)
				}
			} else {
				for _, meta := range requestIndex {
					if meta.Class == common.MainRequestDefVGPU {
						result.Insert(meta.MainRequest)
					}
				}
			}
		}
	}
	return result
}

// checkCrossPodsVGPURequestConflict implements rule 8: cross-pod vGPU request sharing prohibition.
//
// For each named (non-template) ResourceClaim the new pod references, if the claim already
// has other pods in status.reservedFor, the new pod must not use the same definite-vGPU
// mainRequest as any of those reserved pods.
//
// This is a best-effort early check: concurrent pod admissions that race before either pod is
// reserved are caught later by the ResourceClaim validate webhook.
func (h *validateHandle) checkCrossPodsVGPURequestConflict(ctx context.Context, pod *corev1.Pod) error {
	if !util.HasDRARequests(pod) {
		return nil
	}

	for _, podClaim := range pod.Spec.ResourceClaims {
		// Template claims are per-pod and cannot be shared; skip them.
		if podClaim.ResourceClaimName == nil || *podClaim.ResourceClaimName == "" {
			continue
		}
		actualClaimName := *podClaim.ResourceClaimName

		// Fetch the actual claim.
		var claim resourceapi.ResourceClaim
		claimKey := client.ObjectKey{Namespace: pod.Namespace, Name: actualClaimName}
		if h.reader != nil {
			if err := h.reader.GetResourceClaim(ctx, claimKey, &claim); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("get claim %q: %w", actualClaimName, err)
			}
		} else {
			if err := h.client.Get(ctx, claimKey, &claim); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("get claim %q: %w", actualClaimName, err)
			}
		}

		// Nothing reserved yet — no conflict possible.
		if len(claim.Status.ReservedFor) == 0 {
			continue
		}

		// Build a spec-level request index and skip claims with no definite vGPU requests.
		requestIndex := h.buildPodRequestIndex(ctx, claim.Spec.Devices.Requests)
		if !hasDefiniteVGPURequests(requestIndex) {
			continue
		}

		// Compute which vGPU mainRequests the new pod uses from this claim.
		newPodReqs := h.collectPodClaimVGPURefs(pod, podClaim.Name, requestIndex)
		if newPodReqs.Len() == 0 {
			continue
		}

		// Compare against every pod already reserved for this claim.
		for _, ref := range claim.Status.ReservedFor {
			if ref.APIGroup != "" || ref.Resource != "pods" {
				continue
			}
			var existingPod corev1.Pod
			podKey := client.ObjectKey{Namespace: claim.Namespace, Name: ref.Name}
			var getErr error
			if h.reader != nil {
				getErr = h.reader.GetPod(ctx, podKey, &existingPod)
			} else {
				getErr = h.client.Get(ctx, podKey, &existingPod)
			}
			if getErr != nil {
				if apierrors.IsNotFound(getErr) {
					continue
				}
				return fmt.Errorf("get reserved pod %q for claim %q: %w", ref.Name, actualClaimName, getErr)
			}
			// Skip stale reservations (UID mismatch) and self-references.
			if ref.UID != "" && existingPod.UID != ref.UID {
				continue
			}
			if existingPod.Name == pod.Name && existingPod.Namespace == pod.Namespace {
				continue
			}

			existingPodClaimName := findPodClaimNameForActualClaim(&existingPod, actualClaimName)
			if existingPodClaimName == "" {
				continue
			}
			existingReqs := h.collectPodClaimVGPURefs(&existingPod, existingPodClaimName, requestIndex)
			if overlap := newPodReqs.Intersection(existingReqs); overlap.Len() > 0 {
				return fmt.Errorf(
					"pod %q and pod %s/%s both use vgpu requests %v from claim %q; "+
						"cross-pod sharing of the same vgpu request is not allowed",
					pod.Name, existingPod.Namespace, existingPod.Name,
					sets.List(overlap), actualClaimName,
				)
			}
		}
	}
	return nil
}

func (h *validateHandle) ValidateUpdate(ctx context.Context, oldPod, newPod *corev1.Pod) error {
	return nil
}

func (h *validateHandle) ValidateDelete(ctx context.Context, pod *corev1.Pod) error {
	if h.options.DefaultConvertToDRA {
		return h.deleteResourceClaims(ctx, pod)
	}
	return nil
}

func (h *validateHandle) deleteResourceClaims(ctx context.Context, pod *corev1.Pod) error {
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
	// fast return
	if len(infos) == 0 {
		return nil
	}

	resourceName := pod.Name

	// try delete CombinedResourceClaim
	if h.options.CombinedResourceClaim {
		resourceName, _ = util.HasAnnotation(pod, util.DRAGenNameAnnotation)
		resourceClaimName := util.GenerateK8sSafeResourceName(resourceName)
		if !slices.ContainsFunc(pod.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
			return claim.ResourceClaimName != nil && *claim.ResourceClaimName == resourceClaimName
		}) {
			logger.V(1).Info("ResourceClaimName not found, skip ResourceClaim delete",
				"resourceClaimName", resourceClaimName)
		} else {
			resourceClaim := &resourceapi.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceClaimName,
					Namespace: pod.Namespace,
				},
			}
			if err := h.client.Delete(ctx, resourceClaim); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "Failed to delete ResourceClaim", "resourceClaim", klog.KObj(resourceClaim))
			}
		}
	}

	if pod.GenerateName != "" {
		resourceName, _ = util.HasAnnotation(pod, util.DRAGenNameAnnotation)
	}

	// try delete MultiResourceClaims
	for _, info := range infos {
		resourceClaimName := util.GenerateK8sSafeResourceName(resourceName, info.Name)
		if !slices.ContainsFunc(pod.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
			return claim.ResourceClaimName != nil && *claim.ResourceClaimName == resourceClaimName
		}) {
			logger.V(1).Info("ResourceClaimName for container not found, skip ResourceClaim delete",
				"container", info.Name, "resourceClaimName", resourceClaimName)
			continue
		}
		resourceClaim := &resourceapi.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resourceClaimName,
				Namespace: pod.Namespace,
			},
		}
		if err := h.client.Delete(ctx, resourceClaim); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to delete ResourceClaim", "resourceClaim", klog.KObj(resourceClaim))
		}
	}
	return nil
}

func (h *validateHandle) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("operation", req.Operation)
	logger.V(4).Info("into pod validate handle")

	var err error
	var warnings []string
	ctx = log.IntoContext(ctx, logger)
	switch req.Operation {
	case admissionv1.Create:
		pod := &corev1.Pod{}
		if err = h.decoder.Decode(req, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.ValidateCreate(ctx, pod)
	case admissionv1.Update:
		oldPod, newPod := &corev1.Pod{}, &corev1.Pod{}
		if err = h.decoder.Decode(req, newPod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if err = h.decoder.DecodeRaw(req.OldObject, oldPod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.ValidateUpdate(ctx, oldPod, newPod)
	case admissionv1.Delete:
		pod := &corev1.Pod{}
		if err = h.decoder.DecodeRaw(req.OldObject, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		err = h.ValidateDelete(ctx, pod)
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
