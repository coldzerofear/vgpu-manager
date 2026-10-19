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

package common

import (
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

func TestBuildResourceClaimTopologyConstraint(t *testing.T) {
	podWith := func(mode util.TopologyMode, remoteAccess bool) *corev1.Pod {
		annotations := map[string]string{util.DeviceTopologyModeAnnotation: string(mode)}
		if remoteAccess {
			annotations[util.VGPUAccessModeAnnotation] = util.AccessModeRemote
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Annotations: annotations}}
	}
	// A multi-device request is what triggers topology constraints at all.
	requests := func(count int64) []resourceapi.DeviceRequest {
		return []resourceapi.DeviceRequest{{
			Name: "gpus",
			Exactly: &resourceapi.ExactDeviceRequest{
				DeviceClassName: "vgpu-manager",
				AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
				Count:           count,
			},
		}}
	}
	build := func(mode util.TopologyMode, count int64) *resourceapi.ResourceClaim {
		return BuildResourceClaim(podWith(mode, false), requests(count), "c", "owner", "1")
	}
	// matchAttributes renders every matchAttribute constraint as
	// "<requests>=<attribute>", so a test states which request or subrequest
	// each attribute binds.
	matchAttributes := func(claim *resourceapi.ResourceClaim) []string {
		var got []string
		for _, c := range claim.Spec.Devices.Constraints {
			if c.MatchAttribute != nil {
				got = append(got, strings.Join(c.Requests, ",")+"="+string(*c.MatchAttribute))
			}
		}
		return got
	}
	subRequests := func(claim *resourceapi.ResourceClaim) []string {
		var got []string
		for _, sub := range claim.Spec.Devices.Requests[0].FirstAvailable {
			got = append(got, sub.Name)
		}
		return got
	}

	const (
		nvlinkDomain = util.DRADriverName + "/nvlinkDomain"
		pcieDomain   = util.DRADriverName + "/pcieDomain"
		numaNode     = string(deviceattribute.StandardDeviceAttributeNUMANode)
		nodeName     = util.DRADriverName + "/nodeName"
	)

	// strict keeps the request exact and pins it to the mode's own tier, so a
	// node that cannot offer it is refused.
	for _, tc := range []struct {
		mode util.TopologyMode
		want string
	}{
		{util.LinkTopologyStrict, nvlinkDomain},
		{util.PCIeTopologyStrict, pcieDomain},
		{util.NUMATopologyStrict, numaNode},
	} {
		t.Run(string(tc.mode)+" stays exact on its own tier", func(t *testing.T) {
			claim := build(tc.mode, 2)
			require.NotNil(t, claim.Spec.Devices.Requests[0].Exactly)
			assert.Empty(t, claim.Spec.Devices.Requests[0].FirstAvailable)
			assert.Equal(t, []string{"gpus=" + tc.want}, matchAttributes(claim))
		})
	}

	// Non-strict walks the extender's ladder tightest first and always ends
	// in an unconstrained subrequest, so the pod stays schedulable.
	for _, tc := range []struct {
		mode      util.TopologyMode
		wantSubs  []string
		wantMatch []string
	}{
		{util.LinkTopology,
			[]string{"nvlink", "pcie", "numa", "any"},
			[]string{"gpus/nvlink=" + nvlinkDomain, "gpus/pcie=" + pcieDomain, "gpus/numa=" + numaNode}},
		{util.PCIeTopology,
			[]string{"pcie", "numa", "any"},
			[]string{"gpus/pcie=" + pcieDomain, "gpus/numa=" + numaNode}},
		{util.NUMATopology,
			[]string{"numa", "any"},
			[]string{"gpus/numa=" + numaNode}},
	} {
		t.Run(string(tc.mode)+" degrades tier by tier", func(t *testing.T) {
			claim := build(tc.mode, 2)
			request := claim.Spec.Devices.Requests[0]
			require.Nil(t, request.Exactly, "a request is exact or firstAvailable, never both")
			assert.Equal(t, tc.wantSubs, subRequests(claim))
			assert.Equal(t, tc.wantMatch, matchAttributes(claim))
			for _, sub := range request.FirstAvailable {
				assert.Equal(t, int64(2), sub.Count)
				assert.Equal(t, "vgpu-manager", sub.DeviceClassName)
			}
		})
	}

	// Without a topology mode there is nothing to fall back between.
	for _, mode := range []util.TopologyMode{util.NoneTopology, "", "bogus"} {
		t.Run("no topology mode "+string(mode)+" stays exact and unconstrained", func(t *testing.T) {
			claim := build(mode, 2)
			require.NotNil(t, claim.Spec.Devices.Requests[0].Exactly)
			assert.Empty(t, claim.Spec.Devices.Requests[0].FirstAvailable)
			assert.Empty(t, matchAttributes(claim))
		})
	}

	t.Run("a single-device request is left alone", func(t *testing.T) {
		claim := build(util.LinkTopology, 1)
		require.NotNil(t, claim.Spec.Devices.Requests[0].Exactly)
		assert.Empty(t, claim.Spec.Devices.Constraints)
	})

	// Every multi-device request keeps its distinct-uuid constraint on the
	// main request, which covers every subrequest too.
	t.Run("multi-device requests stay mutually exclusive by uuid", func(t *testing.T) {
		for _, mode := range []util.TopologyMode{util.NoneTopology, util.LinkTopology, util.LinkTopologyStrict} {
			var distinct []string
			for _, c := range build(mode, 2).Spec.Devices.Constraints {
				if c.DistinctAttribute != nil {
					distinct = append(distinct, strings.Join(c.Requests, ",")+"="+string(*c.DistinctAttribute))
				}
			}
			assert.Equal(t, []string{"gpus=" + util.DRADriverName + "/uuid"}, distinct, "mode %q", mode)
		}
	})

	// Remote devices: a topology mode keeps one container on one server, since
	// topology is not defined across nodes; without one a container may span
	// servers.
	t.Run("remote with a topology mode pins the request to one node", func(t *testing.T) {
		for _, mode := range []util.TopologyMode{util.NUMATopology, util.NUMATopologyStrict} {
			claim := BuildResourceClaim(podWith(mode, true), requests(2), "c", "owner", "1")
			assert.Contains(t, matchAttributes(claim), "gpus="+nodeName, "mode %q", mode)
		}
	})

	t.Run("remote without a topology mode may span nodes", func(t *testing.T) {
		claim := BuildResourceClaim(podWith(util.NoneTopology, true), requests(2), "c", "owner", "1")
		assert.Empty(t, matchAttributes(claim))
	})

	t.Run("local pods are never pinned by node name", func(t *testing.T) {
		assert.NotContains(t, matchAttributes(build(util.NUMATopology, 2)), "gpus="+nodeName)
	})

	// A subrequest has no adminAccess field, so converting would drop it.
	t.Run("an admin-access request is not converted", func(t *testing.T) {
		reqs := requests(2)
		reqs[0].Exactly.AdminAccess = ptr.To(true)
		claim := BuildResourceClaim(podWith(util.LinkTopology, false), reqs, "c", "owner", "1")
		require.NotNil(t, claim.Spec.Devices.Requests[0].Exactly)
		assert.Equal(t, []string{"gpus=" + nvlinkDomain}, matchAttributes(claim))
	})
}
