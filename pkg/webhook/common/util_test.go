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
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
)

func TestBuildResourceClaimTopologyConstraint(t *testing.T) {
	podWithTopology := func(mode util.TopologyMode) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Annotations: map[string]string{util.DeviceTopologyModeAnnotation: string(mode)},
		}}
	}
	// A multi-device request is what triggers topology constraints at all.
	requests := func(count int64) []resourceapi.DeviceRequest {
		return []resourceapi.DeviceRequest{{
			Name: "gpus",
			Exactly: &resourceapi.ExactDeviceRequest{
				AllocationMode: resourceapi.DeviceAllocationModeExactCount,
				Count:          count,
			},
		}}
	}
	matchAttributes := func(claim *resourceapi.ResourceClaim) []resourceapi.FullyQualifiedName {
		var got []resourceapi.FullyQualifiedName
		for _, c := range claim.Spec.Devices.Constraints {
			if c.MatchAttribute != nil {
				got = append(got, *c.MatchAttribute)
			}
		}
		return got
	}
	nvlinkDomain := resourceapi.FullyQualifiedName(util.DRADriverName + "/nvlinkDomain")

	// link means NVLink connectivity. It used to match on pcieRoot, which is
	// PCIe locality: it rejects NVLink peers under different roots and accepts
	// PCIe-only GPUs with no NVLink at all.
	t.Run("link matches on nvlinkDomain", func(t *testing.T) {
		claim := BuildResourceClaim(podWithTopology(util.LinkTopology), requests(2), "c", "owner", "1")
		require.Equal(t, []resourceapi.FullyQualifiedName{nvlinkDomain}, matchAttributes(claim))
	})

	t.Run("link-strict resolves to the same attribute", func(t *testing.T) {
		claim := BuildResourceClaim(podWithTopology(util.LinkTopologyStrict), requests(2), "c", "owner", "1")
		require.Equal(t, []resourceapi.FullyQualifiedName{nvlinkDomain}, matchAttributes(claim))
	})

	// pcie means peer-to-peer DMA without crossing a host bridge, which the
	// standard pcieRoot attribute does not express: a root complex also pairs
	// GPUs that have to cross the bridge to talk.
	pcieDomain := resourceapi.FullyQualifiedName(util.DRADriverName + "/pcieDomain")

	t.Run("pcie matches on pcieDomain", func(t *testing.T) {
		claim := BuildResourceClaim(podWithTopology(util.PCIeTopology), requests(2), "c", "owner", "1")
		require.Equal(t, []resourceapi.FullyQualifiedName{pcieDomain}, matchAttributes(claim))
	})

	t.Run("pcie-strict resolves to the same attribute", func(t *testing.T) {
		claim := BuildResourceClaim(podWithTopology(util.PCIeTopologyStrict), requests(2), "c", "owner", "1")
		require.Equal(t, []resourceapi.FullyQualifiedName{pcieDomain}, matchAttributes(claim))
	})

	// numa is a separate axis and must keep its own attribute.
	t.Run("numa still matches on the NUMA node attribute", func(t *testing.T) {
		claim := BuildResourceClaim(podWithTopology(util.NUMATopology), requests(2), "c", "owner", "1")
		require.Equal(t, []resourceapi.FullyQualifiedName{
			resourceapi.FullyQualifiedName(deviceattribute.StandardDeviceAttributeNUMANode),
		}, matchAttributes(claim))
	})

	t.Run("a single-device request gets no topology constraint", func(t *testing.T) {
		claim := BuildResourceClaim(podWithTopology(util.LinkTopology), requests(1), "c", "owner", "1")
		assert.Empty(t, matchAttributes(claim))
	})

	// Every multi-device request keeps its distinct-uuid constraint, so two
	// devices in one request can never be the same physical GPU.
	t.Run("multi-device requests stay mutually exclusive by uuid", func(t *testing.T) {
		claim := BuildResourceClaim(podWithTopology(util.NoneTopology), requests(2), "c", "owner", "1")
		require.Len(t, claim.Spec.Devices.Constraints, 1)
		require.NotNil(t, claim.Spec.Devices.Constraints[0].DistinctAttribute)
		assert.Equal(t, resourceapi.FullyQualifiedName(util.DRADriverName+"/uuid"),
			*claim.Spec.Devices.Constraints[0].DistinctAttribute)
	})
}
