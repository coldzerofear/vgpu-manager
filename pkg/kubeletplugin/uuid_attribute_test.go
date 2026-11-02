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

package kubeletplugin

import (
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/stretchr/testify/require"
)

// Every device publishes the NVML UUID verbatim; selectors match it without
// regard to case, so nothing depends on a normalized spelling.
func TestUUIDAttributesArePublishedVerbatim(t *testing.T) {
	const gpuUUID = "GPU-1d2e3f40-aaaa-bbbb-cccc-000000000001"
	const migUUID = "MIG-5a6b7c80-aaaa-bbbb-cccc-000000000001"

	gpu := newTestGpuInfo(nil)
	gpu.UUID = gpuUUID
	require.Equal(t, gpuUUID, *gpu.Attributes()["uuid"].StringValue, "gpu")

	vgpu := &VGpuDeviceInfo{GpuDeviceInfo: gpu, deviceCoresRatio: 100, deviceMemoryRatio: 100}
	require.Equal(t, gpuUUID, *vgpu.GetDevice().Attributes["uuid"].StringValue, "vgpu")

	dynamic := &MigSpec{
		Parent:            gpu,
		CandidateProfiles: []nvdev.MigProfile{&nvdev.MigProfileInfo{C: 1, G: 1, GB: 10}},
		GIProfileInfo:     nvml.GpuInstanceProfileInfo{Id: 19, MemorySizeMB: 10240},
		Placement:         nvml.GpuInstancePlacement{Start: 0, Size: 1},
	}
	require.Equal(t, gpuUUID, *dynamic.PartGetDevice().Attributes["parentUUID"].StringValue, "dynamic mig")

	static := &MigDeviceInfo{
		MigInfo: &nvidia.MigInfo{
			UUID:          migUUID,
			Profile:       "1g.10gb",
			Parent:        gpu.GpuInfo,
			GiProfileInfo: &nvml.GpuInstanceProfileInfo{Id: 19, MemorySizeMB: 10240},
		},
		PlacementSize: 1,
	}
	attrs := static.GetDevice().Attributes
	require.Equal(t, migUUID, *attrs["uuid"].StringValue, "static mig")
	require.Equal(t, gpuUUID, *attrs["parentUUID"].StringValue, "static mig parent")

	vfio := &VfioDeviceInfo{UUID: gpuUUID}
	require.Equal(t, gpuUUID, *vfio.GetDevice().Attributes["uuid"].StringValue, "vfio")
}
