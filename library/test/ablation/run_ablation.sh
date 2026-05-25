#!/usr/bin/env bash
#
# Orchestrate the SM-throttle ablation: build the workload, then run
# collect.sh once per variant under the right env, organizing outputs into
#   data/<YYYYMMDD-HHMMSS>-<gpu_safe>/<variant>/
# Each variant dir is what plot_compare.py consumes.
#
# Usage:
#   run_ablation.sh                            # default variants: delta, aimd
#   VARIANTS="delta aimd" run_ablation.sh      # explicit list
#
# Required (no defaults guessed -- you want to know which .so you measured):
#   VGPU_SO=/abs/path/to/libvgpu-control.so
#
# Optional knobs (forwarded to collect.sh):
#   CUDA_DEVICE_SM_LIMIT=30
#   ABLATION_DURATION_S=30
#   ABLATION_GPU_ID=0
#   ABLATION_SAMPLE_MS=100
#   AIMD_MD_DIVISOR / AIMD_EFF_RATIO / AIMD_AI_BASE_DIV  (only used when the
#                                                        active variant is aimd)
#   OUT_BASE=./data                             # where the dated dir is created

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

VARIANTS="${VARIANTS:-delta aimd}"
OUT_BASE="${OUT_BASE:-$SCRIPT_DIR/data}"
ABLATION_GPU_ID="${ABLATION_GPU_ID:-0}"

# ---------- Preflight ------------------------------------------------------

if [[ -z "${VGPU_SO:-}" || ! -f "${VGPU_SO}" ]]; then
  echo "error: VGPU_SO must point to libvgpu-control.so (got: '${VGPU_SO:-}')" >&2
  echo "       build it first: cd library && make build" >&2
  exit 2
fi
if ! command -v nvidia-smi >/dev/null 2>&1; then
  echo "error: nvidia-smi not on PATH (need NVIDIA driver to measure)" >&2
  exit 2
fi
if ! command -v nvcc >/dev/null 2>&1; then
  echo "error: nvcc not on PATH (need CUDA toolkit to build the workload)" >&2
  exit 2
fi
if ! nvidia-smi -i "$ABLATION_GPU_ID" >/dev/null 2>&1; then
  echo "error: GPU $ABLATION_GPU_ID not visible to nvidia-smi" >&2
  exit 2
fi

# ---------- Build the workload --------------------------------------------

BUILD_DIR="$SCRIPT_DIR/build"
WORKLOAD_BIN="$BUILD_DIR/workload"
mkdir -p "$BUILD_DIR"

if [[ ! -x "$WORKLOAD_BIN" || "$SCRIPT_DIR/workload.cu" -nt "$WORKLOAD_BIN" ]]; then
  echo "==> Building workload"
  # -O2 is enough; we want sustained compute, not micro-benchmark precision.
  # No -arch flag -> nvcc picks a default; override with NVCC_ARCH=-arch=sm_80 etc.
  nvcc ${NVCC_ARCH:-} -O2 "$SCRIPT_DIR/workload.cu" -o "$WORKLOAD_BIN"
fi
export WORKLOAD_BIN
export VGPU_SO

# ---------- Dated output dir keyed by GPU model ----------------------------

DATESTAMP=$(date +%Y%m%d-%H%M%S)
GPU_RAW=$(nvidia-smi --query-gpu=name --format=csv,noheader -i "$ABLATION_GPU_ID" | head -1)
GPU_SAFE=$(echo "$GPU_RAW" | tr ' /:' '___' | tr -dc 'A-Za-z0-9_-')
OUT_DIR="$OUT_BASE/${DATESTAMP}-${GPU_SAFE}"
mkdir -p "$OUT_DIR"
echo "==> Output: $OUT_DIR"

# ---------- Per-variant run -----------------------------------------------

for variant in $VARIANTS; do
  echo "==> Variant: $variant"
  VAR_OUT="$OUT_DIR/$variant"
  mkdir -p "$VAR_OUT"

  # Each variant sets its own env. Anything not set here falls through to
  # collect.sh's defaults.
  case "$variant" in
    delta)
      env -u CUDA_SM_AIMD_MD_DIVISOR \
          -u CUDA_SM_AIMD_EFF_RATIO \
          -u CUDA_SM_AIMD_AI_BASE_DIV \
        CUDA_SM_CONTROLLER=delta \
        "$SCRIPT_DIR/collect.sh" "$variant" "$VAR_OUT"
      ;;
    aimd)
      CUDA_SM_CONTROLLER=aimd \
      CUDA_SM_AIMD_MD_DIVISOR="${AIMD_MD_DIVISOR:-}" \
      CUDA_SM_AIMD_EFF_RATIO="${AIMD_EFF_RATIO:-}" \
      CUDA_SM_AIMD_AI_BASE_DIV="${AIMD_AI_BASE_DIV:-}" \
        "$SCRIPT_DIR/collect.sh" "$variant" "$VAR_OUT"
      ;;
    *)
      # Free-form variant: caller controls env via the parent shell. Useful
      # for parameter sweeps (e.g. variant="aimd_md2" with AIMD_MD_DIVISOR=2).
      "$SCRIPT_DIR/collect.sh" "$variant" "$VAR_OUT"
      ;;
  esac
done

# ---------- Summary --------------------------------------------------------

echo
echo "==> Done. Variant dirs under: $OUT_DIR"
ls -1 "$OUT_DIR"
echo
echo "Next: plot it"
echo "  python3 $SCRIPT_DIR/plot_compare.py $OUT_DIR"
