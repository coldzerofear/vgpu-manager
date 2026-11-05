/*
 * A graph launch is charged to the device the exec lives on, not the current
 * context's. Run with CUDA_CORE_LIMIT_0=10 and CUDA_CORE_LIMIT_1=100: a graph on a
 * stream of device 0 stays throttled with device 1 current. Before the fix it went
 * from about 15000 to 110000 launches a second. Needs two devices.
 *
 * A kernel launch has no such hole: the driver refuses a stream of another device.
 *
 * Usage: probe_launch_device
 */
#include <cuda_runtime.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

__global__ void kernel(int *p) { (void)p; }

#define CHECK(call)                                                                 \
  do {                                                                              \
    cudaError_t _e = (call);                                                        \
    if (_e != cudaSuccess) {                                                        \
      printf("%s -> %d %s\n", #call, (int)_e, cudaGetErrorString(_e));              \
      exit(1);                                                                      \
    }                                                                               \
  } while (0)

static double now() {
  timespec t;
  clock_gettime(CLOCK_MONOTONIC, &t);
  return t.tv_sec + t.tv_nsec * 1e-9;
}

static double rate(int current, cudaGraphExec_t e, cudaStream_t s) {
  CHECK(cudaSetDevice(current));
  double start = now();
  long n = 0;
  while (now() - start < 5) {
    CHECK(cudaGraphLaunch(e, s));
    n++;
  }
  CHECK(cudaStreamSynchronize(s));
  return n / (now() - start);
}

int main() {
  int count = 0, *buf;
  cudaStream_t s;
  cudaGraph_t g;
  cudaGraphExec_t e;
  CHECK(cudaGetDeviceCount(&count));
  if (count < 2) {
    printf("needs two devices\n");
    return 77;
  }
  CHECK(cudaSetDevice(0));
  CHECK(cudaMalloc(&buf, 4096));
  CHECK(cudaStreamCreate(&s));
  CHECK(cudaStreamBeginCapture(s, cudaStreamCaptureModeRelaxed));
  kernel<<<4096, 256, 0, s>>>(buf);
  CHECK(cudaStreamEndCapture(s, &g));
  CHECK(cudaGraphInstantiateWithFlags(&e, g, 0));
  double a = rate(0, e, s), b = rate(1, e, s), c = rate(0, e, s);
  printf("graph on a stream of device 0: current 0 %.0f, current 1 %.0f, current 0 %.0f launches/s\n", a, b, c);
  return 0;
}
