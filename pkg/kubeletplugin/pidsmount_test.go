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

package kubeletplugin

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/nri"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// mountDepth mirrors how both CDI and NRI order the OCI mount list: by the
// number of path components in the destination.
func mountDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(os.PathSeparator))
}

func TestPartitionMounts_PidsConfigIsReadOnlyAndSourceExists(t *testing.T) {
	hostRoot := t.TempDir()
	contRoot := t.TempDir()
	manager := &VGPUManager{hostManagerPath: hostRoot, contManagerPath: contRoot}

	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-pids")}}
	partitionKey := "single-vgpu"

	edits, err := manager.GetPartitionMountContainerEdits(claim, partitionKey)
	require.NoError(t, err)
	require.NotNil(t, edits.ContainerEdits)

	wantContainerPath := filepath.Join(contRoot, util.Config, registry.PidsConfig)
	idx := slices.IndexFunc(edits.ContainerEdits.Mounts, func(m *cdispec.Mount) bool {
		return m.ContainerPath == wantContainerPath
	})
	require.NotEqual(t, -1, idx, "pids.config must be mounted on its own")
	mount := edits.ContainerEdits.Mounts[idx]

	assert.Contains(t, mount.Options, "ro", "pids.config must not be writable by the container")
	assert.NotContains(t, mount.Options, "rw")

	// The runtime bind-mounts this path; a missing source aborts container
	// creation, so Prepare has to have materialised it.
	contConfigDir := filepath.Join(contRoot, util.Claims, "claim-pids", partitionKey, util.Config)
	info, err := os.Lstat(filepath.Join(contConfigDir, registry.PidsConfig))
	require.NoError(t, err, "the bind-mount source must exist after Prepare")
	assert.True(t, info.Mode().IsRegular())
	assert.Zero(t, info.Size(), "a fresh partition must not inherit a previous PID list")

	// The enclosing directory stays writable: the library materialises
	// vgpu.config there in the DRA path.
	configIdx := slices.IndexFunc(edits.ContainerEdits.Mounts, func(m *cdispec.Mount) bool {
		return m.ContainerPath == filepath.Join(contRoot, util.Config)
	})
	require.NotEqual(t, -1, configIdx)
	assert.Contains(t, edits.ContainerEdits.Mounts[configIdx].Options, "rw")

	// Nesting only works if the parent is mounted first, which both CDI and NRI
	// guarantee by sorting on destination depth.
	assert.Less(t,
		mountDepth(edits.ContainerEdits.Mounts[configIdx].ContainerPath),
		mountDepth(mount.ContainerPath),
		"the config directory must sort before the file nested in it")
}

func TestNRIPartitionInjection_PidsConfigIsReadOnlyAndSourceExists(t *testing.T) {
	hostRoot := t.TempDir()
	contRoot := t.TempDir()
	manager := &VGPUManager{hostManagerPath: hostRoot, contManagerPath: contRoot}

	inj, err := manager.GetNRIPartitionInjection("claim-nri", "pod", "ns", "pod-uid", "ctr")
	require.NoError(t, err)
	require.NotNil(t, inj)

	wantContainerPath := filepath.Join(contRoot, util.Config, registry.PidsConfig)
	idx := slices.IndexFunc(inj.Mounts, func(m nri.Mount) bool {
		return m.ContainerPath == wantContainerPath
	})
	require.NotEqual(t, -1, idx, "pids.config must be mounted on its own")
	assert.Contains(t, inj.Mounts[idx].Options, "ro")
	assert.NotContains(t, inj.Mounts[idx].Options, "rw")

	info, err := os.Lstat(filepath.Join(inj.ConfigDir, registry.PidsConfig))
	require.NoError(t, err, "the bind-mount source must exist after CreateContainer resolves mounts")
	assert.Zero(t, info.Size())

	configIdx := slices.IndexFunc(inj.Mounts, func(m nri.Mount) bool {
		return m.ContainerPath == filepath.Join(contRoot, util.Config)
	})
	require.NotEqual(t, -1, configIdx)
	assert.Less(t,
		mountDepth(inj.Mounts[configIdx].ContainerPath),
		mountDepth(inj.Mounts[idx].ContainerPath))
}

// A partition directory reused by a later incarnation must not hand the new
// container the previous one's PID list.
func TestPartitionMounts_StalePidListIsCleared(t *testing.T) {
	hostRoot := t.TempDir()
	contRoot := t.TempDir()
	manager := &VGPUManager{hostManagerPath: hostRoot, contManagerPath: contRoot}

	inj, err := manager.GetNRIPartitionInjection("claim-stale", "pod", "ns", "pod-uid", "ctr")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(inj.ConfigDir, registry.PidsConfig), []byte("4242\n"), 0o644))

	inj, err = manager.GetNRIPartitionInjection("claim-stale", "pod", "ns", "pod-uid", "ctr")
	require.NoError(t, err)
	content, err := os.ReadFile(filepath.Join(inj.ConfigDir, registry.PidsConfig))
	require.NoError(t, err)
	assert.Empty(t, content)
}
