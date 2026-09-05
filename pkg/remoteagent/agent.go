/*
Copyright 2026 coldzerofear

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

// Package remoteagent implements the GPU-node agent that runs alongside
// lupine-server (design v2.0, D24): it prepares the session base directory,
// signals readiness to the server container through a file on the shared
// volume, materializes session quotas on demand (EnsureSession gRPC, D20)
// and on claim events, garbage-collects sessions of gone claims, and probes
// whether lupine-server is accepting connections.
package remoteagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

const claimUIDIndex = "claim-uid"

type Config struct {
	NodeName   string
	DriverName string
	// SessionBase is the session directory root shared with lupine-server
	// (VGPU_CONFIG_SESSION_BASE on the server side).
	SessionBase         string
	ContainerManagerDir string
	// ReadyFile is written after preflight; the server container waits for it.
	ReadyFile string
	// ServerEndpoint is the lupine-server address to probe (host:port).
	ServerEndpoint string
	// ListenEndpoint is the gRPC listen port (all interfaces).
	ListenEndpoint string
	// GCInterval bounds how often orphaned sessions are swept.
	GCInterval  time.Duration
	FeatureGate featuregate.MutableVersionedFeatureGate
	ClientSets  pkgflags.ClientSets
}

// gateEnabled is a nil-safe feature gate check (nil gate = all off).
func (c Config) gateEnabled(feature featuregate.Feature) bool {
	return c.FeatureGate != nil && c.FeatureGate.Enabled(feature)
}

type Agent struct {
	grpc_health_v1.UnimplementedHealthServer
	remoteagent.UnimplementedRemoteAgentServer

	wg    sync.WaitGroup
	cfg   Config
	store *SessionStore

	sliceInformer cache.SharedIndexInformer
	claimInformer cache.SharedIndexInformer
	claimCache    cache.MutationCache

	nodeDevices            atomic.Pointer[NodeDevices]
	serverCudaVersion      atomic.Pointer[string]
	serverExternalEndpoint atomic.Pointer[string]
	serverUp               atomic.Bool
	smWatcherPresent       atomic.Bool

	// hasReady reports (without blocking) whether every informer cache and
	// event-handler registration has synced; nil until Run wires it.
	hasReady func() bool
}

func New(cfg Config) *Agent {
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = time.Minute
	}
	store := NewSessionStore(cfg)
	return &Agent{cfg: cfg, store: store}
}

// Run blocks until ctx is done.
func (a *Agent) Run(ctx context.Context) error {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 1. Preflight: session skeleton + the watcher symlink. The ready file
	// comes later, once the informers are synced (step 2).
	if err := a.store.Prepare(); err != nil {
		return err
	}

	// 2. Informers: the node's own slices (device snapshot) and all claims
	// (lifecycle). ResourceSlice has a spec.driver field selector; claims
	// have none, narrowing happens client-side.
	a.sliceInformer = cache.NewSharedIndexInformer(
		cache.NewListWatchFromClient(a.cfg.ClientSets.Resource.RESTClient(), "resourceslices", corev1.NamespaceAll,
			fields.AndSelectors(
				fields.OneTermEqualSelector(resourceapi.ResourceSliceSelectorDriver, a.cfg.DriverName),
				// TODO "Failed to watch" err="failed to list *v1.ResourceSlice: field label not supported for resource.k8s.io/v1, Kind=ResourceSlice: spec.pool.name" logger="UnhandledError" reflector="pkg/mod/k8s.io/client-go@v0.37.0-rc.0/tools/cache/reflector.go:343" type="*v1.ResourceSlice"
				//fields.OneTermEqualSelector(resourceapi.ResourceSliceSelectorPoolName, a.cfg.NodeName),
			),
		), &resourceapi.ResourceSlice{}, 10*time.Hour, cache.Indexers{})
	if err := a.sliceInformer.SetTransform(crcache.TransformStripManagedFields()); err != nil {
		return err
	}
	a.claimInformer = cache.NewSharedIndexInformer(
		cache.NewListWatchFromClient(a.cfg.ClientSets.Resource.RESTClient(), "resourceclaims", corev1.NamespaceAll,
			fields.Everything()), &resourceapi.ResourceClaim{}, 10*time.Hour, cache.Indexers{
			claimUIDIndex: func(obj interface{}) ([]string, error) {
				if c, ok := obj.(*resourceapi.ResourceClaim); ok {
					return []string{string(c.UID)}, nil
				}
				return nil, nil
			},
		})
	// The cache keeps only the fields the agent reads (see trimClaim);
	// EnsureSession re-fetches the full object from the API when the
	// trimmed one turns out stale.
	if err := a.claimInformer.SetTransform(trimClaim(a.cfg.DriverName, a.cfg.NodeName)); err != nil {
		return err
	}

	sliceRegistration, err := a.sliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { a.refreshNodeDevices() },
		UpdateFunc: func(_, _ interface{}) { a.refreshNodeDevices() },
		DeleteFunc: func(interface{}) { a.refreshNodeDevices() },
	})
	if err != nil {
		return err
	}
	claimRegistration, err := a.claimInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if c, ok := obj.(*resourceapi.ResourceClaim); ok &&
				(c.Status.Allocation == nil || !c.DeletionTimestamp.IsZero()) {
				a.removeSessionsOfClaim(string(c.UID))
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if c, ok := newObj.(*resourceapi.ResourceClaim); ok &&
				(c.Status.Allocation == nil || !c.DeletionTimestamp.IsZero()) {
				a.removeSessionsOfClaim(string(c.UID))
			}
		},
		DeleteFunc: func(obj interface{}) {
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			if c, ok := obj.(*resourceapi.ResourceClaim); ok {
				a.removeSessionsOfClaim(string(c.UID))
			}
		},
	})
	if err != nil {
		return err
	}

	a.claimCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(),
		a.claimInformer.GetStore(),
		a.claimInformer.GetIndexer(),
		time.Minute, true,
	)

	// Non-blocking on purpose: the health Check must answer within the
	// probe deadline, not wait for a sync (and not log a wait line per probe).
	synced := []cache.InformerSynced{
		a.sliceInformer.HasSynced,
		a.claimInformer.HasSynced,
		sliceRegistration.HasSynced,
		claimRegistration.HasSynced,
	}
	a.hasReady = func() bool {
		for _, hasSynced := range synced {
			if !hasSynced() {
				return false
			}
		}
		return true
	}

	a.wg.Go(func() { a.sliceInformer.RunWithContext(ctx) })
	a.wg.Go(func() { a.claimInformer.RunWithContext(ctx) })

	syncCtx, syncCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer syncCancel()

	if !cache.WaitForNamedCacheSyncWithContext(
		syncCtx,
		a.sliceInformer.HasSynced,
		a.claimInformer.HasSynced,
	) {
		return fmt.Errorf("informers cache synchronization timeout")
	}

	// 3. Background loops.
	a.wg.Go(func() { wait.UntilWithContext(ctx, a.probeServer, 5*time.Second) })
	a.wg.Go(func() { wait.UntilWithContext(ctx, a.checkSMWatcher, 30*time.Second) })
	a.wg.Go(func() { wait.UntilWithContext(ctx, a.gcSessions, a.cfg.GCInterval) })

	// 4. gRPC.
	endpoint, _ := endpointutil.ParseEndpoint(a.cfg.ListenEndpoint)

	lis, err := net.Listen("tcp", endpoint.HostPort())
	if err != nil {
		return fmt.Errorf("listen endpoint %q: %w", endpoint.HostPort(), err)
	}

	srv := grpc.NewServer()
	remoteagent.RegisterRemoteAgentServer(srv, a)
	grpc_health_v1.RegisterHealthServer(srv, a)

	a.wg.Go(func() {
		<-ctx.Done()
		srv.GracefulStop()
	})

	a.wg.Go(func() {
		defer cancel()
		// Signal the server container only now: with the caches synced,
		// EnsureSession answers correctly from the first request.
		if err = a.writeReadyFile(); err != nil {
			return
		}
		klog.Infof("remote-agent serving on %s (node %s, session base %s)", endpoint.HostPort(), a.cfg.NodeName, a.cfg.SessionBase)
		if err = srv.Serve(lis); err != nil && (errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, net.ErrClosed)) {
			err = nil
		}
	})

	a.wg.Wait()

	return err
}

// Check implements [grpc_health_v1.HealthServer].
func (a *Agent) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	knownServices := map[string]func() bool{
		"": a.hasReady, "liveness": a.hasReady, "readiness": func() bool {
			return a.hasReady() && a.serverUp.Load()
		},
	}
	checkFn, known := knownServices[req.GetService()]
	if !known {
		return nil, status.Error(codes.NotFound, "unknown service")
	}
	status := &grpc_health_v1.HealthCheckResponse{
		Status: grpc_health_v1.HealthCheckResponse_SERVING,
	}
	if !checkFn() {
		status.Status = grpc_health_v1.HealthCheckResponse_NOT_SERVING
	}
	return status, nil
}

func (a *Agent) writeReadyFile() error {
	if err := util.EnsureDir(filepath.Dir(a.cfg.ReadyFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(a.cfg.ReadyFile, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write ready file %s: %w", a.cfg.ReadyFile, err)
	}
	klog.Infof("Wrote ready file %s", a.cfg.ReadyFile)
	return nil
}

func (a *Agent) refreshNodeDevices() {
	objs := a.sliceInformer.GetStore().List()
	slices := make([]*resourceapi.ResourceSlice, 0, len(objs))
	for _, obj := range objs {
		if s, ok := obj.(*resourceapi.ResourceSlice); ok && s.DeletionTimestamp.IsZero() &&
			s.Spec.Pool.Name == a.cfg.NodeName && s.Spec.Driver == a.cfg.DriverName {
			slices = append(slices, s)
		}
	}
	nd := NodeRemoteDevicesFromSlices(slices)
	a.nodeDevices.Store(nd)
	klog.V(4).Infof("Node device snapshot: %d device(s), CUDA %q", len(nd.Devices), nd.CudaVersionString())
}

// probeServer checks that lupine-server really answers, not just that the
// port is open: one HTTP GET on the RPC port (served since lupine #660) that
// also tells us which CUDA version the server was built with.
func (a *Agent) probeServer(ctx context.Context) {
	version, err := remote.ProbeServerCUDAVersion(ctx, a.cfg.ServerEndpoint, 2*time.Second)
	up := err == nil
	if a.serverUp.Swap(up) != up {
		if up {
			klog.Infof("lupine-server %s answering, built for CUDA %s", a.cfg.ServerEndpoint, version)
		} else {
			klog.Infof("lupine-server %s not answering: %v", a.cfg.ServerEndpoint, err)
		}
	}
	if up {
		original := version.Original()
		a.serverCudaVersion.Store(&original)
	}
}

// gcSessions removes sessions whose claim no longer exists or is no longer
// allocated. Sessions without a marker are incomplete and removed too — a
// Materialize in flight holds the store mutex, so it cannot be raced here.
func (a *Agent) gcSessions(context.Context) {
	entries, err := a.store.List()
	if err != nil {
		klog.Warningf("list sessions: %v", err)
		return
	}
	for _, e := range entries {
		if e.ClaimUID != "" && a.claimAllocated(e.ClaimUID) {
			continue
		}
		if err := a.store.Remove(e.Token); err != nil {
			klog.Warningf("gc session %s: %v", e.Token, err)
		}
	}
}

func (a *Agent) claimAllocated(uid string) bool {
	c, _ := a.GetClaimByUID(uid)
	return c != nil && c.Status.Allocation != nil
}

func (a *Agent) GetClaimByUID(uid string) (*resourceapi.ResourceClaim, error) {
	objs, err := a.claimCache.ByIndex(claimUIDIndex, uid)
	if err != nil {
		return nil, err
	}
	if len(objs) == 0 {
		return nil, apierrors.NewNotFound(resourceapi.Resource("resourceclaims"), uid)
	}
	return objs[0].(*resourceapi.ResourceClaim), nil
}

func (a *Agent) removeSessionsOfClaim(uid string) {
	for _, token := range a.store.TokensOfClaim(uid) {
		if err := a.store.Remove(token); err != nil {
			klog.Warningf("remove session %s: %v", token, err)
		}
	}
}

// EnsureSession implements remoteagent.RemoteAgentServer.
func (a *Agent) EnsureSession(ctx context.Context, req *remoteagent.EnsureSessionRequest) (*remoteagent.EnsureSessionResponse, error) {
	if err := validateToken(req.Session); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	claim, err := a.GetClaimByUID(req.ClaimUid)
	if err != nil && apierrors.IsNotFound(err) {
		// Informer lag: fall back to a direct read and verify identity.
		claim, err = a.cfg.ClientSets.Resource.ResourceClaims(req.ClaimNamespace).Get(ctx, req.ClaimName, metav1.GetOptions{})
		if err != nil && apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "claim %s not found", req.ClaimUid)
		}
		if err == nil && string(claim.UID) != req.ClaimUid {
			return nil, status.Errorf(codes.NotFound, "claim %s not found", req.ClaimUid)
		}
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "get claim failed: %v", err)
	}
	a.claimCache.Mutation(claim)

	nd := a.nodeDevices.Load()
	if nd == nil || len(nd.Devices) == 0 {
		return nil, status.Error(codes.Unavailable, "node device snapshot not available yet")
	}
	if err = a.store.Materialize(req.Session, claim, nd, req.Requests); err != nil {
		// The cached claim may lag behind the allocation the caller saw;
		// retry once against the live object before giving up.
		fresh, getErr := a.cfg.ClientSets.Resource.ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
		if getErr != nil || string(fresh.UID) != req.ClaimUid {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		a.claimCache.Mutation(fresh)
		if err = a.store.Materialize(req.Session, fresh, nd, req.Requests); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
	}
	var msg string
	ready := a.serverUp.Load()
	if !ready {
		msg = "lupine-server is not accepting connections yet"
	}
	return &remoteagent.EnsureSessionResponse{Ready: ready, CudaDriverVersion: nd.CudaVersionString(), Message: msg}, nil
}

// ServerInfo implements remoteagent.RemoteAgentServer.
func (a *Agent) ServerInfo(context.Context, *remoteagent.ServerInfoRequest) (*remoteagent.ServerInfoResponse, error) {
	resp := &remoteagent.ServerInfoResponse{
		Listening: a.serverUp.Load(),
		Endpoint:  a.cfg.ServerEndpoint,
		NodeName:  a.cfg.NodeName,
	}
	if load := a.serverCudaVersion.Load(); load != nil {
		resp.CudaDriverVersion = *load
	}
	return resp, nil
}

// checkSMWatcher verifies the external SM watcher contract when sessions are
// written with SMWatcher on. The library reads the shared cache at
// <session-base>/watcher/sm_util.config; the store's Prepare() links that
// directory to <manager-dir>/watcher, where the dra-server plugin
// (SharedSMUtilizationWatcher) writes the file. This check stats straight
// through the symlink, so it fails when either the link or the file is
// missing. A missing file is not fatal (the library falls back to
// per-process NVML sampling) but it silently forfeits the shared-sampling
// benefit, so it is called out.
func (a *Agent) checkSMWatcher(context.Context) {
	if !a.cfg.gateEnabled(util.SharedSMUtilizationWatcher) {
		return
	}
	path := filepath.Join(a.cfg.SessionBase, util.Watcher, util.SMUtilFile)
	_, err := os.Stat(path)
	present := err == nil
	if a.smWatcherPresent.Swap(present) != present {
		if present {
			klog.Infof("external SM watcher cache %s is present", path)
		} else {
			klog.Warningf("SharedSMUtilizationWatcher is on but %s is missing: check the dra-server plugin has the gate enabled (it writes the cache), or sessions fall back to NVML sampling", path)
		}
	}
}
