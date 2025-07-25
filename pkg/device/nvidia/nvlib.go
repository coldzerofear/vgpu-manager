/*
 * Copyright (c) 2021, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nvidia

import (
	"fmt"

	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"k8s.io/klog/v2"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

type CudaDriverVersion int

func (v CudaDriverVersion) MajorAndMinor() (major uint, minor uint) {
	major = uint(v / 1000)
	minor = uint(v % 1000 / 10)
	return
}

type DriverVersion struct {
	DriverVersion     string
	CudaDriverVersion CudaDriverVersion
}

type GpuInfo struct {
	Index                 int
	UUID                  string `json:"uuid"`
	Minor                 int
	MigEnabled            bool
	PciInfo               nvml.PciInfo
	Memory                nvml.Memory
	ProductName           string
	Brand                 string
	Architecture          string
	CudaComputeCapability string
	MigProfiles           []*MigProfileInfo
}

func (g *GpuInfo) GetPaths() ([]string, error) {
	return []string{fmt.Sprintf("/dev/nvidia%d", g.Minor)}, nil
}

// GetNumaNode returns the NUMA node associated with the GPU device
func (g GpuInfo) GetNumaNode() (int, bool) {
	node := links.PciInfo(g.PciInfo).NumaNode()
	if node < 0 {
		return 0, false
	}
	return int(node), true
}

type MigInfo struct {
	Index         int
	UUID          string `json:"uuid"`
	Profile       string
	Parent        *GpuInfo
	Memory        nvml.Memory
	Placement     *MigDevicePlacement
	GiProfileInfo *nvml.GpuInstanceProfileInfo
	GiInfo        *nvml.GpuInstanceInfo
	CiProfileInfo *nvml.ComputeInstanceProfileInfo
	CiInfo        *nvml.ComputeInstanceInfo
}

func (m *MigInfo) GetPaths() ([]string, error) {
	capDevicePaths, err := GetMigCapabilityDevicePaths()
	if err != nil {
		return nil, fmt.Errorf("error getting MIG capability device paths: %v", err)
	}
	gi := m.GiInfo.Id
	ci := m.CiInfo.Id

	parentPath := fmt.Sprintf("/dev/nvidia%d", m.Parent.Minor)

	giCapPath := fmt.Sprintf(nvidiaCapabilitiesPath+"/gpu%d/mig/gi%d/access", m.Parent.Minor, gi)
	if _, exists := capDevicePaths[giCapPath]; !exists {
		return nil, fmt.Errorf("missing MIG GPU instance capability path: %v", giCapPath)
	}

	ciCapPath := fmt.Sprintf(nvidiaCapabilitiesPath+"/gpu%d/mig/gi%d/ci%d/access", m.Parent.Minor, gi, ci)
	if _, exists := capDevicePaths[ciCapPath]; !exists {
		return nil, fmt.Errorf("missing MIG GPU instance capability path: %v", giCapPath)
	}

	devicePaths := []string{
		parentPath,
		capDevicePaths[giCapPath],
		capDevicePaths[ciCapPath],
	}

	return devicePaths, nil
}

type MigProfileInfo struct {
	Profile    nvdev.MigProfile
	Placements []*MigDevicePlacement
}

type MigDevicePlacement struct {
	nvml.GpuInstancePlacement
}

type devInterface nvdev.Interface
type nvmlInterface nvml.Interface

type DeviceLib struct {
	devInterface
	nvmlInterface
	DriverLibraryPath string
	DevRoot           string
	NvidiaSMIPath     string
}

func NewDeviceLib(root RootPath) (*DeviceLib, error) {
	driverLibraryPath, err := root.GetDriverLibraryPath()
	if err != nil {
		return nil, fmt.Errorf("failed to locate driver libraries: %w", err)
	}

	nvidiaSMIPath, err := root.getNvidiaSMIPath()
	if err != nil {
		return nil, fmt.Errorf("failed to locate nvidia-smi: %w", err)
	}

	// We construct an NVML library specifying the path to libnvidia-ml.so.1
	// explicitly so that we don't have to rely on the library path.
	nvmllib := nvml.New(
		nvml.WithLibraryPath(driverLibraryPath),
	)

	d := DeviceLib{
		devInterface:      nvdev.New(nvmllib),
		nvmlInterface:     nvmllib,
		DriverLibraryPath: driverLibraryPath,
		DevRoot:           root.GetDevRoot(),
		NvidiaSMIPath:     nvidiaSMIPath,
	}
	return &d, nil
}

//// prependPathListEnvvar prepends a specified list of strings to a specified envvar and returns its value.
//func prependPathListEnvvar(envvar string, prepend ...string) string {
//	if len(prepend) == 0 {
//		return os.Getenv(envvar)
//	}
//	current := filepath.SplitList(os.Getenv(envvar))
//	return strings.Join(append(prepend, current...), string(filepath.ListSeparator))
//}
//
//// setOrOverrideEnvvar adds or updates an envar to the list of specified envvars and returns it.
//func setOrOverrideEnvvar(envvars []string, key, value string) []string {
//	var updated []string
//	for _, envvar := range envvars {
//		pair := strings.SplitN(envvar, "=", 2)
//		if pair[0] == key {
//			continue
//		}
//		updated = append(updated, envvar)
//	}
//	return append(updated, fmt.Sprintf("%s=%s", key, value))
//}

func (l DeviceLib) NvmlInit() error {
	ret := l.nvmlInterface.Init()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error initializing NVML: %v", ret)
	}
	return nil
}

func (l DeviceLib) NvmlShutdown() {
	ret := l.nvmlInterface.Shutdown()
	if ret != nvml.SUCCESS {
		klog.Warningf("error shutting down NVML: %v", ret)
	}
}

//func (l DeviceLib) enumerateAllPossibleDevices(config *Config) (AllocatableDevices, error) {
//	alldevices := make(AllocatableDevices)
//
//	gms, err := l.enumerateGpusAndMigDevices(config)
//	if err != nil {
//		return nil, fmt.Errorf("error enumerating GPUs and MIG devices: %w", err)
//	}
//	for k, v := range gms {
//		alldevices[k] = v
//	}
//
//	return alldevices, nil
//}
//
//func (l deviceLib) enumerateGpusAndMigDevices(config *Config) (AllocatableDevices, error) {
//	if err := l.Init(); err != nil {
//		return nil, err
//	}
//	defer l.alwaysShutdown()
//
//	devices := make(AllocatableDevices)
//	err := l.VisitDevices(func(i int, d nvdev.Device) error {
//		gpuInfo, err := l.getGpuInfo(i, d)
//		if err != nil {
//			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
//		}
//
//		deviceInfo := &AllocatableDevice{
//			Gpu: gpuInfo,
//		}
//		devices[gpuInfo.CanonicalName()] = deviceInfo
//
//		migs, err := l.getMigDevices(gpuInfo)
//		if err != nil {
//			return fmt.Errorf("error getting MIG devices for GPU %d: %w", i, err)
//		}
//
//		for _, migDeviceInfo := range migs {
//			deviceInfo := &AllocatableDevice{
//				Mig: migDeviceInfo,
//			}
//			devices[migDeviceInfo.CanonicalName()] = deviceInfo
//		}
//
//		return nil
//	})
//	if err != nil {
//		return nil, fmt.Errorf("error visiting devices: %w", err)
//	}
//
//	return devices, nil
//}

func (l DeviceLib) GetGpuInfo(index int, device nvdev.Device) (*GpuInfo, error) {
	if err := l.NvmlInit(); err != nil {
		return nil, err
	}
	defer l.NvmlShutdown()
	pciInfo, ret := device.GetPciInfo()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting pci info for device %d: %v", index, ret)
	}
	minor, ret := device.GetMinorNumber()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting minor number for device %d: %v", index, ret)
	}
	uuid, ret := device.GetUUID()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting UUID for device %d: %v", index, ret)
	}
	migEnabled, err := device.IsMigEnabled()
	if err != nil {
		return nil, fmt.Errorf("error checking if MIG mode enabled for device %d: %w", index, err)
	}
	memory, ret := device.GetMemoryInfo()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting memory info for device %d: %v", index, ret)
	}
	productName, ret := device.GetName()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting product name for device %d: %v", index, ret)
	}
	architecture, err := device.GetArchitectureAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting architecture for device %d: %w", index, err)
	}
	brand, err := device.GetBrandAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting brand for device %d: %w", index, err)
	}
	cudaComputeCapability, err := device.GetCudaComputeCapabilityAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting CUDA compute capability for device %d: %w", index, err)
	}

	var migProfiles []*MigProfileInfo
	for i := 0; i < nvml.GPU_INSTANCE_PROFILE_COUNT; i++ {
		giProfileInfo, ret := device.GetGpuInstanceProfileInfo(i)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("error retrieving GpuInstanceProfileInfo for profile %d on GPU %v", i, uuid)
		}

		giPossiblePlacements, ret := device.GetGpuInstancePossiblePlacements(&giProfileInfo)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("error retrieving GpuInstancePossiblePlacements for profile %d on GPU %v", i, uuid)
		}

		var migDevicePlacements []*MigDevicePlacement
		for _, p := range giPossiblePlacements {
			mdp := &MigDevicePlacement{
				GpuInstancePlacement: p,
			}
			migDevicePlacements = append(migDevicePlacements, mdp)
		}

		for j := 0; j < nvml.COMPUTE_INSTANCE_PROFILE_COUNT; j++ {
			for k := 0; k < nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_COUNT; k++ {
				migProfile, err := l.NewMigProfile(i, j, k, giProfileInfo.MemorySizeMB, memory.Total)
				if err != nil {
					return nil, fmt.Errorf("error building MIG profile from GpuInstanceProfileInfo for profile %d on GPU %v", i, uuid)
				}

				if migProfile.GetInfo().G != migProfile.GetInfo().C {
					continue
				}

				profileInfo := &MigProfileInfo{
					Profile:    migProfile,
					Placements: migDevicePlacements,
				}

				migProfiles = append(migProfiles, profileInfo)
			}
		}
	}

	gpuInfo := &GpuInfo{
		UUID:                  uuid,
		Minor:                 minor,
		Index:                 index,
		MigEnabled:            migEnabled,
		PciInfo:               pciInfo,
		Memory:                memory,
		ProductName:           productName,
		Brand:                 brand,
		Architecture:          architecture,
		CudaComputeCapability: cudaComputeCapability,
		MigProfiles:           migProfiles,
	}

	return gpuInfo, nil
}

func (l DeviceLib) GetMigInfos(gpuInfo *GpuInfo) (map[string]*MigInfo, error) {
	if !gpuInfo.MigEnabled {
		return nil, nil
	}

	if err := l.NvmlInit(); err != nil {
		return nil, err
	}
	defer l.NvmlShutdown()

	device, ret := l.DeviceGetHandleByUUID(gpuInfo.UUID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU device handle: %v", ret)
	}

	migInfos := make(map[string]*MigInfo)
	err := walkMigDevices(device, func(i int, migDevice nvml.Device) error {
		memoryInfo, ret := migDevice.GetMemoryInfo()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting memory info for MIG device: %v", ret)
		}
		giID, ret := migDevice.GetGpuInstanceId()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance ID for MIG device: %v", ret)
		}
		gi, ret := device.GetGpuInstanceById(giID)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance for '%v': %v", giID, ret)
		}
		giInfo, ret := gi.GetInfo()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance info for '%v': %v", giID, ret)
		}
		ciID, ret := migDevice.GetComputeInstanceId()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance ID for MIG device: %v", ret)
		}
		ci, ret := gi.GetComputeInstanceById(ciID)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance for '%v': %v", ciID, ret)
		}
		ciInfo, ret := ci.GetInfo()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance info for '%v': %v", ciID, ret)
		}
		uuid, ret := migDevice.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting UUID for MIG device: %v", ret)
		}

		var migProfile *MigProfileInfo
		var giProfileInfo *nvml.GpuInstanceProfileInfo
		var ciProfileInfo *nvml.ComputeInstanceProfileInfo
		for _, profile := range gpuInfo.MigProfiles {
			profileInfo := profile.Profile.GetInfo()
			gipInfo, ret := device.GetGpuInstanceProfileInfo(profileInfo.GIProfileID)
			if ret != nvml.SUCCESS {
				continue
			}
			if giInfo.ProfileId != gipInfo.Id {
				continue
			}
			cipInfo, ret := gi.GetComputeInstanceProfileInfo(profileInfo.CIProfileID, profileInfo.CIEngProfileID)
			if ret != nvml.SUCCESS {
				continue
			}
			if ciInfo.ProfileId != cipInfo.Id {
				continue
			}
			migProfile = profile
			giProfileInfo = &gipInfo
			ciProfileInfo = &cipInfo
		}
		if migProfile == nil {
			return fmt.Errorf("error getting profile info for MIG device: %v", uuid)
		}

		placement := MigDevicePlacement{
			GpuInstancePlacement: giInfo.Placement,
		}

		migInfos[uuid] = &MigInfo{
			Index:         i,
			UUID:          uuid,
			Memory:        memoryInfo,
			Profile:       migProfile.Profile.String(),
			Parent:        gpuInfo,
			Placement:     &placement,
			GiProfileInfo: giProfileInfo,
			GiInfo:        &giInfo,
			CiProfileInfo: ciProfileInfo,
			CiInfo:        &ciInfo,
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error enumerating MIG devices: %w", err)
	}

	if len(migInfos) == 0 {
		return nil, nil
	}

	return migInfos, nil
}

func walkMigDevices(d nvml.Device, f func(i int, d nvml.Device) error) error {
	count, ret := d.GetMaxMigDeviceCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error getting max MIG device count: %v", ret)
	}

	for i := 0; i < count; i++ {
		device, ret := d.GetMigDeviceHandleByIndex(i)
		if ret == nvml.ERROR_NOT_FOUND {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting MIG device handle at index '%v': %v", i, ret)
		}
		if err := f(i, device); err != nil {
			return err
		}
	}
	return nil
}

//func (l DeviceLib) SetTimeSlice(uuids []string, timeSlice int) error {
//	for _, uuid := range uuids {
//		cmd := exec.Command(
//			l.nvidiaSMIPath,
//			"compute-policy",
//			"-i", uuid,
//			"--set-timeslice", fmt.Sprintf("%d", timeSlice))
//
//		// In order for nvidia-smi to run, we need update LD_PRELOAD to include the path to libnvidia-ml.so.1.
//		cmd.Env = setOrOverrideEnvvar(os.Environ(), "LD_PRELOAD", prependPathListEnvvar("LD_PRELOAD", l.driverLibraryPath))
//
//		output, err := cmd.CombinedOutput()
//		if err != nil {
//			klog.Errorf("\n%v", string(output))
//			return fmt.Errorf("error running nvidia-smi: %w", err)
//		}
//	}
//	return nil
//}
//
//func (l DeviceLib) SetComputeMode(uuids []string, mode string) error {
//	for _, uuid := range uuids {
//		cmd := exec.Command(
//			l.nvidiaSMIPath,
//			"-i", uuid,
//			"-c", mode)
//
//		// In order for nvidia-smi to run, we need update LD_PRELOAD to include the path to libnvidia-ml.so.1.
//		cmd.Env = setOrOverrideEnvvar(os.Environ(), "LD_PRELOAD", prependPathListEnvvar("LD_PRELOAD", l.driverLibraryPath))
//
//		output, err := cmd.CombinedOutput()
//		if err != nil {
//			klog.Errorf("\n%v", string(output))
//			return fmt.Errorf("error running nvidia-smi: %w", err)
//		}
//	}
//	return nil
//}

// TODO: Reenable dynamic MIG functionality once it is supported in Kubernetes 1.32
//func (l DeviceLib) CreateMigDevice(gpu *GpuInfo, profile nvdev.MigProfile, placement *nvml.GpuInstancePlacement) (*MigInfo, error) {
//	if err := l.NvmlInit(); err != nil {
//		return nil, err
//	}
//	defer l.NvmlShutdown()
//
//	profileInfo := profile.GetInfo()
//
//	device, ret := l.DeviceGetHandleByUUID(gpu.UUID)
//	if ret != nvml.SUCCESS {
//		return nil, fmt.Errorf("error getting GPU device handle: %v", ret)
//	}
//
//	giProfileInfo, ret := device.GetGpuInstanceProfileInfo(profileInfo.GIProfileID)
//	if ret != nvml.SUCCESS {
//		return nil, fmt.Errorf("error getting GPU instance profile info for '%v': %v", profile, ret)
//	}
//
//	gi, ret := device.CreateGpuInstanceWithPlacement(&giProfileInfo, placement)
//	if ret != nvml.SUCCESS {
//		return nil, fmt.Errorf("error creating GPU instance for '%v': %v", profile, ret)
//	}
//
//	giInfo, ret := gi.GetInfo()
//	if ret != nvml.SUCCESS {
//		return nil, fmt.Errorf("error getting GPU instance info for '%v': %v", profile, ret)
//	}
//
//	ciProfileInfo, ret := gi.GetComputeInstanceProfileInfo(profileInfo.CIProfileID, profileInfo.CIEngProfileID)
//	if ret != nvml.SUCCESS {
//		return nil, fmt.Errorf("error getting Compute instance profile info for '%v': %v", profile, ret)
//	}
//
//	ci, ret := gi.CreateComputeInstance(&ciProfileInfo)
//	if ret != nvml.SUCCESS {
//		return nil, fmt.Errorf("error creating Compute instance for '%v': %v", profile, ret)
//	}
//
//	ciInfo, ret := ci.GetInfo()
//	if ret != nvml.SUCCESS {
//		return nil, fmt.Errorf("error getting GPU instance info for '%v': %v", profile, ret)
//	}
//
//	uuid := ""
//	err := walkMigDevices(device, func(i int, migDevice nvml.Device) error {
//		giID, ret := migDevice.GetGpuInstanceId()
//		if ret != nvml.SUCCESS {
//			return fmt.Errorf("error getting GPU instance ID for MIG device: %v", ret)
//		}
//		ciID, ret := migDevice.GetComputeInstanceId()
//		if ret != nvml.SUCCESS {
//			return fmt.Errorf("error getting Compute instance ID for MIG device: %v", ret)
//		}
//		if giID != int(giInfo.Id) || ciID != int(ciInfo.Id) {
//			return nil
//		}
//		uuid, ret = migDevice.GetUUID()
//		if ret != nvml.SUCCESS {
//			return fmt.Errorf("error getting UUID for MIG device: %v", ret)
//		}
//		return nil
//	})
//	if err != nil {
//		return nil, fmt.Errorf("error processing MIG device for GI and CI just created: %w", err)
//	}
//	if uuid == "" {
//		return nil, fmt.Errorf("unable to find MIG device for GI and CI just created")
//	}
//
//	migInfo := &MigInfo{
//		UUID:    uuid,
//		Parent:  gpu,
//		Profile: profile.String(),
//		GiInfo:  &giInfo,
//		CiInfo:  &ciInfo,
//	}
//
//	return migInfo, nil
//}
//
//func (l DeviceLib) DeleteMigDevice(mig *MigInfo) error {
//	if err := l.NvmlInit(); err != nil {
//		return err
//	}
//	defer l.NvmlShutdown()
//
//	parent, ret := l.DeviceGetHandleByUUID(mig.Parent.UUID)
//	if ret != nvml.SUCCESS {
//		return fmt.Errorf("error getting device from UUID '%v': %v", mig.Parent.UUID, ret)
//	}
//	gi, ret := parent.GetGpuInstanceById(int(mig.GiInfo.Id))
//	if ret != nvml.SUCCESS {
//		return fmt.Errorf("error getting GPU instance ID for MIG device: %v", ret)
//	}
//	ci, ret := gi.GetComputeInstanceById(int(mig.CiInfo.Id))
//	if ret != nvml.SUCCESS {
//		return fmt.Errorf("error getting Compute instance ID for MIG device: %v", ret)
//	}
//	ret = ci.Destroy()
//	if ret != nvml.SUCCESS {
//		return fmt.Errorf("error destroying Compute Instance: %v", ret)
//	}
//	ret = gi.Destroy()
//	if ret != nvml.SUCCESS {
//		return fmt.Errorf("error destroying GPU Instance: %v", ret)
//	}
//	return nil
//}
