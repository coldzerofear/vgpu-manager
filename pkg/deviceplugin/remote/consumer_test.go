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

	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	kubeletremote "github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	testConsumerNode = "consumer-node"
	testServerNode   = "gpu-server"
	testGPUUUID      = "GPU-00000000-0000-0000-0000-000000000000"
)

// serverNode is a GPU server as the device plugin publishes it.
func serverNode(t *testing.T) *corev1.Node {
	t.Helper()
	endpoints, err := remotegpu.ServerEndpointInfo{
		ServerEndpoint: "http://10.0.0.7:14833", AgentEndpoint: "grpc://10.0.0.7:14834",
		ServerCUDAVersion: "13.3.73",
	}.Encode()
	require.NoError(t, err)
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testServerNode,
			Labels:      map[string]string{util.NodeRemoteServerLabel: "true"},
			Annotations: map[string]string{util.NodeRemoteEndpointsAnnotation: endpoints},
		},
	}
}

// allocatingPod is a remote pod kubelet is admitting on the consumer node.
func allocatingPod(t *testing.T, containers ...string) *corev1.Pod {
	t.Helper()
	if len(containers) == 0 {
		containers = []string{"cont1"}
	}
	claims := make(device.PodDeviceClaim, 0, len(containers))
	spec := corev1.PodSpec{NodeName: testConsumerNode}
	for _, name := range containers {
		claims = append(claims, device.ContainerDeviceClaim{
			Name:         name,
			DeviceClaims: []device.DeviceClaim{{Id: 0, Uuid: testGPUUUID, Cores: 50, Memory: 4096}},
		})
		spec.Containers = append(spec.Containers, corev1.Container{
			Name: name,
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("1"),
			}},
		})
	}
	preAllocated, err := claims.MarshalText()
	require.NoError(t, err)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "remote-pod", Namespace: "ns", UID: k8stypes.UID("pod-uid"),
			Labels: map[string]string{util.PodAssignedPhaseLabel: string(util.AssignPhaseAllocating)},
			Annotations: map[string]string{
				util.VGPUAccessModeAnnotation:   util.AccessModeRemote,
				util.PodPredicateNodeAnnotation: testServerNode,
				util.PodPredicateTimeAnnotation: "1757000000000000000",
				util.PodVGPUPreAllocAnnotation:  preAllocated,
			},
		},
		Spec:   spec,
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

// stagedArtifacts is a client shim directory as an operator (or a bundle
// download) leaves it: one directory per CUDA version.
func stagedArtifacts(t *testing.T, versions ...string) string {
	t.Helper()
	artifactsDir := t.TempDir()
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
	// endpoint is the lupine-server address the agent answers with; empty
	// means it knows none and the published one has to do.
	endpoint string
	err      error
}

func (e *ensuredSessions) ensure(_ context.Context, _ string, session remotegpu.PodSession) (string, error) {
	e.calls = append(e.calls, session)
	return e.endpoint, e.err
}

func (e *ensuredSessions) tokens() []string {
	tokens := make([]string, 0, len(e.calls))
	for _, call := range e.calls {
		tokens = append(tokens, call.Token)
	}
	return tokens
}

func newConsumerPlugin(t *testing.T, kubeClient kubernetes.Interface, artifactsDir string) (*Plugin, *ensuredSessions) {
	t.Helper()
	// A consumer node has no GPUs, so its manager has no devices either.
	nodeConfig, err := node.NewNodeConfig(node.WithNodeNameOption(testConsumerNode))
	require.NoError(t, err)
	plugin, err := New(Config{
		NodeName:     testConsumerNode,
		ResourceName: util.VGPUNumberResourceName,
		Socket:       filepath.Join(t.TempDir(), "remote.sock"),
	}, manager.NewDevicelessManager(nodeConfig),
		WithConsumerRole(kubeClient, ConsumerOptions{
			VGPUNumber:       4,
			ArtifactsDir:     artifactsDir,
			HostArtifactsDir: "/host/vgpu-manager/driver",
		}))
	require.NoError(t, err)
	sessions := &ensuredSessions{}
	plugin.consumer.ensureSession = sessions.ensure
	return plugin, sessions
}

func allocateOne(t *testing.T, plugin *Plugin, containers int) (*pluginapi.AllocateResponse, error) {
	t.Helper()
	requests := make([]*pluginapi.ContainerAllocateRequest, 0, containers)
	for range containers {
		requests = append(requests, &pluginapi.ContainerAllocateRequest{DevicesIds: []string{"remote-vgpu-0"}})
	}
	return plugin.Allocate(context.Background(), &pluginapi.AllocateRequest{ContainerRequests: requests})
}

func TestConsumerDevices(t *testing.T) {
	plugin, _ := newConsumerPlugin(t, fake.NewClientset(), t.TempDir())

	devices := plugin.Devices()

	require.Len(t, devices, 4, "the node serves as many remote vGPUs as configured")
	assert.Equal(t, "remote-vgpu-0", devices[0].ID)
	for _, dev := range devices {
		assert.Equal(t, pluginapi.Healthy, dev.Health, "slots are not hardware; they never turn unhealthy")
	}
	options, err := plugin.GetDevicePluginOptions(context.Background(), &pluginapi.Empty{})
	require.NoError(t, err)
	assert.False(t, options.PreStartRequired, "everything is prepared in Allocate")
}

func TestConsumerAllocate(t *testing.T) {
	pod := allocatingPod(t)
	kubeClient := fake.NewClientset(pod, serverNode(t))
	// 14.0 is newer than the server, so it must not be picked.
	artifactsDir := stagedArtifacts(t, "12.9", "14.0")
	plugin, sessions := newConsumerPlugin(t, kubeClient, artifactsDir)

	resp, err := allocateOne(t, plugin, 1)

	require.NoError(t, err)
	require.Len(t, resp.ContainerResponses, 1)
	container := resp.ContainerResponses[0]

	// The container's session was created on the server before it may start.
	require.Equal(t, []string{remotegpu.SessionToken("pod-uid", "cont1")}, sessions.tokens())
	assert.Equal(t, remotegpu.PodSession{
		Token: remotegpu.SessionToken("pod-uid", "cont1"), PodUID: "pod-uid",
		PodNamespace: "ns", PodName: "remote-pod", ResourceVersion: pod.ResourceVersion,
	}, sessions.calls[0])

	// It is told where its GPUs are and which session they run in.
	assert.Equal(t, "http://10.0.0.7:14833", container.Envs[kubeletremote.EnvLupineServer])
	assert.Equal(t, remotegpu.SessionToken("pod-uid", "cont1"), container.Envs[kubeletremote.EnvLupineSession])
	assert.Equal(t, "1", container.Envs[kubeletremote.EnvLupineDisableLocal])
	assert.Equal(t, "void", container.Envs["NVIDIA_VISIBLE_DEVICES"])
	assert.Equal(t, "cont1", container.Envs[util.ContNameEnv])

	// The client shim it loads is the newest one not newer than the server.
	require.Len(t, container.Mounts, 2)
	assert.Equal(t, &pluginapi.Mount{
		ContainerPath: filepath.Join(util.ManagerRootPath, util.Driver),
		HostPath:      "/host/vgpu-manager/driver/12.9",
		ReadOnly:      true,
	}, container.Mounts[0])
	assert.Equal(t, &pluginapi.Mount{
		ContainerPath: vgpu.ContPreLoadFilePath,
		HostPath:      "/host/vgpu-manager/driver/12.9/remote-ld.so.preload",
		ReadOnly:      true,
	}, container.Mounts[1])
	assert.Empty(t, container.Devices, "no local device is injected")

	// The preload list names the shims by their in-container path.
	preload, err := os.ReadFile(filepath.Join(artifactsDir, "12.9", "remote-ld.so.preload"))
	require.NoError(t, err)
	assert.Equal(t, "/etc/vgpu-manager/driver/libcuda.so.1\n/etc/vgpu-manager/driver/libnvidia-ml.so.1\n", string(preload))

	// The allocation is recorded on the pod, with the server still reporting it.
	got, err := kubeClient.CoreV1().Pods("ns").Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, got.Annotations[util.PodVGPURealAllocAnnotation], testGPUUUID)
	assert.Equal(t, string(util.AssignPhaseSucceed), got.Labels[util.PodAssignedPhaseLabel])
	assert.Equal(t, testServerNode, got.Labels[util.PodMetricsNodeLabel])
}

// Every container gets its own session, and the pre-allocation says which
// container each request belongs to -- kubelet's device ids never have to.
func TestConsumerAllocateEveryContainer(t *testing.T) {
	pod := allocatingPod(t, "init", "app")
	plugin, sessions := newConsumerPlugin(t, fake.NewClientset(pod, serverNode(t)), stagedArtifacts(t, "12.9"))

	resp, err := allocateOne(t, plugin, 2)

	require.NoError(t, err)
	require.Len(t, resp.ContainerResponses, 2)
	assert.Equal(t, []string{
		remotegpu.SessionToken("pod-uid", "init"),
		remotegpu.SessionToken("pod-uid", "app"),
	}, sessions.tokens())
	assert.Equal(t, remotegpu.SessionToken("pod-uid", "init"), resp.ContainerResponses[0].Envs[kubeletremote.EnvLupineSession])
	assert.Equal(t, remotegpu.SessionToken("pod-uid", "app"), resp.ContainerResponses[1].Envs[kubeletremote.EnvLupineSession])
}

func TestConsumerAllocateFailures(t *testing.T) {
	// A pod may only start once its session exists and its shim is staged, so
	// each of these fails admission and marks the pod.
	tests := map[string]struct {
		pod          func(*testing.T) *corev1.Pod
		artifactsDir func(*testing.T) string
		sessionErr   error
		wantSessions int
	}{
		"no remote GPU server": {
			pod: func(t *testing.T) *corev1.Pod {
				pod := allocatingPod(t)
				pod.Annotations[util.PodPredicateNodeAnnotation] = testConsumerNode
				return pod
			},
			artifactsDir: func(t *testing.T) string { return stagedArtifacts(t, "12.9") },
		},
		"no client shim on the node": {
			pod:          func(t *testing.T) *corev1.Pod { return allocatingPod(t) },
			artifactsDir: func(t *testing.T) string { return t.TempDir() },
		},
		"the agent refuses the session": {
			pod:          func(t *testing.T) *corev1.Pod { return allocatingPod(t) },
			artifactsDir: func(t *testing.T) string { return stagedArtifacts(t, "12.9") },
			sessionErr:   assert.AnError,
			wantSessions: 1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pod := tt.pod(t)
			kubeClient := fake.NewClientset(pod, serverNode(t))
			plugin, sessions := newConsumerPlugin(t, kubeClient, tt.artifactsDir(t))
			sessions.err = tt.sessionErr

			_, err := allocateOne(t, plugin, 1)

			require.Error(t, err)
			assert.Contains(t, err.Error(), util.AllocateCheckErrMsg)
			assert.Len(t, sessions.calls, tt.wantSessions)
			got, getErr := kubeClient.CoreV1().Pods("ns").Get(context.Background(), pod.Name, metav1.GetOptions{})
			require.NoError(t, getErr)
			assert.Equal(t, string(util.AssignPhaseFailed), got.Labels[util.PodAssignedPhaseLabel])
		})
	}
}

// A server that has not told the agent its build version yet cannot be used:
// the client shim must not be newer than the server.
func TestConsumerAllocateWithoutServerVersion(t *testing.T) {
	node := serverNode(t)
	endpoints, err := remotegpu.ServerEndpointInfo{
		ServerEndpoint: "http://10.0.0.7:14833", AgentEndpoint: "grpc://10.0.0.7:14834",
	}.Encode()
	require.NoError(t, err)
	node.Annotations[util.NodeRemoteEndpointsAnnotation] = endpoints
	pod := allocatingPod(t)
	plugin, sessions := newConsumerPlugin(t, fake.NewClientset(pod, node), stagedArtifacts(t, "12.9"))

	_, err = allocateOne(t, plugin, 1)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CUDA version")
	assert.Empty(t, sessions.calls, "nothing is asked for before the shim is settled")
}

// The node annotation is only how the agent is found: where lupine-server is
// right now is what the agent answers, and that is what the container is told.
func TestConsumerAllocateUsesAgentReportedServer(t *testing.T) {
	pod := allocatingPod(t)
	plugin, sessions := newConsumerPlugin(t, fake.NewClientset(pod, serverNode(t)), stagedArtifacts(t, "12.9"))
	sessions.endpoint = "http://10.0.0.9:14999"

	resp, err := allocateOne(t, plugin, 1)

	require.NoError(t, err)
	require.Len(t, resp.ContainerResponses, 1)
	assert.Equal(t, sessions.endpoint, resp.ContainerResponses[0].Envs[kubeletremote.EnvLupineServer],
		"the address the agent reports wins over the one the node published")
}
