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
 * Honest limitation: none of these prove the cuGraphDestroy discharge does any
 * work, because cuStreamEndCapture has already retired the charge by then. That
 * second discharge exists for a capture whose graph could no longer be
 * identified at end-of-capture, a state a test cannot force.
 *
 * Built twice: the _ptsz variant compiles this same source with
 * CUDA_API_PER_THREAD_DEFAULT_STREAM so the _ptsz copies of the hooks -- which
 * are separate code -- get the same coverage. Without it a defect introduced in
 * cuMemAllocAsync_ptsz passes every test in the suite.
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so CUDA_MEM_LIMIT=2048m \
 *     VMEMORY_NODE_ENABLED=1 ./test_alloc_async_accounting
 */
#include <cuda.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/mman.h>
#include <sys/stat.h>

#include "test_utils.h"

/* ------------------------------------------------------------------ ledger --
 *
 * Mirror of the shared virtual-memory ledger (include/hook.h: process_used_t /
 * device_vmem_used_t / device_vmemory_t). Reading it is what makes these cases
 * able to see a charge instead of guessing at one from an allocation result.
 * Keep in step with hook.h; a layout change here shows up as a charge that
 * never appears, and the cases say so rather than passing quietly. */
#define LEDGER_PATH        "/tmp/.vmem_node/vmem_node.config"
#define LEDGER_MAX_PIDS    1024
#define LEDGER_MAX_DEVICES 16

typedef struct { int pid; size_t used; } ledger_proc_t;
typedef struct {
  ledger_proc_t processes[LEDGER_MAX_PIDS];
  unsigned int  processes_size;
  unsigned char lock_byte;
} ledger_dev_t;
typedef struct { ledger_dev_t devices[LEDGER_MAX_DEVICES]; } ledger_t;

static const volatile ledger_t *g_ledger;

static void ledger_open(void) {
  int fd = open(LEDGER_PATH, O_RDONLY);
  if (fd < 0) return;
  struct stat sb;
  if (fstat(fd, &sb) != 0) { close(fd); return; }
  /* Exact, not >=. The library sizes this file to its own struct, so a
   * mismatch means the mirror above has drifted from hook.h -- in which case
   * every field read here would land at the wrong offset. Refusing to map it
   * turns that into a visible SKIP instead of assertions on garbage. */
  if ((size_t)sb.st_size != sizeof(ledger_t)) {
    printf("  [warn] " LEDGER_PATH " is %lld bytes, expected %zu --\n"
           "         the ledger mirror in this test no longer matches hook.h\n",
           (long long)sb.st_size, sizeof(ledger_t));
    close(fd);
    return;
  }
  void *p = mmap(NULL, sizeof(ledger_t), PROT_READ, MAP_SHARED, fd, 0);
  if (p != MAP_FAILED) g_ledger = (const volatile ledger_t *)p;
  close(fd);
}

/* Bytes currently charged to this process, summed over devices, or -1 if the
 * ledger is not available. Read without the record lock: every field consulted
 * is a naturally aligned scalar, and a charge that is momentarily stale only
 * makes the assertions below more conservative, never less. */
static long long ledger_used_self(void) {
  if (g_ledger == NULL) return -1;
  int self = getpid();
  long long total = 0;
  for (int d = 0; d < LEDGER_MAX_DEVICES; d++) {
    unsigned int n = g_ledger->devices[d].processes_size;
    if (n > LEDGER_MAX_PIDS) continue;   /* torn/garbage read, skip this device */
    for (unsigned int i = 0; i < n; i++) {
      if (g_ledger->devices[d].processes[i].pid == self) {
        total += (long long)g_ledger->devices[d].processes[i].used;
      }
    }
  }
  return total;
}

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

/* Common preconditions. Returns 0 when the case can meaningfully run. */
static int can_assert(void) {
  if (!preloaded()) {
    printf("  SKIP (needs LD_PRELOAD=libvgpu-control.so)\n");
    return -1;
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
  /* The allocating thread is now blocked in its post-allocation synchronize
   * (or, if that synchronize was removed, has already returned). Either way
   * this is the moment the charge has to be visible. */
  usleep(50 * 1000);
  g->charged = ledger_used_self();

  pthread_mutex_lock(&g->m);
  g->release = 1;
  pthread_cond_broadcast(&g->cv);
  pthread_mutex_unlock(&g->m);
  return NULL;
}

/* [A] An allocation that has not landed yet must still be charged, so that a
 * concurrent allocator counts it. */
static int inflight_allocation_is_charged(CUstream stream) {
  if (can_assert() != 0) return -1;

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
  if (can_assert() != 0) return -1;

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
  if (can_assert() != 0) return -1;

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
  if (can_assert() != 0) return -1;

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
  #undef RUN_CASE

  int verdict = vgpu_test_verdict(failures, skipped);

  cuStreamDestroy(stream);
  CHECK_DRV_API(cuCtxDestroy(ctx));
  return verdict;
}
