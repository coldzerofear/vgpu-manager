#!/usr/bin/env bash
#
# Verify the built libvgpu-control.so exports BOTH ABI versions of every
# ABI-conflict CUDA API family.
#
# Context: CUDA 13 redefines the unversioned C identifier `cuCtxCreate` to
# the `_v4` symbol, while CUDA 12 binaries still emit `_v2`. A single ELF
# can serve both if and only if every versioned symbol is exported with its
# own correct-ABI wrapper. Missing any one of them means LD_PRELOAD silently
# bypasses us for that caller.
#
# This script is intended to run at build time, after the .so is produced.
# Usage:  check_exported_symbols.sh <path/to/libvgpu-control.so>

set -o errexit
set -o nounset
set -o pipefail

SO_PATH="${1:-}"
if [[ -z "${SO_PATH}" ]]; then
  echo "usage: $0 <path/to/libvgpu-control.so>" >&2
  exit 2
fi
if [[ ! -f "${SO_PATH}" ]]; then
  echo "[FAIL] .so not found: ${SO_PATH}" >&2
  exit 1
fi
if ! command -v nm >/dev/null 2>&1; then
  echo "[SKIP] nm not in PATH - cannot validate exported symbols"
  exit 0
fi

# ABI-conflict families: "base old_abi_sym new_abi_sym".
# Must stay in sync with is_abi_conflict_base() in include/cuda-helper.h
# and ABI_CONFLICT_FAMILIES in check_cuda_hook_consistency.py.
FAMILIES=(
  "cuCtxCreate                   cuCtxCreate_v2                 cuCtxCreate_v4"
  "cuMemAdvise                   cuMemAdvise                    cuMemAdvise_v2"
  "cuMemPrefetchAsync            cuMemPrefetchAsync             cuMemPrefetchAsync_v2"
  "cuGraphGetEdges               cuGraphGetEdges                cuGraphGetEdges_v2"
  "cuGraphNodeGetDependencies    cuGraphNodeGetDependencies     cuGraphNodeGetDependencies_v2"
  "cuGraphNodeGetDependentNodes  cuGraphNodeGetDependentNodes   cuGraphNodeGetDependentNodes_v2"
  "cuGraphAddDependencies        cuGraphAddDependencies         cuGraphAddDependencies_v2"
  "cuGraphRemoveDependencies     cuGraphRemoveDependencies      cuGraphRemoveDependencies_v2"
  "cuGraphAddNode                cuGraphAddNode                 cuGraphAddNode_v2"
  "cuGetProcAddress              cuGetProcAddress               cuGetProcAddress_v2"
  # cuGetProcAddress above is listed for export-completeness (we must
  # export both ABI versions of this symbol) even though its substitution
  # logic is handled specially inside cuGetProcAddress() / _v2() in
  # cuda_hook.c rather than via is_abi_conflict_base().
)

# Snapshot exported defined text symbols.
EXPORTED=$(nm --defined-only --extern-only "${SO_PATH}" \
           | awk '$2 == "T" || $2 == "W" { print $3 }' | sort -u)

errors=0
missing_list=()
for line in "${FAMILIES[@]}"; do
  read -r base old_abi new_abi <<< "${line}"
  for sym in "${old_abi}" "${new_abi}"; do
    if ! grep -qxF "${sym}" <<< "${EXPORTED}"; then
      missing_list+=("${base}: ${sym}")
      errors=$((errors + 1))
    fi
  done
done

if (( errors > 0 )); then
  echo "[FAIL] ABI-conflict family symbol export check on ${SO_PATH}"
  echo "       missing ${errors} versioned symbol(s):"
  for m in "${missing_list[@]}"; do
    echo "         - ${m}"
  done
  echo "       every conflict family must export BOTH the old-ABI and the"
  echo "       new-ABI versioned name so the single ELF can serve CUDA 11/12"
  echo "       callers AND CUDA 13 callers simultaneously."
  exit 1
fi

echo "[PASS] ABI-conflict symbol export check: all ${#FAMILIES[@]} family "\
"pairs present in $(basename "${SO_PATH}")"
