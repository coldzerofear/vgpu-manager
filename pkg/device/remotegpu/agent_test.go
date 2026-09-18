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

package remotegpu

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseServerEndpoint(t *testing.T) {
	for raw, want := range map[string]string{
		":14833":                  "http://:14833",
		"10.0.0.7":                "http://10.0.0.7:14833",
		"http://10.0.0.7:15000/p": "http://10.0.0.7:15000/p",
		"https://gw.corp/pool-a":  "https://gw.corp:443/pool-a",
		"https://gw.corp:8443":    "https://gw.corp:8443",
		"[2001:db8::7]":           "http://[2001:db8::7]:14833",
		"127.0.0.1:14833":         "http://127.0.0.1:14833",
	} {
		e, err := ParseServerEndpoint(raw)
		if err != nil || e.String() != want {
			t.Errorf("ParseServerEndpoint(%q) = %v, %v, want %q", raw, e, err, want)
		}
	}
	for _, bad := range []string{"", "grpc://x", "unix:///run/x.sock", "http://x/p?q=1", "ftp://x"} {
		if e, err := ParseServerEndpoint(bad); err == nil {
			t.Errorf("ParseServerEndpoint(%q) = %v, want an error", bad, e)
		}
	}
}

func TestParseAgentEndpoint(t *testing.T) {
	for raw, want := range map[string]string{
		":14834":                          "grpc://:14834",
		"10.0.0.7":                        "grpc://10.0.0.7:14834",
		"grpc://10.0.0.7:15000":           "grpc://10.0.0.7:15000",
		"unix:///etc/vgpu-manager/a.sock": "unix:///etc/vgpu-manager/a.sock",
		"http://gpu-a/pool":               "grpc://gpu-a:14834/pool", // older publishers
		"https://gpu-a.example.com":       "grpc://gpu-a.example.com:14834",
		"0.0.0.0:0":                       "grpc://0.0.0.0:0", // listen on any free port
	} {
		e, err := ParseAgentEndpoint(raw)
		if err != nil || e.String() != want {
			t.Errorf("ParseAgentEndpoint(%q) = %v, %v, want %q", raw, e, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "unix://relative.sock", "grpc://x:70000", "tcp://x"} {
		if e, err := ParseAgentEndpoint(bad); err == nil {
			t.Errorf("ParseAgentEndpoint(%q) = %v, want an error", bad, e)
		}
	}
}

func TestAgentDialTarget(t *testing.T) {
	cases := map[string]string{
		// No port: the default agent port fills in.
		"10.0.0.7":                  "10.0.0.7:14834",
		"https://gpu-a.example.com": "gpu-a.example.com:14834",
		// Explicit port wins; scheme and path are stripped for the gRPC dial.
		"10.0.0.7:14834":              "10.0.0.7:14834",
		"http://gpu-a:15000/pool-a":   "gpu-a:15000",
		"gpu-a.zone.vgpu.internal:19": "gpu-a.zone.vgpu.internal:19",
		// The published form, and an IPv6 host.
		"grpc://10.0.0.7:14834":  "10.0.0.7:14834",
		"grpc://[2001:db8::7]":   "[2001:db8::7]:14834",
		"[2001:db8::7]:15000":    "[2001:db8::7]:15000",
		"grpc://gpu-a:14834/api": "gpu-a:14834",
		// A same-node socket is handed to grpc-go as its unix:// target.
		"unix:///etc/vgpu-manager/agent.sock": "unix:///etc/vgpu-manager/agent.sock",
	}
	for in, want := range cases {
		got, err := agentDialTarget(in)
		if err != nil || got != want {
			t.Errorf("agentDialTarget(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"ftp://x", "", ":14834", "grpc://", "unix://relative.sock", "grpc://gpu-a:0", "grpc://gpu-a:70000"} {
		if got, err := agentDialTarget(in); err == nil {
			t.Errorf("agentDialTarget(%q) = %q, want an error", in, got)
		}
	}
}

func TestPublishableEndpoints(t *testing.T) {
	s, a, err := PublishableEndpoints("10.0.0.7", "10.0.0.7")
	if err != nil || s != "http://10.0.0.7:14833" || a != "grpc://10.0.0.7:14834" {
		t.Fatalf("defaults: %q %q %v", s, a, err)
	}
	s, a, err = PublishableEndpoints("https://gw.corp/pool", "grpc://[2001:db8::7]:15000")
	if err != nil || s != "https://gw.corp:443/pool" || a != "grpc://[2001:db8::7]:15000" {
		t.Fatalf("explicit: %q %q %v", s, a, err)
	}
	for _, bad := range [][2]string{{"", "grpc://10.0.0.7:14834"}, {"http://127.0.0.1:14833", "grpc://10.0.0.7:14834"}, {"http://10.0.0.7", "unix:///run/agent.sock"}} {
		if _, _, err := PublishableEndpoints(bad[0], bad[1]); err == nil {
			t.Errorf("PublishableEndpoints(%q, %q) must be an error", bad[0], bad[1])
		}
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
	kubeClient := fake.NewClientset(node)

	// A caller outside hostNetwork: an empty host must become the node's
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
		got, err := ResolveAgentDial(ctx, kubeClient.CoreV1().Nodes(), "gpu-node", raw)
		if want == "" {
			if err == nil {
				t.Errorf("ResolveAgentDial(%q) = %q, want an error", raw, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("ResolveAgentDial(%q) = %q, %v, want %q", raw, got, err, want)
		}
	}

	// No InternalIP: the operator has to say where the agent is.
	bare := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node"}})
	if got, err := ResolveAgentDial(ctx, bare.CoreV1().Nodes(), "gpu-node", ":14834"); err == nil {
		t.Fatalf("node without InternalIP must be an error, got %q", got)
	}
	if got, err := ResolveAgentDial(ctx, bare.CoreV1().Nodes(), "gpu-node", "unix:///run/agent.sock"); err != nil || got != "unix:///run/agent.sock" {
		t.Fatalf("unix socket must not need the node: %q %v", got, err)
	}
}

// fakeAgent answers ServerInfo with whatever the test sets.
type fakeAgent struct {
	remoteagent.UnimplementedRemoteAgentServer
	mu                           sync.Mutex
	listening                    bool
	server, agent, version, etag string
}

func (f *fakeAgent) set(listening bool, server, agent, version, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listening, f.server, f.agent, f.version, f.etag = listening, server, agent, version, etag
}

func (f *fakeAgent) ServerInfo(context.Context, *remoteagent.ServerInfoRequest) (*remoteagent.ServerInfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &remoteagent.ServerInfoResponse{
		Listening: f.listening, Endpoint: f.server, AgentEndpoint: f.agent,
		CudaDriverVersion: f.version, ClientBundleEtag: f.etag, NodeName: "gpu-node",
	}, nil
}

func TestProbeServer(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{}
	srv := grpc.NewServer()
	remoteagent.RegisterRemoteAgentServer(srv, agent)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	agentDial := "grpc://" + lis.Addr().String()
	ctx := context.Background()

	agent.set(true, "192.168.1.7", "grpc://192.168.1.7:14834", "13.3.73", `"sha256:abc"`)
	got, err := ProbeServer(ctx, agentDial)
	want := ServerEndpointInfo{
		ServerEndpoint: "http://192.168.1.7:14833", AgentEndpoint: "grpc://192.168.1.7:14834",
		ServerCUDAVersion: "13.3.73", BundleETag: `"sha256:abc"`,
	}
	if err != nil || *got != want {
		t.Fatalf("ProbeServer = %+v, %v, want %+v", got, err, want)
	}

	agent.set(false, "192.168.1.7", "grpc://192.168.1.7:14834", "", "")
	if _, err := ProbeServer(ctx, agentDial); !errors.Is(err, ErrServerNotListening) {
		t.Fatalf("a server that is not listening must be ErrServerNotListening, got %v", err)
	}

	// Without its own routable address the agent's dial address stands in,
	// and a loopback one must not be published.
	agent.set(true, "192.168.1.7", "", "", "")
	if got, err := ProbeServer(ctx, agentDial); err == nil {
		t.Fatalf("a loopback agent address must not be publishable, got %+v", got)
	}

	if _, err := ProbeServer(ctx, "grpc://127.0.0.1:1"); err == nil {
		t.Fatal("an unreachable agent must be an error")
	}
}
