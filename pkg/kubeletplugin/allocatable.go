/*
 * Copyright (c) 2022-2025, NVIDIA CORPORATION.  All rights reserved.
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

package kubeletplugin

import (
	"fmt"
	"maps"
	"slices"

	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// The device name is the canonical device name announced by us a DRA
// ResourceSlice). It must be a (node-local) unambiguous device identifier. It's
// exposed to users in error messages. It's played back to us upon a
// NodePrepareResources request, which is when we look it up in the
// `AllocatableDevices` map. Conceptually, this the same as
// kubeletplugin.Device.DeviceName (documented with 'DeviceName identifies the
// device inside that pool').
type DeviceName = string

// Represents the PCI address (BDF format) of a physical GPU device.
type PCIBusID = string

type AllocatableDevices map[DeviceName]*AllocatableDevice

type PerGPUAllocatableDevices struct {
	allocatablesMap map[PCIBusID]AllocatableDevices
}

// AllocatableDevice represents an individual device that can be allocated.
type AllocatableDevice struct {
	VGpu       *VGpuDeviceInfo
	Gpu        *GpuDeviceInfo
	MigDynamic *MigSpec
	MigStatic  *MigDeviceInfo
	Vfio       *VfioDeviceInfo

	// taints holds DRA device taints set by the health monitor. Published
	// as part of the device's ResourceSlice entry so the scheduler and
	// kubelet honour them per KEP-5055.
	taints []resourceapi.DeviceTaint
}

func (d AllocatableDevice) Type() string {
	if d.VGpu != nil {
		return VGpuDeviceType
	}
	if d.Gpu != nil {
		return GpuDeviceType
	}
	if d.MigDynamic != nil {
		return MigDynamicDeviceType
	}
	if d.MigStatic != nil {
		return MigStaticDeviceType
	}
	if d.Vfio != nil {
		return VfioDeviceType
	}
	return UnknownDeviceType
}

func (d AllocatableDevice) IsStaticOrDynMigDevice() bool {
	switch d.Type() {
	case MigStaticDeviceType, MigDynamicDeviceType:
		return true
	default:
		return false
	}
}

func (d *AllocatableDevice) CanonicalName() string {
	switch d.Type() {
	case VGpuDeviceType:
		return d.VGpu.CanonicalName()
	case GpuDeviceType:
		return d.Gpu.CanonicalName()
	case MigStaticDeviceType:
		return d.MigStatic.CanonicalName()
	case MigDynamicDeviceType:
		return d.MigDynamic.CanonicalName()
	case VfioDeviceType:
		return d.Vfio.CanonicalName()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) GetDevice() resourceapi.Device {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.GetDevice()
	case MigStaticDeviceType:
		return d.MigStatic.GetDevice()
	case MigDynamicDeviceType:
		panic("GetDevice() must currently not be called for MigDynamicDeviceType")
	case VfioDeviceType:
		return d.Vfio.GetDevice()
	case VGpuDeviceType:
		return d.VGpu.GetDevice()
	}
	panic("unexpected type for AllocatableDevice")
}

// UUID() is here for `AllocatableDevices` to implement the `UUIDProvider`
// interface. Conceptually, at least since introduction of DynamicMIG, some
// allocatable devices are abstract devices that do not have a UUID before
// actualization -- hence, the idea of `AllocatableDevices` implementing
// UUIDProvider is brittle.
func (d AllocatableDevice) UUID() string {
	if d.VGpu != nil {
		return d.VGpu.UUID
	}
	if d.Gpu != nil {
		return d.Gpu.UUID
	}
	if d.MigStatic != nil {
		return d.MigStatic.UUID
	}
	if d.MigDynamic != nil {
		// For now, the caller must sure to never call UUID() on such a device.
		// This method for now exists because when the DynamicMIG feature gate
		// is disabled, `AllocatableDevices` _can_ implement UUIDProvider; and
		// that feature is used throughout the code base. This needs
		// restructuring and cleanup.
		panic("unexpected UUID() call for AllocatableDevice of type MigDynamic")
	}
	if d.Vfio != nil {
		return d.Vfio.UUID
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) GetGPUPCIBusID() string {
	switch d.Type() {
	case VGpuDeviceType:
		return d.VGpu.pciBusID
	case GpuDeviceType:
		return d.Gpu.pciBusID
	case MigStaticDeviceType:
		return d.MigStatic.pciBusID
	case MigDynamicDeviceType:
		return d.MigDynamic.Parent.pciBusID
	case VfioDeviceType:
		return d.Vfio.pciBusID
	}
	panic("unexpected type for AllocatableDevice")
}

func (d AllocatableDevices) GetGPUs() []*AllocatableDevice {
	var devices []*AllocatableDevice
	for _, device := range d {
		if device.Type() == GpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d AllocatableDevices) GetVfioDevices() []*AllocatableDevice {
	var devices []*AllocatableDevice
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

// Required for implementing UUIDProvider. Meant to return (only) full GPU UUIDs.
func (d AllocatableDevices) GpuUUIDs() []string {
	uuids := sets.NewString()
	for _, device := range d {
		if device.Type() == GpuDeviceType {
			uuids.Insert(device.UUID())
		}
		if device.Type() == VGpuDeviceType {
			uuids.Insert(device.UUID())
		}
	}
	return uuids.List()
}

// Required for implementing UUIDProvider. Meant to return MIG device UUIDs.
// Must not be called when the DynamicMIG featuregate is enabled.
func (d AllocatableDevices) MigDeviceUUIDs() []string {
	if featuregates.Enabled(featuregates.DynamicMIG) {
		panic("MigDeviceUUIDs() unexpectedly called (DynamicMIG is enabled)")
	}
	var uuids []string
	for _, dev := range d {
		if dev.Type() == MigStaticDeviceType {
			uuids = append(uuids, dev.UUID())
		}
	}
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) VfioDeviceUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			uuids = append(uuids, device.Vfio.UUID)
		}
	}
	slices.Sort(uuids)
	return uuids
}

// Required for implementing UUIDProvider. Meant to return full GPU UUIDs and
// MIG device UUIDs. Must not be used when the DynamicMIG featuregate is
// enabled. Unsure what it's supposed to return for VFIO devices.
func (d AllocatableDevices) UUIDs() []string {
	var uuids = make([]string, 0, len(d))
	for _, dev := range d {
		uuids = append(uuids, dev.UUID())
	}
	slices.Sort(uuids)
	return uuids
}

func (d *PerGPUAllocatableDevices) GetGPUDeviceByPCIBusID(pciBusID string) *AllocatableDevice {
	if devices, ok := d.allocatablesMap[pciBusID]; ok {
		for _, device := range devices {
			if device.Type() != GpuDeviceType {
				continue
			}
			return device
		}
	}
	return nil
}

func (d *PerGPUAllocatableDevices) AddGPUAllocatables(pciBusID string, allocatables AllocatableDevices) error {
	if allocatables == nil {
		return fmt.Errorf("allocatables is nil")
	}
	if _, ok := d.allocatablesMap[pciBusID]; !ok {
		d.allocatablesMap[pciBusID] = make(AllocatableDevices)
	}
	klog.Infof("Adding allocatables for PCI bus ID: %s", pciBusID)
	d.allocatablesMap[pciBusID] = allocatables
	return nil
}

func (d *PerGPUAllocatableDevices) AddAllocatableDevice(allocatable *AllocatableDevice) error {
	if allocatable == nil {
		return fmt.Errorf("allocatable is nil")
	}
	pciBusID := allocatable.GetGPUPCIBusID()
	if _, ok := d.allocatablesMap[pciBusID]; !ok {
		d.allocatablesMap[pciBusID] = make(AllocatableDevices)
	}
	klog.Infof("Adding allocatable device %q for PCI bus ID: %s", allocatable.CanonicalName(), pciBusID)
	d.allocatablesMap[pciBusID][allocatable.CanonicalName()] = allocatable
	return nil
}

func (d *PerGPUAllocatableDevices) GetAllocatableDevice(deviceName DeviceName) *AllocatableDevice {
	for _, devices := range d.allocatablesMap {
		if device, ok := devices[deviceName]; ok {
			return device
		}
	}
	return nil
}

func (d *PerGPUAllocatableDevices) GetAllDevices() AllocatableDevices {
	all := make(AllocatableDevices)
	for _, devices := range d.allocatablesMap {
		maps.Copy(all, devices)
	}
	return all
}

// TODO: This needs a code comment, clarifying the complexity across device
// types. This function is tied to PassthroughSuppert and hence for now
// guaranteed to not be exercised when DynamicMIG is enabled.
func (d *PerGPUAllocatableDevices) RemoveSiblingDevices(device *AllocatableDevice) {
	var pciBusID string
	switch device.Type() {
	case VGpuDeviceType:
		pciBusID = device.VGpu.pciBusID
	case GpuDeviceType:
		pciBusID = device.Gpu.pciBusID
	case VfioDeviceType:
		pciBusID = device.Vfio.pciBusID
	case MigStaticDeviceType:
		// TODO: Implement once/if static MIG is supported in the context of
		// PassthroughSupport.
		return
	case MigDynamicDeviceType:
		// TODO: Implement once/if dynamic MIG is supported in the context of
		// PassthroughSupport.
		return
	}

	for _, sibling := range d.allocatablesMap[pciBusID] {
		if sibling.Type() == device.Type() {
			continue
		}
		switch sibling.Type() {
		case VGpuDeviceType:
			delete(d.allocatablesMap[pciBusID], sibling.VGpu.CanonicalName())
		case GpuDeviceType:
			delete(d.allocatablesMap[pciBusID], sibling.Gpu.CanonicalName())
		case VfioDeviceType:
			delete(d.allocatablesMap[pciBusID], sibling.Vfio.CanonicalName())
		case MigStaticDeviceType:
			// TODO
			continue
		case MigDynamicDeviceType:
			// TODO
			continue
		}
	}
}

// Taints returns a copy of the device's taints to prevent data races
// when being read concurrently by the ResourceSlice builder.
func (d *AllocatableDevice) Taints() []resourceapi.DeviceTaint {
	return slices.Clone(d.taints)
}

// AddOrUpdateTaint adds a new taint or updates an existing one with the same
// key. The value and effect are always updated to the latest event received.
// Meaning, if a device receives multiple events for the same taint dimension
// (e.g., XID 48 followed by XID 63), the value is overwritten and only the most recent event data is retained.
// Returns true if the taint set was modified.
func (d *AllocatableDevice) AddOrUpdateTaint(taint *resourceapi.DeviceTaint) bool {
	for i, existing := range d.taints {
		if existing.Key == taint.Key {

			// 1. If nothing actually changed, exit early to avoid API calls
			if existing.Value == taint.Value && existing.Effect == taint.Effect {
				return false
			}

			// 2. Otherwise, update the fields and the timestamp
			d.taints[i].Value = taint.Value
			d.taints[i].Effect = taint.Effect
			d.taints[i].TimeAdded = nil // reset timestamp for the API server
			return true
		}
	}

	// 3. Key doesn't exist yet, append the dereferenced struct
	d.taints = append(d.taints, *taint)
	return true
}
