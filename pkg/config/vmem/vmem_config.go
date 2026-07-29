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

// Frozen header of the vmem_node region. Must match library/include/hook.h
// byte for byte: VMEM_NODE_MAGIC / VMEM_NODE_LAYOUT_VERSION, and the file size
// the library ftruncates to.
const (
	VMemNodeMagic         uint32 = 0x564D4E44 // "VMND"
	VMemNodeLayoutVersion uint32 = 1
	VMemNodeFileSize      int64  = 320 * 1024
)

// ErrUnknownLayout means the region was written by a library version this
// binary does not understand. The correct response is to treat the container
// as having no data for now, never to rebuild: the library may rebuild because
// a layout mismatch proves the file predates the running container, but the
// manager lives outside any container's lifecycle and would be erasing the
// ledger of a container that is very much alive.
var ErrUnknownLayout = fmt.Errorf("unknown vmem_node layout")

// DeviceVMemoryT mirrors device_vmemory_t. The 128-byte header is NOT
// decoration: it shifts Devices, and therefore every per-device lock offset,
// down by one cache line. See getVmemoryLockOffset.
type DeviceVMemoryT struct {
	Magic         uint32
	LayoutVersion uint32
	RegionSize    uint32
	DeviceCount   uint32
	_             [112]byte // pad the header to one 128B cache line
	Devices       [util.MaxDeviceCount]DeviceVMemUsedT
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

func (m *MmapDeviceVMemory) RLock(deviceIndex int) (unlock func() error, err error) {
	if deviceIndex < 0 || deviceIndex >= util.MaxDeviceCount {
		return util.NilUnlock, fmt.Errorf("device index %d out of range [0, %v)", deviceIndex, util.MaxDeviceCount)
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
		return util.NilUnlock, err
	}
	offset := getVmemoryLockOffset(deviceIndex)
	if err = util.FcntlRecordLock(file.Fd(), syscall.F_RDLCK, true, offset); err != nil {
		_ = file.Close()
		m.mutex.RUnlock()
		return util.NilUnlock, fmt.Errorf("fcntl read lock device %d at offset %d: %w", deviceIndex, offset, err)
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
	// The base offset of the Devices array is REQUIRED and easy to forget.
	// unsafe.Offsetof(DeviceVMemUsedT{}.LockByte) is the offset within one
	// element; without adding where the array itself starts, every lock lands
	// 128 bytes short of the range C locks.
	//
	// Getting this wrong reports no error anywhere. Go and C would simply lock
	// disjoint byte ranges, each believing it holds the lock, and the manager
	// would read a ledger being mutated underneath it -- wrong numbers, no
	// warning. C is immune because GET_VMEMORY_LOCK_OFFSET uses offsetof() on
	// the whole struct, which accounts for the header automatically; this is
	// the only side computed by hand. TestVMemoryLockOffsetMatchesC pins it.
	base := int64(unsafe.Offsetof(DeviceVMemoryT{}.Devices))
	deviceSize := int64(unsafe.Sizeof(DeviceVMemUsedT{}))
	lockByteOffset := int64(unsafe.Offsetof(DeviceVMemUsedT{}.LockByte))
	return base + int64(deviceIndex)*deviceSize + lockByteOffset
}

func NewMmapDeviceVMemory(filePath string) (*MmapDeviceVMemory, error) {
	mmapFile, err := util.OpenMmap(filePath, util.DefaultReadOnlyMmap)
	if err != nil {
		return nil, err
	}
	// The file is a fixed size now, decoupled from sizeof(DeviceVMemoryT), so
	// that the library never resizes it -- a shrink would leave this mapping
	// extending past EOF and touching it would be SIGBUS in the manager.
	size := mmapFile.FileInfo.Size()
	if size != VMemNodeFileSize {
		klog.V(4).Infof("vmem_node size mismatch, expected: %d, actual: %d", VMemNodeFileSize, size)
		_ = mmapFile.Close()
		return nil, ErrUnknownLayout
	}
	data := (*DeviceVMemoryT)(unsafe.Pointer(&mmapFile.Data[0]))
	// Header check, deliberately after the size check and deliberately not
	// fatal. It also covers what a size check cannot: a layout that changed
	// without changing size. Skipping is the only correct action -- see
	// ErrUnknownLayout. A container mid-rebuild publishes magic last, so it is
	// observed here as "not ready yet" and picked up on a later pass.
	if data.Magic != VMemNodeMagic || data.LayoutVersion != VMemNodeLayoutVersion ||
		data.RegionSize != uint32(unsafe.Sizeof(DeviceVMemoryT{})) ||
		data.DeviceCount != uint32(util.MaxDeviceCount) {
		klog.V(4).Infof("vmem_node layout not recognised (magic=%#x ver=%d size=%d count=%d)",
			data.Magic, data.LayoutVersion, data.RegionSize, data.DeviceCount)
		_ = mmapFile.Close()
		return nil, ErrUnknownLayout
	}
	return &MmapDeviceVMemory{
		vMemory:  data,
		mmapFile: mmapFile,
	}, nil
}
