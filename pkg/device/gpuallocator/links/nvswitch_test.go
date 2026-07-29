package links

import (
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

var (
	gpu1Pci   = pciInfoWithBusID("0000:01:00.0")
	gpu2Pci   = pciInfoWithBusID("0000:02:00.0")
	switchPci = pciInfoWithBusID("0000:ff:00.0")
)

// linkDev builds a device whose first `enabled` NVLinks are up, every one of
// them reporting `remote` as its remote PCI and `remoteType` as its remote
// device type; `self` is the device's own PCI info.
func linkDev(enabled int, remote nvml.PciInfo, remoteType nvml.IntNvLinkDeviceType, self nvml.PciInfo) *testDevice {
	return &testDevice{
		getNvLinkState: func(i int) (nvml.EnableState, nvml.Return) {
			if i < enabled {
				return nvml.FEATURE_ENABLED, nvml.SUCCESS
			}
			return nvml.FEATURE_DISABLED, nvml.SUCCESS
		},
		getNvLinkRemotePciInfo: func(int) (nvml.PciInfo, nvml.Return) {
			return remote, nvml.SUCCESS
		},
		getNvLinkRemoteDeviceType: func(int) (nvml.IntNvLinkDeviceType, nvml.Return) {
			return remoteType, nvml.SUCCESS
		},
		getPciInfo: func() (nvml.PciInfo, nvml.Return) {
			return self, nvml.SUCCESS
		},
	}
}

// TestGetNVLinkNVSwitch covers the NVSwitch-fabric path: on HGX/DGX every link
// terminates at a switch, so remote PCI never equals the peer GPU and the
// direct-match count is always zero. Before this was handled, a fully connected
// 8-GPU board reported "no NVLink" and every NVLink-dependent scheduling
// decision silently degraded.
func TestGetNVLinkNVSwitch(t *testing.T) {
	t.Run("direct GPU-GPU links are counted by matching remote bus id", func(t *testing.T) {
		dev1 := linkDev(4, gpu2Pci, nvml.NVLINK_DEVICE_TYPE_GPU, gpu1Pci)
		dev2 := linkDev(4, gpu1Pci, nvml.NVLINK_DEVICE_TYPE_GPU, gpu2Pci)

		got, err := GetNVLink(dev1, dev2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != FourNVLINKLinks {
			t.Fatalf("got %v, want FourNVLINKLinks", got)
		}
	})

	t.Run("NVSwitch fabric is detected when BOTH sides attach to a switch", func(t *testing.T) {
		// Remote PCI is the switch on both sides → zero direct matches.
		dev1 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu1Pci)
		dev2 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu2Pci)

		got, err := GetNVLink(dev1, dev2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != SixNVLINKLinks {
			t.Fatalf("got %v, want SixNVLINKLinks (switch-attached pair)", got)
		}
	})

	t.Run("pair width is the weaker side's link count", func(t *testing.T) {
		dev1 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu1Pci)
		dev2 := linkDev(4, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu2Pci)

		got, err := GetNVLink(dev1, dev2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != FourNVLINKLinks {
			t.Fatalf("got %v, want FourNVLINKLinks (min of 6 and 4)", got)
		}
	})

	t.Run("only one side on the switch is NOT connected", func(t *testing.T) {
		dev1 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu1Pci)
		dev2 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_GPU, gpu2Pci)

		got, err := GetNVLink(dev1, dev2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != P2PLinkUnknown {
			t.Fatalf("got %v, want P2PLinkUnknown", got)
		}
	})

	t.Run("neither direct nor switch -> unknown (PCIe-only box)", func(t *testing.T) {
		dev1 := linkDev(0, switchPci, nvml.NVLINK_DEVICE_TYPE_GPU, gpu1Pci)
		dev2 := linkDev(0, switchPci, nvml.NVLINK_DEVICE_TYPE_GPU, gpu2Pci)

		got, err := GetNVLink(dev1, dev2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != P2PLinkUnknown {
			t.Fatalf("got %v, want P2PLinkUnknown", got)
		}
	})

	t.Run("ERROR_GPU_IS_LOST on remote-device-type skips only that link", func(t *testing.T) {
		const lost = 2
		mk := func(self nvml.PciInfo) *testDevice {
			d := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, self)
			d.getNvLinkRemoteDeviceType = func(i int) (nvml.IntNvLinkDeviceType, nvml.Return) {
				if i == lost {
					return nvml.NVLINK_DEVICE_TYPE_UNKNOWN, nvml.ERROR_GPU_IS_LOST
				}
				return nvml.NVLINK_DEVICE_TYPE_SWITCH, nvml.SUCCESS
			}
			return d
		}

		got, err := GetNVLink(mk(gpu1Pci), mk(gpu2Pci))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != FiveNVLINKLinks {
			t.Fatalf("got %v, want FiveNVLINKLinks (6 links, 1 lost)", got)
		}
	})

	t.Run("unrelated remote-device-type error surfaces", func(t *testing.T) {
		dev1 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu1Pci)
		dev1.getNvLinkRemoteDeviceType = func(int) (nvml.IntNvLinkDeviceType, nvml.Return) {
			return nvml.NVLINK_DEVICE_TYPE_UNKNOWN, nvml.ERROR_UNKNOWN
		}
		dev2 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu2Pci)

		if _, err := GetNVLink(dev1, dev2); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("direct connect at the 18-link maximum", func(t *testing.T) {
		dev1 := linkDev(18, gpu2Pci, nvml.NVLINK_DEVICE_TYPE_GPU, gpu1Pci)
		dev2 := linkDev(18, gpu1Pci, nvml.NVLINK_DEVICE_TYPE_GPU, gpu2Pci)

		got, err := GetNVLink(dev1, dev2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != EighteenNVLINKLinks {
			t.Fatalf("got %v, want EighteenNVLINKLinks", got)
		}
	})

	t.Run("switch-attached count ABOVE the top tier saturates, never Unknown", func(t *testing.T) {
		// NVML allows up to NVLINK_MAX_LINKS (36) while the enum stops at 18.
		// Falling back to Unknown here would claim "no NVLink" for a fully
		// fabric-connected pair — the very failure this file guards against.
		dev1 := linkDev(nvml.NVLINK_MAX_LINKS, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu1Pci)
		dev2 := linkDev(nvml.NVLINK_MAX_LINKS, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu2Pci)

		got, err := GetNVLink(dev1, dev2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != EighteenNVLINKLinks {
			t.Fatalf("got %v, want EighteenNVLINKLinks (saturated, NOT Unknown)", got)
		}
	})

	t.Run("only switch-terminated links are counted when types are mixed", func(t *testing.T) {
		// 10 links to a switch, the rest to GPUs: the pair width is 10.
		mixed := func(self nvml.PciInfo) *testDevice {
			d := linkDev(20, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, self)
			d.getNvLinkRemoteDeviceType = func(i int) (nvml.IntNvLinkDeviceType, nvml.Return) {
				if i < 10 {
					return nvml.NVLINK_DEVICE_TYPE_SWITCH, nvml.SUCCESS
				}
				return nvml.NVLINK_DEVICE_TYPE_GPU, nvml.SUCCESS
			}
			return d
		}

		got, err := GetNVLink(mixed(gpu1Pci), mixed(gpu2Pci))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != TenNVLINKLinks {
			t.Fatalf("got %v, want TenNVLINKLinks (only the 10 switch links count)", got)
		}
	})

	t.Run("asymmetric width is symmetric in argument order", func(t *testing.T) {
		wide := func(self nvml.PciInfo) *testDevice {
			return linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, self)
		}
		narrow := func(self nvml.PciInfo) *testDevice {
			return linkDev(4, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, self)
		}

		fwd, err := GetNVLink(wide(gpu1Pci), narrow(gpu2Pci))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rev, err := GetNVLink(narrow(gpu1Pci), wide(gpu2Pci))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if fwd != FourNVLINKLinks || rev != FourNVLINKLinks {
			t.Fatalf("fwd=%v rev=%v, want both FourNVLINKLinks", fwd, rev)
		}
	})

	t.Run("dev2 not on the switch is NOT connected", func(t *testing.T) {
		dev1 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu1Pci)
		dev2 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_GPU, gpu2Pci)

		got, err := GetNVLink(dev1, dev2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != P2PLinkUnknown {
			t.Fatalf("got %v, want P2PLinkUnknown", got)
		}
	})

	t.Run("dev1 state error surfaces", func(t *testing.T) {
		dev1 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu1Pci)
		dev1.getNvLinkState = func(int) (nvml.EnableState, nvml.Return) {
			return nvml.FEATURE_DISABLED, nvml.ERROR_UNKNOWN
		}
		dev2 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu2Pci)

		if _, err := GetNVLink(dev1, dev2); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("dev2 state error surfaces", func(t *testing.T) {
		dev1 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu1Pci)
		dev2 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu2Pci)
		dev2.getNvLinkState = func(int) (nvml.EnableState, nvml.Return) {
			return nvml.FEATURE_DISABLED, nvml.ERROR_UNKNOWN
		}

		if _, err := GetNVLink(dev1, dev2); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("dev2 remote-device-type error surfaces", func(t *testing.T) {
		dev1 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu1Pci)
		dev2 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu2Pci)
		dev2.getNvLinkRemoteDeviceType = func(int) (nvml.IntNvLinkDeviceType, nvml.Return) {
			return nvml.NVLINK_DEVICE_TYPE_UNKNOWN, nvml.ERROR_UNKNOWN
		}

		if _, err := GetNVLink(dev1, dev2); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("dev1 off the fabric short-circuits without consulting dev2", func(t *testing.T) {
		// Once dev1 is known to have no switch-attached link the pair cannot be
		// fabric-connected, so dev2 is never probed. That keeps an unrelated NVML
		// error on dev2 from aborting the WHOLE node's topology discovery
		// (NewDevices treats any error as fatal). dev2's own health still
		// surfaces when it is the first argument of another pair.
		dev1 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_GPU, gpu1Pci)
		dev2 := linkDev(6, switchPci, nvml.NVLINK_DEVICE_TYPE_SWITCH, gpu2Pci)
		dev2.getNvLinkState = func(int) (nvml.EnableState, nvml.Return) {
			t.Fatalf("dev2 must not be probed once dev1 is known to be off the fabric")
			return nvml.FEATURE_DISABLED, nvml.ERROR_UNKNOWN
		}

		got, err := GetNVLink(dev1, dev2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != P2PLinkUnknown {
			t.Fatalf("got %v, want P2PLinkUnknown", got)
		}
	})
}

func TestNvlinkCountToType(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want P2PLinkType
	}{
		{-1, P2PLinkUnknown},
		{0, P2PLinkUnknown},
		{1, SingleNVLINKLink},
		{2, TwoNVLINKLinks},
		{18, EighteenNVLINKLinks},
		{36, EighteenNVLINKLinks}, // saturates, never indexes out of range
	} {
		if got := nvlinkCountToType(tc.n); got != tc.want {
			t.Errorf("nvlinkCountToType(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}
