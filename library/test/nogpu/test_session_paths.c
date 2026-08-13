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
 * Session path derivation + config region round-trip. No GPU, no driver.
 *
 * Covers what reading the code cannot: that a session root actually moves
 * every per-session file (and only those), that the pre-session paths are
 * unchanged when no session is set, and that a region written by the tool is
 * the region the library maps back.
 */

#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "include/hook.h"
#include "include/session.h"

extern int lock_gpu_device(int device_index);
extern void unlock_gpu_device(int fd);

#define ROOT "/tmp/.vgpu_session_test/sess-1"

static int failures;

static void expect_str(const char *what, const char *got, const char *want) {
  if (strcmp(got, want) != 0) {
    printf("  [FAIL] %s\n         got  %s\n         want %s\n", what, got, want);
    failures++;
    return;
  }
  printf("  [ok] %-26s %s\n", what, got);
}

static void test_local_paths(void) {
  printf("local mode (no %s):\n", SESSION_PATH_ENV);
  unsetenv(SESSION_PATH_ENV);
  session_paths_reset();
  assert(session_enabled() == 0);
  expect_str("config", session_path(SESSION_CONFIG), CONTROLLER_CONFIG_FILE_PATH_LOCAL);
  expect_str("pids", session_path(SESSION_PIDS), CONTAINER_PIDS_CONFIG_FILE_PATH_LOCAL);
  expect_str("lock dir", session_path(SESSION_LOCK_DIR), VGPU_LOCK_PATH_LOCAL);
  expect_str("vmem file", session_path(SESSION_VMEM_FILE), VMEMORY_NODE_FILE_PATH_LOCAL);
  expect_str("sm file", session_path(SESSION_SM_FILE), SM_NODE_FILE_PATH_LOCAL);
  expect_str("sm lock", session_path(SESSION_SM_LOCK), SM_NODE_LOCK_PATH_LOCAL);
  expect_str("sm util", session_path(SESSION_SM_UTIL), CONTROLLER_SM_UTIL_FILE_PATH_LOCAL);
}

static void test_session_paths(void) {
  printf("session mode (%s=%s):\n", SESSION_PATH_ENV, ROOT);
  setenv(SESSION_PATH_ENV, ROOT, 1);
  session_paths_reset();
  assert(session_enabled() == 1);
  expect_str("config", session_path(SESSION_CONFIG), ROOT "/config/vgpu.config");
  expect_str("config dir", session_path(SESSION_CONFIG_DIR), ROOT "/config");
  expect_str("pids", session_path(SESSION_PIDS), ROOT "/pids.config");
  expect_str("lock dir", session_path(SESSION_LOCK_DIR), ROOT "/.vgpu_lock");
  expect_str("vmem file", session_path(SESSION_VMEM_FILE), ROOT "/.vmem_node/vmem_node.config");
  expect_str("sm file", session_path(SESSION_SM_FILE), ROOT "/.sm_node/sm_node.config");
  expect_str("sm lock", session_path(SESSION_SM_LOCK), ROOT "/.sm_node/sm_node.lock");
  /* Shared by every session on the node, so it hangs off the base, not the
   * session -- a per-session copy would give each container its own view of
   * a device utilization that is by definition node-wide. */
  expect_str("sm util (shared)", session_path(SESSION_SM_UTIL),
             "/tmp/.vgpu_session_test/watcher/sm_util.config");
}

/* A relative root cannot be joined with anything sensible, so the whole path
 * set must fall back rather than half-resolve. */
static void test_bad_root_falls_back(void) {
  printf("malformed root:\n");
  setenv(SESSION_PATH_ENV, "not-absolute", 1);
  session_paths_reset();
  if (session_enabled() != 0) {
    printf("  [FAIL] relative root was accepted\n");
    failures++;
  } else {
    expect_str("config falls back", session_path(SESSION_CONFIG),
               CONTROLLER_CONFIG_FILE_PATH_LOCAL);
  }
}

static void test_config_round_trip(void) {
  printf("config region round-trip:\n");
  setenv(SESSION_PATH_ENV, ROOT, 1);
  session_paths_reset();
  assert(session_make_dirs(ROOT) == 0);

  resource_data_t out;
  memset(&out, 0, sizeof(out));
  out.compatibility_mode = SESSION_COMPATIBILITY_MODE;
  snprintf(out.devices[0].uuid, UUID_BUFFER_SIZE, "GPU-aaaa");
  out.devices[0].activate = 1;
  out.devices[0].memory_limit = 1;
  out.devices[0].total_memory = 8ULL * 1024 * 1024 * 1024;
  out.devices[0].hard_core = 50;
  /* index 3, not 1: activate[] may be sparse, and a reader that assumes a
   * dense prefix would silently drop this device. */
  snprintf(out.devices[3].uuid, UUID_BUFFER_SIZE, "GPU-bbbb");
  out.devices[3].activate = 1;

  if (write_file_to_config_path(&out) != 0) {
    printf("  [FAIL] write_file_to_config_path\n");
    failures++;
    return;
  }

  resource_data_t *in = NULL;
  if (mmap_file_to_config_path(&in) != 0 || in == NULL) {
    printf("  [FAIL] mmap_file_to_config_path\n");
    failures++;
    return;
  }
  if (in->magic != CONFIG_MAGIC || in->layout_version != CONFIG_LAYOUT_VERSION ||
      in->region_size != sizeof(resource_data_t) || in->device_count != MAX_DEVICE_COUNT) {
    printf("  [FAIL] frozen header not stamped\n");
    failures++;
    return;
  }
  printf("  [ok] frozen header\n");
  if (in->compatibility_mode != SESSION_COMPATIBILITY_MODE) {
    printf("  [FAIL] compatibility_mode %d\n", in->compatibility_mode);
    failures++;
  }
  if (!in->devices[0].activate || in->devices[0].total_memory != out.devices[0].total_memory ||
      in->devices[0].hard_core != 50 || strcmp(in->devices[0].uuid, "GPU-aaaa") != 0) {
    printf("  [FAIL] devices[0] did not survive the round trip\n");
    failures++;
  }
  if (!in->devices[3].activate || strcmp(in->devices[3].uuid, "GPU-bbbb") != 0) {
    printf("  [FAIL] sparse devices[3] did not survive the round trip\n");
    failures++;
  }
  if (in->devices[1].activate) {
    printf("  [FAIL] devices[1] should be inactive\n");
    failures++;
  }
  printf("  [ok] devices survive the round trip (sparse activate included)\n");
}

/* The container's device numbering. devices[0] and devices[3] are active, so
 * the container must see exactly two devices, in that order -- CUDA ordinal i
 * and NVML index i both come from this list, so a compaction bug here would
 * silently point the two APIs at different cards. */
static void test_allowed_device_order(void) {
  printf("allowed device order:\n");
  resource_data_t *cfg = NULL;
  if (mmap_file_to_config_path(&cfg) != 0 || cfg == NULL) {
    printf("  [FAIL] could not map the config written above\n");
    failures++;
    return;
  }
  int host_indexes[MAX_DEVICE_COUNT];
  int count = config_allowed_devices(cfg, host_indexes, MAX_DEVICE_COUNT);
  if (count != 2 || host_indexes[0] != 0 || host_indexes[1] != 3) {
    printf("  [FAIL] expected [0,3], got %d entries: %d,%d\n", count,
           count > 0 ? host_indexes[0] : -1, count > 1 ? host_indexes[1] : -1);
    failures++;
  } else {
    printf("  [ok] sparse activate compacts to host indexes 0,3\n");
  }

  /* Truncation must clip, not overrun the caller's buffer. */
  int one[1];
  if (config_allowed_devices(cfg, one, 1) != 1 || one[0] != 0) {
    printf("  [FAIL] max= 1 did not clip to the first device\n");
    failures++;
  } else {
    printf("  [ok] honours the caller's cap\n");
  }
}

/* Where the per-device GPU lock FILE actually lands, not merely what the
 * directory macro expands to. The two came apart once: the file name was
 * concatenated from TMP_DIR at compile time while the directory followed the
 * session, so open() hit a path nothing had created, lock_gpu_device()
 * returned -1, and every caller ran its budget check unlocked. A macro-level
 * assertion would not have noticed; this one opens the lock and looks. */
static void test_gpu_lock_file_location(void) {
  printf("gpu lock file location:\n");
  char expected[PATH_MAX];

  setenv(SESSION_PATH_ENV, ROOT, 1);
  session_paths_reset();
  snprintf(expected, sizeof(expected), "%s/.vgpu_lock/vgpu_0.lock", ROOT);
  unlink(expected);
  int fd = lock_gpu_device(0);
  if (fd < 0) {
    printf("  [FAIL] lock_gpu_device returned %d in session mode\n", fd);
    failures++;
  } else if (access(expected, F_OK) != 0) {
    printf("  [FAIL] session lock file not at %s\n", expected);
    failures++;
  } else {
    printf("  [ok] session lock at %s\n", expected);
  }
  unlock_gpu_device(fd);

  unsetenv(SESSION_PATH_ENV);
  session_paths_reset();
  snprintf(expected, sizeof(expected), "%s/vgpu_0.lock", VGPU_LOCK_PATH_LOCAL);
  fd = lock_gpu_device(0);
  if (fd < 0 || access(expected, F_OK) != 0) {
    printf("  [FAIL] local lock file not at %s\n", expected);
    failures++;
  } else {
    printf("  [ok] local lock at %s\n", expected);
  }
  unlock_gpu_device(fd);
}

/* When a forked child must re-read the config and when it must not.
 *
 * A child inherits the parent's mapping. Re-reading unconditionally costs
 * every forking CUDA process a pointless mmap; never re-reading hands one
 * tenant's quota to another as soon as the child belongs to a different
 * session. The deciding question is whether the path still resolves the same,
 * which is what this pins down. */
static void test_config_source_moved(void) {
  printf("config source tracking:\n");

  setenv(SESSION_PATH_ENV, ROOT, 1);
  session_paths_reset();
  config_source_record();

  if (config_source_moved()) {
    printf("  [FAIL] reported moved with nothing changed (local fork would re-map)\n");
    failures++;
  } else {
    printf("  [ok] unchanged path -> no re-read\n");
  }

  /* The child of a lupine-server fork: same process image, different session. */
  setenv(SESSION_PATH_ENV, "/tmp/.vgpu_session_test/sess-2", 1);
  session_paths_reset();
  if (!config_source_moved()) {
    printf("  [FAIL] session change not detected -- child would keep the other session's quota\n");
    failures++;
  } else {
    printf("  [ok] session change -> re-read\n");
  }

  /* Leaving a session behind must count as a move too, not just entering one. */
  config_source_record();
  unsetenv(SESSION_PATH_ENV);
  session_paths_reset();
  if (!config_source_moved()) {
    printf("  [FAIL] session -> local not detected\n");
    failures++;
  } else {
    printf("  [ok] session -> local -> re-read\n");
  }
}

int main(void) {
  test_local_paths();
  test_session_paths();
  test_bad_root_falls_back();
  test_config_round_trip();
  test_allowed_device_order();
  test_gpu_lock_file_location();
  test_config_source_moved();

  if (failures != 0) {
    printf("\n%d check(s) FAILED\n", failures);
    return 1;
  }
  printf("\nall session path checks passed\n");
  return 0;
}
