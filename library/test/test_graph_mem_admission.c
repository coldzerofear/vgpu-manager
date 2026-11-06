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
 *
 * Each case trims the graph pool first so it starts with nothing reserved.
 * Built twice (_ptsz variant) so the per-thread-default-stream hooks are
 * covered too.
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so CUDA_MEM_LIMIT=4096m ./test_graph_mem_admission
 */
#include <cuda.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "test_utils.h"

#define MIB(x) ((double)(x) / (1024.0 * 1024.0))

static CUdevice g_dev;
static CUstream g_stream;

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
  if (cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_RELAXED) != CUDA_SUCCESS) return NULL;
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
  CHECK_DRV_API(cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_RELAXED));
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
  CHECK_DRV_API(cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_RELAXED));
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
  CHECK_DRV_API(cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_RELAXED));
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
  RUN_CASE("[L] an idle pool counts for what is captured next", idle_pool_counts());
  RUN_CASE("[M] temporaries reused inside a graph are not checked as their sum", temporaries_are_reused());
  RUN_CASE("[N] what one capture already charged counts as held by the pool", capture_charges_in_pool());
#undef RUN_CASE
  trim_pool();
  cuStreamDestroy(g_stream);
  cuCtxDestroy(ctx);
  int verdict = vgpu_test_verdict(failures, 0);
  return verdict;
}
