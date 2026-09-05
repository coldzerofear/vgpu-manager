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
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
)

// remotePublisher decides how this node's devices are announced with respect
// to remote access (design v2.0): every device is stamped with accessMode,
// and when RemoteGPUSupport is on the pool's node scope is widened from the
// node itself to "the node OR any node matching --remote-node-selector",
// with the lupine-server endpoint published alongside. No second pool exists
// — but with the gate on this process is publish-only: the DRA service and
// kubelet registration are disabled and a co-located --mode=inject process
// prepares every claim through the remote path (design v2.1 D23), so a pod
// that mixes this node's devices with another node's never sees two
// incompatible injection paths.
//
// What is published about lupine-server comes from the remote-agent on this
// node (ServerInfo), not from the server: the agent probes it, learns the
// CUDA version it was built with and the endpoint other nodes can reach it
// at, and this publisher only needs the agent's address. An operator can
// still pin either endpoint through the flags; a pinned host is never
// replaced by what the agent reports.
type remotePublisher struct {
	nodeName string
	// agentDial is the endpoint this process calls the agent at. Normally
	// the published one with the host resolved for this node; a unix socket
	// when --remote-agent-local-endpoint says so.
	agentDial string
	// serverEndpoint / agentEndpoint are the operator's flags, parsed. A
	// non-empty Host pins the published value.
	serverEndpoint endpointutil.Endpoint
	agentEndpoint  endpointutil.Endpoint
	// nodeIP is the fallback host for an unpinned endpoint while the agent
	// has not reported one.
	nodeIP string
	// mu guards spec, which the watcher updates while publish paths read it.
	mu   sync.RWMutex
	spec *remote.PublishSpec // nil => local-only node
}

const (
	// Poll fast while the agent/server have not answered yet (they normally
	// start a little after this plugin), slowly once they have.
	serverProbeFast = 5 * time.Second
	serverProbeSlow = 60 * time.Second
)

func newRemotePublisher(ctx context.Context, config *Config) (*remotePublisher, error) {
	rp := &remotePublisher{nodeName: config.Flags.NodeName}
	if !featuregates.Enabled(featuregates.RemoteGPUSupport) {
		return rp, nil
	}

	reachable, err := remote.ParseNodeSelector(config.Flags.RemoteNodeSelector)
	if err != nil {
		return nil, err
	}
	serverEndpoint, err := endpointutil.ParseEndpoint(config.Flags.RemoteServerEndpoint,
		endpointutil.WithDefaultScheme(endpointutil.Http),
		endpointutil.WithDefaultPort(remote.DefaultServerPort))
	if err != nil {
		return nil, fmt.Errorf("parse server endpoint failed: %w", err)
	}
	agentEndpoint, err := endpointutil.ParseEndpoint(config.Flags.RemoteAgentEndpoint,
		endpointutil.WithDefaultScheme(endpointutil.Grpc),
		endpointutil.WithDefaultPort(remote.DefaultAgentPort))
	if err != nil {
		return nil, fmt.Errorf("parse agent endpoint failed: %w", err)
	}
	rp.serverEndpoint, rp.agentEndpoint = *serverEndpoint, *agentEndpoint

	// The node's InternalIP stands in for every host nobody supplied:
	// this is the common hostNetwork case, and it is also what the agent
	// itself prefers when it discovers the server's address.
	if serverEndpoint.Host == "" || agentEndpoint.Host == "" {
		rp.nodeIP, err = nodeInternalIP(ctx, config, config.Flags.NodeName)
		if err != nil {
			return nil, fmt.Errorf("derive remote endpoint: %w", err)
		}
	}

	// How this process reaches the agent: the local override, else the
	// published endpoint with the host filled in for this node.
	if config.Flags.RemoteAgentLocalEndpoint != "" {
		local, err := endpointutil.ParseEndpoint(config.Flags.RemoteAgentLocalEndpoint,
			endpointutil.WithDefaultScheme(endpointutil.Grpc),
			endpointutil.WithDefaultPort(remote.DefaultAgentPort))
		if err != nil {
			return nil, fmt.Errorf("parse agent local endpoint failed: %w", err)
		}
		rp.agentDial = local.String()
	} else {
		dial := *agentEndpoint
		if dial.Host == "" {
			if serverEndpoint.Host != "" {
				dial.Host = serverEndpoint.Host
			} else {
				dial.Host = rp.nodeIP
			}
		}
		rp.agentDial = dial.String()
	}

	rp.spec = &remote.PublishSpec{Selector: reachable}
	rp.spec.Endpoint, rp.spec.AgentEndpoint = rp.publishedEndpoints("")
	klog.V(2).Infof("Remote GPU publishing enabled: server-endpoint=%s agent-endpoint=%s (agent dialed at %s) reachable-nodes=%q",
		rp.spec.Endpoint, rp.spec.AgentEndpoint, rp.agentDial, config.Flags.RemoteNodeSelector)

	// Ask the agent once right away so the first publish already carries
	// the server's version and endpoint when it is up. If not, the watcher
	// keeps trying.
	if _, err := rp.refreshServerInfo(ctx); err != nil {
		klog.V(2).Infof("lupine-server state not known yet (%v); publishing with the driver ceiling until it is", err)
	}
	return rp, nil
}

// publishedEndpoints derives the server and agent endpoints to publish from
// the operator's flags and what the agent reported (reported == "" when
// nothing yet). A pinned host wins; an unpinned server host is the reported
// endpoint's host, else the node IP; an unpinned agent host follows the
// server's host, because the two run on the same node.
func (rp *remotePublisher) publishedEndpoints(reported string) (server, agent string) {
	s := rp.serverEndpoint
	if s.Host == "" {
		s.Host = rp.nodeIP
		if reported != "" {
			if r, err := endpointutil.ParseEndpoint(reported,
				endpointutil.WithDefaultScheme(endpointutil.Http),
				endpointutil.WithDefaultPort(remote.DefaultServerPort)); err == nil && r.Host != "" && !r.IsLoopback() {
				s = *r
			} else {
				klog.Warningf("remote-agent reported unusable server endpoint %q; publishing %s instead", reported, s.String())
			}
		}
	}
	a := rp.agentEndpoint
	if a.Host == "" {
		a.Host = s.Host
	}
	return s.String(), a.String()
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
// learned: the build CUDA version and the endpoints to publish. Returns
// true when a published value changed: the first answer, a server that
// came back built from another image, or one that moved. A failed call
// keeps the last known values (a restart with the same image on the same
// address is the common case).
func (rp *remotePublisher) refreshServerInfo(ctx context.Context) (bool, error) {
	info, err := remote.ServerInfo(ctx, rp.agentDial)
	if err != nil {
		return false, err
	}
	v, err := semver.NewVersion(info.CudaDriverVersion)
	if err != nil {
		return false, fmt.Errorf("remote-agent %s reports unparseable CUDA version %q: %w",
			rp.agentDial, info.CudaDriverVersion, err)
	}
	server, agent := rp.publishedEndpoints(info.Endpoint)

	rp.mu.Lock()
	defer rp.mu.Unlock()
	changed := false
	if rp.spec.ServerCUDAVersion == nil || !rp.spec.ServerCUDAVersion.Equal(v) {
		rp.spec.ServerCUDAVersion = v
		changed = true
	}
	if rp.spec.Endpoint != server || rp.spec.AgentEndpoint != agent {
		rp.spec.Endpoint, rp.spec.AgentEndpoint = server, agent
		changed = true
	}
	return changed, nil
}

// watchServerInfo keeps the published serverCudaVersion and endpoints in
// step with the lupine-server actually running on this node, as reported
// by the agent. Every change republishes the slices through republish.
// Runs until ctx is done.
func (rp *remotePublisher) watchServerInfo(ctx context.Context, republish func(context.Context) error) {
	for {
		interval := serverProbeSlow
		changed, err := rp.refreshServerInfo(ctx)
		switch {
		case err != nil:
			interval = serverProbeFast
			klog.V(4).Infof("lupine-server info refresh: %v", err)
		case changed:
			spec := rp.currentSpec()
			klog.Infof("lupine-server at %s (agent %s) is built for CUDA %s; republishing devices",
				spec.Endpoint, spec.AgentEndpoint, spec.ServerCUDAVersion)
			if err := republish(ctx); err != nil {
				klog.Errorf("Failed to republish resources after lupine-server change: %v", err)
				interval = serverProbeFast
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func nodeInternalIP(ctx context.Context, config *Config, nodeName string) (string, error) {
	node, err := config.Core.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{ResourceVersion: "0"})
	if err != nil {
		return "", fmt.Errorf("get node %s: %w", nodeName, err)
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("node %s has no InternalIP address; set --remote-server-endpoint explicitly", nodeName)
}
