/*
 * A graph is charged to the core limit however many graphs live. N graphs with a real
 * kernel, instantiated while another thread keeps launching, so the graph cost table
 * grows under load; then launches per second of the first and the last, alternating.
 * Run under CUDA_CORE_LIMIT: all should settle at about the same rate, far below the
 * rate without the library. An uncharged graph shows as several times the rate of a
 * charged one, which then pays the capacity back.
 *
 * Usage: probe_graph_throttle N
 */
#include <cuda_runtime.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

__global__ void kernel(int *p) { (void)p; }

#define CHECK(call)                                                                 \
  do {                                                                              \
    cudaError_t _e = (call);                                                        \
    if (_e != cudaSuccess) {                                                        \
      printf("%s -> %d %s\n", #call, (int)_e, cudaGetErrorString(_e));              \
      exit(1);                                                                      \
    }                                                                               \
  } while (0)

static volatile int g_stop = 0;
static long g_background = 0;
static cudaGraphExec_t g_first;
static int *g_buf;

static double now() {
  timespec t;
  clock_gettime(CLOCK_MONOTONIC, &t);
  return t.tv_sec + t.tv_nsec * 1e-9;
}

static cudaGraphExec_t make_exec(cudaStream_t s) {
  cudaGraph_t g;
  cudaGraphExec_t e;
  CHECK(cudaStreamBeginCapture(s, cudaStreamCaptureModeRelaxed));
  kernel<<<4096, 256, 0, s>>>(g_buf);
  CHECK(cudaStreamEndCapture(s, &g));
  CHECK(cudaGraphInstantiateWithFlags(&e, g, 0));
  CHECK(cudaGraphDestroy(g));
  return e;
}

static void *launcher(void *) {
  cudaStream_t s;
  CHECK(cudaSetDevice(0));
  CHECK(cudaStreamCreate(&s));
  while (!g_stop) {
    CHECK(cudaGraphLaunch(g_first, s));
    g_background++;
  }
  CHECK(cudaStreamSynchronize(s));
  return nullptr;
}

static double rate(cudaGraphExec_t e, cudaStream_t s) {
  double start = now();
  long n = 0;
  while (now() - start < 4) {
    CHECK(cudaGraphLaunch(e, s));
    n++;
  }
  CHECK(cudaStreamSynchronize(s));
  return n / (now() - start);
}

int main(int argc, char **argv) {
  int n = argc > 1 ? atoi(argv[1]) : 6000;
  cudaStream_t s;
  CHECK(cudaSetDevice(0));
  CHECK(cudaMalloc(&g_buf, 4096));
  CHECK(cudaStreamCreate(&s));
  g_first = make_exec(s);

  pthread_t t;
  pthread_create(&t, nullptr, launcher, nullptr);
  double start = now();
  cudaGraphExec_t last = g_first;
  for (int i = 1; i < n; i++) last = make_exec(s);
  double took = now() - start;
  g_stop = 1;
  pthread_join(t, nullptr);
  printf("N=%d instantiated in %.2f s with %ld launches going on\n", n, took, g_background);

  double a = rate(g_first, s), b = rate(last, s), c = rate(g_first, s), d = rate(last, s);
  printf("first %.0f, last %.0f, first %.0f, last %.0f launches/s\n", a, b, c, d);

  pid_t child = fork();
  if (child == 0) _exit(0);
  int status = 0;
  alarm(20);
  waitpid(child, &status, 0);
  alarm(0);
  CHECK(cudaGraphLaunch(last, s));
  CHECK(cudaStreamSynchronize(s));
  printf("after a fork the parent still launches\n");
  return 0;
}
