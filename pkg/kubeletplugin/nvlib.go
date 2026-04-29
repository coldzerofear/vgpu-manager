package kubeletplugin

import (
	"fmt"
	"slices"
	"time"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	"github.com/google/uuid"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/klog/v2"
)

type GPUMinor = int

type deviceLib struct {
	*nvidia.DeviceLib
	gpuInfosByUUID    map[string]*GpuDeviceInfo
	gpuUUIDbyPCIBusID map[PCIBusID]string
	devhandleByUUID   map[string]nvml.Device
}

func newDeviceLib(root nvidia.RootPath) (*deviceLib, error) {
	devlib, err := nvidia.NewDeviceLib(root)
	if err != nil {
		return nil, err
	}

	d := &deviceLib{
		DeviceLib:         devlib,
		gpuInfosByUUID:    make(map[string]*GpuDeviceInfo),
		gpuUUIDbyPCIBusID: make(map[PCIBusID]string),
		devhandleByUUID:   make(map[string]nvml.Device),
	}

	// Current design: when DynamicMIG is enabled, use one long-lived NVML
	// session.
	if featuregates.Enabled(featuregates.DynamicMIG) {
		klog.V(1).Infof("DynamicMIG enabled: initialize long-lived NVML session")
		// Its possible there are no GPUs available in NVML.
		// (Eg: All gpus prepared in passthrough-mode)
		// We use the INIT_FLAG_NO_GPUS flag to avoid failing if there are no GPUs.
		if err := d.NvmlInitWithFlags(nvml.INIT_FLAG_NO_GPUS); err != nil {
			return nil, fmt.Errorf("failed to initialize NVML: %w", err)
		}
	}

	return d, nil
}

// ensureNVML() calls NVML Init() and returns an NVML shutdown function and an
// error (nvml.Return). The caller is responsible for calling the shutdown
// function (via defer). This is a noop when DynamicMIG is enabled.
func (l deviceLib) ensureNVML() (func(), nvml.Return) {
	// Long-lived NVML: no init needed, return no-op shutdown func. We could
	// achieve the same by just calling init() once more than shutdown because
	// NVML keeps an internal reference count. I however find it more readable
	// to explicitly initialize NVML only once when DynamicMIG is enabled.
	if featuregates.Enabled(featuregates.DynamicMIG) {
		return func() {}, nvml.SUCCESS
	}

	klog.V(6).Infof("Initializing NVML")
	t0 := time.Now()
	// Its possible there are no GPUs available in NVML.
	// (Eg: All gpus prepared in passthrough-mode)
	// We use the INIT_FLAG_NO_GPUS flag to avoid failing if there are no GPUs.
	ret := l.InitWithFlags(nvml.INIT_FLAG_NO_GPUS)
	if ret != nvml.SUCCESS {
		klog.Warningf("Failed to initialize NVML: %s", ret)
		// Init failed, nothing to cleanup: return no-op.
		return func() {}, ret
	}
	klog.V(6).Infof("t_nvml_init %.3f s", time.Since(t0).Seconds())

	return func() { l.NvmlShutdown() }, nvml.SUCCESS
}

// Discover devices that are allocatable, on this node.
func (l deviceLib) enumerateAllPossibleDevices() (*PerGPUAllocatableDevices, error) {
	perGPUAllocatable, err := l.GetPerGpuAllocatableDevices()
	if err != nil {
		return nil, fmt.Errorf("error enumerating allocatable devices: %w", err)
	}

	if featuregates.Enabled(featuregates.PassthroughSupport) {
		// Discover passthrough devices and insert them into the
		// `perGPUAllocatable` devices map
		err = l.enumerateGpuVfioDevices(perGPUAllocatable)
		if err != nil {
			return nil, fmt.Errorf("error enumerating GPU PCI devices: %w", err)
		}
	}

	return perGPUAllocatable, nil
}

func (l deviceLib) GetGpuDeviceInfo(index int, device nvdev.Device) (*GpuDeviceInfo, error) {
	gpuInfo, err := l.GetGpuInfo(index, device)
	if err != nil {
		return nil, err
	}
	return &GpuDeviceInfo{
		GpuInfo:     gpuInfo,
		vfioEnabled: false,
		pciBusID:    links.PciInfo(gpuInfo.PciInfo).BusID(),
	}, nil
}

func (l deviceLib) GetMigDeviceInfos(gpuInfo *GpuDeviceInfo) (map[string]*MigDeviceInfo, error) {
	if !gpuInfo.MigEnabled {
		return nil, nil
	}

	shutdown, ret := l.ensureNVML()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("ensureNVML failed: %w", ret)
	}
	defer shutdown()

	migs, err := l.GetMigInfos(gpuInfo.GpuInfo)
	if err != nil {
		return nil, err
	}
	migMap := make(map[string]*MigDeviceInfo, len(migs))
	for uuid, info := range migs {
		migMap[uuid] = &MigDeviceInfo{
			MigInfo:        info,
			ParentUUID:     gpuInfo.UUID,
			GiProfileID:    int(info.GiProfileInfo.Id),
			ParentMinor:    gpuInfo.Minor,
			CIID:           int(info.CiInfo.Id),
			GIID:           int(info.GiInfo.Id),
			PlacementStart: int(info.Placement.Start),
			PlacementSize:  int(info.Placement.Size),
			pciBusID:       gpuInfo.pciBusID,
			pcieRootAttr:   gpuInfo.PcieRootAttr,
		}
	}
	return migMap, nil
}

// GetPerGpuAllocatableDevices() is called once upon startup. It performs device
// discovery, and assembles the set of allocatable devices that will be
// announced by this DRA driver. A list of GPU indices can optionally be
// provided to limit the discovery to a set of physical GPUs.
func (l deviceLib) GetPerGpuAllocatableDevices(indices ...int) (*PerGPUAllocatableDevices, error) {
	klog.Infof("Traverse GPU devices")

	shutdown, ret := l.ensureNVML()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("ensureNVML failed: %w", ret)
	}
	defer shutdown()

	perGPUAllocatable := &PerGPUAllocatableDevices{
		allocatablesMap: make(map[PCIBusID]AllocatableDevices),
	}

	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		if indices != nil && !slices.Contains(indices, i) {
			return nil
		}

		// Prepare data structure for conceptually allocatable devices on this
		// one physical GPU.
		thisGPUAllocatable := make(AllocatableDevices)

		gpuInfo, err := l.GetGpuDeviceInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %v: %w", i, err)
		}
		parentdev := &AllocatableDevice{
			Gpu: gpuInfo,
		}
		// Store gpuInfo object for later re-use (lookup by UUID).
		l.gpuInfosByUUID[gpuInfo.UUID] = gpuInfo
		l.gpuUUIDbyPCIBusID[gpuInfo.pciBusID] = gpuInfo.UUID

		if featuregates.Enabled(featuregates.DynamicMIG) {
			// Best-effort handle cache warmup: store mapping between full-GPU
			// UUID and NVML device handle in a map. Ignore failures.
			if _, ret := l.DeviceGetHandleByUUID(parentdev.UUID()); ret != nvml.SUCCESS {
				klog.Warningf("DeviceGetHandleByUUIDCached failed: %s", ret)
			}

			// For this full device, inspect all MIG profiles and their possible
			// placements. Side effect: this enriches `gpuInfo` with additional
			// properties (such as the memory slice count, and the maximum
			// capacities as reported by individual MIG profiles).
			migspecs, err := l.inspectMigProfilesAndPlacements(gpuInfo, d)
			if err != nil {
				return fmt.Errorf("error getting MIG info for GPU %v: %w", i, err)
			}

			// Enabling VGPU support will replace physical GPUs and cannot use VFIO
			if featuregates.Enabled(featuregates.VGPUSupport) {
				parentdev.VGpu = &VGpuDeviceInfo{
					GpuDeviceInfo: gpuInfo,
				}
				parentdev.Gpu = nil
			}

			// Announce the full physical GPU.
			thisGPUAllocatable[parentdev.CanonicalName()] = parentdev

			for _, migspec := range migspecs {
				dev := &AllocatableDevice{
					MigDynamic: migspec,
				}
				thisGPUAllocatable[migspec.CanonicalName()] = dev
			}

			err = perGPUAllocatable.AddGPUAllocatables(parentdev.GetGPUPCIBusID(), thisGPUAllocatable)
			if err != nil {
				return fmt.Errorf("error adding allocatables for PCI bus ID %q: %w", parentdev.GetGPUPCIBusID(), err)
			}

			// Terminate this function -- this is mutually exclusive with static MIG and vfio/passthrough.
			return nil
		}

		migdevs, err := l.discoverMigDevicesByGPU(gpuInfo)
		if err != nil {
			return fmt.Errorf("error discovering MIG devices for GPU %q: %w", gpuInfo.CanonicalName(), err)
		}

		if featuregates.Enabled(featuregates.PassthroughSupport) {
			// Only if no MIG devices are found, allow VFIO devices.
			klog.Infof("PassthroughSupport enabled, and %d MIG devices found", len(migdevs))
			gpuInfo.vfioEnabled = len(migdevs) == 0
		}

		if !gpuInfo.MigEnabled {

			// Enabling VGPU support will replace physical GPUs and cannot use VFIO
			if featuregates.Enabled(featuregates.VGPUSupport) && len(migdevs) == 0 {
				gpuInfo.vfioEnabled = false
				parentdev.VGpu = &VGpuDeviceInfo{
					GpuDeviceInfo: gpuInfo,
				}
				parentdev.Gpu = nil
			}

			klog.Infof("Adding device %s to allocatable devices", parentdev.CanonicalName())
			// No static MIG devices prepared for this physical GPU. Announce
			// physical GPU to be allocatable, and terminate discovery for this
			// phyical GPU.
			thisGPUAllocatable[parentdev.CanonicalName()] = parentdev
			err = perGPUAllocatable.AddGPUAllocatables(parentdev.GetGPUPCIBusID(), thisGPUAllocatable)
			if err != nil {
				return fmt.Errorf("error adding allocatables for PCI bus ID %q: %w", parentdev.GetGPUPCIBusID(), err)
			}
			return nil
		}

		// Process statically pre-configured MIG devices.
		for _, mdev := range migdevs {
			klog.Infof("Adding MIG device %s to allocatable devices (parent: %s)", mdev.CanonicalName(), parentdev.CanonicalName())
			thisGPUAllocatable[mdev.CanonicalName()] = mdev
		}

		// Likely unintentionally stranded capacity (misconfiguration).
		if len(migdevs) == 0 {
			klog.Warningf("Physical GPU %s has MIG mode enabled but no configured MIG devices", parentdev.CanonicalName())
		}

		err = perGPUAllocatable.AddGPUAllocatables(parentdev.GetGPUPCIBusID(), thisGPUAllocatable)
		if err != nil {
			return fmt.Errorf("error adding allocatables for PCI bus ID %s: %w", parentdev.GetGPUPCIBusID(), err)
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("error visiting devices: %w", err)
	}

	return perGPUAllocatable, nil
}

func (l deviceLib) discoverMigDevicesByGPU(gpuInfo *GpuDeviceInfo) ([]*AllocatableDevice, error) {
	var devices []*AllocatableDevice
	migs, err := l.GetMigDeviceInfos(gpuInfo)
	if err != nil {
		return nil, fmt.Errorf("error getting MIG devices for GPU %q: %w", gpuInfo.CanonicalName(), err)
	}

	for _, migDeviceInfo := range migs {
		mig := &AllocatableDevice{
			MigStatic: migDeviceInfo,
		}
		devices = append(devices, mig)
	}
	return devices, nil
}

func (l deviceLib) enumerateGpuVfioDevices(perGPUAllocatable *PerGPUAllocatableDevices) error {
	// Discover PCI devices.
	gpuPciDevices, err := l.GetGPUs()
	if err != nil {
		return fmt.Errorf("error getting GPU PCI devices: %w", err)
	}

	// For each discovered PCI device, look up the corresponding full-GPU-device
	// and construct a corresponding new `AllocatableDevice` object.
	for idx, pci := range gpuPciDevices {
		klog.Infof("Adding VFIO device for discovered GPU PCI device: %s", pci.Address)

		parent := perGPUAllocatable.GetGPUDeviceByPCIBusID(pci.Address)
		if parent != nil && !parent.Gpu.vfioEnabled {
			klog.Infof("Skipping VFIO device for discovered GPU PCI device: %s, vfio is not enabled", pci.Address)
			continue
		}

		vfioDeviceInfo, err := l.getVfioDeviceInfo(idx, pci)
		if err != nil {
			return fmt.Errorf("error getting GPU info from PCI device: %w", err)
		}

		if parent != nil {
			vfioDeviceInfo.parent = parent.Gpu
		} else {
			// Its likely that the parent is nil because the GPU is prepared in passthrough mode.
			klog.Warningf("Skipping association with parent GPU device for VFIO device: %s", pci.Address)
		}

		allocatableDevice := &AllocatableDevice{
			Vfio: vfioDeviceInfo,
		}
		err = perGPUAllocatable.AddAllocatableDevice(allocatableDevice)
		if err != nil {
			return fmt.Errorf("error adding GPU VFIO device: %w", err)
		}
	}

	return nil
}

func (l deviceLib) getVfioDeviceInfo(idx int, device *nvpci.NvidiaPCIDevice) (*VfioDeviceInfo, error) {
	iommuFDEnabled, err := checkIommuFDEnabled()
	if err != nil {
		return nil, fmt.Errorf("error checking if IOMMUFD is supported: %w", err)
	}

	var pciBusIDAttr *deviceattribute.DeviceAttribute
	//attr, err := deviceattribute.GetPCIBusIDAttribute(device.Address)
	//if err != nil {
	//	return nil, fmt.Errorf("error getting PCI bus ID for device %s: %w", device.Address, err)
	//}
	//pciBusIDAttr = &attr

	var pcieRootAttr *deviceattribute.DeviceAttribute
	attr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(device.Address)
	if err == nil {
		pcieRootAttr = &attr
	} else {
		klog.Warningf("error getting PCIe root for device %s, continuing without attribute: %v", device.Address, err)
	}

	_, memoryBytes := device.Resources.GetTotalAddressableMemory(true)

	// Generate a unique UUID for the VFIO device based on the PCI bus ID.
	// This will always map to the same PCI bus ID.
	deviceUUID := uuid.NewSHA1(uuid.NameSpaceDNS, []byte(device.Address)).String()
	vfioDeviceInfo := &VfioDeviceInfo{
		UUID:                   deviceUUID,
		index:                  idx,
		productName:            device.DeviceName,
		PciBusID:               device.Address,
		pciBusIDAttr:           pciBusIDAttr,
		pcieRootAttr:           pcieRootAttr,
		deviceID:               fmt.Sprintf("0x%04x", device.Device),
		vendorID:               fmt.Sprintf("0x%04x", device.Vendor),
		numaNode:               device.NumaNode,
		iommuGroup:             device.IommuGroup,
		iommuFDEnabled:         iommuFDEnabled,
		addressableMemoryBytes: memoryBytes,
	}
	return vfioDeviceInfo, nil
}

// TODO: Need go-nvlib util for this.
func (l deviceLib) discoverGPUByPCIBusID(pcieBusID string) (*AllocatableDevice, []*AllocatableDevice, error) {
	if err := l.NvmlInitWithFlags(nvml.INIT_FLAG_NO_GPUS); err != nil {
		return nil, nil, err
	}
	defer l.NvmlShutdown()

	var gpu *AllocatableDevice
	var migs []*AllocatableDevice
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		gpuPCIBusID, err := d.GetPCIBusID()
		if err != nil {
			return fmt.Errorf("error getting PCIe bus ID for device %d: %w", i, err)
		}
		if gpuPCIBusID != pcieBusID {
			return nil
		}
		gpuInfo, err := l.GetGpuDeviceInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}

		migs, err = l.discoverMigDevicesByGPU(gpuInfo)
		if err != nil {
			return fmt.Errorf("error discovering MIG devices for GPU %q: %w", gpuInfo.CanonicalName(), err)
		}
		// If no MIG devices are found, allow VFIO devices.
		gpuInfo.vfioEnabled = len(migs) == 0
		gpu = &AllocatableDevice{
			Gpu: gpuInfo,
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
		if gpu.Address != gpuInfo.pciBusID {
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
	return nil, fmt.Errorf("error discovering VFIO device by PCIe bus ID: %s", gpuInfo.pciBusID)
}

// Tear down any MIG devices that are present and don't belong to completed
// claims. This can be improved for tearing down partial state (GI without CI,
// for example).
func (l deviceLib) obliterateStaleMIGDevices(expectedDeviceNames []DeviceName) error {
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		ginfo, err := l.GetGpuDeviceInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}

		migs, err := l.GetMigDeviceInfos(ginfo)
		if err != nil {
			return fmt.Errorf("error getting MIG devices for GPU %d: %w", i, err)
		}

		for _, mdi := range migs {
			name := mdi.CanonicalName()
			expected := slices.Contains(expectedDeviceNames, name)
			if !expected {
				klog.Warningf("Found unexpected MIG device (%s), attempt to tear down", name)
				if err := l.deleteMigDevice(mdi.LiveTuple()); err != nil {
					return fmt.Errorf("could not delete unexpected MIG device (%s): %w", name, err)
				}
			}
		}

		// If no MIG device was found on this GPU, MIG mode might still be
		// enabled. Disable it in this case.
		if err := l.maybeDisableMigMode(ginfo.UUID, d); err != nil {
			return fmt.Errorf("maybeDisableMigMode failed for GPU %s: %w", ginfo.UUID, err)
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("error visiting devices: %w", err)
	}
	return nil
}

func (l deviceLib) maybeDisableMigMode(uuid string, nvmldev nvml.Device) error {
	// Expect the parent GPU to be represented in in `l.gpuInfosByUUID`
	gpu, ok := l.gpuInfosByUUID[uuid]
	if !ok {
		// TODO: this is a programming error -- panic instead
		return fmt.Errorf("uuid not in gpuInfosByUUID: %s", uuid)
	}

	migs, err := l.GetMigInfos(gpu.GpuInfo)
	if err != nil {
		return fmt.Errorf("error getting MIG devices for %s: %w", gpu.String(), err)
	}

	if len(migs) > 0 {
		klog.V(6).Infof("Leaving MIG mode enabled for device %s (currently present MIG devices: %d)", gpu.String(), len(migs))
		return nil
	}

	klog.V(6).Infof("Attempting to disable MIG mode for device %s", gpu.String())
	t0 := time.Now()
	ret, activationStatus := nvmldev.SetMigMode(nvml.DEVICE_MIG_DISABLE)
	klog.V(6).Infof("t_disable_mig %.3f s", time.Since(t0).Seconds())
	if ret != nvml.SUCCESS {
		// activationStatus would return the appropriate error code upon unsuccessful activation
		klog.Warningf("SetMigMode activationStatus (device %s): %s", gpu.String(), activationStatus)
		// We could also log this as an error and proceed, and hope for the
		// state machine to clean this up in the future. Probably not a good
		// idea.
		return fmt.Errorf("error disabling MIG mode for device %s: %v", gpu.String(), ret)
	}
	// Note: when we're here, disabling MIG mode might still have failed.
	// `activationStatus` may reflect "in use by another client".
	klog.V(1).Infof("Called nvml.SetMigMode(nvml.DEVICE_MIG_DISABLE) for device %s, got activationStatus: %s", gpu.String(), activationStatus)
	return nil
}

// Returns a flat list of all possible physical MIG configurations for a
// specific GPU. Specifically, this discovers all possible profiles, and then
// then determines the possible placements for each profile.
func (l deviceLib) inspectMigProfilesAndPlacements(gpuInfo *GpuDeviceInfo, device nvdev.Device) ([]*MigSpec, error) {
	var infos []*MigSpec

	maxCapacities := make(PartCapacityMap)
	maxMemSlicesConsumed := 0

	err := device.VisitMigProfiles(func(migProfile nvdev.MigProfile) error {
		if migProfile.GetInfo().C != migProfile.GetInfo().G {
			return nil
		}

		if migProfile.GetInfo().CIProfileID == nvml.COMPUTE_INSTANCE_PROFILE_1_SLICE_REV1 {
			return nil
		}

		giProfileInfo, ret := device.GetGpuInstanceProfileInfo(migProfile.GetInfo().GIProfileID)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			return nil
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			return nil
		}
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GI Profile info for MIG profile %v: %w", migProfile, ret)
		}

		giPlacements, ret := device.GetGpuInstancePossiblePlacements(&giProfileInfo)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			return nil
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			return nil
		}
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GI possible placements for MIG profile %v: %w", migProfile, ret)
		}

		for _, giPlacement := range giPlacements {
			mi := &MigSpec{
				Parent:        gpuInfo,
				Profile:       migProfile,
				GIProfileInfo: giProfileInfo,
				Placement:     giPlacement,
			}
			infos = append(infos, mi)

			// Assume that the largest MIG profile consumes all memory slices,
			// and hence we can infer the memory slice count by looking at the
			// Size property of all MigPP objects, and picking the maximum.
			maxMemSlicesConsumed = max(maxMemSlicesConsumed, int(giPlacement.Size))

			// Across all MIG profiles, identify the largest value for each
			// capacity dimension. They probably all corresponding to the same
			// profile.
			caps := mi.Capacities()
			for name, cap := range caps {
				setMax(maxCapacities, name, cap)
			}
		}
		return nil
	})

	klog.V(1).Infof("%s: Per-capacity maximum across all MIG profiles+placements: %v", gpuInfo.String(), maxCapacities)
	klog.V(1).Infof("%s: Largest MIG placement size seen (maxMemSlicesConsumed): %d", gpuInfo.String(), maxMemSlicesConsumed)

	if err != nil {
		return nil, fmt.Errorf("error visiting MIG profiles: %w", err)
	}

	// Mutate the full-device information container `gpuInfo`; enrich it with
	// detail obtained from walking MIG devices. Assume that the largest MIG
	// profile seen consumes all memory slices; equate maxMemSlicesConsumed =
	// memSliceCount.
	gpuInfo.AddDetailAfterWalkingMigProfiles(maxCapacities, maxMemSlicesConsumed)
	return infos, nil
}

// Get an NVML device handle for a physical GPU. When not in DynamicMIG mode,
// this currently always calls out to NVML's DeviceGetHandleByUUID(). In
// DynamicMIG mode, this function maintains an NVML handle cache and hence
// guarantees fast lookups. This is meant to only be called for physical, full
// GPUs (not MIG devices).
func (l deviceLib) DeviceGetHandleByUUID(uuid string) (nvml.Device, nvml.Return) {
	shutdown, ret := l.ensureNVML()
	if ret != nvml.SUCCESS {
		return nil, ret
	}
	defer shutdown()

	// For now, only use long-lived NVML with cached handles when DynamicMIG is
	// enabled. In all other cases, do not cache handles (this is something we
	// may want to do in the future).
	if !featuregates.Enabled(featuregates.DynamicMIG) {
		return l.DeviceLib.DeviceGetHandleByUUID(uuid)
	}

	dev, exists := l.devhandleByUUID[uuid]
	if exists {
		return dev, nvml.SUCCESS
	}

	klog.V(6).Infof("DeviceGetHandleByUUID called for %s, cache miss", uuid)
	// Note(JP): This call can be slow. Hence, the decision to use long-lived
	// handles (at least for DynamicMIG). In theory here we need a request
	// coalescing strategy (otherwise, cache stampede is a thing in practice: a
	// burst of requests with the same UUID might be incoming in a timeframe
	// much shorter than it takes for the call below to succeed. All cache
	// misses then end up doing this expensive lookup, although it only needs to
	// be performed once). For now, I opt for addressing this by warming up the
	// cache during program startup. Given that the set of full GPUs is static
	// and that we currently have no expiry (but a long-lived map), that will
	// work.
	t0 := time.Now()
	dev, ret = l.DeviceLib.DeviceGetHandleByUUID(uuid)
	klog.V(7).Infof("t_device_get_handle_by_uuid %.3f s", time.Since(t0).Seconds())

	if ret != nvml.SUCCESS {
		return nil, ret
	}

	// Populate `devhandleByUUID` for fast lookup.
	l.devhandleByUUID[uuid] = dev
	return dev, ret
}

// Assume long-lived NVML session.
func (l deviceLib) createMigDevice(migspec *MigSpec) (*MigDeviceInfo, error) {
	gpu := migspec.Parent
	profile := migspec.Profile
	placement := &migspec.Placement

	tdhbu0 := time.Now()
	// Without handle caching, I've seen this to take up O(10 s).
	device, ret := l.DeviceGetHandleByUUID(gpu.UUID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU device handle: %v", ret)
	}
	klog.V(7).Infof("t_prep_create_mig_dev_get_dev_handle %.3f s", time.Since(tdhbu0).Seconds())

	tnd0 := time.Now()
	ndev, err := l.NewDevice(device)
	if err != nil {
		return nil, fmt.Errorf("error instantiating nvml dev: %w", err)
	}
	klog.V(7).Infof("t_prep_create_mig_dev_new_dev %.3f s", time.Since(tnd0).Seconds())

	// nvml GetMigMode distinguishes between current and pending -- not exposed
	// in go-nvlib yet. Maybe that distinction is important here.
	// migModeCurrent, migModePending, err := device.GetMigMode()
	// https://github.com/NVIDIA/go-nvlib/blame/7d260da4747c220a6972ebc83e4eb7116fc9b89a/pkg/nvlib/device/device.go#L225
	tcme0 := time.Now()
	migEnabled, err := ndev.IsMigEnabled()
	if err != nil {
		return nil, fmt.Errorf("error checking if MIG mode enabled for device %s: %w", ndev, err)
	}
	klog.V(7).Infof("t_prep_create_mig_dev_check_mig_enabled %.3f s", time.Since(tcme0).Seconds())

	logpfx := fmt.Sprintf("Create %s", migspec.CanonicalName())

	if !migEnabled {
		klog.V(6).Infof("%s: Attempting to enable MIG mode for to-be parent %s", logpfx, gpu.String())
		// If this is newer than A100 and if device unused: enable MIG.
		tem0 := time.Now()
		ret, activationStatus := device.SetMigMode(nvml.DEVICE_MIG_ENABLE)
		if ret != nvml.SUCCESS {
			// activationStatus would return the appropriate error code upon unsuccessful activation
			klog.Warningf("%s: SetMigMode activationStatus (device %s): %s", logpfx, gpu.String(), activationStatus)
			return nil, fmt.Errorf("error enabling MIG mode for device %s: %v", gpu.String(), ret)
		}
		klog.V(1).Infof("%s: MIG mode now enabled for device %s, t_enable_mig %.3f s", logpfx, gpu.String(), time.Since(tem0).Seconds())
	} else {
		klog.V(6).Infof("%s: MIG mode already enabled for device %s", logpfx, gpu.String())
	}

	profileInfo := profile.GetInfo()

	tcgigi0 := time.Now()
	giProfileInfo, ret := device.GetGpuInstanceProfileInfo(profileInfo.GIProfileID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU instance profile info for '%v': %v", profile, ret)
	}

	gi, ret := device.CreateGpuInstanceWithPlacement(&giProfileInfo, placement)

	// Ambiguity in the NVML API: when requesting a specific placement that is
	// already occupied, NVML returns NVML_ERROR_INSUFFICIENT_RESOURCES rather
	// than NVML_ERROR_ALREADY_EXISTS. Seemingly, to robustly distinguish
	// "already exists" from "blocked by something else", one cannot rely on the
	// error code alone. One must check the device state. Unrelatedly, if this
	// GPU instance already exists, it is unclear if we can and should safely
	// proceed using it. When we're here, we're not expecting it to exist -- an
	// active destruction should be performed before retrying creation. Hence,
	// for now, just return an error without distinguishing "already exists"
	// from any other type of fault.
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error creating GPU instance for '%s': %v", migspec.CanonicalName(), ret)
	}

	//giInfo, ret := gi.GetInfo()
	//if ret != nvml.SUCCESS {
	//	return nil, fmt.Errorf("error getting GPU instance info for '%s': %v", migspec.CanonicalName(), ret)
	//}

	ciProfileInfo, ret := gi.GetComputeInstanceProfileInfo(profileInfo.CIProfileID, profileInfo.CIEngProfileID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting Compute instance profile info for '%v': %v", profile, ret)
	}

	ci, ret := gi.CreateComputeInstance(&ciProfileInfo)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error creating Compute instance for '%v': %v", profile, ret)
	}

	ciInfo, ret := ci.GetInfo()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU instance info for '%v': %v", profile, ret)
	}
	klog.V(6).Infof("t_prep_create_mig_dev_cigi %.3f s", time.Since(tcgigi0).Seconds())

	// Note(JP): for obtaining the UUID of the just-created MIG device, some
	// algorithms walk through all MIG devices on the parent GPU to identify the
	// one that matches the CIID and GIID of the MIG device that was just
	// created. While that is correct, I measured that the time spent in NVML
	// API calls for 'walking all MIG devices' under load under can easily be
	// O(10 s). The UUID can also be obtained by first getting the MIG device
	// handle from the CI and then calling GetUUID() on that handle. A MIG
	// device handle maps 1:1 to a CI in NVML, so once the CI is known, the MIG
	// device handle and its UUID can be retrieved directly without scanning
	// through indices.
	uuid, ret := ciInfo.Device.GetUUID()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting UUID from CI info/device for CI %d: %v", ciInfo.Id, ret)
	}

	// Convenient access to all Mig device lists to find newly created Mig device information.
	migMap, err := l.GetMigDeviceInfos(gpu)
	if err != nil {
		return nil, err
	}

	// This now probably needs consolidation with the new types MigLiveTuple and
	// MigSpecTuple. Things get confusing.
	migDevInfo, ok := migMap[uuid]
	if !ok {
		return nil, fmt.Errorf("error getting migInfo from CI info/device for CI %d Uuid %s: %v", ciInfo.Id, uuid, ret)
	}

	klog.V(6).Infof("%s: MIG device created on %s: %+v", logpfx, gpu.String(), migDevInfo.LiveTuple())
	return migDevInfo, nil
}

// Assume long-lived NVML session.
func (l deviceLib) deleteMigDevice(miglt *MigLiveTuple) error {
	parentUUID := miglt.ParentUUID
	giId := miglt.GIID
	ciId := miglt.CIID

	t0 := time.Now()
	migStr := fmt.Sprintf("MIG(parent: %s, %+v)", parentUUID, miglt)
	klog.V(6).Infof("Delete %s", migStr)

	parentNvmlDev, ret := l.DeviceGetHandleByUUID(parentUUID)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error getting device from UUID '%v': %v", parentUUID, ret)
	}

	// The order of destroying 1) compute instance and 2) GPU instance matters.
	// These resources are hierarchical: compute instances are created inside a
	// GPU instance, so the parent GPU instance cannot be destroyed while
	// children (compute instances) still exist.
	gi, gires := parentNvmlDev.GetGpuInstanceById(giId)

	// Ref docs document this error with "If device doesn't have MIG mode
	// enabled" -- for the unlikely case that we end up in this state (MIG mode
	// was disabled out-of-band?), this should be treated as deletion success.
	if gires == nvml.ERROR_NOT_SUPPORTED {
		klog.Infof("Delete %s: GetGpuInstanceById yielded ERROR_NOT_SUPPORTED: MIG disabled, treat as success", migStr)
		return nil
	}

	// UNINITIALIZED, INVALID_ARGUMENT, NO_PERMISSION
	if gires != nvml.SUCCESS && gires != nvml.ERROR_NOT_FOUND {
		return fmt.Errorf("error getting GPU instance handle for MIG device: %v", ret)
	}

	if gires == nvml.ERROR_NOT_FOUND {
		// In this case assume that no compute instances exist (as of the GI>CI
		// hierarchy) and proceed with attempt-to-disable-MIG-mode
		klog.Infof("Delete %s: GI was not found skip CI cleanup", migStr)
		if err := l.maybeDisableMigMode(parentUUID, parentNvmlDev); err != nil {
			return fmt.Errorf("failed maybeDisableMigMode: %w", err)
		}
		return nil
	}

	// Remainder, with `gi` actually being valid.
	ci, cires := gi.GetComputeInstanceById(ciId)

	// Here we could compare the actual MIG UUID with an expected MIG UUID,
	// to be extra sure that this we want to proceed with deletion.
	// ciInfo, res := ci.GetInfo()
	// if res != nvml.SUCCESS {
	// 	return fmt.Errorf("error calling ci.GetInfo(): %v", ret)
	// }

	// actualMigUUID, res := nvml.DeviceGetUUID(ciInfo.Device)
	// if res != nvml.SUCCESS {
	// 	return fmt.Errorf("nvml.DeviceGetUUID() failed: %v", ret)
	// }
	// if actualMigUUID != expectedMigUUID {
	// 	return fmt.Errorf("UUID mismatch upon deletion: expected: %s actual: %s", expectedMigUUID, actualMigUUID)
	// }

	// Can never be `ERROR_NOT_SUPPORTED` at this point. Can be UNINITIALIZED,
	// INVALID_ARGUMENT, NO_PERMISSION: for those three, it's worth erroring out
	// here (to be retried later).
	if cires != nvml.SUCCESS && cires != nvml.ERROR_NOT_FOUND {
		return fmt.Errorf("error getting Compute instance handle for MIG device %s: %v", migStr, ret)
	}

	// A previous, partial cleanup may actually have already deleted that. Seen
	// in practice. Ignore, and proceed with deleting GPU instance below.
	if cires == nvml.ERROR_NOT_FOUND {
		klog.Infof("Delete %s: CI not found, ignore", migStr)
	} else {
		ret := ci.Destroy()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error destroying Compute instance: %v", ret)
		}
	}

	// That can for example fail with "In use by another client", in which case
	// we may have performed only a partial cleanup (CI already destroyed; seen
	// in practice).

	// Note that this operation may take O(1 s). In a machine supporting many
	// MIG devices and significant job throughput, this may become noticeable.
	// In a stressing test, I have seen the prep/unprep lock acquisition time
	// out after 10 seconds, when requests pile up.
	ret = gi.Destroy()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error destroying GPU Instance: %v", ret)
	}
	klog.V(6).Infof("t_delete_mig_device %.3f s", time.Since(t0).Seconds())

	if err := l.maybeDisableMigMode(parentUUID, parentNvmlDev); err != nil {
		return fmt.Errorf("failed maybeDisableMigMode: %w", err)
	}

	return nil
}

// FindMigDevBySpec() tests if a MIG device defined by the provided
// `MigSpecTuple` exists. If it exists, a pointer to a corresponding
// `MigLiveTuple` is returned. If it doesn't exist, a nil pointer is returned.
// if an NVML API call fails along the way, a nil pointer and a non-nil error is
// returned.
func (l deviceLib) FindMigDevBySpec(ms *MigSpecTuple) (*MigLiveTuple, error) {
	parentUUID := l.gpuUUIDbyPCIBusID[ms.ParentPCIBusID]
	parent, ret := l.DeviceGetHandleByUUID(parentUUID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("could not get device handle by UUID for %s", parentUUID)
	}

	count, _ := parent.GetMaxMigDeviceCount()

	for i := range count {
		migHandle, ret := parent.GetMigDeviceHandleByIndex(i)
		if ret != nvml.SUCCESS {
			klog.Infof("GetMigDeviceHandleByIndex ret not success")
			// Slot empty or invalid: treat as device does not currently exist.
			continue
		}

		giId, ret := migHandle.GetGpuInstanceId()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get GI ID: %v", ret)
		}

		giHandle, ret := parent.GetGpuInstanceById(giId)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get GI handle for ID %d: %v", giId, ret)
		}

		giInfo, ret := giHandle.GetInfo()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get GI info: %v", ret)
		}

		klog.V(7).Infof("FindMigDevBySpec: saw MIG dev with profile id %d and placement start %d", giInfo.ProfileId, giInfo.Placement.Start)

		if int(giInfo.ProfileId) != ms.ProfileID {
			klog.V(7).Infof("profile ID mismatch: looking for %d", ms.ProfileID)
			continue
		}

		if int(giInfo.Placement.Start) != ms.PlacementStart {
			klog.V(7).Infof("placement start mismatch: looking for %d", ms.PlacementStart)
			continue
		}

		klog.V(4).Infof("FindMigDevBySpec: match found for profile ID %d and placement start %d", giInfo.ProfileId, giInfo.Placement.Start)

		// In our way of managing MIG devices, this should always be zero --
		// nevertheless, perform the lookup. If the lookup fails, the MIG device
		// may have been created only partially (only GI, but not CI). In that
		// case, proceed.
		ciId := 0
		ciId, ret = migHandle.GetComputeInstanceId()
		if ret != nvml.SUCCESS {
			klog.V(4).Infof("FindMigDevBySpec(): failed to get CI ID: %v", ret)
		}

		uuid := ""
		uuid, ret = migHandle.GetUUID()
		if ret != nvml.SUCCESS {
			klog.V(4).Infof("FindMigDevBySpec(): failed to get MIG UUID: %v", ret)
		}

		// Found device matching the spec, return handle. For subsequent
		// deletion of a potentially partially prepared MIG device, it is OK if
		// CIID and uuid are zero values.
		mlt := MigLiveTuple{
			ParentMinor: ms.ParentMinor,
			ParentUUID:  parentUUID,
			GIID:        giId,
			CIID:        ciId,
			MigUUID:     uuid,
		}

		klog.Infof("FindMigDevBySpec result: %+v", mlt)
		return &mlt, nil
	}

	klog.Infof("Iterated through all potential MIG devs -- no candidate found")
	return nil, nil
}

// Mutate map `m` in-place: insert into map if the current QualifiedName does
// not yet exist as a key. Otherwise, update item in map if the incoming value
// `v` is larger than the one currently stored in the map.
func setMax(m map[resourceapi.QualifiedName]resourceapi.DeviceCapacity, k resourceapi.QualifiedName, v resourceapi.DeviceCapacity) {
	if cur, ok := m[k]; !ok || v.Value.Value() > cur.Value.Value() {
		m[k] = v
	}
}
