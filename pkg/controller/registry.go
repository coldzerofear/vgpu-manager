package controller

import (
	"reflect"

	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"k8s.io/klog/v2"
	ctrm "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Controller interface {
	reconcile.Reconciler
	registryToManager(manager ctrm.Manager) error
}

type newController func(manager ctrm.Manager, config *node.NodeConfig) Controller

var (
	newControllerFuncs []newController
)

func RegistryControllerToManager(manager ctrm.Manager, config *node.NodeConfig) error {
	for _, newControllerFunc := range newControllerFuncs {
		controller := newControllerFunc(manager, config)
		name := reflect.ValueOf(controller).String()
		klog.V(4).Infoln("registry controller to manager", "controller", name)
		if err := controller.registryToManager(manager); err != nil {
			klog.ErrorS(err, "unable to registry controller", "controller", name)
			return err
		}
	}
	return nil
}
