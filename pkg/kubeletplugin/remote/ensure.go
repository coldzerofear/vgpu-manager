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
	"errors"
	"fmt"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

const ensureSessionTimeout = 15 * time.Second

// EnsureSessions calls EnsureSession for one partition on the agent behind
// every endpoint it spans and returns the lupine-server endpoints the
// container must be given, in the same order (which is the order of
// endpointInfos, i.e. by agent endpoint -- LUPINE_SERVER order defines the
// virtual device numbering, so it must be deterministic). Each server
// endpoint is what the agent reports now, falling back to the published
// attribute; an agent that knows neither fails the prepare. Any failure
// fails the whole prepare (design D2: all servers must confirm before the
// pod starts).
func EnsureSessions(
	ctx context.Context, endpointInfos []endpointInfo, claim *resourceapi.ResourceClaim, token, partitionKey string, requests []string,
) ([]string, error) {
	serverEndpoints := make([]string, 0, len(endpointInfos))
	for _, info := range endpointInfos {
		reported, err := ensureOne(ctx, info.agentEndpoint, claim, token, partitionKey, requests)
		if err != nil {
			return nil, fmt.Errorf("EnsureSession on %s: %w", info.agentEndpoint, err)
		}
		serverEndpoint := reported
		if serverEndpoint == "" {
			serverEndpoint = info.serverEndpoint
		}
		if serverEndpoint == "" {
			return nil, fmt.Errorf("EnsureSession on %s: agent reports no lupine-server endpoint and none is published for its devices", info.agentEndpoint)
		}
		if reported != "" && info.serverEndpoint != "" && reported != info.serverEndpoint {
			klog.V(2).Infof("EnsureSession %s for claim %s: agent reports lupine-server at %s, published attribute says %s; using the agent's",
				info.agentEndpoint, klog.KObj(claim), reported, info.serverEndpoint)
		}
		serverEndpoints = append(serverEndpoints, serverEndpoint)
	}
	return serverEndpoints, nil
}

const serverInfoTimeout = 5 * time.Second

// ErrServerNotListening is returned (wrapped) by ServerInfo when the agent
// answers but reports that lupine-server did not pass its last probe.
var ErrServerNotListening = errors.New("lupine-server is not listening")

// ServerInfo asks the remote-agent at agentEndpoint what it knows about its
// lupine-server: reachability, the CUDA version it was built with, and the
// endpoint other nodes should use. This is how every other component learns
// about the server without having its address configured. A server that
// did not pass the agent's last probe is an ErrServerNotListening error.
func ServerInfo(ctx context.Context, agentEndpoint string) (*remoteagent.ServerInfoResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, serverInfoTimeout)
	defer cancel()

	conn, err := dialAgent(agentEndpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	info, err := remoteagent.NewRemoteAgentClient(conn).ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("remote-agent %s: %w", agentEndpoint, err)
	}
	if !info.Listening {
		return nil, fmt.Errorf("remote-agent %s (node %s): %w", agentEndpoint, info.NodeName, ErrServerNotListening)
	}
	return info, nil
}

// dialAgent opens a client connection to the agent. K1: plaintext;
// TLS/credentials arrive with D5 (multi-tenant gate). grpc.NewClient does not
// connect until the first RPC, so this never blocks.
func dialAgent(agentEndpoint string) (*grpc.ClientConn, error) {
	target, err := agentDialTarget(agentEndpoint)
	if err != nil {
		return nil, err
	}
	return grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// agentDialTarget turns an agent endpoint (URL form; grpc://host:port[/path]
// as published, http(s):// accepted for older publishers, or unix:///path
// for a same-node socket) into a gRPC dial target: bare host:port, or the
// unix:// URL grpc-go resolves itself. A future gateway path prefix needs a
// gRPC-aware route, not this dial.
func agentDialTarget(agentEndpoint string) (string, error) {
	endpoint, err := ParseAgentEndpoint(agentEndpoint)
	if err != nil {
		return "", err
	}
	if endpoint.Scheme != endpointutil.Unix && (endpoint.Host == "" || endpoint.Port == "0") {
		return "", fmt.Errorf("invalid remote-agent endpoint %q: a host and a non-zero port are required", agentEndpoint)
	}
	return endpoint.DialTarget(), nil
}

// ensureOne materialises the session on one agent and returns the
// lupine-server endpoint that agent reports ("" if it has none yet).
func ensureOne(ctx context.Context, agentEndpoint string, claim *resourceapi.ResourceClaim, token, partitionKey string, requests []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ensureSessionTimeout)
	defer cancel()

	conn, err := dialAgent(agentEndpoint)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	resp, err := remoteagent.NewRemoteAgentClient(conn).EnsureSession(ctx, &remoteagent.EnsureSessionRequest{
		Session:        token,
		ClaimUid:       string(claim.UID),
		ClaimNamespace: claim.Namespace,
		ClaimName:      claim.Name,
		Requests:       requests,
		Partition:      partitionKey,
	})
	if err != nil {
		return "", err
	}
	if !resp.Ready {
		return "", fmt.Errorf("agent reports session not ready: %s", resp.Message)
	}
	if resp.Message != "" {
		klog.Warningf("EnsureSession %s for claim %s partition %s: %s", agentEndpoint, klog.KObj(claim), partitionKey, resp.Message)
	}
	return resp.ServerEndpoint, nil
}
