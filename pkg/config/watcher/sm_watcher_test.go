package watcher

import (
	"testing"
	"unsafe"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// These sizes are the shared-memory ABI contract with the C side
// (library device_process_t / device_util_t, MAX_PIDS=1024). nvml struct sizes
// must match nvmlProcessUtilizationSample_t (32) and nvmlProcessInfoV2_t (24);
// if go-nvml ever changes them the shared memory layout silently diverges from
// the writer/reader on the C side, so fail loudly here instead.
const (
	wantSampleSize      = 32
	wantProcessInfoSize = 24
	wantDeviceProcess   = 81952
	wantLockByteOffset  = 81948
)

func TestDeviceUtilStructLayout(t *testing.T) {
	if got := unsafe.Sizeof(nvml.ProcessUtilizationSample{}); got != wantSampleSize {
		t.Fatalf("sizeof(nvml.ProcessUtilizationSample) = %d, want %d", got, wantSampleSize)
	}
	if got := unsafe.Sizeof(nvml.ProcessInfo{}); got != wantProcessInfoSize {
		t.Fatalf("sizeof(nvml.ProcessInfo) = %d, want %d", got, wantProcessInfoSize)
	}
	if got := unsafe.Sizeof(DeviceProcessT{}); got != wantDeviceProcess {
		t.Fatalf("sizeof(DeviceProcessT) = %d, want %d", got, wantDeviceProcess)
	}
	if got := unsafe.Offsetof(DeviceProcessT{}.LockByte); got != wantLockByteOffset {
		t.Fatalf("offsetof(DeviceProcessT.LockByte) = %d, want %d", got, wantLockByteOffset)
	}
}

// TestGetDeviceLockOffset guards against the unsafe.Offsetof trap where the
// array index is ignored, which would place every device's lock at the same
// byte and break synchronization with the writer's GET_DEVICE_LOCK_OFFSET
// (library/src/lock.c) for every device index >= 1.
func TestGetDeviceLockOffset(t *testing.T) {
	stride := int64(unsafe.Sizeof(DeviceProcessT{}))
	base := int64(unsafe.Offsetof(DeviceProcessT{}.LockByte))

	for i := 0; i < MaxDeviceCount; i++ {
		want := int64(i)*stride + base
		if got := getDeviceLockOffset(i); got != want {
			t.Errorf("getDeviceLockOffset(%d) = %d, want %d", i, got, want)
		}
	}
	if getDeviceLockOffset(1) == getDeviceLockOffset(0) {
		t.Fatal("device 1 and device 0 resolved to the same lock offset; per-device stride is not applied")
	}
	if got := getDeviceLockOffset(0); got != base {
		t.Fatalf("getDeviceLockOffset(0) = %d, want %d", got, base)
	}
}
