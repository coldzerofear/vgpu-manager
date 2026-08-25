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
	"net"
	"strconv"

	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
)

// remotePublisher decides how this node's devices are announced with respect
// to remote access (design v2.0): every device is stamped with accessMode,
// and when RemoteGPUSupport is on the pool's node scope is widened from the
// node itself to "the node OR any node labelled as reaching its zone", with
// the lupine-server endpoint published alongside. No second pool exists —
// the same device is consumed locally when the pod lands here and remotely
// otherwise, and the DRA allocator keeps a single account of it.
type remotePublisher struct {
	nodeName string
	spec     *remote.PublishSpec // nil => local-only node
}

func newRemotePublisher(ctx context.Context, config *Config) (*remotePublisher, error) {
	rp := &remotePublisher{nodeName: config.Flags.NodeName}
	if !featuregates.Enabled(featuregates.RemoteGPUSupport) {
		return rp, nil
	}

	endpoint := config.Flags.RemoteEndpoint
	if endpoint == "" {
		ip, err := nodeInternalIP(ctx, config, config.Flags.NodeName)
		if err != nil {
			return nil, fmt.Errorf("derive remote endpoint: %w", err)
		}
		endpoint = net.JoinHostPort(ip, strconv.Itoa(remote.DefaultServerPort))
	}
	rp.spec = &remote.PublishSpec{Endpoint: endpoint, NetZone: config.Flags.RemoteNetZone}
	klog.V(2).Infof("Remote GPU publishing enabled: endpoint=%s zone=%s (pool nodeSelector widened to reachable nodes)",
		endpoint, rp.spec.NetZone)
	return rp, nil
}

func (rp *remotePublisher) enabled() bool {
	return rp != nil && rp.spec != nil
}

// apply stamps the attributes on every device of every slice and sets the
// pool node scope. It is the single hook through which all publishing paths
// (combined/split partitionable slices and the legacy single slice) go.
func (rp *remotePublisher) apply(pool resourceslice.Pool) resourceslice.Pool {
	for i := range pool.Slices {
		remote.Decorate(pool.Slices[i].Devices, rp.spec)
	}
	if rp.enabled() {
		pool.NodeSelector = remote.PoolNodeSelector(rp.nodeName, rp.spec.NetZone)
	}
	return pool
}

func nodeInternalIP(ctx context.Context, config *Config, nodeName string) (string, error) {
	node, err := config.Core.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
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
