package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/scheduler/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/bind"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/filter"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/route"
	tlsconfig "github.com/grepplabs/cert-source/config"
	tlsserverconfig "github.com/grepplabs/cert-source/tls/server/config"
	"github.com/julienschmidt/httprouter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
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
		"application/vnd.kubernetes.protobuf,application/json")
	kubeClient, err := client.GetClientSet(mutationContentType, client.MutationQPS(float32(opt.QPS), opt.Burst))
	if err != nil {
		klog.Fatalf("Create k8s kubeClient failed: %v", err)
	}

	var tlsConfig *tls.Config
	if opt.EnableTls {
		if len(opt.TlsKeyFile) == 0 || len(opt.TlsCertFile) == 0 {
			klog.Fatalf("Enable Tls but did not specify a certificate file: "+
				"tlsKeyFile: %s, tlsCertFile: %s", opt.TlsKeyFile, opt.TlsCertFile)
		}
		tlsConfig, err = tlsserverconfig.GetServerTLSConfig(slog.Default(), &tlsconfig.TLSServerConfig{
			Enable:  opt.EnableTls,
			Refresh: time.Duration(opt.CertRefreshInterval) * time.Second,
			File: tlsconfig.TLSServerFiles{
				Key:  opt.TlsKeyFile,
				Cert: opt.TlsCertFile,
			},
		})
		if err != nil {
			klog.Fatalf("GetServerTLSConfig failed: %v", err)
		}
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: opt.SchedulerName})

	factory := informers.NewSharedInformerFactory(kubeClient, 10*time.Hour)

	bindPlugin, err := bind.New(kubeClient, recorder)
	if err != nil {
		klog.Fatalf("Initialization of scheduler BindPlugin failed: %v", err)
	}
	filterPlugin, err := filter.New(kubeClient, factory, recorder)
	if err != nil {
		klog.Fatalf("Initialization of scheduler FilterPlugin failed: %v", err)
	}
	router := httprouter.New()
	route.AddVersion(router)
	route.AddHealthProbe(router)
	route.AddFilterPredicate(router, filterPlugin)
	route.AddBindPredicate(router, bindPlugin)

	ctx, cancelFunc := context.WithCancel(context.Background())

	factory.Start(ctx.Done())
	klog.Infoln("Waiting for InformerFactory cache synchronization...")
	factory.WaitForCacheSync(wait.NeverStop)
	klog.Infoln("InformerFactory cache synchronization successful")

	go func() {
		addr := "0.0.0.0:" + strconv.Itoa(opt.PprofBindPort)
		klog.V(4).Infof("Debug Server starting on <%s>", addr)
		klog.V(4).ErrorS(http.ListenAndServe(addr, nil), "Debug Server error occurred")
	}()

	server := http.Server{
		Addr:      "0.0.0.0:" + strconv.Itoa(opt.ServerBindProt),
		Handler:   router,
		TLSConfig: tlsConfig,
	}
	go func() {
		var serverErr error
		if opt.EnableTls {
			klog.Infof("Tls Server starting on <0.0.0.0:%d>", opt.ServerBindProt)
			serverErr = server.ListenAndServeTLS("", "")
		} else {
			klog.Infof("Server starting on <0.0.0.0:%d>", opt.ServerBindProt)
			serverErr = server.ListenAndServe()
		}
		if serverErr != nil {
			klog.Errorf("Server error occurred: %v", err)
			cancelFunc()
		}
	}()
	exitCode := 0
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	select {
	case s := <-sigChan:
		klog.Infof("Received signal %v, shutting down...", s)
		_ = server.Shutdown(context.Background())
		cancelFunc()
	case <-ctx.Done():
		klog.Errorln("Internal error, service abnormal stop")
		exitCode = 1
	}
	os.Exit(exitCode)
}