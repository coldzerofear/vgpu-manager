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
	"fmt"
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
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

	t.Run("a non-strict single-device request is left alone", func(t *testing.T) {
		for _, mode := range []util.TopologyMode{util.LinkTopology, util.PCIeTopology, util.NUMATopology, util.NoneTopology} {
			claim := build(mode, 1)
			require.NotNil(t, claim.Spec.Devices.Requests[0].Exactly)
			assert.Empty(t, claim.Spec.Devices.Constraints, "mode %q", mode)
		}
	})

	// A strict single card must still sit on its tier, as on the extender
	// path: the attribute is published only for a card with a peer at that
	// level (or a NUMA node), so a card without one is refused.
	for _, tc := range []struct {
		mode util.TopologyMode
		want string
	}{
		{util.LinkTopologyStrict, nvlinkDomain},
		{util.PCIeTopologyStrict, pcieDomain},
		{util.NUMATopologyStrict, numaNode},
	} {
		t.Run(string(tc.mode)+" single-device request must sit on its tier", func(t *testing.T) {
			claim := build(tc.mode, 1)
			require.NotNil(t, claim.Spec.Devices.Requests[0].Exactly)
			require.Len(t, claim.Spec.Devices.Constraints, 1, "no uuid exclusivity for one device")
			assert.Equal(t, []string{"gpus=" + tc.want}, matchAttributes(claim))
		})
	}

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

	// Admin access has no placement to steer, and a subrequest could not
	// carry the field, so such a request gets no topology at all.
	t.Run("an admin-access request is left without topology", func(t *testing.T) {
		for _, mode := range []util.TopologyMode{util.LinkTopology, util.LinkTopologyStrict} {
			for _, count := range []int64{1, 2} {
				reqs := requests(count)
				reqs[0].Exactly.AdminAccess = ptr.To(true)
				claim := BuildResourceClaim(podWith(mode, true), reqs, "c", "owner", "1")
				require.NotNil(t, claim.Spec.Devices.Requests[0].Exactly, "mode %q count %d", mode, count)
				assert.Empty(t, matchAttributes(claim), "mode %q count %d", mode, count)
			}
		}
	})

	// The node pin sits on the main request, so it binds every tier of the
	// ladder, the unconstrained "any" included.
	t.Run("remote non-strict link pins the main request, not a tier", func(t *testing.T) {
		claim := BuildResourceClaim(podWith(util.LinkTopology, true), requests(2), "c", "owner", "1")
		require.Nil(t, claim.Spec.Devices.Requests[0].Exactly)
		assert.Equal(t, []string{
			"gpus=" + nodeName,
			"gpus/nvlink=" + nvlinkDomain, "gpus/pcie=" + pcieDomain, "gpus/numa=" + numaNode,
		}, matchAttributes(claim))
	})
}

// The boundary is stated, not worked around: a combined claim either fits the
// API server's constraint limit with every tier intact, or it is refused.
func TestCheckDeviceConstraintLimit(t *testing.T) {
	claimFor := func(mode util.TopologyMode, remoteAccess bool, containers int) *resourceapi.ResourceClaim {
		annotations := map[string]string{util.DeviceTopologyModeAnnotation: string(mode)}
		if remoteAccess {
			annotations[util.VGPUAccessModeAnnotation] = util.AccessModeRemote
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Annotations: annotations}}
		reqs := make([]resourceapi.DeviceRequest, containers)
		for i := range reqs {
			reqs[i] = resourceapi.DeviceRequest{
				Name: fmt.Sprintf("c%d", i),
				Exactly: &resourceapi.ExactDeviceRequest{
					DeviceClassName: "vgpu-manager",
					AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
					Count:           2,
				},
			}
		}
		return BuildResourceClaim(pod, reqs, "c", "owner", "1")
	}

	for _, tc := range []struct {
		name          string
		mode          util.TopologyMode
		remote        bool
		maxContainers int
	}{
		{"non-strict link", util.LinkTopology, false, 8},
		{"non-strict link, remote", util.LinkTopology, true, 6},
		{"non-strict numa", util.NUMATopology, false, 16},
		{"strict link", util.LinkTopologyStrict, false, 16},
		{"strict link, remote", util.LinkTopologyStrict, true, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := field.NewPath("spec").Child("containers")
			fits := claimFor(tc.mode, tc.remote, tc.maxContainers)
			assert.Empty(t, CheckDeviceConstraintLimit(&fits.Spec, path),
				"%d multi-GPU containers must fit", tc.maxContainers)

			over := claimFor(tc.mode, tc.remote, tc.maxContainers+1)
			errs := CheckDeviceConstraintLimit(&over.Spec, path)
			require.Len(t, errs, 1, "%d multi-GPU containers must be refused", tc.maxContainers+1)
			assert.Contains(t, errs[0].Detail, "--combined-resource-claim")
			assert.Equal(t, "spec.containers", errs[0].Field)
		})
	}
}
