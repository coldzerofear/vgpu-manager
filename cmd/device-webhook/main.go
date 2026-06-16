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
)

var (
	cacheSyncTimeout = 2 * time.Minute
	Scheme           = scheme.Scheme
)

func init() {
	utilruntime.Must(admissionv1.AddToScheme(Scheme))
	utilruntime.Must(admissionv1beta1.AddToScheme(Scheme))
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

func runApp(opt *options.Options) (exitCode int) {
	exitCode = 1

	util.MustInitGlobalDomain(opt.Domain)

	config, err := pkgclient.NewKubeConfig(pkgclient.WithDefaultUserAgent())
	if err != nil {
		klog.Errorf("Initialization of kubeConfig failed: %v", err)
		return exitCode
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
		Scheme: Scheme,
	}

	var (
		resourceReader     resourcereader.ResourceAPIReader
		claimObjectType    = &resourcev1.ResourceClaim{}
		templateObjectType = &resourcev1.ResourceClaimTemplate{}
		classObjectType    = &resourcev1.DeviceClass{}
		podObjectType      = &corev1.Pod{}
		liveClient         rtclient.Client
		recorder           events.EventRecorderLogger
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
		liveClient, err = rtclient.New(config, rtclient.Options{Scheme: Scheme})
		if err != nil {
			klog.Errorf("Create live kubeClient failed: %v", err)
			return exitCode
		}

		kubeClient, err := kubernetes.NewForConfig(config)
		if err != nil {
			klog.Errorf("Create kubeClient failed: %v", err)
			return exitCode
		}

		broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: kubeClient.EventsV1()})
		if err = broadcaster.StartRecordingToSinkWithContext(ctx); err != nil {
			klog.Errorf("Failed to start Sink: %v", err)
			return exitCode
		}
		stopLog, err := broadcaster.StartLogging(klog.Background())
		if err != nil {
			klog.Errorf("Failed to start Logging: %v", err)
			return exitCode
		}
		defer func() {
			stopLog()
			broadcaster.Shutdown()
		}()

		recorder = broadcaster.NewRecorder(Scheme, util.ComponentName)

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

		warmupObjects := []rtclient.Object{
			claimObjectType,
			templateObjectType,
			classObjectType,
			podObjectType,
		}
		if err := startCacheAsync(ctx, cancel, c, warmupObjects, cacheGate); err != nil {
			klog.Errorf("Start clientCache failed: %v", err)
			return exitCode
		}
		objIndexerMap = make(map[rtclient.Object]k8scache.Indexer)
		clientOptions.Cache = &rtclient.CacheOptions{Reader: c}

		for _, object := range warmupObjects {
			informer, err := c.GetInformer(ctx, object, cache.BlockUntilSynced(false))
			if err != nil {
				klog.Errorf("Get %T informer failed: %v", object, err)
				return exitCode
			}
			indexer, _, err := util.NewMirrorIndexer(informer)
			if err != nil {
				klog.Errorf("Create %T mirror indexer failed: %v", object, err)
				return exitCode
			}
			objIndexerMap[object] = indexer
		}

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
		resourceReader = resourcereader.NewResourceAPIReader(liveClient,
			objIndexerMap[claimObjectType], objIndexerMap[templateObjectType],
			objIndexerMap[classObjectType], objIndexerMap[podObjectType],
			30*time.Second)
	}

	if err := pkgwebhook.RegisterWebhookToServer(server, cacheGate, client, opt, resourceReader, recorder); err != nil {
		klog.Errorf("Register webhook to server failed: %v", err)
		return exitCode
	}

	klog.Infoln("Starting webhook server")
	if err := server.Start(ctx); err != nil {
		klog.ErrorS(err, "problem running webhook server")
		exitCode = 1
	} else {
		exitCode = 0
	}

	return exitCode
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
