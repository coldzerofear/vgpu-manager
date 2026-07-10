package watcher

/*
#cgo linux LDFLAGS: -Wl,--unresolved-symbols=ignore-in-object-files

#include <stdlib.h>

// nvmlDeviceGetProcessesUtilizationInfo() requires a caller-allocated sample
// array: it returns NVML_ERROR_INSUFFICIENT_SIZE whenever procUtilArray is NULL,
// and NVML_SUCCESS only once the array has been populated. go-nvml's
// Device.GetProcessesUtilizationInfo() always passes a zero-valued struct, so it
// can never return NVML_SUCCESS and never yields samples, and it exposes no entry
// point that accepts a caller-filled struct. Hence this shim -- it is the only
// part go-nvml does not already provide; the struct layouts and the version
// constant are reused from it rather than restated here.
//
// The symbols are declared extern and left unresolved at link time; go-nvml
// dlopen()s libnvidia-ml.so.1 with RTLD_GLOBAL during nvml.Init(), which is what
// binds them (the same mechanism go-nvml's own cgo bindings rely on).

typedef unsigned int vgpuNvmlReturn_t;
typedef void *vgpuNvmlDevice_t;

extern vgpuNvmlReturn_t nvmlDeviceGetHandleByIndex_v2(unsigned int index, vgpuNvmlDevice_t *device);
extern vgpuNvmlReturn_t nvmlDeviceGetProcessesUtilizationInfo(vgpuNvmlDevice_t device, void *info);

// The device handle is resolved here from its index, so the opaque nvml.Device
// that go-nvml keeps private never has to be unwrapped.
static vgpuNvmlReturn_t vgpuDeviceGetProcessesUtilizationInfo(unsigned int index, void *info) {
  vgpuNvmlDevice_t dev = NULL;
  vgpuNvmlReturn_t ret = nvmlDeviceGetHandleByIndex_v2(index, &dev);
  if (ret != 0) { // NVML_SUCCESS
    return ret;
  }
  return nvmlDeviceGetProcessesUtilizationInfo(dev, info);
}
*/
import "C"

import (
	"unsafe"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// symProcessesUtilizationInfo is the driver symbol backing the extended entry point.
const symProcessesUtilizationInfo = "nvmlDeviceGetProcessesUtilizationInfo"

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
