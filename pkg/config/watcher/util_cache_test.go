package watcher

import (
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/go-nvml/pkg/nvml/mock"
)

// stubDevice builds a mock whose legacy entry point answers with legacyRet and
// which reports itself at the given index. legacyCalls, when non-nil, counts the
// legacy invocations.
func stubDevice(index int, legacy []nvml.ProcessUtilizationSample, legacyRet nvml.Return, legacyCalls *int) *mock.Device {
	return &mock.Device{
		GetIndexFunc: func() (int, nvml.Return) { return index, nvml.SUCCESS },
		GetProcessUtilizationFunc: func(uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) {
			if legacyCalls != nil {
				*legacyCalls++
			}
			return legacy, legacyRet
		},
	}
}

// withStubs swaps the driver-backed seams for the duration of a test.
func withStubs(t *testing.T,
	fetch func(int, uint64) ([]nvml.ProcessUtilizationSample, nvml.Return),
	exists func() bool,
) {
	t.Helper()
	oldFetch, oldExists := fetchExtended, extendedExists
	fetchExtended, extendedExists = fetch, exists
	t.Cleanup(func() { fetchExtended, extendedExists = oldFetch, oldExists })
}

func symbolFound() bool   { return true }
func symbolMissing() bool { return false }

func neverFetch(t *testing.T) func(int, uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) {
	return func(int, uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) {
		t.Helper()
		t.Fatal("extended entry point must not be used")
		return nil, nvml.SUCCESS
	}
}

// A pre-Blackwell device keeps using the legacy entry point.
func TestLegacyDeviceUsesLegacy(t *testing.T) {
	want := []nvml.ProcessUtilizationSample{{Pid: 42, SmUtil: 7}}
	d := stubDevice(0, want, nvml.SUCCESS, nil)
	withStubs(t, neverFetch(t), symbolFound)

	c := NewDeviceUtilCache()
	got, _, rt := c.DeviceGetProcessUtilSamples(d)
	if rt != nvml.SUCCESS {
		t.Fatalf("ret = %v, want SUCCESS", rt)
	}
	if len(got) != 1 || got[0].Pid != 42 {
		t.Fatalf("samples = %+v, want pid 42", got)
	}
}

// Blackwell: the legacy call reports NOT_SUPPORTED, so the extended entry point
// takes over. The previous implementation could never get here -- it demanded
// SUCCESS from a sizing call that only ever returns INSUFFICIENT_SIZE.
func TestBlackwellDeviceUsesExtended(t *testing.T) {
	want := []nvml.ProcessUtilizationSample{{Pid: 7, SmUtil: 55}}
	fetched := 0
	withStubs(t, func(int, uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) {
		fetched++
		return want, nvml.SUCCESS
	}, symbolFound)

	d := stubDevice(0, nil, nvml.ERROR_NOT_SUPPORTED, nil)
	c := NewDeviceUtilCache()
	got, _, rt := c.DeviceGetProcessUtilSamples(d)
	if rt != nvml.SUCCESS {
		t.Fatalf("ret = %v, want SUCCESS", rt)
	}
	if len(got) != 1 || got[0].Pid != 7 {
		t.Fatalf("samples = %+v, want pid 7", got)
	}
	if fetched != 1 {
		t.Fatalf("extended fetch called %d times, want 1", fetched)
	}
}

// Once NOT_SUPPORTED is seen, the legacy entry point is not probed again: driver
// support does not come back mid-process.
func TestExtendedIsLatched(t *testing.T) {
	legacyCalls := 0
	withStubs(t, func(int, uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) {
		return []nvml.ProcessUtilizationSample{{Pid: 1}}, nvml.SUCCESS
	}, symbolFound)

	d := stubDevice(0, nil, nvml.ERROR_NOT_SUPPORTED, &legacyCalls)
	c := NewDeviceUtilCache()
	for i := 0; i < 3; i++ {
		if _, _, rt := c.DeviceGetProcessUtilSamples(d); rt != nvml.SUCCESS {
			t.Fatalf("call %d: ret = %v, want SUCCESS", i, rt)
		}
	}
	if legacyCalls != 1 {
		t.Fatalf("legacy called %d times, want 1 (latched after the first NOT_SUPPORTED)", legacyCalls)
	}
}

// A driver without the extended symbol reports NOT_SUPPORTED rather than
// pretending the call exists, and never latches.
func TestBlackwellWithoutSymbolIsUnsupported(t *testing.T) {
	legacyCalls := 0
	withStubs(t, neverFetch(t), symbolMissing)

	d := stubDevice(0, nil, nvml.ERROR_NOT_SUPPORTED, &legacyCalls)
	c := NewDeviceUtilCache()
	for i := 0; i < 2; i++ {
		if _, _, rt := c.DeviceGetProcessUtilSamples(d); rt != nvml.ERROR_NOT_SUPPORTED {
			t.Fatalf("call %d: ret = %v, want ERROR_NOT_SUPPORTED", i, rt)
		}
	}
	if legacyCalls != 2 {
		t.Fatalf("legacy called %d times, want 2 (no latch without the symbol)", legacyCalls)
	}
}

// NVML return codes reach the caller untouched.
//
// NOT_FOUND ("no samples newer than lastTs") is the common answer at the
// watcher's poll rate even for a busy GPU. Flattening it into an empty success
// would make the watcher zero its cached sample count on most ticks, collapsing
// reported utilization; callers depend on a non-SUCCESS reply to retain the
// previous batch until it ages out of the sample window.
func TestLegacyReturnCodesArePassedThrough(t *testing.T) {
	for _, rt := range []nvml.Return{
		nvml.ERROR_NOT_FOUND,
		nvml.ERROR_INSUFFICIENT_SIZE,
		nvml.ERROR_GPU_IS_LOST,
		nvml.ERROR_UNKNOWN,
	} {
		withStubs(t, neverFetch(t), symbolFound)
		d := stubDevice(0, nil, rt, nil)
		c := NewDeviceUtilCache()
		got, _, ret := c.DeviceGetProcessUtilSamples(d)
		if ret != rt {
			t.Fatalf("legacy %v: ret = %v, want it passed through", rt, ret)
		}
		if got != nil {
			t.Fatalf("legacy %v: samples = %+v, want none", rt, got)
		}
	}
}

// The same holds on the extended path.
func TestExtendedReturnCodesArePassedThrough(t *testing.T) {
	for _, rt := range []nvml.Return{nvml.ERROR_NOT_FOUND, nvml.ERROR_INSUFFICIENT_SIZE} {
		withStubs(t, func(int, uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) {
			return nil, rt
		}, symbolFound)

		d := stubDevice(0, nil, nvml.ERROR_NOT_SUPPORTED, nil)
		c := NewDeviceUtilCache()
		got, _, ret := c.DeviceGetProcessUtilSamples(d)
		if ret != rt {
			t.Fatalf("extended %v: ret = %v, want it passed through", rt, ret)
		}
		if got != nil {
			t.Fatalf("extended %v: samples = %+v, want none", rt, got)
		}
	}
}

// A transient legacy failure is reported, not mistaken for missing support: the
// next call still goes through the legacy entry point.
func TestTransientErrorDoesNotLatch(t *testing.T) {
	calls := 0
	ret := nvml.ERROR_UNKNOWN
	d := &mock.Device{
		GetIndexFunc: func() (int, nvml.Return) { return 0, nvml.SUCCESS },
		GetProcessUtilizationFunc: func(uint64) ([]nvml.ProcessUtilizationSample, nvml.Return) {
			calls++
			if ret == nvml.SUCCESS {
				return []nvml.ProcessUtilizationSample{{Pid: 1}}, nvml.SUCCESS
			}
			return nil, ret
		},
	}
	withStubs(t, neverFetch(t), symbolFound)
	c := NewDeviceUtilCache()

	if _, _, rt := c.DeviceGetProcessUtilSamples(d); rt != nvml.ERROR_UNKNOWN {
		t.Fatalf("first ret = %v, want ERROR_UNKNOWN", rt)
	}
	ret = nvml.SUCCESS
	got, _, rt := c.DeviceGetProcessUtilSamples(d)
	if rt != nvml.SUCCESS || len(got) != 1 {
		t.Fatalf("after recovery: ret = %v samples = %+v, want SUCCESS with 1 sample", rt, got)
	}
	if calls != 2 {
		t.Fatalf("legacy called %d times, want 2", calls)
	}
}

// The real guard, unstubbed, against a driver that does not export the extended
// entry point. dlsym returns NULL, so the symbol must report absent and the
// fallback must never be entered.
//
// An extern declaration would instead bind to address 0 here and jump to NULL the
// moment the extended path was taken, so this also pins the dlsym approach.
func TestMissingSymbolIsDetectedForReal(t *testing.T) {
	if processesUtilizationInfoAvailable() {
		t.Skip("libnvidia-ml.so.1 exports the symbol on this host")
	}

	legacyCalls := 0
	// Only fetchExtended is stubbed; extendedExists stays wired to the real dlsym.
	oldFetch := fetchExtended
	fetchExtended = neverFetch(t)
	t.Cleanup(func() { fetchExtended = oldFetch })

	d := stubDevice(0, nil, nvml.ERROR_NOT_SUPPORTED, &legacyCalls)
	c := NewDeviceUtilCache()
	got, _, rt := c.DeviceGetProcessUtilSamples(d)
	if rt != nvml.ERROR_NOT_SUPPORTED {
		t.Fatalf("ret = %v, want ERROR_NOT_SUPPORTED", rt)
	}
	if got != nil {
		t.Fatalf("samples = %+v, want none", got)
	}
}

// And a legacy-capable device on such a driver is unaffected: the extended entry
// point is never consulted at all.
func TestOldDriverLegacyPathUnaffected(t *testing.T) {
	want := []nvml.ProcessUtilizationSample{{Pid: 9, SmUtil: 3}}
	oldFetch := fetchExtended
	fetchExtended = neverFetch(t)
	t.Cleanup(func() { fetchExtended = oldFetch })

	d := stubDevice(0, want, nvml.SUCCESS, nil)
	c := NewDeviceUtilCache()
	got, _, rt := c.DeviceGetProcessUtilSamples(d)
	if rt != nvml.SUCCESS || len(got) != 1 || got[0].Pid != 9 {
		t.Fatalf("ret = %v samples = %+v, want SUCCESS with pid 9", rt, got)
	}
}
