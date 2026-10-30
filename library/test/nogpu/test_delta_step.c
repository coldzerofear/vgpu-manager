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
 * delta controller step: invariants + closed-loop MAE, old formula vs new.
 * No GPU, no driver. This harness is where the new step's every constant was
 * chosen, so it is also what pins them. The full 55-card x 100-limit
 * dominance grid lives in test_delta_campaign.c; this file is the fast unit
 * layer.
 *
 * Part 1: invariants of the shipped sm_delta_step() -- SM-independence of the
 * step fraction, seed granularity below light-grid consumption (the pinning
 * criterion), the grow cap, the one-shot boot jump, the starved nudge, the
 * smooth anti-windup on mild cuts, the blowout-gated emergency cut floor,
 * the u>=100 pass-through, and the share clamps.
 *
 * Part 2: a closed watcher/bucket/workload loop against a frozen replica of
 * the REMOVED formula (sm^2/2560). One tick = one watcher cycle; the bucket
 * gains `share` per tick clamped to the pool (change_token), the workload
 * drains up to R tokens per tick (its demand at 100% util), util =
 * consumed/R, and the controller sees util DELAYED (NVML window +
 * share-take-effect). GAP path and idle-bypass are orthogonal and not
 * modelled. MAE windows exclude the cold-start full-bucket drain so they
 * measure the steady state the HAMi-core #274 report describes.
 *
 * The model is deliberately harsh (instant util collapse when the bucket
 * empties, discrete ticks); absolute MAE numbers are pessimistic, but both
 * formulas face the same harshness, so the comparison stands.
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "include/hook.h"
#include "include/sm_delta.h"

static int failures;

/* ---- The removed formula, frozen verbatim from 481a012^ (sm^2 step +
 * acceleration + symmetric MIN_INCREMENT-floored ramp floor). Its constants
 * are frozen here too -- the live header no longer carries them. ---- */
#define OLD_SCALE_FACTOR       2560
#define OLD_UTIL_DIFF_THRESH   0.5
#define OLD_RECOVERY_STEP      10

static int64_t old_delta_step(int64_t sm_num, int64_t max_thread, int64_t total,
                              int up_limit, int user_current, int64_t share,
                              int ramp_divisor) {
  int utilization_diff = abs(up_limit - user_current);
  if (utilization_diff < MIN_INCREMENT) {
    utilization_diff = MIN_INCREMENT;
  }
  int64_t increment =
      sm_num * sm_num * max_thread * (int64_t)(utilization_diff) / OLD_SCALE_FACTOR;
  if ((float)utilization_diff / (float)(up_limit) > OLD_UTIL_DIFF_THRESH) {
    increment = increment * utilization_diff * 2 / (up_limit + 1);
  }
  if (increment < 0 || increment > INT_MAX) {
    increment = OLD_RECOVERY_STEP;
  }
  if (ramp_divisor > 0) {
    int64_t floor_up_limit = up_limit > 0 ? up_limit : 1;
    int64_t ramp_floor = total * (int64_t)utilization_diff
                         / (floor_up_limit * ramp_divisor);
    if (increment < ramp_floor) {
      increment = ramp_floor;
    }
  }
  if (user_current <= up_limit) {
    share = (share + increment) > total ? total : (share + increment);
  } else {
    share = (share - increment) < 0 ? 0 : (share - increment);
  }
  return share;
}

#define DIV DELTA_INCREMENT_DIVISOR_DEFAULT

/* ---- Part 1: invariants ---- */

static void test_invariants(void) {
  printf("invariants of sm_delta_step:\n");

  /* The step must be the same FRACTION of the pool on a 7-SM slice and a
   * 188-SM card -- the property whose absence was the #274 bug. Bucket is
   * held pool-proportional too so the anti-windup scale matches. */
  {
    int64_t small = 7LL * 1536 * 32, big = 188LL * 1536 * 32;
    int ok = 1;
    for (int u = 10; u <= 80 && ok; u += 10) {
      for (int util = 0; util <= 100 && ok; util += 5) {
        int64_t a = sm_delta_step(small, u, util, small / 2, small / 4, 0, DIV, 64)
                    - small / 2;
        int64_t b = sm_delta_step(big, u, util, big / 2, big / 4, 0, DIV, 64)
                    - big / 2;
        int64_t fa = a * 1000000 / small, fb = b * 1000000 / big;
        int64_t slack = 1000000 * 2 / small + 2; /* integer-division grain */
        if (fa > fb + slack || fb > fa + slack) ok = 0;
      }
    }
    if (!ok) { printf("  [FAIL] step fraction is not SM-independent\n"); failures++; }
    else printf("  [ok] step fraction identical on 7-SM and 188-SM pools\n");
  }

  /* Seed granularity: the minimum step must undercut even a light-grid
   * workload's per-tick consumption (#274's pool-drained-over-minutes is
   * ~pool/1200); a coarser minimum step pins util at 100%. */
  {
    int64_t total = 188LL * 1536 * 32;
    int64_t min_step = sm_delta_step(total, 50, 50, 0, total, 0, DIV, 64);
    if (min_step * 4 >= total / 1200) {
      printf("  [FAIL] seed %ld not well below light-grid rate %ld\n",
             (long)min_step, (long)(total / 1200));
      failures++;
    } else {
      printf("  [ok] seed %ld (%.4f%% of pool) well under light-grid rate %ld\n",
             (long)min_step, 100.0 * min_step / total, (long)(total / 1200));
    }
  }

  /* Grow never exceeds pool/10, from any share, starved or not. (u=100 is
   * excluded: a 100%% limit is no limit and pass-through is checked below.) */
  {
    int64_t total = 188LL * 1536 * 32;
    int ok = 1;
    for (int u = 1; u <= 99 && ok; u++) {
      for (int util = 0; util <= u && ok; util++) {
        for (int64_t s = 0; s <= total && ok; s += total / 7) {
          int64_t step = sm_delta_step(total, u, util, s, 0, 0, DIV, 64) - s;
          int64_t step2 = sm_delta_step(total, u, util, s, total, 0, DIV, 64) - s;
          if (step > total / DELTA_GROW_CAP_DIVISOR) ok = 0;
          if (step2 > total / DELTA_GROW_CAP_DIVISOR) ok = 0;
        }
      }
    }
    if (!ok) { printf("  [FAIL] a grow step exceeded pool/%d\n", DELTA_GROW_CAP_DIVISOR); failures++; }
    else printf("  [ok] grow step <= pool/%d always\n", DELTA_GROW_CAP_DIVISOR);
  }

  /* u >= 100 means "no limit": the share must rail to the full pool from any
   * state, both directions. */
  {
    int64_t total = 76LL * 1536 * 32;
    if (sm_delta_step(total, 100, 0, 0, total, 1, DIV, 64) != total ||
        sm_delta_step(total, 100, 100, total / 2, 0, 0, DIV, 64) != total) {
      printf("  [FAIL] u>=100 does not pass the full pool through\n");
      failures++;
    } else {
      printf("  [ok] u>=100 rails the share to the full pool\n");
    }
  }

  /* Boot jump: one-shot, proportional to the target, capped at pool/64, and
   * idempotent -- a share already at or above the jump level is not touched
   * by it (so the shared bucket's established share survives a late-joining
   * process's boot window). */
  {
    int64_t total = 148LL * 2048 * 32;
    int64_t j50 = sm_delta_step(total, 50, 0, 0, total, 1, DIV, 64);
    int64_t j99 = sm_delta_step(total, 99, 0, 0, total, 1, DIV, 64);
    int64_t established = total / 3;
    int64_t after = sm_delta_step(total, 50, 0, established, total, 1, DIV, 64);
    int ok = j50 == total * 50 / DELTA_BOOT_JUMP_DIVISOR
          && j99 == total * 99 / DELTA_BOOT_JUMP_DIVISOR
          && j99 <= total / 64 /* the clamp is a never-binding safety net */
          && after >= established && after - established <= total / DELTA_GROW_CAP_DIVISOR;
    if (!ok) {
      printf("  [FAIL] boot jump wrong (j50=%ld j99=%ld after=%ld)\n",
             (long)j50, (long)j99, (long)after);
      failures++;
    } else {
      printf("  [ok] boot jump = pool*target/%d capped pool/64, idempotent over an established share\n",
             DELTA_BOOT_JUMP_DIVISOR);
    }
  }

  /* Starved nudge: with the bucket empty the grow step must be larger than
   * with tokens available (damping 4 vs 6), from the same state. */
  {
    int64_t total = 108LL * 2048 * 32;
    int64_t share = total / 20;
    int64_t starved = sm_delta_step(total, 50, 20, share, 0, 0, DIV, 64);
    int64_t relaxed = sm_delta_step(total, 50, 20, share, total / 2, 0, DIV, 64);
    if (starved <= relaxed) {
      printf("  [FAIL] starved grow (%ld) not larger than relaxed grow (%ld)\n",
             (long)starved, (long)relaxed);
      failures++;
    } else {
      printf("  [ok] empty bucket speeds the grow (damping %d vs %d)\n",
             DELTA_STARVED_DAMPING, DELTA_REL_DAMPING);
    }
  }

  /* Emergency floor: at u=8, util=90 (blowout: raw_diff 82 > 8) the floored
   * cut -- total*82/(8*64) = 16% of the pool -- exceeds any realistic share,
   * so the share must be cut to 0 outright. The successor of the
   * hard_core=8-pinned-at-15 protection. */
  {
    int64_t total = 76LL * 1536 * 32;
    int64_t share = total / 100;
    if (sm_delta_step(total, 8, 90, share, 0, 0, DIV, 64) != 0) {
      printf("  [FAIL] blowout cut did not clamp the share to 0\n");
      failures++;
    } else {
      printf("  [ok] blowout cut clamps the share (emergency floor active)\n");
    }
  }

  /* ...but the emergency path is GATED: raw_diff must reach MIN_INCREMENT,
   * or single-digit targets would take the absolute floor every time integer
   * utilization flaps across the 2x line (u=2, util=5 is such a flap). */
  {
    int64_t total = 170LL * 1536 * 32;
    int64_t share = total / 50;
    int64_t cut = share - sm_delta_step(total, 2, 5, share, 0, 0, DIV, 64);
    int64_t floor_would_be = total * 3 / (2 * 64);
    if (cut >= floor_would_be) {
      printf("  [FAIL] sub-MIN_INCREMENT overshoot took the emergency floor\n");
      failures++;
    } else {
      printf("  [ok] emergency floor gated on raw_diff >= %d\n", MIN_INCREMENT);
    }
  }

  /* In-band cut stays gentle: at u=80, util=100 (raw_diff 20, NOT a blowout)
   * the cut must be share-proportional -- at most share*20/(6*80) -- never
   * the absolute floor. Cutting harder than this is what caused the u=80
   * oscillation regression during tuning. */
  {
    int64_t total = 188LL * 1536 * 32;
    int64_t share = total / 100;
    int64_t cut = share - sm_delta_step(total, 80, 100, share, 0, 0, DIV, 64);
    int64_t expect_max = share * 20 / (DELTA_REL_DAMPING * 80)
                         + total * MIN_INCREMENT / DIV + 1;
    if (cut > expect_max) {
      printf("  [FAIL] mild overshoot cut %ld exceeds proportional bound %ld\n",
             (long)cut, (long)expect_max);
      failures++;
    } else {
      printf("  [ok] in-band cut is share-proportional (%ld <= %ld)\n",
             (long)cut, (long)expect_max);
    }
  }

  /* Smooth anti-windup: the same mild overshoot with a FULL bucket must cut
   * far less than with an empty one -- a full bucket means the backlog, not
   * the share, explains the overshoot -- yet still cut a little (the err/100
   * floor keeps a runaway share cuttable so that state cannot hold forever). */
  {
    int64_t total = 188LL * 1536 * 32;
    int64_t share = total / 100;
    int64_t cut_empty = share - sm_delta_step(total, 80, 100, share, 0, 0, DIV, 64);
    int64_t cut_full = share - sm_delta_step(total, 80, 100, share, total, 0, DIV, 64);
    if (cut_full * 4 >= cut_empty || cut_full <= 0) {
      printf("  [FAIL] anti-windup broken (cut %ld empty vs %ld full)\n",
             (long)cut_empty, (long)cut_full);
      failures++;
    } else {
      printf("  [ok] full-bucket overshoot cut %ld << empty-bucket cut %ld, and > 0\n",
             (long)cut_full, (long)cut_empty);
    }
  }

  /* A NEGATIVE bucket read must be survivable: rate_limiter deducts before
   * it checks, so the watcher can observe g_cur < 0. The hazard is the mild
   * cut's share/(share+bucket+1) denominator hitting zero (share=5,
   * bucket=-6 -> SIGFPE before the clamp existed). Overdrawn must also cut
   * at least as hard as merely-empty -- it is the stronger signal. */
  {
    int64_t total = 132LL * 2048 * 32;
    int64_t share = total / 10;
    int64_t cut_neg = share - sm_delta_step(total, 80, 100, share, -share - 1, 0, DIV, 64);
    int64_t cut_zero = share - sm_delta_step(total, 80, 100, share, 0, 0, DIV, 64);
    int64_t r5 = sm_delta_step(total, 80, 100, 5, -6, 0, DIV, 64); /* the exact SIGFPE state */
    if (cut_neg < cut_zero || r5 < 0 || r5 > 5) {
      printf("  [FAIL] negative bucket mishandled (cut %ld vs %ld, r5=%ld)\n",
             (long)cut_neg, (long)cut_zero, (long)r5);
      failures++;
    } else {
      printf("  [ok] negative (overdrawn) bucket: no div-by-zero, cuts as hard as empty\n");
    }
  }

  /* Output contract sweep: for ANY share in [0,total] the result stays in
   * [0,total] -- 1M deterministic pseudo-random states over degenerate and
   * real pools, out-of-spec limits/utils, negative and overfull buckets,
   * hostile divisors. The 20M-state UBSan/ASan fuzz that found the negative-
   * bucket division and the 2^40-pool overflow was this loop's ancestor. */
  {
    static const int64_t totals[] = {1, 2, 31, 32768, 5LL*2048*32, 40LL*1024*32,
        108LL*2048*32, 188LL*1536*32, 1LL<<40};
    static const int divs[] = {-1, 0, 1, 64, DIV, 2000000000};
    uint64_t st = 0x9e3779b97f4a7c15ULL;
    long bad = 0;
    for (long i = 0; i < 1000000; i++) {
      st ^= st << 13; st ^= st >> 7; st ^= st << 17;
      int64_t total = totals[st % 9];
      int u = (int)((st >> 8) % 161) - 10;
      int util = (int)((st >> 16) % 161) - 10;
      int64_t share = (int64_t)((st >> 3) % (uint64_t)(total + 1));
      int64_t bucket = (int64_t)((st >> 24) % (uint64_t)(2 * total + 1)) - total / 2;
      int64_t r = sm_delta_step(total, u, util, share, bucket, (int)(st & 1),
                                divs[(st >> 32) % 6], divs[(st >> 40) % 6]);
      if (r < 0 || r > total) bad++;
    }
    if (bad) {
      printf("  [FAIL] output contract violated in %ld of 1M adversarial states\n", bad);
      failures++;
    } else {
      printf("  [ok] 1M adversarial states: result always in [0, total]\n");
    }
  }

  /* Share clamps to [0, total]. */
  {
    int64_t total = 76LL * 1536 * 32;
    if (sm_delta_step(total, 50, 0, total, 0, 0, DIV, 64) != total ||
        sm_delta_step(total, 10, 99, 1, 0, 0, DIV, 64) != 0) {
      printf("  [FAIL] share clamp to [0, total] broken\n");
      failures++;
    } else {
      printf("  [ok] share clamps to [0, total]\n");
    }
  }

  /* No bootstrap deadlock, ever: from share=0 the step must be >= 1 even
   * under an absurd env divisor or a degenerate pool -- a zero step would
   * leave share at 0 forever (the proportional term needs a nonzero share
   * to act on). Real pools never produce a zero seed at the default divisor
   * (a hypothetical 1-SM card is already total=32768, seed=2); this guards
   * the operator-tunable divisor. */
  {
    int ok = sm_delta_step(108LL * 2048 * 32, 50, 0, 0, 1, 0, 1000000000, 64) >= 1
          && sm_delta_step(100, 50, 0, 0, 1, 0, DIV, 64) >= 1
          && sm_delta_step(76LL * 1536 * 32, 99, 98, 10, 1, 0, 1000000000, 64) != 10;
    if (!ok) {
      printf("  [FAIL] a zero step escaped: bootstrap deadlock possible\n");
      failures++;
    } else {
      printf("  [ok] step >= 1 from share=0 under any divisor (no bootstrap deadlock)\n");
    }
  }

  /* #360: a kernel filling the GPU for 100 ms with 1024-thread blocks
   * consumes pool/32768 per 100 ms controller cycle under grid charging (a
   * 10 ms one, ten times that). The seed must sit well under it on every
   * pool, or the share can only bang between 0 and seed multiples.
   * Pool-relative, so checking the extremes checks them all. */
  {
    int64_t pools[] = {5LL * 2048 * 32, 188LL * 1536 * 32, 1024LL * 4096 * 32};
    int ok = 1;
    for (int i = 0; i < 3; i++) {
      int64_t seed = sm_delta_step(pools[i], 50, 50, 0, pools[i], 0, DIV, 64);
      if (seed * 4 > pools[i] / 32768) {
        ok = 0;
      }
    }
    if (!ok) {
      printf("  [FAIL] seed not 4x under a 100 ms kernel's per-cycle consumption\n");
      failures++;
    } else {
      printf("  [ok] seed >= 4x under a 100 ms kernel's per-cycle consumption (#360)\n");
    }
  }

  /* The escape's integer square root is exact everywhere it can be fed. */
  {
    int ok = 1;
    for (uint64_t n = 0; n < 200000 && ok; n++) {
      uint64_t r = sm_delta_isqrt(n);
      ok = r * r <= n && (r + 1) * (r + 1) > n;
    }
    uint64_t edge[] = {UINT64_MAX >> 1, (1ULL << 62) - 1, 1ULL << 62,
                       4294967295ULL * 4294967295ULL};
    for (int i = 0; i < 4 && ok; i++) {
      unsigned __int128 r = sm_delta_isqrt(edge[i]);
      ok = r * r <= edge[i] && (r + 1) * (r + 1) > edge[i];
    }
    if (!ok) {
      printf("  [FAIL] sm_delta_isqrt is not floor(sqrt(n))\n");
      failures++;
    } else {
      printf("  [ok] sm_delta_isqrt exact on [0, 2e5) and at the 64-bit edges\n");
    }
  }

  /* Escape: only a zero reading off a starved bucket climbs at
   * sqrt(2*seed*share); a full bucket or a nonzero reading keeps the plain
   * step, and an absurd divisor cannot overflow it. */
  {
    int64_t total = 108LL * 2048 * 32;
    int64_t seed = total * MIN_INCREMENT / DIV, share = 4 * seed;
    int64_t want = (int64_t)sm_delta_isqrt((uint64_t)(2 * seed * share));
    int64_t esc = sm_delta_step(total, 50, 0, share, 0, 0, DIV, 64) - share;
    int64_t full = sm_delta_step(total, 50, 0, share, total, 0, DIV, 64) - share;
    int64_t warm = sm_delta_step(total, 50, 1, share, 0, 0, DIV, 64) - share;
    int64_t wild = sm_delta_step(total, 50, 0, total / 2, 0, 0, 1, 64);
    if (esc != want || full >= want || warm >= want || wild < total / 2 || wild > total) {
      printf("  [FAIL] escape: %ld (want %ld), full %ld, warm %ld, wild %ld\n",
             (long)esc, (long)want, (long)full, (long)warm, (long)wild);
      failures++;
    } else {
      printf("  [ok] escape sqrt(2*seed*share)=%ld only on a starved zero reading\n", (long)want);
    }
  }

  /* 1-token trim: a small share just over the target must move down (the
   * proportional cut truncates to 0 there), but not while the bucket's
   * backlog explains the overshoot. */
  {
    int64_t total = 5LL * 2048 * 32;
    int64_t parked = sm_delta_step(total, 50, 53, 56, 1, 0, DIV, 64);
    int64_t backlog = sm_delta_step(total, 50, 53, 56, 56, 0, DIV, 64);
    if (parked != 55 || backlog != 56) {
      printf("  [FAIL] trim: parked share 56 -> %ld (want 55), backlog case -> %ld (want 56)\n",
             (long)parked, (long)backlog);
      failures++;
    } else {
      printf("  [ok] 1-token trim un-parks a small share, holds it while the backlog drains\n");
    }
  }

  /* Consumption-observed cut: it fires only on a persistent, measurable
   * over-supply well over the target, caps share at consumed*target/util and
   * asks for a drain -- and every gate alone keeps it off. */
  {
    /* Firing state: target 50, util 80, consumed 3000 < share 10000,
     * bucket 100000 > 8 * share. */
    int drain = 0;
    int64_t fired = sm_delta_observe(1000000, 50, 80, 10000, 9000, 100000, 3000, &drain);
    int ok = fired == 3000 * 50 / 80 && drain == 1;
    /* never raises the step's own result */
    ok &= sm_delta_observe(1000000, 50, 80, 10000, 1000, 100000, 3000, &drain) == 1000 && drain == 1;
    struct { int up, util; int64_t share, bucket, consumed; } off[] = {
      {100, 100, 10000, 100000, 3000},   /* unlimited                         */
      {50, 59, 10000, 100000, 3000},     /* overshoot < 10 points             */
      {50, 80, 10000, 100000, 99},       /* consumption below 1 token/point   */
      {50, 80, 10000, 100000, -1},       /* unknown (no refill yet)           */
      {50, 80, 3000, 100000, 3000},      /* supply was binding (consumed=share)*/
      {50, 80, 10000, 80000, 3000},      /* backlog not above 8 cycles: pause */
    };
    for (unsigned i = 0; i < sizeof(off) / sizeof(off[0]); i++) {
      int64_t r = sm_delta_observe(1000000, off[i].up, off[i].util, off[i].share, 7777,
                                   off[i].bucket, off[i].consumed, &drain);
      ok &= r == 7777 && drain == 0;
    }
    if (!ok) {
      printf("  [FAIL] consumption-observed cut: wrong cap, drain, or a gate that lets it through\n");
      failures++;
    } else {
      printf("  [ok] observed cut caps at consumed*target/util + drain; each gate alone keeps it off\n");
    }
  }

  /* v16 headroom: share may pass the pool only as far as twice the measured
   * consumption, never above DELTA_HEADROOM_POOLS pools, and an idle cycle
   * neither grows it past the pool nor pushes an established share down. */
  {
    int64_t total = 108LL * 2048 * 256;
    int ok = sm_delta_share_cap(total, 0, -1) == total                    /* unknown       */
          && sm_delta_share_cap(total, 0, 0) == total                     /* idle          */
          && sm_delta_share_cap(total, 0, total / 2) == total             /* below a pool  */
          && sm_delta_share_cap(total, 0, 3 * total) == 6 * total         /* demand x2     */
          && sm_delta_share_cap(total, 50 * total, 0) == 50 * total       /* idle holds    */
          && sm_delta_share_cap(total, 50 * total, 3 * total) == 50 * total
          && sm_delta_share_cap(total, 0, INT64_MAX / 4) == total * DELTA_HEADROOM_POOLS
          && sm_delta_share_cap(INT64_MAX / 2, 0, INT64_MAX / 2) == INT64_MAX;
    /* the step follows the cap: unlimited targets return it, growth stops at it */
    ok &= sm_delta_step_cap(total, 7 * total, 100, 0, 0, 0, 0, DIV, 64) == 7 * total;
    ok &= sm_delta_step_cap(total, total, 50, 0, total, -1, 0, DIV, 64) == total;
    ok &= sm_delta_step_cap(total, 4 * total, 50, 0, 3 * total, -1, 0, DIV, 64) > 3 * total;
    ok &= sm_delta_step_cap(total, 4 * total, 50, 0, 3 * total, -1, 0, DIV, 64) <= 4 * total;
    if (!ok) {
      printf("  [FAIL] headroom: share cap or unlimited target\n");
      failures++;
    } else {
      printf("  [ok] headroom: share past the pool only on measured demand (x2/cycle), idle holds\n");
    }
  }

  /* v16 observe gates: a saturated reading is the overshoot above a 90..95
   * target (none above 95), and past the pool the backlog gate is 1.9 cycles. */
  {
    int drain = 0, ok = 1;
    int64_t total = 1000000;
    /* target 93: util 100 is 7 over -- fires; util 99 does not */
    ok &= sm_delta_observe(total, 93, 100, 10000, 9000, 100000, 3000, &drain) == 3000 * 93 / 100 && drain;
    ok &= sm_delta_observe(total, 93, 99, 10000, 9000, 100000, 3000, &drain) == 9000 && !drain;
    ok &= sm_delta_observe(total, 95, 100, 10000, 9000, 100000, 3000, &drain) == 3000 * 95 / 100 && drain;
    ok &= sm_delta_observe(total, 96, 100, 10000, 9000, 100000, 3000, &drain) == 9000 && !drain;
    ok &= sm_delta_observe(total, 90, 100, 10000, 9000, 100000, 3000, &drain) == 3000 * 90 / 100 && drain;
    ok &= sm_delta_observe(total, 90, 99, 10000, 9000, 100000, 3000, &drain) == 9000 && !drain;
    /* deep share (past the pool): 1.5-cycle backlog gate */
    int64_t sh = 3 * total;
    ok &= sm_delta_observe(total, 50, 80, sh, sh, sh / 10 * 15 + 1, total, &drain) == total * 50 / 80 && drain;
    ok &= sm_delta_observe(total, 50, 80, sh, sh, sh / 10 * 15, total, &drain) == sh && !drain;
    if (!ok) {
      printf("  [FAIL] observe gates: high-target saturation trigger or deep backlog gate\n");
      failures++;
    } else {
      printf("  [ok] observe: saturated reading triggers at targets 90-95 only; deep share gates at 1.5 cycles\n");
    }
  }

  /* v16.1 sample-driven estimate. One cycle = 100 ms; samples and cycle
   * reads on the same millisecond grid, so window weights are exact. */
  {
    int ok = 1, drain = 0;
    const int64_t total = 1000000;
    sm_delta_est_t st;
    int64_t r = 0;

    /* acts once per window: the first sample opens it, the next one 500 ms
     * later closes it at starved-mean * target / reading; holds in between */
    memset(&st, 0, sizeof st);
    for (int t = 1; t <= 6; t++) {
      r = sm_delta_est_control(&st, total, 25, 50, t < 6 ? 100 : 600, t * 100, 5000, 0, 0,
                               5000, DIV, 64, &drain);
    }
    ok &= r == 5000 * 25 / 50 && st.have_est;
    ok &= sm_delta_est_control(&st, total, 25, 50, 600, 700, 2500, 0, 0, 2500, DIV, 64, &drain) == 2500;
    int once = ok;

    /* a sample every cycle: nothing until 500 ms of samples are in */
    memset(&st, 0, sizeof st);
    int early = 0;
    for (int t = 1; t <= 5; t++) {
      r = sm_delta_est_control(&st, total, 25, 50, t * 100, t * 100, 5000, 0, 0, 5000, DIV, 64, &drain);
      early |= st.have_est;
    }
    ok &= !early;
    r = sm_delta_est_control(&st, total, 25, 50, 600, 600, 5000, 0, 0, 5000, DIV, 64, &drain);
    ok &= r == 2500 && st.have_est;
    int window = ok;

    /* aligned by timestamp: the sample (ts 600) arrives at 900 and the
     * cycles after 600 were idle -- they are not part of its window */
    memset(&st, 0, sizeof st);
    for (int t = 1; t <= 9; t++) {
      int64_t c = t <= 6 ? 5000 : 0;
      r = sm_delta_est_control(&st, total, 25, 50, t < 9 ? 100 : 600, t * 100, 5000,
                               c ? 0 : 5000, 0, c, DIV, 64, &drain);
    }
    ok &= r == 2500;
    int aligned = ok;

    /* a starved zero means util < 1: consumption * target is a lower bound */
    memset(&st, 0, sizeof st);
    for (int t = 1; t <= 6; t++) {
      r = sm_delta_est_control(&st, total, 40, 0, t < 6 ? 100 : 600, t * 100, 1000, 0, 0,
                               1000, DIV, 64, &drain);
    }
    ok &= r == 1000 * 40 && st.zero_starved;
    int lower = ok;

    /* an idle zero (nothing consumed, bucket full) holds share, before and
     * after an estimate */
    memset(&st, 0, sizeof st);
    for (int t = 1; t <= 12; t++) {
      r = sm_delta_est_control(&st, total, 40, 0, t < 6 ? 100 : 600, t * 100, 7000, 7000, 0, 0,
                               DIV, 64, &drain);
    }
    ok &= r == 7000;
    int idle = ok;

    /* an unconstrained window (surplus) cuts to mean consumption in one step
     * and drains the backlog */
    memset(&st, 0, sizeof st);
    for (int t = 1; t <= 6; t++) {
      r = sm_delta_est_control(&st, total, 50, 100, t < 6 ? 100 : 600, t * 100, 100000, 150000,
                               0, 10000, DIV, 64, &drain);
    }
    ok &= r == 10000 * 50 / 100 && drain == 1;
    int cut = ok;

    /* a constrained window is sized by the cycles that hit the share: heavy
     * cycles (8000, starved) alternate with light ones (100, under the cap);
     * the mean would give 2025 */
    memset(&st, 0, sizeof st);
    for (int t = 1; t <= 6; t++) {
      int heavy = t % 2 == 0;
      r = sm_delta_est_control(&st, total, 30, 60, t < 6 ? 100 : 600, t * 100, 8000,
                               heavy ? 0 : 7900, 0, heavy ? 8000 : 100, DIV, 64, &drain);
    }
    ok &= r == 8000 * 30 / 60;
    int constrained = ok;

    /* demand drop: after an estimate on 5000 a cycle, two unthrottled cycles
     * at 1000 (< 5000/4) cut to consumption * target / 100 and drain; one
     * cycle does not, and an idle cycle (0) is not a drop */
    memset(&st, 0, sizeof st);
    for (int t = 1; t <= 6; t++) {
      r = sm_delta_est_control(&st, total, 25, 50, t < 6 ? 100 : 600, t * 100, 5000, 0, 0,
                               5000, DIV, 64, &drain);
    }
    int64_t first = sm_delta_est_control(&st, total, 25, 50, 600, 700, 2500, 4000, 0, 1000, DIV, 64, &drain);
    int64_t idle_r = sm_delta_est_control(&st, total, 25, 50, 600, 800, 2500, 4000, 0, 0, DIV, 64, &drain);
    sm_delta_est_control(&st, total, 25, 50, 600, 900, 2500, 4000, 0, 1000, DIV, 64, &drain);
    r = sm_delta_est_control(&st, total, 25, 50, 600, 1000, 2500, 4000, 0, 1000, DIV, 64, &drain);
    ok &= first == 2500 && idle_r == 2500 && r == 1000 * 25 / 100 && drain == 1;
    int dropped = ok;

    /* a huge launch (2^61 tokens) in a drop cycle: 128-bit, no overflow */
    memset(&st, 0, sizeof st);
    for (int t = 1; t <= 6; t++) {
      sm_delta_est_control(&st, total, 25, 50, t < 6 ? 100 : 600, t * 100, 5000, 0, 0,
                           5000, DIV, 64, &drain);
    }
    st.ref_consumed = INT64_MAX;
    sm_delta_est_control(&st, total, 99, 50, 600, 700, 2500, 4000, 0, (int64_t)1 << 61, DIV, 64, &drain);
    r = sm_delta_est_control(&st, total, 99, 50, 600, 800, 2500, 4000, 0, (int64_t)1 << 61, DIV, 64, &drain);
    ok &= r == 2500;
    int huge = ok;

    /* an unlimited target after an estimate rails share to its cap instead of
     * holding the estimate (a soft limit of 100 once the device is exclusive) */
    memset(&st, 0, sizeof st);
    for (int t = 1; t <= 6; t++) {
      r = sm_delta_est_control(&st, total, 50, 50, t < 6 ? 100 : 600, t * 100, 5000, 0, 0,
                               5000, DIV, 64, &drain);
    }
    ok &= st.have_est && r == 5000;
    r = sm_delta_est_control(&st, total, 100, 50, 600, 700, 5000, 0, 0, 5000, DIV, 64, &drain);
    ok &= r == sm_delta_share_cap(total, 5000, 5000);
    int unlimited = ok;

    /* a sample after an hour without any: the period is bounded, the
     * accumulators stay in range and the window estimates as usual */
    memset(&st, 0, sizeof st);
    sm_delta_est_control(&st, total, 25, 50, 100, 100, 5000, 0, 0, 5000, DIV, 64, &drain);
    for (int t = 2; t <= 6; t++) {
      r = sm_delta_est_control(&st, total, 25, 1000, t < 6 ? 100 : 3600100, t * 100, 5000, 0, 0,
                               5000, DIV, 64, &drain);
    }
    ok &= st.win_ms == 0 && st.win_reading_ms == 0 && r == (5000 * 25 + 500) / 1000;
    int gap = ok;

    /* bucket bounds: two cycles of share above (at least a token), a hundred
     * below, saturating */
    ok &= sm_delta_est_bucket_ceiling(0) == 1 && sm_delta_est_bucket_ceiling(5000) == 10000
       && sm_delta_est_bucket_ceiling(INT64_MAX - 1) == INT64_MAX;
    ok &= sm_delta_debt_floor(5000, 10000) == -500000 && sm_delta_debt_floor(0, 1) == -1
       && sm_delta_debt_floor(INT64_MAX / 4, INT64_MAX) == -INT64_MAX;
    if (!ok) {
      printf("  [FAIL] estimate: once %d window %d aligned %d lower %d idle %d cut %d constrained %d drop %d "
             "huge %d unlimited %d gap %d bounds %d\n",
             once, window, aligned, lower, idle, cut, constrained, dropped, huge, unlimited, gap, ok);
      failures++;
    } else {
      printf("  [ok] estimate: once per >=500 ms window, timestamp-aligned, starved-zero bound, idle holds, "
             "surplus cut+drain, constrained by starved cycles, demand-drop guard, unlimited rails, long gaps bounded; "
             "bucket 2 cycles up, 100 down\n");
    }
  }

  /* Sliding readings (sm_delta_est_control_avg): a 200 ms sample period,
   * each reading the mean of the last second, as NVML returns for a 1 s
   * lookback on an RTX 3090. Cycles every 100 ms, as in the tests above. */
  {
    int ok = 1, drain = 0;
    const int64_t total = 1000000;
    sm_delta_est_t st;
    int64_t r = 0;

    /* steady, starved: each window pairs the mean reading with the mean
     * consumption over the spans its samples cover -- the same C * u / m */
    memset(&st, 0, sizeof st);
    int64_t est = 0;
    for (int t = 1; t <= 30; t++) {
      uint64_t ts = (uint64_t)(t / 2) * 200;
      int32_t open = st.win_ms;
      r = sm_delta_est_control_avg(&st, total, 50, 25, 25, ts, 1000, t * 100, 5000, 0, 0,
                                   5000, DIV, 64, &drain);
      if (open > 0 && st.win_ms == 0) {
        est = r;   /* the cycle that closed a window */
      }
    }
    int steady = st.have_est && st.period_ms == 200 && est == 5000 * 50 / 25;
    ok &= steady;

    /* a 0 with the bucket dry acts on the sample if the device read 0 too
     * (idle because we hold it); not if another container had the GPU */
    int acted[2];
    for (int dev = 0; dev < 2; dev++) {
      memset(&st, 0, sizeof st);
      for (int t = 1; t <= 16; t++) {
        uint64_t ts = (uint64_t)(t / 2) * 200;
        sm_delta_est_control_avg(&st, total, 50, 25, 25, ts, 1000, t * 100, 5000, 0, 0,
                                 5000, DIV, 64, &drain);
      }
      int32_t before = st.win_ms;
      sm_delta_est_control_avg(&st, total, 50, 0, dev ? 40 : 0, 1800, 1000, 1700, 5000, 0, 0,
                               1, DIV, 64, &drain);
      acted[dev] = before < DELTA_EST_MIN_WINDOW_MS && st.win_ms == 0;
    }
    int zero = acted[0] && !acted[1];
    ok &= zero;

    /* a silence (a 3 s gap) does not stretch the period */
    memset(&st, 0, sizeof st);
    for (int t = 1; t <= 10; t++) {
      sm_delta_est_control_avg(&st, total, 50, 25, 25, (uint64_t)(t / 2) * 200, 1000, t * 100,
                               5000, 0, 0, 5000, DIV, 64, &drain);
    }
    sm_delta_est_control_avg(&st, total, 50, 25, 25, 4000, 1000, 4000, 5000, 0, 0, 5000, DIV, 64,
                             &drain);
    int silence = st.period_ms == 200;
    ok &= silence;

    if (!ok) {
      printf("  [FAIL] sliding estimate: steady %d (est %lld) zero-acts %d/%d silence %d (period %d) zero %d\n",
             steady, (long long)est, acted[0], acted[1], silence, st.period_ms, zero);
      failures++;
    } else {
      printf("  [ok] sliding estimate: span-paired C*u/m, zero acts only on an idle device, "
             "silences do not stretch the period\n");
    }
  }

  /* v16 charge: grid x block threads, saturating instead of wrapping. */
  {
    int ok = sm_launch_tokens(1, 1, 1, 1, 1, 1) == 1
          && sm_launch_tokens(2048, 1, 1, 256, 1, 1) == 2048 * 256
          && sm_launch_tokens(132, 1, 1, 384, 1, 1) == 132 * 384
          && sm_launch_tokens(0, 1, 1, 1024, 1, 1) == 0
          && sm_launch_tokens(4294967295ULL, 65535, 65535, 1024, 1, 1) == SM_LAUNCH_TOKENS_MAX
          && sm_launch_tokens(2147483647ULL, 65535, 1, 1024, 1, 1) == (int64_t)2147483647 * 65535 * 1024
          && sm_launch_tokens(65536, 65536, 1, 1024, 1, 1) == (int64_t)1 << 42;
    if (!ok) {
      printf("  [FAIL] sm_launch_tokens: wrong product or wrapped instead of saturating\n");
      failures++;
    } else {
      printf("  [ok] launch charge = grid x block threads, saturating at INT64_MAX/4\n");
    }
  }
}

/* ---- Part 2: closed loop ---- */

#define SIM_TICKS  8000
#define SIM_WARMUP 2200  /* past the cold-start full-bucket drain (1500 ticks
                          * at R=pool/1500), so MAE measures steady state */

typedef struct { const char *name; int64_t sm, thread; } card_t;

typedef struct { double mae, avg_util; } sim_result_t;

static sim_result_t simulate(const card_t *card, int up_limit, int64_t consume_rate,
                             int use_new, int delay) {
  int64_t total = card->sm * card->thread * 32;
  int64_t bucket = total; /* cold start full: the hard case */
  int64_t share = 0;
  int util_hist[8] = {0};
  double abs_err = 0, util_sum = 0;
  int counted = 0;

  for (int t = 0; t < SIM_TICKS; t++) {
    int64_t consumed = bucket < consume_rate ? bucket : consume_rate;
    bucket -= consumed;
    int util = (int)(consumed * 100 / consume_rate);

    int measured = util_hist[t % delay];
    util_hist[t % delay] = util;

    share = use_new
        ? sm_delta_step(total, up_limit, measured, share, bucket, t < 2, DIV, 64)
        : old_delta_step(card->sm, card->thread, total, up_limit, measured, share, 64);

    bucket += share;
    if (bucket > total) bucket = total;

    if (t >= SIM_WARMUP) {
      abs_err += util > up_limit ? util - up_limit : up_limit - util;
      util_sum += util;
      counted++;
    }
  }
  sim_result_t r = {abs_err / counted, util_sum / counted};
  return r;
}

static void test_closed_loop(void) {
  static const card_t cards[] = {
      {"MIG-7sm", 7, 1536},  {"RTX4080", 76, 1536}, {"A100", 108, 2048},
      {"H100", 132, 2048},   {"B-188sm", 188, 1536},
  };
  static const int limits[] = {8, 30, 50, 80};
  static const int rate_div[] = {1500, 150, 15};

  printf("\nclosed-loop MAE (old -> new), 2-tick delay, steady state:\n");
  printf("%-9s %5s | %-16s %-16s %-16s\n", "card", "limit",
         "R=pool/1500", "R=pool/150", "R=pool/15");

  double worst_new = 0, worst_reg = 0;
  for (size_t c = 0; c < sizeof(cards) / sizeof(cards[0]); c++) {
    for (size_t l = 0; l < sizeof(limits) / sizeof(limits[0]); l++) {
      printf("%-9s %5d |", cards[c].name, limits[l]);
      for (size_t rd = 0; rd < sizeof(rate_div) / sizeof(rate_div[0]); rd++) {
        int64_t total = cards[c].sm * cards[c].thread * 32;
        int64_t rate = total / rate_div[rd];
        if (rate < 1) rate = 1;
        sim_result_t o = simulate(&cards[c], limits[l], rate, 0, 2);
        sim_result_t n = simulate(&cards[c], limits[l], rate, 1, 2);
        printf("  %5.1f -> %4.1f", o.mae, n.mae);
        if (n.mae > worst_new) worst_new = n.mae;
        if (n.mae - o.mae > worst_reg) worst_reg = n.mae - o.mae;
      }
      printf("\n");
    }
  }

  /* Every cell must beat or match the old formula, and none may exceed the
   * ceiling the harness itself selected the constants for. */
  if (worst_reg > 0.5) {
    printf("  [FAIL] a cell regressed vs old by %.1f MAE\n", worst_reg);
    failures++;
  } else {
    printf("  [ok] no cell regresses vs old (worst delta %+.1f)\n", worst_reg);
  }
  if (worst_new > 16.0) {
    printf("  [FAIL] worst new-formula cell %.1f exceeds 16 MAE\n", worst_new);
    failures++;
  } else {
    printf("  [ok] worst new-formula cell %.1f (old pinned near 100%% in most light/medium cells)\n",
           worst_new);
  }

  /* HAMi #274 headline: 188 SM, hard_core=50, light grids. The old formula
   * must exhibit the pinning here (or the model lost the bug and every other
   * number is meaningless); the new one must hold the limit. */
  {
    const card_t *b = &cards[4];
    int64_t rate = b->sm * b->thread * 32 / 1500;
    sim_result_t o = simulate(b, 50, rate, 0, 2);
    sim_result_t n = simulate(b, 50, rate, 1, 2);
    printf("\nHAMi #274 (188 SM, hard_core=50, light grids): "
           "old avg util %.1f / MAE %.1f -> new avg util %.1f / MAE %.1f\n",
           o.avg_util, o.mae, n.avg_util, n.mae);
    if (o.avg_util < 90) {
      printf("  [FAIL] model no longer reproduces the old pinning -- test void\n");
      failures++;
    }
    if (n.mae > 16 || n.avg_util > 62) {
      printf("  [FAIL] new formula does not hold the limit\n");
      failures++;
    }
    if (o.avg_util >= 90 && n.mae <= 16 && n.avg_util <= 62) {
      printf("  [ok] old pins at ~100%%, new holds near target\n");
    }
  }

  /* Robustness the constants were tuned for: 4-tick measurement delay
   * (DELTA_REL_DAMPING exists for this -- share/2-scale steps compound
   * 1.5^4 against stale feedback and explode; the starved nudge stops at
   * damping 4 for the same reason) and a workload shift light->heavy
   * mid-run. */
  {
    const card_t *b = &cards[4];
    int64_t total = b->sm * b->thread * 32;
    sim_result_t d4 = simulate(b, 50, total / 150, 1, 4);
    if (d4.mae > 6.0) {
      printf("  [FAIL] 4-tick-delay MAE %.1f (budget 6)\n", d4.mae);
      failures++;
    } else {
      printf("  [ok] 4-tick delay holds: MAE %.1f\n", d4.mae);
    }

    int64_t bucket = total, share = 0;
    int hist[2] = {0};
    double err = 0; int cnt = 0;
    for (int t = 0; t < 9000; t++) {
      int64_t R = total / (t < 4500 ? 1500 : 15); /* light -> heavy */
      int64_t c2 = bucket < R ? bucket : R;
      bucket -= c2;
      int util = (int)(c2 * 100 / R);
      int m = hist[t % 2]; hist[t % 2] = util;
      share = sm_delta_step(total, 50, m, share, bucket, t < 2, DIV, 64);
      bucket += share; if (bucket > total) bucket = total;
      if (t >= 5000) { err += util > 50 ? util - 50 : 50 - util; cnt++; }
    }
    double trans = err / cnt;
    if (trans > 8.0) {
      printf("  [FAIL] light->heavy transition MAE %.1f (budget 8)\n", trans);
      failures++;
    } else {
      printf("  [ok] light->heavy workload shift re-converges: MAE %.1f\n", trans);
    }
  }

  /* Cold start, measured honestly: full bucket (the real init), share 0,
   * heavy load. The bucket carries full speed for ~15 ticks, the boot jump
   * then puts the share within striking distance, and the multiplicative
   * ramp closes the rest. The old formula "reaches target" faster only by
   * flooding -- its speed came from absolute pool-fraction steps, the
   * flooding mechanism that pinned it at 100% in the light/medium regimes.
   * (A REPEATED far-error boost was tried and rejected: damping 2 when
   * diff*2>target exploded the 6-tick-delay MAE from 3 to 45; the one-shot
   * jump gives the speed without joining the oscillation loop.) The budget
   * pins the trade so a future change cannot silently worsen it; the full
   * two-sided cold-start comparison is asserted per-cell in
   * test_delta_campaign.c. */
  {
    const card_t *b = &cards[4];
    int64_t total = b->sm * b->thread * 32;
    int64_t R = total / 15;
    int64_t bucket = total, share = 0;
    int hist[2] = {0};
    double deficit = 0;
    for (int t = 0; t < 200; t++) {
      int64_t c2 = bucket < R ? bucket : R;
      bucket -= c2;
      int util = (int)(c2 * 100 / R);
      int m = hist[t % 2]; hist[t % 2] = util;
      share = sm_delta_step(total, 50, m, share, bucket, t < 2, DIV, 64);
      bucket += share; if (bucket > total) bucket = total;
      if (util < 50) deficit += (50.0 - util) / 50;
    }
    if (deficit > 40.0) {
      printf("  [FAIL] cold-start throughput deficit %.1f tick-equivalents (budget 40)\n", deficit);
      failures++;
    } else {
      printf("  [ok] cold-start deficit %.1f tick-equivalents (one-time; boot jump active)\n",
             deficit);
    }
  }
}

int main(void) {
  test_invariants();
  test_closed_loop();
  if (failures != 0) {
    printf("\n%d check(s) FAILED\n", failures);
    return 1;
  }
  printf("\nall delta step checks passed\n");
  return 0;
}
