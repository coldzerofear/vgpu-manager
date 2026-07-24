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

/* Toolchain assumption gate.
 *
 * libvgpu-control.so is a glibc-only, GCC-compatible-only build target.
 * Hard-failing here gives a clear error instead of letting the build
 * limp on until linker errors point at obscure undefined references
 * like `_dl_sym`. Concrete dependencies that have no portable fallback:
 *
 *   loader.c       — extern void* _dl_sym(...) is a glibc PRIVATE
 *                    symbol used as the last-resort fallback in
 *                    init_real_dlsym(). musl / Bionic do not export it.
 *   loader.c       — dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.X") is glibc
 *                    versioned-symbol lookup. POSIX has dlsym, not
 *                    dlvsym; the GLIBC_* version strings are also
 *                    glibc-internal.
 *   hook.h         — FUNC_ATTR_VISIBLE, UNUSED, likely / unlikely use
 *                    GCC __attribute__ / __builtin_expect extensions.
 *   cuda_hook.c    — CAS uses __sync_bool_compare_and_swap (GCC builtin).
 *   src/vulkan/    — __atomic_compare_exchange_n / __atomic_load_n
 *                    (GCC C11 atomics).
 *
 * Note that __GNUC__ is also defined by Clang for GCC-compat — the
 * gate accepts Clang on glibc, which works in practice (Clang
 * implements all the GCC extensions we use). What we reject is musl
 * (Alpine), Bionic (Android), MSVC, and other non-GNU/non-glibc
 * toolchains where the LD_PRELOAD dlsym-interception mechanism cannot
 * be assembled.
 */
#if !defined(__GNUC__) || !defined(__GLIBC__)
#  error "libvgpu-control.so requires a GCC-compatible compiler "        \
         "(GCC or Clang) AND glibc. The library uses _dl_sym, dlvsym "   \
         "with GLIBC_* versions, __attribute__((visibility/alias/used)), "\
         "__builtin_expect / __builtin_return_address, and "             \
         "__sync_bool_compare_and_swap / __atomic_* builtins, none of "  \
         "which have portable fallbacks. musl libc (Alpine), Bionic "    \
         "(Android), and MSVC are out of scope."
#endif

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

#define FAKE_GPU_UUID "GPU-00000000-0000-0000-0000-000000000000"

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
  char reg_uuid[UUID_BUFFER_SIZE];
} resource_data_t;

/**
 * Dynamic SM controller configuration. All tunables that affect runtime
 * algorithm behaviour live here so the boot log can dump them with a
 * single line and operators have one place to look.
 *
 * Field ordering NOTE: this struct is part of the .so internal layout
 * (g_dynamic_config is non-static but hidden via the linker version
 * script). Reorganising field order across a build that other parts of
 * the codebase already link against would shift offsets; keep
 * usage_threshold at its original position and APPEND new fields to the
 * tail. New fields must be POD with explicit sized types so the dump
 * line in sm_controller_init() stays trivial to write.
 *
 * Loaded once at sm_controller_init() (under pthread_once g_init_set).
 * After init this struct is read-only at runtime by the watcher thread,
 * so no volatile / atomics needed; init runs before any watcher thread
 * is spawned, and fork() re-runs init in the child via the pthread_once
 * reset in child_after_fork() if the child needs it.
 *
 * usage_threshold:    avg-free-headroom threshold for soft-mode up_limit
 *                     periodic adjust. >= 0; env CUDA_SM_USAGE_THRESHOLD.
 * sm_controller_kind: 0=delta (stock), 1=aimd, 2=auto.
 * aimd_md_divisor:    AIMD MD factor as a double so users can pick 1.5
 *                     for a softer cut than 2 or 3. Clamped >= 1.01 at
 *                     load time so we never accidentally /1 (no-op) or
 *                     /<=0 (UB).
 * aimd_eff_ratio:     parts-per-thousand, eff_limit = up * x / 1000.
 * aimd_ai_base_div:   AI step base divisor.
 * aimd_deadband_ratio: parts-per-thousand, deadband lower edge.
 * aimd_md_cooldown_cycles: post-MD watcher-cycle cooldown (0 disables).
 * auto_debounce_cycles: N consecutive observations to flip exclusivity FSM.
 * auto_external_util_threshold: external util percent above which the
 *                     device is considered "shared with other Pods".
 * delta_ramp_floor_divisor: delta()'s grow/cut step is floored at
 *                     g_total*diff/(up_limit*N); N sets the bulk-ramp length in
 *                     watcher cycles (~N cycles, SM-independent). Smaller = faster
 *                     ramp / coarser near-limit tracking on tiny slices. Default
 *                     64. N <= 0 disables the floor (delta reverts to its raw
 *                     sm^2-scaled step); delta() guards the division on N > 0, so
 *                     a non-positive value is not loaded-clamped.
 */
typedef struct {
  /* Preserved: was already in this struct in earlier versions. */
  int    usage_threshold;
  /* Appended for V2.1/P1/P2: consolidates 8 prior file-static globals. */
  int    sm_controller_kind;
  double aimd_md_divisor;
  int    aimd_eff_ratio;
  int    aimd_ai_base_div;
  int    aimd_deadband_ratio;
  int    aimd_md_cooldown_cycles;
  int    auto_debounce_cycles;
  int    auto_external_util_threshold;
  int    delta_ramp_floor_divisor;
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

/* memory_node_t.type -- what a virtual-memory record stands for, and therefore
 * how cuMemFreeAsync must retire it (see cuda_hook.c).
 *
 * UVA_SYNC / UVA_ASYNC name memory the oversold path handed out as managed
 * memory; only UVA_ASYNC has to drain the stream and fall back to cuMemFree.
 *
 * CAPTURE and ASYNC_BRIDGE both name ordinary device memory that is only being
 * ACCOUNTED for, and both exist for exactly one reason: to cover a window in
 * which the allocation is invisible to NVML. They differ in which window.
 *   ASYNC_BRIDGE spans the driver call to the stream synchronize.
 *   CAPTURE spans the capture itself. During capture cuMemAllocAsync only
 *     reserves an address -- no physical memory exists until the graph is
 *     launched, from which point NVML reports the graph pool. The charge is
 *     what makes several allocations inside one capture accumulate against the
 *     limit, and cuStreamEndCapture retires it (see free_gpu_virt_memory_by_graph)
 *     because holding it past the launch would double-count against NVML and
 *     leak whenever the graph, rather than the application, owns the pointer. */
#define MEMORY_TYPE_UVA_SYNC     1
#define MEMORY_TYPE_UVA_ASYNC    2
#define MEMORY_TYPE_CAPTURE      3
#define MEMORY_TYPE_ASYNC_BRIDGE 4

typedef struct {
  CUdeviceptr dptr;
  size_t bytes;
  /* One of MEMORY_TYPE_* above. */
  int type;
  /* Owning graph, for MEMORY_TYPE_CAPTURE only; NULL otherwise. Records are
   * only ever charged when this is known, so every charge can be retired at
   * cuStreamEndCapture. */
  CUgraph graph;
  /* Device the record was charged against, or -1 if it was never charged.
   * Retiring a capture charge must not depend on being able to ask the driver
   * which device is current -- by then the context may be gone, and failing to
   * discharge after the node is dropped would strand the charge forever. */
  int host_index;
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
  /* Count of PIDs on this device that are NOT in our container. Updated
   * by get_used_gpu_utilization in lockstep with user/sys per the active
   * compatibility mode. Used by the watcher to decide whether to reset
   * up_limits on new-process arrival without being fooled by our own
   * intra-container fork (DataLoader workers, etc). Strict counting:
   * NVIDIA driver always-resident threads (nvidia-persistenced, MPS)
   * DO count as external -- but they appear once and stay forever, so
   * they don't cause repeated resets. HOST_COMPATIBILITY_MODE has no
   * container boundary -> this field stays 0. */
  int external_process_num;
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

static inline int get_logger_print_level(void) {
  static int print_level = -1;

  if (print_level == -1) {
    char *print_level_str = getenv("LOGGER_LEVEL");
    if (print_level_str && *print_level_str) {
      print_level = (int)strtoul(print_level_str, NULL, 10);
    }
    print_level = print_level < FATAL ? WARNING : print_level;
    print_level = print_level > DETAIL ? DETAIL : print_level;
  }

  return print_level;
}

#define LOGGER_SHOULD_PRINT(level) \
  ((level) >= 0 && (level) <= get_logger_print_level())

#define LOGGER(level, format, ...)                                  \
  ({                                                                \
    if (LOGGER_SHOULD_PRINT(level)) {                               \
      fprintf(stderr, "[vGPU %s(%d|%" PRIuPTR "|%s:%d)]: " format "\n", \
              _level_names[level], getpid(),                        \
              (uintptr_t)pthread_self(),                            \
              basename(__FILE__), __LINE__, ##__VA_ARGS__);         \
    }                                                               \
    if (unlikely(level == FATAL)) {                                 \
      exit(1);                                                      \
    }                                                               \
  })

/**
 * Given the pointer cuGetProcAddress produced for `symbol`, return our hook for
 * that exact entry point, or NULL.
 *
 * The pointer says which function the driver chose -- version and stream
 * variant included -- and `symbol` bounds which family that may belong to: a
 * version or _ptsz/_ptds suffix stated in the request pins that component, one
 * left out is the driver's to choose. So "cuLaunchKernel" can resolve to
 * cuLaunchKernel_v2_ptsz, while "cuMemAlloc_v2" resolves to nothing but v2.
 *
 * Three outcomes, distinguished by BOTH results together:
 *   return non-NULL             - identified, and this is its hook.
 *   return NULL, *name non-NULL - identified, we hook no version of it.
 *                                 Keep the driver's pointer; substituting a
 *                                 base-named hook here would bind an ABI it
 *                                 does not have.
 *   return NULL, *name NULL     - not a driver entry point this build knows.
 *                                 Fall back to name-based substitution.
 */
void* lookup_cuda_hook_ptr(void *real_fn, const char *symbol, const char **name);

/**
 * Record, once per symbol and at VERBOSE level, a driver symbol that went
 * through us uninstrumented. Leaves a trail for versions a newer driver added
 * that this build does not intercept.
 */
void note_unhooked_symbol(const char *symbol);

/**
 * Load library and initialize some data
 */
void load_necessary_data();

/**
 * Initialize device ID mapping relationship
 */
void init_devices_mapping();

/**
 * Retrieve the currently used memory of the device
 */
void get_used_gpu_memory_by_device(void *, nvmlDevice_t);

/**
 * Retrieve the used virtual memory recorded on the GPU
 */
void get_used_gpu_virt_memory(void *, int device_id);

void check_cleanup_vmem_nodes_by_device(int host_index);

void malloc_gpu_virt_memory(CUdeviceptr dptr, size_t bytes, int type, int device_id);

/**
 * Record a graph-capture allocation. Same as malloc_gpu_virt_memory() with
 * MEMORY_TYPE_CAPTURE, but ties the record to the capturing graph so
 * free_gpu_virt_memory_by_graph() can retire it at cuStreamEndCapture.
 */
void malloc_gpu_virt_memory_captured(CUdeviceptr dptr, size_t bytes,
                                     CUgraph graph, int device_id);

void free_gpu_virt_memory(CUdeviceptr dptr);

/**
 * Retire every capture record belonging to graph, discharging the shared
 * counter for each. Called when the capture ends -- successfully or not.
 * Each record carries the device it was charged against, so this needs no
 * device argument and cannot be defeated by a missing current context.
 */
void free_gpu_virt_memory_by_graph(CUgraph graph);

int get_gpu_virt_memory_type(CUdeviceptr dptr);

int get_nvml_device_index_by_cuda_device(CUdevice device);

int get_host_device_index_by_cuda_device(CUdevice device);

int get_host_device_index_by_nvml_device(nvmlDevice_t device);

void register_to_remote_with_data(const char* pod_uid, const char* container, const char* reg_uuid);

#ifdef __cplusplus
}
#endif

#endif
