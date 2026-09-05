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
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

// fakeAgent is a remote-agent that answers ServerInfo with whatever the test
// sets: the publisher only ever talks to the agent, never to lupine-server.
type fakeAgent struct {
	remoteagent.UnimplementedRemoteAgentServer
	mu        sync.Mutex
	listening bool
	version   string
	server    string
	agent     string
}

func (f *fakeAgent) set(listening bool, version, server, agent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listening, f.version, f.server, f.agent = listening, version, server, agent
}

func (f *fakeAgent) ServerInfo(context.Context, *remoteagent.ServerInfoRequest) (*remoteagent.ServerInfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &remoteagent.ServerInfoResponse{
		Listening: f.listening, CudaDriverVersion: f.version, Endpoint: f.server, AgentEndpoint: f.agent, NodeName: "gpu-node",
	}, nil
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
	fa.set(true, "13.3.73", "http://192.168.1.7:14833", "grpc://192.168.1.7:14834")
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

func publishedTaint(p resourceslice.Pool, key string) bool {
	for _, taint := range p.Slices[0].Devices[0].Taints {
		if taint.Key == key {
			return true
		}
	}
	return false
}

func newTestPublisher(agentDial string) *remotePublisher {
	return &remotePublisher{nodeName: "gpu-node", agentDial: agentDial, spec: &remote.PublishSpec{}}
}

func TestRemotePublisherServerInfo(t *testing.T) {
	ctx := context.Background()
	fa, agentDial := startFakeAgent(t)
	rp := newTestPublisher(agentDial)
	pool := func() resourceslice.Pool {
		return rp.apply(resourceslice.Pool{Slices: []resourceslice.Slice{{
			Devices: []resourceapi.Device{{Name: "vgpu-0"}},
		}}})
	}
	attrs := func() (server, agent, version string) {
		p := pool()
		return publishedAttr(p, remote.AttrServerEndpoint), publishedAttr(p, remote.AttrAgentEndpoint), publishedAttr(p, remote.AttrServerCUDAVersion)
	}

	// Before the agent answered: remote, tainted, no endpoints, no version.
	if s, a, v := attrs(); s != "" || a != "" || v != "" || !publishedTaint(pool(), remote.TaintKeyRemoteUnavailable) {
		t.Fatalf("nothing learned yet, but published server=%q agent=%q version=%q tainted=%v", s, a, v, publishedTaint(pool(), remote.TaintKeyRemoteUnavailable))
	}
	if got := publishedAttr(pool(), remote.AttrAccessMode); got != remote.AccessModeRemote {
		t.Fatalf("accessMode = %q", got)
	}

	changed, err := rp.refreshServerInfo(ctx)
	if err != nil || !changed {
		t.Fatalf("first answer must count as a change: changed=%v err=%v", changed, err)
	}
	if s, a, v := attrs(); s != "http://192.168.1.7:14833" || a != "grpc://192.168.1.7:14834" || v != "13.3.73" || publishedTaint(pool(), remote.TaintKeyRemoteUnavailable) {
		t.Fatalf("published server=%q agent=%q version=%q tainted=%v", s, a, v, publishedTaint(pool(), remote.TaintKeyRemoteUnavailable))
	}

	changed, err = rp.refreshServerInfo(ctx)
	if err != nil || changed {
		t.Fatalf("same answer must not count as a change: changed=%v err=%v", changed, err)
	}

	// The server comes back built from another image.
	fa.set(true, "12.9.1", "http://192.168.1.7:14833", "grpc://192.168.1.7:14834")
	changed, err = rp.refreshServerInfo(ctx)
	if err != nil || !changed {
		t.Fatalf("new build must count as a change: changed=%v err=%v", changed, err)
	}
	if _, _, v := attrs(); v != "12.9.1" {
		t.Fatalf("published serverCudaVersion=%q, want 12.9.1", v)
	}

	// The node moves to another address (or the operator advertises one).
	fa.set(true, "12.9.1", "https://gpu-a.corp:443/pool-a", "grpc://10.9.9.9:15000")
	changed, err = rp.refreshServerInfo(ctx)
	if err != nil || !changed {
		t.Fatalf("new endpoints must count as a change: changed=%v err=%v", changed, err)
	}
	if s, a, _ := attrs(); s != "https://gpu-a.corp:443/pool-a" || a != "grpc://10.9.9.9:15000" {
		t.Fatalf("published server=%q agent=%q", s, a)
	}

	// A server that stopped answering keeps the last known values.
	fa.set(false, "12.9.1", "https://gpu-a.corp:443/pool-a", "grpc://10.9.9.9:15000")
	if changed, err = rp.refreshServerInfo(ctx); !errors.Is(err, remote.ErrServerNotListening) || changed {
		t.Fatalf("silent server must be ErrServerNotListening without a change: changed=%v err=%v", changed, err)
	}
	if s, a, v := attrs(); s != "https://gpu-a.corp:443/pool-a" || a != "grpc://10.9.9.9:15000" || v != "12.9.1" {
		t.Fatalf("last known values must survive a failed probe: server=%q agent=%q version=%q", s, a, v)
	}

	// So does an agent that lost its routable address, or reports junk.
	for _, bad := range [][2]string{{"", ""}, {"http://127.0.0.1:14833", "grpc://127.0.0.1:14834"}, {"ftp://x", "grpc://10.9.9.9:15000"}, {"http://10.9.9.9", "unix:///run/agent.sock"}} {
		fa.set(true, "12.9.1", bad[0], bad[1])
		if changed, err = rp.refreshServerInfo(ctx); err == nil || changed {
			t.Fatalf("%v must be an error without a change: changed=%v err=%v", bad, changed, err)
		}
	}
	if s, a, _ := attrs(); s != "https://gpu-a.corp:443/pool-a" || a != "grpc://10.9.9.9:15000" {
		t.Fatalf("junk must not replace the last known endpoints: server=%q agent=%q", s, a)
	}

	// An unparseable version is an error without a change.
	fa.set(true, "not-a-version", "https://gpu-a.corp:443/pool-a", "grpc://10.9.9.9:15000")
	if changed, err = rp.refreshServerInfo(ctx); err == nil || changed {
		t.Fatalf("bad version must be an error without a change: changed=%v err=%v", changed, err)
	}
}

func TestRemotePublisherAgentDown(t *testing.T) {
	rp := newTestPublisher("grpc://127.0.0.1:1")
	if changed, err := rp.refreshServerInfo(context.Background()); err == nil || changed {
		t.Fatalf("unreachable agent must be an error without a change: changed=%v err=%v", changed, err)
	}
	if spec := rp.currentSpec(); spec.Reachable() || spec.ServerCUDAVersion != nil {
		t.Fatalf("spec must be untouched: %+v", spec)
	}
}

func TestPublishableEndpoints(t *testing.T) {
	s, a, err := publishableEndpoints("10.0.0.7", "10.0.0.7")
	if err != nil || s != "http://10.0.0.7:14833" || a != "grpc://10.0.0.7:14834" {
		t.Fatalf("defaults: %q %q %v", s, a, err)
	}
	s, a, err = publishableEndpoints("https://gw.corp/pool", "grpc://[2001:db8::7]:15000")
	if err != nil || s != "https://gw.corp:443/pool" || a != "grpc://[2001:db8::7]:15000" {
		t.Fatalf("explicit: %q %q %v", s, a, err)
	}
}

func TestResolveAgentDial(t *testing.T) {
	ctx := context.Background()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-node"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: "gpu-node"},
			{Type: corev1.NodeInternalIP, Address: "10.0.0.7"},
		}},
	}
	config := &Config{Flags: &Flags{NodeName: "gpu-node"}, ClientSets: pkgflags.ClientSets{Core: fake.NewSimpleClientset(node)}}

	// dra-server is not on hostNetwork: an empty host must become the node's
	// InternalIP, never a loopback.
	for raw, want := range map[string]string{
		":14834":                     "grpc://10.0.0.7:14834",
		"":                           "",
		"grpc://":                    "grpc://10.0.0.7:14834",
		"grpc://10.0.0.8:15000":      "grpc://10.0.0.8:15000",
		"unix:///run/agent.sock":     "unix:///run/agent.sock",
		"gpu-node.internal":          "grpc://gpu-node.internal:14834",
		"grpc://[2001:db8::7]:14834": "grpc://[2001:db8::7]:14834",
	} {
		got, err := resolveAgentDial(ctx, config, raw)
		if want == "" {
			if err == nil {
				t.Errorf("resolveAgentDial(%q) = %q, want an error", raw, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("resolveAgentDial(%q) = %q, %v, want %q", raw, got, err, want)
		}
	}

	// No InternalIP: the operator has to say where the agent is.
	bare := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node"}}
	config.Core = fake.NewSimpleClientset(bare)
	if got, err := resolveAgentDial(ctx, config, ":14834"); err == nil {
		t.Fatalf("node without InternalIP must be an error, got %q", got)
	}
	if got, err := resolveAgentDial(ctx, config, "unix:///run/agent.sock"); err != nil || got != "unix:///run/agent.sock" {
		t.Fatalf("unix socket must not need the node: %q %v", got, err)
	}
}
