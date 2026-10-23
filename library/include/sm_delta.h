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
 * -- 33000 cells against a frozen replica of the previous sm^2 formula; then
 * the generation after it against a frozen replica of THAT, over a load range
 * five decades wider and 64 parts including synthetic future ones. See
 * test/nogpu/test_delta_campaign.c and test/nogpu/test_delta_gen_campaign.c
 * (both asserted in CI) and docs/sm_delta_validation.md (the report).
 *
 * THE STEP:
 *
 *   target >= 100        -> share = pool          (a 100% limit is no limit)
 *   grow  (util <= target):
 *     boot & share small  -> share = pool*target/6400   (one-shot jump-start)
 *     step = max(seed, share*err/(damp*target)), damp = starved ? 4 : 6
 *     util == 0 & starved -> step >= sqrt(2*seed*share)   (escape)
 *     capped at pool/10 per tick
 *   cut   (util > target):
 *     blowout (util > 2*target, err >= 5):
 *          step = max(share*err/(6*target), seed, pool*err/(target*64))
 *     otherwise:
 *          step = share*err/(6*target), scaled by
 *                 max( share/(share+bucket),  err/100 ),
 *          at least 1 token unless bucket >= share
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
 * ~0.0004% of the pool. It must sit well below the per-cycle consumption of
 * the lightest workload that matters, and that is set by software, not by
 * the card. One controller cycle is 100 ms (the watcher's per-device
 * cadence; TIME_TICK's 10 ms is only rate_limiter's back-off). Under grid
 * charging a kernel that fills the GPU for 10 ms with 1024-thread blocks
 * consumes pool/3277 per cycle, a 100 ms one pool/32768, and a small kernel
 * on a bigger future part less still. The previous seed (pool/16384) was a
 * fifth of the 10 ms GEMM's whole demand -- too coarse to settle: H100 at
 * target 50 bang-banged at MAE 22.5, p95 50 (HAMi-core #360); this seed
 * holds it at 1.8. Floored at 1 token so an absurd env divisor degrades
 * granularity instead of freezing the controller.
 *
 * THE ESCAPE pays for that fineness. After the boot jump overshoots a light
 * workload, the bucket drains for a while and share decays toward zero; with
 * a coarse seed the climb back was linear and quick, with a fine one it would
 * crawl. A zero reading off a starved bucket proves share < consumption/100,
 * so the grow floor becomes sqrt(2*seed*share) -- what a seed growing by one
 * seed per tick would give: quadratic, not exponential (exponential climbs
 * ring against delayed feedback), and it fades out once share passes
 * 32*seed, where the proportional term is larger. No absolute pool-relative
 * quantum is added; any such kick reproduced #360 at the light end.
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
 * THE 1-TOKEN TRIM. Near the target with a small share, both mild-cut terms
 * truncate to 0 and util parks a few points over the limit for good -- a
 * dead zone the previous generation had too, escaping it only when its
 * coarse grow steps happened to land close. Trimming by the integer
 * resolution closes it. Not while bucket >= share: there the overshoot is the
 * backlog draining (the anti-windup case), and walking share down to 0
 * during a long drain leaves nothing to climb back from.
 *
 * THE EMERGENCY FLOOR handles true blowouts (util beyond twice the target),
 * where share may be tiny against the excess and proportional cuts stall.
 * The err >= MIN_INCREMENT gate keeps it out of single-digit targets where
 * integer utilization flaps across the 2x line constantly.
 *
 * GENERATIONAL RESULT (v14: seed pool*5/1310720 + escape + 1-token trim, vs
 * v13 with seed pool*5/81920; 333234 cells): steady MAE 134304 better / 10
 * worse by >0.5, aggregate 1.35x lower; p95 89731 / 2; cold-start 41710 / 1.
 * Zero regressions on v13's three loads; every load-decade x limit-band
 * region at least as good on average; the isolated cells are integer limit
 * cycles, worst +0.6 / +2 / +1.53 (docs/sm_delta_validation.md section 7).
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
#define DELTA_INCREMENT_DIVISOR_DEFAULT  1310720
#define DELTA_GROW_CAP_DIVISOR           10
#define DELTA_REL_DAMPING                6
#define DELTA_STARVED_DAMPING            4
#define DELTA_BOOT_JUMP_DIVISOR          6400

/* floor(sqrt(n)), bit-by-bit: exact, branch-bounded at 32 iterations. */
static inline uint64_t sm_delta_isqrt(uint64_t n) {
  uint64_t r = 0, bit = 1ULL << 62;
  while (bit > n) {
    bit >>= 2;
  }
  while (bit) {
    if (n >= r + bit) {
      n -= r + bit;
      r = (r >> 1) + bit;
    } else {
      r >>= 1;
    }
    bit >>= 2;
  }
  return r;
}

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
    /* Escape: a zero reading off a starved bucket means share is below 1%
     * of consumption. Climb quadratically out of the seed scale (what a
     * seed growing by one seed per tick would do) instead of crawling. */
    if (user_current == 0 && bucket <= 0) {
      __int128 q = (__int128)2 * seed * share;
      int64_t escape = (int64_t)sm_delta_isqrt(q > INT64_MAX ? INT64_MAX : (uint64_t)q);
      if (increment < escape) {
        increment = escape;
      }
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
  /* Both terms truncate to 0 for a small share near the target, which would
   * park util a few points over it for good. Trim by the integer resolution
   * -- unless the backlog explains the overshoot, where holding share is
   * the design (trimming it there empties share while the bucket drains). */
  if (increment < 1 && backlog < share) {
    increment = 1;
  }
  return (share - increment) < 0 ? 0 : (share - increment);
}

/* ---- Consumption-observed cut (v15) ----
 *
 * sm_delta_step sees utilization, a delayed and share-blind signal. When the
 * share far exceeds what the workload consumes -- after the boot jump on a
 * light workload, or after a heavy workload turns light mid-run -- the
 * surplus piles up in the bucket, and while that backlog lasts util reads 100
 * no matter what share does. Below a 50% target the blowout floor (util >
 * 2*target) recovers in seconds; at or above 50% util can never exceed twice
 * the target, only the mild cut runs, and the bucket floods for minutes
 * (A100, target 75, a light workload: 28 s to 10 min).
 *
 * The bucket says what utilization cannot: consumed = (bucket right after the
 * last refill) - (bucket now) is exactly what every process in the container
 * launched in between. When the last cycle was over-supplied (consumed <
 * share) while util ran over the target, consumed*target/util is the share
 * that would have held the target, and the backlog is surplus by
 * construction. The caller caps share at that and drains the bucket.
 *
 * Gates, each answering a failure of the ungated rule in simulation:
 *   consumed >= 100          one token per utilization point; below that the
 *                            cap truncates and erases the dithering that is
 *                            the best any controller can do (R of 1-10 tokens)
 *   util - target >= 10      a real overshoot; at targets above 90 the rule
 *                            dipped util below target on stale readings for
 *                            a gain of at most 9 points
 *   bucket > 8 * share       a persistent surplus, not a pause: an on/off
 *                            workload's idle cycles also read consumed <
 *                            share with a stale util over target, and cutting
 *                            there starved the next burst (on-phase MAE
 *                            12.7 -> 22.6). After 8 idle cycles the delayed
 *                            util has caught up to 0 and the rule is off.
 *
 * Returns the share to use (never above `next`, sm_delta_step's result) and
 * sets *drain when the caller must empty the bucket before refilling. */
#define DELTA_OBS_MIN_CONSUMED  100
#define DELTA_OBS_MIN_OVERSHOOT (2 * MIN_INCREMENT)
#define DELTA_OBS_BACKLOG_TICKS 8

static inline int64_t sm_delta_observe(int up_limit, int user_current, int64_t share,
                                       int64_t next, int64_t bucket, int64_t consumed,
                                       int *drain) {
  *drain = 0;
  if (up_limit >= 100 || up_limit < 1 || consumed < DELTA_OBS_MIN_CONSUMED ||
      user_current - up_limit < DELTA_OBS_MIN_OVERSHOOT || consumed >= share ||
      bucket <= DELTA_OBS_BACKLOG_TICKS * share) {
    return next;
  }
  /* user_current > up_limit >= 1 here, so the division is safe; consumed is
   * bounded by the pool, so the product cannot overflow. */
  int64_t cap = consumed * up_limit / user_current;
  *drain = 1;
  return next < cap ? next : cap;
}

/* The whole delta cycle as the watcher runs it: the step, then the
 * consumption-observed cut. consumed < 0 means unknown (no refill yet). One
 * function so the simulations exercise exactly what ships. */
static inline int64_t sm_delta_control(int64_t total, int up_limit, int user_current,
                                       int64_t share, int64_t bucket, int boot,
                                       int64_t consumed, int inc_divisor, int ramp_divisor,
                                       int *drain) {
  int64_t next = sm_delta_step(total, up_limit, user_current, share, bucket, boot,
                               inc_divisor, ramp_divisor);
  return sm_delta_observe(up_limit, user_current, share, next, bucket, consumed, drain);
}

#ifdef __cplusplus
}
#endif

#endif /* _VGPU_SM_DELTA_H_ */
