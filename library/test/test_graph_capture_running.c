/*
 * A graph running on one stream keeps its pool memory while another thread holds a
 * capture open in GLOBAL mode, and that capture is left intact.
 *
 * With a capture open the library may not query events under the thread's default
 * mode -- that breaks another thread's GLOBAL capture (docs 6.15) -- and so it counted
 * every running launch finished: a launch on another stream was let take the running
 * graph's pool memory, which the driver cannot reuse, so it mapped new pages past the
 * limit. It now queries switched to RELAXED, which leaves the capture alone (docs 6.20).
 *
 *   [A] G1 held running on s1 (behind a wait on the host), thread T capturing in GLOBAL
 *       mode: G2, the same size, on s2 is refused; T's capture ends well
 *   [B] once G1 finished, G2 goes through, the capture still open
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so CUDA_MEM_LIMIT=4096m ./test_graph_capture_running
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

static CUcontext g_ctx;
static CUdevice g_dev;

static pthread_mutex_t g_m = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t g_cv = PTHREAD_COND_INITIALIZER;
static int g_stage;
static CUresult g_begin = CUDA_ERROR_UNKNOWN, g_end = CUDA_ERROR_UNKNOWN;
static CUdeviceptr g_scratch;

static void set_stage(int s) {
  pthread_mutex_lock(&g_m);
  g_stage = s;
  pthread_cond_broadcast(&g_cv);
  pthread_mutex_unlock(&g_m);
}

static void wait_stage(int s) {
  pthread_mutex_lock(&g_m);
  while (g_stage < s) pthread_cond_wait(&g_cv, &g_m);
  pthread_mutex_unlock(&g_m);
}

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

/* Holds a GLOBAL capture open on a stream of its own until stage 2. */
static void *capturer(void *arg) {
  (void)arg;
  cuCtxSetCurrent(g_ctx);
  CUstream s;
  if (cuStreamCreate(&s, CU_STREAM_NON_BLOCKING) != CUDA_SUCCESS) {
    set_stage(1);
    return NULL;
  }
  g_begin = cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_GLOBAL);
  if (g_begin == CUDA_SUCCESS) cuMemsetD8Async(g_scratch, 0, 1, s);
  set_stage(1);
  wait_stage(2);
  if (g_begin == CUDA_SUCCESS) {
    CUgraph g = NULL;
    g_end = cuStreamEndCapture(s, &g);
    if (g != NULL) cuGraphDestroy(g);
  }
  cuStreamDestroy(s);
  return NULL;
}

static CUgraphExec captured_exec(CUstream s, size_t bytes) {
  CUgraph g = NULL;
  CUdeviceptr d = 0;
  if (cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_RELAXED) != CUDA_SUCCESS) return NULL;
  CUresult a = cuMemAllocAsync(&d, bytes, s);
  if (a == CUDA_SUCCESS) {
    cuMemsetD8Async(d, 1, bytes, s);
    cuMemFreeAsync(d, s);
  }
  cuStreamEndCapture(s, &g);
  if (a != CUDA_SUCCESS || g == NULL) {
    describe("cuMemAllocAsync (in capture)", a);
    if (g != NULL) cuGraphDestroy(g);
    return NULL;
  }
  CUgraphExec ex = NULL;
  CUresult r = cuGraphInstantiateWithFlags(&ex, g, 0);
  cuGraphDestroy(g);
  return r == CUDA_SUCCESS ? ex : NULL;
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
  CHECK_DRV_API(cuDevicePrimaryCtxRetain(&g_ctx, g_dev));
  CHECK_DRV_API(cuCtxSetCurrent(g_ctx));
  CUstream s1, s2;
  CHECK_DRV_API(cuStreamCreate(&s1, CU_STREAM_NON_BLOCKING));
  CHECK_DRV_API(cuStreamCreate(&s2, CU_STREAM_NON_BLOCKING));
  volatile unsigned *flag = NULL;
  CUdeviceptr dflag = 0;
  CHECK_DRV_API(cuMemHostAlloc((void **)&flag, sizeof(*flag), CU_MEMHOSTALLOC_DEVICEMAP));
  CHECK_DRV_API(cuMemHostGetDevicePointer(&dflag, (void *)flag, 0));
  CHECK_DRV_API(cuMemAlloc(&g_scratch, 4096));
  cuCtxSynchronize();
  cuDeviceGraphMemTrim(g_dev);

  size_t need = headroom() / 3;
  CUgraphExec g1 = captured_exec(s1, need), g2 = captured_exec(s2, need);
  if (g1 == NULL || g2 == NULL) {
    printf("FAIL: could not build the graphs\n");
    return 1;
  }
  /* Both streams seen: launches from here on are tracked with events. */
  CHECK_DRV_API(cuGraphLaunch(g1, s1));
  CHECK_DRV_API(cuStreamSynchronize(s1));
  CHECK_DRV_API(cuGraphLaunch(g2, s2));
  CHECK_DRV_API(cuStreamSynchronize(s2));
  CUdeviceptr filler = 0;
  size_t room = headroom();
  if (room > need / 2) CHECK_DRV_API(cuMemAlloc(&filler, room - need / 2));
  printf("graph needs %.0f MiB; headroom %.0f MiB\n", MIB(need), MIB(headroom()));

  int failed = 0;
  printf("[A] G1 running on s1, a GLOBAL capture open: G2 on s2 is refused, the capture intact\n");
  *flag = 0;
  CHECK_DRV_API(cuStreamWaitValue32(s1, dflag, 1, CU_STREAM_WAIT_VALUE_GEQ));
  CUresult r = cuGraphLaunch(g1, s1);
  describe("cuGraphLaunch G1 (s1 held)", r);
  if (r != CUDA_SUCCESS) failed = 1;
  pthread_t th;
  pthread_create(&th, NULL, capturer, NULL);
  wait_stage(1);
  describe("cuStreamBeginCapture GLOBAL (other thread)", g_begin);
  if (g_begin != CUDA_SUCCESS) failed = 1;
  r = cuGraphLaunch(g2, s2);
  describe("cuGraphLaunch G2 (G1 still running)", r);
  if (r == CUDA_SUCCESS) {
    printf("  FAIL: G2 was given G1's pool memory while G1 runs: mapped past the limit\n");
    failed = 1;
  } else if (r != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  FAIL: expected CUDA_ERROR_OUT_OF_MEMORY\n");
    failed = 1;
  }

  printf("[B] once G1 finished, G2 goes through with the capture still open\n");
  /* No synchronize while the other thread captures in GLOBAL mode: that is the
   * application breaking the capture. G1 takes milliseconds once let go. */
  *flag = 1;
  usleep(500 * 1000);
  r = cuGraphLaunch(g2, s2);
  describe("cuGraphLaunch G2 (G1 finished)", r);
  if (r != CUDA_SUCCESS) {
    printf("  FAIL: refused although G1 finished\n");
    failed = 1;
  }
  set_stage(2);
  pthread_join(th, NULL);
  cuCtxSynchronize();
  describe("cuStreamEndCapture GLOBAL (other thread)", g_end);
  if (g_begin == CUDA_SUCCESS && g_end != CUDA_SUCCESS) {
    printf("  FAIL: the library broke the other thread's capture\n");
    failed = 1;
  }

  if (filler) cuMemFree(filler);
  cuGraphExecDestroy(g1);
  cuGraphExecDestroy(g2);
  cuMemFree(g_scratch);
  cuMemFreeHost((void *)flag);
  cuCtxSynchronize();
  cuDeviceGraphMemTrim(g_dev);
  return vgpu_test_verdict(failed, 0);
}
