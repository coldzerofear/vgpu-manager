/*
 * Capture accounting through the CUDA runtime.
 *
 * The runtime does not call the driver by symbol: it resolves each entry point
 * through cuGetProcAddress, so a hook only takes effect if cuda_hooks_entry[]
 * routes that lookup to it. test_alloc_async_accounting covers the logic with
 * driver calls; this covers the routing, for the hooks capture accounting
 * depends on -- cudaStreamBeginCapture (where the graph is recorded),
 * cudaStreamEndCapture (where charges are retired, including for an
 * invalidated capture), and cudaDeviceReset (the primary-context reset that
 * retires a capture left open).
 *
 *   [R1] a capture's cudaMallocAsync is charged, and released at end of capture
 *   [R2] an invalidated capture's charge is released
 *   [R3] cudaDeviceReset with a capture open releases its charge
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so CUDA_MEM_LIMIT=4096m \
 *     VMEMORY_NODE_ENABLED=1 ./test_runtime_capture_accounting
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "test_utils.h"
#include "test_ledger.h"

#define MIB(x) ((double)(x) / (1024.0 * 1024.0))

static int preloaded(void) {
  const char *p = getenv("LD_PRELOAD");
  return p != NULL && strstr(p, "libvgpu-control") != NULL;
}

/* 0 to run, -1 to skip, 1 to fail; as in test_alloc_async_accounting. */
static int can_assert(void) {
  if (!preloaded()) { printf("  SKIP (needs LD_PRELOAD=libvgpu-control.so)\n"); return -1; }
  if (g_ledger_broken) { printf("  FAIL (ledger layout does not match this test's mirror)\n"); return 1; }
  if (g_ledger == NULL) { printf("  SKIP (no ledger -- needs VMEMORY_NODE_ENABLED=1)\n"); return -1; }
  return 0;
}

static size_t quarter_headroom(void) {
  size_t freeb = 0, total = 0;
  if (cudaMemGetInfo(&freeb, &total) != cudaSuccess) return 0;
  printf("  headroom: free=%.0f MiB total=%.0f MiB\n", MIB(freeb), MIB(total));
  return freeb / 4;
}

/* Charged while the capture is open, gone once it ends, else fail. */
static int judge(long long before, long long during, long long after, size_t chunk) {
  printf("  ledger: %.0f MiB before, %.0f MiB during capture, %.0f MiB after\n",
         MIB(before), MIB(during), MIB(after));
  int failed = 0;
  if (during - before < (long long)chunk) {
    printf("  FAIL: the allocation in capture was not charged -- the hook was not reached\n");
    failed = 1;
  }
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged\n", MIB(after - before));
    failed = 1;
  }
  return failed;
}

static int r1_capture(cudaStream_t s) {
  { int pre = can_assert(); if (pre != 0) return pre; }
  size_t chunk = quarter_headroom();
  if (chunk == 0) { printf("  SKIP (no headroom)\n"); return -1; }
  long long before = ledger_used_self();
  if (cudaStreamBeginCapture(s, cudaStreamCaptureModeRelaxed) != cudaSuccess) return 1;
  void *p = NULL;
  cudaError_t a = cudaMallocAsync(&p, chunk, s);
  long long during = ledger_used_self();
  cudaGraph_t g = NULL;
  cudaError_t e = cudaStreamEndCapture(s, &g);
  long long after = ledger_used_self();
  if (g != NULL) cudaGraphDestroy(g);
  printf("  cudaMallocAsync -> %s, cudaStreamEndCapture -> %s\n", cudaGetErrorName(a), cudaGetErrorName(e));
  if (a != cudaSuccess || e != cudaSuccess) return 1;
  return judge(before, during, after, chunk);
}

static int r2_invalidated(cudaStream_t s) {
  { int pre = can_assert(); if (pre != 0) return pre; }
  size_t chunk = quarter_headroom();
  if (chunk == 0) { printf("  SKIP (no headroom)\n"); return -1; }
  long long before = ledger_used_self();
  if (cudaStreamBeginCapture(s, cudaStreamCaptureModeGlobal) != cudaSuccess) return 1;
  void *p = NULL;
  cudaError_t a = cudaMallocAsync(&p, chunk, s);
  long long during = ledger_used_self();
  cudaError_t y = cudaStreamSynchronize(s);
  cudaGraph_t g = NULL;
  cudaError_t e = cudaStreamEndCapture(s, &g);
  long long after = ledger_used_self();
  if (g != NULL) cudaGraphDestroy(g);
  cudaGetLastError();   /* clear the sticky capture error */
  printf("  cudaMallocAsync -> %s, cudaStreamSynchronize -> %s, cudaStreamEndCapture -> %s\n",
         cudaGetErrorName(a), cudaGetErrorName(y), cudaGetErrorName(e));
  if (a != cudaSuccess) return 1;
  if (e != cudaErrorStreamCaptureInvalidated) { printf("  SKIP (capture not invalidated)\n"); return -1; }
  return judge(before, during, after, chunk);
}

static int r3_device_reset(void) {
  { int pre = can_assert(); if (pre != 0) return pre; }
  size_t chunk = quarter_headroom();
  if (chunk == 0) { printf("  SKIP (no headroom)\n"); return -1; }
  cudaStream_t s = NULL;
  if (cudaStreamCreateWithFlags(&s, cudaStreamNonBlocking) != cudaSuccess) return 1;
  long long before = ledger_used_self();
  if (cudaStreamBeginCapture(s, cudaStreamCaptureModeRelaxed) != cudaSuccess) return 1;
  void *p = NULL;
  cudaError_t a = cudaMallocAsync(&p, chunk, s);
  long long during = ledger_used_self();
  cudaError_t r = cudaDeviceReset();
  long long after = ledger_used_self();
  printf("  cudaMallocAsync -> %s, cudaDeviceReset -> %s\n", cudaGetErrorName(a), cudaGetErrorName(r));
  if (a != cudaSuccess || r != cudaSuccess) return 1;
  return judge(before, during, after, chunk);
}

int main(void) {
  CHECK_RUNTIME_API(cudaSetDevice(TEST_DEVICE_ID));
  CHECK_RUNTIME_API(cudaFree(0));   /* create the context; the library maps the ledger */
  ledger_open();
  cudaStream_t s = NULL;
  CHECK_RUNTIME_API(cudaStreamCreateWithFlags(&s, cudaStreamNonBlocking));

  int failures = 0, skipped = 0, rc;
#define RUN_CASE(title, call) do { printf(title "\n"); rc = (call); if (rc < 0) skipped++; else failures += rc; } while (0)
  RUN_CASE("[R1] a captured cudaMallocAsync is charged and released at end of capture", r1_capture(s));
  RUN_CASE("[R2] an invalidated capture's charge is released", r2_invalidated(s));
  cudaStreamDestroy(s);
  RUN_CASE("[R3] cudaDeviceReset with a capture open releases its charge", r3_device_reset());
#undef RUN_CASE
  return vgpu_test_verdict(failures, skipped);
}
