/*
 * Sustained-compute workload for the SM throttle ablation pipeline.
 *
 * Designed to be run under LD_PRELOAD=libvgpu-control.so so vgpu-manager's
 * cuLaunchKernel hook + watcher controller (delta or aimd) drive the share
 * budget. The host loop launches the same fma kernel back-to-back, with one
 * cudaDeviceSynchronize per launch and NO host-side sleeps -- this keeps the
 * GPU under continuous pressure so the controller has steady work to clamp
 * SM utilization at the target. The companion sampler (collect.sh) records
 * real SM utilization via nvidia-smi during the run; the workload itself is
 * deliberately silent about throttling, it just provides the load.
 *
 * Environment knobs:
 *   ABLATION_DURATION_S   total wall time to run the loop (default 30)
 *   ABLATION_GRID         grid dim (number of blocks; default 65536)
 *   ABLATION_BLOCK        block dim (threads/block; default 256)
 *   ABLATION_FMA_ITERS    inner FMA iterations per thread (default 8192)
 *
 * Sized roughly so one kernel takes ~50-200ms on a mid-range datacenter GPU.
 * Tune ABLATION_FMA_ITERS up/down if per-iter is far off on your hardware --
 * cadence matters less than sustained occupancy, but ~100ms keeps the watcher
 * (~80ms cycle) firmly inside its feedback regime.
 *
 * Exit 0 on success. Prints a one-line summary so log scrapers can grep it.
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

#include "../test_utils.h"

__global__ void fma_loop(float *out, int n, int iters) {
  int tid = blockIdx.x * blockDim.x + threadIdx.x;
  /* Two independent accumulators so the compiler can't collapse the loop and
   * so each thread gets two pipelined FMA chains -- closer to real workloads. */
  float a = ((float)(tid & 0xffff)) * 1e-4f + 1.0f;
  float b = ((float)((tid >> 4) & 0xffff)) * 1e-4f + 1.0f;
  float ca = 1.0001f, cb = 0.9999f;
  for (int i = 0; i < iters; ++i) {
    a = fmaf(a, ca, 1e-6f);
    b = fmaf(b, cb, 1e-6f);
  }
  if (tid < n) out[tid] = a + b;
}

static int env_int(const char *name, int dflt) {
  const char *s = getenv(name);
  if (!s || !*s) return dflt;
  char *end = NULL;
  long v = strtol(s, &end, 10);
  if (end == s || v <= 0 || v > 0x7fffffff) {
    fprintf(stderr, "warning: %s=\"%s\" invalid, using default %d\n", name, s, dflt);
    return dflt;
  }
  return (int)v;
}

static double monotonic_s(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (double)ts.tv_sec + (double)ts.tv_nsec * 1e-9;
}

int main(void) {
  const int duration_s = env_int("ABLATION_DURATION_S", 30);
  const int grid       = env_int("ABLATION_GRID", 65536);
  const int block      = env_int("ABLATION_BLOCK", 256);
  const int fma_iters  = env_int("ABLATION_FMA_ITERS", 8192);
  const int n          = grid * block;

  float *d_out;
  CHECK_RUNTIME_API(cudaMalloc(&d_out, n * sizeof(float)));

  /* Warm up so the first measured iteration isn't paying JIT / context cost. */
  fma_loop<<<grid, block>>>(d_out, n, fma_iters / 8);
  CHECK_RUNTIME_API(cudaGetLastError());
  CHECK_RUNTIME_API(cudaDeviceSynchronize());

  const double start = monotonic_s();
  const double deadline = start + (double)duration_s;
  long long iters_done = 0;
  double per_iter_min = 1e9, per_iter_max = 0.0, per_iter_sum = 0.0;

  while (monotonic_s() < deadline) {
    double t0 = monotonic_s();
    fma_loop<<<grid, block>>>(d_out, n, fma_iters);
    CHECK_RUNTIME_API(cudaGetLastError());
    CHECK_RUNTIME_API(cudaDeviceSynchronize());
    double dt = monotonic_s() - t0;
    if (dt < per_iter_min) per_iter_min = dt;
    if (dt > per_iter_max) per_iter_max = dt;
    per_iter_sum += dt;
    iters_done++;
  }

  CHECK_RUNTIME_API(cudaFree(d_out));

  double total = monotonic_s() - start;
  double avg_ms = iters_done > 0 ? per_iter_sum * 1000.0 / (double)iters_done : 0.0;
  /* Single line, easy to grep from logs. */
  printf("ABLATION_RESULT iters=%lld duration_s=%.3f avg_iter_ms=%.2f min_iter_ms=%.2f max_iter_ms=%.2f grid=%d block=%d fma_iters=%d\n",
         iters_done, total, avg_ms,
         per_iter_min * 1000.0, per_iter_max * 1000.0, grid, block, fma_iters);
  return 0;
}
