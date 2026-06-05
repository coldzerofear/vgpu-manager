/*
 * CUDA Graph throttle smoke test -- HAMi issue #1445 regression guard.
 *
 * Before P2 in cuda_hook.c (CUDA Graph cost cache + cuGraphLaunch hook),
 * vgpu-manager could not limit compute through cuGraphLaunch: stream-capture
 * applied rate_limiter() once per kernel at capture time, then cuGraphLaunch
 * (called many times after) bypassed the throttle entirely. PyTorch 2.x,
 * TensorRT, and any reduce/fused-op kernel that captures into a graph hit
 * this path. Issue #1445's repro: a single cudaGraphLaunch loop pegs the GPU
 * at 100% even with a 10% gpucores limit set.
 *
 * What this test guards (when run with LD_PRELOAD=libvgpu-control.so):
 *   - cuGraphLaunch hook (P2) does not crash, hang, or deadlock;
 *   - graph instantiate -> launch -> destroy lifecycle remains correct;
 *   - graph cost cache is populated at instantiate (walk_graph_cost) and
 *     consulted at launch (no NULL deref, no stale entries between
 *     unrelated execs);
 *   - rate_limiter side-effects (token consumption, nanosleep on exhaustion)
 *     do not corrupt the kernel results.
 * It does NOT assert a specific throttle ratio -- that requires NVML SM
 * sampling at the orchestrator level. When core_limit is disabled the cache
 * lookup degrades to a no-op and this stays a plain graph-launch loop, which
 * must still pass.
 *
 * Mirrors test_runtime_launch_gap.cu in style.
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>

#include "test_utils.h"

/* Deterministic, easy-to-verify transform: x <- x * 2 + 1. */
__global__ void scale_add(float *x, int n) {
  int i = blockIdx.x * blockDim.x + threadIdx.x;
  if (i < n) x[i] = x[i] * 2.0f + 1.0f;
}

int main(void) {
  const int N = 1 << 20;       /* 1M elements */
  const int tpb = 256;
  const int blocks = (N + tpb - 1) / tpb;
  const int iters_per_graph = 4;
  const int num_launches = 64;

  float *d_x;
  CHECK_RUNTIME_API(cudaMalloc(&d_x, N * sizeof(float)));

  float *h = (float *)malloc(N * sizeof(float));
  if (!h) {
    fprintf(stderr, "host alloc failed\n");
    return 1;
  }
  for (int i = 0; i < N; ++i) h[i] = 0.0f;
  CHECK_RUNTIME_API(cudaMemcpy(d_x, h, N * sizeof(float), cudaMemcpyHostToDevice));

  cudaStream_t stream;
  CHECK_RUNTIME_API(cudaStreamCreate(&stream));

  /* Capture iters_per_graph back-to-back kernels into one graph. */
  CHECK_RUNTIME_API(cudaStreamBeginCapture(stream, cudaStreamCaptureModeGlobal));
  for (int it = 0; it < iters_per_graph; ++it) {
    scale_add<<<blocks, tpb, 0, stream>>>(d_x, N);
  }
  cudaGraph_t graph;
  CHECK_RUNTIME_API(cudaStreamEndCapture(stream, &graph));

  cudaGraphExec_t graph_exec;
  CHECK_RUNTIME_API(cudaGraphInstantiate(&graph_exec, graph, NULL, NULL, 0));

  /* Re-launch the same exec many times. Each launch executes
   * iters_per_graph kernels worth of work; vgpu-manager's cuGraphLaunch
   * hook should rate_limit each launch by the cached graph cost. */
  for (int k = 0; k < num_launches; ++k) {
    CHECK_RUNTIME_API(cudaGraphLaunch(graph_exec, stream));
  }
  CHECK_RUNTIME_API(cudaStreamSynchronize(stream));

  /* x_0 = 0; after total_iters of x <- 2*x + 1, x = 2^total_iters - 1. */
  int total_iters = iters_per_graph * num_launches;
  double expected = 0.0;
  for (int k = 0; k < total_iters; ++k) expected = expected * 2.0 + 1.0;

  CHECK_RUNTIME_API(cudaMemcpy(h, d_x, N * sizeof(float), cudaMemcpyDeviceToHost));

  int bad = 0;
  /* Sample 16 evenly-spaced indices; full N-element check is unnecessary. */
  for (int i = 0; i < N; i += (N / 16) + 1) {
    if (!isfinite(h[i]) || fabsf(h[i] - (float)expected) >
                              1e-3f * fabsf((float)expected)) {
      fprintf(stderr, "mismatch at %d: got %g want %g\n", i, (double)h[i], expected);
      bad = 1;
      break;
    }
  }

  CHECK_RUNTIME_API(cudaGraphExecDestroy(graph_exec));
  CHECK_RUNTIME_API(cudaGraphDestroy(graph));
  CHECK_RUNTIME_API(cudaStreamDestroy(stream));
  CHECK_RUNTIME_API(cudaFree(d_x));
  free(h);

  if (bad) return 1;
  printf("completed (graph kernels=%d, launches=%d, expected=%.3e)\n",
         iters_per_graph, num_launches, expected);
  return 0;
}
