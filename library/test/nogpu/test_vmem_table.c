/* The virtual-memory record table (include/vmem_table.h).
 *
 * These records used to live on one linked list, so the three point lookups
 * that dominate the path -- the dedup in malloc_gpu_virt_memory_graph,
 * get_gpu_virt_memory_type and free_gpu_virt_memory -- were each O(M), and a
 * caller retiring every record in turn paid O(M^2). That is the shape
 * HAMi-core's allocator is slow for (Project-HAMi/HAMi#3291); our exposure was
 * narrower only because of which paths feed the table, which is not a property
 * worth relying on.
 *
 * What is pinned here:
 *   - the lookups find what was inserted, and only that;
 *   - the hash spreads GRANULARITY-ALIGNED pointers, which is the whole reason
 *     for the multiply: masking the low bits of a real dptr would put every
 *     record in one bucket and re-create the linear scan inside the table;
 *   - remove hands the record back intact, because the caller has to read the
 *     device out of it to discharge the charge;
 *   - retire_if's remove-while-iterating leaves the table consistent, which is
 *     the loop most likely to be got wrong;
 *   - count tracks reality, since the invariant check below reads it.
 *
 * Needs no GPU, no driver and no CUDA toolkit.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "include/vmem_table.h"

static int failures = 0;

#define CHECK(cond, ...)                                                       \
  do {                                                                         \
    if (!(cond)) {                                                             \
      printf("FAIL %s:%d: ", __func__, __LINE__);                              \
      printf(__VA_ARGS__);                                                     \
      printf("\n");                                                            \
      failures++;                                                              \
    }                                                                          \
  } while (0)

/* Mirrors what loader.c does: malloc, populate, insert. */
static memory_node_t *make(CUdeviceptr dptr, size_t bytes, int type, CUgraph graph,
                           int host_index) {
  memory_node_t *node = (memory_node_t *)malloc(sizeof(*node));
  if (node == NULL) {
    printf("FAIL: out of memory in test helper\n");
    exit(1);
  }
  node->dptr = dptr;
  node->bytes = bytes;
  node->type = type;
  node->graph = graph;
  node->host_index = host_index;
  node->hnext = NULL;
  return node;
}

static void test_empty(void) {
  vmem_table_t table;
  memset(&table, 0, sizeof(table));

  /* A zero-initialised table must be a valid empty table: loader.c declares it
   * as a plain static with no init step, because a constructor is forbidden in
   * this library and a lazy guard would sit on every access. */
  CHECK(vmem_table_count(&table) == 0, "fresh count");
  CHECK(vmem_table_find(&table, 0x1000) == NULL, "fresh find");
  CHECK(vmem_table_remove(&table, 0x1000) == NULL, "fresh remove");
  CHECK(vmem_table_retire_if(&table, NULL, NULL) == 0, "fresh retire-all");

  /* NULL table is tolerated rather than crashing: these run on the allocation
   * path, where a crash is worse than a missed record. */
  CHECK(vmem_table_find(NULL, 0x1000) == NULL, "NULL find");
  CHECK(vmem_table_remove(NULL, 0x1000) == NULL, "NULL remove");
  CHECK(vmem_table_count(NULL) == 0, "NULL count");
  CHECK(vmem_table_retire_if(NULL, NULL, NULL) == 0, "NULL retire");
}

static void test_insert_find_remove(void) {
  vmem_table_t table;
  memory_node_t *got;
  memset(&table, 0, sizeof(table));

  vmem_table_insert(&table, make(0x200000, 4096, MEMORY_TYPE_UVA_SYNC, NULL, 3));
  vmem_table_insert(&table, make(0x400000, 8192, MEMORY_TYPE_UVA_ASYNC, NULL, 1));
  CHECK(vmem_table_count(&table) == 2, "count after 2 inserts = %zu",
        vmem_table_count(&table));

  got = vmem_table_find(&table, 0x200000);
  CHECK(got != NULL, "find first");
  if (got != NULL) {
    CHECK(got->bytes == 4096 && got->type == MEMORY_TYPE_UVA_SYNC && got->host_index == 3,
          "first record intact: bytes=%zu type=%d dev=%d", got->bytes, got->type,
          got->host_index);
  }
  CHECK(vmem_table_find(&table, 0x400000) != NULL, "find second");
  CHECK(vmem_table_find(&table, 0x300000) == NULL, "absent dptr must not match");

  /* remove must hand the record back still readable: free_gpu_virt_memory
   * reads bytes and host_index out of it AFTER unlinking, to discharge the
   * right device. */
  got = vmem_table_remove(&table, 0x200000);
  CHECK(got != NULL, "remove returns the record");
  if (got != NULL) {
    CHECK(got->bytes == 4096 && got->host_index == 3,
          "removed record still readable: bytes=%zu dev=%d", got->bytes, got->host_index);
    CHECK(got->hnext == NULL, "removed record must not still point into a bucket");
    free(got);
  }
  CHECK(vmem_table_count(&table) == 1, "count after remove = %zu", vmem_table_count(&table));
  CHECK(vmem_table_find(&table, 0x200000) == NULL, "removed dptr is gone");
  CHECK(vmem_table_find(&table, 0x400000) != NULL, "sibling survives the remove");

  CHECK(vmem_table_remove(&table, 0x200000) == NULL, "double remove is a miss");
  CHECK(vmem_table_count(&table) == 1, "double remove must not decrement again");

  vmem_table_retire_if(&table, NULL, NULL);
}

/* The regression guard for the hash itself.
 *
 * Driver pointers come back aligned to at least 256 bytes and in practice to
 * the 2 MiB allocation granularity, so `dptr & (BUCKETS-1)` would be zero for
 * essentially every record and the table would degenerate into one long chain
 * -- the exact linear scan it replaced. */
static void test_hash_spreads_aligned_pointers(void) {
  enum { N = 2048, GRANULARITY = 2u << 20 };
  size_t seen[VMEM_TABLE_BUCKETS];
  size_t used_buckets = 0, longest = 0;
  int i;

  memset(seen, 0, sizeof(seen));
  for (i = 0; i < N; i++) {
    CUdeviceptr dptr = (CUdeviceptr)0x7F0000000000ULL + (CUdeviceptr)i * GRANULARITY;
    size_t bucket = vmem_table_hash(dptr);
    CHECK(bucket < VMEM_TABLE_BUCKETS, "bucket %zu out of range", bucket);
    seen[bucket]++;
  }
  for (i = 0; i < (int)VMEM_TABLE_BUCKETS; i++) {
    if (seen[i] != 0) {
      used_buckets++;
    }
    if (seen[i] > longest) {
      longest = seen[i];
    }
  }
  /* A perfect spread of 2048 keys over 4096 buckets occupies ~1600 of them
   * (the birthday bound). Demanding a quarter leaves room for any reasonable
   * hash while still failing hard on the degenerate case, which would report
   * used_buckets == 1 and longest == 2048. */
  CHECK(used_buckets > VMEM_TABLE_BUCKETS / 4,
        "2 MiB-aligned pointers must spread: %zu/%u buckets used", used_buckets,
        VMEM_TABLE_BUCKETS);
  CHECK(longest < 16, "longest chain %zu suggests clustering", longest);
}

/* Collisions are expected; they must not confuse the lookups. Two dptrs that
 * land in the same bucket are found both constructed by brute force, so the
 * test does not depend on the hash constant. */
static void test_collisions(void) {
  vmem_table_t table;
  CUdeviceptr a = 0, b = 0;
  CUdeviceptr probe;
  memory_node_t *got;

  memset(&table, 0, sizeof(table));
  a = (CUdeviceptr)0x200000;
  for (probe = 0x400000; probe < 0x400000 + (CUdeviceptr)200000 * 0x1000; probe += 0x1000) {
    if (vmem_table_hash(probe) == vmem_table_hash(a) && probe != a) {
      b = probe;
      break;
    }
  }
  CHECK(b != 0, "could not construct a collision; hash or search range changed");
  if (b == 0) {
    return;
  }

  vmem_table_insert(&table, make(a, 111, MEMORY_TYPE_UVA_SYNC, NULL, 0));
  vmem_table_insert(&table, make(b, 222, MEMORY_TYPE_CAPTURE, NULL, 1));
  CHECK(vmem_table_count(&table) == 2, "both colliding records counted");

  got = vmem_table_find(&table, a);
  CHECK(got != NULL && got->bytes == 111, "colliding find a");
  got = vmem_table_find(&table, b);
  CHECK(got != NULL && got->bytes == 222, "colliding find b");

  /* Remove the one at the chain HEAD and the one behind it, in both orders,
   * since a pointer-to-pointer unlink gets the head case wrong most often. */
  got = vmem_table_remove(&table, b);
  CHECK(got != NULL && got->bytes == 222, "remove chain head");
  free(got);
  got = vmem_table_find(&table, a);
  CHECK(got != NULL && got->bytes == 111, "the other colliding record survives");
  got = vmem_table_remove(&table, a);
  CHECK(got != NULL, "remove last in chain");
  free(got);
  CHECK(vmem_table_count(&table) == 0, "count back to zero");
  CHECK(vmem_table_find(&table, a) == NULL && vmem_table_find(&table, b) == NULL,
        "both gone");
}

struct sweep_ctx {
  CUgraph graph;
  size_t total;
  int visits;
};

static int retire_graph(const memory_node_t *node, void *ctx) {
  struct sweep_ctx *sweep = (struct sweep_ctx *)ctx;
  sweep->visits++;
  if (node->type == MEMORY_TYPE_CAPTURE && node->graph == sweep->graph) {
    sweep->total += node->bytes;
    return 1;
  }
  return 0;
}

/* Mirrors free_gpu_virt_memory_by_graph: a predicate sweep that retires some
 * records and keeps others, accumulating from the records while they are still
 * intact. */
static void test_retire_if_predicate(void) {
  vmem_table_t table;
  CUgraph g1 = (CUgraph)0x11;
  CUgraph g2 = (CUgraph)0x22;
  struct sweep_ctx sweep;
  size_t retired;
  int i;

  memset(&table, 0, sizeof(table));
  /* Enough records, at the real granularity, to land across many buckets --
   * a sweep that only works within one bucket would pass on a tiny table. */
  for (i = 0; i < 300; i++) {
    CUdeviceptr dptr = (CUdeviceptr)0x200000 + (CUdeviceptr)i * (2u << 20);
    int type = (i % 3 == 0) ? MEMORY_TYPE_CAPTURE : MEMORY_TYPE_UVA_SYNC;
    CUgraph graph = (i % 3 == 0) ? ((i % 2 == 0) ? g1 : g2) : NULL;
    vmem_table_insert(&table, make(dptr, (size_t)(i + 1), type, graph, i % 4));
  }
  CHECK(vmem_table_count(&table) == 300, "300 inserted, count=%zu",
        vmem_table_count(&table));

  memset(&sweep, 0, sizeof(sweep));
  sweep.graph = g1;
  retired = vmem_table_retire_if(&table, retire_graph, &sweep);

  CHECK(sweep.visits == 300, "predicate must see every record, saw %d", sweep.visits);
  CHECK(retired > 0, "nothing retired");
  CHECK(vmem_table_count(&table) == 300 - retired,
        "count after retire: %zu, expected %zu", vmem_table_count(&table), 300 - retired);

  /* Everything matching g1 is gone, everything else is still findable -- the
   * check that the remove-while-iterating walk did not drop bystanders. */
  for (i = 0; i < 300; i++) {
    CUdeviceptr dptr = (CUdeviceptr)0x200000 + (CUdeviceptr)i * (2u << 20);
    int is_g1 = (i % 3 == 0) && (i % 2 == 0);
    const memory_node_t *got = vmem_table_find(&table, dptr);
    if (is_g1) {
      CHECK(got == NULL, "record %d (graph g1) should be retired", i);
    } else {
      CHECK(got != NULL, "record %d must survive the sweep", i);
      if (got != NULL) {
        CHECK(got->bytes == (size_t)(i + 1), "record %d corrupted: bytes=%zu", i,
              got->bytes);
      }
    }
  }

  /* Re-sweeping the same graph is a no-op, and the g2 records are still there
   * to be retired separately. */
  memset(&sweep, 0, sizeof(sweep));
  sweep.graph = g1;
  CHECK(vmem_table_retire_if(&table, retire_graph, &sweep) == 0, "re-sweep retires nothing");

  memset(&sweep, 0, sizeof(sweep));
  sweep.graph = g2;
  CHECK(vmem_table_retire_if(&table, retire_graph, &sweep) > 0, "g2 sweep retires");

  /* retire-all is what the fork handler uses. */
  retired = vmem_table_retire_if(&table, NULL, NULL);
  CHECK(vmem_table_count(&table) == 0, "retire-all empties the table");
  CHECK(retired > 0, "retire-all retired %zu", retired);
  for (i = 0; i < 300; i++) {
    CUdeviceptr dptr = (CUdeviceptr)0x200000 + (CUdeviceptr)i * (2u << 20);
    CHECK(vmem_table_find(&table, dptr) == NULL, "record %d gone after retire-all", i);
  }
}

/* Insert, remove and re-insert the same dptr repeatedly, the way the
 * cuMemAllocAsync bridge does (record, synchronize, retire) and the way the
 * driver reusing an address does. The table must not accumulate anything. */
static void test_churn_leaves_nothing_behind(void) {
  vmem_table_t table;
  int round;

  memset(&table, 0, sizeof(table));
  for (round = 0; round < 1000; round++) {
    CUdeviceptr dptr = (CUdeviceptr)0x200000 + (CUdeviceptr)(round % 7) * (2u << 20);
    memory_node_t *got;

    /* loader.c looks before inserting; a producer that skipped the lookup
     * would shadow the previous record, so mirror the real order. */
    got = vmem_table_find(&table, dptr);
    if (got != NULL) {
      got->bytes = (size_t)round;
    } else {
      vmem_table_insert(&table, make(dptr, (size_t)round, MEMORY_TYPE_ASYNC_BRIDGE, NULL, 0));
    }
    CHECK(vmem_table_count(&table) <= 7, "churn grew the table to %zu",
          vmem_table_count(&table));

    got = vmem_table_remove(&table, dptr);
    CHECK(got != NULL, "round %d: remove what was just recorded", round);
    if (got != NULL) {
      free(got);
    }
  }
  CHECK(vmem_table_count(&table) == 0, "churn left %zu records", vmem_table_count(&table));
}

int main(void) {
  test_empty();
  test_insert_find_remove();
  test_hash_spreads_aligned_pointers();
  test_collisions();
  test_retire_if_predicate();
  test_churn_leaves_nothing_behind();

  if (failures != 0) {
    printf("vmem table: %d failure(s)\n", failures);
    return 1;
  }
  printf("vmem table: all checks passed\n");
  return 0;
}
