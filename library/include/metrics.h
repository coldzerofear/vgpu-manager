#ifndef VGPU_METRICS_H
#define VGPU_METRICS_H

#include <stdint.h>

typedef enum {
  METRICS_OOM_TOTAL_LIMIT = 0,
  METRICS_OOM_DRIVER_RETURN = 1,
} metrics_oom_reason_t;

typedef enum {
  METRICS_WATCHER_LOCK_MISS = 0,
  METRICS_WATCHER_EXPIRED = 1,
} metrics_watcher_reason_t;

void metrics_record_lock_wait(int device_index, uint64_t wait_ns, int timeout);
void metrics_record_oom(int host_index, metrics_oom_reason_t reason);
void metrics_record_uva_fallback(int host_index);
void metrics_record_rate_limit_hit(int host_index);
void metrics_record_watcher_miss(int host_index, metrics_watcher_reason_t reason);
void metrics_record_nvml_fallback(int host_index);

/* Record one GAP-path duty-cycle throttle event (called from gap_end()).
 * gpu_us  = measured kernel GPU time for this launch (0 if measurement failed)
 * sleep_us = host sleep actually injected to hold the duty cycle (0 if none)
 * Emits a per-event VERBOSE line plus a power-of-two-sampled INFO aggregate,
 * matching the rest of metrics.c. */
void metrics_record_gap_throttle(int host_index, uint64_t gpu_us, uint64_t sleep_us);

/* Set the SM controller label included in rate_limit_hit emissions. Called
 * once at init from cuda_hook.c's sm_controller_init(). The pointer is
 * captured as-is (caller keeps the string literal alive). Unset => "delta". */
void metrics_set_controller_label(const char *name);

#endif
