package kubeletplugin

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

type GpuDeviceInfo struct {
	*nvidia.GpuInfo `json:",inline"`
	vfioEnabled     bool
	pciBusID        string

	// The following properties that can only be known after inspecting MIG
	// profiles.
	maxCapacities PartCapacityMap
	memSliceCount int
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
	UUID                   string `json:"uuid"`
	deviceID               string
	vendorID               string
	index                  int
	parent                 *GpuDeviceInfo
	productName            string
	PciBusID               string `json:"pciBusID"`
	pciBusIDAttr           *deviceattribute.DeviceAttribute
	pcieRootAttr           *deviceattribute.DeviceAttribute
	numaNode               int
	iommuGroup             int
	iommuFDEnabled         bool
	addressableMemoryBytes uint64
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

	if d.AddressingMode != nil {
		attrs["addressingMode"] = resourceapi.DeviceAttribute{
			StringValue: d.AddressingMode,
		}
	}

	return attrs
}

func (d *GpuDeviceInfo) GetDevice() resourceapi.Device {
	// TODO: Consume GetPCIBusIDAttribute from https://github.com/kubernetes/kubernetes/blob/4c5746c0bc529439f78af458f8131b5def4dbe5d/staging/src/k8s.io/dynamic-resource-allocation/deviceattribute/attribute.go#L39
	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: d.Attributes(),
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(d.Memory.Total), resource.BinarySI),
			},
		},
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
			"deviceID": {
				StringValue: &d.deviceID,
			},
			"vendorID": {
				StringValue: &d.vendorID,
			},
			"numa": {
				IntValue: ptr.To(int64(d.numaNode)),
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
	return device
}
