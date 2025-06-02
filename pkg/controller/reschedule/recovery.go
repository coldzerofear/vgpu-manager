package reschedule

import (
	"context"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type recoveryController struct {
	client             client.Client
	recorder           record.EventRecorder
	queue              workqueue.TypedRateLimitingInterface[*corev1.Pod]
	recoveryCheckpoint *recoveryCheckpoint
}

func newRecoveryController(client client.Client, recorder record.EventRecorder) (*recoveryController, error) {
	checkpoint, err := newRecoveryCheckpoint()
	if err != nil {
		return nil, err
	}
	return &recoveryController{
		client:             client,
		recorder:           recorder,
		recoveryCheckpoint: checkpoint,
		queue: workqueue.NewTypedRateLimitingQueue[*corev1.Pod](
			workqueue.DefaultTypedControllerRateLimiter[*corev1.Pod]()),
	}, nil
}

func (r *recoveryController) AddPodToRecoveryQueue(pod *corev1.Pod, d time.Duration) {
	if err := r.recoveryCheckpoint.AddPod(pod); err != nil {
		klog.ErrorS(err, "add pod to recovery checkpoint failed")
	}
	r.queue.AddAfter(pod, d)
}

var _ manager.Runnable = &recoveryController{}

func (r *recoveryController) Start(ctx context.Context) error {
	klog.V(3).Infoln("Starting pod recovery controller")
	go wait.UntilWithContext(ctx, r.runWorker(), time.Second)

	// First startup, insert pods from the checkpoint into the recovery queue
	pods, err := r.recoveryCheckpoint.ListPod()
	if err != nil {
		klog.ErrorS(err, "list pod for recovery checkpoint failed")
	}
	for i := range pods {
		klog.InfoS("Find unrecovered historical pods from the checkpoint", "pod", klog.KObj(pods[i]))
		r.queue.Add(pods[i])
	}
	<-ctx.Done()
	klog.V(3).Infoln("Stopping pod recovery controller")
	r.queue.ShutDown()
	return nil
}

func (r *recoveryController) runWorker() func(ctx context.Context) {
	return func(_ context.Context) {
		for r.processNextItem() {
		}
	}
}

func (r *recoveryController) processNextItem() bool {
	pod, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(pod)

	if pod == nil {
		r.queue.Forget(pod)
		return true
	}

	result, err := r.recoveryWorker(context.Background(), pod.DeepCopy())
	switch {
	case err != nil:
		klog.V(4).ErrorS(err, "Recovery failed", "pod", klog.KObj(pod))
		r.queue.AddRateLimited(pod)
	case result.RequeueAfter > 0:
		// The result.RequeueAfter request will be lost, if it is returned
		// along with a non-nil error. But this is intended as
		// We need to drive to stable reconcile loops before queuing due
		// to result.RequestAfter
		r.queue.Forget(pod)
		r.queue.AddAfter(pod, result.RequeueAfter)
	case result.Requeue:
		r.queue.AddRateLimited(pod)
	default:
		klog.V(4).InfoS("Recovery successful", "pod", klog.KObj(pod))
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		r.queue.Forget(pod)
		if err = r.recoveryCheckpoint.RemovePod(klog.KObj(pod).String()); err != nil {
			klog.ErrorS(err, "remove pod for recovery checkpoint failed", "pod", klog.KObj(pod))
		}
	}

	return true
}

var (
	removedLabels      = []string{util.PodAssignedPhaseLabel}
	removedAnnotations = []string{
		util.PodVGPUPreAllocAnnotation, util.PodVGPURealAllocAnnotation,
		util.PodPredicateNodeAnnotation, util.PodPredicateTimeAnnotation,
	}
)

// CleanupMetadata Clean up metadata that affects scheduling and allocation.
func CleanupMetadata(pod *corev1.Pod) {
	for _, label := range removedLabels {
		if _, ok := util.HasLabel(pod, label); ok {
			delete(pod.Labels, label)
		}
	}
	for _, anno := range removedAnnotations {
		if _, ok := util.HasAnnotation(pod, anno); ok {
			delete(pod.Annotations, anno)
		}
	}
}

func (r *recoveryController) recoveryWorker(ctx context.Context, pod *corev1.Pod) (reconcile.Result, error) {
	podKey := client.ObjectKeyFromObject(pod)
	currentPod := corev1.Pod{}
	err := r.client.Get(ctx, podKey, &currentPod)
	switch {
	case errors.IsNotFound(err):
		newPod := pod.DeepCopy()
		CleanupMetadata(newPod)
		newPod.UID = ""
		newPod.DeletionTimestamp = nil
		newPod.ResourceVersion = ""
		newPod.Spec.NodeName = ""
		newPod.Status = corev1.PodStatus{}
		// Create a recovered pod
		if err = r.client.Create(ctx, newPod, &client.CreateOptions{}); err == nil {
			r.recorder.Event(newPod, corev1.EventTypeNormal, "Recovery", "Pod recovery successful")
		}
		if errors.IsAlreadyExists(err) {
			err = nil
		}
		return reconcile.Result{}, err
	case err != nil:
		// return err
		return reconcile.Result{}, err
	case currentPod.DeletionTimestamp.IsZero():
		// The pod has not been marked for deletion and is considered to have been restored.
		return reconcile.Result{}, nil
	default:
		nowTime := metav1.Now().Time
		deledTime := currentPod.DeletionTimestamp.Time
		if currentPod.DeletionGracePeriodSeconds != nil {
			deledTime = deledTime.Add(time.Duration(*currentPod.DeletionGracePeriodSeconds) * time.Second)
		}
		requeueAfter := time.Duration(0)
		remaining := deledTime.Sub(nowTime)
		if remaining > 5*time.Second {
			requeueAfter = 5 * time.Second
		} else if remaining > 0 {
			requeueAfter = remaining
		}
		// Ensure that the pod has been GC before the next retry.
		return reconcile.Result{RequeueAfter: requeueAfter}, nil
	}
}
