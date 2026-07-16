package kubeletplugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	vgpu2 "github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/nri"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/docker/go-units"
	"github.com/google/uuid"
	"github.com/opencontainers/cgroups"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
	client2 "sigs.k8s.io/controller-runtime/pkg/client"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
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
	attributes := d.GpuDeviceInfo.Attributes()
	attributes["type"] = resourceapi.DeviceAttribute{
		StringValue: ptr.To(VGpuDeviceType),
	}
	attributes["coreRatio"] = resourceapi.DeviceAttribute{
		IntValue: ptr.To[int64](100),
	}
	attributes["memoryRatio"] = resourceapi.DeviceAttribute{
		IntValue: ptr.To[int64](100),
	}

	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attributes,
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
	clientSets      pkgflags.ClientSets
}

func NewVGPUManager(deviceLib *deviceLib, hostManagerPath string, clientSets pkgflags.ClientSets) *VGPUManager {
	return &VGPUManager{
		nvdevlib:        deviceLib,
		contManagerPath: util.ManagerRootPath,
		hostManagerPath: hostManagerPath,
		clientSets:      clientSets,
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

func (m *VGPUManager) getComputePolicy(claim *resourceapi.ResourceClaim) util.ComputePolicy {
	computePolicy := util.FixedComputePolicy
	for key, val := range claim.GetAnnotations() {
		if strings.HasSuffix(key, "/vgpu-compute-policy") && val != "" {
			computePolicy = vgpu2.GetComputePolicy(val)
			break
		}
	}
	return computePolicy
}

func (m *VGPUManager) ensureClaimDirectories(claimUID string) (string, string) {
	baseContPath := filepath.Join(m.contManagerPath, util.Claims, claimUID)
	baseHostPath := filepath.Join(m.hostManagerPath, util.Claims, claimUID)
	if err := os.RemoveAll(baseContPath); err != nil {
		klog.Warningf("Failed to remove claim container path %s: %s", baseContPath, err)
	}
	if err := util.EnsureDir(baseContPath, 0o777); err != nil {
		klog.Warningf("Failed to ensure directory %s: %s", baseContPath, err)
	}
	return baseContPath, baseHostPath
}

func (m *VGPUManager) ensurePartitionDirectories(claimUID, partitionKey string) (string, string) {
	baseContPath := filepath.Join(m.contManagerPath, util.Claims, claimUID, partitionKey)
	baseHostPath := filepath.Join(m.hostManagerPath, util.Claims, claimUID, partitionKey)
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
	return baseContPath, baseHostPath
}

func (m *VGPUManager) GetClaimCommonContainerEdits(claim *resourceapi.ResourceClaim) *cdiapi.ContainerEdits {
	_, _ = m.ensureClaimDirectories(string(claim.UID))

	compMode := util.HostMode
	switch {
	case featuregates.Enabled(featuregates.DevicePluginClientMode):
		compMode |= util.ClientRegMode
	case cgroups.IsCgroup2UnifiedMode(), cgroups.IsCgroup2HybridMode():
		compMode |= util.CGroupv2Mode
	default:
		compMode |= util.CGroupv1Mode
	}
	compMode |= util.OpenKernelMode
	containerDriverFile := filepath.Join(m.contManagerPath, "driver", vgpu.VGPUControlFileName)
	envs := []string{
		fmt.Sprintf("%s=%s", util.LdPreloadEnv, containerDriverFile),
		fmt.Sprintf("%s=%v", util.ManagerCompatibilityMode, compMode),
		// TODO Overcover possible environmental variable interference that may already exist in the container.
		fmt.Sprintf("%s=", util.ManagerVisibleDevices),
		fmt.Sprintf("%s=%v", util.CudaMemoryRatioEnv, 1),
		fmt.Sprintf("%s=", util.CudaCoreLimitEnv),
		fmt.Sprintf("%s=", util.CudaSoftCoreLimitEnv),
		fmt.Sprintf("%s=", util.CudaMemoryLimitEnv),
		fmt.Sprintf("%s=FALSE", util.CudaMemoryOversoldEnv),
	}
	// In NRI mode the partition mounts + register wiring are applied per-container
	// by the NRI plugin at CreateContainer, not here. Carry the claim UID via CDI
	// env so the NRI hook can correlate the container to its claim (validated
	// against node prepared state; see §12.12.1 in dra_nri_integration_design.md).
	if featuregates.Enabled(featuregates.NRISupport) {
		envs = append(envs, fmt.Sprintf("%s=%s", util.ManagerVGpuClaimUid, string(claim.UID)))
	} else {
		envs = append(envs, fmt.Sprintf("%s=", util.ManagerVGpuClaimUid))
	}
	hostLibraryPath := filepath.Join(m.hostManagerPath, vgpu.VGPUControlFileName)
	hostLibraryPath = fmt.Sprintf("%s.%s", hostLibraryPath, version.Get().Version)
	mounts := []*cdispec.Mount{
		{
			ContainerPath: filepath.Join(m.contManagerPath, util.Registry),
			HostPath:      filepath.Join(m.hostManagerPath, util.Registry),
			Options:       []string{"ro", "nosuid", "nodev", "bind"},
		},
		{
			ContainerPath: containerDriverFile,
			HostPath:      hostLibraryPath,
			Options:       []string{"ro", "nosuid", "nodev", "bind"},
		},
		{
			ContainerPath: filepath.Join(vgpu.ContPreLoadFilePath),
			HostPath:      filepath.Join(m.hostManagerPath, vgpu.LdPreLoadFileName),
			Options:       []string{"ro", "nosuid", "nodev", "bind"},
		},
	}
	if !featuregates.Enabled(featuregates.DevicePluginClientMode) {
		mounts = append(mounts, &cdispec.Mount{
			ContainerPath: m.contManagerPath + "/.host_proc",
			HostPath:      vgpu.HostProcDirectoryPath,
			Options:       []string{"ro", "nosuid", "nodev", "bind"},
		})
	}
	smWatcherEnabled := "FALSE"
	if featuregates.Enabled(featuregates.SharedSMUtilizationWatcher) {
		smWatcherEnabled = "TRUE"
		mounts = append(mounts, &cdispec.Mount{
			ContainerPath: filepath.Join(m.contManagerPath, util.Watcher),
			HostPath:      filepath.Join(m.hostManagerPath, util.Watcher),
			Options:       []string{"ro", "nosuid", "nodev", "bind"},
		})
	}
	envs = append(envs, fmt.Sprintf("%s=%s", util.ExternalSmWatcherEnabled, smWatcherEnabled))
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env:    envs,
			Mounts: mounts,
		},
	}
}

func (m *VGPUManager) GetAllocationEnvContainerEdits(claim *resourceapi.ResourceClaim, result *resourceapi.DeviceRequestAllocationResult, device *AllocatableDevice) *cdiapi.ContainerEdits {
	if result == nil || device == nil || device.Type() != VGpuDeviceType {
		return nil
	}

	computePolicy := m.getComputePolicy(claim)
	idx := device.VGpu.Index
	totalMemoryMB := device.VGpu.Memory.Total / units.MiB
	envs := []string{
		fmt.Sprintf("%s_%d=%v", util.CudaMemoryRatioEnv, idx, 1),
		fmt.Sprintf("%s_%d=FALSE", util.CudaMemoryOversoldEnv, idx),
		fmt.Sprintf("%s_%d=%s", util.ManagerVisibleDevice, idx, device.VGpu.UUID),
	}

	if quantity, ok := result.ConsumedCapacity[CoresResourceName]; ok {
		if hardVal, ok := quantity.AsInt64(); ok {
			softVal := hardVal
			if computePolicy == util.BalanceComputePolicy {
				softVal = util.HundredCore
			} else if computePolicy == util.NoneComputePolicy {
				hardVal = util.HundredCore
			}
			if hardVal > 0 && hardVal < util.HundredCore {
				envs = append(envs, fmt.Sprintf("%s_%d=%v", util.CudaCoreLimitEnv, idx, hardVal))
				envs = append(envs, fmt.Sprintf("%s_%d=%v", util.CudaSoftCoreLimitEnv, idx, softVal))
			} else {
				// unlimited
				envs = append(envs, fmt.Sprintf("%s_%d=", util.CudaCoreLimitEnv, idx))
				envs = append(envs, fmt.Sprintf("%s_%d=", util.CudaSoftCoreLimitEnv, idx))
			}
		}
	}

	if quantity, ok := result.ConsumedCapacity[MemoryResourceName]; ok {
		if val, ok := quantity.AsInt64(); ok {
			requestMB := uint64(val / units.MiB)
			if requestMB < totalMemoryMB {
				envs = append(envs, fmt.Sprintf("%s_%d=%vm", util.CudaMemoryLimitEnv, idx, requestMB))
			} else {
				// unlimited
				envs = append(envs, fmt.Sprintf("%s_%d=", util.CudaMemoryLimitEnv, idx))
			}
		}
	}

	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: envs,
		},
	}
}

func (m *VGPUManager) GetPartitionMountContainerEdits(claim *resourceapi.ResourceClaim, partitionKey string) (*cdiapi.ContainerEdits, error) {
	if partitionKey == "" {
		// TODO It's unlikely to run up to this point
		partitionKey = "default"
	}
	_, partitionHostPath := m.ensurePartitionDirectories(string(claim.UID), partitionKey)

	var envs []string
	if featuregates.Enabled(featuregates.DevicePluginClientMode) {
		partitionUuid := uuid.NewString()
		envs = append(envs, fmt.Sprintf("%s=%s", util.ManagerClientRegisterUuid, partitionUuid))
		metadata := client.PatchMetadata{Annotations: map[string]*string{
			fmt.Sprintf("%s/%s", util.DRADriverName, partitionUuid): &partitionKey,
		}}
		data, err := metadata.JSONBytes()
		if err != nil {
			return nil, err
		}
		_, err = m.clientSets.Core.ResourceV1().ResourceClaims(claim.Namespace).
			Patch(context.Background(), claim.Name, metadata.PatchType(), data, metav1.PatchOptions{})
		if err != nil {
			return nil, err
		}
	}

	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: envs,
			Mounts: []*cdispec.Mount{
				{
					ContainerPath: filepath.Join(m.contManagerPath, util.Config),
					HostPath:      filepath.Join(partitionHostPath, util.Config),
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: filepath.Join(vgpu.ContVGPULockPath),
					HostPath:      filepath.Join(partitionHostPath, vgpu.VGPULockDirName),
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: filepath.Join(vgpu.ContVMemoryNodePath),
					HostPath:      filepath.Join(partitionHostPath, util.VMemNode),
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
			},
		},
	}, nil
}

// GetNRIPartitionInjection ensures the per-container partition directories for a
// vGPU container in NRI mode and returns the mounts + register env for the NRI
// CreateContainer hook to inject. partitionKey is the per-container scope
// "<podUID>_<containerName>", matching the register server's pod-uid path
// (util.GetPodContainerManagerPath under claims/<claimUID>/). Unlike the
// Prepare-time GetPartitionMountContainerEdits, this mints no register UUID and
// patches no claim annotation: in NRI mode the library registers via the pod-uid
// path using the VGPU_POD_UID / VGPU_CONTAINER_NAME env injected here.
func (m *VGPUManager) GetNRIPartitionInjection(claimUID, podName, podNamespace, podUID, containerName string) (*nri.Injection, error) {
	partitionKey := fmt.Sprintf("%s_%s", podUID, containerName)
	contBase, hostBase := m.ensurePartitionDirectories(claimUID, partitionKey)
	return &nri.Injection{
		ConfigDir: filepath.Join(contBase, util.Config),
		Env: []string{
			fmt.Sprintf("%s=%s", util.PodNameEnv, podName),
			fmt.Sprintf("%s=%s", util.PodNamespaceEnv, podNamespace),
			fmt.Sprintf("%s=%s", util.PodUIDEnv, podUID),
			fmt.Sprintf("%s=%s", util.ContNameEnv, containerName),
			fmt.Sprintf("%s=", util.ManagerClientRegisterUuid),
		},
		Mounts: []nri.Mount{
			{
				ContainerPath: filepath.Join(m.contManagerPath, util.Config),
				HostPath:      filepath.Join(hostBase, util.Config),
				Options:       []string{"rw", "nosuid", "nodev", "bind"},
			},
			{
				ContainerPath: vgpu.ContVGPULockPath,
				HostPath:      filepath.Join(hostBase, vgpu.VGPULockDirName),
				Options:       []string{"rw", "nosuid", "nodev", "bind"},
			},
			{
				ContainerPath: vgpu.ContVMemoryNodePath,
				HostPath:      filepath.Join(hostBase, util.VMemNode),
				Options:       []string{"rw", "nosuid", "nodev", "bind"},
			},
		},
	}, nil
}

func (m *VGPUManager) Unprepare(claimRef kubeletplugin.NamespacedObject, devices PreparedDeviceList) error {
	_ = os.RemoveAll(filepath.Join(m.hostManagerPath, util.Claims, string(claimRef.UID)))

	if !featuregates.Enabled(featuregates.DevicePluginClientMode) {
		return nil
	}

	claim, err := m.clientSets.Resource.ResourceClaims(claimRef.Namespace).
		Get(context.Background(), claimRef.Name, metav1.GetOptions{})
	if err != nil {
		return client2.IgnoreNotFound(err)
	}
	// claim marked for deletion, fast return
	if !claim.DeletionTimestamp.IsZero() {
		return nil
	}
	metadata := client.PatchMetadata{Annotations: map[string]*string{}}
	for key := range claim.GetAnnotations() {
		if strings.HasPrefix(key, util.DRADriverName+"/") {
			metadata.Annotations[key] = nil
		}
	}
	if len(metadata.Annotations) > 0 {
		data, err := metadata.JSONBytes()
		if err != nil {
			return err
		}
		_, err = m.clientSets.Core.ResourceV1().ResourceClaims(claim.Namespace).
			Patch(context.Background(), claim.Name, metadata.PatchType(), data, metav1.PatchOptions{})
		if err != nil {
			return client2.IgnoreNotFound(err)
		}
	}
	return nil
}
