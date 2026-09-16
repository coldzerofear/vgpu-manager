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
	"encoding/json"
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
func allocatingPod(t *testing.T) *corev1.Pod {
	t.Helper()
	preAllocated, err := device.PodDeviceClaim{
		{Name: "cont1", DeviceClaims: []device.DeviceClaim{{Id: 0, Uuid: testGPUUUID, Cores: 50, Memory: 4096}}},
	}.MarshalText()
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
		Spec: corev1.PodSpec{
			NodeName: testConsumerNode,
			Containers: []corev1.Container{{
				Name: "cont1",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("1"),
				}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func newConsumerPlugin(t *testing.T, kubeClient kubernetes.Interface) (*consumerDevicePlugin, string) {
	t.Helper()
	managerDir := t.TempDir()
	// A consumer node has no GPUs, so its manager has no devices either.
	nodeConfig, err := node.NewNodeConfig(node.WithNodeNameOption(testConsumerNode))
	require.NoError(t, err)
	devManager := manager.NewDevicelessManager(nodeConfig)
	plugin := NewConsumerDevicePlugin(ConsumerConfig{
		NodeName: testConsumerNode, ResourceName: util.VGPUNumberResourceName,
		Socket: filepath.Join(t.TempDir(), "remote.sock"), VGPUNumber: 4,
		ManagerDir: managerDir, HostManagerDir: "/host/vgpu-manager",
	}, devManager, kubeClient)
	consumer := plugin.(*consumerDevicePlugin)
	// No agent to ask in a test; the cases that care override this.
	consumer.ensureSession = func(context.Context, string, remotegpu.PodSession) (string, error) {
		return "", nil
	}
	return consumer, managerDir
}

func TestConsumerDevices(t *testing.T) {
	plugin, _ := newConsumerPlugin(t, fake.NewClientset())

	devices := plugin.Devices()

	require.Len(t, devices, 4, "the node serves as many remote vGPUs as configured")
	assert.Equal(t, "remote-vgpu-0", devices[0].ID)
	for _, dev := range devices {
		assert.Equal(t, pluginapi.Healthy, dev.Health, "slots are not hardware; they never turn unhealthy")
	}
	options, err := plugin.GetDevicePluginOptions(context.Background(), &pluginapi.Empty{})
	require.NoError(t, err)
	assert.True(t, options.PreStartRequired, "the session is created in PreStartContainer")
}

func TestConsumerAllocate(t *testing.T) {
	pod := allocatingPod(t)
	kubeClient := fake.NewClientset(pod, serverNode(t))
	plugin, managerDir := newConsumerPlugin(t, kubeClient)

	resp, err := plugin.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"remote-vgpu-0"}}},
	})

	require.NoError(t, err)
	require.Len(t, resp.ContainerResponses, 1)
	container := resp.ContainerResponses[0]

	// The container is told where its GPUs are and which session they run in.
	assert.Equal(t, "http://10.0.0.7:14833", container.Envs[kubeletremote.EnvLupineServer])
	assert.Equal(t, remotegpu.SessionToken("pod-uid", "cont1"), container.Envs[kubeletremote.EnvLupineSession])
	assert.Equal(t, "1", container.Envs[kubeletremote.EnvLupineDisableLocal])
	assert.Equal(t, "void", container.Envs["NVIDIA_VISIBLE_DEVICES"])
	assert.Equal(t, "cont1", container.Envs[util.ContNameEnv])

	// The client shim and its preload list are mounted from the container's
	// own directory; PreStartContainer fills them in before the container starts.
	require.Len(t, container.Mounts, 2)
	assert.Equal(t, &pluginapi.Mount{
		ContainerPath: filepath.Join(util.ManagerRootPath, util.Driver),
		HostPath:      "/host/vgpu-manager/pod-uid_cont1/driver",
		ReadOnly:      true,
	}, container.Mounts[0])
	assert.Equal(t, &pluginapi.Mount{
		ContainerPath: vgpu.ContPreLoadFilePath,
		HostPath:      "/host/vgpu-manager/pod-uid_cont1/ld.so.preload",
		ReadOnly:      true,
	}, container.Mounts[1])
	assert.Empty(t, container.Devices, "no local device is injected")

	// PreStartContainer identifies its container by these ids.
	var deviceIDs []string
	data, err := os.ReadFile(filepath.Join(managerDir, "pod-uid_cont1", deviceListFileName))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &deviceIDs))
	assert.Equal(t, []string{"remote-vgpu-0"}, deviceIDs)

	// The allocation is recorded on the pod, with the server still reporting it.
	got, err := kubeClient.CoreV1().Pods("ns").Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, got.Annotations[util.PodVGPURealAllocAnnotation], testGPUUUID)
	assert.Equal(t, string(util.AssignPhaseSucceed), got.Labels[util.PodAssignedPhaseLabel])
	assert.Equal(t, testServerNode, got.Labels[util.PodMetricsNodeLabel])
}

func TestConsumerAllocateRejectsUnservedPod(t *testing.T) {
	// A pod whose predicate node is this node is not a remote pod.
	pod := allocatingPod(t)
	pod.Annotations[util.PodPredicateNodeAnnotation] = testConsumerNode
	kubeClient := fake.NewClientset(pod, serverNode(t))
	plugin, _ := newConsumerPlugin(t, kubeClient)

	_, err := plugin.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"remote-vgpu-0"}}},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), util.AllocateCheckErrMsg)
	got, getErr := kubeClient.CoreV1().Pods("ns").Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, string(util.AssignPhaseFailed), got.Labels[util.PodAssignedPhaseLabel])
}

func TestConsumerPreStartContainerNeedsDeviceIDs(t *testing.T) {
	plugin, _ := newConsumerPlugin(t, fake.NewClientset())

	_, err := plugin.PreStartContainer(context.Background(), &pluginapi.PreStartContainerRequest{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), util.PreStartContainerCheckErrMsg)
}
