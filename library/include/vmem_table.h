/*
Copyright 2024-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
 * The virtual-memory record table: dptr -> memory_node_t, as pure functions.
 *
 * Header-inline for the same reason as sm_delta.h and array_size.h: loader.c
 * keeps its inlining AND the data structure is reachable from the no-GPU
 * tests, so CI exercises the shipped code rather than a replica. The header
 * holds the MECHANICS only -- no locking, no logging, no shared-region
 * updates. Every function here assumes the caller already holds
 * g_memory_node_lock, which is what makes it testable.
 *
 * WHY IT IS NOT A LINKED LIST ANY MORE
 *
 * These records used to live on one doubly-linked list walked from the head.
 * Three of the five accesses are point lookups by dptr
 * (malloc_gpu_virt_memory_graph's dedup, get_gpu_virt_memory_type,
 * free_gpu_virt_memory), so each was O(M) in the number of live records --
 * and a caller that retires every record in turn therefore paid O(M^2).
 *
 * That is exactly the shape HAMi-core's allocator is slow for: its
 * remove_chunk() searches one list per cuMemFree, so when PyTorch's caching
 * allocator hits an OOM and calls release_cached_blocks() -- freeing every
 * cached block in one go -- the flush is quadratic. On an 8-GPU vLLM startup
 * that is the dominant cost (Project-HAMi/HAMi#3291: ~1210 s of engine init
 * against ~313 s with enforcement off, concentrated in the profiling phase
 * where 80 such OOM retries happen).
 *
 * Our exposure to that was narrower, not absent: the table is only fed by the
 * oversold UVA fallback, explicit cuMemAllocManaged, graph-capture accounting
 * and the cuMemAllocAsync bridge -- so a PyTorch workload that uses none of
 * those keeps M at zero or a small transient. The quadratic behaviour was
 * real but hidden behind that usage, which is a bad thing to rely on: bucket
 * the lookups and the hazard is gone for every path at once, present and
 * future.
 *
 * SHAPE
 *
 * Open hashing, fixed bucket count, intrusive singly-linked chains. Chosen so
 * a ZERO-INITIALISED table is a valid empty table: a bucket array of pointers
 * needs no constructor, which matters because hack/check_no_constructors.sh
 * forbids __attribute__((constructor)) in this library, and a lazily
 * initialised table would need a guard on every access. The old code relied
 * on the same property via a statically LIST_HEAD_INIT'd sentinel.
 *
 * No resizing. The bucket count is fixed at 4096, which costs 32 KiB of BSS
 * and keeps the average chain under ten even at ten thousand live records --
 * far past anything these four producers generate. Growing the table would
 * mean rehashing under the lock for no reachable benefit, and a fixed array
 * is what lets the whole thing be statically initialised.
 */

#ifndef _VGPU_VMEM_TABLE_H_
#define _VGPU_VMEM_TABLE_H_

#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>

#include "include/hook.h"

/* Power of two; the hash below takes the top BITS of a 64-bit mix. */
#define VMEM_TABLE_BUCKET_BITS 12
#define VMEM_TABLE_BUCKETS     (1u << VMEM_TABLE_BUCKET_BITS)

typedef struct vmem_table {
  memory_node_t *buckets[VMEM_TABLE_BUCKETS];
  /* Live record count. Kept rather than derived so the invariant tests (and
   * any future metric) can read it without walking the table. */
  size_t count;
} vmem_table_t;

/* Fibonacci hashing: multiply by 2^64/phi and keep the HIGH bits.
 *
 * Masking the low bits of dptr directly would be close to useless here --
 * device pointers come back from the driver aligned to at least 256 bytes and
 * in practice to the 2 MiB allocation granularity, so the low 12 bits are
 * almost always zero and every record would land in bucket 0. The multiply
 * spreads those high-order differences down into the bits we keep.
 *
 * Cast through uint64_t because CUdeviceptr is 32-bit on 32-bit builds
 * (cuda-subset.h), where the shift below would otherwise be undefined. */
static inline size_t vmem_table_hash(CUdeviceptr dptr) {
  uint64_t mixed = (uint64_t)dptr * 0x9E3779B97F4A7C15ULL;
  return (size_t)(mixed >> (64 - VMEM_TABLE_BUCKET_BITS));
}

/* The record for dptr, or NULL.
 *
 * The table parameter is deliberately NOT const: the record comes back
 * mutable because malloc_gpu_virt_memory_graph() updates it in place when the
 * driver re-issues a dptr, and a const table handing out a mutable record
 * would be a const violation dressed up as a cast. */
static inline memory_node_t *vmem_table_find(vmem_table_t *table, CUdeviceptr dptr) {
  memory_node_t *node;

  if (table == NULL) {
    return NULL;
  }
  for (node = table->buckets[vmem_table_hash(dptr)]; node != NULL; node = node->hnext) {
    if (node->dptr == dptr) {
      return node;
    }
  }
  return NULL;
}

/* Link an already-populated node in. The caller must have established that
 * node->dptr is not present (every producer looks first, to update in place
 * instead of shadowing), because a duplicate would leave the older record
 * unreachable to find() yet still counted and still holding its charge. */
static inline void vmem_table_insert(vmem_table_t *table, memory_node_t *node) {
  size_t bucket;

  if (table == NULL || node == NULL) {
    return;
  }
  bucket = vmem_table_hash(node->dptr);
  node->hnext = table->buckets[bucket];
  table->buckets[bucket] = node;
  table->count++;
}

/* Unlink the record for dptr and hand it back, still intact, for the caller
 * to read and free -- the charge it describes has to be discharged against
 * the device recorded IN the node, so the node must outlive the unlink.
 * Returns NULL when dptr is not present. */
static inline memory_node_t *vmem_table_remove(vmem_table_t *table, CUdeviceptr dptr) {
  memory_node_t **link;

  if (table == NULL) {
    return NULL;
  }
  for (link = &table->buckets[vmem_table_hash(dptr)]; *link != NULL; link = &(*link)->hnext) {
    memory_node_t *node = *link;
    if (node->dptr == dptr) {
      *link = node->hnext;
      node->hnext = NULL;
      table->count--;
      return node;
    }
  }
  return NULL;
}

/* Retire every record should_retire() selects, freeing it.
 *
 * The predicate is called with the record INTACT and before it is unlinked,
 * so it is also where a caller accumulates whatever the record was holding
 * (free_gpu_virt_memory_by_graph sums per-device bytes this way). Returns how
 * many records were retired.
 *
 * This is the one access that is not a point lookup: selecting by graph, or
 * by "everything", is a predicate over all records and so is inherently a
 * full sweep. That is fine -- it runs once per cuStreamEndCapture and once
 * per fork, never per allocation. Keeping the remove-while-iterating idiom
 * here, in one tested place, rather than open-coding it at each call site is
 * the point: the pointer-to-pointer walk below is where that kind of loop
 * usually goes wrong.
 *
 * A NULL predicate retires everything, which is what the fork handler wants.
 */
static inline size_t vmem_table_retire_if(vmem_table_t *table,
                                          int (*should_retire)(const memory_node_t *, void *),
                                          void *ctx) {
  size_t bucket;
  size_t retired = 0;

  if (table == NULL) {
    return 0;
  }
  for (bucket = 0; bucket < VMEM_TABLE_BUCKETS; bucket++) {
    memory_node_t **link = &table->buckets[bucket];
    while (*link != NULL) {
      memory_node_t *node = *link;
      if (should_retire == NULL || should_retire(node, ctx)) {
        *link = node->hnext; /* leaves link pointing at the next slot */
        table->count--;
        retired++;
        free(node);
        continue;
      }
      link = &node->hnext;
    }
  }
  return retired;
}

/* Live record count, for the invariant tests and for observability. */
static inline size_t vmem_table_count(const vmem_table_t *table) {
  return table == NULL ? 0 : table->count;
}

#endif /* _VGPU_VMEM_TABLE_H_ */
