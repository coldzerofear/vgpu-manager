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

package remote

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
)

// artifactSelection is the outcome of picking a client artifact version for a
// claim: which host directory to bind-mount and which container path to put
// on the dynamic linker search path.
type artifactSelection struct {
	// Version directory name as found on disk (e.g. "12.9" or "12.9.1").
	Name string
	// HostDir is <hostArtifactsDir>/<Name>, the CDI mount source.
	HostDir string
	// ContainerDir is the fixed in-container mount target: the selected
	// version is always presented at <manager-root>/driver, mirroring where
	// the local path mounts libvgpu-control.so. The shims sit flat in it
	// (libcuda.so.1 / libnvidia-ml.so.1) and are made loadable through the
	// generated ld.so.preload file (see ensureLdPreloadFile).
	ContainerDir string
	// NvidiaSMIHost is the host path of the nvidia-smi binary shipped next
	// to the shims in newer artifact images, or "" when this artifact
	// version does not carry one.
	NvidiaSMIHost string
}

// selectArtifact picks the highest artifact version that is <= serverCeiling
// (design §4.3: client must not be newer than the server). Directory entries
// that do not parse as versions are ignored (so the control library files
// living in the same driver dir are harmless). A miss returns an error the
// kubelet treats as retryable — on a fresh node the artifacts may still be
// materializing (design §4.4).
//
// artifactsDir is the directory as visible to this process (for listing);
// hostArtifactsDir is the same directory as visible to the runtime (for the
// CDI mount source).
func selectArtifact(artifactsDir, hostArtifactsDir string, serverCeiling *semver.Version) (*artifactSelection, error) {
	entries, err := os.ReadDir(artifactsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read client artifacts dir %q: %w", artifactsDir, err)
	}

	var bestName string
	var bestVer *semver.Version
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v, err := semver.NewVersion(e.Name())
		if err != nil {
			continue
		}
		if v.Compare(serverCeiling) > 0 {
			continue
		}
		if bestVer == nil || v.Compare(bestVer) > 0 {
			bestVer = v
			bestName = e.Name()
		}
	}
	if bestVer == nil {
		return nil, fmt.Errorf("no client artifact version <= server CUDA %s found under %q (present: %s)",
			serverCeiling, artifactsDir, dirNames(entries))
	}

	containerDir := filepath.Join(util.ManagerRootPath, util.Driver)
	selection := &artifactSelection{
		Name:         bestName,
		HostDir:      filepath.Join(hostArtifactsDir, bestName),
		ContainerDir: containerDir,
	}
	// Newer artifact images ship nvidia-smi next to the shims. Optional:
	// older artifacts simply do not have it and nothing extra is mounted.
	if st, err := os.Stat(filepath.Join(artifactsDir, bestName, "nvidia-smi")); err == nil && st.Mode().IsRegular() {
		selection.NvidiaSMIHost = filepath.Join(hostArtifactsDir, bestName, "nvidia-smi")
	}
	return selection, nil
}

func dirNames(entries []os.DirEntry) string {
	names := ""
	for _, e := range entries {
		if names != "" {
			names += ","
		}
		names += e.Name()
	}
	if names == "" {
		return "<empty>"
	}
	return names
}

// The driver shims a client artifact ships.
const (
	shimLibCuda   = "libcuda.so.1"
	shimLibNvml   = "libnvidia-ml.so.1"
	shimLibCudart = "libcudart.so.13"
)

var optionalShimLibrary = map[string]bool{
	shimLibCuda:   true,
	shimLibNvml:   true,
	shimLibCudart: false,
}

// ensureLdPreloadFile writes <artifactsDir>/<ver>/RemoteLdPreload listing the
// artifact's shims by their in-container paths, one per line, and returns the
// host path of that file for the CDI mount. Idempotent; the content is
// refreshed (write + rename, so concurrent readers never see a torn file)
// when the shim set changes on an artifact update.
func ensureLdPreloadFile(artifactsDir string, sel *artifactSelection) (string, error) {
	var lines []string
	for lib, require := range optionalShimLibrary {
		if _, err := os.Stat(filepath.Join(artifactsDir, sel.Name, lib)); err == nil {
			lines = append(lines, filepath.Join(sel.ContainerDir, lib))
		} else if require {
			// Without the Client shim the artifact is unusable; fail the
			// prepare (retryable — the artifact may still be materializing).
			return "", fmt.Errorf("client artifact %s has no %s: %w", sel.Name, lib, err)
		}
	}
	content := strings.Join(lines, "\n") + "\n"

	path := filepath.Join(artifactsDir, sel.Name, RemoteLdPreload)
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return filepath.Join(sel.HostDir, RemoteLdPreload), nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("rename %s: %w", tmp, err)
	}
	return filepath.Join(sel.HostDir, RemoteLdPreload), nil
}
