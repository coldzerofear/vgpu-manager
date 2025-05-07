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

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/coldzerofear/vgpu-manager/cmd/scheduler/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/route"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/bind"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/filter"
	tlsconfig "github.com/grepplabs/cert-source/config"
	tlsserverconfig "github.com/grepplabs/cert-source/tls/server/config"
	"github.com/julienschmidt/httprouter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

var Scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(scheme.AddToScheme(Scheme))
}

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
		client.WithQPSBurst(float32(opt.QPS), opt.Burst),
		//client.WithDefaultContentType(),
		client.WithDefaultUserAgent())
	if err != nil {
		klog.Fatalf("Create kubeClient failed: %v", err)
	}

	var tlsConfig *tls.Config
	if opt.EnableTls {
		if len(opt.TlsKeyFile) == 0 || len(opt.TlsCertFile) == 0 {
			klog.Fatalf("Enable Tls but did not specify a certificate file: "+
				"tlsKeyFile: '%s', tlsCertFile: '%s'", opt.TlsKeyFile, opt.TlsCertFile)
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
	recorder := broadcaster.NewRecorder(Scheme, corev1.EventSource{Component: opt.SchedulerName})

	// trim managedFields to reduce cache memory usage.
	option := informers.WithTransform(cache.TransformStripManagedFields())
	factory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 10*time.Hour, option)

	bindPlugin, err := bind.New(kubeClient, recorder, opt.FeatureGate.Enabled(options.SerialBindNode))
	if err != nil {
		klog.Fatalf("Initialization of scheduler BindPlugin failed: %v", err)
	}
	filterPlugin, err := filter.New(kubeClient, factory, recorder)
	if err != nil {
		klog.Fatalf("Initialization of scheduler FilterPlugin failed: %v", err)
	}
	handler := httprouter.New()
	route.AddVersion(handler)
	route.AddHealthProbe(handler)
	route.AddFilterPredicate(handler, filterPlugin)
	route.AddBindPredicate(handler, bindPlugin)

	ctx, cancelFunc := context.WithCancel(context.Background())

	factory.Start(ctx.Done())
	klog.Infoln("Waiting for InformerFactory cache synchronization...")
	factory.WaitForCacheSync(wait.NeverStop)
	klog.Infoln("InformerFactory cache synchronization successful")

	go func() {
		if opt.PprofBindPort > 0 {
			addr := "0.0.0.0:" + strconv.Itoa(opt.PprofBindPort)
			klog.V(4).Infof("Debug Server starting on <%s>", addr)
			klog.V(4).ErrorS(http.ListenAndServe(addr, nil), "Debug Server error occurred")
		}
	}()

	server := http.Server{
		Addr:      "0.0.0.0:" + strconv.Itoa(opt.ServerBindProt),
		Handler:   handler,
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

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	select {
	case s := <-sigChan:
		klog.Infof("Received signal %v, shutting down...", s)
		if err = server.Shutdown(context.Background()); err != nil {
			klog.Errorf("Error while stopping extender service: %s", err.Error())
		}
		cancelFunc()
	case <-ctx.Done():
		klog.Errorln("Internal error, service abnormal stop")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
}
