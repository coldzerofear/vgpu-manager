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
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// fakeLupine answers like lupine-server does on its RPC port: 404 with the
// CUDA version header, unless told to go silent (no header).
func fakeLupine(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var version atomic.Value
	version.Store("13.3.73")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := version.Load().(string); v != "" {
			w.Header().Set(remote.ServerCUDAVersionHeader, v)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &version
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
