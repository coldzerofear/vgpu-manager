package watcher

import (
	"sync"
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
	DeviceGetProcessUtilSamples(nvml.Device) ([]nvml.ProcessUtilizationSample, uint64, nvml.Return)
}

type deviceUtilCache struct {
	once     sync.Once
	strategy atomic.Int32
}

func NewDeviceUtilCache() *deviceUtilCache {
	return &deviceUtilCache{}
}

func (c *deviceUtilCache) probe(d nvml.Device) {
	c.once.Do(func() {
		strat := c.detectStrategy(d)
		c.strategy.Store(int32(strat))
	})
}

func (c *deviceUtilCache) detectStrategy(d nvml.Device) utilizationStrategy {
	lastTs := uint64(time.Now().Add(-1 * time.Second).UnixMicro())
	_, rt := d.GetProcessUtilization(lastTs)
	// High frequency calls to GetProcessUtilization may return NVML_ERROR_NOT_FOUND, which is a normal phenomenon
	if rt == nvml.SUCCESS || rt == nvml.ERROR_INSUFFICIENT_SIZE || rt == nvml.ERROR_NOT_FOUND {
		return strategyUseLegacy
	}

	if rt == nvml.ERROR_NOT_SUPPORTED {
		if nvml.Extensions().LookupSymbol("nvmlDeviceGetProcessesUtilizationInfo") != nil {
			if _, rt = d.GetProcessesUtilizationInfo(); rt == nvml.SUCCESS {
				return strategyUseExtended
			}
		}
	}
	return strategyUnsupported
}

func (c *deviceUtilCache) DeviceGetProcessUtilSamples(d nvml.Device) ([]nvml.ProcessUtilizationSample, uint64, nvml.Return) {
	s := utilizationStrategy(c.strategy.Load())
	if s == strategyUninitialized {
		c.probe(d)
		s = utilizationStrategy(c.strategy.Load())
	}

	lastTs := uint64(time.Now().Add(-1 * time.Second).UnixMicro())
	switch s {
	case strategyUseLegacy:
		samples, rt := d.GetProcessUtilization(lastTs)
		if rt != nvml.SUCCESS {
			return nil, lastTs, rt
		}
		return samples, lastTs, rt
	case strategyUseExtended:
		utilInfo, rt := d.GetProcessesUtilizationInfo()
		if rt != nvml.SUCCESS {
			return nil, lastTs, rt
		}
		lastTs = utilInfo.LastSeenTimeStamp
		samples := make([]nvml.ProcessUtilizationSample, utilInfo.ProcessSamplesCount)
		for index, sample := range unsafe.Slice(utilInfo.ProcUtilArray, utilInfo.ProcessSamplesCount) {
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
		return nil, lastTs, nvml.ERROR_NOT_SUPPORTED
	}
}
