/*
Copyright 2024-2026 coldzerofear

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

/*
 * The delta controller's share-update step, as a pure function.
 *
 * Header-inline so the watcher's hot path keeps its inlining AND the math is
 * reachable from the no-GPU tests. Every constant and every branch below was
 * selected by closed-loop simulation, not argument: 55 real GPU models (GTX
 * 1050 through B300, 5 to 188 SMs, 1024/1536/2048 threads per SM), every
 * core limit from 1 to 100, three workload intensities, two feedback delays
 * -- 33000 cells against a frozen replica of the previous sm^2 formula. See
 * test/nogpu/test_delta_campaign.c (the grid, asserted in CI) and
 * docs/sm_delta_validation.md (the campaign report).
 *
 * THE STEP:
 *
 *   target >= 100        -> share = pool          (a 100% limit is no limit)
 *   grow  (util <= target):
 *     boot & share small  -> share = pool*target/6400   (one-shot jump-start)
 *     step = max(seed, share*err/(damp*target)), damp = starved ? 4 : 6
 *     capped at pool/10 per tick
 *   cut   (util > target):
 *     blowout (util > 2*target, err >= 5):
 *          step = max(share*err/(6*target), seed, pool*err/(target*64))
 *     otherwise:
 *          step = share*err/(6*target), scaled by
 *                 max( share/(share+bucket),  err/100 )
 *
 * where seed = max(1, pool*MIN_INCREMENT/inc_divisor) and err = |target-util|.
 *
 * WHY EACH PIECE EXISTS -- each answers a failure the simulation exhibited:
 *
 * POOL-RELATIVE SHARE-PROPORTIONAL CORE. The old step scaled as sm^2 against
 * a pool linear in sm, so its pool fraction grew with SM count: a 188-SM card
 * got 11-22% of its pool per tick, railed the bucket full in a second, and
 * the limit never engaged (HAMi-core #274). Linear-in-pool alone was not
 * enough either: any absolute step above the workload's per-tick consumption
 * floods the bucket the same way, and consumption is a workload property, not
 * a card property. The share itself is the one quantity that equals per-tick
 * consumption at the target, so steps proportional to share self-scale to
 * the workload. Damping 6 keeps the compounding benign through 6 ticks of
 * measurement delay (share/2-scale steps blow up at 4).
 *
 * THE SEED is the escape from share=0 and the granularity at the setpoint:
 * ~0.006% of the pool, below even a pool-drained-over-minutes consumption
 * rate. Floored at 1 token so an absurd env divisor degrades granularity
 * instead of freezing the controller.
 *
 * THE JUMP-START (boot, first two controller cycles per device) answers the
 * old formula's one legitimate advantage: it reached the target in a tick or
 * two from process start because its steps were huge. A one-shot jump to
 * pool*target/6400 gets within striking distance of any heavy-load
 * equilibrium without the flooding a REPEATED large step causes; being
 * one-shot, it cannot participate in an oscillation loop. Idempotent under
 * the shared bucket (only ever raises share toward the jump level once).
 *
 * THE STARVED NUDGE (damp 4 when the bucket reads empty) speeds mid-run
 * recovery when the workload is genuinely throttled. 4, not 2: the boost
 * compounds against stale feedback like everything else, and the simulation
 * put the knee at 6-tick delay -- damp 2 exploded (MAE 3 -> 25+), damp 4
 * holds (3.2).
 *
 * THE SMOOTH ANTI-WINDUP on mild cuts fixes high-target dips: when the
 * bucket holds a large backlog, "util over target" describes the backlog
 * draining, not the current share -- cutting share then causes the NEXT
 * dip. Scaling the cut by share/(share+bucket) makes it vanish when the
 * bucket is the cause and act normally when share is. The err/100 floor
 * keeps a runaway share (share > consumption, bucket pinned full) cuttable
 * -- without it that state would hold forever.
 *
 * THE EMERGENCY FLOOR handles true blowouts (util beyond twice the target),
 * where share may be tiny against the excess and proportional cuts stall.
 * The err >= MIN_INCREMENT gate keeps it out of single-digit targets where
 * integer utilization flaps across the 2x line constantly.
 *
 * CAMPAIGN RESULT (v13, vs the old formula, 33000 cells): steady-state MAE
 * 31223 better / 1777 equal / 0 worse (aggregate 8.2x lower); p95 sawtooth
 * 27575 / 5425 / 0; two-sided cold-start MAE 20935 / 11908 / 157, the 157
 * within 1.4 utilization-points, confined to targets >= 74 where the old
 * formula's flood-to-100 happens to approximate the target. In-model
 * numbers; hardware validation via test/ablation remains the gate.
 */

#ifndef _VGPU_SM_DELTA_H_
#define _VGPU_SM_DELTA_H_

#include "include/hook.h"

#ifdef __cplusplus
extern "C" {
#endif

#define MIN_INCREMENT                    5
#define DELTA_INCREMENT_DIVISOR_DEFAULT  81920
#define DELTA_GROW_CAP_DIVISOR           10
#define DELTA_REL_DAMPING                6
#define DELTA_STARVED_DAMPING            4
#define DELTA_BOOT_JUMP_DIVISOR          6400

static inline int64_t sm_delta_step(int64_t total, int up_limit, int user_current,
                                    int64_t share, int64_t bucket, int boot,
                                    int inc_divisor, int ramp_divisor) {
  if (up_limit >= 100) {
    return total;
  }
  int raw_diff = abs(up_limit - user_current);
  int64_t up = up_limit > 0 ? up_limit : 1;

  if (unlikely(inc_divisor <= 0)) {
    inc_divisor = DELTA_INCREMENT_DIVISOR_DEFAULT;
  }
  int64_t seed = total * MIN_INCREMENT / inc_divisor;
  if (unlikely(seed < 1)) {
    seed = 1;
  }

  if (user_current <= up_limit) {
    if (boot) {
      int64_t jump = total * up / DELTA_BOOT_JUMP_DIVISOR;
      if (jump > total / 64) {
        jump = total / 64;
      }
      if (share < jump) {
        return jump;
      }
    }
    int damp = bucket <= 0 ? DELTA_STARVED_DAMPING : DELTA_REL_DAMPING;
    int64_t increment = share * (int64_t)raw_diff / (damp * up);
    if (increment < seed) {
      increment = seed;
    }
    int64_t grow_cap = total / DELTA_GROW_CAP_DIVISOR;
    if (increment > grow_cap) {
      increment = grow_cap;
    }
    return (share + increment) > total ? total : (share + increment);
  }

  if (raw_diff > up_limit && raw_diff >= MIN_INCREMENT) {
    /* Blowout: util beyond twice the target. */
    int64_t increment = share * (int64_t)raw_diff / (DELTA_REL_DAMPING * up);
    if (increment < seed) {
      increment = seed;
    }
    if (ramp_divisor > 0) {
      int64_t ramp_floor = total * (int64_t)raw_diff / (up * ramp_divisor);
      if (increment < ramp_floor) {
        increment = ramp_floor;
      }
    }
    if (unlikely(increment > total)) {
      increment = total;
    }
    return (share - increment) < 0 ? 0 : (share - increment);
  }

  /* Mild overshoot: cut in proportion to how much of it share explains.
   * The bucket can read NEGATIVE here (rate_limiter deducts before it
   * checks), which would zero or flip the denominator -- clamp it: an
   * overdrawn bucket is "no backlog", the strongest share-is-the-cause
   * signal. 128-bit product because prop*share overflows int64 for pools
   * beyond ~2^31 tokens (no current GPU, but the contract is any pool). */
  int64_t backlog = bucket > 0 ? bucket : 0;
  int64_t prop = share * (int64_t)raw_diff / (DELTA_REL_DAMPING * up);
  int64_t by_cause = (int64_t)((__int128)prop * share / (share + backlog + 1));
  int64_t by_error = prop * (int64_t)raw_diff / 100;
  int64_t increment = by_cause > by_error ? by_cause : by_error;
  return (share - increment) < 0 ? 0 : (share - increment);
}

#ifdef __cplusplus
}
#endif

#endif /* _VGPU_SM_DELTA_H_ */
