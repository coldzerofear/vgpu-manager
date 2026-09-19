/*
Copyright The Kubernetes Authors

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

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

type fakeNVMLDeviceLib struct {
	nvdev.Interface
	device nvdev.Device
}

func (l fakeNVMLDeviceLib) NewDevice(nvml.Device) (nvdev.Device, error) {
	return l.device, nil
}

type fakeNVDevice struct {
	nvdev.Device
	migEnabled bool
}

func (d fakeNVDevice) IsMigEnabled() (bool, error) {
	return d.migEnabled, nil
}

type fakeNVMLGPU struct {
	nvml.Device
	migDevice       nvml.Device
	gi              nvml.GpuInstance
	giProfile       nvml.GpuInstanceProfileInfo
	setMigModeCalls []int
	events          *[]string
}

func (*fakeNVMLGPU) GetMaxMigDeviceCount() (int, nvml.Return) {
	return 1, nvml.SUCCESS
}

func (d *fakeNVMLGPU) GetMigDeviceHandleByIndex(int) (nvml.Device, nvml.Return) {
	return d.migDevice, nvml.SUCCESS
}

func (d *fakeNVMLGPU) GetGpuInstanceById(int) (nvml.GpuInstance, nvml.Return) {
	return d.gi, nvml.SUCCESS
}

func (d *fakeNVMLGPU) GetGpuInstanceProfileInfo(int) (nvml.GpuInstanceProfileInfo, nvml.Return) {
	return d.giProfile, nvml.SUCCESS
}

func (d *fakeNVMLGPU) CreateGpuInstanceWithPlacement(*nvml.GpuInstanceProfileInfo, *nvml.GpuInstancePlacement) (nvml.GpuInstance, nvml.Return) {
	return d.gi, nvml.SUCCESS
}

func (d *fakeNVMLGPU) GetArchitecture() (nvml.DeviceArchitecture, nvml.Return) {
	return nvml.DEVICE_ARCH_HOPPER, nvml.SUCCESS
}

func (d *fakeNVMLGPU) SetMigMode(mode int) (nvml.Return, nvml.Return) {
	d.setMigModeCalls = append(d.setMigModeCalls, mode)
	if d.events != nil {
		*d.events = append(*d.events, "set-mig-mode")
	}
	return nvml.SUCCESS, nvml.SUCCESS
}

type fakeNVMLGpuInstance struct {
	nvml.GpuInstance
	info      nvml.GpuInstanceInfo
	ci        nvml.ComputeInstance
	ciProfile nvml.ComputeInstanceProfileInfo
	events    *[]string
}

func (g *fakeNVMLGpuInstance) GetInfo() (nvml.GpuInstanceInfo, nvml.Return) {
	return g.info, nvml.SUCCESS
}

func (g *fakeNVMLGpuInstance) GetComputeInstanceById(int) (nvml.ComputeInstance, nvml.Return) {
	return g.ci, nvml.SUCCESS
}

func (g *fakeNVMLGpuInstance) GetComputeInstanceProfileInfo(int, int) (nvml.ComputeInstanceProfileInfo, nvml.Return) {
	return g.ciProfile, nvml.SUCCESS
}

func (g *fakeNVMLGpuInstance) CreateComputeInstance(*nvml.ComputeInstanceProfileInfo) (nvml.ComputeInstance, nvml.Return) {
	return g.ci, nvml.SUCCESS
}

func (g *fakeNVMLGpuInstance) Destroy() nvml.Return {
	if g.events != nil {
		*g.events = append(*g.events, "destroy-gi")
	}
	return nvml.SUCCESS
}

type fakeNVMLComputeInstance struct {
	nvml.ComputeInstance
	info   nvml.ComputeInstanceInfo
	events *[]string
}

func (c *fakeNVMLComputeInstance) GetInfo() (nvml.ComputeInstanceInfo, nvml.Return) {
	return c.info, nvml.SUCCESS
}

func (c *fakeNVMLComputeInstance) Destroy() nvml.Return {
	if c.events != nil {
		*c.events = append(*c.events, "destroy-ci")
	}
	return nvml.SUCCESS
}

type fakeNVMLMigDevice struct {
	nvml.Device
	giID int
	ciID int
	uuid string
}

func (d *fakeNVMLMigDevice) GetGpuInstanceId() (int, nvml.Return) {
	return d.giID, nvml.SUCCESS
}

func (d *fakeNVMLMigDevice) GetComputeInstanceId() (int, nvml.Return) {
	return d.ciID, nvml.SUCCESS
}

func (d *fakeNVMLMigDevice) GetUUID() (string, nvml.Return) {
	return d.uuid, nvml.SUCCESS
}

func (d *fakeNVMLMigDevice) GetMemoryInfo() (nvml.Memory, nvml.Return) {
	return nvml.Memory{}, nvml.SUCCESS
}

func enableDynamicMIGForTest(t *testing.T) {
	t.Helper()
	previous := featuregates.Enabled(featuregates.DynamicMIG)
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.DynamicMIG): true,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.DynamicMIG): previous,
		}))
	})
}

func testMigProfile() nvdev.MigProfile {
	return &nvdev.MigProfileInfo{
		C:              1,
		G:              1,
		GB:             5,
		GIProfileID:    0,
		CIProfileID:    0,
		CIEngProfileID: 0,
	}
}

func TestDeviceLibGetMigDevices(t *testing.T) {
	enableDynamicMIGForTest(t)

	migDevice := &fakeNVMLMigDevice{giID: 3, ciID: 4, uuid: "MIG-1"}
	ci := &fakeNVMLComputeInstance{info: nvml.ComputeInstanceInfo{Id: 4, ProfileId: 7}}
	gi := &fakeNVMLGpuInstance{
		info:      nvml.GpuInstanceInfo{Id: 3, ProfileId: 19, Placement: nvml.GpuInstancePlacement{Start: 2, Size: 1}},
		ci:        ci,
		ciProfile: nvml.ComputeInstanceProfileInfo{Id: 7},
	}
	gpu := &fakeNVMLGPU{
		migDevice: migDevice,
		gi:        gi,
		giProfile: nvml.GpuInstanceProfileInfo{Id: 19},
	}
	gpuInfo := &GpuDeviceInfo{
		GpuInfo: &nvidia.GpuInfo{
			UUID:        "GPU-1",
			Minor:       1,
			MigEnabled:  true,
			MigProfiles: []*nvidia.MigProfileInfo{{Profile: testMigProfile()}},
		},
	}
	l := deviceLib{DeviceLib: &nvidia.DeviceLib{}, devhandleByUUID: map[string]nvml.Device{gpuInfo.UUID: gpu}}

	got, err := l.GetMigDeviceInfos(gpuInfo)
	require.NoError(t, err)
	require.Equal(t, &MigDeviceInfo{
		MigInfo: &nvidia.MigInfo{
			UUID:    "MIG-1",
			Profile: "1g.5gb",
			Placement: &nvidia.MigDevicePlacement{
				GpuInstancePlacement: nvml.GpuInstancePlacement{
					Start: 2, Size: 1,
				},
			},
		},
		ParentDevice:   gpuInfo,
		ParentMinor:    1,
		ParentUUID:     "GPU-1",
		CIID:           4,
		GIID:           3,
		PlacementStart: 2,
		PlacementSize:  1,
		GiProfileID:    19,
	}, withoutMigDeviceRuntimeFields(got["MIG-1"]))
}

//func TestDeviceLibCreateMigDevice(t *testing.T) {
//	enableDynamicMIGForTest(t)
//
//	migDevice := &fakeNVMLMigDevice{uuid: "MIG-1"}
//	ci := &fakeNVMLComputeInstance{info: nvml.ComputeInstanceInfo{Id: 4, ProfileId: 7, Device: migDevice}}
//	gi := &fakeNVMLGpuInstance{
//		info:      nvml.GpuInstanceInfo{Id: 3},
//		ci:        ci,
//		ciProfile: nvml.ComputeInstanceProfileInfo{Id: 7},
//	}
//	gpu := &fakeNVMLGPU{gi: gi, giProfile: nvml.GpuInstanceProfileInfo{Id: 19}}
//	parent := &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{UUID: "GPU-1", Minor: 1}}
//	l := deviceLib{
//		DeviceLib:       nvidia.NewFakeDeviceLib(&fakeNVMLDeviceLib{device: fakeNVDevice{}}, nil, nil, nil),
//		devhandleByUUID: map[string]nvml.Device{parent.UUID: gpu},
//	}
//
//	got, err := l.createMigDevice(&MigSpec{
//		Parent:    parent,
//		Profile:   testMigProfile(),
//		Placement: nvml.GpuInstancePlacement{Start: 2, Size: 1},
//	})
//	require.NoError(t, err)
//	require.Equal(t, []int{nvml.DEVICE_MIG_ENABLE}, gpu.setMigModeCalls)
//	require.Equal(t, &MigDeviceInfo{
//		MigInfo: &nvidia.MigInfo{
//			UUID:    "MIG-1",
//			Profile: "1g.5gb",
//		},
//		CIID:           4,
//		GIID:           3,
//		ParentMinor:    1,
//		ParentUUID:     "GPU-1",
//		PlacementStart: 2,
//		PlacementSize:  1,
//		GiProfileID:    19,
//	}, withoutMigDeviceRuntimeFields(got))
//}

func TestDeviceLibDeleteMigDevice(t *testing.T) {
	enableDynamicMIGForTest(t)

	var events []string
	ci := &fakeNVMLComputeInstance{events: &events}
	gi := &fakeNVMLGpuInstance{ci: ci, events: &events}
	gpu := &fakeNVMLGPU{gi: gi, events: &events}
	parent := &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{UUID: "GPU-1", Minor: 1}}
	l := deviceLib{
		devhandleByUUID: map[string]nvml.Device{parent.UUID: gpu},
		gpuInfosByUUID:  map[string]*GpuDeviceInfo{parent.UUID: parent},
	}

	err := l.deleteMigDevice(&MigLiveTuple{ParentUUID: parent.UUID, GIID: 3, CIID: 4})
	require.NoError(t, err)
	require.Equal(t, []string{"destroy-ci", "destroy-gi", "set-mig-mode"}, events)
	require.Equal(t, []int{nvml.DEVICE_MIG_DISABLE}, gpu.setMigModeCalls)
}

func withoutMigDeviceRuntimeFields(info *MigDeviceInfo) *MigDeviceInfo {
	if info == nil {
		return nil
	}
	copy := *info
	copy.Parent = nil
	copy.GiProfileInfo = nil
	copy.GiInfo = nil
	copy.CiProfileInfo = nil
	copy.CiInfo = nil
	return &copy
}
