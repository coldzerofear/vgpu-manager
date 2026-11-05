/*
 * Which stream queries a capture survives, on this driver, without the library.
 * The library depends on the answer: it never asks the driver for a stream's device
 * during a capture, because on a stream in capture cuStreamGetCtx (driver 550) or
 * cuStreamGetDevice (570, 580) fails and has invalidated the capture by then.
 *
 * Each query gets a capture of its own, so none is blamed for another. Needs two
 * devices. Prints, and fails only if a query that must be safe breaks a capture.
 *
 * Usage: probe_capture_apis
 */
#include <cuda.h>
#include <dlfcn.h>
#include <stdio.h>

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
  printf("  %-36s query -> %4d | end -> %d%s\n", what, (int)query, (int)e,
         e == CUDA_SUCCESS ? "" : "  (capture broken)");
  if (g != NULL) cuGraphDestroy(g);
  cuStreamSynchronize(g_s1);
  return e;
}

int main(void) {
  CUdevice d0, d1, dev;
  CUcontext c0, c1, got, popped;
  CUstream s0;
  int version = 0, count = 0;
  CUresult r;
  cuInit(0);
  cuDriverGetVersion(&version);
  cuDeviceGetCount(&count);
  if (count < 2) {
    printf("needs two devices\n");
    return 77;
  }
  cuDeviceGet(&d0, 0);
  cuDeviceGet(&d1, 1);
  cuDevicePrimaryCtxRetain(&c0, d0);
  cuDevicePrimaryCtxRetain(&c1, d1);
  cuCtxSetCurrent(c1);
  cuStreamCreate(&g_s1, 0);
  cuCtxSetCurrent(c0);
  cuStreamCreate(&s0, 0);

  void *lib = dlopen("libcuda.so.1", RTLD_NOW);
  get_device_t get_device = lib != NULL ? (get_device_t)dlsym(lib, "cuStreamGetDevice") : NULL;
  printf("driver CUDA %d, cuStreamGetDevice %s\n", version, get_device != NULL ? "present" : "absent");

  r = cuStreamGetCtx(g_s1, &got);
  printf("not capturing: cuStreamGetCtx -> %d, the stream's context: %d\n", (int)r, got == c1);
  printf("capturing a stream of device 1, current context on device 0:\n");

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
    /* Unknown so far, so the library avoids it while a capture is open. */
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
  return g_broken != 0;
}
