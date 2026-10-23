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
 * Workloads that change. No GPU, no driver.
 *
 * The generational campaigns hold one load per cell; this holds the shipped
 * cycle against the frozen previous generation (test/nogpu/delta_sim.h) when
 * the load moves, which is where a controller's windup lives:
 *
 *   DROP   heavy -> light mid-run (training -> eval, a batch-size cut). A
 *          share tuned for the heavy load over-supplies the light one; the
 *          surplus piles up in the bucket and util reads 100 until it
 *          drains. Above a 50% target the previous generation took minutes.
 *   RISE   light -> heavy: the share must climb without overshooting.
 *   BURST  on/off duty cycles (data loading, request-driven inference). The
 *          idle cycles look like over-supply to a careless rule; cutting
 *          there starves the next burst.
 *
 * Every class runs on every distinct card shape, both delays, both start
 * paths. Metrics: MAE over the 60 s (600 cycles) after a DROP/RISE switch,
 * on-phase MAE for BURST, and for DROP the seconds spent 15+ points over the
 * target in the 10 minutes after the switch.
 *
 * ACCEPTANCE -- the wide-range bar of test_delta_gen_campaign.c:
 *   - no cell worse than the previous generation by more than 2.0 (ratchet
 *     below)
 *   - no class mean worse (beyond 0.05)
 *   - DROP: total over-target time strictly lower (the reason v15 exists)
 *
 * The per-cell bar is a RATCHET where a generation leaves known residuals:
 * the count of cells beyond it and the worst excess may not grow. v15's are
 * on/off workloads at targets >= 50 whose slow cut (util can never exceed
 * twice the target there) the ceiling's phase change exposes; see
 * docs/sm_delta_validation.md. Lower the budgets when a generation clears
 * cells; never raise them to make a change pass.
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

#define SWITCH_AT   4000
#define TRANSIENT   600
#define AFTER       6000
#define CELL_CAP    2.0
#define CLASS_TOL   0.05

/* The ratchet (see the header): cells beyond CELL_CAP and the worst excess,
 * per class. v15's BURST residuals are all on the hard path: on20/off5 at
 * pool/1500 and targets 60-90 (81 cells, worst +11.9), pool/15 on3/off7 at
 * target 30 (47), pool/15 on20/off5 at 70 (46), 45 scattered. */
static const long   BUDGET_CELLS[] = {0, 0, 219};
static const double BUDGET_WORST[] = {CELL_CAP, CELL_CAP, 11.95};

enum { DROP, RISE, BURST, NCLASS };
static const char *CLASS_NAME[NCLASS] = {"DROP ", "RISE ", "BURST"};

/* heavy, light pairs in R/pool per 100 ms cycle */
static const double PAIRS[][2] = {
  {1.0 / 15, 1.0 / 150}, {1.0 / 15, 1.0 / 1500}, {1.0 / 150, 1.0 / 1500},
  {1.0 / 150, 1.0 / 15000}, {1.0 / 1500, 1.0 / 15000},
};
#define NPAIR (int)(sizeof(PAIRS) / sizeof(PAIRS[0]))
static const int DUTY[][2] = {{1, 1}, {2, 2}, {3, 7}, {5, 5}, {10, 30}, {50, 50}, {20, 5}};
#define NDUTY (int)(sizeof(DUTY) / sizeof(DUTY[0]))
static const double BURST_LOAD[] = {1.0 / 15, 1.0 / 150, 1.0 / 1500};

typedef struct {
  long n[NCLASS], over_cap[NCLASS];
  double o[NCLASS], c[NCLASS], worst[NCLASS];
  char at[NCLASS][160];
  double drop_over_o, drop_over_c; /* seconds 15+ over target, summed */
} stats_t;

/* One run; returns the metric MAE, and for DROP the over-target seconds. */
static double run(int cls, int64_t total, double a, double b, int on, int off, int u, int delay,
                  int candidate, int hard, double *over_s) {
  plant_t p;
  memset(&p, 0, sizeof(p));
  int64_t Ra = llround(total * a), Rb = llround(total * b);
  if (Ra < 1) Ra = 1;
  if (Rb < 1) Rb = 1;
  int ticks = cls == BURST ? 1000 + AFTER : SWITCH_AT + AFTER;
  double err = 0;
  long n = 0, over = 0;
  for (int t = 0; t < ticks; t++) {
    int64_t R;
    if (cls == BURST) {
      R = (t % (on + off)) < on ? Ra : 0;
    } else {
      R = t < SWITCH_AT ? Ra : Rb;
    }
    int util = plant_cycle(&p, total, R, u, delay, t, candidate, hard, DELTA_INCREMENT_DIVISOR_DEFAULT);
    int dev = util > u ? util - u : u - util;
    if (cls == BURST) {
      if (t >= 1000 && R > 0) {
        err += dev;
        n++;
      }
    } else if (t >= SWITCH_AT) {
      if (t < SWITCH_AT + TRANSIENT) {
        err += dev;
        n++;
      }
      over += util >= u + 15;
    }
  }
  if (over_s) *over_s = over / 10.0;
  return n ? err / n : 0;
}

static void cell(stats_t *st, int cls, const char *card, int64_t total, double a, double b, int on,
                 int off, int u, int d, int hard) {
  double oo = 0, oc = 0;
  double mo = run(cls, total, a, b, on, off, u, d, 0, hard, &oo);
  double mc = run(cls, total, a, b, on, off, u, d, 1, hard, &oc);
  st->n[cls]++;
  st->o[cls] += mo;
  st->c[cls] += mc;
  if (cls == DROP) {
    st->drop_over_o += oo;
    st->drop_over_c += oc;
  }
  if (mc - mo > CELL_CAP) {
    st->over_cap[cls]++;
    if (getenv("GEN_VERBOSE")) {
      printf("    %s %s R/pool %.2e/%.2e on%d off%d u=%d d%d %s %.2f->%.2f\n", CLASS_NAME[cls], card,
             a, b, on, off, u, d, hard ? "hard" : "soft", mo, mc);
    }
  }
  if (mc - mo > st->worst[cls]) {
    st->worst[cls] = mc - mo;
    if (cls == BURST) {
      snprintf(st->at[cls], sizeof(st->at[cls]), "%s R/pool=%.2e on%d/off%d u=%d d%d %s %.2f->%.2f",
               card, a, on, off, u, d, hard ? "hard" : "soft", mo, mc);
    } else {
      snprintf(st->at[cls], sizeof(st->at[cls]), "%s %.2e->%.2e u=%d d%d %s %.2f->%.2f", card, a, b,
               u, d, hard ? "hard" : "soft", mo, mc);
    }
  }
}

static int g_todo[NCARD], g_ntodo, g_next;
static stats_t g_total;
static pthread_mutex_t g_mu = PTHREAD_MUTEX_INITIALIZER;

static void run_card(int ci, stats_t *st) {
  int64_t total = (int64_t)CARDS[ci].sm * CARDS[ci].th * 32;
  const char *nm = CARDS[ci].n;
  for (int hard = 0; hard < 2; hard++) {
    for (int d = 2; d <= 4; d += 2) {
      for (int pi = 0; pi < NPAIR; pi++) {
        for (int u = 5; u <= 95; u += 5) {
          cell(st, DROP, nm, total, PAIRS[pi][0], PAIRS[pi][1], 0, 0, u, d, hard);
          cell(st, RISE, nm, total, PAIRS[pi][1], PAIRS[pi][0], 0, 0, u, d, hard);
        }
      }
      for (int di = 0; di < NDUTY; di++) {
        for (int li = 0; li < 3; li++) {
          for (int u = 10; u <= 90; u += 10) {
            cell(st, BURST, nm, total, BURST_LOAD[li], 0, DUTY[di][0], DUTY[di][1], u, d, hard);
          }
        }
      }
    }
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
    for (int c = 0; c < NCLASS; c++) {
      g_total.n[c] += st.n[c];
      g_total.over_cap[c] += st.over_cap[c];
      g_total.o[c] += st.o[c];
      g_total.c[c] += st.c[c];
      if (st.worst[c] > g_total.worst[c]) {
        g_total.worst[c] = st.worst[c];
        memcpy(g_total.at[c], st.at[c], sizeof(st.at[c]));
      }
    }
    g_total.drop_over_o += st.drop_over_o;
    g_total.drop_over_c += st.drop_over_c;
    pthread_mutex_unlock(&g_mu);
  }
}

int main(void) {
  for (int ci = 0; ci < NCARD; ci++) {
    int dup = 0;
    for (int cj = 0; cj < ci; cj++) {
      dup |= CARDS[cj].sm == CARDS[ci].sm && CARDS[cj].th == CARDS[ci].th;
    }
    if (!dup) {
      g_todo[g_ntodo++] = ci;
    }
  }
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
  int failures = 0;
  printf("transient scenarios: shipped cycle vs frozen previous, %d card shapes, both start paths\n",
         g_ntodo);
  for (int c = 0; c < NCLASS; c++) {
    double mo = t->o[c] / t->n[c], mc = t->c[c] / t->n[c];
    printf("  %s %6ld cells: mean MAE %6.2f -> %6.2f, %ld beyond +%.1f (worst %+.2f%s%s)\n",
           CLASS_NAME[c], t->n[c], mo, mc, t->over_cap[c], CELL_CAP, t->worst[c],
           t->worst[c] > 0 ? ": " : "", t->worst[c] > 0 ? t->at[c] : "");
    if (t->over_cap[c] > BUDGET_CELLS[c] || t->worst[c] > BUDGET_WORST[c] || mc - mo > CLASS_TOL) {
      printf("  [FAIL] %s: %ld cells beyond (budget %ld), worst %+.2f (budget %+.2f), mean %+.3f\n",
             CLASS_NAME[c], t->over_cap[c], BUDGET_CELLS[c], t->worst[c], BUDGET_WORST[c], mc - mo);
      failures++;
    }
  }
  printf("  DROP over-target time (util >= target+15, 10 min after the switch): %.0f s -> %.0f s\n",
         t->drop_over_o, t->drop_over_c);
  if (!(t->drop_over_c < t->drop_over_o)) {
    failures++;
  }
  if (failures) {
    printf("[FAIL] %d transient criteria\n", failures);
    return 1;
  }
  printf("[ok] per-cell ratchet held, no class mean worse, DROP over-target time reduced\n");
  printf("\ntransient scenarios passed\n");
  return 0;
}
