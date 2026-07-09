package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/klog/v2"
)

// MaxPids / MaxDeviceCount must stay identical to the writer/reader C side
// (library MAX_PIDS / MAX_DEVICE_COUNT). They are the shared-memory ABI and are
// asserted against the struct layout in sm_watcher_test.go.
const (
	MaxPids        = util.MaxDevicePids
	MaxDeviceCount = util.MaxDeviceCount
)

// DeviceProcessT mirrors C device_process_t. Field order, types and padding
// must match the writer/reader on the C side byte-for-byte.
type DeviceProcessT struct {
	ProcessUtilSamples     [MaxPids]nvml.ProcessUtilizationSample
	ProcessUtilSamplesSize uint32
	LastSeenTimeStamp      uint64
	ComputeProcesses       [MaxPids]nvml.ProcessInfo
	ComputeProcessesSize   uint32
	GraphicsProcesses      [MaxPids]nvml.ProcessInfo
	GraphicsProcessesSize  uint32
	LockByte               uint8
}

// DeviceUtilT mirrors C device_util_t.
type DeviceUtilT struct {
	Devices [MaxDeviceCount]DeviceProcessT
}

type DeviceUtil struct {
	deviceUtil *DeviceUtilT
	mmapFile   *util.MmapFile
	filePath   string
}

type DeviceUtilWrap struct {
	deviceUtil *DeviceUtilT
	filePath   string
	// lockFile holds the open file backing the currently-held POSIX record
	// lock. It must be kept alive for the lock's lifetime: if it were dropped,
	// the runtime finalizer would close the fd and silently release the lock.
	lockFile *os.File
}

func (d *DeviceUtil) GetWrap() *DeviceUtilWrap {
	return &DeviceUtilWrap{
		deviceUtil: d.deviceUtil,
		filePath:   d.filePath,
	}
}

// GetUtil Lock access should be added during use
func (d *DeviceUtilWrap) GetUtil() *DeviceUtilT {
	return d.deviceUtil
}

// getDeviceLockOffset mirrors the writer side (library/src/lock.c
// GET_DEVICE_LOCK_OFFSET): offsetof(device_util_t, devices[deviceIndex].lock_byte).
// unsafe.Offsetof only yields the field offset within a single DeviceProcessT
// (the array index is a compile-time constant and is ignored), so the per-device
// stride must be added explicitly.
func getDeviceLockOffset(deviceIndex int) int64 {
	deviceSize := int64(unsafe.Sizeof(DeviceProcessT{}))
	lockByteOffset := int64(unsafe.Offsetof(DeviceProcessT{}.LockByte))
	return int64(deviceIndex)*deviceSize + lockByteOffset
}

func (d *DeviceUtilWrap) lock(ordinal int, lockType int16, flags int) error {
	if d == nil {
		return fmt.Errorf("DeviceUtilWrap is nil")
	}
	if d.lockFile != nil {
		return fmt.Errorf("lock already exists")
	}
	if len(d.filePath) == 0 || ordinal < 0 || ordinal >= MaxDeviceCount {
		return fmt.Errorf("invalid parameter, filepath=%s, device=%d", d.filePath, ordinal)
	}
	f, err := os.OpenFile(d.filePath, flags, 0644)
	if err != nil {
		return fmt.Errorf("open %q failed: %w", d.filePath, err)
	}
	offset := getDeviceLockOffset(ordinal)
	if err = util.FcntlRecordLock(f.Fd(), lockType, true, offset); err != nil {
		_ = f.Close()
		return fmt.Errorf("fcntl lock device %d at offset %d: %w", ordinal, offset, err)
	}
	d.lockFile = f
	return nil
}

func (d *DeviceUtilWrap) RLock(ordinal int) error {
	return d.lock(ordinal, syscall.F_RDLCK, os.O_RDONLY)
}

func (d *DeviceUtilWrap) WLock(ordinal int) error {
	return d.lock(ordinal, syscall.F_WRLCK, os.O_RDWR|os.O_CREATE)
}

func (d *DeviceUtilWrap) Unlock(ordinal int) error {
	if d == nil {
		return fmt.Errorf("DeviceUtilWrap is nil")
	}
	if d.lockFile == nil || ordinal < 0 || ordinal >= MaxDeviceCount {
		return fmt.Errorf("invalid parameter, locked=%v, device=%d", d.lockFile != nil, ordinal)
	}
	offset := getDeviceLockOffset(ordinal)
	err := util.FcntlRecordLock(d.lockFile.Fd(), syscall.F_UNLCK, false, offset)
	// Closing the fd releases the POSIX record lock regardless; always close.
	_ = d.lockFile.Close()
	d.lockFile = nil
	if err != nil {
		return fmt.Errorf("fcntl F_UNLCK device %d at offset %d: %w", ordinal, offset, err)
	}
	return nil
}

func (d *DeviceUtil) Munmap(msync bool) error {
	if d == nil {
		return fmt.Errorf("DeviceUtil is nil")
	}
	if msync {
		if err := d.mmapFile.Sync(); err != nil {
			klog.V(3).ErrorS(err, "failed to sync mmap", "filepath", d.filePath)
		}
	}
	return d.mmapFile.Close()
}

func NewDeviceUtil(filePath string) (*DeviceUtil, error) {
	mmapFile, err := util.OpenMmap(filePath, util.DefaultReadWriteMmap)
	if err != nil {
		return nil, err
	}
	dataSize := int64(unsafe.Sizeof(DeviceUtilT{}))
	if mmapFile.FileInfo.Size() != dataSize {
		klog.Errorf("File size mismatch, expected: %d, actual: %d", dataSize, mmapFile.FileInfo.Size())
		_ = mmapFile.Close()
		return nil, fmt.Errorf("file size mismatch")
	}
	deviceUtil := (*DeviceUtilT)(unsafe.Pointer(&mmapFile.Data[0]))
	return &DeviceUtil{
		deviceUtil: deviceUtil,
		mmapFile:   mmapFile,
		filePath:   filePath,
	}, nil
}

func CreateDeviceUtilFile(filePath string) error {
	f, err := os.Create(filePath)
	if err != nil {
		klog.ErrorS(err, "Failed to create device util file", "filepath", filePath)
		return err
	}
	defer func() {
		_ = f.Close()
	}()

	dataSize := int64(unsafe.Sizeof(DeviceUtilT{}))
	err = f.Truncate(dataSize)
	if err != nil {
		klog.ErrorS(err, "Failed to truncate device util file", "filepath", filePath)
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
		klog.ErrorS(err, "Failed to remove old device util file", "filepath", filePath)
		return err
	}
	return CreateDeviceUtilFile(filePath)
}
