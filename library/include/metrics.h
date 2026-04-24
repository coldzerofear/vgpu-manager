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

#endif
