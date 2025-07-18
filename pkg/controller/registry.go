package controller

import (
	"sync"

	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/controller/reschedule"
	"k8s.io/klog/v2"
	ctrm "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Controller interface {
	reconcile.Reconciler
	RegistryToManager(manager ctrm.Manager) error
}

type newController func(manager ctrm.Manager, config *node.NodeConfigSpec) (reconcile.Reconciler, error)

var (
	once              sync.Once
	controllerFuncMap map[string]newController

	// Ensure that the controller is implemented.
	_ Controller = &reschedule.RescheduleController{}
)

func init() {
	controllerFuncMap = make(map[string]newController)
	controllerFuncMap[reschedule.Name] = reschedule.NewRescheduleController
}

func RegistryControllerToManager(manager ctrm.Manager, config *node.NodeConfigSpec, controllerSwitch map[string]bool) (err error) {
	once.Do(func() {
		var c reconcile.Reconciler
		for name, newControllerFunc := range controllerFuncMap {
			if !controllerSwitch[name] {
				continue
			}
			c, err = newControllerFunc(manager, config)
			if err != nil {
				klog.ErrorS(err, "unable to create controller", "controller", name)
				return
			}
			controller, ok := c.(Controller)
			if !ok {
				klog.Errorf("%s has not implemented a controller, skip it", name)
				continue
			}
			klog.V(4).InfoS("Registry controller to manager", "controller", name)
			if err = controller.RegistryToManager(manager); err != nil {
				klog.ErrorS(err, "unable to registry controller", "controller", name)
				return
			}
		}
	})
	return err
}
