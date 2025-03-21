#include "include/hook.h"

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <dirent.h>
#include <string.h>
#include <unistd.h>
#include <limits.h>

#define CUDA_MEMORY_LIMIT_ENV "CUDA_MEM_LIMIT"
#define CUDA_MEMORY_RATIO_ENV "CUDA_MEM_RATIO"
#define CUDA_CORE_LIMIT_ENV "CUDA_CORE_LIMIT"
#define CUDA_CORE_SOFT_LIMIT_ENV "CUDA_CORE_SOFT_LIMIT"
#define CUDA_MEM_OVERSOLD_ENV "CUDA_MEM_OVERSOLD"
#define GPU_DEVICES_UUID_ENV "GPU_DEVICES_UUID"

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
  int ret = 0;
  FILE *file = fopen(file_path, "r");
  if (file == NULL) {
    perror("fopen");
    return ret;
  }
  char buffer[128];
  char pid_str[32];
  snprintf(pid_str, sizeof(pid_str), "%d", (int)getpid());

  while (fgets(buffer, sizeof(buffer), file) != NULL) {
    size_t len = strlen(buffer);
    if (len > 0 && buffer[len - 1] == '\n') {
      buffer[len - 1] = '\0';
    }
    if (strcmp(buffer, pid_str) == 0) {
      ret = 1;
      break;
    }
  }
  fclose(file);
  return ret;
}

int is_current_container(char *path) {
  int ret = -1;
  DIR *dir;
  struct dirent *entry;
  if ((dir = opendir(path)) == NULL) {
    return ret;
  }
  while ((entry = readdir(dir)) != NULL) {
    if (strcmp(entry->d_name, ".") == 0 || strcmp(entry->d_name, "..") == 0) {
      continue;
    }
    char full_path[PATH_MAX];
    snprintf(full_path, sizeof(full_path), "%s/%s", path, entry->d_name);

    if (entry->d_type == DT_DIR) {
      ret = is_current_container(full_path);
      if (ret == 0) {
        break;
      }
    } else if (strcmp(entry->d_name, "cgroup.procs") == 0) {
      if (is_current_cgroup(full_path)) {
        ret = 0;
        break;
      }
    }
  }
  closedir(dir);
  return ret;
}

int extract_container_id(char *path, char *container_id, size_t container_id_size) {
  int ret = -1;
  DIR *dir;
  struct dirent *entry;
  if ((dir = opendir(path)) == NULL) {
    return ret;
  }
  while ((entry = readdir(dir)) != NULL) {
    if (strcmp(entry->d_name, ".") == 0 || strcmp(entry->d_name, "..") == 0) {
      continue;
    }
    if (entry->d_type == DT_DIR) {
      char full_path[PATH_MAX];
      snprintf(full_path, sizeof(full_path), "%s/%s", path, entry->d_name);
      ret = is_current_container(full_path);
      if (ret == 0) {
        strncpy(container_id, entry->d_name, container_id_size - 1);
        container_id[container_id_size - 1] = '\0';
        break;
      }
    }
  }
  closedir(dir);
  return ret;
}
