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

package allocator

import (
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pciePod(number int64, strict bool) *corev1.Pod {
	mode := string(util.PCIeTopology)
	if strict {
		mode = string(util.PCIeTopologyStrict)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default",
			Annotations: map[string]string{util.DeviceTopologyModeAnnotation: mode}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{vgpuContainer("c", number, 100, 1024)}},
	}
}

// allocatePCIe runs the allocator and returns the chosen UUIDs plus the filter
// reason (nil when the node was accepted).
func allocatePCIe(t *testing.T, n *device.NodeInfo, pod *corev1.Pod) ([]string, *reason.FilterReason) {
	t.Helper()
	newPod, rsn, err := NewAllocator(n, nil).Allocate(BuildAllocationRequest(pod))
	require.NoError(t, err)
	if rsn != nil {
		return nil, rsn
	}
	pre, ok := util.HasAnnotation(newPod, util.PodVGPUPreAllocAnnotation)
	require.True(t, ok, "pre-allocated annotation missing")
	claim := device.PodDeviceClaim{}
	require.NoError(t, claim.UnmarshalText(pre))
	require.Len(t, claim, 1)
	out := make([]string, 0, len(claim[0].DeviceClaims))
	for _, dc := range claim[0].DeviceClaims {
		out = append(out, dc.Uuid)
	}
	return out, nil
}

// Test_PCIeTopology_AcceptanceFloor is the point of the mode: a node with no
// NVLink at all can still promise peer-to-peer DMA that does not cross a host
// bridge, and `pcie` is the only way to ask for it. `link` on the same node can
// only downgrade, and `numa` is too coarse to promise P2P.
func Test_PCIeTopology_AcceptanceFloor(t *testing.T) {
	t.Run("pcie-strict is satisfied by a switch-connected set", func(t *testing.T) {
		n, _ := pcieNode(t) // PIX pairs, PXB within a socket half, SYS across
		uuids, rsn := allocatePCIe(t, n, pciePod(4, true))
		require.Nil(t, rsn, "a socket half is switch-connected and must satisfy pcie-strict")
		require.Len(t, uuids, 4)
		// All four must come from one socket half; across halves is SYS, which
		// is below the switch tier.
		assert.True(t,
			assert.ObjectsAreEqual(uuidSet("GPU-0", "GPU-1", "GPU-2", "GPU-3"), uuidSet(uuids...)) ||
				assert.ObjectsAreEqual(uuidSet("GPU-4", "GPU-5", "GPU-6", "GPU-7"), uuidSet(uuids...)),
			"got %v, want one whole socket half", uuids)
	})

	// The same node, the same request, the stricter mode: this is what makes
	// the floor real rather than cosmetic.
	t.Run("link-strict rejects the node pcie-strict accepts", func(t *testing.T) {
		n, _ := pcieNode(t)
		_, rsn := allocatePCIe(t, n, linkPod(4, true, ""))
		require.NotNil(t, rsn, "a node with no NVLink must not satisfy link-strict")
		assert.Equal(t, reason.LinkTopologyUnsatisfied, rsn.Primary)
	})

	// Above the floor is still acceptable: the tiers nest, so an NVLink set
	// satisfies pcie too. Refusing it would make pcie unusable on the very
	// nodes with the best interconnect.
	t.Run("pcie-strict accepts an NVLink set", func(t *testing.T) {
		n, _ := nvswitchNode(t)
		uuids, rsn := allocatePCIe(t, n, pciePod(4, true))
		require.Nil(t, rsn)
		assert.Len(t, uuids, 4)
	})

	// Below the floor, strict rejects and non-strict places anyway.
	t.Run("pcie-strict rejects a node with no P2P at all", func(t *testing.T) {
		n := fixtureNode("linkless", topoDevices(4)) // topology published, zero edges
		_, rsn := allocatePCIe(t, n, pciePod(2, true))
		require.NotNil(t, rsn)
		assert.Equal(t, reason.PCIeTopologyUnsatisfied, rsn.Primary)
	})

	t.Run("plain pcie falls back instead of rejecting", func(t *testing.T) {
		n := fixtureNode("linkless", topoDevices(4))
		uuids, rsn := allocatePCIe(t, n, pciePod(2, false))
		require.Nil(t, rsn, "non-strict must still place the pod")
		assert.Len(t, uuids, 2)
	})
}

// Test_PCIeTopology_LeavesLinkModeAlone guards the NVLink path against the
// floor parameterisation: link-strict must still demand NVLink specifically,
// not merely "at or above the switch tier".
func Test_PCIeTopology_LeavesLinkModeAlone(t *testing.T) {
	n, _ := nvswitchNode(t)
	uuids, rsn := allocatePCIe(t, n, linkPod(4, true, ""))
	require.Nil(t, rsn, "an NVSwitch node must still satisfy link-strict")
	assert.Len(t, uuids, 4)

	// And the bridge node, where only adjacent pairs have NVLink: a 4-GPU
	// link-strict request cannot be met even though every socket half is
	// switch-connected.
	bridge, _ := bridgeNode(t)
	_, rsn = allocatePCIe(t, bridge, linkPod(4, true, ""))
	require.NotNil(t, rsn, "switch connectivity must not satisfy link-strict")
	assert.Equal(t, reason.LinkTopologyUnsatisfied, rsn.Primary)

	// The same node and request in pcie-strict succeeds, which is exactly the
	// gap the new mode fills.
	uuids, rsn = allocatePCIe(t, bridge, pciePod(4, true))
	require.Nil(t, rsn)
	assert.Len(t, uuids, 4)
}

func uuidSet(uuids ...string) map[string]bool {
	out := make(map[string]bool, len(uuids))
	for _, u := range uuids {
		out[u] = true
	}
	return out
}
