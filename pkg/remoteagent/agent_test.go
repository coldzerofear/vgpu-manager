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

package remoteagent

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/klog/v2"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

// fakeLupine answers like lupine-server does on its RPC port: 404 with the
// CUDA version header, unless told to go silent (no header).
func fakeLupine(t *testing.T) (*httptest.Server, *atomic.Value) {
	srv, version, _ := fakeLupineWithBundle(t)
	return srv, version
}

// fakeBundle is what fakeLupineWithBundle serves at the client bundle path
// for this platform: nil body = no bundle (404).
type fakeBundle struct {
	body []byte
	etag string
}

func fakeLupineWithBundle(t *testing.T) (*httptest.Server, *atomic.Value, *atomic.Pointer[fakeBundle]) {
	t.Helper()
	var version atomic.Value
	version.Store("13.3.73")
	var bundle atomic.Pointer[fakeBundle]
	bundlePath := remote.ClientBundlePathPrefix + remote.LocalClientBundlePlatform()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := version.Load().(string); v != "" {
			w.Header().Set(remote.ServerCUDAVersionHeader, v)
		}
		if b := bundle.Load(); b != nil && r.URL.Path == bundlePath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.Header().Set("Etag", b.etag)
			w.Header().Set("Content-Type", remote.ClientBundleContentType)
			if r.Header.Get("If-None-Match") == b.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(b.body)))
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(b.body)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &version, &bundle
}

func TestProbeAndServerInfo(t *testing.T) {
	ctx := context.Background()
	srv, version := fakeLupine(t)

	t.Run("up/down/rebuild: reachability flips, the rest is kept", func(t *testing.T) {
		a := New(Config{NodeName: "gpu-node", ServerEndpoint: srv.URL, SessionBase: t.TempDir()})
		info, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
		if info.Listening || info.CudaDriverVersion != "" || info.Endpoint != "" {
			t.Fatalf("before any probe: %+v", info)
		}
		a.probeServer(ctx)
		info, _ = a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
		if !info.Listening || info.CudaDriverVersion != "13.3.73" || info.NodeName != "gpu-node" {
			t.Fatalf("after probe: %+v", info)
		}
		version.Store("")
		a.probeServer(ctx)
		info, _ = a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
		if info.Listening || info.CudaDriverVersion != "13.3.73" {
			t.Fatalf("after silent probe: %+v", info)
		}
		version.Store("12.9.1")
		a.probeServer(ctx)
		info, _ = a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
		if !info.Listening || info.CudaDriverVersion != "12.9.1" {
			t.Fatalf("after rebuild: %+v", info)
		}
	})

	t.Run("routable probe host is advertised as is", func(t *testing.T) {
		ifaces, _ := hostIfaceAddrs()
		candidates := orderCandidates(nil, ifaces)
		if len(candidates) == 0 {
			t.Skip("no routable address on this host")
		}
		lis, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Skip(err)
		}
		hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(remote.ServerCUDAVersionHeader, "13.3.73")
			w.WriteHeader(http.StatusNotFound)
		})}
		go func() { _ = hs.Serve(lis) }()
		t.Cleanup(func() { _ = hs.Close() })
		endpoint := "http://" + net.JoinHostPort(candidates[0], strconv.Itoa(lis.Addr().(*net.TCPAddr).Port))

		a := New(Config{NodeName: "gpu-node", ServerEndpoint: endpoint, SessionBase: t.TempDir()})
		a.agentTCP.Store(&endpointutil.Endpoint{Scheme: endpointutil.Grpc, Port: "14834"}) // as bound on 0.0.0.0
		a.probeServer(ctx)
		info, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
		if !info.Listening || info.Endpoint != endpoint || info.AgentEndpoint != "grpc://"+net.JoinHostPort(candidates[0], "14834") {
			t.Fatalf("routable host must be reported verbatim, agent on the same host: %+v", info)
		}
		// A TCP listener bound to a specific address advertises that address.
		a.agentTCP.Store(&endpointutil.Endpoint{Scheme: endpointutil.Grpc, Host: "10.9.9.9", Port: "15000"})
		a.probeServer(ctx)
		if info, _ = a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{}); info.AgentEndpoint != "grpc://10.9.9.9:15000" {
			t.Fatalf("bound address must win: %+v", info)
		}
	})

	t.Run("advertise endpoint is reported verbatim; agent endpoint still needs a routable host", func(t *testing.T) {
		a := New(Config{NodeName: "gpu-node", ServerEndpoint: srv.URL, AdvertiseEndpoint: "https://gpu-a.corp:443/pool-a", SessionBase: t.TempDir()})
		a.agentTCP.Store(&endpointutil.Endpoint{Scheme: endpointutil.Grpc, Port: "14834"})
		a.probeServer(ctx)
		info, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
		// srv.URL is a loopback and the fake answers only there, so no
		// routable host: the server endpoint is advertised anyway, the
		// agent's own stays unknown.
		if !info.Listening || info.Endpoint != "https://gpu-a.corp:443/pool-a" || info.AgentEndpoint != "" {
			t.Fatalf("%+v", info)
		}
	})

	t.Run("no TCP listener: no agent endpoint", func(t *testing.T) {
		a := New(Config{NodeName: "gpu-node", ServerEndpoint: srv.URL, SessionBase: t.TempDir()})
		a.probeServer(ctx)
		if info, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{}); info.AgentEndpoint != "" {
			t.Fatalf("%+v", info)
		}
	})

	t.Run("loopback probe host: nothing routable answers -> empty endpoint, still listening", func(t *testing.T) {
		// srv.URL is http://127.0.0.1:port, a loopback, so discovery runs; the
		// fake listens on 127.0.0.1 only, so no interface address answers and
		// the agent must report no endpoint rather than the loopback.
		a := New(Config{NodeName: "gpu-node", ServerEndpoint: srv.URL, SessionBase: t.TempDir()})
		a.probeServer(ctx)
		if info, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{}); !info.Listening || info.Endpoint != "" || info.CudaDriverVersion != version.Load().(string) {
			t.Fatalf("got %+v", info)
		}
	})

	t.Run("loopback probe host with a server on all interfaces", func(t *testing.T) {
		// Bind on the wildcard address so the discovered host address answers.
		lis, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Skip(err)
		}
		hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(remote.ServerCUDAVersionHeader, "13.3.73")
			w.WriteHeader(http.StatusNotFound)
		})}
		go func() { _ = hs.Serve(lis) }()
		t.Cleanup(func() { _ = hs.Close() })
		port := lis.Addr().(*net.TCPAddr).Port

		a := New(Config{NodeName: "gpu-node", ServerEndpoint: "http://127.0.0.1:" + strconv.Itoa(port), SessionBase: t.TempDir()})
		a.probeServer(ctx)
		info, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
		if !info.Listening {
			t.Fatalf("%+v", info)
		}
		ifaces, _ := hostIfaceAddrs()
		if len(orderCandidates(nil, ifaces)) == 0 {
			// A host with no routable address (CI sandbox) cannot discover
			// anything; the contract is then an empty endpoint.
			if info.Endpoint != "" {
				t.Fatalf("no candidates, but endpoint %q reported", info.Endpoint)
			}
			return
		}
		if info.Endpoint == "" || info.Endpoint == a.cfg.ServerEndpoint {
			t.Fatalf("discovery must replace the loopback: %+v", info)
		}
		// Sticky: a second probe keeps the same answer.
		a.probeServer(ctx)
		if again, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{}); again.Endpoint != info.Endpoint {
			t.Fatalf("endpoint flapped: %q -> %q", info.Endpoint, again.Endpoint)
		}
	})
}

func TestHealthCheck(t *testing.T) {
	ctx := context.Background()
	srv, version := fakeLupine(t)
	a := New(Config{NodeName: "gpu-node", ServerEndpoint: srv.URL, SessionBase: t.TempDir()})

	check := func(service string) grpc_health_v1.HealthCheckResponse_ServingStatus {
		t.Helper()
		resp, err := a.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: service})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Status
	}
	// Before Run wires hasReady nothing is serving, and Check must not panic.
	for _, s := range []string{"", "liveness", "readiness"} {
		if got := check(s); got != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
			t.Fatalf("%q before sync: %v", s, got)
		}
	}
	if _, err := a.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "bogus"}); err == nil {
		t.Fatal("unknown service must be an error")
	}
	a.hasReady = func() bool { return true }
	if check("liveness") != grpc_health_v1.HealthCheckResponse_SERVING || check("readiness") != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatal("liveness must not depend on the server; readiness must")
	}
	a.probeServer(ctx)
	if check("readiness") != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatal("readiness after a good probe")
	}
	version.Store("")
	a.probeServer(ctx)
	if check("readiness") != grpc_health_v1.HealthCheckResponse_NOT_SERVING || check("liveness") != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatal("server down flips readiness only")
	}
}

func TestListen(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sub", "agent.sock")

	t.Run("tcp and unix together", func(t *testing.T) {
		a := New(Config{ListenEndpoints: []string{"grpc://127.0.0.1:0", "unix://" + sock}})
		listeners, err := a.listen()
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			for _, l := range listeners {
				_ = l.Close()
			}
		}()
		if len(listeners) != 2 || listeners[0].Addr().Network() != "tcp" || listeners[1].Addr().Network() != "unix" {
			t.Fatalf("listeners: %v", listeners)
		}
		fi, err := os.Stat(sock)
		if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o660 {
			t.Fatalf("socket %s: %v %v", sock, fi, err)
		}
		// A second agent on the same live socket is refused.
		if _, err := New(Config{ListenEndpoints: []string{"unix://" + sock}}).listen(); err == nil {
			t.Fatal("live socket must be refused")
		}
	})

	t.Run("stale socket is replaced", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		_ = l.Close() // Go removes the file on Close; recreate a dead one.
		dead, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		dead.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = dead.Close()
		if _, err := os.Lstat(sock); err != nil {
			t.Fatalf("stale socket should remain on disk: %v", err)
		}
		listeners, err := New(Config{ListenEndpoints: []string{"unix://" + sock}}).listen()
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range listeners {
			_ = l.Close()
		}
	})

	t.Run("not a socket, bad scheme, empty list", func(t *testing.T) {
		regular := filepath.Join(dir, "file")
		if err := os.WriteFile(regular, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, eps := range [][]string{{"unix://" + regular}, {"ftp://127.0.0.1:0"}, {}, {"unix://relative.sock"}} {
			if listeners, err := New(Config{ListenEndpoints: eps}).listen(); err == nil {
				for _, l := range listeners {
					_ = l.Close()
				}
				t.Errorf("%v must be refused", eps)
			}
		}
	})
}

func TestProbeKeepsEndpointsWhileDown(t *testing.T) {
	ctx := context.Background()
	srv, version := fakeLupine(t)
	a := New(Config{NodeName: "gpu-node", ServerEndpoint: srv.URL, AdvertiseEndpoint: "https://gpu-a.corp:443/pool-a", SessionBase: t.TempDir()})
	// Pretend discovery found a host and the agent is bound to it.
	a.agentTCP.Store(&endpointutil.Endpoint{Scheme: endpointutil.Grpc, Host: "10.9.9.9", Port: "15000"})
	a.probeServer(ctx)
	up, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
	if !up.Listening || up.AgentEndpoint != "grpc://10.9.9.9:15000" {
		t.Fatalf("%+v", up)
	}
	version.Store("")
	a.probeServer(ctx)
	down, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
	if down.Listening || down.Endpoint != up.Endpoint || down.AgentEndpoint != up.AgentEndpoint || down.CudaDriverVersion != up.CudaDriverVersion {
		t.Fatalf("a failed probe must flip only listening: up=%+v down=%+v", up, down)
	}
}

func TestAgentEndpointFor(t *testing.T) {
	a := New(Config{ServerEndpoint: "http://127.0.0.1:14833"})
	cases := []struct {
		bound *endpointutil.Endpoint
		host  string
		want  string
	}{
		{nil, "10.0.0.7", ""}, // no TCP listener
		{&endpointutil.Endpoint{Scheme: endpointutil.Grpc, Port: "14834"}, "", ""}, // wildcard, host unknown
		{&endpointutil.Endpoint{Scheme: endpointutil.Grpc, Port: "14834"}, "10.0.0.7", "grpc://10.0.0.7:14834"},
		{&endpointutil.Endpoint{Scheme: endpointutil.Grpc, Host: "0.0.0.0", Port: "14834"}, "10.0.0.7", "grpc://10.0.0.7:14834"},
		{&endpointutil.Endpoint{Scheme: endpointutil.Grpc, Host: "10.9.9.9", Port: "15000"}, "10.0.0.7", "grpc://10.9.9.9:15000"},
		{&endpointutil.Endpoint{Scheme: endpointutil.Grpc, Host: "127.0.0.1", Port: "14834"}, "10.0.0.7", ""}, // loopback-only listener
	}
	for _, c := range cases {
		a.agentTCP.Store(c.bound)
		if got := a.agentEndpointFor(c.host); got != c.want {
			t.Errorf("agentEndpointFor(bound=%v, host=%q) = %q, want %q", c.bound, c.host, got, c.want)
		}
	}
}

func TestNewRejectsBadServerEndpoint(t *testing.T) {
	a := New(Config{ServerEndpoint: "ftp://x"})
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("Run must refuse an unparseable server endpoint")
	}
}

func TestSweepClaimAndRelease(t *testing.T) {
	ctx := context.Background()
	a := New(Config{NodeName: testNode, DriverName: testDriver, ServerEndpoint: "http://127.0.0.1:14833", SessionBase: t.TempDir()})
	if err := a.store.Prepare(); err != nil {
		t.Fatal(err)
	}
	nd := NodeRemoteDevicesFromSlices([]*resourceapi.ResourceSlice{testSlice()})
	claimAt := func(rv string, tokens ...string) *resourceapi.ResourceClaim {
		c := testClaim("uid-x", result(testNode, "vgpu-0", "", ""))
		c.ResourceVersion = rv
		for i, tok := range tokens {
			metav1.SetMetaDataAnnotation(&c.ObjectMeta, remote.SessionAnnotationKey("p"+strconv.Itoa(i)), tok)
		}
		metav1.SetMetaDataAnnotation(&c.ObjectMeta, remote.AllocationAnnotation, remote.AllocationID(c))
		return c
	}
	for _, tok := range []string{"t1", "t2"} {
		if err := a.store.Materialize(tok, claimAt("10", "t1", "t2"), nd, nil); err != nil {
			t.Fatal(err)
		}
	}

	// liveSessions: allocated + not deleting => the annotated tokens; else nothing.
	if got := liveSessions(claimAt("10", "t1", "t2")); !got.Equal(sets.New("t1", "t2")) {
		t.Fatalf("liveSessions = %v", got)
	}
	unalloc := claimAt("11", "t1")
	unalloc.Status.Allocation = nil
	deleting := claimAt("11", "t1")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	if liveSessions(unalloc).Len() != 0 || liveSessions(deleting).Len() != 0 {
		t.Fatal("unallocated or deleting claims have no live sessions")
	}
	// Tokens issued for another allocation of the same claim are not live.
	moved := claimAt("11", "t1")
	moved.Status.Allocation.Devices.Results[0].Device = "vgpu-2"
	if liveSessions(moved).Len() != 0 {
		t.Fatal("tokens of a previous allocation must not be live")
	}

	// An update that drops t2 from the annotations sweeps t2 only.
	a.sweepClaim(claimAt("12", "t1"))
	if got := a.store.TokensOfClaim("uid-x"); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("after update: %v", got)
	}
	// A stale deallocation event (older than the session) is ignored ...
	a.sweepClaim(func() *resourceapi.ResourceClaim { c := claimAt("9"); c.Status.Allocation = nil; return c }())
	if got := a.store.TokensOfClaim("uid-x"); len(got) != 1 {
		t.Fatalf("stale event must not sweep: %v", got)
	}
	// ... a current one is not.
	a.sweepClaim(func() *resourceapi.ResourceClaim { c := claimAt("13"); c.Status.Allocation = nil; return c }())
	if got := a.store.TokensOfClaim("uid-x"); len(got) != 0 {
		t.Fatalf("current deallocation must sweep: %v", got)
	}

	// ReleaseSessions RPC: by token, claim-scoped, counts what it removed.
	for _, tok := range []string{"r1", "r2"} {
		if err := a.store.Materialize(tok, claimAt("20", "r1", "r2"), nd, nil); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := a.ReleaseSessions(ctx, &remoteagent.ReleaseSessionsRequest{ClaimUid: "uid-x", Tokens: []string{"r1", "zz"}})
	if err != nil || resp.Released != 1 {
		t.Fatalf("release r1: %+v %v", resp, err)
	}
	if resp, err = a.ReleaseSessions(ctx, &remoteagent.ReleaseSessionsRequest{ClaimUid: "uid-other", Tokens: []string{"r2"}}); err != nil || resp.Released != 0 {
		t.Fatalf("release for another claim must touch nothing: %+v %v", resp, err)
	}
	// A claim UID alone releases nothing: the tokens are the credential.
	if _, err = a.ReleaseSessions(ctx, &remoteagent.ReleaseSessionsRequest{ClaimUid: "uid-x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("release without tokens must be rejected, got %v", err)
	}
	if resp, err = a.ReleaseSessions(ctx, &remoteagent.ReleaseSessionsRequest{ClaimUid: "uid-x", Tokens: []string{"r2"}}); err != nil || resp.Released != 1 {
		t.Fatalf("release r2: %+v %v", resp, err)
	}
	if _, err = a.ReleaseSessions(ctx, &remoteagent.ReleaseSessionsRequest{}); err == nil {
		t.Fatal("empty claim uid must be rejected")
	}
	if _, err = a.ReleaseSessions(ctx, &remoteagent.ReleaseSessionsRequest{ClaimUid: "uid-x", Tokens: []string{"../x"}}); err == nil {
		t.Fatal("malformed token must be rejected")
	}
}

// EnsureSession builds a session only from a claim that records the token
// for its current allocation, and reads the claim from the API when the
// cache is behind the version the caller names.
func TestEnsureSessionRequiresRecordedToken(t *testing.T) {
	ctx := context.Background()
	srv, _ := fakeLupine(t)
	claimAt := func(rv string, tokens ...string) *resourceapi.ResourceClaim {
		c := testClaim("uid-e", result(testNode, "vgpu-0", "", ""))
		c.ResourceVersion = rv
		for i, tok := range tokens {
			metav1.SetMetaDataAnnotation(&c.ObjectMeta, remote.SessionAnnotationKey("p"+strconv.Itoa(i)), tok)
		}
		metav1.SetMetaDataAnnotation(&c.ObjectMeta, remote.AllocationAnnotation, remote.AllocationID(c))
		return c
	}
	apiClaim := claimAt("10", "t1")
	cs := fake.NewSimpleClientset(apiClaim)
	var gets atomic.Int32
	cs.PrependReactor("get", "resourceclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets.Add(1)
		return false, nil, nil
	})
	a := New(Config{
		NodeName: testNode, DriverName: testDriver, ServerEndpoint: srv.URL, SessionBase: t.TempDir(),
		ClientSets: pkgflags.ClientSets{Core: cs, Resource: draclient.New(cs)},
	})
	if err := a.store.Prepare(); err != nil {
		t.Fatal(err)
	}
	a.nodeDevices.Store(NodeRemoteDevicesFromSlices([]*resourceapi.ResourceSlice{testSlice()}))
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, claimIndexers())
	a.claimCache = cache.NewIntegerResourceVersionMutationCache(klog.Background(), indexer, indexer, time.Minute, true)
	if err := indexer.Add(claimAt("10", "t1")); err != nil {
		t.Fatal(err)
	}
	a.probeServer(ctx)

	ensure := func(token, rv string) (*remoteagent.EnsureSessionResponse, error) {
		return a.EnsureSession(ctx, &remoteagent.EnsureSessionRequest{
			Session: token, ClaimUid: string(apiClaim.UID), ClaimNamespace: apiClaim.Namespace, ClaimName: apiClaim.Name,
			Partition: "p", ClaimResourceVersion: rv,
		})
	}

	// Cache is current and records the token: no API read.
	resp, err := ensure("t1", "10")
	if err != nil || !resp.Ready {
		t.Fatalf("t1: %+v %v", resp, err)
	}
	if gets.Load() != 0 {
		t.Fatalf("a current cache must not be re-read from the API (%d gets)", gets.Load())
	}

	// A token the claim does not record is refused, even though the caller
	// names a version the cache already has.
	if _, err = ensure("t9", "10"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unrecorded token: want PermissionDenied, got %v", err)
	}
	if got := a.store.TokensOfClaim("uid-e"); len(got) != 1 {
		t.Fatal("refused session must not be materialized")
	}

	// The claim gained t2 at rv 11 and the cache has not seen it yet: the
	// agent reads the API, accepts, and the cache moves forward.
	apiClaim = claimAt("11", "t1", "t2")
	if _, err = cs.ResourceV1().ResourceClaims(apiClaim.Namespace).Update(ctx, apiClaim, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	before := gets.Load()
	if resp, err = ensure("t2", "11"); err != nil || !resp.Ready {
		t.Fatalf("t2 after API update: %+v %v", resp, err)
	}
	if gets.Load() != before+1 {
		t.Fatalf("a stale cache must be refreshed from the API once (%d gets)", gets.Load()-before)
	}
	if c, err := a.GetClaimByUID("uid-e"); err != nil || c.ResourceVersion != "11" {
		t.Fatalf("cache must hold the fresh claim: %v %v", c, err)
	}
	if got := a.store.TokensOfClaim("uid-e"); len(got) != 2 {
		t.Fatalf("sessions of claim: %v", got)
	}

	// Cache records the token but is older than the caller's version: the
	// caller knows better, so the API is consulted, and the answer stands.
	before = gets.Load()
	if _, err = ensure("t1", "12"); err != nil || gets.Load() != before+1 {
		t.Fatalf("older cache than the caller must be re-read: err=%v gets=%d", err, gets.Load()-before)
	}

	// Claim gone from the API (and from the cache): NotFound, nothing built.
	if err = cs.ResourceV1().ResourceClaims(apiClaim.Namespace).Delete(ctx, apiClaim.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = indexer.Delete(apiClaim); err != nil {
		t.Fatal(err)
	}
	if _, err = ensure("t3", "13"); status.Code(err) != codes.NotFound {
		t.Fatalf("gone claim: want NotFound, got %v", err)
	}
}

// captureStream stands in for the gRPC server stream of FetchClientBundle.
type captureStream struct {
	grpc.ServerStream
	ctx  context.Context
	msgs []*remoteagent.FetchClientBundleResponse
}

func (s *captureStream) Context() context.Context { return s.ctx }
func (s *captureStream) Send(m *remoteagent.FetchClientBundleResponse) error {
	// gRPC marshals on Send, so the agent may reuse its chunk buffer; a
	// capturing stream has to copy like the wire would.
	if chunk := m.GetChunk(); chunk != nil {
		m = &remoteagent.FetchClientBundleResponse{Body: &remoteagent.FetchClientBundleResponse_Chunk{Chunk: append([]byte(nil), chunk...)}}
	}
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *captureStream) body() []byte {
	var out []byte
	for _, m := range s.msgs[1:] {
		out = append(out, m.GetChunk()...)
	}
	return out
}

// The agent proxies the server's client bundle to a caller that holds a
// session token of a claim, and learns the bundle's etag on its probe.
func TestFetchClientBundle(t *testing.T) {
	ctx := context.Background()
	srv, version, bundle := fakeLupineWithBundle(t)
	body := bytes.Repeat([]byte("shim"), remote.ClientBundleChunkSize/2) // 2 chunks + change
	body = append(body, []byte("tail")...)
	bundle.Store(&fakeBundle{body: body, etag: `"sha256:abc"`})

	claim := testClaim("uid-b", result(testNode, "vgpu-0", "", ""))
	claim.ResourceVersion = "5"
	metav1.SetMetaDataAnnotation(&claim.ObjectMeta, remote.SessionAnnotationKey("p"), "tok-b")
	metav1.SetMetaDataAnnotation(&claim.ObjectMeta, remote.AllocationAnnotation, remote.AllocationID(claim))
	cs := fake.NewSimpleClientset(claim)
	a := New(Config{NodeName: testNode, DriverName: testDriver, ServerEndpoint: srv.URL, SessionBase: t.TempDir(),
		ClientSets: pkgflags.ClientSets{Core: cs, Resource: draclient.New(cs)}})
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, claimIndexers())
	a.claimCache = cache.NewIntegerResourceVersionMutationCache(klog.Background(), indexer, indexer, time.Minute, true)
	if err := indexer.Add(claim); err != nil {
		t.Fatal(err)
	}

	// The probe reads the etag; ServerInfo and EnsureSession answers carry it.
	a.probeServer(ctx)
	if info, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{}); info.ClientBundleEtag != `"sha256:abc"` {
		t.Fatalf("ServerInfo etag = %q", info.ClientBundleEtag)
	}
	// The bundle is re-read only across a restart (the binary cannot change
	// while the same server keeps answering).
	bundle.Store(nil)
	a.probeServer(ctx)
	if info, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{}); info.ClientBundleEtag != `"sha256:abc"` {
		t.Fatalf("etag must not be re-read while the server stays up, got %q", info.ClientBundleEtag)
	}
	version.Store("")
	a.probeServer(ctx) // down
	version.Store("13.3.73")
	a.probeServer(ctx) // back: re-read
	if info, _ := a.ServerInfo(ctx, &remoteagent.ServerInfoRequest{}); info.ClientBundleEtag != "" {
		t.Fatalf("a restarted server without a bundle must report no etag, got %q", info.ClientBundleEtag)
	}
	bundle.Store(&fakeBundle{body: body, etag: `"sha256:abc"`})
	version.Store("")
	a.probeServer(ctx)
	version.Store("13.3.73")
	a.probeServer(ctx)

	fetch := func(session, ifNoneMatch string) (*captureStream, error) {
		s := &captureStream{ctx: ctx}
		err := a.FetchClientBundle(&remoteagent.FetchClientBundleRequest{
			Session: session, ClaimUid: "uid-b", ClaimNamespace: claim.Namespace, ClaimName: claim.Name, IfNoneMatch: ifNoneMatch,
		}, s)
		return s, err
	}
	s, err := fetch("tok-b", "")
	if err != nil {
		t.Fatal(err)
	}
	info := s.msgs[0].GetInfo()
	if info == nil || info.Etag != `"sha256:abc"` || info.Size != int64(len(body)) || info.Platform != remote.LocalClientBundlePlatform() || info.NotModified {
		t.Fatalf("info = %+v", info)
	}
	if got := s.body(); !bytes.Equal(got, body) {
		t.Fatalf("body: %d bytes, want %d", len(got), len(body))
	}
	if len(s.msgs) != 4 {
		t.Fatalf("expected info + 3 chunks, got %d messages", len(s.msgs))
	}

	// Current etag: metadata only.
	if s, err = fetch("tok-b", `"sha256:abc"`); err != nil || len(s.msgs) != 1 || !s.msgs[0].GetInfo().NotModified {
		t.Fatalf("if-none-match: %v %+v", err, s.msgs)
	}
	// No credential, wrong credential, unsupported platform.
	if _, err = fetch("tok-zz", ""); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unrecorded token: want PermissionDenied, got %v", err)
	}
	if _, err = fetch("", ""); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty token: want InvalidArgument, got %v", err)
	}
	s = &captureStream{ctx: ctx}
	err = a.FetchClientBundle(&remoteagent.FetchClientBundleRequest{Session: "tok-b", ClaimUid: "uid-b", ClaimNamespace: claim.Namespace, ClaimName: claim.Name, Os: "plan9"}, s)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad platform: want InvalidArgument, got %v", err)
	}
	// Server without a bundle for the platform.
	bundle.Store(nil)
	if _, err = fetch("tok-b", ""); status.Code(err) != codes.NotFound {
		t.Fatalf("no bundle: want NotFound, got %v", err)
	}
}
