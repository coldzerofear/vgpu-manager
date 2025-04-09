/*
 * Tencent is pleased to support the open source community by making TKEStack
 * available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not
 * use this file except in compliance with the License. You may obtain a copy of
 * the License at
 *
 * https://opensource.org/licenses/Apache-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OF ANY KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations under the License.
 */

#ifndef HIJACK_LIBRARY_H
#define HIJACK_LIBRARY_H

#ifdef __cplusplus
extern "C" {
#endif

#include <inttypes.h>
#include <limits.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <unistd.h>

#include "nvml-subset.h"
#include "cuda-subset.h"

/**
 * vGPU manager base path
 */
#define VGPU_MANAGER_PATH "/etc/vgpu-manager"

/**
 * Controller configuration base path
 */
#define VGPU_CONFIG_PATH (VGPU_MANAGER_PATH "/config")

/**
 * Controller configuration file name
 */
#define CONTROLLER_CONFIG_FILE_NAME "vgpu.config"

/**
 * Controller configuration file path
 */
#define CONTROLLER_CONFIG_FILE_PATH (VGPU_MANAGER_PATH "/config/" CONTROLLER_CONFIG_FILE_NAME)

/**
 * Controller driver file name
 */
#define CONTROLLER_DRIVER_FILE_NAME "libvgpu-control.so"

/**
 * Controller driver file path
 */
#define CONTROLLER_DRIVER_FILE_PATH (VGPU_MANAGER_PATH "/driver/" CONTROLLER_DRIVER_FILE_NAME)

#define PID_SELF_CGROUP_PATH "/proc/self/cgroup"

#define HOST_PROC_PATH (VGPU_MANAGER_PATH "/.host_proc")

#define HOST_PROC_CGROUP_PID_PATH (VGPU_MANAGER_PATH "/.host_proc/%d/cgroup")

#define HOST_CGROUP_PATH (VGPU_MANAGER_PATH "/.host_cgroup")

#define HOST_CGROUP_PID_BASE_PATH (VGPU_MANAGER_PATH "/.host_cgroup/%s")

#define CGROUP_PROCS_FILE "cgroup.procs"

/**
 * Proc file path for driver version
 */
#define DRIVER_VERSION_PATH "/proc/driver/nvidia/version"

/**
 * Driver regular expression pattern
 */
#define DRIVER_VERSION_MATCH_PATTERN "([0-9]+)(\\.[0-9]+)+"

#define MAX_DEVICE_COUNT 16

/**
 * Max sample pid size
 */
#define MAX_PIDS (1024)
#define likely(x) __builtin_expect(!!(x), 1)
#define unlikely(x) __builtin_expect(!!(x), 0)

#define ROUND_UP(n, base) ((n) % (base) ? (n) + (base) - (n) % (base) : (n))

#define BUILD_BUG_ON(condition) ((void)sizeof(char[1 - 2 * !!(condition)]))

#define CAS(ptr, old, new) __sync_bool_compare_and_swap((ptr), (old), (new))
#define UNUSED __attribute__((unused))

#define MILLISEC (1000UL * 1000UL)

#define TIME_TICK (10)
#define FACTOR (32)
#define MAX_UTILIZATION (100)

#define GET_VALID_VALUE(x) (((x) >= 0 && (x) <= 100) ? (x) : 0)
#define CODEC_NORMALIZE(x) (x * 85 / 100)

typedef struct {
  void *fn_ptr;
  char *name;
} entry_t;

typedef struct {
  int start_index;
  int end_index;
  int batch_code;
} batch_t;

typedef struct {
  int major;
  int minor;
} __attribute__((packed, aligned(8))) version_t;

typedef struct {
  char uuid[48];
  size_t total_memory;
  size_t real_memory;
  int hard_core;
  int soft_core;
  int core_limit;
  int hard_limit;
  int memory_limit;
  int memory_oversold;
} __attribute__((packed, aligned(8))) device_t;

/**
 * Controller configuration data format
 */
typedef struct {
  version_t driver_version;
  char pod_uid[48];
  char pod_name[64];
  char pod_namespace[64];
  char container_name[64];
  device_t devices[MAX_DEVICE_COUNT];
  int device_count;
  int compatibility_mode;
} __attribute__((packed, aligned(8))) resource_data_t;

/**
 * Dynamic computing power limit configuration
 */
typedef struct {
  int change_limit_interval;
  int usage_threshold;
  int error_recovery_step;
} __attribute__((packed, aligned(8))) dynamic_config_t;

typedef enum {
  INFO = 0,
  ERROR = 1,
  WARNING = 2,
  FATAL = 3,
  VERBOSE = 4,
  DETAIL = 5,
} log_level_enum_t;

typedef struct {
    int tid;
    void *pointer;
} tid_dlsym;

#define DLMAP_SIZE 100

typedef enum VGPU_COMPATIBILITY_MODE_enum {
	HOST_COMPATIBILITY_MODE        = 0,
	CGROUPV1_COMPATIBILITY_MODE    = 1,
	CGROUPV2_COMPATIBILITY_MODE    = 2,
	OPEN_KERNEL_COMPATIBILITY_MODE = 100
} VGPU_COMPATIBILITY_MODE;

extern void* _dl_sym(void*, const char*, void*);

typedef void (*atomic_fn_ptr)(int, void *);

typedef void* (*fp_dlsym)(void*, const char*);

#define FUNC_ATTR_VISIBLE  __attribute__((visibility("default"))) 

#define LOGGER(level, format, ...)                                \
  ({                                                              \
    char *_print_level_str = getenv("LOGGER_LEVEL");              \
    int _print_level = 3;                                         \
    if (_print_level_str) {                                       \
      _print_level = (int)strtoul(_print_level_str, NULL, 10);    \
      _print_level = _print_level < 0 ? 3 : _print_level;         \
    }                                                             \
    if (level <= _print_level) {                                  \
      if (level == INFO) {                                        \
        fprintf(stderr, "[vGPU INFO(%d|%s|%d)]: " format "\n",    \
        getpid(), basename(__FILE__), __LINE__, ##__VA_ARGS__);   \
      } else if (level == ERROR) {                                \
        fprintf(stderr, "[vGPU ERROR(%d|%s|%d)]: " format "\n",   \
        getpid(), basename(__FILE__), __LINE__, ##__VA_ARGS__);   \
      } else if (level == WARNING) {                              \
        fprintf(stderr, "[vGPU WARN(%d|%s|%d)]: " format "\n",    \
        getpid(), basename(__FILE__), __LINE__, ##__VA_ARGS__);   \
      } else if (level == FATAL) {                                \
        fprintf(stderr, "[vGPU FATAL(%d|%s|%d)]: " format "\n",   \
        getpid(), basename(__FILE__), __LINE__, ##__VA_ARGS__);   \
      } else if (level == VERBOSE) {                              \
        fprintf(stderr, "[vGPU VERBOSE(%d|%s|%d)]: " format "\n", \
        getpid(), basename(__FILE__), __LINE__, ##__VA_ARGS__);   \
      } else if (level == DETAIL) {                               \
        fprintf(stderr, "[vGPU DETAIL(%d|%s|%d)]: " format "\n",  \
        getpid(), basename(__FILE__), __LINE__, ##__VA_ARGS__);   \
      }                                                           \
    }                                                             \
    if (level == FATAL) {                                         \
      exit(-1);                                                   \
    }                                                             \
  })

/**
 * Load library and initialize some data
 */
void load_necessary_data();

/**
 * Retrieve the currently used memory of the device
 */
void get_used_gpu_memory_by_device(void *, nvmlDevice_t);

#ifdef __cplusplus
}
#endif

#endif
