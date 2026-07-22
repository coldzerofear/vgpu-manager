/*
 * Async / graph-capture memory accounting regression test.
 *
 * Covers the defects fixed on the "strictly enforce memory limits" branch. The
 * common thread is that the limit check reads NVML plus our own virtual-memory
 * ledger, so an allocation the driver has not physically made yet is invisible
 * unless we account for it ourselves -- and every charge we add has to come off
 * again, or the ledger inflates until it fails allocations that should fit.
 *
 *   [A] cuMemAllocAsync is stream-ordered: the memory is not committed when the
 *       call returns, so a burst of them used to sail past the limit, each one
 *       seeing an NVML figure that did not include its predecessors. The
 *       allocating hook now bridges each allocation into the ledger and
 *       synchronizes before dropping the charge, so a burst stops at the limit.
 *
 *   [B] Allocations made DURING graph capture cannot be synchronized (that
 *       would invalidate the capture), so they are charged to the ledger
 *       instead. Without that charge nothing accumulates and the check can
 *       never fire, letting one capture reserve arbitrarily much.
 *
 *   [C] That capture charge is retired by cuStreamEndCapture. Holding it any
 *       longer double-counts against NVML once the graph is launched, and
 *       leaks outright whenever the graph -- not the application -- owns the
 *       pointer. A leak is invisible in a single pass and only shows up as
 *       repeated capture cycles slowly running the container out of memory,
 *       which is what this case drives.
 *
 *   [D] cuGraphDestroy is the second discharge point, for captures whose graph
 *       could not be identified at end-of-capture. Discharging twice must not
 *       over-credit the ledger, so this case also checks that the limit is
 *       still enforced afterwards.
 *
 * Every case asserts vgpu-manager behaviour and self-skips without the library
 * preloaded. [B]-[D] additionally need the virtual-memory ledger enabled, which
 * is what VMEMORY_NODE_ENABLED=1 in run_tests_with_env.sh is for; they report
 * SKIP rather than FAIL when it is off, since the charge they depend on is not
 * being recorded at all.
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so CUDA_MEM_LIMIT=2048m \
 *     VMEMORY_NODE_ENABLED=1 ./test_alloc_async_accounting
 */
#include <cuda.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>   /* strcasecmp */

#include "test_utils.h"

/* Bound on [A]/[B] so a build where the limit is not enforced fails on the
 * assertion rather than allocating until the machine suffers. */
#define MAX_CHUNKS 64
/* [C] repeats enough times that a leaked per-capture charge is certain to
 * exhaust the limit: each pass charges a quarter of it. */
#define CAPTURE_CYCLES 12

static void describe(const char *label, CUresult res) {
  const char *name = NULL;
  CUresult q = cuGetErrorName(res, &name);
  if (q != CUDA_SUCCESS || name == NULL) {
    printf("  %-44s -> %d  <unrecognized error code>\n", label, (int)res);
  } else {
    printf("  %-44s -> %d  (%s)\n", label, (int)res, name);
  }
}

static int preloaded(void) {
  const char *p = getenv("LD_PRELOAD");
  return p != NULL && strstr(p, "libvgpu-control") != NULL;
}

/* The ledger is only consulted when the vmem node is enabled; without it the
 * capture charge is never recorded and [B]-[D] would be asserting nothing. */
static int ledger_enabled(void) {
  const char *v = getenv("VMEMORY_NODE_ENABLED");
  return v != NULL && strcmp(v, "0") != 0 && strcasecmp(v, "false") != 0;
}

/* Under LD_PRELOAD cuMemGetInfo reports the limit-relative figure, so `free` is
 * the headroom the hooks will actually enforce. */
static CUresult headroom(size_t *out) {
  size_t freeb = 0, total = 0;
  CHECK_DRV_API(cuMemGetInfo(&freeb, &total));
  printf("  headroom: free=%zu MiB total=%zu MiB\n",
         freeb / (1024 * 1024), total / (1024 * 1024));
  *out = freeb;
  return CUDA_SUCCESS;
}

/* [A] A burst of stream-ordered allocations must stop at the limit rather than
 * overrunning it while NVML lags behind. */
static int async_burst_honours_limit(CUstream stream) {
  if (!preloaded()) { printf("  SKIP (needs LD_PRELOAD=libvgpu-control.so)\n"); return 0; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  if (freeb == 0) { printf("  SKIP (no headroom reported)\n"); return 0; }

  /* Chunks sized so the limit is reached well inside MAX_CHUNKS. */
  size_t chunk = freeb / 8;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return 0; }

  CUdeviceptr held[MAX_CHUNKS];
  int n = 0, oom = 0;
  size_t granted = 0;
  for (; n < MAX_CHUNKS; n++) {
    CUresult r = cuMemAllocAsync(&held[n], chunk, stream);
    if (r == CUDA_ERROR_OUT_OF_MEMORY) { oom = 1; break; }
    if (r != CUDA_SUCCESS) { describe("cuMemAllocAsync (unexpected)", r); break; }
    granted += chunk;
  }
  printf("  granted %d chunk(s), %zu MiB before %s\n",
         n, granted / (1024 * 1024), oom ? "OUT_OF_MEMORY" : "the cap");

  int failed = 0;
  if (!oom) {
    printf("  FAIL: %d async allocations never hit the limit\n", n);
    failed = 1;
  } else if (granted > freeb) {
    printf("  FAIL: granted %zu MiB past a %zu MiB limit\n",
           granted / (1024 * 1024), freeb / (1024 * 1024));
    failed = 1;
  }

  for (int i = 0; i < n; i++) cuMemFreeAsync(held[i], stream);
  cuStreamSynchronize(stream);
  return failed;
}

/* Outcome of driving a capture until the limit pushes back. */
typedef enum {
  CAPTURE_REFUSED,   /* the limit fired, as it should */
  CAPTURE_UNBOUNDED, /* max_chunks granted and still counting */
  CAPTURE_ERROR      /* something unrelated went wrong */
} capture_result_t;

/* Begin a capture and allocate into it until the limit refuses. *granted
 * receives the number of chunks that were accepted. The capture is always
 * ended and its graph destroyed, whatever the outcome. */
static capture_result_t capture_until_refused(CUstream stream, size_t chunk,
                                              int max_chunks, int *granted) {
  *granted = 0;
  CUresult r = cuStreamBeginCapture(stream, CU_STREAM_CAPTURE_MODE_RELAXED);
  if (r != CUDA_SUCCESS) { describe("cuStreamBeginCapture", r); return CAPTURE_ERROR; }

  capture_result_t outcome = CAPTURE_UNBOUNDED;
  for (int n = 0; n < max_chunks; n++) {
    CUdeviceptr d = 0;
    r = cuMemAllocAsync(&d, chunk, stream);
    if (r == CUDA_ERROR_OUT_OF_MEMORY) { outcome = CAPTURE_REFUSED; break; }
    if (r != CUDA_SUCCESS) {
      describe("cuMemAllocAsync (in capture)", r);
      outcome = CAPTURE_ERROR;
      break;
    }
    (*granted)++;
  }

  CUgraph graph = NULL;
  r = cuStreamEndCapture(stream, &graph);
  if (r != CUDA_SUCCESS) describe("cuStreamEndCapture", r);
  if (graph != NULL) cuGraphDestroy(graph);

  return outcome;
}

/* [B] Several allocations inside ONE capture have to accumulate, or the check
 * can never fire and a capture can reserve without bound. */
static int capture_allocations_accumulate(CUstream stream) {
  if (!preloaded()) { printf("  SKIP (needs LD_PRELOAD=libvgpu-control.so)\n"); return 0; }
  if (!ledger_enabled()) { printf("  SKIP (needs VMEMORY_NODE_ENABLED=1)\n"); return 0; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 8;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return 0; }

  int granted = 0;
  capture_result_t outcome = capture_until_refused(stream, chunk, MAX_CHUNKS, &granted);
  if (outcome == CAPTURE_ERROR) return 1;
  if (outcome == CAPTURE_UNBOUNDED) {
    printf("  FAIL: capture took %d chunk(s) of %zu MiB without ever hitting a "
           "%zu MiB limit -- charges are not accumulating\n",
           granted, chunk / (1024 * 1024), freeb / (1024 * 1024));
    return 1;
  }
  printf("  capture refused after %d chunk(s) of %zu MiB\n",
         granted, chunk / (1024 * 1024));
  return 0;
}

/* [C] The capture charge must be gone once the capture ends. A leak is silent
 * in one pass, so drive many: each cycle charges a quarter of the limit, and a
 * charge that is never retired starves the fourth cycle onwards. */
static int capture_charge_is_retired(CUstream stream) {
  if (!preloaded()) { printf("  SKIP (needs LD_PRELOAD=libvgpu-control.so)\n"); return 0; }
  if (!ledger_enabled()) { printf("  SKIP (needs VMEMORY_NODE_ENABLED=1)\n"); return 0; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return 0; }

  for (int cycle = 1; cycle <= CAPTURE_CYCLES; cycle++) {
    CUresult r = cuStreamBeginCapture(stream, CU_STREAM_CAPTURE_MODE_RELAXED);
    if (r != CUDA_SUCCESS) { describe("cuStreamBeginCapture", r); return 1; }

    CUdeviceptr d = 0;
    CUresult a = cuMemAllocAsync(&d, chunk, stream);

    CUgraph graph = NULL;
    CUresult e = cuStreamEndCapture(stream, &graph);
    if (e != CUDA_SUCCESS) describe("cuStreamEndCapture", e);
    if (graph != NULL) cuGraphDestroy(graph);

    if (a == CUDA_ERROR_OUT_OF_MEMORY) {
      printf("  FAIL: cycle %d of %d refused %zu MiB against a %zu MiB limit -- "
             "earlier captures never released their charge\n",
             cycle, CAPTURE_CYCLES, chunk / (1024 * 1024), freeb / (1024 * 1024));
      return 1;
    }
    if (a != CUDA_SUCCESS) { describe("cuMemAllocAsync (in capture)", a); return 1; }
  }
  printf("  %d capture cycles of %zu MiB each, none starved\n",
         CAPTURE_CYCLES, chunk / (1024 * 1024));
  return 0;
}

/* [D] Retiring the same charge at both cuStreamEndCapture and cuGraphDestroy
 * must not credit it twice: the limit has to survive the round trip. */
static int double_discharge_does_not_over_credit(CUstream stream) {
  if (!preloaded()) { printf("  SKIP (needs LD_PRELOAD=libvgpu-control.so)\n"); return 0; }
  if (!ledger_enabled()) { printf("  SKIP (needs VMEMORY_NODE_ENABLED=1)\n"); return 0; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return 0; }

  /* One capture that goes through BOTH discharge points. */
  CUresult r = cuStreamBeginCapture(stream, CU_STREAM_CAPTURE_MODE_RELAXED);
  if (r != CUDA_SUCCESS) { describe("cuStreamBeginCapture", r); return 1; }
  CUdeviceptr d = 0;
  CUresult a = cuMemAllocAsync(&d, chunk, stream);
  CUgraph graph = NULL;
  CUresult e = cuStreamEndCapture(stream, &graph);
  if (graph != NULL) cuGraphDestroy(graph);   /* second discharge of the same charge */
  if (a != CUDA_SUCCESS) { describe("cuMemAllocAsync (in capture)", a); return 1; }
  if (e != CUDA_SUCCESS) describe("cuStreamEndCapture", e);

  /* The charge is gone, so a plain allocation of that size must fit again... */
  CUdeviceptr live = 0;
  CUresult ok = cuMemAlloc(&live, chunk);
  if (ok != CUDA_SUCCESS) {
    describe("cuMemAlloc after discharge (want SUCCESS)", ok);
    printf("  FAIL: the capture charge was not released\n");
    return 1;
  }

  /* ...and the limit must still refuse something that cannot fit, which it
   * would not if the double discharge had driven the ledger below zero. */
  CUdeviceptr huge = 0;
  CUresult refused = cuMemAlloc(&huge, freeb + chunk);
  describe("cuMemAlloc beyond the limit (want OOM)", refused);
  int failed = 0;
  if (refused == CUDA_SUCCESS) {
    printf("  FAIL: limit no longer enforced -- discharged more than was charged\n");
    cuMemFree(huge);
    failed = 1;
  }

  cuMemFree(live);
  return failed;
}

int main(void) {
  CHECK_DRV_API(cuInit(0));
  CUdevice device;
  CHECK_DRV_API(cuDeviceGet(&device, TEST_DEVICE_ID));
  CUcontext ctx;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, device));
  CUstream stream;
  CHECK_DRV_API(cuStreamCreate(&stream, CU_STREAM_NON_BLOCKING));

  int failures = 0;

  printf("[A] a cuMemAllocAsync burst stops at the memory limit\n");
  failures += async_burst_honours_limit(stream);

  printf("[B] allocations inside one capture accumulate against the limit\n");
  failures += capture_allocations_accumulate(stream);

  printf("[C] the capture charge is retired at cuStreamEndCapture\n");
  failures += capture_charge_is_retired(stream);

  printf("[D] discharging at both end-of-capture and destroy does not over-credit\n");
  failures += double_discharge_does_not_over_credit(stream);

  printf("\nResult: %s\n", failures ? "FAIL" : "PASS");

  cuStreamDestroy(stream);
  CHECK_DRV_API(cuCtxDestroy(ctx));
  return failures ? 1 : 0;
}
