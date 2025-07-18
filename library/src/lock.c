#include "include/hook.h"

#include <sys/file.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/time.h>
#include <sys/stat.h>
#include <stdio.h>
#include <errno.h>
#include <pthread.h>

#define LOCK_DIR "/tmp/.vgpu_lock"
#define LOCK_PATH_FORMAT (LOCK_DIR "/vgpu_%d.lock")
#define LOCK_PATH_SIZE   32
#define SPIN_INTERVAL_MS 100
#define LOCK_TIMEOUT_MS  5000

static const struct timespec sleep_time = {
  .tv_sec = 0,
  .tv_nsec = SPIN_INTERVAL_MS * MILLISEC,
};

static pthread_mutex_t dir_mutex = PTHREAD_MUTEX_INITIALIZER;

static void ensure_lock_dir() {
  pthread_mutex_lock(&dir_mutex);
  if (access(LOCK_DIR, F_OK) != 0) {
    mkdir(LOCK_DIR, 0755);
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
  if (ordinal >= MAX_DEVICE_COUNT) {
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