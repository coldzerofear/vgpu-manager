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
	corev1 "k8s.io/api/core/v1"
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
//#ifndef FILENAME_MAX
//#define FILENAME_MAX 260
//#endif
//
//struct version_t {
//  int major;
//  int minor;
//} __attribute__((packed, aligned(8)));
//
//struct device_t {
//  char uuid[48];
//  uint64_t total_memory;
//  int hard_core;
//  int soft_core;
//  int core_limit;
//  int hard_limit;
//  int memory_limit;
//  int memory_oversold;
//} __attribute__((packed, aligned(8)));
//
//struct resource_data_t {
//  struct version_t driver_version;
//  char pod_uid[48];
//  char pod_name[64];
//  char pod_namespace[64];
//  char container_name[64];
//  struct device_t devices[MAX_DEVICE_COUNT];
//  int device_count;
//} __attribute__((packed, aligned(8)));
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
//    ret = 2;
//	goto DONE;
//  }
//
//DONE:
//  close(fd);
//
//  return ret;
//}
import "C"

type VersionT struct {
	Major int32
	Minor int32
}

type DeviceT struct {
	UUID           [48]byte
	TotalMemory    uint64
	HardCore       int32
	SoftCore       int32
	CoreLimit      int32
	HardLimit      int32
	MemoryLimit    int32
	MemoryOversold int32
}

type ResourceDataT struct {
	DriverVersion VersionT
	PodUID        [48]byte
	PodName       [64]byte
	PodNamespace  [64]byte
	ContainerName [64]byte
	Devices       [util.MaxDeviceNumber]DeviceT
	DeviceCount   int32
}

func (r *ResourceDataT) DeepCopy() *ResourceDataT {
	jsonBytes, _ := json.Marshal(r)
	data := &ResourceDataT{}
	_ = json.Unmarshal(jsonBytes, data)
	return data
}

func MmapResourceDataT(filePath string) (*ResourceDataT, []byte, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		klog.Errorf("Failed to stat file: %s, error: %v", filePath, err)
		return nil, nil, err
	}
	dataSize := int64(unsafe.Sizeof(ResourceDataT{}))
	if fileInfo.Size() != dataSize {
		klog.Errorf("File size mismatch, expected: %d, actual: %d", dataSize, fileInfo.Size())
		return nil, nil, fmt.Errorf("file size mismatch")
	}
	f, err := os.OpenFile(filePath, os.O_RDWR, 0666)
	if err != nil {
		klog.Errorf("Failed to open file: %s, error: %v", filePath, err)
		return nil, nil, err
	}
	defer f.Close()
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
	major, minor := devManager.GetVersion().CudaVersion.MajorAndMinor()
	convert48Bytes := func(val string) [48]byte {
		var byteArray [48]byte
		copy(byteArray[:], val)
		return byteArray
	}
	convert64Bytes := func(val string) [64]byte {
		var byteArray [64]byte
		copy(byteArray[:], val)
		return byteArray
	}
	computePolicy := GetComputePolicy(pod, node)
	deviceCount := 0
	deviceMap := devManager.GetDeviceMap()
	nodeConfig := devManager.GetNodeConfig()
	devices := [util.MaxDeviceNumber]DeviceT{}
	for i, devInfo := range assignDevices.Devices {
		if i >= util.MaxDeviceNumber {
			break
		}
		deviceCount++
		dev := DeviceT{
			UUID:        convert48Bytes(devInfo.Uuid),
			TotalMemory: uint64(devInfo.Memory << 20),
			HardCore:    int32(devInfo.Core),
			SoftCore:    int32(devInfo.Core),
			CoreLimit:   int32(0),
			HardLimit:   int32(0),
		}
		gpuDevice := deviceMap[devInfo.Uuid]

		// need limit core
		switch computePolicy {
		case util.BalanceComputePolicy:
			//  int soft_core;
			dev.SoftCore = int32(gpuDevice.Core)
			// need limit core
			if devInfo.Core > 0 && devInfo.Core < util.HundredCore {
				//  int core_limit;
				dev.CoreLimit = 1
				if devInfo.Core >= gpuDevice.Core {
					//  int hard_limit;
					dev.HardLimit = 1
				}
			}
		case util.FixedComputePolicy:
			// need limit core
			if devInfo.Core > 0 && devInfo.Core < util.HundredCore {
				//  int core_limit;
				dev.CoreLimit = 1
				//  int hard_limit;
				dev.HardLimit = 1
			}
		case util.NoneComputePolicy:
		}

		//  int memory_limit;
		if devInfo.Memory == gpuDevice.Memory && nodeConfig.DeviceMemoryScaling() == float64(1) {
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
		devices[i] = dev
	}
	data := &ResourceDataT{
		DriverVersion: VersionT{
			Major: int32(major),
			Minor: int32(minor),
		},
		PodUID:        convert48Bytes(string(pod.UID)),
		PodName:       convert64Bytes(pod.Name),
		PodNamespace:  convert64Bytes(pod.Namespace),
		ContainerName: convert64Bytes(assignDevices.Name),
		Devices:       devices,
		DeviceCount:   int32(deviceCount),
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
		major, minor := devManager.GetVersion().CudaVersion.MajorAndMinor()
		driverVersion.major = C.int(major)
		driverVersion.minor = C.int(minor)
		vgpuConfig.driver_version = driverVersion

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

		deviceCount := 0
		deviceMap := devManager.GetDeviceMap()
		nodeConfig := devManager.GetNodeConfig()
		for i, devInfo := range assignDevices.Devices {
			if i >= C.MAX_DEVICE_COUNT {
				break
			}
			deviceCount++
			func() {
				var cDevice C.struct_device_t
				devUuid := C.CString(devInfo.Uuid)
				defer C.free(unsafe.Pointer(devUuid))
				C.strcpy((*C.char)(unsafe.Pointer(&cDevice.uuid[0])), (*C.char)(unsafe.Pointer(devUuid)))
				//  uint64_t total_memory;
				cDevice.total_memory = C.uint64_t(devInfo.Memory << 20)

				gpuDevice := deviceMap[devInfo.Uuid]
				//  int hard_core;
				cDevice.hard_core = C.int(devInfo.Core)
				//  int soft_core;
				cDevice.soft_core = C.int(devInfo.Core)
				//  int core_limit;
				cDevice.core_limit = 0
				//  int hard_limit;
				cDevice.hard_limit = 0
				switch computePolicy {
				case util.BalanceComputePolicy:
					//  int soft_core;
					cDevice.soft_core = C.int(gpuDevice.Core)
					// need limit core
					if devInfo.Core > 0 && devInfo.Core < util.HundredCore {
						//  int core_limit;
						cDevice.core_limit = 1
						if devInfo.Core >= gpuDevice.Core {
							//  int hard_limit;
							cDevice.hard_limit = 1
						}
					}
				case util.FixedComputePolicy:
					// need limit core
					if devInfo.Core > 0 && devInfo.Core < util.HundredCore {
						//  int core_limit;
						cDevice.core_limit = 1
						//  int hard_limit;
						cDevice.hard_limit = 1
					}
				case util.NoneComputePolicy:
				}
				//  int memory_limit;
				if devInfo.Memory == gpuDevice.Memory && nodeConfig.DeviceMemoryScaling() == float64(1) {
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
					unsafe.Pointer(&vgpuConfig.devices[i]),
					unsafe.Pointer(&cDevice),
					C.size_t(unsafe.Sizeof(cDevice)),
				)
			}()
		}
		vgpuConfig.device_count = C.int(deviceCount)

		cFileName := C.CString(filePath)
		defer C.free(unsafe.Pointer(cFileName))
		if C.setting_to_disk(cFileName, &vgpuConfig) != 0 {
			return fmt.Errorf("can't sink config %s", filePath)
		}
	}
	return nil
}
