package vmem

import (
	"testing"
	"unsafe"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

// The struct layout below must stay byte-for-byte compatible with the writer
// side (library/include/hook.h device_vmemory_t / device_vmem_used_t, built
// with MAX_PIDS=1024). These constants are the ABI contract; if the Go structs
// drift from the C definition this test breaks before the mismatch reaches a
// running node.
const (
	wantProcessUsedSize   = 16    // {int32 pid; _; uint64 used}
	wantDeviceVMemUsed    = 16392 // processes[1024] + processes_size + lock_byte + pad
	wantLockByteFieldOffs = 16388 // offset of lock_byte within a single device
)

func TestDeviceVMemStructLayout(t *testing.T) {
	if got := unsafe.Sizeof(ProcessUsedT{}); got != wantProcessUsedSize {
		t.Fatalf("sizeof(ProcessUsedT) = %d, want %d", got, wantProcessUsedSize)
	}
	if got := unsafe.Sizeof(DeviceVMemUsedT{}); got != wantDeviceVMemUsed {
		t.Fatalf("sizeof(DeviceVMemUsedT) = %d, want %d", got, wantDeviceVMemUsed)
	}
	if got := unsafe.Offsetof(DeviceVMemUsedT{}.LockByte); got != wantLockByteFieldOffs {
		t.Fatalf("offsetof(DeviceVMemUsedT.LockByte) = %d, want %d", got, wantLockByteFieldOffs)
	}
}

// TestGetVmemoryLockOffset guards the regression where unsafe.Offsetof ignored
// the array index and returned the same offset for every device, breaking the
// read/write lock synchronization against the writer's GET_VMEMORY_LOCK_OFFSET
// (library/src/lock.c) for every device index >= 1.
func TestGetVmemoryLockOffset(t *testing.T) {
	stride := int64(unsafe.Sizeof(DeviceVMemUsedT{}))
	base := int64(unsafe.Offsetof(DeviceVMemUsedT{}.LockByte))

	for i := 0; i < util.MaxDeviceCount; i++ {
		// Independently reconstruct the C macro:
		//   offsetof(device_vmemory_t, devices[i].lock_byte)
		want := int64(i)*stride + base
		if got := getVmemoryLockOffset(i); got != want {
			t.Errorf("getVmemoryLockOffset(%d) = %d, want %d", i, got, want)
		}
	}

	// Explicit regression guard: distinct devices must map to distinct bytes.
	if getVmemoryLockOffset(1) == getVmemoryLockOffset(0) {
		t.Fatal("device 1 and device 0 resolved to the same lock offset; per-device stride is not applied")
	}
	if got := getVmemoryLockOffset(0); got != base {
		t.Fatalf("getVmemoryLockOffset(0) = %d, want %d", got, base)
	}
}
