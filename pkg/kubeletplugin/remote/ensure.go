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
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

// ensureSessionTimeout bounds one EnsureSession call. Prepare/Unprepare are
// serialized per node (kubeletplugin.Serialize), so this is also the most
// one unreachable agent can hold every other pod's prepare on this node.
const ensureSessionTimeout = 5 * time.Second

// EnsureSessions calls EnsureSession for one partition on the agent behind
// every endpoint it spans and returns the lupine-server endpoints the
// container must be given, in the same order (which is the order of
// endpointInfos, i.e. by agent endpoint -- LUPINE_SERVER order defines the
// virtual device numbering, so it must be deterministic). Each server
// endpoint is what the agent reports now, falling back to the published
// attribute; an agent that knows neither fails the prepare. Any failure
// fails the whole prepare (design D2: all servers must confirm before the
// pod starts). The second result maps each agent endpoint to the etag of
// the client bundle its server embeds ("" when the agent does not know).
func EnsureSessions(
	ctx context.Context, endpointInfos []endpointInfo, claim *resourceapi.ResourceClaim, token, partitionKey string, requests []string,
) ([]string, map[string]string, error) {

	var (
		mu              sync.Mutex
		wg              sync.WaitGroup
		firstErr        error
		errOnce         sync.Once
		serverEndpoints = make([]string, len(endpointInfos))
		etagOf          = make(map[string]string, len(endpointInfos))
	)

	for i, info := range endpointInfos {
		wg.Add(1)
		go func(index int, epInfo endpointInfo) {
			defer wg.Done()
			reported, etag, err := ensureOne(ctx, epInfo.agentEndpoint, claim, token, partitionKey, requests)
			if err != nil {
				errOnce.Do(func() {
					firstErr = fmt.Errorf("EnsureSession on %s: %w", epInfo.agentEndpoint, err)
				})
				return
			}
			serverEndpoint := reported
			if serverEndpoint == "" {
				serverEndpoint = epInfo.serverEndpoint
			}
			if serverEndpoint == "" {
				errOnce.Do(func() {
					firstErr = fmt.Errorf("EnsureSession on %s: agent reports no lupine-server endpoint and none is published for its devices", epInfo.agentEndpoint)
				})
				return
			}
			if reported != "" && epInfo.serverEndpoint != "" && reported != epInfo.serverEndpoint {
				klog.V(2).Infof("EnsureSession %s for claim %s: agent reports lupine-server at %s, published attribute says %s; using the agent's",
					epInfo.agentEndpoint, klog.KObj(claim), reported, epInfo.serverEndpoint)
			}
			mu.Lock()
			etagOf[epInfo.agentEndpoint] = etag
			serverEndpoints[index] = serverEndpoint
			mu.Unlock()
		}(i, info)
	}

	wg.Wait()

	if firstErr != nil {
		return nil, nil, firstErr
	}

	return serverEndpoints, etagOf, nil
}

// ReleaseSessions asks the agent at agentEndpoint to remove the named
// sessions of a claim (tokens are required by the agent). Returns
// how many the agent removed. Callers treat a failure as best effort: the
// agent's claim watch and periodic sweep remove the same sessions later.
func ReleaseSessions(ctx context.Context, agentEndpoint, claimUID string, tokens []string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, remotegpu.AgentCallTimeout)
	defer cancel()

	conn, err := remotegpu.DialAgent(agentEndpoint)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	resp, err := remoteagent.NewRemoteAgentClient(conn).ReleaseSessions(ctx, &remoteagent.ReleaseSessionsRequest{
		ClaimUid: claimUID,
		Tokens:   tokens,
		Owner:    remoteagent.SessionOwner_SESSION_OWNER_CLAIM,
	})
	if err != nil {
		return 0, fmt.Errorf("remote-agent %s: %w", agentEndpoint, err)
	}
	return int(resp.Released), nil
}

// ensureOne materialises the session on one agent and returns the
// lupine-server endpoint that agent reports ("" if it has none yet) and
// the etag of the client bundle its server embeds.
func ensureOne(ctx context.Context, agentEndpoint string, claim *resourceapi.ResourceClaim, token, partitionKey string, requests []string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, ensureSessionTimeout)
	defer cancel()

	conn, err := remotegpu.DialAgent(agentEndpoint)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = conn.Close() }()

	resp, err := remoteagent.NewRemoteAgentClient(conn).EnsureSession(ctx, &remoteagent.EnsureSessionRequest{
		Session:        token,
		ClaimUid:       string(claim.UID),
		ClaimNamespace: claim.Namespace,
		ClaimName:      claim.Name,
		Requests:       requests,
		Partition:      partitionKey,
		// The version that carries the session token: lets the agent tell a
		// claim cache that is merely behind from a token that was never
		// recorded, and read the claim from the API in the former case.
		ClaimResourceVersion: claim.ResourceVersion,
		Owner:                remoteagent.SessionOwner_SESSION_OWNER_CLAIM,
	})
	if err != nil {
		return "", "", err
	}
	if !resp.Ready {
		return "", "", fmt.Errorf("agent reports session not ready: %s", resp.Message)
	}
	if resp.Message != "" {
		klog.Warningf("EnsureSession %s for claim %s partition %s: %s", agentEndpoint, klog.KObj(claim), partitionKey, resp.Message)
	}
	return resp.ServerEndpoint, resp.ClientBundleEtag, nil
}
