package bind

import (
	"context"
	"fmt"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/predicate"
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
	recorder   record.EventRecorder
}

const Name = "BindPredicate"

var _ predicate.BindPredicate = &nodeBinding{}

func New(client kubernetes.Interface, recorder record.EventRecorder, serialBindNode bool) (*nodeBinding, error) {
	minLockingDuration := 30 * time.Millisecond
	locker := serial.NewLocker(serial.WithName(Name),
		serial.WithEnabled(serialBindNode),
		serial.WithLockDuration(&minLockingDuration))
	return &nodeBinding{
		locker:     locker,
		kubeClient: client,
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
	b.locker.Lock(args.Node)
	defer b.locker.Unlock(args.Node)

	klog.V(4).InfoS("BindingNode", "ExtenderBindingArgs", args)
	var (
		binding = &corev1.Binding{
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
		pod *corev1.Pod
		err error
	)

	pod, err = b.kubeClient.CoreV1().Pods(args.PodNamespace).Get(ctx, args.PodName, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "KubeClient Get target Pod failed", "targetPod",
			fmt.Sprintf("%s/%s", args.PodNamespace, args.PodName))
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}
	}
	if pod.UID != args.PodUID {
		errMessage := fmt.Sprintf("different UID from the target pod: "+
			"current: %s, target: %s", pod.UID, args.PodUID)
		klog.Errorln(errMessage)
		return &extenderv1.ExtenderBindingResult{Error: errMessage}
	}
	nodeName, ok := util.HasAnnotation(pod, util.PodPredicateNodeAnnotation)
	if ok && nodeName != args.Node {
		err = fmt.Errorf("predicate node is different from the node to be bound")
		klog.Warningf("Pod <%s> %s", klog.KObj(pod), err.Error())
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}
	}

	if err = client.PatchPodAllocationAllocating(b.kubeClient, pod); err != nil {
		err = fmt.Errorf("patch vgpu metadata failed: %v", err)
		klog.Errorf("Patch Pod <%s> metadata failed: %v", klog.KObj(pod), err)
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}
	}

	err = b.kubeClient.CoreV1().Pods(args.PodNamespace).Bind(ctx, binding, metav1.CreateOptions{})
	if err != nil {
		klog.Errorf("Pod <%s/%s> binding Node <%s> failed: %v", args.PodNamespace, args.PodName, args.Node, err)
		b.recorder.Event(pod, corev1.EventTypeWarning, "BindingFailed", err.Error())
		// patch failed metadata
		if patchErr := client.PatchPodAllocationFailed(b.kubeClient, pod); patchErr != nil {
			klog.ErrorS(err, "PatchPodAllocationFailed", "pod", klog.KObj(pod))
		}
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}
	}

	b.recorder.Eventf(pod, corev1.EventTypeNormal, "BindingSucceed",
		"Successfully binding <%s> to node <%s>", klog.KObj(pod), args.Node)
	klog.V(3).Infof("Pod <%s> binding Node <%s> successful", klog.KObj(pod), args.Node)
	return &extenderv1.ExtenderBindingResult{}
}
