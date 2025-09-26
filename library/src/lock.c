#include "include/hook.h"

#include <sys/file.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/time.h>
#include <sys/stat.h>
#include <stdio.h>
#include <errno.h>
#include <pthread.h>
#include <stddef.h>
#include <sys/mman.h>

#define LOCK_PATH_FORMAT (TMP_DIR VGPU_LOCK_DIR "/vgpu_%d.lock")
#define LOCK_PATH_SIZE   32
#define SPIN_INTERVAL_MS 20
#define LOCK_TIMEOUT_MS  3000

#define GET_DEVICE_LOCK_OFFSET(device_index) \
  offsetof(device_util_t, devices[device_index].lock_byte)

#define GET_VMEMORY_LOCK_OFFSET(device_index) \
  offsetof(device_vmemory_t, devices[device_index].lock_byte)

static const struct timespec sleep_time = {
  .tv_sec = 0,
  .tv_nsec = SPIN_INTERVAL_MS * MILLISEC,
};

static pthread_mutex_t dir_mutex = PTHREAD_MUTEX_INITIALIZER;

static void ensure_lock_dir() {
  pthread_mutex_lock(&dir_mutex);
  if (access(VGPU_LOCK_PATH, F_OK) != 0) {
    mkdir(VGPU_LOCK_PATH, 0755);
  }
  pthread_mutex_unlock(&dir_mutex);
}

static void get_lock_path(int ordinal, char *buffer, size_t size) {
  snprintf(buffer, size, LOCK_PATH_FORMAT, ordinal);
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

int lock_gpu_device(int ordinal) {
  if (unlikely(ordinal < 0 || ordinal >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "invalid device index %d", ordinal);
    return -1;
  }
  LOGGER(VERBOSE, "lock gpu device %d", ordinal);

  ensure_lock_dir();
  char lock_path[LOCK_PATH_SIZE];
  get_lock_path(ordinal, lock_path, LOCK_PATH_SIZE);

  struct timeval start, now;
  gettimeofday(&start, NULL);

  while (1) {
    int fd = try_acquire_lock(lock_path);
    if (fd != -1) {
      return fd; // success
    }

    gettimeofday(&now, NULL);
    long elapsed_ms = (now.tv_sec - start.tv_sec) * 1000 +
                      (now.tv_usec - start.tv_usec) / 1000;
    if (elapsed_ms >= LOCK_TIMEOUT_MS) {
      LOGGER(ERROR, "lock timeout for device %d", ordinal);
      return -1; // timeout
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

int device_util_read_lock(int ordinal) {
  if (unlikely(ordinal < 0 || ordinal >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "(SMWatcher) invalid device index %d", ordinal);
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
  lock.l_start = GET_DEVICE_LOCK_OFFSET(ordinal);
  lock.l_len = 1;
  lock.l_pid = 0;
  if (fcntl(fd, F_SETLKW, &lock) == -1) {
    LOGGER(ERROR, "(SMWatcher) fcntl read lock failed for device %d: %s",
               ordinal, strerror(errno));
    close(fd);
    return -1;
  }
  return fd;
}

int device_util_write_lock(int ordinal) {
  if (unlikely(ordinal < 0 || ordinal >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "(SMWatcher) invalid device index %d", ordinal);
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
  lock.l_start = GET_DEVICE_LOCK_OFFSET(ordinal);
  lock.l_len = 1;
  lock.l_pid = 0;
  if (fcntl(fd, F_SETLKW, &lock) == -1) {
    LOGGER(ERROR, "(SMWatcher) fcntl write lock failed for device %d: %s",
           ordinal, strerror(errno));
    close(fd);
    return -1;
  }
  return fd;
}

void device_util_unlock(int fd, int ordinal) {
  if (fd < 0) return;
  if (unlikely(ordinal < 0 || ordinal >= MAX_DEVICE_COUNT)) return;
  struct flock lock;
  lock.l_type = F_UNLCK;
  lock.l_whence = SEEK_SET;
  lock.l_start = GET_DEVICE_LOCK_OFFSET(ordinal);
  lock.l_len = 1;
  lock.l_pid = 0;
  fcntl(fd, F_SETLK, &lock);
  close(fd);
}

int device_vmem_read_lock(int ordinal) {
  if (unlikely(ordinal < 0 || ordinal >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "(VMemNode) invalid device index %d", ordinal);
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
  lock.l_start = GET_VMEMORY_LOCK_OFFSET(ordinal);
  lock.l_len = 1;
  lock.l_pid = 0;
  if (fcntl(fd, F_SETLKW, &lock) == -1) {
    LOGGER(ERROR, "(VMemNode) fcntl read lock failed for device %d: %s",
               ordinal, strerror(errno));
    close(fd);
    return -1;
  }
  return fd;
}

int device_vmem_write_lock(int ordinal) {
  if (unlikely(ordinal < 0 || ordinal >= MAX_DEVICE_COUNT)) {
    LOGGER(ERROR, "(VMemNode) invalid device index %d", ordinal);
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
  lock.l_start = GET_VMEMORY_LOCK_OFFSET(ordinal);
  lock.l_len = 1;
  lock.l_pid = 0;
  if (fcntl(fd, F_SETLKW, &lock) == -1) {
    LOGGER(ERROR, "(VMemNode) fcntl write lock failed for device %d: %s",
           ordinal, strerror(errno));
    close(fd);
    return -1;
  }
  return fd;
}

void device_vmem_unlock(int fd, int ordinal) {
  if (fd < 0) return;
  if (unlikely(ordinal < 0 || ordinal >= MAX_DEVICE_COUNT)) return;
  struct flock lock;
  lock.l_type = F_UNLCK;
  lock.l_whence = SEEK_SET;
  lock.l_start = GET_VMEMORY_LOCK_OFFSET(ordinal);
  lock.l_len = 1;
  lock.l_pid = 0;
  fcntl(fd, F_SETLK, &lock);
  close(fd);
}