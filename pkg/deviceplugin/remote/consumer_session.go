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
	"fmt"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	kubeletremote "github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	corev1 "k8s.io/api/core/v1"
)

// stageClientShim picks the client shim built for this server and prepares its
// preload list. A client must never be newer than the server it talks to, so
// the server's own build version is the ceiling. The shims must already be on
// the node; a server that has not reported its version yet is an error the
// pod can be retried on.
func (m *consumerDevicePlugin) stageClientShim(server *remotegpu.ServerEndpointInfo) (*kubeletremote.ClientArtifact, error) {
	version, err := semver.NewVersion(server.ServerCUDAVersion)
	if err != nil {
		return nil, fmt.Errorf("remote GPU server reports no usable CUDA version (%q): %w",
			server.ServerCUDAVersion, err)
	}
	return kubeletremote.StageClientArtifact(m.cfg.ArtifactsDir, m.cfg.HostArtifactsDir, version)
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
