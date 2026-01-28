package kubeletplugin

import (
	"fmt"

	"github.com/blang/semver/v4"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

// Defined similarly as https://pkg.go.dev/k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1#Healthy.
type HealthStatus string

const (
	Healthy HealthStatus = "Healthy"
	// With NVMLDeviceHealthCheck, Unhealthy means that there are critcal xid errors on the device.
	Unhealthy HealthStatus = "Unhealthy"
)

type GpuDeviceInfo struct {
	*nvidia.GpuInfo
	VfioEnabled bool
	PcieBusID   string
	Health      HealthStatus
}

type MigDeviceInfo struct {
	*nvidia.MigInfo
	PcieBusID    string
	PcieRootAttr *deviceattribute.DeviceAttribute
	Health       HealthStatus
}

type VfioDeviceInfo struct {
	UUID                   string `json:"uuid"`
	deviceID               string
	vendorID               string
	index                  int
	parent                 *GpuDeviceInfo
	productName            string
	pcieBusID              string
	pcieRootAttr           *deviceattribute.DeviceAttribute
	numaNode               int
	iommuGroup             int
	addressableMemoryBytes uint64
}

func (d *GpuDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%d", d.Minor)
}

func (d *MigDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%d-mig-%d-%d-%d", d.Parent.Minor, d.GiInfo.ProfileId, d.Placement.Start, d.Placement.Size)
}

func (d *VfioDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-vfio-%d", d.index)
}

func (d *GpuDeviceInfo) GetDevice() resourceapi.Device {
	// TODO: Consume GetPCIBusIDAttribute from https://github.com/kubernetes/kubernetes/blob/4c5746c0bc529439f78af458f8131b5def4dbe5d/staging/src/k8s.io/dynamic-resource-allocation/deviceattribute/attribute.go#L39
	pciBusIDAttrName := resourceapi.QualifiedName(deviceattribute.StandardDeviceAttributePrefix + "pciBusID")
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(GpuDeviceType),
			},
			"uuid": {
				StringValue: &d.UUID,
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
			pciBusIDAttrName: {
				StringValue: &d.PcieBusID,
			},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(d.Memory.Total), resource.BinarySI),
			},
		},
	}
	if d.PcieRootAttr != nil {
		device.Attributes[d.PcieRootAttr.Name] = d.PcieRootAttr.Value
	}
	if d.AddressingMode != nil {
		device.Attributes["addressingMode"] = resourceapi.DeviceAttribute{
			StringValue: d.AddressingMode,
		}
	}
	return device
}

func (d *MigDeviceInfo) GetDevice() resourceapi.Device {
	// TODO: Consume GetPCIBusIDAttribute from https://github.com/kubernetes/kubernetes/blob/4c5746c0bc529439f78af458f8131b5def4dbe5d/staging/src/k8s.io/dynamic-resource-allocation/deviceattribute/attribute.go#L39
	pciBusIDAttrName := resourceapi.QualifiedName(deviceattribute.StandardDeviceAttributePrefix + "pciBusID")
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(MigDeviceType),
			},
			"uuid": {
				StringValue: &d.UUID,
			},
			"parentUUID": {
				StringValue: &d.Parent.UUID,
			},
			"profile": {
				StringValue: &d.Profile,
			},
			"productName": {
				StringValue: &d.Parent.ProductName,
			},
			"brand": {
				StringValue: &d.Parent.Brand,
			},
			"architecture": {
				StringValue: &d.Parent.Architecture,
			},
			"cudaComputeCapability": {
				VersionValue: ptr.To(semver.MustParse(d.Parent.CudaComputeCapability).String()),
			},
			"driverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.Parent.DriverVersion.DriverVersion).String()),
			},
			"cudaDriverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.Parent.DriverVersion.CudaDriverVersion.String()).String()),
			},
			pciBusIDAttrName: {
				StringValue: &d.PcieBusID,
			},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"multiprocessors": {
				Value: *resource.NewQuantity(int64(d.GiProfileInfo.MultiprocessorCount), resource.BinarySI),
			},
			"copyEngines": {Value: *resource.NewQuantity(int64(d.GiProfileInfo.CopyEngineCount), resource.BinarySI)},
			"decoders":    {Value: *resource.NewQuantity(int64(d.GiProfileInfo.DecoderCount), resource.BinarySI)},
			"encoders":    {Value: *resource.NewQuantity(int64(d.GiProfileInfo.EncoderCount), resource.BinarySI)},
			"jpegEngines": {Value: *resource.NewQuantity(int64(d.GiProfileInfo.JpegCount), resource.BinarySI)},
			"ofaEngines":  {Value: *resource.NewQuantity(int64(d.GiProfileInfo.OfaCount), resource.BinarySI)},
			"memory":      {Value: *resource.NewQuantity(int64(d.GiProfileInfo.MemorySizeMB*1024*1024), resource.BinarySI)},
		},
	}
	for i := d.Placement.Start; i < d.Placement.Start+d.Placement.Size; i++ {
		capacity := resourceapi.QualifiedName(fmt.Sprintf("memorySlice%d", i))
		device.Capacity[capacity] = resourceapi.DeviceCapacity{
			Value: *resource.NewQuantity(1, resource.BinarySI),
		}
	}
	if d.Parent.PcieRootAttr != nil {
		device.Attributes[d.Parent.PcieRootAttr.Name] = d.Parent.PcieRootAttr.Value
	}
	if d.Parent.AddressingMode != nil {
		device.Attributes["addressingMode"] = resourceapi.DeviceAttribute{
			StringValue: d.Parent.AddressingMode,
		}
	}
	return device
}

func (d *VfioDeviceInfo) GetDevice() resourceapi.Device {
	// TODO: Consume GetPCIBusIDAttribute from https://github.com/kubernetes/kubernetes/blob/4c5746c0bc529439f78af458f8131b5def4dbe5d/staging/src/k8s.io/dynamic-resource-allocation/deviceattribute/attribute.go#L39
	pciBusIDAttrName := resourceapi.QualifiedName(deviceattribute.StandardDeviceAttributePrefix + "pciBusID")
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(VfioDeviceType),
			},
			"uuid": {
				StringValue: &d.UUID,
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
			pciBusIDAttrName: {
				StringValue: &d.pcieBusID,
			},
			"productName": {
				StringValue: &d.productName,
			},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"addressableMemory": {
				Value: *resource.NewQuantity(int64(d.addressableMemoryBytes), resource.BinarySI),
			},
		},
	}
	if d.pcieRootAttr != nil {
		device.Attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}
	return device
}
