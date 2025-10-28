#include <errno.h>
#include <string.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#include "include/hook.h"

#define RPC_CLIENT_FILE_NAME "device-client"
#define RPC_CLIENT_FILE_PATH (VGPU_MANAGER_PATH "/registry/" RPC_CLIENT_FILE_NAME)
#define RPC_SOCKET_FILE_NAME "socket.sock"
#define RPC_ADDRESS (VGPU_MANAGER_PATH "/registry/" RPC_SOCKET_FILE_NAME)

void register_to_remote_with_data(const char* pod_uid, const char* container) {
  int ret = -1, wstatus = 0, wret = 0;
  pid_t child_pid = fork();
  if (child_pid == 0) {
    ret = execl(RPC_CLIENT_FILE_PATH, RPC_CLIENT_FILE_NAME, "--address", RPC_ADDRESS,
                "--pod-uid", pod_uid, "--container-name", container, (char*)NULL);
    if (unlikely(ret == -1)) {
      LOGGER(FATAL, "can't register to manager, error %s", strerror(errno));
    }
    _exit(EXIT_SUCCESS);
  } else if (child_pid < 0) {
    LOGGER(FATAL, "fork failed: %s", strerror(errno));
  } else {
    do {
      wret = waitpid(child_pid, &wstatus, WUNTRACED | WCONTINUED);
      if (unlikely(wret == -1)) {
        LOGGER(FATAL, "waitpid failed, error %s", strerror(errno));
      }
    } while (!WIFEXITED(wstatus) && !WIFSIGNALED(wstatus));
    ret = WEXITSTATUS(wstatus);
    if (unlikely(ret)) {
      LOGGER(FATAL, "rpc client exited with %d", ret);
    }
  }
}
