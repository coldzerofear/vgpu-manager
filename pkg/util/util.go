/*
Copyright 2024-2026 coldzerofear

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

package util

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/component-helpers/resource"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

func InsertAnnotation(obj metav1.Object, k, v string) {
	if obj == nil {
		return
	}
	if obj.GetAnnotations() == nil {
		obj.SetAnnotations(map[string]string{})
	}
	obj.GetAnnotations()[k] = v
}

func HasLabel(obj metav1.Object, label string) (val string, ok bool) {
	if obj == nil {
		return "", false
	}
	if obj.GetLabels() != nil {
		val, ok = obj.GetLabels()[label]
	}
	return val, ok
}

func HasAnnotation(obj metav1.Object, anno string) (val string, ok bool) {
	if obj == nil {
		return "", false
	}
	if obj.GetAnnotations() != nil {
		val, ok = obj.GetAnnotations()[anno]
	}
	return val, ok
}

// GetCapacityOfNode Return the capacity of node resources.
func GetCapacityOfNode(node *corev1.Node, resourceName string) (int64, bool) {
	if node == nil {
		return 0, false
	}
	if val, ok := node.Status.Capacity[corev1.ResourceName(resourceName)]; ok {
		return val.Value(), true
	}
	return 0, false
}

// GetAllocatableOfNode Return the number of resources that can be allocated to the node.
func GetAllocatableOfNode(node *corev1.Node, resourceName string) (int64, bool) {
	if node == nil {
		return 0, false
	}
	if val, ok := node.Status.Allocatable[corev1.ResourceName(resourceName)]; ok {
		return val.Value(), true
	}
	return 0, false
}

// IsVGPUEnabledNode Determine whether there are vGPU devices on the node.
func IsVGPUEnabledNode(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	val, _ := GetAllocatableOfNode(node, VGPUNumberResourceName)
	return val > 0
}

func DelResourceOfContainer(container *corev1.Container, resourceName string) {
	if container == nil {
		return
	}
	if container.Resources.Requests != nil {
		delete(container.Resources.Requests, corev1.ResourceName(resourceName))
	}
	if container.Resources.Limits != nil {
		delete(container.Resources.Limits, corev1.ResourceName(resourceName))
	}
}

// GetResourceOfContainer Return the number of resource limit.
func GetResourceOfContainer(container *corev1.Container, resourceName string) int64 {
	if container == nil {
		return 0
	}
	var count int64
	if val, ok := container.Resources.Limits[corev1.ResourceName(resourceName)]; ok {
		count = val.Value()
	}
	return count
}

// HasExtendedResource Return true when pod requests extended resources
func HasExtendedResource(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	// Extended resources is often defined through limits.
	limits := resource.PodLimits(pod, resource.PodResourcesOptions{})
	for name, qty := range limits {
		if helper.IsExtendedResourceName(name) && qty.Value() > 0 {
			return true
		}
	}
	return false
}

// HasDRARequests Return true when pod requests DRA resourceClaims
func HasDRARequests(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	return len(pod.Spec.ResourceClaims) > 0
}

// IsVGPURequiredContainer tell if the container is a vGPU request container.
func IsVGPURequiredContainer(c *corev1.Container) bool {
	return GetResourceOfContainer(c, VGPUNumberResourceName) > 0
}

// GetResourceOfPod Return the number of resource limit for all containers of Pod.
func GetResourceOfPod(pod *corev1.Pod, resourceName string) int64 {
	if pod == nil {
		return 0
	}
	var total int64
	for i := range pod.Spec.Containers {
		total += GetResourceOfContainer(&pod.Spec.Containers[i], resourceName)
	}
	return total
}

// IsVGPUResourcePod Determine if a pod has vGPU resource request. Init
// containers (including sidecars) are checked alongside regular containers so
// an init-only vGPU pod is recognised; this is kept in lockstep with
// allocator.BuildAllocationRequest (which builds req.Containers from the same
// init+app set) so the filter↔bind predicate-node invariant holds.
func IsVGPUResourcePod(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	return IsVGPUResourcePodSpec(pod.Spec)
}

func IsVGPUResourcePodSpec(spec corev1.PodSpec) bool {
	for i := range spec.Containers {
		if GetResourceOfContainer(&spec.Containers[i], VGPUNumberResourceName) > 0 {
			return true
		}
	}
	for i := range spec.InitContainers {
		if GetResourceOfContainer(&spec.InitContainers[i], VGPUNumberResourceName) > 0 {
			return true
		}
	}
	return false
}

func IsVGPUResourceVcTask(task *v1alpha1.TaskSpec) bool {
	if task == nil {
		return false
	}
	return IsVGPUResourcePodSpec(task.Template.Spec)
}

// IsContainerRunning reports whether the named container is currently in the
// Running state. It searches both init and regular container statuses (names
// are unique pod-wide), so a completed/terminated container — e.g. a finished
// sequential init container — returns false.
func IsContainerRunning(pod *corev1.Pod, containerName string) bool {
	if pod == nil {
		return false
	}
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == containerName {
			return pod.Status.InitContainerStatuses[i].State.Running != nil
		}
	}
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == containerName {
			return pod.Status.ContainerStatuses[i].State.Running != nil
		}
	}
	return false
}

// IsRestartableInitContainer reports whether an init container is a sidecar,
// i.e. it declares restartPolicy: Always. Unlike a regular init container
// (which runs to completion before the app containers start), a sidecar keeps
// running alongside the app containers for the rest of the pod's life, so for
// resource and lifecycle purposes it overlaps the app phase.
var IsRestartableInitContainer = pod.IsRestartableInitContainer

// CollectableContainerNames returns the names of the pod's containers whose
// real-time vGPU usage metrics should be collected right now. Regular
// containers and sidecars (restartable init) run for the app phase and follow
// the pod lifecycle, so they are always included; a sequential
// (non-restartable) init container runs only transiently, so it is included
// only while it is actually Running — once it terminates its (stale) usage
// must stop being reported and its working directory may be reclaimed.
// Init containers come first, matching the device-plugin's per-container layout.
func CollectableContainerNames(pod *corev1.Pod) []string {
	if pod == nil {
		return nil
	}
	names := make([]string, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if IsRestartableInitContainer(c) || IsContainerRunning(pod, c.Name) {
			names = append(names, c.Name)
		}
	}
	for i := range pod.Spec.Containers {
		names = append(names, pod.Spec.Containers[i].Name)
	}
	return names
}

// matchDeviceSelector reports whether deviceValue matches any entry of a
// comma-separated include/exclude annotation, and whether the annotation
// carried any usable entry at all.
//
// need is what keeps a malformed annotation from meaning something drastic.
// "  ", "," and ",," split into nothing but blanks; reporting that as "matched
// nothing" would make an include list reject every device on the node, while
// the same value in an exclude list would do nothing at all. Callers act on
// match only when need is true, so a value with no real entry is treated the
// same as no annotation — which is also what an empty value has always meant.
//
// Both arguments are compared upper-cased, and an entry matches as a substring:
// "A100" selects "NVIDIA A100-SXM4-80GB", and a UUID prefix selects the device
// it belongs to.
func matchDeviceSelector(value, deviceValue string) (match, need bool) {
	for _, entry := range strings.Split(strings.ToUpper(value), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		need = true
		if strings.Contains(deviceValue, entry) {
			return true, true
		}
	}
	return false, need
}

// CheckDeviceType Check if the device type meets expectations.
//
// Include and exclude are independent filters and both apply when both are set:
// a device has to be named by the include list (if that list has any entry) and
// must not be named by the exclude list.
func CheckDeviceType(annotations map[string]string, deviceType string) bool {
	deviceType = strings.ToUpper(strings.TrimSpace(deviceType))
	if includes, ok := annotations[PodIncludeGpuTypeAnnotation]; ok {
		if match, need := matchDeviceSelector(includes, deviceType); need && !match {
			return false
		}
	}
	if excludes, ok := annotations[PodExcludeGpuTypeAnnotation]; ok {
		if match, need := matchDeviceSelector(excludes, deviceType); need && match {
			return false
		}
	}
	return true
}

// CheckDeviceUuid Check if the device uuid meets expectations.
//
// Same rules as CheckDeviceType: both filters apply, and an annotation with no
// usable entry is ignored rather than matching everything or nothing.
func CheckDeviceUuid(annotations map[string]string, deviceUUID string) bool {
	deviceUUID = strings.ToUpper(strings.TrimSpace(deviceUUID))
	if includes, ok := annotations[PodIncludeGPUUUIDAnnotation]; ok {
		if match, need := matchDeviceSelector(includes, deviceUUID); need && !match {
			return false
		}
	}
	if excludes, ok := annotations[PodExcludeGPUUUIDAnnotation]; ok {
		if match, need := matchDeviceSelector(excludes, deviceUUID); need && match {
			return false
		}
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
	if pod == nil {
		return false
	}
	if pod.DeletionTimestamp != nil {
		return true
	}
	if len(pod.Status.ContainerStatuses) > MaxContainerLimit {
		klog.ErrorS(nil, "The number of container exceeds the upper limit", "pod", klog.KObj(pod))
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
	if pod == nil {
		return false
	}
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

func GetPredicateTimeOfPod(pod corev1.Pod) int64 {
	predicateTimeVal, ok := HasAnnotation(&pod, PodPredicateTimeAnnotation)
	if !ok || len(predicateTimeVal) > PodAnnotationMaxLength {
		return math.MaxInt64
	}
	predicateTime, err := strconv.ParseInt(predicateTimeVal, 10, 64)
	if err != nil || predicateTime <= 0 {
		klog.Warningf("failed to parse predicate timestamp %s due to %v", predicateTimeVal, err)
		return math.MaxInt64
	}
	return predicateTime
}

// GetCurrentPodByAllocatingPods find the oldest Pod from the allocating Pods
// to be allocated as the current Pod to be allocated.
func GetCurrentPodByAllocatingPods(allocatingPods []corev1.Pod) (*corev1.Pod, error) {
	switch len(allocatingPods) {
	case 0:
		return nil, fmt.Errorf("unable to find the current pod to be allocated")
	case 1:
		return &allocatingPods[0], nil
	default:
		pods := PodsOrderedByPredicateTime(allocatingPods)
		sort.Sort(pods)
		return &pods[0], nil
	}
}

// FilterAllocatingPods filter out the list of pods to be allocated.
func FilterAllocatingPods(activePods []corev1.Pod) []corev1.Pod {
	var allocatingPods []corev1.Pod
	for i, pod := range activePods {
		klog.V(5).Infof("FilterPod <%s/%s> %s", pod.Namespace, pod.Name, pod.Status.Phase)
		if !IsVGPUResourcePod(&pod) || IsShouldDeletePod(&pod) {
			continue
		}
		if _, ok := HasAnnotation(&pod, PodVGPUPreAllocAnnotation); !ok {
			continue
		}
		if nodeName, ok := HasAnnotation(&pod, PodPredicateNodeAnnotation); !ok {
			continue
		} else if pod.Spec.NodeName != nodeName {
			continue
		}
		if val, ok := HasAnnotation(&pod, PodPredicateTimeAnnotation); !ok {
			continue
		} else {
			predicateTime, err := strconv.ParseInt(val, 10, 64)
			if err != nil || predicateTime <= 0 || predicateTime >= math.MaxInt64 {
				continue
			}
		}
		allocatingPods = append(allocatingPods, activePods[i])
	}
	return allocatingPods
}

func PodPlanSchedulingNode(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	if pod.Spec.NodeName != "" {
		return pod.Spec.NodeName
	}
	predicateNode, _ := HasAnnotation(pod, PodPredicateNodeAnnotation)
	return predicateNode
}

func PodsOnNodeCallback(pods []*corev1.Pod, node *corev1.Node, callbackFn func(*corev1.Pod)) {
	if node == nil {
		klog.Warningln("node is empty")
		return
	}
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
	return filepath.Join(managerBaseDir, fmt.Sprintf("%s_%s", string(podUID), containerName))
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

func ValueEnabled(val string) bool {
	val = strings.TrimSpace(val)
	if val == "" {
		return false
	}
	return val == "1" || strings.EqualFold(val, "enabled") || strings.EqualFold(val, "true")
}

func PodContainerEnvEnabled(pod *corev1.Pod, containerName, envName string) bool {
	if pod == nil {
		return false
	}
	envEnabled := func(cont *corev1.Container) bool {
		return slices.ContainsFunc(cont.Env, func(env corev1.EnvVar) bool {
			return env.Name == envName && ValueEnabled(env.Value)
		})
	}
	// Container names are unique across init and regular containers; search
	// init containers too so the toggle works for an init container once it
	// gets devices allocated. For app containers the result is unchanged.
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == containerName {
			return envEnabled(&pod.Spec.InitContainers[i])
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == containerName {
			return envEnabled(&pod.Spec.Containers[i])
		}
	}
	return false
}

func InformerFactoryHasSynced(factory informers.SharedInformerFactory, ctx context.Context) bool {
	if factory == nil {
		return false
	}
	for _, synced := range factory.WaitForCacheSync(ctx.Done()) {
		if !synced {
			return false
		}
	}
	return true
}

const (
	DNS1123NameMaximumLength         = 63
	DNS1123NotAllowedCharacters      = "[^-a-z0-9]"
	DNS1123NotAllowedStartCharacters = "^[^a-z0-9]+"
	DNS1123NotAllowedEndCharacters   = "[^a-z0-9]+$"
	hashPrefixLength                 = 8
	separator                        = "-"
)

// GenerateK8sSafeResourceName Generate names that comply with the K8s DNS-1123 specification and have a length not exceeding 63
func GenerateK8sSafeResourceName(str ...string) string {
	joined := strings.Join(str, separator)
	if joined == "" {
		return ""
	}

	hashSuffix := GenerateShortHash(joined, hashPrefixLength)

	// Combine the original joined string with the hash suffix.
	nameWithHash := joined + separator + hashSuffix

	// Truncate the final name if it's longer than the maximum allowed length.
	if len(nameWithHash) > DNS1123NameMaximumLength {
		// Calculate the maximum length the prefix can have.
		// The final name will be: truncated_prefix + "-" + hashSuffix
		maxPrefixLen := DNS1123NameMaximumLength - len(hashSuffix) - 1 // -1 for the separator '-'
		// Truncate the part before the hash to fit within the limit.
		truncatedPrefix := nameWithHash[:maxPrefixLen]
		// Re-assemble the name. It might end with '-' from the prefix or start with an invalid char after truncation.
		// We'll pass the whole truncated string to the compliance function.
		nameWithHash = truncatedPrefix + separator + hashSuffix
	}

	// Ensure the final name adheres to DNS-1123 rules.
	return MakeDNS1123Compatible(nameWithHash)
}

// MakeDNS1123Compatible It makes a string compliant with RFC 1123 for use as a DNS subdomain name.
// https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#dns-subdomain-names
func MakeDNS1123Compatible(name string) string {
	name = strings.ToLower(name)

	nameNotAllowedChars := regexp.MustCompile(DNS1123NotAllowedCharacters)
	name = nameNotAllowedChars.ReplaceAllString(name, "")

	nameNotAllowedStartChars := regexp.MustCompile(DNS1123NotAllowedStartCharacters)
	name = nameNotAllowedStartChars.ReplaceAllString(name, "")

	if len(name) > DNS1123NameMaximumLength {
		name = name[0:DNS1123NameMaximumLength]
	}

	nameNotAllowedEndChars := regexp.MustCompile(DNS1123NotAllowedEndCharacters)
	name = nameNotAllowedEndChars.ReplaceAllString(name, "")

	return name
}

// GenerateShortHash Generate a hexadecimal hash prefix of specified length
func GenerateShortHash(input string, length int) string {
	h := sha256.Sum256([]byte(input))
	fullHex := hex.EncodeToString(h[:])
	if length > len(fullHex) {
		length = len(fullHex)
	}
	return fullHex[:length]
}

func GetEnvEnabled(env string) bool {
	if val, ok := os.LookupEnv(env); ok {
		return ValueEnabled(val)
	}
	return false
}

func GetEnvDefault(env, defaultValue string) string {
	if val, ok := os.LookupEnv(env); ok {
		return strings.TrimSpace(val)
	}
	return defaultValue
}

func EnsureDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return os.Chmod(path, perm)
	}
	return nil
}

func CountReservedPods(claim *resourceapi.ResourceClaim) int {
	var count = 0
	if claim == nil {
		return count
	}
	for _, reference := range claim.Status.ReservedFor {
		if reference.APIGroup == "" && reference.Resource == "pods" {
			count++
		}
	}
	return count
}

func NewMirrorIndexer(informer cache.Informer) (k8scache.Indexer, k8scache.ResourceEventHandlerRegistration, error) {
	indexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{})
	registration, err := informer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if err := indexer.Add(obj); err != nil {
				utilruntime.HandleErrorWithLogger(klog.Background(), err, "add object to mirror indexer")
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if err := indexer.Update(newObj); err != nil {
				utilruntime.HandleErrorWithLogger(klog.Background(), err, "update object in mirror indexer")
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := k8scache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err != nil {
				utilruntime.HandleErrorWithLogger(klog.Background(), err, "build delete key for mirror indexer")
				return
			}
			storedObj, exists, err := indexer.GetByKey(key)
			if err != nil {
				utilruntime.HandleErrorWithLogger(klog.Background(), err, "get object from mirror indexer by key")
				return
			}
			if !exists {
				return
			}
			if err := indexer.Delete(storedObj); err != nil {
				utilruntime.HandleErrorWithLogger(klog.Background(), err, "delete object from mirror indexer")
			}
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return indexer, registration, nil
}

func ObjectKeys[T client.Object](objects ...T) []string {
	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, client.ObjectKeyFromObject(object).String())
	}
	return keys
}

func PodHasGangName(pod *corev1.Pod) (string, bool) {
	if pod == nil {
		return "", false
	}
	// Native Gang Scheduling. An empty name must NOT win: it would report
	// membership in a nameless gang and, worse, short-circuit the label /
	// annotation / ownerReference fallbacks below.
	if pod.Spec.SchedulingGroup != nil && pod.Spec.SchedulingGroup.PodGroupName != nil &&
		*pod.Spec.SchedulingGroup.PodGroupName != "" {
		return *pod.Spec.SchedulingGroup.PodGroupName, true
	}
	for _, labelKey := range []string{CoschedulingPodGroupLabel, CoschedulingPodGroupNameLabel} {
		if val, ok := HasLabel(pod, labelKey); ok && val != "" {
			return val, true
		}
	}
	for _, annoKey := range []string{KubeGroupNameAnnotation, VolcanoGroupNameAnnotation, KoordinatorGangNameAnnotation} {
		if val, ok := HasAnnotation(pod, annoKey); ok && val != "" {
			return val, true
		}
	}
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "PodGroup" && ref.Name != "" {
			return ref.Name, true
		}
	}
	return "", false
}

// PodGangKey returns the pod's gang identity as a namespace-qualified
// "<namespace>/<name>" key, and whether the pod belongs to a gang at all.
//
// Use this -- not PodHasGangName -- whenever the value is used to decide
// SAMENESS between two pods (indexing siblings, comparing a candidate against
// req.GangName, protecting brother pods from preemption). Every mechanism
// PodHasGangName understands names a PodGroup that is a NAMESPACED object:
// coscheduling's label, Volcano's / Koordinator's / kube-batch's annotation,
// the native spec.schedulingGroup field, and a PodGroup ownerReference all
// resolve within the pod's own namespace. The bare name is therefore not a
// cluster-unique identity, and matching on it makes pods of two unrelated
// gangs that merely share a name -- "training", or a workload name repeated
// per tenant, which is the norm in multi-tenant clusters -- look like siblings
// of each other.
//
// The raw value is NORMALIZED first, because the annotation-based dialects do
// not agree on a spelling -- see normalizeGangKey. Two pods of one gang written
// two different ways must still produce the same key.
//
// PodHasGangName remains the right call for display: it is what the user wrote.
func PodGangKey(pod *corev1.Pod) (string, bool) {
	name, ok := PodHasGangName(pod)
	if !ok || strings.TrimSpace(name) == "" {
		return "", false
	}
	key := normalizeGangKey(pod.Namespace, name)
	// Nothing usable survived the fold (the reference was punctuation only,
	// e.g. "/"). Report non-membership rather than hand back a name-less key
	// that every equally-degenerate pod in the namespace would match.
	if strings.HasSuffix(key, "/") {
		return "", false
	}
	return key, true
}

// normalizeGangKey folds every spelling of a gang reference onto one
// "<namespace>/<name>" key.
//
// Label-based dialects can only ever carry a bare PodGroup name (a Kubernetes
// label VALUE may not contain '/'), but the annotation-based ones -- Volcano,
// Koordinator, kube-batch -- are free-form and accept either spelling:
//
//	training         -> <pod namespace>/training
//	team-a/training  -> team-a/training
//
// Without folding, a gang whose pods spell the reference both ways inside one
// namespace would split into two gangs -- the opposite failure from the
// cross-namespace collision this key exists to prevent.
//
// An explicit namespace is honoured rather than overridden by the pod's own:
// that is what the author asked for, and for the overwhelmingly common case
// (the qualified namespace IS the pod's namespace) the two agree anyway.
func normalizeGangKey(podNamespace, raw string) string {
	value := strings.TrimSpace(raw)
	if namespace, name, found := strings.Cut(value, "/"); found {
		namespace, name = strings.TrimSpace(namespace), strings.TrimSpace(name)
		if namespace != "" && name != "" {
			return namespace + "/" + name
		}
		// Degenerate ("/x" or "x/"): fall through and treat the non-empty half
		// as a bare name in the pod's own namespace rather than inventing an
		// empty-namespace key that matches nothing.
		if name != "" {
			value = name
		} else {
			value = namespace
		}
	}
	return podNamespace + "/" + value
}

func SafeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// PodVGPUAccessMode returns the vGPU access mode a pod (or pod template) asks
// for via VGPUAccessModeAnnotation: AccessModeLocal when absent, an error for
// any other value than local/remote.
func PodVGPUAccessMode(obj metav1.Object) (string, error) {
	value, ok := HasAnnotation(obj, VGPUAccessModeAnnotation)
	if !ok || strings.TrimSpace(value) == "" {
		return AccessModeLocal, nil
	}
	switch mode := strings.ToLower(strings.TrimSpace(value)); mode {
	case AccessModeLocal, AccessModeRemote:
		return mode, nil
	default:
		return AccessModeLocal, fmt.Errorf("invalid annotation %s=%q: must be %q or %q",
			VGPUAccessModeAnnotation, value, AccessModeLocal, AccessModeRemote)
	}
}
