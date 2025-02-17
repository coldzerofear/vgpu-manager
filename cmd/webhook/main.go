package main

import (
	"flag"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"

	"github.com/coldzerofear/vgpu-manager/cmd/webhook/options"
	pkgwebhook "github.com/coldzerofear/vgpu-manager/pkg/webhook"
	"github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

func main() {
	klog.InitFlags(flag.CommandLine)
	opt := options.NewOptions()
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	opt.InitFlags(pflag.CommandLine)
	defer klog.Flush()
	opt.PrintAndExitIfRequested()
	ctrl.SetLogger(klog.NewKlogr())

	go func() {
		if opt.PprofBindPort > 0 {
			addr := "0.0.0.0:" + strconv.Itoa(opt.PprofBindPort)
			klog.V(4).Infof("Debug Server starting on <%s>", addr)
			klog.V(4).ErrorS(http.ListenAndServe(addr, nil), "Debug Server error occurred")
		}
	}()

	klog.Infoln("Create webhook server")
	server := webhook.NewServer(webhook.Options{
		Port:    opt.ServerBindProt,
		CertDir: opt.CertDir,
	})

	// init probe
	probeHandler := &healthz.Handler{
		Checks: map[string]healthz.Checker{
			"healthz": healthz.Ping,
			"readyz":  server.StartedChecker(),
		},
	}
	klog.Infoln("Init webhook server probe")
	server.Register("/healthz", probeHandler)
	server.Register("/readyz", probeHandler)

	if err := pkgwebhook.RegistryWebhookToServer(server, scheme.Scheme, opt); err != nil {
		klog.Fatalf("Registry webhook to server failed: %v", err)
	}

	klog.Infoln("Starting webhook server")
	if err := server.Start(ctrl.SetupSignalHandler()); err != nil {
		klog.ErrorS(err, "problem running webhook server")
		os.Exit(1)
	}
}
