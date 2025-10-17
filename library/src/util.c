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

#define MAX_PID_STR_LEN 32
#define COMPATIBILITY_MODE_ENV "ENV_COMPATIBILITY_MODE"
#define CUDA_MEMORY_LIMIT_ENV "CUDA_MEM_LIMIT"
#define CUDA_MEMORY_RATIO_ENV "CUDA_MEM_RATIO"
#define CUDA_CORE_LIMIT_ENV "CUDA_CORE_LIMIT"
#define CUDA_CORE_SOFT_LIMIT_ENV "CUDA_CORE_SOFT_LIMIT"
#define CUDA_MEM_OVERSOLD_ENV "CUDA_MEM_OVERSOLD"
#define GPU_DEVICES_UUID_ENV "GPU_DEVICES_UUID"
#define VMEM_NODE_ENABLED_ENV "VMEMORY_NODE_ENABLED"

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
  int ret = -1;
  char *str = NULL;
  str = getenv(COMPATIBILITY_MODE_ENV);
  if (unlikely(!str)) {
    goto DONE;
  }
  *mode = iec_to_bytes(str);
  ret = 0;
DONE:
  return ret;
}

int get_mem_ratio(uint32_t index, double *ratio) {
  int ret = -1;
  char *str = NULL;
  char env[32] = {0};
  sprintf(env, "%s_%d", CUDA_MEMORY_RATIO_ENV, index);
  str = getenv(env);
  if (unlikely(!str)) {
    str = getenv(CUDA_MEMORY_RATIO_ENV);
    if (unlikely(!str)) {
      goto DONE;
    }
  }
  *ratio = atof(str);
  ret = 0;
DONE:
  return ret;
}

int get_mem_limit(uint32_t index, size_t *limit) {
  int ret = -1;
  char *str = NULL;
  char env[32] = {0};
  sprintf(env, "%s_%d", CUDA_MEMORY_LIMIT_ENV, index);
  str = getenv(env);
  if (unlikely(!str)) {
    str = getenv(CUDA_MEMORY_LIMIT_ENV);
    if (unlikely(!str)) {
      goto DONE;
    }
  }
  *limit = iec_to_bytes(str);
  ret = 0;
DONE:
  return ret;
}

int get_core_limit(uint32_t index, int *limit) {
  int ret = -1;
  char *str = NULL;
  char env[32] = {0};
  sprintf(env, "%s_%d", CUDA_CORE_LIMIT_ENV, index);
  str = getenv(env);
  if (unlikely(!str)) {
    str = getenv(CUDA_CORE_LIMIT_ENV);
    if (unlikely(!str)) {
      goto DONE;
    }
  }
  *limit = iec_to_bytes(str);
  ret = 0;
DONE:
  return ret;
}

int get_core_soft_limit(uint32_t index, int *limit) {
  int ret = -1;
  char *str = NULL;
  char env[32] = {0};
  sprintf(env, "%s_%d", CUDA_CORE_SOFT_LIMIT_ENV, index);
  str = getenv(env);
  if (unlikely(!str)) {
    str = getenv(CUDA_CORE_SOFT_LIMIT_ENV);
    if (unlikely(!str)) {
      goto DONE;
    }
  }
  *limit = iec_to_bytes(str);
  ret = 0;
DONE:
  return ret;
}

int get_devices_uuid(char *uuids) {
  int ret = -1;
  char *str = NULL;
  str = getenv(GPU_DEVICES_UUID_ENV);
  if (unlikely(!str)) {
    return ret;
  }
  strcpy(uuids, str);
  return 0;
}

int get_vmem_node_enabled(int *i) {
  int ret = -1;
  char *str = NULL;
  str = getenv(VMEM_NODE_ENABLED_ENV);
  if (unlikely(!str)) {
    goto DONE;
  }
  if (strcmp(str, "true") == 0 || strcmp(str, "TRUE") == 0 || strcmp(str,"1") == 0) {
    *i = 1;
  } else {
    *i = 0;
  }
  ret = 0;
DONE:
  return ret;
}

int get_mem_oversold(uint32_t index, int *i) {
  int ret = -1;
  char *str = NULL;
  char env[32] = {0};
  sprintf(env, "%s_%d", CUDA_MEM_OVERSOLD_ENV, index);
  str = getenv(env);
  if (unlikely(!str)) {
    str = getenv(CUDA_MEM_OVERSOLD_ENV);
    if (unlikely(!str)) {
      goto DONE;
    }
  }
  if (strcmp(str, "true") == 0 || strcmp(str, "TRUE") == 0 || strcmp(str,"1") == 0) {
    *i = 1;
  } else {
    *i = 0;
  }
  ret = 0;
DONE:
  return ret;
}

static int is_current_cgroup(const char *cgroup_procs_path) {
  int ret = -1;
  if (!cgroup_procs_path) {
    LOGGER(ERROR, "invalid NULL cgroup_procs_path parameter");
    return ret;
  }

  char pid_str[MAX_PID_STR_LEN];
  snprintf(pid_str, sizeof(pid_str), "%d", (int)getpid());

  FILE *fp = NULL;
  if ((fp = fopen(cgroup_procs_path, "r")) == NULL) {
    return ret;
  }

  char line[MAX_PID_STR_LEN];
  while (fgets(line, sizeof(line), fp)) {
    line[strcspn(line, "\n")] = '\0';
    if (strcmp(line, pid_str) == 0) {
      ret = 0;
      break;
    }
  }
  fclose(fp);
  return ret;
}

static int is_current_container(const char *path) {
  int ret = -1;
  if (!path) {
    LOGGER(ERROR, "invalid NULL path parameter");
    return ret;
  }

  DIR *dir = NULL;
  if ((dir = opendir(path)) == NULL) {
    LOGGER(ERROR, "cannot open directory %s: %s", path, strerror(errno));
    return ret;
  }

  struct dirent *entry;
  while ((entry = readdir(dir)) != NULL) {
    if (strcmp(entry->d_name, ".") == 0 || strcmp(entry->d_name, "..") == 0) {
      continue;
    }
    char full_path[PATH_MAX];
    snprintf(full_path, sizeof(full_path), "%s/%s", path, entry->d_name);

    if (entry->d_type == DT_DIR) {
      if ((ret = is_current_container(full_path)) == 0) {
        break;
      }
    } else if (strcmp(entry->d_name, CGROUP_PROCS_FILE) == 0) {
      if (is_current_cgroup(full_path) == 0) {
        ret = 0;
      } else if (errno == ENOTSUP) {
        // For a threaded cgroup, read returns ENOTSUP, and we should
        // read from cgroup.threads instead.
        char threads_path[PATH_MAX];
        snprintf(threads_path, sizeof(threads_path), "%s/%s", path, CGROUP_THREADS_FILE);
        ret = is_current_cgroup(threads_path);
      }
      if (ret == 0) {
        break;
      }
    }
  }

  closedir(dir);
  return ret;
}

int extract_container_id(char *base_path, char *container_id, size_t container_id_size) {
  int ret = -1;
  if (!base_path) {
    LOGGER(ERROR, "invalid NULL base_path parameter");
    return ret;
  }

  DIR *dir = NULL;
  if ((dir = opendir(base_path)) == NULL) {
    return ret;
  }

  struct dirent *entry;
  while ((entry = readdir(dir)) != NULL) {
    if (strcmp(entry->d_name, ".") == 0 || strcmp(entry->d_name, "..") == 0) {
      continue;
    }
    if (entry->d_type != DT_DIR)  continue;

    char full_path[PATH_MAX];
    snprintf(full_path, sizeof(full_path), "%s/%s", base_path, entry->d_name);

    if ((ret = is_current_container(full_path)) == 0) {
      strncpy(container_id, entry->d_name, container_id_size - 1);
      container_id[container_id_size - 1] = '\0';
      break;
    }
  }

  closedir(dir);
  return ret;
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

static int read_procs_file(const char *dir, const char *filename, int *pids, int max_size, int *current_count) {
  char filepath[PATH_MAX];
  snprintf(filepath, sizeof(filepath), "%s/%s", dir, filename);

  FILE *f = fopen(filepath, "r");
  if (!f) {
    if (errno == ENOENT) {
      return 0;
    }
    return -1;
  }

  char line[MAX_PID_STR_LEN];
  while (fgets(line, sizeof(line), f) && *current_count < max_size) {
    line[strcspn(line, "\n")] = '\0';
    if (strlen(line) == 0) {
      continue;
    }

    char *endptr;
    long pid = strtol(line, &endptr, 10);
    if (endptr == line || *endptr != '\0') {
      continue;
    }
    if (pid <= 0 || pid > INT_MAX) {
      continue;
    }
    pids[*current_count] = (int)pid;
    (*current_count)++;
  }

  fclose(f);
  return 0;
}

static int process_directory(const char *path, int *pids, int max_size, int *current_count) {
  // Attempt to read the cgroup.procs file from the current directory.
  int ret = read_procs_file(path, CGROUP_PROCS_FILE, pids, max_size, current_count);
  // If reading cgroup.com fails and the error is ENOTSUP, try reading cgroup.threads.
  if (ret != 0 && errno == ENOTSUP) {
    ret = read_procs_file(path, CGROUP_THREADS_FILE, pids, max_size, current_count);
  }
  return ret;
}

// Recursively traverse the directory and collect all PIDs.
static int walk_directory(const char *path, int *pids, int max_size, int *current_count) {
  // First, handle the current directory
  if (process_directory(path, pids, max_size, current_count) != 0) {
    if (errno != ENOENT) {
      LOGGER(WARNING, "failed to read process file in %s: %s", path, strerror(errno));
    }
  }
  DIR *dir = opendir(path);
  if (!dir) {
    if (errno != EACCES) {
      LOGGER(WARNING, "cannot open directory %s: %s", path, strerror(errno));
    }
    return 0;
  }

  struct dirent *entry;
  while ((entry = readdir(dir)) != NULL && *current_count < max_size) {
    if (strcmp(entry->d_name, ".") == 0 || strcmp(entry->d_name, "..") == 0) {
      continue;
    }

    char full_path[PATH_MAX];
    snprintf(full_path, sizeof(full_path), "%s/%s", path, entry->d_name);

    // Skip files that cannot be stated.
    struct stat statbuf;
    if (stat(full_path, &statbuf) != 0) {
      continue;
    }

    if (S_ISDIR(statbuf.st_mode)) {
      // Recursive processing of subdirectories.
      if (walk_directory(full_path, pids, max_size, current_count) != 0) {
        closedir(dir);
        return -1;
      }
    }
  }

  closedir(dir);
  return 0;
}

int extract_container_pids(char *base_path, int *pids, int *pids_size) {
  if (!base_path || !pids || !pids_size) {
    LOGGER(ERROR, "invalid NULL parameter");
    *pids_size = 0;
    return -1;
  }

  if (access(base_path, F_OK) != 0) {
    *pids_size = 0;
    return -1;
  }

  int max_size = *pids_size;
  int actual_count = 0;

  if (walk_directory(base_path, pids, max_size, &actual_count) != 0) {
    LOGGER(ERROR, "failed to walk directory %s", base_path);
    *pids_size = 0;
    return -1;
  }

  if (actual_count > 0) {
    qsort(pids, actual_count, sizeof(int), compare_pids);
  }

  *pids_size = actual_count;
  if (actual_count >= max_size) {
    LOGGER(WARNING, "PID array full, only stored %d PIDs", max_size);
  }
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