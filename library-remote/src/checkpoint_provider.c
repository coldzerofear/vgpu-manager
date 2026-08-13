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
 * LUPINE checkpoint provider embedded into library-remote (libvgpu-remote.so).
 *
 * lupine-server dlopens the provider per connection child and calls:
 *   start()      - after fork, before the first CUDA call
 *   restore(id)  - before the first CUDA RPC is dispatched (id = LUPINE_SESSION)
 *   checkpoint() - after SIGTERM drain
 *   stop()       - on child shutdown (this is the child's exit-cleanup hook)
 *
 * The same libvgpu-remote.so is both the LD_PRELOAD'd hook library (C-1) and
 * this provider (C-2): lupine dlopen()s it (via LUPINE_CHECKPOINT_LIBRARY) and
 * dlsym()s lupinecr_get_lupine_provider_v1; the four callbacks are reached
 * through the returned struct, so they need not be exported themselves.
 *
 * What restore()/stop() do here (design docs/remote_gpu_pool_research_design.md
 * §4.3.3 / §6):
 *   restore(id)  sanitizes the client-supplied session id, idempotently creates
 *                the per-session directories, checks the session quota exists,
 *                setenv()s VGPU_CONFIG_SESSION_PATH (+ REMOTE mode) so the
 *                library resolves every per-session path from it, and
 *                registers this child's PID into <session>/pids.config keeping
 *                the list sorted + deduplicated so the library's SESSION-mode
 *                accounting can filter NVML by the session's container PIDs
 *                with binary search. Returns non-zero when the config region
 *                is missing -> lupine closes the connection (fail-closed).
 *   stop()       removes this child's PID from pids.config (child exit),
 *                keeping the list sorted.
 *
 * Session layout (base defaults to /etc/vgpu-manager/remote-sessions, override
 * via VGPU_CONFIG_SESSION_BASE). All per-session paths derive from
 * VGPU_CONFIG_SESSION_PATH=<base>/<session>:
 *   <base>/<session>/config/vgpu.config   session quota (resource_data_t)
 *   <base>/<session>/.vgpu_lock           per-session GPU lock dir
 *   <base>/<session>/.vmem_node           per-session vmem ledger
 *   <base>/<session>/.sm_node             per-session SM shared bucket
 *   <base>/<session>/pids.config          session container PIDs (sorted here)
 *   <base>/watcher/sm_util.config         shared SM watcher (external writer)
 */

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <unistd.h>

#include "include/checkpoint_provider.h"
#include "include/hook.h"

#define SESSION_CONFIG_BASE_DEFAULT "/etc/vgpu-manager/remote-sessions"
#define SESSION_ID_MAX 64
#define SESSION_PIDS_MAX 256
#define SESSION_PIDS_FILE_MAX (1024 * 1024) /* sanity cap for a rewrite */

/* Per-session subdirectories created idempotently by restore(). */
static const char *const SESSION_SUBDIRS[] = {
    "config", ".vgpu_lock", ".vmem_node", ".sm_node",
};

static const char *session_config_base(void) {
  const char *base = getenv("VGPU_CONFIG_SESSION_BASE");
  return (base != NULL && base[0] != '\0') ? base : SESSION_CONFIG_BASE_DEFAULT;
}

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

/* mkdir -p semantics; a pre-existing directory is not an error (idempotent). */
static int mkdir_p(const char *path) {
  char tmp[PATH_MAX];
  if (snprintf(tmp, sizeof(tmp), "%s", path) >= (int)sizeof(tmp)) {
    return -1;
  }
  size_t len = strlen(tmp);
  if (len == 0) {
    return -1;
  }
  while (len > 1 && tmp[len - 1] == '/') {
    tmp[--len] = '\0';
  }
  for (char *p = tmp + 1; *p; p++) {
    if (*p == '/') {
      *p = '\0';
      if (mkdir(tmp, 0755) != 0 && errno != EEXIST) {
        return -1;
      }
      *p = '/';
    }
  }
  if (mkdir(tmp, 0755) != 0 && errno != EEXIST) {
    return -1;
  }
  return 0;
}

/* Create every per-session directory under <session> (idempotent). */
static int ensure_session_dirs(const char *session_path) {
  if (mkdir_p(session_path) != 0) {
    return -1;
  }
  for (size_t i = 0; i < sizeof(SESSION_SUBDIRS) / sizeof(SESSION_SUBDIRS[0]);
       i++) {
    char sub[PATH_MAX];
    if (snprintf(sub, sizeof(sub), "%s/%s", session_path,
                 SESSION_SUBDIRS[i]) >= (int)sizeof(sub) ||
        mkdir_p(sub) != 0) {
      return -1;
    }
  }
  return 0;
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
static int write_pids(int fd, const int *pids, int count) {
  if (ftruncate(fd, 0) != 0 || lseek(fd, 0, SEEK_SET) < 0) {
    return -1;
  }
  for (int i = 0; i < count; i++) {
    char line[32];
    int n = snprintf(line, sizeof(line), "%d\n", pids[i]);
    if (write(fd, line, (size_t)n) != n) {
      return -1;
    }
  }
  return 0;
}

/* Register `pid`: no-op if already present; otherwise insert, sort ascending
 * and overwrite, keeping pids.config always sorted so readers can binary
 * search (the library's check_device_pid_in_ordered_container_pids does). */
static int session_register_pid(const char *session_path, pid_t pid) {
  char pids_path[PATH_MAX];
  if (snprintf(pids_path, sizeof(pids_path), "%s/pids.config", session_path) >=
      (int)sizeof(pids_path)) {
    return -1;
  }
  int fd = open(pids_path, O_RDWR | O_CREAT | O_CLOEXEC, 0644);
  if (fd < 0) {
    LOGGER(ERROR, "failed to open %s: %s", pids_path, strerror(errno));
    return -1;
  }
  int rc = -1;
  if (flock(fd, LOCK_EX) == 0) {
    int pids[SESSION_PIDS_MAX];
    int count = 0;
    if (read_pids(fd, pids, SESSION_PIDS_MAX, &count) == 0) {
      qsort(pids, (size_t)count, sizeof(int), pid_compare);
      if (bsearch(&pid, pids, (size_t)count, sizeof(int), pid_compare) == NULL) {
        if (count >= SESSION_PIDS_MAX) {
          LOGGER(ERROR, "session pid list full (%d) in %s", count, pids_path);
        } else {
          pids[count++] = (int)pid;
          qsort(pids, (size_t)count, sizeof(int), pid_compare);
          rc = (write_pids(fd, pids, count) == 0) ? 0 : -1;
        }
      } else {
        rc = 0; /* already registered */
      }
    } else {
      LOGGER(ERROR, "failed to read %s: %s", pids_path, strerror(errno));
    }
    (void)flock(fd, LOCK_UN);
  } else {
    LOGGER(ERROR, "flock failed on %s: %s", pids_path, strerror(errno));
  }
  close(fd);
  return rc;
}

/* Remove `pid` from pids.config, keeping the file sorted. Dead PIDs are
 * harmless to accounting (they no longer appear in NVML), so this is purely
 * hygiene at child exit. */
static void session_unregister_pid(const char *session_path, pid_t pid) {
  char pids_path[PATH_MAX];
  if (snprintf(pids_path, sizeof(pids_path), "%s/pids.config", session_path) >=
      (int)sizeof(pids_path)) {
    return;
  }
  int fd = open(pids_path, O_RDWR | O_CLOEXEC);
  if (fd < 0) {
    return; /* never registered / already removed */
  }
  if (flock(fd, LOCK_EX) != 0) {
    close(fd);
    return;
  }

  int pids[SESSION_PIDS_MAX];
  int count = 0;
  if (read_pids(fd, pids, SESSION_PIDS_MAX, &count) == 0) {
    int kept = 0;
    for (int i = 0; i < count; i++) {
      if (pids[i] != (int)pid) {
        pids[kept++] = pids[i]; /* already sorted: removal preserves order */
      }
    }
    if (kept != count) {
      (void)write_pids(fd, pids, kept);
    }
  }
  (void)flock(fd, LOCK_UN);
  close(fd);
}

static int checkpoint_start(void) {
  LOGGER(INFO, "provider start()");
  return 0;
}

static int checkpoint_restore(const char *connection_id) {
  LOGGER(INFO, "provider restore() connection_id=%s",
         connection_id == NULL ? "<null>" : connection_id);

  if (!valid_session_id(connection_id)) {
    LOGGER(ERROR, "invalid or unsafe session id, refusing connection");
    return -1; /* lupine closes the connection (fail-closed) */
  }

  char session_path[PATH_MAX];
  if (snprintf(session_path, sizeof(session_path), "%s/%s",
               session_config_base(), connection_id) >=
      (int)sizeof(session_path)) {
    LOGGER(ERROR, "session path too long");
    return -1;
  }

  /* Idempotently create the per-session directories (no error if present):
   * the library later mmaps/creates .vgpu_lock/.vmem_node/.sm_node files. */
  if (ensure_session_dirs(session_path) != 0) {
    LOGGER(ERROR, "failed to create session dirs under %s: %s",
           session_path, strerror(errno));
    return -1;
  }

  /* The agent must have materialized the session quota before the pod's first
   * CUDA call. Without it the child must not serve (fail-closed). */
  char config_path[PATH_MAX];
  if (snprintf(config_path, sizeof(config_path), "%s/config/vgpu.config",
               session_path) >= (int)sizeof(config_path) ||
      access(config_path, R_OK) != 0) {
    LOGGER(ERROR, "session config not found: %s", config_path);
    return -1;
  }

  if (setenv("VGPU_CONFIG_SESSION_PATH", session_path, 1) != 0 ||
      setenv("VGPU_REMOTE_MODE", "1", 0) != 0) {
    LOGGER(ERROR, "setenv failed: %s", strerror(errno));
    return -1;
  }

  if (session_register_pid(session_path, getpid()) != 0) {
    LOGGER(ERROR, "failed to register pid %ld in session %s",
           (long)getpid(), connection_id);
    return -1;
  }
  LOGGER(INFO, "registered pid %ld into %s/pids.config",
         (long)getpid(), session_path);
  return 0;
}

static int checkpoint_checkpoint(const char *connection_id) {
  LOGGER(INFO, "provider checkpoint() connection_id=%s",
         connection_id == NULL ? "<null>" : connection_id);
  /* TODO(remote): no-op for now (we do not persist GPU state). */
  return 0;
}

static void checkpoint_stop(void) {
  LOGGER(INFO, "provider stop()");
  const char *session_path = getenv("VGPU_CONFIG_SESSION_PATH");
  if (session_path != NULL && session_path[0] != '\0') {
    session_unregister_pid(session_path, getpid());
    LOGGER(INFO, "removed pid %ld from %s/pids.config",
           (long)getpid(), session_path);
  }
}

static const lupine_checkpoint_provider_v1 checkpoint_provider = {
    sizeof(checkpoint_provider), LUPINE_CHECKPOINT_PROVIDER_ABI_VERSION,
    checkpoint_start, checkpoint_restore, checkpoint_checkpoint,
    checkpoint_stop};

const lupine_checkpoint_provider_v1 *lupinecr_get_lupine_provider_v1(void) {
  return &checkpoint_provider;
}
