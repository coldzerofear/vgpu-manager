package watcher

/*
#cgo CFLAGS: -D_GNU_SOURCE
#cgo LDFLAGS: -ldl

#include <dlfcn.h>
#include <stdlib.h>

// nvmlDeviceGetProcessesUtilizationInfo() requires a caller-allocated sample
// array: per nvml.h it returns NVML_ERROR_INSUFFICIENT_SIZE whenever
// procUtilArray is NULL, and NVML_SUCCESS only once that array has been
// populated. go-nvml's Device.GetProcessesUtilizationInfo() hands the driver a
// zero-valued struct and never calls back, so it can never return NVML_SUCCESS,
// and it exposes no entry point that accepts a caller-filled struct. Hence this
// shim; the struct layouts and the version constant are reused from go-nvml
// rather than restated here.
//
// The symbols are resolved with dlsym() instead of being declared extern, the
// way the in-container library does it (NVML_FIND_ENTRY in library/src). An
// extern reference would link fine but bind to address 0 on a driver that does
// not export it, turning the guarded-off path into a SIGSEGV the moment anything
// reached it. A NULL function pointer is checkable. go-nvml dlopen()s
// libnvidia-ml.so.1 with RTLD_GLOBAL, so RTLD_DEFAULT finds the symbols.

typedef unsigned int vgpuNvmlReturn_t;
typedef void *vgpuNvmlDevice_t;

typedef vgpuNvmlReturn_t (*vgpuFnHandleByIndex)(unsigned int index, vgpuNvmlDevice_t *device);
typedef vgpuNvmlReturn_t (*vgpuFnProcsUtilInfo)(vgpuNvmlDevice_t device, void *info);

static vgpuFnHandleByIndex vgpuHandleByIndex;
static vgpuFnProcsUtilInfo vgpuProcsUtilInfo;

// Reports whether both entry points exist in the loaded driver. Re-resolves while
// unresolved: the first attempt may land before libnvidia-ml.so.1 is dlopen()ed.
static int vgpuResolveProcessesUtilizationInfo(void) {
  if (vgpuHandleByIndex == NULL) {
    vgpuHandleByIndex = (vgpuFnHandleByIndex)dlsym(RTLD_DEFAULT, "nvmlDeviceGetHandleByIndex_v2");
  }
  if (vgpuProcsUtilInfo == NULL) {
    vgpuProcsUtilInfo = (vgpuFnProcsUtilInfo)dlsym(RTLD_DEFAULT, "nvmlDeviceGetProcessesUtilizationInfo");
  }
  return vgpuHandleByIndex != NULL && vgpuProcsUtilInfo != NULL;
}

// The device handle is resolved here from its index, so the opaque nvml.Device
// that go-nvml keeps private never has to be unwrapped.
static vgpuNvmlReturn_t vgpuDeviceGetProcessesUtilizationInfo(unsigned int index, void *info) {
  if (!vgpuResolveProcessesUtilizationInfo()) {
    return 3; // NVML_ERROR_NOT_SUPPORTED
  }
  vgpuNvmlDevice_t dev = NULL;
  vgpuNvmlReturn_t ret = vgpuHandleByIndex(index, &dev);
  if (ret != 0) { // NVML_SUCCESS
    return ret;
  }
  return vgpuProcsUtilInfo(dev, info);
}
*/
import "C"

import (
	"unsafe"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// processesUtilizationInfoAvailable reports whether the driver exports the
// extended entry point.
//
// The check goes through dlsym rather than nvml.Extensions().LookupSymbol():
// that one interrogates go-nvml's package-level library singleton, which this
// project never initializes -- it builds its own instance with nvml.New() -- so
// it would answer "absent" on every driver.
func processesUtilizationInfoAvailable() bool {
	return C.vgpuResolveProcessesUtilizationInfo() != 0
}

// getProcessesUtilizationSamples is the Go counterpart of the library's
// get_process_utilization_samples() fallback (library/src/cuda_hook.c): size the
// buffer with a NULL array, then fetch into it. NVML return codes are passed
// through untouched.
func getProcessesUtilizationSamples(index int, lastTs uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) {
	var info nvml.ProcessesUtilizationInfo
	info.Version = nvml.STRUCT_VERSION(info, 1)
	info.ProcessSamplesCount = 0
	info.LastSeenTimeStamp = lastTs // input filter: only samples newer than this
	info.ProcUtilArray = nil

	ret := nvml.Return(C.vgpuDeviceGetProcessesUtilizationInfo(C.uint(index), unsafe.Pointer(&info)))
	if ret != nvml.ERROR_INSUFFICIENT_SIZE {
		// Anything else leaves nothing to fetch: NVML reports SUCCESS only once it
		// has populated the array, which it cannot have done with a NULL one.
		return nil, ret
	}

	// The array lives in C memory. Handing C a Go pointer to memory that itself
	// contains a Go pointer is forbidden; a C pointer stored in the Go-side info
	// struct is not, so the struct itself can stay on the Go heap.
	buf := C.calloc(C.size_t(MaxPids), C.size_t(unsafe.Sizeof(nvml.ProcessUtilizationInfo_v1{})))
	if buf == nil {
		return nil, nvml.ERROR_MEMORY
	}
	defer C.free(buf)

	info.ProcUtilArray = (*nvml.ProcessUtilizationInfo_v1)(buf)
	info.ProcessSamplesCount = MaxPids
	ret = nvml.Return(C.vgpuDeviceGetProcessesUtilizationInfo(C.uint(index), unsafe.Pointer(&info)))
	if ret != nvml.SUCCESS {
		return nil, ret
	}

	count := info.ProcessSamplesCount
	if count > MaxPids {
		count = MaxPids
	}
	if count == 0 {
		return nil, nvml.SUCCESS
	}
	samples := make([]nvml.ProcessUtilizationSample, count)
	for i, s := range unsafe.Slice(info.ProcUtilArray, int(count)) {
		samples[i] = nvml.ProcessUtilizationSample{
			Pid:       s.Pid,
			TimeStamp: s.TimeStamp,
			SmUtil:    s.SmUtil,
			MemUtil:   s.MemUtil,
			EncUtil:   s.EncUtil,
			DecUtil:   s.DecUtil,
		}
	}
	return samples, nvml.SUCCESS
}
