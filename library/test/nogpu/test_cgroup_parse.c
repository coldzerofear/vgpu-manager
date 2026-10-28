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
#include "include/cgroup_match.h"

extern int split_str(const char *line, char *key, size_t key_size,
                     char *value, size_t value_size, char d);
/* Takes an open stream, not a path: one /proc/<pid>/cgroup is now asked for
 * more than one controller (the v2 shape then the v1 shape), and reopening it
 * per key would cost a second fopen on a per-PID path. */
extern int read_cgroup(FILE *fp, const char *cgroup_key,
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
  /* Two leading hierarchies so the match is not just "first line wins", plus a
   * comma-joined controller list and a v2 unified line -- the shapes a real
   * file mixes and the ones the key matcher has to tell apart. */
  fprintf(f, "12:pids:" REAL_PATH "\n");
  fprintf(f, "11:memory:" REAL_PATH "\n");
  fprintf(f, "5:net_prio,net_cls:" REAL_PATH "\n");
  fprintf(f, "0::" REAL_PATH "\n");
  fclose(f);

  FILE *r = fopen(tmpl, "re");
  CHECK(r != NULL, "could not reopen the temp cgroup file");
  if (r == NULL) {
    unlink(tmpl);
    return;
  }

  char value[CGROUP_LINE_MAX];
  int rc = read_cgroup(r, "memory", value, sizeof(value));
  CHECK(rc == 0, "read_cgroup must find the memory hierarchy, got rc=%d", rc);
  CHECK(strcmp(value, REAL_PATH) == 0, "value was '%s'", value);

  /* The same stream, asked for a second controller. This is how the caller
   * uses it -- v2 shape then v1 shape on one open file -- and it only works
   * because read_cgroup rewinds: a previous miss (or hit) leaves the stream
   * part-way through, and without the rewind every later key would fail for
   * the position rather than for the contents. */
  rc = read_cgroup(r, "pids", value, sizeof(value));
  CHECK(rc == 0, "a second key on the same stream must still be found "
                 "(read_cgroup must rewind), got rc=%d", rc);
  CHECK(strcmp(value, REAL_PATH) == 0, "value was '%s'", value);

  /* A key inside a comma-joined controller list. */
  rc = read_cgroup(r, "net_cls", value, sizeof(value));
  CHECK(rc == 0, "a key in a comma-joined list must be found, got rc=%d", rc);

  /* An empty key addresses the v2 unified line, and ONLY that line: on a v1
   * host every line carries a controller name, so asking for "" must not
   * match one of them. */
  rc = read_cgroup(r, "", value, sizeof(value));
  CHECK(rc == 0, "the empty key must match the \"0::\" unified line, got rc=%d", rc);
  CHECK(strcmp(value, REAL_PATH) == 0, "value was '%s'", value);

  /* A controller that is a prefix of a real one must not match. */
  rc = read_cgroup(r, "mem", value, sizeof(value));
  CHECK(rc != 0, "\"mem\" must not match the \"memory\" hierarchy");

  /* An undersized buffer must fail rather than return a truncated path: the
   * caller prefix-matches it against another process's path, and a path cut at
   * a '/' also prefixes sibling cgroups -- a sibling container's process would
   * be claimed as ours. */
  char small[128];
  rc = read_cgroup(r, "memory", small, sizeof(small));
  CHECK(rc != 0, "an undersized buffer must not report success");

  /* A hierarchy that is not in the file. */
  rc = read_cgroup(r, "cpuset", value, sizeof(value));
  CHECK(rc != 0, "an absent hierarchy must not report success");

  fclose(r);
  unlink(tmpl);
}

/* Why the CALLER'S key order matters on a cgroup v1 host, pinned here because
 * the order itself lives in cuda_hook.c, which the no-GPU tests cannot reach.
 *
 * This is the hybrid shape measured on a real host (Ubuntu 24.04 booted with
 * systemd.unified_cgroup_hierarchy=0): the v1 hierarchies carry a path per
 * container, and cgroup2 is mounted as well -- but whether the container has a
 * group of its own in the unified hierarchy is the runtime's choice. Where it
 * does not, the unified line degenerates while the memory line stays correct,
 * and the two keys then disagree about the same process. Asking "" first would
 * read the degenerate answer: every container on the host reads the same
 * string, so a sibling container's process prefix-matches ours and gets
 * claimed -- over-counting, which throttles the container against memory it
 * does not hold. Hence {"memory", ""} when the manager says cgroup v1.
 *
 * If this test ever starts reporting the two keys as equal, the fixture has
 * stopped representing the shape and the order has stopped being testable. */
static void test_hybrid_unified_line_can_degenerate(void) {
  char tmpl[] = "/tmp/vgpu_cgroup_hybrid_XXXXXX";
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
  fprintf(f, "13:memory:" REAL_PATH "\n");
  fprintf(f, "9:pids:" REAL_PATH "\n");
  fprintf(f, "0::/\n");            /* the degenerate unified line */
  fclose(f);

  FILE *r = fopen(tmpl, "re");
  CHECK(r != NULL, "could not reopen the hybrid fixture");
  if (r == NULL) {
    unlink(tmpl);
    return;
  }

  char unified[CGROUP_LINE_MAX], memory[CGROUP_LINE_MAX];
  CHECK(read_cgroup(r, "", unified, sizeof(unified)) == 0,
        "the unified line must still be readable");
  CHECK(read_cgroup(r, "memory", memory, sizeof(memory)) == 0,
        "the memory hierarchy must still be readable");
  CHECK(strcmp(unified, "/") == 0,
        "the fixture's unified line should be \"/\", got '%s'", unified);
  CHECK(strcmp(memory, REAL_PATH) == 0,
        "the memory hierarchy should carry the per-container path, got '%s'",
        memory);
  CHECK(strcmp(unified, memory) != 0,
        "the two keys must disagree here, or this fixture no longer "
        "represents the shape that makes the key order load-bearing");

  /* And the consequence, stated in the matcher's own terms: the degenerate
   * answer is not specific to any container, so it cannot distinguish ours
   * from a sibling's, while the memory path can. */
  CHECK(cgroup_path_is_ours(unified, "/") == 1,
        "a degenerate \"/\" self path claims an unrelated \"/\"");
  CHECK(cgroup_path_is_ours(memory, REAL_PATH) == 1,
        "the per-container path must still claim our own process");

  fclose(r);
  unlink(tmpl);
}

/* Opening is the caller's job now, so what used to be "a missing file" is a
 * NULL stream here. The other guards are checked alongside it: every one of
 * them is a path that must not write to the output buffer. */
static void test_read_cgroup_rejects_bad_arguments(void) {
  char value[CGROUP_LINE_MAX];
  char tmpl[] = "/tmp/vgpu_cgroup_null_XXXXXX";
  int fd = mkstemp(tmpl);

  CHECK(read_cgroup(NULL, "memory", value, sizeof(value)) != 0,
        "a NULL stream must not report success");

  if (fd < 0) {
    printf("SKIP %s: mkstemp failed\n", __func__);
    return;
  }
  FILE *f = fdopen(fd, "w+");
  if (f == NULL) {
    close(fd);
    unlink(tmpl);
    printf("SKIP %s: fdopen failed\n", __func__);
    return;
  }
  fprintf(f, "11:memory:" REAL_PATH "\n");
  fflush(f);

  CHECK(read_cgroup(f, NULL, value, sizeof(value)) != 0,
        "a NULL key must not report success");
  CHECK(read_cgroup(f, "memory", NULL, sizeof(value)) != 0,
        "a NULL output buffer must not report success");
  CHECK(read_cgroup(f, "memory", value, 0) != 0,
        "a zero-sized output buffer must not report success");

  fclose(f);
  unlink(tmpl);
}

int main(void) {
  test_oversized_value_is_refused();
  test_real_path_round_trips();
  test_oversized_key_is_refused();
  test_missing_delimiter();
  test_trimming_matches_legacy_behaviour();
  test_zero_sized_buffers();
  test_read_cgroup_reads_a_real_file();
  test_hybrid_unified_line_can_degenerate();
  test_read_cgroup_rejects_bad_arguments();

  if (failures) {
    printf("%d check(s) failed\n", failures);
    return 1;
  }
  printf("cgroup parser bounds: all checks passed\n");
  return 0;
}
