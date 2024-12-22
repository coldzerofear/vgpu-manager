package vgpu

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/version"
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
//  uint64_t device_memory;
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
	DeviceMemory   uint64
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
	json.Unmarshal(jsonBytes, data)
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

func NewResourceDataT(version version.Version, pod *corev1.Pod, assignDevices device.ContainerDevices) *ResourceDataT {
	major, minor := version.CudaVersion.MajorAndMinor()
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
	deviceCount := 0
	devices := [util.MaxDeviceNumber]DeviceT{}
	for i, devInfo := range assignDevices.Devices {
		if i >= util.MaxDeviceNumber {
			break
		}
		deviceCount++
		dev := DeviceT{
			UUID:         convert48Bytes(devInfo.Uuid),
			TotalMemory:  uint64(devInfo.Memory << 20),
			DeviceMemory: uint64(devInfo.Memory << 20),
			HardCore:     int32(devInfo.Core),
			SoftCore:     int32(devInfo.Core),
		}
		// need limit core
		if devInfo.Core > 0 && devInfo.Core < util.HundredCore {
			//  int core_limit;
			dev.CoreLimit = 1
			//  int hard_limit;
			dev.HardLimit = 1
		} else {
			//  int core_limit;
			dev.CoreLimit = 0
			//  int hard_limit;
			dev.HardLimit = 0
		}
		//  int memory_limit;
		dev.MemoryLimit = 1
		//  int memory_oversold
		dev.MemoryOversold = 0
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

func WriteVGPUConfigFile(filePath string, version version.Version, pod *corev1.Pod, assignDevices device.ContainerDevices) error {
	if _, err := os.Stat(filePath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		var vgpuConfig C.struct_resource_data_t
		var driverVersion C.struct_version_t
		major, minor := version.CudaVersion.MajorAndMinor()
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

		deviceCount := 0
		for i, devInfo := range assignDevices.Devices {
			if i >= C.MAX_DEVICE_COUNT {
				break // 防止溢出
			}
			deviceCount++
			func() {
				var cDevice C.struct_device_t
				devUuid := C.CString(devInfo.Uuid)
				defer C.free(unsafe.Pointer(devUuid))
				C.strcpy((*C.char)(unsafe.Pointer(&cDevice.uuid[0])), (*C.char)(unsafe.Pointer(devUuid)))
				//  uint64_t total_memory;
				cDevice.total_memory = C.uint64_t(devInfo.Memory << 20)
				//  uint64_t device_memory;
				cDevice.device_memory = C.uint64_t(devInfo.Memory << 20)

				//  int hard_core;
				cDevice.hard_core = C.int(devInfo.Core)
				//  int soft_core;
				cDevice.soft_core = C.int(devInfo.Core)
				// need limit core
				if devInfo.Core > 0 && devInfo.Core < util.HundredCore {
					//  int core_limit;
					cDevice.core_limit = 1
					//  int hard_limit;
					cDevice.hard_limit = 1
				} else {
					//  int core_limit;
					cDevice.core_limit = 0
					//  int hard_limit;
					cDevice.hard_limit = 0
				}

				//  int memory_limit;
				cDevice.memory_limit = 1
				//  int memory_oversold
				cDevice.memory_oversold = 0
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
