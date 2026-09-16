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
	"os"
	"path/filepath"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// stagedArtifacts is a client shim directory as an operator (or the bundle
// download) leaves it: one directory per CUDA version.
func stagedArtifacts(t *testing.T, versions ...string) (artifactsDir string) {
	t.Helper()
	artifactsDir = t.TempDir()
	for _, version := range versions {
		dir := filepath.Join(artifactsDir, version)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		for _, lib := range []string{"libcuda.so.1", "libnvidia-ml.so.1"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, lib), []byte("so"), 0o644))
		}
	}
	return artifactsDir
}

// ensuredSessions records what the plugin asked the agent for.
type ensuredSessions struct {
	calls []remotegpu.PodSession
	err   error
}

func (e *ensuredSessions) ensure(_ context.Context, _ string, session remotegpu.PodSession) (string, error) {
	e.calls = append(e.calls, session)
	return "", e.err
}

func (e *ensuredSessions) tokens() []string {
	tokens := make([]string, 0, len(e.calls))
	for _, call := range e.calls {
		tokens = append(tokens, call.Token)
	}
	return tokens
}

// newPreStartPlugin is a consumer plugin with kubelet and the agent stubbed.
func newPreStartPlugin(t *testing.T, pod *corev1.Pod, artifactsDir string, matches ...containerMatch) (*consumerDevicePlugin, *ensuredSessions) {
	t.Helper()
	kubeClient := fake.NewClientset(pod, serverNode(t))
	plugin, _ := newConsumerPlugin(t, kubeClient)
	plugin.cfg.ArtifactsDir = artifactsDir
	plugin.cfg.HostArtifactsDir = "/host/vgpu-manager/driver"
	sessions := &ensuredSessions{}
	plugin.ensureSession = sessions.ensure
	plugin.lookup = func(context.Context, []string) ([]containerMatch, error) { return matches, nil }
	return plugin, sessions
}

func TestPreStartContainer(t *testing.T) {
	pod := allocatingPod(t)
	artifactsDir := stagedArtifacts(t, "12.9", "14.0") // 14.0 is newer than the server
	plugin, sessions := newPreStartPlugin(t, pod, artifactsDir, containerMatch{pod: pod, container: "cont1"})
	contDir, _ := plugin.containerPaths(pod.UID, "cont1")
	require.NoError(t, util.EnsureDir(contDir, 0o755))

	_, err := plugin.PreStartContainer(context.Background(), &pluginapi.PreStartContainerRequest{
		DevicesIds: []string{"remote-vgpu-0"},
	})

	require.NoError(t, err)
	assert.Equal(t, []string{remotegpu.SessionToken(string(pod.UID), "cont1")}, sessions.tokens())

	// The container's mount sources now resolve to the shim built for this
	// server: 12.9, because a client must not be newer than the server (13.3.73).
	target, err := os.Readlink(filepath.Join(contDir, driverLinkName))
	require.NoError(t, err)
	assert.Equal(t, "/host/vgpu-manager/driver/12.9", target)
	target, err = os.Readlink(filepath.Join(contDir, ldPreloadFileName))
	require.NoError(t, err)
	assert.Equal(t, "/host/vgpu-manager/driver/12.9/remote-ld.so.preload", target)

	// The preload list names the shims by their in-container path.
	preload, err := os.ReadFile(filepath.Join(artifactsDir, "12.9", "remote-ld.so.preload"))
	require.NoError(t, err)
	assert.Equal(t, "/etc/vgpu-manager/driver/libcuda.so.1\n/etc/vgpu-manager/driver/libnvidia-ml.so.1\n", string(preload))

	// Repeating it changes nothing: kubelet retries, and a container restart
	// goes through PreStartContainer again.
	_, err = plugin.PreStartContainer(context.Background(), &pluginapi.PreStartContainerRequest{
		DevicesIds: []string{"remote-vgpu-0"},
	})
	require.NoError(t, err)
	assert.Len(t, sessions.calls, 2, "the session is ensured again, idempotently")
}

// kubelet may hand an init container's device ids to the app container, so one
// request can belong to two containers. Both must end up with a session --
// picking one of them is what left the other without one.
func TestPreStartContainerPreparesEveryMatchingContainer(t *testing.T) {
	preAllocated, err := device.PodDeviceClaim{
		{Name: "init", DeviceClaims: []device.DeviceClaim{{Id: 0, Uuid: testGPUUUID, Cores: 50, Memory: 4096}}},
		{Name: "app", DeviceClaims: []device.DeviceClaim{{Id: 0, Uuid: testGPUUUID, Cores: 50, Memory: 4096}}},
	}.MarshalText()
	require.NoError(t, err)
	pod := allocatingPod(t)
	pod.Annotations[util.PodVGPUPreAllocAnnotation] = preAllocated
	plugin, sessions := newPreStartPlugin(t, pod, stagedArtifacts(t, "12.9"),
		containerMatch{pod: pod, container: "init"}, containerMatch{pod: pod, container: "app"})
	for _, container := range []string{"init", "app"} {
		contDir, _ := plugin.containerPaths(pod.UID, container)
		require.NoError(t, util.EnsureDir(contDir, 0o755))
	}

	_, err = plugin.PreStartContainer(context.Background(), &pluginapi.PreStartContainerRequest{
		DevicesIds: []string{"remote-vgpu-0"},
	})

	require.NoError(t, err)
	assert.Equal(t, []string{
		remotegpu.SessionToken(string(pod.UID), "init"),
		remotegpu.SessionToken(string(pod.UID), "app"),
	}, sessions.tokens())
}

func TestPreStartContainerFailures(t *testing.T) {
	pod := allocatingPod(t)

	t.Run("no client shim on the node", func(t *testing.T) {
		plugin, sessions := newPreStartPlugin(t, pod, t.TempDir(), containerMatch{pod: pod, container: "cont1"})

		_, err := plugin.PreStartContainer(context.Background(), &pluginapi.PreStartContainerRequest{
			DevicesIds: []string{"remote-vgpu-0"},
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), util.PreStartContainerCheckErrMsg)
		assert.Empty(t, sessions.calls, "nothing is asked for before the shim is there")
	})
	t.Run("the agent refuses the session", func(t *testing.T) {
		plugin, sessions := newPreStartPlugin(t, pod, stagedArtifacts(t, "12.9"),
			containerMatch{pod: pod, container: "cont1"})
		contDir, _ := plugin.containerPaths(pod.UID, "cont1")
		require.NoError(t, util.EnsureDir(contDir, 0o755))
		sessions.err = assert.AnError

		_, err := plugin.PreStartContainer(context.Background(), &pluginapi.PreStartContainerRequest{
			DevicesIds: []string{"remote-vgpu-0"},
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), util.PreStartContainerCheckErrMsg)
	})
	t.Run("no container holds these devices", func(t *testing.T) {
		plugin, _ := newPreStartPlugin(t, pod, stagedArtifacts(t, "12.9"))
		plugin.lookup = func(context.Context, []string) ([]containerMatch, error) {
			return nil, assert.AnError
		}

		_, err := plugin.PreStartContainer(context.Background(), &pluginapi.PreStartContainerRequest{
			DevicesIds: []string{"remote-vgpu-0"},
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), util.PreStartContainerCheckErrMsg)
	})
}

// Allocate asks for the session of the container it knows exactly, and a
// failure there is left to PreStartContainer rather than failing admission.
func TestAllocateTriesTheSession(t *testing.T) {
	pod := allocatingPod(t)
	kubeClient := fake.NewClientset(pod, serverNode(t))
	plugin, _ := newConsumerPlugin(t, kubeClient)
	sessions := &ensuredSessions{err: assert.AnError}
	plugin.ensureSession = sessions.ensure

	_, err := plugin.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"remote-vgpu-0"}}},
	})

	require.NoError(t, err, "an unreachable agent must not fail admission")
	assert.Equal(t, []string{remotegpu.SessionToken(string(pod.UID), "cont1")}, sessions.tokens())
}
