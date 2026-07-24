/*
 * GAP-path SM-throttle regression test.
 *
 * Deliberately reproduces the pattern the GAP throttle targets: a sizable
 * kernel, a host-side idle gap (> GAP_THRESHOLD, 200ms), then a synchronize,
 * repeated. Unlike test_runtime_launch.cu (which launches back-to-back and so
 * stays in the token-bucket / BATCH path), the >200ms gap here makes the hook
 * classify every iteration as a GAP launch, driving gap_begin()/gap_end()
 * (cuEvent record + synchronize + duty-cycle sleep) in cuda_hook.c.
 *
 * What it guards (when run with LD_PRELOAD=libvgpu-control.so on a real GPU):
 *   - the new launch-site wrapping does not crash, hang, or deadlock;
 *   - the injected cuEvent record/synchronize and the throttle sleep do not
 *     corrupt kernel results;
 *   - the per-device lock is released every iteration (a leak would deadlock
 *     the second iteration).
 * It does NOT assert a specific utilization percentage -- that depends on the
 * vgpu config (hard_core/soft_core) and needs NVML sampling. When core_limit
 * is disabled the GAP path is a no-op and this degrades to a plain launch loop,
 * which must still pass.
 *
 * Ported in the style of test_runtime_launch.cu.
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

#include "test_utils.h"

/* Deterministic, easy-to-verify transform: x <- x*2 + 1. */
__global__ void scale_add(float *x, int n) {
  int i = blockIdx.x * blockDim.x + threadIdx.x;
  if (i < n) x[i] = x[i] * 2.0f + 1.0f;
}

int main(void) {
  VGPU_REQUIRE_PRELOAD();
  const int N = 1 << 22; /* 4M elements: large enough to register on the GPU */
  const int iters = 8;
  const int tpb = 256;
  const int blocks = (N + tpb - 1) / tpb;
  const useconds_t gap_us = 250 * 1000; /* > 200ms GAP_THRESHOLD */

  float *d_x;
  CHECK_RUNTIME_API(cudaMalloc(&d_x, N * sizeof(float)));

  float *h = (float *)malloc(N * sizeof(float));
  if (!h) {
    fprintf(stderr, "host alloc failed\n");
    return 1;
  }
  for (int i = 0; i < N; ++i) h[i] = 1.0f;
  CHECK_RUNTIME_API(cudaMemcpy(d_x, h, N * sizeof(float), cudaMemcpyHostToDevice));

  /* x_{k+1} = 2*x_k + 1, x_0 = 1  =>  after `iters` steps x = 2^(iters+1) - 1. */
  double expected = 1.0;
  for (int k = 0; k < iters; ++k) expected = expected * 2.0 + 1.0;

  for (int it = 0; it < iters; ++it) {
    scale_add<<<blocks, tpb>>>(d_x, N);
    CHECK_RUNTIME_API(cudaGetLastError());
    CHECK_RUNTIME_API(cudaDeviceSynchronize());
    /* Idle gap so the next launch is classified as a GAP launch by the hook. */
    usleep(gap_us);
  }

  CHECK_RUNTIME_API(cudaMemcpy(h, d_x, N * sizeof(float), cudaMemcpyDeviceToHost));

  int bad = 0;
  for (int i = 0; i < N; i += (N / 16) + 1) {
    if (fabsf(h[i] - (float)expected) > 1e-3f * (float)expected) {
      fprintf(stderr, "mismatch at %d: got %f want %f\n", i, h[i], (float)expected);
      bad = 1;
      break;
    }
  }

  free(h);
  CHECK_RUNTIME_API(cudaFree(d_x));

  if (bad) return 1;
  printf("completed (iters=%d, expected=%.0f)\n", iters, expected);
  return 0;
}
