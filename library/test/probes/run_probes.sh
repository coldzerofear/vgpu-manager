#!/usr/bin/env bash
#
# The long-running checks: see README.md. Needs a GPU, and root on a bare host (the
# reclaim probe writes the SESSION accounting's pid list). Takes several minutes.
#
# Usage: run_probes.sh <probe-build-dir> <path/to/libvgpu-control.so>

set -o nounset
set -o pipefail

PROBES="${1:-}"
VGPU_SO="${2:-}"
if [[ -z "${PROBES}" || -z "${VGPU_SO}" ]]; then
  echo "usage: $0 <probe-build-dir> <path/to/libvgpu-control.so>" >&2
  exit 2
fi
CONFIG_DIR=/etc/vgpu-manager/config

if ! GPUS="$(nvidia-smi --query-gpu=uuid --format=csv,noheader)" || [[ -z "${GPUS}" ]]; then
  echo "[FAIL] no GPU found: ${GPUS}" >&2
  exit 1
fi
GPUS="$(printf '%s\n' "${GPUS}" | paste -sd, -)"
FIRST_GPU="${GPUS%%,*}"
export NVIDIA_VISIBLE_DEVICES="${GPUS}" VMEMORY_NODE_ENABLED=1 CUDA_MEM_LIMIT=8192m

fail=0
report_fail() { echo "  FAIL: $*"; fail=1; }

# A config left by an earlier run overrides this environment.
fresh_config() { rm -f "${CONFIG_DIR}/vgpu.config"; }

# churn MODE N TABLE CAPACITY COUNT: after N cycles TABLE must be at CAPACITY or below
# holding COUNT entries or fewer.
churn() {
  local mode="$1" n="$2" table="$3" max_capacity="$4" max_count="$5" log rc=0 capacity count
  log="$(mktemp)"
  fresh_config
  LOGGER_LEVEL=4 LD_PRELOAD="${VGPU_SO}" timeout 300 "${PROBES}/probe_churn" "${mode}" "${n}" >"${log}" 2>&1 || rc=$?
  if (( rc == 77 )); then
    printf '%-28s skipped: needs two devices\n' "${mode}"
    rm -f "${log}"
    return
  fi
  capacity="$(sed -n "s/.*table stats:.*${table} capacity=\([0-9]*\).*/\1/p" "${log}" | tail -1)"
  count="$(sed -n "s/.*table stats:.*${table} capacity=[0-9]* count=\([0-9]*\).*/\1/p" "${log}" | tail -1)"
  printf '%-28s N=%-7s %s capacity=%s count=%s\n' "${mode}" "${n}" "${table}" "${capacity:-?}" "${count:-?}"
  if (( rc != 0 )); then
    report_fail "rc=${rc}"; tail -5 "${log}" | sed 's/^/    /'
  elif [[ -z "${capacity}" ]]; then
    report_fail "no table stats in the log"
  elif (( capacity > max_capacity || count > max_count )); then
    report_fail "expected capacity <= ${max_capacity}, count <= ${max_count}"
  fi
  rm -f "${log}"
}

echo "=== tables under churn"
churn exec                        20000 cost    4096 0
churn exec_multi                   3000 cost    4096 0
churn exec_update                  3000 cost    4096 0
churn ctx                           100 cost    4096 0
churn ctx_not_current               100 cost    4096 0
churn ctx_multi                     100 cost    4096 0
churn primary_release                50 cost    4096 0
churn primary_reset                  50 cost    4096 0
churn capture                    100000 capture   64 0
churn capture_invalidated          5000 capture   64 0
churn capture_wrong_thread         3000 capture   64 0
churn capture_stream_destroyed     5000 capture   64 0
churn capture_ctx_destroyed         100 capture   64 0
churn capture_other_ctx_destroyed   100 capture   64 0
churn capture_begin_ctx_destroyed   100 capture   64 0
# Thread ids come back, so one record; the point is it stays at one.
churn capture_thread_exit          5000 capture   64 1
churn held                        10000 held      64 0
churn held_ctx                      100 held      64 0
churn pool                        10000 pool      64 0
churn pool_get                   100000 pool      64 2

echo "=== fork under table churn"
fresh_config
if ! LD_PRELOAD="${VGPU_SO}" timeout 200 "${PROBES}/probe_fork" 60; then report_fail "probe_fork"; fi

echo "=== implicit pool reclaim (SESSION accounting, GPU 0)"
dir="$(mktemp -d)"
fresh_config
mkdir -p "${CONFIG_DIR}"
CUDA_VISIBLE_DEVICES=0 "${PROBES}/probe_reclaim" B "${dir}" 600 &
other=$!
rc=0
# sh so the pid written is the one that execs into the probe.
CUDA_VISIBLE_DEVICES=0 NVIDIA_VISIBLE_DEVICES="${FIRST_GPU}" MANAGER_COMPATIBILITY_MODE=300 CUDA_MEM_LIMIT=4096m \
  timeout 120 sh -c "echo \$\$ > '${CONFIG_DIR}/pids.config'; LD_PRELOAD='${VGPU_SO}' exec '${PROBES}/probe_reclaim' A '${dir}'" 2>/dev/null || rc=$?
wait "${other}"
rm -f "${CONFIG_DIR}/pids.config"
rm -rf "${dir}"
if (( rc == 77 )); then
  echo "  skipped: the driver did not reclaim"
elif (( rc != 0 )); then
  report_fail "probe_reclaim rc=${rc}"
fi

echo "=== capture and stream queries on this driver, and the library changes none of it"
plain="$(mktemp)"
hooked="$(mktemp)"
rc=0
env -u VMEMORY_NODE_ENABLED -u CUDA_MEM_LIMIT -u NVIDIA_VISIBLE_DEVICES "${PROBES}/probe_capture_apis" >"${plain}" 2>&1 || rc=$?
cat "${plain}"
(( rc == 0 )) || report_fail "probe_capture_apis rc=${rc}"
fresh_config
rc=0
LD_PRELOAD="${VGPU_SO}" "${PROBES}/probe_capture_apis" >"${hooked}" 2>/dev/null || rc=$?
(( rc == 0 )) || report_fail "probe_capture_apis under the library rc=${rc}"
diff "${plain}" "${hooked}" || report_fail "the library changes what a capture survives"
rm -f "${plain}" "${hooked}"

if [[ -x "${PROBES}/probe_graph_throttle" ]]; then
  echo "=== graphs stay throttled in number (core limit 10, compare with the same line without the library)"
  fresh_config
  CUDA_VISIBLE_DEVICES=0 NVIDIA_VISIBLE_DEVICES="${FIRST_GPU}" CUDA_CORE_LIMIT=10 LD_PRELOAD="${VGPU_SO}" \
    timeout 200 "${PROBES}/probe_graph_throttle" 6000 2>/dev/null || report_fail "probe_graph_throttle"
fi
if [[ -x "${PROBES}/probe_launch_device" && "${GPUS}" == *,* ]]; then
  echo "=== a graph launch is charged to the exec's device (device 0 at 10%)"
  fresh_config
  CUDA_CORE_LIMIT_0=10 CUDA_CORE_LIMIT_1=100 LD_PRELOAD="${VGPU_SO}" \
    timeout 60 "${PROBES}/probe_launch_device" 2>/dev/null || report_fail "probe_launch_device"
fi

if python3 -c 'import torch' 2>/dev/null; then
  echo "=== many live torch graphs"
  fresh_config
  LOGGER_LEVEL=4 LD_PRELOAD="${VGPU_SO}" timeout 200 python3 -W ignore "${PROBES}/torch_many_graphs.py" default 1700 2>&1 |
    grep -E '^live|^captured|^dropped|^failed|table stats' | sed 's/.*table stats/table stats/'
  (( PIPESTATUS[0] == 0 )) || report_fail "torch_many_graphs"
else
  echo "=== many live torch graphs: skipped, no torch"
fi

fresh_config
exit "${fail}"
