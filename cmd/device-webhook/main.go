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
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/route"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	pkgwebhook "github.com/coldzerofear/vgpu-manager/pkg/webhook"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	tlsserver "github.com/grepplabs/cert-source/tls/server"
	resourcev1 "k8s.io/api/resource/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	rtclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

var (
	cacheSyncTimeout = 2 * time.Minute
)

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
	parentCtx context.Context,
	cancel context.CancelFunc,
	c cache.Cache,
	warmupObjects []rtclient.Object,
	gate *cacheSyncGate,
) error {

	for _, obj := range warmupObjects {
		if obj == nil {
			continue
		}
		if _, err := c.GetInformer(parentCtx, obj, cache.BlockUntilSynced(false)); err != nil {
			return fmt.Errorf("prewarm informer for %T failed: %w", obj, err)
		}
		klog.InfoS("Prewarmed informer", "type", fmt.Sprintf("%T", obj))
	}

	// Starting cache.Start in the background will block until the end of ctx.
	go func() {
		if err := c.Start(parentCtx); err != nil {
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
		syncCtx, syncCancel := context.WithTimeout(parentCtx, cacheSyncTimeout)
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

func newMirrorIndexer(informer cache.Informer) (k8scache.Indexer, error) {
	indexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{})
	_, err := informer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if err := indexer.Add(obj); err != nil {
				utilruntime.HandleErrorWithLogger(klog.Background(), err, "add object to mirror indexer")
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if err := indexer.Update(newObj); err != nil {
				utilruntime.HandleErrorWithLogger(klog.Background(), err, "update object in mirror indexer")
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := k8scache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err != nil {
				utilruntime.HandleErrorWithLogger(klog.Background(), err, "build delete key for mirror indexer")
				return
			}
			storedObj, exists, err := indexer.GetByKey(key)
			if err != nil {
				utilruntime.HandleErrorWithLogger(klog.Background(), err, "get object from mirror indexer by key")
				return
			}
			if !exists {
				return
			}
			if err := indexer.Delete(storedObj); err != nil {
				utilruntime.HandleErrorWithLogger(klog.Background(), err, "delete object from mirror indexer")
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return indexer, nil
}

func main() {
	opt := options.NewOptions()
	opt.InitFlags(flag.CommandLine)
	opt.PrintAndExitIfRequested()

	logs.InitLogs()
	defer logs.FlushLogs()

	log.SetLogger(klog.NewKlogr())
	util.MustInitGlobalDomain(opt.Domain)

	config, err := pkgclient.NewKubeConfig(pkgclient.WithDefaultUserAgent())
	if err != nil {
		klog.Fatalf("Initialization of kubeConfig failed: %v", err)
	}

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

	baseCtx := signals.SetupSignalHandler()
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	cacheGate := &cacheSyncGate{}

	// init probe
	probeHandler := &healthz.Handler{
		Checks: map[string]healthz.Checker{
			"healthz": healthz.Ping,
			"readyz":  combinedReadyz(server, cacheGate),
		},
	}
	klog.Infoln("Init webhook server probe")
	server.Register("/healthz", probeHandler)
	server.Register("/readyz", probeHandler)

	clientOptions := rtclient.Options{
		Scheme: scheme.Scheme,
	}

	var claimReader resourcereader.ClaimRequestReader
	var claimIndexer k8scache.Indexer
	var templateIndexer k8scache.Indexer
	var liveClient rtclient.Client

	if opt.DefaultConvertToDRA {
		if opt.VGPUDeviceClassName == "" {
			klog.Fatalln("When DRA resource conversion is enabled, an available vgpu device class must be specified")
		}
		// DRA defaults to enabling multi card GPU topology management.
		device.SetGPUTopologyEnabled(true)

		liveClient, err = rtclient.New(config, rtclient.Options{Scheme: scheme.Scheme})
		if err != nil {
			klog.Fatalf("Create live kubeClient failed: %v", err)
		}

		c, err := cache.New(config, cache.Options{
			Scheme:     clientOptions.Scheme,
			HTTPClient: clientOptions.HTTPClient,
			Mapper:     clientOptions.Mapper,
		})
		if err != nil {
			klog.Fatalf("Create clientCache failed: %v", err)
		}
		warmupObjects := []rtclient.Object{
			&resourcev1.ResourceClaim{},
			&resourcev1.ResourceClaimTemplate{},
		}
		if err := startCacheAsync(ctx, cancel, c, warmupObjects, cacheGate); err != nil {
			klog.Fatalf("Start clientCache failed: %v", err)
		}

		clientOptions.Cache = &rtclient.CacheOptions{Reader: c}

		claimInformer, err := c.GetInformer(ctx, &resourcev1.ResourceClaim{}, cache.BlockUntilSynced(false))
		if err != nil {
			klog.Fatalf("Get ResourceClaim informer failed: %v", err)
		}
		templateInformer, err := c.GetInformer(ctx, &resourcev1.ResourceClaimTemplate{}, cache.BlockUntilSynced(false))
		if err != nil {
			klog.Fatalf("Get ResourceClaimTemplate informer failed: %v", err)
		}

		claimIndexer, err = newMirrorIndexer(claimInformer)
		if err != nil {
			klog.Fatalf("Create ResourceClaim mirror indexer failed: %v", err)
		}
		templateIndexer, err = newMirrorIndexer(templateInformer)
		if err != nil {
			klog.Fatalf("Create ResourceClaimTemplate mirror indexer failed: %v", err)
		}
	} else {
		cacheGate.MarkReady()
	}

	client, err := rtclient.New(config, clientOptions)
	if err != nil {
		klog.Fatalf("Create kubeClient failed: %v", err)
	}
	if opt.DefaultConvertToDRA {
		// The mutation cache overlays informer snapshots with fresher write-through
		// updates and live-API fallback results.
		claimReader = resourcereader.NewClaimRequestReader(client, liveClient, claimIndexer, templateIndexer, time.Minute)
	}

	if err := pkgwebhook.RegisterWebhookToServer(server, client, opt, claimReader); err != nil {
		klog.Fatalf("Register webhook to server failed: %v", err)
	}

	klog.Infoln("Starting webhook server")
	if err := server.Start(ctx); err != nil {
		klog.ErrorS(err, "problem running webhook server")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
}
