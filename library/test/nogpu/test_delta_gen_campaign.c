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
 * test_delta_campaign.c proved the current controller beats the removed sm^2
 * formula. This campaign holds the shipped sm_delta_step() to the same bar
 * against a frozen replica of the generation it replaces, over a far wider
 * load range -- because the narrow range was where the previous campaign
 * stopped looking, and where the generation it accepted still failed.
 *
 * WHY THE LOAD RANGE HAD TO WIDEN
 *
 * test_delta_campaign.c sweeps three loads, consumption pool/1500, pool/150
 * and pool/15 per tick. A real workload's consumption follows from what it
 * launches, and the limiter charges a launch its grid size. For a kernel that
 * fills the GPU for a wave of wave_us microseconds with tpb threads per block,
 * at 100% utilization:
 *
 *     R / pool = (10000 / wave_us) * occupancy / (32 * tpb)
 *
 * which is independent of the card (both R and the pool scale with
 * sm * threads_per_sm). A long-running GEMM -- wave 10 ms, tpb 1024 -- lands at
 * pool/32768, twenty times lighter than the lightest band tested. There the
 * previous generation's granularity seed (pool*5/81920) is twice the entire
 * demand: share can only bang between 0 and seed multiples, and on an A100 at
 * limit 25 the workload runs at 100% for 80% of ticks. That is HAMi-core
 * issue #360, present in our controller and invisible to its campaign.
 *
 * WHY THE CARD LIST INCLUDES PARTS THAT DO NOT EXIST
 *
 * In pool units the closed loop is card-independent except for integer
 * effects: every controller constant is a fraction of the pool, and so is R.
 * What changes with hardware is WHERE workloads land. A small kernel's grid is
 * fixed by its problem size, not by the card, so on a part with more SMs its
 * occupancy -- and its R/pool -- shrinks. Each hardware generation pushes the
 * same software further toward the light end. The synthetic parts below (up to
 * 1024 SMs x 4096 threads/SM, a pool of 134M tokens) check that nothing in the
 * arithmetic breaks as the pool grows, and the light end of the load range is
 * swept deep enough to hold the small kernels of those parts. This component
 * serves every mainstream part and whatever comes next; nothing here may be
 * tuned to one model.
 *
 * AXES
 *   cards  : every mainstream/top NVIDIA part since Pascal, MIG slices, and
 *            synthetic future parts
 *   load   : R/pool log-spaced 4 per decade over [1e-7, 2], plus the three
 *            bands of test_delta_campaign.c for continuity. R is an integer
 *            token count, floored at 1 -- real token accounting is integer
 *            and the lightest loads on small pools hit that floor for real.
 *   limits : 1..99 (100 is not limited)
 *   delays : 2 and 4 ticks of measurement lag
 *   start  : empty bucket, which is what g_dev_hot's zero-init does. The
 *            full-bucket start test_delta_campaign.c uses is not tractable at
 *            the light end (draining a full bucket at pool/1e7 per tick takes
 *            ten million ticks); test_delta_campaign.c keeps covering it on
 *            the range where it is.
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

typedef struct { const char *n; int sm, th; } card_t;
static const card_t CARDS[] = {
  /* Pascal / Volta */
  {"GTX1050",5,2048},{"GTX1060",10,2048},{"GTX1070",15,2048},{"GTX1080",20,2048},
  {"GTX1080Ti",28,2048},{"P40",30,2048},{"P100",56,2048},
  {"TitanV",80,2048},{"V100",80,2048},{"V100S",80,2048},{"V100-32g",80,2048},
  /* Turing (1024 threads/SM) */
  {"GTX1650",14,1024},{"GTX1660",22,1024},{"GTX1660Ti",24,1024},{"T4",40,1024},
  {"RTX2060",30,1024},{"RTX2070",36,1024},{"RTX2080",46,1024},{"RTX2080Ti",68,1024},
  /* Ampere consumer / pro (1536) */
  {"RTX3050",20,1536},{"RTX3060",28,1536},{"RTX3070",46,1536},{"RTX3070Ti",48,1536},
  {"RTX3080",68,1536},{"RTX3080Ti",80,1536},{"RTX3090",82,1536},
  {"A2",10,1536},{"A10",72,1536},{"A40",84,1536},
  {"RTXA4000",48,1536},{"RTXA5000",64,1536},{"RTXA6000",84,1536},
  /* Ampere datacenter (2048) incl. MIG */
  {"A100-1g",14,2048},{"A30",56,2048},{"A800",108,2048},{"A100",108,2048},
  /* Ada (1536) */
  {"RTX4060",24,1536},{"RTX4070",46,1536},{"RTX4070Ti",60,1536},{"RTX4080",76,1536},
  {"RTX4090",128,1536},{"L4",58,1536},{"L20",92,1536},{"L40",142,1536},
  {"L40S",142,1536},{"RTX6000Ada",142,1536},
  /* Hopper (2048) incl. MIG and PCIe */
  {"H100-1g",16,2048},{"H20",78,2048},{"H100-PCIe",114,2048},{"H800",132,2048},
  {"H100",132,2048},{"H200",132,2048},
  /* Blackwell datacenter (2048) / consumer and pro (1536) */
  {"B200",148,2048},{"B300",160,2048},
  {"RTX5060",36,1536},{"RTX5070",50,1536},{"RTX5070Ti",70,1536},{"RTX5080",84,1536},
  {"RTX5090",170,1536},{"RTXPRO6000",188,1536},
  /* Synthetic future parts: more SMs, and twice today's threads per SM. */
  {"FUT-256x2048",256,2048},{"FUT-384x2048",384,2048},
  {"FUT-512x4096",512,4096},{"FUT-1024x4096",1024,4096},
};
#define NCARD (int)(sizeof(CARDS) / sizeof(CARDS[0]))

/* ---- The previous generation, frozen verbatim from include/sm_delta.h as of
 * the commit that introduced this campaign. Every constant it reads is frozen
 * with it: the shipped header may change any of them, and a baseline that
 * moved with the code under test would not be a baseline. ---- */
#define PREV_MIN_INCREMENT     5
#define PREV_INC_DIVISOR       81920
#define PREV_GROW_CAP_DIVISOR  10
#define PREV_REL_DAMPING       6
#define PREV_STARVED_DAMPING   4
#define PREV_BOOT_JUMP_DIVISOR 6400
#define PREV_RAMP_DIVISOR      64

static int64_t prev_delta_step(int64_t total, int up_limit, int user_current,
                               int64_t share, int64_t bucket, int boot) {
  if (up_limit >= 100) {
    return total;
  }
  int raw_diff = abs(up_limit - user_current);
  int64_t up = up_limit > 0 ? up_limit : 1;
  int64_t seed = total * PREV_MIN_INCREMENT / PREV_INC_DIVISOR;
  if (seed < 1) {
    seed = 1;
  }
  if (user_current <= up_limit) {
    if (boot) {
      int64_t jump = total * up / PREV_BOOT_JUMP_DIVISOR;
      if (jump > total / 64) {
        jump = total / 64;
      }
      if (share < jump) {
        return jump;
      }
    }
    int damp = bucket <= 0 ? PREV_STARVED_DAMPING : PREV_REL_DAMPING;
    int64_t increment = share * (int64_t)raw_diff / (damp * up);
    if (increment < seed) {
      increment = seed;
    }
    int64_t grow_cap = total / PREV_GROW_CAP_DIVISOR;
    if (increment > grow_cap) {
      increment = grow_cap;
    }
    return (share + increment) > total ? total : (share + increment);
  }
  if (raw_diff > up_limit && raw_diff >= PREV_MIN_INCREMENT) {
    int64_t increment = share * (int64_t)raw_diff / (PREV_REL_DAMPING * up);
    if (increment < seed) {
      increment = seed;
    }
    int64_t ramp_floor = total * (int64_t)raw_diff / (up * PREV_RAMP_DIVISOR);
    if (increment < ramp_floor) {
      increment = ramp_floor;
    }
    if (increment > total) {
      increment = total;
    }
    return (share - increment) < 0 ? 0 : (share - increment);
  }
  int64_t backlog = bucket > 0 ? bucket : 0;
  int64_t prop = share * (int64_t)raw_diff / (PREV_REL_DAMPING * up);
  int64_t by_cause = (int64_t)((__int128)prop * share / (share + backlog + 1));
  int64_t by_error = prop * (int64_t)raw_diff / 100;
  int64_t increment = by_cause > by_error ? by_cause : by_error;
  return (share - increment) < 0 ? 0 : (share - increment);
}

#define WARM  2000
#define STEADY 3000
#define COLD  300

typedef struct { double mae, p95, cold; } res_t;

static res_t run_cell(int64_t total, int64_t R, int u, int delay, int use_new,
                      int inc_divisor) {
  int64_t bucket = 0, share = 0;
  int hist[8] = {0};
  double err = 0, cold = 0;
  int h[101] = {0};

  for (int t = 0; t < WARM + STEADY; t++) {
    int64_t c = bucket < R ? bucket : R;
    bucket -= c;
    int util = (int)(c * 100 / R);
    int m = hist[t % delay];
    hist[t % delay] = util;
    share = use_new
        ? sm_delta_step(total, u, m, share, bucket, t < 2, inc_divisor, PREV_RAMP_DIVISOR)
        : prev_delta_step(total, u, m, share, bucket, t < 2);
    bucket += share;
    if (bucket > total) {
      bucket = total;
    }
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
  printf("generational campaign: candidate inc_divisor=%d vs frozen previous (%d)\n",
         g_inc_divisor, PREV_INC_DIVISOR);

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
  if (t->cap_fail) {
    printf("[FAIL] %ld cells beyond the single-cell cap (1.0 / 2 / 2.0)\n", t->cap_fail);
    failures++;
  } else {
    printf("[ok] no single cell beyond the cap (1.0 / 2 / 2.0)\n");
  }
  if (failures) {
    printf("\n%d generational criteria FAILED\n", failures);
    return 1;
  }
  printf("\ngenerational campaign passed\n");
  return 0;
}
