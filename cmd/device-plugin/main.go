package main

import (
	"context"
	"flag"
	"path/filepath"
	"syscall"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/fsnotify/fsnotify"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/klog/v2"
)

func main() {
	klog.InitFlags(flag.CommandLine)
	opt := options.NewOptions()
	opt.InitFlags(flag.CommandLine)
	defer klog.Flush()
	opt.PrintAndExitIfRequested()
	err := client.InitKubeConfig(opt.MasterURL, opt.KubeConfigFile)
	if err != nil {
		klog.Fatalf("Initialization of k8s client configuration failed: %v", err)
	}
	mutationContentType := client.MutationContentType(
		"application/vnd.kubernetes.protobuf,application/json",
		"application/json")
	kubeClient, err := client.GetClientSet(mutationContentType, client.MutationQPS(float32(opt.QPS), opt.Burst))
	if err != nil {
		klog.Fatalf("Create k8s kubeClient failed: %v", err)
	}
	nodeConfig, err := node.NewNodeConfig(*opt)
	if err != nil {
		klog.Fatalf("Initialization of node config failed: %v", err)
	}
	klog.V(4).Infoln("Current NodeConfig", nodeConfig.String())
	util.InitializeCGroupDriver(nodeConfig)

	klog.V(3).Info("Initialize Device Resource Manager")
	deviceManager := manager.NewDeviceManager(nodeConfig, kubeClient)

	klog.V(3).Info("Starting FS watcher.")
	devicePluginSocket := filepath.Join(opt.DevicePluginPath, "kubelet.sock")
	watcher, err := NewFSWatcher(opt.DevicePluginPath)
	if err != nil {
		klog.Fatalf("Failed to create FS watcher: %v", err)
	}
	defer watcher.Close()
	klog.V(3).Info("Starting OS watcher.")
	sigs := NewOSWatcher(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	factory := informers.NewSharedInformerFactory(kubeClient, 10*time.Hour)
	plugins := deviceplugin.InitDevicePlugins(opt, deviceManager, factory, kubeClient)
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	factory.Start(ctx.Done())
	klog.V(4).Infoln("Waiting for InformerFactory cache synchronization...")
	factory.WaitForCacheSync(wait.NeverStop)
	klog.V(4).Infoln("InformerFactory cache synchronization successful")
	deviceManager.Start()

restart:
	started := 0
	// Loop through all plugins, idempotently stopping them, and then starting
	// them if they have any devices to serve. If even one plugin fails to
	// start properly, try starting them all again.
	for _, p := range plugins {
		p.Stop()

		// Just continue if there are no devices to serve for plugin p.
		if len(p.Devices()) == 0 {
			continue
		}
		// Start the gRPC server for plugin p and connect it with the kubelet.
		if err := p.Start(); err != nil {
			klog.Infof("Plugin %s failed to start: %v", p.Name(), err)
			// If there was an error starting any plugins, restart them all.
			goto restart
		}
		started++
	}
	if started == 0 {
		klog.Warningln("No devices found. Waiting indefinitely.")
	}

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
		p.Stop()
	}
}
