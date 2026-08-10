/*
Copyright 2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
