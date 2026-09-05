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

package kubeletplugin

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	"google.golang.org/grpc"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
)

// fakeAgent is a remote-agent that answers ServerInfo with whatever the test
// sets: the publisher only ever talks to the agent, never to lupine-server.
type fakeAgent struct {
	remoteagent.UnimplementedRemoteAgentServer
	mu        sync.Mutex
	listening bool
	version   string
	endpoint  string
}

func (f *fakeAgent) set(listening bool, version, endpoint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listening, f.version, f.endpoint = listening, version, endpoint
}

func (f *fakeAgent) ServerInfo(context.Context, *remoteagent.ServerInfoRequest) (*remoteagent.ServerInfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &remoteagent.ServerInfoResponse{Listening: f.listening, CudaDriverVersion: f.version, Endpoint: f.endpoint, NodeName: "gpu-node"}, nil
}

// startFakeAgent serves fakeAgent on a loopback TCP port and returns the
// grpc:// endpoint to dial it at.
func startFakeAgent(t *testing.T) (*fakeAgent, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeAgent{}
	fa.set(true, "13.3.73", "")
	srv := grpc.NewServer()
	remoteagent.RegisterRemoteAgentServer(srv, fa)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return fa, "grpc://" + lis.Addr().String()
}

func publishedAttr(p resourceslice.Pool, name resourceapi.QualifiedName) string {
	attr, ok := p.Slices[0].Devices[0].Attributes[name]
	if !ok {
		return ""
	}
	if attr.VersionValue != nil {
		return *attr.VersionValue
	}
	if attr.StringValue != nil {
		return *attr.StringValue
	}
	return ""
}

func newTestPublisher(t *testing.T, agentDial, serverFlag, agentFlag string) *remotePublisher {
	t.Helper()
	serverEndpoint, err := endpointutil.ParseEndpoint(serverFlag,
		endpointutil.WithDefaultScheme(endpointutil.Http), endpointutil.WithDefaultPort(remote.DefaultServerPort))
	if err != nil {
		t.Fatal(err)
	}
	agentEndpoint, err := endpointutil.ParseEndpoint(agentFlag,
		endpointutil.WithDefaultScheme(endpointutil.Grpc), endpointutil.WithDefaultPort(remote.DefaultAgentPort))
	if err != nil {
		t.Fatal(err)
	}
	rp := &remotePublisher{
		nodeName:       "gpu-node",
		agentDial:      agentDial,
		serverEndpoint: *serverEndpoint,
		agentEndpoint:  *agentEndpoint,
		nodeIP:         "10.0.0.7",
		spec:           &remote.PublishSpec{},
	}
	rp.spec.Endpoint, rp.spec.AgentEndpoint = rp.publishedEndpoints("")
	return rp
}

func TestRemotePublisherServerInfo(t *testing.T) {
	ctx := context.Background()
	fa, agentDial := startFakeAgent(t)
	rp := newTestPublisher(t, agentDial, ":14833", ":14834")
	pool := func() resourceslice.Pool {
		return rp.apply(resourceslice.Pool{Slices: []resourceslice.Slice{{
			Devices: []resourceapi.Device{{Name: "vgpu-0"}},
		}}})
	}

	// Before the agent answered: node IP everywhere, no server version.
	if got := publishedAttr(pool(), remote.AttrServerCUDAVersion); got != "" {
		t.Fatalf("nothing learned yet, but serverCudaVersion=%q was published", got)
	}
	if got := publishedAttr(pool(), remote.AttrServerEndpoint); got != "http://10.0.0.7:14833" {
		t.Fatalf("initial serverEndpoint = %q", got)
	}
	if got := publishedAttr(pool(), remote.AttrAgentEndpoint); got != "grpc://10.0.0.7:14834" {
		t.Fatalf("initial agentEndpoint = %q", got)
	}

	changed, err := rp.refreshServerInfo(ctx)
	if err != nil || !changed {
		t.Fatalf("first answer must count as a change: changed=%v err=%v", changed, err)
	}
	if got := publishedAttr(pool(), remote.AttrServerCUDAVersion); got != "13.3.73" {
		t.Fatalf("published serverCudaVersion=%q, want 13.3.73", got)
	}

	changed, err = rp.refreshServerInfo(ctx)
	if err != nil || changed {
		t.Fatalf("same answer must not count as a change: changed=%v err=%v", changed, err)
	}

	// The server comes back built from another image.
	fa.set(true, "12.9.1", "")
	changed, err = rp.refreshServerInfo(ctx)
	if err != nil || !changed {
		t.Fatalf("new build must count as a change: changed=%v err=%v", changed, err)
	}
	if got := publishedAttr(pool(), remote.AttrServerCUDAVersion); got != "12.9.1" {
		t.Fatalf("published serverCudaVersion=%q, want 12.9.1", got)
	}

	// The agent discovers the routable address: both endpoints follow it,
	// the agent keeping its own port.
	fa.set(true, "12.9.1", "http://192.168.1.7:14833")
	changed, err = rp.refreshServerInfo(ctx)
	if err != nil || !changed {
		t.Fatalf("new endpoint must count as a change: changed=%v err=%v", changed, err)
	}
	if got := publishedAttr(pool(), remote.AttrServerEndpoint); got != "http://192.168.1.7:14833" {
		t.Fatalf("published serverEndpoint=%q", got)
	}
	if got := publishedAttr(pool(), remote.AttrAgentEndpoint); got != "grpc://192.168.1.7:14834" {
		t.Fatalf("published agentEndpoint=%q", got)
	}

	// A server that stopped answering keeps the last known values.
	fa.set(false, "12.9.1", "http://192.168.1.7:14833")
	if changed, err = rp.refreshServerInfo(ctx); !errors.Is(err, remote.ErrServerNotListening) || changed {
		t.Fatalf("silent server must be ErrServerNotListening without a change: changed=%v err=%v", changed, err)
	}
	if got := publishedAttr(pool(), remote.AttrServerCUDAVersion); got != "12.9.1" {
		t.Fatalf("last known version must survive a failed probe, got %q", got)
	}
	if got := publishedAttr(pool(), remote.AttrServerEndpoint); got != "http://192.168.1.7:14833" {
		t.Fatalf("last known endpoint must survive a failed probe, got %q", got)
	}

	// An unparseable version is an error without a change.
	fa.set(true, "not-a-version", "http://192.168.1.7:14833")
	if changed, err = rp.refreshServerInfo(ctx); err == nil || changed {
		t.Fatalf("bad version must be an error without a change: changed=%v err=%v", changed, err)
	}

	// A loopback reported by a misconfigured agent is never published.
	fa.set(true, "12.9.1", "http://127.0.0.1:14833")
	if _, err = rp.refreshServerInfo(ctx); err != nil {
		t.Fatal(err)
	}
	if got := publishedAttr(pool(), remote.AttrServerEndpoint); got != "http://10.0.0.7:14833" {
		t.Fatalf("loopback from the agent must fall back to the node IP, got %q", got)
	}
}

func TestRemotePublisherPinnedHosts(t *testing.T) {
	ctx := context.Background()
	fa, agentDial := startFakeAgent(t)
	fa.set(true, "13.3.73", "http://192.168.1.7:14833")

	t.Run("pinned server host wins, agent follows it", func(t *testing.T) {
		rp := newTestPublisher(t, agentDial, "https://gpu-a.corp/pool-a", ":14834")
		if _, err := rp.refreshServerInfo(ctx); err != nil {
			t.Fatal(err)
		}
		spec := rp.currentSpec()
		if spec.Endpoint != "https://gpu-a.corp:14833/pool-a" || spec.AgentEndpoint != "grpc://gpu-a.corp:14834" {
			t.Fatalf("spec = %+v", spec)
		}
	})
	t.Run("pinned agent host wins independently", func(t *testing.T) {
		rp := newTestPublisher(t, agentDial, ":14833", "grpc://agent.corp:15000")
		if _, err := rp.refreshServerInfo(ctx); err != nil {
			t.Fatal(err)
		}
		spec := rp.currentSpec()
		if spec.Endpoint != "http://192.168.1.7:14833" || spec.AgentEndpoint != "grpc://agent.corp:15000" {
			t.Fatalf("spec = %+v", spec)
		}
	})
}

func TestRemotePublisherAgentDown(t *testing.T) {
	rp := newTestPublisher(t, "grpc://127.0.0.1:1", ":14833", ":14834")
	if changed, err := rp.refreshServerInfo(context.Background()); err == nil || changed {
		t.Fatalf("unreachable agent must be an error without a change: changed=%v err=%v", changed, err)
	}
	if spec := rp.currentSpec(); spec.Endpoint != "http://10.0.0.7:14833" || spec.ServerCUDAVersion != nil {
		t.Fatalf("spec must be untouched: %+v", spec)
	}
}
