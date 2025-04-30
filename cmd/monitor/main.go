package main

import (
	"context"
	"flag"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/coldzerofear/vgpu-manager/cmd/monitor/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
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
	kubeClient, err := client.NewClientSet(
		client.WithQPS(float32(opt.QPS), opt.Burst),
		client.WithDefaultContentType())
	if err != nil {
		klog.Fatalf("Create kubeClient failed: %v", err)
	}
	nodeConfig, err := node.NewNodeConfig(node.WithMonitorOptions(*opt))
	if err != nil {
		klog.Fatalf("Initialization of node config failed: %v", err)
	}
	klog.V(4).Infoln("Current NodeConfig:\n", nodeConfig.String())
	util.InitializeCGroupDriver(nodeConfig.CGroupDriver())

	// trim managedFields to reduce cache memory usage.
	option := informers.WithTransform(cache.TransformStripManagedFields())
	factory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 10*time.Hour, option)

	nodeInformer := metrics.GetNodeInformer(factory, nodeConfig.NodeName())
	podInformer := metrics.GetPodInformer(factory, nodeConfig.NodeName())
	nodeLister := listerv1.NewNodeLister(nodeInformer.GetIndexer())
	podLister := listerv1.NewPodLister(podInformer.GetIndexer())

	containerLister := metrics.NewContainerLister(
		deviceplugin.ContManagerDirectoryPath, nodeConfig.NodeName(), podLister)
	nodeCollector, err := metrics.NewNodeGPUCollector(
		nodeConfig.NodeName(), nodeLister, podLister, containerLister)
	if err != nil {
		klog.Fatalf("Create node gpu collector failed: %v", err)
	}
	server := metrics.NewServer(
		metrics.WithRegistry(nodeCollector.Registry()),
		metrics.WithLimiter(rate.NewLimiter(rate.Every(time.Second), 1)),
		metrics.WithPort(ptr.To[int](opt.ServerBindProt)))

	ctx, cancelCtx := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	klog.V(4).Infoln("Waiting for InformerFactory cache synchronization...")
	factory.WaitForCacheSync(wait.NeverStop)
	klog.V(4).Infoln("InformerFactory cache synchronization successful")

	containerLister.Start(5*time.Second, ctx.Done())
	// Start pprof debug debugging service.
	go func() {
		if opt.PprofBindPort > 0 {
			addr := "0.0.0.0:" + strconv.Itoa(opt.PprofBindPort)
			klog.V(4).Infof("Debug Server starting on <%s>", addr)
			klog.V(4).ErrorS(http.ListenAndServe(addr, nil), "Debug Server error occurred")
		}
	}()
	// Start prometheus indicator collection service.
	go func() {
		serverErr := server.Start(ctx.Done())
		if serverErr != nil {
			klog.Errorf("Server error occurred: %v", serverErr)
			cancelCtx()
		}
	}()

	exitCode := 0
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	select {
	case s := <-sigChan:
		klog.Infof("Received signal %v, shutting down...", s)
		cancelCtx()
		time.Sleep(5 * time.Second)
	case <-ctx.Done():
		klog.Errorln("Internal error, service abnormal stop")
		exitCode = 1
	}
	os.Exit(exitCode)
}
