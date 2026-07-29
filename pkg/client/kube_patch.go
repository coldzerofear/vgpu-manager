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
	"k8s.io/utils/pointer"
)

type PatchMetadata struct {
	Annotations map[string]*string `json:"annotations,omitempty"`
	Labels      map[string]*string `json:"labels,omitempty"`
}

func (p PatchMetadata) PatchType() k8stypes.PatchType {
	return k8stypes.MergePatchType
}

func (p PatchMetadata) JSONBytes() ([]byte, error) {
	type patchPod struct {
		Metadata PatchMetadata `json:"metadata"`
	}
	patch := patchPod{
		Metadata: p,
	}
	return json.Marshal(patch)
}

func PatchPodMetadata(kubeClient kubernetes.Interface, pod *corev1.Pod, patchMetadata PatchMetadata) error {
	bytes, err := patchMetadata.JSONBytes()
	if err != nil {
		return err
	}
	rsPod, err := kubeClient.CoreV1().Pods(pod.Namespace).
		Patch(context.Background(), pod.Name, patchMetadata.PatchType(), bytes, metav1.PatchOptions{})
	if err == nil {
		rsPod.DeepCopyInto(pod)
	}
	return err
}

func PatchNodeMetadata(kubeClient kubernetes.Interface, nodeName string, patchMetadata PatchMetadata) error {
	bytes, err := patchMetadata.JSONBytes()
	if err != nil {
		return err
	}
	_, err = kubeClient.CoreV1().Nodes().
		Patch(context.Background(), nodeName, patchMetadata.PatchType(), bytes, metav1.PatchOptions{})
	return err
}

// PatchPodAllocationSucceed patch pod metadata marking device allocation successful.
func PatchPodAllocationSucceed(kubeClient kubernetes.Interface, pod *corev1.Pod) error {
	preAlloc, _ := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	preDevices := device.PodDeviceClaim{}
	if err := preDevices.UnmarshalText(preAlloc); err != nil {
		return fmt.Errorf("parse pod pre device claims failed: %v", err)
	}

	realAlloc, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	realDevices := device.PodDeviceClaim{}
	if err := realDevices.UnmarshalText(realAlloc); err != nil {
		return fmt.Errorf("parse pod real device claims failed: %v", err)
	}

	assignedPhase := util.AssignPhaseAllocating
	predicateTime, _ := util.HasAnnotation(pod, util.PodPredicateTimeAnnotation)
	// All containers have been allocated.
	if len(realDevices) >= len(preDevices) {
		assignedPhase = util.AssignPhaseSucceed
		predicateTime = fmt.Sprintf("%d", uint64(math.MaxInt64))
	}
	patchData := PatchMetadata{
		Labels: map[string]*string{
			util.PodAssignedPhaseLabel: pointer.String(string(assignedPhase)),
		},
		Annotations: map[string]*string{
			util.PodVGPURealAllocAnnotation: pointer.String(realAlloc),
			util.PodPredicateTimeAnnotation: pointer.String(predicateTime),
		},
	}
	if len(pod.Spec.NodeName) > 0 {
		// Covering to correct certain possible errors
		patchData.Labels[util.PodMetricsNodeLabel] = &pod.Spec.NodeName
	}
	return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return PatchPodMetadata(kubeClient, pod, patchData)
	})
}

// PatchPodAllocationAllocating patch pod metadata marking device allocation allocating.
func PatchPodAllocationAllocating(kubeClient kubernetes.Interface, pod *corev1.Pod) error {
	var (
		nodeName      *string
		assignedPhase *string
		predicateTime *string
		needPatch     bool
	)
	if util.IsVGPUResourcePod(pod) {
		needPatch = true
		assignedPhase = pointer.String(string(util.AssignPhaseAllocating))
		predicateTime = pointer.String(fmt.Sprintf("%d", metav1.NowMicro().UnixNano()))
		if node, _ := util.HasAnnotation(pod, util.PodPredicateNodeAnnotation); len(node) > 0 {
			nodeName = &node // Covering to correct certain possible errors
		}
	} else {
		// If a non vGPU Pod carries this metadata, it needs to be cleaned up
		_, ok1 := util.HasLabel(pod, util.PodMetricsNodeLabel)
		_, ok2 := util.HasLabel(pod, util.PodAssignedPhaseLabel)
		_, ok3 := util.HasAnnotation(pod, util.PodPredicateTimeAnnotation)
		needPatch = ok1 || ok2 || ok3
	}
	if needPatch {
		patchData := PatchMetadata{
			Labels: map[string]*string{
				util.PodMetricsNodeLabel:   nodeName,
				util.PodAssignedPhaseLabel: assignedPhase,
			},
			Annotations: map[string]*string{
				util.PodPredicateTimeAnnotation: predicateTime,
			},
		}
		return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
			return PatchPodMetadata(kubeClient, pod, patchData)
		})
	}
	return nil
}

// PatchPodAllocationFailed patch pod metadata marking device allocation failed.
func PatchPodAllocationFailed(kubeClient kubernetes.Interface, pod *corev1.Pod) error {
	var (
		assignedPhase *string
		predicateTime *string
		needPatch     bool
	)
	if util.IsVGPUResourcePod(pod) {
		needPatch = true
		assignedPhase = pointer.String(string(util.AssignPhaseFailed))
		predicateTime = pointer.String(fmt.Sprintf("%d", uint64(math.MaxInt64)))
	} else {
		// If a non vGPU Pod carries this metadata, it needs to be cleaned up
		_, ok1 := util.HasLabel(pod, util.PodMetricsNodeLabel)
		_, ok2 := util.HasLabel(pod, util.PodAssignedPhaseLabel)
		_, ok3 := util.HasAnnotation(pod, util.PodPredicateTimeAnnotation)
		needPatch = ok1 || ok2 || ok3
	}
	if needPatch {
		patchData := PatchMetadata{
			Labels: map[string]*string{
				// Device allocation failed, the tag should be deleted to prevent the monitoring from scanning it again
				util.PodMetricsNodeLabel:   nil,
				util.PodAssignedPhaseLabel: assignedPhase,
			},
			Annotations: map[string]*string{
				util.PodPredicateTimeAnnotation: predicateTime,
			},
		}
		return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
			return PatchPodMetadata(kubeClient, pod, patchData)
		})
	}
	return nil
}

// PatchPodPreAllocatedMetadata patch vGPU pre allocated metadata annotations
func PatchPodPreAllocatedMetadata(kubeClient kubernetes.Interface, pod *corev1.Pod) error {
	nodeName := pod.Annotations[util.PodPredicateNodeAnnotation]
	preAlloc := pod.Annotations[util.PodVGPUPreAllocAnnotation]
	if len(preAlloc) > util.PodAnnotationMaxLength {
		return fmt.Errorf("pre allocated device value is too long")
	}
	// Stamp the current Filter wall-clock time. ShouldCountPodDeviceAllocation
	// uses this as both:
	//   - the "filter ran after condition was set" signal (compared against
	//     PodScheduled.LastTransitionTime), to ignore stale Unschedulable
	//     conditions left by a previous failed cycle, and
	//   - the bind-window grace input (compared against time.Now()), to free
	//     the GPU once a pod has been stuck for longer than a bind could
	//     plausibly take. Kubernetes does NOT advance LastTransitionTime on
	//     repeated same-status failures, so the wall-clock difference is what
	//     lets us distinguish "just pre-allocated, bind in progress" from
	//     "stuck across many failed cycles".
	predicateTime := fmt.Sprintf("%d", uint64(metav1.NowMicro().UnixNano()))
	patchData := PatchMetadata{
		Labels: map[string]*string{
			// Enable monitoring to identify this pod
			util.PodMetricsNodeLabel:   &nodeName,
			util.PodAssignedPhaseLabel: pointer.String(string(util.AssignPhaseFiltering)),
		},
		Annotations: map[string]*string{
			util.PodPredicateNodeAnnotation: &nodeName,
			util.PodVGPUPreAllocAnnotation:  &preAlloc,
			util.PodVGPURealAllocAnnotation: pointer.String(""),
			util.PodPredicateTimeAnnotation: &predicateTime,
		},
	}
	return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return PatchPodMetadata(kubeClient, pod, patchData)
	})
}
