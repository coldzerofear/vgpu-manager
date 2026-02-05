#include "include/hook.h"

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <dirent.h>
#include <string.h>
#include <unistd.h>
#include <limits.h>
#include <errno.h>
#include <sys/stat.h>
#include <signal.h>

#define MAX_PID_STR_LEN 32
#define CUDA_MEMORY_LIMIT_ENV "CUDA_MEM_LIMIT"
#define CUDA_MEMORY_RATIO_ENV "CUDA_MEM_RATIO"
#define CUDA_CORE_LIMIT_ENV "CUDA_CORE_LIMIT"
#define CUDA_CORE_SOFT_LIMIT_ENV "CUDA_CORE_SOFT_LIMIT"
#define CUDA_MEM_OVERSOLD_ENV "CUDA_MEM_OVERSOLD"
#define VMEM_NODE_ENABLED_ENV "VMEMORY_NODE_ENABLED"
#define MANAGER_VISIBLE_DEVICE_ENV "MANAGER_VISIBLE_DEVICE"
#define MANAGER_VISIBLE_DEVICES_ENV (MANAGER_VISIBLE_DEVICE_ENV "S")
//#define NVIDIA_VISIBLE_DEVICES_ENV "NVIDIA_VISIBLE_DEVICES"
#define MANAGER_COMPATIBILITY_MODE_ENV "MANAGER_COMPATIBILITY_MODE"

size_t iec_to_bytes(const char *iec_value) {
  char *endptr = NULL;
  double value = 0.0;

  value = strtod(iec_value, &endptr);
  switch (*endptr) {
  case 'K':
  case 'k':
    value *= 1024UL;
    break;
  case 'M':
  case 'm':
    value *= 1024UL * 1024UL;
    break;
  case 'G':
  case 'g':
    value *= 1024UL * 1024UL * 1024UL;
    break;
  case 'T':
  case 't':
    value *= 1024UL * 1024UL * 1024UL * 1024UL;
    break;
  default:
    break;
  }
  return (size_t)value;
}

int get_compatibility_mode(int *mode) {
  char *str = getenv(MANAGER_COMPATIBILITY_MODE_ENV);
  if (!str || str[0] == '\0') {
    return -1;
  }
  *mode = iec_to_bytes(str);
  return 0;
}

int get_mem_ratio(uint32_t index, double *ratio) {
  char env[32] = {0};
  sprintf(env, "%s_%d", CUDA_MEMORY_RATIO_ENV, index);
  char *str = getenv(env);
  if (!str) {
    str = getenv(CUDA_MEMORY_RATIO_ENV);
    if (!str) {
      return -1;
    }
  }
  if (str[0] == '\0') {
    return -1;
  }
  *ratio = atof(str);
  return 0;
}

int get_mem_limit(uint32_t index, size_t *limit) {
  char env[32] = {0};
  sprintf(env, "%s_%d", CUDA_MEMORY_LIMIT_ENV, index);
  char *str = getenv(env);
  if (!str) {
    str = getenv(CUDA_MEMORY_LIMIT_ENV);
    if (!str) {
      return -1;
    }
  }
  if (str[0] == '\0') {
    return -1;
  }
  *limit = iec_to_bytes(str);
  return 0;
}

int get_core_limit(uint32_t index, int *limit) {
  char env[32] = {0};
  sprintf(env, "%s_%d", CUDA_CORE_LIMIT_ENV, index);
  char *str = getenv(env);
  if (!str) {
    str = getenv(CUDA_CORE_LIMIT_ENV);
    if (!str) {
      return -1;
    }
  }
  if (str[0] == '\0') {
    return -1;
  }
  *limit = iec_to_bytes(str);
  return 0;
}

int get_core_soft_limit(uint32_t index, int *limit) {
  char env[32] = {0};
  sprintf(env, "%s_%d", CUDA_CORE_SOFT_LIMIT_ENV, index);
  char *str = getenv(env);
  if (!str) {
    str = getenv(CUDA_CORE_SOFT_LIMIT_ENV);
    if (!str) {
      return -1;
    }
  }
  if (str[0] == '\0') {
    return -1;
  }
  *limit = iec_to_bytes(str);
  return 0;
}

int get_device_uuid(uint32_t index, char *uuid) {
  char env[32] = {0};
  sprintf(env, "%s_%d", MANAGER_VISIBLE_DEVICE_ENV, index);
  char *str = getenv(env);
  if (!str || str[0] == '\0') {
    return -1;
  }
  strcpy(uuid, str);
  return 0;
}

int get_device_uuids(char *uuids) {
  char *str = NULL;
  str = getenv(MANAGER_VISIBLE_DEVICES_ENV);
  if (!str || str[0] == '\0') {
    return -1;
  }
  strcpy(uuids, str);
  return 0;
}

int get_vmem_node_enabled(int *i) {
  char *str = NULL;
  str = getenv(VMEM_NODE_ENABLED_ENV);
  if (!str) {
    return -1;
  }
  if (strcmp(str, "true") == 0 ||
      strcmp(str, "TRUE") == 0 ||
      strcmp(str,"1") == 0) {
    *i = 1;
  } else {
    *i = 0;
  }
  return 0;
}

int get_mem_oversold(uint32_t index, int *i) {
  char env[32] = {0};
  sprintf(env, "%s_%d", CUDA_MEM_OVERSOLD_ENV, index);
  char *str = getenv(env);
  if (!str) {
    str = getenv(CUDA_MEM_OVERSOLD_ENV);
    if (!str) {
      return -1;
    }
  }
  if (strcmp(str, "true") == 0 ||
      strcmp(str, "TRUE") == 0 ||
      strcmp(str,"1") == 0) {
    *i = 1;
  } else {
    *i = 0;
  }
  return 0;
}

static int compare_pids(const void *a, const void *b) {
  int pid1 = *(const int *)a;
  int pid2 = *(const int *)b;
  return (pid1 > pid2) - (pid1 < pid2);
}

int get_container_pids_by_filepath(char *file_path, int *pids, int *pids_size) {
  if (!file_path || !pids || !pids_size) {
    LOGGER(ERROR, "invalid NULL parameter");
    *pids_size = 0;
    return -1;
  }

  if (access(file_path, F_OK) != 0) {
    *pids_size = 0;
    return -1;
  }

  FILE *fp = fopen(file_path, "r");
  if (!fp) {
    LOGGER(WARNING, "error opening %s: %s", file_path, strerror(errno));
    *pids_size = 0;
    return -1;
  }

  int max_size = *pids_size;
  int actual_count = 0;
  char line[MAX_PID_STR_LEN];

  while (fgets(line, sizeof(line), fp) && actual_count < max_size) {
    char *endptr;
    long pid = strtol(line, &endptr, 10);

    if (endptr == line || (*endptr != '\n' && *endptr != '\0')) {
      LOGGER(ERROR, "invalid PID format: %s", line);
      continue;
    }
    if (pid <= 0 || pid > INT_MAX) {
      continue;
    }
    pids[actual_count++] = (int)pid;
  }

  if (actual_count > 0) {
    qsort(pids, actual_count, sizeof(int), compare_pids);
  }

  *pids_size = actual_count;
  if (!feof(fp) && actual_count >= max_size) {
    LOGGER(WARNING, "PID array full, only stored %d PIDs", max_size);
  }
  fclose(fp);
  return 0;
}

char *GetNthMapsToken(char *line, int n) {
  char *context = NULL;
  // coverity[var_deref_model] Yes, we're using strtok_r correctly
  char *token = strtok_r(line, " ", &context);
  while (token && --n > 0) {
    token = strtok_r(NULL, " ", &context);
  }
  return token;
}

int library_exists_in_process_maps(char const *libName, unsigned int pid) {
  int ret = -1;
  char fileName[512];
  sprintf(fileName, "/proc/%d/maps", pid);

  FILE *fMaps = fopen(fileName, "r");
  if (NULL == fMaps) {
    return ret;
  }

  // Read the file line by line
  char line[1024];
  while (fgets(line, sizeof(line), fMaps)) {
    char *libPath = GetNthMapsToken(line, 6);
    if (libPath == NULL) {
      continue;
    }
    char *p = strstr(libPath, libName);
    if (p == NULL) {
      continue;
    }
    ret = 0;
    break;
  }

  fclose(fMaps);
  return ret;
}

int device_pid_in_same_container(unsigned int pid) {
  // For the k8s container, these two namespace types already
  // determine whether the PID is in the same container or not.
  const char *ns_types[] = {"mnt", "cgroup", NULL};
  for (int i = 0; ns_types[i] != NULL; i++) {
    char device_path[128];
    struct stat device_st;
    snprintf(device_path, sizeof(device_path), "/proc/%d/ns/%s", pid, ns_types[i]);
    if (stat(device_path, &device_st) != 0) {
      return -1;
    }
    char self_path[128];
    struct stat self_st;
    snprintf(self_path, sizeof(self_path), "%s/%s", PID_SELF_NS_PATH, ns_types[i]);
    if (stat(self_path, &self_st) != 0) {
      return -1;
    }
    if (device_st.st_ino != self_st.st_ino) {
      return -1;
    }
  }
  return 0;
}

int file_exist(const char *file_path) {
  return (access(file_path, F_OK) == 0) ? 0 : -1;
}

int pid_exist(int pid) {
  if (pid <= 0) return -1;
  return (kill(pid, 0) == 0 ? 0 : (errno == ESRCH ? -1 : 0));
//  int result = kill(pid, 0);
//  if (result == 0) {
//    return 0;
//  }
//  switch (errno) {
//  case ESRCH:
//    return -1;
//  case EPERM:
//    return 0;
//  }
//  char path[64];
//  snprintf(path, sizeof(path), "/proc/%d", pid);
//  return file_exist(path);
}

// 1: is zombie
// 0: not zombie
// -1: error
int is_zombie_proc(int pid) {
  if (pid <= 0) return -1;
  char path[64];
  snprintf(path, sizeof(path), "/proc/%d/stat", pid);

  FILE *fp = fopen(path, "r");
  if (fp == NULL) return -1;

  int unused_pid;
  char comm[1024];
  char state;
  int ret = fscanf(fp, "%d %s %c", &unused_pid, comm, &state);
  fclose(fp);

  if (ret != 3 || ret == EOF) {
    return -1;
  } else if (state == 'Z' || state == 'z') {
    return 1;
  }
  return 0;
}