/*
 * include/graph_pool_sim.h against what the driver did (RTX 3080, driver 595.84,
 * docs/library_graph_memory_accounting.md 6.14), and its invariants under random traffic.
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
      graph_pool_sim_alloc(&s, (uint64_t)(i + 1), (size_t)mib[i] * MIB);
    } else {
      graph_pool_sim_free(&s, (uint64_t)(i + 1));
    }
  }
  size_t top = s.top / MIB;
  graph_pool_sim_destroy(&s);
  return top;
}

static void measured(void) {
  static const struct {
    const char *what, *script;
    int mib[5];
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
  };
  for (size_t i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
    size_t got = run(cases[i].script, cases[i].mib);
    EXPECT(got == cases[i].pool, "%s: %zu MiB, the driver %zu MiB", cases[i].what, got, cases[i].pool);
  }
}

static void edges(void) {
  graph_pool_sim_t s;
  graph_pool_sim_init(&s);
  EXPECT(graph_pool_sim_free(&s, 7) == 0, "a free of memory from elsewhere found something");
  graph_pool_sim_alloc(&s, 1, 0);
  EXPECT(s.top == 0, "a zero-sized allocation took %zu", s.top);
  EXPECT(graph_pool_sim_free(&s, 1) == 1, "the zero-sized allocation was not found");
  graph_pool_sim_alloc(&s, 2, SIZE_MAX - 1);
  graph_pool_sim_alloc(&s, 3, SIZE_MAX - 1);
  EXPECT(s.top == SIZE_MAX, "no saturation: %zu", s.top);
  graph_pool_sim_alloc(&s, 4, 4096);
  EXPECT(s.top == SIZE_MAX, "wrapped after saturating: %zu", s.top);
  graph_pool_sim_free(&s, 2);
  graph_pool_sim_free(&s, 3);
  graph_pool_sim_destroy(&s);

  /* No free at all, which is what a driver without cuGraphMemFreeNodeGetParams gives. */
  graph_pool_sim_init(&s);
  for (int i = 0; i < 4; i++) graph_pool_sim_alloc(&s, (uint64_t)(i + 1), 64 * MIB);
  EXPECT(s.top == 256 * MIB, "without frees it should be the sum: %zu", s.top);
  graph_pool_sim_destroy(&s);
}

static void check_invariants(const graph_pool_sim_t *s, size_t peak, size_t rounded_sum) {
  size_t total = 0, last_end = 0;
  for (size_t i = 0; i < s->nfreed; i++) {
    EXPECT(s->freed[i].size != 0, "an empty freed block");
    EXPECT(i == 0 || s->freed[i].off > last_end, "freed blocks out of order or touching");
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

static void random_traffic(void) {
  srand(12345);
  for (int round = 0; round < 2000 && !g_failed; round++) {
    graph_pool_sim_t s;
    graph_pool_sim_init(&s);
    uint64_t next = 1;
    size_t live_bytes = 0, peak = 0, rounded_sum = 0;
    int ops = 1 + rand() % 400;
    for (int op = 0; op < ops; op++) {
      if (s.nlive == 0 || rand() % 2 == 0) {
        size_t bytes = (size_t)(1 + rand() % 512) * MIB;
        graph_pool_sim_alloc(&s, next++, bytes);
        live_bytes += bytes;
        rounded_sum += graph_pool_round(bytes);
        if (live_bytes > peak) peak = live_bytes;
      } else {
        size_t pick = (size_t)rand() % s.nlive;
        uint64_t key = s.live[pick].key;
        live_bytes -= s.live[pick].size;
        EXPECT(graph_pool_sim_free(&s, key) == 1, "a live allocation was not found");
      }
      check_invariants(&s, peak, rounded_sum);
    }
    graph_pool_sim_destroy(&s);
  }
}

int main(void) {
  measured();
  edges();
  random_traffic();
  return g_failed;
}
