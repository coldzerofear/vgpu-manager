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

	"github.com/coldzerofear/vgpu-manager/cmd/monitor/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/util/cgroup"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	listerv1 "k8s.io/client-go/listers/core/v1"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

func main() {
	klog.InitFlags(flag.CommandLine)
	opt := options.NewOptions()
	opt.InitFlags(flag.CommandLine)
	opt.PrintAndExitIfRequested()
	defer klog.Flush()
	util.SetGlobalDomain(opt.Domain)

	err := client.InitKubeConfig(opt.MasterURL, opt.KubeConfigFile)
	if err != nil {
		klog.Fatalf("Initialization of kubeConfig failed: %v", err)
	}
	kubeClient, err := client.NewClientSet(
		client.WithQPSBurst(float32(opt.QPS), opt.Burst),
		//client.WithDefaultContentType(),
		client.WithDefaultUserAgent())
	if err != nil {
		klog.Fatalf("Create kubeClient failed: %v", err)
	}
	nodeConfig, err := node.NewNodeConfig(node.WithMonitorOptions(*opt), false)
	if err != nil {
		klog.Fatalf("Initialization of node config failed: %v", err)
	}
	klog.V(4).Infof("Current NodeConfig:\n%s", nodeConfig.String())
	cgroup.MustInitCGroupDriver(nodeConfig.GetCGroupDriver())

	// trim managedFields to reduce cache memory usage.
	option := informers.WithTransform(cache.TransformStripManagedFields())
	factory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 10*time.Hour, option)

	nodeInformer := metrics.GetNodeInformer(factory, nodeConfig.GetNodeName())
	podInformer := metrics.GetPodInformer(factory, nodeConfig.GetNodeName())
	nodeLister := listerv1.NewNodeLister(nodeInformer.GetIndexer())
	podLister := listerv1.NewPodLister(podInformer.GetIndexer())

	containerLister := metrics.NewContainerLister(
		deviceplugin.ContManagerDirectoryPath, nodeConfig.GetNodeName(), podLister)
	nodeCollector, err := metrics.NewNodeGPUCollector(nodeConfig.GetNodeName(),
		nodeLister, podLister, containerLister, opt.FeatureGate)
	if err != nil {
		klog.Fatalf("Create node gpu collector failed: %v", err)
	}
	rateLimiter := rate.NewLimiter(rate.Every(time.Second), 1)
	server := metrics.NewServer(
		metrics.WithRegistry(nodeCollector.Registry()),
		metrics.WithPort(&opt.ServerBindPort),
		metrics.WithLimiter(rateLimiter),
		metrics.WithTimeoutSecond(30))

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
		if serverErr := server.Start(ctx.Done()); serverErr != nil {
			klog.Errorf("Server error occurred: %v", serverErr)
			cancelCtx()
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	select {
	case s := <-sigChan:
		klog.Infof("Received signal %v, shutting down...", s)
		cancelCtx()
	case <-ctx.Done():
		klog.Errorln("Internal error, service abnormal stop")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
}
