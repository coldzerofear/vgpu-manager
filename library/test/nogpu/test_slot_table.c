/* The growable handle table (include/slot_table.h) behind the graph cost cache,
 * the capture registry and the pool table.
 *
 * What is pinned here:
 *   - lookups find what was inserted, and only that, across growth;
 *   - key2 tells apart records with the same key (the per-thread stream);
 *   - removal never strands a record, which is what the back-shift is for and
 *     what is easiest to get wrong, around the wrap in particular: checked
 *     against a model over random operations on small tables;
 *   - retire_if leaves the table consistent;
 *   - a failed or forbidden growth keeps the table working until it is full;
 *   - aligned handles spread, since masking would pile them on one probe chain;
 *   - abandon leaves a usable empty table without touching the old cells.
 *
 * Needs no GPU, no driver and no CUDA toolkit.
 */
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int g_fail_calloc = 0;
static void *test_calloc(size_t n, size_t size) { return g_fail_calloc ? NULL : calloc(n, size); }
#define SLOT_TABLE_CALLOC test_calloc
#include "include/slot_table.h"

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

typedef struct {
  slot_header_t h;
  uint64_t value;
} cell_t;

/* Device handles come back aligned; test with that, not with 1, 2, 3. */
static uintptr_t handle(size_t i) { return (uintptr_t)(i + 1) * 0x200000u; }

static void check_invariants(const slot_table_t *t, const char *when) {
  size_t used = 0;
  for (size_t i = 0; i < t->capacity; i++) {
    const slot_header_t *h = slot_table_cell(t, i);
    if (h->key == 0) continue;
    used++;
    CHECK(slot_table_find(t, h->key, h->key2) == h, "%s: record in cell %zu unreachable", when, i);
  }
  CHECK(used == t->count, "%s: %zu cells used, count %zu", when, used, t->count);
  CHECK(t->capacity == 0 || t->count < t->capacity, "%s: no empty cell left", when);
}

static void test_empty(void) {
  slot_table_t t = SLOT_TABLE_INIT(cell_t, 8, 1024);
  int created = 7;
  CHECK(slot_table_find(&t, handle(0), 0) == NULL, "found in an empty table");
  CHECK(slot_table_insert(&t, 0, 0, &created) == NULL && created == 0, "key 0 accepted");
  CHECK(slot_table_find(&t, 0, 0) == NULL, "key 0 found");
  free(t.cells);
}

static void test_growth_keeps_everything(void) {
  enum { N = 200000 };
  slot_table_t t = SLOT_TABLE_INIT(cell_t, 8, 1u << 20);
  size_t last_capacity = 0;
  int grew = 0;
  for (size_t i = 0; i < N; i++) {
    int created = 0;
    cell_t *c = (cell_t *)slot_table_insert(&t, handle(i), 0, &created);
    CHECK(c != NULL && created == 1, "insert %zu", i);
    if (c == NULL) break;
    CHECK(c->value == 0, "new cell not zero-filled");
    c->value = i * 3;
    if (t.capacity != last_capacity) { grew++; last_capacity = t.capacity; }
    CHECK(t.count * 10 <= t.capacity * 7, "load above 70%%: %zu/%zu", t.count, t.capacity);
  }
  CHECK(grew > 10, "only grew %d times", grew);
  for (size_t i = 0; i < N; i++) {
    const cell_t *c = (const cell_t *)slot_table_find(&t, handle(i), 0);
    CHECK(c != NULL && c->value == i * 3, "lost record %zu after growth", i);
    if (c == NULL) break;
  }
  CHECK(slot_table_find(&t, handle(N), 0) == NULL, "found a key never inserted");
  /* Inserting what is there returns it, and does not grow or count it again. */
  int created = 1;
  size_t count = t.count;
  CHECK(slot_table_insert(&t, handle(5), 0, &created) == slot_table_find(&t, handle(5), 0) && created == 0,
        "re-insert did not return the record");
  CHECK(t.count == count, "re-insert counted");
  check_invariants(&t, "after growth");
  free(t.cells);
}

static void test_key2(void) {
  slot_table_t t = SLOT_TABLE_INIT(cell_t, 8, 1024);
  int created;
  cell_t *a = (cell_t *)slot_table_insert(&t, 2, 111, &created);
  a->value = 1;
  cell_t *b = (cell_t *)slot_table_insert(&t, 2, 222, &created);
  b->value = 2;
  CHECK(created == 1, "same key, other key2 not a new record");
  CHECK(((cell_t *)slot_table_find(&t, 2, 111))->value == 1, "key2 111");
  CHECK(((cell_t *)slot_table_find(&t, 2, 222))->value == 2, "key2 222");
  CHECK(slot_table_find(&t, 2, 333) == NULL, "key2 333");
  CHECK(slot_table_find(&t, 2, 0) == NULL, "key2 0");
  free(t.cells);
}

/* Random operations against a model. Small tables and a small key range, so
 * collisions and wraps happen all the time. */
static void test_random_against_model(void) {
  enum { KEYS = 300, ROUNDS = 400000 };
  static uint64_t model[KEYS];   /* 0 = absent */
  unsigned seed = 12345;
  for (int cap = 2; cap <= 64; cap *= 2) {
    slot_table_t t = SLOT_TABLE_INIT(cell_t, (size_t)cap, 1u << 12);
    memset(model, 0, sizeof(model));
    size_t live = 0;
    for (long r = 0; r < ROUNDS / 6; r++) {
      size_t k = (size_t)rand_r(&seed) % KEYS;
      int op = rand_r(&seed) % 3;
      if (op == 0) {
        int created;
        cell_t *c = (cell_t *)slot_table_insert(&t, handle(k), 0, &created);
        CHECK(c != NULL, "insert refused");
        if (c == NULL) continue;
        CHECK(created == (model[k] == 0), "created %d, model %llu", created, (unsigned long long)model[k]);
        if (created) live++;
        c->value = (uint64_t)r + 1;
        model[k] = (uint64_t)r + 1;
      } else if (op == 1) {
        void *c = slot_table_find(&t, handle(k), 0);
        CHECK((c != NULL) == (model[k] != 0), "presence of key %zu", k);
        if (c != NULL) {
          slot_table_remove(&t, c);
          model[k] = 0;
          live--;
        }
      } else {
        const cell_t *c = (const cell_t *)slot_table_find(&t, handle(k), 0);
        CHECK((c == NULL ? 0 : c->value) == model[k], "value of key %zu", k);
      }
      CHECK(t.count == live, "count %zu, model %zu", t.count, live);
      if (failures > 20) break;
    }
    check_invariants(&t, "after random operations");
    for (size_t k = 0; k < KEYS; k++) {
      const cell_t *c = (const cell_t *)slot_table_find(&t, handle(k), 0);
      CHECK((c == NULL ? 0 : c->value) == model[k], "final value of key %zu", k);
    }
    free(t.cells);
  }
}

static int odd_values(void *cell, void *ctx) {
  size_t *seen = (size_t *)ctx;
  if (((cell_t *)cell)->value % 2 == 0) return 0;
  (*seen)++;
  return 1;
}
static int everything(void *cell, void *ctx) { (void)cell; (void)ctx; return 1; }

static void test_retire_if(void) {
  enum { N = 5000 };
  slot_table_t t = SLOT_TABLE_INIT(cell_t, 8, 1u << 16);
  int created;
  for (size_t i = 0; i < N; i++) {
    ((cell_t *)slot_table_insert(&t, handle(i), 0, &created))->value = i;
  }
  size_t seen = 0;
  CHECK(slot_table_retire_if(&t, odd_values, &seen) == N / 2, "retired count");
  CHECK(seen == N / 2, "predicate selected %zu records", seen);
  CHECK(t.count == N - N / 2, "count after retire");
  check_invariants(&t, "after retire_if");
  for (size_t i = 0; i < N; i++) {
    CHECK((slot_table_find(&t, handle(i), 0) != NULL) == (i % 2 == 0), "key %zu after retire", i);
  }
  CHECK(slot_table_retire_if(&t, everything, NULL) == N - N / 2, "retire the rest");
  CHECK(t.count == 0, "not empty");
  /* The same with long probe chains: series keys hardly ever collide, so removal
   * would never move a record into the cell being looked at. */
  for (unsigned round = 1; round <= 200; round++) {
    enum { K = 700 };
    static uintptr_t keys[K];
    slot_table_t d = SLOT_TABLE_INIT(cell_t, 1024, 1024);
    unsigned seed = round;
    for (size_t i = 0; i < K; i++) {
      do {
        keys[i] = (((uintptr_t)rand_r(&seed) << 24) ^ (uintptr_t)rand_r(&seed)) << 12;
      } while (keys[i] == 0 || slot_table_find(&d, keys[i], 0) != NULL);
      ((cell_t *)slot_table_insert(&d, keys[i], 0, &created))->value = i;
    }
    size_t selected = 0;
    slot_table_retire_if(&d, odd_values, &selected);
    CHECK(selected == K / 2, "round %u selected %zu", round, selected);
    for (size_t i = 0; i < K; i++) {
      CHECK((slot_table_find(&d, keys[i], 0) != NULL) == (i % 2 == 0), "round %u key %zu", round, i);
    }
    check_invariants(&d, "after retire_if on a dense table");
    free(d.cells);
    if (failures > 20) break;
  }
  /* An empty table that never allocated has nothing to sweep. */
  slot_table_t e = SLOT_TABLE_INIT(cell_t, 8, 64);
  CHECK(slot_table_retire_if(&e, everything, NULL) == 0, "swept an unallocated table");
  free(t.cells);
}

static void test_cannot_grow(void) {
  /* Capped: fills to one short of the cap and refuses from there, finding what
   * it holds and accepting again once a record is gone. */
  slot_table_t t = SLOT_TABLE_INIT(cell_t, 4, 16);
  int created;
  size_t held = 0;
  for (size_t i = 0; i < 100; i++) {
    if (slot_table_insert(&t, handle(i), 0, &created) == NULL) break;
    held++;
  }
  CHECK(t.capacity == 16, "capacity %zu past the cap", t.capacity);
  CHECK(held == 15, "held %zu of 16 cells", held);
  check_invariants(&t, "at the cap");
  CHECK(slot_table_find(&t, handle(1000), 0) == NULL, "absent key must end its probe");
  void *c = slot_table_find(&t, handle(3), 0);
  CHECK(c != NULL, "record lost at the cap");
  if (c != NULL) slot_table_remove(&t, c);
  CHECK(slot_table_insert(&t, handle(200), 0, &created) != NULL, "room not reused");
  free(t.cells);

  /* Out of memory at growth: same, at the size it had. */
  slot_table_t u = SLOT_TABLE_INIT(cell_t, 8, 1u << 20);
  for (size_t i = 0; i < 5; i++) slot_table_insert(&u, handle(i), 0, &created);
  size_t capacity = u.capacity;
  g_fail_calloc = 1;
  held = u.count;
  for (size_t i = 5; i < 100; i++) {
    if (slot_table_insert(&u, handle(i), 0, &created) == NULL) break;
    held++;
  }
  CHECK(u.capacity == capacity, "capacity changed without memory");
  CHECK(held == capacity - 1, "held %zu of %zu cells", held, capacity);
  check_invariants(&u, "out of memory");
  g_fail_calloc = 0;
  CHECK(slot_table_insert(&u, handle(100), 0, &created) != NULL, "did not recover with memory back");
  CHECK(u.capacity > capacity, "did not grow with memory back");
  check_invariants(&u, "recovered");
  free(u.cells);

  /* Out of memory at the very first insert. */
  slot_table_t v = SLOT_TABLE_INIT(cell_t, 8, 64);
  g_fail_calloc = 1;
  CHECK(slot_table_insert(&v, handle(0), 0, &created) == NULL, "inserted with no cells");
  g_fail_calloc = 0;
  CHECK(slot_table_find(&v, handle(0), 0) == NULL, "found with no cells");
  free(v.cells);
}

static void test_spread(void) {
  /* Aligned handles must not form long chains. */
  enum { N = 2800 };   /* just under 70% of 4096 */
  slot_table_t t = SLOT_TABLE_INIT(cell_t, 4096, 4096);
  int created;
  for (size_t i = 0; i < N; i++) slot_table_insert(&t, handle(i), 0, &created);
  size_t longest = 0;
  for (size_t i = 0; i < N; i++) {
    const slot_header_t *h = (const slot_header_t *)slot_table_find(&t, handle(i), 0);
    size_t index = (size_t)((const unsigned char *)h - t.cells) / t.cell_size;
    size_t distance = (index - slot_table_home(t.capacity, h->key, 0)) & (t.capacity - 1);
    if (distance > longest) longest = distance;
  }
  printf("longest probe among %d aligned handles in 4096 cells: %zu\n", N, longest);
  CHECK(longest < 64, "longest probe %zu", longest);
  free(t.cells);

  /* Real handles are not an arithmetic series, which multiplicative hashing
   * spreads perfectly: the same with aligned but random ones. */
  slot_table_t r = SLOT_TABLE_INIT(cell_t, 4096, 4096);
  unsigned seed = 42;
  size_t placed = 0;
  uintptr_t *keys = (uintptr_t *)malloc(N * sizeof(*keys));
  while (placed < N) {
    uintptr_t k = (((uintptr_t)rand_r(&seed) << 24) ^ (uintptr_t)rand_r(&seed)) << 12;
    if (k == 0 || slot_table_find(&r, k, 0) != NULL) continue;
    slot_table_insert(&r, k, 0, &created);
    keys[placed++] = k;
  }
  longest = 0;
  for (size_t i = 0; i < N; i++) {
    const slot_header_t *h = (const slot_header_t *)slot_table_find(&r, keys[i], 0);
    size_t index = (size_t)((const unsigned char *)h - r.cells) / r.cell_size;
    size_t distance = (index - slot_table_home(r.capacity, h->key, 0)) & (r.capacity - 1);
    if (distance > longest) longest = distance;
  }
  printf("longest probe among %d random aligned handles in 4096 cells: %zu\n", N, longest);
  CHECK(longest < 128, "longest probe %zu", longest);
  free(keys);
  free(r.cells);
}

static void test_abandon(void) {
  slot_table_t t = SLOT_TABLE_INIT(cell_t, 8, 1024);
  int created;
  for (size_t i = 0; i < 20; i++) slot_table_insert(&t, handle(i), 0, &created);
  unsigned char *old = t.cells;
  size_t old_capacity = t.capacity;
  unsigned char *before = (unsigned char *)malloc(old_capacity * t.cell_size);
  memcpy(before, old, old_capacity * t.cell_size);
  slot_table_abandon(&t);
  CHECK(t.count == 0 && t.capacity == 0 && t.cells == NULL, "not empty");
  CHECK(memcmp(before, old, old_capacity * t.cell_size) == 0, "old cells were written");
  CHECK(slot_table_find(&t, handle(1), 0) == NULL, "found after abandon");
  CHECK(slot_table_insert(&t, handle(1), 0, &created) != NULL && created == 1, "unusable after abandon");
  CHECK(t.capacity == 8, "did not restart at the initial size");
  free(before);
  free(old);
  free(t.cells);
}

/* Threads churning under one mutex, as the callers do: for ASan and for growth
 * racing lookups. */
enum { THREADS = 8, PER_THREAD = 20000 };
static slot_table_t g_shared = SLOT_TABLE_INIT(cell_t, 8, 1u << 20);
static pthread_mutex_t g_shared_lock = PTHREAD_MUTEX_INITIALIZER;
static int g_thread_errors = 0;

static void *churn(void *arg) {
  size_t base = (size_t)(uintptr_t)arg * 1000000u;
  for (size_t i = 0; i < PER_THREAD; i++) {
    int created;
    pthread_mutex_lock(&g_shared_lock);
    cell_t *c = (cell_t *)slot_table_insert(&g_shared, handle(base + i), 0, &created);
    if (c == NULL) __atomic_add_fetch(&g_thread_errors, 1, __ATOMIC_RELAXED);
    else c->value = base + i;
    pthread_mutex_unlock(&g_shared_lock);
    pthread_mutex_lock(&g_shared_lock);
    c = (cell_t *)slot_table_find(&g_shared, handle(base + i), 0);
    if (c == NULL || c->value != base + i) __atomic_add_fetch(&g_thread_errors, 1, __ATOMIC_RELAXED);
    if (c != NULL && i % 2 == 0) slot_table_remove(&g_shared, c);
    pthread_mutex_unlock(&g_shared_lock);
  }
  return NULL;
}

static void test_threads(void) {
  pthread_t t[THREADS];
  for (size_t i = 0; i < THREADS; i++) pthread_create(&t[i], NULL, churn, (void *)(uintptr_t)(i + 1));
  for (size_t i = 0; i < THREADS; i++) pthread_join(t[i], NULL);
  CHECK(g_thread_errors == 0, "%d errors", g_thread_errors);
  CHECK(g_shared.count == THREADS * PER_THREAD / 2, "count %zu", g_shared.count);
  check_invariants(&g_shared, "after threads");
  free(g_shared.cells);
}

int main(void) {
  test_empty();
  test_growth_keeps_everything();
  test_key2();
  test_random_against_model();
  test_retire_if();
  test_cannot_grow();
  test_spread();
  test_abandon();
  test_threads();
  if (failures) {
    printf("FAILED: %d\n", failures);
    return 1;
  }
  printf("PASS\n");
  return 0;
}
