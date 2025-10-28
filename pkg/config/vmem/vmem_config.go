package vmem

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

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
//struct process_used_t {
//  int pid;
//  size_t used;
//};
//
//struct device_vmem_used_t {
//  struct process_used_t processes[MAX_PIDS];
//  unsigned int processes_size;
//  unsigned char lock_byte;
//};
//
//struct device_vmemory_t {
//  struct device_vmem_used_t devices[MAX_DEVICE_COUNT];
//};
//
//#define GET_VMEMORY_LOCK_OFFSET(device_index) \
//  offsetof(struct device_vmemory_t, devices[device_index].lock_byte)
//
//int device_vmem_read_lock(int ordinal, const char* filepath) {
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
//  lock.l_start = GET_VMEMORY_LOCK_OFFSET(ordinal);
//  lock.l_len = 1;
//  lock.l_pid = 0;
//  if (fcntl(fd, F_SETLKW, &lock) == -1) {
//    close(fd);
//    return -1;
//  }
//  return fd;
//}
//
//void device_vmem_unlock(int fd, int ordinal) {
//  if (fd < 0) return;
//  struct flock lock;
//  lock.l_type = F_UNLCK;
//  lock.l_whence = SEEK_SET;
//  lock.l_start = GET_VMEMORY_LOCK_OFFSET(ordinal);
//  lock.l_len = 1;
//  lock.l_pid = 0;
//  fcntl(fd, F_SETLK, &lock);
//  close(fd);
//}
//
import "C"

const (
	MaxPids        = C.MAX_PIDS
	MaxDeviceCount = C.MAX_DEVICE_COUNT
)

type ProcessUsedT struct {
	Pid  int32
	Used uint64
}

type DeviceVMemUsedT struct {
	Processes     [MaxPids]ProcessUsedT
	ProcessesSize uint32
	LockByte      uint8
}

type DeviceVMemoryT struct {
	Devices [C.MAX_DEVICE_COUNT]DeviceVMemUsedT
}

type DeviceVMemory struct {
	deviceVMem *DeviceVMemoryT
	deviceData []byte
	filePath   string
	fd         int
}

func (d *DeviceVMemory) GetVMem() *DeviceVMemoryT {
	return d.deviceVMem
}

func (d *DeviceVMemory) RLock(ordinal int) error {
	if d == nil {
		return fmt.Errorf("DeviceVMemory is nil")
	}
	if len(d.filePath) == 0 || ordinal < 0 || ordinal >= MaxDeviceCount {
		return fmt.Errorf("invalid parameter, filepath=%s, device=%d", d.filePath, ordinal)
	}
	fd, err := DeviceVMemRLock(ordinal, d.filePath)
	if err != nil {
		return err
	}
	d.fd = fd
	return nil
}

func (d *DeviceVMemory) Unlock(ordinal int) error {
	if d == nil {
		return fmt.Errorf("DeviceVMemory is nil")
	}
	if d.fd < 0 || ordinal < 0 || ordinal >= MaxDeviceCount {
		return fmt.Errorf("invalid parameter, fd=%d, device=%d", d.fd, ordinal)
	}
	DeviceVMemUnlock(d.fd, ordinal)
	return nil
}

func (d *DeviceVMemory) Munmap() error {
	if d == nil {
		return fmt.Errorf("DeviceVMemory is nil")
	}
	return syscall.Munmap(d.deviceData)
}

func NewDeviceVMemory(filePath string) (*DeviceVMemory, error) {
	vmem, data, err := MmapDeviceVMemoryT(filePath)
	if err != nil {
		return nil, err
	}
	return &DeviceVMemory{
		deviceVMem: vmem,
		deviceData: data,
		filePath:   filePath,
		fd:         -1,
	}, nil
}

func MmapDeviceVMemoryT(filePath string) (*DeviceVMemoryT, []byte, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		//klog.Errorf("Failed to stat file: %s, error: %v", filePath, err)
		return nil, nil, err
	}
	dataSize := int64(unsafe.Sizeof(DeviceVMemoryT{}))
	if fileInfo.Size() != dataSize {
		klog.Errorf("File size mismatch, expected: %d, actual: %d", dataSize, fileInfo.Size())
		return nil, nil, fmt.Errorf("file size mismatch")
	}
	f, err := os.OpenFile(filePath, os.O_RDWR, 0644)
	if err != nil {
		klog.Errorf("Failed to open file: %s, error: %v", filePath, err)
		return nil, nil, err
	}
	defer func() {
		_ = f.Close()
	}()
	data, err := syscall.Mmap(int(f.Fd()), 0, int(dataSize), syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		klog.Errorf("Failed to mmap file: %s, error: %v", filePath, err)
		return nil, nil, err
	}
	resourceData := (*DeviceVMemoryT)(unsafe.Pointer(&data[0]))
	return resourceData, data, nil
}

// DeviceVMemRLock get device virtual memory file read lock
func DeviceVMemRLock(ordinal int, filepath string) (int, error) {
	cFilePath := C.CString(filepath)
	defer C.free(unsafe.Pointer(cFilePath))

	fd := C.device_vmem_read_lock(C.int(ordinal), cFilePath)
	if fd == -1 {
		return -1, fmt.Errorf("failed to acquire lock for device %d at path %s", ordinal, filepath)
	}
	return int(fd), nil
}

// DeviceVMemUnlock unlock device virtual memory file
func DeviceVMemUnlock(fd int, ordinal int) {
	C.device_vmem_unlock(C.int(fd), C.int(ordinal))
}
