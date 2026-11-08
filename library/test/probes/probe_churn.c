/*
 * Run one way of coming and going of a table entry in a loop. Paired with the
 * "table stats" line the library logs at exit (LOGGER_LEVEL=4), this tells a table
 * that reclaims from one that grows for as long as the process runs. run_probes.sh
 * holds the expected numbers.
 *
 * Usage: probe_churn MODE N
 */
#include <cuda.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "test_utils.h"

static CUdevice g_dev, g_other;
static CUcontext g_ctx;
static CUstream g_stream;
static int g_two_devices;
static long g_bad;

#define CHECK(call)                                                            \
  do {                                                                         \
    CUresult _r = (call);                                                      \
    if (_r != CUDA_SUCCESS) {                                                  \
      printf("%s -> %d\n", #call, (int)_r);                                    \
      exit(1);                                                                 \
    }                                                                          \
  } while (0)
#define COUNT_FAILURE(call) do { if ((call) != CUDA_SUCCESS) g_bad++; } while (0)

static long rss_kb(void) {
  long size = 0, resident = 0;
  FILE *f = fopen("/proc/self/statm", "r");
  if (f != NULL) {
    if (fscanf(f, "%ld %ld", &size, &resident) != 2) resident = 0;
    fclose(f);
  }
  return resident * 4;
}

static void add_alloc_free(CUgraph g, CUdevice dev, CUgraphNode *prev) {
  CUDA_MEM_ALLOC_NODE_PARAMS a;
  memset(&a, 0, sizeof(a));
  a.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  a.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  a.poolProps.location.id = dev;
  a.bytesize = 1 << 20;
  CUgraphNode alloc, freed;
  CHECK(cuGraphAddMemAllocNode(&alloc, g, *prev != NULL ? prev : NULL, *prev != NULL ? 1 : 0, &a));
  CHECK(cuGraphAddMemFreeNode(&freed, g, &alloc, 1, a.dptr));
  *prev = freed;
}

/* An exec with its graph gone: allocating on dev, and on g_other too if asked. */
static CUgraphExec make_exec(CUdevice dev, int both) {
  CUgraph g;
  CUgraphExec e;
  CUgraphNode prev = NULL;
  CHECK(cuGraphCreate(&g, 0));
  add_alloc_free(g, dev, &prev);
  if (both) add_alloc_free(g, g_other, &prev);
  CHECK(cuGraphInstantiateWithFlags(&e, g, 0));
  CHECK(cuGraphDestroy(g));
  return e;
}

static CUmemoryPool make_pool(void) {
  CUmemPoolProps props;
  memset(&props, 0, sizeof(props));
  props.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  props.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  props.location.id = g_dev;
  CUmemoryPool pool;
  CHECK(cuMemPoolCreate(&pool, &props));
  return pool;
}

/* A context of its own, created current, for the caller to destroy. */
static CUcontext new_context(void) {
  CUcontext x;
  CHECK(CUCTX_CREATE(&x, 0, g_dev));
  return x;
}

static void *end_from_here(void *arg) {
  CUgraph g = NULL;
  (void)arg;
  cuCtxSetCurrent(g_ctx);
  if (cuStreamEndCapture(g_stream, &g) != CUDA_ERROR_STREAM_CAPTURE_WRONG_THREAD) g_bad++;
  return NULL;
}

static void *capture_and_exit(void *arg) {
  CUdeviceptr p;
  (void)arg;
  cuCtxSetCurrent(g_ctx);
  cuStreamBeginCapture(CU_STREAM_PER_THREAD, CU_STREAM_CAPTURE_MODE_RELAXED);
  cuMemAllocAsync(&p, 1 << 16, CU_STREAM_PER_THREAD);
  return NULL;
}

/* ---- the graph cost table ---- */
static void m_exec(void) { COUNT_FAILURE(cuGraphExecDestroy(make_exec(g_dev, 0))); }
static void m_exec_multi(void) { COUNT_FAILURE(cuGraphExecDestroy(make_exec(g_dev, 1))); }

static void m_ctx(void) {
  CUcontext x = new_context();
  make_exec(g_dev, 0);
  COUNT_FAILURE(cuCtxDestroy(x));
  cuCtxSetCurrent(g_ctx);
}

static void m_ctx_not_current(void) {
  CUcontext x = new_context(), popped;
  make_exec(g_dev, 0);
  CHECK(cuCtxPopCurrent(&popped));
  cuCtxSetCurrent(g_ctx);
  COUNT_FAILURE(cuCtxDestroy(x));
}

static void m_ctx_multi(void) {
  CUcontext x = new_context();
  make_exec(g_dev, 1);
  COUNT_FAILURE(cuCtxDestroy(x));
  cuCtxSetCurrent(g_ctx);
}

/* An update on the way, of a graph that stays the exec's. */
static void m_exec_update(void) {
  CUgraph g;
  CUgraphExec e;
  CUgraphNode prev = NULL;
  CHECK(cuGraphCreate(&g, 0));
  add_alloc_free(g, g_dev, &prev);
  CHECK(cuGraphInstantiateWithFlags(&e, g, 0));
  COUNT_FAILURE(cuGraphExecUpdate(e, g, NULL));
  COUNT_FAILURE(cuGraphExecDestroy(e));
  COUNT_FAILURE(cuGraphDestroy(g));
}

static CUdevice teardown_device(void) { return g_two_devices ? g_other : g_dev; }

static void exec_on_primary(CUdevice d) {
  CUcontext x;
  CHECK(cuDevicePrimaryCtxRetain(&x, d));
  cuCtxSetCurrent(x);
  make_exec(d, 0);
  cuCtxSetCurrent(g_ctx);
}

static void m_primary_release(void) {
  exec_on_primary(teardown_device());
  COUNT_FAILURE(cuDevicePrimaryCtxRelease(teardown_device()));
}

static void m_primary_reset(void) {
  exec_on_primary(teardown_device());
  COUNT_FAILURE(cuDevicePrimaryCtxReset(teardown_device()));
  cuDevicePrimaryCtxRelease(teardown_device());
}

/* ---- the capture registry ---- */
static void m_capture(void) {
  CUgraph g = NULL;
  CHECK(cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_RELAXED));
  COUNT_FAILURE(cuStreamEndCapture(g_stream, &g));
  if (g != NULL) cuGraphDestroy(g);
}

static void m_capture_invalidated(void) {
  CUgraph g = NULL;
  CUdeviceptr p;
  CHECK(cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_GLOBAL));
  cuMemAllocAsync(&p, 1 << 16, g_stream);
  cuStreamSynchronize(g_stream);   /* not allowed in a capture: invalidates it */
  if (cuStreamEndCapture(g_stream, &g) != CUDA_ERROR_STREAM_CAPTURE_INVALIDATED) g_bad++;
}

static void m_capture_wrong_thread(void) {
  pthread_t t;
  CHECK(cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_GLOBAL));
  pthread_create(&t, NULL, end_from_here, NULL);
  pthread_join(t, NULL);
}

static void m_capture_stream_destroyed(void) {
  CUstream s;
  CHECK(cuStreamCreate(&s, 0));
  CHECK(cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_RELAXED));
  COUNT_FAILURE(cuStreamDestroy(s));
}

static void m_capture_ctx_destroyed(void) {
  CUcontext x = new_context();
  CUstream s;
  CHECK(cuStreamCreate(&s, 0));
  CHECK(cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_RELAXED));
  COUNT_FAILURE(cuCtxDestroy(x));
  cuCtxSetCurrent(g_ctx);
}

/* The stream's context goes while another one is current. */
static void m_capture_other_ctx_destroyed(void) {
  CUcontext x = new_context(), popped;
  CUstream s;
  CUdeviceptr p;
  CHECK(cuStreamCreate(&s, 0));
  CHECK(cuCtxPopCurrent(&popped));
  cuCtxSetCurrent(g_ctx);
  CHECK(cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_RELAXED));
  cuMemAllocAsync(&p, 1 << 16, s);
  COUNT_FAILURE(cuCtxDestroy(x));
  cuCtxSetCurrent(g_ctx);
}

/* The context current at the beginning goes first, and the stream's outlives it. */
static void m_capture_begin_ctx_destroyed(void) {
  CUcontext x = new_context();
  CUgraph g = NULL;
  CUdeviceptr p;
  CHECK(cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_RELAXED));
  COUNT_FAILURE(cuMemAllocAsync(&p, 1 << 16, g_stream));
  COUNT_FAILURE(cuCtxDestroy(x));
  cuCtxSetCurrent(g_ctx);
  COUNT_FAILURE(cuMemFreeAsync(p, g_stream));
  COUNT_FAILURE(cuStreamEndCapture(g_stream, &g));
  if (g != NULL) cuGraphDestroy(g);
}

static void m_capture_thread_exit(void) {
  pthread_t t;
  pthread_create(&t, NULL, capture_and_exit, NULL);
  pthread_join(t, NULL);
}

/* ---- the table of blocks graphs left allocated ---- */
static CUgraphExec unfreed_exec(CUdeviceptr *dptr) {
  CUgraph g;
  CUgraphExec e;
  CUgraphNode n;
  CUDA_MEM_ALLOC_NODE_PARAMS a;
  memset(&a, 0, sizeof(a));
  a.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  a.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  a.poolProps.location.id = g_dev;
  a.bytesize = 1 << 20;
  CHECK(cuGraphCreate(&g, 0));
  CHECK(cuGraphAddMemAllocNode(&n, g, NULL, 0, &a));
  CHECK(cuGraphInstantiateWithFlags(&e, g, 0));
  CHECK(cuGraphDestroy(g));
  *dptr = a.dptr;
  return e;
}

static void m_held(void) {
  CUdeviceptr p;
  CUgraphExec e = unfreed_exec(&p);
  COUNT_FAILURE(cuGraphLaunch(e, g_stream));
  COUNT_FAILURE(cuStreamSynchronize(g_stream));
  COUNT_FAILURE(cuGraphExecDestroy(e));
  COUNT_FAILURE(cuMemFree(p));
}

static void m_held_ctx(void) {
  CUcontext x = new_context();
  CUstream s;
  CUdeviceptr p;
  CHECK(cuStreamCreate(&s, 0));
  CUgraphExec e = unfreed_exec(&p);
  COUNT_FAILURE(cuGraphLaunch(e, s));
  COUNT_FAILURE(cuStreamSynchronize(s));
  COUNT_FAILURE(cuCtxDestroy(x));
  cuCtxSetCurrent(g_ctx);
  /* The block outlives its context; freed from another. */
  COUNT_FAILURE(cuMemFree(p));
}

/* ---- the pool table ---- */
static void m_pool(void) { COUNT_FAILURE(cuMemPoolDestroy(make_pool())); }

static void m_pool_get(void) {
  CUmemoryPool p;
  COUNT_FAILURE(cuDeviceGetDefaultMemPool(&p, g_dev));
  COUNT_FAILURE(cuDeviceGetMemPool(&p, g_dev));
}

static const struct {
  const char *name;
  void (*run)(void);
  int needs_two_devices;
} g_modes[] = {
  {"exec", m_exec, 0},
  {"exec_multi", m_exec_multi, 1},
  {"exec_update", m_exec_update, 0},
  {"ctx", m_ctx, 0},
  {"ctx_not_current", m_ctx_not_current, 0},
  {"ctx_multi", m_ctx_multi, 1},
  /* On one device this is not the last reference, and its execs stay, rightly. */
  {"primary_release", m_primary_release, 1},
  {"primary_reset", m_primary_reset, 0},
  {"capture", m_capture, 0},
  {"capture_invalidated", m_capture_invalidated, 0},
  {"capture_wrong_thread", m_capture_wrong_thread, 0},
  {"capture_stream_destroyed", m_capture_stream_destroyed, 0},
  {"capture_ctx_destroyed", m_capture_ctx_destroyed, 0},
  {"capture_other_ctx_destroyed", m_capture_other_ctx_destroyed, 0},
  {"capture_begin_ctx_destroyed", m_capture_begin_ctx_destroyed, 0},
  {"capture_thread_exit", m_capture_thread_exit, 0},
  {"held", m_held, 0},
  {"held_ctx", m_held_ctx, 0},
  {"pool", m_pool, 0},
  {"pool_get", m_pool_get, 0},
};

int main(int argc, char **argv) {
  if (argc != 3) {
    printf("usage: %s MODE N\n", argv[0]);
    return 2;
  }
  int count = 0;
  CHECK(cuInit(0));
  CHECK(cuDeviceGet(&g_dev, 0));
  CHECK(cuDeviceGetCount(&count));
  g_two_devices = count > 1;
  if (g_two_devices) CHECK(cuDeviceGet(&g_other, 1));
  CHECK(cuDevicePrimaryCtxRetain(&g_ctx, g_dev));
  CHECK(cuCtxSetCurrent(g_ctx));
  CHECK(cuStreamCreate(&g_stream, 0));

  for (size_t m = 0; m < sizeof(g_modes) / sizeof(g_modes[0]); m++) {
    if (strcmp(argv[1], g_modes[m].name) != 0) continue;
    if (g_modes[m].needs_two_devices && !g_two_devices) {
      printf("%s: needs two devices\n", argv[1]);
      return 77;
    }
    long n = atol(argv[2]), before = rss_kb();
    for (long i = 0; i < n; i++) g_modes[m].run();
    printf("%-28s N=%-7ld failures=%ld rss %ld -> %ld kB\n", argv[1], n, g_bad, before, rss_kb());
    return g_bad != 0;
  }
  printf("unknown mode %s\n", argv[1]);
  return 2;
}
