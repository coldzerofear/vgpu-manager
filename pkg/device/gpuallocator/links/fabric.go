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

package links

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"
)

// A clique key is "<clusterUUID>.<cliqueID>": the identity of the NVLink
// fabric a GPU is attached to. GPUs with different keys cannot reach each
// other over NVLink even when both report switch-attached links.
//
// The empty key means UNKNOWN, not "no fabric": the driver may be too old to
// expose fabric info, the GPU may not be fabric-attached at all, or Fabric
// Manager may still be bringing the fabric up (in which case the cliqueId it
// reports is not yet meaningful). Callers must therefore never read an empty
// key as "a different fabric" — see DifferentFabric.

// DifferentFabric reports whether two clique keys are known to name different
// NVLink fabrics. It is deliberately false whenever either key is unknown, so
// that missing fabric information can only ever leave the caller's previous
// conclusion intact, never invert it.
func DifferentFabric(clique1, clique2 string) bool {
	return clique1 != "" && clique2 != "" && clique1 != clique2
}

// errFabricPending marks a reading taken while the fabric is still coming up:
// unlike every other failure it is expected to resolve on its own.
var errFabricPending = errors.New("fabric not yet completed")

// How long FabricCliques waits for fabrics that are still coming up. Package
// variables so tests can shrink them; sleep likewise.
var (
	fabricSettleTimeout  = 30 * time.Second
	fabricSettleInterval = 2 * time.Second
	sleep                = time.Sleep
)

// FabricCliques returns the clique key of every device in devs, indexed the
// same way. Devices whose fabric identity cannot be established get the empty
// key.
//
// The result is read once, at startup, and kept for the life of the process,
// so a fabric caught mid-initialisation -- the plugin started before Fabric
// Manager finished -- would leave its identity unknown until the next restart,
// and two NVSwitch fabrics in one chassis would be merged for all that time.
// Devices reporting a fabric that has not started or is in progress are
// therefore re-read for up to fabricSettleTimeout. Devices with no fabric
// support never report that, so nodes without NVSwitch never wait.
//
// The accessor is resolved once for the whole node rather than per device: the
// answer depends only on the loaded NVML, and probing per device would repeat
// a symbol lookup N times for no gain.
func FabricCliques(nvmllib nvml.Interface, devs []device.Device) []string {
	keys := make([]string, len(devs))
	read := fabricCliqueReader(nvmllib)
	if read == nil {
		klog.V(4).Info("NVML exports no GPU fabric info; NVLink fabric identities are unknown on this node")
		return keys
	}

	pending := make([]int, 0, len(devs))
	for i := range devs {
		pending = append(pending, i)
	}
	deadline := time.Now().Add(fabricSettleTimeout)
	for {
		var still []int
		for _, i := range pending {
			key, err := read(devs[i])
			switch {
			case err == nil:
				keys[i] = key
			case errors.Is(err, errFabricPending):
				still = append(still, i)
			default:
				klog.V(4).Infof("no NVLink fabric identity for GPU %d: %v", i, err)
			}
		}
		if len(still) == 0 {
			return keys
		}
		if !time.Now().Before(deadline) {
			klog.Warningf("NVLink fabric still initialising on GPUs %v after %v; treating their fabric identity as unknown "+
				"until the next restart, so independent NVSwitch fabrics on this node may be reported as one", still, fabricSettleTimeout)
			return keys
		}
		klog.V(2).Infof("Waiting for the NVLink fabric to finish initialising on GPUs %v", still)
		pending = still
		sleep(fabricSettleInterval)
	}
}

// fabricCliqueReader returns a reader backed by the newest fabric-info entry
// point the loaded NVML actually exports, or nil when it exports neither.
//
// The symbol lookup is mandatory, not an optimisation: the cgo bindings are
// linked with --unresolved-symbols=ignore-in-object-files, so calling an entry
// point that the installed driver does not provide aborts the process instead
// of returning an error.
func fabricCliqueReader(nvmllib nvml.Interface) func(device.Device) (string, error) {
	if nvmllib == nil {
		return nil
	}
	ext := nvmllib.Extensions()
	if ext == nil {
		return nil
	}
	if ext.LookupSymbol("nvmlDeviceGetGpuFabricInfoV") == nil {
		return func(dev device.Device) (string, error) {
			info, ret := dev.GetGpuFabricInfoV().V2()
			if ret != nvml.SUCCESS {
				return "", fmt.Errorf("nvmlDeviceGetGpuFabricInfoV: %v", ret)
			}
			return cliqueKey(info.ClusterUuid, info.CliqueId, info.State, info.Status)
		}
	}
	if ext.LookupSymbol("nvmlDeviceGetGpuFabricInfo") == nil {
		return func(dev device.Device) (string, error) {
			info, ret := dev.GetGpuFabricInfo()
			if ret != nvml.SUCCESS {
				return "", fmt.Errorf("nvmlDeviceGetGpuFabricInfo: %v", ret)
			}
			return cliqueKey(info.ClusterUuid, info.CliqueId, info.State, info.Status)
		}
	}
	return nil
}

// cliqueKey renders one fabric-info reading as a clique key, rejecting any
// reading that is not yet trustworthy. A fabric that has not finished coming
// up reports a cliqueId that does not describe its final shape, so treating it
// as an identity would split GPUs that are about to be peers.
func cliqueKey(clusterUUID [16]uint8, cliqueID uint32, state uint8, status uint32) (string, error) {
	switch state {
	case nvml.GPU_FABRIC_STATE_COMPLETED:
	case nvml.GPU_FABRIC_STATE_NOT_STARTED, nvml.GPU_FABRIC_STATE_IN_PROGRESS:
		return "", fmt.Errorf("fabric state is %d: %w", state, errFabricPending)
	default:
		return "", fmt.Errorf("fabric state is %d, not completed", state)
	}
	if ret := nvml.Return(status); ret != nvml.SUCCESS {
		return "", fmt.Errorf("fabric status: %v", ret)
	}
	if clusterUUID == [16]uint8{} {
		return "", fmt.Errorf("empty fabric cluster uuid")
	}
	return fmt.Sprintf("%s.%d", hex.EncodeToString(clusterUUID[:]), cliqueID), nil
}
