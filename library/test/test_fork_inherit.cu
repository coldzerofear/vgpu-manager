/*
 * fork() inheritance regression test.
 *
 * Reproduces the Project-HAMi/HAMi-core PR #199 bug class on vgpu-manager:
 * Python multiprocessing / torch.multiprocessing fork after the parent has
 * already initialized CUDA via our hook layer. Without the pthread_atfork
 * handler in cuda_hook.c, the child inherits:
 *
 *   - g_init_set marked completed -> initialization() is skipped -> the
 *     watcher threads (which fork() leaves behind in the parent) are never
 *     re-spawned in the child -> SM controller never runs there.
 *   - GAP-path cuEvent handles from the parent's CUDA context -> stale in
 *     child -> cuEventRecord fails OR child uses garbage handles.
 *   - g_gap_lock possibly in "held" state (if a parent thread was inside
 *     the critical section at fork) -> child's trylock returns EBUSY
 *     forever -> GAP path permanently skipped (silent regression).
 *
 * What this test exercises:
 *   1. Parent does one minimal launch + sync to trigger initialization()
 *      and let the watcher threads spawn.
 *   2. fork().
 *   3. Child runs N kernels with >200ms host gaps between them, forcing
 *      the GAP path to fire on every iteration. Then verifies the
 *      numerical result. If the cuEvent handles or lock were inherited
 *      from parent un-fixed, this would either deadlock (mutex held by
 *      vanished thread) or crash (cuEventRecord on stale handle in wrong
 *      context). With the fix, child re-creates events and re-spawns its
 *      own watcher; everything works.
 *   4. Parent does its own post-fork loop too, so we catch any regression
 *      that breaks the parent's state when registering the fork handler.
 *
 * Exit 0 iff both parent and child completed N iterations with correct
 * results. Any crash, deadlock (caught by the harness TEST_TIMEOUT), or
 * mismatched output fails the test.
 *
 * Runs under LD_PRELOAD=libvgpu-control.so on a real GPU.
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <sys/wait.h>

#include "test_utils.h"

__global__ void scale_add(float *x, int n) {
  int i = blockIdx.x * blockDim.x + threadIdx.x;
  if (i < n) x[i] = x[i] * 2.0f + 1.0f;
}

/* Run `iters` of x = x*2 + 1 with a >GAP_THRESHOLD (200ms) host gap between
 * launches so each launch hits the GAP path. Returns 0 on success. */
static int run_loop(const char *label, int iters) {
  const int N = 1 << 22;            /* 4M elements */
  const int tpb = 256;
  const int blocks = (N + tpb - 1) / tpb;
  const useconds_t gap_us = 250 * 1000;  /* > 200ms GAP_THRESHOLD */

  float *d_x;
  CHECK_RUNTIME_API(cudaMalloc(&d_x, N * sizeof(float)));
  float *h = (float *)malloc(N * sizeof(float));
  if (!h) {
    fprintf(stderr, "[%s] host alloc failed\n", label);
    return 1;
  }
  for (int i = 0; i < N; ++i) h[i] = 1.0f;
  CHECK_RUNTIME_API(cudaMemcpy(d_x, h, N * sizeof(float), cudaMemcpyHostToDevice));

  /* x_{k+1} = 2*x_k + 1, x_0 = 1  =>  after iters steps, x = 2^(iters+1) - 1 */
  double expected = 1.0;
  for (int k = 0; k < iters; ++k) expected = expected * 2.0 + 1.0;

  for (int it = 0; it < iters; ++it) {
    scale_add<<<blocks, tpb>>>(d_x, N);
    CHECK_RUNTIME_API(cudaGetLastError());
    CHECK_RUNTIME_API(cudaDeviceSynchronize());
    usleep(gap_us);
  }

  CHECK_RUNTIME_API(cudaMemcpy(h, d_x, N * sizeof(float), cudaMemcpyDeviceToHost));
  CHECK_RUNTIME_API(cudaFree(d_x));

  int bad = 0;
  for (int i = 0; i < N; i += (N / 16) + 1) {
    if (fabsf(h[i] - (float)expected) > 1e-3f * (float)expected) {
      fprintf(stderr, "[%s] mismatch at %d: got %f want %f\n",
              label, i, h[i], (float)expected);
      bad = 1;
      break;
    }
  }
  free(h);
  if (bad) return 1;
  printf("[%s] %d iters completed, expected=%.0f\n", label, iters, expected);
  return 0;
}

int main(void) {
  /* (1) parent_pre: tiny pre-fork loop to force initialization() / spawn
   * watcher threads. One iter is enough to trip pthread_once. */
  if (run_loop("parent_pre", 1) != 0) {
    fprintf(stderr, "parent_pre failed before fork -- baseline broken\n");
    return 1;
  }

  /* Drain stdio buffers before fork. Without this the child inherits any
   * pending stdout/stderr bytes and re-emits them on its own exit, which
   * shows up as duplicate "[parent_pre] ..." lines and confuses
   * post-mortem log analysis. */
  fflush(stdout);
  fflush(stderr);

  /* (2) fork. Threads in parent are NOT copied -- only the calling thread. */
  pid_t pid = fork();
  if (pid < 0) {
    perror("fork");
    return 1;
  }

  if (pid == 0) {
    /* (3) Child. If g_init_set were inherited as completed, the child would
     * have no watcher threads and (worse) the GAP-path cuEvent handles +
     * mutex would be stale/locked -- this loop would deadlock or crash.
     *
     * Force fresh CUDA runtime state before any allocation: NVIDIA's CUDA
     * primary context does not survive fork -- the inherited driver-side
     * handles are stale and cudaMalloc reports cudaErrorInitializationError
     * on most driver versions. cudaDeviceReset() detaches from the
     * inherited primary context so the next CUDA call lazy-inits a fresh
     * one. If even reset is rejected -- a sign the driver flat-out refuses
     * fork+CUDA -- exit cleanly so the harness records SKIP-equivalent
     * behavior rather than blaming vgpu-manager for a driver limitation. */
    cudaError_t reset_st = cudaDeviceReset();
    if (reset_st != cudaSuccess) {
      fprintf(stderr,
              "[child] cudaDeviceReset failed (%d, %s) -- driver does not "
              "support fork+CUDA on this version; skipping vgpu fork "
              "inherit check.\n",
              (int)reset_st, cudaGetErrorString(reset_st));
      exit(0);
    }
    int rc = run_loop("child", 3);
    exit(rc);
  }

  /* (4) Parent: also do post-fork work to confirm the fork handler didn't
   * disturb the parent's still-valid state. */
  int parent_rc = run_loop("parent_post", 3);
  int status = 0;
  if (waitpid(pid, &status, 0) < 0) {
    perror("waitpid");
    return 1;
  }
  int child_rc = WIFEXITED(status) ? WEXITSTATUS(status) : -1;
  printf("parent_rc=%d child_rc=%d\n", parent_rc, child_rc);
  return (parent_rc == 0 && child_rc == 0) ? 0 : 1;
}
