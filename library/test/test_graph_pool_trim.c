/*
 * An allocation outside any graph, refused only for this process's idle graph pool,
 * has the pool trimmed for it.
 *
 * NVML counts the pages the device's graph pool keeps after its graphs have run, so
 * they count against the limit. When a cuMemAlloc outgrows the free device memory the
 * driver hands idle pool pages back by itself (docs 6.5); under a limit the device is
 * never short, so it never does, and without the library trimming the pool the
 * allocation is refused where a device of the limit's size would grant it.
 *
 *   [A] cuMemAlloc fitting once the idle pool is trimmed: granted, the pool trimmed,
 *       and the graph's next launch is checked again (refused while the memory is
 *       taken, accepted once freed)
 *   [B] the same through cuMemAllocAsync on a stream not in capture
 *   [C] a request the idle pool cannot cover: refused, the pool left alone
 *   [D] the pool memory of a graph still running is not given back by a trim: refused
 *       (what the trim gave back is measured, not estimated), granted once it finished
 *   [E] blocks a graph left allocated are not idle: a request the idle part alone cannot
 *       cover leaves the pool alone; granted once the held block is freed
 *   [F] with a capture open (relaxed, another stream) nothing is trimmed: refused, the
 *       pool left alone; granted once the capture ended
 *   [G] a graph launch is not given another graph's idle pool by a trim: refused, the
 *       pool left alone (its idle part already counts for the launch)
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so CUDA_MEM_LIMIT=4096m ./test_graph_pool_trim
 */
#include <cuda.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include "test_utils.h"

#define MIB(x) ((double)(x) / (1024.0 * 1024.0))
#define ALIGN (64u << 20)   /* slack for the pool's rounding and NVML's granularity */
#define WATCHDOG_S 10.0     /* [D]: the held stream is let go by then, whatever happens */
#define WAITED_S   5.0      /* [D]: an allocation back later than this waited for the graph */

static double now_s(void) {
  struct timespec t;
  clock_gettime(CLOCK_MONOTONIC, &t);
  return (double)t.tv_sec + (double)t.tv_nsec / 1e9;
}

static CUdevice g_dev;
static CUstream g_stream;

static void describe(const char *label, CUresult res) {
  const char *name = NULL;
  if (cuGetErrorName(res, &name) != CUDA_SUCCESS || name == NULL) name = "?";
  printf("  %-52s -> %d (%s)\n", label, (int)res, name);
}

static size_t headroom(void) {
  size_t freeb = 0, total = 0;
  if (cuMemGetInfo(&freeb, &total) != CUDA_SUCCESS) return 0;
  return freeb;
}

static size_t pool_reserved(void) {
  cuuint64_t v = 0;
  cuDeviceGetGraphMemAttribute(g_dev, CU_GRAPH_MEM_ATTR_RESERVED_MEM_CURRENT, &v);
  return (size_t)v;
}

static void trim_pool(void) {
  cuCtxSynchronize();
  cuDeviceGraphMemTrim(g_dev);
}

/* A captured graph allocating `bytes` and touching them; freed in the graph unless
 * keep, then *kept is the block it leaves allocated. */
static CUgraphExec captured_exec(size_t bytes, int keep, CUdeviceptr *kept) {
  CUgraph g = NULL;
  CUdeviceptr d = 0;
  if (cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_RELAXED) != CUDA_SUCCESS) return NULL;
  CUresult a = cuMemAllocAsync(&d, bytes, g_stream);
  if (a == CUDA_SUCCESS) {
    cuMemsetD8Async(d, 1, bytes, g_stream);
    if (!keep) cuMemFreeAsync(d, g_stream);
  }
  cuStreamEndCapture(g_stream, &g);
  if (a != CUDA_SUCCESS || g == NULL) {
    describe("cuMemAllocAsync (in capture)", a);
    if (g != NULL) cuGraphDestroy(g);
    return NULL;
  }
  CUgraphExec ex = NULL;
  CUresult r = cuGraphInstantiateWithFlags(&ex, g, 0);
  cuGraphDestroy(g);
  if (r != CUDA_SUCCESS) {
    describe("cuGraphInstantiateWithFlags", r);
    return NULL;
  }
  if (kept != NULL) *kept = d;
  return ex;
}

static CUresult launch_and_wait(CUgraphExec ex) {
  CUresult r = cuGraphLaunch(ex, g_stream);
  cuStreamSynchronize(g_stream);
  return r;
}

/* An exec of `need` bytes launched once, its pool left idle. */
static CUgraphExec idle_pool_of(size_t need) {
  CUgraphExec ex = captured_exec(need, 0, NULL);
  if (ex == NULL) return NULL;
  CUresult r = launch_and_wait(ex);
  if (r != CUDA_SUCCESS) {
    describe("cuGraphLaunch (to fill the pool)", r);
    cuGraphExecDestroy(ex);
    return NULL;
  }
  printf("  pool idle at %.0f MiB, headroom %.0f MiB\n", MIB(pool_reserved()), MIB(headroom()));
  return ex;
}

/* Refused, and the pool as it was: nothing trimmed for it. */
static int refused_untrimmed(const char *label, CUresult r, size_t pool_before) {
  describe(label, r);
  size_t pool = pool_reserved();
  if (r != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: expected CUDA_ERROR_OUT_OF_MEMORY\n");
    return 1;
  }
  if (pool != pool_before) {
    printf("  FAIL: the pool went from %.0f to %.0f MiB for an allocation it could not make fit\n",
           MIB(pool_before), MIB(pool));
    return 1;
  }
  return 0;
}

/* --------------------------------------------------------------------- [A] */
static int sync_alloc_trims(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraphExec ex = idle_pool_of(need);
  if (ex == NULL) return 1;
  size_t ask = headroom() + need / 2;
  CUdeviceptr d = 0;
  CUresult r = cuMemAlloc(&d, ask);
  describe("cuMemAlloc (fits once the idle pool is trimmed)", r);
  int failed = 0;
  if (r != CUDA_SUCCESS) {
    printf("  FAIL: refused although the idle pool covers it\n");
    failed = 1;
  } else if (pool_reserved() != 0) {
    printf("  FAIL: granted with the pool still at %.0f MiB\n", MIB(pool_reserved()));
    failed = 1;
  }
  if (r == CUDA_SUCCESS) {
    r = launch_and_wait(ex);
    describe("cuGraphLaunch (pool trimmed, headroom taken)", r);
    if (r != CUDA_ERROR_OUT_OF_MEMORY) {
      printf("  FAIL: the launch mapped %.0f MiB past the limit\n", MIB(need));
      failed = 1;
    }
    cuMemFree(d);
  }
  r = launch_and_wait(ex);
  describe("cuGraphLaunch (headroom freed)", r);
  if (r != CUDA_SUCCESS) failed = 1;
  cuGraphExecDestroy(ex);
  return failed;
}

/* --------------------------------------------------------------------- [B] */
static int async_alloc_trims(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraphExec ex = idle_pool_of(need);
  if (ex == NULL) return 1;
  size_t ask = headroom() + need / 2;
  CUdeviceptr d = 0;
  CUresult r = cuMemAllocAsync(&d, ask, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuMemAllocAsync (not in capture, fits once trimmed)", r);
  int failed = 0;
  if (r != CUDA_SUCCESS) {
    printf("  FAIL: refused although the idle pool covers it\n");
    failed = 1;
  } else {
    if (pool_reserved() != 0) {
      printf("  FAIL: granted with the pool still at %.0f MiB\n", MIB(pool_reserved()));
      failed = 1;
    }
    cuMemFreeAsync(d, g_stream);
    cuStreamSynchronize(g_stream);
  }
  cuGraphExecDestroy(ex);
  return failed;
}

/* --------------------------------------------------------------------- [C] */
static int futile_trim_skipped(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraphExec ex = idle_pool_of(need);
  if (ex == NULL) return 1;
  size_t pool = pool_reserved();
  CUdeviceptr d = 0;
  CUresult r = cuMemAlloc(&d, headroom() + pool + ALIGN);
  if (r == CUDA_SUCCESS) cuMemFree(d);
  int failed = refused_untrimmed("cuMemAlloc (past the idle pool too)", r, pool);
  cuGraphExecDestroy(ex);
  return failed;
}

/* --------------------------------------------------------------------- [D] */
static volatile unsigned *g_flag;
static volatile int g_released;

/* Lets the held stream go after WATCHDOG_S unless the case did: a trim that waited for
 * the running graph would otherwise wait for good, the stream waiting on this thread. */
static void *watchdog(void *arg) {
  (void)arg;
  double end = now_s() + WATCHDOG_S;
  while (!g_released && now_s() < end) usleep(10 * 1000);
  *g_flag = 1;
  return NULL;
}

/* The library takes the held graph for idle: its record has no event, being on the one
 * stream graphs have run on, so the trim is tried and what it gives back is what is
 * checked. Launching graphs on a second stream in an earlier case would give the record
 * an event and spare the trim: this case would then pass without testing that. */
static int running_pool_not_given_back(void) {
  volatile unsigned *flag = NULL;
  CUdeviceptr dflag = 0;
  if (cuMemHostAlloc((void **)&flag, sizeof(*flag), CU_MEMHOSTALLOC_DEVICEMAP) != CUDA_SUCCESS ||
      cuMemHostGetDevicePointer(&dflag, (void *)flag, 0) != CUDA_SUCCESS) {
    printf("  SKIP: no mapped host memory\n");
    return -1;
  }
  g_flag = flag;
  trim_pool();
  size_t need = headroom() / 3;
  CUgraphExec ex = captured_exec(need, 0, NULL);
  if (ex == NULL) { cuMemFreeHost((void *)flag); return 1; }
  /* Held behind a wait on the host, the graph is mapped but cannot run. */
  *flag = 0;
  CUresult w = cuStreamWaitValue32(g_stream, dflag, 1, CU_STREAM_WAIT_VALUE_GEQ);
  if (w != CUDA_SUCCESS) {
    describe("cuStreamWaitValue32", w);
    printf("  SKIP: the stream cannot be held here\n");
    cuGraphExecDestroy(ex);
    cuMemFreeHost((void *)flag);
    return -1;
  }
  g_released = 0;
  pthread_t th;
  if (pthread_create(&th, NULL, watchdog, NULL) != 0) {
    *flag = 1;
    cuStreamSynchronize(g_stream);
    cuGraphExecDestroy(ex);
    cuMemFreeHost((void *)flag);
    printf("  FAIL: no watchdog thread\n");
    return 1;
  }
  int failed = 0;
  CUresult r = cuGraphLaunch(ex, g_stream);
  describe("cuGraphLaunch (behind the held stream)", r);
  if (r != CUDA_SUCCESS) failed = 1;
  size_t pool = pool_reserved();
  printf("  pool %.0f MiB, in use by the held graph; headroom %.0f MiB\n", MIB(pool), MIB(headroom()));
  CUdeviceptr d = 0;
  double t0 = now_s();
  r = cuMemAlloc(&d, headroom() + need / 2);
  double took = now_s() - t0;
  g_released = 1;
  pthread_join(th, NULL);   /* the flag is written by now */
  describe("cuMemAlloc (needs the running graph's memory)", r);
  printf("  back after %.3f s\n", took);
  if (took > WAITED_S) {
    printf("  FAIL: the allocation waited for the running graph (the watchdog lets it go at %.0f s)\n",
           WATCHDOG_S);
    failed = 1;
  }
  if (r == CUDA_SUCCESS) {
    printf("  FAIL: granted with the pool at %.0f MiB, still in use: over the limit\n",
           MIB(pool_reserved()));
    cuMemFree(d);
    failed = 1;
  } else if (r != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: expected CUDA_ERROR_OUT_OF_MEMORY\n");
    failed = 1;
  }
  cuStreamSynchronize(g_stream);
  r = cuMemAlloc(&d, headroom() + need / 2);
  describe("cuMemAlloc (the graph finished)", r);
  if (r != CUDA_SUCCESS) {
    printf("  FAIL: refused although the pool is idle now\n");
    failed = 1;
  } else {
    cuMemFree(d);
  }
  cuGraphExecDestroy(ex);
  cuMemFreeHost((void *)flag);
  return failed;
}

/* --------------------------------------------------------------------- [E] */
/* The pool holds a block a graph left allocated (held) and an idle one; the request is
 * short of more than the idle block, less than both. Counted right, the idle part does
 * not cover it and nothing is trimmed. Counting the held block as idle would trim -- the
 * driver keeps the held block, so the idle one goes and the pool shrinks: that is what
 * fails here, not the allocation, which is refused either way. */
static int held_blocks_not_idle(void) {
  trim_pool();
  size_t room = headroom();
  size_t held = room / 4, idle = room / 8;
  CUdeviceptr kept = 0;
  CUgraphExec eh = captured_exec(held, 1, &kept);
  if (eh == NULL) return 1;
  CUresult r = launch_and_wait(eh);
  describe("cuGraphLaunch (leaves its block allocated)", r);
  if (r != CUDA_SUCCESS) { cuGraphExecDestroy(eh); return 1; }
  CUgraphExec ei = idle_pool_of(idle);
  if (ei == NULL) { cuMemFree(kept); cuGraphExecDestroy(eh); return 1; }
  size_t pool = pool_reserved();
  CUdeviceptr d = 0;
  r = cuMemAlloc(&d, headroom() + idle + held / 2);
  if (r == CUDA_SUCCESS) cuMemFree(d);
  int failed = refused_untrimmed("cuMemAlloc (short of more than the idle block)", r, pool);
  CHECK_DRV_API(cuMemFree(kept));
  r = cuMemAlloc(&d, headroom() + idle + held / 2);
  describe("cuMemAlloc (the held block freed, all of the pool idle)", r);
  if (r != CUDA_SUCCESS) {
    printf("  FAIL: refused although the pool is idle now\n");
    failed = 1;
  } else {
    cuMemFree(d);
  }
  cuGraphExecDestroy(ei);
  cuGraphExecDestroy(eh);
  return failed;
}

/* --------------------------------------------------------------------- [F] */
static int no_trim_with_capture_open(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraphExec ex = idle_pool_of(need);
  if (ex == NULL) return 1;
  CUstream other;
  CHECK_DRV_API(cuStreamCreate(&other, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamBeginCapture(other, CU_STREAM_CAPTURE_MODE_RELAXED));
  size_t pool = pool_reserved();
  CUdeviceptr d = 0;
  CUresult r = cuMemAlloc(&d, headroom() + need / 2);
  if (r == CUDA_SUCCESS) cuMemFree(d);
  int failed = refused_untrimmed("cuMemAlloc (a capture open on another stream)", r, pool);
  CUgraph g = NULL;
  CUresult e = cuStreamEndCapture(other, &g);
  if (g != NULL) cuGraphDestroy(g);
  if (e != CUDA_SUCCESS) {
    describe("cuStreamEndCapture", e);
    failed = 1;
  }
  r = cuMemAlloc(&d, headroom() + need / 2);
  describe("cuMemAlloc (the capture ended)", r);
  if (r != CUDA_SUCCESS) {
    printf("  FAIL: refused although no capture is open now\n");
    failed = 1;
  } else {
    cuMemFree(d);
  }
  cuStreamDestroy(other);
  cuGraphExecDestroy(ex);
  return failed;
}

/* --------------------------------------------------------------------- [G] */
static int launch_not_trimmed_for(void) {
  trim_pool();
  size_t a = headroom() / 4;
  CUgraphExec ea = idle_pool_of(a);
  if (ea == NULL) return 1;
  CUgraphExec eb = captured_exec(a * 2, 0, NULL);
  if (eb == NULL) { cuGraphExecDestroy(ea); return 1; }
  size_t pool = pool_reserved();
  CUdeviceptr filler = 0;
  size_t room = headroom();
  if (room > a / 2) CHECK_DRV_API(cuMemAlloc(&filler, room - a / 2));
  CUresult r = launch_and_wait(eb);
  int failed = refused_untrimmed("cuGraphLaunch (grows past the headroom)", r, pool);
  if (filler) cuMemFree(filler);
  cuGraphExecDestroy(eb);
  cuGraphExecDestroy(ea);
  return failed;
}

int main(void) {
  VGPU_REQUIRE_PRELOAD();
  const char *limit = getenv("CUDA_MEM_LIMIT");
  if (limit == NULL || *limit == '\0') {
    printf("SKIP (needs a memory limit: CUDA_MEM_LIMIT)\n");
    return VGPU_TEST_RC_SKIP;
  }
  CHECK_DRV_API(cuInit(0));
  CHECK_DRV_API(cuDeviceGet(&g_dev, TEST_DEVICE_ID));
  CUcontext ctx;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, g_dev));
  CHECK_DRV_API(cuStreamCreate(&g_stream, CU_STREAM_NON_BLOCKING));

  int failures = 0, skipped = 0, rc;
#define RUN_CASE(title, call) do { \
    printf(title "\n"); \
    rc = (call); \
    if (rc < 0) skipped++; else failures += rc > 0; \
  } while (0)
  RUN_CASE("[A] cuMemAlloc has the idle pool trimmed for it", sync_alloc_trims());
  RUN_CASE("[B] so does cuMemAllocAsync outside a capture", async_alloc_trims());
  RUN_CASE("[C] a request the idle pool cannot cover leaves the pool alone", futile_trim_skipped());
  RUN_CASE("[D] a running graph's pool memory is not given back", running_pool_not_given_back());
  RUN_CASE("[E] blocks a graph left allocated are not idle", held_blocks_not_idle());
  RUN_CASE("[F] nothing is trimmed with a capture open", no_trim_with_capture_open());
  RUN_CASE("[G] a graph launch is not given the pool by a trim", launch_not_trimmed_for());
#undef RUN_CASE
  trim_pool();
  cuStreamDestroy(g_stream);
  cuCtxDestroy(ctx);
  return vgpu_test_verdict(failures, skipped);
}
