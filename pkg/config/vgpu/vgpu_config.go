package vgpu

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

//#cgo CFLAGS: -D_GNU_SOURCE
//#include <stdint.h>
//#include <sys/types.h>
//#include <sys/stat.h>
//#include <fcntl.h>
//#include <string.h>
//#include <sys/file.h>
//#include <time.h>
//#include <stdlib.h>
//#include <unistd.h>
//
//#ifndef MAX_DEVICE_COUNT
//#define MAX_DEVICE_COUNT 16
//#endif
//
//#ifndef UUID_BUFFER_SIZE
//#define UUID_BUFFER_SIZE 48
//#endif
//
//#ifndef NAME_BUFFER_SIZE
//#define NAME_BUFFER_SIZE 64
//#endif
//
//#ifndef FILENAME_MAX
//#define FILENAME_MAX 260
//#endif
//
//struct version_t {
//  int major;
//  int minor;
//};
//
//struct device_t {
//  char uuid[UUID_BUFFER_SIZE];
//  uint64_t total_memory;
//  uint64_t real_memory;
//  int hard_core;
//  int soft_core;
//  int core_limit;
//  int hard_limit;
//  int memory_limit;
//  int memory_oversold;
//  int activate;
//};
//
//struct resource_data_t {
//  struct version_t driver_version;
//  char pod_uid[UUID_BUFFER_SIZE];
//  char pod_name[NAME_BUFFER_SIZE];
//  char pod_namespace[NAME_BUFFER_SIZE];
//  char container_name[NAME_BUFFER_SIZE];
//  struct device_t devices[MAX_DEVICE_COUNT];
//  int compatibility_mode;
//  int sm_watcher;
//  int vmem_node;
//};
//
//int setting_to_disk(const char* filename, struct resource_data_t* data) {
//  int fd = 0;
//  int wsize = 0;
//  int ret = 0;
//
//  fd = open(filename, O_CREAT | O_TRUNC | O_WRONLY, 00777);
//  if (fd == -1) {
//    return 1;
//  }
//
//  wsize = (int)write(fd, (void*)data, sizeof(struct resource_data_t));
//  if (wsize != sizeof(struct resource_data_t)) {
//    ret = 1;
//	  goto DONE;
//  }
//
//DONE:
//  close(fd);
//
//  return ret;
//}
import "C"

const (
	MaxDeviceCount = C.MAX_DEVICE_COUNT
	NameBufferSize = C.NAME_BUFFER_SIZE
	UuidBufferSize = C.UUID_BUFFER_SIZE
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
}

type ResourceData struct {
	resourceCfg  *ResourceDataT
	resourceData []byte
	filePath     string
}

func (r *ResourceData) GetCfg() *ResourceDataT {
	return r.resourceCfg
}

func (r *ResourceData) Munmap() error {
	if r == nil {
		return fmt.Errorf("ResourceData is nil")
	}
	return syscall.Munmap(r.resourceData)
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

func NewResourceData(filePath string) (*ResourceData, error) {
	cfg, data, err := MmapResourceDataT(filePath)
	if err != nil {
		return nil, err
	}
	return &ResourceData{
		resourceCfg:  cfg,
		resourceData: data,
		filePath:     filePath,
	}, nil
}

func (r *ResourceDataT) DeepCopy() *ResourceDataT {
	jsonBytes, _ := json.Marshal(r)
	data := &ResourceDataT{}
	_ = json.Unmarshal(jsonBytes, data)
	return data
}

type CompatibilityMode int32

const (
	HostMode       CompatibilityMode = 0
	CGroupv1Mode   CompatibilityMode = 1
	CGroupv2Mode   CompatibilityMode = 2
	OpenKernelMode CompatibilityMode = 100
	ClientMode     CompatibilityMode = 200
)

func getCompatibilityMode(featureGate featuregate.FeatureGate) CompatibilityMode {
	mode := HostMode
	switch {
	case featureGate.Enabled(util.ClientMode):
		mode |= ClientMode
	case cgroups.IsCgroup2UnifiedMode():
		mode |= CGroupv2Mode
	case cgroups.IsCgroup2HybridMode():
		mode |= CGroupv2Mode
	default:
		mode |= CGroupv1Mode
	}
	if mode != HostMode {
		mode |= OpenKernelMode
	}
	return mode
}

func MmapResourceDataT(filePath string) (*ResourceDataT, []byte, error) {
	if err := CheckResourceDataSize(filePath); err != nil {
		klog.Errorln(err)
		return nil, nil, err
	}
	f, err := os.OpenFile(filePath, os.O_RDWR, 0666)
	if err != nil {
		klog.Errorf("Failed to open file: %s, error: %v", filePath, err)
		return nil, nil, err
	}
	defer func() {
		_ = f.Close()
	}()
	dataSize := int64(unsafe.Sizeof(ResourceDataT{}))
	data, err := syscall.Mmap(int(f.Fd()), 0, int(dataSize), syscall.PROT_WRITE|syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		klog.Errorf("Failed to mmap file: %s, error: %v", filePath, err)
		return nil, nil, err
	}
	resourceData := (*ResourceDataT)(unsafe.Pointer(&data[0]))
	return resourceData, data, nil
}

func NewResourceDataT(devManager *manager.DeviceManager, pod *corev1.Pod,
	assignDevices device.ContainerDevices, memoryOversold bool, node *corev1.Node) *ResourceDataT {
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
	computePolicy := GetComputePolicy(pod, node)

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
	for i, devInfo := range assignDevices.Devices {
		if i >= MaxDeviceCount {
			break
		}
		totalMemoryBytes := uint64(devInfo.Memory) << 20
		realMemoryBytes := totalMemoryBytes
		if ratio > 1 {
			memoryOversold = true
			realMemoryBytes = uint64(float64(realMemoryBytes) / ratio)
		}
		dev := DeviceT{
			UUID:        convert48Bytes(devInfo.Uuid),
			TotalMemory: totalMemoryBytes,
			RealMemory:  realMemoryBytes,
			HardCore:    int32(devInfo.Cores),
			SoftCore:    int32(devInfo.Cores),
			CoreLimit:   int32(0),
			HardLimit:   int32(0),
			Activate:    int32(1),
		}
		gpuDevice := deviceInfoMap[devInfo.Uuid]

		// need limit core
		switch computePolicy {
		case util.BalanceComputePolicy:
			//  int soft_core;
			dev.SoftCore = int32(gpuDevice.Core)
			// need limit core
			if devInfo.Cores > 0 && devInfo.Cores < util.HundredCore {
				//  int core_limit;
				dev.CoreLimit = 1
				if devInfo.Cores >= gpuDevice.Core {
					//  int hard_limit;
					dev.HardLimit = 1
				}
			}
		case util.FixedComputePolicy:
			// need limit core
			if devInfo.Cores > 0 && devInfo.Cores < util.HundredCore {
				//  int core_limit;
				dev.CoreLimit = 1
				//  int hard_limit;
				dev.HardLimit = 1
			}
		case util.NoneComputePolicy:
		}

		//  int memory_limit;
		if devInfo.Memory == gpuDevice.Memory && ratio == 1 {
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
		devices[gpuDevice.Id] = dev
	}
	compMode := getCompatibilityMode(devManager.GetFeatureGate())
	data := &ResourceDataT{
		DriverVersion: VersionT{
			Major: int32(major),
			Minor: int32(minor),
		},
		PodUID:            convert48Bytes(string(pod.UID)),
		PodName:           convert64Bytes(pod.Name),
		PodNamespace:      convert64Bytes(pod.Namespace),
		ContainerName:     convert64Bytes(assignDevices.Name),
		Devices:           devices,
		CompatibilityMode: int32(compMode),
		SMWatcher:         int32(smWatcher),
		VMemoryNode:       int32(vMemoryNode),
	}
	return data
}

func GetComputePolicy(pod *corev1.Pod, node *corev1.Node) util.ComputePolicy {
	computePolicy, ok := util.HasAnnotation(pod, util.VGPUComputePolicyAnnotation)
	if !ok || len(computePolicy) == 0 {
		computePolicy, _ = util.HasAnnotation(node, util.VGPUComputePolicyAnnotation)
	}
	switch strings.ToLower(computePolicy) {
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

func WriteVGPUConfigFile(filePath string, devManager *manager.DeviceManager, pod *corev1.Pod,
	assignDevices device.ContainerDevices, memoryOversold bool, node *corev1.Node) error {
	if _, err := os.Stat(filePath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		var vgpuConfig C.struct_resource_data_t
		var driverVersion C.struct_version_t
		major, minor := devManager.GetDriverVersion().CudaDriverVersion.MajorAndMinor()
		ratio := devManager.GetNodeConfig().GetDeviceMemoryScaling()

		driverVersion.major = C.int(major)
		driverVersion.minor = C.int(minor)
		vgpuConfig.driver_version = driverVersion
		compMode := getCompatibilityMode(devManager.GetFeatureGate())
		vgpuConfig.compatibility_mode = C.int(compMode)
		if devManager.GetFeatureGate().Enabled(util.SMWatcher) {
			vgpuConfig.sm_watcher = C.int(1)
		} else {
			vgpuConfig.sm_watcher = C.int(0)
		}
		if devManager.GetFeatureGate().Enabled(util.VMemoryNode) {
			vgpuConfig.vmem_node = C.int(1)
		} else {
			vgpuConfig.vmem_node = C.int(0)
		}
		podUID := C.CString(string(pod.UID))
		defer C.free(unsafe.Pointer(podUID))
		C.strcpy(&vgpuConfig.pod_uid[0], (*C.char)(unsafe.Pointer(podUID)))

		podName := C.CString(pod.Name)
		defer C.free(unsafe.Pointer(podName))
		C.strcpy(&vgpuConfig.pod_name[0], (*C.char)(unsafe.Pointer(podName)))

		podNamespace := C.CString(pod.Namespace)
		defer C.free(unsafe.Pointer(podNamespace))
		C.strcpy(&vgpuConfig.pod_namespace[0], (*C.char)(unsafe.Pointer(podNamespace)))

		containerName := C.CString(assignDevices.Name)
		defer C.free(unsafe.Pointer(containerName))
		C.strcpy(&vgpuConfig.container_name[0], (*C.char)(unsafe.Pointer(containerName)))

		computePolicy := GetComputePolicy(pod, node)

		deviceInfos := devManager.GetNodeDeviceInfo()
		deviceInfoMap := make(map[string]device.DeviceInfo, len(deviceInfos))
		for i := range deviceInfos[:min(MaxDeviceCount, len(deviceInfos))] {
			deviceInfoMap[deviceInfos[i].Uuid] = deviceInfos[i]
		}

		for i, devInfo := range assignDevices.Devices {
			if i >= C.MAX_DEVICE_COUNT {
				break
			}

			func() {
				var cDevice C.struct_device_t
				devUuid := C.CString(devInfo.Uuid)
				defer C.free(unsafe.Pointer(devUuid))
				C.strcpy((*C.char)(unsafe.Pointer(&cDevice.uuid[0])), (*C.char)(unsafe.Pointer(devUuid)))
				totalMemoryBytes := uint64(devInfo.Memory) << 20
				realMemoryBytes := totalMemoryBytes
				//  uint64_t total_memory;
				cDevice.total_memory = C.uint64_t(totalMemoryBytes)
				if ratio > 1 {
					memoryOversold = true
					realMemoryBytes = uint64(float64(realMemoryBytes) / ratio)
				}
				//  uint64_t real_memory;
				cDevice.real_memory = C.uint64_t(realMemoryBytes)
				gpuDevice := deviceInfoMap[devInfo.Uuid]
				//  int hard_core;
				cDevice.hard_core = C.int(devInfo.Cores)
				//  int soft_core;
				cDevice.soft_core = C.int(devInfo.Cores)
				//  int core_limit;
				cDevice.core_limit = 0
				//  int hard_limit;
				cDevice.hard_limit = 0
				cDevice.activate = 1
				switch computePolicy {
				case util.BalanceComputePolicy:
					//  int soft_core;
					cDevice.soft_core = C.int(gpuDevice.Core)
					// need limit core
					if devInfo.Cores > 0 && devInfo.Cores < util.HundredCore {
						//  int core_limit;
						cDevice.core_limit = 1
						if devInfo.Cores >= gpuDevice.Core {
							//  int hard_limit;
							cDevice.hard_limit = 1
						}
					}
				case util.FixedComputePolicy:
					// need limit core
					if devInfo.Cores > 0 && devInfo.Cores < util.HundredCore {
						//  int core_limit;
						cDevice.core_limit = 1
						//  int hard_limit;
						cDevice.hard_limit = 1
					}
				case util.NoneComputePolicy:
				}

				//  int memory_limit;
				if devInfo.Memory == gpuDevice.Memory && ratio == 1 {
					cDevice.memory_limit = 0
				} else {
					cDevice.memory_limit = 1
				}
				//  int memory_oversold
				if memoryOversold {
					cDevice.memory_oversold = 1
				} else {
					cDevice.memory_oversold = 0
				}
				C.memcpy(
					unsafe.Pointer(&vgpuConfig.devices[gpuDevice.Id]),
					unsafe.Pointer(&cDevice),
					C.size_t(unsafe.Sizeof(cDevice)),
				)
			}()
		}

		cFileName := C.CString(filePath)
		defer C.free(unsafe.Pointer(cFileName))
		if C.setting_to_disk(cFileName, &vgpuConfig) != 0 {
			return fmt.Errorf("can't sink config %s", filePath)
		}
	}
	return nil
}
