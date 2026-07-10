#include "include/hook.h"
#include "include/metrics.h"

#include <sys/file.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/stat.h>
#include <stdio.h>
#include <errno.h>
#include <stddef.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>

#define LOCK_PATH_FORMAT (TMP_DIR VGPU_LOCK_DIR "/vgpu_%d.lock")
#define LOCK_PATH_SIZE   32
/* Exponential backoff bounds for lock_gpu_device()'s spin: the critical section
 * it guards is ~1-3ms (two NVML process enumerations plus the real driver
 * allocation), so a waiter that just missed the lock almost always wins it back
 * within the first couple of intervals. Starting at 10ms made every such waiter
 * over-wait by 3-10x.
 * The 10ms cap is deliberately equal to the old fixed interval: once saturated,
 * a long waiter polls on exactly the grid it used to, so no wait gets slower
 * than before. Raising it (e.g. 20ms) would halve the syscalls of a deeply
 * queued waiter but overshoot the release point by up to a full interval. */
#define SPIN_INTERVAL_MIN_MS 1
#define SPIN_INTERVAL_MAX_MS 10
#define LOCK_TIMEOUT_MS      10000

#define GET_DEVICE_LOCK_OFFSET(device_index) \
  offsetof(device_util_t, devices[device_index].lock_byte)

#define GET_VMEMORY_LOCK_OFFSET(device_index) \
  offsetof(device_vmemory_t, devices[device_index].lock_byte)

/* The per-device byte-range locks on the shared sm-util / vmem files use OFD
 * (Open File Description) locks instead of classic process-associated locks.
 * Classic POSIX locks are released when the process closes ANY fd on the inode,
 * so a fresh-fd-per-lock caller that holds locks on several device bytes (or
 * also keeps the file mmap'd) would have one unlock/close silently drop a
 * sibling device's still-held lock. OFD locks are owned by the open file
 * description, so an unrelated close never touches them, while still conflicting
 * across fds and cross-process. Requires Linux >= 3.15; the build defines
 * _GNU_SOURCE (see CMakeLists.txt), and the fallbacks below cover builds that
 * do not (the values are fixed across all Linux architectures).
 * lock_gpu_device()'s per-device file lock also uses OFD: those files are not
 * exposed to the sibling-drop problem (one inode per device), but OFD gives it
 * intra-process mutual exclusion, which a classic per-process lock cannot (fds
 * of one process never conflict), closing a TOCTOU race between threads of the
 * same container allocating on the same device. */
#ifndef F_OFD_SETLK
#define F_OFD_SETLK 37
#endif
#ifndef F_OFD_SETLKW
#define F_OFD_SETLKW 38
#endif

/* Prefer OFD locks (Linux >= 3.15); fall back to classic POSIX locks at runtime
 * when the kernel rejects them with EINVAL. On modern kernels the OFD call
 * succeeds on the first try, so there is no extra syscall. A classic F_UNLCK
 * does not release an OFD lock and vice versa, but that never mixes here: a
 * kernel either supports OFD (every call, lock and unlock, uses it) or does not
 * (every call falls back), so acquire and release always stay in one family. */
static int ofd_fcntl(int fd, int wait, struct flock *fl) {
  int ret = fcntl(fd, wait ? F_OFD_SETLKW : F_OFD_SETLK, fl);
  if (ret != -1 || errno != EINVAL) return ret;
  return fcntl(fd, wait ? F_SETLKW : F_SETLK, fl); /* legacy kernels */
}

/* CLOCK_MONOTONIC, not gettimeofday(): the spin below measures a duration, and a
 * CLOCK_REALTIME step (NTP slew/step, container clock sync) would otherwise make
 * the 10s timeout fire early or stretch arbitrarily. Already used elsewhere in
 * this library (cuda_hook.c), so it adds no link-time requirement. */
static uint64_t elapsed_time_ns(const struct timespec *start,
                                const struct timespec *end) {
  int64_t sec = (int64_t)(end->tv_sec - start->tv_sec);
  int64_t nsec = (int64_t)(end->tv_nsec - start->tv_nsec);
  return (uint64_t)(sec * 1000000000LL + nsec);
}

/* Sleep the current backoff interval, then double it up to SPIN_INTERVAL_MAX_MS.
 * The interval is clamped by two things:
 *
 *   1. The time left before the deadline, so the timeout never overshoots by a
 *      full interval.
 *   2. The next SPIN_INTERVAL_MAX_MS-aligned instant. Doubling alone would put
 *      the wake-ups at 1,3,7,15,25,... -- a 10ms grid phase-shifted off the
 *      0,10,20,... grid the old fixed sleep used, so for some hold times the
 *      waiter sleeps straight past a release it would previously have caught.
 *      Aligning makes the wake-ups 1,3,7,10,20,30,... a strict superset of the
 *      old ones: every retry the old code performed still happens, just with
 *      extra early ones. No wait can therefore get slower than it was.
 *
 * A signal that cuts nanosleep(2) short just means the caller retries sooner;
 * the doubling is tied to a failed acquire, not to a completed sleep, so no
 * EINTR handling is needed. */
static void backoff_sleep(long *interval_ms, long elapsed_ms, long remaining_ms) {
  long to_grid = SPIN_INTERVAL_MAX_MS - (elapsed_ms % SPIN_INTERVAL_MAX_MS);
  long ms = *interval_ms;
  if (to_grid < ms) ms = to_grid;
  if (remaining_ms < ms) ms = remaining_ms;
  if (likely(ms > 0)) {
    struct timespec ts = {
      .tv_sec = 0,
      .tv_nsec = ms * MILLISEC,
    };
    nanosleep(&ts, NULL);
  }
  *interval_ms <<= 1;
  if (*interval_ms > SPIN_INTERVAL_MAX_MS) {
    *interval_ms = SPIN_INTERVAL_MAX_MS;
  }
}

/* Idempotent directory create. mkdir(2) is an atomic kernel syscall, so
 * multiple threads or processes racing on the same path resolve cleanly:
 * at most one wins with 0, the rest get EEXIST. Treating EEXIST as success
 * gives the same effect as the previous mutex-guarded access()+mkdir()
 * pair, with three side benefits:
 *
 *   1. Removes a mutex from a hot path (lock_gpu_device runs on every
 *      memory hook, which fires from every cuLaunch* indirectly).
 *   2. Removes a held-at-fork hazard -- the old `mutex` had the same
 *      "parent thread holds it at fork -> child deadlocks forever"
 *      shape as the four loader.c mutexes already covered by
 *      loader_child_after_fork(). Eliminating the mutex eliminates
 *      the hazard rather than papering over it with re-init.
 *   3. Drops one syscall per call (the access() probe).
 *
 * Any errno other than EEXIST (EACCES / ENOSPC / ENOENT on parent dir,
 * etc.) is propagated implicitly: the open(O_RDWR|O_CREAT) in
 * try_acquire_lock() below will return -1 with a related errno and
 * lock_gpu_device's standard failure path handles it -- so no extra
 * error reporting is needed here. */
static void ensure_create_lock_dir(void) {
  if (mkdir(VGPU_LOCK_PATH, 0755) == 0) return;
  if (errno == EEXIST) return;
  /* fall through; try_acquire_lock's open() will surface the real error */
}

/* Returns the locked fd, or -1 with errno set. On failure *retryable reports
 * whether this was mere lock contention (spin and try again) or a hard error
 * such as a missing/unwritable lock dir (give up immediately -- retrying an
 * ENOSPC/EACCES open() for the full 10s timeout only delays the caller and
 * buries the real errno behind a misleading "lock timeout" log). */
static int try_acquire_lock(const char *path, int *retryable) {
  int fd = open(path, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
  if (unlikely(fd == -1)) {
    *retryable = 0;
    return -1;
  }
  struct flock fl = {
    .l_type = F_WRLCK,
    .l_whence = SEEK_SET,
    .l_start = 0,
    .l_len = 0, // lock entire file
  };
  // OFD lock (non-blocking): gives intra-process mutual exclusion too, which a
  // classic per-process lock on this per-device file would not (same-process
  // fds never conflict). lock_gpu_device() backs off and retries on contention.
  // .l_pid is zero-initialized above, as OFD requires.
  if (ofd_fcntl(fd, 0, &fl) == -1) {
    int err = errno;
    close(fd); // may clobber errno, so restore it for the caller below
    // A held lock surfaces as EACCES or EAGAIN (POSIX allows either).
    *retryable = (err == EACCES || err == EAGAIN || err == EINTR);
    errno = err;
    return -1;
  }
  return fd;
}

int lock_gpu_device(int device_index) {
  if (unlikely(device_index < 0 || device_index >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "invalid device index %d", device_index);
    return -1;
  }

  ensure_create_lock_dir();
  char lock_path[LOCK_PATH_SIZE];
  snprintf(lock_path, LOCK_PATH_SIZE, LOCK_PATH_FORMAT, device_index);

  struct timespec start, now;
  clock_gettime(CLOCK_MONOTONIC, &start);
  long interval_ms = SPIN_INTERVAL_MIN_MS;

  while (1) {
    int retryable = 0;
    int fd = try_acquire_lock(lock_path, &retryable);
    int err = errno; // capture before clock_gettime() can touch errno
    // Sampled after the attempt, so the elapsed time covers it: a leading
    // sample credits the acquire's own cost to the next iteration and reports
    // ~0ns of wait whenever the very first attempt succeeds.
    clock_gettime(CLOCK_MONOTONIC, &now);
    uint64_t waited_ns = elapsed_time_ns(&start, &now);

    if (fd != -1) {
      metrics_record_lock_wait(device_index, waited_ns, 0);
      return fd; // success
    }
    if (unlikely(!retryable)) {
      LOGGER(ERROR, "lock failed for device %d: %s", device_index, strerror(err));
      return -1;
    }

    long elapsed_ms = (long)(waited_ns / MILLISEC);
    if (unlikely(elapsed_ms >= LOCK_TIMEOUT_MS)) {
      metrics_record_lock_wait(device_index, waited_ns, 1);
      LOGGER(ERROR, "lock timeout for device %d", device_index);
      return -1;
    }
    backoff_sleep(&interval_ms, elapsed_ms, LOCK_TIMEOUT_MS - elapsed_ms);
  }
}

void unlock_gpu_device(int fd) {
  if (fd < 0) return;

  struct flock fl = {
    .l_type = F_UNLCK,
    .l_whence = SEEK_SET,
    .l_start = 0,
    .l_len = 0,
  };
  ofd_fcntl(fd, 0, &fl);
  close(fd);
}

int device_util_read_lock(int device_index) {
  if (unlikely(device_index < 0 || device_index >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "(SMWatcher) invalid device index %d", device_index);
    return -1;
  }
  int fd = open(CONTROLLER_SM_UTIL_FILE_PATH, O_RDONLY | O_CLOEXEC);
  if (fd == -1) {
    LOGGER(ERROR, "(SMWatcher) failed to open shared file: %s", strerror(errno));
    return -1;
  }
  struct flock lock;
  lock.l_type = F_RDLCK;
  lock.l_whence = SEEK_SET;
  lock.l_start = GET_DEVICE_LOCK_OFFSET(device_index);
  lock.l_len = 1;
  lock.l_pid = 0;
  if (ofd_fcntl(fd, 1, &lock) == -1) {
    LOGGER(ERROR, "(SMWatcher) fcntl read lock failed for device %d: %s",
               device_index, strerror(errno));
    close(fd);
    return -1;
  }
  return fd;
}

int device_util_write_lock(int device_index) {
  if (unlikely(device_index < 0 || device_index >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "(SMWatcher) invalid device index %d", device_index);
    return -1;
  }
  int fd = open(CONTROLLER_SM_UTIL_FILE_PATH, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
  if (fd == -1) {
    LOGGER(ERROR, "(SMWatcher) failed to open shared file: %s", strerror(errno));
    return -1;
  }
  struct flock lock;
  lock.l_type = F_WRLCK;
  lock.l_whence = SEEK_SET;
  lock.l_start = GET_DEVICE_LOCK_OFFSET(device_index);
  lock.l_len = 1;
  lock.l_pid = 0;
  if (ofd_fcntl(fd, 1, &lock) == -1) {
    LOGGER(ERROR, "(SMWatcher) fcntl write lock failed for device %d: %s",
           device_index, strerror(errno));
    close(fd);
    return -1;
  }
  return fd;
}

void device_util_unlock(int fd, int device_index) {
  if (fd < 0) return;
  if (unlikely(device_index < 0 || device_index >= MAX_DEVICE_COUNT)) return;
  struct flock lock;
  lock.l_type = F_UNLCK;
  lock.l_whence = SEEK_SET;
  lock.l_start = GET_DEVICE_LOCK_OFFSET(device_index);
  lock.l_len = 1;
  lock.l_pid = 0;
  ofd_fcntl(fd, 0, &lock);
  close(fd);
}

int device_vmem_read_lock(int device_index) {
  if (unlikely(device_index < 0 || device_index >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "(VMemNode) invalid device index %d", device_index);
    return -1;
  }
  int fd = open(VMEMORY_NODE_FILE_PATH, O_RDONLY | O_CLOEXEC);
  if (fd == -1) {
    LOGGER(ERROR, "(VMemNode) failed to open shared file: %s", strerror(errno));
    return -1;
  }
  struct flock lock;
  lock.l_type = F_RDLCK;
  lock.l_whence = SEEK_SET;
  lock.l_start = GET_VMEMORY_LOCK_OFFSET(device_index);
  lock.l_len = 1;
  lock.l_pid = 0;
  if (ofd_fcntl(fd, 1, &lock) == -1) {
    LOGGER(ERROR, "(VMemNode) fcntl read lock failed for device %d: %s",
               device_index, strerror(errno));
    close(fd);
    return -1;
  }
  return fd;
}

int device_vmem_write_lock(int device_index) {
  if (unlikely(device_index < 0 || device_index >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "(VMemNode) invalid device index %d", device_index);
    return -1;
  }
  int fd = open(VMEMORY_NODE_FILE_PATH, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
  if (fd == -1) {
    LOGGER(ERROR, "(VMemNode) failed to open shared file: %s", strerror(errno));
    return -1;
  }
  struct flock lock;
  lock.l_type = F_WRLCK;
  lock.l_whence = SEEK_SET;
  lock.l_start = GET_VMEMORY_LOCK_OFFSET(device_index);
  lock.l_len = 1;
  lock.l_pid = 0;
  if (ofd_fcntl(fd, 1, &lock) == -1) {
    LOGGER(ERROR, "(VMemNode) fcntl write lock failed for device %d: %s",
           device_index, strerror(errno));
    close(fd);
    return -1;
  }
  return fd;
}

void device_vmem_unlock(int fd, int device_index) {
  if (fd < 0) return;
  if (unlikely(device_index < 0 || device_index >= MAX_DEVICE_COUNT)) return;
  struct flock lock;
  lock.l_type = F_UNLCK;
  lock.l_whence = SEEK_SET;
  lock.l_start = GET_VMEMORY_LOCK_OFFSET(device_index);
  lock.l_len = 1;
  lock.l_pid = 0;
  ofd_fcntl(fd, 0, &lock);
  close(fd);
}
