#!/usr/bin/env bash
#
# The driver-API GPU tests against a library built with -DVGPU_SANITIZE=ON, with the
# sanitizer runtime preloaded first. Only the library is instrumented, the tests stay
# plain binaries.
#
# Not the whole suite: a sanitized library does not export dlsym (see CMakeLists.txt),
# so the runtime (.cu) and python tests would reach the driver past it and prove
# nothing. What runs here calls the driver API directly.
#
#   protect_shadow_gap=0  the driver maps memory into ASan's shadow gap
#   detect_leaks=0        the driver's own allocations at exit are not ours
#
# Usage: run_gpu_sanitize.sh <test-build-dir> <path/to/sanitized/libvgpu-control.so>

set -o nounset
set -o pipefail

TEST_DIR="${1:-}"
VGPU_SO="${2:-}"
if [[ -z "${TEST_DIR}" || -z "${VGPU_SO}" ]]; then
  echo "usage: $0 <test-build-dir> <path/to/sanitized/libvgpu-control.so>" >&2
  exit 2
fi

ASAN_RT="$("${CC:-gcc}" -print-file-name=libasan.so)"
if ! GPUS="$(nvidia-smi --query-gpu=uuid --format=csv,noheader)" || [[ -z "${GPUS}" ]]; then
  echo "[FAIL] no GPU found: ${GPUS}" >&2
  exit 1
fi
GPUS="$(printf '%s\n' "${GPUS}" | paste -sd, -)"

export NVIDIA_VISIBLE_DEVICES="${GPUS}"
export CUDA_MEM_LIMIT=4096m CUDA_CORE_LIMIT=30 VMEMORY_NODE_ENABLED=1
export ASAN_OPTIONS=protect_shadow_gap=0:detect_leaks=0:halt_on_error=1
export UBSAN_OPTIONS=print_stacktrace=1

fail=0
for t in test_graph_mem_admission test_graph_mem_admission_ptsz \
         test_alloc_async_accounting test_alloc_async_accounting_ptsz \
         test_vmm_accounting; do
  printf '=== %-36s ' "${t}"
  log="$(mktemp)"
  # Overrides the environment above if left behind by an earlier run.
  rm -f /etc/vgpu-manager/config/vgpu.config
  rc=0
  LD_PRELOAD="${ASAN_RT}:${VGPU_SO}" timeout 300 "${TEST_DIR}/${t}" >"${log}" 2>&1 || rc=$?
  if (( rc != 0 )) || grep -qE 'ERROR: AddressSanitizer|runtime error' "${log}"; then
    echo "FAIL (rc=${rc})"
    tail -40 "${log}" | sed 's/^/  /'
    fail=1
  else
    echo PASS
  fi
  rm -f "${log}"
done
exit "${fail}"
