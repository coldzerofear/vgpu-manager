/*
 * A graph launch belongs to the device the exec lives on, not the current context's.
 * Run with CUDA_CORE_LIMIT_0=10 and CUDA_CORE_LIMIT_1=100. Needs two devices.
 *
 *   gap      The first launch ever is made with device 1 current. The gap events must
 *            not end up in its context: a launch after an idle gap, with device 0
 *            current, still waits out its duty cycle.
 *   activity A launch with device 1 current counts as activity on device 0: the launch
 *            right after it is not one after an idle gap.
 *   rates    A graph on a stream of device 0 stays throttled with device 1 current,
 *            also after an update under device 1.
 *
 * The kernel is sized here to take about 20 ms: an empty one shows little utilization
 * on a fast card, and its launch rate says nothing about the limit. Run with both limits
 * at 100 to see the rate to compare with.
 *
 * A kernel launch has no such hole: the driver refuses a stream of another device.
 *
 * Usage: probe_launch_device
 */
#include <cuda_runtime.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>
#include <unistd.h>

__global__ void kernel(unsigned *p, int n) {
  unsigned x = threadIdx.x + blockIdx.x;
  for (int i = 0; i < n; i++) x = x * 1664525u + 1013904223u;
  p[threadIdx.x] = x;
}

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

/* Waited for one by one: a device left unthrottled would queue minutes of work. */
static double rate(int current, cudaGraphExec_t e, cudaStream_t s) {
  CHECK(cudaSetDevice(current));
  double start = now();
  long n = 0;
  while (now() - start < 5) {
    CHECK(cudaGraphLaunch(e, s));
    CHECK(cudaStreamSynchronize(s));
    n++;
  }
  return n / (now() - start);
}

static double timed_launch_ms(cudaGraphExec_t e, cudaStream_t s) {
  double start = now();
  CHECK(cudaGraphLaunch(e, s));
  double ms = (now() - start) * 1000;
  CHECK(cudaStreamSynchronize(s));
  return ms;
}

/* On device 1, which has no limit: on device 0 the throttling's sleeps would count, by
 * any clock. After a first launch of the same size, which also loads the module and sets
 * up the kernel's resources. */
static int size_kernel(double *kernel_ms) {
  unsigned *buf;
  cudaStream_t s;
  CHECK(cudaSetDevice(1));
  CHECK(cudaMalloc(&buf, 256 * sizeof(unsigned)));
  CHECK(cudaStreamCreate(&s));
  kernel<<<4096, 256, 0, s>>>(buf, 1);
  CHECK(cudaStreamSynchronize(s));
  int n = 1000;
  for (; n < (1 << 28); n *= 2) {
    double start = now();
    kernel<<<4096, 256, 0, s>>>(buf, n);
    CHECK(cudaStreamSynchronize(s));
    *kernel_ms = (now() - start) * 1000;
    if (*kernel_ms >= 20) break;
  }
  CHECK(cudaStreamDestroy(s));
  CHECK(cudaFree(buf));
  return n;
}

static const useconds_t kPastGapUs = 300 * 1000;   /* the library's gap threshold is 200 ms */

int main() {
  int count = 0, failed = 0, n;
  unsigned *buf;
  cudaStream_t s;
  cudaGraph_t g;
  cudaGraphExec_t e;
  double kernel_ms = 0;
  CHECK(cudaGetDeviceCount(&count));
  if (count < 2) {
    printf("needs two devices\n");
    return 77;
  }
  n = size_kernel(&kernel_ms);
  CHECK(cudaSetDevice(0));
  CHECK(cudaMalloc(&buf, 256 * sizeof(unsigned)));
  CHECK(cudaStreamCreate(&s));
  CHECK(cudaStreamBeginCapture(s, cudaStreamCaptureModeRelaxed));
  kernel<<<4096, 256, 0, s>>>(buf, n);
  CHECK(cudaStreamEndCapture(s, &g));
  CHECK(cudaGraphInstantiateWithFlags(&e, g, 0));
  printf("kernel: %.1f ms\n", kernel_ms);

  /* First of all, while the gap events do not exist yet. */
  CHECK(cudaSetDevice(1));
  CHECK(cudaGraphLaunch(e, s));
  CHECK(cudaStreamSynchronize(s));
  CHECK(cudaSetDevice(0));
  double idle = 1e9, busy = 1e9;
  for (int i = 0; i < 3; i++) {
    usleep(kPastGapUs);
    double ms = timed_launch_ms(e, s);
    if (ms < idle) idle = ms;
  }
  /* The fastest: any one of these can run into the token bucket's sleep. */
  for (int i = 0; i < 5; i++) {
    usleep(kPastGapUs);
    CHECK(cudaSetDevice(1));
    CHECK(cudaGraphLaunch(e, s));
    CHECK(cudaStreamSynchronize(s));
    CHECK(cudaSetDevice(0));
    double ms = timed_launch_ms(e, s);
    if (ms < busy) busy = ms;
  }
  printf("launch call: after an idle gap %.3f ms, right after a launch from device 1 %.3f ms\n", idle, busy);

  double a = rate(0, e, s), b = rate(1, e, s), c = rate(0, e, s);
  printf("graph on a stream of device 0: current 0 %.1f, current 1 %.1f, current 0 %.1f launches/s\n", a, b, c);
  if (b > 3 * a) {
    printf("FAIL: not throttled with device 1 current\n");
    failed = 1;
  }
  CHECK(cudaSetDevice(1));
  cudaError_t u = cudaGraphExecUpdate(e, g, NULL);
  printf("cudaGraphExecUpdate with device 1 current -> %d\n", (int)u);
  if (u == cudaSuccess) {
    double d = rate(1, e, s);
    printf("after that update: current 1 %.1f launches/s\n", d);
    if (d > 3 * a) {
      printf("FAIL: the update moved the exec to device 1\n");
      failed = 1;
    }
  }
  /* Only meaningful with a limit on device 0; at 100 this is the rate to compare with. */
  const char *limit = getenv("CUDA_CORE_LIMIT_0");
  if (limit != NULL && atoi(limit) < 100) {
    if (idle < 2 * kernel_ms) {
      printf("FAIL: no duty cycle after an idle gap: the gap events were lost to device 1\n");
      failed = 1;
    }
    if (busy > idle / 3) {
      printf("FAIL: a launch from device 1 did not count as activity on device 0\n");
      failed = 1;
    }
  }
  return failed;
}
