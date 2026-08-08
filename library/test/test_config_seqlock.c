/*
 * Per-device seqlock: does a reader ever see a torn device_t?
 *
 * This exercises the exact read/write dance that get_device_snapshot() (C
 * reader, loader.c) and ResourceDataT.ModifyDevice() (Go writer,
 * pkg/config/vgpu) use on resource_data_t.devices[i]. It does NOT link the
 * library or touch CUDA -- the property under test is the concurrency
 * algorithm, so the seqlock read and the seqlock write are reproduced here
 * verbatim over a plain shared struct and hammered from many threads.
 *
 * A writer thread flips the device between two SELF-CONSISTENT states, writing
 * the fields one at a time with a barrier between them so a torn intermediate
 * is observable mid-update. The invariant b == 2*a + 1 holds in both states, so
 * any snapshot that mixes one state's `a` with the other's `b` violates it.
 *
 *   - seqlock readers must observe ZERO violations (the assertion).
 *   - a naive (no-seqlock) control reader counts violations too, purely to
 *     demonstrate that torn windows really do occur -- so a zero from the
 *     seqlock reader means the lock worked, not that there was nothing to catch.
 *
 * Build/run standalone:  gcc -O2 -pthread test_config_seqlock.c -o t && ./t
 */
#define _GNU_SOURCE
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

#define CACHELINE 128
#define READERS   6
#define ROUNDS    2000000u

/* Same shape as device_t: seq at offset 0, one cache line. a/b stand in for the
 * co-varying config fields (e.g. total_memory + memory_oversold). */
typedef struct {
  uint32_t seq;
  uint32_t _pad;
  uint64_t a;
  uint64_t b;
  uint8_t  _rest[CACHELINE - 24];
} __attribute__((aligned(CACHELINE))) dev_t_;

static dev_t_ g_dev;
static volatile int g_stop = 0;

static inline void cpu_relax(void) {
#if defined(__x86_64__)
  __builtin_ia32_pause();
#else
  __asm__ __volatile__("" ::: "memory");
#endif
}

/* --- writer: mirrors ModifyDevice (seq++, mutate, seq++) --- */
static void *writer(void *arg) {
  (void)arg;
  uint64_t vals[2] = { 0x1111111111111111ULL, 0x3333333333333333ULL };
  unsigned i = 0;
  while (!g_stop) {
    uint64_t a = vals[i & 1u];
    uint64_t b = 2u * a + 1u;
    __atomic_add_fetch(&g_dev.seq, 1, __ATOMIC_ACQ_REL);   /* even -> odd */
    g_dev.a = a;
    __atomic_signal_fence(__ATOMIC_SEQ_CST);               /* expose a torn window */
    g_dev.b = b;
    __atomic_add_fetch(&g_dev.seq, 1, __ATOMIC_ACQ_REL);   /* odd  -> even */
    i++;
  }
  return NULL;
}

/* --- reader: mirrors get_device_snapshot's seqlock fast path --- */
static uint64_t g_violations = 0;

static void *reader(void *arg) {
  (void)arg;
  uint64_t local_viol = 0;
  for (uint32_t r = 0; r < ROUNDS; r++) {
    uint64_t a, b;
    for (;;) {
      uint32_t s1 = __atomic_load_n(&g_dev.seq, __ATOMIC_ACQUIRE);
      if (s1 & 1u) { cpu_relax(); continue; }
      a = g_dev.a;
      b = g_dev.b;
      __atomic_thread_fence(__ATOMIC_ACQUIRE);
      uint32_t s2 = __atomic_load_n(&g_dev.seq, __ATOMIC_ACQUIRE);
      if (s1 == s2) break;
      cpu_relax();
    }
    if (b != 2u * a + 1u) local_viol++;   /* torn: must never happen */
  }
  __atomic_add_fetch(&g_violations, local_viol, __ATOMIC_RELAXED);
  return NULL;
}

/* --- control: naive read, no seqlock -- counts tears to prove they exist --- */
static uint64_t g_control_viol = 0;

static void *control(void *arg) {
  (void)arg;
  uint64_t viol = 0;
  for (uint32_t r = 0; r < ROUNDS; r++) {
    uint64_t a = g_dev.a;
    __atomic_signal_fence(__ATOMIC_SEQ_CST);
    uint64_t b = g_dev.b;
    if (b != 2u * a + 1u) viol++;
  }
  __atomic_add_fetch(&g_control_viol, viol, __ATOMIC_RELAXED);
  return NULL;
}

int main(void) {
  g_dev.a = 0x1111111111111111ULL;
  g_dev.b = 2u * g_dev.a + 1u;

  pthread_t wt, ct, rt[READERS];
  pthread_create(&wt, NULL, writer, NULL);
  pthread_create(&ct, NULL, control, NULL);
  for (int i = 0; i < READERS; i++) pthread_create(&rt[i], NULL, reader, NULL);

  for (int i = 0; i < READERS; i++) pthread_join(rt[i], NULL);
  pthread_join(ct, NULL);
  g_stop = 1;
  pthread_join(wt, NULL);

  printf("seqlock readers: %llu violation(s) over %d x %u reads\n",
         (unsigned long long)g_violations, READERS, ROUNDS);
  printf("control (naive): %llu tear(s) seen over %u reads "
         "(nonzero just proves torn windows exist)\n",
         (unsigned long long)g_control_viol, ROUNDS);

  if (g_violations != 0) {
    printf("Result: FAIL -- seqlock let a torn device_t through\n");
    return 1;
  }
  printf("Result: PASS -- seqlock readers never saw a torn device_t\n");
  return 0;
}
