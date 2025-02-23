#include "include/hook.h"

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <dirent.h>
#include <string.h>
#include <unistd.h>

#define CUDA_MEMORY_LIMIT_ENV "CUDA_MEM_LIMIT_%d"
#define CUDA_CORE_LIMIT_ENV "CUDA_CORE_LIMIT_%d"
#define CUDA_CORE_SOFT_LIMIT_ENV "CUDA_CORE_SOFT_LIMIT_%d"
#define CUDA_MEM_OVERSOLD_ENV "CUDA_MEM_OVERSOLD_%d"
#define GPU_DEVICES_UUID_ENV "GPU_DEVICES_UUID"

static inline int get_limit(const char *name, char *data) {
  char *str = NULL;
  int ret = -1;
  str = getenv(name);
  if (unlikely(!str)) {
    goto DONE;
  }
  // memcpy(data, str, strlen(str));
  strcpy(data, str);
  ret = 0;
DONE:
  return ret;
}

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

int get_mem_limit(uint32_t index, size_t *limit) {
  int ret = -1;
  char tmp[16] = {0};
  char env[32] = {0};
  sprintf(env, CUDA_MEMORY_LIMIT_ENV, index);
  ret = get_limit(env, tmp);
  if (unlikely(ret)) {
    return ret;
  }
  *limit = iec_to_bytes(tmp);
  return 0;
}

int get_core_limit(uint32_t index, int *limit) {
  int ret = -1;
  char *str = NULL;
  char env[32] = {0};
  sprintf(env, CUDA_CORE_LIMIT_ENV, index);
  str = getenv(env);
  if (unlikely(!str)) {
    str = getenv("CUDA_CORE_LIMIT");
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
  sprintf(env, CUDA_CORE_SOFT_LIMIT_ENV, index);
  str = getenv(env);
  if (unlikely(!str)) {
    str = getenv("CUDA_CORE_SOFT_LIMIT");
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
  char tmp[768] = {0};
  ret = get_limit(GPU_DEVICES_UUID_ENV, tmp);
  if (unlikely(ret)) {
    return ret;
  }
  strcpy(uuids, tmp);
  return 0;
}

int get_mem_oversold(uint32_t index, int *i) {
  int ret = -1;
  char *str = NULL;
  char env[32] = {0};
  sprintf(env, CUDA_MEM_OVERSOLD_ENV, index);
  str = getenv(env);
  if (unlikely(!str)) {
    str = getenv("CUDA_MEM_OVERSOLD");
    if (unlikely(!str)) {
        goto DONE;
    }
  }
  if (strcmp(str, "true") == 0 ||
      strcmp(str, "TRUE") == 0 ||
      strcmp(str,"1") == 0) {
    *i = 1;
  } else {
    *i = 0;
  }
  ret = 0;
DONE:
  return ret;
}

int is_current_cgroup(const char *file_path) {
  FILE *file = fopen(file_path, "r");
  if (file == NULL) {
    perror("fopen");
    return 0;
  }
  char buffer[128];
  char pid_str[32];
  snprintf(pid_str, sizeof(pid_str), "%d", (int)getpid());
  while (fgets(buffer, sizeof(buffer), file) != NULL) {
    size_t len = strlen(buffer);
    if (len > 0 && buffer[len - 1] == '\n') {
      buffer[len - 1] = '\0';
    }
    if (strcmp(buffer, pid_str) == 0 || strcmp(buffer, "1") == 0) {
      fclose(file);
      return 1;
    }
  }
  fclose(file);
  return 0;
}

int extract_container_id_v2(char *path, char *container_id, size_t container_id_size) {
  DIR *dir;
  struct dirent *entry;
  if ((dir = opendir(path)) == NULL) {
//    perror("opendir");
//    LOGGER(ERROR, "Failed to open %s", path);
    return -1;
  }

  while ((entry = readdir(dir)) != NULL) {
    if (strcmp(entry->d_name, ".") == 0 ||
        strcmp(entry->d_name, "..") == 0) {
      continue;
    }
    if (entry->d_type == DT_DIR) {
      char full_path[FILENAME_MAX];
      snprintf(full_path, sizeof(full_path), "%s/%s/cgroup.procs", path, entry->d_name);
      if (is_current_cgroup(full_path)) {
        strncpy(container_id, entry->d_name, container_id_size - 1);
        container_id[container_id_size - 1] = '\0';
        closedir(dir);
        return 0;
      }
    }
  }
  closedir(dir);
  return -1;
}
