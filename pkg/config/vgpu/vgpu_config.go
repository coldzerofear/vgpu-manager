package vgpu

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"unsafe"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/opencontainers/cgroups"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// These sizes are the shared config-file ABI mirrored from the C side
// (library resource_data_t / device_t). MaxDeviceCount is the shared
// MAX_DEVICE_COUNT; the struct layout is asserted in vgpu_config_test.go.
const (
	MaxDeviceCount = util.MaxDeviceCount
	NameBufferSize = 64
	UuidBufferSize = 48
)

type VersionT struct {
	Major int32
	Minor int32
}

type DeviceT struct {
	UUID           [UuidBufferSize]byte
	TotalMemory    uint64
	RealMemory     uint64
	HardCore       int32
	SoftCore       int32
	CoreLimit      int32
	HardLimit      int32
	MemoryLimit    int32
	MemoryOversold int32
	Activate       int32
}

type ResourceDataT struct {
	DriverVersion     VersionT
	PodUID            [UuidBufferSize]byte
	PodName           [NameBufferSize]byte
	PodNamespace      [NameBufferSize]byte
	ContainerName     [NameBufferSize]byte
	Devices           [MaxDeviceCount]DeviceT
	CompatibilityMode int32
	SMWatcher         int32
	VMemoryNode       int32
	RegisterUUID      [UuidBufferSize]byte
}

type MmapResourceData struct {
	resource *ResourceDataT
	mmapFile *util.MmapFile
	mutex    sync.Mutex
}

func (r *MmapResourceData) GetResource() *ResourceDataT {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.resource
}

// CopyResource returns a deep copy taken while holding the lock, so the read is
// safe against a concurrent Reload/Close munmapping the mapping. Callers that
// only need a snapshot (not the live pointer) must use this, not GetResource:
// GetResource hands out a pointer into the mmap and releases the lock, so
// dereferencing it afterwards races with Reload's munmap.
func (r *MmapResourceData) CopyResource() *ResourceDataT {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.resource.DeepCopy()
}

func (r *MmapResourceData) Close() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.mmapFile.Close()
}

func (r *MmapResourceData) NeedsReload() (reload bool, err error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return r.mmapFile.NeedsReload()
}

func (r *MmapResourceData) Reload() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	data, err := NewMmapResourceData(r.mmapFile.Path)
	if err != nil {
		return fmt.Errorf("reload %q failed: %w", r.mmapFile.Path, err)
	}
	_ = r.mmapFile.Close()
	r.resource = data.resource
	r.mmapFile = data.mmapFile
	return nil
}

func CheckResourceDataSize(filePath string) error {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	dataSize := int64(unsafe.Sizeof(ResourceDataT{}))
	if fileInfo.Size() != dataSize {
		return fmt.Errorf("vGPU config file size mismatch, expected: %d, actual: %d", dataSize, fileInfo.Size())
	}
	return nil
}

func NewMmapResourceData(filePath string) (*MmapResourceData, error) {
	mmapFile, err := util.OpenMmap(filePath, util.DefaultReadWriteMmap)
	if err != nil {
		return nil, err
	}
	dataSize := int64(unsafe.Sizeof(ResourceDataT{}))
	if mmapFile.FileInfo.Size() != dataSize {
		klog.Errorf("File size mismatch, expected: %d, actual: %d", dataSize, mmapFile.FileInfo.Size())
		_ = mmapFile.Close()
		return nil, fmt.Errorf("vGPU config file size mismatch")
	}
	data := (*ResourceDataT)(unsafe.Pointer(&mmapFile.Data[0]))
	return &MmapResourceData{
		resource: data,
		mmapFile: mmapFile,
	}, nil
}

func (r *ResourceDataT) DeepCopy() *ResourceDataT {
	jsonBytes, _ := json.Marshal(r)
	data := &ResourceDataT{}
	_ = json.Unmarshal(jsonBytes, data)
	return data
}

func GetCompatibilityMode(devManager *manager.DeviceManager) util.CompatibilityMode {
	mode := util.HostMode
	switch {
	case devManager.GetFeatureGate().Enabled(util.ClientMode):
		mode |= util.ClientRegMode
	case cgroups.IsCgroup2UnifiedMode():
		mode |= util.CGroupv2Mode
	case cgroups.IsCgroup2HybridMode():
		mode |= util.CGroupv2Mode
	default:
		mode |= util.CGroupv1Mode
	}
	if devManager.GetNodeConfig().GetOpenKernelModules() {
		mode |= util.OpenKernelMode
	}
	return mode
}

func NewResourceDataT(
	devManager *manager.DeviceManager, pod *corev1.Pod,
	containerClaim device.ContainerDeviceClaim,
	memoryOversold bool, node *corev1.Node,
) *ResourceDataT {
	major, minor := devManager.GetDriverVersion().CudaDriverVersion.MajorAndMinor()
	ratio := devManager.GetNodeConfig().GetDeviceMemoryScaling()
	convert48Bytes := func(val string) [UuidBufferSize]byte {
		var byteArray [UuidBufferSize]byte
		copy(byteArray[:], val)
		return byteArray
	}
	convert64Bytes := func(val string) [NameBufferSize]byte {
		var byteArray [NameBufferSize]byte
		copy(byteArray[:], val)
		return byteArray
	}
	computePolicy := GetDefaultComputePolicy(pod, node)

	deviceInfos := devManager.GetNodeDeviceInfo()
	deviceInfoMap := make(map[string]device.DeviceInfo, len(deviceInfos))
	for i := range deviceInfos[:min(MaxDeviceCount, len(deviceInfos))] {
		deviceInfoMap[deviceInfos[i].Uuid] = deviceInfos[i]
	}

	smWatcher := 0
	if devManager.GetFeatureGate().Enabled(util.SMWatcher) {
		smWatcher = 1
	}
	vMemoryNode := 0
	if devManager.GetFeatureGate().Enabled(util.VMemoryNode) {
		vMemoryNode = 1
	}
	devices := [MaxDeviceCount]DeviceT{}
	for i, claim := range containerClaim.DeviceClaims {
		if i >= MaxDeviceCount {
			break
		}
		totalMemoryBytes := uint64(claim.Memory) << 20
		realMemoryBytes := totalMemoryBytes
		if ratio > 1 {
			memoryOversold = true
			realMemoryBytes = uint64(float64(realMemoryBytes) / ratio)
		}
		dev := DeviceT{
			UUID:        convert48Bytes(claim.Uuid),
			TotalMemory: totalMemoryBytes,
			RealMemory:  realMemoryBytes,
			HardCore:    int32(claim.Cores),
			SoftCore:    int32(claim.Cores),
			CoreLimit:   int32(0),
			HardLimit:   int32(0),
			Activate:    int32(1),
		}
		gpuDevice := deviceInfoMap[claim.Uuid]

		// need limit core
		switch computePolicy {
		case util.BalanceComputePolicy:
			//  int soft_core;
			dev.SoftCore = int32(gpuDevice.Core)
			// need limit core
			if claim.Cores > 0 && claim.Cores < util.HundredCore {
				//  int core_limit;
				dev.CoreLimit = 1
				if claim.Cores >= gpuDevice.Core {
					//  int hard_limit;
					dev.HardLimit = 1
				}
			}
		case util.FixedComputePolicy:
			// need limit core
			if claim.Cores > 0 && claim.Cores < util.HundredCore {
				//  int core_limit;
				dev.CoreLimit = 1
				//  int hard_limit;
				dev.HardLimit = 1
			}
		case util.NoneComputePolicy:
		}

		//  int memory_limit;
		if claim.Memory == gpuDevice.Memory && ratio == 1 {
			dev.MemoryLimit = 0
		} else {
			dev.MemoryLimit = 1
		}
		//  int memory_oversold
		if memoryOversold {
			dev.MemoryOversold = 1
		} else {
			dev.MemoryOversold = 0
		}
		// gpuDevice.Id is the host device index; the shared-memory layout only
		// has MaxDeviceCount slots. Guard the write so a node with more GPUs than
		// that cannot index out of range (the old cgo path did a silent OOB
		// memcpy here instead).
		if gpuDevice.Id < 0 || gpuDevice.Id >= MaxDeviceCount {
			klog.Warningf("Device host index %d out of range [0, %d), skip", gpuDevice.Id, MaxDeviceCount)
			continue
		}
		devices[gpuDevice.Id] = dev
	}
	compMode := GetCompatibilityMode(devManager)
	data := &ResourceDataT{
		DriverVersion: VersionT{
			Major: int32(major),
			Minor: int32(minor),
		},
		PodUID:            convert48Bytes(string(pod.UID)),
		PodName:           convert64Bytes(pod.Name),
		PodNamespace:      convert64Bytes(pod.Namespace),
		ContainerName:     convert64Bytes(containerClaim.Name),
		Devices:           devices,
		CompatibilityMode: int32(compMode),
		SMWatcher:         int32(smWatcher),
		VMemoryNode:       int32(vMemoryNode),
		RegisterUUID:      convert48Bytes(""),
	}
	return data
}

func GetDefaultComputePolicy(pod *corev1.Pod, node *corev1.Node) util.ComputePolicy {
	computePolicy, ok := util.HasAnnotation(pod, util.VGPUComputePolicyAnnotation)
	if !ok || len(computePolicy) == 0 {
		computePolicy, _ = util.HasAnnotation(node, util.VGPUComputePolicyAnnotation)
	}
	return GetComputePolicy(computePolicy)
}

func GetComputePolicy(policy string) util.ComputePolicy {
	switch strings.ToLower(policy) {
	case string(util.BalanceComputePolicy):
		return util.BalanceComputePolicy
	case string(util.FixedComputePolicy):
		return util.FixedComputePolicy
	case string(util.NoneComputePolicy):
		return util.NoneComputePolicy
	default:
		return util.FixedComputePolicy
	}
}

// writeResourceDataToDisk writes the fixed-size ResourceDataT to filePath as a
// raw byte image, matching the C setting_to_disk (O_CREAT|O_TRUNC|O_WRONLY,
// mode 0777). The Go struct layout is byte-for-byte identical to the C
// resource_data_t (asserted by CheckResourceDataSize and the mmap round-trip
// test), so the bytes are interchangeable with the C reader.
func writeResourceDataToDisk(filePath string, data *ResourceDataT) error {
	size := int(unsafe.Sizeof(ResourceDataT{}))
	buf := unsafe.Slice((*byte)(unsafe.Pointer(data)), size)
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0777)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
	}()
	n, err := f.Write(buf)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("short write for config %s: wrote %d of %d bytes", filePath, n, size)
	}
	return nil
}

func WriteVGPUConfigFile(filePath string, devManager *manager.DeviceManager, pod *corev1.Pod,
	containerClaim device.ContainerDeviceClaim, memoryOversold bool, node *corev1.Node) error {
	if _, err := os.Stat(filePath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data := NewResourceDataT(devManager, pod, containerClaim, memoryOversold, node)
		if err = writeResourceDataToDisk(filePath, data); err != nil {
			return fmt.Errorf("can't sink config %s: %w", filePath, err)
		}
	}
	return nil
}
