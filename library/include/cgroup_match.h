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
 * Whether pid_cgroup is our cgroup or sits below it. Both arguments are the
 * path field of a /proc/<pid>/cgroup line, as read_cgroup() returns it.
 *
 * Prefix, not an exact compare: a process of this container may live in a
 * cgroup below ours -- systemd inside the container, a nested runtime, a
 * framework that makes its own cgroup -- and an exact compare would call it
 * someone else's. Nor strstr(), which the cgroup v1 path used to do: that
 * matches our path appearing anywhere inside a longer one.
 *
 * Two shapes, because what "below ours" means depends on where our own cgroup
 * namespace root is. Every string below was captured on a real host (Ubuntu
 * 24.04, kernel 7.0, Docker 29.1) across cgroup v1 and v2 and cgroupns
 * private and host:
 *
 *   WE ARE AT THE NAMESPACE ROOT -- cgroupns=private, self == "/"
 *     Every absolute path starts with "/", so the prefix alone says nothing.
 *     The real question is whether the pid escaped our root, and the kernel
 *     answers it by spelling the path with "..":
 *         ours, at the root:  "/"
 *         ours, nested:       "/nested"
 *         host process:       "/../../init.scope"
 *         sibling container:  "/../docker-<other-id>.scope"
 *     So the test is the absence of "/..", not a boundary check. Requiring a
 *     boundary here is what made "/nested" look foreign, and it under-counted
 *     every process the container placed in a child cgroup -- measured, not
 *     theorised: it was 4 of 50 cases in the cgroup v2 matrix.
 *
 *   WE ARE BELOW THE ROOT -- cgroupns=host, self == "/system.slice/..."
 *         ours:               "/system.slice/docker-<id>.scope"
 *         ours, nested:       "/system.slice/docker-<id>.scope/nested"
 *         host process:       "/user.slice/user-0.slice/session-18.scope"
 *         sibling container:  "/system.slice/docker-<other-id>.scope"
 *         cgroup v1 form:     "/docker/<id>"
 *     A plain prefix would let "/a/bc" pass as being under "/a/b", so the
 *     character after the prefix has to be a separator or the end of string.
 *
 * Returns 1 when the pid is ours, 0 otherwise. A NULL or empty self path
 * returns 0: with nothing to compare against, claiming the pid would be a
 * guess, and over-counting is not the safer guess here -- the caller has a
 * second cgroup key to try.
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

static inline int cgroup_path_is_ours(const char *self_cgroup, const char *pid_cgroup) {
  size_t self_len;

  if (self_cgroup == NULL || pid_cgroup == NULL) {
    return 0;
  }
  self_len = strlen(self_cgroup);
  if (self_len == 0) {
    return 0;
  }
  if (strncmp(pid_cgroup, self_cgroup, self_len) != 0) {
    return 0;
  }
  if (self_len == 1 && self_cgroup[0] == '/') {
    return !cgroup_path_escapes_root(pid_cgroup);
  }
  return pid_cgroup[self_len] == '\0' || pid_cgroup[self_len] == '/';
}

#endif /* _VGPU_CGROUP_MATCH_H_ */
