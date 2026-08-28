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
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

const claimUIDIndex = "claim-uid"

type Config struct {
	NodeName   string
	DriverName string
	// SessionBase is the session directory root shared with lupine-server
	// (VGPU_CONFIG_SESSION_BASE on the server side).
	SessionBase string
	// ReadyFile is written after preflight; the server container waits for it.
	ReadyFile string
	// ServerAddr is the lupine-server address to probe (host:port).
	ServerAddr string
	// ListenPort is the gRPC listen port (all interfaces).
	ListenPort int
	// Endpoint is the endpoint value reported by ServerInfo (informational).
	Endpoint string
	// SMWatcher marks sessions as using the node-wide external SM watcher.
	SMWatcher bool
	// GCInterval bounds how often orphaned sessions are swept.
	GCInterval time.Duration

	ClientSets pkgflags.ClientSets
}

type Agent struct {
	remoteagent.UnimplementedRemoteAgentServer

	wg    sync.WaitGroup
	cfg   Config
	store *SessionStore

	sliceInformer cache.SharedIndexInformer
	claimInformer cache.SharedIndexInformer
	claimCache    cache.MutationCache

	nodeDevices atomic.Pointer[NodeDevices]
	serverUp    atomic.Bool
}

func New(cfg Config) *Agent {
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = time.Minute
	}
	store := NewSessionStore(cfg.SessionBase, cfg.SMWatcher)
	return &Agent{cfg: cfg, store: store}
}

// Run blocks until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	// 1. Preflight, then signal the server container. Readiness means "the
	// session skeleton exists", not "the agent is fully synced": the server
	// only needs the directories to start, and sessions are created later.
	if err := a.store.Prepare(); err != nil {
		return err
	}
	if err := a.writeReadyFile(); err != nil {
		return err
	}

	// 2. Informers: the node's own slices (device snapshot) and all claims
	// (lifecycle). ResourceSlice has a spec.driver field selector; claims
	// have none, narrowing happens client-side.
	a.sliceInformer = cache.NewSharedIndexInformer(
		cache.NewListWatchFromClient(a.cfg.ClientSets.Resource.RESTClient(), "resourceslices", corev1.NamespaceAll,
			fields.AndSelectors(
				fields.OneTermEqualSelector(resourceapi.ResourceSliceSelectorDriver, a.cfg.DriverName),
				fields.OneTermEqualSelector(resourceapi.ResourceSliceSelectorPoolName, a.cfg.NodeName),
			),
		), &resourceapi.ResourceSlice{}, 10*time.Hour, cache.Indexers{})
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

	if _, err := a.sliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { a.refreshNodeDevices() },
		UpdateFunc: func(_, _ interface{}) { a.refreshNodeDevices() },
		DeleteFunc: func(interface{}) { a.refreshNodeDevices() },
	}); err != nil {
		return err
	}
	if _, err := a.claimInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
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
	}); err != nil {
		return err
	}

	a.claimCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(),
		a.claimInformer.GetStore(),
		a.claimInformer.GetIndexer(),
		time.Minute, true)

	a.wg.Go(func() {
		a.sliceInformer.RunWithContext(ctx)
	})
	a.wg.Go(func() {
		a.claimInformer.RunWithContext(ctx)
	})

	syncCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if !cache.WaitForNamedCacheSyncWithContext(
		syncCtx,
		a.sliceInformer.HasSynced,
		a.claimInformer.HasSynced,
	) {
		return fmt.Errorf("informers did not sync")
	}
	a.refreshNodeDevices()

	// 3. Background loops.
	a.wg.Go(func() {
		wait.UntilWithContext(ctx, a.probeServer, 5*time.Second)
	})
	a.wg.Go(func() {
		wait.UntilWithContext(ctx, a.gcSessions, a.cfg.GCInterval)
	})

	// 4. gRPC.
	listenAddr := fmt.Sprintf(":%d", a.cfg.ListenPort)
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}

	srv := grpc.NewServer()
	remoteagent.RegisterRemoteAgentServer(srv, a)

	a.wg.Go(func() {
		<-ctx.Done()
		srv.GracefulStop()
	})

	a.wg.Go(func() {
		klog.Infof("remote-agent serving on %s (node %s, session base %s)", listenAddr, a.cfg.NodeName, a.cfg.SessionBase)
		if err = srv.Serve(lis); err != nil && (errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, net.ErrClosed)) {
			err = nil
		}
	})

	a.wg.Wait()

	return err
}

func (a *Agent) writeReadyFile() error {
	if err := os.MkdirAll(filepath.Dir(a.cfg.ReadyFile), 0o755); err != nil {
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

func (a *Agent) probeServer(context.Context) {
	conn, err := net.DialTimeout("tcp", a.cfg.ServerAddr, 2*time.Second)
	up := err == nil
	if up {
		_ = conn.Close()
	}
	if a.serverUp.Swap(up) != up {
		klog.Infof("lupine-server %s listening: %v", a.cfg.ServerAddr, up)
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
	c := a.claimByUID(uid)
	return c != nil && c.Status.Allocation != nil
}

func (a *Agent) claimByUID(uid string) *resourceapi.ResourceClaim {
	objs, err := a.claimCache.ByIndex(claimUIDIndex, uid)
	if err != nil || len(objs) == 0 {
		return nil
	}
	c, _ := objs[0].(*resourceapi.ResourceClaim)
	return c
}

func (a *Agent) removeSessionsOfClaim(uid string) {
	entries, err := a.store.List()
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.ClaimUID == uid {
			if err := a.store.Remove(e.Token); err != nil {
				klog.Warningf("remove session %s: %v", e.Token, err)
			}
		}
	}
}

// EnsureSession implements remoteagent.RemoteAgentServer.
func (a *Agent) EnsureSession(ctx context.Context, req *remoteagent.EnsureSessionRequest) (*remoteagent.EnsureSessionResponse, error) {
	if err := validateToken(req.Session); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	claim := a.claimByUID(req.ClaimUid)
	if claim == nil && req.ClaimNamespace != "" && req.ClaimName != "" {
		// Informer lag: fall back to a direct read and verify identity.
		c, err := a.cfg.ClientSets.Resource.ResourceClaims(req.ClaimNamespace).Get(ctx, req.ClaimName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.Unavailable, "get claim: %v", err)
		}
		if err == nil && string(c.UID) == req.ClaimUid {
			claim = c
			a.claimCache.Mutation(c)
		}
	}
	if claim == nil {
		return nil, status.Errorf(codes.NotFound, "claim %s not found", req.ClaimUid)
	}
	nd := a.nodeDevices.Load()
	if nd == nil || len(nd.Devices) == 0 {
		return nil, status.Error(codes.Unavailable, "node device snapshot not available yet")
	}
	if err := a.store.Materialize(req.Session, claim, nd, a.cfg.NodeName, a.cfg.DriverName, req.Requests); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	msg := ""
	if !a.serverUp.Load() {
		msg = "lupine-server is not accepting connections yet"
	}
	return &remoteagent.EnsureSessionResponse{Ready: true, CudaDriverVersion: nd.CudaVersionString(), Message: msg}, nil
}

// ServerInfo implements remoteagent.RemoteAgentServer.
func (a *Agent) ServerInfo(context.Context, *remoteagent.ServerInfoRequest) (*remoteagent.ServerInfoResponse, error) {
	resp := &remoteagent.ServerInfoResponse{
		Listening: a.serverUp.Load(),
		Endpoint:  a.cfg.Endpoint,
		NodeName:  a.cfg.NodeName,
	}
	if nd := a.nodeDevices.Load(); nd != nil {
		resp.CudaDriverVersion = nd.CudaVersionString()
	}
	return resp, nil
}
