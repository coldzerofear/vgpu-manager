#!/usr/bin/env bash
#
# Static sanity check for the Vulkan implicit layer artifact.
#
# Asserts (after a BUILD_VULKAN_LAYER=ON build):
#   1. libvgpu-control.so ELF-exports vkNegotiateLoaderLayerInterfaceVersion
#   2. libvgpu-control.so does NOT ELF-export any other vk* symbol
#      (would let the dynamic linker hijack Vulkan dispatch around the
#      loader)
#   3. The implicit-layer manifest JSON parses and carries the expected
#      layer name + library_path
#
# Usage:
#   check_vulkan_layer.sh <path/to/libvgpu-control.so> <path/to/manifest.json>
#
# Exit codes:
#   0  all checks pass
#   1  any check failed
#   2  invalid invocation / missing dependency
set -o errexit
set -o nounset
set -o pipefail

SO="${1:-}"
MANIFEST="${2:-}"

if [[ -z "${SO}" || -z "${MANIFEST}" ]]; then
  echo "usage: $0 <libvgpu-control.so> <manifest.json>" >&2
  exit 2
fi

if [[ ! -f "${SO}" ]]; then
  echo "[FAIL] .so not found: ${SO}" >&2
  exit 1
fi
if [[ ! -f "${MANIFEST}" ]]; then
  echo "[FAIL] manifest not found: ${MANIFEST}" >&2
  exit 1
fi

if ! command -v nm >/dev/null 2>&1; then
  echo "[FAIL] 'nm' tool not found" >&2
  exit 2
fi

fail=0

# ---- (1) Negotiate symbol present ----------------------------------
if nm -D --defined-only "${SO}" 2>/dev/null \
     | awk '$2=="T"||$2=="W" {print $3}' \
     | grep -qx 'vkNegotiateLoaderLayerInterfaceVersion'; then
  echo "[PASS] vkNegotiateLoaderLayerInterfaceVersion is exported"
else
  echo "[FAIL] vkNegotiateLoaderLayerInterfaceVersion NOT exported"
  fail=1
fi

# ---- (2) No OTHER vk* symbol exported -------------------------------
other=$(nm -D --defined-only "${SO}" 2>/dev/null \
     | awk '$2=="T"||$2=="W" {print $3}' \
     | grep -E '^vk' \
     | grep -vx 'vkNegotiateLoaderLayerInterfaceVersion' || true)
if [[ -n "${other}" ]]; then
  echo "[FAIL] unexpected vk* exports — would bypass Vulkan loader:"
  echo "${other}" | sed 's/^/        /'
  fail=1
else
  echo "[PASS] no other vk* symbols exported"
fi

# ---- (3) Manifest JSON shape ---------------------------------------
expect_name="VK_LAYER_VGPU_MANAGER_vgpu"
expect_path="/etc/vgpu-manager/driver/libvgpu-control.so"

if command -v python3 >/dev/null 2>&1; then
  py_check=$(python3 - <<PY 2>&1 || echo PARSE_FAIL
import json, sys
with open("${MANIFEST}") as f:
    d = json.load(f)
layer = d.get("layer", {})
checks = []
if layer.get("name") != "${expect_name}":
    checks.append("name=" + repr(layer.get("name")))
if layer.get("library_path") != "${expect_path}":
    checks.append("library_path=" + repr(layer.get("library_path")))
if layer.get("type") != "GLOBAL":
    checks.append("type=" + repr(layer.get("type")))
if "VGPU_VULKAN_ENABLE" not in (layer.get("enable_environment") or {}):
    checks.append("enable_environment missing VGPU_VULKAN_ENABLE")
if checks:
    sys.exit("BAD: " + "; ".join(checks))
print("OK")
PY
)
  if [[ "${py_check}" == "OK" ]]; then
    echo "[PASS] manifest shape (name=${expect_name}, library_path=${expect_path})"
  else
    echo "[FAIL] manifest check: ${py_check}"
    fail=1
  fi
else
  echo "[SKIP] python3 not available; manifest JSON shape not checked"
fi

if [[ ${fail} -ne 0 ]]; then
  echo "[FAIL] check_vulkan_layer.sh: one or more checks failed"
  exit 1
fi
echo "[PASS] check_vulkan_layer.sh: all checks passed"
exit 0
