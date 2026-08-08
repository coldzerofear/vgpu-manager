/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fabricmanager

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

const nvlink5SwitchManagedMarker = "SW_MNG"

// HasFabricManagerFabric reports whether the node has an NVSwitch-managed fabric
// that requires NVIDIA Fabric Manager. Detection mirrors the gpu-driver-container
// checks used before starting Fabric Manager.
func HasFabricManagerFabric(hostRoot string) (bool, error) {
	nvswitch, err := hasNVSwitchDevices(hostRoot)
	if err != nil {
		return false, err
	}
	if nvswitch {
		return true, nil
	}

	return hasNVLink5SwitchManagedFabric(hostRoot)
}

func hasNVSwitchDevices(hostRoot string) (bool, error) {
	devicesDir := hostPath(hostRoot, "proc", "driver", "nvidia-nvswitch", "devices")
	entries, err := os.ReadDir(devicesDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", devicesDir, err)
	}
	return len(entries) > 0, nil
}

func hasNVLink5SwitchManagedFabric(hostRoot string) (bool, error) {
	pattern := hostPath(hostRoot, "sys", "class", "infiniband", "*", "device", "vpd")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return false, fmt.Errorf("glob %s: %w", pattern, err)
	}

	for _, vpdFile := range matches {
		data, err := os.ReadFile(vpdFile)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("read %s: %w", vpdFile, err)
		}
		if bytes.Contains(data, []byte(nvlink5SwitchManagedMarker)) {
			return true, nil
		}
	}
	return false, nil
}

func hostPath(hostRoot string, elem ...string) string {
	if hostRoot == "" {
		hostRoot = "/"
	}
	all := append([]string{hostRoot}, elem...)
	return filepath.Join(all...)
}
