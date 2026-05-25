#!/usr/bin/env bash
#
# Run one ablation variant: launch the workload under LD_PRELOAD of
# libvgpu-control.so while concurrently sampling SM utilization via
# nvidia-smi. Writes:
#
#   <OUT_DIR>/samples.csv     elapsed_ms, sm_util_pct (one row per sample)
#   <OUT_DIR>/workload.log    stdout/stderr of the workload
#   <OUT_DIR>/meta.json       variant, target, controller, GPU model, ts
#
# Usage:
#   collect.sh <variant_label> <output_dir>
#
# Required env:
#   VGPU_SO            absolute path to libvgpu-control.so
#   WORKLOAD_BIN       absolute path to the built workload binary
#
# Optional env (caller chooses the variant by setting these):
#   CUDA_DEVICE_SM_LIMIT       target SM utilization, percent (default 30)
#   CUDA_SM_CONTROLLER         delta (default) | aimd
#   CUDA_SM_AIMD_MD_DIVISOR    AIMD MD factor (only if controller=aimd)
#   CUDA_SM_AIMD_EFF_RATIO     AIMD buffer / 1000
#   CUDA_SM_AIMD_AI_BASE_DIV   AIMD AI step base divisor
#   ABLATION_DURATION_S        workload wall time (default 30)
#   ABLATION_GPU_ID            GPU index for both workload + nvidia-smi (default 0)
#   ABLATION_SAMPLE_MS         nvidia-smi sample interval ms (default 100)
#
# Honest measurement note:
#   The GAP path is always-on in vgpu-manager (gated by core_limit, no env
#   switch). So this script measures "controller + GAP" combinations as
#   shipped -- it is not a pure controller-only A/B. To compare controllers
#   in isolation you would need a build with GAP stripped (out of scope here).

set -o errexit
set -o nounset
set -o pipefail

VARIANT="${1:-}"
OUT_DIR="${2:-}"

if [[ -z "$VARIANT" || -z "$OUT_DIR" ]]; then
  echo "usage: $0 <variant_label> <output_dir>" >&2
  exit 2
fi
if [[ -z "${VGPU_SO:-}" || ! -f "$VGPU_SO" ]]; then
  echo "error: VGPU_SO must point to libvgpu-control.so (got: '${VGPU_SO:-}')" >&2
  exit 2
fi
if [[ -z "${WORKLOAD_BIN:-}" || ! -x "$WORKLOAD_BIN" ]]; then
  echo "error: WORKLOAD_BIN must point to the built workload binary (got: '${WORKLOAD_BIN:-}')" >&2
  exit 2
fi
if ! command -v nvidia-smi >/dev/null 2>&1; then
  echo "error: nvidia-smi not on PATH" >&2
  exit 2
fi

CUDA_DEVICE_SM_LIMIT="${CUDA_DEVICE_SM_LIMIT:-30}"
CUDA_SM_CONTROLLER="${CUDA_SM_CONTROLLER:-delta}"
ABLATION_DURATION_S="${ABLATION_DURATION_S:-30}"
ABLATION_GPU_ID="${ABLATION_GPU_ID:-0}"
ABLATION_SAMPLE_MS="${ABLATION_SAMPLE_MS:-100}"

mkdir -p "$OUT_DIR"
SAMPLES="$OUT_DIR/samples.csv"
WORKLOAD_LOG="$OUT_DIR/workload.log"
META="$OUT_DIR/meta.json"

# Capture GPU model name once (needed for meta + sanity).
GPU_NAME=$(nvidia-smi --query-gpu=name --format=csv,noheader -i "$ABLATION_GPU_ID" | head -1 | sed 's/^ *//;s/ *$//')

# Sampler: nvidia-smi -lms emits one util value per cycle from a single long-
# lived process (no per-sample fork overhead). We synthesize elapsed_ms from
# the sample index * interval, which is exact for -lms's fixed cadence.
echo "elapsed_ms,sm_util_pct" > "$SAMPLES"
nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits \
  -i "$ABLATION_GPU_ID" -lms "$ABLATION_SAMPLE_MS" 2>/dev/null \
  | awk -v ms="$ABLATION_SAMPLE_MS" '{print (NR-1)*ms","$1; fflush()}' \
  >> "$SAMPLES" &
SAMPLER_PID=$!
# Guarantee the sampler is killed on any exit path (success, fail, Ctrl-C).
trap 'kill "$SAMPLER_PID" 2>/dev/null || true; wait "$SAMPLER_PID" 2>/dev/null || true' EXIT

# Brief wait so the sampler has at least one pre-workload sample (provides a
# visible idle baseline at the start of the time series).
sleep 0.5

# Run the workload. CUDA_VISIBLE_DEVICES pins it to the same GPU we're sampling.
START_TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)
START_NS=$(date +%s%N)
set +e
CUDA_VISIBLE_DEVICES="$ABLATION_GPU_ID" \
  LD_PRELOAD="$VGPU_SO" \
  CUDA_DEVICE_SM_LIMIT="$CUDA_DEVICE_SM_LIMIT" \
  CUDA_SM_CONTROLLER="$CUDA_SM_CONTROLLER" \
  CUDA_SM_AIMD_MD_DIVISOR="${CUDA_SM_AIMD_MD_DIVISOR:-}" \
  CUDA_SM_AIMD_EFF_RATIO="${CUDA_SM_AIMD_EFF_RATIO:-}" \
  CUDA_SM_AIMD_AI_BASE_DIV="${CUDA_SM_AIMD_AI_BASE_DIV:-}" \
  ABLATION_DURATION_S="$ABLATION_DURATION_S" \
  "$WORKLOAD_BIN" >"$WORKLOAD_LOG" 2>&1
WORKLOAD_RC=$?
set -e
END_NS=$(date +%s%N)
WALL_MS=$(( (END_NS - START_NS) / 1000000 ))

# Stop sampler and let trap clean it up; brief settle so last samples flush.
sleep 0.2
kill "$SAMPLER_PID" 2>/dev/null || true
wait "$SAMPLER_PID" 2>/dev/null || true
trap - EXIT

# Surface the workload's one-line ABLATION_RESULT for convenience.
WORKLOAD_RESULT=$(grep '^ABLATION_RESULT' "$WORKLOAD_LOG" | tail -1 || true)

# Compose meta.json with both the requested variant params and the actual
# measured wall time / GPU model.
cat > "$META" <<JSON
{
  "variant": "$VARIANT",
  "controller": "$CUDA_SM_CONTROLLER",
  "target_sm_pct": $CUDA_DEVICE_SM_LIMIT,
  "duration_s_requested": $ABLATION_DURATION_S,
  "wall_ms_actual": $WALL_MS,
  "gpu_id": $ABLATION_GPU_ID,
  "gpu_name": "$GPU_NAME",
  "sample_interval_ms": $ABLATION_SAMPLE_MS,
  "aimd_md_divisor": "${CUDA_SM_AIMD_MD_DIVISOR:-default}",
  "aimd_eff_ratio": "${CUDA_SM_AIMD_EFF_RATIO:-default}",
  "aimd_ai_base_div": "${CUDA_SM_AIMD_AI_BASE_DIV:-default}",
  "workload_bin": "$WORKLOAD_BIN",
  "vgpu_so": "$VGPU_SO",
  "started_at": "$START_TS",
  "workload_rc": $WORKLOAD_RC,
  "workload_result": "$WORKLOAD_RESULT"
}
JSON

echo "[$VARIANT] gpu=$GPU_NAME target=${CUDA_DEVICE_SM_LIMIT}% controller=$CUDA_SM_CONTROLLER duration=${ABLATION_DURATION_S}s rc=$WORKLOAD_RC samples=$(wc -l < "$SAMPLES")"

exit "$WORKLOAD_RC"
