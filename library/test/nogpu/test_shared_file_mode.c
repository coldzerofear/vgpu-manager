/* Checks for relax_shared_file_mode() in src/util.c.
 *
 * The plugin creates the directories holding the library's shared state 0777
 * so that a workload running as any uid can populate them, but the library
 * created the files inside them 0644: whoever got there first owned them, and
 * a second uid sharing the directory -- two containers on one DRA claim with
 * different runAsUser, or an entrypoint that drops privileges -- could not
 * open them O_RDWR. These files are widened after creation instead.
 *
 * The first case is the reason this is an fchmod and not a mode argument to
 * open(): a mode argument is masked by the process umask, so a hardened
 * image's umask would quietly defeat it. That is the property most likely to
 * be lost in a later "simplification", so it is pinned here.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/stat.h>

#include "include/hook.h"

static int failures = 0;

#define CHECK(cond, ...)                                                       \
  do {                                                                         \
    if (!(cond)) {                                                             \
      printf("FAIL %s:%d: ", __func__, __LINE__);                              \
      printf(__VA_ARGS__);                                                     \
      printf("\n");                                                            \
      failures++;                                                              \
    }                                                                          \
  } while (0)

static mode_t mode_of(const char *path) {
  struct stat sb;
  if (stat(path, &sb) != 0) {
    return 0;
  }
  return sb.st_mode & 07777;
}

/* Creates a file the way the library does, under a umask hostile enough to
 * prove the point, and returns its fd (or -1). */
static int make_file(char *path_out, size_t path_size) {
  snprintf(path_out, path_size, "/tmp/vgpu_shared_mode_test_XXXXXX");
  int fd = mkstemp(path_out);
  if (fd < 0) {
    return -1;
  }
  /* mkstemp always makes 0600; chmod to what the library's open() asked for
   * so the starting point matches production. */
  if (fchmod(fd, 0644) != 0) {
    close(fd);
    unlink(path_out);
    return -1;
  }
  return fd;
}

static void test_widens_past_the_umask(void) {
  char path[64];
  mode_t saved = umask(077); /* the mode argument to open() would yield 0600 */
  int fd = make_file(path, sizeof(path));
  umask(saved);
  if (fd < 0) {
    printf("SKIP: could not create a temp file\n");
    return;
  }

  CHECK(mode_of(path) == 0644, "precondition: mode was %04o", mode_of(path));

  relax_shared_file_mode(fd, path);

  CHECK(mode_of(path) == VGPU_SHARED_FILE_MODE,
        "mode is %04o, expected %04o -- a second uid sharing the directory "
        "cannot open this O_RDWR",
        mode_of(path), VGPU_SHARED_FILE_MODE);

  close(fd);
  unlink(path);
}

static void test_once_runs_exactly_once(void) {
  char path[64];
  int fd = make_file(path, sizeof(path));
  if (fd < 0) {
    printf("SKIP: could not create a temp file\n");
    return;
  }
  unsigned char slot = 0;

  relax_shared_file_mode_once(fd, path, &slot);
  CHECK(mode_of(path) == VGPU_SHARED_FILE_MODE, "first call left mode %04o",
        mode_of(path));
  CHECK(slot != 0, "the slot must be marked after the first call");

  /* Narrow it again behind the helper's back: a second call must not touch
   * it, which is what keeps the per-allocation lock path free of syscalls. */
  CHECK(fchmod(fd, 0600) == 0, "could not narrow the file for the second half");
  relax_shared_file_mode_once(fd, path, &slot);
  CHECK(mode_of(path) == 0600,
        "a second call re-ran the fchmod (mode %04o); the lock path would pay "
        "a syscall on every allocation",
        mode_of(path));

  close(fd);
  unlink(path);
}

static void test_failure_is_survivable(void) {
  /* Only the owner may fchmod, so in the case this hardening exists for --
   * the file belongs to another uid -- the call fails. It must stay quiet and
   * return normally; the caller's fd is usable either way. A closed
   * descriptor reproduces the failure without needing a second uid. */
  relax_shared_file_mode(-1, "/tmp/vgpu-not-a-file");

  unsigned char slot = 0;
  relax_shared_file_mode_once(-1, "/tmp/vgpu-not-a-file", &slot);
  CHECK(slot != 0, "a failed attempt still counts as attempted");
}

int main(void) {
  test_widens_past_the_umask();
  test_once_runs_exactly_once();
  test_failure_is_survivable();

  if (failures) {
    printf("%d check(s) failed\n", failures);
    return 1;
  }
  printf("shared file mode: all checks passed\n");
  return 0;
}
