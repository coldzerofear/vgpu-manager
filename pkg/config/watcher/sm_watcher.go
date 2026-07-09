package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

type MmapDeviceUtil struct {
	deviceUtil *DeviceUtilT
	mmapFile   *util.MmapFile
}

// GetDeviceUtil Lock access should be added during use
func (d *MmapDeviceUtil) GetDeviceUtil(deviceIndex int) (*DeviceProcessT, error) {
	if deviceIndex >= util.MaxDeviceCount || deviceIndex < 0 {
		return nil, fmt.Errorf("device index %d out of range [0, %v)", deviceIndex, util.MaxDeviceCount)
	}
	return &d.deviceUtil.Devices[deviceIndex], nil
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

func (d *MmapDeviceUtil) lock(deviceIndex int, lockType int16, flags int) (*os.File, error) {
	if deviceIndex < 0 || deviceIndex >= util.MaxDeviceCount {
		return nil, fmt.Errorf("device index %d out of range [0, %v)", deviceIndex, util.MaxDeviceCount)
	}
	f, err := os.OpenFile(d.mmapFile.Path, flags, 0644)
	if err != nil {
		return nil, fmt.Errorf("open %q failed: %w", d.mmapFile.Path, err)
	}
	offset := getDeviceLockOffset(deviceIndex)
	if err = util.FcntlRecordLock(f.Fd(), lockType, true, offset); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("fcntl lock device %d at offset %d: %w", deviceIndex, offset, err)
	}
	return f, nil
}

func (d *MmapDeviceUtil) RLock(deviceIndex int) (unlock func() error, err error) {
	file, err := d.lock(deviceIndex, syscall.F_RDLCK, os.O_RDONLY)
	if err != nil {
		return util.NilUnlock, err
	}
	var unlockOnce sync.Once
	var unlockErr error
	return func() error {
		// Guard the whole teardown with Once so a second call is a no-op instead
		// of double-closing the fd or unlocking an unlocked mutex.
		unlockOnce.Do(func() {
			offset := getDeviceLockOffset(deviceIndex)
			unlockErr = util.FcntlRecordLock(file.Fd(), syscall.F_UNLCK, false, offset)
			_ = file.Close()
		})
		return unlockErr
	}, nil
}

func (d *MmapDeviceUtil) WLock(deviceIndex int) (unlock func() error, err error) {
	file, err := d.lock(deviceIndex, syscall.F_WRLCK, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return util.NilUnlock, err
	}
	var unlockOnce sync.Once
	var unlockErr error
	return func() error {
		// Guard the whole teardown with Once so a second call is a no-op instead
		// of double-closing the fd or unlocking an unlocked mutex.
		unlockOnce.Do(func() {
			offset := getDeviceLockOffset(deviceIndex)
			unlockErr = util.FcntlRecordLock(file.Fd(), syscall.F_UNLCK, false, offset)
			_ = file.Close()
		})
		return unlockErr
	}, nil
}

//func (d *MmapDeviceUtil) Munmap(msync bool) error {
//	if d == nil {
//		return fmt.Errorf("DeviceUtil is nil")
//	}
//	if msync {
//		if err := d.mmapFile.Sync(); err != nil {
//			klog.V(3).ErrorS(err, "failed to sync mmap", "filepath", d.filePath)
//		}
//	}
//	return d.mmapFile.Close()
//}

func (d *MmapDeviceUtil) Sync() error {
	return d.mmapFile.Sync()
}

func (d *MmapDeviceUtil) Close() error {
	return d.mmapFile.Close()
}

func NewMmapDeviceUtil(filePath string) (*MmapDeviceUtil, error) {
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
	return &MmapDeviceUtil{
		deviceUtil: deviceUtil,
		mmapFile:   mmapFile,
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
