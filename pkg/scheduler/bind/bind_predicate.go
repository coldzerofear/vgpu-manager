package bind

import (
	"context"
	"fmt"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/predicate"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/serial"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

type nodeBinding struct {
	locker     *serial.Locker
	kubeClient kubernetes.Interface
	podLister  client.PodLister
	recorder   record.EventRecorder
}

const Name = "BindPredicate"

var _ predicate.BindPredicate = &nodeBinding{}

func New(client kubernetes.Interface, recorder record.EventRecorder, podLister client.PodLister, serialBindNode bool) (*nodeBinding, error) {
	minLockingDuration := 30 * time.Millisecond
	locker := serial.NewLocker(serial.WithName(Name),
		serial.WithEnabled(serialBindNode),
		serial.WithLockDuration(&minLockingDuration))
	return &nodeBinding{
		locker:     locker,
		kubeClient: client,
		podLister:  podLister,
		recorder:   recorder,
	}, nil
}

func (b *nodeBinding) Name() string {
	return Name
}

func (b *nodeBinding) IsReady(ctx context.Context) bool {
	return true
}

func (b *nodeBinding) Bind(ctx context.Context, args extenderv1.ExtenderBindingArgs) *extenderv1.ExtenderBindingResult {
	klog.V(4).InfoS("BindingNode", "ExtenderBindingArgs", args)
	if len(args.Node) == 0 {
		return &extenderv1.ExtenderBindingResult{Error: "ExtenderBindingArgs.Node cannot be empty"}
	}

	b.locker.Lock(args.Node)
	defer b.locker.Unlock(args.Node)

	var (
		pod *corev1.Pod
		err error
	)

	defer func() {
		if pod != nil && b.podLister != nil {
			b.podLister.Mutation(pod)
		}
	}()
	pod, err = b.kubeClient.CoreV1().Pods(args.PodNamespace).Get(ctx, args.PodName, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "kubeClient get target pod failed", "targetPod",
			fmt.Sprintf("%s/%s", args.PodNamespace, args.PodName))
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}
	}
	if pod.UID != args.PodUID {
		msg := "different UID from the target pod"
		klog.InfoS(msg, "pod", klog.KObj(pod), "current", pod.UID, "target", args.PodUID)
		return &extenderv1.ExtenderBindingResult{Error: msg}
	}
	if util.IsVGPUResourcePod(pod) {
		nodeName, _ := util.HasAnnotation(pod, util.PodPredicateNodeAnnotation)
		if nodeName != args.Node {
			err = fmt.Errorf("predicate node and binding node do not match")
			klog.ErrorS(err, "", "pod", klog.KObj(pod), "predicateNode", nodeName, "binding node", args.Node)
			b.recorder.Event(pod, corev1.EventTypeWarning, reason.EventBindingFailed, err.Error())
			// patch failed metadata
			if patchErr := client.PatchPodAllocationFailed(b.kubeClient, pod); patchErr != nil {
				klog.ErrorS(patchErr, "PatchPodAllocationFailed", "pod", klog.KObj(pod))
			}
			return &extenderv1.ExtenderBindingResult{Error: err.Error()}
		}
		// Check to prevent node overallocation
		if !device.ShouldCountPodDeviceAllocation(pod) {
			err = fmt.Errorf("device pre allocation failed, unable to bind to node <%s>", nodeName)
			klog.ErrorS(err, "", "pod", klog.KObj(pod))
			b.recorder.Event(pod, corev1.EventTypeWarning, reason.EventBindingFailed, err.Error())
			return &extenderv1.ExtenderBindingResult{Error: err.Error()}
		}
	}

	if err = client.PatchPodAllocationAllocating(b.kubeClient, pod); err != nil {
		err = fmt.Errorf("patch vgpu metadata failed: %v", err)
		klog.Errorf("Patch Pod <%s> metadata failed: %v", klog.KObj(pod), err)
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}
	}

	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      args.PodName,
			Namespace: args.PodNamespace,
			UID:       args.PodUID,
		},
		Target: corev1.ObjectReference{
			Kind: "Node",
			Name: args.Node,
		},
	}

	err = b.kubeClient.CoreV1().Pods(args.PodNamespace).Bind(ctx, binding, metav1.CreateOptions{})
	if err != nil {
		klog.Errorf("Pod <%s/%s> binding Node <%s> failed: %v", args.PodNamespace, args.PodName, args.Node, err)
		b.recorder.Event(pod, corev1.EventTypeWarning, reason.EventBindingFailed, err.Error())
		// patch failed metadata
		if patchErr := client.PatchPodAllocationFailed(b.kubeClient, pod); patchErr != nil {
			klog.ErrorS(patchErr, "PatchPodAllocationFailed", "pod", klog.KObj(pod))
		}
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}
	}

	b.recorder.Eventf(pod, corev1.EventTypeNormal, reason.EventBindingSucceed,
		"Successfully bound pod %q to node %q", klog.KObj(pod), args.Node)
	klog.V(3).Infof("Pod <%s> binding Node <%s> successful", klog.KObj(pod), args.Node)
	return &extenderv1.ExtenderBindingResult{}
}
