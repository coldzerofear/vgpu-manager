/*
Copyright 2026 coldzerofear

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
 * "Is this pid's cgroup ours?" as a pure function.
 *
 * Header-inline for the same reason as sm_delta.h, array_size.h and
 * vmem_table.h: the decision is reachable from the no-GPU tests, so CI
 * exercises the shipped code rather than a replica. Its caller --
 * check_device_pid_in_proc_cgroup_path() in cuda_hook.c -- cannot be reached
 * from there, because cuda_hook.c needs the driver.
 *
 * This runs when the namespaces could not be inspected (another uid owns the
 * pid, so /proc/<pid>/ns, /proc/<pid>/fd and /proc/<pid>/maps are all refused
 * by PTRACE_MODE_READ) and /proc/<pid>/cgroup is the only thing left that can
 * settle ownership. Getting it wrong goes two ways and neither is cosmetic:
 * claiming a pid that is not ours over-counts and throttles the container
 * against memory it does not hold; refusing one that IS ours under-counts and
 * lets the container hold memory nobody charged it for.
 */

#ifndef _VGPU_CGROUP_MATCH_H_
#define _VGPU_CGROUP_MATCH_H_

#include <string.h>

/*
 * The shapes this has to tell apart. Every string below was captured on a real
 * host (Ubuntu 24.04, kernel 7.0, containerd 2.2.1 CRI + Docker 29.1) across
 * cgroup v2, systemd hybrid v1 and legacy v1, with cgroupns private and host:
 *
 *   cgroupns=private -- self == "/", we are AT the namespace root
 *       ours, at the root:  "/"
 *       ours, nested:       "/nested"
 *       host process:       "/../../init.scope"
 *       sibling container:  "/../docker-<other-id>.scope"
 *       sibling in a Pod:   "/../cri-containerd-<other-id>.scope"
 *       host root cgroup:   "/../.."
 *
 *   cgroupns=host -- self is a real path, we are BELOW the root
 *       ours:               "/kubepods.slice/.../cri-containerd-<id>.scope"
 *       ours, nested:       "/kubepods.slice/.../cri-containerd-<id>.scope/x"
 *       sibling container:  "/kubepods.slice/.../cri-containerd-<other>.scope"
 *       host process:       "/user.slice/user-0.slice/session-18.scope"
 *       host root cgroup:   "/"
 *       cgroup v1 form:     "/docker/<id>"
 *
 * Note the last line of each block: a process in the host's root cgroup and
 * our own container's root render as the same two characters, "/", in the two
 * modes. That is the whole reason this function is three-valued.
 */
/*
 * Whether any component of the path is exactly "..", which is how the kernel
 * says the cgroup sits outside the reader's namespace root.
 *
 * Component-wise, not a plain strstr(pid, "/.."): a cgroup directory may
 * legitimately be NAMED with leading dots -- "..foo" is a valid filename --
 * and a substring search would read "/..foo" as an escape and refuse a
 * process that is ours. Only a component that IS ".." counts, so the
 * character after it has to end the path or start the next component.
 */
static inline int cgroup_path_escapes_root(const char *path) {
  const char *p = path;

  while ((p = strstr(p, "/..")) != NULL) {
    char after = p[3];
    if (after == '\0' || after == '/') {
      return 1;
    }
    p += 3;
  }
  return 0;
}

/*
 * Three-valued on purpose: some shapes cannot be settled by the strings alone.
 *
 * The anchor this compares against is our OWN cgroup, and that is only the
 * container's boundary while nothing has moved us: if our process sits in a
 * CHILD cgroup of the container's, the subtree rooted at self is too small and
 * a sibling process still at the container root is not under it. Measured on a
 * real CRI pod (containerd + kubelet-style cgroup_parent): self "/x", a
 * same-container process "/" -> rejected, which under-counts.
 *
 * The string "/" is exactly where the two readings collide:
 *     cgroupns=private   "/" is OUR container's cgroup root      -> ours
 *     cgroupns=host      "/" is the HOST root cgroup             -> not ours
 * Same two characters, opposite answers, and nothing in the path length,
 * depth or ancestry carries the mode. So the caller -- which may read files --
 * resolves it; this function stays pure, which is what lets the no-GPU tests
 * exercise the shipped decision rather than a copy of it.
 *
 * The ".." test runs BEFORE the prefix test, and that ordering is what keeps
 * the ambiguous branch rare: a path with a ".." component is outside our
 * namespace root, and cgroupns=host never produces one (no boundary to
 * cross -- 930 real-hardware records, no exception). So under
 * cgroupns=private, which is what containerd gives containers on cgroup v2,
 * our own processes answer at step 1 and everything foreign answers at step 2
 * -- the mode is never asked for.
 */
typedef enum {
  CGROUP_MATCH_FOREIGN       = 0, /* settled: not this container's */
  CGROUP_MATCH_OURS          = 1, /* settled: this container's */
  CGROUP_MATCH_NEEDS_NS_MODE = 2, /* undecidable from the strings; needs the cgroupns mode */
} cgroup_match_t;

static inline cgroup_match_t cgroup_path_match(const char *self_cgroup, const char *pid_cgroup) {
  size_t self_len;

  if (self_cgroup == NULL || pid_cgroup == NULL || self_cgroup[0] == '\0') {
    /* Nothing to compare against. Claiming the pid would be a guess, and
     * over-counting is not the safer guess: it throttles the container
     * against memory it does not hold. */
    return CGROUP_MATCH_FOREIGN;
  }
  if (pid_cgroup[0] != '/') {
    /* Every cgroup path the kernel renders is absolute. Anything else came
     * from a malformed line, and step 3 below would read an empty path as
     * "below our namespace root" when self is "/" -- claiming a pid on the
     * strength of a parse failure. */
    return CGROUP_MATCH_FOREIGN;
  }
  /* 1. The only shape a standard Kubernetes container ever produces: every
   *    process of a container shares one cgroup, so self and the pid read the
   *    same string. Measured 6.8 ns, cheaper than the comparison below. */
  if (strcmp(self_cgroup, pid_cgroup) == 0) {
    return CGROUP_MATCH_OURS;
  }
  /* 2. Outside our namespace root. Must precede step 3: with self == "/"
   *    every absolute path is a prefix match, including "/../x". */
  if (cgroup_path_escapes_root(pid_cgroup)) {
    return CGROUP_MATCH_FOREIGN;
  }
  /* 3. At or below our own cgroup.
   *
   *    Prefix, not an exact compare: a process of this container may live in a
   *    cgroup below ours -- systemd inside the container, a nested runtime, a
   *    framework that makes its own cgroup. Nor strstr(), which the cgroup v1
   *    path used to do: that matches our path appearing anywhere inside a
   *    longer one. The character after the prefix has to be a separator or the
   *    end of string, or "/a/bc" would pass as being under "/a/b".
   *
   *    self == "/" means we are at the namespace root, so every path that
   *    survived step 2 is below us. */
  self_len = strlen(self_cgroup);
  if (self_len == 1 && self_cgroup[0] == '/') {
    return CGROUP_MATCH_OURS;
  }
  if (strncmp(pid_cgroup, self_cgroup, self_len) == 0 &&
      (pid_cgroup[self_len] == '\0' || pid_cgroup[self_len] == '/')) {
    return CGROUP_MATCH_OURS;
  }
  /* 4. An ancestor of our cgroup, or a sibling of it, with no ".." to say it
   *    left our namespace root. Under cgroupns=private that is still inside
   *    this container; under cgroupns=host it is not. */
  return CGROUP_MATCH_NEEDS_NS_MODE;
}

#endif /* _VGPU_CGROUP_MATCH_H_ */
