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

# Two different things get called "skipped", and only one of them is a problem:
#
#   SKIPPED  a test ran and reported that its own preconditions were not met
#            (rc=77). Those preconditions -- LD_PRELOAD, the vmem ledger -- are
#            things this harness is supposed to provide, so a skip here means
#            the setup is wrong and the assertions silently did not run.
#   ABSENT   an optional dependency is not installed (no torch, no tensorflow).
#            Nothing is misconfigured; there is simply nothing to run against.
#
# Keeping them apart is what lets VGPU_TEST_STRICT fail on the first without
# also failing every machine that happens not to have a framework installed.
declare -i TOTAL=0 PASSED=0 FAILED=0 SKIPPED=0 ABSENT=0
FAILED_NAMES=()
SKIPPED_NAMES=()
ABSENT_NAMES=()

# Exit status a test uses to say "I did not actually run" (the autotools
# convention). A test whose preconditions were not met must NOT report success:
# a green run would then be indistinguishable from one where the assertions
# never executed, which is how a real regression slips through unnoticed.
readonly RC_SKIP=77

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
    if (( rc == RC_SKIP )); then
      echo "SKIP"
      SKIPPED=$((SKIPPED + 1))
      SKIPPED_NAMES+=("${name}")
      echo "------- output -------"
      tail -30 "${log}" | sed 's/^/  /'
      echo "----------------------"
      rm -f "${log}"
      return 0
    fi
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
      printf "=== %-36s ABSENT (module '%s' not installed)\n" "${name}" "${mod}"
      ABSENT=$((ABSENT + 1))
      ABSENT_NAMES+=("${name} (no ${mod})")
      continue
    fi
    run_one "${name}" env LD_PRELOAD="${VGPU_SO}" python3 "${py}" \
                              --device "${TEST_DEVICE_ID}"
  done
fi

echo
echo "============================================================"
echo "Summary: total=${TOTAL} pass=${PASSED} fail=${FAILED} skip=${SKIPPED} absent=${ABSENT}"
if (( ABSENT > 0 )); then
  echo "Not runnable here (optional dependency missing):"
  for n in "${ABSENT_NAMES[@]}"; do echo "  - ${n}"; done
fi
if (( FAILED > 0 )); then
  echo "Failed tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
if (( SKIPPED > 0 )); then
  echo "Skipped tests (preconditions not met -- assertions did NOT run):"
  for n in "${SKIPPED_NAMES[@]}"; do echo "  - ${n}"; done
  # These preconditions are ours to provide, so a skip means the harness did
  # not deliver what the test was promised and a green result would overstate
  # what was verified. run_tests_with_env.sh sets everything up and turns this
  # on. Absent optional dependencies are deliberately NOT counted here: nothing
  # is misconfigured when a machine simply has no torch installed.
  if [[ "${VGPU_TEST_STRICT:-0}" != "0" ]]; then
    echo "[FAIL] VGPU_TEST_STRICT is set: these tests were expected to run."
    exit 1
  fi
fi
exit 0
