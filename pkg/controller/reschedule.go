package controller

import (
	"context"

	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrm "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func init() {
	newControllerFuncs = append(newControllerFuncs, NewRescheduleController)
}

type RescheduleController struct {
	nodeName string
	client.Client
}

func NewRescheduleController(manager ctrm.Manager, config *node.NodeConfig) Controller {
	return &RescheduleController{
		nodeName: config.NodeName(),
		Client:   manager.GetClient(),
	}
}

func (r *RescheduleController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	pod := &corev1.Pod{}
	if err := r.Client.Get(ctx, req.NamespacedName, pod); err != nil {
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
			return reconcile.Result{}, nil
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
		if shouldDeletePod {
			klog.V(4).Infof("Try to delete pod <%s/%s> uid <%s>", pod.Namespace, pod.Name, pod.UID)
			// Attempt to delete pod
			err := r.Client.Delete(ctx, pod, &client.DeleteOptions{
				Preconditions: metav1.NewUIDPreconditions(string(pod.UID)),
			})
			if err != nil {
				klog.Errorf("Failed to delete pod <%s/%s>: %v", pod.Namespace, pod.Name, err)
				return reconcile.Result{}, err
			}
		}
	}
	return reconcile.Result{}, nil
}

func (r *RescheduleController) registryToManager(manager ctrm.Manager) error {
	return builder.ControllerManagedBy(manager).
		For(&corev1.Pod{}).Named("Reschedule").
		Complete(r)
}
