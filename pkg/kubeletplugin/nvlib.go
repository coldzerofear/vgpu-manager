package kubeletplugin

import (
	"fmt"
	"maps"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	"github.com/google/uuid"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/klog/v2"
)

type deviceLib struct {
	*nvidia.DeviceLib
}

func newDeviceLib(root nvidia.RootPath) (*deviceLib, error) {
	devlib, err := nvidia.NewDeviceLib(root)
	if err != nil {
		return nil, err
	}
	return &deviceLib{DeviceLib: devlib}, nil
}

func (l deviceLib) enumerateAllPossibleDevices(config *Config) (AllocatableDevices, error) {
	alldevices := make(AllocatableDevices)
	gms, err := l.enumerateGpusAndMigDevices(config)
	if err != nil {
		return nil, fmt.Errorf("error enumerating GPUs and MIG devices: %w", err)
	}
	maps.Copy(alldevices, gms)

	if featuregates.Enabled(featuregates.PassthroughSupport) {
		passthroughDevices, err := l.enumerateGpuPciDevices(config, gms)
		if err != nil {
			return nil, fmt.Errorf("error enumerating GPU PCI devices: %w", err)
		}
		maps.Copy(alldevices, passthroughDevices)
	}
	return alldevices, nil
}

func (l deviceLib) enumerateGpusAndMigDevices(config *Config) (AllocatableDevices, error) {
	if err := l.NvmlInit(); err != nil {
		return nil, err
	}
	defer l.NvmlShutdown()

	devices := make(AllocatableDevices)
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		info, err := l.GetGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}

		parentdev := &AllocatableDevice{
			Gpu: &GpuDeviceInfo{
				GpuInfo:   info,
				PcieBusID: links.PciInfo(info.PciInfo).BusID(),
				Health:    Healthy,
			},
		}

		migdevs, err := l.discoverMigDevicesByGPU(parentdev.Gpu)
		if err != nil {
			return fmt.Errorf("error discovering MIG devices for GPU %q: %w", parentdev.Gpu.CanonicalName(), err)
		}

		if featuregates.Enabled(featuregates.PassthroughSupport) {
			// If no MIG devices are found, allow VFIO devices.
			parentdev.Gpu.VfioEnabled = len(migdevs) == 0
		}

		if !parentdev.Gpu.MigEnabled {
			// Enabling VGPU support will replace physical GPUs and cannot use VFIO
			if featuregates.Enabled(featuregates.VGPUSupport) && len(migdevs) == 0 {
				parentdev.Gpu.VfioEnabled = false
				parentdev.VGpu = &VGpuDeviceInfo{
					GpuDeviceInfo: parentdev.Gpu,
				}
				parentdev.Gpu = nil
			}

			klog.Infof("Adding device %s to allocatable devices", parentdev.CanonicalName())
			devices[parentdev.CanonicalName()] = parentdev
			return nil
		}

		// Likely unintentionally stranded capacity (misconfiguration).
		if len(migdevs) == 0 {
			klog.Warningf("Physical GPU %s has MIG mode enabled but no configured MIG devices", parentdev.Gpu.CanonicalName())
		}

		for _, mdev := range migdevs {
			klog.Infof("Adding MIG device %s to allocatable devices (parent: %s)", mdev.CanonicalName(), parentdev.Gpu.CanonicalName())
			devices[mdev.CanonicalName()] = mdev
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error visiting devices: %w", err)
	}

	return devices, nil
}

func (l deviceLib) discoverMigDevicesByGPU(gpuInfo *GpuDeviceInfo) (AllocatableDeviceList, error) {
	var devices AllocatableDeviceList

	migs, err := l.GetMigInfos(gpuInfo.GpuInfo)
	if err != nil {
		return nil, fmt.Errorf("error getting MIG devices for GPU %q: %w", gpuInfo.CanonicalName(), err)
	}

	for _, migInfo := range migs {
		mig := &AllocatableDevice{
			Mig: &MigDeviceInfo{
				MigInfo:      migInfo,
				PcieBusID:    gpuInfo.PcieBusID,
				PcieRootAttr: gpuInfo.PcieRootAttr,
				Health:       Healthy,
			},
		}
		devices = append(devices, mig)
	}
	return devices, nil
}

func (l deviceLib) enumerateGpuPciDevices(config *Config, gms AllocatableDevices) (AllocatableDevices, error) {
	devices := make(AllocatableDevices)
	gpuPciDevices, err := l.GetGPUs()
	if err != nil {
		return nil, fmt.Errorf("error getting GPU PCI devices: %w", err)
	}
	for idx, pci := range gpuPciDevices {
		parent := gms.GetGPUByPCIeBusID(pci.Address)
		if parent == nil || !parent.Gpu.VfioEnabled {
			continue
		}
		vfioDeviceInfo, err := l.getVfioDeviceInfo(idx, pci)
		if err != nil {
			return nil, fmt.Errorf("error getting GPU info from PCI device: %w", err)
		}
		vfioDeviceInfo.parent = parent.Gpu
		devices[vfioDeviceInfo.CanonicalName()] = &AllocatableDevice{
			Vfio: vfioDeviceInfo,
		}
	}
	return devices, nil
}

func (l deviceLib) getVfioDeviceInfo(idx int, device *nvpci.NvidiaPCIDevice) (*VfioDeviceInfo, error) {
	var pcieRootAttr *deviceattribute.DeviceAttribute
	attr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(device.Address)
	if err == nil {
		pcieRootAttr = &attr
	} else {
		klog.Warningf("error getting PCIe root for device %s, continuing without attribute: %v", device.Address, err)
	}

	_, memoryBytes := device.Resources.GetTotalAddressableMemory(true)

	vfioDeviceInfo := &VfioDeviceInfo{
		UUID:                   uuid.NewSHA1(uuid.NameSpaceDNS, []byte(device.Address)).String(),
		index:                  idx,
		productName:            device.DeviceName,
		pcieBusID:              device.Address,
		pcieRootAttr:           pcieRootAttr,
		deviceID:               fmt.Sprintf("0x%04x", device.Device),
		vendorID:               fmt.Sprintf("0x%04x", device.Vendor),
		numaNode:               device.NumaNode,
		iommuGroup:             device.IommuGroup,
		addressableMemoryBytes: memoryBytes,
	}
	return vfioDeviceInfo, nil
}

// TODO: Need go-nvlib util for this.
func (l deviceLib) discoverGPUByPCIBusID(pcieBusID string) (*AllocatableDevice, AllocatableDeviceList, error) {
	if err := l.NvmlInit(); err != nil {
		return nil, nil, err
	}
	defer l.NvmlShutdown()

	var gpu *AllocatableDevice
	var migs AllocatableDeviceList
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		gpuPCIBusID, err := d.GetPCIBusID()
		if err != nil {
			return fmt.Errorf("error getting PCIe bus ID for device %d: %w", i, err)
		}
		if gpuPCIBusID != pcieBusID {
			return nil
		}
		gpuInfo, err := l.GetGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}
		gpuDevInfo := &GpuDeviceInfo{
			GpuInfo:   gpuInfo,
			PcieBusID: links.PciInfo(gpuInfo.PciInfo).BusID(),
			Health:    Healthy,
		}
		migs, err = l.discoverMigDevicesByGPU(gpuDevInfo)
		if err != nil {
			return fmt.Errorf("error discovering MIG devices for GPU %q: %w", gpuDevInfo.CanonicalName(), err)
		}
		// If no MIG devices are found, allow VFIO devices.
		gpuDevInfo.VfioEnabled = len(migs) == 0
		gpu = &AllocatableDevice{
			Gpu: gpuDevInfo,
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("error visiting devices: %w", err)
	}
	return gpu, migs, nil
}

// TODO: Need go-nvlib util for this.
func (l deviceLib) discoverVfioDevice(gpuInfo *GpuDeviceInfo) (*AllocatableDevice, error) {
	gpus, err := l.GetGPUs()
	if err != nil {
		return nil, fmt.Errorf("error getting GPU PCI devices: %w", err)
	}
	for idx, gpu := range gpus {
		if gpu.Address != gpuInfo.PcieBusID {
			continue
		}
		vfioDeviceInfo, err := l.getVfioDeviceInfo(idx, gpu)
		if err != nil {
			return nil, fmt.Errorf("error getting VFIO device info: %w", err)
		}
		vfioDeviceInfo.parent = gpuInfo
		return &AllocatableDevice{
			Vfio: vfioDeviceInfo,
		}, nil
	}
	return nil, fmt.Errorf("error discovering VFIO device by PCIe bus ID: %s", gpuInfo.PcieBusID)
}
