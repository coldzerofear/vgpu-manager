package main

import (
	"crypto/tls"
	"flag"

	"github.com/coldzerofear/vgpu-manager/cmd/webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/route"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	pkgwebhook "github.com/coldzerofear/vgpu-manager/pkg/webhook"
	tlsserver "github.com/grepplabs/cert-source/tls/server"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

func main() {
	opt := options.NewOptions()
	opt.InitFlags(flag.CommandLine)
	opt.PrintAndExitIfRequested()
	logs.InitLogs()
	defer logs.FlushLogs()
	log.SetLogger(klog.NewKlogr())
	util.MustInitGlobalDomain(opt.Domain)

	// Start pprof debug debugging service.
	route.StartDebugServer(opt.PprofBindPort)
	klog.Infoln("Create webhook server")
	server := webhook.NewServer(webhook.Options{
		Port:    opt.ServerBindPort,
		CertDir: opt.CertDir,
		TLSOpts: []func(*tls.Config){
			// Using http/1.1 will prevent from being vulnerable to the HTTP/2 Stream Cancellation and Rapid Reset CVEs.
			// For more information see:
			// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
			// - https://github.com/advisories/GHSA-4374-p667-p6c8
			tlsserver.WithTLSServerNextProtos([]string{"http/1.1"}),
		},
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

	if err := pkgwebhook.RegisterWebhookToServer(server, scheme.Scheme, opt); err != nil {
		klog.Fatalf("Register webhook to server failed: %v", err)
	}

	klog.Infoln("Starting webhook server")
	if err := server.Start(signals.SetupSignalHandler()); err != nil {
		klog.ErrorS(err, "problem running webhook server")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
}
