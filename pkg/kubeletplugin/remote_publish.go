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
	"fmt"
	"sync"
	"time"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
)

// remotePublisher decides how this node's devices are announced with respect
// to remote access (design v2.0): every device is stamped with accessMode,
// and when RemoteGPUSupport is on the pool's node scope is widened from the
// node itself to "the node OR any node matching --remote-node-selector",
// with the lupine-server and remote-agent endpoints published alongside. No
// second pool exists — but with the gate on this process is publish-only:
// the DRA service and kubelet registration are disabled and a co-located
// --mode=inject process prepares every claim through the remote path
// (design v2.1 D23), so a pod that mixes this node's devices with another
// node's never sees two incompatible injection paths.
//
// Everything published about the remote path comes from the remote-agent on
// this node (ServerInfo, design D26): the agent probes lupine-server, learns
// the CUDA version it was built with, and works out the address other nodes
// can reach the server -- and the agent itself -- at. This publisher only
// needs to know how to reach the agent (--remote-agent-endpoint). Until the
// agent has answered, the devices are published without endpoints and with
// a NoSchedule taint (see remote.Decorate).
type remotePublisher struct {
	nodeName string
	// agentDial is the endpoint this process calls the agent at: a unix
	// socket bind-mounted from the host, or grpc at the node's IP (this
	// plugin is in the pod network; the agent is on hostNetwork).
	agentDial string
	// mu guards spec, which the watcher updates while publish paths read it.
	mu   sync.RWMutex
	spec *remote.PublishSpec // nil => local-only node
}

func newRemotePublisher(ctx context.Context, config *Config) (*remotePublisher, error) {
	rp := &remotePublisher{nodeName: config.Flags.NodeName}
	if !featuregates.Enabled(featuregates.RemoteGPUSupport) {
		return rp, nil
	}

	reachable, err := remote.ParseNodeSelector(config.Flags.RemoteNodeSelector)
	if err != nil {
		return nil, err
	}
	rp.agentDial, err = remotegpu.ResolveAgentDial(ctx, config.Core, config.Flags.NodeName, config.Flags.RemoteAgentEndpoint)
	if err != nil {
		return nil, err
	}
	rp.spec = &remote.PublishSpec{Selector: reachable}
	klog.V(2).Infof("Remote GPU publishing enabled: agent at %s, reachable-nodes=%q",
		rp.agentDial, config.Flags.RemoteNodeSelector)

	// Ask the agent once right away so the first publish already carries
	// the endpoints and the server's version when it is up. If not, the
	// devices go out tainted and the watcher keeps trying.
	if _, err := rp.refreshServerInfo(ctx); err != nil {
		klog.V(2).Infof("lupine-server state not known yet (%v); publishing the devices tainted until it is", err)
	}
	return rp, nil
}

func (rp *remotePublisher) enabled() bool {
	return rp != nil && rp.spec != nil
}

// apply stamps the attributes on every device of every slice and sets the
// pool node scope. It is the single hook through which all publishing paths
// (combined/split partitionable slices and the legacy single slice) go.
func (rp *remotePublisher) apply(pool resourceslice.Pool) resourceslice.Pool {
	spec := rp.currentSpec()
	for i := range pool.Slices {
		remote.Decorate(pool.Slices[i].Devices, spec)
	}
	if spec != nil {
		pool.NodeSelector = remote.PoolNodeSelector(spec.Selector)
	}
	return pool
}

// currentSpec returns a copy of the spec that is safe to read without the
// lock, or nil on a local-only node.
func (rp *remotePublisher) currentSpec() *remote.PublishSpec {
	if rp == nil || rp.spec == nil {
		return nil
	}
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	spec := *rp.spec
	return &spec
}

// refreshServerInfo asks the agent about lupine-server and stores what it
// learned: the endpoints to publish and the build CUDA version. Returns
// true when a published value changed. A probe failure (the agent is
// unreachable, or reports lupine-server is not listening) or an
// unpublishable answer clears the endpoints right away rather than keeping
// the last known ones: Decorate reads Reachable() off exactly these two
// fields, so clearing them is what flips the NoSchedule taint back on and
// drops the endpoint/version attributes on this same cycle -- the
// scheduler must not keep sending pods at a device whose reachability
// just failed. The CUDA version is the one exception: an answer that
// parses the endpoints but not the version string keeps the last known
// version rather than discarding it over what is likely a cosmetic
// formatting issue on an otherwise-reachable server.
func (rp *remotePublisher) refreshServerInfo(ctx context.Context) (bool, error) {
	setSpec := func(v *semver.Version, server, agent string) bool {
		rp.mu.Lock()
		defer rp.mu.Unlock()

		changed := false
		if v == nil {
			v = rp.spec.ServerCUDAVersion
		}
		if (rp.spec.ServerCUDAVersion == nil) != (v == nil) || (v != nil && !rp.spec.ServerCUDAVersion.Equal(v)) {
			rp.spec.ServerCUDAVersion = v
			changed = true
		}
		if rp.spec.Endpoint != server || rp.spec.AgentEndpoint != agent {
			rp.spec.Endpoint, rp.spec.AgentEndpoint = server, agent
			changed = true
		}
		return changed
	}

	info, err := remotegpu.ProbeServer(ctx, rp.agentDial)
	if err != nil {
		return setSpec(nil, "", ""), err
	}

	v, err := semver.NewVersion(info.ServerCUDAVersion)
	if err != nil {
		return setSpec(nil, info.ServerEndpoint, info.AgentEndpoint), fmt.Errorf("remote-agent %s reports unparseable CUDA version %q: %w",
			rp.agentDial, info.ServerCUDAVersion, err)
	}

	return setSpec(v, info.ServerEndpoint, info.AgentEndpoint), nil
}

// watchServerInfo keeps the published endpoints and serverCudaVersion in
// step with the lupine-server actually running on this node, as reported
// by the agent. Every change republishes the slices through republish.
// Runs until ctx is done.
func (rp *remotePublisher) watchServerInfo(ctx context.Context, republish func(context.Context) error) {
	changedChan := make(chan bool, 0)

	go func() {
		defer close(changedChan)
		wait.UntilWithContext(ctx, func(ctx context.Context) {
			changed, err := rp.refreshServerInfo(ctx)
			if err != nil {
				klog.V(4).Infof("lupine-server info refresh: %v", err)
			}
			changedChan <- changed
		}, 5*time.Second)
	}()

	for {
		select {
		case changed := <-changedChan:
			if changed {
				spec := rp.currentSpec()
				klog.Infof("lupine-server at %s (agent %s) is built for CUDA %s; republishing devices",
					spec.Endpoint, spec.AgentEndpoint, spec.ServerCUDAVersion)
				if err := republish(ctx); err != nil {
					klog.Errorf("Failed to republish resources after lupine-server change: %v", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}
