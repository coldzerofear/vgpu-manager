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

package remoteagent

import (
	"os"
	"path/filepath"
	"testing"

	vgpuconfig "github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

const (
	testGPU0 = "GPU-00000000-0000-0000-0000-000000000000"
	testGPU1 = "GPU-11111111-1111-1111-1111-111111111111"
)

// testServerNode is a GPU server node as the device plugin publishes it.
func testServerNode(t *testing.T, memoryScaling float64) *corev1.Node {
	t.Helper()
	devices := device.NodeDeviceInfo{
		{Id: 0, Uuid: testGPU0, Core: util.HundredCore, Memory: 12288, Type: "TestGPU", Number: 10, Healthy: true},
		{Id: 1, Uuid: testGPU1, Core: util.HundredCore, Memory: 24576, Type: "TestGPU", Number: 10, Healthy: true},
		{Id: 2, Uuid: "GPU-mig", Mig: true, Healthy: true},
	}
	registered, err := devices.Encode()
	require.NoError(t, err)
	config, err := device.NodeConfigInfo{DeviceSplit: 10, CoresScaling: 1, MemoryFactor: 1, MemoryScaling: memoryScaling}.Encode()
	require.NoError(t, err)
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNode,
			Labels: map[string]string{
				util.NodeNvidiaCudaVersionLabel:   "13.3.73",
				util.NodeNvidiaDriverVersionLabel: "580.65.06",
			},
			Annotations: map[string]string{
				util.NodeDeviceRegisterAnnotation: registered,
				util.NodeConfigInfoAnnotation:     config,
			},
		},
	}
}

// testRemotePod is a remote pod the scheduler pre-allocated devices to.
func testRemotePod(t *testing.T, claims device.PodDeviceClaim) *corev1.Pod {
	t.Helper()
	preAllocated, err := claims.MarshalText()
	require.NoError(t, err)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "remote-pod", Namespace: "ns", UID: k8stypes.UID("pod-uid"), ResourceVersion: "42",
			Annotations: map[string]string{
				util.VGPUAccessModeAnnotation:   util.AccessModeRemote,
				util.PodPredicateNodeAnnotation: testNode,
				util.PodVGPUPreAllocAnnotation:  preAllocated,
			},
		},
		Spec: corev1.PodSpec{NodeName: "consumer"},
	}
}

func TestNodeDevicesFromNode(t *testing.T) {
	nd, err := NodeDevicesFromNode(testServerNode(t, 2))
	require.NoError(t, err)

	assert.Equal(t, "13.3.73", nd.CudaVersionString())
	require.NotNil(t, nd.DriverVersion)
	assert.Equal(t, "580.65.06", nd.DriverVersion.Original())
	assert.Len(t, nd.Devices, 2, "MIG devices are not served remotely")
	assert.Equal(t, NodeDevice{
		Name: testGPU1, Minor: 1, UUID: testGPU1, MemoryMiB: 24576,
		Cores: util.HundredCore, MemoryRatio: 200,
	}, nd.Devices[testGPU1])

	if _, err := NodeDevicesFromNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNode}}); err == nil {
		t.Fatal("a node without a device registry must be an error")
	}
}

func TestPodSessionSpec(t *testing.T) {
	nd, err := NodeDevicesFromNode(testServerNode(t, 1))
	require.NoError(t, err)
	pod := testRemotePod(t, device.PodDeviceClaim{
		{Name: "init", DeviceClaims: []device.DeviceClaim{{Id: 1, Uuid: testGPU1, Cores: 100, Memory: 24576}}},
		{Name: "app", DeviceClaims: []device.DeviceClaim{
			{Id: 1, Uuid: testGPU1, Cores: 20, Memory: 2048},
			{Id: 0, Uuid: testGPU0, Cores: 50, Memory: 4096},
		}},
	})

	spec, err := PodSessionSpec(pod, "app", nd)
	require.NoError(t, err)

	assert.Equal(t, SessionOwner{
		Kind: OwnerPod, UID: "pod-uid", Namespace: "ns", Name: "remote-pod", Version: 42,
	}, spec.Owner)
	assert.Equal(t, float64(1), spec.MemoryRatio)
	assert.Equal(t, []device.DeviceClaim{
		{Id: 0, Uuid: testGPU0, Cores: 50, Memory: 4096},
		{Id: 1, Uuid: testGPU1, Cores: 20, Memory: 2048},
	}, spec.Claims, "claims are in slot order")
	assert.Equal(t, []device.DeviceClaim{
		{Id: 0, Uuid: testGPU0, Cores: util.HundredCore, Memory: 12288},
		{Id: 1, Uuid: testGPU1, Cores: util.HundredCore, Memory: 24576},
	}, spec.Infos, "infos carry the full device capacity")

	// Each container has its own session, so init keeps its own devices.
	initSpec, err := PodSessionSpec(pod, "init", nd)
	require.NoError(t, err)
	assert.Equal(t, []device.DeviceClaim{{Id: 1, Uuid: testGPU1, Cores: 100, Memory: 24576}}, initSpec.Claims)

	if _, err := PodSessionSpec(pod, "sidecar", nd); err == nil {
		t.Fatal("a container without pre-allocated devices must be an error")
	}
	unknown := testRemotePod(t, device.PodDeviceClaim{
		{Name: "app", DeviceClaims: []device.DeviceClaim{{Id: 7, Uuid: "GPU-elsewhere", Cores: 10, Memory: 1024}}},
	})
	if _, err := PodSessionSpec(unknown, "app", nd); err == nil {
		t.Fatal("a device of another node must be an error")
	}
}

func TestPodSessionTokens(t *testing.T) {
	pod := testRemotePod(t, device.PodDeviceClaim{
		{Name: "app", DeviceClaims: []device.DeviceClaim{{Id: 0, Uuid: testGPU0, Cores: 50, Memory: 4096}}},
		{Name: "no-device"},
	})

	assert.Equal(t, []string{remotegpu.SessionToken("pod-uid", "app")}, PodSessionTokens(pod))
	assert.Empty(t, PodSessionTokens(&corev1.Pod{}))
}

// A pod session writes the same files as a claim session and records the pod
// as its owner, so a sweep can tell the two apart.
func TestMaterializePodSession(t *testing.T) {
	base := t.TempDir()
	store := NewSessionStore(Config{NodeName: testNode, SessionBase: base, ContainerManagerDir: base})
	require.NoError(t, store.Prepare())
	nd, err := NodeDevicesFromNode(testServerNode(t, 1))
	require.NoError(t, err)
	pod := testRemotePod(t, device.PodDeviceClaim{
		{Name: "app", DeviceClaims: []device.DeviceClaim{{Id: 0, Uuid: testGPU0, Cores: 50, Memory: 4096}}},
	})
	spec, err := PodSessionSpec(pod, "app", nd)
	require.NoError(t, err)
	token := remotegpu.SessionToken("pod-uid", "app")

	require.NoError(t, store.Materialize(token, spec, nd, vgpuconfig.GetDefaultComputePolicy(pod, nil)))

	root := filepath.Join(base, token)
	for _, name := range []string{
		filepath.Join(util.Config, vgpu.VGPUConfigFileName), "pids.config",
		sessionLockDir, sessionVMemDir, sessionSMDir, sessionOwnerMarker,
	} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	data, err := vgpuconfig.NewMmapResourceData(filepath.Join(root, util.Config, vgpu.VGPUConfigFileName))
	require.NoError(t, err)
	defer func() { _ = data.Close() }()
	cfg := data.GetResource()
	assert.Equal(t, "remote-pod", string(trimZero(cfg.PodName[:])), "the session carries the pod's identity")
	assert.Equal(t, int32(util.SessionMode), cfg.CompatibilityMode)
	// Slot = host device index: the pod's GPU 0 lands in slot 0, slot 1 is idle.
	assert.Equal(t, testGPU0, string(trimZero(cfg.Devices[0].UUID[:])))
	assert.Equal(t, int32(1), cfg.Devices[0].Activate)
	assert.Equal(t, int32(0), cfg.Devices[1].Activate)

	owner, err := readMarker(filepath.Join(root, sessionOwnerMarker))
	require.NoError(t, err)
	assert.Equal(t, SessionOwner{Kind: OwnerPod, UID: "pod-uid", Version: 42}, owner)
	assert.Equal(t, []string{token}, store.TokensOfOwner("pod-uid"))

	// Idempotent: the library may already have state in the session.
	require.NoError(t, store.Materialize(token, spec, nd, vgpuconfig.GetDefaultComputePolicy(pod, nil)))

	// Another pod must not take over a live session.
	other := spec
	other.Owner.UID = "other-uid"
	assert.Error(t, store.Materialize(token, other, nd, vgpuconfig.GetDefaultComputePolicy(pod, nil)))
}
