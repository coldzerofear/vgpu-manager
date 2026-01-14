package util

import (
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/klog/v2"
)

func InsertAnnotation(obj metav1.Object, k, v string) {
	if obj.GetAnnotations() == nil {
		obj.SetAnnotations(map[string]string{})
	}
	obj.GetAnnotations()[k] = v
}

func HasLabel(obj metav1.Object, label string) (val string, ok bool) {
	if obj.GetLabels() != nil {
		val, ok = obj.GetLabels()[label]
	}
	return val, ok
}

func HasAnnotation(obj metav1.Object, anno string) (val string, ok bool) {
	if obj.GetAnnotations() != nil {
		val, ok = obj.GetAnnotations()[anno]
	}
	return val, ok
}

// GetCapacityOfNode Return the capacity of node resources.
func GetCapacityOfNode(node *corev1.Node, resourceName string) (int64, bool) {
	if val, ok := node.Status.Capacity[corev1.ResourceName(resourceName)]; ok {
		return val.Value(), true
	}
	return 0, false
}

// GetAllocatableOfNode Return the number of resources that can be allocated to the node.
func GetAllocatableOfNode(node *corev1.Node, resourceName string) (int64, bool) {
	if val, ok := node.Status.Allocatable[corev1.ResourceName(resourceName)]; ok {
		return val.Value(), true
	}
	return 0, false
}

// IsVGPUEnabledNode Determine whether there are vGPU devices on the node.
func IsVGPUEnabledNode(node *corev1.Node) bool {
	val, _ := GetAllocatableOfNode(node, VGPUNumberResourceName)
	return val > 0
}

// GetResourceOfContainer Return the number of resource limit.
func GetResourceOfContainer(container *corev1.Container, resourceName string) int64 {
	var count int64
	if val, ok := container.Resources.Limits[corev1.ResourceName(resourceName)]; ok {
		count = val.Value()
	}
	return count
}

// IsVGPURequiredContainer tell if the container is a vGPU request container.
func IsVGPURequiredContainer(c *corev1.Container) bool {
	return GetResourceOfContainer(c, VGPUNumberResourceName) > 0
}

// GetResourceOfPod Return the number of resource limit for all containers of Pod.
func GetResourceOfPod(pod *corev1.Pod, resourceName string) int64 {
	var total int64
	for i := range pod.Spec.Containers {
		total += GetResourceOfContainer(&pod.Spec.Containers[i], resourceName)
	}
	return total
}

// IsVGPUResourcePod Determine if a pod has vGPU resource request.
func IsVGPUResourcePod(pod *corev1.Pod) bool {
	for i := range pod.Spec.Containers {
		if GetResourceOfContainer(&pod.Spec.Containers[i], VGPUNumberResourceName) > 0 {
			return true
		}
	}
	return false
}

// CheckDeviceType Check if the device type meets expectations.
func CheckDeviceType(annotations map[string]string, deviceType string) bool {
	deviceType = strings.ToUpper(strings.TrimSpace(deviceType))
	if includes, ok := annotations[PodIncludeGpuTypeAnnotation]; ok {
		includeTypes := strings.Split(strings.ToUpper(includes), ",")
		if !slices.ContainsFunc(includeTypes, func(devType string) bool {
			return strings.Contains(deviceType, strings.TrimSpace(devType))
		}) {
			return false
		}
	}
	if excludes, ok := annotations[PodExcludeGpuTypeAnnotation]; ok {
		excludeTypes := strings.Split(strings.ToUpper(excludes), ",")
		if slices.ContainsFunc(excludeTypes, func(devType string) bool {
			return strings.Contains(deviceType, strings.TrimSpace(devType))
		}) {
			return false
		}
	}
	return true
}

// CheckDeviceUuid Check if the device uuid meets expectations.
func CheckDeviceUuid(annotations map[string]string, deviceUUID string) bool {
	deviceUUID = strings.ToUpper(strings.TrimSpace(deviceUUID))
	if includes, ok := annotations[PodIncludeGPUUUIDAnnotation]; ok {
		includeUUIDs := strings.Split(strings.ToUpper(includes), ",")
		return slices.ContainsFunc(includeUUIDs, func(uuid string) bool {
			return strings.Contains(deviceUUID, strings.TrimSpace(uuid))
		})
	}
	if excludes, ok := annotations[PodExcludeGPUUUIDAnnotation]; ok {
		excludeUUIDs := strings.Split(strings.ToUpper(excludes), ",")
		return !slices.ContainsFunc(excludeUUIDs, func(uuid string) bool {
			return strings.Contains(deviceUUID, strings.TrimSpace(uuid))
		})
	}
	return true
}

// ShouldRetry Determine whether the error of apiserver is of the type that needs to be retried.
func ShouldRetry(err error) bool {
	// these errors indicate a transient error that should be retried.
	return errors.IsConflict(err) || errors.IsServerTimeout(err) ||
		errors.IsTooManyRequests(err) || net.IsConnectionReset(err) ||
		net.IsHTTP2ConnectionLost(err)
}

// IsShouldDeletePod Determine whether the pod has been deleted or needs to be deleted.
func IsShouldDeletePod(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return true
	}
	if len(pod.Status.ContainerStatuses) > MaxContainerLimit {
		klog.Error("The number of container exceeds the upper limit")
		return true
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil &&
			(strings.Contains(status.State.Waiting.Message, PreStartContainerCheckErrMsg) ||
				strings.Contains(status.State.Waiting.Message, AllocateCheckErrMsg)) {
			return true
		}
	}
	return pod.Status.Reason == "UnexpectedAdmissionError"
}

func PodIsTerminated(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed ||
		pod.Status.Phase == corev1.PodSucceeded ||
		(pod.DeletionTimestamp != nil && notRunning(pod.Status.ContainerStatuses))
}

// notRunning returns true if every status is terminated or waiting, or the status list
// is empty.
func notRunning(statuses []corev1.ContainerStatus) bool {
	for _, status := range statuses {
		if status.State.Terminated == nil && status.State.Waiting == nil {
			return false
		}
	}
	return true
}

type PodsOrderedByPredicateTime []corev1.Pod

func (pods PodsOrderedByPredicateTime) Len() int {
	return len(pods)
}

func (pods PodsOrderedByPredicateTime) Less(i, j int) bool {
	return GetPredicateTimeOfPod(pods[i]) < GetPredicateTimeOfPod(pods[j])
}

func (pods PodsOrderedByPredicateTime) Swap(i, j int) {
	pods[i], pods[j] = pods[j], pods[i]
}

func GetPredicateTimeOfPod(pod corev1.Pod) uint64 {
	assumeTimeStr, ok := HasAnnotation(&pod, PodPredicateTimeAnnotation)
	if !ok || len(assumeTimeStr) > PodAnnotationMaxLength {
		return math.MaxUint64
	}
	predicateTime, err := strconv.ParseUint(assumeTimeStr, 10, 64)
	if err != nil {
		klog.Warningf("failed to parse predicate timestamp %s due to %v", assumeTimeStr, err)
		return math.MaxUint64
	}
	return predicateTime
}

// GetCurrentPodByAllocatingPods find the oldest Pod from the allocating Pods
// to be allocated as the current Pod to be allocated.
func GetCurrentPodByAllocatingPods(allocatingPods []corev1.Pod) (*corev1.Pod, error) {
	if len(allocatingPods) == 0 {
		return nil, fmt.Errorf("unable to find the current pod to be allocated")
	}
	pods := PodsOrderedByPredicateTime(allocatingPods)
	if len(allocatingPods) > 1 {
		sort.Sort(pods)
	}
	return &pods[0], nil
}

// FilterAllocatingPods filter out the list of pods to be allocated.
func FilterAllocatingPods(activePods []corev1.Pod) []corev1.Pod {
	var allocatingPods []corev1.Pod
	requiredAnnoKeys := []string{
		PodPredicateTimeAnnotation, PodPredicateNodeAnnotation, PodVGPUPreAllocAnnotation,
	}
	for i, pod := range activePods {
		klog.V(5).Infof("FilterPod <%s/%s> %s", pod.Namespace, pod.Name, pod.Status.Phase)
		if !IsVGPUResourcePod(&pod) || IsShouldDeletePod(&pod) {
			continue
		}
		if slices.ContainsFunc(requiredAnnoKeys, func(key string) bool {
			_, exists := HasAnnotation(&pod, key)
			return !exists
		}) {
			continue
		}
		allocatingPods = append(allocatingPods, activePods[i])
	}
	return allocatingPods
}

func IsSingleContainerMultiGPUs(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.Containers {
		if GetResourceOfContainer(&container, VGPUNumberResourceName) > 1 {
			return true
		}
	}
	return false
}

func PodPlanSchedulingNode(pod *corev1.Pod) string {
	if pod.Spec.NodeName != "" {
		return pod.Spec.NodeName
	}
	predicateNode, _ := HasAnnotation(pod, PodPredicateNodeAnnotation)
	return predicateNode
}

func PodsOnNodeCallback(pods []*corev1.Pod, node *corev1.Node, callbackFn func(*corev1.Pod)) {
	if callbackFn == nil {
		klog.Warningln("PodsOnNodeCallback callback function is empty")
		return
	}
	klog.V(5).InfoS("pods on node callback", "node", node.Name)
	for _, pod := range pods {
		if PodPlanSchedulingNode(pod) == node.Name &&
			pod.Status.Phase != corev1.PodSucceeded &&
			pod.Status.Phase != corev1.PodFailed {
			callbackFn(pod)
		}
	}
}

func PathIsNotExist(fullPath string) bool {
	_, err := os.Stat(fullPath)
	return os.IsNotExist(err)
}

func GetPodContainerManagerPath(managerBaseDir string, podUID types.UID, containerName string) string {
	return fmt.Sprintf("%s/%s_%s", managerBaseDir, string(podUID), containerName)
}

// MakeDeviceID generates compact binary encoded device IDs.
// gpuId must be in [0, 255], i must be non-negative.
func MakeDeviceID(gpuId, i int64) string {
	if gpuId < 0 || gpuId >= 256 {
		panic(fmt.Errorf("gpuId must be in [0, 255], got %d", gpuId))
	}
	if i < 0 {
		panic(fmt.Errorf("i must be non-negative, got %d", i))
	}
	combined := (uint64(i) << 8) | uint64(gpuId)
	var buf [10]byte
	w := buf[:0]
	w = protowire.AppendVarint(w, combined)
	return base64.RawURLEncoding.EncodeToString(w)
}

// ParseDeviceID parses a device ID into gpuId and i.
func ParseDeviceID(devId string) (gpuId, i int64, err error) {
	if devId == "" {
		return 0, 0, fmt.Errorf("empty device ID")
	}

	data, err := base64.RawURLEncoding.DecodeString(devId)
	if err != nil {
		return 0, 0, fmt.Errorf("base64 decode failed: %w", err)
	}

	v, n := protowire.ConsumeVarint(data)
	if n <= 0 {
		return 0, 0, fmt.Errorf("invalid varint encoding")
	}
	if n != len(data) {
		return 0, 0, fmt.Errorf("extra data in device ID: expected %d bytes, got %d", n, len(data))
	}

	gpuId = int64(v & 0xFF)
	i = int64(v >> 8)

	// Check if there is any extra data (strict mode)
	if gpuId < 0 || gpuId >= 256 {
		return 0, 0, fmt.Errorf("invalid gpuId in device ID: %d", gpuId)
	}

	return gpuId, i, nil
}

func GetValidValue(x uint32) uint32 {
	if x <= 100 {
		return x
	}
	return 0
}

func CodecNormalize(x uint32) uint32 {
	return x * 85 / 100
}

func GetPercentageValue(x uint32) uint32 {
	switch {
	case x > 100:
		return 100
	case x < 0:
		return 0
	default:
		return x
	}
}

func VGPUControlDisabled(pod *corev1.Pod, containerName string) bool {
	index := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool {
		return c.Name == containerName
	})
	if index < 0 {
		return false
	}
	disabled := false
	for _, envVar := range pod.Spec.Containers[index].Env {
		if envVar.Name == DisableVGPUEnv {
			disabled = envVar.Value == "true"
			break
		}
	}
	return disabled
}
