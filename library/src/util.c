/*
Copyright 2025-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

#include "include/hook.h"
#include "include/sm_delta.h"

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <dirent.h>
#include <string.h>
#include <unistd.h>
#include <limits.h>
#include <errno.h>
#include <sched.h>
#include <sys/file.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <signal.h>
#include <time.h>

#define MAX_PID_STR_LEN 32
#define CUDA_MEMORY_LIMIT_ENV "CUDA_MEM_LIMIT"
#define CUDA_MEMORY_RATIO_ENV "CUDA_MEM_RATIO"
#define CUDA_CORE_LIMIT_ENV "CUDA_CORE_LIMIT"
#define CUDA_CORE_SOFT_LIMIT_ENV "CUDA_CORE_SOFT_LIMIT"
#define CUDA_MEM_OVERSOLD_ENV "CUDA_MEM_OVERSOLD"
#define VMEM_NODE_ENABLED_ENV "VMEMORY_NODE_ENABLED"
/* How to advise the driver about oversold managed allocations.
 *   0  off (default)
 *   1  SET_PREFERRED_LOCATION = device  -- "keep it on the card"
 *   2  the above plus SET_ACCESSED_BY = device -- "and when it cannot stay,
 *      read it where it lies instead of migrating it back"
 * One knob with three arms rather than several knobs: the arms are mutually
 * exclusive hypotheses and want to be told apart in a measurement, not
 * combined. */
#define CUDA_MEM_UVA_ADVISE_ENV "CUDA_MEM_UVA_ADVISE"
#define CUDA_MEM_UVA_ADVISE_MAX_MODE 2
#define MANAGER_VISIBLE_DEVICE_ENV "MANAGER_VISIBLE_DEVICE"
#define MANAGER_VISIBLE_DEVICES_ENV (MANAGER_VISIBLE_DEVICE_ENV "S")
#define NVIDIA_VISIBLE_DEVICES_ENV "NVIDIA_VISIBLE_DEVICES"
#define MANAGER_COMPATIBILITY_MODE_ENV "MANAGER_COMPATIBILITY_MODE"
#define EXTERNAL_SM_WATCHER_ENABLED_ENV "EXTERNAL_SM_WATCHER_ENABLED"

/* SM throttle controller selection + AIMD parameters. The watcher's per-cycle
 * share update is delegated to a controller. Default keeps stock behaviour.
 * AIMD ("aimd") does slow additive increase (gap-proportional) + fast
 * multiplicative decrease.
 *
 *   CUDA_SM_CONTROLLER       = "delta" (default) | "aimd"
 *   CUDA_SM_AIMD_MD_DIVISOR  = MD factor (share /= div) - default 3
 *   CUDA_SM_AIMD_EFF_RATIO   = effective-limit buffer / 1000 - default 875 (87.5%)
 *   CUDA_SM_AIMD_AI_BASE_DIV = AI step base divisor - default 400
 */
#define CUDA_SM_CONTROLLER_ENV       "CUDA_SM_CONTROLLER"
#define CUDA_SM_AIMD_MD_DIVISOR_ENV  "CUDA_SM_AIMD_MD_DIVISOR"
#define CUDA_SM_AIMD_EFF_RATIO_ENV   "CUDA_SM_AIMD_EFF_RATIO"
#define CUDA_SM_AIMD_AI_BASE_DIV_ENV "CUDA_SM_AIMD_AI_BASE_DIV"

/* "auto" controller mode (experimental): switch between delta (single-Pod,
 * for throughput) and aimd (multi-Pod, for fairness) based on per-device
 * sys_process_num observed by the watcher. Debounced to avoid pingponging
 * on NVML's process-count jitter. */
#define CUDA_SM_AUTO_DEBOUNCE_CYCLES_ENV         "CUDA_SM_AUTO_DEBOUNCE_CYCLES"
/* SM-utilization threshold (percent) above which a non-self-container
 * process is considered to actually compete with us. Default 1: just
 * enough to filter the always-on driver/persistenced threads (which
 * report ~0%) while still recognising the smallest real Pod activity.
 * Used by both the AUTO controller and the watcher's soft_core burst
 * decision to identify "real exclusive use" of the GPU. */
#define CUDA_SM_AUTO_EXTERNAL_UTIL_THRESHOLD_ENV "CUDA_SM_AUTO_EXTERNAL_UTIL_THRESHOLD"

/* AIMD anti-sawtooth tunables (P1, see sm_controller_aimd_sawtooth_analysis.md).
 *
 *   DEADBAND_RATIO  -- per-thousand. Defines the AI lower edge of the
 *                      hysteresis band around eff_limit. Default 800 means
 *                      AI fires only when user_current < up_limit*0.80
 *                      (i.e. below 80% of target), MD fires above eff_limit
 *                      (up_limit*0.875 by default). Inside the band the
 *                      controller leaves share alone. Must be < EFF_RATIO.
 *
 *   MD_COOLDOWN_CYCLES -- how many watcher cycles after an MD the controller
 *                         will NOT MD again, even if user_current still
 *                         exceeds eff_limit. Default 3 (~240ms at 80ms
 *                         watcher cadence). Set 0 to disable cooldown
 *                         entirely (retain V2.1 behaviour). */
#define CUDA_SM_AIMD_DEADBAND_RATIO_ENV     "CUDA_SM_AIMD_DEADBAND_RATIO"
#define CUDA_SM_AIMD_MD_COOLDOWN_CYCLES_ENV "CUDA_SM_AIMD_MD_COOLDOWN_CYCLES"

/* Avg-free-headroom threshold (percent) used by the soft-mode periodic
 * up_limit adjuster. avg > x -> climb, avg < x -> step back down,
 * avg == x -> hold. Range: non-negative integer. 0 means "any headroom
 * triggers climb" (use with care, can cause up_limits oscillation in
 * mostly-idle workloads). */
#define CUDA_SM_USAGE_THRESHOLD_ENV         "CUDA_SM_USAGE_THRESHOLD"

/* delta() emergency cut-floor divisor N: when util exceeds TWICE the target
 * (a real blowout, where the share-proportional cut stalls because share is
 * tiny relative to the excess), the cut is floored at g_total*diff/(u*N).
 * Default 64. <= 0 disables the emergency floor. Grow is never floored --
 * an absolute grow step is exactly the bucket-flooding mechanism delta was
 * rebuilt to remove (see include/sm_delta.h). */
/* Container-wide shared token bucket. ON by default -- one bucket per container
 * is what a core quota means, since a per-process bucket lets an N-process
 * container use N times its cores. Set to 0 to fall back to the per-process
 * bucket; a session refuses that opt-out.
 * See docs/sm_multiproc_shared_bucket_design.md. */
#define CUDA_SM_SHARED_BUCKET_ENV "CUDA_SM_SHARED_BUCKET"
#define CUDA_SM_DELTA_RAMP_FLOOR_DIVISOR_ENV "CUDA_SM_DELTA_RAMP_FLOOR_DIVISOR"

/* Seed divisor R in delta(): the minimum step is total*MIN_INCREMENT/R -- the
 * controller's GRANULARITY. A step coarser than a light workload's per-tick
 * consumption pins util at 100% (the share cannot express a small enough
 * refill), so R errs fine: 1310720 ~= 0.0004% of the pool. Response speed is
 * owned by the share-proportional term, not this knob. See sm_delta.h. */
#define CUDA_SM_DELTA_INCREMENT_DIVISOR_ENV "CUDA_SM_DELTA_INCREMENT_DIVISOR"

/*-- Cache: Read only once during the process lifecycle /proc/1/environ --*/
static char           *g_environ_buf  = NULL;   /* The cached raw content */
static size_t          g_environ_len  = 0;      /* Effective byte count   */
static pthread_once_t  g_once         = PTHREAD_ONCE_INIT;

static void _load_pid1_environ(void) {
  FILE *f = fopen("/proc/1/environ", "re");
  if (!f) return;

  /* Obtain actual size */
  struct stat st;
  if (fstat(fileno(f), &st) != 0 || st.st_size <= 0) {
    fclose(f);
    return;
  }

  /* Allocate according to actual size */
  size_t cap = (size_t)st.st_size;
  char *buf = malloc(cap + 1);
  if (!buf) {
    fclose(f);
    return;
  }

  size_t nread = fread(buf, 1, cap, f);
  fclose(f);

  if (nread == 0) {
    free(buf);
    return;
  }
  // Bottom line to prevent crossing the boundary when the NUL is missing at the end.
  buf[nread] = '\0';
  g_environ_buf = buf;
  g_environ_len = nread;
}

char* _getenv(const char* name) {
  if (!name || !name[0]) return NULL;

  char *val = getenv(name);
  if (val || getpid() == 1) return val;

  pthread_once(&g_once, _load_pid1_environ);
  if (!g_environ_buf) return NULL;

  size_t name_len = strlen(name);
  char *p = g_environ_buf;
  char *end = g_environ_buf + g_environ_len;

  while (p < end) {
    size_t entry_len = strnlen(p, end - p);
    if (entry_len > name_len &&
      strncmp(p, name, name_len) == 0 &&
      p[name_len] == '=') {
      return p + name_len + 1;
    }
    p += entry_len + 1;
  }
  return NULL;
}

size_t iec_to_bytes(const char *iec_value) {
  char *endptr = NULL;
  double value = 0.0;

  value = strtod(iec_value, &endptr);
  switch (*endptr) {
  case 'K':
  case 'k':
    value *= 1024UL;
    break;
  case 'M':
  case 'm':
    value *= 1024UL * 1024UL;
    break;
  case 'G':
  case 'g':
    value *= 1024UL * 1024UL * 1024UL;
    break;
  case 'T':
  case 't':
    value *= 1024UL * 1024UL * 1024UL * 1024UL;
    break;
  default:
    break;
  }
  return (size_t)value;
}

int get_compatibility_mode(int *mode) {
  char *str = _getenv(MANAGER_COMPATIBILITY_MODE_ENV);
  if (!str || str[0] == '\0') {
    return -1;
  }
  *mode = iec_to_bytes(str);
  return 0;
}

int get_mem_ratio(uint32_t index, double *ratio) {
  char env[32] = {0};
  snprintf(env, sizeof(env), "%s_%u", CUDA_MEMORY_RATIO_ENV, index);
  char *str = _getenv(env);
  if (!str) {
    str = _getenv(CUDA_MEMORY_RATIO_ENV);
    if (!str) {
      return -1;
    }
  }
  if (str[0] == '\0') {
    return -1;
  }
  *ratio = atof(str);
  return 0;
}

int get_mem_limit(uint32_t index, size_t *limit) {
  char env[32] = {0};
  snprintf(env, sizeof(env), "%s_%u", CUDA_MEMORY_LIMIT_ENV, index);
  char *str = _getenv(env);
  if (!str) {
    str = _getenv(CUDA_MEMORY_LIMIT_ENV);
    if (!str) {
      return -1;
    }
  }
  if (str[0] == '\0') {
    return -1;
  }
  *limit = iec_to_bytes(str);
  return 0;
}

int get_core_limit(uint32_t index, int *limit) {
  char env[32] = {0};
  snprintf(env, sizeof(env), "%s_%u", CUDA_CORE_LIMIT_ENV, index);
  char *str = _getenv(env);
  if (!str) {
    str = _getenv(CUDA_CORE_LIMIT_ENV);
    if (!str) {
      return -1;
    }
  }
  if (str[0] == '\0') {
    return -1;
  }
  *limit = iec_to_bytes(str);
  return 0;
}

int get_core_soft_limit(uint32_t index, int *limit) {
  char env[32] = {0};
  snprintf(env, sizeof(env), "%s_%u", CUDA_CORE_SOFT_LIMIT_ENV, index);
  char *str = _getenv(env);
  if (!str) {
    str = _getenv(CUDA_CORE_SOFT_LIMIT_ENV);
    if (!str) {
      return -1;
    }
  }
  if (str[0] == '\0') {
    return -1;
  }
  *limit = iec_to_bytes(str);
  return 0;
}

int get_manager_device_uuid(uint32_t index, char *uuid, size_t uuid_size) {
  char env[32] = {0};
  snprintf(env, sizeof(env), "%s_%u", MANAGER_VISIBLE_DEVICE_ENV, index);
  char *str = _getenv(env);
  if (!str || str[0] == '\0') {
    return -1;
  }
  if (snprintf(uuid, uuid_size, "%s", str) >= uuid_size) {
    LOGGER(WARNING, "device uuid env %s truncated", env);
    return -1;
  }
  return 0;
}

int get_nvidia_device_uuids(char *uuids, size_t uuids_size) {
  char *str = NULL;
  str = _getenv(NVIDIA_VISIBLE_DEVICES_ENV);
  if (!str || str[0] == '\0') {
    return -1;
  }
  if (snprintf(uuids, uuids_size, "%s", str) >= uuids_size) {
    LOGGER(WARNING, "device uuids env %s truncated", NVIDIA_VISIBLE_DEVICES_ENV);
    return -1;
  }
  return 0;
}

int get_manager_device_uuids(char *uuids, size_t uuids_size) {
  char *str = NULL;
  str = _getenv(MANAGER_VISIBLE_DEVICES_ENV);
  if (!str || str[0] == '\0') {
    return -1;
  }
  if (snprintf(uuids, uuids_size, "%s", str) >= uuids_size) {
    LOGGER(WARNING, "device uuids env %s truncated", MANAGER_VISIBLE_DEVICES_ENV);
    return -1;
  }
  return 0;
}

void value_enabled(char *str, int *i) {
  if (strcmp(str, "true") == 0 ||
      strcmp(str, "TRUE") == 0 ||
      strcmp(str,"1") == 0) {
    *i = 1;
  } else {
    *i = 0;
  }
}

/* Advise mode for oversold managed allocations. Unset leaves *out at the
 * caller's default.
 *
 * Parsed as an integer, NOT through value_enabled(): that helper maps anything
 * other than true/TRUE/1 to zero, so it would read "2" as OFF -- silently
 * disabling the very arm the caller asked for. "true"/"TRUE" still mean 1,
 * because this knob was boolean before it grew a second arm and a config
 * carrying the old spelling should not quietly turn into "off". */
int get_uva_advise(int *out) {
  char *str = _getenv(CUDA_MEM_UVA_ADVISE_ENV);
  if (!str || !*str) {
    return -1;
  }
  if (strcmp(str, "true") == 0 || strcmp(str, "TRUE") == 0) {
    *out = 1;
    return 0;
  }
  int mode = atoi(str);
  if (mode < 0) {
    mode = 0;
  } else if (mode > CUDA_MEM_UVA_ADVISE_MAX_MODE) {
    mode = CUDA_MEM_UVA_ADVISE_MAX_MODE;
  }
  *out = mode;
  return 0;
}

int get_vmem_node_enabled(int *i) {
  char *str = NULL;
  str = _getenv(VMEM_NODE_ENABLED_ENV);
  if (!str) {
    return -1;
  }
  value_enabled(str, i);
  return 0;
}

int get_mem_oversold(uint32_t index, int *i) {
  char env[32] = {0};
  snprintf(env, sizeof(env), "%s_%u", CUDA_MEM_OVERSOLD_ENV, index);
  char *str = _getenv(env);
  if (!str) {
    str = _getenv(CUDA_MEM_OVERSOLD_ENV);
    if (!str) {
      return -1;
    }
  }
  value_enabled(str, i);
  return 0;
}

int get_sm_watcher_enabled(int *i) {
  char *str = _getenv(EXTERNAL_SM_WATCHER_ENABLED_ENV);
  if (!str) {
    return -1;
  }
  value_enabled(str, i);
  return 0;
}

/* Returns 0 for the stock "delta" controller (default), 1 for "aimd",
 * 2 for the experimental "auto" mode (debounced sys_process_num-based
 * routing between delta and aimd). Anything else stays on delta. */
int get_sm_controller_kind(int *kind) {
  *kind = 0;
  char *str = _getenv(CUDA_SM_CONTROLLER_ENV);
  if (!str) return -1;
  if (strcasecmp(str, "aimd") == 0) {
    *kind = 1;
    return 0;
  }
  if (strcasecmp(str, "auto") == 0) {
    *kind = 2;
    return 0;
  }
  return 0;
}

/* Generic positive-int env getter with a fallback. Returns 0 if env was set
 * and parsed; -1 if env unset (caller keeps its default). Parse failure or a
 * non-positive value falls back to `dflt` and still returns 0 so the caller
 * sees a usable value rather than a silent zero. */
/* Positive double getter: accepts decimals like "1.5" so users can dial in
 * finer-grained MD softness than the integer-divisor flavour. The min
 * argument is a hard floor enforced after parsing; values <= min produce
 * a WARNING and fall back to dflt. Returns 0 if env was set and parsed
 * to a usable value (whether or not it had to fall back to dflt due to
 * range violation); -1 if env was unset (caller's dflt is used). */
static int get_positive_double_env(const char *name, double dflt, double min,
                                   double *out) {
  char *str = _getenv(name);
  if (!str || !*str) {
    *out = dflt;
    return -1;
  }
  char *endp = NULL;
  double v = strtod(str, &endp);
  if (endp == str || !(v > 0) || v < min || v != v /* NaN */) {
    LOGGER(WARNING, "%s=\"%s\" is not a positive double (>= %.3f), using default %.3f",
           name, str, min, dflt);
    *out = dflt;
    return 0;
  }
  *out = v;
  return 0;
}

/* Non-negative variant: accepts 0 (some knobs use 0 as the "disable" value).
 * Same parse + fallback semantics as get_positive_int_env otherwise. */
static int get_nonneg_int_env(const char *name, int dflt, int *out) {
  char *str = _getenv(name);
  if (!str || !*str) {
    *out = dflt;
    return -1;
  }
  char *endp = NULL;
  long v = strtol(str, &endp, 10);
  if (endp == str || v < 0 || v > INT_MAX) {
    LOGGER(WARNING, "%s=\"%s\" is not a non-negative int, using default %d",
           name, str, dflt);
    *out = dflt;
    return 0;
  }
  *out = (int)v;
  return 0;
}

/* Like the above but accepts ANY parseable int (including 0 and negatives);
 * only an unset/empty value or non-numeric garbage falls back to the default.
 * Used where a non-positive value is a meaningful sentinel (e.g. "disable"). */
static int get_int_env(const char *name, int dflt, int *out) {
  char *str = _getenv(name);
  if (!str || !*str) {            /* unset OR set-to-empty -> use default */
    *out = dflt;
    return -1;
  }
  char *endp = NULL;
  long v = strtol(str, &endp, 10);
  if (endp == str || v < INT_MIN || v > INT_MAX) {
    LOGGER(WARNING, "%s=\"%s\" is not an int, using default %d", name, str, dflt);
    *out = dflt;
    return 0;
  }
  *out = (int)v;
  return 0;
}

static int get_positive_int_env(const char *name, int dflt, int *out) {
  char *str = _getenv(name);
  if (!str || !*str) {            /* unset OR set-to-empty -> use default */
    *out = dflt;
    return -1;
  }
  char *endp = NULL;
  long v = strtol(str, &endp, 10);
  if (endp == str || v <= 0 || v > INT_MAX) {
    LOGGER(WARNING, "%s=\"%s\" is not a positive int, using default %d",
           name, str, dflt);
    *out = dflt;
    return 0;
  }
  *out = (int)v;
  return 0;
}

/* MD divisor is a double so users can dial in 1.5 or 2.5 for softer cuts
 * than integer 2/3. Floor 1.01 -- /1 is a no-op, /<1 would AMPLIFY share
 * on overshoot which inverts the algorithm. The 0.01 margin avoids
 * pathological behaviour from floating-point rounding near 1.0. */
int get_aimd_md_divisor(double *out) {
  return get_positive_double_env(CUDA_SM_AIMD_MD_DIVISOR_ENV, 3.0, 1.01, out);
}

/* Usage threshold (percent) for soft-mode up_limit periodic adjust. */
int get_usage_threshold(int *out) {
  return get_nonneg_int_env(CUDA_SM_USAGE_THRESHOLD_ENV, 5, out);
}

/* Effective-limit ratio expressed as parts-per-thousand (875 = 87.5%) so the
 * env can be a plain integer. Clamped to (0, 1000]. */
int get_aimd_eff_ratio(int *out) {
  int v = 875;
  int rc = get_positive_int_env(CUDA_SM_AIMD_EFF_RATIO_ENV, 875, &v);
  if (v > 1000) {
    LOGGER(WARNING, "%s=%d > 1000, clamped to 1000",
           CUDA_SM_AIMD_EFF_RATIO_ENV, v);
    v = 1000;
  }
  *out = v;
  return rc;
}

int get_aimd_ai_base_div(int *out) {
  return get_positive_int_env(CUDA_SM_AIMD_AI_BASE_DIV_ENV, 400, out);
}

/* Number of consecutive observed-different watcher cycles required before
 * the "auto" mode flips between delta and aimd. Default 10 ~ 800ms, which
 * absorbs multi-cycle NVML jitter in sys_process_num (the extra margin over
 * the old 5 further suppresses exclusivity flapping) without adding meaningful
 * Pod start/exit latency (Pod start dominates anyway). */
int get_sm_auto_debounce_cycles(int *out) {
  return get_positive_int_env(CUDA_SM_AUTO_DEBOUNCE_CYCLES_ENV, 10, out);
}

/* External-utilization threshold (percent) for the "is_exclusive" predicate
 * shared by AUTO routing and the watcher's soft_core burst path. Default 1:
 * filters out always-on driver / persistenced threads (which sit at ~0%
 * SM) while still recognising the smallest real Pod activity. Caller can
 * raise it (env) if their host has other low-utilization tools touching
 * the GPU that should be ignored. Clamped >= 1 by get_positive_int_env. */
int get_sm_auto_external_util_threshold(int *out) {
  return get_positive_int_env(CUDA_SM_AUTO_EXTERNAL_UTIL_THRESHOLD_ENV, 1, out);
}

/* delta() ramp-floor divisor. Accepts any int: a value <= 0 is the explicit
 * "disable the ramp floor" sentinel (delta reverts to its raw pool-relative
 * step); > 0 is the active divisor. Unset -> default 64.
 * delta() guards the division on divisor > 0, so a non-positive value is safe. */
int get_delta_ramp_floor_divisor(int *out) {
  return get_int_env(CUDA_SM_DELTA_RAMP_FLOOR_DIVISOR_ENV, 64, out);
}

/* delta() pool-relative increment divisor R (see the env comment above and
 * include/sm_delta.h). Positive only -- 0 would divide by zero and a negative
 * value would invert the controller; bad values fall back to the default. */
int get_delta_increment_divisor(int *out) {
  return get_positive_int_env(CUDA_SM_DELTA_INCREMENT_DIVISOR_ENV, DELTA_INCREMENT_DIVISOR_DEFAULT, out);
}

/* AIMD deadband lower edge / 1000. Caller MUST additionally check
 * deadband_ratio < eff_ratio to keep AI/deadband/MD regions well-ordered. */
int get_aimd_deadband_ratio(int *out) {
  return get_positive_int_env(CUDA_SM_AIMD_DEADBAND_RATIO_ENV, 800, out);
}

/* AIMD post-MD cooldown in watcher cycles. 0 = disabled (V2.1 behaviour). */
int get_aimd_md_cooldown_cycles(int *out) {
  return get_nonneg_int_env(CUDA_SM_AIMD_MD_COOLDOWN_CYCLES_ENV, 3, out);
}

/* Container-wide shared token bucket on/off. Same true/TRUE/1 spelling as the
 * other boolean envs. Unset leaves *out untouched at the caller's default,
 * which is ON -- this is an opt-OUT knob. */
int get_sm_shared_bucket(int *out) {
  char *str = _getenv(CUDA_SM_SHARED_BUCKET_ENV);
  if (!str) {
    return -1;
  }
  value_enabled(str, out);
  return 0;
}

static int compare_pids(const void *a, const void *b) {
  int pid1 = *(const int *)a;
  int pid2 = *(const int *)b;
  return (pid1 > pid2) - (pid1 < pid2);
}

/* Bounded wait for the shared lock on pids.config. The manager rewrites
 * this file in place under LOCK_EX once per CUDA process at registration;
 * reading without the matching LOCK_SH could land mid-rewrite.
 *
 * Budget kept deliberately tiny: the writer's critical section is a
 * microsecond-scale ftruncate+pwrite, so contention outlasting the first
 * few yields means something is wedged, not merely busy -- and the caller
 * runs under lock_gpu_device (whose own critical section is ~1-3ms, held
 * TWICE per call), so time spent here is spent twice and queues every
 * other process waiting on that device. LOCK_NB throughout, never a
 * blocking LOCK_SH, so a stalled writer can't wedge CUDA calls behind it.
 *
 * Giving up and reading anyway is safe: no writer truncates before writing,
 * so an unlocked reader sees a complete list, at worst with dead trailing
 * PIDs from a previous, longer-lived registration that match nothing on the
 * device.
 *
 * That is a contract on the writers, not an observation about them, and it
 * has two parties: the manager at container registration, and the checkpoint
 * provider on every session connect/disconnect (write_pids in
 * checkpoint_provider.c, which writes the whole list then shrinks the file
 * for exactly this reason). Any third writer must do the same -- a reader
 * that sees a short list under-counts used memory, and the accounting path
 * treats an empty one as "never registered" and aborts the process.
 * test/nogpu/test_session_pids_concurrency.c holds the provider to it. */
#define PIDS_CONFIG_LOCK_YIELDS 4
#define PIDS_CONFIG_LOCK_SLEEPS 5
#define PIDS_CONFIG_LOCK_BACKOFF_NS (200 * 1000L) /* 200us, so ~1ms worst case */

static int lock_pids_config_shared(int fd) {
  struct timespec backoff = {.tv_sec = 0, .tv_nsec = PIDS_CONFIG_LOCK_BACKOFF_NS};
  int attempts = PIDS_CONFIG_LOCK_YIELDS + PIDS_CONFIG_LOCK_SLEEPS;
  int reason = 0;

  for (int attempt = 0; attempt < attempts; attempt++) {
    if (flock(fd, LOCK_SH | LOCK_NB) == 0) {
      return 0;
    }
    /* Captured now: sched_yield() and nanosleep() below can overwrite errno,
     * and the caller logs this to say why it is reading unlocked. */
    reason = errno;
    if (reason == EINTR) {
      continue;
    }
    /* EWOULDBLOCK (== EAGAIN) is the writer holding LOCK_EX: retry. Anything
     * else -- a filesystem without flock support, a bad descriptor -- does not
     * get better by waiting. */
    if (reason != EWOULDBLOCK) {
      break;
    }
    if (attempt < PIDS_CONFIG_LOCK_YIELDS) {
      /* Cheaper than any timer and usually enough: the writer only has to be
       * scheduled long enough to finish one pwrite. */
      sched_yield();
    } else {
      nanosleep(&backoff, NULL);
    }
  }
  errno = reason;
  return -1;
}

/* Copies line[start,end) into out, trimming leading blanks and the same
 * trailing junk this parser has always trimmed. Returns -1 when the field
 * would not fit, so no caller ever compares a silently truncated path. */
static int copy_trimmed_field(const char *line, size_t start, size_t end,
                              char *out, size_t out_size) {
  if (out_size == 0) {
    return -1;
  }
  for (; start < end && (line[start] == ' ' || line[start] == '\t'); start++) {}
  size_t len = end - start;
  if (len >= out_size) {
    return -1;
  }
  memcpy(out, line + start, len);
  for (; len > 0 && (out[len - 1] == '\0' || out[len - 1] == '\n' ||
                     out[len - 1] == '\t'); len--) {}
  out[len] = '\0';
  return 0;
}

/* Splits "<key><d><value>" out of line. Both out-params carry their own size:
 * these used to be written with no bound at all, and a real container cgroup
 * path (191 bytes) overflowed the caller's 128-byte buffers by 64 bytes.
 * Returns 0 on success, 1 when the delimiter is absent, -1 on overflow. */
int split_str(const char *line, char *key, size_t key_size,
              char *value, size_t value_size, char d) {
  size_t len = strlen(line);
  size_t index = 0;
  for (; index < len && line[index] != d; index++) {}

  if (index == len) {
    if (key_size) key[0] = '\0';
    if (value_size) value[0] = '\0';
    return 1;
  }

  if (copy_trimmed_field(line, 0, index, key, key_size) != 0 ||
      copy_trimmed_field(line, index + 1, len, value, value_size) != 0) {
    if (key_size) key[0] = '\0';
    if (value_size) value[0] = '\0';
    return -1;
  }
  return 0;
}

/*
 * Reads the cgroup path for cgroup_key from an open /proc/<pid>/cgroup.
 *
 *   line: <hierarchy-id>:<controller-list>:<cgroup-path>
 *
 *   cgroup_key == ""  -> controller-list must be empty (v2 "0::/path")
 *   cgroup_key != ""  -> key must be one ','-separated member of the list
 *
 * Returns 0 on match (cgroup_value = path, NUL-terminated, truncated to
 * value_size), -1 otherwise.  cgroup_value is written only on success.
 *
 * Rewinds first, so the search always covers the whole file. Callers hand the
 * same FILE* in twice -- once per cgroup version they try -- and a miss on the
 * first key leaves the stream at EOF, which would make every later key fail
 * for the position rather than for the contents. On a cgroup v1 host that is
 * exactly the sequence that happens: the empty (v2) key matches nothing, and
 * "memory" would then be asked of a stream with nothing left to read.
 */
int read_cgroup(FILE *fp, const char *cgroup_key, char *cgroup_value, size_t value_size) {
  if (fp == NULL || cgroup_key == NULL || cgroup_value == NULL || value_size == 0) {
    return -1;
  }
  rewind(fp);

  char buff[CGROUP_LINE_MAX];
  const size_t key_len = strlen(cgroup_key);

  while (fgets(buff, sizeof(buff), fp)) {
    char *first = strchr(buff, ':');
    if (first == NULL) continue;
    char *second = strchr(first + 1, ':');
    if (second == NULL) continue;

    if (key_len == 0) {
      if (second != first + 1) continue;
    } else {
      int matched = 0;
      const char *p = first + 1, *end = second;
      while (p < end) {
        const char *comma = memchr(p, ',', (size_t)(end - p));
        const char *mend = comma ? comma : end;
        if ((size_t)(mend - p) == key_len &&
            memcmp(p, cgroup_key, key_len) == 0) {
          matched = 1;
          break;
        }
        p = comma ? comma + 1 : end;
      }
      if (!matched) continue;
    }

    const char *path = second + 1;
    size_t path_len = strcspn(path, "\r\n");
    if (path_len >= value_size) {
      /* Fail rather than truncate. The caller prefix-matches this against
       * another process's path, and a truncated path is a prefix of OTHER
       * cgroups too: cut "/kubepods/pod-A/container-1" at a '/' and it also
       * prefixes "/kubepods/pod-A/container-2", so a sibling container's
       * process would be claimed as ours and over-counted. A caller that gets
       * no answer treats the pid as undecidable, which is the safe direction.
       * A stock Kubernetes path is ~191 bytes, which is why the buffers here
       * are CGROUP_LINE_MAX and not something smaller. */
      continue;
    }
    memcpy(cgroup_value, path, path_len);
    cgroup_value[path_len] = '\0';
    return 0;
  }
  return -1;
}

int get_container_pids_by_filepath(const char *file_path, int *pids, int *pids_size, int sort_pids) {
  if (!file_path || !pids || !pids_size) {
    LOGGER(ERROR, "invalid NULL parameter");
    if (pids_size) *pids_size = 0;
    return -1;
  }

  /* No access(F_OK) pre-check: it only duplicated what fopen already reports,
   * and the gap between the two was a time-of-check/time-of-use window. A
   * missing file stays quiet, as it did when access() screened it out. */
  /* "e" = O_CLOEXEC, prevent fork inheritance */
  FILE *fp = fopen(file_path, "re");
  if (!fp) {
    if (errno != ENOENT) {
      LOGGER(WARNING, "error opening %s: %s", file_path, strerror(errno));
    }
    *pids_size = 0;
    return -1;
  }

  int lock_fd = fileno(fp);
  int locked = (lock_pids_config_shared(lock_fd) == 0);
  if (!locked) {
    LOGGER(VERBOSE, "reading %s without a shared lock: %s", file_path, strerror(errno));
  }

  int max_size = *pids_size;
  int actual_count = 0;
  char line[MAX_PID_STR_LEN];

  while (fgets(line, sizeof(line), fp) && actual_count < max_size) {
    char *endptr;
    long pid = strtol(line, &endptr, 10);

    if (endptr == line || (*endptr != '\n' && *endptr != '\0')) {
      LOGGER(ERROR, "invalid PID format: %s", line);
      continue;
    }
    if (pid <= 0 || pid > INT_MAX) {
      continue;
    }
    pids[actual_count++] = (int)pid;
  }

  if (sort_pids && actual_count > 0) {
    qsort(pids, (size_t)actual_count, sizeof(int), compare_pids);
  }

  *pids_size = actual_count;

  if (!feof(fp) && actual_count >= max_size) {
    LOGGER(WARNING, "PID array full, only stored %d PIDs from %s", max_size, file_path);
  }

  if (locked) {
    flock(lock_fd, LOCK_UN);
  }
  fclose(fp);
  return 0;
}

/* Every NVIDIA device node a GPU process can hold open shares this prefix:
 * nvidia0..N, nvidiactl, nvidia-uvm, nvidia-uvm-tools and nvidia-caps entries.
 * Anchored at the start rather than searched for anywhere in the target so a
 * path that merely CONTAINS the word -- /home/user/dev/nvidia-notes/data.bin
 * -- cannot match. */
#define GPU_DEV_PREFIX     "/dev/nvidia"
#define GPU_DEV_PREFIX_LEN (sizeof(GPU_DEV_PREFIX) - 1)

/* Whether the process holds an open descriptor on an NVIDIA device node.
 *
 * This is the ONLY test for "is this pid using the GPU". It replaced a scan of
 * /proc/<pid>/maps that matched "nvidia" anywhere in a mapping's path; that
 * scan was kept as a fallback for a while and is now gone. Two reasons.
 *
 * COST. Generating /proc/<pid>/maps makes the kernel walk the target's whole
 * VMA tree holding its mmap_lock, so that read costs what the target's address
 * space is shaped like, not what we are looking for -- and it contends for the
 * very lock the target needs in order to allocate. Measured in this repo's
 * devcontainer: 2.1ms at 1k VMAs, 44ms at 20k, 135ms at 60k, and a PyTorch
 * process with expandable_segments:True maps and unmaps constantly, so tens of
 * thousands of VMAs is ordinary rather than pathological. This runs inside the
 * per-device lock on the allocation path (load_limited_memory_view), whose spin
 * parameters are sized for the "~1-3ms" critical section lock.c documents --
 * two orders of magnitude out, and overshooting LOCK_TIMEOUT_MS there makes
 * lock_gpu_device() return -1 and every caller carry on UNLOCKED.
 *
 * Walking the fd table instead costs what the FD TABLE is shaped like, and
 * nothing at all for the address space: measured flat across 0 and 20k VMAs
 * (65.5us vs 66.0us for the same 45 descriptors). What it does scale with is
 * descriptor count, at roughly 1.5us each (readdir plus one readlinkat), and
 * whether the walk can stop early -- a hit breaks out, so a GPU process whose
 * device nodes sit in the low descriptors (they are opened by cuInit) answers
 * in about 20us, while a full scan of 45 descriptors costs 66us and of 200
 * costs 281us. Either way it is two to three orders of magnitude below the
 * maps scan it replaces.
 *
 * CORRECTNESS. The descriptor is also the more fundamental fact. The CUDA
 * driver speaks to /dev/nvidiactl and /dev/nvidia<N> by ioctl and holds those
 * descriptors for as long as it holds a context; the mappings that turn up in
 * maps are built ON those descriptors. So a GPU process has the descriptor
 * whether or not it has the mapping, but not the other way round. Matching the
 * link target also cannot be fooled the way searching maps for "nvidia"
 * anywhere in a path can be.
 *
 * Deliberately NOT enumerating /dev and comparing st_rdev: that list would have
 * to be kept in step with the driver, and missing a node it later adds means
 * under-counting memory, which is a quota escape. A prefix covers nodes that do
 * not exist yet.
 *
 * No cap on how many descriptors get examined. Linux does walk /proc/<pid>/fd
 * in ascending descriptor order, so a cap WOULD be meaningful here -- but it is
 * not worth a tunable. A hit stops the walk, and a hit is the normal case,
 * because this is only reached once the pid has already been confirmed to be in
 * our container. A miss reads the whole table, which is why a process holding
 * ten thousand descriptors costs milliseconds here, but a miss means the pid is
 * in our container and is NOT on the GPU -- NVML named a pid from another
 * namespace that collided with one of ours -- which is rare. Capping the walk
 * would trade that rare cost for a silent under-count whenever a process opened
 * enough descriptors before cuInit, and under-counting is a quota escape; the
 * maps fallback below already bounds the damage of a miss without inventing a
 * limit to tune.
 *
 * Returns 0 when such a descriptor is held, -1 otherwise. -1 also covers
 * "cannot look": /proc/<pid>/fd needs PTRACE_MODE_READ just as
 * /proc/<pid>/maps does, so a process owned by another uid is unreadable here
 * too and this is no worse than what it replaces.
 */
int gpu_device_fd_in_process(unsigned int pid) {
  char dir_path[64];
  snprintf(dir_path, sizeof(dir_path), "/proc/%u/fd", pid);

  /* No explicit O_CLOEXEC counterpart to the "e" this file passes fopen()
   * elsewhere: POSIX.1-2008 requires opendir() to set FD_CLOEXEC on the
   * descriptor it returns, and glibc does (verified: F_GETFD reports
   * FD_CLOEXEC on 2.39). Saying so here because the fopen() call sites
   * deliberately spell it out, and a reader would otherwise read this as the
   * one that forgot. */
  DIR *dir = opendir(dir_path);
  if (dir == NULL) {
    return -1;
  }

  int ret = -1;
  struct dirent *entry;
  while ((entry = readdir(dir)) != NULL) {
    /* Every real entry is a descriptor number, so this also drops "." and
     * ".." without a strcmp. */
    if (entry->d_name[0] < '0' || entry->d_name[0] > '9') {
      continue;
    }
    /* readlinkat against the open directory rather than a rebuilt path: no
     * second snprintf, and nothing for -Wformat-truncation to worry about.
     *
     * Only as much of the target as the prefix needs is read. readlink neither
     * NUL-terminates nor reports truncation, and both are fine when the
     * question is just about the leading GPU_DEV_PREFIX_LEN bytes -- a shorter
     * target cannot match the prefix, so a short read is already an answer. */
    char target[GPU_DEV_PREFIX_LEN];
    ssize_t len = readlinkat(dirfd(dir), entry->d_name, target, sizeof(target));
    /* A descriptor closed between readdir and readlinkat is ordinary here, not
     * a condition worth logging. */
    if (len < (ssize_t)GPU_DEV_PREFIX_LEN) {
      continue;
    }
    if (memcmp(target, GPU_DEV_PREFIX, GPU_DEV_PREFIX_LEN) == 0) {
      ret = 0;
      break;
    }
  }

  closedir(dir);
  return ret;
}

/* The namespace types that decide "same container", and one cache slot each.
 *
 * At file scope so self_ns_inode()'s slot count is bound to this array at
 * compile time rather than by hand: a third entry added here without widening
 * the caches below would otherwise write past them.
 *
 * "mnt" MUST stay first. On a kernel without cgroup namespaces the loop below
 * runs over the first entry only, and mnt is the one test that stands alone
 * (see device_pid_in_same_container). */
static const char *const g_ns_names[] = {"mnt", "cgroup"};
#define NS_SLOT_COUNT (sizeof(g_ns_names) / sizeof(g_ns_names[0]))
_Static_assert(NS_SLOT_COUNT >= 1, "device_pid_in_same_container needs at least the mnt leg");

/* Whether this kernel has cgroup namespaces at all.
 *
 * Resolved once and cached, because it is a property of the kernel and cannot
 * change under a running process. Values: 0 = not probed yet, 1 = present,
 * 2 = known absent.
 *
 * This exists because cgroup namespaces were only added in Linux 4.6. On an
 * older kernel /proc/<pid>/ns/cgroup simply does not exist, and the loop in
 * device_pid_in_same_container() used to stat it unconditionally -- the stat
 * failed, the function returned "not ours", and it did so for EVERY pid,
 * INCLUDING THE CALLER'S OWN. The whole open-kernel identification path then
 * silently reported zero GPU memory in use, which turns the budget check into
 * a no-op and hands the container the entire card. Verified on
 * 3.10.0-1160.el7: /proc/self/ns holds only ipc, mnt, net, pid, user, uts.
 *
 * ONLY ENOENT is treated as "this kernel has none". Any other failure is left
 * unprobed so the next call retries, and is reported as "present" so the
 * comparison still tries the cgroup leg and fails closed on it -- i.e. exactly
 * the behaviour this function replaced. Caching an arbitrary errno as "absent"
 * would permanently narrow the test for the rest of the process's life on the
 * strength of one transient failure, which is the silent degradation this
 * whole change exists to remove.
 *
 * Not reset after fork: a child inherits the same kernel.
 *
 * Returns 0 when the cgroup leg should be attempted, -1 when it must be
 * skipped.
 */
static unsigned char g_cgroup_ns_state;

static int cgroup_ns_available(void) {
  unsigned char state = __atomic_load_n(&g_cgroup_ns_state, __ATOMIC_RELAXED);

  if (unlikely(state == 0)) {
    struct stat st;
    if (stat(PID_SELF_NS_PATH "/cgroup", &st) == 0) {
      state = 1;
    } else if (errno == ENOENT) {
      state = 2;
    } else {
      /* Transient: do not cache, do not narrow the test. */
      return 0;
    }
    __atomic_store_n(&g_cgroup_ns_state, state, __ATOMIC_RELAXED);
    if (state == 2) {
      /* A fixed property of the kernel, so a warning rather than a refusal to
       * serve: there is nothing to retry and nothing the operator can change
       * at this point, and mnt alone still answers the question this function
       * is asked (see device_pid_in_same_container). Once per process -- the
       * exchange keeps a racing pair of threads to a single line. */
      static unsigned char warned;
      if (!__atomic_exchange_n(&warned, 1, __ATOMIC_RELAXED)) {
        LOGGER(WARNING, "%s/cgroup is absent (cgroup namespaces need Linux 4.6+); "
                        "matching container PIDs on the mount namespace alone",
               PID_SELF_NS_PATH);
      }
    }
  }
  return state == 2 ? -1 : 0;
}

/* The inode of one of our own namespaces, resolved once per type.
 *
 * Our own namespace inodes cannot change under us: nothing in this library
 * calls unshare() or setns(), and a process cannot be moved into another
 * namespace from the outside. So re-stat()ing /proc/self/ns/<type> for every
 * pid on the device was two syscalls per pid of pure waste -- measured at
 * 4.4us of the 7.0us this comparison cost, i.e. most of it.
 *
 * Not invalidated after fork: a child inherits our namespaces, so the inode it
 * would read is the one already cached.
 *
 * A failure is deliberately NOT cached. It is far more likely to be transient
 * (descriptors exhausted) than a permanent property of the system, and caching
 * it would silently disable this comparison for the rest of the process's life
 * -- which means claiming every pid is "not ours" and under-counting memory.
 *
 * Two threads arriving together both stat and both store the same inode, which
 * is harmless but still a data race unless the stores are atomic, so they are.
 * The flag carries release/acquire so a reader that sees it set is guaranteed
 * to see the inode too; the inode itself only needs relaxed ordering on top of
 * that.
 *
 * Returns 0 and writes *ino on success, -1 otherwise.
 */
static int self_ns_inode(const char *ns_type, unsigned slot, ino_t *ino) {
  static ino_t cached[NS_SLOT_COUNT];
  static unsigned char ready[NS_SLOT_COUNT];

  if (slot >= NS_SLOT_COUNT) {
    return -1;
  }
  if (__atomic_load_n(&ready[slot], __ATOMIC_ACQUIRE)) {
    *ino = __atomic_load_n(&cached[slot], __ATOMIC_RELAXED);
    return 0;
  }

  char self_path[128];
  struct stat self_st;
  snprintf(self_path, sizeof(self_path), "%s/%s", PID_SELF_NS_PATH, ns_type);
  if (stat(self_path, &self_st) != 0) {
    return -1;
  }
  __atomic_store_n(&cached[slot], self_st.st_ino, __ATOMIC_RELAXED);
  __atomic_store_n(&ready[slot], 1, __ATOMIC_RELEASE);
  *ino = self_st.st_ino;
  return 0;
}

/* Report that we were not allowed to inspect a pid, which is NOT the same
 * thing as that pid not being ours.
 *
 * /proc/<pid>/ns entries need PTRACE_MODE_READ, so a process owned by another uid
 * is unreadable from here and device_pid_in_same_container() has to answer
 * "not ours". Two cases hide behind that one answer, and they are opposites:
 *
 *   ENOENT -- the pid exited between NVML sampling it and us reading it. The
 *             common case on this path, and correctly "not ours".
 *   EACCES/EPERM -- the pid is real and might well be ours, but it runs as a
 *             different uid (runAsUser, a deliberately de-privileged child, a
 *             sidecar) and we may not look. Answering "not ours" then
 *             UNDER-COUNTS its memory, which is the quota-escape direction.
 *
 * The denial is REPORTED here and resolved by the caller, not decided here:
 * this file deliberately knows nothing about compatibility modes, and the
 * fallback that can still settle ownership -- the cgroup check -- lives with
 * them. What must never happen is a denial silently reading as "not ours":
 * that under-counts a process that IS ours, which is the quota-escape
 * direction. Nothing can remove the ambiguity from here; it needs
 * CAP_SYS_PTRACE, which is not ours to grant.
 *
 * Rate-limited on powers of two rather than logged once: the count is the
 * interesting part. One denial in a whole run is a transient to ignore; a
 * count that climbs with every allocation says the deployment really does run
 * GPU processes under another uid. This runs per-pid inside the device lock,
 * so an unthrottled line here would be one per pid per allocation.
 */
static void note_pid_inspect_denied(unsigned int pid, const char *ns_type, int err) {
  static volatile uint64_t denied_total;
  uint64_t total = __sync_add_and_fetch(&denied_total, 1);

  if ((total & (total - 1)) == 0) {
    LOGGER(WARNING, "not permitted to inspect pid %u (/proc/%u/ns/%s: %s); ownership cannot be "
                    "confirmed from the namespaces, so the caller decides -- reported because a "
                    "denial must not be mistaken for a pid that simply exited. "
                    "Denials so far: %" PRIu64,
           pid, pid, ns_type, strerror(err), total);
  }
}

/* Three-valued; the caller distinguishes all three (see hook.h for the names).
 *
 *   YES     confirmed ours -- the namespace inodes match.
 *   UNKNOWN the pid exists but we may not inspect it (another uid). The caller
 *           resolves this; it is NOT "not ours".
 *   NO      confirmed not ours -- inodes differ, or the pid is gone.
 *
 * Why UNKNOWN is a separate answer: from CUDA 13.x the driver lists only the
 * pids of the caller's own PID namespace, so for an ordinary container every
 * pid NVML hands us is already one of ours and this function is a confirmation
 * rather than a filter. Folding a permission denial into "not ours" would
 * under-count a process that is ours, and under-counting is the quota-escape
 * direction. What a denial means instead is decided by
 * check_device_pid_in_local_container_pid(), which knows the compatibility
 * mode and can fall back to the cgroup check.
 */
int device_pid_in_same_container(unsigned int pid) {
  /* The mount namespace is the load-bearing test, and on its own it already
   * answers both threats this function exists for: with hostPID=true we can
   * see the host's processes, which have a different rootfs; with
   * shareProcessNamespace=true we can see a sibling container's, which also
   * has its own rootfs. Either way the mnt inode differs and the pid is
   * correctly rejected.
   *
   * The cgroup namespace is kept as a second test where the kernel has one,
   * but it is worth being clear that it adds less than it looks: it
   * discriminates only under cgroupns=private (containerd's default on cgroup
   * v2, one namespace per container). Under cgroupns=host every container
   * shares the host's cgroup namespace, so the inodes match for everyone and
   * the test is satisfied by processes that are not ours at all. It narrows
   * the answer when it can and never widens it, which is why it stays. */
  unsigned count = (cgroup_ns_available() == 0) ? NS_SLOT_COUNT : 1;

  for (unsigned i = 0; i < count; i++) {
    char device_path[128];
    struct stat device_st;
    snprintf(device_path, sizeof(device_path), "/proc/%u/ns/%s", pid, g_ns_names[i]);
    if (stat(device_path, &device_st) != 0) {
      /* A permission denial means "cannot tell", which the caller resolves.
       * Anything else -- ENOENT above all, the pid having exited between NVML
       * sampling it and now -- is genuinely not ours. */
      if (unlikely(errno == EACCES || errno == EPERM)) {
        note_pid_inspect_denied(pid, g_ns_names[i], errno);
        return PID_IN_CONTAINER_UNKNOWN;
      }
      return PID_IN_CONTAINER_NO;
    }
    ino_t self_ino;
    if (self_ns_inode(g_ns_names[i], i, &self_ino) != 0) {
      /* Our OWN namespace is unreadable, so there is nothing to compare
       * against. Not a statement about the target pid, so stay conservative;
       * self_ns_inode() does not cache the failure, so the next call retries. */
      return PID_IN_CONTAINER_NO;
    }
    if (device_st.st_ino != self_ino) {
      return PID_IN_CONTAINER_NO;
    }
  }
  return PID_IN_CONTAINER_YES;
}

/* Widens a shared-state file this library just created, so that another uid
 * sharing the directory can still open it O_RDWR. Two reasons this is an
 * fchmod and not a mode argument to open(): the mode passed to open() is
 * masked by the process umask, so a hardened image's umask 077 would quietly
 * produce 0600 whatever we asked for; and fchmod succeeds only for the file's
 * owner, which makes this exactly the best-effort hardening it should be --
 * when the file was created by a different uid there is nothing we may change
 * and nothing we need to, and the caller is no worse off than before.
 *
 * Deliberately not reported as a failure: the mode is a courtesy to whoever
 * comes next, never a precondition for the caller's own use of the fd. */
void relax_shared_file_mode(int fd, const char *file_path) {
  if (fchmod(fd, VGPU_SHARED_FILE_MODE) != 0) {
    LOGGER(VERBOSE, "could not relax the mode of %s: %s -- another uid sharing "
                    "this directory may be unable to open it read-write",
           file_path, strerror(errno));
  }
}

/* The directory counterpart, for the directories the library creates itself
 * when the plugin did not provide the mount. Same best-effort contract.
 *
 * Opened and fchmod'ed rather than chmod'ed by path, with O_NOFOLLOW: these
 * directories sit in world-writable parents, which is the one place a
 * path-based chmod is the wrong tool -- a directory swapped for a symlink
 * under us is refused outright instead of followed. */
void relax_shared_dir_mode(const char *dir_path) {
  int fd = open(dir_path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
  if (fd < 0) {
    LOGGER(VERBOSE, "could not open %s to relax its mode: %s",
           dir_path, strerror(errno));
    return;
  }
  if (fchmod(fd, VGPU_SHARED_DIR_MODE) != 0) {
    LOGGER(VERBOSE, "could not relax the mode of %s: %s -- another uid sharing "
                    "it may be unable to create files in it",
           dir_path, strerror(errno));
  }
  close(fd);
}

/* As relax_shared_file_mode(), but at most once per process for a given slot.
 * The device lock paths reopen the same file on every allocation, and an
 * unconditional fchmod there would undo the syscall economy try_lock_fd() is
 * built around. Only whoever created the file can change its mode anyway, so
 * every later open has nothing to do. The exchange means exactly one caller
 * attempts it even if several threads arrive together. */
void relax_shared_file_mode_once(int fd, const char *file_path,
                                 unsigned char *slot) {
  if (unlikely(!__atomic_exchange_n(slot, 1, __ATOMIC_RELAXED))) {
    relax_shared_file_mode(fd, file_path);
  }
}

int file_exist(const char *file_path) {
  return (access(file_path, F_OK) == 0) ? 0 : -1;
}

int pid_exist(int pid) {
  if (pid <= 0) return -1;
  return (kill(pid, 0) == 0 ? 0 : (errno == ESRCH ? -1 : 0));
}

// 1: is zombie
// 0: not zombie
// -1: error
int is_zombie_proc(int pid) {
  if (pid <= 0) return -1;
  char path[64];
  snprintf(path, sizeof(path), "/proc/%d/stat", pid);

  FILE *fp = fopen(path, "re");  /* "e" = O_CLOEXEC, prevent fork inheritance */
  if (fp == NULL) return -1;

  int unused_pid;
  char comm[1024];
  char state;
  /* Field width bounds comm to the buffer: /proc/<pid>/stat is kernel-written,
   * but an unbounded %s is one driver change away from a stack overflow. */
  int ret = fscanf(fp, "%d %1023s %c", &unused_pid, comm, &state);
  fclose(fp);

  if (ret != 3 || ret == EOF) {
    return -1;
  } else if (state == 'Z' || state == 'z') {
    return 1;
  }
  return 0;
}
