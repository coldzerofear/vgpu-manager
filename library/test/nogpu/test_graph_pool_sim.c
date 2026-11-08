/*
 * include/graph_pool_sim.h against what the driver did (RTX 3080 and RTX 2080 Ti, drivers
 * 580 and 595, docs/library_graph_memory_accounting.md 6.14 and 6.15), and its invariants
 * under random traffic.
 */
#include <stdio.h>
#include <stdlib.h>

#include "include/graph_pool_sim.h"

#define MIB ((size_t)1 << 20)

static int g_failed;

#define EXPECT(cond, ...)      \
  do {                         \
    if (!(cond)) {             \
      printf("FAIL: ");        \
      printf(__VA_ARGS__);     \
      printf("\n");            \
      g_failed = 1;            \
    }                          \
  } while (0)

/* "a0" allocates block 0, "f0" frees it, sizes in MiB. */
static size_t run(const char *script, const int *mib) {
  graph_pool_sim_t s;
  graph_pool_sim_init(&s);
  for (const char *c = script; *c != 0; c += 2) {
    int i = c[1] - '0';
    if (*c == 'a') {
      graph_pool_sim_alloc(&s, (uint64_t)(i + 1), (size_t)mib[i] * MIB, (int)(c - script) / 2);
    } else {
      graph_pool_sim_free(&s, (uint64_t)(i + 1), (int)(c - script) / 2);
    }
  }
  size_t top = s.top / MIB;
  graph_pool_sim_destroy(&s);
  return top;
}

static void measured(void) {
  static const struct {
    const char *what, *script;
    int mib[6];
    size_t pool;
  } cases[] = {
    {"chain of the same size", "a0f0a1f1a2f2a3f3", {256, 256, 256, 256}, 256},
    {"mixed sizes", "a0f0a1f1a2f2a3f3a4f4", {256, 512, 128, 1024, 64}, 1792},
    {"growing", "a0f0a1f1a2f2", {256, 512, 1024}, 1792},
    {"shrinking", "a0f0a1f1a2f2", {1024, 512, 256}, 1024},
    {"two at a time", "a0a1f0a2f1f2", {256, 256, 256}, 512},
    {"all at once", "a0a1a2a3f0f1f2f3", {256, 256, 256, 256}, 1024},
    {"split, together", "a0f0a1a2f1f2", {512, 256, 256}, 512},
    {"split, four together", "a0f0a1a2a3a4f1f2f3f4", {1024, 256, 256, 256, 256}, 1024},
    {"split, in turn", "a0f0a1f1a2f2", {512, 256, 256}, 512},
    {"merge", "a0a1f0f1a2f2", {256, 256, 512}, 512},
    {"no growing in place, 2 MiB", "a0f0a1f1", {256, 258}, 544},
    {"no growing in place, 64 MiB", "a0f0a1f1", {256, 320}, 576},
    {"small, large, small", "a0f0a1f1a2f2", {256, 512, 256}, 768},
    {"small, large, small, large", "a0f0a1f1a2f2a3f3", {256, 512, 256, 512}, 768},
    {"rounded up", "a0f0", {1800}, 1824},
    /* Best fit: the small hole for the small block, so the big one still fits. With the
     * first fit the 128 would go into the 512 and the 448 would need a step of its own. */
    {"best fit", "a0a1a2a3f0f2a4a5f4f5f1f3", {512, 32, 256, 32, 128, 448}, 832},
  };
  for (size_t i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
    size_t got = run(cases[i].script, cases[i].mib);
    EXPECT(got == cases[i].pool, "%s: %zu MiB, the driver %zu MiB", cases[i].what, got, cases[i].pool);
  }
}

/* ---- with dependencies: nodes in creation order, each with the nodes it depends on */
typedef struct {
  char op;      /* 'a' alloc, 'f' free of node k, 'e' empty */
  int k, mib, nd, d[3];
} dag_op_t;

#define MAX_NODES 16
typedef struct {
  int n;
  unsigned char reach[MAX_NODES][MAX_NODES];   /* reach[a][b]: b depends on a */
} dag_t;

static int dag_depends(void *ctx, int earlier, int later) {
  const dag_t *g = (const dag_t *)ctx;
  return earlier >= 0 && later >= 0 && earlier < g->n && later < g->n && g->reach[earlier][later];
}

/* extra: an edge from node extra[0] to extra[1] added after both exist, -1 for none. */
static size_t run_dag(const dag_op_t *ops, int n, const int *extra) {
  dag_t g;
  memset(&g, 0, sizeof(g));
  g.n = n;
  for (int i = 0; i < n; i++) {
    for (int j = 0; j < ops[i].nd; j++) g.reach[ops[i].d[j]][i] = 1;
  }
  if (extra[0] >= 0) g.reach[extra[0]][extra[1]] = 1;
  for (int k = 0; k < n; k++) {
    for (int a = 0; a < n; a++) {
      for (int b = 0; b < n; b++) {
        if (g.reach[a][k] && g.reach[k][b]) g.reach[a][b] = 1;
      }
    }
  }
  graph_pool_sim_t s;
  graph_pool_sim_init(&s);
  graph_pool_sim_set_depends(&s, dag_depends, &g);
  for (int i = 0; i < n; i++) {
    if (ops[i].op == 'a') graph_pool_sim_alloc(&s, (uint64_t)(i + 1), (size_t)ops[i].mib * MIB, i);
    else if (ops[i].op == 'f') graph_pool_sim_free(&s, (uint64_t)(ops[i].k + 1), i);
  }
  size_t top = s.top / MIB;
  graph_pool_sim_destroy(&s);
  return top;
}

static void measured_dags(void) {
  static const struct {
    const char *what;
    dag_op_t ops[8];
    int n, extra[2];
    size_t pool;
  } cases[] = {
    {"chain", {{'a',0,256,0,{0}}, {'f',0,0,1,{0}}, {'a',0,256,1,{1}}, {'f',2,0,1,{2}}}, 4, {-1,-1}, 256},
    {"parallel lines", {{'a',0,256,0,{0}}, {'f',0,0,1,{0}}, {'a',0,256,0,{0}}, {'f',2,0,1,{2}}}, 4, {-1,-1}, 512},
    {"2B after the frees of two lines", {{'a',0,256,0,{0}}, {'f',0,0,1,{0}}, {'a',0,256,0,{0}}, {'f',2,0,1,{2}}, {'a',0,512,2,{1,3}}, {'f',4,0,1,{4}}}, 6, {-1,-1}, 1024},
    {"B after the free of one of two lines", {{'a',0,256,0,{0}}, {'f',0,0,1,{0}}, {'a',0,256,0,{0}}, {'f',2,0,1,{2}}, {'a',0,256,1,{1}}, {'f',4,0,1,{4}}}, 6, {-1,-1}, 512},
    {"siblings after one free", {{'a',0,256,0,{0}}, {'f',0,0,1,{0}}, {'a',0,256,1,{1}}, {'a',0,256,1,{1}}, {'f',2,0,1,{2}}, {'f',3,0,1,{3}}}, 6, {-1,-1}, 512},
    {"after the allocation, beside its free", {{'a',0,256,0,{0}}, {'f',0,0,1,{0}}, {'a',0,256,1,{0}}, {'f',2,0,1,{2}}}, 4, {-1,-1}, 512},
    {"after the free through an empty node", {{'a',0,256,0,{0}}, {'f',0,0,1,{0}}, {'e',0,0,1,{1}}, {'a',0,256,1,{2}}, {'f',3,0,1,{3}}}, 5, {-1,-1}, 256},
    {"after the free and another root", {{'a',0,256,0,{0}}, {'f',0,0,1,{0}}, {'e',0,0,0,{0}}, {'a',0,256,2,{1,2}}, {'f',3,0,1,{3}}}, 5, {-1,-1}, 256},
    {"diamond", {{'a',0,256,0,{0}}, {'e',0,0,1,{0}}, {'f',0,0,1,{1}}, {'a',0,256,1,{0}}, {'f',3,0,1,{3}}}, 5, {-1,-1}, 512},
    {"created before its dependency was added", {{'a',0,256,0,{0}}, {'a',0,256,0,{0}}, {'f',0,0,1,{0}}, {'f',1,0,1,{1}}}, 4, {2,1}, 512},
  };
  for (size_t i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
    size_t got = run_dag(cases[i].ops, cases[i].n, cases[i].extra);
    EXPECT(got == cases[i].pool, "%s: %zu MiB, the driver %zu MiB", cases[i].what, got, cases[i].pool);
  }
}

/* Rules of the tags themselves, beyond the measured shapes. */
static void tags(void) {
  /* The later free's tag wins a merge: a node depending only on the earlier one cannot
   * use the merged block, one depending on the later can. */
  static const dag_op_t merged[] = {
    {'a',0,256,0,{0}}, {'a',0,256,1,{0}}, {'f',0,0,1,{1}}, {'f',1,0,1,{2}},
    {'a',0,512,1,{2}},  /* after the first free only: the merged block is not its */
    {'f',4,0,1,{4}},
  };
  int none[2] = {-1, -1};
  size_t got = run_dag(merged, 6, none);
  EXPECT(got == 1024, "a merged block went to a node not after its later free: %zu", got);
  static const dag_op_t merged_ok[] = {
    {'a',0,256,0,{0}}, {'a',0,256,1,{0}}, {'f',0,0,1,{1}}, {'f',1,0,1,{2}},
    {'a',0,512,1,{3}}, {'f',4,0,1,{4}},
  };
  got = run_dag(merged_ok, 6, none);
  EXPECT(got == 512, "a node after the later free did not get the merged block: %zu", got);
  /* The rest of a step is anyone's while it stands alone. */
  static const dag_op_t rest[] = {
    {'a',0,16,0,{0}}, {'a',0,8,0,{0}}, {'f',0,0,1,{0}}, {'f',1,0,1,{1}},
  };
  got = run_dag(rest, 4, none);
  EXPECT(got == 32, "a parallel node did not use the rest of the step: %zu", got);
  /* A node and itself: what the model asks when two ops share a child graph's index. */
  graph_pool_sim_t s;
  graph_pool_sim_init(&s);
  EXPECT(graph_pool_sim_reaches(&s, 3, 3) == 1, "a node does not reach itself");
  graph_pool_sim_destroy(&s);
}

static void edges(void) {
  graph_pool_sim_t s;
  graph_pool_sim_init(&s);
  EXPECT(graph_pool_sim_free(&s, 7, 0) == 0, "a free of memory from elsewhere found something");
  graph_pool_sim_alloc(&s, 1, 0, 0);
  EXPECT(s.top == 0, "a zero-sized allocation took %zu", s.top);
  EXPECT(graph_pool_sim_free(&s, 1, 1) == 1, "the zero-sized allocation was not found");
  graph_pool_sim_alloc(&s, 2, SIZE_MAX - 1, 2);
  graph_pool_sim_alloc(&s, 3, SIZE_MAX - 1, 3);
  EXPECT(s.top == SIZE_MAX, "no saturation: %zu", s.top);
  graph_pool_sim_alloc(&s, 4, 4096, 4);
  EXPECT(s.top == SIZE_MAX, "wrapped after saturating: %zu", s.top);
  graph_pool_sim_free(&s, 2, 5);
  graph_pool_sim_free(&s, 3, 6);
  graph_pool_sim_destroy(&s);

  /* No free at all, which is what a driver without cuGraphMemFreeNodeGetParams gives. */
  graph_pool_sim_init(&s);
  for (int i = 0; i < 4; i++) graph_pool_sim_alloc(&s, (uint64_t)(i + 1), 64 * MIB, i);
  EXPECT(s.top == 256 * MIB, "without frees it should be the sum: %zu", s.top);
  EXPECT(s.live_bytes == 256 * MIB, "kept: %zu", s.live_bytes);
  graph_pool_sim_free(&s, 2, 4);
  EXPECT(s.live_bytes == 192 * MIB, "kept after a free: %zu", s.live_bytes);
  graph_pool_sim_destroy(&s);
}

static int cmp_range(const void *a, const void *b) {
  const size_t *x = (const size_t *)a, *y = (const size_t *)b;
  return x[0] < y[0] ? -1 : x[0] > y[0];
}

static void check_invariants(const graph_pool_sim_t *s, size_t peak, size_t rounded_sum) {
  /* Every byte below the top in exactly one block, live or freed. */
  size_t n = s->nfreed + s->nlive;
  size_t *r = (size_t *)malloc((n ? n : 1) * 2 * sizeof(*r));
  if (r != NULL) {
    for (size_t i = 0; i < s->nfreed; i++) { r[2 * i] = s->freed[i].off; r[2 * i + 1] = s->freed[i].size; }
    for (size_t i = 0; i < s->nlive; i++) { r[2 * (s->nfreed + i)] = s->live[i].off; r[2 * (s->nfreed + i) + 1] = s->live[i].size; }
    qsort(r, n, 2 * sizeof(*r), cmp_range);
    size_t at = 0;
    for (size_t i = 0; i < n; i++) {
      if (r[2 * i + 1] == 0) continue;
      EXPECT(r[2 * i] == at, "a gap or an overlap at %zu (block at %zu)", at, r[2 * i]);
      at = r[2 * i] + r[2 * i + 1];
    }
    EXPECT(at == s->top || n == 0, "blocks end at %zu, top %zu", at, s->top);
    free(r);
  }
  /* Without dependencies, two freed neighbours are always one block. */
  for (size_t i = 0; s->depends == NULL && i + 1 < s->nfreed; i++) {
    EXPECT(s->freed[i].off + s->freed[i].size != s->freed[i + 1].off, "freed neighbours left apart");
  }
  size_t total = 0, last_end = 0;
  for (size_t i = 0; i < s->nfreed; i++) {
    EXPECT(s->freed[i].size != 0, "an empty freed block");
    EXPECT(i == 0 || s->freed[i].off >= last_end, "freed blocks out of order or overlapping");
    last_end = s->freed[i].off + s->freed[i].size;
    EXPECT(last_end <= s->top, "a freed block past the top");
    total += s->freed[i].size;
  }
  for (size_t i = 0; i < s->nlive; i++) {
    EXPECT(s->live[i].off + s->live[i].size <= s->top, "a live block past the top");
    total += s->live[i].size;
  }
  EXPECT(total == s->top, "blocks add up to %zu of %zu", total, s->top);
  EXPECT(s->top >= peak, "%zu under the peak %zu", s->top, peak);
  EXPECT(s->top <= rounded_sum, "%zu over the sum %zu", s->top, rounded_sum);
}

/* A node depends on an earlier one with a fixed random pattern: any relation will do for
 * the invariants, which hold whatever the dependencies are. */
static int random_depends(void *ctx, int earlier, int later) {
  (void)ctx;
  return earlier < later && ((unsigned)(earlier * 2654435761u ^ later * 40503u) >> 7) % 3 != 0;
}

static void random_traffic(void) {
  srand(12345);
  for (int round = 0; round < 4000 && !g_failed; round++) {
    graph_pool_sim_t s;
    graph_pool_sim_init(&s);
    if (round % 2) graph_pool_sim_set_depends(&s, random_depends, NULL);
    uint64_t next = 1;
    size_t live_bytes = 0, peak = 0, rounded_sum = 0;
    int ops = 1 + rand() % 400;
    for (int op = 0; op < ops; op++) {
      if (s.nlive == 0 || rand() % 2 == 0) {
        size_t bytes = (size_t)(1 + rand() % 512) * MIB;
        graph_pool_sim_alloc(&s, next++, bytes, op);
        live_bytes += bytes;
        rounded_sum += graph_pool_round(bytes);
        if (live_bytes > peak) peak = live_bytes;
      } else {
        size_t pick = (size_t)rand() % s.nlive;
        uint64_t key = s.live[pick].key;
        live_bytes -= s.live[pick].size;
        EXPECT(graph_pool_sim_free(&s, key, op) == 1, "a live allocation was not found");
      }
      check_invariants(&s, peak, rounded_sum);
      EXPECT(s.live_bytes == live_bytes, "kept %zu, live %zu", s.live_bytes, live_bytes);
    }
    graph_pool_sim_destroy(&s);
  }
}

int main(void) {
  measured();
  measured_dags();
  tags();
  edges();
  random_traffic();
  return g_failed;
}
