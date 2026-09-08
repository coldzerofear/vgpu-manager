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
	"os"
	"runtime"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

// floorDevice is the device whose CUDA ceiling is the claim's floor (the
// one artifact selection is decided against); its agent is where a
// bundle is fetched from.
func floorDevice(devices []resultDevice) resultDevice {
	floor := devices[0]
	for _, rd := range devices[1:] {
		if rd.info.CUDAVersion.Compare(floor.info.CUDAVersion) < 0 {
			floor = rd
		}
	}
	return floor
}

// AgentClientBundleETag asks the agent for the etag of the bundle its
// server embeds (ServerInfo; "" when unknown), whether or not the server
// is listening right now.
func AgentClientBundleETag(ctx context.Context, agentEndpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, serverInfoTimeout)
	defer cancel()
	conn, err := dialAgent(agentEndpoint)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	info, err := remoteagent.NewRemoteAgentClient(conn).ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
	if err != nil {
		return "", fmt.Errorf("remote-agent %s: %w", agentEndpoint, err)
	}
	return info.ClientBundleEtag, nil
}

// ensureArtifact picks the client artifact for a claim and makes sure it is
// the build the servers embed:
//
//   - a version directory installed from a bundle (it records its etag) is
//     used when its etag is the one the floor server reports now, and
//     re-fetched when it is not (a server upgraded in place);
//   - a directory seeded by other means (no etag: an init container) is
//     used as is -- it cannot be verified, and the operator vouched for it;
//   - no usable directory at all: the bundle is fetched from the floor
//     device's agent and installed under the floor version's name.
//
// etagOf maps agent endpoints to the bundle etag they reported in this
// prepare (EnsureSession answers); an agent not in it is asked. token
// supplies, on demand, a session token recorded on the claim -- the
// credential the agent requires for a download.
func (d *InjectDriver) ensureArtifact(ctx context.Context, claim *resourceapi.ResourceClaim, devices []resultDevice, etagOf map[string]string, token func() (string, error)) (*artifactSelection, error) {
	floor := floorDevice(devices)
	agent := floor.info.AgentEndpoint
	want, known := etagOf[agent]
	if !known {
		etag, err := AgentClientBundleETag(ctx, agent)
		if err != nil {
			klog.Warningf("Claim %s: cannot learn the client bundle of %s: %v", klog.KObj(claim), agent, err)
		}
		want = etag
	}
	for other, etag := range etagOf {
		if want != "" && etag != "" && etag != want {
			klog.Warningf("Claim %s spans servers built with different clients (%s embeds %s, %s embeds %s); one of them will refuse the pod's shims",
				klog.KObj(claim), agent, want, other, etag)
		}
	}

	sel, selErr := selectArtifact(d.config.ArtifactsDir, d.config.HostArtifactsDir, cudaFloor(devices))
	var name string
	if selErr == nil {
		have := artifactETag(d.config.ArtifactsDir, sel.Name)
		switch {
		case have == "":
			return sel, nil
		case want == "" || have == want:
			sel.ETag = have
			return sel, nil
		}
		klog.Infof("Client artifact %s was installed from bundle %s but %s now embeds %s; refreshing it", sel.Name, have, agent, want)
		name = sel.Name
	} else {
		if want == "" {
			return nil, fmt.Errorf("%w; and %s reports no client bundle to fetch", selErr, agent)
		}
		name = floor.info.CUDAVersion.Original()
		klog.Infof("Claim %s: %v; fetching the client bundle %s from %s as version %s", klog.KObj(claim), selErr, want, agent, name)
	}

	session, err := token()
	if err != nil {
		return nil, err
	}
	if err := d.fetchArtifact(ctx, agent, session, claim, name); err != nil {
		return nil, fmt.Errorf("client artifact %s: %w", name, err)
	}
	sel, err = selectArtifact(d.config.ArtifactsDir, d.config.HostArtifactsDir, cudaFloor(devices))
	if err != nil {
		return nil, err
	}
	sel.ETag = artifactETag(d.config.ArtifactsDir, sel.Name)
	return sel, nil
}

// fetchArtifact downloads the bundle for this node's platform through
// agent and installs it as version directory name.
func (d *InjectDriver) fetchArtifact(ctx context.Context, agent, session string, claim *resourceapi.ResourceClaim, name string) error {
	if err := os.MkdirAll(d.config.ArtifactsDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d.config.ArtifactsDir, ".bundle-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	info, err := FetchClientBundle(ctx, agent, &remoteagent.FetchClientBundleRequest{
		Session:        session,
		ClaimUid:       string(claim.UID),
		ClaimNamespace: claim.Namespace,
		ClaimName:      claim.Name,
		Os:             "linux",
		Arch:           runtime.GOARCH, // this node's: the pod runs here, not on the GPU node
	}, tmp)
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if info.Platform != "" && info.Platform != LocalClientBundlePlatform() {
		return fmt.Errorf("agent %s served a %s bundle, this node is %s", agent, info.Platform, LocalClientBundlePlatform())
	}
	if err := installClientBundle(d.config.ArtifactsDir, name, tmp.Name(), info); err != nil {
		return err
	}
	klog.Infof("Installed client artifact %s (bundle %s, %d bytes) from %s", name, info.Etag, info.Size, agent)
	return nil
}

// claimPrepareToken mints (or reuses) a claim-scoped token that authorizes
// claim operations at the agents before any per-container session exists
// (NRI mode prepares the artifact at NodePrepare, sessions come later at
// CreateContainer). It is recorded like a session token and cleaned with
// them; no session is ever materialized from it.
func (d *InjectDriver) claimPrepareToken(ctx context.Context, claim *resourceapi.ResourceClaim) (string, error) {
	p := &partition{key: claimPreparePartitionKey}
	if err := d.assignTokens(ctx, claim, []*partition{p}); err != nil {
		return "", err
	}
	d.updatePreparedClaim(claim)
	return p.token, nil
}

// claimPreparePartitionKey names the claim-scoped token's annotation.
const claimPreparePartitionKey = "claim-prepare"
