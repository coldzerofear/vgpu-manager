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
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes/fake"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// withRegistrar makes the role publications observable; it must come first,
// the role options publish through it.
func withRegistrar(reg registrar) Option {
	return func(p *Plugin) error {
		p.reg = reg
		return nil
	}
}

// gpuNode is a device manager with one GPU split into 10 slots.
func gpuNode(t *testing.T) *manager.DeviceManager {
	t.Helper()
	nodeConfig, err := node.NewNodeConfig(
		node.WithNodeNameOption(testServerNode),
		node.WithDeviceSplitCountOption(10),
		node.WithDeviceMemoryFactorOption(1),
		node.WithDeviceCoresScalingOption(1),
		node.WithDeviceMemoryScalingOption(1))
	require.NoError(t, err)
	return manager.NewFakeDeviceManager(
		manager.WithNodeConfigSpec(nodeConfig),
		manager.WithNvidiaVersion(nvidia.DriverVersion{CudaDriverVersion: nvidia.CudaDriverVersion(12020)}),
		manager.WithDevices([]*manager.Device{{
			GPU: &manager.GPUDevice{
				GpuInfo: &nvidia.GpuInfo{
					Index: 0, UUID: testGPUUUID, Minor: 0,
					Memory:      nvml.Memory{Total: 12288 << 20},
					ProductName: "Nvidia RTX 3080Ti",
				},
				Healthy: true,
			},
		}}))
}

// assertRole checks what a process publishes for one role. A role it runs is
// published while it runs and removed when it stops; a role it does not run is
// not touched at all -- that role may belong to another process on the node,
// and removing "roles this process does not run" deletes what that one
// publishes.
func assertRole(t *testing.T, reg *fakeRegistrar, name, label string, runs bool) {
	t.Helper()
	publish, published := reg.registry[name]
	cleanup, cleans := reg.cleanup[name]
	if !runs {
		assert.False(t, published, "%s is not this process's to publish or remove", name)
		assert.False(t, cleans, "%s is not this process's to clean up", name)
		return
	}
	require.True(t, published, "%s must be published", name)
	require.True(t, cleans, "%s must be removed when the process stops", name)
	metadata, err := publish(nil)
	require.NoError(t, err)
	value := metadata.Labels[label]
	require.NotNil(t, value, "%s must publish its label", name)
	assert.Equal(t, "true", *value)
	metadata, err = cleanup(nil)
	require.NoError(t, err)
	value, ok := metadata.Labels[label]
	assert.True(t, ok)
	assert.Nil(t, value, "%s must remove its label on the way out", name)
}

// Each role publishes its own label and offers its own slot count, and leaves
// the role it does not run alone.
func TestRoleMatrix(t *testing.T) {
	for _, test := range []struct {
		name           string
		server         bool
		consumer       bool
		wantServer     bool
		wantConsumer   bool
		wantSlots      int
		wantAllocateOK bool
	}{
		// The server offers the slots its own GPUs amount to: that is what
		// makes the scheduler see a vGPU node at all.
		{name: "server only", server: true, wantServer: true, wantSlots: 10},
		// A consumer has no GPUs of its own; it runs as many remote vGPUs as
		// configured, and it is the one that answers Allocate.
		{name: "consumer only", consumer: true, wantConsumer: true, wantSlots: 4, wantAllocateOK: true},
		// Both roles in one process: one resource registration, never fewer
		// slots than the node's own GPUs offer.
		{name: "both", server: true, consumer: true, wantServer: true, wantConsumer: true,
			wantSlots: 10, wantAllocateOK: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reg := newFakeRegistrar()
			devManager := gpuNode(t)
			if !test.server {
				nodeConfig, err := node.NewNodeConfig(node.WithNodeNameOption(testConsumerNode))
				require.NoError(t, err)
				devManager = manager.NewDevicelessManager(nodeConfig)
			}
			opts := []Option{withRegistrar(reg)}
			if test.server {
				opts = append(opts, func(p *Plugin) error {
					// The real option probes the agent; the role itself is
					// covered by TestServerRoleRefresh.
					setupServerRole(p.reg, &serverRole{notify: reg.RegisterNotify})
					p.publishDevices = true
					return nil
				})
			}
			if test.consumer {
				opts = append(opts, WithConsumerRole(fake.NewClientset(), ConsumerOptions{VGPUNumber: 4}))
			}

			plugin, err := New(Config{
				NodeName:     testServerNode,
				ResourceName: util.VGPUNumberResourceName,
				Socket:       filepath.Join(t.TempDir(), "remote.sock"),
			}, devManager, opts...)
			require.NoError(t, err)

			assertRole(t, reg, serverRoleName, util.NodeRemoteServerLabel, test.wantServer)
			assertRole(t, reg, consumerRoleName, util.NodeRemoteConsumerLabel, test.wantConsumer)
			assert.Len(t, plugin.Devices(), test.wantSlots)

			_, err = plugin.Allocate(context.Background(), &pluginapi.AllocateRequest{})
			if test.wantAllocateOK {
				// No pod is admitting here, so it fails -- but not by refusing
				// the role.
				assert.NotEqual(t, codes.FailedPrecondition, status.Code(err))
				return
			}
			assert.Equal(t, codes.FailedPrecondition, status.Code(err),
				"a node that only serves its GPUs must refuse to allocate them to its own pods")
		})
	}
}

func TestNewWithoutRole(t *testing.T) {
	nodeConfig, err := node.NewNodeConfig(node.WithNodeNameOption(testConsumerNode))
	require.NoError(t, err)
	_, err = New(Config{NodeName: testConsumerNode}, manager.NewDevicelessManager(nodeConfig))
	assert.Error(t, err, "a plugin with no role has nothing to serve")
}

// stubDevicePlugin answers just enough for a liveness probe.
type stubDevicePlugin struct {
	pluginapi.UnimplementedDevicePluginServer
}

func (stubDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// servePeer runs a device plugin on socket until the test ends.
func servePeer(t *testing.T, socket string) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(server, stubDevicePlugin{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
}

// kubelet keeps one endpoint per resource name, so a server that does not run
// remote pods must leave the resource to the process that does.
func TestStandDownToPeerConsumer(t *testing.T) {
	dir := t.TempDir()
	peer := filepath.Join(dir, ConsumerSocketName)
	newPlugin := func(t *testing.T, consumer bool) *Plugin {
		opts := []Option{withRegistrar(newFakeRegistrar()), func(p *Plugin) error {
			setupServerRole(p.reg, &serverRole{notify: func() {}})
			p.publishDevices = true
			return nil
		}}
		if consumer {
			opts = append(opts, WithConsumerRole(fake.NewClientset(), ConsumerOptions{VGPUNumber: 4}))
		}
		plugin, err := New(Config{
			NodeName:           testServerNode,
			ResourceName:       util.VGPUNumberResourceName,
			Socket:             filepath.Join(dir, ServerSocketName),
			PeerConsumerSocket: peer,
		}, gpuNode(t), opts...)
		require.NoError(t, err)
		return plugin
	}

	// A stale socket file of a process that crashed is not a peer.
	require.NoError(t, os.WriteFile(peer, nil, 0o644))
	assert.False(t, newPlugin(t, false).standDown(), "a socket nobody answers on is not a peer")
	require.NoError(t, os.Remove(peer))

	servePeer(t, peer)
	assert.True(t, newPlugin(t, false).standDown(), "the peer consumer owns the resource")
	assert.False(t, newPlugin(t, true).standDown(),
		"a plugin that serves the consumer role itself never stands down")
}
