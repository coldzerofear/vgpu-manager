#!/usr/bin/env bash
#
# Forbid __attribute__((constructor)) / __attribute__((destructor)) in
# library/src/. These run at .so load / unload time, which for an
# LD_PRELOAD'd library means they fire inside every CUDA-capable process
# on the node — including the very early window where the dynamic
# linker is still resolving libGLX_nvidia.so / libEGL_nvidia.so / etc.
#
# Why this matters:
#   Static initializer / constructor side effects from newly linked TUs
#   are a known root-cause candidate for NVIDIA-ICD init crashes (see
#   note 2026-04-28-vk-trace-isaac-sim.md) -- Vulkan/dispatch/hook
#   constructors firing in that early window can collide with the
#   driver's own ICD init.
#
#   We contain this with a linker version script (narrowing .dynsym) plus
#   manifest enable_environment gating. For that strategy to hold, the
#   .so must stay constructor-free: any future __attribute__((constructor))
#   on a non-static function would re-introduce the exact load-time
#   side-effect surface this is meant to avoid.
#
#   readelf -W libvgpu-control.so .init_array currently shows ONLY
#   __frame_dummy_init_array_entry (8 bytes, GCC stock dwarf frame
#   setup, unrelated to our code). This script fails the build the
#   moment a developer adds a constructor that would push that count
#   above the GCC baseline.
#
# Run from repo root or library/. Exit non-zero if any forbidden
# attribute appears in library/src/.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SRC_DIR="${LIB_ROOT}/src"

if [[ ! -d "${SRC_DIR}" ]]; then
  echo "[FAIL] library/src not found at ${SRC_DIR}" >&2
  exit 2
fi

# grep returns 0 if any match found, 1 if no match — invert for our
# pass/fail semantics.
matches=$(grep -rnE \
  '__attribute__\(\(\s*constructor[^)]*\)\)|__attribute__\(\(\s*destructor[^)]*\)\)' \
  "${SRC_DIR}" 2>/dev/null || true)

if [[ -n "${matches}" ]]; then
  echo "[FAIL] forbidden constructor / destructor attribute(s) in library/src/:"
  echo "${matches}" | sed 's/^/         /'
  echo
  echo "       Functions tagged with __attribute__((constructor)) run at"
  echo "       .so load time, before the LD_PRELOAD'd process can react."
  echo "       Inside an NVIDIA-driver process, that runs concurrently"
  echo "       with libGLX_nvidia / libEGL_nvidia / libvulkan ICD init"
  echo "       -- exactly the regression class this check exists to"
  echo "       prevent (see Step C trace notes)."
  echo
  echo "       Use lazy initialization (pthread_once or first-call check)"
  echo "       inside the function bodies that need state, not a"
  echo "       constructor."
  exit 1
fi

echo "[PASS] no constructor/destructor attributes in library/src/"
