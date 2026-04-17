package kubeletplugin

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/docker/go-units"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

type VGpuDeviceInfo struct {
	*GpuDeviceInfo `json:",inline"`
}

func (d *VGpuDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("vgpu-%d", d.Minor)
}

func (d *VGpuDeviceInfo) GetDevice() resourceapi.Device {
	attr := d.GpuDeviceInfo.Attributes()
	attr["type"] = resourceapi.DeviceAttribute{
		StringValue: ptr.To(VGpuDeviceType),
	}
	attr["coreRatio"] = resourceapi.DeviceAttribute{
		IntValue: ptr.To[int64](100),
	}
	attr["memoryRatio"] = resourceapi.DeviceAttribute{
		IntValue: ptr.To[int64](100),
	}
	if numaNode, ok := d.GetNumaNode(); ok {
		attr["numaNode"] = resourceapi.DeviceAttribute{
			IntValue: ptr.To(int64(numaNode)),
		}
	}

	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attr,
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			CoresResourceName: {
				Value: *resource.NewQuantity(int64(util.HundredCore), resource.DecimalSI),
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: resource.NewQuantity(int64(util.HundredCore), resource.DecimalSI),
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min:  resource.NewQuantity(int64(0), resource.DecimalSI),
						Max:  resource.NewQuantity(int64(util.HundredCore), resource.DecimalSI),
						Step: resource.NewQuantity(int64(1), resource.DecimalSI),
					},
				},
			},
			MemoryResourceName: {
				Value: *resource.NewQuantity(int64(d.Memory.Total), resource.BinarySI),
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: resource.NewQuantity(int64(d.Memory.Total), resource.BinarySI),
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min:  resource.NewQuantity(int64(units.MiB), resource.BinarySI),
						Max:  resource.NewQuantity(int64(d.Memory.Total), resource.BinarySI),
						Step: resource.NewQuantity(int64(units.MiB), resource.BinarySI),
					},
				},
			},
		},
		AllowMultipleAllocations: pointer.Bool(true),
	}
	return device
}

// For sharing.go
type VGPUManager struct {
	hostManagerPath string
	contManagerPath string
	nvdevlib        *deviceLib
}

func NewVGPUManager(deviceLib *deviceLib, hostManagerPath string) *VGPUManager {
	return &VGPUManager{
		nvdevlib:        deviceLib,
		contManagerPath: util.ManagerRootPath,
		hostManagerPath: hostManagerPath,
	}
}

var (
	CoresResourceName  = resourceapi.QualifiedName("cores")
	MemoryResourceName = resourceapi.QualifiedName("memory")
)

func (m *VGPUManager) getVGpuDeviceSlice(devices AllocatableDevices) []*VGpuDeviceInfo {
	vGPUs := make([]*VGpuDeviceInfo, 0)
	for _, device := range devices {
		if device.Type() != VGpuDeviceType {
			continue
		}
		vGPUs = append(vGPUs, device.VGpu)
	}
	return vGPUs
}

func (m *VGPUManager) getConsumableCapacityMap(claim *resourceapi.ResourceClaim) map[string]map[resourceapi.QualifiedName]resource.Quantity {
	resMap := map[string]map[resourceapi.QualifiedName]resource.Quantity{}
	for _, result := range claim.Status.Allocation.Devices.Results {
		deviceName := result.Device
		if _, exists := resMap[deviceName]; !exists {
			resMap[deviceName] = map[resourceapi.QualifiedName]resource.Quantity{}
		}
		maps.Copy(resMap[deviceName], result.ConsumedCapacity)
	}
	return resMap
}

func (m *VGPUManager) GetCDIContainerEdits(claim *resourceapi.ResourceClaim, devices AllocatableDevices) *cdiapi.ContainerEdits {
	deviceSlice := m.getVGpuDeviceSlice(devices)
	if len(deviceSlice) == 0 {
		return nil
	}
	baseContPath := filepath.Join(m.contManagerPath, util.Claims, string(claim.UID))
	baseHostPath := filepath.Join(m.hostManagerPath, util.Claims, string(claim.UID))
	// TODO: We should check the status of claim, becasue there may be two pod share the claim
	if err := os.RemoveAll(baseContPath); err != nil {
		klog.Warningf("Failed to remove basic host path %s: %s", baseHostPath, err)
	}
	preparedDirs := []string{
		baseContPath,
		filepath.Join(baseContPath, util.Config),
		filepath.Join(baseContPath, vgpu.VGPULockDirName),
		filepath.Join(baseContPath, util.VMemNode),
	}
	for _, dirPath := range preparedDirs {
		if err := util.EnsureDir(dirPath, 0o777); err != nil {
			klog.Warningf("Failed to ensure directory %s: %s", dirPath, err)
		}
	}

	envMode := util.HostMode
	if cgroups.IsCgroup2UnifiedMode() || cgroups.IsCgroup2HybridMode() {
		envMode |= util.CGroupv2Mode
	} else {
		envMode |= util.CGroupv1Mode
	}
	conttainerDriverFile := filepath.Join(m.contManagerPath, "driver", vgpu.VGPUControlFileName)
	vGpuEnvs := []string{
		fmt.Sprintf("%s=", util.ManagerVisibleDevices),
		fmt.Sprintf("%s=%s", util.LdPreloadEnv, conttainerDriverFile),
		fmt.Sprintf("%s=%v", util.ManagerCompatibilityMode, envMode),
	}
	// TODO Covering the visible uuid list
	deviceCapacityMap := m.getConsumableCapacityMap(claim)
	for _, device := range deviceSlice {
		idx := device.Index
		totalMemoryMB := device.Memory.Total / units.MiB
		vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s_%d=%v", util.CudaMemoryRatioEnv, idx, 1))
		vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s_%d=FALSE", util.CudaMemoryOversoldEnv, idx))
		vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s_%d=%s", util.ManagerVisibleDevice, idx, device.UUID))
		if resourceMap, exists := deviceCapacityMap[device.CanonicalName()]; exists {
			if quantity, ok := resourceMap[CoresResourceName]; ok {
				if val, ok := quantity.AsInt64(); ok {
					// Rewrite environment variables to avoid interference from built-in environment variables in container images.
					vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s=", util.CudaCoreLimitEnv))
					vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s=", util.CudaSoftCoreLimitEnv))
					if val < util.HundredCore {
						vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s_%d=%v", util.CudaCoreLimitEnv, idx, val))
						vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s_%d=%v", util.CudaSoftCoreLimitEnv, idx, val))
					} else {
						vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s_%d=", util.CudaCoreLimitEnv, idx))
						vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s_%d=", util.CudaSoftCoreLimitEnv, idx))
					}
				}
			}
			if quantity, ok := resourceMap[MemoryResourceName]; ok {
				if val, ok := quantity.AsInt64(); ok {
					// TODO Only enable memory limit when the request is less than the entire card
					requestMB := uint64(val / units.MiB)
					// Rewrite environment variables to avoid interference from built-in environment variables in container images.
					vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s=", util.CudaMemoryLimitEnv))
					if requestMB < totalMemoryMB {
						vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s_%d=%vm", util.CudaMemoryLimitEnv, idx, requestMB))
					} else {
						vGpuEnvs = append(vGpuEnvs, fmt.Sprintf("%s_%d=", util.CudaMemoryLimitEnv, idx))
					}
				}
			}
		}
	}
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: vGpuEnvs,
			Mounts: []*cdispec.Mount{
				// TODO mount /etc/vgpu-manager/registry dir
				{
					ContainerPath: m.contManagerPath + "/.host_proc",
					HostPath:      vgpu.HostProcDirectoryPath,
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: filepath.Join(m.contManagerPath, util.Watcher),
					HostPath:      filepath.Join(m.hostManagerPath, util.Watcher),
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: conttainerDriverFile,
					HostPath:      filepath.Join(m.hostManagerPath, vgpu.VGPUControlFileName),
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: filepath.Join(vgpu.ContPreLoadFilePath),
					HostPath:      filepath.Join(m.hostManagerPath, vgpu.LdPreLoadFileName),
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: filepath.Join(m.contManagerPath, util.Config),
					HostPath:      filepath.Join(baseHostPath, util.Config),
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: filepath.Join(vgpu.ContVGPULockPath),
					HostPath:      filepath.Join(baseHostPath, vgpu.VGPULockDirName),
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: filepath.Join(vgpu.ContVMemoryNodePath),
					HostPath:      filepath.Join(baseHostPath, util.VMemNode),
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
			},
		},
	}
}

func (m *VGPUManager) Unprepare(claimUID string, _ PreparedDeviceList) error {
	_ = os.RemoveAll(filepath.Join(m.hostManagerPath, util.Claims, claimUID))
	return nil
}
