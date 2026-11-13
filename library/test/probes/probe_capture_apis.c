/*
 * Which stream queries a capture survives, on this driver. The library depends on the
 * answer: it never asks the driver for a stream's device during that stream's capture,
 * because cuStreamGetCtx (driver 550) or cuStreamGetDevice (570 and newer) fails there
 * and has invalidated the capture by then. For another stream it goes through
 * cuStreamGetCtx and a push of the context, which has to stay safe.
 *
 * Each query gets a capture of its own, so none is blamed for another. On one device a
 * second context stands in for the other device. Prints, and fails only if a query that
 * must be safe breaks a capture. run_probes.sh also runs it under the library and
 * compares: the library must not change what a capture survives.
 *
 * Usage: probe_capture_apis
 */
#include <cuda.h>
#include <dlfcn.h>
#include <stdio.h>

#include "test_utils.h"

typedef CUresult (*get_device_t)(CUstream, CUdevice *);

static CUstream g_s1;
static int g_broken;

static void begin(void) {
  CUresult r = cuStreamBeginCapture(g_s1, CU_STREAM_CAPTURE_MODE_GLOBAL);
  if (r != CUDA_SUCCESS) printf("cuStreamBeginCapture -> %d\n", (int)r);
}

static CUresult end(const char *what, CUresult query) {
  CUgraph g = NULL;
  CUresult e = cuStreamEndCapture(g_s1, &g);
  printf("  %-42s query -> %4d | end -> %d%s\n", what, (int)query, (int)e,
         e == CUDA_SUCCESS ? "" : "  (capture broken)");
  if (g != NULL) cuGraphDestroy(g);
  cuStreamSynchronize(g_s1);
  return e;
}

int main(void) {
  CUdevice d0, d1, dev;
  CUcontext c0, c1, got, popped;
  CUstream s0;
  CUdeviceptr p;
  int version = 0, count = 0;
  CUresult r;
  cuInit(0);
  cuDriverGetVersion(&version);
  cuDeviceGetCount(&count);
  cuDeviceGet(&d0, 0);
  cuDevicePrimaryCtxRetain(&c0, d0);
  if (count > 1) {
    cuDeviceGet(&d1, 1);
    cuDevicePrimaryCtxRetain(&c1, d1);
  } else {
    CUCTX_CREATE(&c1, 0, d0);
    cuCtxPopCurrent(&popped);
  }
  cuCtxSetCurrent(c1);
  cuStreamCreate(&g_s1, 0);
  cuCtxSetCurrent(c0);
  cuStreamCreate(&s0, 0);

  void *lib = dlopen("libcuda.so.1", RTLD_NOW);
  get_device_t get_device = lib != NULL ? (get_device_t)dlsym(lib, "cuStreamGetDevice") : NULL;
  printf("driver CUDA %d, cuStreamGetDevice %s\n", version, get_device != NULL ? "present" : "absent");

  r = cuStreamGetCtx(g_s1, &got);
  printf("not capturing: cuStreamGetCtx -> %d, the stream's context: %d\n", (int)r, got == c1);
  printf("capturing a stream of the other context:\n");

  begin();
  g_broken += end("nothing", CUDA_SUCCESS) != CUDA_SUCCESS;
  begin();
  end("cuStreamGetCtx", cuStreamGetCtx(g_s1, &got));
  if (get_device != NULL) {
    begin();
    end("cuStreamGetDevice", get_device(g_s1, &dev));
  }
  begin();
  g_broken += end("cuStreamGetCtx(another, idle stream)", cuStreamGetCtx(s0, &got)) != CUDA_SUCCESS;
  if (get_device != NULL) {
    /* The library's fallback only, for when the above fails. */
    begin();
    end("cuStreamGetDevice(another, idle stream)", get_device(s0, &dev));
  }
  begin();
  r = cuCtxPushCurrent(c1);
  if (r == CUDA_SUCCESS) r = cuCtxGetDevice(&dev);
  cuCtxPopCurrent(&popped);
  g_broken += end("push + cuCtxGetDevice + pop", r) != CUDA_SUCCESS;
  cuCtxSetCurrent(c0);
  begin();
  g_broken += end("cuCtxGetDevice", cuCtxGetDevice(&dev)) != CUDA_SUCCESS;
  /* What the library asks while checking an allocation in a capture. */
  {
    cuuint64_t reserved = 0;
    begin();
    g_broken += end("cuDeviceGetGraphMemAttribute",
                    cuDeviceGetGraphMemAttribute(d0, CU_GRAPH_MEM_ATTR_RESERVED_MEM_CURRENT, &reserved)) != CUDA_SUCCESS;
  }
  /* What goes through the library's stream lookups. */
  begin();
  r = cuMemAllocAsync(&p, 1u << 20, s0);
  end("cuMemAllocAsync(another, idle stream)", r);
  if (r == CUDA_SUCCESS) {
    cuStreamSynchronize(s0);
    cuMemFreeAsync(p, s0);
    cuStreamSynchronize(s0);
  }
  begin();
  r = cuMemAllocAsync(&p, 1u << 20, g_s1);
  if (r == CUDA_SUCCESS) r = cuMemFreeAsync(p, g_s1);
  g_broken += end("cuMemAllocAsync(the stream in capture)", r) != CUDA_SUCCESS;
  return g_broken != 0;
}
