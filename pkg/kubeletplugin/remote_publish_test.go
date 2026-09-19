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
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
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

	// A server that stopped answering must promptly go unreachable: the
	// scheduler must not keep sending pods at a device whose probe just
	// failed, so this is a change (taint back on, endpoint/version
	// attributes gone) even though the agent itself answered fine.
	fa.set(false, "12.9.1", "https://gpu-a.corp:443/pool-a", "grpc://10.9.9.9:15000")
	if changed, err = rp.refreshServerInfo(ctx); !errors.Is(err, remotegpu.ErrServerNotListening) || !changed {
		t.Fatalf("silent server must be ErrServerNotListening and un-publish now: changed=%v err=%v", changed, err)
	}
	if s, a, v := attrs(); s != "" || a != "" || v != "" || !publishedTaint(pool(), remote.TaintKeyRemoteUnavailable) {
		t.Fatalf("a failed probe must clear the endpoints and re-taint: server=%q agent=%q version=%q tainted=%v",
			s, a, v, publishedTaint(pool(), remote.TaintKeyRemoteUnavailable))
	}

	// An already-unreachable device does not need repeated republishing:
	// once the taint is on, further failures (agent lost its routable
	// address, reports junk, or a unix-scheme agent endpoint -- which must
	// never be accepted as publishable, unix works only for this node's own
	// dial) are still errors, but no longer a change.
	for _, bad := range [][2]string{{"", ""}, {"http://127.0.0.1:14833", "grpc://127.0.0.1:14834"}, {"ftp://x", "grpc://10.9.9.9:15000"}, {"http://10.9.9.9", "unix:///run/agent.sock"}} {
		fa.set(true, "12.9.1", bad[0], bad[1])
		if changed, err = rp.refreshServerInfo(ctx); err == nil || changed {
			t.Fatalf("%v while already unreachable must be an error without a further change: changed=%v err=%v", bad, changed, err)
		}
	}
	if s, a, v := attrs(); s != "" || a != "" || v != "" || !publishedTaint(pool(), remote.TaintKeyRemoteUnavailable) {
		t.Fatalf("must stay cleared and tainted: server=%q agent=%q version=%q tainted=%v",
			s, a, v, publishedTaint(pool(), remote.TaintKeyRemoteUnavailable))
	}

	// Reachable again, but with an unparseable version: the endpoints
	// still count as a change (the taint must come off right away), while
	// the version falls back to the last known good one rather than the
	// garbled string or nothing at all.
	fa.set(true, "not-a-version", "https://gpu-a.corp:443/pool-a", "grpc://10.9.9.9:15000")
	if changed, err = rp.refreshServerInfo(ctx); err == nil || !changed {
		t.Fatalf("bad version on an otherwise-reachable server must still publish the endpoints: changed=%v err=%v", changed, err)
	}
	if s, a, v := attrs(); s != "https://gpu-a.corp:443/pool-a" || a != "grpc://10.9.9.9:15000" || v != "12.9.1" || publishedTaint(pool(), remote.TaintKeyRemoteUnavailable) {
		t.Fatalf("published server=%q agent=%q version=%q tainted=%v", s, a, v, publishedTaint(pool(), remote.TaintKeyRemoteUnavailable))
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
