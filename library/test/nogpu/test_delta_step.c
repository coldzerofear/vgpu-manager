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
 * chosen, so it is also what pins them.
 *
 * Part 1: invariants of the shipped sm_delta_step() -- SM-independence of the
 * step fraction, seed granularity below light-grid consumption (the pinning
 * criterion), the grow cap, the blowout-gated emergency cut floor, mildness
 * of the in-band cut, and the share clamps.
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
   * 188-SM card -- the property whose absence was the #274 bug. */
  {
    int64_t small = 7LL * 1536 * 32, big = 188LL * 1536 * 32;
    int ok = 1;
    for (int u = 10; u <= 80 && ok; u += 10) {
      for (int util = 0; util <= 100 && ok; util += 5) {
        int64_t a = sm_delta_step(small, u, util, small / 2, DIV, 64) - small / 2;
        int64_t b = sm_delta_step(big, u, util, big / 2, DIV, 64) - big / 2;
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
    int64_t min_step = sm_delta_step(total, 50, 50, 0, DIV, 64);
    if (min_step * 4 >= total / 1200) {
      printf("  [FAIL] seed %ld not well below light-grid rate %ld\n",
             (long)min_step, (long)(total / 1200));
      failures++;
    } else {
      printf("  [ok] seed %ld (%.4f%% of pool) well under light-grid rate %ld\n",
             (long)min_step, 100.0 * min_step / total, (long)(total / 1200));
    }
  }

  /* Grow never exceeds pool/10, from any share. */
  {
    int64_t total = 188LL * 1536 * 32;
    int ok = 1;
    for (int u = 1; u <= 100 && ok; u++) {
      for (int util = 0; util <= u && ok; util++) {
        for (int64_t s = 0; s <= total && ok; s += total / 7) {
          int64_t step = sm_delta_step(total, u, util, s, DIV, 64) - s;
          if (step > total / DELTA_GROW_CAP_DIVISOR) ok = 0;
        }
      }
    }
    if (!ok) { printf("  [FAIL] a grow step exceeded pool/%d\n", DELTA_GROW_CAP_DIVISOR); failures++; }
    else printf("  [ok] grow step <= pool/%d always\n", DELTA_GROW_CAP_DIVISOR);
  }

  /* Emergency floor: at u=8, util=90 (blowout: raw_diff 82 > 8) the floored
   * cut -- total*82/(8*64) = 16% of the pool -- exceeds any realistic share,
   * so the share must be cut to 0 outright. The successor of the
   * hard_core=8-pinned-at-15 protection. */
  {
    int64_t total = 76LL * 1536 * 32;
    int64_t share = total / 100;
    if (sm_delta_step(total, 8, 90, share, DIV, 64) != 0) {
      printf("  [FAIL] blowout cut did not clamp the share to 0\n");
      failures++;
    } else {
      printf("  [ok] blowout cut clamps the share (emergency floor active)\n");
    }
  }

  /* In-band cut stays gentle: at u=80, util=100 (raw_diff 20, NOT a blowout)
   * the cut must be share-proportional -- share*20/(6*80) or the seed --
   * never the absolute floor. Cutting harder than this is what caused the
   * u=80 oscillation regression during tuning. */
  {
    int64_t total = 188LL * 1536 * 32;
    int64_t share = total / 100;
    int64_t cut = share - sm_delta_step(total, 80, 100, share, DIV, 64);
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

  /* Share clamps to [0, total]. */
  {
    int64_t total = 76LL * 1536 * 32;
    if (sm_delta_step(total, 50, 0, total, DIV, 64) != total ||
        sm_delta_step(total, 10, 99, 1, DIV, 64) != 0) {
      printf("  [FAIL] share clamp to [0, total] broken\n");
      failures++;
    } else {
      printf("  [ok] share clamps to [0, total]\n");
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
        ? sm_delta_step(total, up_limit, measured, share, DIV, 64)
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
  if (worst_reg > 1.0) {
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
   * 1.5^4 against stale feedback and explode) and a workload shift
   * light->heavy mid-run. */
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
      share = sm_delta_step(total, 50, m, share, DIV, 64);
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
