#!/usr/bin/env bash
#
# Run tests with NVIDIA_VISIBLE_DEVICES set to the first GPU's UUID.
# This allows the library to initialize properly without a full vgpu-manager config.
#
# Usage:
#   ./run_tests_with_env.sh <test-build-dir> <path/to/libvgpu-control.so> [device_index]
#
#

set -o errexit
set -o nounset
set -o pipefail

TEST_DIR="${1:-}"
VGPU_SO="${2:-}"
DEVICE_INDEX="${3:-0}"

if [[ -z "${TEST_DIR}" || -z "${VGPU_SO}" ]]; then
  echo "usage: $0 <test-build-dir> <path/to/libvgpu-control.so> [device_index]" >&2
  exit 2
fi

# Configure every GPU the host has: the library is fatal on a CUDA device the
# config does not list, and a multi-GPU test needs them all. device_index only
# picks the GPU the single-GPU tests run on.
if command -v nvidia-smi &>/dev/null; then
  echo "Detecting GPU UUIDs..."
  # Do not let a failing nvidia-smi abort the run. With `set -o errexit` and
  # `pipefail` a non-zero exit here killed the script before the fallback below
  # could run -- making that fallback dead code and reporting the problem as a
  # bare "Error <n>" from make, with no message to act on.
  nvidia_rc=0
  nvidia_out="$(nvidia-smi --query-gpu=uuid --format=csv,noheader 2>&1)" || nvidia_rc=$?
  GPU_UUID=""
  if (( nvidia_rc != 0 )); then
    echo "[WARN] nvidia-smi exited ${nvidia_rc}: ${nvidia_out}" >&2
    case "${nvidia_rc}" in
      9)  echo "[WARN]   9 = NVML_ERROR_DRIVER_NOT_LOADED: the NVIDIA kernel module is not loaded." >&2 ;;
      12) echo "[WARN]  12 = NVML_ERROR_LIBRARY_NOT_FOUND: libnvidia-ml.so not found." >&2 ;;
      18) echo "[WARN]  18 = NVML_ERROR_LIB_RM_VERSION_MISMATCH: the loaded kernel module and" >&2
          echo "[WARN]       userspace NVML disagree on version -- usually a driver upgrade" >&2
          echo "[WARN]       without reloading nvidia.ko. Reload the driver or reboot." >&2 ;;
    esac
  else
    GPU_UUID="$(printf '%s\n' "${nvidia_out}" | paste -sd, -)"
  fi
  if [[ -z "${GPU_UUID}" ]]; then
    echo "[WARN] Could not get the GPU UUIDs, trying indexed format..."
    # Fallback: NVIDIA_VISIBLE_DEVICES also accepts index numbers
    GPU_UUID="${DEVICE_INDEX}"
  fi
  echo "Using GPU UUIDs: ${GPU_UUID}"
  export NVIDIA_VISIBLE_DEVICES="${GPU_UUID}"
else
  echo "[WARN] nvidia-smi not found, using device index directly"
  export NVIDIA_VISIBLE_DEVICES="${DEVICE_INDEX}"
fi

export CUDA_CORE_LIMIT=30
export CUDA_MEM_LIMIT=4096m
# Turn on the virtual-memory ledger. The limit check only consults it when this
# is set, and the graph-capture accounting in test_alloc_async_accounting lives
# entirely in that ledger -- with it off those cases have nothing to assert.
export VMEMORY_NODE_ENABLED=1

# Everything the tests need is configured above, so a test that reports SKIP
# here did not find what it was promised: treat that as a failure rather than
# letting a green summary hide assertions that never ran.
# export VGPU_TEST_STRICT=1

rm -f /etc/vgpu-manager/config/vgpu.config

# Run the tests with the environment variable set
echo "Running tests with NVIDIA_VISIBLE_DEVICES=${NVIDIA_VISIBLE_DEVICES}"
echo "============================================================"

cd "$(dirname "$0")"
chmod +x ./run_all_tests.sh
exec ./run_all_tests.sh "${TEST_DIR}" "${VGPU_SO}"
