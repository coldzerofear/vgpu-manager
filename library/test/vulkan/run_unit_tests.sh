#!/usr/bin/env bash
#
# Runs every Vulkan layer unit test in $TEST_DIR (defaults to the
# directory this script lives in after CMake stages it). No GPU /
# CUDA / NVML required.
#
# Usage:
#   run_unit_tests.sh [<test-build-dir>] [<path/to/libvgpu-control.so>]
#
# - <test-build-dir> defaults to the script's own dir (post-stage).
# - <libvgpu-control.so> defaults to ../../libvgpu-control.so (the
#   layer .so emitted alongside the test build dir). Used by
#   test_layer_init for the dlopen+negotiate test.
#
# Environment:
#   VGPU_TEST_TIMEOUT  seconds per test (default 30)

set -o errexit
set -o nounset
set -o pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_DIR="${1:-${SELF_DIR}}"
VGPU_SO="${2:-${TEST_DIR}/../../libvgpu-control.so}"

VGPU_TEST_TIMEOUT="${VGPU_TEST_TIMEOUT:-30}"

if [[ ! -d "${TEST_DIR}" ]]; then
  echo "error: test dir ${TEST_DIR} does not exist" >&2
  exit 2
fi
if [[ ! -f "${VGPU_SO}" ]]; then
  echo "error: libvgpu-control.so not found at ${VGPU_SO}" >&2
  exit 2
fi

# Convert to absolute path so test binaries (which may run from any cwd)
# can dlopen() it reliably.
VGPU_SO="$(cd "$(dirname "${VGPU_SO}")" && pwd)/$(basename "${VGPU_SO}")"
export VGPU_TEST_SO="${VGPU_SO}"

# List of tests to run. Order is intentional — cheap / mock-only first,
# dlopen-based last. Add new tests here.
TESTS=(
  test_dispatch
  test_queue_index
  test_physdev_index
  test_memprops_clamp
  test_alloc_budget
  test_alloc_import
  test_submit_throttle
  test_layer_init
)

passed=0
failed=0
failed_names=()

echo "[vgpu-vk-tests] running ${#TESTS[@]} unit test(s) from ${TEST_DIR}"
echo "[vgpu-vk-tests] using libvgpu-control.so: ${VGPU_TEST_SO}"
echo

for t in "${TESTS[@]}"; do
  bin="${TEST_DIR}/${t}"
  if [[ ! -x "${bin}" ]]; then
    echo "[SKIP] ${t} (binary not built)"
    continue
  fi
  echo "----- ${t} -----"
  if timeout "${VGPU_TEST_TIMEOUT}" "${bin}"; then
    passed=$((passed + 1))
    echo "[PASS] ${t}"
  else
    failed=$((failed + 1))
    failed_names+=("${t}")
    echo "[FAIL] ${t}"
  fi
  echo
done

echo "================================================================"
echo "[vgpu-vk-tests] ${passed} passed, ${failed} failed"
if [[ ${failed} -gt 0 ]]; then
  echo "[vgpu-vk-tests] failed: ${failed_names[*]}"
  exit 1
fi
exit 0
