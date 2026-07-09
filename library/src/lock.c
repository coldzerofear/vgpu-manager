#include "include/hook.h"
#include "include/metrics.h"

#include <sys/file.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/stat.h>
#include <stdio.h>
#include <errno.h>
#include <stddef.h>
#include <sys/mman.h>
#include <sys/time.h>

#define LOCK_PATH_FORMAT (TMP_DIR VGPU_LOCK_DIR "/vgpu_%d.lock")
#define LOCK_PATH_SIZE   32
#define SPIN_INTERVAL_MS 10
#define LOCK_TIMEOUT_MS  5000

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
 * NOTE: lock_gpu_device() below intentionally stays on classic F_SETLK -- it
 * locks a separate per-device file, so it is not exposed to the sibling-drop
 * problem. */
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

static const struct timespec sleep_time = {
  .tv_sec = 0,
  .tv_nsec = SPIN_INTERVAL_MS * MILLISEC,
};

static uint64_t elapsed_time_ns(const struct timeval *start,
                                const struct timeval *end) {
  int64_t sec = (int64_t)(end->tv_sec - start->tv_sec);
  int64_t usec = (int64_t)(end->tv_usec - start->tv_usec);
  return (uint64_t)(sec * 1000000000LL + usec * 1000LL);
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

static int try_acquire_lock(const char *path) {
  int fd = open(path, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
  if (fd == -1) return -1;
  struct flock fl = {
    .l_type = F_WRLCK,
    .l_whence = SEEK_SET,
    .l_start = 0,
    .l_len = 0, // lock entire file
  };
  if (fcntl(fd, F_SETLK, &fl) == -1) {
    close(fd);
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

  struct timeval start, now;
  gettimeofday(&start, NULL);

  while (1) {
    gettimeofday(&now, NULL);
    int fd = try_acquire_lock(lock_path);
    if (fd != -1) {
      metrics_record_lock_wait(device_index, elapsed_time_ns(&start, &now), 0);
      return fd; // success
    }

    long elapsed_ms = (now.tv_sec - start.tv_sec) * 1000 +
                      (now.tv_usec - start.tv_usec) / 1000;
    if (unlikely(elapsed_ms >= LOCK_TIMEOUT_MS)) {
      metrics_record_lock_wait(device_index, elapsed_time_ns(&start, &now), 1);
      LOGGER(ERROR, "lock timeout for device %d", device_index);
      return -1;
    }
    // retry
    nanosleep(&sleep_time, NULL);
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
  fcntl(fd, F_SETLK, &fl);
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
