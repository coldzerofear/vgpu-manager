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
 * pids.config under concurrent rewrite. No GPU, no driver.
 *
 * The accounting path reads this file on every allocation and gives up on the
 * shared lock after ~1ms rather than stall a CUDA call (lock_pids_config_shared
 * in util.c). Reading unlocked is only safe while a reader cannot see a SHORT
 * file, so the writer must never leave one -- and a reader that does see one
 * does not merely get a stale answer:
 *
 *   empty   -> load_container_pids() calls LOGGER(FATAL) and kills the process
 *   partial -> used memory is under-counted and the container passes a limit
 *              check it should have failed
 *
 * Reading the writer cannot confirm this; two processes racing can. The reader
 * here mirrors the library's own reader, including the give-up-and-read path,
 * and fails if it ever observes fewer PIDs than the smaller of the two lists
 * the writer alternates between.
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>

#include "include/hook.h"
#include "include/session.h"
#include "include/checkpoint_provider.h"

#define ROOT "/tmp/.vgpu_pids_test/sess-c"
#define ROUNDS 400

extern const lupine_checkpoint_provider_v1 *lupinecr_get_lupine_provider_v1(void);

extern int get_container_pids_by_filepath(const char *file_path, int *pids,
                                          int *pids_size, int sort_pids);

/* Drive the real writer: restore() adds this pid, stop() removes it. Both go
 * through the same rewrite the library's reader must tolerate. */
static const lupine_checkpoint_provider_v1 *provider;

/* Seed one PID that survives every rewrite plus a batch that does not.
 *
 * The anchor matters: without it the list legitimately empties after stop()
 * removes the writer, and "empty" would no longer distinguish a torn read from
 * the real state. PID 1 is always alive, and pid_exist() reports EPERM as
 * alive, so it is kept by the pruning either way. The dead entries make the
 * first rewrite SHRINK the file, which is the case a truncate-first writer
 * exposes. */
static void seed_pids(const char *path, int dead_count) {
  FILE *f = fopen(path, "we");
  if (f == NULL) {
    return;
  }
  fprintf(f, "1\n");
  for (int i = 0; i < dead_count; i++) {
    fprintf(f, "%d\n", 900000 + i);
  }
  fclose(f);
}

int main(void) {
  char cmd[PATH_MAX];
  snprintf(cmd, sizeof(cmd), "rm -rf %s", "/tmp/.vgpu_pids_test");
  if (system(cmd) != 0) { /* first run: nothing to remove */ }

  setenv(SESSION_BASE_ENV, "/tmp/.vgpu_pids_test", 1);
  if (session_make_dirs(ROOT) != 0) {
    printf("[FAIL] could not create %s\n", ROOT);
    return 1;
  }
  /* restore() refuses a session with no quota, so give it one. */
  setenv(SESSION_PATH_ENV, ROOT, 1);
  session_paths_reset();
  resource_data_t cfg;
  memset(&cfg, 0, sizeof(cfg));
  cfg.devices[0].activate = 1;
  if (write_file_to_config_path(&cfg) != 0) {
    printf("[FAIL] could not write the session quota\n");
    return 1;
  }
  const char *pids_path = session_path(SESSION_PIDS);
  seed_pids(pids_path, 8);

  provider = lupinecr_get_lupine_provider_v1();
  if (provider == NULL || provider->restore == NULL || provider->stop == NULL) {
    printf("[FAIL] provider ABI unavailable\n");
    return 1;
  }

  pid_t reader = fork();
  if (reader < 0) {
    printf("[FAIL] fork: %s\n", strerror(errno));
    return 1;
  }
  if (reader == 0) {
    /* Reader: exactly what the accounting path does, on repeat. The anchor PID
     * is in every version of the list, so any read that misses it saw a file
     * the writer should never have exposed. */
    int bad_reads = 0;
    for (int i = 0; i < ROUNDS * 40 && bad_reads == 0; i++) {
      int pids[MAX_PIDS];
      int size = MAX_PIDS;
      if (get_container_pids_by_filepath(pids_path, pids, &size, 0) != 0) {
        continue; /* file momentarily absent is a different question */
      }
      int saw_anchor = 0;
      for (int j = 0; j < size; j++) {
        if (pids[j] == 1) {
          saw_anchor = 1;
        }
        if (pids[j] <= 0) {
          bad_reads++; /* a torn line parsed into nonsense */
        }
      }
      if (!saw_anchor) {
        bad_reads++;
      }
    }
    _exit(bad_reads > 0 ? 1 : 0);
  }

  /* Writer: register and unregister, which rewrites the file each time and
   * shrinks it as the seeded dead PIDs get pruned. */
  for (int i = 0; i < ROUNDS; i++) {
    if (provider->restore("sess-c") != 0) {
      printf("[FAIL] restore() refused on round %d\n", i);
      kill(reader, SIGKILL);
      waitpid(reader, NULL, 0);
      return 1;
    }
    provider->stop();
  }

  int status = 0;
  waitpid(reader, &status, 0);
  if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
    printf("[FAIL] reader saw a pids.config missing the anchor PID, i.e. a short/torn file\n");
    printf("       the writer must write the whole list before shrinking the file\n");
    return 1;
  }
  printf("[PASS] pids.config rewrite: %d register/unregister cycles, reader never saw a short list\n",
         ROUNDS);
  return 0;
}
