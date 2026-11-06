/*
 * The driver gives idle graph-pool pages back on its own when a cuMemAlloc outgrows
 * the free device memory, with no notification. A tenant then allocates into what
 * that freed, relaunches its graph, and must be refused. A cached "already mapped"
 * skip would remap past the limit.
 *
 * Two processes sharing a directory for synchronization: A with the library, and B
 * without it, standing in for another tenant filling the card. run_probes.sh runs
 * them, with per-process accounting so that only A's memory counts against A's limit.
 *
 * Usage: probe_reclaim A DIR | probe_reclaim B DIR LEAVE_MIB
 */
#include <cuda.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define MIB(x) ((double)(x) / 1048576.0)
#define CHECK(call)                                                            \
  do {                                                                         \
    CUresult _r = (call);                                                      \
    if (_r != CUDA_SUCCESS) {                                                  \
      printf("%s -> %d\n", #call, (int)_r);                                    \
      exit(2);                                                                 \
    }                                                                          \
  } while (0)

static char g_dir[256];
static CUdevice g_dev;

static void post(const char *name) {
  char path[300];
  snprintf(path, sizeof(path), "%s/%s", g_dir, name);
  FILE *f = fopen(path, "w");
  if (f != NULL) fclose(f);
}

/* Bounded: the other process may have died, and then nobody posts this. */
static void wait_for(const char *name) {
  char path[300];
  snprintf(path, sizeof(path), "%s/%s", g_dir, name);
  for (int i = 0; access(path, F_OK) != 0; i++) {
    if (i == 15000) {
      printf("timed out waiting for %s\n", name);
      exit(2);
    }
    usleep(10000);
  }
}

static size_t pool_reserved(void) {
  cuuint64_t v = 0;
  cuDeviceGetGraphMemAttribute(g_dev, CU_GRAPH_MEM_ATTR_RESERVED_MEM_CURRENT, &v);
  return (size_t)v;
}

static int other_tenant(size_t leave) {
  size_t freeb, total;
  CUdeviceptr d;
  wait_for("fill");
  CHECK(cuMemGetInfo(&freeb, &total));
  CHECK(cuMemAlloc(&d, freeb - leave));
  printf("B: took %.0f MiB, leaving %.0f MiB\n", MIB(freeb - leave), MIB(leave));
  post("filled");
  wait_for("release");
  cuMemFree(d);
  post("released");
  return 0;
}

static int tenant(void) {
  const size_t need = 1200ull << 20;
  CUstream s;
  CUgraph g;
  CUgraphExec ex;
  CUdeviceptr d;
  size_t freeb, total;
  CHECK(cuStreamCreate(&s, 0));
  CHECK(cuStreamBeginCapture(s, CU_STREAM_CAPTURE_MODE_RELAXED));
  CHECK(cuMemAllocAsync(&d, need, s));
  CHECK(cuMemsetD8Async(d, 1, need, s));
  CHECK(cuMemFreeAsync(d, s));
  CHECK(cuStreamEndCapture(s, &g));
  CHECK(cuGraphInstantiateWithFlags(&ex, g, 0));
  CHECK(cuGraphLaunch(ex, s));
  CHECK(cuStreamSynchronize(s));
  CHECK(cuMemGetInfo(&freeb, &total));
  printf("A: launched, pool %.0f MiB, view %.0f / %.0f MiB free\n", MIB(pool_reserved()), MIB(freeb), MIB(total));

  post("fill");
  wait_for("filled");
  CUdeviceptr squeezed = 0;
  CUresult r = cuMemAlloc(&squeezed, need);
  size_t after_squeeze = pool_reserved();
  printf("A: cuMemAlloc(%.0f MiB) under pressure -> %d, pool now %.0f MiB\n", MIB(need), (int)r, MIB(after_squeeze));
  post("release");
  wait_for("released");

  CHECK(cuMemGetInfo(&freeb, &total));
  CUdeviceptr rest = 0;
  CHECK(cuMemAlloc(&rest, freeb - (64u << 20)));
  r = cuGraphLaunch(ex, s);
  cuStreamSynchronize(s);
  printf("A: relaunch with the headroom taken -> %d, pool %.0f MiB\n", (int)r, MIB(pool_reserved()));
  if (after_squeeze != 0) {
    printf("A: the driver did not reclaim the pool here: nothing proved\n");
    return 77;
  }
  if (r != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("FAIL: the graph was remapped past the limit\n");
    return 1;
  }
  return 0;
}

int main(int argc, char **argv) {
  if (argc < 3 || (argv[1][0] == 'B' && argc < 4)) {
    printf("usage: %s A DIR | %s B DIR LEAVE_MIB\n", argv[0], argv[0]);
    return 2;
  }
  CUcontext ctx;
  snprintf(g_dir, sizeof(g_dir), "%s", argv[2]);
  CHECK(cuInit(0));
  CHECK(cuDeviceGet(&g_dev, 0));
  CHECK(cuDevicePrimaryCtxRetain(&ctx, g_dev));
  CHECK(cuCtxSetCurrent(ctx));
  return argv[1][0] == 'B' ? other_tenant((size_t)atol(argv[3]) << 20) : tenant();
}
