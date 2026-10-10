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
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	vgpuconfig "github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func nodeWithPolicy(t *testing.T, policy util.ComputePolicy) *corev1.Node {
	t.Helper()
	node := testServerNode(t, 1)
	node.Annotations[util.VGPUComputePolicyAnnotation] = string(policy)
	return node
}

func TestSessionStoreNodeObject(t *testing.T) {
	store := NewSessionStore(Config{NodeName: testNode})

	// Nothing wired: the owner's informer may not be running (or, in a unit
	// test, may not exist), and node-level defaults are simply skipped.
	require.True(t, store.nodeObject() == nil, "an unwired accessor must yield a nil interface")

	// A lister miss hands back a typed nil pointer; it must not travel on as a
	// non-nil metav1.Object, which panics in the annotation lookup.
	store.GetNodeFn = func() (*corev1.Node, error) {
		return nil, apierrors.NewNotFound(corev1.Resource("nodes"), testNode)
	}
	require.True(t, store.nodeObject() == nil, "a typed nil node must yield a nil interface")

	store.GetNodeFn = func() (*corev1.Node, error) { return nodeWithPolicy(t, util.BalanceComputePolicy), nil }
	got := store.nodeObject()
	require.NotNil(t, got)
	assert.Equal(t, testNode, got.GetName())
}

// The compute policy of a pod session: the pod's own annotation first, the
// node's as the default, and fixed when neither says anything - including when
// the node cannot be read at all.
func TestEnsurePodSessionComputePolicy(t *testing.T) {
	const claimedCores = 50 // podClaims gives the app container half of GPU 0

	tests := map[string]struct {
		podPolicy     util.ComputePolicy
		nodeFn        func() (*corev1.Node, error)
		wantSoftCore  int32
		wantCoreLimit int32
		wantHardLimit int32
	}{
		"no accessor falls back to fixed": {
			wantSoftCore: claimedCores, wantCoreLimit: 1, wantHardLimit: 1,
		},
		"unreadable node falls back to fixed": {
			nodeFn:       func() (*corev1.Node, error) { return nil, errors.New("node cache miss") },
			wantSoftCore: claimedCores, wantCoreLimit: 1, wantHardLimit: 1,
		},
		"node default applies": {
			nodeFn:       func() (*corev1.Node, error) { return nodeWithPolicy(t, util.BalanceComputePolicy), nil },
			wantSoftCore: util.HundredCore, wantCoreLimit: 1, wantHardLimit: 0,
		},
		"pod annotation wins over the node": {
			podPolicy:    util.NoneComputePolicy,
			nodeFn:       func() (*corev1.Node, error) { return nodeWithPolicy(t, util.BalanceComputePolicy), nil },
			wantSoftCore: claimedCores, wantCoreLimit: 0, wantHardLimit: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			pod := testRemotePod(t, podClaims())
			if tc.podPolicy != "" {
				pod.Annotations[util.VGPUComputePolicyAnnotation] = string(tc.podPolicy)
			}
			a := newPodModeAgent(t, pod)
			a.store.GetNodeFn = tc.nodeFn

			token := remotegpu.SessionToken(string(pod.UID), "app")
			require.NoError(t, a.ensurePodSession(context.Background(), &remoteagent.EnsureSessionRequest{
				Session: token, ClaimUid: string(pod.UID),
				ClaimNamespace: pod.Namespace, ClaimName: pod.Name,
			}, a.podDevices.Load()))

			data, err := vgpuconfig.NewMmapResourceData(
				filepath.Join(a.cfg.SessionBase, token, util.Config, vgpu.VGPUConfigFileName))
			require.NoError(t, err)
			defer func() { _ = data.Close() }()

			gpu0 := data.GetResource().Devices[0]
			assert.Equal(t, tc.wantSoftCore, gpu0.SoftCore)
			assert.Equal(t, tc.wantCoreLimit, gpu0.CoreLimit)
			assert.Equal(t, tc.wantHardLimit, gpu0.HardLimit)
		})
	}
}
