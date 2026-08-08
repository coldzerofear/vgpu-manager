/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubeletplugin

import (
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/require"
)

func newTestGpuInfo(numaNode *int) *GpuDeviceInfo {
	return &GpuDeviceInfo{
		GpuInfo: &nvidia.GpuInfo{
			UUID:                  "GPU-test",
			Minor:                 0,
			ProductName:           "NVIDIA Test GPU",
			Brand:                 "NVIDIA",
			Architecture:          "Test",
			CudaComputeCapability: "9.0",
			DriverVersion: nvidia.DriverVersion{
				DriverVersion:     "580.0.0",
				CudaDriverVersion: 13000,
			},
			NumaNodeAttr: &deviceattribute.DeviceAttribute{
				Name: nvidia.StandardDeviceAttributeNumaNode,
				Value: resourceapi.DeviceAttribute{IntValue: func(n *int) *int64 {
					if n == nil {
						return nil
					}
					node := int64(*n)
					return &node
				}(numaNode)},
			},
		},
	}
}

func requireNumaNodeAttribute(t *testing.T, attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, expected int64) {
	t.Helper()

	attr, ok := attrs[nvidia.StandardDeviceAttributeNumaNode]
	require.True(t, ok)
	require.NotNil(t, attr.IntValue)
	require.Equal(t, expected, *attr.IntValue)
}

func requireNumaNodeListAttribute(t *testing.T, attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, expected []int64) {
	t.Helper()

	attr, ok := attrs[nvidia.StandardDeviceAttributeNumaNode]
	require.True(t, ok)
	require.Nil(t, attr.IntValue)
	require.Equal(t, expected, attr.IntValues)
}

func TestGpuInfoAttributesIncludeStandardNumaNode(t *testing.T) {
	gpu := newTestGpuInfo(ptr.To(1))

	requireNumaNodeAttribute(t, gpu.Attributes(), 1)
}

func TestGpuInfoAttributesIncludeStandardNumaNodeListWhenEnabled(t *testing.T) {
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.DRAListTypeAttributes): true,
	}))
	defer func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.DRAListTypeAttributes): false,
		}))
	}()

	gpu := newTestGpuInfo(ptr.To(1))

	requireNumaNodeListAttribute(t, gpu.Attributes(), []int64{1})
}

func TestCommonMigAttributesIncludeStandardNumaNode(t *testing.T) {
	parent := newTestGpuInfo(ptr.To(2))

	requireNumaNodeAttribute(t, CommonAttributesMig(parent.GpuInfo, "1g.10gb"), 2)
}

func TestVfioDeviceIncludesStandardNumaNode(t *testing.T) {
	vfio := &VfioDeviceInfo{
		UUID:        "vfio-test",
		deviceID:    "0x1234",
		vendorID:    "0x10de",
		index:       0,
		productName: "NVIDIA Test GPU",
		numaNode:    3,
		numaNodeAttr: &deviceattribute.DeviceAttribute{
			Name:  nvidia.StandardDeviceAttributeNumaNode,
			Value: resourceapi.DeviceAttribute{IntValue: ptr.To(int64(3))},
		},
		addressableMemoryBytes: 1024,
	}

	requireNumaNodeAttribute(t, vfio.GetDevice().Attributes, 3)
}
