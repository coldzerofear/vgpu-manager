#!/usr/bin/env bash
#
# Run every compiled test under LD_PRELOAD of libvgpu-control.so, then run
# any available Python framework smoke test.
#
# Requirements:
#   - NVIDIA GPU + driver + libcuda.so.1 on LD_LIBRARY_PATH
#   - test binaries already built (e.g. via `make build-tests` at repo root)
#
# Usage:
#   run_all_tests.sh <test-build-dir> <path/to/libvgpu-control.so>
#
# Environment:
#   TEST_DEVICE_ID    default 0
#   SKIP_PYTHON=1     skip the four framework smoke tests (default: auto)
#   TEST_TIMEOUT      seconds per test (default 120)

set -o errexit
set -o nounset
set -o pipefail

TEST_DIR="${1:-}"
VGPU_SO="${2:-}"

if [[ -z "${TEST_DIR}" || -z "${VGPU_SO}" ]]; then
  echo "usage: $0 <test-build-dir> <path/to/libvgpu-control.so>" >&2
  exit 2
fi

if [[ ! -d "${TEST_DIR}" ]]; then
  echo "[FAIL] test build dir not found: ${TEST_DIR}" >&2
  exit 1
fi
if [[ ! -f "${VGPU_SO}" ]]; then
  echo "[FAIL] vgpu-control shared object not found: ${VGPU_SO}" >&2
  exit 1
fi

TEST_TIMEOUT="${TEST_TIMEOUT:-120}"
export TEST_DEVICE_ID="${TEST_DEVICE_ID:-0}"

declare -i TOTAL=0 PASSED=0 FAILED=0 SKIPPED=0
FAILED_NAMES=()

run_one() {
  local name="$1"; shift
  TOTAL=$((TOTAL + 1))
  printf "=== %-36s " "${name}"
  local log
  log=$(mktemp)
  if timeout "${TEST_TIMEOUT}" "$@" >"${log}" 2>&1; then
    echo "PASS"
    PASSED=$((PASSED + 1))
  else
    local rc=$?
    echo "FAIL (rc=${rc})"
    FAILED=$((FAILED + 1))
    FAILED_NAMES+=("${name}")
    echo "------- output -------"
    tail -30 "${log}" | sed 's/^/  /'
    echo "----------------------"
  fi
  rm -f "${log}"
}

# -------- C / CUDA tests --------
cd "${TEST_DIR}"
shopt -s nullglob
for exe in test_*; do
  if [[ -x "${exe}" && ! -d "${exe}" ]]; then
    run_one "${exe}" env LD_PRELOAD="${VGPU_SO}" "./${exe}"
  fi
done

# -------- Python framework tests (skipped if not importable) --------
PY_DIR="$(cd "$(dirname "$0")" && pwd)/python"
if [[ "${SKIP_PYTHON:-0}" == "0" && -d "${PY_DIR}" ]]; then
  for py in "${PY_DIR}"/limit_*.py; do
    name="python/$(basename "${py}")"
    # Probe: if the framework isn't installed, skip.
    case "$(basename "${py}")" in
      limit_pytorch.py)      mod=torch ;;
      limit_tensorflow.py)   mod=tensorflow ;;
      limit_tensorflow2.py)  mod=tensorflow ;;
      limit_mxnet.py)        mod=mxnet ;;
      *)                     mod= ;;
    esac
    if [[ -n "${mod}" ]] && ! python3 -c "import ${mod}" >/dev/null 2>&1; then
      printf "=== %-36s SKIP (module '%s' not installed)\n" "${name}" "${mod}"
      SKIPPED=$((SKIPPED + 1))
      continue
    fi
    run_one "${name}" env LD_PRELOAD="${VGPU_SO}" python3 "${py}" \
                              --device "${TEST_DEVICE_ID}"
  done
fi

echo
echo "============================================================"
echo "Summary: total=${TOTAL} pass=${PASSED} fail=${FAILED} skip=${SKIPPED}"
if (( FAILED > 0 )); then
  echo "Failed tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
exit 0
