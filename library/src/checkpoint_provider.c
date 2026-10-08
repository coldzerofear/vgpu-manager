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
 * LUPINE checkpoint provider, built into library/ (libvgpu-control.so).
 *
 * lupine-server dlopens the provider per connection child and calls:
 *   start()      - after fork, before the first CUDA call
 *   restore(id)  - before the first CUDA RPC is dispatched (id = LUPINE_SESSION)
 *   checkpoint() - after SIGTERM drain
 *   stop()       - on child shutdown (this is the child's exit-cleanup hook)
 *
 * The same libvgpu-control.so is both the LD_PRELOAD'd hook library (C-1) and
 * this provider (C-2): lupine dlopen()s it (via LUPINE_CHECKPOINT_LIBRARY) and
 * dlsym()s lupinecr_get_lupine_provider_v1; the four callbacks are reached
 * through the returned struct, so they need not be exported themselves.
 *
 * What restore()/stop() do here (design docs/remote_gpu_pool_research_design.md
 * §4.3.3 / §6):
 *   restore(id)  sanitizes the client-supplied session id, idempotently creates
 *                the per-session directories, setenv()s VGPU_CONFIG_SESSION_PATH
 *                (+ REMOTE mode) so the library resolves every per-session path
 *                from it, maps the session quota to publish CUDA_VISIBLE_DEVICES
 *                for that session's devices, and registers this child's PID into
 *                <session>/pids.config keeping the list sorted + deduplicated so
 *                the library's SESSION-mode accounting can filter NVML by the
 *                session's container PIDs with binary search. Returns non-zero
 *                when the quota is missing or empty -> lupine closes the
 *                connection (fail-closed).
 *   stop()       removes this child's PID from pids.config (child exit),
 *                keeping the list sorted.
 *
 * The session directory layout itself lives in session.h -- this file only
 * decides which session the child belongs to and publishes it.
 *
 * Downstream chaining: lupine only has room for one LUPINE_CHECKPOINT_LIBRARY,
 * and we already occupy that slot. VGPU_CHECKPOINT_LIBRARY lets an operator
 * chain a second, independent provider underneath us -- e.g. a real GPU state
 * checkpoint/restore tool -- without lupine ever knowing it exists:
 * lupine -> us -> that provider. See load_downstream_provider() and the
 * ordering note in checkpoint_restore(). lupinecr_cuda_symbol_v1() at the
 * bottom of this file is a transparent passthrough of the same kind, for
 * lupine's separate (non-struct) optional-override lookup -- we never
 * override anything ourselves, we just forward the question down.
 */

#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/file.h>
#include <sys/mman.h>
#include <unistd.h>

#include "include/checkpoint_provider.h"
#include "include/hook.h"

#define SESSION_ID_MAX 64
#define SESSION_PIDS_MAX 256
#define SESSION_PIDS_FILE_MAX (1024 * 1024) /* sanity cap for a rewrite */

#define VGPU_CHECKPOINT_LIBRARY_ENV "VGPU_CHECKPOINT_LIBRARY"
#define LUPINE_CHECKPOINT_LIBRARY_ENV "LUPINE_CHECKPOINT_LIBRARY"

/* "stop" is the ABI's last required field (checkpoint_provider.h); anything
 * shorter did not finish declaring all four callbacks. Mirrors the
 * required_size check in lupine's own server_checkpoint.cpp load_provider(). */
#define DOWNSTREAM_PROVIDER_REQUIRED_SIZE \
  (offsetof(lupine_checkpoint_provider_v1, stop) + sizeof(void (*)(void)))

extern int pid_exist(int pid);

/* Session id is client-controlled and feeds a filesystem path: allow only a
 * safe charset and reject anything that could traverse (design §6.2.1). */
static int valid_session_id(const char *id) {
  if (id == NULL || id[0] == '\0' || strlen(id) >= SESSION_ID_MAX) {
    return 0;
  }
  for (const char *p = id; *p; p++) {
    if (!((*p >= 'a' && *p <= 'z') || (*p >= 'A' && *p <= 'Z') ||
          (*p >= '0' && *p <= '9') || *p == '_' || *p == '.' || *p == '-')) {
      return 0;
    }
  }
  return 1;
}

static int pid_compare(const void *a, const void *b) {
  return *(const int *)a - *(const int *)b;
}

/* Read decimal PIDs (one per line) into a caller buffer. Caller must hold the
 * file lock. Returns 0 on success (count may be 0). */
static int read_pids(int fd, int *pids, int max, int *count) {
  *count = 0;
  off_t size = lseek(fd, 0, SEEK_END);
  if (size <= 0 || size > SESSION_PIDS_FILE_MAX) {
    return 0;
  }
  char *buf = malloc((size_t)size + 1);
  if (buf == NULL || lseek(fd, 0, SEEK_SET) < 0 ||
      read(fd, buf, (size_t)size) != size) {
    free(buf);
    return -1;
  }
  buf[size] = '\0';
  char *save = NULL;
  int n = 0;
  for (char *tok = strtok_r(buf, "\n", &save); tok != NULL && n < max;
       tok = strtok_r(NULL, "\n", &save)) {
    char *end = NULL;
    long v = strtol(tok, &end, 10);
    if (end != tok && v > 0 && v <= INT_MAX) {
      pids[n++] = (int)v;
    }
  }
  free(buf);
  *count = n;
  return 0;
}

/* Rewrite the file with `count` PIDs (truncate + write). Caller holds the
 * lock and has sorted pids[0..count). */
/* Whole list in one pwrite, THEN shrink -- never truncate first.
 *
 * Readers take LOCK_SH but give up after ~1ms and read anyway rather than
 * stall a CUDA call behind a wedged writer (see lock_pids_config_shared in
 * util.c). That is only safe while a reader cannot observe a SHORT file, which
 * is exactly what ftruncate-then-write produced: land after the truncate and
 * the list reads back empty, which the accounting path treats as "this
 * container never registered" and answers with LOGGER(FATAL); land mid-write
 * and it reads back partial, so used memory is under-counted and the container
 * gets past a limit it should not.
 *
 * Writing first and shrinking after leaves one benign window instead: while a
 * shrinking list is being written, a reader may see the new PIDs followed by
 * stale trailing ones from the longer previous list. Those match nothing in
 * the NVML process table, which is the tolerated case the reader documents. */
static int write_pids(int fd, const int *pids, int count) {
  /* Bounded by SESSION_PIDS_MAX; 12 covers "-2147483648\n" so no entry can
   * overflow its slot regardless of what was parsed out of the file. */
  char buf[SESSION_PIDS_MAX * 12];
  size_t len = 0;

  for (int i = 0; i < count; i++) {
    int n = snprintf(buf + len, sizeof(buf) - len, "%d\n", pids[i]);
    if (n < 0 || (size_t)n >= sizeof(buf) - len) {
      return -1;
    }
    len += (size_t)n;
  }
  if (pwrite(fd, buf, len, 0) != (ssize_t)len) {
    return -1;
  }
  return ftruncate(fd, (off_t)len) == 0 ? 0 : -1;
}

/* Rewrite pids.config as: the live PIDs it already held, minus `pid` when
 * removing, plus `pid` when adding, sorted ascending. Both callers go through
 * here so the pruning and the sort order have exactly one implementation.
 *
 * The pruning matters because stop() is not guaranteed to run: a child killed
 * by SIGKILL (or a crashing one) never removes itself, so without a sweep the
 * file grows stale entries for the life of the session. Accounting tolerates
 * them -- NVML stops reporting a dead PID -- but the list is bounded, and a
 * session that churns children would otherwise hit that bound and start
 * refusing registrations. A recycled PID can survive the sweep; harmless,
 * since NVML attribution is what decides usage, not this list.
 *
 * `add` == 0 leaves the file alone when it does not exist: nothing to clean. */
static int session_pids_update(const char *pids_path, pid_t pid, int add) {
  int fd = open(pids_path, add ? (O_RDWR | O_CREAT | O_CLOEXEC) : (O_RDWR | O_CLOEXEC), 0644);
  if (fd < 0) {
    if (add) {
      LOGGER(ERROR, "failed to open %s: %s", pids_path, strerror(errno));
    }
    return add ? -1 : 0;
  }
  if (flock(fd, LOCK_EX) != 0) {
    LOGGER(ERROR, "flock failed on %s: %s", pids_path, strerror(errno));
    close(fd);
    return -1;
  }

  int rc = -1;
  int pids[SESSION_PIDS_MAX];
  int count = 0;
  if (read_pids(fd, pids, SESSION_PIDS_MAX, &count) != 0) {
    LOGGER(ERROR, "failed to read %s: %s", pids_path, strerror(errno));
    goto DONE;
  }

  int kept = 0;
  for (int i = 0; i < count; i++) {
    if (pids[i] != (int)pid && pid_exist(pids[i]) == 0) {
      pids[kept++] = pids[i];
    }
  }
  if (kept != count) {
    LOGGER(VERBOSE, "pruned %d stale pid(s) from %s", count - kept, pids_path);
  }
  if (add) {
    if (kept >= SESSION_PIDS_MAX) {
      LOGGER(ERROR, "session pid list full (%d) in %s", kept, pids_path);
      goto DONE;
    }
    pids[kept++] = (int)pid;
  }
  qsort(pids, (size_t)kept, sizeof(int), pid_compare);
  rc = write_pids(fd, pids, kept) == 0 ? 0 : -1;

DONE:
  (void)flock(fd, LOCK_UN);
  close(fd);
  return rc;
}

/* Restrict the child to the session's devices by handing the driver a
 * CUDA_VISIBLE_DEVICES list of their UUIDs.
 *
 * The driver applies this at cuInit, which has not happened yet: lupine's
 * first RPC is cuInit and restore() runs before it, and the server's parent
 * never touches CUDA, so nothing in this process has initialized the driver.
 * That is what makes a plain setenv work here.
 *
 * Letting the driver enumerate is worth more than filtering cuDeviceGetCount /
 * cuDeviceGet ourselves would be: the client builds its device table from
 * those two RPCs, so it sees exactly the allowed devices, already renumbered
 * 0..n-1, with no hook of ours in the path. UUIDs rather than indexes because
 * the config names devices by UUID and PCI order is not stable across reboots.
 *
 * NVML is NOT affected by CUDA_VISIBLE_DEVICES -- it always enumerates every
 * physical device -- so the NVML side is restricted by hooks instead.
 *
 * We assume every UUID in the config exists on this node. CUDA's contract for
 * an unknown entry is to silently truncate the visible list at that point, so
 * a stale config surfaces as "the container sees fewer GPUs than it asked
 * for", not as an error. Deliberate: validating would mean initializing NVML
 * here, and this is the one place that must not touch the driver. */
static int apply_visible_devices(void) {
  resource_data_t *cfg = NULL;
  if (mmap_file_to_config_path(&cfg) != 0 || cfg == NULL) {
    LOGGER(ERROR, "no readable session quota at %s", session_path(SESSION_CONFIG));
    return -1;
  }

  int host_indexes[MAX_DEVICE_COUNT];
  int count = config_allowed_devices(cfg, host_indexes, MAX_DEVICE_COUNT);
  int rc = -1;
  char list[MAX_DEVICE_COUNT * UUID_BUFFER_SIZE];
  size_t len = 0;

  if (count == 0) {
    /* An empty allowlist is a real quota ("no GPUs"), not a missing one, and
     * must not become "all GPUs" -- which is what an unset env would mean. */
    LOGGER(ERROR, "session quota activates no device; refusing connection");
    goto DONE;
  }
  for (int i = 0; i < count; i++) {
    device_t d = get_device_snapshot_of(cfg, host_indexes[i]);
    int n = snprintf(list + len, sizeof(list) - len, "%s%s", len ? "," : "", d.uuid);
    if (n < 0 || (size_t)n >= sizeof(list) - len) {
      LOGGER(ERROR, "device uuid list overflow at device %d", i);
      goto DONE;
    }
    len += (size_t)n;
  }
  if (setenv("CUDA_VISIBLE_DEVICES", list, 1) != 0) {
    LOGGER(ERROR, "setenv CUDA_VISIBLE_DEVICES failed: %s", strerror(errno));
    goto DONE;
  }
  LOGGER(INFO, "session exposes %d device(s): %s", count, list);
  rc = 0;

DONE:
  munmap(cfg, CONFIG_FILE_SIZE);
  return rc;
}

/* State for the optional second provider VGPU_CHECKPOINT_LIBRARY chains to.
 * One child process, one connection, so no locking: every field is written
 * only from load_downstream_provider()/checkpoint_stop(), both called on the
 * child's single thread (same guarantee restore() itself relies on). */
typedef struct {
  void *handle;
  const lupine_checkpoint_provider_v1 *api;
  /* lupine's optional cuMemFree_v2-override hook (server_checkpoint.cpp):
   * not part of lupine_checkpoint_provider_v1, looked up by lupine via a
   * separate dlsym("lupinecr_cuda_symbol_v1") against this .so directly.
   * NULL if the downstream provider does not export one -- this is the
   * expected case, not an error. */
  void *(*cuda_symbol)(const char *name);
  int broken; /* VGPU_CHECKPOINT_LIBRARY was set but failed to load/start */
} downstream_provider_t;

static downstream_provider_t g_downstream;

/* Loads the optional downstream provider. Validation mirrors lupine's own
 * load_provider() in server_checkpoint.cpp: same symbol, same ABI check.
 * Unset or "none" leaves g_downstream all-zero -- the default, and a no-op
 * for every deployment that does not set this env var.
 *
 * A configured-but-broken path is recorded in g_downstream.broken rather than
 * failing checkpoint_start() here: lupine treats a nonzero start() as "this
 * whole provider is unusable" and discards it (server_checkpoint.cpp),
 * which would silently disable ALL of our own hooks' enforcement for the
 * connection instead of refusing it -- restore() is the callback lupine
 * actually treats as fail-closed, so that is where we surface this instead
 * (see the check at the top of checkpoint_restore()). */
static void load_downstream_provider(void) {
  const char *path = getenv(VGPU_CHECKPOINT_LIBRARY_ENV);
  if (path == NULL || path[0] == '\0' || strcmp(path, "none") == 0) {
    return;
  }

  /* Pointing this at ourselves (lupine's LUPINE_CHECKPOINT_LIBRARY, which is
   * this very .so) would dlopen our own lupinecr_get_lupine_provider_v1,
   * which would call our own start() again, which would load this "downstream"
   * provider again, forever -- catch the one-hop case cheaply instead of
   * recursing until the stack overflows. A longer cycle through an actual
   * third-party library would be that library's own bug to avoid. */
  const char *outer = getenv(LUPINE_CHECKPOINT_LIBRARY_ENV);
  if (outer != NULL && strcmp(path, outer) == 0) {
    LOGGER(ERROR, "%s must not name the same library as %s",
           VGPU_CHECKPOINT_LIBRARY_ENV, LUPINE_CHECKPOINT_LIBRARY_ENV);
    g_downstream.broken = 1;
    return;
  }

  void *handle = dlopen(path, RTLD_NOW | RTLD_LOCAL);
  if (handle == NULL) {
    LOGGER(ERROR, "%s=%s: dlopen failed: %s", VGPU_CHECKPOINT_LIBRARY_ENV, path,
           dlerror());
    g_downstream.broken = 1;
    return;
  }

  lupine_checkpoint_provider_get_v1_fn get_provider =
      (lupine_checkpoint_provider_get_v1_fn)dlsym(
          handle, LUPINE_CHECKPOINT_PROVIDER_SYMBOL);
  const lupine_checkpoint_provider_v1 *api =
      get_provider != NULL ? get_provider() : NULL;
  if (api == NULL || api->struct_size < DOWNSTREAM_PROVIDER_REQUIRED_SIZE ||
      api->abi_version != LUPINE_CHECKPOINT_PROVIDER_ABI_VERSION ||
      api->start == NULL || api->restore == NULL || api->checkpoint == NULL ||
      api->stop == NULL) {
    LOGGER(ERROR, "%s=%s: incompatible checkpoint provider",
           VGPU_CHECKPOINT_LIBRARY_ENV, path);
    dlclose(handle);
    g_downstream.broken = 1;
    return;
  }
  if (api->start() != 0) {
    LOGGER(ERROR, "%s=%s: provider start() failed", VGPU_CHECKPOINT_LIBRARY_ENV,
           path);
    dlclose(handle);
    g_downstream.broken = 1;
    return;
  }

  g_downstream.handle = handle;
  g_downstream.api = api;
  /* Optional and independent of the ABI check above: absent is normal. */
  g_downstream.cuda_symbol =
      (void *(*)(const char *))dlsym(handle, "lupinecr_cuda_symbol_v1");
  LOGGER(INFO, "chained downstream checkpoint provider %s", path);
}

static int checkpoint_start(void) {
  LOGGER(INFO, "provider start()");
  load_downstream_provider();
  return 0;
}

static int checkpoint_restore(const char *connection_id) {
  LOGGER(INFO, "provider restore() connection_id=%s",
         connection_id == NULL ? "<null>" : connection_id);

  if (g_downstream.broken) {
    LOGGER(ERROR, "%s is set but failed to load; refusing connection",
           VGPU_CHECKPOINT_LIBRARY_ENV);
    return -1; /* lupine closes the connection (fail-closed) */
  }

  if (!valid_session_id(connection_id)) {
    LOGGER(ERROR, "invalid or unsafe session id, refusing connection");
    return -1; /* lupine closes the connection (fail-closed) */
  }

  char root[PATH_MAX];
  if (snprintf(root, sizeof(root), "%s/%s", session_base(), connection_id) >=
      (int)sizeof(root)) {
    LOGGER(ERROR, "session path too long");
    return -1;
  }

  /* Idempotently create the per-session directories (no error if present):
   * the library later mmaps/creates .vgpu_lock/.vmem_node/.sm_node files. */
  if (session_make_dirs(root) != 0) {
    LOGGER(ERROR, "failed to create session dirs under %s: %s", root, strerror(errno));
    return -1;
  }

  /* Publish the root before touching any per-session file, so the layout is
   * only ever spelled out in session.c. The reset is what makes the new env
   * visible -- paths are resolved once and cached. Safe here: restore() runs
   * on the child's only thread, before the first RPC. */
  if (setenv(SESSION_PATH_ENV, root, 1) != 0 || setenv(REMOTE_MODE_ENV, "1", 0) != 0) {
    LOGGER(ERROR, "setenv failed: %s", strerror(errno));
    return -1;
  }
  session_paths_reset();

  /* The agent must have materialized the session quota before the pod's first
   * CUDA call. Without it the child must not serve (fail-closed). Mapping it
   * rather than access()ing it also validates the frozen header, and gives us
   * the device list for the visibility env below. */
  if (apply_visible_devices() != 0) {
    return -1;
  }

  if (session_pids_update(session_path(SESSION_PIDS), getpid(), 1) != 0) {
    LOGGER(ERROR, "failed to register pid %ld in session %s", (long)getpid(), connection_id);
    return -1;
  }
  LOGGER(INFO, "registered pid %ld into %s", (long)getpid(), session_path(SESSION_PIDS));

  /* Our own masking (apply_visible_devices() above) is fully committed by
   * this point; only now may the downstream provider touch the driver.
   * CUDA_VISIBLE_DEVICES only has an effect up to this process's first
   * cuInit (see the comment on apply_visible_devices()), so calling the
   * downstream provider any earlier could let it initialize the driver
   * before the mask is in place -- and once that happens it cannot be
   * undone for the rest of this process's life.
   *
   * TODO(remote): once a real downstream provider exists, additionally
   * verify by NVML that the device(s) it actually restored onto are still
   * within config_allowed_devices(). CUDA_VISIBLE_DEVICES only constrains
   * the CUDA Driver API's own enumeration -- a provider that selects a
   * device through NVML or a raw ioctl underneath it is not bound by it,
   * the same way our own NVML hooks exist because NVML itself is not. */
  if (g_downstream.api != NULL && g_downstream.api->restore(connection_id) != 0) {
    LOGGER(ERROR, "downstream checkpoint provider refused connection %s",
           connection_id);
    (void)session_pids_update(session_path(SESSION_PIDS), getpid(), 0);
    return -1;
  }
  return 0;
}

static int checkpoint_checkpoint(const char *connection_id) {
  LOGGER(INFO, "provider checkpoint() connection_id=%s",
         connection_id == NULL ? "<null>" : connection_id);
  /* TODO(remote): no-op for now (we do not persist GPU state). */
  return g_downstream.api != NULL ? g_downstream.api->checkpoint(connection_id) : 0;
}

static void checkpoint_stop(void) {
  LOGGER(INFO, "provider stop()");
  /* Reverse of load_downstream_provider()'s start() -- unwind it regardless
   * of whether restore() ever ran, since start() (and this) run per child
   * regardless of that. */
  if (g_downstream.api != NULL) {
    g_downstream.api->stop();
  }
  if (g_downstream.handle != NULL) {
    dlclose(g_downstream.handle);
  }
  g_downstream = (downstream_provider_t){0};

  if (!session_enabled()) {
    return; /* restore() never ran: nothing was registered */
  }
  (void)session_pids_update(session_path(SESSION_PIDS), getpid(), 0);
  LOGGER(INFO, "removed pid %ld from %s", (long)getpid(), session_path(SESSION_PIDS));
}

static const lupine_checkpoint_provider_v1 checkpoint_provider = {
    sizeof(checkpoint_provider), LUPINE_CHECKPOINT_PROVIDER_ABI_VERSION,
    checkpoint_start, checkpoint_restore, checkpoint_checkpoint,
    checkpoint_stop};

const lupine_checkpoint_provider_v1 *lupinecr_get_lupine_provider_v1(void) {
  return &checkpoint_provider;
}

/* lupine dlsym()s this directly off this .so (independently of the provider
 * struct above) to ask whether to use a provider-supplied replacement for a
 * native CUDA call instead of the real driver -- currently only cuMemFree_v2,
 * when releasing a VMM allocation restored from a checkpoint. We never
 * answer this ourselves; we only forward to whatever the downstream
 * provider exports under the same name, so chaining one underneath us costs
 * it nothing. Returning NULL (no downstream provider, or one that does not
 * export this) tells lupine to use the real driver call, same as today. */
void *lupinecr_cuda_symbol_v1(const char *name) {
  return g_downstream.cuda_symbol != NULL ? g_downstream.cuda_symbol(name) : NULL;
}
