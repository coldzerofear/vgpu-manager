/* Bounds checks for the cgroup line parser in src/util.c.
 *
 * This test exists because split_str() used to take no buffer sizes at all.
 * read_cgroup() handed it two 128-byte stack arrays, and a stock Kubernetes
 * node's cgroup path -- systemd driver plus containerd, 191 bytes -- was
 * copied straight into them, overflowing by 64 bytes on every call. Nothing
 * in the suite touched this parser, so the overflow sat there unnoticed.
 *
 * Run under ASan (make test-sanitize) the first case below faults on the old
 * code and passes on the new; the rest pin the semantics the cgroup matcher
 * depends on, so a later "cleanup" cannot quietly reintroduce truncation.
 */
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <stddef.h>
#include <unistd.h>

#include "include/hook.h"

extern int split_str(const char *line, char *key, size_t key_size,
                     char *value, size_t value_size, char d);
extern int read_cgroup(const char *pid_path, const char *cgroup_key,
                       char *cgroup_value, size_t value_size);

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

/* A real line from a node running the systemd cgroup driver on containerd.
 * The third field is 191 bytes -- the length that used to do the damage. */
#define REAL_PATH                                                              \
  "/kubepods.slice/kubepods-burstable.slice/"                                  \
  "kubepods-burstable-pod9a96a35d_b3ce_4431_ad3e_022ac59309c1.slice/"          \
  "cri-containerd-"                                                            \
  "5f6cd15b77a056b53826e9a348275933f23c4da5788151fbf3fd9560fe43ee48.scope"

/* split_str is given the text after the hierarchy id, as read_cgroup does. */
#define REAL_TAIL "memory:" REAL_PATH

static void test_oversized_value_is_refused(void) {
  char key[CGROUP_KEY_MAX];
  char value[128]; /* the size that used to be overflowed */
  value[sizeof(value) - 1] = 'X';

  int rc = split_str(REAL_TAIL, key, sizeof(key), value, sizeof(value), ':');

  CHECK(rc == -1, "a value that cannot fit must be refused, got rc=%d", rc);
  CHECK(strlen(value) == 0, "a refused split must not leave a partial value");
}

static void test_real_path_round_trips(void) {
  char key[CGROUP_KEY_MAX];
  char value[CGROUP_LINE_MAX];

  int rc = split_str(REAL_TAIL, key, sizeof(key), value, sizeof(value), ':');

  CHECK(rc == 0, "a real cgroup line must parse, got rc=%d", rc);
  CHECK(strcmp(key, "memory") == 0, "key was '%s'", key);
  CHECK(strcmp(value, REAL_PATH) == 0, "value was '%s'", value);
  CHECK(strlen(value) == 191, "value length was %zu, expected 191",
        strlen(value));
}

static void test_oversized_key_is_refused(void) {
  /* cgroup v1 merges controllers into one field, so the key grows too. */
  char line[512];
  memset(line, 'c', sizeof(line) - 1);
  line[sizeof(line) - 1] = '\0';
  line[400] = ':';

  char key[CGROUP_KEY_MAX];
  char value[CGROUP_LINE_MAX];
  int rc = split_str(line, key, sizeof(key), value, sizeof(value), ':');

  CHECK(rc == -1, "a key that cannot fit must be refused, got rc=%d", rc);
}

static void test_missing_delimiter(void) {
  char key[CGROUP_KEY_MAX];
  char value[CGROUP_LINE_MAX];

  int rc = split_str("no-delimiter-at-all", key, sizeof(key), value,
                     sizeof(value), ':');

  CHECK(rc == 1, "a line without the delimiter must report 1, got rc=%d", rc);
  CHECK(strlen(key) == 0 && strlen(value) == 0,
        "both fields must be cleared when there is nothing to split");
}

static void test_trimming_matches_legacy_behaviour(void) {
  char key[CGROUP_KEY_MAX];
  char value[CGROUP_LINE_MAX];

  /* Leading blanks are trimmed from both fields; a trailing newline, as
   * fgets leaves it, is trimmed from the value. */
  int rc = split_str("  memory:\t/a/b\n", key, sizeof(key), value,
                     sizeof(value), ':');

  CHECK(rc == 0, "rc=%d", rc);
  CHECK(strcmp(key, "memory") == 0, "key was '%s'", key);
  CHECK(strcmp(value, "/a/b") == 0, "value was '%s'", value);
}

static void test_zero_sized_buffers(void) {
  char key[CGROUP_KEY_MAX];
  char value[CGROUP_LINE_MAX];

  CHECK(split_str(REAL_TAIL, key, 0, value, sizeof(value), ':') == -1,
        "a zero-sized key buffer must be refused, not written to");
  CHECK(split_str(REAL_TAIL, key, sizeof(key), value, 0, ':') == -1,
        "a zero-sized value buffer must be refused, not written to");
}

static void test_read_cgroup_reads_a_real_file(void) {
  char tmpl[] = "/tmp/vgpu_cgroup_test_XXXXXX";
  int fd = mkstemp(tmpl);
  if (fd < 0) {
    printf("SKIP: mkstemp failed\n");
    return;
  }
  FILE *f = fdopen(fd, "w");
  if (!f) {
    close(fd);
    unlink(tmpl);
    printf("SKIP: fdopen failed\n");
    return;
  }
  /* Two leading hierarchies so the match is not just "first line wins". */
  fprintf(f, "12:pids:" REAL_PATH "\n");
  fprintf(f, "11:memory:" REAL_PATH "\n");
  fclose(f);

  char value[CGROUP_LINE_MAX];
  int rc = read_cgroup(tmpl, "memory", value, sizeof(value));
  CHECK(rc == 0, "read_cgroup must find the memory hierarchy, got rc=%d", rc);
  CHECK(strcmp(value, REAL_PATH) == 0, "value was '%s'", value);

  /* An undersized buffer must fail rather than return a truncated path:
   * the caller substring-matches it against the host's path, and a
   * truncated prefix would answer the wrong question. */
  char small[128];
  rc = read_cgroup(tmpl, "memory", small, sizeof(small));
  CHECK(rc != 0, "an undersized buffer must not report success");

  /* A hierarchy that is not in the file. */
  rc = read_cgroup(tmpl, "cpuset", value, sizeof(value));
  CHECK(rc != 0, "an absent hierarchy must not report success");

  unlink(tmpl);
}

static void test_read_cgroup_missing_file(void) {
  char value[CGROUP_LINE_MAX];
  CHECK(read_cgroup("/nonexistent/vgpu/cgroup", "memory", value,
                    sizeof(value)) != 0,
        "a missing file must not report success");
}

int main(void) {
  test_oversized_value_is_refused();
  test_real_path_round_trips();
  test_oversized_key_is_refused();
  test_missing_delimiter();
  test_trimming_matches_legacy_behaviour();
  test_zero_sized_buffers();
  test_read_cgroup_reads_a_real_file();
  test_read_cgroup_missing_file();

  if (failures) {
    printf("%d check(s) failed\n", failures);
    return 1;
  }
  printf("cgroup parser bounds: all checks passed\n");
  return 0;
}
