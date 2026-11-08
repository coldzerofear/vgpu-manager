/*
 * What a graph's allocations take from the device's graph pool, estimated by replaying
 * them against a model of the driver's allocator (drivers 580 and 595,
 * docs/library_graph_memory_accounting.md 6.14 and 6.15):
 *
 *   - nodes are taken in creation order, the driver's: a node given a dependency after
 *     it was created does not reuse what that dependency freed;
 *   - an allocation reuses the smallest block big enough for it among those freed by a
 *     free node it depends on, splitting it; one in a parallel branch does not;
 *   - neighbouring freed blocks merge when one free depends on the other, not when
 *     the two are unordered;
 *   - a freed block does not grow in place: 256 then 512 MiB, in turn, take 768;
 *   - the pool grows in steps of GRAPH_POOL_GRAIN.
 *
 * The sum of the allocations is an upper bound and wrong for temporaries, the peak of
 * those alive at once is a lower bound and wrong for growing sizes. This is exact for
 * the shapes measured and close on random ones, where the driver's choice of block is
 * not fully known; the smallest was the closest of the policies tried.
 *
 * Dependencies come from a callback, graph_pool_depends(ctx, a, b): 1 if node b depends
 * on node a. Without one (NULL), every earlier free counts as a dependency, which
 * under-estimates parallel branches; the walk falls back on that when a graph is too
 * big to work out its dependencies.
 *
 * Header-only with no driver dependency, so test/nogpu/test_graph_pool_sim.c can drive it
 * directly. Not thread-safe: one walk of one graph owns it.
 */
#ifndef VGPU_GRAPH_POOL_SIM_H
#define VGPU_GRAPH_POOL_SIM_H

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#define GRAPH_POOL_GRAIN ((size_t)32 << 20)

/* The tag of a block nobody freed: the rest of a step the pool grew by. */
#define GRAPH_POOL_ANY_NODE (-1)

typedef int (*graph_pool_depends_fn)(void *ctx, int earlier, int later);

typedef struct {
  size_t off;
  size_t size;
  int node;   /* the free node that gave it back */
} graph_pool_block_t;

typedef struct {
  uint64_t key;  /* the allocation's address */
  size_t off;
  size_t size;
} graph_pool_live_t;

typedef struct {
  size_t top;                 /* the estimate: how far the pool had to grow */
  graph_pool_block_t *freed;  /* by offset */
  size_t nfreed, cap_freed;
  graph_pool_live_t *live;
  size_t nlive, cap_live;
  size_t live_bytes;          /* what is still allocated at the end: kept after a launch */
  int degraded;               /* out of memory: from here on, the sum */
  graph_pool_depends_fn depends;
  void *depends_ctx;
} graph_pool_sim_t;

static inline void graph_pool_sim_init(graph_pool_sim_t *s) {
  memset(s, 0, sizeof(*s));
}

static inline void graph_pool_sim_set_depends(graph_pool_sim_t *s, graph_pool_depends_fn fn, void *ctx) {
  s->depends = fn;
  s->depends_ctx = ctx;
}

static inline void graph_pool_sim_destroy(graph_pool_sim_t *s) {
  free(s->freed);
  free(s->live);
  graph_pool_sim_init(s);
}

static inline size_t graph_pool_add_sat(size_t a, size_t b) {
  return a > SIZE_MAX - b ? SIZE_MAX : a + b;
}

static inline size_t graph_pool_round(size_t bytes) {
  size_t over = bytes % GRAPH_POOL_GRAIN;
  return over == 0 ? bytes : graph_pool_add_sat(bytes, GRAPH_POOL_GRAIN - over);
}

/* Whether what node `earlier` freed is there for node `later`. */
static inline int graph_pool_sim_reaches(const graph_pool_sim_t *s, int earlier, int later) {
  if (earlier == GRAPH_POOL_ANY_NODE || s->depends == NULL) return 1;
  if (later == GRAPH_POOL_ANY_NODE) return 0;
  return earlier == later || s->depends(s->depends_ctx, earlier, later);
}

/* Two neighbours merge into one block, tagged with the later free, if one free depends on
 * the other. Returns the tag, or 0 with *merged unset if they stay apart. */
static inline int graph_pool_sim_merge_tag(const graph_pool_sim_t *s, int a, int b, int *merged) {
  if (graph_pool_sim_reaches(s, a, b)) { *merged = b; return 1; }
  if (graph_pool_sim_reaches(s, b, a)) { *merged = a; return 1; }
  return 0;
}

/* Inserts at its place by offset, merging with the neighbours it may. 0 if out of memory. */
static inline int graph_pool_give_back(graph_pool_sim_t *s, size_t off, size_t size, int node) {
  size_t i = 0;
  while (i < s->nfreed && s->freed[i].off < off) i++;
  int tag;
  if (i > 0 && s->freed[i - 1].off + s->freed[i - 1].size == off &&
      graph_pool_sim_merge_tag(s, s->freed[i - 1].node, node, &tag)) {
    i--;
    s->freed[i].size += size;
    s->freed[i].node = tag;
  } else {
    if (s->nfreed == s->cap_freed) {
      size_t cap = s->cap_freed != 0 ? s->cap_freed * 2 : 16;
      graph_pool_block_t *grown = (graph_pool_block_t *)realloc(s->freed, cap * sizeof(*grown));
      if (grown == NULL) return 0;
      s->freed = grown;
      s->cap_freed = cap;
    }
    memmove(&s->freed[i + 1], &s->freed[i], (s->nfreed - i) * sizeof(s->freed[0]));
    s->freed[i].off = off;
    s->freed[i].size = size;
    s->freed[i].node = node;
    s->nfreed++;
  }
  /* The block at i may now touch the next one. */
  if (i + 1 < s->nfreed && s->freed[i].off + s->freed[i].size == s->freed[i + 1].off &&
      graph_pool_sim_merge_tag(s, s->freed[i].node, s->freed[i + 1].node, &tag)) {
    s->freed[i].size += s->freed[i + 1].size;
    s->freed[i].node = tag;
    memmove(&s->freed[i + 1], &s->freed[i + 2], (s->nfreed - i - 2) * sizeof(s->freed[0]));
    s->nfreed--;
  }
  return 1;
}

/* An alloc node of bytes at address key; node is its index, for the dependencies. */
static inline void graph_pool_sim_alloc(graph_pool_sim_t *s, uint64_t key, size_t bytes, int node) {
  s->live_bytes = graph_pool_add_sat(s->live_bytes, bytes);
  if (s->degraded) {
    s->top = graph_pool_add_sat(s->top, bytes);
    return;
  }
  if (s->nlive == s->cap_live) {
    size_t cap = s->cap_live != 0 ? s->cap_live * 2 : 16;
    graph_pool_live_t *grown = (graph_pool_live_t *)realloc(s->live, cap * sizeof(*grown));
    if (grown == NULL) {
      s->degraded = 1;
      s->top = graph_pool_add_sat(s->top, bytes);
      return;
    }
    s->live = grown;
    s->cap_live = cap;
  }
  size_t off = 0;
  size_t i = s->nfreed;
  for (size_t j = 0; j < s->nfreed; j++) {
    if (s->freed[j].size >= bytes && (i == s->nfreed || s->freed[j].size < s->freed[i].size) &&
        graph_pool_sim_reaches(s, s->freed[j].node, node)) {
      i = j;
    }
  }
  if (i < s->nfreed) {
    off = s->freed[i].off;
    s->freed[i].off += bytes;
    s->freed[i].size -= bytes;
    if (s->freed[i].size == 0) {
      memmove(&s->freed[i], &s->freed[i + 1], (s->nfreed - i - 1) * sizeof(s->freed[0]));
      s->nfreed--;
    }
  } else {
    size_t step = graph_pool_round(bytes);
    off = s->top;
    if (step > SIZE_MAX - off) {
      s->top = SIZE_MAX;
      s->degraded = 1;
      return;
    }
    s->top = off + step;
    if (step > bytes && !graph_pool_give_back(s, off + bytes, step - bytes, GRAPH_POOL_ANY_NODE)) {
      s->degraded = 1;
      return;
    }
  }
  s->live[s->nlive].key = key;
  s->live[s->nlive].off = off;
  s->live[s->nlive].size = bytes;
  s->nlive++;
}

/* A free node, index node. Returns 1 if key is an allocation of this simulation. */
static inline int graph_pool_sim_free(graph_pool_sim_t *s, uint64_t key, int node) {
  /* Latest first: the usual graph frees what it allocated last. */
  for (size_t i = s->nlive; i-- > 0;) {
    if (s->live[i].key != key) continue;
    size_t off = s->live[i].off, size = s->live[i].size;
    s->live_bytes -= size < s->live_bytes ? size : s->live_bytes;
    s->live[i] = s->live[s->nlive - 1];
    s->nlive--;
    if (!s->degraded && !graph_pool_give_back(s, off, size, node)) s->degraded = 1;
    return 1;
  }
  return 0;
}

#endif /* VGPU_GRAPH_POOL_SIM_H */
