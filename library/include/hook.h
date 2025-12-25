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
#include <pthread.h>

#include "list.h"
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
 * Container pids configuration file name
 */
#define CONTAINER_PIDS_CONFIG_FILE_NAME "pids.config"
/**
 * Container pids configuration file path
 */
#define CONTAINER_PIDS_CONFIG_FILE_PATH (VGPU_MANAGER_PATH "/config/" CONTAINER_PIDS_CONFIG_FILE_NAME)

/**
 * Controller sm utilization watcher file name
 */
#define CONTROLLER_SM_UTIL_FILE_NAME "sm_util.config"
/**
 * Controller sm utilization watcher file path
 */
#define CONTROLLER_SM_UTIL_FILE_PATH (VGPU_MANAGER_PATH "/watcher/" CONTROLLER_SM_UTIL_FILE_NAME)

/**
 * Controller driver file name
 */
#define CONTROLLER_DRIVER_FILE_NAME "libvgpu-control.so"
/**
 * Controller driver file path
 */
#define CONTROLLER_DRIVER_FILE_PATH (VGPU_MANAGER_PATH "/driver/" CONTROLLER_DRIVER_FILE_NAME)

#define PID_SELF_CGROUP_PATH "/proc/self/cgroup"

#define PID_SELF_NS_PATH "/proc/self/ns"

#define HOST_PROC_PATH (VGPU_MANAGER_PATH "/.host_proc")

#define HOST_PROC_CGROUP_PID_PATH (VGPU_MANAGER_PATH "/.host_proc/%d/cgroup")

#define HOST_CGROUP_PATH (VGPU_MANAGER_PATH "/.host_cgroup")

#define CGROUP_PROCS_FILE "cgroup.procs"
#define CGROUP_THREADS_FILE "cgroup.threads"

#define HOST_CGROUP_PID_BASE_PATH (VGPU_MANAGER_PATH "/.host_cgroup/%s")

#define TMP_DIR "/tmp"

#define VGPU_LOCK_DIR "/.vgpu_lock"

#define VGPU_LOCK_PATH (TMP_DIR VGPU_LOCK_DIR)

#define VMEMORY_NODE_DIR "/.vmem_node"

#define VMEMORY_NODE_PATH (TMP_DIR VMEMORY_NODE_DIR)

#define VMEMORY_NODE_FILE_PATH (TMP_DIR VMEMORY_NODE_DIR "/vmem_node.config")

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
#define UUID_BUFFER_SIZE (48)
#define NAME_BUFFER_SIZE (64)
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
} version_t;

typedef struct {
  char uuid[UUID_BUFFER_SIZE];
  size_t total_memory;
  size_t real_memory;
  int hard_core;
  int soft_core;
  int core_limit;
  int hard_limit;
  int memory_limit;
  int memory_oversold;
  int activate;
} device_t;

/**
 * Controller configuration data format
 */
typedef struct {
  version_t driver_version;
  char pod_uid[UUID_BUFFER_SIZE];
  char pod_name[NAME_BUFFER_SIZE];
  char pod_namespace[NAME_BUFFER_SIZE];
  char container_name[NAME_BUFFER_SIZE];
  device_t devices[MAX_DEVICE_COUNT];
  // TODO No modifications allowed during runtime.
  int compatibility_mode;
  int sm_watcher;
  int vmem_node;
} resource_data_t;

/**
 * Dynamic computing power limit configuration
 */
typedef struct {
  int change_limit_interval;
  int usage_threshold;
  int error_recovery_step;
} dynamic_config_t;

typedef struct {
  unsigned int pid;
  unsigned long long usedGpuMemory;
  unsigned int  gpuInstanceId;
  unsigned int  computeInstanceId;
} nvmlProcessInfoV2_t;

typedef struct {
  nvmlProcessUtilizationSample_t process_util_samples[MAX_PIDS];
  unsigned int process_util_samples_size;
  unsigned long long lastSeenTimeStamp;
  nvmlProcessInfoV2_t compute_processes[MAX_PIDS];
  unsigned int compute_processes_size;
  nvmlProcessInfoV2_t graphics_processes[MAX_PIDS];
  unsigned int graphics_processes_size;
  unsigned char lock_byte;
} device_process_t;

typedef struct {
  device_process_t devices[MAX_DEVICE_COUNT];
} device_util_t;

typedef struct {
  CUdeviceptr dptr;
  size_t bytes;
  struct list_head node;
} memory_node_t;

typedef struct {
  int pid;
  size_t used;
} process_used_t;

typedef struct {
  process_used_t processes[MAX_PIDS];
  unsigned int processes_size;
  unsigned char lock_byte;
} device_vmem_used_t;

typedef struct {
  device_vmem_used_t devices[MAX_DEVICE_COUNT];
} device_vmemory_t;

/** dynamic rate control */
typedef struct {
  int user_current;
  int sys_current;
  uint64_t checktime;
  int valid;
  int sys_process_num;
} utilization_t;

typedef struct {
  pthread_t tid;
  void *pointer;
} tid_dlsym;

#define DLMAP_SIZE 100

typedef enum VGPU_COMPATIBILITY_MODE_enum {
  HOST_COMPATIBILITY_MODE        = 0,
  CGROUPV1_COMPATIBILITY_MODE    = 1,
  CGROUPV2_COMPATIBILITY_MODE    = 2,
  OPEN_KERNEL_COMPATIBILITY_MODE = 100,
  CLIENT_COMPATIBILITY_MODE      = 200
} VGPU_COMPATIBILITY_MODE;

extern void* _dl_sym(void*, const char*, void*);

typedef void (*atomic_fn_ptr)(int, void *);

typedef void* (*fp_dlsym)(void*, const char*);

#define FUNC_ATTR_VISIBLE  __attribute__((visibility("default")))

#define container_of(ptr, type, member)                                        \
  ({                                                                           \
    const typeof(((type *)0)->member) *__mptr =                                \
        (const typeof(((type *)0)->member) *)(ptr);                            \
    (type *)((char *)__mptr - offsetof(type, member));                         \
  })

typedef enum {
  FATAL = 0,
  ERROR = 1,
  WARNING = 2,
  INFO = 3,
  VERBOSE = 4,
  DETAIL = 5,
} log_level_enum_t;

static const char *_level_names[] = {
  "FATAL",    /* LOG_LEVEL_FATAL   */
  "ERROR",    /* LOG_LEVEL_ERROR   */
  "WARNING",  /* LOG_LEVEL_WARNING */
  "INFO",     /* LOG_LEVEL_INFO    */
  "VERBOSE",  /* LOG_LEVEL_VERBOSE */
  "DETAIL"    /* LOG_LEVEL_DETAIL  */
};

#define LOGGER(level, format, ...)                                  \
  ({                                                                \
    static int _print_level = -1;                                   \
    if (_print_level == -1) {                                       \
      char *_print_level_str = getenv("LOGGER_LEVEL");              \
      if (_print_level_str && *_print_level_str) {                  \
        _print_level = (int)strtoul(_print_level_str, NULL, 10);    \
      }                                                             \
      _print_level = _print_level < FATAL ? INFO : _print_level;    \
      _print_level = _print_level > DETAIL ? DETAIL : _print_level; \
    }                                                               \
    if (level >= 0 && level <= _print_level) {                      \
      fprintf(stderr, "[vGPU %s(%d|%ld|%s|%d)]: " format "\n",      \
              _level_names[level], getpid(), pthread_self(),        \
              basename(__FILE__), __LINE__, ##__VA_ARGS__);         \
    }                                                               \
    if (unlikely(level == FATAL)) {                                 \
      exit(1);                                                      \
    }                                                               \
  })

/**
 * Load library and initialize some data
 */
void load_necessary_data();
void _load_necessary_data();

/**
 * Retrieve the currently used memory of the device
 */
void get_used_gpu_memory_by_device(void *, nvmlDevice_t);

/**
 * Retrieve the used virtual memory recorded on the GPU
 */
void get_used_gpu_virt_memory(void *, int device_id);

void malloc_gpu_virt_memory(CUdeviceptr dptr, size_t bytes, int device_id);

void free_gpu_virt_memory(CUdeviceptr dptr, int device_id);

int get_nvml_device_index_by_cuda_device(CUdevice device);

int get_host_device_index_by_cuda_device(CUdevice device);

int get_host_device_index_by_nvml_device(nvmlDevice_t device);

void register_to_remote_with_data(const char* pod_uid, const char* container);

#ifdef __cplusplus
}
#endif

#endif
