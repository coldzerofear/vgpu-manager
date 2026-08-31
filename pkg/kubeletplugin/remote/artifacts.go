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
	// the local path mounts libvgpu-control.so.
	ContainerDir string
	// LibDir is the container path to expose via LD_LIBRARY_PATH (flat
	// layout: libcuda.so.1 / libnvidia-ml.so.1 directly in ContainerDir).
	LibDir string
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
	return &artifactSelection{
		Name:         bestName,
		HostDir:      filepath.Join(hostArtifactsDir, bestName),
		ContainerDir: containerDir,
		LibDir:       containerDir,
	}, nil
}

// listArtifactVersions enumerates the materialized client artifact versions
// under dir (subdirectories whose names parse as versions; the control
// library files sharing the driver dir are skipped). Used by the inject
// metrics; a read failure yields an empty list.
func listArtifactVersions(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var versions []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := semver.NewVersion(e.Name()); err != nil {
			continue
		}
		versions = append(versions, e.Name())
	}
	return versions
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
