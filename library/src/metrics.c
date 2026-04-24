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
  maybe_log_counter_metric("kernel_rate_limit_hit", host_index,
                           &g_kernel_rate_limit_hit_total[host_index]);
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
