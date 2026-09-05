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

package remote

import (
	"context"
	"net"
	"reflect"
	"sync"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"google.golang.org/grpc"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeSessionAgent answers EnsureSession with a fixed readiness and server
// endpoint, and records what it was asked.
type fakeSessionAgent struct {
	remoteagent.UnimplementedRemoteAgentServer
	mu             sync.Mutex
	ready          bool
	serverEndpoint string
	requests       []*remoteagent.EnsureSessionRequest
}

func (f *fakeSessionAgent) EnsureSession(_ context.Context, req *remoteagent.EnsureSessionRequest) (*remoteagent.EnsureSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	resp := &remoteagent.EnsureSessionResponse{Ready: f.ready, ServerEndpoint: f.serverEndpoint, CudaDriverVersion: "13.3.73"}
	if !f.ready {
		resp.Message = "lupine-server is not accepting connections yet"
	}
	return resp, nil
}

func startSessionAgent(t *testing.T, ready bool, serverEndpoint string) (*fakeSessionAgent, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeSessionAgent{ready: ready, serverEndpoint: serverEndpoint}
	srv := grpc.NewServer()
	remoteagent.RegisterRemoteAgentServer(srv, fa)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return fa, "grpc://" + lis.Addr().String()
}

func TestEnsureSessions(t *testing.T) {
	ctx := context.Background()
	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "uid-1"}}

	t.Run("agent's server endpoint wins over the published attribute; order follows the agents", func(t *testing.T) {
		fa1, agent1 := startSessionAgent(t, true, "http://10.0.0.1:14833")
		_, agent2 := startSessionAgent(t, true, "http://10.0.0.2:14833")
		infos := endpointInfosOf([]resultDevice{
			{info: &DeviceInfo{AgentEndpoint: agent2, Endpoint: "http://stale:1"}},
			{info: &DeviceInfo{AgentEndpoint: agent1, Endpoint: ""}},
			{info: &DeviceInfo{AgentEndpoint: agent2, Endpoint: "http://10.0.0.2:14833"}},
		})
		// Sorted by agent endpoint, deduplicated, first non-empty server kept.
		if len(infos) != 2 || infos[0].agentEndpoint > infos[1].agentEndpoint || infos[0].agentEndpoint == infos[1].agentEndpoint {
			t.Fatalf("infos = %+v", infos)
		}
		got, err := EnsureSessions(ctx, infos, claim, "tok", "part", []string{"r1"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{}
		for _, info := range infos {
			if info.agentEndpoint == agent1 {
				want = append(want, "http://10.0.0.1:14833")
			} else {
				want = append(want, "http://10.0.0.2:14833")
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("servers = %v, want %v", got, want)
		}
		if len(fa1.requests) != 1 || fa1.requests[0].Session != "tok" || fa1.requests[0].ClaimUid != "uid-1" || fa1.requests[0].Partition != "part" {
			t.Fatalf("agent1 saw %+v", fa1.requests)
		}
	})

	t.Run("published attribute is the fallback when the agent reports none", func(t *testing.T) {
		_, agent := startSessionAgent(t, true, "")
		got, err := EnsureSessions(ctx, []endpointInfo{{agentEndpoint: agent, serverEndpoint: "http://10.0.0.1:14833"}}, claim, "tok", "part", nil)
		if err != nil || !reflect.DeepEqual(got, []string{"http://10.0.0.1:14833"}) {
			t.Fatalf("got %v, %v", got, err)
		}
	})

	t.Run("neither known: prepare fails", func(t *testing.T) {
		_, agent := startSessionAgent(t, true, "")
		if got, err := EnsureSessions(ctx, []endpointInfo{{agentEndpoint: agent}}, claim, "tok", "part", nil); err == nil {
			t.Fatalf("expected an error, got %v", got)
		}
	})

	t.Run("server down: prepare fails with the agent's message", func(t *testing.T) {
		_, agent := startSessionAgent(t, false, "http://10.0.0.1:14833")
		if _, err := EnsureSessions(ctx, []endpointInfo{{agentEndpoint: agent}}, claim, "tok", "part", nil); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("unreachable agent fails", func(t *testing.T) {
		if _, err := EnsureSessions(ctx, []endpointInfo{{agentEndpoint: "grpc://127.0.0.1:1"}}, claim, "tok", "part", nil); err == nil {
			t.Fatal("expected an error")
		}
	})
}
