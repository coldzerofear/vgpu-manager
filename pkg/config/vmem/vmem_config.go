package vmem

import "C"
import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/klog/v2"
)

type ProcessUsedT struct {
	Pid  int32
	Used uint64
}

type DeviceVMemUsedT struct {
	Processes     [watcher.MaxPids]ProcessUsedT
	ProcessesSize uint32
	LockByte      uint8
}

type DeviceVMemoryT struct {
	Devices [watcher.MaxDeviceCount]DeviceVMemUsedT
}

func (d *DeviceVMemUsedT) GetTotalUsed() uint64 {
	used := uint64(0)
	for index := uint32(0); index < d.ProcessesSize; index++ {
		used += d.Processes[index].Used
	}
	return used
}

type MmapDeviceVMemory struct {
	mutex    sync.Mutex
	vMemory  *DeviceVMemoryT
	mmapFile *util.MmapFile
}

func (m *MmapDeviceVMemory) Close() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.mmapFile.Close()
}

var nilUnlock = func() error { return nil }

func (m *MmapDeviceVMemory) RLock(deviceIndex int) (Unlock func() error, err error) {
	m.mutex.Lock()

	file := m.mmapFile.File
	if err = deviceReadLock(file, deviceIndex); err != nil {
		m.mutex.Unlock()
		return nilUnlock, err
	}

	var unlockOnce sync.Once
	var unlockErr error
	return func() error {
		defer m.mutex.Unlock()
		unlockOnce.Do(func() {
			unlockErr = deviceUnlock(file, deviceIndex)
		})
		return unlockErr
	}, nil
}

func (m *MmapDeviceVMemory) GetDeviceMemory(deviceIndex int) (*DeviceVMemUsedT, error) {
	if deviceIndex >= watcher.MaxDeviceCount || deviceIndex < 0 {
		return nil, fmt.Errorf("device index %d out of range [0, %v)", deviceIndex, watcher.MaxDeviceCount)
	}
	return &m.vMemory.Devices[deviceIndex], nil
}

func (m *MmapDeviceVMemory) NeedsReload() (reload bool, err error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.mmapFile.NeedsReload()
}

func (m *MmapDeviceVMemory) Reload() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	data, err := NewMmapDeviceVMemory(m.mmapFile.Path)
	if err != nil {
		return fmt.Errorf("reload %q failed: %w", m.mmapFile.Path, err)
	}
	_ = m.mmapFile.Close()
	m.vMemory = data.vMemory
	m.mmapFile = data.mmapFile
	return nil
}

func getVmemoryLockOffset(deviceIndex int) int64 {
	var dv DeviceVMemoryT
	return int64(unsafe.Offsetof(dv.Devices[deviceIndex].LockByte))
}

func NewMmapDeviceVMemory(filePath string) (*MmapDeviceVMemory, error) {
	mmapFile, err := util.OpenMmap(filePath, util.DefaultReadOnlyMmap)
	if err != nil {
		return nil, err
	}
	dataSize := int64(unsafe.Sizeof(DeviceVMemoryT{}))
	size := mmapFile.FileInfo.Size()
	if mmapFile.FileInfo.Size() != dataSize {
		klog.Errorf("File size mismatch, expected: %d, actual: %d", dataSize, size)
		return nil, fmt.Errorf("file size mismatch")
	}
	data := (*DeviceVMemoryT)(unsafe.Pointer(&mmapFile.Data[0]))
	return &MmapDeviceVMemory{
		vMemory:  data,
		mmapFile: mmapFile,
	}, nil
}

func deviceReadLock(file *os.File, deviceIndex int) error {
	if file == nil {
		return fmt.Errorf("device file is nil")
	}
	if deviceIndex >= watcher.MaxDeviceCount || deviceIndex < 0 {
		return fmt.Errorf("device index %d out of range [0, %v)", deviceIndex, watcher.MaxDeviceCount)
	}

	offset := getVmemoryLockOffset(deviceIndex)
	lock := syscall.Flock_t{
		Type:   syscall.F_RDLCK,
		Whence: 0, // SEEK_SET
		Start:  offset,
		Len:    1,
		Pid:    0,
	}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLKW, &lock); err != nil {
		return fmt.Errorf("fcntl F_SETLKW at offset %d: %w", offset, err)
	}
	return nil
}

func deviceUnlock(file *os.File, deviceIndex int) error {
	if file == nil {
		return fmt.Errorf("device file is nil")
	}
	if deviceIndex >= watcher.MaxDeviceCount || deviceIndex < 0 {
		return fmt.Errorf("device index %d out of range [0, %v)", deviceIndex, watcher.MaxDeviceCount)
	}

	offset := getVmemoryLockOffset(deviceIndex)
	lock := syscall.Flock_t{
		Type:   syscall.F_UNLCK,
		Whence: 0, // SEEK_SET
		Start:  offset,
		Len:    1,
		Pid:    0,
	}

	if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock); err != nil {
		return fmt.Errorf("fcntl F_SETLK at offset %d: %w", offset, err)
	}
	return nil
}
