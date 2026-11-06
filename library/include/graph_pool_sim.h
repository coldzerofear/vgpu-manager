/*
 * What a graph's allocations take from the device's graph pool, estimated by replaying
 * them in node order against a model of the driver's allocator (measured on driver 595,
 * docs/library_graph_memory_accounting.md 6.14):
 *
 *   - an allocation reuses the smallest freed block big enough for it, splitting it;
 *   - neighbouring freed blocks merge;
 *   - a freed block does not grow in place: 256 then 512 MiB, in turn, take 768;
 *   - the pool grows in steps of GRAPH_POOL_GRAIN.
 *
 * The sum of the allocations is an upper bound and wrong for temporaries, the peak of
 * those alive at once is a lower bound and wrong for growing sizes. This is exact for
 * those two and close on random sequences, where the driver's choice of block is not
 * fully known; the smallest was the closest of the policies tried. One known
 * under-estimate: the driver only reuses a block whose free the allocation depends on,
 * and the model does not look at dependencies. Parallel branches can take more than
 * estimated; NVML reports that once they are mapped.
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

typedef struct {
  size_t off;
  size_t size;
} graph_pool_block_t;

typedef struct {
  uint64_t key;  /* the allocation's address */
  size_t off;
  size_t size;
} graph_pool_live_t;

typedef struct {
  size_t top;                 /* the estimate: how far the pool had to grow */
  graph_pool_block_t *freed;  /* by offset, none touching */
  size_t nfreed, cap_freed;
  graph_pool_live_t *live;
  size_t nlive, cap_live;
  int degraded;               /* out of memory: from here on, the sum */
} graph_pool_sim_t;

static inline void graph_pool_sim_init(graph_pool_sim_t *s) {
  memset(s, 0, sizeof(*s));
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

/* Inserts at its place by offset, merging with the neighbours. 0 if out of memory. */
static inline int graph_pool_give_back(graph_pool_sim_t *s, size_t off, size_t size) {
  size_t i = 0;
  while (i < s->nfreed && s->freed[i].off < off) i++;
  int with_prev = i > 0 && s->freed[i - 1].off + s->freed[i - 1].size == off;
  int with_next = i < s->nfreed && off + size == s->freed[i].off;
  if (with_prev && with_next) {
    s->freed[i - 1].size += size + s->freed[i].size;
    memmove(&s->freed[i], &s->freed[i + 1], (s->nfreed - i - 1) * sizeof(s->freed[0]));
    s->nfreed--;
    return 1;
  }
  if (with_prev) {
    s->freed[i - 1].size += size;
    return 1;
  }
  if (with_next) {
    s->freed[i].off = off;
    s->freed[i].size += size;
    return 1;
  }
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
  s->nfreed++;
  return 1;
}

static inline void graph_pool_sim_alloc(graph_pool_sim_t *s, uint64_t key, size_t bytes) {
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
    if (s->freed[j].size >= bytes && (i == s->nfreed || s->freed[j].size < s->freed[i].size)) i = j;
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
    if (step > bytes && !graph_pool_give_back(s, off + bytes, step - bytes)) {
      s->degraded = 1;
      return;
    }
  }
  s->live[s->nlive].key = key;
  s->live[s->nlive].off = off;
  s->live[s->nlive].size = bytes;
  s->nlive++;
}

/* Returns 1 if key is an allocation of this simulation. */
static inline int graph_pool_sim_free(graph_pool_sim_t *s, uint64_t key) {
  /* Latest first: the usual graph frees what it allocated last. */
  for (size_t i = s->nlive; i-- > 0;) {
    if (s->live[i].key != key) continue;
    size_t off = s->live[i].off, size = s->live[i].size;
    s->live[i] = s->live[s->nlive - 1];
    s->nlive--;
    if (!s->degraded && !graph_pool_give_back(s, off, size)) s->degraded = 1;
    return 1;
  }
  return 0;
}

#endif /* VGPU_GRAPH_POOL_SIM_H */
