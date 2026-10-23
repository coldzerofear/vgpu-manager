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
 * Charge-unit study (not a CI test; the Makefile only runs test_*.c).
 *
 * Should a launch be charged its grid size (blocks, shipped) or
 * grid*block (threads, HAMi-core PR #376)? Neither is "accurate": NVML
 * util is busy TIME, and a launch's time is unknown when it is charged.
 * What matters to the controller is how stable "tokens per util point" (k)
 * stays as a workload's kernel mix changes -- a mix change is a jump in R.
 * kmix_model.h times typical PyTorch kernels on real GPU specs (roofline,
 * modelled, not measured); this prints
 *   1. open loop: per-phase k jump between workload phases, per unit
 *   2. closed loop: the shipped cycle (delta_sim.h) under phase
 *      alternation, grids at several pool FACTORs and threads at several,
 *      against grids FACTOR=32 (shipped)
 * Results and conclusions: docs/sm_delta_validation.md section 9.
 *
 * Build: gcc -O2 -I. -Iinclude -o charge_unit_study test/nogpu/charge_unit_study.c -lm
 */
#include <stdio.h>
#include <string.h>
#include "test/nogpu/delta_sim.h"
#include "test/nogpu/kmix_model.h"

#include "test/nogpu/kmix_model.h"
static int open_loop(void) {
  printf("phase k ratio (max/min across the %d phases) and key transitions, per unit\n", NPHASE);
  printf("%-8s | %-28s | %-28s | %-28s\n", "GPU", "all phases max/min g / t", "fwd_bwd<->optimizer g / t", "prefill<->decode g / t");
  for (int gi = 0; gi < NGPU; gi++) {
    const gpu_t *g = &GPUS[gi];
    double k[2][NPHASE];
    for (int u = 0; u < 2; u++) for (int p = 0; p < NPHASE; p++) k[u][p] = phase_k(g, &PHASES[p], u);
    double sp[2];
    for (int u = 0; u < 2; u++) { double mn = 1e300, mx = 0; for (int p = 0; p < NPHASE; p++) { if (k[u][p] < mn) mn = k[u][p]; if (k[u][p] > mx) mx = k[u][p]; } sp[u] = mx / mn; }
    #define R(a,b,u) (k[u][a] > k[u][b] ? k[u][a]/k[u][b] : k[u][b]/k[u][a])
    printf("%-8s | %10.1fx / %10.1fx    | %10.1fx / %10.1fx    | %10.1fx / %10.1fx\n", g->n, sp[0], sp[1], R(0,1,0), R(0,1,1), R(2,3,0), R(2,3,1));
  }
  return 0;
}

/* Closed loop: v15 (shipped cycle) under phase alternation, grids vs threads charging. */
static const int PAIR[][2] = {{0, 1}, {2, 3}, {0, 4}};   /* fwd_bwd<->opt, prefill<->decode, fwd_bwd<->aug */
static const int LEN[] = {1, 3, 10, 50};
static double run(const gpu_t *g, int unit, long factor, int pi, int len, int u, int d, int hard) {
  double C = (double)g->sm * g->th;
  int64_t total = (int64_t)(C * factor);
  int64_t R[2];
  for (int i = 0; i < 2; i++) { double r = phase_k(g, &PHASES[PAIR[pi][i]], unit) * 100.0; R[i] = r < 1 ? 1 : (int64_t)llround(r); }
  plant_t p; memset(&p, 0, sizeof p);
  double err = 0; long n = 0;
  for (int t = 0; t < 6000; t++) {
    int64_t r = R[(t / len) % 2];
    int util = plant_cycle(&p, total, r, u, d, t, 1, hard, DELTA_INCREMENT_DIVISOR_DEFAULT);
    if (t >= 1000) { err += util > u ? util - u : u - util; n++; }
  }
  return err / n;
}
static int closed_loop(void) {
  struct { int unit; long f; } V[] = {{0,8},{0,16},{0,64},{0,128},{0,256},{1,4096},{1,8192},{1,16384},{1,32768},{1,65536}};
  printf("v15 closed loop, phase alternation; baseline = grids F=32 (shipped). cells %s\n", "8 GPUs x 3 pairs x 4 lens x 9 limits x 2 delays x 2 paths");
  for (unsigned vi = 0; vi < sizeof V / sizeof V[0]; vi++) {
    double sg = 0, st = 0; long n = 0, worse = 0, better = 0; double worst = 0;
    double gpu_g[NGPU] = {0}, gpu_t[NGPU] = {0}; long gpu_n[NGPU] = {0};
    for (int gi = 0; gi < NGPU; gi++) for (int pi = 0; pi < 3; pi++) for (int li = 0; li < 4; li++)
      for (int u = 10; u <= 90; u += 10) for (int d = 2; d <= 4; d += 2) for (int hard = 0; hard < 2; hard++) {
        double a = run(&GPUS[gi], 0, 32, pi, LEN[li], u, d, hard);
        double b = run(&GPUS[gi], V[vi].unit, V[vi].f, pi, LEN[li], u, d, hard);
        sg += a; st += b; n++; gpu_g[gi] += a; gpu_t[gi] += b; gpu_n[gi]++;
        worse += b > a + 2; better += b < a - 2; if (b - a > worst) worst = b - a;
      }
    printf("  %-7s F=%-6ld: MAE %.2f -> %.2f | worse %4ld better %4ld | worst %+5.1f | per GPU:", V[vi].unit ? "threads" : "grids", V[vi].f, sg/n, st/n, worse, better, worst);
    for (int gi = 0; gi < NGPU; gi++) printf(" %s %.1f>%.1f", GPUS[gi].n, gpu_g[gi]/gpu_n[gi], gpu_t[gi]/gpu_n[gi]);
    printf("\n");
  }
  return 0;
}

int main(void) {
  open_loop();
  printf("\n");
  return closed_loop();
}
