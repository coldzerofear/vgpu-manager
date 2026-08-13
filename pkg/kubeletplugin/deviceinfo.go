/*
Copyright The Kubernetes Authors
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
	"fmt"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const compatibilityNumaNodeAttribute resourceapi.QualifiedName = "dra.net/numaNode"

type GpuDeviceInfo struct {
	*nvidia.GpuInfo `json:",inline"`
	vfioEnabled     bool

	// The following properties that can only be known after inspecting MIG
	// profiles.
	maxCapacities PartCapacityMap
	memSliceCount int

	// Fabric Manager attributes. Populated only
	// when an FM Manager is available and the GPU is visible to NVML at
	// discovery time.
	gpuModuleID int

	// partitionsBySize maps an FM partition size (number of GPUs in the
	// partition) to the partitionId of the partition of that size that
	// includes this GPU. Used to publish the `partition1`/`partition2`/
	// `partition4`/`partition8` device attributes.
	partitionsBySize map[int]int
}

// Represents a specific (concrete, incarnated, created) MIG device. Annotated
// properties are stored in the checkpoint JSON upon prepare.
type MigDeviceInfo struct {
	*nvidia.MigInfo `json:",inline"`
	ParentUUID      string `json:"parentUUID"`
	GiProfileID     int    `json:"profileId"`

	// TODO: maybe embed MigLiveTuple.
	ParentMinor int `json:"parentMinor"`
	CIID        int `json:"ciId"`
	GIID        int `json:"giId"`

	// Store PlacementStart in the JSON checkpoint because in CanonicalName() we
	// rely on this -- and this must work after JSON deserialization.
	PlacementStart int `json:"placementStart"`
	PlacementSize  int `json:"placementSize"`

	pciBusID     string
	pcieRootAttr *deviceattribute.DeviceAttribute
}

type VfioDeviceInfo struct {
	UUID        string `json:"uuid"`
	deviceID    string
	vendorID    string
	index       int
	parent      *GpuDeviceInfo
	productName string
	// `omitempty`: postdates 25.12.0; emitting "pciBusID":"" would trip
	// CorruptCheckpointError on upgrade. See issue 1080.
	PciBusID               string `json:"pciBusID,omitempty"`
	pciBusIDAttr           *deviceattribute.DeviceAttribute
	pcieRootAttr           *deviceattribute.DeviceAttribute
	numaNodeAttr           *deviceattribute.DeviceAttribute
	numaNode               int
	iommuGroup             int
	iommuFDEnabled         bool
	addressableMemoryBytes uint64
	vfioModule             string
}

// CanonicalName returns the nameused for device announcement (in ResourceSlice
// objects). There is quite a bit of history to using the minor number for
// device announcement. Some context can be found at
// https://sigs.k8s.io/dra-driver-nvidia-gpu/issues/563#issuecomment-3345631087.
func (d *GpuDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%d", d.Minor)
}

// String returns both the GPU minor for easy recognizability, but also the
// UUID for precision. It is intended for usage in log messages.
func (d *GpuDeviceInfo) String() string {
	return fmt.Sprintf("%s-%s", d.CanonicalName(), d.UUID)
}

func (m *MigDeviceInfo) SpecTuple() *MigSpecTuple {
	return &MigSpecTuple{
		ParentMinor:    m.ParentMinor,
		ProfileID:      m.GiProfileID,
		PlacementStart: m.PlacementStart,
	}
}

func (m *MigDeviceInfo) LiveTuple() *MigLiveTuple {
	return &MigLiveTuple{
		ParentMinor: m.ParentMinor,
		ParentUUID:  m.ParentUUID,
		GIID:        m.GIID,
		CIID:        m.CIID,
		MigUUID:     m.UUID,
	}
}

// Return the canonical MIG device name. The name unambiguously defines the
// physical configuration, but doesn't reflect the fact that this represents a
// curently-live MIG device.
func (d *MigDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%d-mig-%d-%d-%d", d.Parent.Minor, d.GiInfo.ProfileId, d.Placement.Start, d.Placement.Size)
}

func (d *VfioDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-vfio-%d", d.index)
}

// Populate internal data structures -- detail that is only known after
// inspecting all individual MIG profiles associated with this physical GPU.
func (d *GpuDeviceInfo) AddDetailAfterWalkingMigProfiles(maxcap PartCapacityMap, memSliceCount int) {
	d.maxCapacities = maxcap
	d.memSliceCount = memSliceCount
}

func (d *GpuDeviceInfo) Attributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To(GpuDeviceType),
		},
		"uuid": {
			StringValue: ptr.To(strings.ToLower(d.UUID)),
		},
		"minor": {
			IntValue: ptr.To(int64(d.Minor)),
		},
		"numa": {
			IntValue: ptr.To(int64(d.GetNumaNode())),
		},
		"productName": {
			StringValue: &d.ProductName,
		},
		"brand": {
			StringValue: &d.Brand,
		},
		"architecture": {
			StringValue: &d.Architecture,
		},
		"cudaComputeCapability": {
			VersionValue: ptr.To(semver.MustParse(d.CudaComputeCapability).String()),
		},
		"driverVersion": {
			VersionValue: ptr.To(semver.MustParse(d.DriverVersion.DriverVersion).String()),
		},
		"cudaDriverVersion": {
			VersionValue: ptr.To(semver.MustParse(d.DriverVersion.CudaDriverVersion.String()).String()),
		},
	}

	if d.PciBusIDAttr != nil {
		attrs[d.PciBusIDAttr.Name] = d.PciBusIDAttr.Value
	}
	if d.PcieRootAttr != nil {
		attrs[d.PcieRootAttr.Name] = d.PcieRootAttr.Value
	}
	addCompatibilityNumaNodeAttribute(attrs, d.NumaNodeAttr)

	if d.AddressingMode != nil {
		attrs["addressingMode"] = resourceapi.DeviceAttribute{
			StringValue: d.AddressingMode,
		}
	}

	if featuregates.Enabled(featuregates.FabricManagerPartitioning) {
		d.addFabricManagerAttributes(attrs)
	}

	return attrs
}

// addFabricManagerAttributes publishes the Fabric Manager-derived attributes
// (`gpuModuleID` and `partitionN`) for this physical GPU. The values are
// resolved from NVML / FM at discovery time (see attachFabricManagerPartitions).
func (d *GpuDeviceInfo) addFabricManagerAttributes(attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) {
	if d == nil {
		return
	}

	if d.gpuModuleID == 0 && len(d.partitionsBySize) == 0 {
		klog.V(4).Infof("No Fabric Manager attributes for %s", d.CanonicalName())
		return
	}

	klog.V(4).Infof("Adding Fabric Manager attributes for %s: gpuModuleID=%d partitionsBySize=%v",
		d.CanonicalName(), d.gpuModuleID, d.partitionsBySize)
	if d.gpuModuleID != 0 {
		attrs["gpuModuleID"] = resourceapi.DeviceAttribute{
			IntValue: ptr.To(int64(d.gpuModuleID)),
		}
	}

	for size, partitionID := range d.partitionsBySize {
		key := resourceapi.QualifiedName(fmt.Sprintf("partition%d", size))
		attrs[key] = resourceapi.DeviceAttribute{
			IntValue: ptr.To(int64(partitionID)),
		}
	}
}

func (d *GpuDeviceInfo) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: d.Attributes(),
		Capacity:   d.fullGpuCapacity(),
	}
	return device
}

func (d *MigDeviceInfo) GetDevice() resourceapi.Device {

	attrs := CommonAttributesMig(d.Parent, d.Profile)
	attrs["uuid"] = resourceapi.DeviceAttribute{
		StringValue: ptr.To(strings.ToLower(d.UUID)),
	}

	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attrs,
		Capacity:   CommonCapacitiesMig(d.GiProfileInfo),
	}

	// Note(JP): noted elsewhere; what's the purpose of announcing memory slices
	// as capacity? Do we want to allow users to request specific placement?
	for i := d.PlacementStart; i < d.PlacementStart+d.PlacementSize; i++ {
		capacity := resourceapi.QualifiedName(fmt.Sprintf("memorySlice%d", i))
		device.Capacity[capacity] = resourceapi.DeviceCapacity{
			Value: *resource.NewQuantity(1, resource.BinarySI),
		}
	}

	return device
}

func (d *VfioDeviceInfo) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(VfioDeviceType),
			},
			"uuid": {
				StringValue: ptr.To(strings.ToLower(d.UUID)),
			},
			"numa": {
				IntValue: ptr.To(int64(d.numaNode)),
			},
			"deviceID": {
				StringValue: &d.deviceID,
			},
			"vendorID": {
				StringValue: &d.vendorID,
			},
			"productName": {
				StringValue: &d.productName,
			},
			"iommuFDEnabled": {
				BoolValue: ptr.To(d.iommuFDEnabled),
			},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"addressableMemory": {
				Value: *resource.NewQuantity(int64(d.addressableMemoryBytes), resource.BinarySI),
			},
		},
	}

	if d.pciBusIDAttr != nil {
		device.Attributes[d.pciBusIDAttr.Name] = d.pciBusIDAttr.Value
	}
	if d.pcieRootAttr != nil {
		device.Attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}

	if featuregates.Enabled(featuregates.FabricManagerPartitioning) {
		if d.parent == nil {
			klog.V(4).Infof("No parent GPU for %s; skipping Fabric Manager attributes", d.CanonicalName())
		} else {
			d.parent.addFabricManagerAttributes(device.Attributes)
		}
	}
	addCompatibilityNumaNodeAttribute(device.Attributes, d.numaNodeAttr)

	return device
}

func addCompatibilityNumaNodeAttribute(attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, numaNodeAttr *deviceattribute.DeviceAttribute) {
	if numaNodeAttr == nil {
		return
	}
	numaNode := numaNodeAttr.Value.IntValue
	if numaNode == nil || *numaNode < 0 {
		return
	}

	if featuregates.Enabled(featuregates.DRAListTypeAttributes) {
		// KEP-6072 prefers the list form when DRAListTypeAttributes is enabled.
		// Until this driver computes same-socket minimum-SLIT-distance nodes,
		// publish the physical NUMA node as a valid single-element list.
		attrs[numaNodeAttr.Name] = resourceapi.DeviceAttribute{
			IntValues: []int64{int64(*numaNode)},
		}
		attrs[compatibilityNumaNodeAttribute] = resourceapi.DeviceAttribute{
			IntValues: []int64{int64(*numaNode)},
		}
		return
	}

	attrs[numaNodeAttr.Name] = resourceapi.DeviceAttribute{
		IntValue: ptr.To(int64(*numaNode)),
	}
	attrs[compatibilityNumaNodeAttribute] = resourceapi.DeviceAttribute{
		IntValue: ptr.To(int64(*numaNode)),
	}
}
