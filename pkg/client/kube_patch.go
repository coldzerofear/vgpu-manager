package client

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

type PatchMetadata struct {
	Annotations map[string]string `json:"annotations,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

func PatchPodMetadata(kubeClient kubernetes.Interface, pod *corev1.Pod, patchMetadata PatchMetadata) error {
	type patchPod struct {
		Metadata PatchMetadata `json:"metadata"`
	}
	p := patchPod{
		Metadata: patchMetadata,
	}

	bytes, err := json.Marshal(p)
	if err != nil {
		return err
	}
	rsPod, err := kubeClient.CoreV1().Pods(pod.Namespace).
		Patch(context.Background(), pod.Name, k8stypes.StrategicMergePatchType, bytes, metav1.PatchOptions{})
	if err == nil {
		rsPod.DeepCopyInto(pod)
	}
	return err
}

func PatchNodeMetadata(kubeClient kubernetes.Interface, nodeName string, patchMetadata PatchMetadata) error {
	type patchNode struct {
		Metadata PatchMetadata `json:"metadata"`
	}
	p := patchNode{
		Metadata: patchMetadata,
	}

	bytes, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = kubeClient.CoreV1().Nodes().
		Patch(context.Background(), nodeName, k8stypes.StrategicMergePatchType, bytes, metav1.PatchOptions{})
	return err
}

// PatchPodAllocationSucceed patch pod metadata marking device allocation successful.
func PatchPodAllocationSucceed(kubeClient kubernetes.Interface, pod *corev1.Pod) error {
	preAlloc, _ := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	preDevices := device.PodDevices{}
	_ = preDevices.UnmarshalText(preAlloc)

	realAlloc, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	realDevices := device.PodDevices{}
	_ = realDevices.UnmarshalText(realAlloc)

	assignedPhase := util.AssignPhaseAllocating
	predicateTime, _ := util.HasAnnotation(pod, util.PodPredicateTimeAnnotation)
	// All containers have been allocated.
	if len(realDevices) >= len(preDevices) {
		assignedPhase = util.AssignPhaseSucceed
		predicateTime = fmt.Sprintf("%d", uint64(math.MaxUint64))
	}
	patchData := PatchMetadata{
		Labels: map[string]string{
			util.PodAssignedPhaseLabel: string(assignedPhase),
		},
		Annotations: map[string]string{
			util.PodVGPURealAllocAnnotation: realAlloc,
			util.PodPredicateTimeAnnotation: predicateTime,
		},
	}
	return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return PatchPodMetadata(kubeClient, pod, patchData)
	})
}

// PatchPodAllocationAllocating patch pod metadata marking device allocation allocating.
func PatchPodAllocationAllocating(kubeClient kubernetes.Interface, pod *corev1.Pod) error {
	assignedPhase := util.AssignPhaseSucceed
	predicateTime := fmt.Sprintf("%d", uint64(math.MaxUint64))
	if util.IsVGPUResourcePod(pod) {
		assignedPhase = util.AssignPhaseAllocating
		predicateTime = fmt.Sprintf("%d", metav1.NowMicro().UnixNano())
	}
	patchData := PatchMetadata{
		Labels:      map[string]string{util.PodAssignedPhaseLabel: string(assignedPhase)},
		Annotations: map[string]string{util.PodPredicateTimeAnnotation: predicateTime},
	}
	return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return PatchPodMetadata(kubeClient, pod, patchData)
	})
}

// PatchPodAllocationFailed patch pod metadata marking device allocation failed.
func PatchPodAllocationFailed(kubeClient kubernetes.Interface, pod *corev1.Pod) error {
	patchData := PatchMetadata{
		Labels: map[string]string{
			util.PodAssignedPhaseLabel: string(util.AssignPhaseFailed),
		},
		Annotations: map[string]string{
			util.PodPredicateTimeAnnotation: fmt.Sprintf("%d", uint64(math.MaxUint64)),
		},
	}
	return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return PatchPodMetadata(kubeClient, pod, patchData)
	})
}

// PatchPodVGPUAnnotation patch pod vGPU scheduling and allocation metadata.
func PatchPodVGPUAnnotation(kubeClient kubernetes.Interface, pod *corev1.Pod) error {
	patchData := PatchMetadata{Annotations: map[string]string{}}
	nodeName := pod.Annotations[util.PodPredicateNodeAnnotation]
	patchData.Annotations[util.PodPredicateNodeAnnotation] = nodeName
	preAlloc := pod.Annotations[util.PodVGPUPreAllocAnnotation]
	patchData.Annotations[util.PodVGPUPreAllocAnnotation] = preAlloc
	return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return PatchPodMetadata(kubeClient, pod, patchData)
	})
}
