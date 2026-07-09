package vmem

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/klog/v2"
)

type ProcessUsedT struct {
	Pid  int32
	Used uint64
}

type DeviceVMemUsedT struct {
	Processes     [util.MaxDevicePids]ProcessUsedT
	ProcessesSize uint32
	LockByte      uint8
}

type DeviceVMemoryT struct {
	Devices [util.MaxDeviceCount]DeviceVMemUsedT
}

func (d *DeviceVMemUsedT) GetTotalUsed() uint64 {
	used := uint64(0)
	for index := uint32(0); index < d.ProcessesSize; index++ {
		used += d.Processes[index].Used
	}
	return used
}

type MmapDeviceVMemory struct {
	mutex    sync.RWMutex
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
	if deviceIndex < 0 || deviceIndex >= util.MaxDeviceCount {
		return nilUnlock, fmt.Errorf("device index %d out of range [0, %v)", deviceIndex, util.MaxDeviceCount)
	}
	// The RWMutex read lock only guards the mmap lifetime: it keeps Reload/Close
	// (which take the exclusive lock) from unmapping m.vMemory while a reader is
	// dereferencing it. It does NOT stand in for the fcntl lock.
	m.mutex.RLock()

	// Open a dedicated fd per reader for the cross-process fcntl lock. fcntl
	// record locks are not refcounted per open-file-description, so a shared fd
	// would let one reader's F_UNLCK drop a concurrent same-device reader's lock
	// and expose it to a torn read from the writer. A private fd gives each
	// reader an independent OFD lock; with OFD, closing it never disturbs others.
	file, err := os.Open(m.mmapFile.Path)
	if err != nil {
		m.mutex.RUnlock()
		return nilUnlock, err
	}
	offset := getVmemoryLockOffset(deviceIndex)
	if err = util.FcntlRecordLock(file.Fd(), syscall.F_RDLCK, true, offset); err != nil {
		_ = file.Close()
		m.mutex.RUnlock()
		return nilUnlock, fmt.Errorf("fcntl read lock device %d at offset %d: %w", deviceIndex, offset, err)
	}

	var unlockOnce sync.Once
	var unlockErr error
	return func() error {
		// Guard the whole teardown with Once so a second call is a no-op instead
		// of double-closing the fd or unlocking an unlocked mutex.
		unlockOnce.Do(func() {
			unlockErr = util.FcntlRecordLock(file.Fd(), syscall.F_UNLCK, false, offset)
			_ = file.Close()
			m.mutex.RUnlock()
		})
		return unlockErr
	}, nil
}

func (m *MmapDeviceVMemory) GetDeviceMemory(deviceIndex int) (*DeviceVMemUsedT, error) {
	if deviceIndex >= util.MaxDeviceCount || deviceIndex < 0 {
		return nil, fmt.Errorf("device index %d out of range [0, %v)", deviceIndex, util.MaxDeviceCount)
	}
	return &m.vMemory.Devices[deviceIndex], nil
}

func (m *MmapDeviceVMemory) NeedsReload() (reload bool, err error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
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
	// Must mirror the writer side (library/src/lock.c GET_VMEMORY_LOCK_OFFSET):
	// offsetof(device_vmemory_t, devices[deviceIndex].lock_byte).
	// unsafe.Offsetof only yields the field offset within a single
	// DeviceVMemUsedT (the array index is ignored, it is a compile-time
	// constant), so the per-device stride must be added explicitly.
	deviceSize := int64(unsafe.Sizeof(DeviceVMemUsedT{}))
	lockByteOffset := int64(unsafe.Offsetof(DeviceVMemUsedT{}.LockByte))
	return int64(deviceIndex)*deviceSize + lockByteOffset
}

func NewMmapDeviceVMemory(filePath string) (*MmapDeviceVMemory, error) {
	mmapFile, err := util.OpenMmap(filePath, util.DefaultReadOnlyMmap)
	if err != nil {
		return nil, err
	}
	dataSize := int64(unsafe.Sizeof(DeviceVMemoryT{}))
	size := mmapFile.FileInfo.Size()
	if size != dataSize {
		klog.Errorf("File size mismatch, expected: %d, actual: %d", dataSize, size)
		_ = mmapFile.Close()
		return nil, fmt.Errorf("file size mismatch")
	}
	data := (*DeviceVMemoryT)(unsafe.Pointer(&mmapFile.Data[0]))
	return &MmapDeviceVMemory{
		vMemory:  data,
		mmapFile: mmapFile,
	}, nil
}

