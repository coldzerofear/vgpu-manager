/* Multi-process checks for the vmem_node ledger. No GPU, no CUDA.
 *
 * The sm_node region already has test_sm_node_shared.c beside this file. The
 * vmem region is the one with the properties that region does NOT have, and
 * they are the two that go wrong silently:
 *
 *   - it is the only shared region holding a SLOT ARRAY that gets compacted
 *     (swap-with-last) when a process dies, so a record can be moved out from
 *     under a concurrent reader, and
 *   - it is a CROSS-LANGUAGE ABI: pkg/config/vmem computes the byte-range lock
 *     offset by hand, and hook.h says what happens when that is wrong -- "fcntl
 *     locks taken on non-overlapping byte ranges, mutual exclusion silently
 *     gone, and torn reads reported as valid metrics". No error anywhere.
 *
 * TestVMemoryLayoutMatchesC / TestVMemoryLockOffsetMatchesC on the Go side pin
 * the LAYOUT. Nothing pins the BEHAVIOUR that layout is supposed to produce.
 * That is what this covers:
 *
 *   [0] the region ABI is the size and shape hook.h declares, and the two ways
 *       of deriving the per-device lock offset agree -- the base-offset mistake
 *       the Go side is one typo away from, checked where it can be checked.
 *   [1] N processes registering and accumulating concurrently end up with one
 *       slot per PID and exact totals -- no lost updates, no duplicate slots.
 *   [2] the per-device lock, taken at the REAL computed offsets, excludes on
 *       the same device and does not on another. This is the assertion that
 *       fails if the stride or base offset is ever wrong.
 *   [3] compaction moves whole records: every survivor keeps its own `used`,
 *       including when the dead entries form a run at the tail (the case a
 *       forward loop with swap-from-last silently skips).
 *   [4] an exited-but-unwaited child still answers kill(pid,0)==0 -- asserted
 *       to be REAL, so the is_zombie_proc() arm of the reaper cannot be
 *       "simplified" away by someone who thinks liveness alone is enough.
 *   [5] the MAX_PIDS guard refuses instead of writing one past the array, which
 *       would land on processes_size itself.
 *
 * Mirrors the protocol rather than linking it: the real functions live in
 * loader.c, which pulls in the whole CUDA surface. Same approach, and same
 * caveat, as test_sm_node_shared.c -- when loader.c's slot handling changes,
 * the mirrors below have to change with it.
 *
 * Run via `make test` or `make test-nogpu`.
 */
#include "hook.h"

#include <sys/mman.h>
#include <sys/wait.h>
#include <fcntl.h>
#include <errno.h>
#include <signal.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#ifndef F_OFD_SETLK
#define F_OFD_SETLK 37
#endif
#ifndef F_OFD_SETLKW
#define F_OFD_SETLKW 38
#endif

/* Exactly lock.c's GET_VMEMORY_LOCK_OFFSET. Deliberately the same expression,
 * not a re-derivation: the whole point of [0] is to compare this against a
 * hand-computed stride form, which is how the Go side has to do it. */
#define VMEM_LOCK_OFF(dev) offsetof(device_vmemory_t, devices[dev].lock_byte)

/* The hand-computed form, i.e. what getVmemoryLockOffset() must reproduce:
 * where the array starts + which element + where the byte sits inside it. The
 * first term is the one that is easy to forget; without it every lock lands one
 * cache line short of the range C locks. */
#define VMEM_LOCK_OFF_MANUAL(dev)                       \
  ((size_t)offsetof(device_vmemory_t, devices) +        \
   (size_t)(dev) * sizeof(device_vmem_used_t) +         \
   (size_t)offsetof(device_vmem_used_t, lock_byte))

static const char *g_region_path;
static device_vmemory_t *g_region;

/* ---- mirrors of the production protocol -------------------------------- */

static int ofd_fcntl_(int fd, int wait, struct flock *fl) {
  int ret = fcntl(fd, wait ? F_OFD_SETLKW : F_OFD_SETLK, fl);
  if (ret != -1 || errno != EINVAL) return ret;
  return fcntl(fd, wait ? F_SETLKW : F_SETLK, fl);
}

/* mirrors device_vmem_write_lock(): fresh fd per acquisition, byte-range lock
 * at the device's lock_byte, blocking. The fresh-fd-per-lock shape is part of
 * what is under test -- it is why these have to be OFD locks. */
static int vmem_write_lock(int dev) {
  int fd = open(g_region_path, O_RDWR | O_CLOEXEC);
  if (fd < 0) return -1;
  struct flock fl;
  memset(&fl, 0, sizeof(fl));
  fl.l_type = F_WRLCK;
  fl.l_whence = SEEK_SET;
  fl.l_start = (off_t)VMEM_LOCK_OFF(dev);
  fl.l_len = 1;
  if (ofd_fcntl_(fd, 1, &fl) == -1) {
    close(fd);
    return -1;
  }
  return fd;
}

static void vmem_unlock(int fd, int dev) {
  if (fd < 0) return;
  struct flock fl;
  memset(&fl, 0, sizeof(fl));
  fl.l_type = F_UNLCK;
  fl.l_whence = SEEK_SET;
  fl.l_start = (off_t)VMEM_LOCK_OFF(dev);
  fl.l_len = 1;
  ofd_fcntl_(fd, 0, &fl);
  close(fd);
}

/* mirrors the accounting body of malloc_gpu_virt_memory_graph(): scan by PID
 * under the write lock, accumulate if present, append if not. Returns 0 on
 * success, -1 when the table is full. Note there is no cached slot index
 * anywhere -- every access re-resolves by PID, which is what makes compaction
 * safe here. [3] is what keeps that true. */
static int register_or_add(int dev, int pid, ssize_t delta) {
  unsigned int n = g_region->devices[dev].processes_size;
  for (unsigned int i = 0; i < n; i++) {
    if (g_region->devices[dev].processes[i].pid == pid) {
      size_t cur = g_region->devices[dev].processes[i].used;
      if (delta >= 0) {
        g_region->devices[dev].processes[i].used = cur + (size_t)delta;
      } else {
        size_t dec = (size_t)(-delta);
        g_region->devices[dev].processes[i].used = (cur >= dec) ? (cur - dec) : 0;
      }
      return 0;
    }
  }
  if (n >= MAX_PIDS) return -1;          /* mirrors the overflow guard */
  g_region->devices[dev].processes[n].pid = pid;
  g_region->devices[dev].processes[n].used = (delta > 0) ? (size_t)delta : 0;
  g_region->devices[dev].processes_size++;
  return 0;
}

/* mirrors rm_vmem_node_by_non_existent_device_pid(): backward scan, swap the
 * last live slot into the hole, clear the tail, shrink. Backward matters: the
 * element swapped in always comes from an index above i, which this loop has
 * already examined. is_dead is a hook so [3] can drive a fixed pattern. */
typedef int (*dead_fn)(int pid);

static void reap_dead(int dev, dead_fn is_dead) {
  unsigned int n = g_region->devices[dev].processes_size;
  for (int i = (int)n - 1; i >= 0; i--) {
    int pid = g_region->devices[dev].processes[i].pid;
    if (!is_dead(pid)) continue;
    g_region->devices[dev].processes[i] = g_region->devices[dev].processes[n - 1];
    g_region->devices[dev].processes[n - 1].pid = 0;
    g_region->devices[dev].processes[n - 1].used = 0;
    g_region->devices[dev].processes_size--;
    n--;
  }
}

/* ---- 0. ABI, and the two derivations of the lock offset agree ----------- */
static int test_abi_and_lock_offsets(void) {
  int ok = 1;

  if (offsetof(device_vmemory_t, magic) != 0) ok = 0;
  if (offsetof(device_vmemory_t, devices) != CACHELINE_SIZE) ok = 0;
  if (sizeof(device_vmemory_t) > VMEM_NODE_FILE_SIZE) ok = 0;

  /* The cross-language hazard, checked in the language that gets it right for
   * free. If these two ever disagree, the offsetof() form is authoritative and
   * getVmemoryLockOffset() is what needs fixing. */
  int derivations_agree = 1;
  int distinct_and_in_file = 1;
  size_t prev = 0;
  for (int d = 0; d < MAX_DEVICE_COUNT; d++) {
    size_t via_offsetof = (size_t)VMEM_LOCK_OFF(d);
    size_t via_stride = VMEM_LOCK_OFF_MANUAL(d);
    if (via_offsetof != via_stride) derivations_agree = 0;
    /* Strictly increasing => per-device ranges are distinct, and a lock past
     * EOF is exactly the silent failure this guards: fcntl happily locks a
     * byte range beyond the end of the file. */
    if (d > 0 && via_offsetof <= prev) distinct_and_in_file = 0;
    if (via_offsetof >= VMEM_NODE_FILE_SIZE) distinct_and_in_file = 0;
    prev = via_offsetof;
  }
  if (!derivations_agree || !distinct_and_in_file) ok = 0;

  printf("  [0] ABI: region=%zuB devices@%zu stride=%zuB file=%d "
         "lock_off[0]=%zu lock_off[%d]=%zu\n",
         sizeof(device_vmemory_t), offsetof(device_vmemory_t, devices),
         sizeof(device_vmem_used_t), VMEM_NODE_FILE_SIZE,
         (size_t)VMEM_LOCK_OFF(0), MAX_DEVICE_COUNT - 1,
         (size_t)VMEM_LOCK_OFF(MAX_DEVICE_COUNT - 1));
  printf("      derivations-agree=%d offsets-distinct-and-in-file=%d -> %s\n",
         derivations_agree, distinct_and_in_file, ok ? "PASS" : "FAIL");
  return ok ? 0 : 1;
}

/* ---- 1. concurrent registration: one slot per PID, exact totals --------- */
static int test_concurrent_registration(void) {
  const int NPROC = 16;
  const int ITERS = 500;
  const ssize_t CHUNK = 4096;
  const int DEV = 0;

  memset(&g_region->devices[DEV], 0, sizeof(device_vmem_used_t));

  /* Start barrier, same reason as test_sm_node_shared's: fork is sequential
   * and the loop is short, so without it the children barely overlap and the
   * test would pass even with the locking removed. */
  int32_t *go = (int32_t *)&g_region->devices[MAX_DEVICE_COUNT - 1].processes[0].pid;
  __atomic_store_n(go, 0, __ATOMIC_RELAXED);

  for (int i = 0; i < NPROC; i++) {
    pid_t pid = fork();
    if (pid == 0) {
      while (__atomic_load_n(go, __ATOMIC_ACQUIRE) == 0) { /* spin */ }
      int me = (int)getpid();
      for (int k = 0; k < ITERS; k++) {
        int fd = vmem_write_lock(DEV);
        if (fd < 0) _exit(2);
        int rc = register_or_add(DEV, me, CHUNK);
        vmem_unlock(fd, DEV);
        if (rc != 0) _exit(3);
      }
      _exit(0);
    }
    if (pid < 0) { perror("fork"); return 1; }
  }
  usleep(50000);
  __atomic_store_n(go, 1, __ATOMIC_RELEASE);

  int children_ok = 1;
  for (int i = 0; i < NPROC; i++) {
    int st = 0;
    wait(&st);
    if (!WIFEXITED(st) || WEXITSTATUS(st) != 0) children_ok = 0;
  }

  unsigned int n = g_region->devices[DEV].processes_size;
  int no_dupes = 1;
  int exact_totals = 1;
  for (unsigned int i = 0; i < n; i++) {
    if (g_region->devices[DEV].processes[i].used != (size_t)(ITERS * CHUNK)) {
      exact_totals = 0;
    }
    for (unsigned int j = i + 1; j < n; j++) {
      if (g_region->devices[DEV].processes[i].pid ==
          g_region->devices[DEV].processes[j].pid) {
        no_dupes = 0;
      }
    }
  }
  int ok = children_ok && no_dupes && exact_totals && n == (unsigned int)NPROC;
  printf("  [1] %d procs x %d charges: slots=%u expect=%d one-slot-per-pid=%d "
         "exact-per-pid-total=%d -> %s\n",
         NPROC, ITERS, n, NPROC, no_dupes, exact_totals, ok ? "PASS" : "FAIL");
  return ok ? 0 : 1;
}

/* ---- 2. the per-device lock excludes, at the real offsets ---------------
 * The failure this exists for does not look like a failure: if the offset math
 * drifts, every party still "takes the lock" and gets a success return -- they
 * are just locking bytes nobody else locks. Asserting the DENIAL is the only
 * way to notice.
 *
 * The holder locks via offsetof() (what lock.c does); the probe locks via the
 * stride form (what getVmemoryLockOffset() has to do). So this is the
 * cross-language hazard played out for real: if the two derivations ever
 * diverge -- the dropped base offset being the obvious way -- the probe stops
 * being denied and this goes red, instead of Go and C quietly locking disjoint
 * ranges in production. Device 0 vs 1 additionally pins that the stride really
 * separates devices rather than collapsing them onto one byte. */
static int test_lock_excludes(void) {
  int held = vmem_write_lock(0);        /* offsetof() form, via lock.c's mirror */
  if (held < 0) { printf("  [2] could not take initial lock -> FAIL\n"); return 1; }

  int probe = open(g_region_path, O_RDWR | O_CLOEXEC);
  if (probe < 0) { vmem_unlock(held, 0); return 1; }

  struct flock fl;
  memset(&fl, 0, sizeof(fl));
  fl.l_type = F_WRLCK;
  fl.l_whence = SEEK_SET;
  fl.l_len = 1;

  fl.l_start = (off_t)VMEM_LOCK_OFF_MANUAL(0);   /* the Go-side derivation */
  int same_device_denied = (ofd_fcntl_(probe, 0, &fl) == -1);

  fl.l_start = (off_t)VMEM_LOCK_OFF_MANUAL(1);
  int other_device_free = (ofd_fcntl_(probe, 0, &fl) == 0);
  if (other_device_free) {
    fl.l_type = F_UNLCK;
    ofd_fcntl_(probe, 0, &fl);
  }
  close(probe);

  vmem_unlock(held, 0);

  int probe2 = open(g_region_path, O_RDWR | O_CLOEXEC);
  memset(&fl, 0, sizeof(fl));
  fl.l_type = F_WRLCK;
  fl.l_whence = SEEK_SET;
  fl.l_len = 1;
  fl.l_start = (off_t)VMEM_LOCK_OFF(0);
  int free_after_release = (ofd_fcntl_(probe2, 0, &fl) == 0);
  close(probe2);

  int ok = same_device_denied && other_device_free && free_after_release;
  printf("  [2] byte-range lock: same-device-denied=%d other-device-free=%d "
         "released-after-unlock=%d -> %s%s\n",
         same_device_denied, other_device_free, free_after_release,
         ok ? "PASS" : "FAIL",
         same_device_denied ? "" : "  (offsets may not overlap -- check "
                                   "GET_VMEMORY_LOCK_OFFSET and its Go twin)");
  return ok ? 0 : 1;
}

/* ---- 3. compaction moves whole records --------------------------------- */
static int g_dead[16];
static int g_dead_n;

static int fixed_is_dead(int pid) {
  for (int i = 0; i < g_dead_n; i++) {
    if (g_dead[i] == pid) return 1;
  }
  return 0;
}

static int test_compaction_preserves_records(void) {
  const int DEV = 2;
  const int N = 10;
  memset(&g_region->devices[DEV], 0, sizeof(device_vmem_used_t));

  /* Distinct `used` per PID, so the check below proves the whole RECORD was
   * moved and not just the pid -- a swap that copied one field would keep every
   * pid present and still corrupt every accounting number. */
  for (int i = 0; i < N; i++) {
    g_region->devices[DEV].processes[i].pid = 1001 + i;
    g_region->devices[DEV].processes[i].used = (size_t)(1001 + i) * 100;
  }
  g_region->devices[DEV].processes_size = N;

  /* 1008/1009/1010 is a RUN at the tail. A forward loop with swap-from-last
   * skips the element it swaps in, so a tail run is where that bug shows up;
   * 1002 in the middle covers the ordinary case. */
  g_dead_n = 0;
  g_dead[g_dead_n++] = 1002;
  g_dead[g_dead_n++] = 1008;
  g_dead[g_dead_n++] = 1009;
  g_dead[g_dead_n++] = 1010;

  reap_dead(DEV, fixed_is_dead);

  const int expect_pids[] = {1001, 1003, 1004, 1005, 1006, 1007};
  const unsigned int expect_n = sizeof(expect_pids) / sizeof(expect_pids[0]);
  unsigned int n = g_region->devices[DEV].processes_size;

  int all_survivors_intact = 1;
  for (unsigned int e = 0; e < expect_n; e++) {
    int seen = 0;
    for (unsigned int i = 0; i < n; i++) {
      if (g_region->devices[DEV].processes[i].pid != expect_pids[e]) continue;
      seen++;
      if (g_region->devices[DEV].processes[i].used != (size_t)expect_pids[e] * 100) {
        all_survivors_intact = 0;
      }
    }
    if (seen != 1) all_survivors_intact = 0;      /* lost or duplicated */
  }
  int no_dead_left = 1;
  for (unsigned int i = 0; i < n; i++) {
    if (fixed_is_dead(g_region->devices[DEV].processes[i].pid)) no_dead_left = 0;
  }
  /* The vacated tail must be cleared, not left holding a stale copy of the
   * record that was swapped down -- a reader walking past processes_size (or a
   * later append that trusts the slot is blank) would otherwise see a live pid. */
  int tail_cleared = (g_region->devices[DEV].processes[n].pid == 0 &&
                      g_region->devices[DEV].processes[n].used == 0);

  int ok = (n == expect_n) && all_survivors_intact && no_dead_left && tail_cleared;
  printf("  [3] compaction: slots=%u expect=%u survivors-intact=%d "
         "no-dead-left=%d tail-cleared=%d -> %s\n",
         n, expect_n, all_survivors_intact, no_dead_left, tail_cleared,
         ok ? "PASS" : "FAIL");
  return ok ? 0 : 1;
}

/* ---- 4. the zombie hazard is real --------------------------------------
 * Asserted rather than assumed, in the style of test_sm_node_shared's fork
 * check: a child that has exited but not been waited for is still a process as
 * far as kill(2) is concerned. A reaper that tested liveness alone would keep
 * its slot -- and its memory charge -- forever, which is why
 * rm_vmem_node_by_non_existent_device_pid has a separate is_zombie_proc arm.
 * If this ever prints hazard=0, that arm has become genuinely redundant; until
 * then it must not be removed. */
static int test_zombie_hazard_is_real(void) {
  pid_t kid = fork();
  if (kid == 0) _exit(0);
  if (kid < 0) { perror("fork"); return 1; }
  usleep(100000);                        /* it has exited; not waited for yet */

  int zombie_looks_alive = (kill(kid, 0) == 0);

  int st = 0;
  waitpid(kid, &st, 0);
  int reaped_looks_dead = (kill(kid, 0) == -1 && errno == ESRCH);

  int ok = zombie_looks_alive && reaped_looks_dead;
  printf("  [4] zombie hazard: unwaited-child-answers-kill0=%d "
         "waited-child-is-ESRCH=%d -> %s%s\n",
         zombie_looks_alive, reaped_looks_dead, ok ? "PASS" : "FAIL",
         zombie_looks_alive ? "  (hazard confirmed: is_zombie_proc MUST stay)" : "");
  return ok ? 0 : 1;
}

/* ---- 5. the MAX_PIDS guard refuses instead of overrunning ---------------
 * processes[MAX_PIDS] is not spare space -- it is where processes_size itself
 * lives. Without the guard a full table does not merely lose an entry, it
 * corrupts its own length. */
static int test_full_table_guard(void) {
  const int DEV = 3;
  memset(&g_region->devices[DEV], 0, sizeof(device_vmem_used_t));

  for (unsigned int i = 0; i < MAX_PIDS; i++) {
    g_region->devices[DEV].processes[i].pid = 90000 + (int)i;
    g_region->devices[DEV].processes[i].used = 1;
  }
  g_region->devices[DEV].processes_size = MAX_PIDS;
  g_region->devices[DEV].lock_byte = 0xA5;             /* canary just past the array */

  int refused = (register_or_add(DEV, 4242, 4096) == -1);
  int size_intact = (g_region->devices[DEV].processes_size == MAX_PIDS);
  int canary_intact = (g_region->devices[DEV].lock_byte == 0xA5);
  /* An existing PID must still be chargeable when the table is full -- the
   * guard gates the APPEND, not the update. */
  int update_still_works = (register_or_add(DEV, 90000, 4096) == 0) &&
                           (g_region->devices[DEV].processes[0].used == 4097);

  int ok = refused && size_intact && canary_intact && update_still_works;
  printf("  [5] full table (%d slots): append-refused=%d size-intact=%d "
         "canary-intact=%d update-still-works=%d -> %s\n",
         MAX_PIDS, refused, size_intact, canary_intact, update_still_works,
         ok ? "PASS" : "FAIL");
  return ok ? 0 : 1;
}

int main(void) {
  printf("vmem_node ledger checks (no GPU required)\n");

  char path[] = "/tmp/vmem_node_test_XXXXXX";
  int tfd = mkstemp(path);
  if (tfd < 0) { perror("mkstemp"); return 1; }
  if (ftruncate(tfd, VMEM_NODE_FILE_SIZE) != 0) { perror("ftruncate"); return 1; }
  void *p = mmap(NULL, VMEM_NODE_FILE_SIZE, PROT_READ | PROT_WRITE, MAP_SHARED, tfd, 0);
  if (p == MAP_FAILED) { perror("mmap"); return 1; }
  close(tfd);

  g_region_path = path;
  g_region = (device_vmemory_t *)p;
  g_region->magic = VMEM_NODE_MAGIC;
  g_region->layout_version = VMEM_NODE_LAYOUT_VERSION;
  g_region->region_size = (uint32_t)sizeof(device_vmemory_t);
  g_region->device_count = MAX_DEVICE_COUNT;

  int rc = 0;
  rc |= test_abi_and_lock_offsets();
  rc |= test_concurrent_registration();
  rc |= test_lock_excludes();
  rc |= test_compaction_preserves_records();
  rc |= test_zombie_hazard_is_real();
  rc |= test_full_table_guard();

  munmap(p, VMEM_NODE_FILE_SIZE);
  unlink(path);
  printf("%s\n", rc ? "FAILED" : "ALL PASS");
  return rc;
}
