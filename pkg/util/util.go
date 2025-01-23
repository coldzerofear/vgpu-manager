package util

import (
	"fmt"
	"math"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

func InsertAnnotation(obj metav1.Object, k, v string) {
	if obj.GetAnnotations() == nil {
		obj.SetAnnotations(map[string]string{})
	}
	obj.GetAnnotations()[k] = v
}

func HasAnnotation(obj metav1.Object, anno string) (string, bool) {
	val, ok := "", false
	if obj.GetAnnotations() != nil {
		val, ok = obj.GetAnnotations()[anno]
	}
	return val, ok
}

// GetCapacityOfNode Return the capacity of node resources.
func GetCapacityOfNode(node *corev1.Node, resourceName string) int {
	val, ok := node.Status.Capacity[corev1.ResourceName(resourceName)]
	if !ok {
		return 0
	}
	return int(val.Value())
}

// GetAllocatableOfNode Return the number of resources that can be allocated to the node.
func GetAllocatableOfNode(node *corev1.Node, resourceName string) int {
	val, ok := node.Status.Allocatable[corev1.ResourceName(resourceName)]
	if !ok {
		return 0
	}
	return int(val.Value())
}

// IsVGPUEnabledNode Determine whether there are VGPU devices on the node.
func IsVGPUEnabledNode(node *corev1.Node) bool {
	return GetAllocatableOfNode(node, VGPUNumberResourceName) > 0
}

func Max(a, b int) int {
	if a >= b {
		return a
	}
	return b
}

// GetResourceOfContainer Return the number of resource limit.
func GetResourceOfContainer(container *corev1.Container, resourceName corev1.ResourceName) int {
	var count int
	if val, ok := container.Resources.Limits[resourceName]; ok {
		count = int(val.Value())
	}
	return count
}

// IsVGPURequiredContainer tell if the container is a vGPU request container.
func IsVGPURequiredContainer(c *corev1.Container) bool {
	return GetResourceOfContainer(c, VGPUNumberResourceName) > 0
}

// GetResourceOfPod Return the number of resource limit for all containers of Pod.
func GetResourceOfPod(pod *corev1.Pod, resourceName corev1.ResourceName) int {
	var total int
	for i := range pod.Spec.Containers {
		total += GetResourceOfContainer(&pod.Spec.Containers[i], resourceName)
	}
	return total
}

// IsVGPUResourcePod Determine if a pod has vGPU resource request.
func IsVGPUResourcePod(pod *corev1.Pod) bool {
	return GetResourceOfPod(pod, VGPUNumberResourceName) > 0
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
	return errors.IsConflict(err) || errors.IsServerTimeout(err) || errors.IsTooManyRequests(err)
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
			(strings.Contains(status.State.Waiting.Message, "PreStartContainer failed") ||
				strings.Contains(status.State.Waiting.Message, "Allocate failed")) {
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
	if !ok {
		return math.MaxUint64
	}
	if len(assumeTimeStr) > PodAnnotationMaxLength {
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
	for i, pod := range activePods {
		klog.V(5).Infof("FilterPod <%s/%s> %s", pod.Namespace, pod.Name, pod.Status.Phase)
		if !IsVGPUResourcePod(&pod) {
			continue
		}
		if IsShouldDeletePod(&pod) {
			continue
		}
		if _, ok := HasAnnotation(&pod, PodPredicateTimeAnnotation); !ok {
			continue
		}
		if _, ok := HasAnnotation(&pod, PodPredicateNodeAnnotation); !ok {
			continue
		}
		if _, ok := HasAnnotation(&pod, PodVGPUPreAllocAnnotation); !ok {
			continue
		}
		allocatingPods = append(allocatingPods, activePods[i])
	}
	return allocatingPods
}

func GetNumaInformation(idx int) (int, error) {
	cmd := exec.Command("nvidia-smi", "topo", "-m")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, err
	}
	klog.V(5).InfoS("nvidia-smi topo -m output", "result", string(out))
	return parseNvidiaNumaInfo(idx, string(out))
}

func parseNvidiaNumaInfo(idx int, nvidiaTopoStr string) (int, error) {
	result := 0
	numaAffinityColumnIndex := 0
	for index, val := range strings.Split(nvidiaTopoStr, "\n") {
		if !strings.Contains(val, "GPU") {
			continue
		}
		// Example: GPU0	 X 	0-7		N/A		N/A
		// Many values are separated by two tabs, but this actually represents 5 values instead of 7
		// So add logic to remove multiple tabs
		words := strings.Split(strings.ReplaceAll(val, "\t\t", "\t"), "\t")
		klog.V(5).InfoS("parseNumaInfo", "words", words)
		// get numa affinity column number
		if index == 0 {
			for columnIndex, headerVal := range words {
				// The topology output of a single card is as follows:
				// 			GPU0	CPU Affinity	NUMA Affinity	GPU NUMA ID
				// GPU0	 X 	0-7		N/A		N/A
				//Legend: Other content omitted

				// The topology output in the case of multiple cards is as follows:
				// 			GPU0	GPU1	CPU Affinity	NUMA Affinity
				// GPU0	 X 	PHB	0-31		N/A
				// GPU1	PHB	 X 	0-31		N/A
				// Legend: Other content omitted

				// We need to get the value of the NUMA Affinity column, but their column indexes are inconsistent,
				// so we need to get the index first and then get the value.
				if strings.Contains(headerVal, "NUMA Affinity") {
					// The header is one column less than the actual row.
					numaAffinityColumnIndex = columnIndex
					continue
				}
			}
			continue
		}
		klog.V(5).InfoS("nvidia-smi topo -m row output", "row output", words, "length", len(words))
		if strings.Contains(words[0], fmt.Sprint(idx)) {
			if words[numaAffinityColumnIndex] == "N/A" {
				klog.InfoS("current card has not established numa topology", "gpu row info", words, "index", idx)
				return 0, nil
			}
			result, err := strconv.Atoi(words[numaAffinityColumnIndex])
			if err != nil {
				return result, err
			}
		}
	}
	return result, nil
}
