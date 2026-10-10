/*
Copyright 2025-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vmem

import (
	"errors"
	"os"
	"path/filepath"
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
	lockByte := int64(unsafe.Offsetof(DeviceVMemUsedT{}.LockByte))
	// The region gained a 128-byte frozen header, so the Devices array no
	// longer starts at offset 0 and every lock byte moved down with it. This
	// term used to be absent because it used to be zero; leaving it out now
	// would put Go's locks 128 bytes below C's, which produces no error at all
	// -- both sides would just stop excluding each other.
	devicesBase := int64(unsafe.Offsetof(DeviceVMemoryT{}.Devices))

	for i := 0; i < util.MaxDeviceCount; i++ {
		// Independently reconstruct the C macro:
		//   offsetof(device_vmemory_t, devices[i].lock_byte)
		want := devicesBase + int64(i)*stride + lockByte
		if got := getVmemoryLockOffset(i); got != want {
			t.Errorf("getVmemoryLockOffset(%d) = %d, want %d", i, got, want)
		}
	}

	// Explicit regression guard: distinct devices must map to distinct bytes.
	if getVmemoryLockOffset(1) == getVmemoryLockOffset(0) {
		t.Fatal("device 1 and device 0 resolved to the same lock offset; per-device stride is not applied")
	}
	if got := getVmemoryLockOffset(0); got != devicesBase+lockByte {
		t.Fatalf("getVmemoryLockOffset(0) = %d, want %d", got, devicesBase+lockByte)
	}
	// The header byte the library write-locks during init must not collide
	// with any per-device lock range.
	if getVmemoryLockOffset(0) <= 0 {
		t.Fatal("device 0's lock byte overlaps the header byte locked at init")
	}
}

// A mapping the metrics lister closed must refuse readers instead of letting
// them dereference unmapped memory (the file may be long gone by then).
func TestMmapDeviceVMemoryClosedRefusesReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), util.VMemNodeFile)
	region := &DeviceVMemoryT{
		Magic:         VMemNodeMagic,
		LayoutVersion: VMemNodeLayoutVersion,
		RegionSize:    uint32(unsafe.Sizeof(DeviceVMemoryT{})),
		DeviceCount:   uint32(util.MaxDeviceCount),
	}
	buf := make([]byte, VMemNodeFileSize)
	copy(buf, unsafe.Slice((*byte)(unsafe.Pointer(region)), unsafe.Sizeof(*region)))
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	vMemory, err := NewMmapDeviceVMemory(path)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := vMemory.RLock(0)
	if err != nil {
		t.Fatalf("RLock on a live mapping: %v", err)
	}
	if _, err = vMemory.GetDeviceMemory(0); err != nil {
		t.Fatalf("GetDeviceMemory: %v", err)
	}
	if err = unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	if err = vMemory.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err = vMemory.Close(); err != nil {
		t.Fatalf("a second close must be a no-op: %v", err)
	}
	if _, err = vMemory.RLock(0); err == nil {
		t.Error("RLock on a closed mapping must fail, not hand out unmapped memory")
	}
	if _, err = vMemory.NeedsReload(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("NeedsReload on a closed mapping: %v", err)
	}
	if err = vMemory.Reload(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("Reload of a closed mapping: %v", err)
	}
}
