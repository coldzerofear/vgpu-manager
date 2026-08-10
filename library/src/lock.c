#include "include/hook.h"
#include "include/metrics.h"

#include <sched.h>
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
/* Spin parameters for lock_gpu_device(). The critical section it guards is
 * ~1-3ms (two NVML process enumerations plus the real driver allocation);
 * waiters are the other processes of the same container, since the lock
 * file lives under the container's own /tmp.
 *
 * This fixes TAIL LATENCY, not throughput -- a waiter resets to the minimum
 * interval after every acquire, so the lock already stays highly utilised.
 * The failure case is a waiter that loses a few handoffs in a row: it
 * climbs to the backoff cap and then polls far slower than newly arrived
 * contenders, so it keeps losing and can ride all the way to
 * LOCK_TIMEOUT_MS while neighbours get served. Two fixes: the cap is sized
 * to the critical section (not 10x it), and past SPIN_AGING_MS a waiter
 * resets to the minimum interval instead of continuing to back off, putting
 * a long waiter ahead of any newcomer. Measured on a synthetic 2ms critical
 * section: worst-case observed wait dropped from ~3s to ~0.2s, with both
 * the floor and the aging rule load-bearing for that improvement.
 *
 * Cost: a retry is one fcntl on a descriptor opened once outside the loop,
 * a fraction of a percent of a core even under contention. The first few
 * attempts yield rather than sleep, since the fast path can release in
 * microseconds; the 200us floor matches the kernel's default timer slack,
 * below which a shorter sleep buys nothing but syscalls. */
#define SPIN_YIELD_ATTEMPTS  4
#define SPIN_INTERVAL_MIN_US 200
#define SPIN_INTERVAL_MAX_US 1000
#define SPIN_AGING_MS        50
#define LOCK_TIMEOUT_MS      10000

#define GET_DEVICE_LOCK_OFFSET(device_index) \
  offsetof(device_util_t, devices[device_index].lock_byte)

#define GET_VMEMORY_LOCK_OFFSET(device_index) \
  offsetof(device_vmemory_t, devices[device_index].lock_byte)

/* The per-device byte-range locks on the shared sm-util/vmem files use OFD
 * (Open File Description) locks instead of classic process-associated locks.
 * Classic POSIX locks release when the process closes ANY fd on the inode,
 * so a caller holding locks on several device bytes could have one
 * unlock/close silently drop a sibling device's still-held lock. OFD locks
 * are owned by the open file description instead, so an unrelated close
 * never touches them, while still conflicting across fds and processes.
 * Requires Linux >= 3.15; the fallbacks below cover older kernels.
 * lock_gpu_device() also uses OFD for a different reason: intra-process
 * mutual exclusion, which a classic per-process lock can't give (fds of one
 * process never conflict), closing a TOCTOU race between threads of the
 * same container allocating on the same device. */
#ifndef F_OFD_SETLK
#define F_OFD_SETLK 37
#endif
#ifndef F_OFD_SETLKW
#define F_OFD_SETLKW 38
#endif

/* Prefer OFD locks; fall back to classic POSIX locks at runtime when the
 * kernel rejects them with EINVAL. A classic F_UNLCK doesn't release an OFD
 * lock and vice versa, but that never mixes here: a kernel either supports
 * OFD for every call or falls back for every call, so acquire and release
 * always stay in one family. */
/* Not static: loader.c's sm_node region builder reuses it rather than growing a
 * second locking primitive. Stays out of .dynsym via the linker version script. */
int ofd_fcntl(int fd, int wait, struct flock *fl) {
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

/* Wait out one failed acquire, then pick the next interval.
 *
 * The sleep is clamped to the time left before the deadline so the timeout
 * never overshoots by a full interval.
 *
 * A signal that cuts nanosleep(2) short just means the caller retries sooner;
 * the interval update is tied to a failed acquire, not to a completed sleep,
 * so no EINTR handling is needed. */
static void backoff_spin(long *interval_us, int attempt, long elapsed_ms,
                         long remaining_ms) {
  if (attempt < SPIN_YIELD_ATTEMPTS) {
    sched_yield();
    return;
  }

  long us = *interval_us;
  long remaining_us = remaining_ms * 1000;
  if (remaining_us < us) us = remaining_us;
  if (likely(us > 0)) {
    struct timespec ts = {
      .tv_sec = 0,
      .tv_nsec = us * 1000,
    };
    nanosleep(&ts, NULL);
  }

  if (elapsed_ms >= SPIN_AGING_MS) {
    /* Aged in. Back at the minimum, this waiter now polls at least as often as
     * any newcomer, so it stops losing every handoff to freshly arrived
     * contenders. See the note on fairness above. */
    *interval_us = SPIN_INTERVAL_MIN_US;
    return;
  }
  *interval_us <<= 1;
  if (*interval_us > SPIN_INTERVAL_MAX_US) {
    *interval_us = SPIN_INTERVAL_MAX_US;
  }
}

/* Idempotent directory create. mkdir(2) is an atomic kernel syscall, so
 * concurrent callers resolve cleanly: at most one wins with 0, the rest get
 * EEXIST, which we treat as success. This avoids a mutex on a hot path
 * (lock_gpu_device runs on every memory hook) and the held-at-fork hazard
 * a mutex there would carry. Any other errno is left to surface naturally
 * from lock_gpu_device()'s own open() call below. */
static void ensure_create_lock_dir(void) {
  if (mkdir(VGPU_LOCK_PATH, 0755) == 0) return;
  if (errno == EEXIST) return;
  /* fall through; lock_gpu_device's open() will surface the real error */
}

/* Arm the lock on an already-open descriptor. Returns 0 on acquisition, or
 * -1 with errno set; *retryable then reports whether this was mere
 * contention (spin and try again) or a hard error.
 *
 * The caller opens the fd once per lock_gpu_device() call, not once per
 * attempt: re-arming the same descriptor is equivalent to re-opening (an
 * OFD lock belongs to the open file description), and nothing ever unlinks
 * these files, so the descriptor can't go stale under a long wait. This
 * drops a retry from three syscalls to one, which is what pays for the
 * much shorter spin interval, and also shrinks the legacy-kernel hazard
 * where closing any descriptor on the inode drops the whole process's lock. */
static int try_lock_fd(int fd, int *retryable) {
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
    // A held lock surfaces as EACCES or EAGAIN (POSIX allows either).
    *retryable = (errno == EACCES || errno == EAGAIN || errno == EINTR);
    return -1;
  }
  return 0;
}

int lock_gpu_device(int device_index) {
  if (unlikely(device_index < 0 || device_index >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "invalid device index %d", device_index);
    return -1;
  }

  ensure_create_lock_dir();
  char lock_path[LOCK_PATH_SIZE];
  snprintf(lock_path, LOCK_PATH_SIZE, LOCK_PATH_FORMAT, device_index);

  /* Opened once for the whole wait. Every path out of this function either
   * hands the descriptor to the caller -- locked, for unlock_gpu_device() to
   * close -- or closes it here. */
  int fd = open(lock_path, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
  if (unlikely(fd == -1)) {
    /* A missing or unwritable lock dir is not contention. Spinning on it for
     * the full 10s would only delay the caller and bury the real errno behind
     * a misleading "lock timeout". */
    LOGGER(ERROR, "cannot open lock file for device %d: %s", device_index, strerror(errno));
    return -1;
  }

  struct timespec start, now;
  clock_gettime(CLOCK_MONOTONIC, &start);
  long interval_us = SPIN_INTERVAL_MIN_US;

  for (int attempt = 0; ; attempt++) {
    int retryable = 0;
    int acquired = try_lock_fd(fd, &retryable);
    int err = errno; // capture before clock_gettime() can touch errno
    // Sampled after the attempt, so the elapsed time covers it: a leading
    // sample credits the acquire's own cost to the next iteration and reports
    // ~0ns of wait whenever the very first attempt succeeds.
    clock_gettime(CLOCK_MONOTONIC, &now);
    uint64_t waited_ns = elapsed_time_ns(&start, &now);

    if (acquired == 0) {
      metrics_record_lock_wait(device_index, waited_ns, 0);
      return fd; // success
    }
    if (unlikely(!retryable)) {
      LOGGER(ERROR, "lock failed for device %d: %s", device_index, strerror(err));
      close(fd);
      return -1;
    }

    long elapsed_ms = (long)(waited_ns / MILLISEC);
    if (unlikely(elapsed_ms >= LOCK_TIMEOUT_MS)) {
      metrics_record_lock_wait(device_index, waited_ns, 1);
      LOGGER(ERROR, "lock timeout for device %d", device_index);
      close(fd);
      return -1;
    }
    backoff_spin(&interval_us, attempt, elapsed_ms, LOCK_TIMEOUT_MS - elapsed_ms);
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

/* Per-device byte-range lock on vgpu.config's devices[i].seq word. Only the
 * slow-path fallback in get_device_snapshot() (F_RDLCK) and the Go writer
 * (F_WRLCK) take it; the seqlock fast path is lock-free. See
 * docs/resource_data_seqlock_versioning_design.md. */
int config_device_read_lock(int device_index) {
  if (unlikely(device_index < 0 || device_index >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "(config) invalid device index %d", device_index);
    return -1;
  }
  int fd = open(CONTROLLER_CONFIG_FILE_PATH, O_RDONLY | O_CLOEXEC);
  if (fd == -1) {
    LOGGER(ERROR, "(config) failed to open %s: %s", CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
    return -1;
  }
  struct flock lock;
  lock.l_type = F_RDLCK;
  lock.l_whence = SEEK_SET;
  lock.l_start = GET_CONFIG_LOCK_OFFSET(device_index);
  lock.l_len = 1;
  lock.l_pid = 0;
  if (ofd_fcntl(fd, 1, &lock) == -1) {
    LOGGER(ERROR, "(config) fcntl read lock failed for device %d: %s",
           device_index, strerror(errno));
    close(fd);
    return -1;
  }
  return fd;
}

void config_device_unlock(int fd, int device_index) {
  if (fd < 0) return;
  if (unlikely(device_index < 0 || device_index >= MAX_DEVICE_COUNT)) return;
  struct flock lock;
  lock.l_type = F_UNLCK;
  lock.l_whence = SEEK_SET;
  lock.l_start = GET_CONFIG_LOCK_OFFSET(device_index);
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
