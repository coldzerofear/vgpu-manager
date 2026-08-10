#!/usr/bin/env bash
# Copyright 2024-2026 coldzerofear
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
set -o errexit
set -o nounset
set -o pipefail

CUDA_LIBRARY=$1
ML_LIBRARY=$2

echo "find new library"

while read item; do
  grep -q ${item} include/cuda-helper.h || echo "$item,"
done < <(nm -D ${CUDA_LIBRARY} | grep " T " | awk '{print "CUDA_ENTRY_ENUM("$3")"}')

echo ""

while read item; do
  grep -q ${item} include/nvml-helper.h || echo "$item,"
done < <(nm -D ${ML_LIBRARY} | grep " T " | awk '{print "NVML_ENTRY_ENUM("$3")"}')
