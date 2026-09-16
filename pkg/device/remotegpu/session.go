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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/device/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

// On the device-plugin path every container of a remote pod gets its own
// session on the GPU server: its devices and limits are per container, and
// containers that run at the same time must not share one quota.

// sessionTokenLength keeps a derived token the same shape as the random ones
// the DRA path mints (16 bytes, hex).
const sessionTokenLength = 32

// SessionKey identifies the session of one container of a pod.
func SessionKey(podUID, containerName string) string {
	return util.NRIPartitionKey(podUID, containerName)
}

// SessionContainer returns the container name in a session key of podUID.
func SessionContainer(podUID, sessionKey string) (string, bool) {
	name, ok := strings.CutPrefix(sessionKey, podUID+"_")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// SessionToken is the session token of one container: the hash of its session
// key, so the device plugin, the agent and the monitor all derive the same
// value and none of them has to publish it. It is not a secret — the agent
// authorizes a request by checking the pod, not by knowing the token.
func SessionToken(podUID, containerName string) string {
	sum := sha256.Sum256([]byte(SessionKey(podUID, containerName)))
	return hex.EncodeToString(sum[:])[:sessionTokenLength]
}

// PodSession identifies the session of one container of a pod to the agent.
// The request names the pod as its owner and carries the pod's identity in
// the claim_* fields (the RPC predates pod-owned sessions); the agent checks
// it against the pod itself.
type PodSession struct {
	Token           string
	PodUID          string
	PodNamespace    string
	PodName         string
	ResourceVersion string
}

// EnsureSession asks the agent at agentEndpoint to materialize one container's
// session, and returns the lupine-server endpoint it reports ("" when it knows
// none). It is idempotent, so a caller may retry it and several callers may
// ask for the same session. The call is bounded by the caller's context, at
// most AgentCallTimeout.
func EnsureSession(ctx context.Context, agentEndpoint string, session PodSession) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, AgentCallTimeout)
	defer cancel()

	conn, err := DialAgent(agentEndpoint)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	resp, err := remoteagent.NewRemoteAgentClient(conn).EnsureSession(ctx, &remoteagent.EnsureSessionRequest{
		Session:              session.Token,
		ClaimUid:             session.PodUID,
		ClaimNamespace:       session.PodNamespace,
		ClaimName:            session.PodName,
		ClaimResourceVersion: session.ResourceVersion,
		Owner:                remoteagent.SessionOwner_SESSION_OWNER_POD,
	})
	if err != nil {
		return "", fmt.Errorf("remote-agent %s: %w", agentEndpoint, err)
	}
	if !resp.Ready {
		return "", fmt.Errorf("remote-agent %s reports session not ready: %s", agentEndpoint, resp.Message)
	}
	return resp.ServerEndpoint, nil
}

// Session directory layout on the GPU node, as the agent writes it and the
// library fills it in (pkg/remoteagent/session.go documents the whole tree).
// Readers elsewhere -- the monitor on the server node -- go through these, so
// the layout is spelled out once.
const sessionVMemDir = "." + util.VMemNode

// SessionDir is one session's own directory under the session base.
func SessionDir(base, token string) string {
	return filepath.Join(base, token)
}

// SessionQuotaFile is the session's quota region: the devices and limits of
// the container the session belongs to.
func SessionQuotaFile(base, token string) string {
	return filepath.Join(base, token, util.Config, util.VGPUConfigFile)
}

// SessionPidsFile lists the host PIDs accounted to the session -- the
// lupine-server children doing the container's GPU work.
func SessionPidsFile(base, token string) string {
	return filepath.Join(base, token, registry.PidsConfig)
}

// SessionVMemFile is the session's shared virtual-memory region.
func SessionVMemFile(base, token string) string {
	return filepath.Join(base, token, sessionVMemDir, util.VMemNodeFile)
}
