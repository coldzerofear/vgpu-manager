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

/* Direction of an exclusivity-FSM flip emitted by the shared debounced
 * predicate (host_index_is_exclusive_debounced in cuda_hook.c). GAINED =
 * device was "shared with external Pods" and is now "exclusively ours";
 * LOST = the reverse. Both transitions are after the N-cycle debounce. */
typedef enum {
  METRICS_EXCLUSIVITY_FLIP_GAINED = 0,
  METRICS_EXCLUSIVITY_FLIP_LOST   = 1,
} metrics_exclusivity_flip_direction_t;

/* AIMD per-cycle events emitted from aimd_controller. MD_FIRED = the cut
 * was actually applied; MD_BLOCKED = MD path entered but suppressed by
 * the cooldown still in effect from a previous cut; DEADBAND_HIT = util
 * landed inside the hysteresis band so share was held steady (the metric
 * that tells you P1 deadband is actually doing work). */
typedef enum {
  METRICS_AIMD_MD_FIRED     = 0,
  METRICS_AIMD_MD_BLOCKED   = 1,
  METRICS_AIMD_DEADBAND_HIT = 2,
} metrics_aimd_event_t;

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

/* Record an exclusivity FSM flip event (called from host_index_is_exclusive_
 * debounced after the N-cycle debounce confirms the transition). Useful for
 * verifying the FSM is actually flipping at the rate the workload implies,
 * and for spotting pathological ping-pong that would suggest the user needs
 * a larger CUDA_SM_AUTO_DEBOUNCE_CYCLES. */
void metrics_record_exclusivity_flip(int host_index,
                                     metrics_exclusivity_flip_direction_t direction);

/* Record an AIMD-controller event (called from aimd_controller). Triggered
 * by every aimd dispatch, so visible in CUDA_SM_CONTROLLER=aimd and in
 * CUDA_SM_CONTROLLER=auto whenever auto routes to aimd (i.e. when the
 * device is shared with an external Pod). MD_BLOCKED and DEADBAND_HIT
 * together quantify how much of the V2.1+P1 anti-sawtooth work is firing
 * in production. */
void metrics_record_aimd_event(int host_index, metrics_aimd_event_t event);

/* Set the SM controller label included in rate_limit_hit emissions. Called
 * once at init from cuda_hook.c's sm_controller_init(). The pointer is
 * captured as-is (caller keeps the string literal alive). Unset => "delta". */
void metrics_set_controller_label(const char *name);

#endif
