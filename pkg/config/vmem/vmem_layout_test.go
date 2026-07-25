package vmem

import (
	"testing"
	"unsafe"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
)

// The vmem_node region is a cross-language ABI: the library writes it, this
// package reads it, and BOTH take fcntl record locks on the same byte ranges.
//
// Every other mismatch in this design announces itself. This one does not. If
// the Go offsets drift from the C ones, the two sides lock disjoint ranges,
// each believes it holds the lock, and the manager reads a ledger being
// mutated underneath it. No error, no log -- just wrong numbers.
//
// So the C values are pinned here as literals rather than derived. Deriving
// them from the Go structs would only prove Go agrees with itself. Regenerate
// with:
//
//	printf '#include "hook.h"\n#include <stdio.h>\nint main(void){
//	  printf("%%zu %%zu %%zu %%zu\\n",
//	    offsetof(device_vmemory_t, devices), sizeof(device_vmem_used_t),
//	    offsetof(device_vmem_used_t, lock_byte), sizeof(device_vmemory_t)); }' > /tmp/o.c
//	gcc -D_GNU_SOURCE -Ilibrary -Ilibrary/include -o /tmp/o /tmp/o.c && /tmp/o
const (
	cDevicesBaseOffset  = 128    // offsetof(device_vmemory_t, devices)
	cDeviceVMemUsedSize = 16392  // sizeof(device_vmem_used_t)
	cLockByteOffset     = 16388  // offsetof(device_vmem_used_t, lock_byte)
	cRegionSize         = 262400 // sizeof(device_vmemory_t)
)

func TestVMemoryLayoutMatchesC(t *testing.T) {
	assert.Equal(t, uintptr(cDevicesBaseOffset), unsafe.Offsetof(DeviceVMemoryT{}.Devices),
		"Devices base offset drifted from C: the 128B frozen header shifts every device")
	assert.Equal(t, uintptr(cDeviceVMemUsedSize), unsafe.Sizeof(DeviceVMemUsedT{}),
		"per-device stride drifted from C")
	assert.Equal(t, uintptr(cLockByteOffset), unsafe.Offsetof(DeviceVMemUsedT{}.LockByte),
		"lock_byte offset within a device drifted from C")
	assert.Equal(t, uintptr(cRegionSize), unsafe.Sizeof(DeviceVMemoryT{}),
		"total region size drifted from C")

	// Frozen header: these four fields are a permanent ABI, read before the
	// version is even known, so their offsets may never move.
	assert.Equal(t, uintptr(0), unsafe.Offsetof(DeviceVMemoryT{}.Magic))
	assert.Equal(t, uintptr(4), unsafe.Offsetof(DeviceVMemoryT{}.LayoutVersion))
	assert.Equal(t, uintptr(8), unsafe.Offsetof(DeviceVMemoryT{}.RegionSize))
	assert.Equal(t, uintptr(12), unsafe.Offsetof(DeviceVMemoryT{}.DeviceCount))

	assert.LessOrEqual(t, int64(unsafe.Sizeof(DeviceVMemoryT{})), VMemNodeFileSize,
		"region no longer fits the permanently reserved file size")
}

// Reproduces GET_VMEMORY_LOCK_OFFSET(i) from library/src/lock.c independently
// of the implementation, so a regression in getVmemoryLockOffset -- notably
// dropping the Devices base again -- fails here instead of silently disabling
// mutual exclusion.
func TestVMemoryLockOffsetMatchesC(t *testing.T) {
	for i := 0; i < util.MaxDeviceCount; i++ {
		want := int64(cDevicesBaseOffset + i*cDeviceVMemUsedSize + cLockByteOffset)
		assert.Equal(t, want, getVmemoryLockOffset(i), "lock offset for device %d", i)
	}
	// Spot-check the two the C build printed, so the formula itself is anchored
	// to observed values and not just to itself.
	assert.Equal(t, int64(16516), getVmemoryLockOffset(0))
	assert.Equal(t, int64(32908), getVmemoryLockOffset(1))

	// The header byte the library locks during init must never collide with a
	// per-device lock, or initialisation would contend with live readers.
	assert.Greater(t, getVmemoryLockOffset(0), int64(0),
		"device 0's lock must not overlap the header byte locked at init")
}
