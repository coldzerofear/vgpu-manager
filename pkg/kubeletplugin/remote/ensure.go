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
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

// DefaultAgentPort is the remote-agent gRPC port; the agent is reached at the
// host of the published endpoint on this port.
const DefaultAgentPort = 14834

const ensureSessionTimeout = 15 * time.Second

// AgentAddr derives the remote-agent address from a published endpoint
// ("host", "host:port", "http://host:port", "https://host"): same host,
// agent port.
func AgentAddr(endpoint string, agentPort int) string {
	host := endpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return net.JoinHostPort(host, strconv.Itoa(agentPort))
}

// EnsureSessions calls EnsureSession on the agent behind every endpoint and
// returns the lowest CUDA version they report. Any failure fails the whole
// prepare (design D2: all servers must confirm before the pod starts).
func EnsureSessions(ctx context.Context, endpoints []string, agentPort int, token string, claim *resourceapi.ResourceClaim) error {
	for _, endpoint := range endpoints {
		addr := AgentAddr(endpoint, agentPort)
		if err := ensureOne(ctx, addr, token, claim); err != nil {
			return fmt.Errorf("EnsureSession on %s (endpoint %s): %w", addr, endpoint, err)
		}
	}
	return nil
}

func ensureOne(ctx context.Context, addr, token string, claim *resourceapi.ResourceClaim) error {
	ctx, cancel := context.WithTimeout(ctx, ensureSessionTimeout)
	defer cancel()

	// K1: plaintext. TLS/credentials arrive with D5 (multi-tenant gate).
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	resp, err := remoteagent.NewRemoteAgentClient(conn).EnsureSession(ctx, &remoteagent.EnsureSessionRequest{
		Session:        token,
		ClaimUid:       string(claim.UID),
		ClaimNamespace: claim.Namespace,
		ClaimName:      claim.Name,
	})
	if err != nil {
		return err
	}
	if !resp.Ready {
		return fmt.Errorf("agent reports session not ready: %s", resp.Message)
	}
	if resp.Message != "" {
		klog.Warningf("EnsureSession %s for claim %s: %s", addr, klog.KObj(claim), resp.Message)
	}
	return nil
}
