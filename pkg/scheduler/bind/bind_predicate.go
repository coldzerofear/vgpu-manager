package bind

import (
	"context"
	"fmt"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/predicate"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

type nodeBinding struct {
	kubeClient kubernetes.Interface
	recorder   record.EventRecorder
}

const Name = "BindPredicate"

var _ predicate.BindPredicate = &nodeBinding{}

func New(client kubernetes.Interface, recorder record.EventRecorder) (*nodeBinding, error) {
	return &nodeBinding{
		kubeClient: client,
		recorder:   recorder,
	}, nil
}

func (b *nodeBinding) Name() string {
	return Name
}

// TODO 在绑定节点前，尝试对节点加锁
func (b *nodeBinding) Bind(args extenderv1.ExtenderBindingArgs) *extenderv1.ExtenderBindingResult {
	klog.V(4).InfoS("BindNode", "args", args)
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

	pod, err := b.kubeClient.CoreV1().Pods(args.PodNamespace).Get(context.Background(), args.PodName, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "get target Pod <%s/%s> failed", args.PodNamespace, args.PodName)
		return &extenderv1.ExtenderBindingResult{
			Error: err.Error(),
		}
	}
	if pod.UID != args.PodUID {
		err = fmt.Errorf("different UID from the target pod: "+
			"current: %s, target: %s", pod.UID, args.PodUID)
		klog.Errorf(err.Error())
		return &extenderv1.ExtenderBindingResult{
			Error: err.Error(),
		}
	}
	nodeName, ok := util.HasAnnotation(pod, util.PodPredicateNodeAnnotation)
	if ok && nodeName != args.Node {
		err := fmt.Errorf("predicate node is different from the node to be bound")
		klog.Warningf("Pod <%s/%s> %s", pod.Namespace, pod.Name, err.Error())
		return &extenderv1.ExtenderBindingResult{
			Error: err.Error(),
		}
	}
	patchData := client.PatchMetadata{
		Labels: map[string]string{
			util.PodAssignedPhaseLabel: string(util.AssignPhaseAllocating),
		},
		Annotations: map[string]string{
			util.PodPredicateTimeAnnotation: fmt.Sprintf("%d", metav1.NowMicro().UnixMicro()),
		},
	}
	err = retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return client.PatchPodMetadata(b.kubeClient, pod, patchData)
	})
	if err != nil {
		klog.Errorf("Patch Pod <%s/%s> metadata failed: %v", args.PodNamespace, args.PodName, err)
		return &extenderv1.ExtenderBindingResult{
			Error: fmt.Sprintf("Patch vgpu metadata failed: %v", err),
		}
	}
	err = b.kubeClient.CoreV1().Pods(args.PodNamespace).Bind(context.Background(), binding, metav1.CreateOptions{})
	if err != nil {
		klog.Errorf("Pod <%s/%s> binding Node <%s> failed: %v", args.PodNamespace, args.PodName, args.Node, err)
		b.recorder.Event(pod, corev1.EventTypeWarning, "BindingFailed", err.Error())
		return &extenderv1.ExtenderBindingResult{
			Error: err.Error(),
		}
	}
	b.recorder.Eventf(pod, corev1.EventTypeNormal, "BindingSucceed", "Successfully binding <%s/%s> to node <%s>", pod.Namespace, pod.Name, args.Node)
	klog.V(3).Infof("Pod <%s/%s> binding Node <%s> successful", args.PodNamespace, args.PodName, args.Node)
	return &extenderv1.ExtenderBindingResult{}
}
