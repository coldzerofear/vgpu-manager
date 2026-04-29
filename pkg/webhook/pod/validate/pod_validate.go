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
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/filter"
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
	if h.options.DefaultConvertToDRA {
		if err := h.checkResourceClaimRequests(ctx, pod); err != nil {
			return &apierrors.StatusError{
				ErrStatus: metav1.Status{
					Message: err.Error(),
					Reason:  metav1.StatusReasonInvalid,
				},
			}
		}
		if err := h.createResourceClaims(ctx, pod); err != nil {
			return err
		}
	}
	return nil
}

// buildPodRequestIndex 按“主 request”建立 Pod 阶段可判定索引:
// - Exactly: 直接判断是否 definite vgpu
// - FirstAvailable:
//   - 全是 vgpu -> definite vgpu
//   - 全不是 vgpu -> non-vgpu
//   - 混合 -> mixed-maybe-vgpu，Pod 阶段不做最终冲突判断
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

// checkResourceClaimRequests：Pod 阶段校验规则
// 1. 一个容器最多只能命中 1 个 definite-vgpu claim
// 2. 一个容器可以命中同一个 claim 下多个 definite-vgpu request
// 3. init-init 不允许同 mainRequest 重叠
// 4. app-app 不允许同 mainRequest 重叠
// 5. init-app 允许同 mainRequest 重叠
// 6. mixed FirstAvailable 先不判，留给 claim webhook
func (h *validateHandle) checkResourceClaimRequests(ctx context.Context, pod *corev1.Pod) error {
	// fast return
	if !util.HasDRARequests(pod) {
		return nil
	}

	allContainers := common.GetAllPodContainers(pod)
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
					return fmt.Errorf("%s %q claim %q validation failed: %w", c.Kind, c.Name, claimRef.Name, err)
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
				"%s %q references multiple vgpu claims %v; one container can use at most one vgpu claim",
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

			switch c.Kind {
			case common.ContainerKindInit:
				usage.InitContainers.Insert(c.Name)
				if usage.InitContainers.Len() > 1 {
					return fmt.Errorf(
						"vgpu request %q is referenced by multiple init containers %v; sharing is only allowed between init and app containers",
						reqKey, sets.List(usage.InitContainers),
					)
				}
			case common.ContainerKindApp:
				usage.AppContainers.Insert(c.Name)
				if usage.AppContainers.Len() > 1 {
					return fmt.Errorf(
						"vgpu request %q is referenced by multiple app containers %v; sharing is only allowed between init and app containers",
						reqKey, sets.List(usage.AppContainers),
					)
				}
			default:
				return fmt.Errorf("unknown container kind %q for container %q", c.Kind, c.Name)
			}

			usages[reqKey] = usage
		}
	}

	return nil
}

// resolveDefiniteVGPURequestsFromContainerClaim 只返回 Pod 阶段“能确定”的 vgpu main request。
// 规则：
// - claimRef.Request != "": 仅检查这个主 request
// - claimRef.Request == "": 返回该 claim 下全部 definite-vgpu 主 request
// - mixed FirstAvailable: 这里不返回，留给 claim webhook 兜底
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
			// Non VGPU or mixed, both left for claim webhook to check
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

	//// Handling multiple request device allocation constraints
	//if len(requests) > 1 {
	//	// All requests are mutually exclusive by device UUID to ensure that multiple requests are not assigned the same device
	//	deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
	//		Requests:          []string{}, // match all requests
	//		DistinctAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/uuid"),
	//	})
	//
	//	switch filter.PodUsedGPUTopologyMode(pod) {
	//	case util.LinkTopology:
	//		deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
	//			Requests:       []string{}, // match all requests
	//			MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](resourceapi.FullyQualifiedName(deviceattribute.StandardDeviceAttributePCIeRoot)),
	//		})
	//	case util.NUMATopology:
	//		deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
	//			Requests:       []string{}, // match all requests
	//			MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/numaNode"),
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
			switch filter.PodUsedGPUTopologyMode(pod) {
			case util.LinkTopology:
				deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
					Requests:       []string{request.Name},
					MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](resourceapi.FullyQualifiedName(deviceattribute.StandardDeviceAttributePCIeRoot)),
				})
			case util.NUMATopology:
				deviceConstraints = append(deviceConstraints, resourceapi.DeviceConstraint{
					Requests:       []string{request.Name},
					MatchAttribute: ptr.To[resourceapi.FullyQualifiedName](util.DRADriverName + "/numaNode"),
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
		containerIndex, err := checkResourceInfo(pod, i, info)
		if err != nil {
			logger.V(3).Error(err, "")
			return err
		}
		container := &pod.Spec.Containers[containerIndex]
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

func checkResourceInfo(pod *corev1.Pod, infoIndex int, resourceInfo common.ResourceInfo) (containerIndex int, err error) {
	containerIndex = slices.IndexFunc(pod.Spec.Containers, func(container corev1.Container) bool {
		return container.Name == resourceInfo.Name
	})
	if containerIndex < 0 {
		err = apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, field.ErrorList{
			field.Invalid(field.NewPath("metadata").Child("annotations").
				Child(util.DRAOriResAnnotation).Index(infoIndex).Child("containerName"),
				resourceInfo.Name, "container not found")})
		return containerIndex, err
	}

	var errs field.ErrorList
	quantity, ok := resourceInfo.Resources[corev1.ResourceName(util.VGPUCoreResourceName)]
	if ok && quantity.Value() > util.HundredCore {
		msg := fmt.Sprintf("container %s requests vGPU core exceeding limit", resourceInfo.Name)
		errs = append(errs, field.Invalid(field.NewPath("spec").Child("containers").Index(containerIndex).
			Child("resources").Child("limits").Key(util.VGPUCoreResourceName), quantity.Value(), msg))
	}

	quantity, ok = resourceInfo.Resources[corev1.ResourceName(util.VGPUNumberResourceName)]
	if ok && quantity.Value() > vgpu.MaxDeviceCount {
		msg := fmt.Sprintf("container %s requests vGPU number exceeding limit", resourceInfo.Name)
		errs = append(errs, field.Invalid(field.NewPath("spec").Child("containers").Index(containerIndex).
			Child("resources").Child("limits").Key(util.VGPUNumberResourceName), quantity.Value(), msg))
	}
	if len(errs) > 0 {
		err = apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, errs)
	}

	return containerIndex, err
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
		containerIndex, err := checkResourceInfo(pod, i, info)
		if err != nil {
			logger.V(3).Error(err, "")
			return err
		}

		container := &pod.Spec.Containers[containerIndex]
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
		if slices.ContainsFunc(pod.Spec.ResourceClaims, func(claim corev1.PodResourceClaim) bool {
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

		containerIndex := slices.IndexFunc(pod.Spec.Containers, func(container corev1.Container) bool {
			return container.Name == info.Name
		})
		if containerIndex < 0 {
			logger.V(1).Info("Container not found, skip ResourceClaim delete", "container", info.Name)
			continue
		}

		container := &pod.Spec.Containers[containerIndex]
		resourceClaimName := util.GenerateK8sSafeResourceName(resourceName, container.Name)
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
	logger.V(5).Info("into pod validate handle")

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
