/*
 * Async / graph-capture memory accounting regression test.
 *
 * The limit check reads NVML plus our own virtual-memory ledger, because an
 * allocation the driver has not physically made yet is invisible to NVML. Two
 * kinds of allocation are in that state, and each is covered by a charge the
 * hook adds and later retires:
 *
 *   in flight  cuMemAllocAsync is stream-ordered, so between the driver call
 *              and the synchronize that follows it, the memory is promised but
 *              not committed (MEMORY_TYPE_ASYNC_BRIDGE).
 *   captured   an allocation made during graph capture cannot be synchronized
 *              at all -- that would invalidate the capture -- so it stays
 *              charged for the length of the capture (MEMORY_TYPE_CAPTURE).
 *
 * These cases OBSERVE THE LEDGER DIRECTLY rather than inferring it from an
 * out-of-memory result, which is what an earlier version of this file did and
 * why it was worthless: an over-large request is refused by the driver too, so
 * "the allocation was refused" says nothing about whether our accounting ran.
 * Reading the charge itself cannot be faked by driver behaviour.
 *
 * What each case fails on -- worth re-checking by hand (comment the line out,
 * rebuild, expect red) whenever this file or the hooks are touched:
 *
 *   [A] the bridge charge in the non-capture branch of cuMemAllocAsync /
 *       cuMemAllocFromPoolAsync. The charge only exists between the driver call
 *       and the synchronize, so it is invisible to the thread doing the
 *       allocating -- it exists for OTHER allocators. This case parks the
 *       stream so the allocation cannot complete, and has a second thread read
 *       the ledger while the allocating thread is still inside the hook.
 *
 *   [B] the capture charge (malloc_gpu_virt_memory_captured).
 *
 *   [C] free_gpu_virt_memory_by_graph() in the cuStreamEndCapture hook, which
 *       retires that charge. Holding it double-counts against NVML once the
 *       graph launches and leaks when the graph rather than the application
 *       owns the pointer.
 *
 *   [D] that the capture charge actually feeds the limit, rather than merely
 *       being written down: while a capture holds a charge, a plain allocation
 *       that would have fitted before must now be refused.
 *
 *   [E] cuGraphAddMemAllocNode on a hand-built graph: checked against the
 *       limit (an oversized node is refused) but not charged -- nothing would
 *       retire a charge on a graph built by hand.
 *
 *   [F] an alloc node added to the graph a capture is building: checked like
 *       [E] and not charged either -- graph allocations are checked as a
 *       whole when the graph maps them (test_graph_mem_admission).
 *
 *   [G] an invalidated capture: the driver stops reporting its graph and ends
 *       it with an error and no graph, so its charges are only retired if the
 *       graph was recorded when the capture began (capture_reg_begin) and the
 *       end-of-capture hook discharges on CUDA_ERROR_STREAM_CAPTURE_INVALIDATED.
 *
 *   [Q] [R] [S] a capture ended where the driver refuses it, from another thread,
 *       on a joined stream, unjoined: released once the capture is over.
 *
 *   [T] an async allocation is checked on the device of its stream, not of the
 *       current context, also in a capture (needs two devices).
 *
 *   [H] [I] [J] a capture left open when its context goes -- cuCtxDestroy, a
 *       primary-context reset, the release of its last reference. The driver
 *       destroys the graphs with no cuGraphDestroy; the teardown hooks retire
 *       the charges. [I]/[J] fail if the primary context is looked up after
 *       the call: it reads as inactive by then.
 *
 * Honest limitation: none of these prove the cuGraphDestroy discharge does any
 * work, because cuStreamEndCapture has already retired the charge by then.
 *
 * Built twice: the _ptsz variant compiles this same source with
 * CUDA_API_PER_THREAD_DEFAULT_STREAM so the _ptsz copies of the hooks -- which
 * are separate code -- get the same coverage. Without it a defect introduced in
 * cuMemAllocAsync_ptsz passes every test in the suite.
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so CUDA_MEM_LIMIT=4096m \
 *     VMEMORY_NODE_ENABLED=1 ./test_alloc_async_accounting
 */
#include <cuda.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/mman.h>
#include <sys/stat.h>

#include "test_utils.h"

#include "test_ledger.h"

/* ------------------------------------------------------------------ helpers */

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

#define MIB(x) ((double)(x) / (1024.0 * 1024.0))

/* Under LD_PRELOAD cuMemGetInfo reports the limit-relative figure, so `free` is
 * the headroom the hooks will actually enforce. */
static CUresult headroom(size_t *out) {
  size_t freeb = 0, total = 0;
  CHECK_DRV_API(cuMemGetInfo(&freeb, &total));
  printf("  headroom: free=%.0f MiB total=%.0f MiB\n", MIB(freeb), MIB(total));
  *out = freeb;
  return CUDA_SUCCESS;
}

/* Common preconditions. Returns 0 to run, -1 to skip, or 1 to FAIL, matching
 * the case-function convention the runner counts (rc < 0 skip, rc > 0 fail).
 *
 * The distinction between skip and fail is the whole point here. "The ledger is
 * absent" is a configuration, and skipping is honest. "The ledger is present but
 * this test cannot read it" is a defect, and skipping would hide it -- which is
 * precisely how the addition of the frozen header turned eight assertions into
 * a green-looking SKIP with a warning nobody had to act on. */
static int can_assert(void) {
  if (!preloaded()) {
    printf("  SKIP (needs LD_PRELOAD=libvgpu-control.so)\n");
    return -1;
  }
  if (g_ledger_broken) {
    printf("  FAIL (ledger at " LEDGER_PATH " exists but its layout does not match\n"
           "        this test's mirror -- update the mirror to track hook.h)\n");
    return 1;
  }
  if (g_ledger == NULL) {
    printf("  SKIP (no ledger at " LEDGER_PATH " -- needs VMEMORY_NODE_ENABLED=1)\n");
    return -1;
  }
  return 0;
}

/* --------------------------------------------------------------------- [A] --
 *
 * A host function parks the stream until another thread lets it go.
 *
 * This is what gives [A] its teeth. A stream-ordered allocation only becomes
 * physical -- and only then visible to NVML -- when the stream reaches it. On an
 * idle stream that happens within microseconds, so the bridge charge would come
 * and go faster than anything could observe. Parking the stream holds the
 * allocation in flight for as long as we like, which is exactly the window the
 * bridge exists to cover. */
typedef struct {
  pthread_mutex_t m;
  pthread_cond_t  cv;
  int       entered;      /* the stream reached the host function */
  int       release;      /* let it go */
  long long charged;      /* ledger reading taken while the alloc was in flight */
  long long baseline;     /* ledger reading taken before it started */
  long long want;         /* the allocation's size: the charge to wait for */
} stream_gate_t;

static void CUDA_CB gate_hold(void *user) {
  stream_gate_t *g = (stream_gate_t *)user;
  pthread_mutex_lock(&g->m);
  g->entered = 1;
  pthread_cond_broadcast(&g->cv);
  while (!g->release) pthread_cond_wait(&g->cv, &g->m);
  pthread_mutex_unlock(&g->m);
}

/* Wait for the stream to park, sample the ledger while the allocating thread is
 * still inside the hook, then release. Bounded waits only: a gate that never
 * arrives must let the case finish and be judged, not wedge the suite. */
static void *gate_observer(void *user) {
  stream_gate_t *g = (stream_gate_t *)user;
  for (int i = 0; i < 2000; i++) {          /* up to ~2s for the gate to arrive */
    pthread_mutex_lock(&g->m);
    int in = g->entered;
    pthread_mutex_unlock(&g->m);
    if (in) break;
    usleep(1000);
  }
  /* The stream reaching the gate says nothing about how far the allocating
   * thread has got: it may still be inside the hook ahead of the charge (the
   * NVML query, the device lock), and on a loaded machine that took longer than
   * a fixed 50 ms -- a flaky FAIL with the charge simply not made yet. The
   * allocation cannot complete while the gate holds, so poll, for up to 2 s,
   * until the charge shows: seen here, it was made while in flight. */
  for (int i = 0; i < 2000; i++) {
    g->charged = ledger_used_self();
    if (g->charged < 0 || g->charged - g->baseline >= g->want) break;
    usleep(1000);
  }

  pthread_mutex_lock(&g->m);
  g->release = 1;
  pthread_cond_broadcast(&g->cv);
  pthread_mutex_unlock(&g->m);
  return NULL;
}

/* [A] An allocation that has not landed yet must still be charged, so that a
 * concurrent allocator counts it. */
static int inflight_allocation_is_charged(CUstream stream) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }   /* -1 skip, 1 fail */

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  stream_gate_t gate;
  pthread_mutex_init(&gate.m, NULL);
  pthread_cond_init(&gate.cv, NULL);
  gate.entered = gate.release = 0;
  gate.charged = -1;
  gate.baseline = ledger_used_self();
  gate.want = (long long)chunk;

  CUresult g = cuLaunchHostFunc(stream, gate_hold, &gate);
  if (g != CUDA_SUCCESS) {
    describe("cuLaunchHostFunc (park the stream)", g);
    printf("  SKIP (cannot hold the stream, so nothing would be proven)\n");
    return -1;
  }

  pthread_t observer;
  if (pthread_create(&observer, NULL, gate_observer, &gate) != 0) {
    printf("  FAIL: could not start the observer thread\n");
    pthread_mutex_lock(&gate.m);
    gate.release = 1;
    pthread_cond_broadcast(&gate.cv);
    pthread_mutex_unlock(&gate.m);
    cuStreamSynchronize(stream);
    return 1;
  }

  /* Queued behind the parked host function, so it cannot complete until the
   * observer has had its look. */
  CUdeviceptr dptr = 0;
  CUresult a = cuMemAllocAsync(&dptr, chunk, stream);
  pthread_join(observer, NULL);

  int failed = 0;
  if (a != CUDA_SUCCESS) {
    describe("cuMemAllocAsync (behind the gate)", a);
    failed = 1;
  } else {
    long long delta = gate.charged - gate.baseline;
    printf("  ledger while in flight: %.0f MiB (was %.0f MiB, allocation %.0f MiB)\n",
           MIB(gate.charged), MIB(gate.baseline), MIB(chunk));
    if (gate.charged < 0) {
      printf("  FAIL: could not read the ledger\n");
      failed = 1;
    } else if (delta < (long long)chunk) {
      printf("  FAIL: an in-flight allocation of %.0f MiB added only %.0f MiB to\n"
             "  the ledger -- a concurrent allocator would not see it and could\n"
             "  be handed memory that is already spoken for\n", MIB(chunk), MIB(delta));
      failed = 1;
    }
    cuMemFreeAsync(dptr, stream);
  }

  cuStreamSynchronize(stream);

  /* And it must not stay charged once the allocation is real: from here on NVML
   * reports it, so keeping the charge would count it twice. */
  if (!failed) {
    long long after = ledger_used_self();
    if (after > gate.baseline) {
      printf("  FAIL: %.0f MiB still charged after the allocation completed\n",
             MIB(after - gate.baseline));
      failed = 1;
    }
  }

  pthread_cond_destroy(&gate.cv);
  pthread_mutex_destroy(&gate.m);
  return failed;
}

/* --------------------------------------------------------------------- [B] --
 * [B] An allocation made during capture must be charged, and [C] that charge
 * must be gone once the capture ends. Both are read straight off the ledger. */
static int capture_allocation_is_charged(CUstream stream) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }   /* -1 skip, 1 fail */

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  long long before = ledger_used_self();
  CUresult r = cuStreamBeginCapture(stream, CU_STREAM_CAPTURE_MODE_RELAXED);
  if (r != CUDA_SUCCESS) { describe("cuStreamBeginCapture", r); return 1; }

  CUdeviceptr dptr = 0;
  CUresult a = cuMemAllocAsync(&dptr, chunk, stream);
  long long during = ledger_used_self();

  CUgraph graph = NULL;
  CUresult e = cuStreamEndCapture(stream, &graph);
  long long after = ledger_used_self();
  if (graph != NULL) cuGraphDestroy(graph);

  if (a != CUDA_SUCCESS) { describe("cuMemAllocAsync (in capture)", a); return 1; }
  if (e != CUDA_SUCCESS) describe("cuStreamEndCapture", e);

  printf("  ledger: %.0f MiB before, %.0f MiB during capture, %.0f MiB after\n",
         MIB(before), MIB(during), MIB(after));

  int failed = 0;
  if (during - before < (long long)chunk) {
    printf("  FAIL: a %.0f MiB allocation inside the capture added only %.0f MiB\n"
           "  to the ledger -- nothing accumulates, so several allocations in one\n"
           "  capture can reserve without ever meeting the limit\n",
           MIB(chunk), MIB(during - before));
    failed = 1;
  }
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged after the capture ended -- once the\n"
           "  graph runs NVML reports this memory too, so it is counted twice,\n"
           "  and a graph that owns its pointers never gives the charge back\n",
           MIB(after - before));
    failed = 1;
  }
  return failed;
}

/* --------------------------------------------------------------------- [C] --
 * A single unreleased charge is easy to miss; repeated cycles are not. This is
 * the shape the leak actually takes in production: a service that captures a
 * graph per request slowly starves itself. */
static int repeated_captures_do_not_accumulate(CUstream stream) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }   /* -1 skip, 1 fail */

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  long long before = ledger_used_self();
  const int cycles = 12;
  for (int i = 1; i <= cycles; i++) {
    CUresult r = cuStreamBeginCapture(stream, CU_STREAM_CAPTURE_MODE_RELAXED);
    if (r != CUDA_SUCCESS) { describe("cuStreamBeginCapture", r); return 1; }
    CUdeviceptr dptr = 0;
    CUresult a = cuMemAllocAsync(&dptr, chunk, stream);
    CUgraph graph = NULL;
    CUresult e = cuStreamEndCapture(stream, &graph);
    if (graph != NULL) cuGraphDestroy(graph);
    if (e != CUDA_SUCCESS) describe("cuStreamEndCapture", e);
    if (a == CUDA_ERROR_OUT_OF_MEMORY) {
      printf("  FAIL: cycle %d of %d was refused %.0f MiB against a %.0f MiB\n"
             "  limit -- earlier captures never gave their charge back\n",
             i, cycles, MIB(chunk), MIB(freeb));
      return 1;
    }
    if (a != CUDA_SUCCESS) { describe("cuMemAllocAsync (in capture)", a); return 1; }
  }

  long long after = ledger_used_self();
  printf("  %d capture cycles of %.0f MiB each; ledger %.0f MiB -> %.0f MiB\n",
         cycles, MIB(chunk), MIB(before), MIB(after));
  if (after > before) {
    printf("  FAIL: %.0f MiB accumulated over %d cycles\n", MIB(after - before), cycles);
    return 1;
  }
  return 0;
}

/* --------------------------------------------------------------------- [D] --
 * Being written down is not the same as being enforced: the charge has to feed
 * the limit check. While a capture holds one, an allocation that would have
 * fitted a moment ago must be refused. */
static int capture_charge_is_enforced(CUstream stream) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }   /* -1 skip, 1 fail */

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }
  /* Fits comfortably before the capture, cannot fit once `chunk` is charged. */
  size_t probe = freeb - chunk / 2;

  CUresult r = cuStreamBeginCapture(stream, CU_STREAM_CAPTURE_MODE_RELAXED);
  if (r != CUDA_SUCCESS) { describe("cuStreamBeginCapture", r); return 1; }
  CUdeviceptr held = 0;
  CUresult a = cuMemAllocAsync(&held, chunk, stream);

  CUdeviceptr probe_ptr = 0;
  CUresult p = cuMemAlloc(&probe_ptr, probe);

  CUgraph graph = NULL;
  CUresult e = cuStreamEndCapture(stream, &graph);
  if (graph != NULL) cuGraphDestroy(graph);
  if (e != CUDA_SUCCESS) describe("cuStreamEndCapture", e);
  if (a != CUDA_SUCCESS) {
    describe("cuMemAllocAsync (in capture)", a);
    if (p == CUDA_SUCCESS) cuMemFree(probe_ptr);
    return 1;
  }

  printf("  with %.0f MiB captured, a %.0f MiB request of a %.0f MiB limit:\n",
         MIB(chunk), MIB(probe), MIB(freeb));
  describe("cuMemAlloc (want OUT_OF_MEMORY)", p);

  int failed = 0;
  if (p == CUDA_SUCCESS) {
    printf("  FAIL: the captured allocation is recorded but not enforced --\n"
           "  the limit was handed out again while a capture still holds it\n");
    cuMemFree(probe_ptr);
    failed = 1;
  } else if (p != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: expected CUDA_ERROR_OUT_OF_MEMORY (%d)\n",
           (int)CUDA_ERROR_OUT_OF_MEMORY);
    failed = 1;
  }
  return failed;
}

/* ------------------------------------------------------------ graph memory --
 *
 * Helpers for the graph-allocation and capture-retirement cases below. */

static CUDA_MEM_ALLOC_NODE_PARAMS alloc_node_params(CUdevice dev, size_t bytes) {
  CUDA_MEM_ALLOC_NODE_PARAMS p;
  memset(&p, 0, sizeof(p));
  p.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  p.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  p.poolProps.location.id = dev;
  p.bytesize = bytes;
  return p;
}

/* The graph a capture on stream is building. CUDA 13 folded the edge-data
 * form into the plain name; earlier toolkits spell it _v2 without edges. */
static CUgraph capture_graph_of(CUstream stream) {
  CUstreamCaptureStatus st = CU_STREAM_CAPTURE_STATUS_NONE;
  cuuint64_t id = 0;
  CUgraph graph = NULL;
  const CUgraphNode *deps = NULL;
  size_t ndeps = 0;
#if CUDA_VERSION >= 13000
  if (cuStreamGetCaptureInfo(stream, &st, &id, &graph, &deps, NULL, &ndeps) != CUDA_SUCCESS) return NULL;
#else
  if (cuStreamGetCaptureInfo_v2(stream, &st, &id, &graph, &deps, &ndeps) != CUDA_SUCCESS) return NULL;
#endif
  return st == CU_STREAM_CAPTURE_STATUS_ACTIVE ? graph : NULL;
}

/* Trim the graph pool so one case's launched graph memory does not count
 * against the next case's headroom. */
static void trim_graph_pool(CUdevice dev) {
  cuCtxSynchronize();
  cuDeviceGraphMemTrim(dev);
}

/* --------------------------------------------------------------------- [E] --
 * A graph built by hand: its alloc node is checked against the limit, and is
 * not charged -- nothing retires a charge on a hand-built graph, and the
 * memory only becomes real (and visible to NVML) at upload or launch. */
static int alloc_node_is_checked_not_charged(CUdevice dev) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  int failed = 0;
  CUgraph graph = NULL;
  CHECK_DRV_API(cuGraphCreate(&graph, 0));

  CUDA_MEM_ALLOC_NODE_PARAMS over = alloc_node_params(dev, freeb + chunk);
  CUgraphNode node = NULL;
  CUresult r = cuGraphAddMemAllocNode(&node, graph, NULL, 0, &over);
  describe("cuGraphAddMemAllocNode (over the limit)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: an alloc node larger than the headroom was accepted -- the\n"
           "  graph would map it at launch past the limit\n");
    failed = 1;
  }

  long long before = ledger_used_self();
  CUDA_MEM_ALLOC_NODE_PARAMS fit = alloc_node_params(dev, chunk);
  r = cuGraphAddMemAllocNode(&node, graph, NULL, 0, &fit);
  long long after = ledger_used_self();
  describe("cuGraphAddMemAllocNode (within the limit)", r);
  if (r != CUDA_SUCCESS) {
    failed = 1;
  } else if (after != before) {
    printf("  FAIL: a hand-built alloc node charged %.0f MiB -- nothing would\n"
           "  ever retire it\n", MIB(after - before));
    failed = 1;
  }
  cuGraphDestroy(graph);
  return failed;
}

/* --------------------------------------------------------------------- [F] --
 * An alloc node added to the graph a capture is building: checked against the
 * limit like any alloc node (an oversized one is refused by the hook before
 * the driver sees it, so the capture goes on), and not charged: its memory is
 * mapped at upload/launch, where the graph's alloc nodes are checked as a
 * whole (test_graph_mem_admission). */
static int alloc_node_in_capture_is_checked_not_charged(CUdevice dev, CUstream stream) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  long long before = ledger_used_self();
  CHECK_DRV_API(cuStreamBeginCapture(stream, CU_STREAM_CAPTURE_MODE_RELAXED));
  CUgraph cap = capture_graph_of(stream);
  CUresult r_over = CUDA_ERROR_INVALID_VALUE, r = CUDA_ERROR_INVALID_VALUE;
  long long during = before;
  if (cap != NULL) {
    CUDA_MEM_ALLOC_NODE_PARAMS over = alloc_node_params(dev, freeb + chunk);
    CUgraphNode node = NULL;
    r_over = cuGraphAddMemAllocNode(&node, cap, NULL, 0, &over);
    CUDA_MEM_ALLOC_NODE_PARAMS p = alloc_node_params(dev, chunk);
    r = cuGraphAddMemAllocNode(&node, cap, NULL, 0, &p);
    during = ledger_used_self();
  }
  CUgraph graph = NULL;
  CUresult e = cuStreamEndCapture(stream, &graph);
  long long after = ledger_used_self();
  if (graph != NULL) cuGraphDestroy(graph);

  describe("cuGraphAddMemAllocNode (into the capture, over)", r_over);
  describe("cuGraphAddMemAllocNode (into the capture, within)", r);
  if (e != CUDA_SUCCESS) describe("cuStreamEndCapture", e);
  printf("  ledger: %.0f MiB before, %.0f MiB during capture, %.0f MiB after\n",
         MIB(before), MIB(during), MIB(after));
  if (cap == NULL) { printf("  FAIL: could not read the capture graph\n"); return 1; }
  int failed = 0;
  if (r_over != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: an alloc node larger than the headroom was accepted\n");
    failed = 1;
  }
  if (r != CUDA_SUCCESS || e != CUDA_SUCCESS) failed = 1;
  if (during != before || after != before) {
    printf("  FAIL: the alloc node was charged (%.0f MiB during, %.0f MiB after)\n",
           MIB(during - before), MIB(after - before));
    failed = 1;
  }
  return failed;
}

/* --------------------------------------------------------------------- [G] --
 * A capture the driver invalidates ends with an error and no graph -- the
 * application gets no handle to destroy -- and the driver stops reporting the
 * graph as soon as it is invalidated. Its charges must come off all the same. */
static int invalidated_capture_is_retired(CUstream stream) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  long long before = ledger_used_self();
  /* Global mode: a synchronize on the capturing stream is illegal and
   * invalidates the capture. */
  CHECK_DRV_API(cuStreamBeginCapture(stream, CU_STREAM_CAPTURE_MODE_GLOBAL));
  CUdeviceptr dptr = 0;
  CUresult a = cuMemAllocAsync(&dptr, chunk, stream);
  long long during = ledger_used_self();
  CUresult s = cuStreamSynchronize(stream);
  CUgraph graph = NULL;
  CUresult e = cuStreamEndCapture(stream, &graph);
  long long after = ledger_used_self();
  if (graph != NULL) cuGraphDestroy(graph);

  describe("cuMemAllocAsync (in capture)", a);
  describe("cuStreamSynchronize (invalidates)", s);
  describe("cuStreamEndCapture (want INVALIDATED)", e);
  printf("  ledger: %.0f MiB before, %.0f MiB during capture, %.0f MiB after\n",
         MIB(before), MIB(during), MIB(after));
  if (a != CUDA_SUCCESS) return 1;
  if (e != CUDA_ERROR_STREAM_CAPTURE_INVALIDATED) {
    printf("  SKIP (the capture was not invalidated; nothing to prove)\n");
    return -1;
  }
  int failed = 0;
  if (during - before < (long long)chunk) {
    printf("  FAIL: the allocation in capture was not charged\n");
    failed = 1;
  }
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged by an invalidated capture -- no graph\n"
           "  handle exists to destroy, so nothing else would ever retire it\n",
           MIB(after - before));
    failed = 1;
  }
  return failed;
}

/* --------------------------------------------------------------------- [H] --
 * A capture abandoned without cuStreamEndCapture, in a context that is then
 * destroyed: the driver destroys the context's graphs with no cuGraphDestroy
 * for the hooks to see. The context teardown must retire the charges. */
static int abandoned_capture_retired_by_ctx_destroy(CUdevice dev, CUcontext back) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  long long before = ledger_used_self();
  CUcontext ctx = NULL;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, dev));
  CUstream s = NULL;
  CHECK_DRV_API(cuStreamCreate(&s, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_RELAXED));
  CUdeviceptr dptr = 0;
  CUresult a = cuMemAllocAsync(&dptr, chunk, s);
  long long during = ledger_used_self();
  CUresult d = cuCtxDestroy(ctx);
  long long after = ledger_used_self();
  cuCtxSetCurrent(back);

  describe("cuMemAllocAsync (in capture)", a);
  describe("cuCtxDestroy (capture still open)", d);
  printf("  ledger: %.0f MiB before, %.0f MiB during capture, %.0f MiB after\n",
         MIB(before), MIB(during), MIB(after));
  if (a != CUDA_SUCCESS || d != CUDA_SUCCESS) return 1;
  int failed = 0;
  if (during - before < (long long)chunk) {
    printf("  FAIL: the allocation in capture was not charged\n");
    failed = 1;
  }
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged after the context was destroyed\n",
           MIB(after - before));
    failed = 1;
  }
  return failed;
}

/* --------------------------------------------------------------------- [I] --
 * The primary-context forms: a reset, and the release that drops the last
 * reference (which resets it), each with a capture left open. The primary
 * context reads as inactive right after either, so the hooks must find it
 * before the call. */
static int abandoned_capture_retired_by_primary(CUdevice dev, CUcontext back, int reset) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }

  unsigned int flags = 0;
  int active = 0;
  CHECK_DRV_API(cuDevicePrimaryCtxGetState(dev, &flags, &active));
  if (active) {
    printf("  SKIP (the primary context is already in use; tearing it down would\n"
           "        not be this case's to do)\n");
    return -1;
  }
  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  long long before = ledger_used_self();
  CUcontext pctx = NULL;
  CHECK_DRV_API(cuDevicePrimaryCtxRetain(&pctx, dev));
  CHECK_DRV_API(cuCtxSetCurrent(pctx));
  CUstream s = NULL;
  CHECK_DRV_API(cuStreamCreate(&s, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_RELAXED));
  CUdeviceptr dptr = 0;
  CUresult a = cuMemAllocAsync(&dptr, chunk, s);
  long long during = ledger_used_self();
  CUresult t = reset ? cuDevicePrimaryCtxReset(dev) : cuDevicePrimaryCtxRelease(dev);
  long long after = ledger_used_self();
  if (reset) cuDevicePrimaryCtxRelease(dev);
  cuCtxSetCurrent(back);

  describe("cuMemAllocAsync (in capture)", a);
  describe(reset ? "cuDevicePrimaryCtxReset (capture still open)"
                 : "cuDevicePrimaryCtxRelease (last reference)", t);
  printf("  ledger: %.0f MiB before, %.0f MiB during capture, %.0f MiB after\n",
         MIB(before), MIB(during), MIB(after));
  if (a != CUDA_SUCCESS || t != CUDA_SUCCESS) return 1;
  int failed = 0;
  if (during - before < (long long)chunk) {
    printf("  FAIL: the allocation in capture was not charged\n");
    failed = 1;
  }
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged after the primary context went away\n",
           MIB(after - before));
    failed = 1;
  }
  return failed;
}

/* --------------------------------------------------------------------- [K] --
 * The allocation is made on a stream joined into the capture by an event, and
 * the capture is invalidated there. The capture still ends on its origin
 * stream, whose graph is the one the joined stream's charge is keyed by. */
static int joined_stream_invalidation_is_retired(CUstream origin) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  CUstream joined = NULL;
  CUevent fork_ev = NULL;
  CHECK_DRV_API(cuStreamCreate(&joined, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuEventCreate(&fork_ev, CU_EVENT_DISABLE_TIMING));

  long long before = ledger_used_self();
  CHECK_DRV_API(cuStreamBeginCapture(origin, CU_STREAM_CAPTURE_MODE_GLOBAL));
  CHECK_DRV_API(cuEventRecord(fork_ev, origin));
  CHECK_DRV_API(cuStreamWaitEvent(joined, fork_ev, 0));
  CUdeviceptr dptr = 0;
  CUresult a = cuMemAllocAsync(&dptr, chunk, joined);
  long long during = ledger_used_self();
  CUresult s = cuStreamSynchronize(joined);
  CUgraph graph = NULL;
  CUresult e = cuStreamEndCapture(origin, &graph);
  long long after = ledger_used_self();
  if (graph != NULL) cuGraphDestroy(graph);
  cuEventDestroy(fork_ev);
  cuStreamDestroy(joined);

  describe("cuMemAllocAsync (on the joined stream)", a);
  describe("cuStreamSynchronize (joined, invalidates)", s);
  describe("cuStreamEndCapture (origin, want INVALIDATED)", e);
  printf("  ledger: %.0f MiB before, %.0f MiB during capture, %.0f MiB after\n",
         MIB(before), MIB(during), MIB(after));
  if (a != CUDA_SUCCESS) return 1;
  if (e != CUDA_ERROR_STREAM_CAPTURE_INVALIDATED) {
    printf("  SKIP (the capture was not invalidated; nothing to prove)\n");
    return -1;
  }
  int failed = 0;
  if (during - before < (long long)chunk) {
    printf("  FAIL: the allocation on the joined stream was not charged\n");
    failed = 1;
  }
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged by the invalidated capture\n", MIB(after - before));
    failed = 1;
  }
  return failed;
}

/* --------------------------------------------------------------------- [L] --
 * cuStreamBeginCaptureToGraph: capture into a graph the application owns, then
 * invalidate it. Needs the BeginCaptureToGraph hook to record the graph. */
static int capture_to_graph_invalidation_is_retired(CUstream stream) {
#if CUDA_VERSION >= 12030
  { int _pre = can_assert(); if (_pre != 0) return _pre; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  CUgraph owned = NULL;
  CHECK_DRV_API(cuGraphCreate(&owned, 0));
  long long before = ledger_used_self();
  CHECK_DRV_API(cuStreamBeginCaptureToGraph(stream, owned, NULL, NULL, 0, CU_STREAM_CAPTURE_MODE_GLOBAL));
  CUdeviceptr dptr = 0;
  CUresult a = cuMemAllocAsync(&dptr, chunk, stream);
  long long during = ledger_used_self();
  CUresult s = cuStreamSynchronize(stream);
  CUgraph graph = NULL;
  CUresult e = cuStreamEndCapture(stream, &graph);
  long long after = ledger_used_self();
  /* An invalidated capture takes the application's own graph with it (the
   * driver answers a later cuGraphDestroy with CUDA_ERROR_INVALID_VALUE, and
   * in a long-running process, once the handle's memory is reused, hangs). */
  if (e != CUDA_ERROR_STREAM_CAPTURE_INVALIDATED) cuGraphDestroy(owned);

  describe("cuMemAllocAsync (capture to graph)", a);
  describe("cuStreamSynchronize (invalidates)", s);
  describe("cuStreamEndCapture (want INVALIDATED)", e);
  printf("  ledger: %.0f MiB before, %.0f MiB during capture, %.0f MiB after\n",
         MIB(before), MIB(during), MIB(after));
  if (a != CUDA_SUCCESS) return 1;
  if (e != CUDA_ERROR_STREAM_CAPTURE_INVALIDATED) {
    printf("  SKIP (the capture was not invalidated; nothing to prove)\n");
    return -1;
  }
  int failed = 0;
  if (during - before < (long long)chunk) {
    printf("  FAIL: the allocation in capture was not charged\n");
    failed = 1;
  }
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged by the invalidated capture\n", MIB(after - before));
    failed = 1;
  }
  return failed;
#else
  (void)stream;
  printf("  SKIP (cuStreamBeginCaptureToGraph needs CUDA 12.3)\n");
  return -1;
#endif
}

/* --------------------------------------------------------------------- [Q] --
 * Ending a capture from another thread. In GLOBAL and THREAD_LOCAL mode the
 * driver refuses with CUDA_ERROR_STREAM_CAPTURE_WRONG_THREAD and terminates
 * the capture all the same: the stream reports no capture afterwards, and an
 * end on the right thread answers CUDA_ERROR_ILLEGAL_STATE (V100, driver 550).
 * No graph comes out and nothing else ends it, so that refused end is the last
 * chance to retire the charges. In RELAXED mode the end simply succeeds. */
typedef struct {
  CUcontext ctx;
  CUstream stream;
  CUgraph graph;
  CUresult ret;
} end_elsewhere_t;

static void *end_elsewhere(void *arg) {
  end_elsewhere_t *e = (end_elsewhere_t *)arg;
  cuCtxSetCurrent(e->ctx);
  e->ret = cuStreamEndCapture(e->stream, &e->graph);
  return NULL;
}

static int wrong_thread_end_is_retired(CUcontext ctx, CUstream stream) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  static const struct { CUstreamCaptureMode mode; const char *name; CUresult want; } modes[] = {
    {CU_STREAM_CAPTURE_MODE_GLOBAL, "GLOBAL", CUDA_ERROR_STREAM_CAPTURE_WRONG_THREAD},
    {CU_STREAM_CAPTURE_MODE_THREAD_LOCAL, "THREAD_LOCAL", CUDA_ERROR_STREAM_CAPTURE_WRONG_THREAD},
    {CU_STREAM_CAPTURE_MODE_RELAXED, "RELAXED", CUDA_SUCCESS},
  };
  int failed = 0;
  for (size_t m = 0; m < sizeof(modes) / sizeof(modes[0]); m++) {
    long long before = ledger_used_self();
    CHECK_DRV_API(cuStreamBeginCapture(stream, modes[m].mode));
    CUdeviceptr dptr = 0;
    CUresult a = cuMemAllocAsync(&dptr, chunk, stream);
    long long during = ledger_used_self();
    end_elsewhere_t e = {ctx, stream, NULL, CUDA_SUCCESS};
    pthread_t t;
    pthread_create(&t, NULL, end_elsewhere, &e);
    pthread_join(t, NULL);
    long long after = ledger_used_self();
    printf("  %-12s end from another thread -> %d; ledger %.0f / %.0f / %.0f MiB\n",
           modes[m].name, (int)e.ret, MIB(before), MIB(during), MIB(after));
    if (e.graph != NULL) cuGraphDestroy(e.graph);
    if (a != CUDA_SUCCESS) { describe("cuMemAllocAsync", a); failed = 1; continue; }
    if (e.ret != modes[m].want) {
      printf("  FAIL: expected %d from the end on another thread\n", (int)modes[m].want);
      failed = 1;
    }
    if (during - before < (long long)chunk) {
      printf("  FAIL: the allocation in capture was not charged\n");
      failed = 1;
    }
    if (after != before) {
      printf("  FAIL: %.0f MiB left charged by a capture that is over\n", MIB(after - before));
      failed = 1;
    }
    /* The stream must be usable for the next capture. */
    CUgraph g = NULL;
    CHECK_DRV_API(cuStreamBeginCapture(stream, modes[m].mode));
    CHECK_DRV_API(cuStreamEndCapture(stream, &g));
    if (g != NULL) cuGraphDestroy(g);
  }
  return failed;
}

/* --------------------------------------------------------------------- [R] --
 * Ending a capture on a stream that joined it, not the origin: the driver
 * answers CUDA_ERROR_STREAM_CAPTURE_UNMATCHED and invalidates the capture, so
 * the end on the origin stream reports INVALIDATED, and retires the charges. */
static int unmatched_end_is_retired(CUstream origin) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  CUstream side = NULL;
  CUevent ev = NULL;
  CHECK_DRV_API(cuStreamCreate(&side, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuEventCreate(&ev, CU_EVENT_DISABLE_TIMING));
  long long before = ledger_used_self();
  CHECK_DRV_API(cuStreamBeginCapture(origin, CU_STREAM_CAPTURE_MODE_RELAXED));
  CHECK_DRV_API(cuEventRecord(ev, origin));
  CHECK_DRV_API(cuStreamWaitEvent(side, ev, 0));
  CUdeviceptr dptr = 0;
  CUresult a = cuMemAllocAsync(&dptr, chunk, side);
  long long during = ledger_used_self();
  CUgraph g = NULL;
  CUresult e1 = cuStreamEndCapture(side, &g);
  long long kept = ledger_used_self();
  CUresult e2 = cuStreamEndCapture(origin, &g);
  long long after = ledger_used_self();
  describe("cuStreamEndCapture (joined stream)", e1);
  describe("cuStreamEndCapture (origin)", e2);
  printf("  ledger: %.0f MiB before, %.0f MiB during capture, %.0f MiB after\n",
         MIB(before), MIB(during), MIB(after));
  if (a == CUDA_SUCCESS && kept - before < (long long)chunk) {
    printf("  FAIL: the end on the joined stream dropped the charge before the origin ended\n");
    return 1;
  }
  int failed = 0;
  if (a != CUDA_SUCCESS) failed = 1;
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged\n", MIB(after - before));
    failed = 1;
  }
  cuEventDestroy(ev);
  cuStreamDestroy(side);
  return failed;
}

/* --------------------------------------------------------------------- [S] --
 * A fork not joined back: CUDA_ERROR_STREAM_CAPTURE_UNJOINED and the capture is
 * over, so its charges go. A NULL phGraph is no refusal: the driver answers
 * CUDA_SUCCESS and the capture is over too (V100, drivers 550, 570, 580). */
static int other_ends_release(CUstream origin) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }

  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  size_t chunk = freeb / 4;
  if (chunk == 0) { printf("  SKIP (limit too small to subdivide)\n"); return -1; }

  int failed = 0;
  CUstream side = NULL;
  CUevent ev = NULL;
  CHECK_DRV_API(cuStreamCreate(&side, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuEventCreate(&ev, CU_EVENT_DISABLE_TIMING));
  long long before = ledger_used_self();
  CHECK_DRV_API(cuStreamBeginCapture(origin, CU_STREAM_CAPTURE_MODE_RELAXED));
  CHECK_DRV_API(cuEventRecord(ev, origin));
  CHECK_DRV_API(cuStreamWaitEvent(side, ev, 0));
  CUdeviceptr dptr = 0;
  CUresult a = cuMemAllocAsync(&dptr, chunk, side);
  long long during = ledger_used_self();
  CUgraph g = NULL;
  CUresult e = cuStreamEndCapture(origin, &g);
  long long after = ledger_used_self();
  describe("cuStreamEndCapture (fork not joined)", e);
  printf("  ledger: %.0f MiB before, %.0f MiB during capture, %.0f MiB after\n",
         MIB(before), MIB(during), MIB(after));
  if (g != NULL) cuGraphDestroy(g);
  if (a != CUDA_SUCCESS || e != CUDA_ERROR_STREAM_CAPTURE_UNJOINED) failed = 1;
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged by a capture that is over\n", MIB(after - before));
    failed = 1;
  }
  cuStreamSynchronize(side);
  cuEventDestroy(ev);
  cuStreamDestroy(side);

  before = ledger_used_self();
  CHECK_DRV_API(cuStreamBeginCapture(origin, CU_STREAM_CAPTURE_MODE_RELAXED));
  a = cuMemAllocAsync(&dptr, chunk, origin);
  e = cuStreamEndCapture(origin, NULL);
  after = ledger_used_self();
  describe("cuStreamEndCapture (NULL phGraph)", e);
  printf("  ledger: %.0f MiB before, %.0f MiB after\n", MIB(before), MIB(after));
  if (a != CUDA_SUCCESS || e != CUDA_SUCCESS) failed = 1;
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged after the capture ended\n", MIB(after - before));
    failed = 1;
  }
  return failed;
}

/* --------------------------------------------------------------------- [T] --
 * An asynchronous allocation lands on its stream's device, which need not be
 * the current context's. Checked against the stream's device: refused when that
 * one is full, not refused because the current one is. Inside a capture as well,
 * where the driver must not be asked for the stream's device -- doing so
 * invalidates the capture. Needs two devices. */
static size_t headroom_of(CUcontext c, CUcontext back) {
  size_t freeb = 0, total = 0;
  cuCtxSetCurrent(c);
  cuMemGetInfo(&freeb, &total);
  cuCtxSetCurrent(back);
  return freeb;
}

static CUdeviceptr fill_ctx(CUcontext c, CUcontext back, size_t leave) {
  CUdeviceptr d = 0;
  size_t room = headroom_of(c, back);
  cuCtxSetCurrent(c);
  if (room <= leave || cuMemAlloc(&d, room - leave) != CUDA_SUCCESS) d = 0;
  cuCtxSetCurrent(back);
  return d;
}

static void free_ctx(CUcontext c, CUcontext back, CUdeviceptr d) {
  cuCtxSetCurrent(c);
  if (d) cuMemFree(d);
  cuCtxSetCurrent(back);
}

static int async_alloc_uses_stream_device(CUdevice dev, CUcontext ctx) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }
  int count = 0;
  cuDeviceGetCount(&count);
  /* Not a skip: that would report the whole file as skipped on a one-GPU host,
   * hiding every other case's verdict. */
  if (count < 2) { printf("  not run (needs 2 devices, have %d)\n", count); return 0; }
  CUdevice other;
  CHECK_DRV_API(cuDeviceGet(&other, dev == 0 ? 1 : 0));
  CUcontext octx = NULL;
  CHECK_DRV_API(cuDevicePrimaryCtxRetain(&octx, other));
  CUstream s1 = NULL;
  cuCtxSetCurrent(octx);
  CHECK_DRV_API(cuStreamCreate(&s1, CU_STREAM_NON_BLOCKING));
  cuCtxSetCurrent(ctx);
  size_t chunk = headroom_of(ctx, ctx) / 4;
  int failed = 0;
  CUresult r;
  CUdeviceptr p = 0;

  /* The stream's device full, the current one free. */
  CUdeviceptr filler = fill_ctx(octx, ctx, chunk / 2);
  r = cuMemAllocAsync(&p, chunk, s1);
  describe("cuMemAllocAsync (stream's device full)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: allocated past the limit of the stream's device\n");
    failed = 1;
    if (r == CUDA_SUCCESS) { cuStreamSynchronize(s1); cuMemFreeAsync(p, s1); }
  }
  CUmemPoolProps props;
  memset(&props, 0, sizeof(props));
  props.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  props.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  props.location.id = other;
  CUmemoryPool pool = NULL;
  CHECK_DRV_API(cuMemPoolCreate(&pool, &props));
  r = cuMemAllocFromPoolAsync(&p, chunk, pool, s1);
  describe("cuMemAllocFromPoolAsync (stream's device full)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: allocated past the limit of the stream's device\n");
    failed = 1;
    if (r == CUDA_SUCCESS) { cuStreamSynchronize(s1); cuMemFreeAsync(p, s1); }
  }
  cuStreamSynchronize(s1);
  cuMemPoolDestroy(pool);
  /* The same in a capture, which must survive it. */
  CUgraph g = NULL;
  CHECK_DRV_API(cuStreamBeginCapture(s1, CU_STREAM_CAPTURE_MODE_RELAXED));
  r = cuMemAllocAsync(&p, chunk, s1);
  describe("cuMemAllocAsync (in capture, stream's device full)", r);
  if (r != CUDA_ERROR_OUT_OF_MEMORY) failed = 1;
  CUresult e = cuStreamEndCapture(s1, &g);
  describe("cuStreamEndCapture", e);
  if (e != CUDA_SUCCESS) failed = 1;
  if (g != NULL) cuGraphDestroy(g);
  free_ctx(octx, ctx, filler);

  /* The current device full, the stream's free: no false refusal, and the
   * capture's charge lands on the stream's device. */
  filler = fill_ctx(ctx, ctx, chunk / 2);
  r = cuMemAllocAsync(&p, chunk, s1);
  describe("cuMemAllocAsync (current device full)", r);
  if (r != CUDA_SUCCESS) {
    printf("  FAIL: refused because of the wrong device\n");
    failed = 1;
  } else {
    cuStreamSynchronize(s1);
    cuMemFreeAsync(p, s1);
    cuStreamSynchronize(s1);
  }
  size_t room = headroom_of(octx, ctx);
  g = NULL;
  CHECK_DRV_API(cuStreamBeginCapture(s1, CU_STREAM_CAPTURE_MODE_RELAXED));
  r = cuMemAllocAsync(&p, chunk, s1);
  size_t in_capture = headroom_of(octx, ctx);
  e = r == CUDA_SUCCESS ? cuMemFreeAsync(p, s1) : r;
  CUresult e2 = cuStreamEndCapture(s1, &g);
  describe("cuMemAllocAsync (in capture, current device full)", r);
  describe("cuStreamEndCapture", e2);
  printf("  stream's device headroom: %.0f MiB, in capture %.0f MiB\n", MIB(room), MIB(in_capture));
  if (r != CUDA_SUCCESS || e != CUDA_SUCCESS || e2 != CUDA_SUCCESS) failed = 1;
  if (room - in_capture < chunk) {
    printf("  FAIL: the capture's allocation was not charged to the stream's device\n");
    failed = 1;
  }
  if (g != NULL) cuGraphDestroy(g);
  free_ctx(ctx, ctx, filler);
  cuStreamDestroy(s1);
  cuDevicePrimaryCtxRelease(other);
  cuCtxSetCurrent(ctx);
  return failed;
}

/* ------------------------------------------------------------ concurrency --
 * [M] Threads capturing concurrently on their own streams, every third capture
 * invalidated: every charge comes off. Thread-local mode, so one thread's
 * illegal synchronize invalidates only its own capture. */
typedef struct {
  CUcontext ctx;
  size_t chunk;
  int iters;
  int errors;
} capture_worker_t;

static void *capture_worker(void *arg) {
  capture_worker_t *w = (capture_worker_t *)arg;
  cuCtxSetCurrent(w->ctx);
  CUstream s = NULL;
  if (cuStreamCreate(&s, CU_STREAM_NON_BLOCKING) != CUDA_SUCCESS) { w->errors++; return NULL; }
  for (int i = 0; i < w->iters; i++) {
    if (cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_THREAD_LOCAL) != CUDA_SUCCESS) { w->errors++; continue; }
    CUdeviceptr d = 0;
    if (cuMemAllocAsync(&d, w->chunk, s) != CUDA_SUCCESS) w->errors++;
    if (i % 3 == 2) cuStreamSynchronize(s);   /* invalidates */
    CUgraph g = NULL;
    CUresult e = cuStreamEndCapture(s, &g);
    if (e != CUDA_SUCCESS && e != CUDA_ERROR_STREAM_CAPTURE_INVALIDATED) w->errors++;
    if (g != NULL) cuGraphDestroy(g);
  }
  cuStreamDestroy(s);
  return NULL;
}

static int concurrent_captures_retire(CUcontext ctx) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }
  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  enum { THREADS = 8, ITERS = 30 };
  size_t chunk = freeb / (THREADS * 4);
  long long before = ledger_used_self();
  pthread_t t[THREADS];
  capture_worker_t w[THREADS];
  for (int i = 0; i < THREADS; i++) {
    w[i].ctx = ctx; w[i].chunk = chunk; w[i].iters = ITERS; w[i].errors = 0;
    pthread_create(&t[i], NULL, capture_worker, &w[i]);
  }
  int errors = 0;
  for (int i = 0; i < THREADS; i++) { pthread_join(t[i], NULL); errors += w[i].errors; }
  long long after = ledger_used_self();
  printf("  %d threads x %d captures (a third invalidated), %d errors; ledger %.0f -> %.0f MiB\n",
         THREADS, ITERS, errors, MIB(before), MIB(after));
  int failed = errors != 0;
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged\n", MIB(after - before));
    failed = 1;
  }
  return failed;
}

/* [N] More captures open at once than the registry has slots (64): the
 * overflow falls back to the lookup at end of capture, and every capture that
 * ends normally still gives its charge back. */
static int registry_overflow_retires(void) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }
  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  enum { OPEN = 80 };
  size_t chunk = freeb / (OPEN * 2);
  CUstream s[OPEN];
  long long before = ledger_used_self();
  int errors = 0;
  for (int i = 0; i < OPEN; i++) {
    CHECK_DRV_API(cuStreamCreate(&s[i], CU_STREAM_NON_BLOCKING));
    if (cuStreamBeginCapture(s[i], CU_STREAM_CAPTURE_MODE_RELAXED) != CUDA_SUCCESS) errors++;
    CUdeviceptr d = 0;
    if (cuMemAllocAsync(&d, chunk, s[i]) != CUDA_SUCCESS) errors++;
  }
  long long during = ledger_used_self();
  for (int i = 0; i < OPEN; i++) {
    CUgraph g = NULL;
    if (cuStreamEndCapture(s[i], &g) != CUDA_SUCCESS) errors++;
    if (g != NULL) cuGraphDestroy(g);
    cuStreamDestroy(s[i]);
  }
  long long after = ledger_used_self();
  printf("  %d captures open at once, %d errors; ledger %.0f -> %.0f -> %.0f MiB\n",
         OPEN, errors, MIB(before), MIB(during), MIB(after));
  int failed = errors != 0;
  if (during - before < (long long)(chunk * OPEN)) {
    printf("  FAIL: not every open capture was charged\n");
    failed = 1;
  }
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged\n", MIB(after - before));
    failed = 1;
  }
  return failed;
}

/* [O] The per-thread default stream is one handle for every thread. Two
 * threads capture on it at once; one invalidates its capture, the other ends
 * normally: each must retire its own capture, not the other's. */
typedef struct { CUcontext ctx; size_t chunk; int invalidate; CUresult end; CUresult alloc; } ptds_worker_t;

static void *ptds_worker(void *arg) {
  ptds_worker_t *w = (ptds_worker_t *)arg;
  cuCtxSetCurrent(w->ctx);
  w->end = CUDA_ERROR_UNKNOWN;
  if (cuStreamBeginCapture(CU_STREAM_PER_THREAD, CU_STREAM_CAPTURE_MODE_THREAD_LOCAL) != CUDA_SUCCESS) return NULL;
  CUdeviceptr d = 0;
  w->alloc = cuMemAllocAsync(&d, w->chunk, CU_STREAM_PER_THREAD);
  usleep(20 * 1000);   /* both captures open at once */
  if (w->invalidate) cuStreamSynchronize(CU_STREAM_PER_THREAD);
  CUgraph g = NULL;
  w->end = cuStreamEndCapture(CU_STREAM_PER_THREAD, &g);
  if (g != NULL) cuGraphDestroy(g);
  return NULL;
}

static int per_thread_stream_captures_retire(CUcontext ctx) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }
  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  long long before = ledger_used_self();
  ptds_worker_t w[2] = {{ctx, freeb / 8, 1, CUDA_SUCCESS, CUDA_SUCCESS},
                        {ctx, freeb / 8, 0, CUDA_SUCCESS, CUDA_SUCCESS}};
  pthread_t t[2];
  for (int i = 0; i < 2; i++) pthread_create(&t[i], NULL, ptds_worker, &w[i]);
  for (int i = 0; i < 2; i++) pthread_join(t[i], NULL);
  long long after = ledger_used_self();
  describe("thread 1 cuStreamEndCapture (invalidated)", w[0].end);
  describe("thread 2 cuStreamEndCapture", w[1].end);
  printf("  ledger %.0f -> %.0f MiB\n", MIB(before), MIB(after));
  if (w[0].alloc != CUDA_SUCCESS || w[1].alloc != CUDA_SUCCESS) {
    printf("  FAIL: allocation in capture on the per-thread stream failed\n");
    return 1;
  }
  if (w[0].end != CUDA_ERROR_STREAM_CAPTURE_INVALIDATED || w[1].end != CUDA_SUCCESS) {
    printf("  SKIP (captures did not end as arranged)\n");
    return -1;
  }
  if (after != before) {
    printf("  FAIL: %.0f MiB left charged\n", MIB(after - before));
    return 1;
  }
  return 0;
}

/* [P] Fork while another thread keeps taking the capture registry's lock. The
 * child inherits whatever state the lock was in; it must still get through the
 * capture hooks (its CUDA calls fail -- the context did not survive fork --
 * but the hooks run first) instead of deadlocking. */
static volatile int g_hammer_stop;

static void *registry_hammer(void *arg) {
  capture_worker_t *w = (capture_worker_t *)arg;
  cuCtxSetCurrent(w->ctx);
  CUstream s = NULL;
  if (cuStreamCreate(&s, CU_STREAM_NON_BLOCKING) != CUDA_SUCCESS) return NULL;
  while (!g_hammer_stop) {
    if (cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_RELAXED) != CUDA_SUCCESS) continue;
    CUdeviceptr d = 0;
    cuMemAllocAsync(&d, w->chunk, s);
    CUgraph g = NULL;
    cuStreamEndCapture(s, &g);
    if (g != NULL) cuGraphDestroy(g);
  }
  cuStreamDestroy(s);
  return NULL;
}

static int fork_during_capture_traffic(CUcontext ctx) {
  { int _pre = can_assert(); if (_pre != 0) return _pre; }
  size_t freeb = 0;
  if (headroom(&freeb) != CUDA_SUCCESS) return 1;
  capture_worker_t w = {ctx, freeb / 64, 0, 0};
  g_hammer_stop = 0;
  pthread_t t;
  pthread_create(&t, NULL, registry_hammer, &w);
  usleep(50 * 1000);
  int failed = 0, forks = 40;
  for (int i = 0; i < forks && !failed; i++) {
    pid_t pid = fork();
    if (pid == 0) {
      CUstream s = (CUstream)0x1;   /* any handle: the hooks take the lock before the driver says no */
      CUgraph g = NULL;
      cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_RELAXED);
      cuStreamEndCapture(s, &g);
      CUgraph mine = NULL;
      cuGraphCreate(&mine, 0);
      CUDA_MEM_ALLOC_NODE_PARAMS p;
      memset(&p, 0, sizeof(p));
      p.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
      p.bytesize = 1 << 20;
      CUgraphNode n = NULL;
      cuGraphAddMemAllocNode(&n, mine, NULL, 0, &p);
      _exit(0);
    }
    if (pid < 0) { printf("  FAIL: fork failed\n"); failed = 1; break; }
    int status = 0, waited = 0;
    for (int ms = 0; ms < 10000; ms += 10) {
      if (waitpid(pid, &status, WNOHANG) == pid) { waited = 1; break; }
      usleep(10 * 1000);
    }
    if (!waited) {
      printf("  FAIL: child %d of %d did not finish in 10 s -- deadlocked on an\n"
             "  inherited lock\n", i + 1, forks);
      kill(pid, SIGKILL);
      waitpid(pid, &status, 0);
      failed = 1;
    } else if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
      printf("  FAIL: child %d exited abnormally (status %#x)\n", i + 1, status);
      failed = 1;
    }
  }
  g_hammer_stop = 1;
  pthread_join(t, NULL);
  if (!failed) printf("  %d forks during capture traffic: every child finished\n", forks);
  return failed;
}

int main(void) {
  ledger_open();

  CHECK_DRV_API(cuInit(0));
  CUdevice device;
  CHECK_DRV_API(cuDeviceGet(&device, TEST_DEVICE_ID));
  CUcontext ctx;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, device));
  CUstream stream;
  CHECK_DRV_API(cuStreamCreate(&stream, CU_STREAM_NON_BLOCKING));

  /* The ledger file is created during the library's lazy init, so a first
   * attempt before any CUDA call can legitimately miss it. */
  if (g_ledger == NULL) ledger_open();

  int failures = 0, skipped = 0;
  int rc;
  #define RUN_CASE(title, call)                                               \
    do {                                                                      \
      printf(title "\n");                                                     \
      rc = (call);                                                            \
      if (rc < 0) skipped++; else failures += rc;                             \
    } while (0)

  RUN_CASE("[A] an in-flight async allocation is charged while it is pending",
           inflight_allocation_is_charged(stream));
  RUN_CASE("[B] an allocation made during capture is charged, and released at end",
           capture_allocation_is_charged(stream));
  RUN_CASE("[C] repeated capture cycles do not accumulate charges",
           repeated_captures_do_not_accumulate(stream));
  RUN_CASE("[D] a capture's charge is enforced, not just recorded",
           capture_charge_is_enforced(stream));
  RUN_CASE("[E] a hand-built graph's alloc node is checked, not charged",
           alloc_node_is_checked_not_charged(device));
  RUN_CASE("[F] an alloc node added to a capture is checked, not charged",
           alloc_node_in_capture_is_checked_not_charged(device, stream));
  RUN_CASE("[G] an invalidated capture's charges are released",
           invalidated_capture_is_retired(stream));
  RUN_CASE("[K] an invalidated capture's charges on a joined stream are released",
           joined_stream_invalidation_is_retired(stream));
  RUN_CASE("[L] an invalidated capture into an owned graph is released",
           capture_to_graph_invalidation_is_retired(stream));
  RUN_CASE("[Q] a capture ended from another thread is released",
           wrong_thread_end_is_retired(ctx, stream));
  RUN_CASE("[R] a capture ended on a joined stream is released",
           unmatched_end_is_retired(stream));
  RUN_CASE("[S] an unjoined end and a NULL phGraph end release the charges",
           other_ends_release(stream));
  RUN_CASE("[T] an async allocation is checked on its stream's device",
           async_alloc_uses_stream_device(device, ctx));
  RUN_CASE("[M] concurrent captures across threads, some invalidated, all released",
           concurrent_captures_retire(ctx));
  RUN_CASE("[N] more captures open at once than the registry holds are released",
           registry_overflow_retires());
  RUN_CASE("[O] per-thread default stream captured by two threads at once",
           per_thread_stream_captures_retire(ctx));
  RUN_CASE("[P] fork during capture traffic: the child never deadlocks",
           fork_during_capture_traffic(ctx));
  RUN_CASE("[H] a capture abandoned in a destroyed context is released",
           abandoned_capture_retired_by_ctx_destroy(device, ctx));
  RUN_CASE("[I] a capture abandoned in a reset primary context is released",
           abandoned_capture_retired_by_primary(device, ctx, 1));
  RUN_CASE("[J] a capture abandoned when the primary context's last reference goes",
           abandoned_capture_retired_by_primary(device, ctx, 0));
  trim_graph_pool(device);
  #undef RUN_CASE

  int verdict = vgpu_test_verdict(failures, skipped);

  cuStreamDestroy(stream);
  CHECK_DRV_API(cuCtxDestroy(ctx));
  return verdict;
}
