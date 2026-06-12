#!/usr/bin/env bash
#
# Forbid __attribute__((constructor)) / __attribute__((destructor)) in
# library/src/. These run at .so load / unload time, which for an
# LD_PRELOAD'd library means they fire inside every CUDA-capable process
# on the node — including the very early window where the dynamic
# linker is still resolving libGLX_nvidia.so / libEGL_nvidia.so / etc.
#
# Why this matters (HAMi-core PR #182 lessons):
#   HAMi-core's unsolved Step C regression (note 2026-04-28-vk-trace-isaac-sim.md)
#   identified "static initializer / constructor side effects from new
#   TUs being linked in" as one of the prime root-cause candidates
#   for an NVIDIA-ICD init crash that they could not pin down. Their
#   remediation was to physically split libvgpu_vk.so out of libvgpu.so
#   so the LD_PRELOAD'd binary contains no Vulkan / dispatch / hook
#   constructors at all.
#
#   We rely on a different containment strategy (linker version script
#   to narrow .dynsym + manifest enable_environment gating). For that
#   strategy to be equivalent in practice we need the .so to remain
#   constructor-free: any future __attribute__((constructor)) on a
#   non-static function would re-introduce the exact load-time side-
#   effect surface that drove HAMi to split.
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
  echo "       and is exactly the regression class HAMi-core PR #182"
  echo "       could not pin down (see Step C trace notes)."
  echo
  echo "       Use lazy initialization (pthread_once or first-call check)"
  echo "       inside the function bodies that need state, not a"
  echo "       constructor."
  exit 1
fi

echo "[PASS] no constructor/destructor attributes in library/src/"
