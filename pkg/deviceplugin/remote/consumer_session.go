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

// The two things a remote container needs prepared before it starts: the
// client shim it loads, and its session on the GPU server.

import (
	"context"
	"fmt"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	kubeletremote "github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// stageClientShim picks the client shim built for this server and prepares its
// preload list. A client must never be newer than the server it talks to, so
// the server's own build version is the ceiling. A node that has no fitting
// shim (or one from a bundle the server no longer embeds) downloads it from
// the server's agent, which is also how the DRA path gets it -- so the shims
// need not be pre-staged. A server that has not reported its version yet is
// an error the pod can be retried on.
func (m *consumerDevicePlugin) stageClientShim(
	ctx context.Context, pod *corev1.Pod, containerName string, server *remotegpu.ServerEndpointInfo,
) (*kubeletremote.ClientArtifact, error) {
	version, err := semver.NewVersion(server.ServerCUDAVersion)
	if err != nil {
		return nil, fmt.Errorf("remote GPU server reports no usable CUDA version (%q): %w",
			server.ServerCUDAVersion, err)
	}
	return kubeletremote.EnsureClientArtifact(ctx, "pod "+klog.KObj(pod).String(),
		m.cfg.ArtifactsDir, m.cfg.HostArtifactsDir, version,
		server.AgentEndpoint, server.BundleETag,
		func() (*remoteagent.FetchClientBundleRequest, error) {
			// The agent authorizes a download exactly as it authorizes a
			// session: this pod must still be one whose GPUs it serves, and
			// the token must be one of the pod's containers.
			session := m.podSession(pod, containerName)
			return &remoteagent.FetchClientBundleRequest{
				Session:        session.Token,
				ClaimUid:       session.PodUID,
				ClaimNamespace: session.PodNamespace,
				ClaimName:      session.PodName,
				Owner:          remoteagent.SessionOwner_SESSION_OWNER_POD,
			}, nil
		})
}

// podSession is how the agent is asked for one container's session. The token
// is derived from the pod and the container, so no one has to publish it.
func (m *consumerDevicePlugin) podSession(pod *corev1.Pod, containerName string) remotegpu.PodSession {
	return remotegpu.PodSession{
		Token:           remotegpu.SessionToken(string(pod.UID), containerName),
		PodUID:          string(pod.UID),
		PodNamespace:    pod.Namespace,
		PodName:         pod.Name,
		ResourceVersion: pod.ResourceVersion,
	}
}
