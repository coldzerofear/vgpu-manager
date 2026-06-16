#include "include/hook.h"
#include "include/metrics.h"

static volatile uint64_t g_lock_wait_ns[MAX_DEVICE_COUNT] = {0};
static volatile uint64_t g_lock_success_total[MAX_DEVICE_COUNT] = {0};
static volatile uint64_t g_lock_timeout_total[MAX_DEVICE_COUNT] = {0};

static volatile uint64_t g_uva_fallback_total[MAX_DEVICE_COUNT] = {0};
static volatile uint64_t g_kernel_rate_limit_hit_total[MAX_DEVICE_COUNT] = {0};
static volatile uint64_t g_watcher_miss_total[MAX_DEVICE_COUNT] = {0};
static volatile uint64_t g_nvml_fallback_total[MAX_DEVICE_COUNT] = {0};
static volatile uint64_t g_oom_total_limit_total[MAX_DEVICE_COUNT] = {0};
static volatile uint64_t g_oom_driver_return_total[MAX_DEVICE_COUNT] = {0};

/* GAP-path SM-throttle observability. count = number of GAP-path entries
 * (gap_begin returned 1 and gap_end ran); sleep_us_total = cumulative host
 * sleep injected to hold the duty cycle. */
static volatile uint64_t g_gap_throttle_total[MAX_DEVICE_COUNT] = {0};
static volatile uint64_t g_gap_sleep_us_total[MAX_DEVICE_COUNT] = {0};

/* V2.1 + P1 observability counters. Together they let an operator confirm
 * the new anti-sawtooth + exclusivity-FSM machinery is actually firing in
 * production -- without these, those code paths are invisible from logs. */
static volatile uint64_t g_exclusivity_flip_gained_total[MAX_DEVICE_COUNT] = {0};
static volatile uint64_t g_exclusivity_flip_lost_total[MAX_DEVICE_COUNT]   = {0};
static volatile uint64_t g_aimd_md_total[MAX_DEVICE_COUNT]                 = {0};
static volatile uint64_t g_aimd_md_blocked_total[MAX_DEVICE_COUNT]         = {0};
static volatile uint64_t g_aimd_deadband_hit_total[MAX_DEVICE_COUNT]       = {0};

/* SM controller label included in rate_limit_hit emissions. Set once at
 * init by sm_controller_init() in cuda_hook.c; "delta" is the safe default
 * if init is delayed (counter still increments, label is just the default). */
static const char *g_sm_controller_label = "delta";

void metrics_set_controller_label(const char *name) {
  if (name && *name) g_sm_controller_label = name;
}

static int is_valid_metric_index(int index) {
  return index >= 0 && index < MAX_DEVICE_COUNT;
}

static int should_collect_metrics(void) {
  return LOGGER_SHOULD_PRINT(INFO);
}

static void maybe_log_counter_metric(const char *name, int host_index,
                                     volatile uint64_t *counter) {
  uint64_t total;

  if (!should_collect_metrics() || !is_valid_metric_index(host_index)) {
    return;
  }

  total = __sync_add_and_fetch(counter, 1);
  if ((total & (total - 1)) == 0) {
    LOGGER(INFO, "metric=%s host_device=%d total=%" PRIu64,
           name, host_index, total);
  }
}

void metrics_record_lock_wait(int device_index, uint64_t wait_ns, int timeout) {
  uint64_t total;

  if (!should_collect_metrics() || !is_valid_metric_index(device_index)) {
    return;
  }

  total = timeout ?
      __sync_add_and_fetch(&g_lock_timeout_total[device_index], 1) :
      __sync_add_and_fetch(&g_lock_success_total[device_index], 1);
  if (!timeout) {
    __sync_add_and_fetch(&g_lock_wait_ns[device_index], wait_ns);
  }
  if ((total & (total - 1)) == 0) {
    LOGGER(INFO,
           "gpu lock metrics device=%d success=%" PRIu64 " timeout=%" PRIu64 " wait_ns=%" PRIu64,
           device_index,
           (uint64_t)g_lock_success_total[device_index],
           (uint64_t)g_lock_timeout_total[device_index],
           (uint64_t)g_lock_wait_ns[device_index]);
  }
}

void metrics_record_oom(int host_index, metrics_oom_reason_t reason) {
  if (!should_collect_metrics() || !is_valid_metric_index(host_index)) {
    return;
  }
  if (reason == METRICS_OOM_TOTAL_LIMIT) {
    maybe_log_counter_metric("oom_total_limit", host_index,
                             &g_oom_total_limit_total[host_index]);
    return;
  }
  if (reason == METRICS_OOM_DRIVER_RETURN) {
    maybe_log_counter_metric("oom_driver_return", host_index,
                             &g_oom_driver_return_total[host_index]);
    return;
  }
}

void metrics_record_uva_fallback(int host_index) {
  maybe_log_counter_metric("uva_fallback", host_index,
                           &g_uva_fallback_total[host_index]);
}

void metrics_record_rate_limit_hit(int host_index) {
  uint64_t total;
  if (!should_collect_metrics() || !is_valid_metric_index(host_index)) {
    return;
  }
  total = __sync_add_and_fetch(&g_kernel_rate_limit_hit_total[host_index], 1);
  /* Power-of-two sampling matches maybe_log_counter_metric. The controller
   * label lets operators distinguish hit-rate distribution shifts caused by
   * switching CUDA_SM_CONTROLLER (e.g. delta -> aimd) from real workload
   * changes. AIMD typically increases the hit count (tighter control => more
   * frequent shorter rate_limiter sleeps) which would otherwise look like a
   * regression on dashboards. */
  if ((total & (total - 1)) == 0) {
    LOGGER(INFO, "metric=kernel_rate_limit_hit host_device=%d controller=%s total=%" PRIu64,
           host_index, g_sm_controller_label, total);
  }
}

void metrics_record_watcher_miss(int host_index, metrics_watcher_reason_t reason) {
  if (!should_collect_metrics() || !is_valid_metric_index(host_index)) {
    return;
  }

  if (reason == METRICS_WATCHER_LOCK_MISS) {
    maybe_log_counter_metric("watcher_lock_miss", host_index,
                             &g_watcher_miss_total[host_index]);
    return;
  }
  if (reason == METRICS_WATCHER_EXPIRED) {
    maybe_log_counter_metric("watcher_expired", host_index,
                             &g_watcher_miss_total[host_index]);
    return;
  }
}

void metrics_record_nvml_fallback(int host_index) {
  maybe_log_counter_metric("nvml_fallback", host_index,
                           &g_nvml_fallback_total[host_index]);
}

void metrics_record_gap_throttle(int host_index, uint64_t gpu_us, uint64_t sleep_us) {
  uint64_t total;

  if (!should_collect_metrics() || !is_valid_metric_index(host_index)) {
    return;
  }

  total = __sync_add_and_fetch(&g_gap_throttle_total[host_index], 1);
  __sync_add_and_fetch(&g_gap_sleep_us_total[host_index], sleep_us);

  /* Per-event line: lets operators confirm the GAP path fired and see each
   * sleep, at VERBOSE level. Suppressed by the logger when level < VERBOSE,
   * so it adds no cost on the default INFO path. */
  LOGGER(VERBOSE, "gap throttle host_device=%d gpu_us=%" PRIu64 " sleep_us=%" PRIu64,
         host_index, gpu_us, sleep_us);

  /* Aggregate line: power-of-two sampled like the other counters (INFO). */
  if ((total & (total - 1)) == 0) {
    LOGGER(INFO,
           "metric=gap_throttle host_device=%d count=%" PRIu64 " total_sleep_ms=%" PRIu64 " last_gpu_us=%" PRIu64 " last_sleep_us=%" PRIu64,
           host_index, total,
           (uint64_t)(g_gap_sleep_us_total[host_index] / 1000),
           gpu_us, sleep_us);
  }
}

void metrics_record_exclusivity_flip(int host_index,
                                     metrics_exclusivity_flip_direction_t direction) {
  if (!should_collect_metrics() || !is_valid_metric_index(host_index)) {
    return;
  }
  if (direction == METRICS_EXCLUSIVITY_FLIP_GAINED) {
    maybe_log_counter_metric("exclusivity_flip_gained", host_index,
                             &g_exclusivity_flip_gained_total[host_index]);
    return;
  }
  if (direction == METRICS_EXCLUSIVITY_FLIP_LOST) {
    maybe_log_counter_metric("exclusivity_flip_lost", host_index,
                             &g_exclusivity_flip_lost_total[host_index]);
    return;
  }
}

void metrics_record_aimd_event(int host_index, metrics_aimd_event_t event) {
  if (!should_collect_metrics() || !is_valid_metric_index(host_index)) {
    return;
  }
  switch (event) {
    case METRICS_AIMD_MD_FIRED:
      maybe_log_counter_metric("aimd_md", host_index,
                               &g_aimd_md_total[host_index]);
      return;
    case METRICS_AIMD_MD_BLOCKED:
      maybe_log_counter_metric("aimd_md_blocked", host_index,
                               &g_aimd_md_blocked_total[host_index]);
      return;
    case METRICS_AIMD_DEADBAND_HIT:
      maybe_log_counter_metric("aimd_deadband_hit", host_index,
                               &g_aimd_deadband_hit_total[host_index]);
      return;
  }
}
