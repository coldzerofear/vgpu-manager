/*
 * The SM limit's idle-gap path (CUDA_CORE_LIMIT under 100) times the first launch
 * after 200 ms idle with two events and waits for the end one. That event completes
 * only once all the stream had queued before has run -- here a cuStreamWaitValue32 on
 * host memory this thread writes only after its launch returns. Natively the launch
 * returns at once; the library waited without end, a hang for good. It now gives the
 * measurement up after GAP_WAIT_MAX_NS (1 s).
 *
 * A watchdog writes the value after WATCHDOG_S, so a library that still waits fails
 * the case instead of hanging the suite. A launch back within 0.5 s means the gap path
 * did not engage (no limit, watcher not up): nothing was checked, reported as SKIP.
 *
 * Driver API only, graph launch and kernel launch (an empty kernel JIT-compiled from
 * PTX; skipped if the driver refuses it).
 */
#include <cuda.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#define WATCHDOG_S 10.0
#define BOUND_S    3.0   /* the 1 s wait, with room for the duty-cycle sleep and a slow box */
#define ENGAGED_S  0.5

static const char *k_ptx =
    ".version 6.0\n"
    ".target sm_50\n"
    ".address_size 64\n"
    ".visible .entry noop()\n"
    "{\n"
    "  ret;\n"
    "}\n";

static volatile unsigned *g_flag;
static volatile int g_done;

static double now_s(void) {
  struct timespec t;
  clock_gettime(CLOCK_MONOTONIC, &t);
  return (double)t.tv_sec + (double)t.tv_nsec / 1e9;
}

static void *watchdog(void *arg) {
  (void)arg;
  double end = now_s() + WATCHDOG_S;
  while (!g_done && now_s() < end) usleep(10 * 1000);
  *g_flag = 1;
  return NULL;
}

typedef struct {
  const char *name;
  CUgraphExec exec;
  CUfunction fn;
} launcher_t;

static CUresult launch(const launcher_t *l, CUstream s) {
  if (l->exec != NULL) return cuGraphLaunch(l->exec, s);
  return cuLaunchKernel(l->fn, 1, 1, 1, 1, 1, 1, 0, s, NULL, NULL);
}

/* 0 pass, 1 fail, 77 not engaged or not supported. */
static int one(const launcher_t *l, CUstream s, CUdeviceptr dflag) {
  for (int i = 0; i < 3; i++) {   /* the watcher up, and a gap to measure */
    if (launch(l, s) != CUDA_SUCCESS || cuStreamSynchronize(s) != CUDA_SUCCESS) {
      printf("  %s: warm-up launch failed\n", l->name);
      return 1;
    }
    usleep(100 * 1000);
  }
  usleep(300 * 1000);   /* idle past the 200 ms threshold */
  *g_flag = 0;
  g_done = 0;
  CUresult w = cuStreamWaitValue32(s, dflag, 1, CU_STREAM_WAIT_VALUE_GEQ);
  if (w != CUDA_SUCCESS) {
    printf("  %s: cuStreamWaitValue32 -> %d, not supported here\n", l->name, (int)w);
    return 77;
  }
  pthread_t th;
  pthread_create(&th, NULL, watchdog, NULL);
  double t0 = now_s();
  CUresult r = launch(l, s);
  double took = now_s() - t0;
  *g_flag = 1;   /* what the stream waits for, written once the launch is back */
  g_done = 1;
  pthread_join(th, NULL);
  CUresult sy = cuStreamSynchronize(s);
  printf("  %s: launch -> %d after %.3f s, stream -> %d\n", l->name, (int)r, took, (int)sy);
  if (r != CUDA_SUCCESS || sy != CUDA_SUCCESS) {
    printf("  FAIL: the launch or the stream failed\n");
    return 1;
  }
  if (took > BOUND_S) {
    printf("  FAIL: the launch waited for the stream (%.1f s; the watchdog writes at %.0f s)\n",
           took, WATCHDOG_S);
    return 1;
  }
  if (took < ENGAGED_S) {
    printf("  (back at once: the idle-gap path did not engage, nothing checked)\n");
    return 77;
  }
  return 0;
}

int main(void) {
  const char *lim = getenv("CUDA_CORE_LIMIT");
  if (getenv("LD_PRELOAD") == NULL || lim == NULL || atoi(lim) <= 0 || atoi(lim) >= 100) {
    printf("SKIP: needs the library preloaded and CUDA_CORE_LIMIT under 100 (the idle-gap path)\n");
    return 77;
  }
  CUdevice dev;
  CUcontext ctx;
  CUstream s;
  if (cuInit(0) != CUDA_SUCCESS || cuDeviceGet(&dev, 0) != CUDA_SUCCESS ||
      cuDevicePrimaryCtxRetain(&ctx, dev) != CUDA_SUCCESS ||
      cuCtxSetCurrent(ctx) != CUDA_SUCCESS || cuStreamCreate(&s, CU_STREAM_NON_BLOCKING) != CUDA_SUCCESS) {
    printf("FAIL: no context\n");
    return 1;
  }
  CUdeviceptr dflag = 0;
  if (cuMemHostAlloc((void **)&g_flag, sizeof(*g_flag), CU_MEMHOSTALLOC_DEVICEMAP) != CUDA_SUCCESS ||
      cuMemHostGetDevicePointer(&dflag, (void *)g_flag, 0) != CUDA_SUCCESS) {
    printf("FAIL: no mapped host memory\n");
    return 1;
  }

  launcher_t graph = {"graph launch", NULL, NULL}, kernel = {"kernel launch", NULL, NULL};
  CUgraph g = NULL;
  CUgraphNode n = NULL;
  if (cuGraphCreate(&g, 0) != CUDA_SUCCESS || cuGraphAddEmptyNode(&n, g, NULL, 0) != CUDA_SUCCESS ||
      cuGraphInstantiateWithFlags(&graph.exec, g, 0) != CUDA_SUCCESS) {
    printf("FAIL: could not build the graph\n");
    return 1;
  }
  CUmodule mod = NULL;
  int have_kernel = cuModuleLoadData(&mod, k_ptx) == CUDA_SUCCESS &&
                    cuModuleGetFunction(&kernel.fn, mod, "noop") == CUDA_SUCCESS;

  printf("[A] a launch after an idle gap does not wait for a stream waiting on the host\n");
  int failed = 0, checked = 0;
  int rg = one(&graph, s, dflag);
  failed += rg == 1;
  checked += rg == 0;
  if (have_kernel) {
    int rk = one(&kernel, s, dflag);
    failed += rk == 1;
    checked += rk == 0;
  } else {
    printf("  kernel launch: the driver refused the PTX, not checked\n");
  }
  if (failed) {
    printf("Result: FAIL\n");
    return 1;
  }
  if (!checked) {
    printf("Result: SKIP (nothing engaged the idle-gap path)\n");
    return 77;
  }
  printf("Result: PASS\n");
  return 0;
}
