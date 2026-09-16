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

package lister

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/component-base/featuregate"
)

// A remote pod's session directory stands in for the container directory it
// has none of: the lister reads the session's quota under the same key, and
// leaves the directory itself to the agent.
func TestContainerListerRemoteSessions(t *testing.T) {
	const nodeName = "gpu-node"
	const containerName = "app"

	sessionBase := t.TempDir()
	managerRoot := t.TempDir()

	gpuUUID := "GPU-" + string(uuid.NewUUID())
	nodeConfig, err := node.NewNodeConfig(
		node.WithNodeNameOption(nodeName),
		node.WithDeviceSplitCountOption(10),
		node.WithDeviceMemoryFactorOption(1),
		node.WithDeviceCoresScalingOption(1),
		node.WithDeviceMemoryScalingOption(1))
	require.NoError(t, err)
	featureGate := featuregate.NewFeatureGate()
	runtime.Must(featureGate.Add(map[featuregate.Feature]featuregate.FeatureSpec{
		util.SharedSMUtilizationWatcher: {Default: true, PreRelease: featuregate.Alpha},
		util.VirtualMemoryTracking:      {Default: true, PreRelease: featuregate.Alpha},
		util.DevicePluginClientMode:     {Default: true, PreRelease: featuregate.Alpha},
	}))
	devManager := manager.NewFakeDeviceManager(
		manager.WithNodeConfigSpec(nodeConfig),
		manager.WithFeatureGate(featureGate),
		manager.WithNvidiaVersion(nvidia.DriverVersion{CudaDriverVersion: nvidia.CudaDriverVersion(12020)}),
		manager.WithDevices([]*manager.Device{{
			GPU: &manager.GPUDevice{
				GpuInfo: &nvidia.GpuInfo{
					Index: 0, UUID: gpuUUID, Minor: 0,
					Memory:      nvml.Memory{Total: 12288 << 20},
					ProductName: "Nvidia RTX 3080Ti",
				},
				Healthy: true,
			},
		}}))

	// The pod runs on another node; this node's GPUs serve it.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       uuid.NewUUID(),
			Namespace: "default",
			Name:      "remote-pod",
			Annotations: map[string]string{
				util.VGPUAccessModeAnnotation:   util.AccessModeRemote,
				util.PodPredicateNodeAnnotation: nodeName,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "consumer-node",
			Containers: []corev1.Container{{Name: containerName}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kubeClient := fake.NewClientset(pod)
	factory := informers.NewSharedInformerFactory(kubeClient, 0)
	podLister := factory.Core().V1().Pods().Lister()
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// The session as the agent materializes it.
	token := remotegpu.SessionToken(string(pod.UID), containerName)
	quotaFile := remotegpu.SessionQuotaFile(sessionBase, token)
	require.NoError(t, os.MkdirAll(filepath.Dir(quotaFile), 0o755))
	claims := device.ContainerDeviceClaim{
		Name:         containerName,
		DeviceClaims: []device.DeviceClaim{{Id: 0, Uuid: gpuUUID, Cores: 20, Memory: 1024}},
	}
	require.NoError(t, vgpu.WriteVGPUConfigFile(quotaFile, devManager, pod, claims, false, &corev1.Node{}))

	contLister := NewContainerLister(nodeName, managerRoot, sessionBase, podLister)
	require.NoError(t, contLister.update())

	key := GetContainerKey(pod.UID, containerName)
	data, ok := contLister.GetResourceData(key)
	require.True(t, ok, "the session quota must be readable under the container key")
	snapshot := data.GetDeviceSnapshot(0)
	require.NotNil(t, snapshot)
	assert.Equal(t, gpuUUID, string(snapshot.UUID[0:40]))

	// The pod is gone: the mapping goes, the agent's directory stays.
	require.NoError(t, kubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
	require.Eventually(t, func() bool {
		_ = contLister.update()
		_, ok := contLister.GetResourceData(key)
		return !ok
	}, 2*time.Second, 50*time.Millisecond)
	assert.FileExists(t, quotaFile, "the session belongs to the agent, not to this lister")
}
