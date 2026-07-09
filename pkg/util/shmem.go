package util

// Shared-memory ABI constants mirrored from the C side (library MAX_PIDS /
// MAX_DEVICE_COUNT). They live here, free of any cgo/nvml dependency, so the
// packages that only need the dimensions (e.g. vmem) do not have to import a
// cgo-tainted package to get them.
const (
	// MaxDevicePids is the per-device process capacity (C MAX_PIDS).
	MaxDevicePids = 1024
	// MaxDeviceCount is the maximum number of devices (C MAX_DEVICE_COUNT).
	MaxDeviceCount = 16
)
