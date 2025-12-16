package client

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/types"
)

// Reason is the reason reported back in status.
const Reason = "Evicted"

type Eviction interface {
	Evict(ctx context.Context, pod *corev1.Pod, eventRecorder record.EventRecorder, gracePeriodSeconds int64, evictMsg string) bool
}

func GetEvictionVersion(kubeClient kubernetes.Interface) (string, error) {
	resourceList, err := kubeClient.Discovery().ServerResourcesForGroupVersion("v1")
	if err != nil {
		return "", err
	}

	for _, apiResource := range resourceList.APIResources {
		if apiResource.Name == "pods/eviction" && apiResource.Kind == "Eviction" {
			return apiResource.Version, nil
		}
	}
	return "", fmt.Errorf("eviction version not found")
}

type eviction struct {
	kubeClient      kubernetes.Interface
	nodeName        string
	evictionVersion string
}

func NewEviction(client kubernetes.Interface, nodeName string) (Eviction, error) {
	e := &eviction{
		kubeClient: client,
		nodeName:   nodeName,
	}
	version, err := GetEvictionVersion(client)
	if err != nil {
		klog.ErrorS(err, "Failed to get eviction version")
		return nil, err
	}
	e.evictionVersion = version
	return e, nil
}

func (e *eviction) evictPod(ctx context.Context, gracePeriodSeconds *int64, pod *corev1.Pod) error {
	if *gracePeriodSeconds < int64(0) {
		*gracePeriodSeconds = int64(0)
	}

	switch e.evictionVersion {
	case "v1":
		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
			DeleteOptions: &metav1.DeleteOptions{
				GracePeriodSeconds: gracePeriodSeconds,
			},
		}
		return e.kubeClient.PolicyV1().Evictions(pod.Namespace).Evict(ctx, eviction)
	case "v1beta1":
		eviction := &policyv1beta1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
			DeleteOptions: &metav1.DeleteOptions{
				GracePeriodSeconds: gracePeriodSeconds,
			},
		}
		return e.kubeClient.PolicyV1beta1().Evictions(pod.Namespace).Evict(ctx, eviction)
	default:
		return fmt.Errorf("unsupported eviction version: %s", e.evictionVersion)
	}
}

func (e *eviction) Evict(ctx context.Context, pod *corev1.Pod, eventRecorder record.EventRecorder, gracePeriodSeconds int64, evictMsg string) bool {
	if types.IsCriticalPod(pod) {
		klog.ErrorS(nil, "Cannot evict a critical pod", "pod", klog.KObj(pod))
		return false
	}

	if err := e.evictPod(ctx, &gracePeriodSeconds, pod); err != nil {
		klog.ErrorS(err, "Failed to evict pod", "pod", klog.KObj(pod))
		return false
	}

	eventRecorder.Eventf(pod, corev1.EventTypeWarning, Reason, evictMsg)
	klog.InfoS("Successfully evicted pod", "pod", klog.KObj(pod))
	return true
}
