package main

import (
	"flag"
	"os"

	monitoroptions "github.com/coldzerofear/vgpu-manager/cmd/monitor/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/controller"
	"github.com/coldzerofear/vgpu-manager/pkg/controller/reschedule"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	rtcache "sigs.k8s.io/controller-runtime/pkg/cache"
	rtclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrm "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	metrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	klog.InitFlags(flag.CommandLine)
	flag.Parse()
	defer klog.Flush()

	err := client.InitKubeConfig("", "/root/.kube/config")
	if err != nil {
		klog.Fatalf("Initialization of k8s client configuration failed: %v", err)
	}
	mutationContentType := client.MutationContentType(
		"application/vnd.kubernetes.protobuf,application/json",
		"application/json")
	kubeConfig, err := client.GetKubeConfig(mutationContentType, client.MutationQPS(20, 30))
	if err != nil {
		klog.Fatalf("Init k8s restConfig failed: %v", err)
	}
	manager, err := ctrm.New(kubeConfig, ctrm.Options{
		Cache: rtcache.Options{
			// trim managedFields to reduce cache memory usage.
			DefaultTransform:         rtcache.TransformStripManagedFields(),
			DefaultWatchErrorHandler: toolscache.DefaultWatchErrorHandler,
			ByObject: map[rtclient.Object]rtcache.ByObject{
				&corev1.Pod{}: {
					Field:     fields.OneTermEqualSelector("spec.nodeName", "master"),
					Transform: rtcache.TransformStripManagedFields(),
				},
			},
		},
		Metrics: metrics.Options{BindAddress: "0"},
		Logger:  klogr.New(),
	})
	if err != nil {
		klog.Fatalf("Create cluster manager failed: %v", err)
	}

	nodeConfig, err := node.NewNodeConfig("", node.MutationMonitorOptions(monitoroptions.Options{
		NodeName: "master",
	}))
	if err != nil {
		klog.Fatalf("Initialization of node config failed: %v", err)
	}
	controllerSwitch := map[string]bool{
		reschedule.Name: true,
	}
	err = controller.RegistryControllerToManager(manager, nodeConfig, controllerSwitch)
	if err != nil {
		klog.Fatalf("Registry controller to manager failed: %v", err)
	}

	klog.Infoln("Starting cluster manager.")

	if err = manager.Start(signals.SetupSignalHandler()); err != nil {
		klog.V(3).ErrorS(err, "failed staring cluster manager")
		os.Exit(1)
	}
}
