package watcher

import (
	"sync/atomic"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// sampleWindow bounds how far back utilization samples are collected.
const sampleWindow = time.Second

// Indirection over the driver-backed entry points so the fallback can be
// exercised without a GPU. Never reassigned outside tests.
var (
	fetchExtended  = getProcessesUtilizationSamples
	extendedExists = processesUtilizationInfoAvailable
)

type DeviceUtilInterface interface {
	DeviceGetProcessUtilSamples(nvml.Device) ([]nvml.ProcessUtilizationSample, uint64, nvml.Return)
}

// deviceUtilCache remembers whether the legacy nvmlDeviceGetProcessUtilization
// entry point is usable. Support for it is a property of the driver and the
// hardware generation, not of an individual device, so the answer is learned once
// and reused: Blackwell and newer answer NOT_SUPPORTED and are served through
// nvmlDeviceGetProcessesUtilizationInfo instead.
type deviceUtilCache struct {
	useExtended atomic.Bool
}

func NewDeviceUtilCache() DeviceUtilInterface {
	return &deviceUtilCache{}
}

// DeviceGetProcessUtilSamples returns the device's process utilization samples
// along with the timestamp they were filtered against. It mirrors the library's
// get_process_utilization_samples(): try the legacy entry point, and only when it
// reports NOT_SUPPORTED fall back to the extended one.
//
// NVML return codes are passed through untouched. NOT_FOUND in particular means
// "no samples newer than lastTs" and is the common answer at the watcher's poll
// rate even for a busy GPU; callers rely on a non-SUCCESS reply to keep their
// cached samples until a fresh batch arrives.
func (c *deviceUtilCache) DeviceGetProcessUtilSamples(d nvml.Device) ([]nvml.ProcessUtilizationSample, uint64, nvml.Return) {
	lastTs := uint64(time.Now().Add(-sampleWindow).UnixMicro())

	if !c.useExtended.Load() {
		samples, ret := d.GetProcessUtilization(lastTs)
		if ret != nvml.ERROR_NOT_SUPPORTED {
			return samples, lastTs, ret
		}
		// An older driver exports no extended entry point, so there is no fallback
		// to take: report the driver's own NOT_SUPPORTED and never call into it.
		if !extendedExists() {
			return nil, lastTs, ret
		}
		c.useExtended.Store(true)
	}

	index, ret := d.GetIndex()
	if ret != nvml.SUCCESS {
		return nil, lastTs, ret
	}
	samples, ret := fetchExtended(index, lastTs)
	return samples, lastTs, ret
}
