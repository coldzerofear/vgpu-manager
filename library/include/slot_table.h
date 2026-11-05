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
 * A growable handle -> record table, as pure functions.
 *
 * Header-inline like vmem_table.h, so the no-GPU tests exercise the shipped
 * code. Mechanics only: no locking and no logging, the caller holds its own
 * mutex around every call here.
 *
 * WHY
 *
 * The graph cost cache, the capture registry and the pool table were fixed
 * arrays searched linearly. Past the array's size a graph went unthrottled and
 * unchecked, and vLLM's default piecewise mode captures one graph per submodule
 * per batch size, which reaches the thousands. Only raising the size was no way
 * out: the cost cache is searched on every cuGraphLaunch, and 4096 linear slots
 * measured ~1.9 us per launch on a ~1.5 us call.
 *
 * SHAPE
 *
 * Open addressing with linear probing, doubling past 70% load. Removal shifts
 * the following cells back rather than leaving tombstones, so a long-running
 * process churning handles does not degrade. A zero key marks an empty cell,
 * so callers must never use 0 as a key (handles are not NULL here, and the
 * registry maps the NULL stream to a special handle first).
 *
 * The cells are allocated at the first insert: a statically initialised table
 * is a valid empty one, because hack/check_no_constructors.sh forbids
 * constructors in this library.
 *
 * Failure to grow is not an error: the table keeps working at the size it has,
 * until it is one cell short of full, and only then does an insert return NULL.
 * The caller decides what losing that record costs.
 *
 * A cell pointer is valid only until the next insert or remove on the table.
 */

#ifndef _VGPU_SLOT_TABLE_H_
#define _VGPU_SLOT_TABLE_H_

#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#ifndef SLOT_TABLE_CALLOC
#define SLOT_TABLE_CALLOC calloc   /* overridable to test allocation failure */
#endif

/* Every record type starts with this. key2 is 0 unless a handle needs a second
 * component (the registry's per-thread default stream). */
typedef struct {
  uintptr_t key;
  uintptr_t key2;
} slot_header_t;

typedef struct {
  unsigned char *cells;
  size_t capacity;           /* a power of two, 0 until the first insert */
  size_t count;
  size_t cell_size;
  size_t initial_capacity;   /* a power of two, >= 2 */
  size_t max_capacity;       /* a power of two */
} slot_table_t;

#define SLOT_TABLE_INIT(cell_type, initial, max) \
  { NULL, 0, 0, sizeof(cell_type), (initial), (max) }

/* Fibonacci hashing, keeping the high bits: see vmem_table_hash() for why the
 * low bits of a handle are useless on their own. */
static inline size_t slot_table_home(size_t capacity, uintptr_t key, uintptr_t key2) {
  uint64_t mixed = ((uint64_t)key ^ ((uint64_t)key2 * 0xC2B2AE3D27D4EB4FULL)) * 0x9E3779B97F4A7C15ULL;
  return (size_t)(mixed >> (64 - __builtin_ctzl((unsigned long)capacity)));
}

static inline slot_header_t *slot_table_cell(const slot_table_t *t, size_t index) {
  return (slot_header_t *)(void *)(t->cells + index * t->cell_size);
}

/* The record for (key, key2), or NULL. */
static inline void *slot_table_find(const slot_table_t *t, uintptr_t key, uintptr_t key2) {
  if (t->capacity == 0 || key == 0) return NULL;
  size_t mask = t->capacity - 1;
  for (size_t i = slot_table_home(t->capacity, key, key2);; i = (i + 1) & mask) {
    slot_header_t *h = slot_table_cell(t, i);
    if (h->key == 0) return NULL;
    if (h->key == key && h->key2 == key2) return h;
  }
}

static inline int slot_table_grow(slot_table_t *t) {
  size_t capacity = t->capacity == 0 ? t->initial_capacity : t->capacity * 2;
  if (capacity > t->max_capacity) return 0;
  unsigned char *cells = (unsigned char *)SLOT_TABLE_CALLOC(capacity, t->cell_size);
  if (cells == NULL) return 0;
  size_t mask = capacity - 1;
  for (size_t i = 0; i < t->capacity; i++) {
    const slot_header_t *h = slot_table_cell(t, i);
    if (h->key == 0) continue;
    size_t j = slot_table_home(capacity, h->key, h->key2);
    while (((slot_header_t *)(void *)(cells + j * t->cell_size))->key != 0) j = (j + 1) & mask;
    memcpy(cells + j * t->cell_size, h, t->cell_size);
  }
  free(t->cells);
  t->cells = cells;
  t->capacity = capacity;
  return 1;
}

/* The record for (key, key2), created zero-filled if absent (*created says
 * which). NULL only if it is absent and there is no room for it. */
static inline void *slot_table_insert(slot_table_t *t, uintptr_t key, uintptr_t key2, int *created) {
  *created = 0;
  if (key == 0) return NULL;
  void *found = slot_table_find(t, key, key2);
  if (found != NULL) return found;
  if ((t->count + 1) * 10 > t->capacity * 7) slot_table_grow(t);
  /* One cell always stays empty: it is what ends a probe. */
  if (t->count + 1 >= t->capacity) return NULL;
  size_t mask = t->capacity - 1;
  size_t i = slot_table_home(t->capacity, key, key2);
  while (slot_table_cell(t, i)->key != 0) i = (i + 1) & mask;
  slot_header_t *h = slot_table_cell(t, i);
  h->key = key;
  h->key2 = key2;
  t->count++;
  *created = 1;
  return h;
}

/* Remove a record slot_table_find() or slot_table_insert() returned. */
static inline void slot_table_remove(slot_table_t *t, void *cell) {
  size_t mask = t->capacity - 1;
  size_t hole = (size_t)((unsigned char *)cell - t->cells) / t->cell_size;
  /* Move back every following cell whose probe passes through the hole, so no
   * later lookup stops short at an empty cell. */
  for (size_t j = (hole + 1) & mask;; j = (j + 1) & mask) {
    const slot_header_t *h = slot_table_cell(t, j);
    if (h->key == 0) break;
    size_t home = slot_table_home(t->capacity, h->key, h->key2);
    if (((j - home) & mask) >= ((j - hole) & mask)) {
      memcpy(slot_table_cell(t, hole), h, t->cell_size);
      hole = j;
    }
  }
  memset(slot_table_cell(t, hole), 0, t->cell_size);
  t->count--;
}

/* Remove every record should_retire() selects, returning how many. The predicate
 * sees each record intact, so it can release what the record owns; it may see a
 * record it declined more than once (removal can move a record to a later cell),
 * so it must not count the ones it declines. */
static inline size_t slot_table_retire_if(slot_table_t *t, int (*should_retire)(void *cell, void *ctx),
                                          void *ctx) {
  size_t retired = 0;
  for (size_t i = 0; i < t->capacity;) {
    slot_header_t *h = slot_table_cell(t, i);
    if (h->key != 0 && should_retire(h, ctx)) {
      slot_table_remove(t, h);
      retired++;
      continue;   /* the cell may hold a record moved back: look again */
    }
    i++;
  }
  return retired;
}

/* For a forked child: forget the cells without touching them. Another thread may
 * have been mid-insert or mid-growth in the parent, so what the child inherited
 * cannot be trusted to be a consistent table or a live pointer. The child's copy
 * of the old cells stays allocated, once per fork generation, and writing nothing
 * also keeps those pages shared with the parent. */
static inline void slot_table_abandon(slot_table_t *t) {
  t->cells = NULL;
  t->capacity = 0;
  t->count = 0;
}

#endif /* _VGPU_SLOT_TABLE_H_ */
