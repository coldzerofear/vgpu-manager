/**
# Copyright (c) NVIDIA CORPORATION.  All rights reserved.
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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTryResolveLibrary(t *testing.T) {
	const libraryName = "libnvidia-ml.so.1"

	testCases := []struct {
		description string
		// dirs and files are created relative to the test root before resolving.
		dirs  []string
		files []string
		// expected is the resolved path relative to the test root, or "" if
		// the input library name is expected to be returned as is.
		expected string
	}{
		{
			description: "library in first search path",
			files:       []string{"usr/lib64/" + libraryName},
			expected:    "usr/lib64/" + libraryName,
		},
		{
			description: "directory in earlier search path does not shadow library",
			dirs:        []string{"usr/lib64/" + libraryName},
			files:       []string{"usr/lib/x86_64-linux-gnu/" + libraryName},
			expected:    "usr/lib/x86_64-linux-gnu/" + libraryName,
		},
		{
			description: "only directories found",
			dirs:        []string{"usr/lib64/" + libraryName},
			expected:    "",
		},
		{
			description: "library not found",
			expected:    "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			testRoot := t.TempDir()
			for _, d := range tc.dirs {
				require.NoError(t, os.MkdirAll(filepath.Join(testRoot, d), 0o755))
			}
			for _, f := range tc.files {
				path := filepath.Join(testRoot, f)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte{}, 0o644))
			}

			resolved, _ := RootPath(testRoot).GetDriverLibraryPath()
			if tc.expected == "" {
				require.Equal(t, tc.expected, resolved)
				return
			}

			// t.TempDir may itself contain symlinks, so compare against the
			// resolved expected path.
			expected, err := filepath.EvalSymlinks(filepath.Join(testRoot, tc.expected))
			require.NoError(t, err)
			require.Equal(t, expected, resolved)
		})
	}
}
