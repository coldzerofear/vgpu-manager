/*
Copyright 2026 coldzerofear

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

void metrics_record_lock_wait(int device_index, uint64_t wait_ns, int timeout);
void metrics_record_oom(int host_index, metrics_oom_reason_t reason);
/* Reactive: the driver refused a device allocation and we retried it as managed
 * memory. This is oversold memory arriving as a surprise. */
void metrics_record_uva_fallback(int host_index);

/**
 * Proactive: prepare_memory_allocation predicted the request would not fit in
 * physical memory and routed it to managed memory by policy. This is the path
 * oversubscription takes when it is WORKING, and until now it was the one with
 * no counter at all -- only the reactive fallback above was visible, so a
 * healthy oversold container looked identical to one not oversubscribing.
 *
 * Records bytes rather than only occurrences: four 8 GiB allocations and four
 * 1 MiB allocations are the same number of events and completely different
 * amounts of pressure. device_used and real_memory are carried so each sample
 * states how far past physical memory the container is being pushed, which is
 * the figure that predicts whether Unified Memory will thrash.
 */
void metrics_record_uva_oversold(int host_index, uint64_t request_bytes,
                                 uint64_t device_used, uint64_t real_memory);
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

#endif
