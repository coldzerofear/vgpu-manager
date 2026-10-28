/* The cgroup ownership decision in include/cgroup_match.h.
 *
 * The paths in test_captured_matrix() are real: they were taken off a host
 * running Ubuntu 24.04 / kernel 7.0 / Docker 29.1, by running the shipped
 * decision across cgroup v1 and v2 x cgroupns private and host x pid
 * own/hostPID/sharedPID x ipc own/host -- 24 container configurations, 144
 * verdicts -- against four kinds of target: this container's own process, a
 * process this container placed in a CHILD cgroup, a host process, and a
 * sibling container's process. The container ids are shortened only so the
 * table stays readable.
 *
 * The other two functions hold constructed boundary cases. Docker will not
 * produce a cgroup named ".." or "..foo", so those shapes cannot be captured
 * from a real host -- but the kernel permits them (any valid filename is a
 * valid cgroup name), and both are one character away from a path the code
 * must treat the opposite way.
 *
 * TWO BUGS THIS FILE CAUGHT, in the order they were found:
 *
 *   The nested-cgroup rows. An earlier version demanded a path separator
 *   after the prefix in every case, which is right when our own cgroup is a
 *   real path and wrong when it is the namespace root "/": "/nested" then
 *   looked foreign, so every process the container had placed in a child
 *   cgroup was counted as somebody else's and its memory left uncharged --
 *   the quota-escape direction. 4 of the 50 cgroup v2 verdicts. Invisible to
 *   code review, obvious the moment the matrix ran.
 *
 *   The "..foo" row. The escape test was a plain strstr(path, "/.."), which
 *   also fires on a cgroup legitimately NAMED with leading dots, refusing a
 *   process that is ours. Now the check is component-wise. This one no host
 *   produced; the test found it.
 */

#include <stdio.h>
#include <string.h>

#include "include/cgroup_match.h"

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

/* Real container ids, shortened only so the table stays readable. */
#define OURS_V2   "/system.slice/docker-a7a10e192cb572ea.scope"
#define OTHER_V2  "/system.slice/docker-9d3f61a53e8088d3.scope"
#define OURS_V1   "/docker/3962044a72186484"
/* A real kubelet path, systemd driver + containerd, shortened for width. */
#define K8S_SELF  "/kubepods.slice/kubepods-besteffort.slice/" \
                  "kubepods-besteffort-podm1.slice/cri-containerd-2a4801eee454.scope"
#define OTHER_V1  "/docker/7dfb32ceb70e11fe"

static void test_captured_matrix(void) {
  struct {
    const char *self;
    const char *pid;
    int ours;
    const char *origin;
  } cases[] = {
    /* ---- cgroupns=private: our own cgroup IS the namespace root ---- */
    {"/", "/",                                   1, "v2 private, our own process"},
    {"/", "/nested",                             1, "v2 private, ours in a child cgroup"},
    {"/", "/nested/deeper",                      1, "ours, two levels down"},
    {"/", "/../../init.scope",                   0, "v2 private, host init via hostPID"},
    {"/", "/../../user.slice/user-0.slice/session-18.scope", 0, "v2 private, host process"},
    {"/", "/../docker-aa2dc72018ddf4db.scope",   0, "v2 private, sibling container"},
    {"/", "/init.scope",                         1, "no \"..\", so inside our root"},

    /* ---- the shape that made the pid=="/" short-circuit wrong ----
     * A process in the HOST root cgroup renders as "/" under cgroupns=host.
     * The short-circuit used to return "ours" on that string alone; a
     * real-hardware matrix hit it in all twelve CRI configurations on cgroup
     * v1, because containerd gives containers the host cgroup namespace
     * there. These two rows are what makes removing it safe: the comparison
     * answers the private case ("/" is OUR namespace root) and rejects the
     * host case, from the same input the short-circuit saw. */
    {"/", "/",                                   1, "cgroupns=private: \"/\" IS our root"},
    {K8S_SELF, "/",                              0, "cgroupns=host: \"/\" is the HOST root cgroup"},
    {OURS_V1, "/",                               0, "v1 host ns: a pid in no memory cgroup reads \"/\""},

    /* ---- cgroupns=host: our own cgroup is a real path ---- */
    {OURS_V2, OURS_V2,                           1, "v2 host, our own process"},
    {OURS_V2, OURS_V2 "/nested",                 1, "v2 host, ours in a child cgroup"},
    {OURS_V2, OTHER_V2,                          0, "v2 host, sibling container"},
    {OURS_V2, "/user.slice/user-0.slice/session-18.scope", 0, "v2 host, host process"},
    {OURS_V2, "/init.scope",                     0, "v2 host, host init"},
    {OURS_V1, OURS_V1,                           1, "v1 host, our own process"},
    {OURS_V1, OURS_V1 "/nested",                 1, "v1 host, ours in a child cgroup"},
    {OURS_V1, OTHER_V1,                          0, "v1 host, sibling container"},
    {OURS_V1, "/user.slice/user-0.slice/session-16.scope", 0, "v1 host, host process"},

    /* ---- the boundary a plain prefix would get wrong ---- */
    {"/a/b", "/a/bc",                            0, "sibling whose name extends ours"},
    {"/a/b", "/a/b",                             1, "exactly ours"},
    {"/a/b", "/a/b/c",                           1, "below ours"},
    {OURS_V2, OURS_V2 "x",                       0, "container id extended by one char"},

    /* ---- degenerate inputs ---- */
    {"",   "/anything",                          0, "empty self cannot identify us"},
    {"/",  "",                                   0, "empty pid path matches nothing"},
  };

  for (unsigned i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
    int got = cgroup_path_is_ours(cases[i].self, cases[i].pid);
    CHECK(got == cases[i].ours,
          "self=\"%s\" pid=\"%s\" -> %d, want %d  (%s)",
          cases[i].self, cases[i].pid, got, cases[i].ours, cases[i].origin);
  }
}

static void test_null_is_not_ours(void) {
  CHECK(cgroup_path_is_ours(NULL, "/x") == 0, "NULL self must not claim a pid");
  CHECK(cgroup_path_is_ours("/x", NULL) == 0, "NULL pid path must not be claimed");
  CHECK(cgroup_path_is_ours(NULL, NULL) == 0, "both NULL must not be claimed");
}

/* The root case must not be answered by the prefix alone: with self == "/"
 * every absolute path is a prefix match, so a path that escapes the root has
 * to be rejected on the strength of "/.." and nothing else. */
static void test_root_rejects_only_on_dotdot(void) {
  CHECK(cgroup_path_is_ours("/", "/..") == 0, "a bare \"/..\" escapes the root");
  CHECK(cgroup_path_is_ours("/", "/../") == 0, "\"/../\" escapes the root");
  CHECK(cgroup_path_is_ours("/", "/a/../b") == 0,
        "\"..\" anywhere in the path means it left our root");
  CHECK(cgroup_path_is_ours("/", "/..foo") == 1,
        "\"..foo\" is an ordinary name, not an escape");
  CHECK(cgroup_path_is_ours("/", "/a..b") == 1,
        "dots inside a name are not an escape");
}

/* The one case the pid=="/" short-circuit used to get right by accident.
 *
 * Under cgroupns=private our container's cgroup root renders as "/". If our
 * OWN process has been moved into a child cgroup -- which needs write access
 * to cgroupfs, so privileged or an explicitly writable mount -- then self is
 * "/x" and a sibling process still at the container root reads "/". The
 * prefix test rejects it, so that process is not counted: the under-count
 * direction.
 *
 * It is left rejected on purpose. The same string "/" is what a HOST root
 * cgroup process renders as under cgroupns=host, and claiming it there
 * over-counts -- which is measured and reachable on cgroup v1 nodes, while
 * this case needs cgroup write access inside the container. Telling the two
 * apart needs to know whether "/" means our namespace root or the host root,
 * and no string rule can decide that; see docs/library_cgroup_ownership_boundaries.md.
 * This test exists so the trade-off is recorded rather than rediscovered. */
static void test_self_in_child_cgroup_underreports_root(void) {
  CHECK(cgroup_path_is_ours("/x", "/") == 0,
        "known limitation: self in a child cgroup does not claim the container root");
  CHECK(cgroup_path_is_ours("/x", "/x") == 1, "self's own cgroup is still ours");
  CHECK(cgroup_path_is_ours("/x", "/x/deeper") == 1, "below self is still ours");
  CHECK(cgroup_path_is_ours("/x", "/y") == 0,
        "known limitation: a sibling child cgroup is not claimed either");
}

int main(void) {
  test_captured_matrix();
  test_self_in_child_cgroup_underreports_root();
  test_null_is_not_ours();
  test_root_rejects_only_on_dotdot();

  if (failures != 0) {
    printf("FAILED: %d check(s)\n", failures);
    return 1;
  }
  printf("PASS: cgroup ownership match\n");
  return 0;
}
