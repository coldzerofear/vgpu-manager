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
	"context"
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	resourceapi "k8s.io/api/resource/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockHealthMonitor implements deviceHealthMonitor for testing healthEventToTaint.
type mockHealthMonitor struct {
	nonFatalXids map[uint64]bool
}

func (m *mockHealthMonitor) Start(context.Context) error          { return nil }
func (m *mockHealthMonitor) Stop()                                {}
func (m *mockHealthMonitor) Unhealthy() <-chan *DeviceHealthEvent { return nil }
func (m *mockHealthMonitor) IsEventNonFatal(e *DeviceHealthEvent) bool {
	if e.EventType == HealthEventXID {
		return m.nonFatalXids[e.EventData]
	}
	return false
}

func TestAddOrUpdateTaint_NewTaint(t *testing.T) {
	dev := &AllocatableDevice{}
	taint := &resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	}

	changed := dev.AddOrUpdateTaint(taint)

	require.True(t, changed)
	require.Len(t, dev.Taints(), 1)
	assert.Equal(t, TaintKeyXID, dev.Taints()[0].Key)
	assert.Equal(t, "48", dev.Taints()[0].Value)
	assert.Equal(t, resourceapi.DeviceTaintEffectNoSchedule, dev.Taints()[0].Effect)
}

func TestAddOrUpdateTaint_DuplicateNoChange(t *testing.T) {
	dev := &AllocatableDevice{}
	taint := &resourceapi.DeviceTaint{
		Key:    TaintKeyGPULost,
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	}

	dev.AddOrUpdateTaint(taint)
	changed := dev.AddOrUpdateTaint(taint)

	assert.False(t, changed, "identical taint should not count as a change")
	assert.Len(t, dev.Taints(), 1)
}

func TestAddOrUpdateTaint_UpdateValue(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	changed := dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "63",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	require.True(t, changed)
	require.Len(t, dev.Taints(), 1)
	assert.Equal(t, "63", dev.Taints()[0].Value, "value should be overwritten to latest XID")
}

func TestAddOrUpdateTaint_UpdateEffect(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNone,
	})

	changed := dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	require.True(t, changed)
	assert.Equal(t, resourceapi.DeviceTaintEffectNoSchedule, dev.Taints()[0].Effect)
}

func TestAddOrUpdateTaint_DifferentKeysAppended(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyGPULost,
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	taints := dev.Taints()
	require.Len(t, taints, 2)
	assert.Equal(t, TaintKeyXID, taints[0].Key)
	assert.Equal(t, TaintKeyGPULost, taints[1].Key)
}

func TestAddOrUpdateTaint_TimeAddedResetOnChange(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNone,
	})

	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "63",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	assert.Nil(t, dev.Taints()[0].TimeAdded, "TimeAdded should be nil so the API server sets a fresh timestamp")
}

func TestPartGetDeviceIncludesHealthTaints(t *testing.T) {
	parent := &GpuDeviceInfo{
		GpuInfo: &nvidia.GpuInfo{
			UUID:                  "GPU-parent-1",
			Minor:                 0,
			CudaComputeCapability: "9.0",
			DriverVersion: nvidia.DriverVersion{
				DriverVersion:     "580.0.0",
				CudaDriverVersion: 13000,
			},
		},
	}
	dev := &AllocatableDevice{MigDynamic: &MigSpec{
		Parent:        parent,
		Profile:       &nvdev.MigProfileInfo{G: 1, GB: 5, GIProfileID: 19},
		GIProfileInfo: nvml.GpuInstanceProfileInfo{Id: 19},
		Placement:     nvml.GpuInstancePlacement{Start: 0, Size: 1},
	}}
	taint := &resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "43",
		Effect: resourceapi.DeviceTaintEffectNone,
	}
	require.True(t, dev.AddOrUpdateTaint(taint))

	got := dev.PartGetDevice(nil)
	require.Len(t, got.Taints, 1)
	assert.Equal(t, *taint, got.Taints[0])
}

func TestHealthEventToTaint(t *testing.T) {
	monitor := &mockHealthMonitor{
		nonFatalXids: map[uint64]bool{13: true, 31: true},
	}

	tests := []struct {
		name           string
		event          *DeviceHealthEvent
		monitor        deviceHealthMonitor
		expectedKey    string
		expectedValue  string
		expectedEffect resourceapi.DeviceTaintEffect
	}{
		{
			name: "fatal XID",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 48,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyXID,
			expectedValue:  "48",
			expectedEffect: resourceapi.DeviceTaintEffectNoSchedule,
		},
		{
			name: "non-fatal XID (skipped)",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 13,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyXID,
			expectedValue:  "13",
			expectedEffect: resourceapi.DeviceTaintEffectNone,
		},
		{
			name: "XID with nil monitor defaults to fatal",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 13,
			},
			monitor:        nil,
			expectedKey:    TaintKeyXID,
			expectedValue:  "13",
			expectedEffect: resourceapi.DeviceTaintEffectNoSchedule,
		},
		{
			name: "GPU lost",
			event: &DeviceHealthEvent{
				EventType: HealthEventGPULost,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyGPULost,
			expectedValue:  "",
			expectedEffect: resourceapi.DeviceTaintEffectNoSchedule,
		},
		{
			name: "unmonitored",
			event: &DeviceHealthEvent{
				EventType: HealthEventUnmonitored,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyUnmonitored,
			expectedValue:  "",
			expectedEffect: resourceapi.DeviceTaintEffectNone,
		},
		{
			name: "unknown event type defaults to unmonitored",
			event: &DeviceHealthEvent{
				EventType: DeviceHealthEventType("bogus"),
			},
			monitor:        monitor,
			expectedKey:    TaintKeyUnmonitored,
			expectedValue:  "",
			expectedEffect: resourceapi.DeviceTaintEffectNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			taint := healthEventToTaint(tc.monitor, tc.event)
			assert.Equal(t, tc.expectedKey, taint.Key)
			assert.Equal(t, tc.expectedValue, taint.Value)
			assert.Equal(t, tc.expectedEffect, taint.Effect)
		})
	}
}

func TestIsEventNonFatal(t *testing.T) {
	m := &nvmlDeviceHealthMonitor{
		skippedXids: map[uint64]bool{
			13: true,
			31: true,
			43: true,
		},
	}

	tests := []struct {
		name     string
		event    *DeviceHealthEvent
		expected bool
	}{
		{
			name: "skipped XID is non-fatal",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 13,
			},
			expected: true,
		},
		{
			name: "non-skipped XID is fatal",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 48,
			},
			expected: false,
		},
		{
			name: "GPU_LOST is always fatal",
			event: &DeviceHealthEvent{
				EventType: HealthEventGPULost,
			},
			expected: false,
		},
		{
			name: "unmonitored is not an XID event",
			event: &DeviceHealthEvent{
				EventType: HealthEventUnmonitored,
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, m.IsEventNonFatal(tc.event))
		})
	}
}

func TestAllocatableDevicesFindByAddress(t *testing.T) {
	parent := &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{UUID: "GPU-parent-1", Minor: 0, PciBusID: "0000:01:00.0"}}
	fullGPU := &AllocatableDevice{Gpu: parent}
	staticMIG := &AllocatableDevice{
		MigStatic: &MigDeviceInfo{
			MigInfo: &nvidia.MigInfo{
				Parent: parent.GpuInfo,
			},
			ParentUUID: parent.UUID,
			GIID:       2,
			CIID:       3,
		},
	}
	devices := AllocatableDevices{
		"gpu":    fullGPU,
		"static": staticMIG,
	}

	assert.Equal(t, fullGPU, devices.GetGPUDeviceByUUID(parent.UUID))
	assert.Nil(t, devices.GetGPUDeviceByUUID("GPU-unknown"))
	assert.Equal(t, staticMIG, devices.GetMigStaticDeviceByLiveTuple(&MigLiveTuple{
		ParentUUID: parent.UUID,
		GIID:       2,
		CIID:       3,
	}))
	assert.Nil(t, devices.GetMigStaticDeviceByLiveTuple(&MigLiveTuple{
		ParentUUID: parent.UUID,
		GIID:       2,
		CIID:       4,
	}))
	assert.Nil(t, devices.GetMigStaticDeviceByLiveTuple(&MigLiveTuple{
		ParentUUID: "GPU-unknown",
		GIID:       2,
		CIID:       3,
	}))
	assert.Nil(t, devices.GetMigStaticDeviceByLiveTuple(nil))
}

func TestAllocatableDevicesFindDynamicMIGBySpec(t *testing.T) {
	parent := &GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{UUID: "GPU-parent-1", Minor: 0, PciBusID: "0000:01:00.0"}}
	dynamicMIG := &AllocatableDevice{MigDynamic: &MigSpec{
		Parent:        parent,
		GIProfileInfo: nvml.GpuInstanceProfileInfo{Id: 19},
		Placement:     nvml.GpuInstancePlacement{Start: 0},
	}}
	devices := AllocatableDevices{"dynamic": dynamicMIG}

	tests := []struct {
		name string
		spec *MigSpecTuple
		want *AllocatableDevice
	}{
		{name: "exact match", spec: &MigSpecTuple{ParentPCIBusID: parent.PciBusID, ProfileID: 19, PlacementStart: 0}, want: dynamicMIG},
		{name: "profile mismatch", spec: &MigSpecTuple{ParentPCIBusID: parent.PciBusID, ProfileID: 14, PlacementStart: 0}},
		{name: "placement mismatch", spec: &MigSpecTuple{ParentPCIBusID: parent.PciBusID, ProfileID: 19, PlacementStart: 1}},
		{name: "parent mismatch", spec: &MigSpecTuple{ParentPCIBusID: "0000:02:00.0", ProfileID: 19, PlacementStart: 0}},
		{name: "nil spec"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, devices.GetMigDynamicDeviceByTuple(tc.spec))
		})
	}
}

func TestResolveDeviceByEventAddressUsesGPUUUIDIndex(t *testing.T) {
	parent := &nvidia.GpuInfo{UUID: "GPU-parent-1", Minor: 0, PciBusID: "0000:01:00.0"}
	staticMIG := &AllocatableDevice{
		MigStatic: &MigDeviceInfo{
			MigInfo: &nvidia.MigInfo{
				Parent: parent,
			},
			ParentUUID: parent.UUID,
			GIID:       2,
			CIID:       3,
		},
	}
	monitor := &nvmlDeviceHealthMonitor{
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				parent.PciBusID: {
					"static": staticMIG,
				},
			},
		},
		gpuInfosByUUID: map[string]*GpuDeviceInfo{parent.UUID: &GpuDeviceInfo{GpuInfo: parent}},
	}

	got, err := monitor.resolveDeviceByEventAddress(parent.UUID, nil, 2, 3)
	require.NoError(t, err)
	require.Equal(t, staticMIG, got)

	got, err = monitor.resolveDeviceByEventAddress(parent.UUID, nil, FullGPUInstanceID, 3)
	require.Nil(t, got)
	require.NoError(t, err)

	_, err = monitor.resolveDeviceByEventAddress("GPU-unknown", nil, 2, 3)
	require.ErrorContains(t, err, "failed to find parent GPU UUID")

}

func TestAllocatableDevicesFindRejectsWrongParent(t *testing.T) {
	parent := &GpuDeviceInfo{
		GpuInfo: &nvidia.GpuInfo{
			UUID:     "GPU-parent-1",
			PciBusID: "0000:01:00.0",
		},
	}
	otherParent := &GpuDeviceInfo{
		GpuInfo: &nvidia.GpuInfo{
			UUID:     "GPU-parent-2",
			PciBusID: parent.PciBusID,
		},
	}

	devices := AllocatableDevices{
		"wrong-gpu": {
			Gpu: otherParent,
		},
		"wrong-static-mig": {
			MigStatic: &MigDeviceInfo{
				ParentUUID: otherParent.UUID,
				GIID:       2,
				CIID:       3,
				MigInfo: &nvidia.MigInfo{
					Parent: otherParent.GpuInfo,
				},
			},
		},
	}

	require.Nil(t, devices.GetGPUDeviceByUUID(parent.UUID))
	require.Nil(t, devices.GetMigStaticDeviceByLiveTuple(&MigLiveTuple{
		ParentUUID: parent.UUID,
		GIID:       2,
		CIID:       3,
	}))
}

func TestHealthMonitorStartRequiresRegisteredEvents(t *testing.T) {
	m := &nvmlDeviceHealthMonitor{}
	require.ErrorContains(t, m.Start(context.Background()), "events have not been registered")
}

func TestClearDynamicMIGXIDTaint(t *testing.T) {
	dynamic := &AllocatableDevice{MigDynamic: &MigSpec{}}
	dynamic.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:   TaintKeyXID,
		Value: "43",
	})
	dynamic.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key: TaintKeyGPULost,
	})

	static := &AllocatableDevice{MigStatic: &MigDeviceInfo{}}
	static.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:   TaintKeyXID,
		Value: "43",
	})

	state := &DeviceState{
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				"0000:01:00.0": {
					"dynamic": dynamic,
					"static":  static,
				},
			},
		},
	}

	state.clearDynamicMIGXIDTaint("dynamic")
	state.clearDynamicMIGXIDTaint("static")

	require.Len(t, dynamic.Taints(), 1)
	assert.Equal(t, TaintKeyGPULost, dynamic.Taints()[0].Key)

	require.Len(t, static.Taints(), 1)
	assert.Equal(t, TaintKeyXID, static.Taints()[0].Key)
}
