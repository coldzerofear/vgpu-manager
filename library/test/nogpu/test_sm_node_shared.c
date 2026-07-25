/* Multi-process checks for the sm_node shared token bucket. No GPU, no CUDA.
 *
 * Covers the two properties the design rests on and that cannot be reasoned
 * about statically:
 *   1. N processes CASing one MAP_SHARED counter lose no updates -- i.e. the
 *      container cannot over-launch by racing.
 *   2. The per-cycle refill election admits at most one winner per period, so
 *      N watchers cannot supply the bucket N times over.
 */
#include "hook.h"

#include <sys/mman.h>
#include <sys/wait.h>
#include <fcntl.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <time.h>

#define CAS_(p, o, n) __sync_bool_compare_and_swap((p), (o), (n))
#define SM_REFILL_PERIOD_NS (90LL * 1000000LL)

static int64_t mono_ns(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (int64_t)ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

static sm_node_region_t *map_region(const char *path) {
  int fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0644);
  if (fd < 0) { perror("open"); exit(1); }
  if (ftruncate(fd, SM_NODE_FILE_SIZE) != 0) { perror("ftruncate"); exit(1); }
  void *p = mmap(NULL, SM_NODE_FILE_SIZE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  if (p == MAP_FAILED) { perror("mmap"); exit(1); }
  close(fd);
  return (sm_node_region_t *)p;
}

/* ---- 1. no lost updates across processes ------------------------------- */
static int test_no_lost_tokens(sm_node_region_t *r) {
  const int   NPROC = 8;
  const int   ITERS = 400000;
  const int64_t KERNEL = 3;
  const int64_t START = (int64_t)NPROC * ITERS * KERNEL;

  volatile int64_t *bucket = (volatile int64_t *)&r->devices[0].cur_cuda_cores;
  *bucket = START;

  /* Start barrier. Without it the children run essentially serially -- fork is
   * sequential and the loop is short -- so the counter is never actually
   * contended and the test would pass even with a non-atomic deduction.
   * (Verified: it did.) Every child must be spinning before any of them
   * deducts, or this measures nothing. */
  int32_t *go = &r->devices[0].excl_streak;
  __atomic_store_n(go, 0, __ATOMIC_RELAXED);

  for (int i = 0; i < NPROC; i++) {
    pid_t pid = fork();
    if (pid == 0) {
      while (__atomic_load_n(go, __ATOMIC_ACQUIRE) == 0) { /* spin */ }
      for (int k = 0; k < ITERS; k++) {
        int64_t before, after;
        do {                      /* mirrors rate_limiter's deduction loop */
          before = *bucket;
          after  = before - KERNEL;
        } while (!CAS_(bucket, before, after));
      }
      _exit(0);
    }
    if (pid < 0) { perror("fork"); return 1; }
  }
  usleep(50000);                                  /* let every child reach the spin */
  __atomic_store_n(go, 1, __ATOMIC_RELEASE);      /* release them together */
  for (int i = 0; i < NPROC; i++) wait(NULL);

  int64_t expect = START - (int64_t)NPROC * ITERS * KERNEL;
  printf("  [1] %d procs x %d deductions: bucket=%ld expect=%ld -> %s\n",
         NPROC, ITERS, (long)*bucket, (long)expect,
         (*bucket == expect) ? "PASS" : "FAIL");
  return (*bucket == expect) ? 0 : 1;
}

/* ---- 2. election admits one winner per period -------------------------- */
static int test_election_single_winner(sm_node_region_t *r) {
  const int NPROC = 8;
  const int64_t RUN_NS = 900LL * 1000000LL;      /* ~10 periods */

  int64_t *stamp = &r->devices[1].last_refill_ns;
  /* Shared win counter, incremented atomically by every winner. */
  int64_t *wins  = &r->devices[2].cur_cuda_cores;
  __atomic_store_n(stamp, mono_ns(), __ATOMIC_RELAXED);
  __atomic_store_n(wins, 0, __ATOMIC_RELAXED);

  int64_t deadline = mono_ns() + RUN_NS;
  for (int i = 0; i < NPROC; i++) {
    pid_t pid = fork();
    if (pid == 0) {
      while (mono_ns() < deadline) {
        /* mirrors sm_try_claim_refill() */
        int64_t last = __atomic_load_n(stamp, __ATOMIC_RELAXED);
        int64_t now  = mono_ns();
        if (now - last >= SM_REFILL_PERIOD_NS && CAS_(stamp, last, now)) {
          __atomic_add_fetch(wins, 1, __ATOMIC_RELAXED);
        }
      }
      _exit(0);
    }
    if (pid < 0) { perror("fork"); return 1; }
  }
  for (int i = 0; i < NPROC; i++) wait(NULL);

  int64_t got = __atomic_load_n(wins, __ATOMIC_RELAXED);
  int64_t max_allowed = RUN_NS / SM_REFILL_PERIOD_NS + 1;
  /* Lower bound: the election must not starve the bucket either. */
  int64_t min_expected = RUN_NS / SM_REFILL_PERIOD_NS - 1;
  int ok = (got <= max_allowed) && (got >= min_expected);
  printf("  [2] %d procs hammering election for %ldms: wins=%ld allowed=[%ld..%ld] -> %s\n",
         NPROC, (long)(RUN_NS / 1000000), (long)got,
         (long)min_expected, (long)max_allowed, ok ? "PASS" : "FAIL");
  return ok ? 0 : 1;
}

#ifndef F_OFD_SETLK
#define F_OFD_SETLK 37
#endif

static int try_lock_byte(int fd, int byte) {
  struct flock fl;
  memset(&fl, 0, sizeof(fl));
  fl.l_type = F_WRLCK; fl.l_whence = SEEK_SET; fl.l_start = byte; fl.l_len = 1;
  return fcntl(fd, F_OFD_SETLK, &fl) == 0;
}

/* ---- 3. one sampling owner per device, and takeover when it dies -------- */
static int test_sampling_ownership(const char *lockpath) {
  int fd = open(lockpath, O_RDWR | O_CREAT, 0644);
  if (fd < 0) { perror("open lock"); return 1; }

  pid_t owner = fork();
  if (owner == 0) {
    int f = open(lockpath, O_RDWR);
    if (!try_lock_byte(f, 0)) _exit(2);
    usleep(400000);
    _exit(0);                      /* exit releases the lock via the kernel */
  }
  usleep(150000);                  /* let the owner take it */

  int denied_while_alive = !try_lock_byte(fd, 0);
  int other_device_free  = try_lock_byte(fd, 5);   /* per-device independence */

  int st = 0;
  waitpid(owner, &st, 0);
  usleep(50000);
  int acquired_after_death = try_lock_byte(fd, 0);

  int ok = denied_while_alive && other_device_free && acquired_after_death;
  printf("  [3] ownership: denied-while-owner-alive=%d other-device-free=%d "
         "took-over-after-death=%d -> %s\n",
         denied_while_alive, other_device_free, acquired_after_death,
         ok ? "PASS" : "FAIL");
  close(fd);
  return ok ? 0 : 1;
}

/* ---- 4. fork() shares the lock's open file description ------------------
 * This is the hazard child_after_fork() exists to defuse: an OFD lock belongs
 * to the description, not the process, so a forked child keeps its parent's
 * lock alive after the parent dies -- and no standby can ever take over. The
 * test asserts the hazard is REAL, so the close in child_after_fork can never
 * be "cleaned up" by someone who thinks it is redundant. */
static int test_fork_keeps_lock_alive(const char *lockpath) {
  pid_t owner = fork();
  if (owner == 0) {
    int f = open(lockpath, O_RDWR);
    if (!try_lock_byte(f, 2)) _exit(2);
    pid_t kid = fork();
    if (kid == 0) {
      usleep(700000);              /* keeps the INHERITED fd open */
      _exit(0);
    }
    _exit(0);                      /* owner dies immediately */
  }
  int st = 0;
  waitpid(owner, &st, 0);
  usleep(200000);                  /* owner is gone; grandchild still holds fd */

  int fd = open(lockpath, O_RDWR);
  int still_held = !try_lock_byte(fd, 2);
  usleep(700000);                  /* grandchild exits, dropping the last ref */
  int free_after = try_lock_byte(fd, 2);

  int ok = still_held && free_after;
  printf("  [4] fork hazard: lock-outlives-dead-owner=%d released-once-child-exits=%d"
         " -> %s%s\n", still_held, free_after, ok ? "PASS" : "FAIL",
         still_held ? "  (hazard confirmed: child_after_fork MUST close this fd)" : "");
  close(fd);
  return ok ? 0 : 1;
}

/* ---- 5. the owner wins refills; standbys stay a backstop ----------------
 * Mirrors sm_try_claim_refill's asymmetric thresholds. Two properties matter
 * and they pull in opposite directions, which is why both are asserted:
 *   - the owner should win essentially every refill (that is the point), and
 *   - the refill RATE must not drop, or the bucket starves.
 * The second is the one that catches the subtle failure: if no process holds
 * the 1x ticket, everyone waits 2x and the container is refilled half as
 * often, which looks like nothing at all until throughput drops. */
static int test_owner_wins_refill(sm_node_region_t *r) {
  const int64_t RUN_NS = 900LL * 1000000LL;
  const int64_t STANDBY_PERIOD_NS = 2 * SM_REFILL_PERIOD_NS;

  int64_t *stamp = &r->devices[3].last_refill_ns;
  int64_t *owner_wins = &r->devices[3].share;
  int64_t *standby_wins = &r->devices[4].share;
  __atomic_store_n(stamp, mono_ns(), __ATOMIC_RELAXED);
  __atomic_store_n(owner_wins, 0, __ATOMIC_RELAXED);
  __atomic_store_n(standby_wins, 0, __ATOMIC_RELAXED);

  int64_t deadline = mono_ns() + RUN_NS;
  for (int i = 0; i < 4; i++) {
    pid_t pid = fork();
    if (pid == 0) {
      int is_owner = (i == 0);
      int64_t threshold = is_owner ? SM_REFILL_PERIOD_NS : STANDBY_PERIOD_NS;
      /* Give the OWNER the latest phase deliberately. If the owner started
       * first it would win on timing alone and this test would pass even with
       * the asymmetry removed -- verified: it did. Starting it last means the
       * only reason it can win is the lower threshold, so removing the
       * asymmetry makes a standby win and the assertion below fails. */
      usleep((useconds_t)((3 - i) * 25000));
      while (mono_ns() < deadline) {
        int64_t last = __atomic_load_n(stamp, __ATOMIC_RELAXED);
        int64_t now = mono_ns();
        if (now - last >= threshold && CAS_(stamp, last, now)) {
          __atomic_add_fetch(is_owner ? owner_wins : standby_wins, 1, __ATOMIC_RELAXED);
        }
        usleep(50000);             /* poll well under the threshold so the
                                    * owner reliably claims at its own cadence
                                    * rather than aliasing against it */
      }
      _exit(0);
    }
    if (pid < 0) { perror("fork"); return 1; }
  }
  for (int i = 0; i < 4; i++) wait(NULL);

  int64_t owned = __atomic_load_n(owner_wins, __ATOMIC_RELAXED);
  int64_t stood = __atomic_load_n(standby_wins, __ATOMIC_RELAXED);
  int64_t total = owned + stood;
  /* The failure this guards is "nobody holds the 1x ticket, so the whole
   * container refills at 2x". Compare against the rate that regime would
   * produce rather than against the ideal, so the bound separates the two
   * outcomes with margin on both sides instead of sitting on the boundary. */
  int64_t halved_rate = RUN_NS / STANDBY_PERIOD_NS;
  int ok = (owned > stood) && (total > halved_rate + 1);
  printf("  [5] owner priority: owner=%ld standby=%ld total=%ld (must exceed halved-rate %ld) -> %s\n",
         (long)owned, (long)stood, (long)total, (long)(halved_rate + 1),
         ok ? "PASS" : "FAIL");
  return ok ? 0 : 1;
}

int main(void) {
  printf("sm_node shared-bucket checks (no GPU required)\n");
  printf("  [0] ABI: dev=%zuB region=%zuB devices@%zu file=%d -> %s\n",
         sizeof(sm_node_dev_t), sizeof(sm_node_region_t),
         offsetof(sm_node_region_t, devices), SM_NODE_FILE_SIZE,
         (sizeof(sm_node_dev_t) == CACHELINE_SIZE &&
          sizeof(sm_node_region_t) <= SM_NODE_FILE_SIZE) ? "PASS" : "FAIL");

  char path[] = "/tmp/sm_node_test_XXXXXX";
  int tfd = mkstemp(path);
  if (tfd >= 0) close(tfd);
  sm_node_region_t *r = map_region(path);

  char lockpath[] = "/tmp/sm_node_lock_XXXXXX";
  int lfd = mkstemp(lockpath);
  if (lfd >= 0) close(lfd);

  int rc = 0;
  rc |= test_no_lost_tokens(r);
  rc |= test_election_single_winner(r);
  rc |= test_sampling_ownership(lockpath);
  rc |= test_fork_keeps_lock_alive(lockpath);
  rc |= test_owner_wins_refill(r);

  munmap(r, SM_NODE_FILE_SIZE);
  unlink(path);
  unlink(lockpath);
  printf("%s\n", rc ? "FAILED" : "ALL PASS");
  return rc;
}
