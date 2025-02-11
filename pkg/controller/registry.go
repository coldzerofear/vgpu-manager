package controller

import (
	"fmt"
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

type newController func(manager ctrm.Manager, config *node.NodeConfig) reconcile.Reconciler

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

func RegistryControllerToManager(manager ctrm.Manager, config *node.NodeConfig, controllerSwitch map[string]bool) (err error) {
	once.Do(func() {
		for name, newControllerFunc := range controllerFuncMap {
			if !controllerSwitch[name] {
				continue
			}
			controller, ok := newControllerFunc(manager, config).(Controller)
			if !ok {
				err = fmt.Errorf("%s has not implemented a controller, skip it", name)
				klog.Errorln(err.Error())
				continue
			}
			klog.V(4).Infoln("registry controller to manager", "controller", name)
			if err = controller.RegistryToManager(manager); err != nil {
				klog.ErrorS(err, "unable to registry controller", "controller", name)
				return
			}
		}
	})
	return err
}
