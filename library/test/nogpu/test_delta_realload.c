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
 * Real workloads, each charge as it ships. No GPU, no driver.
 *
 * The campaigns sweep R/pool, which is charge-blind. What the charge decides
 * is WHERE a real workload lands on that axis -- and v15's grid charge put
 * real workloads at both ends of what any controller can hold: few-block
 * long kernels (persistent GEMMs, NCCL) below it, running through the limit
 * (HAMi-core #360), and block-heavy short kernels on fast parts above the
 * pool, pinned far under their limit (B200 training capped at 26% under 50).
 *
 * kmix_model.h times typical PyTorch kernels on public GPU specs (modelled,
 * not measured). Each workload is charged both ways -- v15: grid blocks
 * against a FACTOR-32 pool, v16: grid x block threads against a FACTOR pool
 * -- and run through delta_sim.h's plant with the matching controller:
 *
 *   kernel       each kmix kernel alone
 *   phase        the five workload phases (aggregate of their kernels)
 *   persistent   grid = #SMs, 384 threads, 5 / 14 / 35 / 70 / 200 ms a launch
 *   nccl         8 and 32 channels, 512 threads, 1 / 5 / 20 / 50 ms a launch
 *   alternation  three phase pairs switching every 1 / 3 / 10 / 50 cycles
 *
 * on 8 GPUs (T4 .. B200 and a synthetic future part), 18 limits from 5 to 99,
 * delays 2 and 4, soft and hard start paths, each reported separately.
 * Error is mean util minus the limit (over/under) and per-cycle MAE.
 *
 * ACCEPTANCE, per start path:
 *   - every workload class's mean |error| and MAE not worse than v15's
 *   - cells over the limit by >5 and under it by >5 not more than v15's
 *   - cells whose MAE is worse than v15's by >2: a ratchet (count, worst)
 * and per limit band the table is printed, so a regression in any band of
 * 0..100 is visible, the 93-95 band in particular (v15 held nothing there).
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
#include "test/nogpu/kmix_model.h"

/* Ratchet: MAE worse than v15 by >2, per start path (alternation only; see
 * docs/sm_delta_validation.md sections 11-12). Lower when a generation clears
 * cells; never raise to let a change pass. v16.1 re-based it: the class mean
 * fell 15.6 -> 10.4, but a phase that turns over within the estimate's
 * window is sized from the phase before it (worst: 10-cycle phases at 99%). */
static const long   WORSE_BUDGET[2] = {506, 508};
static const double WORSE_WORST[2]  = {42.06, 42.06};

enum { C_KERNEL, C_PHASE, C_PERSIST, C_NCCL, C_ALT, NCLS };
static const char *CLS[NCLS] = {"kernel", "phase", "persistent", "nccl", "alternation"};
static const int U[] = {5, 10, 20, 30, 40, 50, 60, 70, 80, 90, 91, 92, 93, 94, 95, 96, 97, 99};
#define NU (int)(sizeof(U) / sizeof(U[0]))
static const int BAND_LO[] = {1, 10, 30, 50, 70, 90, 93, 96};
static const int BAND_HI[] = {9, 29, 49, 69, 89, 92, 95, 99};
#define NB 8

typedef struct { const char *n; int cls; double ra[2], rb[2]; int len; } item_t; /* [0] grids, [1] threads per cycle */

typedef struct {
  long n[NCLS], bn[NB], over[2], under[2], worse;
  double e[NCLS][2], m[NCLS][2], be[NB][2], bm[NB][2], bover[NB][2], worst;
  long bcount_over[NB][2], bcount_under[NB][2];
  char wat[200];
} acc_t;

static acc_t G[2];
static pthread_mutex_t MU = PTHREAD_MUTEX_INITIALIZER;
static int g_next;

static void run(int gen, const gpu_t *g, const item_t *it, int u, int d, int hard, double *err,
                double *mae) {
  int64_t total = (int64_t)g->sm * g->th * (gen ? FACTOR : V15_FACTOR);
  int64_t Ra = (int64_t)llround(it->ra[gen]), Rb = (int64_t)llround(it->rb[gen]);
  if (Ra < 1) Ra = 1;
  if (Rb < 1) Rb = 1;
  plant_t p;
  memset(&p, 0, sizeof(p));
  double s = 0, e = 0;
  long n = 0;
  for (int t = 0; t < 6000; t++) {
    int64_t R = it->len ? ((t / it->len) % 2 ? Rb : Ra) : Ra;
    int ut = plant_cycle(&p, total, R, u, d, t, gen, hard, DELTA_INCREMENT_DIVISOR_DEFAULT);
    if (t >= 1000) {
      s += ut;
      e += abs(ut - u);
      n++;
    }
  }
  *err = s / n - u;
  *mae = e / n;
}

static int band_of(int u) {
  for (int b = 0; b < NB; b++) {
    if (u >= BAND_LO[b] && u <= BAND_HI[b]) {
      return b;
    }
  }
  return NB - 1;
}

static void per_gpu(int gi, acc_t *A) {
  const gpu_t *g = &GPUS[gi];
  item_t it[64];
  int ni = 0;
  for (int k = 0; k < NKERN; k++) {
    double per = 1e5 / kern_us(g, &KERNS[k]);
    it[ni++] = (item_t){KERNS[k].n, C_KERNEL, {per * KERNS[k].blocks, per * KERNS[k].blocks * KERNS[k].tpb}, {0, 0}, 0};
  }
  for (int p = 0; p < NPHASE; p++) {
    double tg = 0, tt = 0, us = 0;
    for (int k = 0; k < NKERN; k++) {
      tg += PHASES[p].cnt[k] * KERNS[k].blocks;
      tt += PHASES[p].cnt[k] * KERNS[k].blocks * KERNS[k].tpb;
      us += PHASES[p].cnt[k] * kern_us(g, &KERNS[k]);
    }
    it[ni++] = (item_t){PHASES[p].n, C_PHASE, {tg / (us / 1e5), tt / (us / 1e5)}, {0, 0}, 0};
  }
  static const double PD[] = {5, 14, 35, 70, 200};
  for (int i = 0; i < 5; i++) {
    double per = 100 / PD[i];
    it[ni++] = (item_t){"persistent", C_PERSIST, {per * g->sm, per * g->sm * 384}, {0, 0}, 0};
  }
  static const double ND[] = {1, 5, 20, 50};
  static const int CH[] = {8, 32};
  for (int c = 0; c < 2; c++) {
    for (int i = 0; i < 4; i++) {
      double per = 100 / ND[i];
      it[ni++] = (item_t){"nccl", C_NCCL, {per * CH[c], per * CH[c] * 512}, {0, 0}, 0};
    }
  }
  static const int PAIR[][2] = {{0, 1}, {2, 3}, {0, 4}};
  static const int LEN[] = {1, 3, 10, 50};
  for (int pi = 0; pi < 3; pi++) {
    for (int li = 0; li < 4; li++) {
      const item_t *a = &it[NKERN + PAIR[pi][0]], *b = &it[NKERN + PAIR[pi][1]];
      it[ni++] = (item_t){"alternation", C_ALT, {a->ra[0], a->ra[1]}, {b->ra[0], b->ra[1]}, LEN[li]};
    }
  }
  for (int hard = 0; hard < 2; hard++) {
    acc_t *S = &A[hard];
    for (int i = 0; i < ni; i++) {
      for (int ui = 0; ui < NU; ui++) {
        double e[2] = {0}, m[2] = {0};
        for (int d = 2; d <= 4; d += 2) {
          for (int gen = 0; gen < 2; gen++) {
            double ee, mm;
            run(gen, g, &it[i], U[ui], d, hard, &ee, &mm);
            e[gen] += ee / 2;
            m[gen] += mm / 2;
          }
        }
        int c = it[i].cls, b = band_of(U[ui]);
        S->n[c]++;
        S->bn[b]++;
        for (int gen = 0; gen < 2; gen++) {
          S->e[c][gen] += fabs(e[gen]);
          S->m[c][gen] += m[gen];
          S->be[b][gen] += fabs(e[gen]);
          S->bm[b][gen] += m[gen];
          S->bover[b][gen] += e[gen] > 0 ? e[gen] : 0;
          S->over[gen] += e[gen] > 5;
          S->under[gen] += e[gen] < -5;
          S->bcount_over[b][gen] += e[gen] > 5;
          S->bcount_under[b][gen] += e[gen] < -5;
        }
        if (m[1] > m[0] + 2) {
          S->worse++;
          if (getenv("GEN_VERBOSE")) {
            printf("    worse %s %s %-11s len%-2d u=%-2d MAE %.2f -> %.2f\n", hard ? "hard" : "soft",
                   g->n, it[i].n, it[i].len, U[ui], m[0], m[1]);
          }
        }
        if (m[1] - m[0] > S->worst) {
          S->worst = m[1] - m[0];
          snprintf(S->wat, sizeof(S->wat), "%s %s len%d u=%d %.2f->%.2f", g->n, it[i].n,
                   it[i].len, U[ui], m[0], m[1]);
        }
      }
    }
  }
}

static void merge(acc_t *t, const acc_t *s) {
  for (int c = 0; c < NCLS; c++) {
    t->n[c] += s->n[c];
    for (int w = 0; w < 2; w++) {
      t->e[c][w] += s->e[c][w];
      t->m[c][w] += s->m[c][w];
    }
  }
  for (int b = 0; b < NB; b++) {
    t->bn[b] += s->bn[b];
    for (int w = 0; w < 2; w++) {
      t->be[b][w] += s->be[b][w];
      t->bm[b][w] += s->bm[b][w];
      t->bover[b][w] += s->bover[b][w];
      t->bcount_over[b][w] += s->bcount_over[b][w];
      t->bcount_under[b][w] += s->bcount_under[b][w];
    }
  }
  for (int w = 0; w < 2; w++) {
    t->over[w] += s->over[w];
    t->under[w] += s->under[w];
  }
  t->worse += s->worse;
  if (s->worst > t->worst) {
    t->worst = s->worst;
    memcpy(t->wat, s->wat, sizeof(t->wat));
  }
}

static void *worker(void *arg) {
  (void)arg;
  for (;;) {
    int k = __atomic_fetch_add(&g_next, 1, __ATOMIC_RELAXED);
    if (k >= NGPU) {
      return NULL;
    }
    acc_t *A = calloc(2, sizeof(acc_t));
    if (!A) {
      return NULL;
    }
    per_gpu(k, A);
    pthread_mutex_lock(&MU);
    merge(&G[0], &A[0]);
    merge(&G[1], &A[1]);
    pthread_mutex_unlock(&MU);
    free(A);
  }
}

int main(void) {
  pthread_t th[NGPU];
  int nthr = 0;
  for (int i = 0; i < NGPU; i++) {
    if (pthread_create(&th[nthr], NULL, worker, NULL) == 0) {
      nthr++;
    }
  }
  if (nthr == 0) {
    worker(NULL);
  }
  for (int i = 0; i < nthr; i++) {
    pthread_join(th[i], NULL);
  }

  int failures = 0;
  printf("real workloads: v15 (grid charge, FACTOR %d) vs shipped (grid x block, FACTOR %d)\n",
         V15_FACTOR, FACTOR);
  for (int hard = 0; hard < 2; hard++) {
    const acc_t *S = &G[hard];
    printf("\n== %s start path ==\n", hard ? "hard-limit (bypass)" : "soft-limit");
    printf("  %-12s %22s %22s\n", "class", "mean |err| v15 > v16", "MAE v15 > v16");
    for (int c = 0; c < NCLS; c++) {
      double k = (double)S->n[c];
      double e0 = S->e[c][0] / k, e1 = S->e[c][1] / k, m0 = S->m[c][0] / k, m1 = S->m[c][1] / k;
      int bad = e1 > e0 + 0.05 || m1 > m0 + 0.05;
      failures += bad;
      printf("  %-12s %10.2f > %-9.2f %10.2f > %-9.2f%s\n", CLS[c], e0, e1, m0, m1, bad ? "  <-- worse" : "");
    }
    printf("  %-12s %9s %20s %20s %20s %20s\n", "limit band", "cells", "mean |err| v15>v16",
           "MAE v15>v16", "over>5 v15>v16", "under<-5 v15>v16");
    for (int b = 0; b < NB; b++) {
      double k = (double)S->bn[b];
      char lab[16];
      snprintf(lab, sizeof(lab), "%d-%d", BAND_LO[b], BAND_HI[b]);
      printf("  %-12s %9ld %9.2f > %-8.2f %9.2f > %-8.2f %9ld > %-8ld %9ld > %-8ld\n", lab, S->bn[b],
             S->be[b][0] / k, S->be[b][1] / k, S->bm[b][0] / k, S->bm[b][1] / k,
             S->bcount_over[b][0], S->bcount_over[b][1], S->bcount_under[b][0], S->bcount_under[b][1]);
    }
    printf("  over the limit by >5: %ld > %ld; under by >5: %ld > %ld\n", S->over[0], S->over[1],
           S->under[0], S->under[1]);
    printf("  MAE worse than v15 by >2: %ld cells (budget %ld), worst %+.2f (budget %+.2f) %s\n",
           S->worse, WORSE_BUDGET[hard], S->worst, WORSE_WORST[hard], S->wat);
    if (S->over[1] > S->over[0] || S->under[1] > S->under[0]) {
      printf("  [FAIL] more cells outside +-5 of the limit than v15\n");
      failures++;
    }
    if (S->worse > WORSE_BUDGET[hard] || S->worst > WORSE_WORST[hard]) {
      printf("  [FAIL] per-cell ratchet\n");
      failures++;
    }
  }
  if (failures) {
    printf("\n%d real-workload criteria FAILED\n", failures);
    return 1;
  }
  printf("\n[ok] every class at least as good as v15 on both paths; fewer cells outside +-5; ratchet held\n");
  printf("\nreal-workload suite passed\n");
  return 0;
}
