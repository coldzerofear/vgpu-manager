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
	"fmt"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

// DefaultAgentPort is the remote-agent gRPC port; the agent is reached at the
// host of the published endpoint on this port.
const DefaultAgentPort = 14834

const ensureSessionTimeout = 15 * time.Second

// EnsureSessions calls EnsureSession for one partition on the agent behind
// every endpoint it spans. Any failure fails the whole prepare (design D2:
// all servers must confirm before the pod starts).
func EnsureSessions(
	ctx context.Context, endpointInfos []endpointInfo, claim *resourceapi.ResourceClaim, token, partitionKey string, requests []string,
) error {
	for _, info := range endpointInfos {
		if err := ensureOne(ctx, info.agentEndpoint, claim, token, partitionKey, requests); err != nil {
			return fmt.Errorf("EnsureSession on %s (endpoint %s): %w", info.agentEndpoint, info.serverEndpoint, err)
		}
	}
	return nil
}

// agentDialTarget turns the published agentEndpoint (URL form,
// http://host:port[/path]) into a gRPC dial target (bare host:port). A
// future gateway path prefix needs a gRPC-aware route, not this dial.
func agentDialTarget(agentEndpoint string) (string, error) {
	endpoint, err := endpointutil.ParseEndpoint(agentEndpoint)
	if err != nil {
		return "", fmt.Errorf("invalid agent endpoint %q: %w", agentEndpoint, err)
	}
	endpoint.DefaultPort(DefaultAgentPort)
	return endpoint.HostPort(), nil
}

func ensureOne(ctx context.Context, agentEndpoint string, claim *resourceapi.ResourceClaim, token, partitionKey string, requests []string) error {
	ctx, cancel := context.WithTimeout(ctx, ensureSessionTimeout)
	defer cancel()

	target, err := agentDialTarget(agentEndpoint)
	if err != nil {
		return err
	}
	// K1: plaintext. TLS/credentials arrive with D5 (multi-tenant gate).
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
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
		return err
	}
	if !resp.Ready {
		return fmt.Errorf("agent reports session not ready: %s", resp.Message)
	}
	if resp.Message != "" {
		klog.Warningf("EnsureSession %s for claim %s partition %s: %s", agentEndpoint, klog.KObj(claim), partitionKey, resp.Message)
	}
	return nil
}
