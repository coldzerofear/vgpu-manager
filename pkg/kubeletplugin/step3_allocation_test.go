package kubeletplugin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/docker/go-units"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestBuildRequestScopeKey_Sanitize(t *testing.T) {
	assert.Equal(t, "req-name", buildRequestScopeKey("req@name"))
}

func TestGetClaimDeviceName_UsesRequestScopeForVGPU(t *testing.T) {
	cdi := &CDIHandler{}
	device := &AllocatableDevice{VGpu: &VGpuDeviceInfo{GpuDeviceInfo: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{Minor: 2}}}}

	withRequest := cdi.GetClaimDeviceName("claim-uid", "multi-vgpu-vgpu-2-0", device)
	assert.Equal(t, "k8s.manager.nvidia.com/claim=claim-uid-multi-vgpu-vgpu-2-0", withRequest)

	fallback := cdi.GetClaimDeviceName("claim-uid", "", device)
	assert.Equal(t, "k8s.manager.nvidia.com/claim=claim-uid-vgpu-2", fallback)
}

func TestGetClaimDeviceName_DifferentRequestsAreDistinct(t *testing.T) {
	cdi := &CDIHandler{}
	device := &AllocatableDevice{VGpu: &VGpuDeviceInfo{GpuDeviceInfo: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{Minor: 0}}}}

	first := cdi.GetClaimDeviceName("claim-uid", "request-a-vgpu-0-0", device)
	second := cdi.GetClaimDeviceName("claim-uid", "request-b-vgpu-0-0", device)

	assert.NotEqual(t, first, second)
	assert.Equal(t, "k8s.manager.nvidia.com/claim=claim-uid-request-a-vgpu-0-0", first)
	assert.Equal(t, "k8s.manager.nvidia.com/claim=claim-uid-request-b-vgpu-0-0", second)
}

func TestBuildCDIDeviceID_UsesShareIDWhenPresent(t *testing.T) {
	shareID := types.UID("share@1")
	result := resourceapi.DeviceRequestAllocationResult{Request: "req@a", ShareID: &shareID}
	device := &AllocatableDevice{VGpu: &VGpuDeviceInfo{GpuDeviceInfo: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{Minor: 0}}}}

	assert.Equal(t, "req-a-vgpu-0-share-share-1", buildCDIDeviceID(result, 3, device))
}

func TestBuildCDIDeviceID_UsesOrdinalWithoutShareID(t *testing.T) {
	result := resourceapi.DeviceRequestAllocationResult{Request: "req@a"}
	device := &AllocatableDevice{VGpu: &VGpuDeviceInfo{GpuDeviceInfo: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{Minor: 1}}}}

	assert.Equal(t, "req-a-vgpu-1-2", buildCDIDeviceID(result, 2, device))
}

func TestVGPURequestMountEdits_AreRequestScopedForMounts(t *testing.T) {
	hostRoot := t.TempDir()
	contRoot := t.TempDir()
	manager := &VGPUManager{hostManagerPath: hostRoot, contManagerPath: contRoot}

	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-a")}}
	requestKey := "single-vgpu"
	result := &resourceapi.DeviceRequestAllocationResult{
		ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{
			CoresResourceName:  *resource.NewQuantity(50, resource.DecimalSI),
			MemoryResourceName: *resource.NewQuantity(4*units.GiB, resource.BinarySI),
		},
	}
	device := &AllocatableDevice{VGpu: &VGpuDeviceInfo{GpuDeviceInfo: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{
		Index: 0,
		UUID:  "GPU-ABC",
		Memory: nvml.Memory{
			Total: uint64(8 * units.GiB),
		},
	}}}}

	edits := manager.GetAllocationEnvContainerEdits(claim, result, device)
	edits = edits.Append(manager.GetRequestMountContainerEdits(claim, requestKey))
	require.NotNil(t, edits)
	require.NotNil(t, edits.ContainerEdits)

	hostBase := filepath.Join(hostRoot, util.Claims, "claim-a", requestKey)

	hostPaths := make([]string, 0, len(edits.ContainerEdits.Mounts))
	for _, m := range edits.ContainerEdits.Mounts {
		hostPaths = append(hostPaths, m.HostPath)
	}
	assert.Contains(t, hostPaths, filepath.Join(hostBase, util.Config))
	assert.Contains(t, hostPaths, filepath.Join(hostBase, vgpu.VGPULockDirName))
	assert.Contains(t, hostPaths, filepath.Join(hostBase, util.VMemNode))
}

func TestVGPUClaimCommonEdits_DoNotIncludeAllocationStateMounts(t *testing.T) {
	hostRoot := t.TempDir()
	contRoot := t.TempDir()
	manager := &VGPUManager{hostManagerPath: hostRoot, contManagerPath: contRoot}

	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-b")}}
	edits := manager.GetClaimCommonContainerEdits(claim)
	require.NotNil(t, edits)
	require.NotNil(t, edits.ContainerEdits)

	for _, m := range edits.ContainerEdits.Mounts {
		assert.NotEqual(t, filepath.Join(contRoot, util.Config), m.ContainerPath)
		assert.NotEqual(t, vgpu.ContVGPULockPath, m.ContainerPath)
		assert.NotEqual(t, vgpu.ContVMemoryNodePath, m.ContainerPath)
	}
}

func TestVGPUAllocationEdits_EnvReflectConsumedCapacity(t *testing.T) {
	hostRoot := t.TempDir()
	contRoot := t.TempDir()
	manager := &VGPUManager{hostManagerPath: hostRoot, contManagerPath: contRoot}

	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-c")}}
	result := &resourceapi.DeviceRequestAllocationResult{
		ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{
			CoresResourceName:  *resource.NewQuantity(40, resource.DecimalSI),
			MemoryResourceName: *resource.NewQuantity(2*units.GiB, resource.BinarySI),
		},
	}
	device := &AllocatableDevice{VGpu: &VGpuDeviceInfo{GpuDeviceInfo: &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{
		Index: 1,
		UUID:  "GPU-DEF",
		Memory: nvml.Memory{
			Total: uint64(8 * units.GiB),
		},
	}}}}

	edits := manager.GetAllocationEnvContainerEdits(claim, result, device)
	require.NotNil(t, edits)
	require.NotNil(t, edits.ContainerEdits)

	env := strings.Join(edits.ContainerEdits.Env, "\n")
	assert.Contains(t, env, "MANAGER_VISIBLE_DEVICE_1=GPU-DEF")
	assert.Contains(t, env, "CUDA_CORE_LIMIT_1=40")
	assert.Contains(t, env, "CUDA_CORE_SOFT_LIMIT_1=40")
	assert.Contains(t, env, "CUDA_MEM_LIMIT_1=2048m")
}
