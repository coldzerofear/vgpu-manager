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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHasFabricManagerFabric(t *testing.T) {
	t.Run("no fabric hardware", func(t *testing.T) {
		hostRoot := t.TempDir()

		hasFabric, err := HasFabricManagerFabric(hostRoot)
		require.NoError(t, err)
		require.False(t, hasFabric)
	})

	t.Run("NVSwitch devices present", func(t *testing.T) {
		hostRoot := t.TempDir()
		devicesDir := filepath.Join(hostRoot, "proc", "driver", "nvidia-nvswitch", "devices")
		require.NoError(t, os.MkdirAll(devicesDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(devicesDir, "0000:00:00.0"), []byte("device"), 0o644))

		hasFabric, err := HasFabricManagerFabric(hostRoot)
		require.NoError(t, err)
		require.True(t, hasFabric)
	})

	t.Run("empty NVSwitch devices directory", func(t *testing.T) {
		hostRoot := t.TempDir()
		devicesDir := filepath.Join(hostRoot, "proc", "driver", "nvidia-nvswitch", "devices")
		require.NoError(t, os.MkdirAll(devicesDir, 0o755))

		hasFabric, err := HasFabricManagerFabric(hostRoot)
		require.NoError(t, err)
		require.False(t, hasFabric)
	})

	t.Run("NVLink5 SW_MNG marker in VPD", func(t *testing.T) {
		hostRoot := t.TempDir()
		vpdFile := filepath.Join(hostRoot, "sys", "class", "infiniband", "mlx5_0", "device", "vpd")
		require.NoError(t, os.MkdirAll(filepath.Dir(vpdFile), 0o755))
		require.NoError(t, os.WriteFile(vpdFile, []byte("part SW_MNG other"), 0o644))

		hasFabric, err := HasFabricManagerFabric(hostRoot)
		require.NoError(t, err)
		require.True(t, hasFabric)
	})

	t.Run("NVSwitch takes precedence over NVLink5 probe", func(t *testing.T) {
		hostRoot := t.TempDir()

		devicesDir := filepath.Join(hostRoot, "proc", "driver", "nvidia-nvswitch", "devices")
		require.NoError(t, os.MkdirAll(devicesDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(devicesDir, "switch0"), []byte("device"), 0o644))

		hasFabric, err := HasFabricManagerFabric(hostRoot)
		require.NoError(t, err)
		require.True(t, hasFabric)
	})
}
