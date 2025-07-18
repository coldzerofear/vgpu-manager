package reschedule

import (
	"context"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrm "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const Name string = "Reschedule"

type RescheduleController struct {
	nodeName string
	client   client.Client
	recovery *recoveryController
	recorder record.EventRecorder
}

func NewRescheduleController(manager ctrm.Manager, config *node.NodeConfigSpec) (reconcile.Reconciler, error) {
	client := manager.GetClient()
	recorder := manager.GetEventRecorderFor("re-schedule")
	recovery, err := newRecoveryController(client, recorder)
	if err != nil {
		return nil, err
	}
	return &RescheduleController{
		nodeName: config.GetNodeName(),
		client:   client,
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
	if !pod.DeletionTimestamp.IsZero() || !util.IsVGPUResourcePod(pod) ||
		pod.Spec.NodeName != r.nodeName {
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
		shouldDeletePod := false
		for _, reference := range pod.OwnerReferences {
			if reference.Controller != nil &&
				*reference.Controller && reference.Kind != pod.Kind {
				shouldDeletePod = true
				break
			}
		}
		// Manual recovery for bare pods
		if len(pod.OwnerReferences) == 0 {
			r.recorder.Event(pod, corev1.EventTypeWarning, "Recovery",
				"Delete the current pod, causing the controller to recovery the pod and restart scheduling")
			klog.V(4).Infof("Try to recovery pod <%s> uid <%s>", klog.KObj(pod), pod.UID)
			// Attempt to delete pod
			err := r.client.Delete(ctx, pod, &client.DeleteOptions{
				Preconditions: metav1.NewUIDPreconditions(string(pod.UID)),
			})
			if err != nil {
				klog.Errorf("Failed to delete pod <%s>: %v", klog.KObj(pod), err)
				return reconcile.Result{}, err
			}
			// Push the pod into the recovery controller.
			r.recovery.AddPodToRecoveryQueue(pod, 20*time.Millisecond)
		}
		if shouldDeletePod {
			r.recorder.Event(pod, corev1.EventTypeWarning, "Evict",
				"Evict the current pod, causing the controller to rebuild the pod and restart scheduling")
			klog.V(4).Infof("Try to evict pod <%s> uid <%s>", klog.KObj(pod), pod.UID)
			// Attempt to delete pod
			err := r.client.Delete(ctx, pod, &client.DeleteOptions{
				Preconditions: metav1.NewUIDPreconditions(string(pod.UID)),
			})
			if err != nil {
				klog.Errorf("Failed to delete pod <%s>: %v", klog.KObj(pod), err)
				return reconcile.Result{}, err
			}
		}
	}
	return reconcile.Result{}, nil
}

func (r *RescheduleController) RegistryToManager(manager ctrm.Manager) error {
	err := builder.ControllerManagedBy(manager).
		For(&corev1.Pod{}).Named(Name).Complete(r)
	if err == nil {
		err = manager.Add(r.recovery)
	}
	return err
}
