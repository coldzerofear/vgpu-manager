/*
Copyright 2025-2026 coldzerofear

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

package nvidia

import (
	"fmt"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/docker/go-units"
	"k8s.io/klog/v2"
)

// resolveDeviceMemory decides what a device's memory size is, given what NVML
// answered and the configured override (in MiB, 0 = unset).
//
// A device with no framebuffer of its own answers NVML_ERROR_NOT_SUPPORTED (or
// reports a zero total): the GPU shares one physical pool with the CPU, which
// is what NVIDIA's integrated parts do - GB10 / DGX Spark being the reason this
// exists. Such a device is reported as unified, and its size can only come from
// the operator, since nothing on the device can be asked for it.
//
// Returned unified says "no framebuffer of its own", NOT "the size is unknown":
// it stays true when the override supplies a size, because that is what the
// oversold check and the usage-metric fallback need to know.
func resolveDeviceMemory(ret nvml.Return, reported uint64, overrideMB uint64) (total uint64, unified bool, err error) {
	switch {
	case ret == nvml.SUCCESS && reported > 0:
		return reported, false, nil
	case ret == nvml.SUCCESS || ret == nvml.ERROR_NOT_SUPPORTED:
		// Unified memory: no size to read.
		if overrideMB > 0 {
			return overrideMB * units.MiB, true, nil
		}
		return 0, true, nil
	default:
		return 0, false, ret
	}
}

// deviceMemoryLogged remembers what was last said about each device, because
// GetGpuInfo is also on the metrics path: the monitor calls it once per device
// per scrape (every second by default), and a line per scrape would bury
// everything else. A device whose state changes - an override rolled out, a
// GPU replaced - says so again.
var deviceMemoryLogged sync.Map

// logDeviceMemory explains, once per state per device, which of the three
// states the device ended up in. A node whose pods silently run without memory
// isolation has to say so somewhere.
func logDeviceMemory(index int, total uint64, unified bool, overrideMB uint64) {
	state := fmt.Sprintf("%v/%d/%d", unified, total, overrideMB)
	if last, ok := deviceMemoryLogged.Load(index); ok && last == state {
		return
	}
	deviceMemoryLogged.Store(index, state)
	switch {
	case !unified && overrideMB > 0:
		klog.Infof("device %d reports %d MiB of its own memory, ignoring the configured "+
			"memory override of %d MiB", index, total/units.MiB, overrideMB)
	case unified && total > 0:
		klog.Infof("device %d has no memory of its own (unified memory architecture), "+
			"using the configured override of %d MiB", index, overrideMB)
	case unified:
		klog.Warningf("device %d has no memory of its own (unified memory architecture) and no "+
			"memory override is configured: the device registers with 0 memory, pods requesting "+
			"vGPU memory will not be scheduled on it, and pods that request none run WITHOUT "+
			"memory isolation", index)
	}
}
