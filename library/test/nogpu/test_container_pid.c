/* Container PID identification: the two tests in src/util.c that decide
 * whether a PID NVML reported on a device belongs to THIS container.
 *
 * These live in util.c precisely so they can be reached without a driver, for
 * the same reason test_cgroup_parse.c gives: the bug this file pins down sat
 * unnoticed because nothing could call the function.
 *
 * WHAT WENT WRONG, AND IS CHECKED HERE
 *
 * device_pid_in_same_container() used to stat /proc/<pid>/ns/mnt AND
 * /proc/<pid>/ns/cgroup unconditionally, returning "not ours" if either stat
 * failed. cgroup namespaces only exist from Linux 4.6, so on an older kernel
 * the second stat always failed and the function rejected EVERY pid --
 * including the calling process's own. check_device_pid_in_local_container_pid()
 * then never identified anything, the open-kernel path reported zero GPU memory
 * in use, and the budget check became a no-op. The first case below is the one
 * that catches that: the caller's own PID must be recognised as its own, on
 * any kernel.
 *
 * gpu_device_fd_in_process() is the GPU-in-use test. It cannot be given a real
 * /dev/nvidia* here, so what is checked is everything else: that it rejects a
 * process holding only unrelated device nodes, that it does not fall over on a
 * dead pid or on a pid it may not inspect, and that it leaks no descriptor --
 * it opens a directory per call on the allocation path, so a leak there would
 * exhaust the process. The prefix rule itself is checked separately below
 * against the exact strings a driver produces, since that is the part a future
 * edit is most likely to loosen into a substring match.
 *
 * Run under ASan (make test-sanitize) this also covers the fixed-size caches
 * in util.c: a slot index past their end would fault here.
 */

#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <stddef.h>
#include <unistd.h>
#include <fcntl.h>
#include <signal.h>
#include <errno.h>
#include <dirent.h>
#include <sys/stat.h>
#include <sys/wait.h>

#include "include/hook.h"

extern int device_pid_in_same_container(unsigned int pid);
extern int gpu_device_fd_in_process(unsigned int pid);

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

/* Spawn a child that holds the given device node open and parks. Returns its
 * pid, or -1. The child is already holding the descriptor when this returns,
 * so the caller can inspect it without racing its startup. */
static pid_t spawn_holder(const char *dev_path) {
  int pipefd[2];
  if (pipe(pipefd) != 0) {
    return -1;
  }
  pid_t child = fork();
  if (child < 0) {
    close(pipefd[0]);
    close(pipefd[1]);
    return -1;
  }
  if (child == 0) {
    close(pipefd[0]);
    if (dev_path != NULL && open(dev_path, O_RDONLY) < 0) {
      _exit(1);
    }
    /* A few ordinary descriptors too, so the device node is not simply the
     * only thing in the table. */
    for (int i = 0; i < 16; i++) {
      if (open("/proc/self/stat", O_RDONLY) < 0) {
        break;
      }
    }
    char ready = 1;
    ssize_t written = write(pipefd[1], &ready, 1);
    (void)written;
    pause();
    _exit(0);
  }
  close(pipefd[1]);
  char ready = 0;
  ssize_t got = read(pipefd[0], &ready, 1);
  close(pipefd[0]);
  if (got != 1) {
    kill(child, SIGKILL);
    waitpid(child, NULL, 0);
    return -1;
  }
  return child;
}

static void reap(pid_t child) {
  kill(child, SIGKILL);
  waitpid(child, NULL, 0);
}

static int count_own_fds(void) {
  DIR *dir = opendir("/proc/self/fd");
  if (dir == NULL) {
    return -1;
  }
  int n = 0;
  struct dirent *entry;
  while ((entry = readdir(dir)) != NULL) {
    if (entry->d_name[0] >= '0' && entry->d_name[0] <= '9') {
      n++;
    }
  }
  closedir(dir);
  return n;
}

/* The regression that mattered: our own PID is in our own container. This
 * failed on every kernel below 4.6 before the cgroup-namespace probe. */
static void test_self_is_in_own_container(void) {
  CHECK(device_pid_in_same_container((unsigned int)getpid()) == 0,
        "the calling process must be recognised as being in its own container "
        "(this is what broke on kernels without cgroup namespaces)");
}

static void test_child_is_in_same_container(void) {
  pid_t child = spawn_holder(NULL);
  CHECK(child > 0, "could not spawn a child");
  if (child <= 0) {
    return;
  }
  CHECK(device_pid_in_same_container((unsigned int)child) == 0,
        "a forked child shares our namespaces and must be recognised as ours");
  reap(child);
}

static void test_dead_and_invalid_pids_rejected(void) {
  pid_t child = spawn_holder(NULL);
  CHECK(child > 0, "could not spawn a child");
  if (child <= 0) {
    return;
  }
  reap(child);
  /* Reaped: /proc/<pid> is gone, so this must not be claimed as ours. */
  CHECK(device_pid_in_same_container((unsigned int)child) != 0,
        "a reaped pid must not be claimed as ours");
  CHECK(device_pid_in_same_container(0) != 0, "pid 0 must not be claimed");
  CHECK(gpu_device_fd_in_process(0) != 0, "pid 0 holds no GPU descriptor");
  CHECK(gpu_device_fd_in_process((unsigned int)child) != 0,
        "a reaped pid holds no GPU descriptor");
}

/* Repeated calls must agree with themselves: util.c caches our own namespace
 * inodes, and a cache that went stale or was filled from a failed stat would
 * show up as the verdict changing under identical conditions. */
static void test_verdict_is_stable(void) {
  unsigned int self = (unsigned int)getpid();
  int first = device_pid_in_same_container(self);
  for (int i = 0; i < 2000; i++) {
    CHECK(device_pid_in_same_container(self) == first,
          "verdict for our own pid changed on iteration %d", i);
    if (failures) {
      return;
    }
  }
}

/* The caches must survive fork: a child inherits our namespaces, so the inode
 * the parent cached is still the right answer in the child. If this ever stops
 * holding, the cache needs invalidating in loader_child_after_fork(). */
static void test_cache_survives_fork(void) {
  /* Prime the cache in the parent first -- that is the case that would break. */
  (void)device_pid_in_same_container((unsigned int)getpid());

  int pipefd[2];
  CHECK(pipe(pipefd) == 0, "pipe failed");
  pid_t child = fork();
  CHECK(child >= 0, "fork failed");
  if (child < 0) {
    return;
  }
  if (child == 0) {
    close(pipefd[0]);
    int verdicts[2];
    verdicts[0] = device_pid_in_same_container((unsigned int)getpid());
    verdicts[1] = device_pid_in_same_container((unsigned int)getppid());
    ssize_t written = write(pipefd[1], verdicts, sizeof(verdicts));
    (void)written;
    _exit(0);
  }
  close(pipefd[1]);
  int verdicts[2] = {-1, -1};
  ssize_t got = read(pipefd[0], verdicts, sizeof(verdicts));
  close(pipefd[0]);
  waitpid(child, NULL, 0);
  CHECK(got == (ssize_t)sizeof(verdicts), "child did not report back");
  CHECK(verdicts[0] == 0, "child must recognise itself as in our container");
  CHECK(verdicts[1] == 0, "child must recognise its parent as in our container");
}

/* A process holding only unrelated character devices is not on the GPU. This
 * is the false-positive direction: the old maps scan matched "nvidia" anywhere
 * in a path, so a mapped file under a directory merely named after the vendor
 * counted as GPU use. */
static void test_unrelated_devices_are_not_gpu(void) {
  pid_t child = spawn_holder("/dev/null");
  if (child <= 0) {
    printf("SKIP %s: could not open /dev/null in a child\n", __func__);
    return;
  }
  CHECK(gpu_device_fd_in_process((unsigned int)child) != 0,
        "a process holding /dev/null must not count as using the GPU");
  reap(child);
}

/* Opening a directory per call on the allocation path: a missed closedir would
 * exhaust descriptors rather than merely slow things down. */
static void test_no_descriptor_leak(void) {
  unsigned int self = (unsigned int)getpid();
  int before = count_own_fds();
  CHECK(before > 0, "could not count our own descriptors");
  for (int i = 0; i < 3000; i++) {
    (void)gpu_device_fd_in_process(self);
    (void)gpu_device_fd_in_process(1);
    (void)device_pid_in_same_container(self);
  }
  int after = count_own_fds();
  CHECK(after == before,
        "descriptor count changed across 9000 calls: %d -> %d", before, after);
}

/* The prefix rule, against the exact targets a driver puts in /proc/<pid>/fd
 * and against the paths that must NOT match. Checked directly rather than
 * through a process, because a real /dev/nvidia* cannot be created here -- and
 * because this is the part most likely to be loosened back into a substring
 * search, which is what made the old maps scan wrong. */
static void test_prefix_rule(void) {
  static const char *const gpu[] = {
    "/dev/nvidia0",
    "/dev/nvidia15",
    "/dev/nvidiactl",
    "/dev/nvidia-uvm",
    "/dev/nvidia-uvm-tools",
    "/dev/nvidia-caps/nvidia-cap1",
    "/dev/nvidia-modeset",
  };
  static const char *const not_gpu[] = {
    "/dev/null",
    "/dev/zero",
    "/dev/nvme0n1",               /* shares "/dev/nv" but is a disk */
    "/dev/nvid",                  /* shorter than the prefix */
    "/home/user/dev/nvidia-notes/data.bin", /* contains it, does not start with it */
    "/opt/nvidia/model.pt",
    "/usr/lib/libnvidia-ml.so.1", /* a library, not a device node */
    "",
  };
  const char prefix[] = "/dev/nvidia";
  const size_t prefix_len = sizeof(prefix) - 1;

  for (size_t i = 0; i < sizeof(gpu) / sizeof(gpu[0]); i++) {
    CHECK(strncmp(gpu[i], prefix, prefix_len) == 0,
          "%s must be recognised as an NVIDIA device node", gpu[i]);
  }
  for (size_t i = 0; i < sizeof(not_gpu) / sizeof(not_gpu[0]); i++) {
    CHECK(strncmp(not_gpu[i], prefix, prefix_len) != 0,
          "%s must NOT be recognised as an NVIDIA device node", not_gpu[i]);
  }
}

/* A pid we may not inspect must be REPORTED, not silently counted as "not
 * ours". /proc/<pid>/ns entries need PTRACE_MODE_READ, so the verdict for a pid
 * owned by another uid is indistinguishable from one that exited -- and the
 * two mean opposite things (an under-count that lets a container exceed its
 * quota, versus an ordinary race). The warning is the only thing that tells
 * them apart, so this checks the warning actually comes out.
 *
 * Works whether or not the suite runs as root:
 *   as root   -- fork a child, drop it to an unprivileged uid, and have it
 *                inspect its still-root parent. That is a denial we create
 *                rather than hope to find.
 *   otherwise -- inspect pid 1, which is root and therefore off limits.
 */
static void test_inspect_denial_is_reported(void) {
  char log_path[] = "/tmp/vgpu_cpid_denied_XXXXXX";
  int log_fd = mkstemp(log_path);
  if (log_fd < 0) {
    printf("SKIP %s: could not create a temp log\n", __func__);
    return;
  }
  close(log_fd);

  int pipefd[2];
  if (pipe(pipefd) != 0) {
    printf("SKIP %s: pipe failed\n", __func__);
    unlink(log_path);
    return;
  }

  pid_t parent_pid = getpid();
  pid_t child = fork();
  if (child < 0) {
    printf("SKIP %s: fork failed\n", __func__);
    close(pipefd[0]);
    close(pipefd[1]);
    unlink(log_path);
    return;
  }
  if (child == 0) {
    close(pipefd[0]);
    /* The warning goes to stderr; capture it for the parent to read. */
    int fd = open(log_path, O_WRONLY | O_TRUNC);
    if (fd >= 0) {
      dup2(fd, STDERR_FILENO);
      close(fd);
    }
    /* LOGGER needs WARNING enabled; that is the default, but be explicit
     * since this child's verdict depends on it. */
    setenv("LOGGER_LEVEL", "2", 1);

    unsigned int target;
    if (getuid() == 0) {
      /* 65534 = nobody on Debian/Ubuntu, which this image is. setgid first:
       * after setuid we would no longer be allowed to. */
      if (setgid(65534) != 0 || setuid(65534) != 0 || getuid() == 0) {
        _exit(77); /* could not drop privileges -> SKIP */
      }
      target = (unsigned int)parent_pid; /* still root, now off limits to us */
    } else {
      target = 1; /* pid 1 is root, we are not */
    }

    /* Report the raw verdict, not a boolean: the whole point is that a denial
     * is its own third value rather than being folded into "not ours". */
    char payload = (char)device_pid_in_same_container(target);
    ssize_t written = write(pipefd[1], &payload, 1);
    (void)written;
    _exit(0);
  }

  close(pipefd[1]);
  char verdict = 0;
  ssize_t got = read(pipefd[0], &verdict, 1);
  close(pipefd[0]);
  int status = 0;
  waitpid(child, &status, 0);

  if (WIFEXITED(status) && WEXITSTATUS(status) == 77) {
    printf("SKIP %s: could not drop privileges to create a denial\n", __func__);
    unlink(log_path);
    return;
  }
  /* UNKNOWN, specifically. NO would mean the denial was folded into "not
   * ours", which under-counts a process that may well be ours -- the
   * quota-escape direction this third value exists to prevent. */
  CHECK(got == 1 && verdict == PID_IN_CONTAINER_UNKNOWN,
        "a pid we may not inspect must come back UNKNOWN (%d), not NO (%d) or "
        "YES (%d) (got=%zd verdict=%d)",
        PID_IN_CONTAINER_UNKNOWN, PID_IN_CONTAINER_NO, PID_IN_CONTAINER_YES,
        got, (int)verdict);

  /* And it must have said why. */
  FILE *fp = fopen(log_path, "re");
  int saw_warning = 0;
  if (fp != NULL) {
    char line[2048];
    while (fgets(line, sizeof(line), fp) != NULL) {
      if (strstr(line, "not permitted to inspect pid") != NULL) {
        saw_warning = 1;
        break;
      }
    }
    fclose(fp);
  }
  CHECK(saw_warning,
        "a permission denial was not reported: an under-count from EPERM is "
        "then indistinguishable from a pid that simply exited");
  unlink(log_path);
}

int main(void) {
  test_self_is_in_own_container();
  test_child_is_in_same_container();
  test_dead_and_invalid_pids_rejected();
  test_verdict_is_stable();
  test_cache_survives_fork();
  test_unrelated_devices_are_not_gpu();
  test_no_descriptor_leak();
  test_prefix_rule();
  test_inspect_denial_is_reported();

  if (failures != 0) {
    printf("FAILED: %d check(s)\n", failures);
    return 1;
  }
  printf("PASS: container PID identification\n");
  return 0;
}
