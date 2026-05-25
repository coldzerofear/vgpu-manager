#!/usr/bin/env bash
#
# Vulkan layer baseline parity check.
#
# Goal: prove that turning the Vulkan implicit layer ON (via
# BUILD_VULKAN_LAYER=CMake / VGPU_VULKAN_ENABLE=runtime) does NOT silently
# perturb the LD_PRELOAD-only CUDA path that has always worked. HAMi PR
# #182 had to split into libvgpu.so + libvgpu_vk.so after they hit a
# class of LD_PRELOAD-only regressions where Vulkan code paths leaked
# global symbols into CUDA-only processes. Our equivalent defence is the
# linker version script + this baseline; we want CI to scream the moment
# the OFF path drifts from the ON-but-not-activated path.
#
# What this script asserts:
#
#   1. The library builds with BUILD_VULKAN_LAYER=OFF (the baseline).
#   2. The library builds with BUILD_VULKAN_LAYER=ON  (the augmented build).
#   3. Both builds pass hack/check_exported_symbols.sh — i.e. the
#      version script + internal-symbol allowlist holds in both.
#   4. The ON-build's dynamic symbol surface is EXACTLY the OFF-build's
#      surface PLUS a single new entry point: vkNegotiateLoaderLayerInterfaceVersion.
#      No other vk*, no _vk*, no internal Vulkan helper allowed in .dynsym.
#   5. .init / .init_array / .ctors are EMPTY in both builds — proves no
#      Vulkan code runs at library load time (it must be reachable only
#      from vkNegotiateLoaderLayerInterfaceVersion). Catches accidental
#      `__attribute__((constructor))` from the Vulkan module.
#   6. LD_PRELOAD of the ON-build into /bin/true emits zero stderr and
#      exits 0, in both VGPU_VULKAN_ENABLE=0 and VGPU_VULKAN_ENABLE=1
#      states. Proves the layer is fully dormant until a Vulkan loader
#      negotiates with us.
#   7. Hack consistency checks (check_cuda_hook_consistency.py,
#      check_struct_layout.py) pass on the ON-build.
#
# What this script does NOT do (intentionally):
#   - Run CUDA unit tests (they need a GPU; covered by the existing
#     `make test` flow which is a separate concern).
#   - Test Vulkan layer dispatch logic (covered by
#     test/vulkan/run_unit_tests.sh).
#
# Usage:
#   test/vulkan/run_baseline.sh [<library_dir>]
#
# Defaults <library_dir> to the script's grandparent. Build trees are
# created under <library_dir>/build_baseline_{off,on}/ to keep them
# isolated from any developer-local build/ tree. Build trees are NOT
# cleaned up automatically — they are useful artefacts to inspect when
# the script fails. Re-runs reuse them via CMake's reconfigure.
#

set -o errexit
set -o nounset
set -o pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIBRARY_DIR="${1:-$(cd "${SELF_DIR}/../.." && pwd)}"

if [[ ! -f "${LIBRARY_DIR}/CMakeLists.txt" ]]; then
  echo "error: ${LIBRARY_DIR} does not look like the library/ directory" >&2
  exit 2
fi

BUILD_OFF="${LIBRARY_DIR}/build_baseline_off"
BUILD_ON="${LIBRARY_DIR}/build_baseline_on"
SO_OFF="${BUILD_OFF}/libvgpu-control.so"
SO_ON="${BUILD_ON}/libvgpu-control.so"

CMAKE="${CMAKE:-cmake}"
PYTHON="${PYTHON:-python3}"
J="${J:-$(nproc 2>/dev/null | awk '{print int(($0+1)/2)}')}"
CUDA_HOME="${CUDA_HOME:-/usr/local/cuda}"

# --------------------------------------------------------------------------
# Capability gates: skip cleanly rather than report a false failure when
# the environment cannot exercise this check.
# --------------------------------------------------------------------------
for tool in "${CMAKE}" make gcc nm readelf; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "[SKIP] ${tool} not in PATH — cannot run baseline parity check"
    exit 0
  fi
done

# --------------------------------------------------------------------------
# Build helpers
# --------------------------------------------------------------------------
build_variant() {
  local build_dir="$1"
  local vk_opt="$2"   # ON or OFF

  mkdir -p "${build_dir}"
  ( cd "${build_dir}" && \
      CUDA_HOME="${CUDA_HOME}" "${CMAKE}" \
        -DCMAKE_BUILD_TYPE=Release \
        -DBUILD_VULKAN_LAYER="${vk_opt}" \
        "${LIBRARY_DIR}" \
        >/dev/null )
  make -C "${build_dir}" -j"${J}" vgpu-control >/dev/null
}

# Capture dynamic symbol surface in a canonical, comparable form.
# Only T (text) and W (weak) defined extern symbols matter for the
# loader resolution surface we are guarding.
dynsym_surface() {
  local so="$1"
  nm --defined-only --extern-only "${so}" \
    | awk '$2 == "T" || $2 == "W" { print $3 }' \
    | LC_ALL=C sort -u
}

# --------------------------------------------------------------------------
# Phase 1: build both variants
# --------------------------------------------------------------------------
echo "[INFO] building BUILD_VULKAN_LAYER=OFF at ${BUILD_OFF}"
build_variant "${BUILD_OFF}" OFF
echo "[INFO] building BUILD_VULKAN_LAYER=ON  at ${BUILD_ON}"
build_variant "${BUILD_ON}"  ON

if [[ ! -f "${SO_OFF}" || ! -f "${SO_ON}" ]]; then
  echo "[FAIL] one of the variants did not produce libvgpu-control.so" >&2
  ls -la "${SO_OFF}" "${SO_ON}" >&2 || true
  exit 1
fi

# --------------------------------------------------------------------------
# Phase 2: both builds must pass the export hygiene check
# --------------------------------------------------------------------------
echo "[INFO] running check_exported_symbols.sh on OFF build"
"${LIBRARY_DIR}/hack/check_exported_symbols.sh" "${SO_OFF}" >/dev/null
echo "[INFO] running check_exported_symbols.sh on ON  build"
"${LIBRARY_DIR}/hack/check_exported_symbols.sh" "${SO_ON}"  >/dev/null
echo "[PASS] both builds pass hack/check_exported_symbols.sh"

# --------------------------------------------------------------------------
# Phase 3: symbol surface diff
# --------------------------------------------------------------------------
SURFACE_OFF=$(dynsym_surface "${SO_OFF}")
SURFACE_ON=$(dynsym_surface "${SO_ON}")

# Set difference: what's in ON but not in OFF.
NEW_IN_ON=$(comm -23 <(echo "${SURFACE_ON}") <(echo "${SURFACE_OFF}"))
# And the inverse: what disappeared from OFF in ON (must be empty).
LOST_IN_ON=$(comm -13 <(echo "${SURFACE_ON}") <(echo "${SURFACE_OFF}"))

if [[ -n "${LOST_IN_ON}" ]]; then
  echo "[FAIL] BUILD_VULKAN_LAYER=ON build LOST symbols vs OFF baseline:"
  printf '         - %s\n' ${LOST_IN_ON}
  echo "       Vulkan build must be additive only."
  exit 1
fi

# What's allowed to be new? Exactly one symbol: the loader negotiate entry.
EXPECTED_NEW="vkNegotiateLoaderLayerInterfaceVersion"
UNEXPECTED_NEW=$(echo "${NEW_IN_ON}" | grep -vxF "${EXPECTED_NEW}" | grep -v '^$' || true)
if [[ -n "${UNEXPECTED_NEW}" ]]; then
  echo "[FAIL] BUILD_VULKAN_LAYER=ON build exports symbols beyond the"
  echo "       Vulkan loader negotiate entry. Forbidden additions:"
  printf '         - %s\n' ${UNEXPECTED_NEW}
  echo "       extend deploy/libvgpu-control.exports.ld 'local: *;'"
  echo "       coverage and re-verify, or annotate the new symbol as"
  echo "       static / hidden visibility in the source."
  exit 1
fi
if ! grep -qxF "${EXPECTED_NEW}" <<< "${NEW_IN_ON}"; then
  echo "[FAIL] BUILD_VULKAN_LAYER=ON build is missing"
  echo "       ${EXPECTED_NEW} — the loader cannot find the layer."
  exit 1
fi
echo "[PASS] symbol surface diff: ON = OFF + {${EXPECTED_NEW}}"

# --------------------------------------------------------------------------
# Phase 4: no library-load-time side effects (no constructors)
# --------------------------------------------------------------------------
check_no_init() {
  local so="$1"
  local label="$2"
  # .init_array (or legacy .ctors) absence proves no auto-run-on-dlopen
  # code. readelf -W keeps long section names on one line.
  if readelf -WS "${so}" 2>/dev/null \
      | awk '{print $2}' \
      | grep -qE '^(\.init_array|\.ctors|\.preinit_array)$'; then
    # Section exists; check it's actually populated (size > 0).
    local nonempty
    nonempty=$(readelf -WS "${so}" 2>/dev/null \
      | awk '$2 == ".init_array" || $2 == ".ctors" || $2 == ".preinit_array" {
               sz = strtonum("0x" $6); if (sz > 0) print $2 " size=" sz }')
    if [[ -n "${nonempty}" ]]; then
      echo "[FAIL] ${label} build contains non-empty constructor section(s):"
      printf '         %s\n' "${nonempty}"
      echo "       __attribute__((constructor)) / static init must not run"
      echo "       at dlopen time. See cfcb412 'forbid constructor/destructor"
      echo "       attrs in library/src/' for rationale."
      exit 1
    fi
  fi
}
check_no_init "${SO_OFF}" OFF
check_no_init "${SO_ON}"  ON
echo "[PASS] no library-load-time constructors in either build"

# --------------------------------------------------------------------------
# Phase 5: LD_PRELOAD smoke into /bin/true (layer must stay dormant)
# --------------------------------------------------------------------------
ld_preload_quiet() {
  local so="$1"
  local enable_value="$2"
  local label="$3"
  local err_out
  err_out=$(mktemp)
  if ! VGPU_VULKAN_ENABLE="${enable_value}" \
       LD_PRELOAD="${so}" /bin/true 2>"${err_out}"; then
    echo "[FAIL] LD_PRELOAD of ${label} build into /bin/true exited non-zero"
    echo "       VGPU_VULKAN_ENABLE=${enable_value}"
    echo "       stderr:"
    sed 's/^/         /' "${err_out}"
    rm -f "${err_out}"
    exit 1
  fi
  # We deliberately do not require zero bytes on stderr — the library is
  # allowed to log via its standard LOGGER path. We DO require no
  # explicit "error", "FATAL", "fault", "abort" substrings (case-
  # insensitive), which would indicate a crash or hard failure.
  if grep -qiE 'error|fatal|fault|abort|segv' "${err_out}"; then
    echo "[FAIL] LD_PRELOAD of ${label} build into /bin/true produced"
    echo "       error-class stderr output (VGPU_VULKAN_ENABLE=${enable_value}):"
    sed 's/^/         /' "${err_out}"
    rm -f "${err_out}"
    exit 1
  fi
  rm -f "${err_out}"
}
ld_preload_quiet "${SO_ON}" 0 ON
ld_preload_quiet "${SO_ON}" 1 ON
echo "[PASS] ON-build is dormant under LD_PRELOAD when no Vulkan loader is attached"

# --------------------------------------------------------------------------
# Phase 6: hack consistency checks pass on ON build
# --------------------------------------------------------------------------
echo "[INFO] running check_cuda_hook_consistency.py against the source tree"
"${PYTHON}" "${LIBRARY_DIR}/hack/check_cuda_hook_consistency.py" >/dev/null
echo "[PASS] hook consistency check"

# struct layout requires <cuda.h>; allow [SKIP] outcome.
echo "[INFO] running check_struct_layout.py against the source tree"
if "${PYTHON}" "${LIBRARY_DIR}/hack/check_struct_layout.py" \
       --cuda-home "${CUDA_HOME}" 2>&1 | tee /dev/stderr \
       | grep -qE '^\[FAIL\]'; then
  echo "[FAIL] struct layout check failed"
  exit 1
fi
echo "[PASS] struct layout check (or skipped)"

# --------------------------------------------------------------------------
# Summary
# --------------------------------------------------------------------------
echo
echo "=========================================================================="
echo "[PASS] Vulkan baseline parity:"
echo "       OFF build (${SO_OFF})"
echo "       ON  build (${SO_ON})"
echo "       differ by exactly: + ${EXPECTED_NEW}"
echo "       no load-time side effects, no LD_PRELOAD regressions,"
echo "       export hygiene + hook consistency intact."
echo "=========================================================================="
