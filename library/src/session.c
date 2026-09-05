/*
 * Copyright (c) 2024, vgpu-manager Authors. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include <errno.h>
#include <limits.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>

#include "include/hook.h"
#include "include/session.h"

/* One entry per session_path_id. `suffix` is appended to the session root,
 * except for SESSION_SM_UTIL which is shared node-wide and so hangs off the
 * base directory instead. `local` is the pre-session path, used verbatim
 * whenever no session is configured. */
typedef struct {
  const char *suffix;
  const char *local;
  int         from_base;
} path_spec;

static const path_spec SPECS[SESSION_PATH_COUNT] = {
  [SESSION_CONFIG_DIR] = {"/config",                                    VGPU_CONFIG_PATH_LOCAL,             0},
  [SESSION_CONFIG]     = {"/config/" CONTROLLER_CONFIG_FILE_NAME,       CONTROLLER_CONFIG_FILE_PATH_LOCAL,  0},
  [SESSION_PIDS]       = {"/" CONTAINER_PIDS_CONFIG_FILE_NAME,          CONTAINER_PIDS_CONFIG_FILE_PATH_LOCAL, 0},
  [SESSION_LOCK_DIR]   = {VGPU_LOCK_DIR,                                VGPU_LOCK_PATH_LOCAL,               0},
  [SESSION_VMEM_DIR]   = {VMEMORY_NODE_DIR,                             VMEMORY_NODE_PATH_LOCAL,            0},
  [SESSION_VMEM_FILE]  = {VMEMORY_NODE_DIR "/vmem_node.config",         VMEMORY_NODE_FILE_PATH_LOCAL,       0},
  [SESSION_SM_DIR]     = {SM_NODE_DIR,                                  SM_NODE_PATH_LOCAL,                 0},
  [SESSION_SM_FILE]    = {SM_NODE_DIR "/sm_node.config",                SM_NODE_FILE_PATH_LOCAL,            0},
  [SESSION_SM_LOCK]    = {SM_NODE_DIR "/sm_node.lock",                  SM_NODE_LOCK_PATH_LOCAL,            0},
  [SESSION_SM_UTIL]    = {"/watcher/" CONTROLLER_SM_UTIL_FILE_NAME,     CONTROLLER_SM_UTIL_FILE_PATH_LOCAL, 1},
};

/* Subdirectories session_make_dirs() creates under a session root. The quota
 * file's parent is here; the flat files (pids.config) need no directory. */
static const char *const SUBDIRS[] = {"/config", VGPU_LOCK_DIR, VMEMORY_NODE_DIR, SM_NODE_DIR};

static char g_paths[SESSION_PATH_COUNT][PATH_MAX];
static int  g_enabled;
static int  g_remote;
static pthread_once_t g_once = PTHREAD_ONCE_INIT;

const char *session_base(void) {
  const char *base = getenv(SESSION_BASE_ENV);
  return (base != NULL && base[0] != '\0') ? base : SESSION_BASE_DEFAULT;
}

/* Copy `root` minus its last component into `out`. Used only for
 * SESSION_SM_UTIL, whose file is shared by every session on the node. */
static int parent_dir(const char *root, char *out, size_t len) {
  const char *slash = strrchr(root, '/');
  if (slash == NULL || slash == root) {
    return -1;
  }
  size_t n = (size_t)(slash - root);
  if (n >= len) {
    return -1;
  }
  memcpy(out, root, n);
  out[n] = '\0';
  return 0;
}

/* Read the environment once and materialise every path. Falls back to local
 * mode as a whole -- a half-session/half-local path set would scatter one
 * container's state across two locations. */
static void session_init(void) {
  const char *root = getenv(SESSION_PATH_ENV);
  const char *remote = getenv(REMOTE_MODE_ENV);
  char base[PATH_MAX];

  g_remote = (remote != NULL && remote[0] != '\0' && strcmp(remote, "0") != 0);
  g_enabled = 0;
  if (root != NULL && root[0] == '/' && parent_dir(root, base, sizeof(base)) == 0) {
    g_enabled = 1;
    for (int i = 0; i < SESSION_PATH_COUNT; i++) {
      const char *prefix = SPECS[i].from_base ? base : root;
      if (snprintf(g_paths[i], PATH_MAX, "%s%s", prefix, SPECS[i].suffix) >= PATH_MAX) {
        LOGGER(ERROR, "session path %s%s exceeds PATH_MAX, falling back to local paths",
               prefix, SPECS[i].suffix);
        g_enabled = 0;
        break;
      }
    }
  } else if (root != NULL && root[0] != '\0') {
    LOGGER(ERROR, "%s=\"%s\" is not an absolute path below a base directory, "
                  "falling back to local paths", SESSION_PATH_ENV, root);
  }

  if (!g_enabled) {
    for (int i = 0; i < SESSION_PATH_COUNT; i++) {
      snprintf(g_paths[i], PATH_MAX, "%s", SPECS[i].local);
    }
    return;
  }
  LOGGER(INFO, "session paths rooted at %s", root);
}

const char *session_path(session_path_id id) {
  pthread_once(&g_once, session_init);
  if (unlikely(id < 0 || id >= SESSION_PATH_COUNT)) {
    return SPECS[SESSION_CONFIG].local;
  }
  return g_paths[id];
}

int session_enabled(void) {
  pthread_once(&g_once, session_init);
  return g_enabled;
}

int session_remote_mode(void) {
  pthread_once(&g_once, session_init);
  return g_remote;
}

void session_paths_reset(void) {
  g_once = (pthread_once_t)PTHREAD_ONCE_INIT;
}

int session_mkdir_p(const char *path) {
  char tmp[PATH_MAX];
  if (path == NULL || snprintf(tmp, sizeof(tmp), "%s", path) >= (int)sizeof(tmp)) {
    return -1;
  }
  size_t len = strlen(tmp);
  while (len > 1 && tmp[len - 1] == '/') {
    tmp[--len] = '\0';
  }
  if (len == 0) {
    return -1;
  }
  for (char *p = tmp + 1; *p != '\0'; p++) {
    if (*p != '/') {
      continue;
    }
    *p = '\0';
    if (mkdir(tmp, 0755) != 0 && errno != EEXIST) {
      return -1;
    }
    *p = '/';
  }
  return (mkdir(tmp, 0755) != 0 && errno != EEXIST) ? -1 : 0;
}

int session_make_dirs(const char *root) {
  if (root == NULL || session_mkdir_p(root) != 0) {
    return -1;
  }
  for (size_t i = 0; i < sizeof(SUBDIRS) / sizeof(SUBDIRS[0]); i++) {
    char sub[PATH_MAX];
    if (snprintf(sub, sizeof(sub), "%s%s", root, SUBDIRS[i]) >= (int)sizeof(sub) ||
        session_mkdir_p(sub) != 0) {
      return -1;
    }
  }
  return 0;
}
