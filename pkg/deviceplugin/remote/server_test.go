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
	"errors"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeRegistrar struct {
	registry, cleanup map[string]manager.RegistryFunc
	notified          int
}

func newFakeRegistrar() *fakeRegistrar {
	return &fakeRegistrar{registry: map[string]manager.RegistryFunc{}, cleanup: map[string]manager.RegistryFunc{}}
}

func (f *fakeRegistrar) AddRegistryFunc(name string, fn manager.RegistryFunc) { f.registry[name] = fn }
func (f *fakeRegistrar) AddCleanupRegistryFunc(name string, fn manager.RegistryFunc) {
	f.cleanup[name] = fn
}
func (f *fakeRegistrar) RegisterNotify() { f.notified++ }

// published returns the role label and endpoints a registry function writes;
// nil means the key is removed.
func published(t *testing.T, fn manager.RegistryFunc) (label, endpoints *string) {
	t.Helper()
	require.NotNil(t, fn)
	metadata, err := fn(nil)
	require.NoError(t, err)
	label, ok := metadata.Labels[util.NodeRemoteServerLabel]
	require.True(t, ok, "the role label must always be written")
	endpoints, ok = metadata.Annotations[util.NodeRemoteEndpointsAnnotation]
	require.True(t, ok, "the endpoints annotation must always be written")
	return label, endpoints
}

// The local plugin clears whatever remote role a gone remote process left on
// the node: both labels, and the server's endpoints.
func TestRemoveRoles(t *testing.T) {
	reg := newFakeRegistrar()

	removeRoles(reg)

	label, endpoints := published(t, reg.registry[serverRoleName])
	assert.Nil(t, label, "a stale server label is removed")
	assert.Nil(t, endpoints, "stale endpoints are removed")

	require.NotNil(t, reg.registry[consumerRoleName])
	metadata, err := reg.registry[consumerRoleName](nil)
	require.NoError(t, err)
	consumer, ok := metadata.Labels[util.NodeRemoteConsumerLabel]
	require.True(t, ok, "the consumer label must be written, as a removal")
	assert.Nil(t, consumer, "a stale consumer label is removed")

	assert.Empty(t, reg.cleanup, "there is nothing of its own for the local side to clean up")
}

func TestNewServerRoleBadAgentEndpoint(t *testing.T) {
	reg := newFakeRegistrar()

	role, err := newServerRole(context.Background(), reg, fake.NewClientset().CoreV1().Nodes(), "gpu-node", "ftp://x")

	assert.Error(t, err)
	assert.Nil(t, role)
	// A role that could not be set up publishes nothing -- the plugin fails to
	// build, so the process exits and nothing of this role is advertised.
	assert.NotContains(t, reg.registry, serverRoleName)
}

func TestServerRoleRefresh(t *testing.T) {
	reg := newFakeRegistrar()
	var info *remotegpu.ServerEndpointInfo
	var probeErr error
	role := &serverRole{
		probe:     func(context.Context) (*remotegpu.ServerEndpointInfo, error) { return info, probeErr },
		notify:    reg.RegisterNotify,
		endpoints: remotegpu.UnreachableServerEndpointInfo,
	}
	endpoints := func() string {
		label, value := published(t, role.registry)
		require.NotNil(t, label)
		require.Equal(t, "true", *label)
		require.NotNil(t, value)
		return *value
	}
	ctx := context.Background()

	// Before the first answer the node is a server that takes no remote pods.
	assert.Equal(t, remotegpu.UnreachableServerEndpointInfo, endpoints())

	info = &remotegpu.ServerEndpointInfo{ServerEndpoint: "http://10.0.0.7:14833", AgentEndpoint: "grpc://10.0.0.7:14834", ServerCUDAVersion: "13.3.73"}
	role.refresh(ctx)
	got, err := remotegpu.DecodeServerEndpointInfo(endpoints())
	require.NoError(t, err)
	assert.Equal(t, *info, *got)
	assert.Equal(t, 1, reg.notified, "a change is republished right away")

	role.refresh(ctx)
	assert.Equal(t, 1, reg.notified, "the same answer is not republished")

	probeErr = errors.New("agent down")
	role.refresh(ctx)
	assert.Equal(t, remotegpu.UnreachableServerEndpointInfo, endpoints())
	assert.Equal(t, 2, reg.notified)

	// An answer that cannot be published counts as unreachable too.
	info, probeErr = &remotegpu.ServerEndpointInfo{ServerEndpoint: "http://127.0.0.1:14833", AgentEndpoint: "grpc://10.0.0.7:14834"}, nil
	role.refresh(ctx)
	assert.Equal(t, remotegpu.UnreachableServerEndpointInfo, endpoints())
	assert.Equal(t, 2, reg.notified)
}
