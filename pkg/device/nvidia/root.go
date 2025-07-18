/**
# Copyright 2023 NVIDIA CORPORATION
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package nvidia

import (
	"fmt"
	"os"
	"path/filepath"
)

type RootPath string

// GetDriverLibraryPath returns path to `libnvidia-ml.so.1` in the driver root.
// The folder for this file is also expected to be the location of other driver files.
func (r RootPath) GetDriverLibraryPath() (string, error) {
	librarySearchPaths := []string{
		"/usr/lib64",
		"/usr/lib/x86_64-linux-gnu",
		"/usr/lib/aarch64-linux-gnu",
		"/lib64",
		"/lib/x86_64-linux-gnu",
		"/lib/aarch64-linux-gnu",
	}

	libraryPath, err := r.findFile("libnvidia-ml.so.1", librarySearchPaths...)
	if err != nil {
		return "", err
	}

	return libraryPath, nil
}

// getNvidiaSMIPath returns path to the `nvidia-smi` executable in the driver root.
func (r RootPath) getNvidiaSMIPath() (string, error) {
	binarySearchPaths := []string{
		"/usr/bin",
		"/usr/sbin",
		"/bin",
		"/sbin",
	}

	binaryPath, err := r.findFile("nvidia-smi", binarySearchPaths...)
	if err != nil {
		return "", err
	}

	return binaryPath, nil
}

// isDevRoot checks whether the specified root is a dev root.
// A dev root is defined as a root containing a /dev folder.
func (r RootPath) isDevRoot() bool {
	stat, err := os.Stat(filepath.Join(string(r), "dev"))
	if err != nil {
		return false
	}
	return stat.IsDir()
}

// GetDevRoot returns the dev root associated with the root.
// If the root is not a dev root, this defaults to "/".
func (r RootPath) GetDevRoot() string {
	if r.isDevRoot() {
		return string(r)
	}
	return "/"
}

// findFile searches the root for a specified file.
// A number of folders can be specified to search in addition to the root itself.
// If the file represents a symlink, this is resolved and the final path is returned.
func (r RootPath) findFile(name string, searchIn ...string) (string, error) {

	for _, d := range append([]string{"/"}, searchIn...) {
		l := filepath.Join(string(r), d, name)
		candidate, err := resolveLink(l)
		if err != nil {
			continue
		}
		return candidate, nil
	}

	return "", fmt.Errorf("error locating %q", name)
}

// resolveLink finds the target of a symlink or the file itself in the
// case of a regular file.
// This is equivalent to running `readlink -f ${l}`.
func resolveLink(l string) (string, error) {
	resolved, err := filepath.EvalSymlinks(l)
	if err != nil {
		return "", fmt.Errorf("error resolving link '%v': %v", l, err)
	}
	return resolved, nil
}
