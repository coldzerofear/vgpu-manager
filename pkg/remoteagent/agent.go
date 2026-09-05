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
	// ServerEndpoint is the lupine-server address to probe, URL form
	// (http://host:port[/path]). The host is normally a loopback: the server
	// runs in the same pod. What other nodes are told is decided separately,
	// see AdvertiseEndpoint and serverState.Endpoint.
	ServerEndpoint string
	// AdvertiseEndpoint, when set, is reported to callers of ServerInfo as the
	// server's endpoint verbatim (URL form) instead of the discovered one.
	// For DNS names and gateways that this host cannot resolve or reach
	// itself; it is not probed.
	AdvertiseEndpoint string
	// ListenEndpoints are the gRPC listen addresses, URL form; grpc://host:port
	// for TCP (empty host = all interfaces) and unix:///path for a socket.
	// The same service is served on every one of them.
	ListenEndpoints []string
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

	nodeDevices      atomic.Pointer[NodeDevices]
	smWatcherPresent atomic.Bool

	// server is the latest lupine-server observation (never nil after New).
	server atomic.Pointer[serverState]
	// probeMu serialises probes: the periodic loop and an on-demand probe
	// from EnsureSession must not run discovery twice at once.
	probeMu sync.Mutex

	// nodeAddrs caches the node's InternalIP list for discovery.
	nodeAddrsMu sync.Mutex
	nodeAddrs   []string
	nodeAddrsAt time.Time

	// hasReady reports (without blocking) whether every informer cache and
	// event-handler registration has synced; nil until Run wires it.
	hasReady func() bool
}

func New(cfg Config) *Agent {
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = time.Minute
	}
	store := NewSessionStore(cfg)
	a := &Agent{cfg: cfg, store: store}
	a.server.Store(&serverState{})
	return a
}

// serverSnapshot returns the latest lupine-server observation.
func (a *Agent) serverSnapshot() *serverState {
	return a.server.Load()
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

	// 4. gRPC: one server, every configured listener (TCP for other nodes,
	// a unix socket for same-node callers). All listeners must bind before
	// the ready file is written, so a bad address fails startup loudly.
	listeners, err := a.listen()
	if err != nil {
		return err
	}

	srv := grpc.NewServer()
	remoteagent.RegisterRemoteAgentServer(srv, a)
	grpc_health_v1.RegisterHealthServer(srv, a)

	a.wg.Go(func() {
		<-ctx.Done()
		srv.GracefulStop()
	})

	// Signal the server container only now: with the caches synced,
	// EnsureSession answers correctly from the first request.
	if err := a.writeReadyFile(); err != nil {
		for _, lis := range listeners {
			_ = lis.Close()
		}
		cancel()
		a.wg.Wait()
		return err
	}

	var (
		serveMu  sync.Mutex
		serveErr error
	)
	for _, lis := range listeners {
		a.wg.Go(func() {
			klog.Infof("remote-agent serving on %s://%s (node %s, session base %s)",
				lis.Addr().Network(), lis.Addr().String(), a.cfg.NodeName, a.cfg.SessionBase)
			err := srv.Serve(lis)
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) && !errors.Is(err, net.ErrClosed) {
				serveMu.Lock()
				if serveErr == nil {
					serveErr = fmt.Errorf("serve %s://%s: %w", lis.Addr().Network(), lis.Addr().String(), err)
				}
				serveMu.Unlock()
			}
			// One listener failing takes the agent down: a half-reachable
			// agent is worse than a restart.
			cancel()
		})
	}

	a.wg.Wait()

	return serveErr
}

// listen binds every configured endpoint. A unix socket path left behind by
// a previous instance is removed first, unless something still answers on
// it, which means two agents were configured for the same socket.
func (a *Agent) listen() ([]net.Listener, error) {
	var listeners []net.Listener
	closeAll := func() {
		for _, lis := range listeners {
			_ = lis.Close()
		}
	}
	if len(a.cfg.ListenEndpoints) == 0 {
		return nil, fmt.Errorf("no listen endpoint configured")
	}
	for _, raw := range a.cfg.ListenEndpoints {
		endpoint, err := endpointutil.ParseEndpoint(raw,
			endpointutil.WithDefaultScheme(endpointutil.Grpc),
			endpointutil.WithDefaultPort(remote.DefaultAgentPort))
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("listen endpoint %q: %w", raw, err)
		}
		var lis net.Listener
		switch endpoint.Scheme {
		case endpointutil.Unix:
			lis, err = listenUnix(endpoint.Path)
		case endpointutil.Grpc:
			lis, err = net.Listen("tcp", endpoint.HostPort())
		default:
			err = fmt.Errorf("unsupported listen scheme %q (want grpc:// or unix://)", endpoint.Scheme)
		}
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("listen endpoint %q: %w", raw, err)
		}
		listeners = append(listeners, lis)
	}
	return listeners, nil
}

func listenUnix(path string) (net.Listener, error) {
	if err := util.EnsureDir(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		// Stale socket from a previous instance, or a live one from another?
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%s is already served by another process", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The callers are node components running as root; keep the socket
	// from being a channel for anything else on the host.
	if err := os.Chmod(path, 0o660); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return lis, nil
}

// Check implements [grpc_health_v1.HealthServer].
//
//   - "" and "liveness": the informer caches have synced (the agent can
//     answer EnsureSession at all);
//   - "readiness": that, and lupine-server answered the last probe -- the
//     signal a consumer should gate on before sending sessions here.
func (a *Agent) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	ready := func() bool { return a.hasReady != nil && a.hasReady() }
	knownServices := map[string]func() bool{
		"":         ready,
		"liveness": ready,
		"readiness": func() bool {
			return ready() && a.serverSnapshot().Up
		},
	}
	checkFn, known := knownServices[req.GetService()]
	if !known {
		return nil, status.Error(codes.NotFound, "unknown service")
	}
	resp := &grpc_health_v1.HealthCheckResponse{
		Status: grpc_health_v1.HealthCheckResponse_SERVING,
	}
	if !checkFn() {
		resp.Status = grpc_health_v1.HealthCheckResponse_NOT_SERVING
	}
	return resp, nil
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
// also tells us which CUDA version the server was built with. On success it
// also settles the endpoint other nodes should use (see resolveEndpoint) and
// publishes the whole observation at once.
//
// Probes are serialised: when the periodic loop and an on-demand probe from
// EnsureSession collide, only one runs (see probe).
func (a *Agent) probeServer(ctx context.Context) {
	a.probe(ctx, true)
}

// probe is one observation of lupine-server. discover allows the (bounded
// but slow) address discovery; the on-demand probe from EnsureSession runs
// without it so it stays well inside the caller's deadline, and skips
// entirely when a periodic probe is already in flight -- its result lands
// in a moment anyway.
func (a *Agent) probe(ctx context.Context, discover bool) {
	if discover {
		a.probeMu.Lock()
	} else if !a.probeMu.TryLock() {
		return
	}
	defer a.probeMu.Unlock()

	prev := a.serverSnapshot()
	next := &serverState{CudaVersion: prev.CudaVersion, Endpoint: prev.Endpoint, LastProbe: time.Now()}

	version, err := remote.ProbeServerCUDAVersion(ctx, a.cfg.ServerEndpoint, candidateProbeTimeout)
	if err != nil {
		// Keep the last known version and endpoint (see serverState); only
		// reachability flips.
		a.server.Store(next)
		if prev.Up {
			klog.Infof("lupine-server %s not answering: %v", a.cfg.ServerEndpoint, err)
		}
		return
	}
	next.Up = true
	next.CudaVersion = version.Original()
	next.Endpoint = a.resolveEndpoint(ctx, prev.Endpoint, discover)
	a.server.Store(next)

	switch {
	case !prev.Up:
		klog.Infof("lupine-server %s answering, built for CUDA %s, advertised as %q", a.cfg.ServerEndpoint, next.CudaVersion, next.Endpoint)
	case prev.CudaVersion != next.CudaVersion:
		klog.Infof("lupine-server %s now built for CUDA %s (was %s)", a.cfg.ServerEndpoint, next.CudaVersion, prev.CudaVersion)
	}
	if prev.Endpoint != next.Endpoint {
		klog.Infof("lupine-server advertised endpoint changed: %q -> %q", prev.Endpoint, next.Endpoint)
	}
}

// resolveEndpoint decides what ServerInfo reports as the server's endpoint,
// given that the probe endpoint answered just now:
//
//   - AdvertiseEndpoint set: that, verbatim (operator knows best);
//   - probe host routable (not loopback/unspecified/localhost): the probe
//     endpoint itself;
//   - probe host is a loopback: the previously discovered endpoint if it
//     still answers (sticky, so a flaky candidate does not make the
//     published attribute flap), else -- when discover allows it -- a fresh
//     discovery over this host's addresses; "" when nothing routable
//     answers (or discovery was not allowed this time).
func (a *Agent) resolveEndpoint(ctx context.Context, current string, discover bool) string {
	if a.cfg.AdvertiseEndpoint != "" {
		return a.cfg.AdvertiseEndpoint
	}
	probe, err := endpointutil.ParseEndpoint(a.cfg.ServerEndpoint,
		endpointutil.WithDefaultScheme(endpointutil.Http),
		endpointutil.WithDefaultPort(remote.DefaultServerPort))
	if err != nil {
		// Validated at startup; unreachable in practice.
		klog.Errorf("server endpoint %q: %v", a.cfg.ServerEndpoint, err)
		return ""
	}
	if !probe.IsLoopback() {
		return probe.String()
	}
	if current != "" {
		if _, err := remote.ProbeServerCUDAVersion(ctx, current, candidateProbeTimeout); err == nil {
			return current
		}
		if !discover {
			// Keep it until the periodic probe can rediscover: flapping the
			// published attribute on a quick check helps nobody.
			return current
		}
		klog.V(2).Infof("advertised endpoint %s stopped answering; rediscovering", current)
	}
	if !discover {
		return ""
	}
	return a.discoverExternalEndpoint(ctx, probe)
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
	// The session is on disk; whether the pod may start depends on the
	// server accepting connections. The periodic probe can lag a server
	// restart by one period, so a negative answer is re-checked right now
	// rather than failing the caller's NodePrepare on stale state.
	state := a.serverSnapshot()
	if !state.Up {
		a.probe(ctx, false)
		state = a.serverSnapshot()
	}
	var msg string
	if !state.Up {
		msg = "lupine-server is not accepting connections yet"
	}
	return &remoteagent.EnsureSessionResponse{Ready: state.Up, CudaDriverVersion: nd.CudaVersionString(), Message: msg}, nil
}

// ServerInfo implements remoteagent.RemoteAgentServer. It is the one place
// other components learn about lupine-server from: whether it answers, the
// CUDA version it was built with, and the endpoint they should hand to
// clients -- so none of them needs the server's address configured, and a
// server that moves (or an operator that sets --advertise-server-endpoint)
// is picked up on the next call.
//
// endpoint is "" while no routable address is known (probe host is a
// loopback and discovery found nothing); cuda_driver_version is "" until
// the first successful probe. Both keep their last value while listening
// is false.
func (a *Agent) ServerInfo(context.Context, *remoteagent.ServerInfoRequest) (*remoteagent.ServerInfoResponse, error) {
	state := a.serverSnapshot()
	return &remoteagent.ServerInfoResponse{
		Listening:         state.Up,
		Endpoint:          state.Endpoint,
		CudaDriverVersion: state.CudaVersion,
		NodeName:          a.cfg.NodeName,
	}, nil
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
