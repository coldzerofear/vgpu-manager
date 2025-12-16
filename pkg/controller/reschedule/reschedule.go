package reschedule

import (
	"context"
	"time"

	client2 "github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrm "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const Name string = "Reschedule"

type RescheduleController struct {
	nodeName string
	client   client.Client
	eviction client2.Eviction
	recovery *recoveryController
	recorder record.EventRecorder
}

func NewRescheduleController(manager ctrm.Manager, config *node.NodeConfigSpec) (reconcile.Reconciler, error) {
	recorder := manager.GetEventRecorderFor("re-schedule")
	recovery, err := newRecoveryController(manager.GetClient(), recorder)
	if err != nil {
		return nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(manager.GetConfig())
	if err != nil {
		return nil, err
	}
	eviction, err := client2.NewEviction(kubeClient, config.GetNodeName())
	if err != nil {
		return nil, err
	}
	return &RescheduleController{
		nodeName: config.GetNodeName(),
		client:   manager.GetClient(),
		eviction: eviction,
		recorder: recorder,
		recovery: recovery,
	}, nil
}

func (r *RescheduleController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	pod := &corev1.Pod{}
	if err := r.client.Get(ctx, req.NamespacedName, pod); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// Skip invalid pod
	if !pod.DeletionTimestamp.IsZero() || !util.IsVGPUResourcePod(pod) || pod.Spec.NodeName != r.nodeName {
		return reconcile.Result{}, nil
	}
	switch pod.Status.Phase {
	case corev1.PodFailed, corev1.PodPending:
		if !util.IsShouldDeletePod(pod) {
			break
		}
		// Determine whether it is necessary to delete the pod
		// Only when a pod is controlled by a controller and
		// not controlled by another pod, is it eligible for deletion.
		shouldEvictPod := false
		for _, reference := range pod.OwnerReferences {
			if reference.Controller != nil &&
				*reference.Controller && reference.Kind != pod.Kind {
				shouldEvictPod = true
				break
			}
		}
		// Manual recovery for bare pods
		if len(pod.OwnerReferences) == 0 {
			if types.IsCriticalPod(pod) {
				klog.ErrorS(nil, "Cannot evict a critical pod", "pod", klog.KObj(pod))
				break
			}
			klog.V(4).InfoS("Try to recovery pod", "pod", klog.KObj(pod), "uid", pod.UID)
			// Attempt to delete pod
			if err := r.client.Delete(ctx, pod, &client.DeleteOptions{
				Preconditions: metav1.NewUIDPreconditions(string(pod.UID)),
			}); err != nil {
				klog.ErrorS(err, "Failed to delete pod", "pod", klog.KObj(pod))
				return reconcile.Result{}, err
			}
			r.recorder.Event(pod, corev1.EventTypeWarning, "Recovery",
				"Deleting pod to allow its controller to recreate and reschedule it.")
			// Push the pod into the recovery controller.
			r.recovery.AddPodToRecoveryQueue(pod, 20*time.Millisecond)
		}
		if shouldEvictPod {
			klog.V(4).InfoS("Try to evict pod", "pod", klog.KObj(pod), "uid", pod.UID)
			gracePeriodSeconds := int64(0)
			if pod.Spec.TerminationGracePeriodSeconds != nil {
				gracePeriodSeconds = *pod.Spec.TerminationGracePeriodSeconds
			}
			r.eviction.Evict(ctx, pod, r.recorder, gracePeriodSeconds,
				"Evicting pod to trigger controller reconciliation and rescheduling.")
		}
	}
	return reconcile.Result{}, nil
}

func (r *RescheduleController) RegisterToManager(manager ctrm.Manager) error {
	err := builder.ControllerManagedBy(manager).
		For(&corev1.Pod{}).Named(Name).Complete(r)
	if err == nil {
		err = manager.Add(r.recovery)
	}
	return err
}
