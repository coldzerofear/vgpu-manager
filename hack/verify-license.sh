#!/usr/bin/env bash
# Copyright 2024-2026 coldzerofear
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Verify every tracked Go source carries a license header.
#
# A file passes with either our Apache header (hack/boilerplate.txt) or an
# upstream copyright line, because parts of this tree are derived from NVIDIA
# and Kubernetes code and keep the header they arrived with. Generated
# bindings are skipped: whatever regenerates them decides their first line.

set -o errexit
set -o nounset
set -o pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

missing=()
while IFS= read -r file; do
  case "${file}" in
    *.pb.go | *_pb2.go | *.gen.go) continue ;;
  esac
  if head -8 "${file}" | grep -qE 'Licensed under the Apache License|Copyright \(c\) [0-9]|SPDX-License-Identifier'; then
    continue
  fi
  missing+=("${file}")
done < <(git ls-files '*.go')

if [[ ${#missing[@]} -gt 0 ]]; then
  echo "The following files have no license header:" >&2
  printf '  %s\n' "${missing[@]}" >&2
  echo >&2
  echo "Prepend hack/boilerplate.txt (as a // comment block) to each." >&2
  exit 1
fi

echo "[PASS] license headers: $(git ls-files '*.go' | wc -l) Go files checked"
