package bind

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/predicate"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

type Lock struct {
	sync.Mutex
	timestamp int64
	enable    bool
}

func NewLock(enable bool) *Lock {
	return &Lock{
		enable: enable,
	}
}

func (l *Lock) Lock() {
	if l.enable {
		l.Mutex.Lock()
	}
	l.timestamp = time.Now().UnixMicro()
}

func (l *Lock) Unlock() {
	since := time.Since(time.UnixMicro(l.timestamp))
	l.timestamp = 0
	if l.enable {
		klog.V(5).Infof("Binding node took %d milliseconds", since.Milliseconds())
		if sleepTimestamp := 30*time.Millisecond - since; sleepTimestamp > 0 {
			time.Sleep(sleepTimestamp)
		}
		l.Mutex.Unlock()
	}
}

type nodeBinding struct {
	sync.Locker
	kubeClient kubernetes.Interface
	recorder   record.EventRecorder
}

const Name = "BindPredicate"

var _ predicate.BindPredicate = &nodeBinding{}

func New(client kubernetes.Interface, recorder record.EventRecorder, serialBindNode bool) (*nodeBinding, error) {
	return &nodeBinding{
		Locker:     NewLock(serialBindNode),
		kubeClient: client,
		recorder:   recorder,
	}, nil
}

func (b *nodeBinding) Name() string {
	return Name
}

func (b *nodeBinding) Bind(ctx context.Context, args extenderv1.ExtenderBindingArgs) *extenderv1.ExtenderBindingResult {
	b.Lock()
	defer b.Unlock()

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
		klog.ErrorS(err, "Get target Pod <%s/%s> failed", args.PodNamespace, args.PodName)
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}
	}
	if pod.UID != args.PodUID {
		errMessage := fmt.Sprintf("different UID from the target pod: "+
			"current: %s, target: %s", pod.UID, args.PodUID)
		klog.Errorf(errMessage)
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
		_ = client.PatchPodAllocationFailed(b.kubeClient, pod)
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}
	}

	b.recorder.Eventf(pod, corev1.EventTypeNormal, "BindingSucceed",
		"Successfully binding <%s> to node <%s>", klog.KObj(pod), args.Node)
	klog.V(3).Infof("Pod <%s> binding Node <%s> successful", klog.KObj(pod), args.Node)
	return &extenderv1.ExtenderBindingResult{}
}
