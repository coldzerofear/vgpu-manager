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
type remotePublisher struct {
	nodeName string
	// mu guards spec.ServerCUDAVersion, which the watcher updates while
	// publish paths read it. spec itself is set once at construction.
	mu   sync.RWMutex
	spec *remote.PublishSpec // nil => local-only node
}

const (
	// How long one probe of lupine-server may take.
	serverProbeTimeout = 3 * time.Second
	// Poll fast while the server has not answered yet (it normally starts a
	// little after this plugin), slowly once it has.
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
	endpoint, err := endpointutil.ParseEndpoint(config.Flags.RemoteServerEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parse server endpoint failed: %w", err)
	}
	if endpoint.Host == "" {
		ip, err := nodeInternalIP(ctx, config, config.Flags.NodeName)
		if err != nil {
			return nil, fmt.Errorf("derive remote endpoint: %w", err)
		}
		endpoint.Host = ip
	}
	agentEndpoint, err := endpointutil.ParseEndpoint(config.Flags.RemoteAgentEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parse agent endpoint failed: %w", err)
	}
	// TODO When no host is specified, the same host address as the server is used by default
	if agentEndpoint.Host == "" {
		agentEndpoint.Host = endpoint.Host
	}
	rp.spec = &remote.PublishSpec{
		Endpoint:      endpoint.String(),
		AgentEndpoint: agentEndpoint.String(),
		Selector:      reachable,
	}
	klog.V(2).Infof("Remote GPU publishing enabled: server-endpoint=%s agent-endpoint=%s reachable-nodes=%q",
		endpoint.String(), agentEndpoint.String(), config.Flags.RemoteNodeSelector)

	// Ask the server once right away so the first publish already carries
	// its version when it is up. If not, the watcher keeps trying.
	if _, err := rp.refreshServerVersion(ctx); err != nil {
		klog.V(2).Infof("lupine-server not answering yet (%v); publishing with the driver ceiling until it does", err)
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

// refreshServerVersion asks lupine-server for its CUDA version and stores
// it. Returns true when the stored value changed: the first answer, or a
// server that came back built from another image. A probe failure keeps
// the last known value (a restart with the same image is the common case).
func (rp *remotePublisher) refreshServerVersion(ctx context.Context) (bool, error) {
	v, err := remote.ProbeServerCUDAVersion(ctx, rp.spec.Endpoint, serverProbeTimeout)
	if err != nil {
		return false, err
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.spec.ServerCUDAVersion != nil && rp.spec.ServerCUDAVersion.Equal(v) {
		return false, nil
	}
	rp.spec.ServerCUDAVersion = v
	return true, nil
}

// watchServerVersion keeps the published serverCudaVersion in step with the
// lupine-server actually running on this node. Every change republishes the
// slices through republish. Runs until ctx is done.
func (rp *remotePublisher) watchServerVersion(ctx context.Context, republish func(context.Context) error) {
	for {
		interval := serverProbeSlow
		changed, err := rp.refreshServerVersion(ctx)
		switch {
		case err != nil:
			interval = serverProbeFast
			klog.V(4).Infof("lupine-server version probe: %v", err)
		case changed:
			klog.Infof("lupine-server %s is built for CUDA %s; republishing devices with serverCudaVersion",
				rp.spec.Endpoint, rp.currentSpec().ServerCUDAVersion)
			if err := republish(ctx); err != nil {
				klog.Errorf("Failed to republish resources after lupine-server version change: %v", err)
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
	return "", fmt.Errorf("node %s has no InternalIP address; set --remote-endpoint explicitly", nodeName)
}
