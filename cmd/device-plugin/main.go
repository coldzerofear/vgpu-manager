package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/controller"
	"github.com/coldzerofear/vgpu-manager/pkg/controller/reschedule"
	devm "github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/fsnotify/fsnotify"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	rtcache "sigs.k8s.io/controller-runtime/pkg/cache"
	rtclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrm "sigs.k8s.io/controller-runtime/pkg/manager"
	metrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	klog.InitFlags(flag.CommandLine)
	opt := options.NewOptions()
	opt.InitFlags(flag.CommandLine)
	opt.PrintAndExitIfRequested()
	defer klog.Flush()

	err := client.InitKubeConfig(opt.MasterURL, opt.KubeConfigFile)
	if err != nil {
		klog.Fatalf("Initialization of kubeConfig failed: %v", err)
	}

	kubeConfig, err := client.NewKubeConfig(
		client.WithQPS(float32(opt.QPS), opt.Burst),
		client.WithDefaultContentType())
	if err != nil {
		klog.Fatalf("Create kubeConfig failed: %v", err)
	}
	kubeClient, err := client.NewClientSet(
		client.WithQPS(float32(opt.QPS), opt.Burst),
		client.WithDefaultContentType())
	if err != nil {
		klog.Fatalf("Create kubeClient failed: %v", err)
	}
	nodeConfig, err := node.NewNodeConfig(node.WithDevicePluginOptions(*opt))
	if err != nil {
		klog.Fatalf("Initialization of node config failed: %v", err)
	}
	klog.V(4).Infof("Current NodeConfig:\n%s", nodeConfig.String())
	util.InitializeCGroupDriver(nodeConfig.CGroupDriver())

	klog.V(3).Info("Initialize Device Resource Manager")
	deviceManager, err := devm.NewDeviceManager(nodeConfig, kubeClient)
	if err != nil {
		klog.Fatalf("Create device manager failed: %v", err)
	}

	klog.V(3).Info("Starting FS watcher.")
	devicePluginSocket := filepath.Join(opt.DevicePluginPath, "kubelet.sock")
	watcher, err := NewFSWatcher(opt.DevicePluginPath)
	if err != nil {
		klog.Fatalf("Failed to create FS watcher: %v", err)
	}
	clusterCtx, cancelFunc := context.WithCancel(context.Background())
	defer func() {
		_ = watcher.Close()
		cancelFunc()
		time.Sleep(5 * time.Second)
	}()
	klog.V(3).Info("Starting OS watcher.")
	sigs := NewOSWatcher(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	manager, err := ctrm.New(kubeConfig, ctrm.Options{
		HealthProbeBindAddress: "0", // disable health probe
		PprofBindAddress:       fmt.Sprintf("%d", opt.PprofBindPort),
		Cache: rtcache.Options{
			// trim managedFields to reduce cache memory usage.
			DefaultTransform:         rtcache.TransformStripManagedFields(),
			DefaultWatchErrorHandler: toolscache.DefaultWatchErrorHandler,
			ByObject: map[rtclient.Object]rtcache.ByObject{
				&corev1.Pod{}: {
					Field:     fields.OneTermEqualSelector("spec.nodeName", opt.NodeName),
					Transform: rtcache.TransformStripManagedFields(),
				},
			},
		},
		Metrics: metrics.Options{BindAddress: "0"}, // disable metrics service
		Logger:  klog.NewKlogr(),
	})
	if err != nil {
		klog.Fatalf("Create cluster manager failed: %v", err)
	}
	controllerSwitch := map[string]bool{
		reschedule.Name: opt.FeatureGate.Enabled(options.Reschedule),
	}
	err = controller.RegistryControllerToManager(manager, nodeConfig, controllerSwitch)
	if err != nil {
		klog.Fatalf("Registry controller to manager failed: %v", err)
	}
	plugins, err := deviceplugin.GetDevicePlugins(opt, deviceManager, manager, kubeClient)
	if err != nil {
		klog.Fatalf("Get device plugins failed: %v", err)
	}

	klog.Infoln("Starting cluster manager.")
	go func() {
		if err = manager.Start(clusterCtx); err != nil {
			klog.V(3).ErrorS(err, "failed staring cluster manager")
			cancelFunc()
		}
	}()
	klog.V(4).Infoln("Waiting for cluster manager cache synchronization...")
	if ok := manager.GetCache().WaitForCacheSync(clusterCtx); !ok {
		klog.Fatalf("Cannot wait for cluster manager cache sync")
	}
	klog.V(4).Infoln("Cluster manager cache synchronization successful")
	deviceManager.Start()

restart:
	started := 0
	// Loop through all plugins, idempotently stopping them, and then starting
	// them if they have any devices to serve. If even one plugin fails to
	// start properly, try starting them all again.
	for _, p := range plugins {
		_ = p.Stop()

		// Just continue if there are no devices to serve for plugin p.
		if len(p.Devices()) == 0 {
			klog.Warningf("Plugin %s devices is empty, skip it", p.Name())
			continue
		}
		// Start the gRPC server for plugin p and connect it with the kubelet.
		if err := p.Start(); err != nil {
			klog.Errorf("Plugin %s failed to start: %v", p.Name(), err)
			// If there was an error starting any plugins, restart them all.
			goto restart
		}
		started++
	}
	if started == 0 {
		klog.Warningln("No devices found. Waiting indefinitely.")
	}
	exitCode := 0
	// Start an infinite loop, waiting for several indicators to either log
	// some messages, trigger a restart of the plugins, or exit the program.
	for {
		select {
		// Detect a kubelet restart by watching for a newly created
		// 'pluginapi.KubeletSocket' file. When this occurs, restart this loop,
		// restarting all of the plugins in the process.
		case event := <-watcher.Events:
			if event.Name == devicePluginSocket && event.Op&fsnotify.Create == fsnotify.Create {
				time.Sleep(time.Second)
				klog.Infof("inotify: %s created, restarting.", devicePluginSocket)
				goto restart
			}
		// Watch for any other fs errors and log them.
		case err := <-watcher.Errors:
			klog.Infof("inotify: %v", err)
		// When cluster cache stops abnormally, exit the program.
		case <-clusterCtx.Done():
			exitCode = 1
			goto exit
		// Watch for any signals from the OS. On SIGHUP, restart this loop,
		// restarting all of the plugins in the process. On all other
		// signals, exit the loop and exit the program.
		case s := <-sigs:
			switch s {
			case syscall.SIGHUP:
				klog.Info("Received SIGHUP, restarting.")
				goto restart
			default:
				klog.Infof("Received signal '%v', shutting down.", s)
				goto exit
			}
		}
	}
exit:
	deviceManager.Stop()
	for _, p := range plugins {
		_ = p.Stop()
	}

	os.Exit(exitCode)
}
