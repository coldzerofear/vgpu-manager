package watcher

import (
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

type utilizationStrategy int32

const (
	strategyUninitialized utilizationStrategy = iota
	strategyUseLegacy
	strategyUseExtended
	strategyUnsupported
)

type DeviceUtilInterface interface {
	DeviceGetEnhanceCompatibilityProcessUtilSamples(nvml.Device) ([]nvml.ProcessUtilizationSample, uint64, nvml.Return)
	DeviceGetEnhanceCompatibilityProcessUtilSamplesByCount(nvml.Device, uint32) ([]nvml.ProcessUtilizationSample, uint64, nvml.Return)
	DeviceGetComputeRunningProcessesByCount(nvml.Device, uint32) ([]nvml.ProcessInfo, nvml.Return)
	DeviceGetGraphicsRunningProcessesByCount(nvml.Device, uint32) ([]nvml.ProcessInfo, nvml.Return)
	DeviceGetProcessUtilizationByCount(nvml.Device, uint64, uint32) ([]nvml.ProcessUtilizationSample, nvml.Return)
	DeviceGetProcessesUtilization(nvml.Device, uint64) ([]nvml.ProcessUtilizationInfo_v1, nvml.Return)
	DeviceGetProcessesUtilizationByCount(nvml.Device, uint64, uint32) ([]nvml.ProcessUtilizationInfo_v1, nvml.Return)
}

type deviceUtilAdapter struct {
	strategy atomic.Int32
}

func (c *deviceUtilAdapter) getStrategy(d nvml.Device) utilizationStrategy {
	strategy := utilizationStrategy(c.strategy.Load())
	if strategy == strategyUninitialized {
		detectStrategy := c.detectStrategy(d)
		if c.strategy.CompareAndSwap(int32(strategy), int32(detectStrategy)) {
			strategy = detectStrategy
		} else {
			strategy = utilizationStrategy(c.strategy.Load())
		}
	}
	return strategy
}

func NewDeviceUtilAdapter() DeviceUtilInterface {
	adapter := &deviceUtilAdapter{}
	adapter.strategy.Store(int32(strategyUninitialized))
	return adapter
}

func (c *deviceUtilAdapter) detectStrategy(d nvml.Device) utilizationStrategy {
	_, ret := d.GetProcessUtilization(uint64(time.Now().Add(-time.Second).UnixMicro()))
	// High frequency calls to GetProcessUtilization may return NVML_ERROR_NOT_FOUND, which is a normal phenomenon
	if ret == nvml.SUCCESS || ret == nvml.ERROR_INSUFFICIENT_SIZE || ret == nvml.ERROR_NOT_FOUND {
		return strategyUseLegacy
	}

	if ret == nvml.ERROR_NOT_SUPPORTED {
		if err := nvml.Extensions().LookupSymbol("nvmlDeviceGetProcessesUtilizationInfo"); err == nil {
			_, ret = d.GetProcessesUtilizationInfo()
			// High frequency calls to GetProcessesUtilizationInfo may return NVML_ERROR_NOT_FOUND, which is a normal phenomenon
			if ret == nvml.SUCCESS || ret == nvml.ERROR_INSUFFICIENT_SIZE || ret == nvml.ERROR_NOT_FOUND {
				return strategyUseExtended
			}
		}
	}
	return strategyUnsupported
}

// DeviceGetComputeRunningProcessesByCount When the actual result set returned by nvml exceeds infoCount, it will be truncated to infoCount instead of returning ERROR_INSUFFICIENT_SIZE
func (c *deviceUtilAdapter) DeviceGetComputeRunningProcessesByCount(d nvml.Device, infoCount uint32) ([]nvml.ProcessInfo, nvml.Return) {
	count := infoCount
	for {
		infos, ret := d.GetComputeRunningProcessesByCount(count)
		if ret == nvml.SUCCESS {
			return infos[:min(len(infos), int(infoCount))], ret
		}
		if ret != nvml.ERROR_INSUFFICIENT_SIZE {
			return nil, ret
		}
		count *= 2
	}
}

// DeviceGetGraphicsRunningProcessesByCount When the actual result set returned by nvml exceeds infoCount, it will be truncated to infoCount instead of returning ERROR_INSUFFICIENT_SIZE
func (c *deviceUtilAdapter) DeviceGetGraphicsRunningProcessesByCount(d nvml.Device, infoCount uint32) ([]nvml.ProcessInfo, nvml.Return) {
	count := infoCount
	for {
		infos, ret := d.GetGraphicsRunningProcessesByCount(count)
		if ret == nvml.SUCCESS {
			return infos[:min(len(infos), int(infoCount))], ret
		}
		if ret != nvml.ERROR_INSUFFICIENT_SIZE {
			return nil, ret
		}
		count *= 2
	}
}

// DeviceGetProcessesUtilization Query the utilization rate of all processes existing on the device
func (c *deviceUtilAdapter) DeviceGetProcessesUtilization(d nvml.Device, lastTs uint64) ([]nvml.ProcessUtilizationInfo_v1, nvml.Return) {
	processesUtilInfo := nvml.ProcessesUtilizationInfo{
		ProcessSamplesCount: 0,
		LastSeenTimeStamp:   lastTs,
	}
	for {
		ret := d.GetProcessesUtilizationByInfo(&processesUtilInfo)
		if ret == nvml.SUCCESS {
			if processesUtilInfo.ProcUtilArray == nil {
				return []nvml.ProcessUtilizationInfo_v1{}, ret
			}
			infos := unsafe.Slice(processesUtilInfo.ProcUtilArray, processesUtilInfo.ProcessSamplesCount)
			return infos, ret
		}
		if ret != nvml.ERROR_INSUFFICIENT_SIZE {
			return nil, ret
		}
		// Build capacity using the results returned above
		count := processesUtilInfo.ProcessSamplesCount
		array := make([]nvml.ProcessUtilizationInfo_v1, count)
		processesUtilInfo.ProcUtilArray = &array[0]
	}
}

// DeviceGetProcessesUtilizationByCount When the actual result set returned by nvml exceeds infoCount, it will be truncated to infoCount instead of returning ERROR_INSUFFICIENT_SIZE
func (c *deviceUtilAdapter) DeviceGetProcessesUtilizationByCount(d nvml.Device, lastTs uint64, infoCount uint32) ([]nvml.ProcessUtilizationInfo_v1, nvml.Return) {
	infos, ret := c.DeviceGetProcessesUtilization(d, lastTs)
	if ret != nvml.SUCCESS {
		return nil, ret
	}
	return infos[:min(len(infos), int(infoCount))], ret
}

// DeviceGetProcessUtilizationByCount When the actual result set returned by nvml exceeds sampleCount, it will be truncated to sampleCount instead of returning ERROR_INSUFFICIENT_SIZE
func (c *deviceUtilAdapter) DeviceGetProcessUtilizationByCount(d nvml.Device, lastTs uint64, sampleCount uint32) ([]nvml.ProcessUtilizationSample, nvml.Return) {
	processSamplesCount := sampleCount
	for {
		utilSamples, ret := d.GetProcessUtilizationByCount(lastTs, processSamplesCount)
		if ret == nvml.SUCCESS {
			return utilSamples[:min(len(utilSamples), int(sampleCount))], ret
		}
		if ret != nvml.ERROR_INSUFFICIENT_SIZE {
			return nil, ret
		}
		processSamplesCount *= 2
	}
}

// DeviceGetEnhanceCompatibilityProcessUtilSamples Enhance compatibility by querying the utilization information of all processes on the device
func (c *deviceUtilAdapter) DeviceGetEnhanceCompatibilityProcessUtilSamples(d nvml.Device) ([]nvml.ProcessUtilizationSample, uint64, nvml.Return) {
	strategy := c.getStrategy(d)
	lastTs := uint64(time.Now().Add(-time.Second).UnixMicro())
	switch strategy {
	case strategyUseLegacy:
		samples, ret := d.GetProcessUtilization(lastTs)
		if ret != nvml.SUCCESS {
			return nil, lastTs, ret
		}
		return samples, lastTs, ret
	case strategyUseExtended:
		utilSamples, rt := c.DeviceGetProcessesUtilization(d, lastTs)
		if rt != nvml.SUCCESS {
			return nil, lastTs, rt
		}
		samples := make([]nvml.ProcessUtilizationSample, len(utilSamples))
		for index, sample := range utilSamples {
			samples[index] = nvml.ProcessUtilizationSample{
				Pid:       sample.Pid,
				TimeStamp: sample.TimeStamp,
				SmUtil:    sample.SmUtil,
				MemUtil:   sample.MemUtil,
				EncUtil:   sample.EncUtil,
				DecUtil:   sample.DecUtil,
			}
		}
		return samples, lastTs, rt
	default:
		return nil, 0, nvml.ERROR_NOT_SUPPORTED
	}
}

// DeviceGetEnhanceCompatibilityProcessUtilSamplesByCount Compared to DeviceGetEnhanceCompatibilityProcessUtilSamples,
// When the actual result set returned by nvml exceeds sampleCount, it will be truncated to sampleCount instead of returning ERROR_INSUFFICIENT_SIZE
func (c *deviceUtilAdapter) DeviceGetEnhanceCompatibilityProcessUtilSamplesByCount(d nvml.Device, sampleCount uint32) ([]nvml.ProcessUtilizationSample, uint64, nvml.Return) {
	strategy := c.getStrategy(d)
	lastTs := uint64(time.Now().Add(-time.Second).UnixMicro())
	switch strategy {
	case strategyUseLegacy:
		samples, ret := c.DeviceGetProcessUtilizationByCount(d, lastTs, sampleCount)
		if ret != nvml.SUCCESS {
			return nil, lastTs, ret
		}
		return samples, lastTs, ret
	case strategyUseExtended:
		utilSamples, rt := c.DeviceGetProcessesUtilizationByCount(d, lastTs, sampleCount)
		if rt != nvml.SUCCESS {
			return nil, lastTs, rt
		}
		samples := make([]nvml.ProcessUtilizationSample, len(utilSamples))
		for index, sample := range utilSamples {
			samples[index] = nvml.ProcessUtilizationSample{
				Pid:       sample.Pid,
				TimeStamp: sample.TimeStamp,
				SmUtil:    sample.SmUtil,
				MemUtil:   sample.MemUtil,
				EncUtil:   sample.EncUtil,
				DecUtil:   sample.DecUtil,
			}
		}
		return samples, lastTs, rt
	default:
		return nil, 0, nvml.ERROR_NOT_SUPPORTED
	}
}
