package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"
)

//#include <stdio.h>
//#include <stdint.h>
//#include <sys/types.h>
//#include <sys/stat.h>
//#include <fcntl.h>
//#include <string.h>
//#include <sys/file.h>
//#include <time.h>
//#include <stdlib.h>
//#include <unistd.h>
//#include <sys/mman.h>
//
//#ifndef MAX_DEVICE_COUNT
//#define MAX_DEVICE_COUNT 16
//#endif
//
//#ifndef MAX_PIDS
//#define MAX_PIDS 1024
//#endif
//
//struct nvmlProcessUtilizationSample_t {
//  unsigned int pid;             //!< PID of process
//  unsigned long long timeStamp; //!< CPU Timestamp in microseconds
//  unsigned int smUtil;          //!< SM (3D/Compute) Util Value
//  unsigned int memUtil;         //!< Frame Buffer Memory Util Value
//  unsigned int encUtil;         //!< Encoder Util Value
//  unsigned int decUtil;         //!< Decoder Util Value
//};
//
//struct nvmlProcessInfoV2_t {
//  unsigned int pid;
//  unsigned long long usedGpuMemory;
//  unsigned int  gpuInstanceId;
//  unsigned int  computeInstanceId;
//};
//
//struct device_process_t {
//  struct nvmlProcessUtilizationSample_t process_util_samples[MAX_PIDS];
//  unsigned int process_util_samples_size;
//  unsigned long long lastSeenTimeStamp;
//  struct nvmlProcessInfoV2_t compute_processes[MAX_PIDS];
//  unsigned int compute_processes_size;
//  struct nvmlProcessInfoV2_t graphics_processes[MAX_PIDS];
//  unsigned int graphics_processes_size;
//  unsigned char lock_byte;
//};
//
//struct device_util_t {
//  struct device_process_t devices[MAX_DEVICE_COUNT];
//};
//
//#define GET_DEVICE_LOCK_OFFSET(device_index) \
//  offsetof(struct device_util_t, devices[device_index].lock_byte)
//
//int device_util_read_lock(int ordinal, const char* filepath) {
//  if (ordinal >= MAX_DEVICE_COUNT) {
//    return -1;
//  }
//  int fd = open(filepath, O_RDONLY | O_CLOEXEC);
//  if (fd == -1) {
//    return -1;
//  }
//  struct flock lock;
//  lock.l_type = F_RDLCK;
//  lock.l_whence = SEEK_SET;
//  lock.l_start = GET_DEVICE_LOCK_OFFSET(ordinal);
//  lock.l_len = 1;
//  lock.l_pid = 0;
//  if (fcntl(fd, F_SETLKW, &lock) == -1) {
//    close(fd);
//    return -1;
//  }
//  return fd;
//}
//
//int device_util_write_lock(int ordinal, const char* filepath) {
//  if (ordinal >= MAX_DEVICE_COUNT) {
//    return -1;
//  }
//  int fd = open(filepath, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
//  if (fd == -1) {
//    return -1;
//  }
//  struct flock lock;
//  lock.l_type = F_WRLCK;
//  lock.l_whence = SEEK_SET;
//  lock.l_start = GET_DEVICE_LOCK_OFFSET(ordinal);
//  lock.l_len = 1;
//  lock.l_pid = 0;
//  if (fcntl(fd, F_SETLKW, &lock) == -1) {
//    close(fd);
//    return -1;
//  }
//  return fd;
//}
//
//void device_util_unlock(int fd, int ordinal) {
//  if (fd < 0) return;
//  struct flock lock;
//  lock.l_type = F_UNLCK;
//  lock.l_whence = SEEK_SET;
//  lock.l_start = GET_DEVICE_LOCK_OFFSET(ordinal);
//  lock.l_len = 1;
//  lock.l_pid = 0;
//  fcntl(fd, F_SETLK, &lock);
//  close(fd);
//}
//
import "C"

const (
	MAX_PIDS         = C.MAX_PIDS
	MAX_DEVICE_COUNT = C.MAX_DEVICE_COUNT
)

type DeviceProcessT struct {
	ProcessUtilSamples     [MAX_PIDS]nvml.ProcessUtilizationSample
	ProcessUtilSamplesSize uint32
	LastSeenTimeStamp      uint64
	ComputeProcesses       [MAX_PIDS]nvml.ProcessInfo
	ComputeProcessesSize   uint32
	GraphicsProcesses      [MAX_PIDS]nvml.ProcessInfo
	GraphicsProcessesSize  uint32
	LockByte               uint8
}

type DeviceUtilT struct {
	Devices [MAX_DEVICE_COUNT]DeviceProcessT
}

type DeviceUtil struct {
	deviceUtil *DeviceUtilT
	deviceData []byte
	filePath   string
	fd         int
}

func (d *DeviceUtil) GetUtil() *DeviceUtilT {
	return d.deviceUtil
}

func (d *DeviceUtil) RLock(ordinal int) error {
	if d == nil {
		return fmt.Errorf("DeviceUtil is nil")
	}
	if len(d.filePath) == 0 || ordinal < 0 || ordinal >= MAX_DEVICE_COUNT {
		return fmt.Errorf("invalid parameter, filepath=%s, device=%d", d.filePath, ordinal)
	}
	fd, err := DeviceUtilRLock(ordinal, d.filePath)
	if err != nil {
		return err
	}
	d.fd = fd
	return nil
}

func (d *DeviceUtil) WLock(ordinal int) error {
	if d == nil {
		return fmt.Errorf("DeviceUtil is nil")
	}
	if len(d.filePath) == 0 || ordinal < 0 || ordinal >= MAX_DEVICE_COUNT {
		return fmt.Errorf("invalid parameter, filepath=%s, device=%d", d.filePath, ordinal)
	}
	fd, err := DeviceUtilWLock(ordinal, d.filePath)
	if err != nil {
		return err
	}
	d.fd = fd
	return nil
}

func (d *DeviceUtil) Unlock(ordinal int) error {
	if d == nil {
		return fmt.Errorf("DeviceUtil is nil")
	}
	if d.fd < 0 || ordinal < 0 || ordinal >= MAX_DEVICE_COUNT {
		return fmt.Errorf("invalid parameter, fd=%d, device=%d", d.fd, ordinal)
	}
	DeviceUtilUnlock(d.fd, ordinal)
	return nil
}

func (d *DeviceUtil) Munmap(msync bool) error {
	if d == nil {
		return fmt.Errorf("DeviceUtil is nil")
	}
	if msync {
		_, _, errno := syscall.Syscall(
			syscall.SYS_MSYNC,
			uintptr(unsafe.Pointer(&d.deviceData[0])),
			uintptr(len(d.deviceData)),
			uintptr(syscall.MS_SYNC),
		)
		if errno != 0 {
			klog.V(3).ErrorS(fmt.Errorf("msync error: %d", errno), "failed to sync mmap")
		}
	}
	return syscall.Munmap(d.deviceData)
}

func NewDeviceUtil(filePath string) (*DeviceUtil, error) {
	util, data, err := MmapDeviceUtilT(filePath)
	if err != nil {
		return nil, err
	}
	return &DeviceUtil{
		deviceUtil: util,
		deviceData: data,
		filePath:   filePath,
		fd:         -1,
	}, nil
}

func CreateDeviceUtilFile(filePath string) error {
	f, err := os.Create(filePath)
	if err != nil {
		klog.Errorf("Failed to create file: %s, error: %v", filePath, err)
		return err
	}
	defer func() {
		_ = f.Close()
	}()

	dataSize := int64(unsafe.Sizeof(DeviceUtilT{}))
	err = f.Truncate(dataSize)
	if err != nil {
		klog.Errorf("Failed to truncate file: %s, error: %v", filePath, err)
		return err
	}
	if err = f.Sync(); err != nil {
		klog.Warningf("Failed to sync file: %s, error: %v", filePath, err)
	}
	return nil
}

func PrepareDeviceUtilFile(filePath string) error {
	dirPath := filepath.Dir(filePath)
	_ = os.MkdirAll(dirPath, 0755)
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err = CreateDeviceUtilFile(filePath); err != nil {
			klog.Errorf("Failed to create file: %s, error: %v", filePath, err)
			return err
		}
		fileInfo, err = os.Stat(filePath)
		if err != nil {
			return err
		}
	}
	dataSize := int64(unsafe.Sizeof(DeviceUtilT{}))

	if fileInfo.Size() == dataSize {
		klog.Infof("File %s already exists with correct size", filePath)
		return nil
	}

	klog.Warningf("File %s exists but size mismatch (%d != %d), deleting",
		filePath, fileInfo.Size(), dataSize)
	if err = os.Remove(filePath); err != nil {
		klog.Errorf("Failed to remove file: %s, error: %v", filePath, err)
		return err
	}
	if err = CreateDeviceUtilFile(filePath); err != nil {
		klog.Errorf("Failed to create file: %s, error: %v", filePath, err)
		return err
	}
	return nil
}

func MmapDeviceUtilT(filePath string) (*DeviceUtilT, []byte, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		klog.Errorf("Failed to stat file: %s, error: %v", filePath, err)
		return nil, nil, err
	}
	dataSize := int64(unsafe.Sizeof(DeviceUtilT{}))
	if fileInfo.Size() != dataSize {
		klog.Errorf("File size mismatch, expected: %d, actual: %d", dataSize, fileInfo.Size())
		return nil, nil, fmt.Errorf("file size mismatch")
	}
	f, err := os.OpenFile(filePath, os.O_RDWR, 0666)
	if err != nil {
		klog.Errorf("Failed to open file: %s, error: %v", filePath, err)
		return nil, nil, err
	}
	defer func() {
		_ = f.Close()
	}()
	data, err := syscall.Mmap(int(f.Fd()), 0, int(dataSize), syscall.PROT_WRITE|syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		klog.Errorf("Failed to mmap file: %s, error: %v", filePath, err)
		return nil, nil, err
	}
	resourceData := (*DeviceUtilT)(unsafe.Pointer(&data[0]))
	return resourceData, data, nil
}

// DeviceUtilRLock get device util file read lock
func DeviceUtilRLock(ordinal int, filepath string) (int, error) {
	cFilePath := C.CString(filepath)
	defer C.free(unsafe.Pointer(cFilePath))

	fd := C.device_util_read_lock(C.int(ordinal), cFilePath)
	if fd == -1 {
		return -1, fmt.Errorf("failed to acquire lock for device %d at path %s", ordinal, filepath)
	}
	return int(fd), nil
}

// DeviceUtilWLock get device util file write lock
func DeviceUtilWLock(ordinal int, filepath string) (int, error) {
	cFilePath := C.CString(filepath)
	defer C.free(unsafe.Pointer(cFilePath))

	fd := C.device_util_write_lock(C.int(ordinal), cFilePath)
	if fd == -1 {
		return -1, fmt.Errorf("failed to acquire lock for device %d at path %s", ordinal, filepath)
	}
	return int(fd), nil
}

// DeviceUtilUnlock unlock device util file
func DeviceUtilUnlock(fd int, ordinal int) {
	C.device_util_unlock(C.int(fd), C.int(ordinal))
}
