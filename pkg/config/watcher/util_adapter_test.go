package watcher

import (
	"testing"
	"unsafe"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/go-nvml/pkg/nvml/mock"
)

type symErr struct{}

func (*symErr) Error() string { return "symbol not found" }

func extendedMock(found bool) nvml.ExtendedInterface {
	return &mock.ExtendedInterface{
		LookupSymbolFunc: func(string) error {
			if found {
				return nil
			}
			return &symErr{}
		},
	}
}

// byInfoTwoCall implements GetProcessesUtilizationByInfo the way the driver does:
// the first call (nil array) reports the needed count via INSUFFICIENT_SIZE, the
// second fills the caller-provided array and succeeds. It also asserts the
// adapter actually provided a buffer of the reported size on the second call.
func byInfoTwoCall(t *testing.T, total uint32) func(*nvml.ProcessesUtilizationInfo) nvml.Return {
	return func(info *nvml.ProcessesUtilizationInfo) nvml.Return {
		if info.ProcUtilArray == nil {
			info.ProcessSamplesCount = total
			return nvml.ERROR_INSUFFICIENT_SIZE
		}
		if info.ProcessSamplesCount < total {
			t.Fatalf("second call buffer holds %d, want >= %d", info.ProcessSamplesCount, total)
		}
		samples := unsafe.Slice(info.ProcUtilArray, int(total))
		for k := range samples {
			samples[k] = nvml.ProcessUtilizationInfo_v1{Pid: uint32(1000 + k), SmUtil: uint32(k)}
		}
		info.ProcessSamplesCount = total
		return nvml.SUCCESS
	}
}

// Blackwell: legacy reports NOT_SUPPORTED, so the extended two-call path runs and
// its ProcessUtilizationInfo_v1 results are converted to ProcessUtilizationSample.
func TestExtendedPathTwoCall(t *testing.T) {
	const total = 3
	d := &mock.Device{
		GetIndexFunc:              func() (int, nvml.Return) { return 0, nvml.SUCCESS },
		GetProcessUtilizationFunc: func(uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) { return nil, nvml.ERROR_NOT_SUPPORTED },
		GetProcessesUtilizationInfoFunc: func() (nvml.ProcessesUtilizationInfo, nvml.Return) {
			return nvml.ProcessesUtilizationInfo{}, nvml.ERROR_INSUFFICIENT_SIZE
		},
		GetProcessesUtilizationByInfoFunc: byInfoTwoCall(t, total),
	}
	adapter := NewDeviceUtilAdapter(WithExtendedInterface(extendedMock(true)))
	samples, _, rt := adapter.DeviceGetEnhanceCompatibilityProcessUtilSamplesByCount(d, MaxPids)
	if rt != nvml.SUCCESS {
		t.Fatalf("ret = %v, want SUCCESS", rt)
	}
	if len(samples) != total {
		t.Fatalf("got %d samples, want %d", len(samples), total)
	}
	for k, s := range samples {
		if s.Pid != uint32(1000+k) || s.SmUtil != uint32(k) {
			t.Fatalf("sample[%d] = %+v, want pid %d sm %d", k, s, 1000+k, k)
		}
	}
}

// INSUFFICIENT_SIZE with a zero count must not index &array[0] on an empty slice.
func TestExtendedZeroCountNoPanic(t *testing.T) {
	d := &mock.Device{
		GetIndexFunc:              func() (int, nvml.Return) { return 0, nvml.SUCCESS },
		GetProcessUtilizationFunc: func(uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) { return nil, nvml.ERROR_NOT_SUPPORTED },
		GetProcessesUtilizationInfoFunc: func() (nvml.ProcessesUtilizationInfo, nvml.Return) {
			return nvml.ProcessesUtilizationInfo{}, nvml.ERROR_INSUFFICIENT_SIZE
		},
		GetProcessesUtilizationByInfoFunc: func(info *nvml.ProcessesUtilizationInfo) nvml.Return {
			info.ProcessSamplesCount = 0 // INSUFFICIENT_SIZE but nothing to size
			return nvml.ERROR_INSUFFICIENT_SIZE
		},
	}
	adapter := NewDeviceUtilAdapter(WithExtendedInterface(extendedMock(true)))
	_, _, rt := adapter.DeviceGetEnhanceCompatibilityProcessUtilSamplesByCount(d, MaxPids)
	if rt != nvml.ERROR_INSUFFICIENT_SIZE {
		t.Fatalf("ret = %v, want ERROR_INSUFFICIENT_SIZE", rt)
	}
}

// Legacy driver: GetProcessUtilization works, extended path never used.
func TestLegacyStrategySelected(t *testing.T) {
	want := []nvml.ProcessUtilizationSample{{Pid: 7, SmUtil: 9}}
	d := &mock.Device{
		GetIndexFunc:                     func() (int, nvml.Return) { return 0, nvml.SUCCESS },
		GetProcessUtilizationFunc:        func(uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) { return want, nvml.SUCCESS },
		GetProcessUtilizationByCountFunc: func(uint64, uint32) ([]nvml.ProcessUtilizationSample, nvml.Return) { return want, nvml.SUCCESS },
	}
	adapter := NewDeviceUtilAdapter(WithExtendedInterface(extendedMock(true)))
	got, _, rt := adapter.DeviceGetEnhanceCompatibilityProcessUtilSamplesByCount(d, MaxPids)
	if rt != nvml.SUCCESS || len(got) != 1 || got[0].Pid != 7 {
		t.Fatalf("ret=%v got=%+v, want SUCCESS pid 7", rt, got)
	}
}

// No extended symbol -> unsupported, and the extended call is never made.
func TestUnsupportedWhenSymbolMissing(t *testing.T) {
	d := &mock.Device{
		GetIndexFunc:              func() (int, nvml.Return) { return 0, nvml.SUCCESS },
		GetProcessUtilizationFunc: func(uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) { return nil, nvml.ERROR_NOT_SUPPORTED },
		GetProcessesUtilizationByInfoFunc: func(*nvml.ProcessesUtilizationInfo) nvml.Return {
			t.Fatal("extended path must not run when the symbol is absent")
			return nvml.SUCCESS
		},
	}
	adapter := NewDeviceUtilAdapter(WithExtendedInterface(extendedMock(false)))
	if _, _, rt := adapter.DeviceGetEnhanceCompatibilityProcessUtilSamplesByCount(d, MaxPids); rt != nvml.ERROR_NOT_SUPPORTED {
		t.Fatalf("ret = %v, want ERROR_NOT_SUPPORTED", rt)
	}
}
