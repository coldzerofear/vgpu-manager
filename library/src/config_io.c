/*
Copyright 2024-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
 * vgpu.config region I/O: map it for reading, create it for writing.
 *
 * Split out of loader.c so the session-config tool can produce a region with
 * the very code the library consumes it with -- header stamping, fixed file
 * size and lock discipline included -- instead of a second implementation
 * that agrees with this one only until someone edits one of them.
 *
 * Both functions resolve their target through CONTROLLER_CONFIG_FILE_PATH,
 * i.e. the session's config in remote mode and the node-global one otherwise.
 */

#include <errno.h>
#include <fcntl.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include "include/hook.h"
#include "include/session.h"

extern int file_exist(const char *file_path);
extern int ofd_fcntl(int fd, int wait, struct flock *fl);
int mmap_file_to_config_path(resource_data_t** data) {
  int ret = 1;
  if (unlikely(file_exist(CONTROLLER_CONFIG_FILE_PATH) != 0)) {
    return ret;
  }
  int fd = open(CONTROLLER_CONFIG_FILE_PATH, O_RDONLY | O_CLOEXEC);
  if (unlikely(fd == -1)) {
    LOGGER(ERROR, "can't open %s, error %s", CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
    return ret;
  }
  /* Read-lock byte 0 so we never validate a file a concurrent writer is
   * mid-write on; the header check below is a backstop if locking fails.
   * Released at DONE -- the mapping itself outlives the lock. */
  struct flock rl;
  memset(&rl, 0, sizeof(rl));
  rl.l_type = F_RDLCK;
  rl.l_whence = SEEK_SET;
  rl.l_start = 0;
  rl.l_len = 1;
  if (unlikely(ofd_fcntl(fd, 1, &rl) == -1)) {
    LOGGER(WARNING, "can't read-lock %s (%s); validating without it",
           CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
  }
  struct stat sb;
  if (fstat(fd, &sb) == -1) {
    LOGGER(ERROR, "fstat failed: %s", strerror(errno));
    goto DONE;
  }
  if (sb.st_size != CONFIG_FILE_SIZE) {
    LOGGER(ERROR, "vgpu config size mismatch: expected %d, got %lld",
                  CONFIG_FILE_SIZE, (long long)sb.st_size);
    goto DONE;
  }
  /* PROT_READ + MAP_PRIVATE: we never write, so no page is ever COW-copied
   * and every read sees the writer's live update. Tear-free consistency
   * comes from the per-device seqlock in get_device_snapshot(), not here. */
  resource_data_t *m = (resource_data_t*)mmap(NULL, CONFIG_FILE_SIZE, PROT_READ,
                                              MAP_PRIVATE, fd, 0);
  if (m == MAP_FAILED) {
    LOGGER(ERROR, "mmap global config failed: %s", strerror(errno));
    goto DONE;
  }
  /* Frozen-header check, same contract as vmem_node/sm_node: a config from a
   * mismatched layout_version is rejected cleanly instead of misread. */
  if (m->magic != CONFIG_MAGIC || m->layout_version != CONFIG_LAYOUT_VERSION ||
      m->region_size != sizeof(resource_data_t) || m->device_count != MAX_DEVICE_COUNT) {
    LOGGER(ERROR, "vgpu config header mismatch: magic=%#x ver=%u size=%u count=%u "
                  "(want %#x/%u/%zu/%d)",
                  m->magic, m->layout_version, m->region_size, m->device_count,
                  CONFIG_MAGIC, CONFIG_LAYOUT_VERSION, sizeof(resource_data_t),
                  MAX_DEVICE_COUNT);
    munmap(m, CONFIG_FILE_SIZE);
    goto DONE;
  }
  *data = m;
  ret = 0;
DONE:
  close(fd);
  return ret;
}

int write_file_to_config_path(resource_data_t* data) {
  int ret = 1;
  /* mkdir -p rather than the two fixed levels this used to create: in session
   * mode the parent chain is <base>/<session>/config, which is deeper and not
   * rooted at VGPU_MANAGER_PATH. */
  if (unlikely(file_exist(VGPU_CONFIG_PATH) != 0)) {
    session_mkdir_p(VGPU_CONFIG_PATH);
  }
  /* Deliberately not O_TRUNC: truncation must happen after the write lock is
   * held, not at open() time, or a concurrent peer/reader races the empty
   * window. Same discipline as mmap_file_to_vmem_node. */
  int fd = open(CONTROLLER_CONFIG_FILE_PATH, O_CREAT | O_RDWR | O_CLOEXEC, 0644);
  if (unlikely(fd == -1)) {
    LOGGER(ERROR, "can't open %s, error %s", CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
    return ret;
  }
  /* Serialise concurrent creators on byte 0 of the header (the same byte
   * readers F_RDLCK). A dead writer's lock releases automatically on close. */
  struct flock fl;
  memset(&fl, 0, sizeof(fl));
  fl.l_type = F_WRLCK;
  fl.l_whence = SEEK_SET;
  fl.l_start = 0;
  fl.l_len = 1;
  if (unlikely(ofd_fcntl(fd, 1, &fl) == -1)) {
    LOGGER(ERROR, "can't lock %s: %s", CONTROLLER_CONFIG_FILE_PATH, strerror(errno));
    goto DONE;
  }
  /* If a peer already wrote a full-size, valid file under this lock, skip
   * the rewrite -- every process builds identical data from the same env, so
   * a peer's file is ours. Verify the header, not just the size: a
   * stale/corrupt file of the right length must still be rewritten. */
  struct stat sb;
  uint32_t hdr[4];
  if (fstat(fd, &sb) == 0 && sb.st_size == CONFIG_FILE_SIZE &&
      pread(fd, hdr, sizeof(hdr), 0) == (ssize_t)sizeof(hdr) &&
      hdr[0] == CONFIG_MAGIC && hdr[1] == CONFIG_LAYOUT_VERSION &&
      hdr[2] == (uint32_t)sizeof(resource_data_t) && hdr[3] == (uint32_t)MAX_DEVICE_COUNT) {
    ret = 0;
    goto DONE;
  }
  /* Stamp the frozen header so the validator in mmap_file_to_config_path
   * accepts whatever build path reached this writer. */
  data->magic          = CONFIG_MAGIC;
  data->layout_version = CONFIG_LAYOUT_VERSION;
  data->region_size    = sizeof(resource_data_t);
  data->device_count   = MAX_DEVICE_COUNT;
  /* Clear, write at offset 0, then size to the reserved total. Starting from 0
   * zeroes the reserved tail; the lock keeps any reader from seeing the middle;
   * the fixed final size means a later larger struct never resizes the file
   * (which would SIGBUS an old map). */
  if (unlikely(ftruncate(fd, 0) == -1) ||
      pwrite(fd, (void*)data, sizeof(resource_data_t), 0) != (ssize_t)sizeof(resource_data_t) ||
      ftruncate(fd, CONFIG_FILE_SIZE) == -1) {
    LOGGER(ERROR, "can't write %s to %d bytes: %s",
                  CONTROLLER_CONFIG_FILE_PATH, CONFIG_FILE_SIZE, strerror(errno));
    goto DONE;
  }
  ret = 0;
DONE:
  close(fd);   /* closing the fd releases its OFD lock */
  return ret;
}
/* Config lock helpers (config_device_read_lock / config_device_unlock) live in
 * lock.c, mirroring the device_util_* pattern. */
extern int  config_device_read_lock(int device_index);
extern void config_device_unlock(int fd, int device_index);

#define CONFIG_SEQ_SPIN_LIMIT 1024

static inline void config_cpu_relax(void) {
#if defined(__x86_64__)
  __builtin_ia32_pause();
#elif defined(__i386__)
  __asm__ __volatile__("pause" ::: "memory");
#elif defined(__aarch64__) || defined(__arm__)
  __asm__ __volatile__("yield" ::: "memory");
#else
  __asm__ __volatile__("" ::: "memory");
#endif
}

/* Tear-free snapshot of devices[host_index] via the per-device seqlock.
 *
 * Fast path is syscall-free: two acquire loads around a plain struct copy,
 * retried if the seq is odd (writer mid-update) or changed between loads.
 * The writer's update window is nanoseconds, so this almost never spins.
 *
 * Slow path (writer crashed mid-update, or we got descheduled past the spin
 * cap): take the per-device F_RDLCK once. A crashed writer's lock is already
 * released by the kernel on fd close, so this can't hang. */
device_t get_device_snapshot_of(const resource_data_t *cfg, int host_index) {
  device_t snap;
  if (unlikely(host_index < 0 || host_index >= MAX_DEVICE_COUNT || cfg == NULL)) {
    memset(&snap, 0, sizeof(snap));
    return snap;
  }
  const device_t *d = &cfg->devices[host_index];
  unsigned spins = 0;
  for (;;) {
    uint32_t s1 = __atomic_load_n(&d->seq, __ATOMIC_ACQUIRE);
    if (likely(!(s1 & 1u))) {
      /* Plain struct copy, deliberately -- standard seqlock discipline. A
       * torn copy is caught and discarded by the s1==s2 check below, and the
       * ACQUIRE fence stops the compiler hoisting a field read past that
       * check. A whole-struct atomic load isn't an option (device_t is 128B,
       * past any lock-free width -- __atomic_load would silently fall back
       * to a libatomic lock table and break cross-process safety). */
      snap = *d;
      __atomic_thread_fence(__ATOMIC_ACQUIRE);
      uint32_t s2 = __atomic_load_n(&d->seq, __ATOMIC_ACQUIRE);
      if (likely(s1 == s2)) return snap;          /* stable copy */
    }
    config_cpu_relax();
    if (unlikely(++spins >= CONFIG_SEQ_SPIN_LIMIT)) {
      int fd = config_device_read_lock(host_index);
      snap = *d;
      if (fd >= 0) config_device_unlock(fd, host_index);
      LOGGER(WARNING, "get_device_snapshot(%d): seqlock spin cap hit, RDLCK fallback",
             host_index);
      return snap;
    }
  }
}

/* Host indexes of the activated devices, ascending. Returns how many were
 * written (capped at `max`).
 *
 * This ordering IS the container's device numbering: entry i becomes both CUDA
 * ordinal i (via CUDA_VISIBLE_DEVICES, written in this order) and NVML index i
 * (via the GetHandleByIndex hook). Both sides must go through this one
 * function -- applications routinely assume "cuda:i is nvml i", which holds in
 * an ordinary container and would quietly stop holding here if the two orders
 * were derived separately.
 *
 * activate[] may be sparse, so this compacts rather than assuming a prefix. */
int config_allowed_devices(const resource_data_t *cfg, int *host_indexes, int max) {
  int count = 0;
  for (int i = 0; i < MAX_DEVICE_COUNT && count < max; i++) {
    if (get_device_snapshot_of(cfg, i).activate) {
      host_indexes[count++] = i;
    }
  }
  /* Stamp the tail with an index that is invalid everywhere rather than leaving
   * it as whatever was on the caller's stack. Reading past `count` is a caller
   * bug either way, but the two fail very differently: -1 is rejected by every
   * downstream check (is_valid_device_index, get_nvml_device_index_by_host_index,
   * the UUID lookup), while stack residue can easily look like a perfectly
   * valid device index and quietly resolve to the wrong physical GPU. */
  for (int i = count; i < max && i < MAX_DEVICE_COUNT; i++) {
    host_indexes[i] = -1;
  }
  return count;
}

/* Host index for the `visible_index`-th allowed device, or -1.
 *
 * The bounds check lives here, not at the call site, because getting it wrong
 * is an isolation bug rather than a crash: an out-of-range read yields some
 * other device's index, and the caller then serves a GPU this session was
 * never given. One implementation, and callers cannot skip it. */
int config_allowed_device_at(const resource_data_t *cfg, unsigned int visible_index) {
  int host_indexes[MAX_DEVICE_COUNT];
  int count = config_allowed_devices(cfg, host_indexes, MAX_DEVICE_COUNT);
  if (visible_index >= (unsigned int)count) {
    return -1;
  }
  return host_indexes[visible_index];
}

/* Inverse: where `host_index` sits in the visible ordering, or -1 if this
 * session was not given it. Passing -1 (an unmapped device) is expected and
 * answers -1, since the table never holds a negative index. */
int config_visible_index_of(const resource_data_t *cfg, int host_index) {
  if (host_index < 0) {
    return -1;
  }
  int host_indexes[MAX_DEVICE_COUNT];
  int count = config_allowed_devices(cfg, host_indexes, MAX_DEVICE_COUNT);
  for (int i = 0; i < count; i++) {
    if (host_indexes[i] == host_index) {
      return i;
    }
  }
  return -1;
}

/* Which file the live config was built from, so a caller can tell an inherited
 * config that is still correct from one that is not.
 *
 * Written under the same pthread_once as g_vgpu_config itself
 * (load_controller_configuration), so it needs no separate synchronisation. */
static char g_config_loaded_from[PATH_MAX];

/* 1 if the config path now resolves somewhere other than where the live config
 * was read from. Only a session change does that, and only after a fork: the
 * checkpoint provider publishes the child's session AFTER the fork, so the
 * child re-enters config loading holding the parent's mapping. Answering "no"
 * for a plain local fork is the point -- both sides resolve to the same file,
 * and re-mapping it would cost every forking CUDA process an extra mmap for
 * nothing. */
int config_source_moved(void) {
  if (g_config_loaded_from[0] == '\0') {
    return 0;
  }
  if (strcmp(g_config_loaded_from, CONTROLLER_CONFIG_FILE_PATH) == 0) {
    return 0;
  }
  LOGGER(VERBOSE, "config source moved from %s to %s, re-reading",
         g_config_loaded_from, CONTROLLER_CONFIG_FILE_PATH);
  return 1;
}

void config_source_record(void) {
  snprintf(g_config_loaded_from, sizeof(g_config_loaded_from), "%s",
           CONTROLLER_CONFIG_FILE_PATH);
}
