package controller

import (
	"sync"

	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"k8s.io/klog/v2"
	ctrm "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Controller interface {
	reconcile.Reconciler
	RegisterToManager(ctrm.Manager) error
}

type NewControllerFunc func(manager ctrm.Manager, config *node.NodeConfigSpec) (Controller, error)

var (
	registerOnce       sync.Once
	registerErr        error
	mutex              sync.Mutex
	controllerRegistry map[string]NewControllerFunc
)

func init() {
	controllerRegistry = make(map[string]NewControllerFunc)
}

func RegisterController(name string, fn NewControllerFunc) {
	mutex.Lock()
	controllerRegistry[name] = fn
	mutex.Unlock()
}

func RegisterControllerToManager(
	manager ctrm.Manager,
	config *node.NodeConfigSpec,
	controlSwitch map[string]bool,
) error {
	registerOnce.Do(func() {
		var controller Controller
		for name, newController := range controllerRegistry {
			if !controlSwitch[name] {
				continue
			}
			if controller, registerErr = newController(manager, config); registerErr != nil {
				klog.ErrorS(registerErr, "unable to create controller", "controller", name)
				break
			}
			klog.V(4).InfoS("Register controller to manager", "controller", name)
			if registerErr = controller.RegisterToManager(manager); registerErr != nil {
				klog.ErrorS(registerErr, "unable to register controller", "controller", name)
				break
			}
		}
	})
	return registerErr
}
