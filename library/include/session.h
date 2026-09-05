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

/*
 * Per-session path derivation.
 *
 * lupine-server forks one child per client connection, so a single GPU node
 * hosts children of many different containers at once. The checkpoint provider
 * gives each child a VGPU_CONFIG_SESSION_PATH naming its own session directory;
 * everything that used to be node-global (quota, PID list, GPU lock, ledgers)
 * is derived from it, which is what keeps one container's state out of
 * another's. With the env unset the library is in plain local mode and every
 * path falls back to its historical location, so library-remote still runs
 * unchanged inside an ordinary container.
 *
 * Layout (see design doc 4.3.3):
 *   <base>/<session>/config/vgpu.config   quota, resource_data_t + seqlock
 *   <base>/<session>/pids.config          session PID list, SESSION accounting
 *   <base>/<session>/.vgpu_lock           per-session GPU lock directory
 *   <base>/<session>/.vmem_node           per-session vmem ledger
 *   <base>/<session>/.sm_node             per-session SM bucket
 *   <base>/watcher/sm_util.config         shared by every session on the node
 */

#ifndef _VGPU_SESSION_H_
#define _VGPU_SESSION_H_

#ifdef __cplusplus
extern "C" {
#endif

/* Env naming the session directory of the calling process. Set by the
 * checkpoint provider inside each lupine-server child, never by the operator. */
#define SESSION_PATH_ENV "VGPU_CONFIG_SESSION_PATH"

/* Env naming where session directories live. Operator-settable on the server. */
#define SESSION_BASE_ENV "VGPU_CONFIG_SESSION_BASE"

#define SESSION_BASE_DEFAULT "/etc/vgpu-manager/remote-sessions"

/* Set process-wide on the lupine-server. Marks "this process only ever serves
 * remote sessions", which is what lets the library refuse to run without a
 * valid session quota instead of falling back to a permissive local config. */
#define REMOTE_MODE_ENV "VGPU_REMOTE_MODE"

typedef enum {
  SESSION_CONFIG_DIR,  /* directory holding vgpu.config                  */
  SESSION_CONFIG,      /* the quota file itself                          */
  SESSION_PIDS,        /* PID list backing SESSION-mode accounting       */
  SESSION_LOCK_DIR,    /* per-device GPU lock files live here            */
  SESSION_VMEM_DIR,
  SESSION_VMEM_FILE,
  SESSION_SM_DIR,
  SESSION_SM_FILE,
  SESSION_SM_LOCK,
  SESSION_SM_UTIL,     /* shared across sessions, hence <base>/watcher/  */
  SESSION_PATH_COUNT
} session_path_id;

/* Absolute path for `id`. Never NULL: an out-of-range id or an unusable
 * session root yields the local-mode path. */
const char *session_path(session_path_id id);

/* 1 when SESSION_PATH_ENV named a usable directory, 0 in local mode. */
int session_enabled(void);

/* 1 when REMOTE_MODE_ENV marks this process as serving remote sessions only. */
int session_remote_mode(void);

/* Re-read the environment on the next session_path() call. Only safe while
 * single-threaded -- the two callers are the fork-child handler and the
 * provider's restore(), both of which run before any watcher thread exists. */
void session_paths_reset(void);

/* mkdir -p. Returns 0 on success or when the directory already exists. */
int session_mkdir_p(const char *path);

/* Create <root> and every per-session subdirectory. Idempotent. */
int session_make_dirs(const char *root);

/* Value of SESSION_BASE_ENV, or SESSION_BASE_DEFAULT. */
const char *session_base(void);

#ifdef __cplusplus
}
#endif

#endif /* _VGPU_SESSION_H_ */
