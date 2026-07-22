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

# Get the UUID of the specified GPU
if command -v nvidia-smi &>/dev/null; then
  echo "Detecting GPU ${DEVICE_INDEX} UUID..."
  GPU_UUID=$(nvidia-smi --query-gpu=uuid --format=csv,noheader | sed -n "$((DEVICE_INDEX + 1))p")
  if [[ -z "${GPU_UUID}" ]]; then
    echo "[WARN] Could not get UUID for GPU ${DEVICE_INDEX}, trying indexed format..."
    # Fallback: NVIDIA_VISIBLE_DEVICES also accepts index numbers
    GPU_UUID="${DEVICE_INDEX}"
  fi
  echo "Using GPU UUID: ${GPU_UUID}"
  export NVIDIA_VISIBLE_DEVICES="${GPU_UUID}"
else
  echo "[WARN] nvidia-smi not found, using device index directly"
  export NVIDIA_VISIBLE_DEVICES="${DEVICE_INDEX}"
fi

export CUDA_CORE_LIMIT=30
export CUDA_MEM_LIMIT=2048m
# Turn on the virtual-memory ledger. The limit check only consults it when this
# is set, and the graph-capture accounting in test_alloc_async_accounting lives
# entirely in that ledger -- with it off those cases have nothing to assert.
export VMEMORY_NODE_ENABLED=1

# Everything the tests need is configured above, so a test that reports SKIP
# here did not find what it was promised: treat that as a failure rather than
# letting a green summary hide assertions that never ran.
export VGPU_TEST_STRICT=1

rm -f /etc/vgpu-manager/config/vgpu.config

# Run the tests with the environment variable set
echo "Running tests with NVIDIA_VISIBLE_DEVICES=${NVIDIA_VISIBLE_DEVICES}"
echo "============================================================"

cd "$(dirname "$0")"
chmod +x ./run_all_tests.sh
exec ./run_all_tests.sh "${TEST_DIR}" "${VGPU_SO}"
