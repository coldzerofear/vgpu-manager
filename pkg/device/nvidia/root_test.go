/*
Copyright The Kubernetes Authors

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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFindFile(t *testing.T) {
	// The production lists, not copies of them: a copy stops covering what
	// the driver root actually gets searched for as soon as one is extended.
	tests := map[string]struct {
		// name is the file findFile searches for.
		name string
		// searchIn are the folders searched in addition to the root.
		searchIn []string
		// dirs are created (relative to the test root) before searching.
		dirs []string
		// files are created (relative to the test root) before searching.
		files []string
		// expected is the path (relative to the test root) findFile should
		// return, or "" if an error is expected.
		expected string
	}{
		"library in first search path": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			files:    []string{"usr/lib64/libnvidia-ml.so.1"},
			expected: "usr/lib64/libnvidia-ml.so.1",
		},
		"directory in earlier search path does not shadow library": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			dirs:     []string{"usr/lib64/libnvidia-ml.so.1"},
			files:    []string{"usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1"},
			expected: "usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
		},
		"only directories found": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			dirs:     []string{"usr/lib64/libnvidia-ml.so.1"},
			expected: "",
		},
		"library not found": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			expected: "",
		},
		"nvidia-smi in binary search path": {
			name:     "nvidia-smi",
			searchIn: binarySearchPaths,
			files:    []string{"usr/bin/nvidia-smi"},
			expected: "usr/bin/nvidia-smi",
		},
		// An immutable distribution puts the driver under /usr/local: Talos
		// mounts its NVIDIA system extension there, so nothing is found in
		// the paths a package manager would have used.
		"library under /usr/local/lib": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			files:    []string{"usr/local/lib/libnvidia-ml.so.1"},
			expected: "usr/local/lib/libnvidia-ml.so.1",
		},
		"library under /usr/local/lib64": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			files:    []string{"usr/local/lib64/libnvidia-ml.so.1"},
			expected: "usr/local/lib64/libnvidia-ml.so.1",
		},
		"nvidia-smi under /usr/local/bin": {
			name:     "nvidia-smi",
			searchIn: binarySearchPaths,
			files:    []string{"usr/local/bin/nvidia-smi"},
			expected: "usr/local/bin/nvidia-smi",
		},
		"nvidia-smi under /opt/bin": {
			name:     "nvidia-smi",
			searchIn: binarySearchPaths,
			files:    []string{"opt/bin/nvidia-smi"},
			expected: "opt/bin/nvidia-smi",
		},
		// Order is what the lists declare: a driver root that has both keeps
		// being read from where a package manager put it.
		"package manager paths win over /usr/local": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			files: []string{
				"usr/local/lib/libnvidia-ml.so.1",
				"lib64/libnvidia-ml.so.1",
			},
			expected: "lib64/libnvidia-ml.so.1",
		},
		"a directory under /usr/local does not shadow the library": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			dirs:     []string{"usr/local/lib/libnvidia-ml.so.1"},
			files:    []string{"usr/local/lib64/libnvidia-ml.so.1"},
			expected: "usr/local/lib64/libnvidia-ml.so.1",
		},
		"directory in earlier search path does not shadow nvidia-smi": {
			name:     "nvidia-smi",
			searchIn: binarySearchPaths,
			dirs:     []string{"opt/bin/nvidia-smi"},
			files:    []string{"usr/bin/nvidia-smi"},
			expected: "usr/bin/nvidia-smi",
		},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			testRoot := t.TempDir()
			for _, d := range tc.dirs {
				require.NoError(t, os.MkdirAll(filepath.Join(testRoot, d), 0o755))
			}
			for _, f := range tc.files {
				path := filepath.Join(testRoot, f)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte{}, 0o644))
			}

			found, err := RootPath(testRoot).findFile(tc.name, tc.searchIn...)
			if tc.expected == "" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			// t.TempDir may itself contain symlinks (e.g. on macOS), so
			// compare against the resolved expected path.
			expected, err := filepath.EvalSymlinks(filepath.Join(testRoot, tc.expected))
			require.NoError(t, err)
			require.Equal(t, expected, found)
		})
	}
}

func TestFindFileFollowsSymlink(t *testing.T) {
	const name = "libnvidia-ml.so.1"

	t.Run("symlink to a regular file is resolved", func(t *testing.T) {
		testRoot := t.TempDir()
		target := filepath.Join(testRoot, "opt", name)
		require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
		require.NoError(t, os.WriteFile(target, []byte{}, 0o644))

		linkDir := filepath.Join(testRoot, "usr", "lib64")
		require.NoError(t, os.MkdirAll(linkDir, 0o755))
		require.NoError(t, os.Symlink(target, filepath.Join(linkDir, name)))

		found, err := RootPath(testRoot).findFile(name, "usr/lib64")
		require.NoError(t, err)
		want, err := filepath.EvalSymlinks(target)
		require.NoError(t, err)
		require.Equal(t, want, found)
	})

	t.Run("symlink to a directory is rejected", func(t *testing.T) {
		testRoot := t.TempDir()
		targetDir := filepath.Join(testRoot, "opt", name)
		require.NoError(t, os.MkdirAll(targetDir, 0o755))

		linkDir := filepath.Join(testRoot, "usr", "lib64")
		require.NoError(t, os.MkdirAll(linkDir, 0o755))
		require.NoError(t, os.Symlink(targetDir, filepath.Join(linkDir, name)))

		_, err := RootPath(testRoot).findFile(name, "usr/lib64")
		require.Error(t, err)
	})

	t.Run("dangling symlink is rejected", func(t *testing.T) {
		testRoot := t.TempDir()
		linkDir := filepath.Join(testRoot, "usr", "lib64")
		require.NoError(t, os.MkdirAll(linkDir, 0o755))
		require.NoError(t, os.Symlink(filepath.Join(testRoot, "missing.so"), filepath.Join(linkDir, name)))

		_, err := RootPath(testRoot).findFile(name, "usr/lib64")
		require.Error(t, err)
	})
}

func TestRootGetDevRoot(t *testing.T) {
	withDev := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(withDev, "dev"), 0o755))
	require.Equal(t, withDev, RootPath(withDev).GetDevRoot())

	require.Equal(t, "/", RootPath(t.TempDir()).GetDevRoot())

	devIsFile := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(devIsFile, "dev"), []byte{}, 0o644))
	require.Equal(t, "/", RootPath(devIsFile).GetDevRoot())
}

func TestRootGetDriverAndBinaryPaths(t *testing.T) {
	testRoot := t.TempDir()
	writeFile := func(rel string) string {
		p := filepath.Join(testRoot, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte{}, 0o644))
		want, err := filepath.EvalSymlinks(p)
		require.NoError(t, err)
		return want
	}
	wantNVML := writeFile("usr/lib64/libnvidia-ml.so.1")
	wantFM := writeFile("usr/lib64/libnvfm.so")
	wantSMI := writeFile("usr/bin/nvidia-smi")

	r := RootPath(testRoot)

	got, err := r.GetDriverLibraryPath()
	require.NoError(t, err)
	require.Equal(t, wantNVML, got)

	got, err = r.GetFMLibraryPath()
	require.NoError(t, err)
	require.Equal(t, wantFM, got)

	got, err = r.GetNvidiaSMIPath()
	require.NoError(t, err)
	require.Equal(t, wantSMI, got)

	_, err = RootPath(t.TempDir()).GetDriverLibraryPath()
	require.Error(t, err)
}

// A driver root laid out the way an immutable distribution mounts it: the
// whole driver lives under /usr/local, and the paths a package manager would
// have used do not exist at all.
func TestRootGetDriverAndBinaryPathsUsrLocal(t *testing.T) {
	testRoot := t.TempDir()
	writeFile := func(rel string) string {
		p := filepath.Join(testRoot, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte{}, 0o644))
		want, err := filepath.EvalSymlinks(p)
		require.NoError(t, err)
		return want
	}
	wantNVML := writeFile("usr/local/lib/libnvidia-ml.so.1")
	wantFM := writeFile("usr/local/lib/libnvfm.so")
	wantSMI := writeFile("usr/local/bin/nvidia-smi")

	r := RootPath(testRoot)

	got, err := r.GetDriverLibraryPath()
	require.NoError(t, err)
	require.Equal(t, wantNVML, got)

	got, err = r.GetFMLibraryPath()
	require.NoError(t, err)
	require.Equal(t, wantFM, got)

	got, err = r.GetNvidiaSMIPath()
	require.NoError(t, err)
	require.Equal(t, wantSMI, got)
}

// The driver root may reach its files through a symlinked directory (a system
// extension mounted elsewhere and linked into place); the search resolves it.
func TestRootGetDriverLibraryPathThroughLinkedDir(t *testing.T) {
	testRoot := t.TempDir()
	real := filepath.Join(testRoot, "extension", "lib")
	require.NoError(t, os.MkdirAll(real, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(real, "libnvidia-ml.so.1"), []byte{}, 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(testRoot, "usr", "local"), 0o755))
	require.NoError(t, os.Symlink(real, filepath.Join(testRoot, "usr", "local", "lib")))

	got, err := RootPath(testRoot).GetDriverLibraryPath()
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(filepath.Join(real, "libnvidia-ml.so.1"))
	require.NoError(t, err)
	require.Equal(t, want, got)
}
