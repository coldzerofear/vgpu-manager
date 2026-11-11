/*
 * Graph-memory admission at upload / launch (window B).
 *
 * A graph's allocations are mapped from the device's graph pool inside
 * cuGraphUpload / cuGraphLaunch (or an instantiation with
 * CUDA_GRAPH_INSTANTIATE_FLAG_UPLOAD); until then neither NVML nor the ledger
 * holds them. So an allocation made after the capture ended could take the
 * headroom the graph needs, and the launch would map past the limit. The hooks
 * check the pool's growth (alloc-node total minus what the pool already
 * reserves) against the limit at that moment and refuse with
 * CUDA_ERROR_OUT_OF_MEMORY when it does not fit.
 *
 *   [A] launch: refused while the headroom is taken, accepted once it is freed
 *   [B] a replay of a graph whose pool is already mapped is never refused
 *   [C] cuGraphUpload: refused / accepted the same way
 *   [D] cuGraphInstantiateWithParams with the UPLOAD flag: refused / accepted
 *   [E] a hand-built graph's alloc node: launch refused / accepted
 *   [F] a larger graph after a smaller one: only the pool's growth is checked
 *   [G] after a trim returns the pool's pages, a launched exec is checked again
 *   [H] a graph allocating on two devices is checked on each (needs 2 GPUs)
 *   [I] graphs past what a fixed table of 256 held are still checked
 *   [J] graphs stay checked in a process cycling contexts
 *   [K] a cuGraphExecUpdate under another context leaves the exec's record with its own
 *   [L] an idle pool counts for what is captured next, and only as far as it goes
 *   [M] temporaries reused inside a graph are not checked as their sum
 *   [N] what one capture already charged counts as held by the pool too
 *       ([L] and [N] run in each capture mode: global, thread-local, relaxed)
 *   [O] blocks a graph leaves allocated are not lent out again until freed
 *   [P] the pool memory of a graph still running on another stream is not either
 *   [Q] parallel branches of one graph do not reuse each other's blocks, with more than
 *       64 free nodes too
 *   [R] none of it disturbs another thread's capture in global mode
 *   [S] nor when that thread begins captures while launches go on: the check and the
 *       event calls must not straddle the beginning of a capture
 *   [T] the first launch after an idle gap, waiting for its stream (SM limit under 100),
 *       holds neither a capture beginning nor other launches off
 *   [U] with a thread blocked enqueuing on a full stream, a capture beginning and a
 *       launch whose event has to be made hold other launches off neither: the driver
 *       makes those two wait until the stream drains, and what drains it may be one of
 *       those other launches
 *   [V] a launch counts as running from its admission on: another stream's launch made
 *       while the first is still in the driver, its memory mapped, is not lent it
 *   [W] the per-thread default stream counts as a stream of its own per thread
 *   [X] a synchronize of the stream, or of its context, settles what it waited for: the
 *       stream stops holding on to the largest graph it ran
 *   [Y] graphs running on more streams than a fixed table of 32 held are all counted
 *   [Z] a launch on a stream of a context that is not the current one is counted, and so
 *       is a graph's pool memory on its second device (needs 2 GPUs)
 *
 * Each case trims the graph pool first so it starts with nothing reserved.
 * Built twice (_ptsz variant) so the per-thread-default-stream hooks are
 * covered too.
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so CUDA_MEM_LIMIT=4096m ./test_graph_mem_admission
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

static CUdevice g_dev;
static CUstream g_stream;
static CUstreamCaptureMode g_mode = CU_STREAM_CAPTURE_MODE_RELAXED;

static int preloaded(void) {
  const char *p = getenv("LD_PRELOAD");
  return p != NULL && strstr(p, "libvgpu-control") != NULL;
}

static void describe(const char *label, CUresult res) {
  const char *name = NULL;
  if (cuGetErrorName(res, &name) != CUDA_SUCCESS || name == NULL) name = "?";
  printf("  %-48s -> %d (%s)\n", label, (int)res, name);
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

/* A captured graph that allocates `bytes`, touches it, and frees it. */
static CUgraph captured_graph(size_t bytes) {
  CUgraph g = NULL;
  CUdeviceptr d = 0;
  if (cuStreamBeginCapture(g_stream, g_mode) != CUDA_SUCCESS) return NULL;
  CUresult a = cuMemAllocAsync(&d, bytes, g_stream);
  if (a == CUDA_SUCCESS) {
    cuMemsetD8Async(d, 1, bytes, g_stream);
    cuMemFreeAsync(d, g_stream);
  }
  cuStreamEndCapture(g_stream, &g);
  if (a != CUDA_SUCCESS) {
    describe("cuMemAllocAsync (in capture)", a);
    if (g != NULL) cuGraphDestroy(g);
    return NULL;
  }
  return g;
}

/* Take all of the headroom but `leave` bytes. Returns the filler (0 if none). */
static CUdeviceptr fill_leaving(size_t leave) {
  size_t room = headroom();
  if (room <= leave) return 0;
  CUdeviceptr d = 0;
  if (cuMemAlloc(&d, room - leave) != CUDA_SUCCESS) return 0;
  return d;
}

/* --------------------------------------------------------------------- [A] */
static int launch_admission(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraph g = captured_graph(need);
  if (g == NULL) return 1;
  CUgraphExec ex = NULL;
  CHECK_DRV_API(cuGraphInstantiateWithFlags(&ex, g, 0));
  cuGraphDestroy(g);

  int failed = 0;
  CUdeviceptr filler = fill_leaving(need / 2);
  printf("  graph needs %.0f MiB; %.0f MiB left after the filler\n", MIB(need), MIB(headroom()));
  CUresult r = cuGraphLaunch(ex, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphLaunch (headroom taken)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: the launch mapped %.0f MiB past the limit\n", MIB(need));
    failed = 1;
  } else if (pool_reserved() != 0) {
    printf("  FAIL: a refused launch still mapped %.0f MiB\n", MIB(pool_reserved()));
    failed = 1;
  }
  if (filler) cuMemFree(filler);
  r = cuGraphLaunch(ex, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphLaunch (headroom freed)", r);
  if (r != CUDA_SUCCESS) failed = 1;
  cuGraphExecDestroy(ex);
  return failed;
}

/* --------------------------------------------------------------------- [B] */
static int steady_replay_not_refused(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraph g = captured_graph(need);
  if (g == NULL) return 1;
  CUgraphExec ex = NULL;
  CHECK_DRV_API(cuGraphInstantiateWithFlags(&ex, g, 0));
  cuGraphDestroy(g);
  CUresult r = cuGraphLaunch(ex, g_stream);
  cuStreamSynchronize(g_stream);
  if (r != CUDA_SUCCESS) { describe("cuGraphLaunch (first)", r); cuGraphExecDestroy(ex); return 1; }

  /* The pool now holds the graph's pages and NVML counts them, so taking the
   * rest of the headroom must not make a replay look like new memory. */
  CUdeviceptr filler = fill_leaving(0);
  int failed = 0;
  for (int i = 0; i < 50 && !failed; i++) {
    r = cuGraphLaunch(ex, g_stream);
    if (r != CUDA_SUCCESS) {
      describe("cuGraphLaunch (replay, pool already mapped)", r);
      printf("  FAIL: replay %d refused although it maps nothing new\n", i);
      failed = 1;
    }
  }
  cuStreamSynchronize(g_stream);
  if (!failed) printf("  50 replays with the headroom otherwise exhausted: all accepted\n");
  if (filler) cuMemFree(filler);
  cuGraphExecDestroy(ex);
  return failed;
}

/* --------------------------------------------------------------------- [C] */
static int upload_admission(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraph g = captured_graph(need);
  if (g == NULL) return 1;
  CUgraphExec ex = NULL;
  CHECK_DRV_API(cuGraphInstantiateWithFlags(&ex, g, 0));
  cuGraphDestroy(g);

  int failed = 0;
  CUdeviceptr filler = fill_leaving(need / 2);
  CUresult r = cuGraphUpload(ex, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphUpload (headroom taken)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY || pool_reserved() != 0) {
    printf("  FAIL: the upload was not refused (pool %.0f MiB)\n", MIB(pool_reserved()));
    failed = 1;
  }
  if (filler) cuMemFree(filler);
  r = cuGraphUpload(ex, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphUpload (headroom freed)", r);
  if (r != CUDA_SUCCESS || pool_reserved() < need) failed = 1;
  cuGraphExecDestroy(ex);
  return failed;
}

/* --------------------------------------------------------------------- [D] */
static int instantiate_upload_admission(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraph g = captured_graph(need);
  if (g == NULL) return 1;

  CUDA_GRAPH_INSTANTIATE_PARAMS p;
  memset(&p, 0, sizeof(p));
  p.flags = CUDA_GRAPH_INSTANTIATE_FLAG_UPLOAD;
  p.hUploadStream = g_stream;

  int failed = 0;
  CUdeviceptr filler = fill_leaving(need / 2);
  CUgraphExec ex = NULL;
  CUresult r = cuGraphInstantiateWithParams(&ex, g, &p);
  cuStreamSynchronize(g_stream);
  describe("InstantiateWithParams+UPLOAD (headroom taken)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY || pool_reserved() != 0) {
    printf("  FAIL: instantiate-with-upload was not refused (pool %.0f MiB)\n", MIB(pool_reserved()));
    failed = 1;
    if (r == CUDA_SUCCESS) cuGraphExecDestroy(ex);
  }
  if (r == CUDA_ERROR_OUT_OF_MEMORY && p.result_out != CUDA_GRAPH_INSTANTIATE_ERROR) {
    printf("  FAIL: refused, but result_out says %d\n", (int)p.result_out);
    failed = 1;
  }
  p.result_out = CUDA_GRAPH_INSTANTIATE_SUCCESS;
  if (filler) cuMemFree(filler);
  ex = NULL;
  r = cuGraphInstantiateWithParams(&ex, g, &p);
  cuStreamSynchronize(g_stream);
  describe("InstantiateWithParams+UPLOAD (headroom freed)", r);
  if (r != CUDA_SUCCESS || pool_reserved() < need) failed = 1;
  if (r == CUDA_SUCCESS) cuGraphExecDestroy(ex);
  cuGraphDestroy(g);
  return failed;
}

/* --------------------------------------------------------------------- [E] */
static int hand_built_admission(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraph g = NULL;
  CHECK_DRV_API(cuGraphCreate(&g, 0));
  CUDA_MEM_ALLOC_NODE_PARAMS ap;
  memset(&ap, 0, sizeof(ap));
  ap.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  ap.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  ap.poolProps.location.id = g_dev;
  ap.bytesize = need;
  CUgraphNode an = NULL, fn = NULL;
  CHECK_DRV_API(cuGraphAddMemAllocNode(&an, g, NULL, 0, &ap));
  CHECK_DRV_API(cuGraphAddMemFreeNode(&fn, g, &an, 1, ap.dptr));
  CUgraphExec ex = NULL;
  CHECK_DRV_API(cuGraphInstantiateWithFlags(&ex, g, 0));
  cuGraphDestroy(g);

  int failed = 0;
  CUdeviceptr filler = fill_leaving(need / 2);
  CUresult r = cuGraphLaunch(ex, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphLaunch (hand-built, headroom taken)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) failed = 1;
  if (filler) cuMemFree(filler);
  r = cuGraphLaunch(ex, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphLaunch (hand-built, headroom freed)", r);
  if (r != CUDA_SUCCESS) failed = 1;
  cuGraphExecDestroy(ex);
  return failed;
}

/* --------------------------------------------------------------------- [F] */
static int growth_only(void) {
  trim_pool();
  size_t small = headroom() / 6;
  CUgraph gs = captured_graph(small), gb = captured_graph(small * 2);
  if (gs == NULL || gb == NULL) return 1;
  CUgraphExec es = NULL, eb = NULL;
  CHECK_DRV_API(cuGraphInstantiateWithFlags(&es, gs, 0));
  CHECK_DRV_API(cuGraphInstantiateWithFlags(&eb, gb, 0));
  cuGraphDestroy(gs);
  cuGraphDestroy(gb);
  CHECK_DRV_API(cuGraphLaunch(es, g_stream));
  cuStreamSynchronize(g_stream);

  /* The pool holds `small`; the bigger graph grows it by another `small`.
   * Leaving 1.5 x small must admit it (growth fits), leaving 0.5 x small must
   * not. Checking the full 2 x small need instead would refuse both. */
  int failed = 0;
  CUdeviceptr filler = fill_leaving(small + small / 2);
  CUresult r = cuGraphLaunch(eb, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphLaunch (2x graph, growth fits)", r);
  if (r != CUDA_SUCCESS) failed = 1;
  if (filler) cuMemFree(filler);

  trim_pool();
  CHECK_DRV_API(cuGraphLaunch(es, g_stream));
  cuStreamSynchronize(g_stream);
  filler = fill_leaving(small / 2);
  r = cuGraphLaunch(eb, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphLaunch (2x graph, growth does not fit)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) failed = 1;
  if (filler) cuMemFree(filler);
  cuGraphExecDestroy(es);
  cuGraphExecDestroy(eb);
  return failed;
}

/* --------------------------------------------------------------------- [G] */
/* Once a trim has returned the pool's pages, the next launch of an exec that
 * already ran maps them anew, so it has to pass the check again. */
static int trim_rechecks(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraph g = captured_graph(need);
  if (g == NULL) return 1;
  CUgraphExec ex = NULL;
  CHECK_DRV_API(cuGraphInstantiateWithFlags(&ex, g, 0));
  cuGraphDestroy(g);
  int failed = 0;
  CUresult r = cuGraphLaunch(ex, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphLaunch (first, fits)", r);
  if (r != CUDA_SUCCESS) failed = 1;

  trim_pool();
  printf("  pool after trim: %.0f MiB\n", MIB(pool_reserved()));
  CUdeviceptr filler = fill_leaving(need / 2);
  r = cuGraphLaunch(ex, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphLaunch (after trim, headroom taken)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: after the trim the launch mapped %.0f MiB past the limit\n", MIB(pool_reserved()));
    failed = 1;
  }
  if (filler) cuMemFree(filler);
  r = cuGraphLaunch(ex, g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphLaunch (after trim, headroom freed)", r);
  if (r != CUDA_SUCCESS) failed = 1;
  cuGraphExecDestroy(ex);
  return failed;
}

/* --------------------------------------------------------------------- [H] */
/* A graph allocating on two devices is checked on each of them: its launch is
 * refused while either device's headroom is taken -- including the device
 * that is not the current context's -- and accepted once freed. Never more
 * than one device lock is held, so it cannot deadlock. Needs two devices. */
static CUresult launch_and_wait(CUgraphExec ex) {
  CUresult r = cuGraphLaunch(ex, g_stream);
  cuStreamSynchronize(g_stream);
  return r;
}

static int multi_device_checked(void) {
  int count = 0;
  cuDeviceGetCount(&count);
  if (count < 2) { printf("  SKIP (needs 2 devices, have %d)\n", count); return -1; }
  CUdevice other;
  CHECK_DRV_API(cuDeviceGet(&other, g_dev == 0 ? 1 : 0));
  CUcontext octx = NULL;
  CHECK_DRV_API(cuDevicePrimaryCtxRetain(&octx, other));
  trim_pool();
  cuDeviceGraphMemTrim(other);
  size_t need = headroom() / 3;
  CUgraph g = NULL;
  CHECK_DRV_API(cuGraphCreate(&g, 0));
  CUgraphNode prev = NULL;
  CUdevice devs[2] = {g_dev, other};
  for (int i = 0; i < 2; i++) {
    CUDA_MEM_ALLOC_NODE_PARAMS ap;
    memset(&ap, 0, sizeof(ap));
    ap.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
    ap.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    ap.poolProps.location.id = devs[i];
    ap.bytesize = need;
    CUgraphNode an = NULL, fn = NULL;
    CHECK_DRV_API(cuGraphAddMemAllocNode(&an, g, prev ? &prev : NULL, prev ? 1 : 0, &ap));
    CHECK_DRV_API(cuGraphAddMemFreeNode(&fn, g, &an, 1, ap.dptr));
    prev = fn;
  }
  CUgraphExec ex = NULL;
  CHECK_DRV_API(cuGraphInstantiateWithFlags(&ex, g, 0));
  cuGraphDestroy(g);

  int failed = 0;
  /* Current device full. */
  CUdeviceptr filler = fill_leaving(need / 2);
  CUresult r = launch_and_wait(ex);
  describe("cuGraphLaunch (current device's headroom taken)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) failed = 1;
  if (filler) cuMemFree(filler);
  /* The other device full: checked by the allocations' location, not by the
   * current context. */
  CHECK_DRV_API(cuCtxPushCurrent(octx));
  filler = fill_leaving(need / 2);
  CUcontext popped = NULL;
  CHECK_DRV_API(cuCtxPopCurrent(&popped));
  r = launch_and_wait(ex);
  describe("cuGraphLaunch (other device's headroom taken)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) failed = 1;
  if (filler) {
    CHECK_DRV_API(cuCtxPushCurrent(octx));
    cuMemFree(filler);
    CHECK_DRV_API(cuCtxPopCurrent(&popped));
  }
  r = launch_and_wait(ex);
  describe("cuGraphLaunch (both freed)", r);
  if (r != CUDA_SUCCESS) failed = 1;
  cuGraphExecDestroy(ex);
  cuCtxSynchronize();
  cuDeviceGraphMemTrim(other);
  cuDevicePrimaryCtxRelease(other);
  return failed;
}

/* A hand-built alloc-node graph's exec, with its graph gone. */
static CUgraphExec hand_exec(size_t bytes) {
  CUgraph g = NULL;
  if (cuGraphCreate(&g, 0) != CUDA_SUCCESS) return NULL;
  CUDA_MEM_ALLOC_NODE_PARAMS ap;
  memset(&ap, 0, sizeof(ap));
  ap.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  ap.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  ap.poolProps.location.id = g_dev;
  ap.bytesize = bytes;
  CUgraphNode an = NULL, fn = NULL;
  CUgraphExec ex = NULL;
  if (cuGraphAddMemAllocNode(&an, g, NULL, 0, &ap) != CUDA_SUCCESS ||
      cuGraphAddMemFreeNode(&fn, g, &an, 1, ap.dptr) != CUDA_SUCCESS ||
      cuGraphInstantiateWithFlags(&ex, g, 0) != CUDA_SUCCESS) {
    ex = NULL;
  }
  cuGraphDestroy(g);
  return ex;
}

/* The big graph must be refused with the headroom taken, however many graphs
 * came before it. */
static int big_graph_is_refused(const char *what) {
  size_t need = headroom() / 3;
  CUgraphExec big = hand_exec(need);
  if (big == NULL) { printf("  FAIL: could not build the big graph\n"); return 1; }
  CUdeviceptr filler = fill_leaving(need / 2);
  CUresult r = cuGraphLaunch(big, g_stream);
  cuStreamSynchronize(g_stream);
  describe(what, r);
  int failed = r != CUDA_ERROR_OUT_OF_MEMORY;
  if (failed) printf("  FAIL: the graph was not checked\n");
  if (filler) cuMemFree(filler);
  cuGraphExecDestroy(big);
  return failed;
}

/* --------------------------------------------------------------------- [I] */
/* Past what a fixed table of 256 held, a graph went unthrottled and unchecked.
 * 300 because the driver itself refuses alloc nodes past a number that depends on the
 * card, about 1000 on a V100 and 600 on an RTX 3080, and fewer with big ones among them;
 * the growth of the table past its first size is exercised in
 * test/nogpu/test_slot_table.c. */
static int many_live_graphs(void) {
  enum { LIVE = 300 };
  trim_pool();
  static CUgraphExec ex[LIVE];
  int made = 0;
  for (int i = 0; i < LIVE; i++) {
    ex[i] = hand_exec(1 << 20);
    if (ex[i] == NULL) break;
    made++;
  }
  printf("  %d live graphs\n", made);
  int failed = made != LIVE;
  failed += big_graph_is_refused("cuGraphLaunch (after them, headroom taken)");
  /* An early one still has its record. */
  CUresult r = cuGraphLaunch(ex[0], g_stream);
  cuStreamSynchronize(g_stream);
  describe("cuGraphLaunch (the first of them)", r);
  if (r != CUDA_SUCCESS) failed++;
  for (int i = 0; i < made; i++) cuGraphExecDestroy(ex[i]);
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [J] */
/* Graphs stay checked in a process cycling contexts, whose execs go with them and
 * are never destroyed. With a growable table this cannot see a leak of records, only
 * what one would break: probe_churn's table stats are what catch that. */
static int context_teardown_frees_records(CUcontext main_ctx) {
  for (int i = 0; i < 300; i++) {
    CUcontext c = NULL;
    CHECK_DRV_API(CUCTX_CREATE(&c, 0, g_dev));
    CUgraphExec ex = hand_exec(1 << 20);
    if (ex == NULL) { printf("  FAIL: could not build a graph\n"); return 1; }
    CHECK_DRV_API(cuCtxDestroy(c));
  }
  CHECK_DRV_API(cuCtxSetCurrent(main_ctx));
  trim_pool();
  return big_graph_is_refused("cuGraphLaunch (after 300 context lifetimes)");
}

/* --------------------------------------------------------------------- [K] */
/* The exec stays its context's when updated under another. Were the record moved, that
 * context going would take the record along, and the graph would go unchecked. */
static int update_keeps_owner(void) {
  trim_pool();
  size_t need = headroom() / 3;
  CUgraph g = captured_graph(need);
  if (g == NULL) { printf("  FAIL: could not build a graph\n"); return 1; }
  CUgraphExec ex = NULL;
  CHECK_DRV_API(cuGraphInstantiateWithFlags(&ex, g, 0));
  CUcontext x = NULL;
  CHECK_DRV_API(CUCTX_CREATE(&x, 0, g_dev));
  CUresult r = cuGraphExecUpdate(ex, g, NULL);
  describe("cuGraphExecUpdate (another context current)", r);
  CHECK_DRV_API(cuCtxDestroy(x));
  cuGraphDestroy(g);
  int failed = 0;
  if (r != CUDA_SUCCESS) {
    printf("  not run: this driver refuses the update\n");
  } else {
    CUdeviceptr filler = fill_leaving(need / 2);
    r = cuGraphLaunch(ex, g_stream);
    describe("cuGraphLaunch (headroom taken)", r);
    if (r != CUDA_ERROR_OUT_OF_MEMORY) {
      printf("  FAIL: the exec went unchecked after its update\n");
      failed = 1;
    }
    cuStreamSynchronize(g_stream);
    if (filler) cuMemFree(filler);
    r = launch_and_wait(ex);
    describe("cuGraphLaunch (freed)", r);
    if (r != CUDA_SUCCESS) failed = 1;
  }
  cuGraphExecDestroy(ex);
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [L] */
/* A graph bigger than half of the headroom, run, and the same again: the pool holds the
 * second, which used to be refused while being captured. What the pool cannot hold is
 * still refused, in the same capture and past what it has. */
static int idle_pool_counts(void) {
  trim_pool();
  size_t x = headroom() * 2 / 3;
  int failed = 0;
  CUgraph g = captured_graph(x);
  if (g == NULL) { printf("  FAIL: could not build the first graph\n"); return 1; }
  CUgraphExec first = NULL;
  CHECK_DRV_API(cuGraphInstantiateWithFlags(&first, g, 0));
  cuGraphDestroy(g);
  CUresult r = launch_and_wait(first);
  describe("cuGraphLaunch (first graph)", r);
  if (r != CUDA_SUCCESS) failed = 1;
  size_t pool = pool_reserved();

  g = captured_graph(x);
  if (g == NULL) {
    printf("  FAIL: refused while capturing what the idle pool holds\n");
    failed = 1;
  } else {
    CUgraphExec second = NULL;
    CHECK_DRV_API(cuGraphInstantiateWithFlags(&second, g, 0));
    cuGraphDestroy(g);
    r = launch_and_wait(second);
    describe("cuGraphLaunch (second graph)", r);
    if (r != CUDA_SUCCESS || pool_reserved() != pool) failed = 1;
    cuGraphExecDestroy(second);
  }
  CUgraphExec node = hand_exec(x);
  if (node == NULL) {
    printf("  FAIL: refused an alloc node the idle pool holds\n");
    failed = 1;
  } else {
    r = launch_and_wait(node);
    describe("cuGraphLaunch (hand-built graph)", r);
    if (r != CUDA_SUCCESS || pool_reserved() != pool) failed = 1;
    cuGraphExecDestroy(node);
  }

  /* Two at once in one capture: the pool holds one. */
  CUdeviceptr a = 0, b = 0;
  CUgraph two = NULL;
  CHECK_DRV_API(cuStreamBeginCapture(g_stream, g_mode));
  CUresult ra = cuMemAllocAsync(&a, x, g_stream);
  CUresult rb = cuMemAllocAsync(&b, x, g_stream);
  describe("capture: cuMemAllocAsync (the pool holds it)", ra);
  describe("capture: cuMemAllocAsync (it does not)", rb);
  if (ra == CUDA_SUCCESS) cuMemFreeAsync(a, g_stream);
  if (rb == CUDA_SUCCESS) cuMemFreeAsync(b, g_stream);
  cuStreamEndCapture(g_stream, &two);
  if (two != NULL) cuGraphDestroy(two);
  if (ra != CUDA_SUCCESS || rb != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: expected the first to fit and the second to be refused\n");
    failed = 1;
  }

  /* Past the pool and the headroom together. */
  CUdeviceptr big = 0;
  CUgraph over = NULL;
  size_t past = pool + headroom() + (64u << 20);
  CHECK_DRV_API(cuStreamBeginCapture(g_stream, g_mode));
  r = cuMemAllocAsync(&big, past, g_stream);
  describe("capture: cuMemAllocAsync (past pool and headroom)", r);
  if (r == CUDA_SUCCESS) cuMemFreeAsync(big, g_stream);
  cuStreamEndCapture(g_stream, &over);
  if (over != NULL) cuGraphDestroy(over);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) failed = 1;

  cuGraphExecDestroy(first);
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [M] */
/* A graph of sizes[n], each freed before the next allocation if chained, or all held. */
static CUgraphExec sized_graph(const size_t *sizes, int n, int chained) {
  CUgraph g = NULL;
  CUgraphNode prev = NULL;
  CUdeviceptr ptrs[32];
  CUgraphExec ex = NULL;
  if (cuGraphCreate(&g, 0) != CUDA_SUCCESS) return NULL;
  for (int i = 0; i < n; i++) {
    CUDA_MEM_ALLOC_NODE_PARAMS ap;
    memset(&ap, 0, sizeof(ap));
    ap.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
    ap.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    ap.poolProps.location.id = g_dev;
    ap.bytesize = sizes[i];
    CUgraphNode an = NULL;
    if (cuGraphAddMemAllocNode(&an, g, prev ? &prev : NULL, prev ? 1 : 0, &ap) != CUDA_SUCCESS ||
        (chained && cuGraphAddMemFreeNode(&an, g, &an, 1, ap.dptr) != CUDA_SUCCESS)) {
      cuGraphDestroy(g);
      return NULL;
    }
    prev = an;
    ptrs[i] = ap.dptr;
  }
  for (int i = 0; !chained && i < n; i++) {
    CUgraphNode fn = NULL;
    if (cuGraphAddMemFreeNode(&fn, g, &prev, 1, ptrs[i]) != CUDA_SUCCESS) {
      cuGraphDestroy(g);
      return NULL;
    }
    prev = fn;
  }
  if (cuGraphInstantiateWithFlags(&ex, g, 0) != CUDA_SUCCESS) ex = NULL;
  cuGraphDestroy(g);
  return ex;
}

/* The launch's result, with what the pool came to if it went through. */
static CUresult launch_sized(const char *what, const size_t *sizes, int n, int chained, size_t *pool) {
  trim_pool();
  CUgraphExec ex = sized_graph(sizes, n, chained);
  if (ex == NULL) { printf("  %s: could not build\n", what); return CUDA_ERROR_UNKNOWN; }
  CUresult r = launch_and_wait(ex);
  *pool = pool_reserved();
  describe(what, r);
  printf("  pool %.0f MiB\n", MIB(*pool));
  cuGraphExecDestroy(ex);
  trim_pool();
  return r;
}

static int temporaries_are_reused(void) {
  trim_pool();
  size_t room = headroom();
  size_t b = room / 4;
  size_t same[8] = {b, b, b, b, b, b, b, b};   /* the sum is twice the headroom */
  size_t pool = 0;
  int failed = 0;
  if (launch_sized("cuGraphLaunch (8 in turn)", same, 8, 1, &pool) != CUDA_SUCCESS) {
    printf("  FAIL: checked as their sum\n");
    failed = 1;
  }
  if (pool >= 2 * b) { printf("  FAIL: expected about one block\n"); failed = 1; }

  /* Captured, the way an application's temporaries get there. */
  CUgraph g = NULL;
  CUresult bad = CUDA_SUCCESS;
  CHECK_DRV_API(cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_RELAXED));
  for (int i = 0; i < 8 && bad == CUDA_SUCCESS; i++) {
    CUdeviceptr d = 0;
    bad = cuMemAllocAsync(&d, b, g_stream);
    if (bad == CUDA_SUCCESS) {
      cuMemsetD8Async(d, 1, b, g_stream);
      cuMemFreeAsync(d, g_stream);
    }
  }
  cuStreamEndCapture(g_stream, &g);
  describe("capture: 8 in turn", bad);
  if (bad != CUDA_SUCCESS || g == NULL) {
    failed = 1;
  } else {
    CUgraphExec ex = NULL;
    CHECK_DRV_API(cuGraphInstantiateWithFlags(&ex, g, 0));
    CUresult r = launch_and_wait(ex);
    describe("cuGraphLaunch (captured, 8 in turn)", r);
    if (r != CUDA_SUCCESS) {
      printf("  FAIL: checked as their sum\n");
      failed = 1;
    }
    cuGraphExecDestroy(ex);
  }
  if (g != NULL) cuGraphDestroy(g);

  /* These do need more than the headroom. */
  if (launch_sized("cuGraphLaunch (8 held at once)", same, 8, 0, &pool) != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: a graph holding all of them must be refused\n");
    failed = 1;
  }
  size_t growing[3] = {room / 6, room / 3, room * 3 / 4};
  if (launch_sized("cuGraphLaunch (growing sizes)", growing, 3, 1, &pool) != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: growing sizes take their sum, each fitting does not make it fit\n");
    failed = 1;
  }
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [N] */
/* A pool of two blocks holds two allocations of one capture: its charge for the first
 * is in the pool as well, and is not counted again for the second. */
static int capture_charges_in_pool(void) {
  trim_pool();
  size_t y = headroom() * 45 / 100;
  size_t both[2] = {y, y};
  CUgraphExec ex = sized_graph(both, 2, 0);
  if (ex == NULL) { printf("  FAIL: could not build the graph\n"); return 1; }
  CUresult r = launch_and_wait(ex);
  describe("cuGraphLaunch (two at once)", r);
  cuGraphExecDestroy(ex);
  int failed = r != CUDA_SUCCESS;
  CUdeviceptr a = 0, b = 0;
  CUgraph g = NULL;
  CHECK_DRV_API(cuStreamBeginCapture(g_stream, g_mode));
  CUresult ra = cuMemAllocAsync(&a, y, g_stream);
  CUresult rb = cuMemAllocAsync(&b, y, g_stream);
  describe("capture: first of two", ra);
  describe("capture: second of two", rb);
  if (ra == CUDA_SUCCESS) cuMemFreeAsync(a, g_stream);
  if (rb == CUDA_SUCCESS) cuMemFreeAsync(b, g_stream);
  cuStreamEndCapture(g_stream, &g);
  if (g != NULL) cuGraphDestroy(g);
  if (ra != CUDA_SUCCESS || rb != CUDA_SUCCESS) {
    printf("  FAIL: refused what the pool holds\n");
    failed = 1;
  }
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [O] */
/* A graph that allocates bytes and leaves it allocated, instantiated with flags. */
static CUgraphExec unfreed_graph(size_t bytes, unsigned long long flags, CUdeviceptr *dptr) {
  CUgraph g = NULL;
  CUgraphExec ex = NULL;
  CUDA_MEM_ALLOC_NODE_PARAMS ap;
  memset(&ap, 0, sizeof(ap));
  ap.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  ap.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  ap.poolProps.location.id = g_dev;
  ap.bytesize = bytes;
  CUgraphNode an = NULL;
  if (cuGraphCreate(&g, 0) != CUDA_SUCCESS) return NULL;
  if (cuGraphAddMemAllocNode(&an, g, NULL, 0, &ap) != CUDA_SUCCESS ||
      cuGraphInstantiateWithFlags(&ex, g, flags) != CUDA_SUCCESS) {
    ex = NULL;
  }
  *dptr = ap.dptr;
  cuGraphDestroy(g);
  return ex;
}

/* A graph whose only node frees dptr, allocated by another graph. */
static CUgraphExec freeing_graph(CUdeviceptr dptr) {
  CUgraph g = NULL;
  CUgraphExec ex = NULL;
  CUgraphNode fn = NULL;
  if (cuGraphCreate(&g, 0) != CUDA_SUCCESS) return NULL;
  if (cuGraphAddMemFreeNode(&fn, g, NULL, 0, dptr) != CUDA_SUCCESS ||
      cuGraphInstantiateWithFlags(&ex, g, 0) != CUDA_SUCCESS) {
    ex = NULL;
  }
  cuGraphDestroy(g);
  return ex;
}

/* Frees a block that a failed expectation may have left unallocated: no early return. */
static void free_quietly(CUdeviceptr p) {
  if (p != 0) cuMemFree(p);
}

static int expect_launch(const char *what, CUgraphExec ex, CUresult want) {
  CUresult r = launch_and_wait(ex);
  describe(what, r);
  if (r != want) {
    printf("  FAIL: expected %d\n", (int)want);
    return 1;
  }
  return 0;
}

/* Each unfreed graph found the pool already as big as it needed, so they added up past
 * the limit with nothing refused. */
static int held_blocks_count(void) {
  trim_pool();
  size_t x = headroom() * 2 / 3;
  printf("  pool %.0f MiB, headroom %.0f MiB\n", MIB(pool_reserved()), MIB(headroom()));
  int failed = 0;
  CUdeviceptr p1 = 0, p2 = 0, pa = 0;
  CUgraphExec u1 = unfreed_graph(x, 0, &p1), u2 = unfreed_graph(x, 0, &p2);
  if (u1 == NULL || u2 == NULL) { printf("  FAIL: could not build the graphs\n"); return 1; }

  failed += expect_launch("cuGraphLaunch (first unfreed graph)", u1, CUDA_SUCCESS);
  failed += expect_launch("cuGraphLaunch (second, the first's block held)", u2, CUDA_ERROR_OUT_OF_MEMORY);
  free_quietly(p1);
  failed += expect_launch("cuGraphLaunch (second, after cuMemFree)", u2, CUDA_SUCCESS);
  free_quietly(p2);

  failed += expect_launch("cuGraphLaunch (first again)", u1, CUDA_SUCCESS);
  cuMemFreeAsync(p1, g_stream);
  failed += expect_launch("cuGraphLaunch (second, after cuMemFreeAsync)", u2, CUDA_SUCCESS);
  free_quietly(p2);

  /* Given back by another graph's free node, when that graph runs. */
  failed += expect_launch("cuGraphLaunch (first again)", u1, CUDA_SUCCESS);
  CUgraphExec freer = freeing_graph(p1);
  if (freer == NULL) {
    printf("  FAIL: could not build the freeing graph\n");
    failed++;
    free_quietly(p1);
  } else {
    failed += expect_launch("cuGraphLaunch (second, before the free node ran)", u2, CUDA_ERROR_OUT_OF_MEMORY);
    failed += expect_launch("cuGraphLaunch (the graph freeing the first's block)", freer, CUDA_SUCCESS);
    failed += expect_launch("cuGraphLaunch (second, after it)", u2, CUDA_SUCCESS);
    free_quietly(p2);
    cuGraphExecDestroy(freer);
  }

  /* Freed in a capture: a free node, which gives the block back only when it runs. */
  failed += expect_launch("cuGraphLaunch (first again)", u1, CUDA_SUCCESS);
  CUgraph captured = NULL;
  CUgraphExec captured_freer = NULL;
  if (cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_RELAXED) == CUDA_SUCCESS) {
    CUresult f = cuMemFreeAsync(p1, g_stream);
    if (cuStreamEndCapture(g_stream, &captured) != CUDA_SUCCESS || f != CUDA_SUCCESS ||
        cuGraphInstantiateWithFlags(&captured_freer, captured, 0) != CUDA_SUCCESS) {
      captured_freer = NULL;
    }
    if (captured != NULL) cuGraphDestroy(captured);
  }
  if (captured_freer == NULL) {
    printf("  FAIL: could not capture the free\n");
    failed++;
    free_quietly(p1);
  } else {
    failed += expect_launch("cuGraphLaunch (second, the free only captured)", u2, CUDA_ERROR_OUT_OF_MEMORY);
    failed += expect_launch("cuGraphLaunch (the captured free)", captured_freer, CUDA_SUCCESS);
    failed += expect_launch("cuGraphLaunch (second, after it ran)", u2, CUDA_SUCCESS);
    free_quietly(p2);
    cuGraphExecDestroy(captured_freer);
  }

  /* An upload maps the block but does not hold it: the pool still lends it out. */
  if (cuGraphUpload(u1, g_stream) != CUDA_SUCCESS) failed++;
  cuStreamSynchronize(g_stream);
  failed += expect_launch("cuGraphLaunch (second, the first only uploaded)", u2, CUDA_SUCCESS);
  free_quietly(p2);
  cuGraphExecDestroy(u1);
  cuGraphExecDestroy(u2);

  /* AUTO_FREE_ON_LAUNCH: relaunched, it gets its own block back first. */
  CUgraphExec af = unfreed_graph(x, CUDA_GRAPH_INSTANTIATE_FLAG_AUTO_FREE_ON_LAUNCH, &pa);
  if (af == NULL) {
    printf("  FAIL: could not build the auto-free graph\n");
    failed++;
  } else {
    failed += expect_launch("cuGraphLaunch (auto-free)", af, CUDA_SUCCESS);
    failed += expect_launch("cuGraphLaunch (auto-free, again)", af, CUDA_SUCCESS);
    free_quietly(pa);
    cuGraphExecDestroy(af);
  }

  /* The blocks of a context do not go with it: only a free gives them back. */
  CUcontext x_ctx = NULL, back = NULL, popped = NULL;
  CHECK_DRV_API(cuCtxGetCurrent(&back));
  CHECK_DRV_API(CUCTX_CREATE(&x_ctx, 0, g_dev));
  CUstream xs = NULL;
  CHECK_DRV_API(cuStreamCreate(&xs, CU_STREAM_NON_BLOCKING));
  CUdeviceptr px = 0;
  CUgraphExec ux = unfreed_graph(x, 0, &px);
  CUresult r = ux != NULL ? cuGraphLaunch(ux, xs) : CUDA_ERROR_UNKNOWN;
  cuStreamSynchronize(xs);
  describe("cuGraphLaunch (unfreed, in a context about to go)", r);
  CHECK_DRV_API(cuCtxPopCurrent(&popped));
  CHECK_DRV_API(cuCtxDestroy(x_ctx));
  CHECK_DRV_API(cuCtxSetCurrent(back));
  if (r != CUDA_SUCCESS) failed++;
  /* Even its alloc node is refused now: the pool has nothing to lend. */
  CUdeviceptr p3 = 0;
  CUgraphExec u3 = unfreed_graph(x, 0, &p3);
  if (u3 != NULL) {
    printf("  FAIL: an alloc node the held block leaves no room for was accepted\n");
    failed++;
    cuGraphExecDestroy(u3);
  } else {
    printf("  alloc node (that context gone, its block not)    -> refused\n");
  }
  free_quietly(px);
  u3 = unfreed_graph(x, 0, &p3);
  if (u3 == NULL) {
    printf("  FAIL: refused after the block was freed from this context\n");
    failed++;
  } else {
    failed += expect_launch("cuGraphLaunch (after its cuMemFree from this context)", u3, CUDA_SUCCESS);
    cuMemFree(p3);
    cuGraphExecDestroy(u3);
  }
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [P] */
static volatile int g_release;
static void CUDA_CB hold_stream(void *arg) {
  (void)arg;
  for (int i = 0; !g_release && i < 30000; i++) usleep(1000);
}

static void hold(CUstream s) {
  g_release = 0;
  if (cuLaunchHostFunc(s, hold_stream, NULL) != CUDA_SUCCESS) g_release = 1;
}

static void release(void) {
  g_release = 1;
  cuCtxSynchronize();
}

/* A graph on another stream that has not run yet keeps its pool memory: the next one
 * there needs its own. On the same stream, once the first is done, or for the same exec,
 * it is reused. Run before any case launches on a second stream: the first part is the
 * moment a second stream appears, when what ran before counts as finished. */
static int running_graph_counts(void) {
  trim_pool();
  size_t x = headroom() * 2 / 3;
  printf("  pool %.0f MiB, headroom %.0f MiB\n", MIB(pool_reserved()), MIB(headroom()));
  size_t one[1] = {x};
  int failed = 0;
  CUstream other = NULL;
  CHECK_DRV_API(cuStreamCreate(&other, CU_STREAM_NON_BLOCKING));
  CUgraphExec a = sized_graph(one, 1, 1), b = sized_graph(one, 1, 1);
  if (a == NULL || b == NULL) { printf("  FAIL: could not build the graphs\n"); return 1; }
  failed += expect_launch("cuGraphLaunch (first, to have the pool)", a, CUDA_SUCCESS);

  hold(g_stream);
  CUresult r = cuGraphLaunch(a, g_stream);
  CUresult rb = cuGraphLaunch(b, other);
  release();
  describe("cuGraphLaunch (a second stream, first seen now)", rb);
  if (r != CUDA_SUCCESS || rb != CUDA_SUCCESS) {
    printf("  FAIL: expected both through: until a second stream, no event is kept\n");
    failed++;
  }
  trim_pool();
  failed += expect_launch("cuGraphLaunch (first again, the pool back)", a, CUDA_SUCCESS);

  hold(g_stream);
  r = cuGraphLaunch(a, g_stream);
  rb = cuGraphLaunch(b, other);
  release();
  describe("cuGraphLaunch (another stream, the first not run)", rb);
  if (r != CUDA_SUCCESS || rb != CUDA_ERROR_OUT_OF_MEMORY) { printf("  FAIL: expected it refused\n"); failed++; }
  rb = cuGraphLaunch(b, other);
  cuStreamSynchronize(other);
  describe("cuGraphLaunch (another stream, the first done)", rb);
  if (rb != CUDA_SUCCESS) failed++;

  hold(g_stream);
  r = cuGraphLaunch(a, g_stream);
  rb = cuGraphLaunch(b, g_stream);
  release();
  describe("cuGraphLaunch (same stream, the first not run)", rb);
  if (r != CUDA_SUCCESS || rb != CUDA_SUCCESS) failed++;

  hold(g_stream);
  r = cuGraphLaunch(a, g_stream);
  rb = cuGraphLaunch(a, other);
  release();
  describe("cuGraphLaunch (same exec on another stream, not run)", rb);
  if (r != CUDA_SUCCESS || rb != CUDA_SUCCESS) failed++;

  cuGraphExecDestroy(a);
  cuGraphExecDestroy(b);
  cuStreamDestroy(other);
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [Q] */
/* Two allocations of x on parallel branches take 2x, though one after the other in node
 * order; chained, they take x. */
static int parallel_branches_count(void) {
  trim_pool();
  size_t x = headroom() * 2 / 3;
  printf("  pool %.0f MiB, headroom %.0f MiB\n", MIB(pool_reserved()), MIB(headroom()));
  int failed = 0;
  for (int round = 0; round < 4; round++) {
    int chained = round % 2, padded = round >= 2;
    trim_pool();   /* what the last round's launch left would count against these nodes */
    CUgraph g = NULL;
    CUgraphNode a0, f0, a1, f1;
    CUDA_MEM_ALLOC_NODE_PARAMS p0, p1;
    memset(&p0, 0, sizeof(p0));
    p0.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
    p0.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    p0.poolProps.location.id = g_dev;
    p0.bytesize = x;
    p1 = p0;
    CHECK_DRV_API(cuGraphCreate(&g, 0));
    CHECK_DRV_API(cuGraphAddMemAllocNode(&a0, g, NULL, 0, &p0));
    CHECK_DRV_API(cuGraphAddMemFreeNode(&f0, g, &a0, 1, p0.dptr));
    CHECK_DRV_API(cuGraphAddMemAllocNode(&a1, g, chained ? &f0 : NULL, chained ? 1 : 0, &p1));
    CHECK_DRV_API(cuGraphAddMemFreeNode(&f1, g, &a1, 1, p1.dptr));
    /* A line of its own with 70 small blocks, more free nodes than one word of bits. After
     * the big nodes: their own checks would count these on some cards. */
    CUgraphNode prev = NULL;
    for (int k = 0; padded && k < 70; k++) {
      CUDA_MEM_ALLOC_NODE_PARAMS pk = p0;
      CUgraphNode ak, fk;
      pk.bytesize = 2u << 20;
      CHECK_DRV_API(cuGraphAddMemAllocNode(&ak, g, prev ? &prev : NULL, prev ? 1 : 0, &pk));
      CHECK_DRV_API(cuGraphAddMemFreeNode(&fk, g, &ak, 1, pk.dptr));
      prev = fk;
    }
    CUgraphExec ex = NULL;
    CHECK_DRV_API(cuGraphInstantiateWithFlags(&ex, g, 0));
    cuGraphDestroy(g);
    trim_pool();
    static const char *const what[4] = {"cuGraphLaunch (parallel branches)", "cuGraphLaunch (chained)",
                                        "cuGraphLaunch (parallel, 70 more free nodes)",
                                        "cuGraphLaunch (chained, 70 more free nodes)"};
    failed += expect_launch(what[round], ex, chained ? CUDA_SUCCESS : CUDA_ERROR_OUT_OF_MEMORY);
    cuGraphExecDestroy(ex);
  }
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [R] */
typedef struct {
  CUcontext ctx;
  CUstream stream;
  volatile int phase;   /* 1 capture open, 2 may end */
  CUresult alloc, end;
} capture_thread_t;

static void *capture_in_global_mode(void *arg) {
  capture_thread_t *t = (capture_thread_t *)arg;
  CUgraph g = NULL;
  CUdeviceptr d = 0;
  cuCtxSetCurrent(t->ctx);
  t->end = cuStreamBeginCapture(t->stream, CU_STREAM_CAPTURE_MODE_GLOBAL);
  if (t->end == CUDA_SUCCESS) {
    t->alloc = cuMemAllocAsync(&d, 1u << 20, t->stream);
    if (t->alloc == CUDA_SUCCESS) cuMemFreeAsync(d, t->stream);
  }
  t->phase = 1;
  for (int i = 0; t->phase != 2 && i < 20000; i++) usleep(1000);
  if (t->end == CUDA_SUCCESS) t->end = cuStreamEndCapture(t->stream, &g);
  if (g != NULL) cuGraphDestroy(g);
  return NULL;
}

/* With events in use on this device ([P] ran on two streams), a launch while another
 * thread captures in global mode must leave that capture intact. Querying an event is
 * among what a global capture forbids: without the check the other thread's end returned
 * 901 (RTX 2080 Ti, 595). A graph launched into a capture is refused by the
 * driver itself (900), library or not, so there is nothing of that to test. */
static int captures_left_alone(CUcontext ctx) {
  trim_pool();
  size_t one[1] = {headroom() / 8};
  int failed = 0;
  CUstream other = NULL, cap = NULL;
  CHECK_DRV_API(cuStreamCreate(&other, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&cap, CU_STREAM_NON_BLOCKING));
  CUgraphExec a = sized_graph(one, 1, 1);
  if (a == NULL) { printf("  FAIL: could not build the graph\n"); return 1; }
  /* Records with events behind them, on two streams. */
  launch_and_wait(a);
  cuGraphLaunch(a, other);
  cuStreamSynchronize(other);

  capture_thread_t t;
  memset(&t, 0, sizeof(t));
  t.ctx = ctx;
  t.stream = cap;
  pthread_t th;
  pthread_create(&th, NULL, capture_in_global_mode, &t);
  for (int i = 0; t.phase != 1 && i < 20000; i++) usleep(1000);
  CUresult r1 = cuGraphLaunch(a, g_stream);
  t.phase = 2;
  pthread_join(th, NULL);
  cuCtxSynchronize();
  describe("cuGraphLaunch while another thread captures (global)", r1);
  describe("cuStreamEndCapture of that thread", t.end);
  if (t.alloc != CUDA_SUCCESS || t.end != CUDA_SUCCESS) {
    printf("  FAIL: the other thread's capture was broken\n");
    failed++;
  }

  /* The first launch after an idle gap, which the SM limit (CUDA_CORE_LIMIT under 100)
   * measures with an event it synchronizes on: not with a capture open elsewhere. */
  CUgraph eg = NULL;
  CUgraphNode en = NULL;
  CUgraphExec empty = NULL;
  if (cuGraphCreate(&eg, 0) == CUDA_SUCCESS && cuGraphAddEmptyNode(&en, eg, NULL, 0) == CUDA_SUCCESS &&
      cuGraphInstantiateWithFlags(&empty, eg, 0) == CUDA_SUCCESS) {
    memset(&t, 0, sizeof(t));
    t.ctx = ctx;
    t.stream = cap;
    pthread_create(&th, NULL, capture_in_global_mode, &t);
    for (int i = 0; t.phase != 1 && i < 20000; i++) usleep(1000);
    usleep(300 * 1000);   /* past the 200 ms gap threshold */
    CUresult r2 = cuGraphLaunch(empty, g_stream);
    t.phase = 2;
    pthread_join(th, NULL);
    cuCtxSynchronize();
    describe("cuGraphLaunch after an idle gap, a capture open", r2);
    describe("cuStreamEndCapture of that thread", t.end);
    if (r2 != CUDA_SUCCESS || t.alloc != CUDA_SUCCESS || t.end != CUDA_SUCCESS) {
      printf("  FAIL: the other thread's capture was broken\n");
      failed++;
    }
    cuGraphExecDestroy(empty);
  }
  if (eg != NULL) cuGraphDestroy(eg);

  cuGraphExecDestroy(a);
  cuStreamDestroy(other);
  cuStreamDestroy(cap);
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [S] */
typedef struct {
  CUcontext ctx;
  CUstream stream;
  volatile int stop;
  int captures, broken;
} capture_loop_t;

static void *capture_over_and_over(void *arg) {
  capture_loop_t *t = (capture_loop_t *)arg;
  cuCtxSetCurrent(t->ctx);
  while (!t->stop && t->captures < 2000) {
    CUgraph g = NULL;
    CUdeviceptr d = 0;
    if (cuStreamBeginCapture(t->stream, CU_STREAM_CAPTURE_MODE_GLOBAL) != CUDA_SUCCESS) {
      t->broken++;
      break;
    }
    if (cuMemAllocAsync(&d, 1u << 20, t->stream) == CUDA_SUCCESS) cuMemFreeAsync(d, t->stream);
    if (cuStreamEndCapture(t->stream, &g) != CUDA_SUCCESS) t->broken++;
    if (g != NULL) cuGraphDestroy(g);
    t->captures++;
  }
  return NULL;
}

static int captures_begun_during_launches(CUcontext ctx) {
  trim_pool();
  size_t one[1] = {headroom() / 16};
  int failed = 0;
  CUstream other = NULL, cap = NULL;
  CHECK_DRV_API(cuStreamCreate(&other, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&cap, CU_STREAM_NON_BLOCKING));
  CUgraphExec a = sized_graph(one, 1, 1);
  if (a == NULL) { printf("  FAIL: could not build the graph\n"); return 1; }
  capture_loop_t t;
  memset(&t, 0, sizeof(t));
  t.ctx = ctx;
  t.stream = cap;
  pthread_t th;
  pthread_create(&th, NULL, capture_over_and_over, &t);
  int launches = 0;
  for (; launches < 4000 && t.captures < 2000; launches++) {
    /* No synchronize here: it is among what the other thread's global capture forbids. */
    cuGraphLaunch(a, launches & 1 ? other : g_stream);
  }
  t.stop = 1;
  pthread_join(th, NULL);
  cuCtxSynchronize();
  printf("  %d launches on two streams, %d captures alongside, %d broken\n", launches, t.captures, t.broken);
  if (t.broken != 0) {
    printf("  FAIL: captures were broken by launches on other streams\n");
    failed = 1;
  }
  cuGraphExecDestroy(a);
  cuStreamDestroy(other);
  cuStreamDestroy(cap);
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [T] */
static double now_s(void) {
  struct timespec t;
  clock_gettime(CLOCK_MONOTONIC, &t);
  return (double)t.tv_sec + (double)t.tv_nsec * 1e-9;
}

typedef struct {
  CUcontext ctx;
  CUstream stream;
  CUgraphExec exec;
  volatile int done;
  CUresult r;
} launcher_t;

static void *launch_in_thread(void *arg) {
  launcher_t *l = (launcher_t *)arg;
  cuCtxSetCurrent(l->ctx);
  l->r = cuGraphLaunch(l->exec, l->stream);
  l->done = 1;
  return NULL;
}

/* Starts a global capture on another thread and waits up to limit_s for it to begin. */
static double begin_capture_elsewhere(capture_thread_t *t, pthread_t *th, CUcontext ctx,
                                      CUstream cap, double limit_s) {
  memset(t, 0, sizeof(*t));
  t->ctx = ctx;
  t->stream = cap;
  double t0 = now_s();
  pthread_create(th, NULL, capture_in_global_mode, t);
  while (t->phase != 1 && now_s() - t0 < limit_s) usleep(1000);
  return now_s() - t0;
}

/* T1 makes the first launch after an idle gap on a parked stream, and waits for it (the
 * SM limit's measurement); T2 then begins a global capture; T3 launches a graph with
 * memory and only then lets the stream go. Waiting with the capture gate held, T2 waited
 * for the stream, and T3 behind T2 (writers first): the three stuck until the host
 * function gave up. Now none waits on another, and the capture survives. */
static int gap_wait_holds_nothing(CUcontext ctx) {
  const char *lim = getenv("CUDA_CORE_LIMIT");
  if (lim == NULL || atoi(lim) <= 0 || atoi(lim) >= 100) {
    printf("  SKIP (needs CUDA_CORE_LIMIT under 100: the gap path is not taken otherwise)\n");
    return -1;
  }
  trim_pool();
  size_t one[1] = {headroom() / 8};
  int failed = 0;
  CUstream other = NULL, cap = NULL;
  CHECK_DRV_API(cuStreamCreate(&other, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&cap, CU_STREAM_NON_BLOCKING));
  CUgraphExec a = sized_graph(one, 1, 1);
  CUgraph eg = NULL;
  CUgraphNode en = NULL;
  CUgraphExec empty = NULL;
  if (a == NULL || cuGraphCreate(&eg, 0) != CUDA_SUCCESS ||
      cuGraphAddEmptyNode(&en, eg, NULL, 0) != CUDA_SUCCESS ||
      cuGraphInstantiateWithFlags(&empty, eg, 0) != CUDA_SUCCESS) {
    printf("  FAIL: could not build the graphs\n");
    return 1;
  }
  launch_and_wait(a);
  cuGraphLaunch(a, other);
  cuStreamSynchronize(other);

  hold(g_stream);
  usleep(300 * 1000);   /* past the 200 ms gap threshold */
  launcher_t t1 = {ctx, g_stream, empty, 0, CUDA_SUCCESS};
  pthread_t th1, th2;
  pthread_create(&th1, NULL, launch_in_thread, &t1);
  usleep(200 * 1000);   /* T1 waits for its parked stream */
  capture_thread_t t2;
  double began = begin_capture_elsewhere(&t2, &th2, ctx, cap, 5.0);
  double t0 = now_s();
  CUresult r3 = cuGraphLaunch(a, other);
  double took = now_s() - t0;
  g_release = 1;   /* not release(): no synchronize while T2 captures */
  for (double w = now_s(); t2.phase != 1 && now_s() - w < 40.0;) usleep(1000);
  t2.phase = 2;
  pthread_join(th2, NULL);
  pthread_join(th1, NULL);
  cuCtxSynchronize();
  printf("  capture began after %.3f s, the launch behind it took %.3f s\n", began, took);
  describe("cuGraphLaunch (T1, after the idle gap)", t1.r);
  describe("cuGraphLaunch (T3, during the wait)", r3);
  describe("cuStreamEndCapture (T2)", t2.end);
  if (began > 2.0 || took > 2.0) {
    printf("  FAIL: the capture or the launch waited for T1's stream\n");
    failed++;
  }
  if (t1.r != CUDA_SUCCESS || r3 != CUDA_SUCCESS || t2.alloc != CUDA_SUCCESS || t2.end != CUDA_SUCCESS) {
    printf("  FAIL: a launch failed or the capture was broken\n");
    failed++;
  }
  cuGraphExecDestroy(empty);
  cuGraphDestroy(eg);
  cuGraphExecDestroy(a);
  cuStreamDestroy(other);
  cuStreamDestroy(cap);
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [U] */
typedef struct {
  CUcontext ctx;
  CUstream stream;
  CUdeviceptr buf;
  volatile int filled, stop;
} filler_t;

static void *fill_stream(void *arg) {
  filler_t *f = (filler_t *)arg;
  cuCtxSetCurrent(f->ctx);
  while (!f->stop && cuMemsetD8Async(f->buf, 1, 4096, f->stream) == CUDA_SUCCESS) f->filled++;
  return NULL;
}

/* In a context of its own, so that no event of an earlier case is there to reuse:
 * F fills a parked stream until the driver blocks it (about 1000 operations, driver 595);
 * T1 then launches a graph with memory on a new stream, whose event has to be made
 * (cuEventCreate waits while F is blocked); T2 begins a global capture (so does
 * cuStreamBeginCapture); T3 launches a graph with memory on a third, parked stream and
 * then lets the streams go. T1 made the event with g_graph_use_lock held, and T2 began
 * with the capture gate held for writing: either kept T3 out until the host function gave
 * up. */
static int full_stream_holds_nothing(void) {
  CUcontext ctx2 = NULL, prev = NULL;
  int failed = 0;
  cuCtxGetCurrent(&prev);
  CHECK_DRV_API(CUCTX_CREATE(&ctx2, 0, g_dev));
  trim_pool();
  size_t one[1] = {headroom() / 8};
  CUstream s1 = NULL, s3 = NULL, s4 = NULL, cap = NULL;
  CHECK_DRV_API(cuStreamCreate(&s1, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&s3, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&s4, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&cap, CU_STREAM_NON_BLOCKING));
  CUdeviceptr buf = 0;
  CHECK_DRV_API(cuMemAlloc(&buf, 4096));
  CUgraphExec a1 = sized_graph(one, 1, 1), a3 = sized_graph(one, 1, 1);
  if (a1 == NULL || a3 == NULL) { printf("  FAIL: could not build the graphs\n"); return 1; }

  /* T3's stream parked with a launch behind it, its record busy so T1 cannot take it. */
  hold(s1);
  if (cuLaunchHostFunc(s3, hold_stream, NULL) != CUDA_SUCCESS) g_release = 1;
  CUresult warm = cuGraphLaunch(a3, s3);
  filler_t f = {ctx2, s1, buf, 0, 0};
  pthread_t thf, th1, th2;
  pthread_create(&thf, NULL, fill_stream, &f);
  for (int last = -1, i = 0; i < 100 && f.filled != last; i++) { last = f.filled; usleep(100 * 1000); }
  printf("  %d operations queued on the parked stream before the driver blocked the next\n", f.filled);

  launcher_t t1 = {ctx2, s4, a1, 0, CUDA_SUCCESS};
  pthread_create(&th1, NULL, launch_in_thread, &t1);
  usleep(300 * 1000);
  capture_thread_t t2;
  double began = begin_capture_elsewhere(&t2, &th2, ctx2, cap, 0.3);
  double t0 = now_s();
  CUresult r3 = cuGraphLaunch(a3, s3);
  double took = now_s() - t0;
  int t1_waiting = !t1.done;
  g_release = 1;
  f.stop = 1;
  for (double w = now_s(); t2.phase != 1 && now_s() - w < 40.0;) usleep(1000);
  t2.phase = 2;
  pthread_join(th2, NULL);
  pthread_join(th1, NULL);
  pthread_join(thf, NULL);
  cuCtxSynchronize();
  printf("  T1 %s, capture %s, T3's launch took %.3f s\n",
         t1_waiting ? "was still in its launch" : "had returned",
         began < 0.3 ? "began at once" : "waited for the stream (the driver)", took);
  describe("cuGraphLaunch (T1, new stream)", t1.r);
  describe("cuGraphLaunch (T3, while the stream is full)", r3);
  describe("cuStreamEndCapture (T2)", t2.end);
  if (took > 2.0) {
    printf("  FAIL: T3's launch waited for the full stream\n");
    failed++;
  }
  if (warm != CUDA_SUCCESS || t1.r != CUDA_SUCCESS || r3 != CUDA_SUCCESS ||
      t2.alloc != CUDA_SUCCESS || t2.end != CUDA_SUCCESS) {
    printf("  FAIL: a launch failed or the capture was broken\n");
    failed++;
  }
  cuGraphExecDestroy(a1);
  cuGraphExecDestroy(a3);
  cuMemFree(buf);
  cuStreamDestroy(s1);
  cuStreamDestroy(s3);
  cuStreamDestroy(s4);
  cuStreamDestroy(cap);
  trim_pool();
  cuCtxSetCurrent(prev);
  cuCtxDestroy(ctx2);
  return failed;
}

/* --------------------------------------------------------------------- [V] */
/* As in [U], F blocks enqueuing on a full stream of a context of its own; no capture
 * begins. T1 launches a graph on a new stream: the driver maps its memory and then holds
 * the call (or the library's cuEventCreate does), so T1 is still in its launch. T3, on
 * another stream, launches a graph as large: it cannot reuse T1's memory, and what is
 * left of the headroom does not hold it -- refused, at once. Claimed only after the
 * driver returned, T1's record was not there yet: T3 went through and the pool grew past
 * the limit (docs 6.17). */
static int launch_counts_from_admission(void) {
  CUcontext ctx3 = NULL, prev = NULL;
  int failed = 0;
  cuCtxGetCurrent(&prev);
  CHECK_DRV_API(CUCTX_CREATE(&ctx3, 0, g_dev));
  trim_pool();
  /* The headroom taken down to 1.5 graphs: T1's fits, T3's only by reusing T1's. */
  size_t room = headroom(), x = room / 8, tiny[1] = {2u << 20}, big[1] = {x};
  CUdeviceptr hog = 0, buf = 0;
  CHECK_DRV_API(cuMemAlloc(&hog, room - x * 3 / 2 - (64u << 20)));
  CHECK_DRV_API(cuMemAlloc(&buf, 4096));
  CUstream s1 = NULL, s3 = NULL, s4 = NULL;
  CHECK_DRV_API(cuStreamCreate(&s1, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&s3, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&s4, CU_STREAM_NON_BLOCKING));
  CUgraphExec warm = sized_graph(tiny, 1, 1), a1 = sized_graph(big, 1, 1), a3 = sized_graph(big, 1, 1);
  if (warm == NULL || a1 == NULL || a3 == NULL) { printf("  FAIL: could not build the graphs\n"); return 1; }

  hold(s1);
  if (cuLaunchHostFunc(s3, hold_stream, NULL) != CUDA_SUCCESS) g_release = 1;
  CUresult rw = cuGraphLaunch(warm, s3);   /* s3's record busy: T1 cannot take its event */
  size_t before = pool_reserved();
  filler_t f = {ctx3, s1, buf, 0, 0};
  pthread_t thf, th1;
  pthread_create(&thf, NULL, fill_stream, &f);
  for (int last = -1, i = 0; i < 100 && f.filled != last; i++) { last = f.filled; usleep(100 * 1000); }
  launcher_t t1 = {ctx3, s4, a1, 0, CUDA_SUCCESS};
  pthread_create(&th1, NULL, launch_in_thread, &t1);
  for (int i = 0; i < 2000 && pool_reserved() < before + x; i++) usleep(1000);
  usleep(100 * 1000);
  int mapped = pool_reserved() >= before + x;
  int t1_waiting = !t1.done;
  double t0 = now_s();
  CUresult r3 = cuGraphLaunch(a3, s3);
  double took = now_s() - t0;
  g_release = 1;
  f.stop = 1;
  pthread_join(th1, NULL);
  pthread_join(thf, NULL);
  cuCtxSynchronize();
  printf("  pool %.0f MiB, T1 %s, T3's launch took %.3f s\n", MIB(pool_reserved()),
         !mapped ? "did not map" : t1_waiting ? "was still in its launch" : "had returned", took);
  describe("cuGraphLaunch (T1, new stream)", t1.r);
  describe("cuGraphLaunch (T3, another stream, as large)", r3);
  if (rw != CUDA_SUCCESS || !mapped || t1.r != CUDA_SUCCESS) {
    printf("  FAIL: T1's graph did not go through as set up\n");
    failed++;
  } else if (!t1_waiting) {
    printf("  (T1 was not held up on this driver: its graph done, nothing to check)\n");
  } else if (took > 2.0 || r3 != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: expected T3 refused at once, T1 counted as running from its admission\n");
    failed++;
  }
  cuGraphExecDestroy(warm);
  cuGraphExecDestroy(a1);
  cuGraphExecDestroy(a3);
  cuMemFree(hog);
  cuMemFree(buf);
  cuStreamDestroy(s1);
  cuStreamDestroy(s3);
  cuStreamDestroy(s4);
  trim_pool();
  cuCtxSetCurrent(prev);
  cuCtxDestroy(ctx3);
  return failed;
}

/* A launch on g_stream, waited for: the SM limit's idle-gap path (CUDA_CORE_LIMIT under
 * 100) makes the first launch after 200 ms idle wait for its stream to finish, which a
 * parked stream never does. Launching onto one right after this keeps clear of it. */
static void keep_busy(void) {
  static CUgraphExec empty;
  if (empty == NULL) {
    CUgraph g = NULL;
    CUgraphNode n = NULL;
    if (cuGraphCreate(&g, 0) != CUDA_SUCCESS) return;
    if (cuGraphAddEmptyNode(&n, g, NULL, 0) == CUDA_SUCCESS) cuGraphInstantiateWithFlags(&empty, g, 0);
    cuGraphDestroy(g);
  }
  if (empty != NULL) launch_and_wait(empty);
}

/* --------------------------------------------------------------------- [W] */
typedef struct {
  CUcontext ctx;
  CUgraphExec ex;
  volatile int phase;   /* 1 launched, 2 launch again, 3 again done, 4 may end */
  CUresult r1, r2;
} ptds_thread_t;

static void *launch_on_own_ptds(void *arg) {
  ptds_thread_t *p = (ptds_thread_t *)arg;
  cuCtxSetCurrent(p->ctx);
  if (cuLaunchHostFunc(CU_STREAM_PER_THREAD, hold_stream, NULL) != CUDA_SUCCESS) g_release = 1;
  p->r1 = cuGraphLaunch(p->ex, CU_STREAM_PER_THREAD);
  p->phase = 1;
  for (int i = 0; p->phase != 2 && i < 30000; i++) usleep(1000);
  p->r2 = cuGraphLaunch(p->ex, CU_STREAM_PER_THREAD);
  p->phase = 3;
  for (int i = 0; p->phase != 4 && i < 30000; i++) usleep(1000);
  cuStreamSynchronize(CU_STREAM_PER_THREAD);
  return NULL;
}

/* T's per-thread stream is parked with a graph launched on it; this thread's per-thread
 * stream is another stream. A graph as large launched there cannot reuse T's memory, and
 * the headroom left holds half of it: refused. T launching its own again reuses it. The
 * per-thread stream was not tracked before: one handle for as many streams as threads. */
static int per_thread_stream_counts(CUcontext ctx) {
  trim_pool();
  size_t x = headroom() / 4, one[1] = {x};
  int failed = 0;
  CUgraphExec a = sized_graph(one, 1, 1), b = sized_graph(one, 1, 1);
  if (a == NULL || b == NULL) { printf("  FAIL: could not build the graphs\n"); return 1; }
  g_release = 0;
  ptds_thread_t t;
  memset(&t, 0, sizeof(t));
  t.ctx = ctx;
  t.ex = a;
  pthread_t th;
  keep_busy();
  pthread_create(&th, NULL, launch_on_own_ptds, &t);
  for (int i = 0; t.phase != 1 && i < 20000; i++) usleep(1000);
  CUdeviceptr filler = fill_leaving(x / 2);
  CUresult rb = cuGraphLaunch(b, CU_STREAM_PER_THREAD);
  t.phase = 2;
  for (int i = 0; t.phase != 3 && i < 20000; i++) usleep(1000);
  g_release = 1;
  t.phase = 4;
  pthread_join(th, NULL);
  cuCtxSynchronize();
  if (filler) cuMemFree(filler);
  describe("cuGraphLaunch (T, its per-thread stream parked)", t.r1);
  describe("cuGraphLaunch (this thread's per-thread stream)", rb);
  describe("cuGraphLaunch (T again, its own stream)", t.r2);
  if (t.r1 != CUDA_SUCCESS || rb != CUDA_ERROR_OUT_OF_MEMORY || t.r2 != CUDA_SUCCESS) {
    printf("  FAIL: expected T's through, this thread's refused, T's again through\n");
    failed++;
  }
  cuGraphExecDestroy(a);
  cuGraphExecDestroy(b);
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [X] */
/* S1 runs a large graph and is synchronized; then, parked, a small one. A stream keeps
 * counting the largest graph it ran while busy (the driver reuses its blocks only on that
 * stream), until something shows it finished: the synchronize does. So a graph on S2 can
 * reuse all but the small one, and goes through with the headroom taken. */
static int sync_settles(int whole_ctx) {
  trim_pool();
  size_t room = headroom(), big[1] = {room / 2}, small[1] = {room / 8}, mid[1] = {room / 4};
  int failed = 0;
  CUstream s1 = NULL, s2 = NULL;
  CHECK_DRV_API(cuStreamCreate(&s1, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&s2, CU_STREAM_NON_BLOCKING));
  CUgraphExec gb = sized_graph(big, 1, 1), gs = sized_graph(small, 1, 1), gm = sized_graph(mid, 1, 1);
  if (gb == NULL || gs == NULL || gm == NULL) { printf("  FAIL: could not build the graphs\n"); return 1; }
  keep_busy();
  hold(s1);
  CUresult r1 = cuGraphLaunch(gb, s1);
  g_release = 1;
  CUresult rs = whole_ctx ? cuCtxSynchronize() : cuStreamSynchronize(s1);
  hold(s1);
  CUresult r2 = cuGraphLaunch(gs, s1);
  CUdeviceptr filler = fill_leaving(mid[0] / 2);
  CUresult r3 = cuGraphLaunch(gm, s2);
  g_release = 1;
  cuStreamSynchronize(s1);
  cuStreamSynchronize(s2);
  if (filler) cuMemFree(filler);
  describe(whole_ctx ? "cuCtxSynchronize (after the large graph)" : "cuStreamSynchronize (after the large graph)", rs);
  describe("cuGraphLaunch (S2, S1 busy with the small one)", r3);
  if (r1 != CUDA_SUCCESS || rs != CUDA_SUCCESS || r2 != CUDA_SUCCESS || r3 != CUDA_SUCCESS) {
    printf("  FAIL: expected all through, S1 counting only its small graph\n");
    failed++;
  }
  cuGraphExecDestroy(gb);
  cuGraphExecDestroy(gs);
  cuGraphExecDestroy(gm);
  cuStreamDestroy(s1);
  cuStreamDestroy(s2);
  trim_pool();
  return failed;
}

static int synchronize_settles(void) {
  int failed = 0;
  printf("  (stream)\n");
  failed += sync_settles(0);
  printf("  (context)\n");
  failed += sync_settles(1);
  return failed;
}

/* --------------------------------------------------------------------- [Y] */
#define MANY_STREAMS 70
/* 70 parked streams, each with a graph of its own launched on it (execs differ: a graph
 * relaunched is not counted against itself). With the headroom then taken, a 71st on a
 * stream of its own cannot reuse any of theirs: refused. A table of 32 records dropped
 * the oldest, and lent their memory out. 70 is past the 64 records kept before an idle
 * one is recycled, too: running ones never are. */
static int many_streams_count(void) {
  trim_pool();
  size_t one[1] = {32u << 20};
  int failed = 0, made = 0;
  CUstream st[MANY_STREAMS + 1];
  CUgraphExec ex[MANY_STREAMS + 1];
  memset(st, 0, sizeof(st));
  memset(ex, 0, sizeof(ex));
  for (int i = 0; i <= MANY_STREAMS; i++) {
    if (cuStreamCreate(&st[i], CU_STREAM_NON_BLOCKING) != CUDA_SUCCESS || (ex[i] = sized_graph(one, 1, 1)) == NULL) break;
    made++;
  }
  if (made <= MANY_STREAMS || headroom() < (MANY_STREAMS + 8) * one[0]) {
    printf("  FAIL: could not set up %d streams and graphs\n", MANY_STREAMS + 1);
    failed = 1;
  } else {
    keep_busy();
    g_release = 0;
    int through = 0;
    for (int i = 0; i < MANY_STREAMS; i++) {
      if (cuLaunchHostFunc(st[i], hold_stream, NULL) != CUDA_SUCCESS) g_release = 1;
      through += cuGraphLaunch(ex[i], st[i]) == CUDA_SUCCESS;
    }
    CUdeviceptr filler = fill_leaving(one[0] / 2);
    CUresult r = cuGraphLaunch(ex[MANY_STREAMS], st[MANY_STREAMS]);
    g_release = 1;
    cuCtxSynchronize();
    if (filler) cuMemFree(filler);
    printf("  %d of %d launched on parked streams\n", through, MANY_STREAMS);
    describe("cuGraphLaunch (one stream more, the headroom taken)", r);
    if (through != MANY_STREAMS || r != CUDA_ERROR_OUT_OF_MEMORY) {
      printf("  FAIL: expected all of them through and the last refused\n");
      failed = 1;
    }
  }
  for (int i = 0; i <= MANY_STREAMS; i++) {
    if (ex[i] != NULL) cuGraphExecDestroy(ex[i]);
    if (st[i] != NULL) cuStreamDestroy(st[i]);
  }
  trim_pool();
  return failed;
}

/* --------------------------------------------------------------------- [Z] */
/* A graph allocating sizes[i] on devs[i], each freed after. */
static CUgraphExec devices_graph(const CUdevice *devs, const size_t *sizes, int n) {
  CUgraph g = NULL;
  CUgraphNode prev = NULL;
  CUgraphExec ex = NULL;
  if (cuGraphCreate(&g, 0) != CUDA_SUCCESS) return NULL;
  for (int i = 0; i < n; i++) {
    CUDA_MEM_ALLOC_NODE_PARAMS ap;
    memset(&ap, 0, sizeof(ap));
    ap.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
    ap.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    ap.poolProps.location.id = devs[i];
    ap.bytesize = sizes[i];
    CUgraphNode an = NULL, fn = NULL;
    if (cuGraphAddMemAllocNode(&an, g, prev ? &prev : NULL, prev ? 1 : 0, &ap) != CUDA_SUCCESS ||
        cuGraphAddMemFreeNode(&fn, g, &an, 1, ap.dptr) != CUDA_SUCCESS) {
      cuGraphDestroy(g);
      return NULL;
    }
    prev = fn;
  }
  if (cuGraphInstantiateWithFlags(&ex, g, 0) != CUDA_SUCCESS) ex = NULL;
  cuGraphDestroy(g);
  return ex;
}

/* With another context current, a graph launched on this context's parked stream S1: its
 * event has to be of S1's context. Then, this context current again, a graph as large on
 * S2 with the headroom taken: refused. Made in the current context, the event did not take
 * on S1, and the launch went uncounted. On two devices, a graph allocating a little on the
 * first and much on the second, parked; then one as large on the second, on another
 * stream, with that device's headroom taken: refused. Only the first device was counted.
 * Contexts are made and headroom taken before parking: either takes long enough to make
 * the next launch one after an idle gap (see keep_busy). */
static int other_context_and_device_count(CUcontext ctx) {
  int failed = 0;
  trim_pool();
  size_t x = headroom() / 4, one[1] = {x};
  CUstream s1 = NULL, s2 = NULL;
  CHECK_DRV_API(cuStreamCreate(&s1, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&s2, CU_STREAM_NON_BLOCKING));
  CUgraphExec a = sized_graph(one, 1, 1), b = sized_graph(one, 1, 1);
  if (a == NULL || b == NULL) { printf("  FAIL: could not build the graphs\n"); return 1; }
  CUcontext other = NULL;
  CHECK_DRV_API(CUCTX_CREATE(&other, 0, g_dev));
  cuCtxSetCurrent(ctx);
  CUdeviceptr filler = fill_leaving(x * 3 / 2);   /* a's x, then half of b's */
  keep_busy();
  hold(s1);
  cuCtxSetCurrent(other);
  double t0 = now_s();
  CUresult ra = cuGraphLaunch(a, s1);
  double took = now_s() - t0;
  cuCtxSetCurrent(ctx);
  CUresult rb = cuGraphLaunch(b, s2);
  g_release = 1;
  cuCtxSynchronize();
  if (filler) cuMemFree(filler);
  describe("cuGraphLaunch (S1 of this context, another current)", ra);
  describe("cuGraphLaunch (S2, this context current, headroom taken)", rb);
  if (ra != CUDA_SUCCESS) {
    printf("  (the driver refuses a launch on another context's stream: nothing to check)\n");
  } else if (took > 2.0) {
    printf("  FAIL: the launch on the parked stream waited %.1f s: not set up as meant\n", took);
    failed++;
  } else if (rb != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: expected it refused, the graph on S1 counted\n");
    failed++;
  }
  cuCtxDestroy(other);
  cuCtxSetCurrent(ctx);
  cuGraphExecDestroy(a);
  cuGraphExecDestroy(b);
  trim_pool();

  int count = 0;
  cuDeviceGetCount(&count);
  if (count < 2) {
    printf("  (second device: SKIP, needs 2 devices, have %d)\n", count);
  } else {
    CUdevice dev1;
    CHECK_DRV_API(cuDeviceGet(&dev1, g_dev == 0 ? 1 : 0));
    CUcontext octx = NULL, popped = NULL;
    CHECK_DRV_API(cuDevicePrimaryCtxRetain(&octx, dev1));
    cuDeviceGraphMemTrim(dev1);
    CHECK_DRV_API(cuCtxPushCurrent(octx));
    size_t y = headroom() / 4;
    CUdeviceptr filler1 = fill_leaving(y * 3 / 2);   /* g2's y, then half of g3's */
    CHECK_DRV_API(cuCtxPopCurrent(&popped));
    CUdevice both[2] = {g_dev, dev1}, second[1] = {dev1};
    size_t both_sizes[2] = {32u << 20, y}, second_sizes[1] = {y};
    CUgraphExec g2 = devices_graph(both, both_sizes, 2), g3 = devices_graph(second, second_sizes, 1);
    if (g2 == NULL || g3 == NULL) {
      printf("  FAIL: could not build the two-device graphs\n");
      failed++;
    } else {
      keep_busy();
      hold(s1);
      t0 = now_s();
      CUresult r2 = cuGraphLaunch(g2, s1);
      took = now_s() - t0;
      CUresult r3 = cuGraphLaunch(g3, s2);
      g_release = 1;
      cuCtxSynchronize();
      describe("cuGraphLaunch (S1: a little on this device, much on the other)", r2);
      describe("cuGraphLaunch (S2: as much on the other, its headroom taken)", r3);
      if (took > 2.0) {
        printf("  FAIL: the launch on the parked stream waited %.1f s: not set up as meant\n", took);
        failed++;
      } else if (r2 != CUDA_SUCCESS || r3 != CUDA_ERROR_OUT_OF_MEMORY) {
        printf("  FAIL: expected the first through and the second refused\n");
        failed++;
      }
    }
    CHECK_DRV_API(cuCtxPushCurrent(octx));
    if (filler1) cuMemFree(filler1);
    CHECK_DRV_API(cuCtxPopCurrent(&popped));
    if (g2 != NULL) cuGraphExecDestroy(g2);
    if (g3 != NULL) cuGraphExecDestroy(g3);
    cuDeviceGraphMemTrim(dev1);
    cuDevicePrimaryCtxRelease(dev1);
  }
  cuStreamDestroy(s1);
  cuStreamDestroy(s2);
  trim_pool();
  return failed;
}

int main(void) {
  if (!preloaded()) {
    printf("SKIP (needs LD_PRELOAD=libvgpu-control.so and a memory limit)\n");
    return VGPU_TEST_RC_SKIP;
  }
  CHECK_DRV_API(cuInit(0));
  CHECK_DRV_API(cuDeviceGet(&g_dev, TEST_DEVICE_ID));
  CUcontext ctx;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, g_dev));
  CHECK_DRV_API(cuStreamCreate(&g_stream, CU_STREAM_NON_BLOCKING));

  int failures = 0, rc;
#define RUN_CASE(title, call) do { printf(title "\n"); rc = (call); failures += rc > 0 ? rc : 0; } while (0)
  RUN_CASE("[A] launch refused while the headroom is taken, accepted once freed", launch_admission());
  RUN_CASE("[B] a replay of an already-mapped graph is never refused", steady_replay_not_refused());
  RUN_CASE("[C] cuGraphUpload refused / accepted", upload_admission());
  RUN_CASE("[D] instantiate with the UPLOAD flag refused / accepted", instantiate_upload_admission());
  RUN_CASE("[E] a hand-built alloc-node graph's launch refused / accepted", hand_built_admission());
  RUN_CASE("[F] only the pool's growth is checked", growth_only());
  RUN_CASE("[G] a trim makes the next launch checked again", trim_rechecks());
  RUN_CASE("[H] a graph allocating on two devices is checked on each", multi_device_checked());
  RUN_CASE("[I] graphs past what a fixed table of 256 held are still checked", many_live_graphs());
  RUN_CASE("[J] graphs stay checked in a process cycling contexts", context_teardown_frees_records(ctx));
  RUN_CASE("[K] an update under another context leaves the exec's record", update_keeps_owner());
  static const struct { CUstreamCaptureMode mode; const char *name; } modes[] = {
    {CU_STREAM_CAPTURE_MODE_GLOBAL, "global"},
    {CU_STREAM_CAPTURE_MODE_THREAD_LOCAL, "thread-local"},
    {CU_STREAM_CAPTURE_MODE_RELAXED, "relaxed"},
  };
  for (int m = 0; m < 3; m++) {
    g_mode = modes[m].mode;
    printf("(capture mode %s)\n", modes[m].name);
    RUN_CASE("[L] an idle pool counts for what is captured next", idle_pool_counts());
    RUN_CASE("[N] what one capture already charged counts as held by the pool", capture_charges_in_pool());
  }
  g_mode = CU_STREAM_CAPTURE_MODE_RELAXED;
  RUN_CASE("[M] temporaries reused inside a graph are not checked as their sum", temporaries_are_reused());
  /* [P] first of the cases using a second stream: it starts with the device's first. */
  RUN_CASE("[P] the pool memory of a graph running on another stream is not lent out", running_graph_counts());
  RUN_CASE("[O] blocks a graph leaves allocated are not lent out again", held_blocks_count());
  RUN_CASE("[Q] parallel branches do not reuse each other's blocks", parallel_branches_count());
  RUN_CASE("[R] an open capture is left alone", captures_left_alone(ctx));
  RUN_CASE("[S] captures begun while launches go on are left alone", captures_begun_during_launches(ctx));
  RUN_CASE("[T] an idle-gap launch waiting for its stream holds nothing off", gap_wait_holds_nothing(ctx));
  RUN_CASE("[U] a full stream elsewhere holds no launch off", full_stream_holds_nothing());
  RUN_CASE("[V] a launch counts as running from its admission on", launch_counts_from_admission());
  RUN_CASE("[W] the per-thread default stream is a stream of its own per thread", per_thread_stream_counts(ctx));
  RUN_CASE("[X] a synchronize settles what it waited for", synchronize_settles());
  RUN_CASE("[Y] graphs running on more than 32 streams are all counted", many_streams_count());
  RUN_CASE("[Z] another context's stream, and a graph's second device, are counted", other_context_and_device_count(ctx));
#undef RUN_CASE
  trim_pool();
  cuStreamDestroy(g_stream);
  cuCtxDestroy(ctx);
  int verdict = vgpu_test_verdict(failures, 0);
  return verdict;
}
