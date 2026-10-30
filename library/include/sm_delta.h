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

/* total: the pool, which every fine constant (seed, boot jump, ramp floor)
 * is a fraction of. cap: the most share may become -- the pool in v15, up to
 * DELTA_HEADROOM_POOLS pools in v16 (see sm_delta_control). */
static inline int64_t sm_delta_step_cap(int64_t total, int64_t cap, int up_limit,
                                        int user_current, int64_t share, int64_t bucket,
                                        int boot, int inc_divisor, int ramp_divisor) {
  if (up_limit >= 100) {
    return cap;
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
    /* A tenth of the pool, or of share once share is past it: a share many
     * pools deep must still be able to grow by a proportional step. */
    int64_t grow_cap = (share > total ? share : total) / DELTA_GROW_CAP_DIVISOR;
    if (increment > grow_cap) {
      increment = grow_cap;
    }
    return (share + increment) > cap ? cap : (share + increment);
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
    if (unlikely(increment > cap)) {
      increment = cap;
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

/* The step with share capped at the pool (v15 semantics). */
static inline int64_t sm_delta_step(int64_t total, int up_limit, int user_current,
                                    int64_t share, int64_t bucket, int boot,
                                    int inc_divisor, int ramp_divisor) {
  return sm_delta_step_cap(total, total, up_limit, user_current, share, bucket, boot,
                           inc_divisor, ramp_divisor);
}

/* ---- Headroom (v16) ----
 *
 * v15 capped share, and the bucket, at one pool. Under grid*block charging
 * (v16) a block-heavy kernel on a fast part consumes tens to hundreds of
 * pools per cycle at full speed (B200 elementwise: ~340 pools at FACTOR 256),
 * and a one-pool cap pins it at a fraction of its limit -- the same
 * over-throttling grid charging already showed at FACTOR 32 (B200 training
 * capped at 26% under a 50% limit). Raising FACTOR instead would raise the
 * bucket's backlog capacity for every workload and flood the light ones
 * (docs/sm_delta_validation.md section 10).
 *
 * So share may exceed the pool, but only as far as measured demand
 * justifies: the cap is twice the last cycle's consumption, floored at the
 * pool and never below the share already held. Idle cycles (consumed 0) thus
 * neither grow share past the pool -- the soft path has no bypass, and an
 * uncapped share wound up to 1000s of pools while a workload idled -- nor
 * push an established share down, which would make every burst after a
 * pause re-climb from one pool. Climbing to a deep share doubles per cycle
 * at most: ~9 cycles to 340 pools. */
#define DELTA_HEADROOM_POOLS 65536

static inline int64_t sm_delta_share_cap(int64_t total, int64_t share, int64_t consumed) {
  int64_t lim = total > INT64_MAX / DELTA_HEADROOM_POOLS ? INT64_MAX
                                                         : total * DELTA_HEADROOM_POOLS;
  int64_t cap = consumed > total / 2 ? (consumed < lim / 2 ? 2 * consumed : lim) : total;
  if (share > cap) {
    cap = share < lim ? share : lim;
  }
  return cap;
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
/* Past the pool the bucket holds at most two cycles of share
 * (sm_delta_est_bucket_ceiling), so an 8-cycle backlog can never form there and
 * a heavy workload that turns light would ride its deep share through the
 * limit: with no gate, over-target time after a heavy -> light drop was 5x
 * v15's. There the gate is 1.5 cycles of backlog -- a pause cannot reach
 * it, a sustained surplus does within a few cycles -- and it puts that time
 * at 0.31x v15's. The price is phase alternation: the light phase's cut
 * makes the heavy phase re-climb (real-workload alternation MAE 17.6 at a
 * 1.9 gate, 20.1 at 1.5; v15 25.0). Leaning to under rather than over the
 * limit is the isolation-safe side. In tenths of a share. */
#define DELTA_OBS_DEEP_BACKLOG_TENTHS 15

static inline int64_t sm_delta_observe(int64_t total, int up_limit, int user_current,
                                       int64_t share, int64_t next, int64_t bucket,
                                       int64_t consumed, int *drain) {
  *drain = 0;
  int64_t backlog_gate = share > total ? share / 10 * DELTA_OBS_DEEP_BACKLOG_TENTHS
                                       : DELTA_OBS_BACKLOG_TICKS * share;
  /* Above a 90 target util cannot read 10 points over it; there a saturated
   * reading (100) is the overshoot signal -- but not above 95, where v15 sits
   * at most 4 points over and chasing those turned a steady +1..+4 into a
   * sawtooth (p95 +4..+11). */
  int need = DELTA_OBS_MIN_OVERSHOOT;
  if (100 - up_limit < need) {
    need = 100 - up_limit < MIN_INCREMENT ? MIN_INCREMENT : 100 - up_limit;
  }
  if (up_limit >= 100 || up_limit < 1 || consumed < DELTA_OBS_MIN_CONSUMED ||
      user_current - up_limit < need || consumed >= share ||
      bucket <= backlog_gate) {
    return next;
  }
  /* user_current > up_limit >= 1 here, so the division is safe; consumed is
   * bounded by twice the share cap (DELTA_HEADROOM_POOLS pools), so the
   * product stays far inside int64 for any pool below 2^40. */
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
  int64_t next = sm_delta_step_cap(total, sm_delta_share_cap(total, share, consumed),
                                   up_limit, user_current, share, bucket, boot,
                                   inc_divisor, ramp_divisor);
  return sm_delta_observe(total, up_limit, user_current, share, next, bucket, consumed, drain);
}

/* ---- Sample-driven estimate (v16.1) ----
 *
 * Measured on an RTX 2080 Ti (driver 595): NVML process utilization arrives
 * once per ~1000 ms, each sample the mean of the second before it. The
 * watcher runs every 100 ms, so the step above acted ten times on one stale
 * reading, overshot, and held every target in a 2-3 s limit cycle (1 s
 * throughput sd 8-17 points). The sample period is a property of the part and
 * driver (NVML documents 1/6 s to 1 s), so it is detected, not assumed: a
 * sample is fresh when its timestamp moves.
 *
 * Between samples the bucket measures what the reading cannot: the tokens the
 * container consumed. Over one sample window, consumption C (tokens/cycle)
 * produced reading m, so the share that holds the target u is C * u / m --
 * the charge's tokens-per-utilization-point measured on this workload, this
 * part, at this clock. That is the step taken on each fresh sample; between
 * samples share holds. It needs no gain, so it is the same on every card:
 *
 *   - the limit cycle goes (hard-path sim, 1 s samples: 1 s sd 16 -> 0.4);
 *   - targets >= 90 hold: a saturated reading (93-99 on the 2080 Ti) gives
 *     C * u / m below C, where the observed cut's "10 points over" gate never
 *     opened at u = 90;
 *   - a deep share is reached in one sample instead of growing 10% a cycle
 *     (elementwise at 250 pools/cycle: 10 s+ -> 1-2 s).
 *
 * A zero reading carries no ratio. If the window was starved (the bucket ran
 * dry in most cycles) util was < 1, so C * u is a lower bound on the share the
 * target needs and is taken as is. Otherwise a zero is idleness and share
 * holds; before the first estimate (cold start) the step grows as before.
 * Growing on a stale zero is what made an on/off workload's idle second
 * inflate share 9x for the next burst.
 *
 * The pairing is by timestamp, not by arrival. A sample describes the period
 * between its timestamp and the previous one, and reaches the watcher some
 * time later (56-95 ms on the 2080 Ti). The ring records each cycle's
 * consumption with the wall-clock time it was read; a cycle covers the time
 * since the record before it and is weighted by its overlap with the
 * sample's period. Pairing by arrival instead mixed cycles after the period
 * into it: harmless at a 1 s period, but at 1/6 s with 100-300 ms of
 * publishing latency it took the target error from 2 to 6-11 points (sim).
 *
 * When the estimate is below the bucket, the backlog is surplus from a share
 * that no longer applies and is drained. */
#define DELTA_EST_MIN_READING 1
/* The estimate spans at least this much sampled time. A sample period
 * shorter than a phase of the workload pairs one phase's consumption with
 * the reading and applies the share to the next: a load alternating every
 * cycle locked into antiphase and sat at 50% under a 99% target (sim, fresh
 * samples each cycle). Readings within the window are averaged by period.
 * Slow parts (1 s samples) act on every sample, as before. */
#define DELTA_EST_MIN_WINDOW_MS 500
/* Demand-drop guard (below): a cycle consuming under 1/DELTA_EST_DROP_FACTOR
 * of the consumption the estimate was made on, with tokens left in the
 * bucket, for DELTA_EST_DROP_CYCLES cycles in a row. On the RTX 2080 Ti it
 * took GEMM / elementwise phases alternating every 3 s from 21.5 / 31.2 to
 * 13.0 / 26.1 points of mean error at targets 20 / 50 (hard path; soft
 * 28.0 / 36.8 -> 14.1 / 24.1), steady state unchanged. Capping each upward
 * estimate at 4x instead did nothing for the alternation (23.0 / 31.5). */
#define DELTA_EST_DROP_FACTOR 4
#define DELTA_EST_DROP_CYCLES 2
/* A sample period longer than this (no samples while the GPU idled) counts
 * as this long: the ring covers only the last 1.6 s anyway, and the bound
 * keeps reading x period inside 32 bits. */
#define DELTA_EST_MAX_PERIOD_MS 10000

/* A reading averaged over more than this many sample periods is a sliding
 * mean (x 2 / 3: 1.5 periods), paired as such below. */
#define DELTA_EST_SLIDING_NUM 3
#define DELTA_EST_SLIDING_DEN 2

/* now_ms: wall-clock ms of this cycle's read (CLOCK_REALTIME, the clock NVML
 * stamps samples with). sample_ts_ms: the current sample's timestamp in ms,
 * 0 when there is none. Times are compared as wrapping 32-bit differences.
 *
 * reading_avg_ms: the time each reading averages over, 0 when it is one sample
 * period. NVML returns utilization since the caller's lastSeenTimeStamp,
 * averaging every sample after it (nvml.h), and the library and its watcher
 * query a fixed second back: a part that samples once a second reads one
 * period, but an RTX 3090 (200 ms samples) reads a 1 s sliding mean. Paired
 * with only the time since the previous sample, as for discrete readings, a
 * reading still carrying the previous second's load set the share for it:
 * 1 s throughput sd 2.4-3.2, 5 s on/off loads 35-56% in the first second of
 * a 20% target. A sliding reading is paired with the consumption over the
 * span it averages (see below); a discrete one takes the path it always had.
 *
 * device_current: the whole device's reading over the same span (this
 * container's plus every other's), to tell "idle because we hold it" from
 * "another container had the GPU" -- see zero_now. */
static inline int64_t sm_delta_est_control_avg(sm_delta_est_t *st, int64_t total, int up_limit,
                                               int user_current, int device_current,
                                               uint64_t sample_ts_ms, int reading_avg_ms,
                                               uint32_t now_ms, int64_t share, int64_t bucket,
                                               int boot, int64_t consumed, int inc_divisor,
                                               int ramp_divisor, int *drain) {
  *drain = 0;
  int demand = bucket <= 0 || (consumed >= 0 && consumed >= share - share / 8);
  if (consumed >= 0) {
    uint32_t h = st->head % SM_DELTA_EST_RING;
    st->t_ms[h] = now_ms;
    st->consumed[h] = consumed;
    if (demand) {
      st->starved_mask |= 1u << h;
    } else {
      st->starved_mask &= ~(1u << h);
    }
    st->head++;
  }
  if (sample_ts_ms != 0 && sample_ts_ms != st->last_ts) {
    uint64_t prev = st->last_ts;
    st->last_ts = sample_ts_ms;
    if (prev == 0) {
      st->win_ts = sample_ts_ms;   /* the first sample only opens the window */
    } else {
      int64_t period = (int32_t)((uint32_t)sample_ts_ms - (uint32_t)prev);
      if (period > DELTA_EST_MAX_PERIOD_MS) {
        period = DELTA_EST_MAX_PERIOD_MS;
      }
      if (period > 0) {
        st->win_reading_ms += user_current * (int32_t)period;
        st->win_ms += (int32_t)period;
        /* Smoothed sample period, 3:1. A gap over twice the period is a
         * silence (an idle stretch, a stalled watcher), not the part's
         * period: counting it flipped the sliding test for a few samples. */
        if (st->period_ms == 0) {
          st->period_ms = (int32_t)period;
        } else if (period <= 2 * (int64_t)st->period_ms) {
          st->period_ms = (3 * st->period_ms + (int32_t)period + 2) / 4;
        }
      }
    }
    int32_t avg_ms = reading_avg_ms > st->period_ms ? reading_avg_ms : st->period_ms;
    int sliding = st->period_ms > 0 &&
                  (int64_t)avg_ms * DELTA_EST_SLIDING_DEN > (int64_t)st->period_ms * DELTA_EST_SLIDING_NUM;
    /* A sliding reading at 0 with the bucket dry and the device idle too: the
     * GPU idled the whole averaging span under our throttle -- no lag to wait
     * out, so act on this sample rather than at the window's end (sim, RTX
     * 3090 model: GEMM/elementwise 3 s alternation 22.5 -> 18.5). Not when
     * only this container reads 0: under time slicing another container may
     * have held the GPU, and acting then let this one grab it for a second
     * (RTX 3090: a soft limit then judged the device exclusive). A discrete 0
     * already acts once per window, as it always did. */
    int zero_now = sliding && prev != 0 && st->win_ms > 0 && user_current < DELTA_EST_MIN_READING &&
                   device_current < DELTA_EST_MIN_READING &&
                   st->head > 0 && ((st->starved_mask >> ((st->head - 1) % SM_DELTA_EST_RING)) & 1u);
    if (prev != 0 && (zero_now || st->win_ms >= DELTA_EST_MIN_WINDOW_MS)) {
      uint64_t from = st->win_ts;
      int reading = st->win_reading_ms / st->win_ms;
      int32_t win = st->win_ms;
      st->win_ts = sample_ts_ms;
      st->win_reading_ms = 0;
      st->win_ms = 0;
      /* Weighted mean of the records over (from, sample]; weights in
       * thousandths of a cycle. A sliding reading instead covers, sample by
       * sample, (t_j - avg_ms, t_j] for the window's samples t_j = sample -
       * j periods: the window's mean reading is paired with the mean of the
       * consumption over each of those spans. Measured on the RTX 3090 model
       * (sim, 1 s sliding over 200 ms samples): 1 s sd 1.5 -> 0.2, GEMM 1 s
       * on/off 26.1 -> 7.1, GEMM/elementwise 3 s alternation 23.5 -> 18.2;
       * the 1 s-sample model unchanged. */
      __int128 num = 0, starved_num = 0;
      int64_t den = 0, starved_den = 0, active_den = 0;
      int32_t spans = 1;
      if (sliding) {
        spans = (win + st->period_ms / 2) / st->period_ms;
        if (spans < 1) {
          spans = 1;
        }
      }
      for (int32_t j = 0; j < spans; j++) {
      int32_t end = (int32_t)((uint32_t)sample_ts_ms - (uint32_t)from);
      if (sliding) {
        from = sample_ts_ms - (uint64_t)j * (uint64_t)st->period_ms - (uint64_t)avg_ms;
        end = avg_ms;
      }
      for (uint32_t i = 0; i + 1 < SM_DELTA_EST_RING && i + 1 < st->head; i++) {
        uint32_t k = (st->head - 1 - i) % SM_DELTA_EST_RING;
        uint32_t kp = (st->head - 2 - i) % SM_DELTA_EST_RING;
        int32_t t1 = (int32_t)(st->t_ms[k] - (uint32_t)from);
        int32_t t0 = (int32_t)(st->t_ms[kp] - (uint32_t)from);
        if (t1 <= 0) {
          break;
        }
        int32_t lo = t0 > 0 ? t0 : 0, hi = t1 < end ? t1 : end, len = t1 - t0;
        if (hi <= lo || len <= 0) {
          continue;
        }
        int64_t w = (int64_t)(hi - lo) * 1000 / len;
        num += (__int128)st->consumed[k] * w;
        den += w;
        /* Idle is tokens left unused. A cycle that waited on an empty
         * bucket is busy -- a long kernel overdraws it and the next cycles
         * wait while the GPU runs it -- so it is active, consumption or not. */
        if (st->consumed[k] > 0 || ((st->starved_mask >> k) & 1u)) {
          active_den += w;
        }
        if ((st->starved_mask >> k) & 1u) {
          starved_num += (__int128)st->consumed[k] * w;
          starved_den += w;
        }
      }
      }
      /* Rounded, not truncated: a light load consumes a handful of tokens a
       * cycle, where truncation halved share at a 99% target (2 * 99/100 -> 1). */
      int64_t cbar = den > 0 ? (int64_t)((num + den / 2) / den) : 0;
      /* A constrained window -- the bucket ran dry in a quarter or more of
       * its active cycles -- is sized by the cycles that hit the share, not
       * by the mean: share is a per-cycle cap, and a load whose phases
       * alternate faster than the window (heavy cycles throttled, light ones
       * under the cap) needs the heavy cycles' share. The mean put such a
       * load at 55% of a 99% target (sim). Those cycles consumed what they
       * were given, so their mean is the share in force; the reading is
       * scaled to active time so an on/off load's idle cycles neither
       * inflate nor dilute it. An unconstrained window (a surplus, e.g. a
       * heavy load turned light) keeps the mean: it cuts to what is used in
       * one step. */
      int constrained = active_den > 0 && 4 * starved_den >= active_den;
      st->zero_starved = reading < DELTA_EST_MIN_READING && den > 0 && 2 * starved_den >= den;
      int m = reading >= DELTA_EST_MIN_READING ? reading : (st->zero_starved ? 1 : 0);
      if (m >= 1 && cbar > 0 && up_limit >= 1 && up_limit < 100) {
        /* cbar is one cycle's consumption: at most a bucket's worth plus one
         * overdrawing launch (each under 2^62), so x 99 fits 128 bits. */
        int64_t lim = total > INT64_MAX / DELTA_HEADROOM_POOLS ? INT64_MAX
                                                               : total * DELTA_HEADROOM_POOLS;
        __int128 est = ((__int128)cbar * up_limit + m / 2) / m;
        if (constrained && starved_den > 0) {
          /* m * den / active_den: the reading over active time, >= m. */
          __int128 m_active = (__int128)m * den / active_den;
          if (m_active < 1) {
            m_active = 1;
          }
          est = (((starved_num + starved_den / 2) / starved_den) * up_limit + m_active / 2) / m_active;
        }
        int64_t next = est > lim ? lim : (est < 0 ? 0 : (int64_t)est);
        st->have_est = 1;
        st->ref_consumed = cbar;
        st->drop_cycles = 0;
        if (bucket > next) {
          *drain = 1;
        }
        return next;
      }
    }
  }
  /* Demand drop between samples: the bucket has tokens left, yet the cycle
   * consumed a DROP_FACTOR-th of what the estimate was made on -- the load
   * changed shape (a light phase after a heavy one), and the share sized for
   * the old phase would run the new one unthrottled until the next sample.
   * Assume the new phase is busy: consumption * target / 100 cannot run it
   * past the target; the next sample re-estimates. Idle cycles (0) are not
   * a drop. */
  if (st->have_est && consumed > 0 && bucket > 0 &&
      up_limit >= 1 && up_limit < 100 &&
      (__int128)consumed * DELTA_EST_DROP_FACTOR < st->ref_consumed) {
    if (++st->drop_cycles >= DELTA_EST_DROP_CYCLES) {
      int64_t next = (int64_t)(((__int128)consumed * up_limit + 50) / 100);
      if (next < 1) {
        next = 1;
      }
      st->drop_cycles = 0;
      st->ref_consumed = consumed;
      if (bucket > next) {
        *drain = 1;
      }
      return next < share ? next : share;
    }
  } else {
    st->drop_cycles = 0;
  }
  /* An unlimited target (a soft limit of 100) has nothing to estimate: the
   * step rails share to its cap. Holding instead kept the share an earlier,
   * lower target had estimated. */
  if (up_limit < 100 && st->last_ts != 0 && ((st->have_est && !st->zero_starved) || !demand)) {
    return share;
  }
  return sm_delta_control(total, up_limit, user_current, share, bucket, boot, consumed,
                          inc_divisor, ramp_divisor, drain);
}

/* Discrete readings: each sample averages over its own period. */
static inline int64_t sm_delta_est_control(sm_delta_est_t *st, int64_t total, int up_limit,
                                           int user_current, uint64_t sample_ts_ms,
                                           uint32_t now_ms, int64_t share, int64_t bucket,
                                           int boot, int64_t consumed, int inc_divisor,
                                           int ramp_divisor, int *drain) {
  return sm_delta_est_control_avg(st, total, up_limit, user_current, user_current, sample_ts_ms, 0, now_ms,
                                  share, bucket, boot, consumed, inc_divisor, ramp_divisor,
                                  drain);
}

/* The bucket holds at most this many cycles of share (at least one token):
 * an idle stretch banks two cycles, not a pool -- v16's pool floor let a
 * light workload bank 60+ cycles and spend them in its next burst. */
#define DELTA_BUCKET_CYCLES 2
/* A launch may overdraw the bucket by up to this many cycles of share and
 * the debt is carried into the next refills. Clamping it at 0 (v16) let a
 * kernel dearer than a cycle's share run once per cycle whatever the limit:
 * a 20 ms persistent kernel floored at ~17%. The bound keeps one absurdly
 * charged launch from blocking the container for more than ~10 s. */
#define DELTA_DEBT_CYCLES 100

static inline int64_t sm_delta_est_bucket_ceiling(int64_t share) {
  if (share > INT64_MAX / DELTA_BUCKET_CYCLES) {
    return INT64_MAX;
  }
  return share * DELTA_BUCKET_CYCLES < 1 ? 1 : share * DELTA_BUCKET_CYCLES;
}

static inline int64_t sm_delta_debt_floor(int64_t share, int64_t ceiling) {
  int64_t d = share > INT64_MAX / 2 / DELTA_DEBT_CYCLES ? INT64_MAX / 2 : share * DELTA_DEBT_CYCLES;
  return -(d > ceiling ? d : ceiling);
}

/* ---- Launch charge (v16) ----
 *
 * A launch costs its thread count, grid x block. v15 charged the grid alone,
 * which makes the cost of the same GPU time differ by the block size (32 to
 * 1024) and leaves few-block long kernels nearly free: a persistent kernel
 * (grid = #SMs, as cuBLAS/CUTLASS use on Hopper/Blackwell) running longer
 * than ~14 ms per launch, or an NCCL kernel (8-32 channels) longer than
 * ~1-5 ms, fell below the share the controller can hold and ran through the
 * limit (HAMi-core #360). Threads lift those 384-512x, at FACTOR 256 8x
 * lighter than the pool would make them; block-heavy kernels land above the
 * pool and are carried by the headroom above.
 *
 * Saturating: grid dims are up to 2^31-1 x 65535 x 65535 and the product
 * with a 1024-thread block overflows 64 bits. Anything near the cap is a
 * launch no refill could ever cover anyway; the cap keeps bucket - cost
 * from wrapping. */
#define SM_LAUNCH_TOKENS_MAX (INT64_MAX / 4)

static inline int64_t sm_launch_tokens(uint64_t gx, uint64_t gy, uint64_t gz,
                                       uint64_t bx, uint64_t by, uint64_t bz) {
  unsigned __int128 t = (unsigned __int128)gx * gy;
  t = t * gz;
  if (t > SM_LAUNCH_TOKENS_MAX) {
    return SM_LAUNCH_TOKENS_MAX;
  }
  t = t * bx;
  if (t > SM_LAUNCH_TOKENS_MAX) {
    return SM_LAUNCH_TOKENS_MAX;
  }
  t = t * by;
  if (t > SM_LAUNCH_TOKENS_MAX) {
    return SM_LAUNCH_TOKENS_MAX;
  }
  t = t * bz;
  return t > SM_LAUNCH_TOKENS_MAX ? SM_LAUNCH_TOKENS_MAX : (int64_t)t;
}

#ifdef __cplusplus
}
#endif

#endif /* _VGPU_SM_DELTA_H_ */
