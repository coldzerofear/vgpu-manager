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

# Snapshot exported defined text symbols (functions only — used by the
# ABI-conflict family pair check below, which is meaningless for data).
EXPORTED=$(nm --defined-only --extern-only "${SO_PATH}" \
           | awk '$2 == "T" || $2 == "W" { print $3 }' | sort -u)

# Snapshot ALL defined external symbols (functions + data). Used by the
# leak / bulk-pattern checks: internal data tables such as cuda_hooks_entry
# (nm type D), cuda_hook_nums (R), nvml_library_entry (D) leak via .dynsym
# just like internal helper functions do, and they collide / are dlsym-
# observable the same way. Filtering to T/W would miss the entire data
# class — that is exactly the regression that prompted adding this set.
EXPORTED_ALL=$(nm --defined-only --extern-only "${SO_PATH}" \
               | awk '{ print $3 }' | sort -u)

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

# ---------------------------------------------------------------------------
# Negative assertions: internal helpers MUST NOT leak into .dynsym.
#
# The version script at deploy/libvgpu-control.exports.ld confines exports
# to cu* / nvml* / dlsym / vkNegotiateLoaderLayerInterfaceVersion. If a
# future change drops or weakens that script, internal helpers (~80 in
# the pre-script baseline) silently re-appear in .dynsym, where they are
# eligible for NVIDIA-ICD / loader-side global-symbol resolution to
# collide with — exactly the class of risk that drove HAMi-core PR #182's
# libvgpu_vk.so split. We catch that regression here.
#
# We do not enumerate every internal symbol — that list would drift. Two
# checks are sufficient:
#   1. A representative deny-list of internal helpers we know existed
#      before the version script was applied. Any one of these
#      reappearing means the script broke.
#   2. A bulk count: anything outside the cu* / _cu* / nvml* / dlsym /
#      vkNegotiateLoaderLayerInterfaceVersion patterns is suspicious;
#      report and fail.
# ---------------------------------------------------------------------------

FORBIDDEN_HELPERS=(
  formatUuid
  accumulate_used_memory
  cleanup_vmem_nodes
  device_util_read_lock
  device_util_write_lock
  device_util_unlock
  device_vmem_read_lock
  device_vmem_write_lock
  device_vmem_unlock
  init_devices_mapping
  init_g_vgpu_config_by_env
  init_real_dlsym
  load_necessary_data
  load_controller_configuration
  load_cuda_libraries
  prepare_memory_allocation
  vgpu_check_alloc_budget
  vgpu_rate_limit_by_host_index
  vgpu_ensure_sm_watcher_started
  register_to_remote_with_data
  reset_cuda_index_mapping
  metrics_record_oom
  get_compatibility_mode
  get_used_gpu_memory_by_device
  get_used_gpu_virt_memory
  lookup_cuda_hook_ptr
  note_unhooked_symbol
  malloc_gpu_virt_memory
  free_gpu_virt_memory
  print_global_vgpu_config
  # Internal data tables shared between cuda_hook.c / nvml_hook.c / loader.c
  # via extern declarations. They must be linker-global at static-link time
  # but MUST NOT leak into .dynsym — the broad "cu*"/"nvml*" globs in the
  # version script previously caught them by accident (they start with
  # cu/nvml but use snake_case rather than CUDA's PascalCase). The script
  # now uses cu[A-Z]*/nvml[A-Z]* to discriminate; these entries are the
  # explicit regression guards that fail loudly if anyone loosens the glob.
  cuda_hooks_entry
  cuda_library_entry
  cuda_hook_nums
  nvml_hooks_entry
  nvml_library_entry
  nvml_hook_nums
)

leaked_list=()
for sym in "${FORBIDDEN_HELPERS[@]}"; do
  if grep -qxF "${sym}" <<< "${EXPORTED_ALL}"; then
    leaked_list+=("${sym}")
  fi
done

if (( ${#leaked_list[@]} > 0 )); then
  echo "[FAIL] internal-symbol leak check on $(basename "${SO_PATH}")"
  echo "       ${#leaked_list[@]} internal helper(s) appear in .dynsym:"
  for s in "${leaked_list[@]}"; do
    echo "         - ${s}"
  done
  echo "       these names should be hidden by the version script at"
  echo "       deploy/libvgpu-control.exports.ld. Verify the script is"
  echo "       still wired into the link command (target_link_options)"
  echo "       and that no new top-level helper was introduced without"
  echo "       being matched by 'local: *;'."
  exit 1
fi

# Bulk pattern: anything outside the allowed export families is also
# a regression. The regex mirrors the version script's globs — only
# cu[A-Z]* / cudbg* (CUDA debugger API, lowercase dbg) / nvml[A-Z]* /
# _cu[A-Z]* (defensive — _cu trampolines should never reach .dynsym
# but if they did, they are at least CUDA-related) / dlsym /
# vkNegotiateLoaderLayerInterfaceVersion are allowed. Runs against
# EXPORTED_ALL so leaked data symbols (cuda_hooks_entry & friends)
# are caught too.
unexpected=$(comm -23 \
  <(printf '%s\n' "${EXPORTED_ALL}" | sort -u) \
  <(printf '%s\n' "${EXPORTED_ALL}" \
      | grep -E '^(cu[A-Z]|cudbg|_cu[A-Z]|nvml[A-Z])' \
      | sort -u))
unexpected=$(printf '%s\n' "${unexpected}" \
              | grep -vxE 'dlsym|vkNegotiateLoaderLayerInterfaceVersion' \
              | grep -v '^$' || true)

if [[ -n "${unexpected}" ]]; then
  echo "[FAIL] $(basename "${SO_PATH}") exports symbol(s) outside the"
  echo "       documented ABI surface (cu[A-Z]* / cudbg* / _cu[A-Z]* /"
  echo "       nvml[A-Z]* / dlsym / vkNegotiateLoaderLayerInterfaceVersion):"
  while IFS= read -r s; do echo "         - ${s}"; done <<< "${unexpected}"
  echo "       extend deploy/libvgpu-control.exports.ld global: list"
  echo "       AND this script's allow-list if intentional."
  exit 1
fi

echo "[PASS] internal-symbol leak check: ${#FORBIDDEN_HELPERS[@]} known internal" \
     "helpers/tables absent from .dynsym; export surface confined to" \
     "cu[A-Z]* / cudbg* / nvml[A-Z]* / dlsym / vkNegotiateLoaderLayerInterfaceVersion."
