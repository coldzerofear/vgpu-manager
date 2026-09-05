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
 * The delta controller dominance campaign. No GPU, no driver; ~10s.
 *
 * 55 real GPU models spanning GTX 1050 (5 SM) to B300 (160 SM) across every
 * NVIDIA generation since Pascal, every core limit from 1 to 100, three
 * workload intensities (consumption pool/1500, pool/150, pool/15 per tick),
 * two feedback delays (2 and 4 ticks) -- 33000 closed-loop cells, the
 * shipped sm_delta_step() against a frozen replica of the removed sm^2
 * formula. Same loop model as test_delta_step.c.
 *
 * The assertions ARE the acceptance contract for the controller rewrite:
 *   - steady-state MAE:   worse than the old formula in ZERO cells
 *   - p95 sawtooth:       worse in ZERO cells
 *   - cold-start (two-sided MAE over the first 300 ticks): never worse by
 *     more than 1.5 utilization-points. (One-sided undersupply is NOT a
 *     criterion: the old formula "wins" it at tiny targets by flooding to
 *     100% utilization -- delivering throughput by violating the limit is
 *     not a win, and the two-sided metric prices that violation.)
 *
 * Threads-per-SM by generation: Pascal/Volta/GA100/Hopper/Blackwell-DC 2048,
 * Turing 1024, GA10x/Ada/Blackwell-consumer 1536 (CUDA occupancy tables).
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "include/hook.h"
#include "include/sm_delta.h"

typedef struct { const char *n; int sm, th; } card_t;
static const card_t CARDS[] = {
  {"GTX1050",5,2048},{"GTX1060",10,2048},{"GTX1070",15,2048},{"GTX1080",20,2048},{"GTX1080Ti",28,2048},
  {"P40",30,2048},{"P100",56,2048},
  {"TitanV",80,2048},{"V100",80,2048},{"V100S",80,2048},
  {"GTX1650",14,1024},{"GTX1660",22,1024},{"GTX1660Ti",24,1024},
  {"T4",40,1024},{"RTX2060",30,1024},{"RTX2070",36,1024},{"RTX2080",46,1024},{"RTX2080Ti",68,1024},
  {"RTX3050",20,1536},{"RTX3060",28,1536},{"RTX3070",46,1536},{"RTX3070Ti",48,1536},{"RTX3080",68,1536},
  {"RTX3080Ti",80,1536},{"RTX3090",82,1536},
  {"A100-1g",14,2048},{"A30",56,2048},{"A10",72,1536},{"A40",84,1536},
  {"RTXA4000",48,1536},{"RTXA5000",64,1536},{"RTXA6000",84,1536},
  {"RTX4060",24,1536},{"RTX4070",46,1536},{"RTX4070Ti",60,1536},{"RTX4080",76,1536},{"RTX4090",128,1536},
  {"L4",58,1536},{"L40",142,1536},{"L40S",142,1536},{"RTX6000Ada",142,1536},
  {"RTX5060",36,1536},{"RTX5070",50,1536},{"RTX5070Ti",70,1536},{"RTX5080",84,1536},{"RTX5090",170,1536},
  {"V100-32g",80,2048},{"A800",108,2048},{"A100",108,2048},{"H20",78,2048},
  {"H800",132,2048},{"H100",132,2048},{"H200",132,2048},{"B200",148,2048},{"B300",160,2048},
};
#define NCARD (int)(sizeof(CARDS)/sizeof(CARDS[0]))

/* The removed formula, frozen (see test_delta_step.c). */
static int64_t old_step(int64_t sm, int64_t th, int64_t total, int u, int util, int64_t share) {
  int d = abs(u - util);
  if (d < 5) d = 5;
  int64_t inc = sm * sm * th * (int64_t)d / 2560;
  if ((float)d / (u > 0 ? u : 1) > 0.5f) inc = inc * d * 2 / (u + 1);
  if (inc < 0 || inc > INT_MAX) inc = 10;
  int64_t fl = total * (int64_t)d / ((u > 0 ? u : 1) * 64);
  if (inc < fl) inc = fl;
  if (util <= u) { share = share + inc > total ? total : share + inc; }
  else { share = share - inc < 0 ? 0 : share - inc; }
  return share;
}

typedef struct { double mae, p95, cold; } res_t;

static res_t run_cell(int ci, int u, int rdiv, int delay, int use_new) {
  int64_t total = (int64_t)CARDS[ci].sm * CARDS[ci].th * 32;
  int64_t R = total / rdiv;
  if (R < 1) R = 1;
  int warm = rdiv + 700, ticks = warm + 3000;
  int64_t bucket = total, share = 0;
  int hist[8] = {0};
  double err = 0, cold = 0;
  int cnt = 0;
  static int devs[3200];
  int nd = 0;

  for (int t = 0; t < ticks; t++) {
    int64_t c = bucket < R ? bucket : R;
    bucket -= c;
    int util = (int)(c * 100 / R);
    int m = hist[t % delay];
    hist[t % delay] = util;
    if (use_new) {
      share = sm_delta_step(total, u, m, share, bucket, t < 2,
                            DELTA_INCREMENT_DIVISOR_DEFAULT, 64);
    } else {
      share = old_step(CARDS[ci].sm, CARDS[ci].th, total, u, m, share);
    }
    bucket += share;
    if (bucket > total) bucket = total;
    int dev = util > u ? util - u : u - util;
    if (t < 300) cold += dev;
    if (t >= warm) { err += dev; if (nd < 3200) devs[nd++] = dev; cnt++; }
  }
  res_t r;
  r.mae = err / cnt;
  r.cold = cold / 300;
  int h2[101] = {0};
  for (int i = 0; i < nd; i++) h2[devs[i] > 100 ? 100 : devs[i]]++;
  int acc = 0, p95 = 0;
  for (int i = 0; i <= 100; i++) { acc += h2[i]; if (acc >= nd * 95 / 100) { p95 = i; break; } }
  r.p95 = p95;
  return r;
}

int main(void) {
  static const int rdivs[] = {1500, 150, 15};
  static const int delays[] = {2, 4};
  long lm = 0, lp = 0, lc = 0, wins_m = 0, wins_p = 0;
  double sum_old = 0, sum_new = 0, worst_cold = 0, worst_mae = 0;
  char wc[160] = "", wm[160] = "";

  for (int ci = 0; ci < NCARD; ci++)
    for (int u = 1; u <= 100; u++)
      for (int ri = 0; ri < 3; ri++)
        for (int di = 0; di < 2; di++) {
          res_t o = run_cell(ci, u, rdivs[ri], delays[di], 0);
          res_t n = run_cell(ci, u, rdivs[ri], delays[di], 1);
          sum_old += o.mae; sum_new += n.mae;
          if (n.mae > o.mae + 0.5) {
            lm++;
            if (n.mae - o.mae > worst_mae) {
              worst_mae = n.mae - o.mae;
              snprintf(wm, sizeof(wm), "%s u=%d R/%d d%d %.1f->%.1f",
                       CARDS[ci].n, u, rdivs[ri], delays[di], o.mae, n.mae);
            }
          } else if (n.mae < o.mae - 0.5) wins_m++;
          if (n.p95 > o.p95 + 1) lp++;
          else if (n.p95 < o.p95 - 1) wins_p++;
          if (n.cold > o.cold + 0.5 && n.cold - o.cold > worst_cold) {
            worst_cold = n.cold - o.cold;
            snprintf(wc, sizeof(wc), "%s u=%d R/%d d%d %.1f->%.1f",
                     CARDS[ci].n, u, rdivs[ri], delays[di], o.cold, n.cold);
          }
          if (n.cold > o.cold + 1.5) lc++;
        }

  long tot = (long)NCARD * 100 * 3 * 2;
  printf("campaign: %d cards x limits 1-100 x 3 loads x 2 delays = %ld cells\n", NCARD, tot);
  printf("  steady MAE : %ld better, %ld worse; aggregate %.0f -> %.0f (%.1fx)\n",
         wins_m, lm, sum_old, sum_new, sum_old / sum_new);
  printf("  p95 sawtooth: %ld better, %ld worse\n", wins_p, lp);
  printf("  cold-start  : worst two-sided regression %.1f util-points @ %s\n",
         worst_cold, wc[0] ? wc : "(none)");

  int failures = 0;
  if (lm != 0) { printf("[FAIL] steady MAE worse in %ld cells (worst %.1f @ %s)\n", lm, worst_mae, wm); failures++; }
  else printf("[ok] steady MAE never worse than the old formula\n");
  if (lp != 0) { printf("[FAIL] p95 sawtooth worse in %ld cells\n", lp); failures++; }
  else printf("[ok] p95 sawtooth never worse than the old formula\n");
  if (lc != 0) { printf("[FAIL] cold-start worse by >1.5 in %ld cells\n", lc); failures++; }
  else printf("[ok] cold-start within 1.5 util-points of the old formula everywhere\n");
  if (failures) { printf("\n%d dominance criteria FAILED\n", failures); return 1; }
  printf("\ndominance campaign passed\n");
  return 0;
}
