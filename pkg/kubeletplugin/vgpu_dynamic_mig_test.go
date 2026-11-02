/*
Copyright 2026 coldzerofear

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
	"context"
	"fmt"
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/dynamic-resource-allocation/structured"
	"k8s.io/utils/ptr"

	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

const simNode = "node-1"

type classes map[string]*resourceapi.DeviceClass

func (c classes) List() ([]*resourceapi.DeviceClass, error) {
	var out []*resourceapi.DeviceClass
	for _, v := range c {
		out = append(out, v)
	}
	return out, nil
}
func (c classes) Get(name string) (*resourceapi.DeviceClass, error) {
	if v, ok := c[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("class %s not found", name)
}

func class(name, typ string) *resourceapi.DeviceClass {
	return &resourceapi.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourceapi.DeviceClassSpec{Selectors: []resourceapi.DeviceSelector{{CEL: &resourceapi.CELDeviceSelector{
			Expression: fmt.Sprintf("device.driver == '%s' && device.attributes['%s'].type == '%s'", util.DRADriverName, util.DRADriverName, typ),
		}}}},
	}
}

// H100-like MIG-capable GPU: 8 memory slices, placements for 1g at 0..6 and 7g at 0.
func simGPU(minor int) (*AllocatableDevice, AllocatableDevices) {
	gpu := newPartTestGpu(PartCapacityMap{
		"multiprocessors": intcap(132), "copyEngines": intcap(9), "decoders": intcap(5),
		"encoders": intcap(0), "jpegEngines": intcap(7), "ofaEngines": intcap(1),
		"memory": intcap(int64(80 * 1024 * 1024 * 1024)),
	}, 8)
	gpu.Minor = minor
	gpu.UUID = fmt.Sprintf("GPU-%d", minor)
	gpu.PciBusID = fmt.Sprintf("0000:%02d:00.0", minor+1)
	gpu.Memory = nvml.Memory{Total: 80 * 1024 * 1024 * 1024}
	all := AllocatableDevices{}
	vg := &AllocatableDevice{VGpu: &VGpuDeviceInfo{GpuDeviceInfo: gpu, deviceCoresRatio: 100, deviceMemoryRatio: 100}}
	all[vg.CanonicalName()] = vg
	for start := uint32(0); start < 7; start++ {
		s := newPartTestMigSpec(gpu, start, 1)
		d := &AllocatableDevice{MigDynamic: s}
		all[d.CanonicalName()] = d
	}
	full := newPartTestMigSpec(gpu, 0, 8)
	full.CandidateProfiles = []nvdev.MigProfile{&nvdev.MigProfileInfo{C: 7, G: 7, GB: 80, GIProfileID: 0}}
	full.GIProfileInfo.Id = 0
	full.GIProfileInfo.MultiprocessorCount = 132
	full.GIProfileInfo.CopyEngineCount = 9
	full.GIProfileInfo.DecoderCount = 5
	full.GIProfileInfo.JpegCount = 7
	full.GIProfileInfo.MemorySizeMB = 80 * 1024
	d := &AllocatableDevice{MigDynamic: full}
	all[d.CanonicalName()] = d
	return vg, all
}

func simSlices(t *testing.T, gpus int) []*resourceapi.ResourceSlice {
	per := &PerGPUAllocatableDevices{allocatablesMap: map[PCIBusID]AllocatableDevices{}}
	for i := 0; i < gpus; i++ {
		vg, all := simGPU(i)
		require.NoError(t, per.AddGPUAllocatables(vg.GetGPUPCIBusID(), all))
	}
	d := &driver{state: &DeviceState{perGPUAllocatable: per, config: &Config{}}, useSplitResourceSlices: true}
	res := d.GenerateDriverResources(simNode)
	return toAPI(res.Pools[simNode])
}

func toAPI(pool resourceslice.Pool) []*resourceapi.ResourceSlice {
	var out []*resourceapi.ResourceSlice
	for i, s := range pool.Slices {
		out = append(out, &resourceapi.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("slice-%d", i)},
			Spec: resourceapi.ResourceSliceSpec{
				Driver:         util.DRADriverName,
				NodeName:       ptr.To(simNode),
				Pool:           resourceapi.ResourcePool{Name: simNode, Generation: 1, ResourceSliceCount: int64(len(pool.Slices))},
				Devices:        s.Devices,
				SharedCounters: s.SharedCounters,
			},
		})
	}
	return out
}

var claimSeq int

func vgpuClaim(cores, memGi int64) *resourceapi.ResourceClaim {
	claimSeq++
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("vgpu-%d", claimSeq), Namespace: "ns", UID: types.UID(fmt.Sprintf("uid-v-%d", claimSeq))},
		Spec: resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{{
			Name: "r", Exactly: &resourceapi.ExactDeviceRequest{
				DeviceClassName: "vgpu-manager", AllocationMode: resourceapi.DeviceAllocationModeExactCount, Count: 1,
				Capacity: &resourceapi.CapacityRequirements{Requests: map[resourceapi.QualifiedName]resource.Quantity{
					CoresResourceName:  *resource.NewQuantity(cores, resource.DecimalSI),
					MemoryResourceName: *resource.NewQuantity(memGi*1024*1024*1024, resource.BinarySI),
				}},
			},
		}}}},
	}
}

func migClaim(selector string) *resourceapi.ResourceClaim {
	claimSeq++
	req := &resourceapi.ExactDeviceRequest{DeviceClassName: "mig-manager", AllocationMode: resourceapi.DeviceAllocationModeExactCount, Count: 1}
	if selector != "" {
		req.Selectors = []resourceapi.DeviceSelector{{CEL: &resourceapi.CELDeviceSelector{Expression: selector}}}
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("mig-%d", claimSeq), Namespace: "ns", UID: types.UID(fmt.Sprintf("uid-m-%d", claimSeq))},
		Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{{Name: "r", Exactly: req}}}},
	}
}

// Mirrors scheduler foreachAllocatedDevice semantics.
type simState struct {
	st structured.AllocatedState
}

func newSimState() *simState {
	return &simState{st: structured.AllocatedState{
		AllocatedDevices:         sets.New[structured.DeviceID](),
		AllocatedSharedDeviceIDs: sets.New[structured.SharedDeviceID](),
		AggregatedCapacity:       structured.NewConsumedCapacityCollection(),
	}}
}

func (s *simState) add(res resourceapi.DeviceRequestAllocationResult) {
	id := structured.MakeDeviceID(res.Driver, res.Pool, res.Device)
	if res.ShareID == nil {
		s.st.AllocatedDevices.Insert(id)
		return
	}
	s.st.AllocatedSharedDeviceIDs.Insert(structured.MakeSharedDeviceID(id, res.ShareID))
	s.st.AggregatedCapacity.Insert(structured.NewDeviceConsumedCapacity(id, res.ConsumedCapacity))
}

func (s *simState) rebuild(all [][]resourceapi.DeviceRequestAllocationResult) {
	*s = *newSimState()
	for _, rs := range all {
		for _, r := range rs {
			s.add(r)
		}
	}
}

var simClasses = classes{"vgpu-manager": class("vgpu-manager", "vgpu"), "mig-manager": class("mig-manager", "mig")}

// try runs the real scheduler allocator and commits a successful result.
func try(t *testing.T, s *simState, slices []*resourceapi.ResourceSlice, claims ...*resourceapi.ResourceClaim) ([][]resourceapi.DeviceRequestAllocationResult, bool) {
	t.Helper()
	features := structured.Features{ConsumableCapacity: true, PartitionableDevices: true, PrioritizedList: true}
	a, err := structured.NewAllocator(context.Background(), features, s.st, simClasses, slices, cel.NewCache(10, cel.Features{}))
	require.NoError(t, err)
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: simNode, Labels: map[string]string{"kubernetes.io/hostname": simNode}}}
	res, err := a.Allocate(context.Background(), node, claims)
	require.NoError(t, err)
	if len(res) == 0 {
		return nil, false
	}
	var out [][]resourceapi.DeviceRequestAllocationResult
	for _, r := range res {
		out = append(out, r.Devices.Results)
		for _, d := range r.Devices.Results {
			s.add(d)
		}
	}
	return out, true
}

func names(rs [][]resourceapi.DeviceRequestAllocationResult) []string {
	var n []string
	for _, r := range rs {
		for _, d := range r {
			n = append(n, d.Device)
		}
	}
	return n
}

const oneG = `device.attributes['manager.nvidia.com'].profile == '1g.10gb'`

func TestVGPUDynamicMIG_VGPUBlocksMIG_SingleGPU(t *testing.T) {
	{
		sl := simSlices(t, 1)
		s := newSimState()
		r1, ok := try(t, s, sl, vgpuClaim(10, 1))
		require.True(t, ok)
		require.Equal(t, []string{"vgpu-0"}, names(r1))
		r2, ok := try(t, s, sl, vgpuClaim(20, 2))
		require.True(t, ok, "second vGPU share must still fit")
		require.Equal(t, []string{"vgpu-0"}, names(r2))
		_, ok = try(t, s, sl, migClaim(oneG))
		require.False(t, ok, "MIG must be refused while a vGPU share is persisted")
	}
}

func TestVGPUDynamicMIG_MIGBlocksVGPU_ThenRelease(t *testing.T) {
	sl := simSlices(t, 1)
	s := newSimState()
	m1, ok := try(t, s, sl, migClaim(oneG))
	require.True(t, ok)
	_, ok = try(t, s, sl, migClaim(oneG))
	require.True(t, ok)
	_, ok = try(t, s, sl, vgpuClaim(10, 1))
	require.False(t, ok, "vGPU must be refused while MIG is allocated")
	s.rebuild(m1) // m2 released, m1 still there
	_, ok = try(t, s, sl, vgpuClaim(10, 1))
	require.False(t, ok)
	s.rebuild(nil)
	_, ok = try(t, s, sl, vgpuClaim(10, 1))
	require.True(t, ok)
}

func TestVGPUDynamicMIG_TwoGPUs_Coexist(t *testing.T) {
	sl := simSlices(t, 2)
	s := newSimState()
	v1a, ok := try(t, s, sl, vgpuClaim(10, 1))
	require.True(t, ok)
	_, ok = try(t, s, sl, vgpuClaim(10, 1))
	require.True(t, ok)
	m, ok := try(t, s, sl, migClaim(oneG))
	require.True(t, ok)
	_, ok = try(t, s, sl, migClaim(oneG))
	require.True(t, ok)
	v2, ok := try(t, s, sl, vgpuClaim(10, 1))
	require.True(t, ok)
	require.Equal(t, names(v1a), names(v2))
	require.NotContains(t, names(m)[0], "gpu-0-")
}

func TestVGPUDynamicMIG_SameCycle(t *testing.T) {
	sl := simSlices(t, 1)
	s := newSimState()
	_, ok := try(t, s, sl, vgpuClaim(10, 1), migClaim(oneG))
	require.False(t, ok)
	_, ok = try(t, s, sl, vgpuClaim(10, 1), vgpuClaim(10, 1), vgpuClaim(10, 1))
	require.True(t, ok, "several shares in one cycle must charge counters once")
	sl2 := simSlices(t, 2)
	s2 := newSimState()
	r2, ok := try(t, s2, sl2, vgpuClaim(10, 1), migClaim(oneG), vgpuClaim(10, 1))
	require.True(t, ok)
	require.Equal(t, []string{"vgpu-0", "gpu-1-mig-1g10gb-19-0", "vgpu-0"}, names(r2))
}

// Full-card MIG (7g) and first-fit order: 1g on a free GPU must not take the 7g placement.
func TestVGPUDynamicMIG_FullMIG(t *testing.T) {
	sl := simSlices(t, 1)
	s := newSimState()
	full := `device.attributes['manager.nvidia.com'].profile == '7g.80gb'`
	_, ok := try(t, s, sl, migClaim(full))
	require.True(t, ok)
	_, ok = try(t, s, sl, migClaim(oneG))
	require.False(t, ok)
	_, ok = try(t, s, sl, vgpuClaim(1, 1))
	require.False(t, ok)
}

func newCoexistState(t *testing.T) (*DeviceState, *AllocatableDevice, string) {
	t.Helper()
	vg, all := simGPU(0)
	var mig string
	for name, device := range all {
		if device.Type() == MigDynamicDeviceType {
			mig = name
			break
		}
	}
	per := &PerGPUAllocatableDevices{allocatablesMap: map[PCIBusID]AllocatableDevices{vg.GetGPUPCIBusID(): all}}
	return &DeviceState{perGPUAllocatable: per, config: &Config{}}, vg, mig
}

func allocationOf(device string) *resourceapi.AllocationResult {
	return &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{
		{Driver: util.DRADriverName, Pool: simNode, Device: device, Request: "r"},
	}}}
}

func TestValidateNoGPUModeConflict(t *testing.T) {
	st, vg, mig := newCoexistState(t)
	checkpointWith := func(device string, state ClaimCheckpointState) *Checkpoint {
		return &Checkpoint{V2: &CheckpointV2{PreparedClaims: PreparedClaimsByUIDV2{
			"other": {CheckpointState: state, Status: resourceapi.ResourceClaimStatus{Allocation: allocationOf(device)}},
		}}}
	}
	claimFor := func(device string) *resourceapi.ResourceClaim {
		return &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: "new"}, Status: resourceapi.ResourceClaimStatus{Allocation: allocationOf(device)}}
	}

	require.ErrorContains(t, st.validateNoGPUModeConflict(checkpointWith(mig, ClaimCheckpointStatePrepareCompleted), claimFor(vg.CanonicalName())), "different mode")
	require.ErrorContains(t, st.validateNoGPUModeConflict(checkpointWith(vg.CanonicalName(), ClaimCheckpointStatePrepareCompleted), claimFor(mig)), "different mode")
	require.NoError(t, st.validateNoGPUModeConflict(checkpointWith(vg.CanonicalName(), ClaimCheckpointStatePrepareCompleted), claimFor(vg.CanonicalName())), "vGPU shares a GPU with vGPU")
	require.NoError(t, st.validateNoGPUModeConflict(checkpointWith(mig, ClaimCheckpointStatePrepareStarted), claimFor(vg.CanonicalName())), "only completed claims count")
}

func TestFullGPUXIDAffectsWholeCard(t *testing.T) {
	st, vg, _ := newCoexistState(t)
	m := &nvmlDeviceHealthMonitor{
		perGPUAllocatable: st.perGPUAllocatable,
		gpuInfosByUUID:    map[string]*GpuDeviceInfo{vg.VGpu.UUID: vg.VGpu.GpuDeviceInfo},
	}

	got, err := m.resolveDevicesByEventAddress(vg.VGpu.UUID, nil, FullGPUInstanceID, FullGPUInstanceID)
	require.NoError(t, err)
	require.Len(t, got, 9, "the vGPU device and all 8 MIG devices of the card")
	require.Contains(t, got, vg)
}

func TestClearDynamicMIGXIDTaintKeepsCardLevelXID(t *testing.T) {
	st, vg, mig := newCoexistState(t)
	device := st.perGPUAllocatable.GetAllocatableDevice(mig)
	taint := &resourceapi.DeviceTaint{Key: TaintKeyXID, Value: "79", Effect: resourceapi.DeviceTaintEffectNoSchedule}

	device.AddOrUpdateTaint(taint)
	require.True(t, st.clearDynamicMIGXIDTaint(mig), "an XID of this MIG device goes with it")

	device.AddOrUpdateTaint(taint)
	vg.AddOrUpdateTaint(taint)
	require.False(t, st.clearDynamicMIGXIDTaint(mig), "an XID of the card outlives the MIG device")
	require.Len(t, device.Taints(), 1)
}

func TestValidateVGPUDynamicMIGSupport(t *testing.T) {
	for version, wantErr := range map[string]bool{
		"v1.34.8":       true,
		"v1.35.9":       true,
		"v1.36.1":       true,
		"v1.36.1-gke.3": true,
		"v1.36.2":       false,
		"v1.36.2-gke.1": false,
		"v1.36.2+k3s1":  false,
		"v1.37.0":       false,
	} {
		err := validateVGPUDynamicMIGSupport(fakeClientWithVersion(t, version))
		require.Equal(t, wantErr, err != nil, "version %s: %v", version, err)
	}
}

func TestHasVGPUOnPartitionableGPU(t *testing.T) {
	st, vg, _ := newCoexistState(t)
	require.True(t, st.hasVGPUOnPartitionableGPU())

	vg.VGpu.GpuDeviceInfo.maxCapacities = nil
	require.False(t, st.hasVGPUOnPartitionableGPU(), "a GPU without MIG profiles has no counters to share")
}

func TestMaybeDisableMigMode(t *testing.T) {
	newLib := func(gpu *GpuDeviceInfo, dev nvml.Device) deviceLib {
		return deviceLib{
			DeviceLib:       &nvidia.DeviceLib{},
			gpuInfosByUUID:  map[string]*GpuDeviceInfo{gpu.UUID: gpu},
			devhandleByUUID: map[string]nvml.Device{gpu.UUID: dev},
		}
	}
	newGPU := func(migEnabled bool) *GpuDeviceInfo {
		gpu := newPartTestGpu(nil, 0)
		gpu.MigEnabled = migEnabled
		return gpu
	}

	t.Run("leaves a GPU outside MIG mode alone", func(t *testing.T) {
		enableDynamicMIGForTest(t)
		gpu, dev := newGPU(false), &fakeNVMLGPU{migModeDisabled: true}
		require.NoError(t, newLib(gpu, dev).maybeDisableMigMode(gpu.UUID, dev))
		require.Empty(t, dev.setMigModeCalls)
	})

	t.Run("leaves a GPU without MIG support alone", func(t *testing.T) {
		enableDynamicMIGForTest(t)
		gpu, dev := newGPU(false), &fakeNVMLGPU{migModeRet: nvml.ERROR_NOT_SUPPORTED}
		require.NoError(t, newLib(gpu, dev).maybeDisableMigMode(gpu.UUID, dev))
		require.Empty(t, dev.setMigModeCalls)
	})

	t.Run("trusts the live mode over a stale flag", func(t *testing.T) {
		enableDynamicMIGForTest(t)
		gpu, dev := newGPU(false), &fakeNVMLGPU{}
		require.NoError(t, newLib(gpu, dev).maybeDisableMigMode(gpu.UUID, dev))
		require.Equal(t, []int{nvml.DEVICE_MIG_DISABLE}, dev.setMigModeCalls)
		require.False(t, gpu.MigEnabled)
	})

	t.Run("reports a failed call", func(t *testing.T) {
		enableDynamicMIGForTest(t)
		gpu, dev := newGPU(true), &fakeNVMLGPU{setMigModeRet: nvml.ERROR_NO_PERMISSION}
		require.ErrorIs(t, newLib(gpu, dev).maybeDisableMigMode(gpu.UUID, dev), nvml.ERROR_NO_PERMISSION)
		require.True(t, gpu.MigEnabled)
	})

	t.Run("reports a failed activation", func(t *testing.T) {
		enableDynamicMIGForTest(t)
		gpu, dev := newGPU(true), &fakeNVMLGPU{activationStatus: nvml.ERROR_IN_USE}
		require.ErrorIs(t, newLib(gpu, dev).maybeDisableMigMode(gpu.UUID, dev), nvml.ERROR_IN_USE)
		require.True(t, gpu.MigEnabled)
	})
}

func TestEnsureMigDisabled(t *testing.T) {
	enableDynamicMIGForTest(t)
	setup := func(dev *fakeNVMLGPU) (deviceLib, *GpuDeviceInfo) {
		gpu := newPartTestGpu(nil, 0)
		return deviceLib{
			DeviceLib:       &nvidia.DeviceLib{},
			gpuInfosByUUID:  map[string]*GpuDeviceInfo{gpu.UUID: gpu},
			devhandleByUUID: map[string]nvml.Device{gpu.UUID: dev},
		}, gpu
	}

	lib, gpu := setup(&fakeNVMLGPU{migModeDisabled: true})
	require.NoError(t, lib.ensureMigDisabled(gpu))

	dev := &fakeNVMLGPU{}
	lib, gpu = setup(dev)
	require.NoError(t, lib.ensureMigDisabled(gpu), "a left-over MIG mode with no MIG device is cleared")
	require.Equal(t, []int{nvml.DEVICE_MIG_DISABLE}, dev.setMigModeCalls)

	lib, gpu = setup(&fakeNVMLGPU{activationStatus: nvml.ERROR_IN_USE})
	require.ErrorIs(t, lib.ensureMigDisabled(gpu), nvml.ERROR_IN_USE)
}

func TestIsGpuUUIDInUseByOtherClaimsCountsVGPU(t *testing.T) {
	gpu := newPartTestGpu(nil, 0)
	checkpoint := &Checkpoint{V2: &CheckpointV2{PreparedClaims: PreparedClaimsByUIDV2{
		"other": {
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			PreparedDevices: PreparedDevices{{Devices: PreparedDeviceList{{VGpu: &PreparedVGpuDevice{
				Info:   &VGpuDeviceInfo{GpuDeviceInfo: gpu},
				Device: &CheckpointedDevice{DeviceName: "vgpu-0"},
			}}}}},
		},
	}}}

	require.True(t, isGpuUUIDInUseByOtherClaims(checkpoint, "self", gpu.UUID))
	require.False(t, isGpuUUIDInUseByOtherClaims(checkpoint, "other", gpu.UUID))
}

func TestUpdateSMWatcherHealth(t *testing.T) {
	vg, _ := simGPU(0)
	watched := &manager.GPUDevice{Healthy: true}
	devices := map[string]*manager.GPUDevice{vg.CanonicalName(): watched}

	vg.AddOrUpdateTaint(&resourceapi.DeviceTaint{Key: TaintKeyUnmonitored, Effect: resourceapi.DeviceTaintEffectNone})
	updateSMWatcherHealth(devices, vg)
	require.True(t, watched.Healthy, "a non-fatal taint keeps the samples flowing")

	vg.AddOrUpdateTaint(&resourceapi.DeviceTaint{Key: TaintKeyXID, Value: "79", Effect: resourceapi.DeviceTaintEffectNoSchedule})
	updateSMWatcherHealth(devices, vg)
	require.False(t, watched.Healthy, "a fatal XID stops sampling so workloads fall back and log it")
}
