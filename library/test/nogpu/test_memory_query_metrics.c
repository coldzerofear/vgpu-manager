/* The per-allocation memory-usage query timer in src/metrics.c.
 *
 * This counter exists to produce a number we cannot obtain without a GPU: how
 * long get_used_gpu_memory() really takes inside the device lock, and how
 * often the allocation path calls it. A metric that lies is worse than no
 * metric, so what this file checks is that the recorded figures stay sane on
 * the edge cases, not merely that nothing crashes.
 *
 * The counters are file-static, so the assertions read the numbers back out of
 * the INFO line metrics.c emits at each power-of-two sample count, which is
 * the same way an operator would see them. A child writes its stderr to a
 * temp file and the parent parses `mean_ns=`.
 *
 * WHAT WOULD GO WRONG WITHOUT THE GUARDS
 *
 *   - The duration is now - start. A start stamp from the future, or a clock
 *     read that fails and returns 0, would underflow that subtraction to a
 *     value near 2^64 and poison the running total for the life of the
 *     process -- every later mean would be garbage. Checking only for a crash
 *     misses this entirely, which is why the mean is asserted against a
 *     ceiling.
 *   - 0 is the "not measuring" sentinel. If begin() ever returned a real 0
 *     stamp, that sample would be silently dropped; if record() did not treat
 *     0 as the sentinel, a build with metrics off would time from the epoch.
 *   - The counters are fixed MAX_DEVICE_COUNT arrays, so an unchecked index
 *     writes out of bounds. Under ASan (make test-sanitize) that case faults.
 *
 * get_logger_print_level() caches its answer in a function-local static on
 * first use, so one process can only observe one level; each level therefore
 * runs in its own child.
 */

#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <stdint.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/wait.h>

#include "include/hook.h"
#include "include/metrics.h"

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

/* No real query here takes a second; anything at or above this is the
 * underflow, not a slow machine. */
#define SANE_MEAN_CEILING_NS 1000000000ULL

/* Runs body() in a child with LOGGER_LEVEL set and its stderr redirected to
 * log_path. Returns the child's exit code, or -1 if it did not exit normally. */
static int in_child(const char *level, const char *log_path, int (*body)(void)) {
  pid_t child = fork();
  if (child < 0) {
    return -1;
  }
  if (child == 0) {
    setenv("LOGGER_LEVEL", level, 1);
    if (log_path != NULL) {
      int fd = open(log_path, O_WRONLY | O_CREAT | O_TRUNC, 0600);
      if (fd < 0) {
        _exit(2);
      }
      dup2(fd, STDERR_FILENO);
      close(fd);
    } else {
      int devnull = open("/dev/null", O_WRONLY | O_CLOEXEC);
      if (devnull >= 0) {
        dup2(devnull, STDERR_FILENO);
        close(devnull);
      }
    }
    _exit(body() == 0 ? 0 : 1);
  }
  int status = 0;
  if (waitpid(child, &status, 0) < 0) {
    return -1;
  }
  return WIFEXITED(status) ? WEXITSTATUS(status) : -1;
}

/* mkstemp rather than a fixed name, matching test_shared_file_mode.c: these
 * run from a shared /tmp and a fixed path would collide with a concurrent or
 * leftover run. Returns 0 on success. */
static int make_log_path(char *out, size_t out_size) {
  int fd;

  if (snprintf(out, out_size, "/tmp/vgpu_mqm_test_XXXXXX") >= (int)out_size) {
    return -1;
  }
  fd = mkstemp(out);
  if (fd < 0) {
    return -1;
  }
  close(fd);
  return 0;
}

/* Largest mean_ns= seen in the log, and how many such lines there were. */
static int scan_means(const char *path, uint64_t *max_mean, int *lines) {
  FILE *fp = fopen(path, "re");
  char line[2048];

  *max_mean = 0;
  *lines = 0;
  if (fp == NULL) {
    return -1;
  }
  while (fgets(line, sizeof(line), fp) != NULL) {
    char *hit = strstr(line, "mean_ns=");
    if (hit == NULL) {
      continue;
    }
    uint64_t value = strtoull(hit + strlen("mean_ns="), NULL, 10);
    if (value > *max_mean) {
      *max_mean = value;
    }
    (*lines)++;
  }
  fclose(fp);
  return 0;
}

/* LOGGER_LEVEL=2 is WARNING, below INFO, so should_collect_metrics() is false. */
static int body_metrics_off(void) {
  int bad = 0;

  if (metrics_memory_query_begin() != 0) {
    bad++; /* must decline, so no clock is read */
  }
  metrics_record_memory_query(0, 0);
  metrics_record_memory_query(0, 12345);
  return bad;
}

/* The stamp contract, with metrics on. */
static int body_stamps(void) {
  int bad = 0;
  uint64_t first = metrics_memory_query_begin();
  uint64_t second = metrics_memory_query_begin();

  if (first == 0 || second == 0) {
    bad++; /* 0 is the sentinel; a real stamp must never collide with it */
  }
  if (second < first) {
    bad++; /* CLOCK_MONOTONIC must not run backwards */
  }
  return bad;
}

/* Feeds record() a good sample, then the two shapes that would underflow the
 * duration, then another good sample -- so the parent sees a mean that only
 * stays sane if the guard rejected the bad ones. */
static int body_underflow(void) {
  uint64_t start = metrics_memory_query_begin();
  if (start == 0) {
    return 1;
  }
  metrics_record_memory_query(0, start);                     /* count -> 1 */

  /* A stamp one second in the future. */
  metrics_record_memory_query(0, metrics_memory_query_begin() + 1000000000ULL);
  /* The largest possible stamp. */
  metrics_record_memory_query(0, UINT64_MAX);

  start = metrics_memory_query_begin();
  if (start == 0) {
    return 1;
  }
  metrics_record_memory_query(0, start);                     /* count -> 2 */
  return 0;
}

/* Out-of-range indices must not reach the fixed-size counter arrays. */
static int body_bad_index(void) {
  uint64_t start = metrics_memory_query_begin();
  if (start == 0) {
    return 1;
  }
  metrics_record_memory_query(-1, start);
  metrics_record_memory_query(MAX_DEVICE_COUNT, start);
  metrics_record_memory_query(MAX_DEVICE_COUNT + 7, start);
  metrics_record_memory_query(INT32_MAX, start);
  metrics_record_memory_query(INT32_MIN, start);
  return 0;
}

/* Enough samples to cross several reporting points and to drive the
 * max-tracking CAS loop, across every valid device index. */
static int body_many(void) {
  for (int i = 0; i < 400; i++) {
    uint64_t start = metrics_memory_query_begin();
    if (start == 0) {
      return 1;
    }
    metrics_record_memory_query(i % MAX_DEVICE_COUNT, start);
  }
  return 0;
}

static void test_metrics_off(void) {
  int rc = in_child("2", NULL, body_metrics_off);
  CHECK(rc == 0, "with metrics off, begin() must return the 0 sentinel and "
                 "record() must be a no-op (child rc=%d)", rc);
}

static void test_stamps(void) {
  int rc = in_child("3", NULL, body_stamps);
  CHECK(rc == 0, "stamps must be non-zero and monotonic (child rc=%d)", rc);
}

/* The one that matters: a poisoned total shows up as an absurd mean. */
static void test_underflow_is_rejected(void) {
  char path[64];
  if (make_log_path(path, sizeof(path)) != 0) {
    printf("SKIP %s: could not create a temp log\n", __func__);
    return;
  }
  int rc = in_child("3", path, body_underflow);
  CHECK(rc == 0, "underflow child failed to run (rc=%d)", rc);
  if (rc != 0) {
    return;
  }

  uint64_t max_mean = 0;
  int lines = 0;
  CHECK(scan_means(path, &max_mean, &lines) == 0, "could not read %s", path);
  CHECK(lines > 0, "no 'mean_ns=' line was emitted, so nothing was verified "
                   "-- the reporting schedule or the log level changed");
  CHECK(max_mean < SANE_MEAN_CEILING_NS,
        "mean_ns=%llu is not a real duration: a start stamp from the future "
        "underflowed now - start and poisoned the running total",
        (unsigned long long)max_mean);
  unlink(path);
}

static void test_bad_index(void) {
  int rc = in_child("3", NULL, body_bad_index);
  CHECK(rc == 0, "out-of-range device indices must be rejected, not written "
                 "(child rc=%d)", rc);
}

/* Many samples must also leave the mean sane, which catches an underflow that
 * only a later sample would introduce. */
static void test_many_samples_stay_sane(void) {
  char path[64];
  if (make_log_path(path, sizeof(path)) != 0) {
    printf("SKIP %s: could not create a temp log\n", __func__);
    return;
  }
  int rc = in_child("3", path, body_many);
  CHECK(rc == 0, "bulk child failed to run (rc=%d)", rc);
  if (rc != 0) {
    return;
  }

  uint64_t max_mean = 0;
  int lines = 0;
  CHECK(scan_means(path, &max_mean, &lines) == 0, "could not read %s", path);
  CHECK(lines > 0, "no 'mean_ns=' line was emitted across 400 samples");
  CHECK(max_mean < SANE_MEAN_CEILING_NS,
        "mean_ns=%llu across 400 samples is not a real duration",
        (unsigned long long)max_mean);
  unlink(path);
}

int main(void) {
  test_metrics_off();
  test_stamps();
  test_underflow_is_rejected();
  test_bad_index();
  test_many_samples_stay_sane();

  if (failures != 0) {
    printf("FAILED: %d check(s)\n", failures);
    return 1;
  }
  printf("PASS: memory query metrics\n");
  return 0;
}
