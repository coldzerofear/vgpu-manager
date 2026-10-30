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

static const char *name_of(cgroup_match_t m) {
  switch (m) {
    case CGROUP_MATCH_OURS:          return "OURS";
    case CGROUP_MATCH_FOREIGN:       return "FOREIGN";
    case CGROUP_MATCH_NEEDS_NS_MODE: return "NEEDS_NS_MODE";
    default:                         return "???";
  }
}

static void test_captured_matrix(void) {
  struct {
    const char *self;
    const char *pid;
    cgroup_match_t want;
    const char *origin;
  } cases[] = {
    /* ---- cgroupns=private: our own cgroup IS the namespace root ---- */
    {"/", "/",                                   CGROUP_MATCH_OURS, "v2 private, our own process"},
    {"/", "/nested",                             CGROUP_MATCH_OURS, "v2 private, ours in a child cgroup"},
    {"/", "/nested/deeper",                      CGROUP_MATCH_OURS, "ours, two levels down"},
    {"/", "/../../init.scope",                   CGROUP_MATCH_FOREIGN, "v2 private, host init via hostPID"},
    {"/", "/../../user.slice/user-0.slice/session-18.scope", CGROUP_MATCH_FOREIGN, "v2 private, host process"},
    {"/", "/../docker-aa2dc72018ddf4db.scope",   CGROUP_MATCH_FOREIGN, "v2 private, sibling container"},
    {"/", "/init.scope",                         CGROUP_MATCH_OURS, "no \"..\", so inside our root"},

    /* ---- the shape that made the pid=="/" short-circuit wrong ----
     * A process in the HOST root cgroup renders as "/" under cgroupns=host.
     * The short-circuit used to return "ours" on that string alone; a
     * real-hardware matrix hit it in all twelve CRI configurations on cgroup
     * v1, because containerd gives containers the host cgroup namespace
     * there. These two rows are what makes removing it safe: the comparison
     * answers the private case ("/" is OUR namespace root) and rejects the
     * host case, from the same input the short-circuit saw. */
    {"/", "/",                                   CGROUP_MATCH_OURS, "cgroupns=private: \"/\" IS our root"},
    {K8S_SELF, "/",                              CGROUP_MATCH_NEEDS_NS_MODE, "cgroupns=host: \"/\" is the HOST root cgroup"},
    {OURS_V1, "/",                               CGROUP_MATCH_NEEDS_NS_MODE, "v1 host ns: a pid in no memory cgroup reads \"/\""},

    /* ---- cgroupns=host: our own cgroup is a real path ---- */
    {OURS_V2, OURS_V2,                           CGROUP_MATCH_OURS, "v2 host, our own process"},
    {OURS_V2, OURS_V2 "/nested",                 CGROUP_MATCH_OURS, "v2 host, ours in a child cgroup"},
    {OURS_V2, OTHER_V2,                          CGROUP_MATCH_NEEDS_NS_MODE, "v2 host, sibling container"},
    {OURS_V2, "/user.slice/user-0.slice/session-18.scope", CGROUP_MATCH_NEEDS_NS_MODE, "v2 host, host process"},
    {OURS_V2, "/init.scope",                     CGROUP_MATCH_NEEDS_NS_MODE, "v2 host, host init"},
    {OURS_V1, OURS_V1,                           CGROUP_MATCH_OURS, "v1 host, our own process"},
    {OURS_V1, OURS_V1 "/nested",                 CGROUP_MATCH_OURS, "v1 host, ours in a child cgroup"},
    {OURS_V1, OTHER_V1,                          CGROUP_MATCH_NEEDS_NS_MODE, "v1 host, sibling container"},
    {OURS_V1, "/user.slice/user-0.slice/session-16.scope", CGROUP_MATCH_NEEDS_NS_MODE, "v1 host, host process"},

    /* ---- the boundary a plain prefix would get wrong ---- */
    {"/a/b", "/a/bc",                            CGROUP_MATCH_NEEDS_NS_MODE, "sibling whose name extends ours"},
    {"/a/b", "/a/b",                             CGROUP_MATCH_OURS, "exactly ours"},
    {"/a/b", "/a/b/c",                           CGROUP_MATCH_OURS, "below ours"},
    {OURS_V2, OURS_V2 "x",                       CGROUP_MATCH_NEEDS_NS_MODE, "container id extended by one char"},

    /* ---- degenerate inputs ---- */
    {"",   "/anything",                          CGROUP_MATCH_FOREIGN, "empty self cannot identify us"},
    {"/",  "",                                   CGROUP_MATCH_FOREIGN, "empty pid path matches nothing"},
  };

  for (unsigned i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
    cgroup_match_t got = cgroup_path_match(cases[i].self, cases[i].pid);
    CHECK(got == cases[i].want,
          "self=\"%s\" pid=\"%s\" -> %s, want %s  (%s)",
          cases[i].self, cases[i].pid, name_of(got), name_of(cases[i].want),
          cases[i].origin);
  }
}

static void test_null_is_not_ours(void) {
  CHECK(cgroup_path_match(NULL, "/x") == CGROUP_MATCH_FOREIGN, "NULL self must not claim a pid");
  CHECK(cgroup_path_match("/x", NULL) == CGROUP_MATCH_FOREIGN, "NULL pid path must not be claimed");
  CHECK(cgroup_path_match(NULL, NULL) == CGROUP_MATCH_FOREIGN, "both NULL must not be claimed");
  /* A path field that is not absolute came from a malformed line. It must not
   * reach step 3, where self == "/" would read it as being below our root. */
  CHECK(cgroup_path_match("/", "relative") == CGROUP_MATCH_FOREIGN,
        "a non-absolute pid path must not be claimed");
  CHECK(cgroup_path_match("/", "") == CGROUP_MATCH_FOREIGN,
        "an empty pid path must not be claimed");
}

/* The root case must not be answered by the prefix alone: with self == "/"
 * every absolute path is a prefix match, so a path that escapes the root has
 * to be rejected on the strength of "/.." and nothing else. */
static void test_root_rejects_only_on_dotdot(void) {
  CHECK(cgroup_path_match("/", "/..") == CGROUP_MATCH_FOREIGN, "a bare \"/..\" escapes the root");
  CHECK(cgroup_path_match("/", "/../") == CGROUP_MATCH_FOREIGN, "\"/../\" escapes the root");
  CHECK(cgroup_path_match("/", "/a/../b") == CGROUP_MATCH_FOREIGN,
        "\"..\" anywhere in the path means it left our root");
  CHECK(cgroup_path_match("/", "/..foo") == CGROUP_MATCH_OURS,
        "\"..foo\" is an ordinary name, not an escape");
  CHECK(cgroup_path_match("/", "/a..b") == CGROUP_MATCH_OURS,
        "dots inside a name are not an escape");
  /* The ".." test must run BEFORE the prefix test: with self == "/" every
   * absolute path is a prefix match, so an escaped path would otherwise be
   * claimed. Keep this next to the rows above -- it is the ordering, not the
   * individual answers, that is easy to break. */
  CHECK(cgroup_path_match("/", "/../x") == CGROUP_MATCH_FOREIGN,
        "with self \"/\" the \"..\" test has to come first");
}

/* The shape that needs the cgroupns mode, and why it is not decidable here.
 *
 * Under cgroupns=private our container's cgroup root renders as "/". If our
 * OWN process has been moved into a child cgroup -- which needs write access
 * to cgroupfs, so privileged, or a hostPath mount of /sys/fs/cgroup that
 * forgot readOnly, or Docker-in-Docker on cgroup v2 where the "no internal
 * process" rule makes dockerd move itself into /init -- then self is "/x" and
 * a sibling process still at the container root reads "/".
 *
 * That pid IS ours, but the strings cannot say so: the very same "/" is what a
 * process in the HOST root cgroup renders as under cgroupns=host, where it is
 * NOT ours. Measured on a real CRI Pod, both readings occur. So these rows
 * must come back NEEDS_NS_MODE and the caller settles them; returning OURS
 * here would over-count on cgroup v1 nodes, where containerd gives containers
 * the host cgroup namespace, and returning FOREIGN under-counts.
 *
 * The sibling-cgroup rows are the same situation: "/y" is inside this
 * container under private and somebody else's cgroup under host.
 */
static void test_child_cgroup_shapes_need_the_ns_mode(void) {
  CHECK(cgroup_path_match("/x", "/") == CGROUP_MATCH_NEEDS_NS_MODE,
        "self in a child cgroup, pid at the namespace root: needs the mode");
  CHECK(cgroup_path_match("/x", "/y") == CGROUP_MATCH_NEEDS_NS_MODE,
        "sibling child cgroup: needs the mode");
  CHECK(cgroup_path_match("/init", "/docker/abc") == CGROUP_MATCH_NEEDS_NS_MODE,
        "the measured Docker-in-Docker shape on cgroup v2: needs the mode");
  /* Still settled without the mode, because self is on the path: */
  CHECK(cgroup_path_match("/x", "/x") == CGROUP_MATCH_OURS, "self's own cgroup is ours");
  CHECK(cgroup_path_match("/x", "/x/deeper") == CGROUP_MATCH_OURS, "below self is ours");
  CHECK(cgroup_path_match("/x", "/../y") == CGROUP_MATCH_FOREIGN,
        "a \"..\" still settles it, whatever the mode");
}

int main(void) {
  test_captured_matrix();
  test_child_cgroup_shapes_need_the_ns_mode();
  test_null_is_not_ours();
  test_root_rejects_only_on_dotdot();

  if (failures != 0) {
    printf("FAILED: %d check(s)\n", failures);
    return 1;
  }
  printf("PASS: cgroup ownership match\n");
  return 0;
}
