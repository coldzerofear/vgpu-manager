/*
 * Two questions about the graph pool, from reviewing HAMi-core #395/#396.
 *
 * recapture X_MIB   Capture and launch a graph of X, then capture a second one of X.
 *                   Without the library: does the second launch grow the pool? With it and
 *                   a limit under 2X + base: is the second capture refused all the same?
 * inner N B_MIB     One graph with N blocks of B allocated and freed one after the other,
 *                   then one holding all N at once. Does the pool reach the peak or the sum?
 *
 * shapes B_MIB      When does the pool reuse inside a graph? The dependency shapes a limit
 *                   check would have to tell apart.
 * refill X_MIB      recapture by the other ways to allocate in a graph: a node added by hand,
 *                   and cuMemAllocFromPoolAsync in a capture.
 *
 * blocks B_MIB      What the reuse does with blocks of different sizes.
 * model N           N random sequences on one dependency line: the library's estimate
 *                   (include/graph_pool_sim.h) against what the pool came to.
 *
 * Usage: probe_pool_reuse recapture X_MIB | inner N B_MIB | shapes B_MIB | blocks B_MIB |
 *        refill X_MIB | model N
 */
#include <cuda.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "../../include/graph_pool_sim.h"

#define MIB(x) ((double)(x) / 1048576.0)
#define CHECK(call)                                                            \
  do {                                                                         \
    CUresult _r = (call);                                                      \
    if (_r != CUDA_SUCCESS) {                                                  \
      printf("%s -> %d\n", #call, (int)_r);                                    \
      exit(2);                                                                 \
    }                                                                          \
  } while (0)

static CUdevice g_dev;
static CUstream g_s;

static size_t reserved(void) {
  cuuint64_t v = 0;
  cuDeviceGetGraphMemAttribute(g_dev, CU_GRAPH_MEM_ATTR_RESERVED_MEM_CURRENT, &v);
  return (size_t)v;
}

static void trim(void) {
  CHECK(cuCtxSynchronize());
  CHECK(cuDeviceGraphMemTrim(g_dev));
}

static void report(const char *what, CUgraph g) {
  CUgraphExec e;
  size_t before = reserved();
  CHECK(cuGraphInstantiateWithFlags(&e, g, 0));
  CUresult r = cuGraphLaunch(e, g_s);
  CHECK(cuStreamSynchronize(g_s));
  printf("%-34s launch -> %d, pool %.0f -> %.0f MiB\n", what, (int)r, MIB(before), MIB(reserved()));
  CHECK(cuGraphExecDestroy(e));
  CHECK(cuGraphDestroy(g));
}

/* Returns NULL if the driver or the library refused the allocation. */
static CUgraph capture_one(size_t bytes) {
  CUgraph g = NULL;
  CUdeviceptr d = 0;
  CHECK(cuStreamBeginCapture(g_s, CU_STREAM_CAPTURE_MODE_RELAXED));
  CUresult a = cuMemAllocAsync(&d, bytes, g_s);
  printf("capture: cuMemAllocAsync(%.0f MiB) -> %d\n", MIB(bytes), (int)a);
  if (a == CUDA_SUCCESS) {
    CHECK(cuMemsetD8Async(d, 1, bytes, g_s));
    CHECK(cuMemFreeAsync(d, g_s));
  }
  CHECK(cuStreamEndCapture(g_s, &g));
  if (a != CUDA_SUCCESS && g != NULL) {
    CHECK(cuGraphDestroy(g));
    g = NULL;
  }
  return g;
}

static int recapture(size_t x) {
  CUgraph a = capture_one(x);
  if (a == NULL) return 2;
  report("first graph", a);
  CUgraph b = capture_one(x);
  if (b == NULL) {
    printf("second capture refused\n");
    return 1;
  }
  report("second graph", b);
  return 0;
}

static void params(CUDA_MEM_ALLOC_NODE_PARAMS *p, size_t bytes) {
  memset(p, 0, sizeof(*p));
  p->poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  p->poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  p->poolProps.location.id = g_dev;
  p->bytesize = bytes;
}

/* Hand-built: one after the other, each block's free before the next one's allocation. */
static CUgraph chained(int n, size_t b) {
  CUgraph g;
  CUgraphNode prev = NULL;
  CHECK(cuGraphCreate(&g, 0));
  for (int i = 0; i < n; i++) {
    CUDA_MEM_ALLOC_NODE_PARAMS p;
    CUgraphNode a, f;
    params(&p, b);
    CHECK(cuGraphAddMemAllocNode(&a, g, prev != NULL ? &prev : NULL, prev != NULL ? 1 : 0, &p));
    CHECK(cuGraphAddMemFreeNode(&f, g, &a, 1, p.dptr));
    prev = f;
  }
  return g;
}

/* Hand-built: all allocated, then all freed. */
static CUgraph overlapping(int n, size_t b) {
  CUgraph g;
  CUgraphNode prev = NULL;
  CUdeviceptr *ptrs = calloc((size_t)n, sizeof(*ptrs));
  CHECK(cuGraphCreate(&g, 0));
  for (int i = 0; i < n; i++) {
    CUDA_MEM_ALLOC_NODE_PARAMS p;
    CUgraphNode a;
    params(&p, b);
    CHECK(cuGraphAddMemAllocNode(&a, g, prev != NULL ? &prev : NULL, prev != NULL ? 1 : 0, &p));
    ptrs[i] = p.dptr;
    prev = a;
  }
  for (int i = 0; i < n; i++) {
    CUgraphNode f;
    CHECK(cuGraphAddMemFreeNode(&f, g, &prev, 1, ptrs[i]));
    prev = f;
  }
  free(ptrs);
  return g;
}

/* Captured: what an application's temporaries look like. NULL if an allocation was refused. */
static CUgraph captured_chain(int n, size_t b) {
  CUgraph g = NULL;
  CUresult bad = CUDA_SUCCESS;
  CHECK(cuStreamBeginCapture(g_s, CU_STREAM_CAPTURE_MODE_RELAXED));
  for (int i = 0; i < n && bad == CUDA_SUCCESS; i++) {
    CUdeviceptr d = 0;
    bad = cuMemAllocAsync(&d, b, g_s);
    if (bad == CUDA_SUCCESS) {
      CHECK(cuMemsetD8Async(d, 1, b, g_s));
      CHECK(cuMemFreeAsync(d, g_s));
    }
  }
  CHECK(cuStreamEndCapture(g_s, &g));
  if (bad != CUDA_SUCCESS) {
    printf("captured chain: allocation refused -> %d\n", (int)bad);
    if (g != NULL) CHECK(cuGraphDestroy(g));
    return NULL;
  }
  return g;
}

static int inner(int n, size_t b) {
  printf("%d blocks of %.0f MiB: peak %.0f, sum %.0f MiB\n", n, MIB(b), MIB(b), MIB(b) * n);
  trim();
  report("hand-built, one after the other", chained(n, b));
  trim();
  CUgraph c = captured_chain(n, b);
  if (c != NULL) report("captured, one after the other", c);
  trim();
  report("hand-built, all at once", overlapping(n, b));
  trim();
  return 0;
}

/* Hand-built from a script: "a"/"f" with a block number, each op after the previous one
 * of its line; a "|" starts a new line, with no dependency on the one before. A size
 * list gives each block's MiB. */
static void shape(const char *what, const char *script, const int *mib) {
  CUgraph g;
  CUgraphNode prev = NULL;
  CUdeviceptr ptrs[16] = {0};
  CHECK(cuGraphCreate(&g, 0));
  for (const char *c = script; *c != 0; c++) {
    if (*c == '|') {
      prev = NULL;
    } else if (*c == 'a') {
      int i = *++c - '0';
      CUDA_MEM_ALLOC_NODE_PARAMS p;
      CUgraphNode n;
      params(&p, (size_t)mib[i] << 20);
      CHECK(cuGraphAddMemAllocNode(&n, g, prev != NULL ? &prev : NULL, prev != NULL ? 1 : 0, &p));
      ptrs[i] = p.dptr;
      prev = n;
    } else if (*c == 'f') {
      int i = *++c - '0';
      CUgraphNode n;
      CHECK(cuGraphAddMemFreeNode(&n, g, prev != NULL ? &prev : NULL, prev != NULL ? 1 : 0, ptrs[i]));
      prev = n;
    }
  }
  trim();
  report(what, g);
}

static int shapes(int b) {
  int same[4] = {b, b, b, b};
  int mixed[5] = {b, b * 2, b / 2, b * 4, b / 4};
  int down[3] = {b * 4, b * 2, b};
  int up[3] = {b, b * 2, b * 4};
  shape("chain a0 f0 a1 f1 a2 f2 a3 f3", "a0f0a1f1a2f2a3f3", same);
  shape("mixed sizes, chain", "a0f0a1f1a2f2a3f3a4f4", mixed);
  shape("sizes growing, chain", "a0f0a1f1a2f2", up);
  shape("sizes shrinking, chain", "a0f0a1f1a2f2", down);
  shape("two at a time a0 a1 f0 a2 f1 f2", "a0a1f0a2f1f2", same);
  shape("independent lines a0f0 | a1f1", "a0f0|a1f1", same);
  shape("four independent lines", "a0f0|a1f1|a2f2|a3f3", same);
  shape("all at once a0 a1 a2 a3 f0..f3", "a0a1a2a3f0f1f2f3", same);
  return 0;
}

/* What the reuse does with blocks of different sizes: split, merge, or neither. */
static int blocks(int b) {
  int split[3] = {b * 2, b, b};
  int split4[5] = {b * 4, b, b, b, b};
  int merge[3] = {b, b, b * 2};
  int near[2] = {b, b + 2};
  int near_big[2] = {b, b + 64};
  int two_then_one[3] = {b, b * 2, b};
  int pair[4] = {b, b * 2, b, b * 2};
  shape("split? 2B, then B and B together", "a0f0a1a2f1f2", split);
  shape("split? 4B, then 4x B together", "a0f0a1a2a3a4f1f2f3f4", split4);
  shape("split? 2B, then B, B in turn", "a0f0a1f1a2f2", split);
  shape("merge? B and B freed, then 2B", "a0a1f0f1a2f2", merge);
  shape("B, then B + 2 MiB", "a0f0a1f1", near);
  shape("B, then B + 64 MiB", "a0f0a1f1", near_big);
  shape("B, 2B, B", "a0f0a1f1a2f2", two_then_one);
  shape("B, 2B, B, 2B", "a0f0a1f1a2f2a3f3", pair);
  return 0;
}

/* Compares by trimming, launching and reading the pool, so it is done without a limit. */
static int model(int rounds) {
  static const size_t kLive = 4096;  /* MiB alive at most */
  int exact = 0, over = 0, under = 0;
  double worst_over = 0, worst_under = 0, total_over = 0, total_under = 0;
  srand(4242);
  for (int round = 0; round < rounds; round++) {
    CUgraph g;
    CUgraphNode prev = NULL;
    graph_pool_sim_t sim;
    CUdeviceptr live[64];
    size_t size[64];
    int nlive = 0;
    size_t live_mib = 0;
    int small = round % 3 == 0;
    graph_pool_sim_init(&sim);
    CHECK(cuGraphCreate(&g, 0));
    int ops = 2 + rand() % 40;
    for (int op = 0; op < ops || nlive > 0; op++) {
      size_t mib = (size_t)(small ? 1 + rand() % 64 : 1 + rand() % 900);
      if (op < ops && nlive < 64 && live_mib + mib <= kLive && (nlive == 0 || rand() % 2 == 0)) {
        CUDA_MEM_ALLOC_NODE_PARAMS p;
        CUgraphNode n;
        params(&p, mib << 20);
        CHECK(cuGraphAddMemAllocNode(&n, g, prev != NULL ? &prev : NULL, prev != NULL ? 1 : 0, &p));
        prev = n;
        live[nlive] = p.dptr;
        size[nlive++] = mib;
        live_mib += mib;
        graph_pool_sim_alloc(&sim, (uint64_t)p.dptr, mib << 20);
      } else if (nlive > 0) {
        int pick = rand() % nlive;
        CUgraphNode n;
        CHECK(cuGraphAddMemFreeNode(&n, g, &prev, 1, live[pick]));
        prev = n;
        graph_pool_sim_free(&sim, (uint64_t)live[pick]);
        live_mib -= size[pick];
        live[pick] = live[--nlive];
        size[pick] = size[nlive];
      }
    }
    trim();
    CUgraphExec e;
    CHECK(cuGraphInstantiateWithFlags(&e, g, 0));
    CHECK(cuGraphLaunch(e, g_s));
    CHECK(cuStreamSynchronize(g_s));
    double pool = MIB(reserved()), est = MIB(sim.top);
    if (pool == est) exact++;
    else if (est > pool) { over++; total_over += est - pool; if (est - pool > worst_over) worst_over = est - pool; }
    else { under++; total_under += pool - est; if (pool - est > worst_under) worst_under = pool - est; }
    graph_pool_sim_destroy(&sim);
    CHECK(cuGraphExecDestroy(e));
    CHECK(cuGraphDestroy(g));
  }
  printf("%d sequences: %d exact, %d over (worst %.0f, average %.0f MiB), %d under (worst %.0f, average %.0f MiB)\n",
         rounds, exact, over, worst_over, over ? total_over / over : 0, under, worst_under, under ? total_under / under : 0);
  trim();
  return 0;
}

static int refill(size_t x) {
  CUgraph a = capture_one(x);
  if (a == NULL) return 2;
  report("first graph (captured)", a);
  CUDA_MEM_ALLOC_NODE_PARAMS p;
  CUgraph g;
  CUgraphNode n, f;
  params(&p, x);
  CHECK(cuGraphCreate(&g, 0));
  CUresult r = cuGraphAddMemAllocNode(&n, g, NULL, 0, &p);
  printf("cuGraphAddMemAllocNode(%.0f MiB) -> %d\n", MIB(x), (int)r);
  if (r == CUDA_SUCCESS) {
    CHECK(cuGraphAddMemFreeNode(&f, g, &n, 1, p.dptr));
    report("second graph (hand-built)", g);
  } else {
    CHECK(cuGraphDestroy(g));
  }
  CUmemoryPool pool;
  CUmemPoolProps props;
  memset(&props, 0, sizeof(props));
  props.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  props.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  props.location.id = g_dev;
  CHECK(cuMemPoolCreate(&pool, &props));
  CUdeviceptr d = 0;
  CUgraph c = NULL;
  CHECK(cuStreamBeginCapture(g_s, CU_STREAM_CAPTURE_MODE_RELAXED));
  r = cuMemAllocFromPoolAsync(&d, x, pool, g_s);
  printf("cuMemAllocFromPoolAsync(%.0f MiB) in a capture -> %d\n", MIB(x), (int)r);
  if (r == CUDA_SUCCESS) CHECK(cuMemFreeAsync(d, g_s));
  CHECK(cuStreamEndCapture(g_s, &c));
  if (r == CUDA_SUCCESS) report("third graph (from a pool)", c);
  else if (c != NULL) CHECK(cuGraphDestroy(c));
  CHECK(cuMemPoolDestroy(pool));
  return 0;
}

int main(int argc, char **argv) {
  CUcontext ctx;
  if (argc < 3) return 2;
  CHECK(cuInit(0));
  CHECK(cuDeviceGet(&g_dev, 0));
  CHECK(cuDevicePrimaryCtxRetain(&ctx, g_dev));
  CHECK(cuCtxSetCurrent(ctx));
  CHECK(cuStreamCreate(&g_s, 0));
  if (strcmp(argv[1], "recapture") == 0) return recapture((size_t)atol(argv[2]) << 20);
  if (strcmp(argv[1], "inner") == 0 && argc == 4) return inner(atoi(argv[2]), (size_t)atol(argv[3]) << 20);
  if (strcmp(argv[1], "blocks") == 0) return blocks(atoi(argv[2]));
  if (strcmp(argv[1], "model") == 0) return model(atoi(argv[2]));
  if (strcmp(argv[1], "shapes") == 0) return shapes(atoi(argv[2]));
  if (strcmp(argv[1], "refill") == 0) return refill((size_t)atol(argv[2]) << 20);
  return 2;
}
