package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/monitor/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/route"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/util/cgroup"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/component-base/logs"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

func main() {
	opt := options.NewOptions()
	opt.InitFlags(flag.CommandLine)
	opt.PrintAndExitIfRequested()
	logs.InitLogs()
	defer logs.FlushLogs()
	util.MustInitGlobalDomain(opt.Domain)

	err := client.InitKubeConfig(opt.MasterURL, opt.KubeConfigFile)
	if err != nil {
		klog.Fatalf("Initialization of kubeConfig failed: %v", err)
	}
	kubeConfig, err := client.NewKubeConfig(
		client.WithQPSBurst(float32(opt.QPS), opt.Burst),
		//client.WithDefaultContentType(),
		client.WithDefaultUserAgent())
	if err != nil {
		klog.Fatalf("Create kubeConfig failed: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
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
		util.ManagerRootPath, nodeConfig.GetNodeName(), podLister)
	nodeCollector, err := metrics.NewNodeGPUCollector(nodeConfig.GetNodeName(),
		nodeLister, podLister, containerLister, opt.FeatureGate)
	if err != nil {
		klog.Fatalf("Create node gpu collector failed: %v", err)
	}
	rateLimiter := rate.NewLimiter(rate.Every(time.Second), 1)
	opts := []metrics.Option{
		metrics.WithLimiter(rateLimiter),
		metrics.WithCollectors(nodeCollector),
		metrics.WithPort(&opt.ServerBindPort),
		metrics.WithTimeoutSecond(30),
		metrics.WithDebugMetrics(opt.PprofBindPort > 0),
		metrics.WithLabels(prometheus.Labels{"service": "vGPU"}),
	}
	if opt.EnableRBAC {
		httpClient, err := rest.HTTPClientFor(kubeConfig)
		if err != nil {
			klog.Fatalf("Create httpClient failed: %v", err)
		}
		authorization, err := filters.WithAuthenticationAndAuthorization(kubeConfig, httpClient)
		if err != nil {
			klog.Fatalf("Create authClient failed: %v", err)
		}
		opts = append(opts, metrics.WithMiddleware(func(handler http.Handler) (http.Handler, error) {
			return authorization(klog.NewKlogr(), handler)
		}))
	}
	server := metrics.NewServer(opts...)
	ctx, cancelCtx := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	klog.V(4).Infoln("Waiting for InformerFactory cache synchronization...")
	factory.WaitForCacheSync(wait.NeverStop)
	klog.V(4).Infoln("InformerFactory cache synchronization successful")

	containerLister.Start(5*time.Second, ctx.Done())
	// Start pprof debug debugging service.
	route.StartDebugServer(opt.PprofBindPort)
	// Start prometheus indicator collection service.
	go func() {
		if err := server.Start(ctx); err != nil {
			klog.Errorf("Server error occurred: %v", err)
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
