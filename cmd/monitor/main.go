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
	"github.com/coldzerofear/vgpu-manager/pkg/mertics"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
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
	nodeConfig, err := node.NewNodeConfig(opt.NodeConfigPath, node.MutationMonitorOptions(*opt))
	if err != nil {
		klog.Fatalf("Initialization of node config failed: %v", err)
	}
	klog.V(4).Infoln("Current NodeConfig", nodeConfig.String())
	util.InitializeCGroupDriver(nodeConfig)

	// trim managedFields to reduce cache memory usage.
	option := informers.WithTransform(cache.TransformStripManagedFields())
	factory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 10*time.Hour, option)
	nodeInformer := mertics.GetNodeInformer(factory, nodeConfig.NodeName())
	podInformer := mertics.GetPodInformer(factory, nodeConfig.NodeName())
	contLister := mertics.NewContainerLister(podInformer)
	server := mertics.NewServer(nodeInformer, podInformer, contLister, nodeConfig.NodeName(), opt.ServerBindProt)

	ctx, cancelCtx := context.WithCancel(context.Background())

	factory.Start(ctx.Done())
	klog.V(4).Infoln("Waiting for InformerFactory cache synchronization...")
	factory.WaitForCacheSync(wait.NeverStop)
	klog.V(4).Infoln("InformerFactory cache synchronization successful")

	go contLister.Start(ctx.Done())

	go func() {
		addr := "0.0.0.0:" + strconv.Itoa(opt.PprofBindPort)
		klog.V(4).Infof("Debug Server starting on <%s>", addr)
		klog.V(4).ErrorS(http.ListenAndServe(addr, nil), "Debug Server error occurred")
	}()

	go func() {
		serverErr := server.Start(ctx.Done())
		if serverErr != nil {
			klog.Errorf("Server error occurred: %v", err)
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
