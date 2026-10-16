/*
Copyright 2025-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	pkgclient "github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/route"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	pkgwebhook "github.com/coldzerofear/vgpu-manager/pkg/webhook"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	tlsserver "github.com/grepplabs/cert-source/tls/server"
	admissionv1 "k8s.io/api/admission/v1"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	rtclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	vcv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
)

var (
	cacheSyncTimeout = 2 * time.Minute
	Scheme           = scheme.Scheme
)

func init() {
	utilruntime.Must(admissionv1.AddToScheme(Scheme))
	utilruntime.Must(admissionv1beta1.AddToScheme(Scheme))
	utilruntime.Must(vcv1alpha1.AddToScheme(Scheme))
}

// cacheSyncGate indicates whether the cache has completed initial synchronization.
// - ready=false, err=nil: Still synchronizing
// - ready=true: Synchronized completed
// - err!=nil: Startup/synchronization failed
type cacheSyncGate struct {
	mu    sync.RWMutex
	ready bool
	err   error
}

func (g *cacheSyncGate) MarkReady() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ready = true
	g.err = nil
}

func (g *cacheSyncGate) MarkError(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.err = err
}

func (g *cacheSyncGate) Check(_ *http.Request) error {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if g.err != nil {
		return g.err
	}
	if !g.ready {
		return fmt.Errorf("cache not synced yet")
	}
	return nil
}

// combinedReadyz Simultaneously check if the webhook server is started and if the cache is ready.
func combinedReadyz(server webhook.Server, cacheGate *cacheSyncGate) healthz.Checker {
	startedChecker := server.StartedChecker()

	return func(req *http.Request) error {
		if err := startedChecker(req); err != nil {
			return err
		}
		return cacheGate.Check(req)
	}
}

// startCacheAsync：
// 1. Preheat informant (without blocking sync)
// 2. Start cache in the background
// 3. Wait for cache sync in the background
// 4. Cancel the entire process if sync timeout or failure occurs, and restart kubelet
func startCacheAsync(
	ctx context.Context,
	cancel context.CancelFunc,
	c cache.Cache,
	gate *cacheSyncGate,
) error {
	// Starting cache.Start in the background will block until the end of ctx.
	go func() {
		if err := c.Start(ctx); err != nil {
			wrappedErr := fmt.Errorf("cache start failed: %w", err)
			gate.MarkError(wrappedErr)
			klog.ErrorS(err, "cache exited unexpectedly")
			cancel()
			return
		}

		// Normally, we only come here after ParentCtx ends.
		klog.InfoS("Cache stopped")
	}()

	// The backend is waiting for the cache to complete synchronization; Do not block the main.
	go func() {
		syncCtx, syncCancel := context.WithTimeout(ctx, cacheSyncTimeout)
		defer syncCancel()

		ok := c.WaitForCacheSync(syncCtx)
		if !ok {
			err := fmt.Errorf("cache sync timeout or context cancelled")
			gate.MarkError(err)
			klog.ErrorS(err, "cache sync failed")
			cancel()
			return
		}

		gate.MarkReady()
		klog.InfoS("Cache synced successfully")
	}()

	return nil
}

func runApp(opt *options.Options) (exitCode int) {
	exitCode = 1

	util.MustInitGlobalDomain(opt.Domain)
	config, err := pkgclient.NewKubeConfig(
		pkgclient.WithConfigMasterURL(opt.MasterURL),
		pkgclient.WithKubeConfigPath(opt.KubeConfigFile),
		pkgclient.WithQPSBurst(opt.QPS, opt.Burst),
		pkgclient.WithDefaultUserAgent())
	if err != nil {
		klog.Errorf("Create kubeConfig failed: %v", err)
		return exitCode
	}
	if opt.ServerBindPort <= 0 || opt.ServerBindPort > 65535 {
		klog.Errorf("The service binding port range should be between 1-65535, current: %d", opt.ServerBindPort)
		return exitCode
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Errorf("Create kubeClient failed: %v", err)
		return exitCode
	}

	// Start pprof debug debugging service.
	route.StartDebugServer(opt.PprofBindPort)
	klog.Infoln("Create webhook server")
	server := webhook.NewServer(webhook.Options{
		Port:     opt.ServerBindPort,
		CertDir:  opt.CertDir,
		CertName: opt.TlsCertName,
		KeyName:  opt.TlsKeyName,
		TLSOpts: []func(*tls.Config){
			// Using http/1.1 will prevent from being vulnerable to the HTTP/2 Stream Cancellation and Rapid Reset CVEs.
			// For more information see:
			// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
			// - https://github.com/advisories/GHSA-4374-p667-p6c8
			tlsserver.WithTLSServerNextProtos([]string{"http/1.1"}),
		},
	})

	baseCtx := signals.SetupSignalHandler()
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	// init probe
	cacheGate := &cacheSyncGate{}
	probeHandler := &healthz.Handler{
		Checks: map[string]healthz.Checker{
			"healthz": healthz.Ping,
			"readyz":  combinedReadyz(server, cacheGate),
		},
	}
	klog.Infoln("Init webhook server probe")
	server.Register("/healthz", probeHandler)
	server.Register("/readyz", probeHandler)

	var (
		clientOptions      = rtclient.Options{Scheme: Scheme}
		resourceReader     resourcereader.ResourceAPIReader
		claimObjectType    = &resourcev1.ResourceClaim{}
		templateObjectType = &resourcev1.ResourceClaimTemplate{}
		classObjectType    = &resourcev1.DeviceClass{}
		podObjectType      = &corev1.Pod{}
		eventRecorder      events.EventRecorderLogger
		objIndexerMap      map[rtclient.Object]k8scache.Indexer
	)

	if opt.DefaultConvertToDRA {
		if opt.VGPUDeviceClassName == "" {
			klog.Errorf("When DRA resource conversion is enabled, an available vgpu device class must be specified")
			return exitCode
		}
	}

	if opt.CombinedResourceClaim && !opt.DefaultConvertToDRA {
		klog.Errorf("The prerequisite for enabling combination resource declaration is to enable default conversion to DRA")
		return exitCode
	}

	needInitClient := opt.DRAAdmissionEnabled || opt.DefaultConvertToDRA

	if needInitClient {
		c, err := cache.New(config, cache.Options{
			Scheme:           clientOptions.Scheme,
			HTTPClient:       clientOptions.HTTPClient,
			Mapper:           clientOptions.Mapper,
			DefaultTransform: cache.TransformStripManagedFields(),
		})
		if err != nil {
			klog.Errorf("Create clientCache failed: %v", err)
			return exitCode
		}

		broadcaster := events.NewEventBroadcasterAdapterWithContext(ctx, kubeClient)
		// Deferred after cancel, so it runs before it: Shutdown blocks until the
		// queued events reach the API server, and cancelling the context first
		// would stop the sinks that have to deliver them. Function-scoped, not
		// block-scoped, so it covers the whole run.
		defer broadcaster.Shutdown()
		eventRecorder = broadcaster.NewRecorder(util.ComponentName)
		broadcaster.StartRecordingToSink(ctx.Done())

		// Pre initialize informants, prewarmed cache
		objIndexerMap = make(map[rtclient.Object]k8scache.Indexer)
		for _, object := range []rtclient.Object{
			claimObjectType, templateObjectType, classObjectType, podObjectType,
		} {
			informer, err := c.GetInformer(ctx, object, cache.BlockUntilSynced(false))
			if err != nil {
				klog.Errorf("prewarm informer for %T failed: %v", object, err)
				return exitCode
			}
			klog.InfoS("Prewarmed informer", "type", fmt.Sprintf("%T", object))
			indexer, _, err := util.NewMirrorIndexer(informer)
			if err != nil {
				klog.Errorf("Create %T mirror indexer failed: %v", object, err)
				return exitCode
			}
			objIndexerMap[object] = indexer
		}

		if err := startCacheAsync(ctx, cancel, c, cacheGate); err != nil {
			klog.Errorf("Start clientCache failed: %v", err)
			return exitCode
		}
		clientOptions.Cache = &rtclient.CacheOptions{Reader: c}

	} else {
		cacheGate.MarkReady()
	}

	client, err := rtclient.New(config, clientOptions)
	if err != nil {
		klog.Errorf("Create kubeClient failed: %v", err)
		return exitCode
	}
	if needInitClient {
		// The mutation cache overlays informer snapshots with fresher write-through
		// updates and live-API fallback results.
		resourceReader = resourcereader.NewResourceAPIReader(
			kubeClient,
			objIndexerMap[claimObjectType], objIndexerMap[templateObjectType],
			objIndexerMap[classObjectType], objIndexerMap[podObjectType],
			30*time.Second,
		)
	}

	if err = pkgwebhook.RegisterWebhookToServer(
		opt, server, client, resourceReader,
		eventRecorder, healthCheckMiddleware(cacheGate),
	); err != nil {
		klog.Errorf("Register webhook to server failed: %v", err)
		return exitCode
	}

	klog.Infoln("Starting webhook server")
	if err = server.Start(ctx); err != nil {
		klog.ErrorS(err, "problem running webhook server")
		exitCode = 1
	} else {
		exitCode = 0
	}

	return exitCode
}

func healthCheckMiddleware(healthChecker *cacheSyncGate) func(http.Handler) http.Handler {
	return func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if healthChecker != nil {
				if err := healthChecker.Check(r); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			handler.ServeHTTP(w, r)
		})
	}
}

func main() {
	opt := options.NewOptions()
	opt.InitFlags(flag.CommandLine)
	opt.PrintAndExitIfRequested()

	logs.InitLogs()
	defer logs.FlushLogs()
	log.SetLogger(klog.NewKlogr())

	if exitCode := runApp(opt); exitCode != 0 {
		klog.FlushAndExit(klog.ExitFlushTimeout, exitCode)
	}
}
