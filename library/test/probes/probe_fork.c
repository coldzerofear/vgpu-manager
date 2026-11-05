/*
 * Fork in a loop while other threads keep changing all three tables, one of them
 * growing and emptying the graph cost table. Each child, and a grandchild, goes
 * through the hooks that use the tables. In a child of a process that has used CUDA
 * the driver refuses these calls; what matters is that the hooks neither hang on a
 * lock inherited mid-update nor crash on a table inherited mid-growth.
 *
 * Usage: probe_fork N
 */
#include <cuda.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

enum { KEEP_ALIVE = 800 };

static CUdevice g_dev;
static CUcontext g_ctx;
static CUstream g_stream;
static CUgraphExec g_exec;
static volatile int g_stop;

static int make_exec(CUgraphExec *e) {
  CUgraph g;
  CUDA_MEM_ALLOC_NODE_PARAMS a;
  CUgraphNode alloc, freed;
  memset(&a, 0, sizeof(a));
  a.poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  a.poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  a.poolProps.location.id = g_dev;
  a.bytesize = 1 << 20;
  if (cuGraphCreate(&g, 0) != CUDA_SUCCESS) return 0;
  int ok = cuGraphAddMemAllocNode(&alloc, g, NULL, 0, &a) == CUDA_SUCCESS &&
           cuGraphAddMemFreeNode(&freed, g, &alloc, 1, a.dptr) == CUDA_SUCCESS &&
           cuGraphInstantiateWithFlags(e, g, 0) == CUDA_SUCCESS;
  cuGraphDestroy(g);
  return ok;
}

static void *churn_execs(void *arg) {
  (void)arg;
  cuCtxSetCurrent(g_ctx);
  while (!g_stop) {
    CUgraphExec e;
    if (make_exec(&e)) cuGraphExecDestroy(e);
  }
  return NULL;
}

static void *grow_and_empty(void *arg) {
  static CUgraphExec keep[KEEP_ALIVE];
  int n = 0;
  (void)arg;
  cuCtxSetCurrent(g_ctx);
  while (!g_stop) {
    if (n < KEEP_ALIVE) {
      if (make_exec(&keep[n])) n++;
    } else {
      while (n > 0) cuGraphExecDestroy(keep[--n]);
    }
  }
  return NULL;
}

static void *churn_captures(void *arg) {
  CUstream s;
  (void)arg;
  cuCtxSetCurrent(g_ctx);
  cuStreamCreate(&s, 0);
  while (!g_stop) {
    CUgraph g = NULL;
    cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_RELAXED);
    cuStreamEndCapture(s, &g);
    if (g != NULL) cuGraphDestroy(g);
  }
  return NULL;
}

static void *churn_pools(void *arg) {
  CUmemPoolProps props;
  (void)arg;
  cuCtxSetCurrent(g_ctx);
  memset(&props, 0, sizeof(props));
  props.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  props.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  props.location.id = g_dev;
  while (!g_stop) {
    CUmemoryPool p;
    if (cuMemPoolCreate(&p, &props) == CUDA_SUCCESS) cuMemPoolDestroy(p);
  }
  return NULL;
}

static int child_hooks(int depth) {
  CUmemPoolProps props;
  CUmemoryPool pool = NULL;
  CUgraph g = NULL;
  CUdevice d;
  cuGraphExecDestroy(g_exec);
  cuStreamEndCapture(g_stream, NULL);
  memset(&props, 0, sizeof(props));
  props.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  props.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  cuMemPoolCreate(&pool, &props);
  cuGraphCreate(&g, 0);
  cuCtxGetDevice(&d);
  if (depth > 0) {
    pid_t p = fork();
    if (p == 0) _exit(child_hooks(depth - 1));
    int status = 0;
    waitpid(p, &status, 0);
    return WIFEXITED(status) ? WEXITSTATUS(status) : 99;
  }
  return 0;
}

int main(int argc, char **argv) {
  int n = argc > 1 ? atoi(argv[1]) : 60, hung = 0, bad = 0;
  CUgraph g = NULL;
  CUdeviceptr p;
  cuInit(0);
  cuDeviceGet(&g_dev, 0);
  cuDevicePrimaryCtxRetain(&g_ctx, g_dev);
  cuCtxSetCurrent(g_ctx);
  /* Real handles for the children to pass: a made-up one crashes inside the driver. */
  cuStreamCreate(&g_stream, 0);
  cuStreamBeginCapture(g_stream, CU_STREAM_CAPTURE_MODE_RELAXED);
  cuMemAllocAsync(&p, 1 << 20, g_stream);
  cuMemFreeAsync(p, g_stream);
  cuStreamEndCapture(g_stream, &g);
  cuGraphInstantiateWithFlags(&g_exec, g, 0);
  cuGraphDestroy(g);

  void *(*workers[])(void *) = {churn_execs, grow_and_empty, churn_captures, churn_pools};
  enum { WORKERS = sizeof(workers) / sizeof(workers[0]) };
  pthread_t threads[WORKERS];
  for (int i = 0; i < WORKERS; i++) pthread_create(&threads[i], NULL, workers[i], NULL);
  for (int i = 0; i < n; i++) {
    usleep(20000);
    pid_t child = fork();
    if (child == 0) {
      alarm(30);
      _exit(child_hooks(1));
    }
    int status = 0;
    waitpid(child, &status, 0);
    if (WIFSIGNALED(status)) {
      if (WTERMSIG(status) == SIGALRM) hung++;
      else bad++;
    } else if (WEXITSTATUS(status) != 0) {
      bad++;
    }
  }
  g_stop = 1;
  for (int i = 0; i < WORKERS; i++) pthread_join(threads[i], NULL);
  printf("fork loop: %d forks, %d hung, %d bad\n", n, hung, bad);
  return hung != 0 || bad != 0;
}
