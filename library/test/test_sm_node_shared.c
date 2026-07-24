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

  int rc = 0;
  rc |= test_no_lost_tokens(r);
  rc |= test_election_single_winner(r);

  munmap(r, SM_NODE_FILE_SIZE);
  unlink(path);
  printf("%s\n", rc ? "FAILED" : "ALL PASS");
  return rc;
}
