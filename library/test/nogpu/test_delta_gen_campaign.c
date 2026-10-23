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
 * The delta controller GENERATIONAL campaign. No GPU, no driver.
 *
 * Each controller generation is held against a frozen replica of the one it
 * replaces (test/nogpu/delta_sim.h). test_delta_campaign.c did it for the
 * first generation against the removed sm^2 formula; this campaign does it
 * from v14 on, over a far wider load range and through the watcher's real
 * cycle, both start paths:
 *
 *   v14 vs v13: finer seed, escape, 1-token trim (HAMi-core #360)
 *   v15 vs v14: consumption-observed cut (sm_delta_observe / sm_delta_control)
 *
 * Results per generation are in docs/sm_delta_validation.md.
 *
 * WHY THE LOAD RANGE IS WIDE
 *
 * A real workload's per-cycle consumption follows from what it launches;
 * delta_sim.h gives the mapping (a 10 ms GEMM at tpb 1024 is pool/3277 per
 * 100 ms cycle, a 100 ms kernel pool/32768). test_delta_campaign.c sweeps
 * pool/1500, /150 and /15 only, and the generation it accepted bang-banged
 * between 0 and 100% just below that range. And the range drifts: a small
 * kernel's grid is fixed by its problem size, not by the card, so on a part
 * with more SMs its occupancy -- and its R/pool -- shrinks. Each hardware
 * generation pushes the same software further toward the light end.
 *
 * WHY THE CARD LIST INCLUDES PARTS THAT DO NOT EXIST
 *
 * In pool units the closed loop is card-independent except for integer
 * effects: every controller constant is a fraction of the pool, and so is R.
 * The synthetic parts (up to 1024 SMs x 4096 threads/SM, a pool of 134M
 * tokens) check that nothing in the arithmetic breaks as the pool grows.
 * This component serves every mainstream part and whatever comes next;
 * nothing here may be tuned to one model.
 *
 * AXES
 *   cards  : every mainstream/top NVIDIA part since Pascal, MIG slices, and
 *            synthetic future parts (same-shape parts run once)
 *   load   : R/pool log-spaced 4 per decade over [1e-7, 2], plus the three
 *            bands of test_delta_campaign.c for continuity. R is an integer
 *            token count, floored at 1 -- real token accounting is integer
 *            and the lightest loads on small pools hit that floor for real.
 *   limits : 1..99 (100 is not limited)
 *   delays : 2 and 4 cycles of measurement lag
 *   start  : empty bucket, as g_dev_hot's zero-init and a fresh shared region
 *            give; soft-limit start path here, hard-limit (the default Fixed
 *            policy: bypass while util is low) in test_delta_gen_hard.c.
 *
 * ACCEPTANCE. On the three legacy loads the bar is test_delta_campaign.c's,
 * cell by cell: steady MAE not worse by more than 0.5 util-points, p95 not
 * worse by more than 1, cold-start (two-sided MAE over the first 300 ticks)
 * not worse by more than 1.5 -- in ANY cell.
 *
 * On the wide range the bar is per REGION -- one load decade x one limit
 * band (1-9, 10-49, 50-89, 90-99), both delays, every card -- plus a hard
 * cap on every single cell:
 *   - region mean steady MAE, p95 and cold-start: not worse than the
 *     previous generation's (beyond 0.05 util-points), in ANY region
 *   - single cell: steady MAE not worse by more than 1.0, p95 by more than
 *     2, cold-start by more than 2.0
 * Why not cell by cell there: both generations have integer limit cycles
 * (1% util quantization against a whole-token bucket) that switch on and off
 * at isolated loads. Sampling the load axis finely enough to cover future
 * hardware lands some samples on the new generation's cycles and the old
 * one's quiet points. At u=99 with a 4-tick delay, for example, the new
 * generation's mean MAE over a 5%-step load sweep is 4-8x lower on every
 * card, yet a handful of single loads read 0.06 -> 0.6. Chasing those
 * moves the cycles to other loads; it does not remove them. The region
 * mean catches any systematic regression; the cap bounds the isolated ones.
 *
 * Usage: test_delta_gen_campaign [inc_divisor]   (default: the shipped one)
 * The argument exists to search candidates; CI runs it bare.
 */

#include <math.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "include/hook.h"
#include "include/sm_delta.h"
#include "test/nogpu/delta_sim.h"

/* GEN_START_HARD selects the hard-limit start path (test_delta_gen_hard.c
 * includes this file with it set); the default is the soft path. */
#ifndef GEN_START_HARD
#define GEN_START_HARD 0
#endif

/* Per-cell cap RATCHET: cells beyond the cap, and the worst excess, may not
 * grow. v15 (consumption-observed cut) leaves 13 on the hard path, all at
 * R/pool 5.6e-5..1e-4 and targets 51-54 with a 4-cycle delay: v14's own
 * seed-growth limit cycle entered at a different phase after the much
 * better cold start (49 -> 10 there), steady MAE +1.1..+1.7, p95 +3..+5.
 * docs/sm_delta_validation.md section 8. Lower these when a generation
 * clears cells; never raise them to let a change pass. */
#if GEN_START_HARD
#define CAP_BUDGET_CELLS 13
#define CAP_BUDGET_MAE   1.70
#define CAP_BUDGET_P95   5
#else
#define CAP_BUDGET_CELLS 0
#define CAP_BUDGET_MAE   1.0
#define CAP_BUDGET_P95   2
#endif
#define CAP_BUDGET_COLD  2.0

#define WARM  2000
#define STEADY 3000
#define COLD  300

typedef struct { double mae, p95, cold; } res_t;

static res_t run_cell(int64_t total, int64_t R, int u, int delay, int use_new,
                      int inc_divisor) {
  plant_t p;
  memset(&p, 0, sizeof(p));
  double err = 0, cold = 0;
  int h[101] = {0};

  for (int t = 0; t < WARM + STEADY; t++) {
    int util = plant_cycle(&p, total, R, u, delay, t, use_new, GEN_START_HARD, inc_divisor);
    int dev = util > u ? util - u : u - util;
    if (t < COLD) {
      cold += dev;
    }
    if (t >= WARM) {
      err += dev;
      h[dev > 100 ? 100 : dev]++;
    }
  }
  res_t r;
  r.mae = err / STEADY;
  r.cold = cold / COLD;
  int acc = 0;
  r.p95 = 0;
  for (int i = 0; i <= 100; i++) {
    acc += h[i];
    if (acc >= STEADY * 95 / 100) {
      r.p95 = i;
      break;
    }
  }
  return r;
}

/* R/pool grid: 4 per decade over [1e-7, 2], then the three legacy bands. */
#define NLOAD_LOG 30
static double LOADS[NLOAD_LOG + 3];
static int NLOAD;

static void build_loads(void) {
  NLOAD = 0;
  for (int i = 0; i < NLOAD_LOG; i++) {
    LOADS[NLOAD++] = pow(10.0, -7.0 + i * 0.25);   /* 1e-7 .. 10^0.25 */
  }
  LOADS[NLOAD++] = 1.0 / 1500;
  LOADS[NLOAD++] = 1.0 / 150;
  LOADS[NLOAD++] = 1.0 / 15;
}

/* Limit bands of a region; a load decade is the other axis. */
static const int BAND_LO[] = {1, 10, 50, 90};
static const int BAND_HI[] = {9, 49, 89, 99};
#define NBAND 4
/* Region means are averages over ~10^4 cells; 0.05 util-points is 1/30 of
 * the per-cell cold-start bar -- "not worse", not "bit-identical". */
#define REGION_TOL 0.05
#define NDEC 8 /* decades 1e-7 .. 1e0 (the top two samples join the last) */

typedef struct { double o_mae, n_mae, o_p95, n_p95, o_cold, n_cold; long n; } agg_t;

static int decade_of(int li) {
  int d = li / 4;
  return d >= NDEC ? NDEC - 1 : d;
}

typedef struct { double d; char at[200]; } worst_t;

/* Everything one card contributes; cards run in parallel and are merged. */
typedef struct {
  long better, worse, pb, pw, cb, cw, cells, legacy_fail, cap_fail;
  double sum_o, sum_n;
  worst_t wm, wp, wc;
  agg_t reg[NDEC][NBAND];
} stats_t;

static int g_inc_divisor, g_verbose;
static int g_todo[NCARD], g_ntodo, g_next;
static stats_t g_total;
static pthread_mutex_t g_mu = PTHREAD_MUTEX_INITIALIZER;

static void note_worst(worst_t *w, double d, const char *card, int li, int u, int delay,
                       double o, double n) {
  if (d > w->d) {
    w->d = d;
    snprintf(w->at, sizeof(w->at), "%s R/pool=%.2e u=%d d%d %.2f->%.2f", card, LOADS[li], u,
             delay, o, n);
  }
}

static void run_card(int ci, stats_t *st) {
  static const int delays[] = {2, 4};
  int64_t total = (int64_t)CARDS[ci].sm * CARDS[ci].th * 32;
  for (int li = 0; li < NLOAD; li++) {
    int legacy = li >= NLOAD_LOG;
    int64_t R = (int64_t)llround((double)total * LOADS[li]);
    if (R < 1) {
      R = 1;
    }
    for (int u = 1; u <= 99; u++) {
      int band = u < 10 ? 0 : u < 50 ? 1 : u < 90 ? 2 : 3;
      for (int di = 0; di < 2; di++) {
        res_t o = run_cell(total, R, u, delays[di], 0, g_inc_divisor);
        res_t n = run_cell(total, R, u, delays[di], 1, g_inc_divisor);
        double dm = n.mae - o.mae, dp = n.p95 - o.p95, dc = n.cold - o.cold;
        st->cells++;
        st->sum_o += o.mae;
        st->sum_n += n.mae;
        st->better += dm < -0.5;
        st->worse += dm > 0.5;
        st->pb += dp < -1;
        st->pw += dp > 1;
        st->cb += dc < -1.5;
        st->cw += dc > 1.5;
        int fail = legacy ? (dm > 0.5 || dp > 1 || dc > 1.5)
                          : (dm > 1.0 || dp > 2 || dc > 2.0);
        if (fail) {
          if (legacy) {
            st->legacy_fail++;
          } else {
            st->cap_fail++;
          }
        }
        if (g_verbose && (dm > 0.5 || dp > 1 || dc > 1.5)) {
          printf("    cell %s R/pool=%.2e u=%d d%d mae %.2f->%.2f p95 %.0f->%.0f cold %.2f->%.2f%s\n",
                 CARDS[ci].n, LOADS[li], u, delays[di], o.mae, n.mae, o.p95, n.p95, o.cold,
                 n.cold, fail ? "  <-- FAIL" : "");
        }
        note_worst(&st->wm, dm, CARDS[ci].n, li, u, delays[di], o.mae, n.mae);
        note_worst(&st->wp, dp, CARDS[ci].n, li, u, delays[di], o.p95, n.p95);
        note_worst(&st->wc, dc, CARDS[ci].n, li, u, delays[di], o.cold, n.cold);
        if (!legacy) {
          agg_t *g = &st->reg[decade_of(li)][band];
          g->o_mae += o.mae;
          g->n_mae += n.mae;
          g->o_p95 += o.p95;
          g->n_p95 += n.p95;
          g->o_cold += o.cold;
          g->n_cold += n.cold;
          g->n++;
        }
      }
    }
  }
}

static void merge_worst(worst_t *into, const worst_t *w) {
  if (w->d > into->d) {
    *into = *w;
  }
}

static void *worker(void *arg) {
  (void)arg;
  for (;;) {
    int k = __atomic_fetch_add(&g_next, 1, __ATOMIC_RELAXED);
    if (k >= g_ntodo) {
      return NULL;
    }
    static __thread stats_t st;
    memset(&st, 0, sizeof(st));
    run_card(g_todo[k], &st);
    pthread_mutex_lock(&g_mu);
    stats_t *t = &g_total;
    t->better += st.better;
    t->worse += st.worse;
    t->pb += st.pb;
    t->pw += st.pw;
    t->cb += st.cb;
    t->cw += st.cw;
    t->cells += st.cells;
    t->legacy_fail += st.legacy_fail;
    t->cap_fail += st.cap_fail;
    t->sum_o += st.sum_o;
    t->sum_n += st.sum_n;
    merge_worst(&t->wm, &st.wm);
    merge_worst(&t->wp, &st.wp);
    merge_worst(&t->wc, &st.wc);
    for (int d = 0; d < NDEC; d++) {
      for (int b = 0; b < NBAND; b++) {
        agg_t *g = &t->reg[d][b], *s = &st.reg[d][b];
        g->o_mae += s->o_mae;
        g->n_mae += s->n_mae;
        g->o_p95 += s->o_p95;
        g->n_p95 += s->n_p95;
        g->o_cold += s->o_cold;
        g->n_cold += s->n_cold;
        g->n += s->n;
      }
    }
    pthread_mutex_unlock(&g_mu);
  }
}

int main(int argc, char **argv) {
  g_inc_divisor = argc > 1 ? atoi(argv[1]) : DELTA_INCREMENT_DIVISOR_DEFAULT;
  g_verbose = getenv("GEN_VERBOSE") != NULL;
  build_loads();
  printf("generational campaign (%s start): shipped cycle, inc_divisor=%d, vs frozen previous\n",
         GEN_START_HARD ? "hard-limit" : "soft-limit", g_inc_divisor);

  /* In pool units only sm*th matters; same-shape parts are one cell. */
  for (int ci = 0; ci < NCARD; ci++) {
    int dup = 0;
    for (int cj = 0; cj < ci; cj++) {
      dup |= CARDS[cj].sm == CARDS[ci].sm && CARDS[cj].th == CARDS[ci].th;
    }
    if (!dup) {
      g_todo[g_ntodo++] = ci;
    }
  }
  /* One card per task. The grid is ~2 CPU-minutes, so it has to spread
   * over the cores to fit make test-nogpu's per-test timeout. */
  long ncpu = sysconf(_SC_NPROCESSORS_ONLN);
  int nthr = ncpu < 1 ? 1 : ncpu > 64 ? 64 : (int)ncpu;
  pthread_t th[64];
  for (int i = 0; i < nthr; i++) {
    if (pthread_create(&th[i], NULL, worker, NULL) != 0) {
      nthr = i;
      break;
    }
  }
  if (nthr == 0) {
    worker(NULL);
  }
  for (int i = 0; i < nthr; i++) {
    pthread_join(th[i], NULL);
  }

  stats_t *t = &g_total;
  printf("  %d distinct card shapes (of %d listed) x %d loads x limits 1-99 x 2 delays = %ld cells\n",
         g_ntodo, NCARD, NLOAD, t->cells);
  printf("  steady MAE  : %ld better, %ld worse by >0.5; aggregate %.0f -> %.0f (%.2fx)\n",
         t->better, t->worse, t->sum_o, t->sum_n, t->sum_o / t->sum_n);
  printf("  p95 sawtooth: %ld better, %ld worse by >1\n", t->pb, t->pw);
  printf("  cold-start  : %ld better, %ld worse by >1.5\n", t->cb, t->cw);
  printf("  worst cells : steady %+.2f (%s)\n", t->wm.d, t->wm.at);
  printf("                p95    %+.0f (%s)\n", t->wp.d, t->wp.at);
  printf("                cold   %+.2f (%s)\n", t->wc.d, t->wc.at);

  int region_fail = 0;
  printf("\n  region means, previous > candidate (steady MAE | p95 | cold-start):\n");
  printf("  %-8s", "R/pool");
  for (int b = 0; b < NBAND; b++) {
    char h[32];
    snprintf(h, sizeof(h), "limit %d-%d", BAND_LO[b], BAND_HI[b]);
    printf("  %-31s", h);
  }
  printf("\n");
  for (int d = 0; d < NDEC; d++) {
    printf("  1e%-+6d", d - 7);
    for (int b = 0; b < NBAND; b++) {
      agg_t *g = &t->reg[d][b];
      double k = (double)g->n;
      int bad = (g->n_mae - g->o_mae) / k > REGION_TOL || (g->n_p95 - g->o_p95) / k > REGION_TOL ||
                (g->n_cold - g->o_cold) / k > REGION_TOL;
      region_fail += bad;
      printf("  %4.1f>%4.1f|%4.1f>%4.1f|%4.1f>%4.1f%s", g->o_mae / k, g->n_mae / k,
             g->o_p95 / k, g->n_p95 / k, g->o_cold / k, g->n_cold / k, bad ? "!" : " ");
    }
    printf("\n");
  }

  int failures = 0;
  if (t->legacy_fail) {
    printf("[FAIL] legacy loads: %ld cells beyond test_delta_campaign.c's per-cell bar\n",
           t->legacy_fail);
    failures++;
  } else {
    printf("[ok] legacy loads: no cell beyond the per-cell bar (0.5 / 1 / 1.5)\n");
  }
  if (region_fail) {
    printf("[FAIL] %d regions (marked !) worse than the previous generation on average\n",
           region_fail);
    failures++;
  } else {
    printf("[ok] every region at least as good as the previous generation on average (tol %.2f)\n",
           REGION_TOL);
  }
  if (t->cap_fail > CAP_BUDGET_CELLS || t->wm.d > CAP_BUDGET_MAE || t->wp.d > CAP_BUDGET_P95 ||
      t->wc.d > CAP_BUDGET_COLD) {
    printf("[FAIL] single-cell cap (1.0 / 2 / 2.0) ratchet: %ld cells beyond (budget %d), worst "
           "%+.2f / %+.0f / %+.2f (budget %+.2f / %+d / %+.2f)\n",
           t->cap_fail, CAP_BUDGET_CELLS, t->wm.d, t->wp.d, t->wc.d, CAP_BUDGET_MAE,
           CAP_BUDGET_P95, CAP_BUDGET_COLD);
    failures++;
  } else {
    printf("[ok] single-cell cap (1.0 / 2 / 2.0) ratchet: %ld cells beyond (budget %d)\n",
           t->cap_fail, CAP_BUDGET_CELLS);
  }
  if (failures) {
    printf("\n%d generational criteria FAILED\n", failures);
    return 1;
  }
  printf("\ngenerational campaign passed\n");
  return 0;
}
