/*
 * Tencent is pleased to support the open source community by making TKEStack
 * available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 * Copyright 2024-2026 coldzerofear
 * Modifications made for the vgpu-manager project by coldzerofear.
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
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <unistd.h>
#include <pthread.h>

/* Toolchain assumption gate. libvgpu-control.so is a glibc-only,
 * GCC-compatible-only build target: it depends on glibc's private
 * _dl_sym and dlvsym(GLIBC_2.X) symbol versioning in loader.c, GCC
 * __attribute__/__builtin_expect extensions in this header, and GCC
 * atomic builtins elsewhere -- none of which have a portable fallback.
 * Hard-failing here gives a clear error instead of letting the build
 * limp on until linker errors point at obscure undefined references.
 * Clang is accepted (it implements the same GCC extensions on glibc);
 * musl (Alpine), Bionic (Android), and MSVC are out of scope, since the
 * LD_PRELOAD dlsym-interception mechanism can't be assembled on them. */
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
#include "session.h"

/**
 * vGPU manager base path
 */
#define VGPU_MANAGER_PATH "/etc/vgpu-manager"

/**
 * Controller configuration base path
 */
#define VGPU_CONFIG_PATH_LOCAL (VGPU_MANAGER_PATH "/config")

/**
 * Controller configuration file name
 */
#define CONTROLLER_CONFIG_FILE_NAME "vgpu.config"
/**
 * Controller configuration file path
 */
#define CONTROLLER_CONFIG_FILE_PATH_LOCAL (VGPU_MANAGER_PATH "/config/" CONTROLLER_CONFIG_FILE_NAME)

/**
 * Container pids configuration file name
 */
#define CONTAINER_PIDS_CONFIG_FILE_NAME "pids.config"
/**
 * Container pids configuration file path
 */
#define CONTAINER_PIDS_CONFIG_FILE_PATH_LOCAL (VGPU_MANAGER_PATH "/config/" CONTAINER_PIDS_CONFIG_FILE_NAME)

/**
 * Controller sm utilization watcher file name
 */
#define CONTROLLER_SM_UTIL_FILE_NAME "sm_util.config"
/**
 * Controller sm utilization watcher file path
 */
#define CONTROLLER_SM_UTIL_FILE_PATH_LOCAL (VGPU_MANAGER_PATH "/watcher/" CONTROLLER_SM_UTIL_FILE_NAME)

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

#define VGPU_LOCK_PATH_LOCAL (TMP_DIR VGPU_LOCK_DIR)

#define VMEMORY_NODE_DIR "/.vmem_node"

#define VMEMORY_NODE_PATH_LOCAL (TMP_DIR VMEMORY_NODE_DIR)

#define VMEMORY_NODE_FILE_PATH_LOCAL (TMP_DIR VMEMORY_NODE_DIR "/vmem_node.config")

/* Every path above is the LOCAL-mode location. What the code actually uses is
 * the session-aware form below: inside a lupine-server child it resolves under
 * that connection's session directory, everywhere else it is the local path
 * verbatim. See session.h for the layout and why this scoping exists. */
#define VGPU_CONFIG_PATH                session_path(SESSION_CONFIG_DIR)
#define CONTROLLER_CONFIG_FILE_PATH     session_path(SESSION_CONFIG)
#define CONTAINER_PIDS_CONFIG_FILE_PATH session_path(SESSION_PIDS)
#define CONTROLLER_SM_UTIL_FILE_PATH    session_path(SESSION_SM_UTIL)
#define VGPU_LOCK_PATH                  session_path(SESSION_LOCK_DIR)
#define VMEMORY_NODE_PATH               session_path(SESSION_VMEM_DIR)
#define VMEMORY_NODE_FILE_PATH          session_path(SESSION_VMEM_FILE)

/**
 * Proc file path for driver version
 */
#define DRIVER_VERSION_PATH "/proc/driver/nvidia/version"

/**
 * Driver regular expression pattern
 */
#define DRIVER_VERSION_MATCH_PATTERN "([0-9]+)(\\.[0-9]+)+"

#define MAX_DEVICE_COUNT 16

/* Padding granule for per-device hot state. 128 rather than 64 because Intel's
 * L2 adjacent-line prefetcher pulls lines in 128B-aligned pairs (so 64B padding
 * can still leave two devices effectively sharing) and some ARM64 parts use a
 * 128B granule. Lives here, not in cuda_hook.c, because both the per-process
 * dev_hot_t and the shared sm_node_dev_t below are built on it -- and the
 * latter is a cross-version ABI, so the granule must be pinned in one place.
 * See the false-sharing rationale above dev_hot_t in cuda_hook.c. */
#define CACHELINE_SIZE 128

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

/* ---- vgpu.config shared-region ABI ---- *
 * resource_data_t is written by the Go manager (pkg/config/vgpu) or, on the
 * env-fallback path, by the first library process in the container, and
 * read by every library process. Once the Go side mutates a device_t at
 * runtime, a plain multi-field read on the hot path can tear (see
 * docs/resource_data_seqlock_versioning_design.md), so each device_t
 * carries a seqlock version (`seq`) and is padded to one cache line --
 * always read via get_device_snapshot(), never a bare field access on
 * anything that may be mutated. The region opens with a frozen header
 * validated on map, same contract as vmem_node/sm_node.
 *
 * THIS STRUCT IS AN ABI: fixed-width types, explicit padding,
 * _Static_asserts, and a Go mirror (pkg/config/vgpu/vgpu_config.go) pinned
 * by the same offsets. Bump CONFIG_LAYOUT_VERSION on any field type/order/
 * offset change and update both sides' asserts. No _Atomic here -- a
 * non-lock-free _Atomic would downgrade to libatomic's per-process lock
 * table (see the sm_node note below); plain fixed-width types plus
 * __atomic_* with explicit order at each site instead. */
#define CONFIG_MAGIC               0x56474346U   /* "VGCF" */
#define CONFIG_LAYOUT_VERSION      1U
#define CONFIG_FILE_SIZE           8192          /* fixed; decoupled from sizeof so a
                                                  * later, larger struct never resizes
                                                  * the file and SIGBUSes an old map */
#define DRIVER_VERSION_BUFFER_SIZE 32            /* NVIDIA driver string "550.90.07" */
#define DEVICE_T_RESERVED_I32      7             /* per-device growth room, fills line */

typedef struct {
  /* seqlock version: even = stable, odd = write in progress. Offset 0, accessed
   * atomically by C (get_device_snapshot) and Go (ModifyDevice). */
  uint32_t seq;
  uint32_t _seq_pad;               /* keep total_memory 8-byte aligned */
  char     uuid[UUID_BUFFER_SIZE];
  uint64_t total_memory;           /* was size_t; fixed width for the ABI */
  uint64_t real_memory;
  int32_t  hard_core;
  int32_t  soft_core;
  int32_t  core_limit;
  int32_t  hard_limit;
  int32_t  memory_limit;
  int32_t  memory_oversold;
  int32_t  activate;
  int32_t  reserved[DEVICE_T_RESERVED_I32];
} __attribute__((aligned(CACHELINE_SIZE))) device_t;

_Static_assert(sizeof(device_t) == CACHELINE_SIZE, "device_t must be one cache line");
_Static_assert(_Alignof(device_t) == CACHELINE_SIZE, "device_t must be cache-line aligned");
_Static_assert(offsetof(device_t, seq) == 0, "seqlock word must stay at offset 0");
_Static_assert(offsetof(device_t, total_memory) == 56, "device_t total_memory offset");

/**
 * Controller configuration data format
 */
typedef struct {
  /* ---- FROZEN HEADER: 128 bytes (one cache line), permanent ABI ---- */
  uint32_t  magic;                 /* CONFIG_MAGIC */
  uint32_t  layout_version;        /* CONFIG_LAYOUT_VERSION */
  uint32_t  region_size;           /* = sizeof(resource_data_t) */
  uint32_t  device_count;          /* = MAX_DEVICE_COUNT */
  version_t cuda_version;          /* CUDA major.minor (was misnamed driver_version) */
  char      driver_version[DRIVER_VERSION_BUFFER_SIZE]; /* NVIDIA driver string */
  uint8_t   _hdr_reserved[CACHELINE_SIZE - 56];
  /* ---- end frozen header (offset 128) ---- */

  /* Pod identity + flags: written once, never mutated at runtime -- no seqlock. */
  char pod_uid[UUID_BUFFER_SIZE];
  char pod_name[NAME_BUFFER_SIZE];
  char pod_namespace[NAME_BUFFER_SIZE];
  char container_name[NAME_BUFFER_SIZE];
  char reg_uuid[UUID_BUFFER_SIZE];
  int32_t compatibility_mode;
  int32_t sm_watcher;
  int32_t vmem_node;
  uint8_t _meta_reserved[84];      /* pad devices[] onto a cache line (offset 512) */

  /* Per-device config, each one cache line, seqlock-protected via devices[i].seq. */
  device_t devices[MAX_DEVICE_COUNT];
} resource_data_t;

_Static_assert(offsetof(resource_data_t, magic) == 0, "frozen header: magic@0");
_Static_assert(offsetof(resource_data_t, layout_version) == 4, "frozen header: layout_version@4");
_Static_assert(offsetof(resource_data_t, pod_uid) == CACHELINE_SIZE,
               "frozen header must be exactly one cache line");
_Static_assert(offsetof(resource_data_t, devices) == 512,
               "devices[] must start on a cache line (offset 512)");
_Static_assert(offsetof(resource_data_t, devices) % CACHELINE_SIZE == 0,
               "devices[] must be cache-line aligned");
_Static_assert(sizeof(resource_data_t) <= CONFIG_FILE_SIZE,
               "config region must fit the permanently reserved file size");

/* Byte-range lock offset of device i's seq word -- for the Go writer's F_WRLCK
 * and get_device_snapshot()'s F_RDLCK slow-path fallback. Go's
 * getConfigLockOffset() must agree. */
#define GET_CONFIG_LOCK_OFFSET(i) \
  (offsetof(resource_data_t, devices) + (size_t)(i) * sizeof(device_t) + offsetof(device_t, seq))

/* Tear-free snapshot of devices[host_index], read under the per-device seqlock.
 * Out of range or config not yet loaded -> a zeroed device_t (activate=0,
 * memory_limit=0), which every caller already treats as "no limit". Use this
 * for any decision that reads TWO OR MORE co-varying fields together. */
device_t get_device_snapshot(int host_index);

/* Same, against a config the caller mapped itself -- the checkpoint provider
 * reads the session quota before the library is initialized. */
device_t get_device_snapshot_of(const resource_data_t *cfg, int host_index);

/* vgpu.config region I/O (src/config_io.c). Both target
 * CONTROLLER_CONFIG_FILE_PATH, so they follow the session in remote mode.
 * Return 0 on success. */
int mmap_file_to_config_path(resource_data_t **data);
int write_file_to_config_path(resource_data_t *data);

/* Host indexes of activated devices, ascending; returns the count. The single
 * source of the container's device ordering -- see the definition. */
int config_allowed_devices(const resource_data_t *cfg, int *host_indexes, int max);

/* Translate between the container's device numbering and the node's. Prefer
 * these over indexing the array yourself: they carry the bounds check, and
 * getting that wrong resolves to another session's GPU rather than crashing. */
int config_allowed_device_at(const resource_data_t *cfg, unsigned int visible_index);
int config_visible_index_of(const resource_data_t *cfg, int host_index);

/* Has the config path moved since the live config was read? See the definition
 * -- this is what keeps a forked child from using another session.s quota
 * without making a plain local fork re-map the same file. */
int config_source_moved(void);
void config_source_record(void);

extern resource_data_t *g_vgpu_config;

/* Cheap single-field read for a hot gate that consults exactly ONE int32 device
 * field (core_limit / memory_limit / memory_oversold). A single aligned int32
 * cannot tear, so this needs no seqlock and no whole-struct copy -- it is the
 * per-launch-safe path; reach for get_device_snapshot() only when two or more
 * fields must be read as a consistent set. Returns 0 when the index is out of
 * range or the config is not loaded (== feature off), matching the snapshot's
 * zeroed fallback. `host_index` must be side-effect-free (it is evaluated more
 * than once); `field` must be an int32 member of device_t. */
#define get_device_flag(host_index, field)                                     \
  (((host_index) >= 0 && (host_index) < MAX_DEVICE_COUNT &&                    \
    likely(g_vgpu_config != NULL))                                             \
     ? __atomic_load_n(&g_vgpu_config->devices[(host_index)].field,            \
                       __ATOMIC_ACQUIRE)                                       \
     : 0)

/**
 * Dynamic SM controller configuration. All tunables that affect runtime
 * algorithm behaviour live here so the boot log can dump them in a single
 * line and operators have one place to look.
 *
 * Field ordering: keep usage_threshold at its original position and
 * append new fields to the tail, since g_dynamic_config is non-static
 * (hidden via the linker version script) and reordering would shift
 * offsets other linked code relies on.
 *
 * Loaded once at sm_controller_init() (under pthread_once g_init_set),
 * then read-only at runtime by the watcher thread -- no volatile/atomics
 * needed, since init always runs before any watcher thread is spawned.
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
 * delta_increment_divisor: seed divisor -- delta's minimum step is
 *                     total*MIN_INCREMENT/R, its granularity. 81920 default;
 *                     see include/sm_delta.h.
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
  int    delta_increment_divisor;
  int    delta_ramp_floor_divisor;
  /* APPENDED (see the field-ordering note above): container-wide shared token
   * bucket, on by default -- one bucket per container is what a core quota
   * means. 0 = per-process bucket; env CUDA_SM_SHARED_BUCKET opts out, except
   * in a session where the opt-out is refused. */
  int    sm_shared_bucket;
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

/* memory_node_t.type -- what a virtual-memory record stands for, and
 * therefore how cuMemFreeAsync must retire it (see cuda_hook.c).
 *
 * UVA_SYNC/UVA_ASYNC name memory the oversold path handed out as managed
 * memory; only UVA_ASYNC has to drain the stream and fall back to
 * cuMemFree. CAPTURE and ASYNC_BRIDGE both name ordinary device memory
 * that's only being ACCOUNTED for, covering a window where the allocation
 * is invisible to NVML: ASYNC_BRIDGE spans the driver call to the stream
 * synchronize, CAPTURE spans the capture itself (cuMemAllocAsync only
 * reserves an address during capture -- no physical memory exists until
 * the graph launches and NVML reports the pool). cuStreamEndCapture
 * retires the CAPTURE charge, since holding it past launch would
 * double-count against NVML. */
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

/* vmem_node region. Same frozen-header idea as sm_node below (the host
 * directory outlives a container while the .so is version-pinned per
 * container, so a newer library can inherit a file an older one wrote),
 * but with one hard difference: this region has a host-side Go reader
 * (pkg/config/vmem), so the layout is a CROSS-LANGUAGE ABI. Anything
 * changed here must be mirrored in DeviceVMemoryT, and getVmemoryLockOffset()
 * must keep agreeing with GET_VMEMORY_LOCK_OFFSET in lock.c -- Go computes
 * that offset by hand, so a mismatch produces no error, just silently
 * non-overlapping locks. TestVMemoryLayoutMatchesC exists to catch it. */
#define VMEM_NODE_MAGIC          0x564D4E44U   /* "VMND" */
#define VMEM_NODE_LAYOUT_VERSION 1U
/* Permanent constant, like SM_NODE_FILE_SIZE, and here it prevents a real
 * crash rather than merely simplifying: the Go manager keeps this file mmap'd.
 * If a container restarted with a library that shrank the struct and
 * ftruncate'd the file down, the manager's existing mapping would extend past
 * EOF and touching it is SIGBUS. A size that never changes removes that class
 * outright. Current use 256.25 KiB; reserved 320 KiB (~1.25x). */
#define VMEM_NODE_FILE_SIZE      (320 * 1024)

typedef struct {
  /* ---- FROZEN HEADER: 16 bytes, permanent ABI. Same contract as sm_node. */
  uint32_t magic;             /* VMEM_NODE_MAGIC          */
  uint32_t layout_version;    /* VMEM_NODE_LAYOUT_VERSION */
  uint32_t region_size;       /* sizeof(device_vmemory_t) */
  uint32_t device_count;      /* MAX_DEVICE_COUNT         */
  /* ---- end frozen header. */
  uint8_t  _pad[CACHELINE_SIZE - 16];
  device_vmem_used_t devices[MAX_DEVICE_COUNT];   /* shifted down by 128B */
} device_vmemory_t;

_Static_assert(offsetof(device_vmemory_t, devices) == CACHELINE_SIZE,
               "vmem region header must be exactly one cache line");
_Static_assert(sizeof(device_vmemory_t) <= VMEM_NODE_FILE_SIZE,
               "vmem region must fit the permanently reserved file size");
_Static_assert(offsetof(device_vmemory_t, magic) == 0,
               "frozen header ABI: magic stays at offset 0");
_Static_assert(offsetof(device_vmemory_t, layout_version) == 4,
               "frozen header ABI: layout_version stays at offset 4");

/* ---- sm_node -- container-wide shared token bucket for SM (compute) limiting ---- *
 * Symmetric with vmem_node: that region carries cross-process MEMORY
 * isolation state, this one COMPUTE isolation state. See
 * docs/sm_multiproc_shared_bucket_design.md.
 *
 * Why it exists: g_dev_hot[].cur_cuda_cores is per-PROCESS, so N processes
 * in one container each hold their own bucket and can each independently
 * decide "tokens available, go" at the same instant. Moving it into
 * MAP_SHARED memory makes "how much may this container still launch" a
 * physical invariant rather than a statistical average, at no hot-path
 * cost (a CAS doesn't care which address space the word lives in).
 *
 * THIS STRUCT IS AN ABI: written to a file, mapped by several processes,
 * outliving any single library version -- hence fixed-width types,
 * explicit padding, and _Static_asserts. Unlike vmem_node it has no
 * host-side Go reader, but it still crosses library versions. */

/* Container-side path. NOT the container's own /tmp: this directory is bind
 * mounted per container by the device plugin / DRA driver, exactly like
 * /tmp/.vgpu_lock and /tmp/.vmem_node, because the workload's own /tmp may be
 * shadowed, read-only, or swept. */
#define SM_NODE_DIR       "/.sm_node"
#define SM_NODE_PATH_LOCAL      (TMP_DIR SM_NODE_DIR)
#define SM_NODE_FILE_PATH_LOCAL (TMP_DIR SM_NODE_DIR "/sm_node.config")

/* Sampling-ownership lock. A separate file from the region, on purpose:
 * the init lock in map_sm_node_region is taken and dropped inside one
 * function, but this one is held for the process's whole life. On kernels
 * without OFD locks, a classic POSIX record lock drops when the process
 * closes ANY descriptor for that file -- which map_sm_node_region does
 * during init -- so sharing one file would let leadership evaporate
 * silently. Must also never be deleted while containers run: a new inode
 * from unlink+recreate wouldn't be mutually exclusive with the old one's
 * locks, so pre-start cleanup removes sm_node.config but not this file. */
#define SM_NODE_LOCK_PATH_LOCAL (TMP_DIR SM_NODE_DIR "/sm_node.lock")

/* Session-aware forms; see the block next to VMEMORY_NODE_FILE_PATH. */
#define SM_NODE_PATH      session_path(SESSION_SM_DIR)
#define SM_NODE_FILE_PATH session_path(SESSION_SM_FILE)
#define SM_NODE_LOCK_PATH session_path(SESSION_SM_LOCK)

/* The file size is a PERMANENT constant, deliberately decoupled from
 * sizeof(sm_node_region_t): a later version may grow the struct without
 * changing the file size, so the region is never resized, so an older process
 * still holding a mapping can never have its tail fall past EOF (which would
 * be SIGBUS on access). Current use is 128 + 16*128 = 2176B. */
#define SM_NODE_FILE_SIZE 8192

#define SM_NODE_MAGIC          0x534D4E44U   /* "SMND" */
/* BUMP THIS whenever any field below changes type, order, or offset.
 * The guard compares it and rebuilds the region on mismatch; forgetting to
 * bump it means a new library silently reads an old layout's bytes. */
#define SM_NODE_LAYOUT_VERSION 3U   /* v3: + sample_interval_ns (adaptive staleness) */

/* No volatile, no _Atomic. volatile provides no concurrency guarantee (today's
 * correctness comes entirely from the CAS macro), and _Atomic risks a
 * lock-free downgrade: a non-lock-free _Atomic makes the compiler use
 * libatomic's address-keyed lock table, which is PER PROCESS -- two processes
 * mapping the same word would take different locks and the protection would
 * silently evaporate. Plain fixed-width types plus __atomic_* builtins with an
 * explicit memory order at each site. */
typedef struct {
  /* Hot: CAS'd by every launching thread in every process. */
  int64_t cur_cuda_cores;       /* the token bucket itself                  */
  int64_t total_cuda_cores;     /* thread*sm*FACTOR; bucket ceiling         */
  int64_t last_refill_ns;       /* refill election stamp, CAS'd per cycle   */
  int64_t share;                /* was shares[]                             */
  /* Monotonic stamp of the last published utilization sample, written LAST
   * (release) by the sampling owner so a reader that acquire-loads it knows
   * the four s_* fields below are complete. Also the staleness signal: if this
   * falls too far behind, the owner is alive but not sampling (hung in NVML,
   * say) and a standby resamples for itself rather than trusting it. */
  int64_t sample_published_ns;
  /* Controller integrator state. Only the cycle's election winner reads or
   * writes these, so the election itself serialises them -- no lock needed,
   * only acquire/release pairing so each winner sees the previous winner's
   * writes. Every one of these MUST live here: the election hands the device
   * to a different PROCESS each cycle, so a per-process copy would advance at
   * ~1/N rate and fracture into N divergent controllers. */
  int32_t up_limit;             /* was up_limits[]                          */
  int32_t is_cnt;               /* was is[]                                 */
  int32_t avg_sys_free;         /* was avg_sys_frees[]                      */
  int32_t pre_external_proc;    /* was pre_external_process_nums[]          */
  int32_t md_cooldown;          /* was g_aimd_md_cooldown[] -- without this
                                 * AIMD re-fires MD every cycle and cuts
                                 * share by md_divisor^N ("MD avalanche"),
                                 * which is the exact thing the cooldown was
                                 * introduced to prevent.                   */
  int32_t excl_debounced;       /* was g_is_exclusive_debounced[]      ┐    */
  int32_t excl_streak;          /* was g_exclusive_pending_streak[]    │FSM */
  int32_t lost_excl_pending;    /* was g_lost_exclusivity_pending[]    ┘    */
  /* Written by rate_limiter() on throttle (any thread, any process),
   * read-and-cleared once per cycle by the election winner. Sharing it
   * changes the question from "did THIS PROCESS throttle" to "did ANYONE in
   * the container throttle", which is the correct question once the bucket
   * is shared. */
  int32_t throttled_since_watch;
  /* Utilization sample published by whichever process owns sampling for this
   * device. Standbys read these instead of calling NVML themselves.
   *
   * This is the whole point of centralising sampling: nvmlDeviceGetProcessUtilization
   * is expensive and degrades when called often -- the local-driver path already
   * carries a comment saying frequent calls legitimately return NOT_FOUND. N
   * processes each polling it every ~100ms multiplies exactly the call rate the
   * driver dislikes, and N is largest in the notebook containers this design
   * targets. Publishing one sample makes the cost O(1) per device, not O(N). */
  int32_t s_user_current;       /* container-aggregate utilization           */
  int32_t s_sys_current;        /* device-wide utilization                   */
  int32_t s_sys_process_num;
  int32_t s_external_proc_num;
  /* Owning process of the sampling lock. Diagnostics only -- never a liveness
   * signal. Ownership is decided by the kernel-held file lock, which stays
   * correct when a pid is recycled or a record goes stale. */
  int32_t leader_pid;
  /* The owner's OWN measured interval between publishes, so a standby can tell
   * "slow" from "stuck" without assuming how fast sampling ought to be.
   *
   * The watcher's cadence is not guaranteed: when per-device processing
   * overruns its slot the loop falls back to a 10ms floor sleep, so the period
   * becomes (processing + 10ms) per iteration and a device is revisited every
   * dev_count iterations. Slow NVML on a 4-device batch can push a device's
   * period into the hundreds of milliseconds. A fixed staleness limit tuned
   * for ~100ms would then fire permanently, every standby would resume
   * sampling, and the extra NVML load would make the owner slower still --
   * a feedback loop that ends with centralisation providing nothing. */
  int64_t sample_interval_ns;
  uint8_t _pad[CACHELINE_SIZE - 104];
} __attribute__((aligned(CACHELINE_SIZE))) sm_node_dev_t;

typedef struct {
  /* ---- FROZEN HEADER: these 16 bytes are a PERMANENT ABI. ----
   * The layout guard has to read them before it knows which version wrote
   * the file, so they must predate every possible version difference.
   * Never change their type, order, or offset. */
  uint32_t magic;
  uint32_t layout_version;
  uint32_t region_size;
  uint32_t device_count;
  /* ---- end frozen header; everything below may evolve with the version. */
  uint8_t  _pad[CACHELINE_SIZE - 16];
  sm_node_dev_t devices[MAX_DEVICE_COUNT];
} sm_node_region_t;

_Static_assert(sizeof(sm_node_dev_t) == CACHELINE_SIZE,
               "sm_node_dev_t must occupy exactly one padded cache line");
_Static_assert(_Alignof(sm_node_dev_t) == CACHELINE_SIZE,
               "sm_node_dev_t must be cache-line aligned or false sharing returns");
_Static_assert(offsetof(sm_node_region_t, devices) == CACHELINE_SIZE,
               "region header must be exactly one cache line");
_Static_assert(sizeof(sm_node_region_t) <= SM_NODE_FILE_SIZE,
               "region must fit the permanently reserved file size");
_Static_assert(offsetof(sm_node_region_t, magic) == 0,
               "frozen header ABI: magic stays at offset 0");
_Static_assert(offsetof(sm_node_region_t, layout_version) == 4,
               "frozen header ABI: layout_version stays at offset 4");
_Static_assert(offsetof(sm_node_region_t, region_size) == 8,
               "frozen header ABI: region_size stays at offset 8");
_Static_assert(offsetof(sm_node_region_t, device_count) == 12,
               "frozen header ABI: device_count stays at offset 12");

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

typedef enum VGPU_COMPATIBILITY_MODE_enum {
  HOST_COMPATIBILITY_MODE        = 0,
  CGROUPV1_COMPATIBILITY_MODE    = 1,
  CGROUPV2_COMPATIBILITY_MODE    = 2,
  OPEN_KERNEL_COMPATIBILITY_MODE = 100,
  CLIENT_COMPATIBILITY_MODE      = 200,
  /* Remote mode. A lupine-server child is one process of a container, not the
   * container, so attribution is "pid is in this session's pids.config" -- the
   * provider registers every child of the session there. Kept distinct from
   * CLIENT (which reads the same kind of file) because CLIENT also implies the
   * manager-registration handshake, which has no meaning on a GPU node.
   *
   * The dispatch chain tests (mode & X) == X, so the value must not be a
   * subset of another mode's bits, nor they of it: 300 & 200 = 8, 300 & 100 =
   * 36, 200 & 300 = 8, 100 & 300 = 4 -- no false match either way. */
  SESSION_COMPATIBILITY_MODE     = 300
} VGPU_COMPATIBILITY_MODE;

typedef void (*atomic_fn_ptr)(int, void *);

typedef void* (*fp_dlsym)(void*, const char*);

/* Does `symbol` name a driver entry point we hook? Mirrors the export
 * patterns in the version script: cu[A-Z]* / cudbg* / nvml[A-Z]*.
 *
 * The uppercase discriminator is what keeps cuBLAS, cuFFT, cuDNN,
 * cudaMalloc and curl_* out -- they share the "cu" prefix but are not
 * driver API, and matching them costs a symbol lookup plus a misleading
 * unhooked-symbol note on every resolution. Short strings stop at the
 * NUL via && short-circuit, so no read runs past the terminator. */
static inline int symbol_is_cuda_api(const char *s) {
  if (s[0] != 'c' || s[1] != 'u') return 0;
  if (s[2] >= 'A' && s[2] <= 'Z') return 1;      /* cu[A-Z]* */
  return strncmp(s + 2, "dbg", 3) == 0;          /* cudbg*   */
}

static inline int symbol_is_nvml_api(const char *s) {
  return s[0] == 'n' && s[1] == 'v' && s[2] == 'm' && s[3] == 'l' &&
         s[4] >= 'A' && s[4] <= 'Z';             /* nvml[A-Z]* */
}

#define FUNC_ATTR_VISIBLE  __attribute__((visibility("default")))
/* Hidden, but still a global symbol: dlsym_entry.S calls these by name and
 * hidden visibility keeps that a direct call instead of a PLT round trip. */
#define FUNC_ATTR_HIDDEN   __attribute__((visibility("hidden")))

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

#define LOGGER(level, format, ...)                                      \
  ({                                                                    \
    if (LOGGER_SHOULD_PRINT(level)) {                                   \
      fprintf(stderr, "[vGPU %s(%d|%" PRIuPTR "|%s:%d)]: " format "\n", \
              _level_names[level], getpid(), (uintptr_t)pthread_self(), \
              basename(__FILE__), __LINE__, ##__VA_ARGS__);             \
    }                                                                   \
    if (unlikely(level == FATAL)) {                                     \
      exit(1);                                                          \
    }                                                                   \
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

/**
 * Acquire/release an fcntl record lock, preferring OFD locks (Linux >= 3.15)
 * and falling back to classic POSIX locks when the kernel rejects them.
 * wait != 0 blocks (F_OFD_SETLKW), wait == 0 does not. Defined in lock.c.
 */
struct flock;
int ofd_fcntl(int fd, int wait, struct flock *fl);

/**
 * Map the container-wide sm_node shared region, creating or rebuilding it as
 * needed. Returns 0 and sets *data on success. On ANY failure returns non-zero
 * and leaves *data NULL: the caller must then fall back to per-process buckets.
 * This never exits -- shared SM limiting is an optimisation, not a correctness
 * prerequisite.
 */
int map_sm_node_region(sm_node_region_t **data);

/**
 * Warn if the vmem_node region file is no longer the inode we mapped -- deleted
 * or replaced from inside the container. Detection only; see the comment on the
 * definition for why re-attaching to a replacement would be worse than leaving
 * the ledger split. Caller supplies the rate limiting.
 */
void vmem_node_check_identity(void);

/**
 * Open (creating if needed) the sm_node sampling-lock file and return its fd,
 * or -1. The caller keeps the fd for the process lifetime and never closes it
 * -- see SM_NODE_LOCK_PATH for why closing matters on the classic-POSIX-lock
 * fallback path. O_CLOEXEC so an exec'd child does not inherit ownership;
 * fork() still shares the descriptor, which child_after_fork undoes.
 */
int open_sm_node_lock(void);

void malloc_gpu_virt_memory(CUdeviceptr dptr, size_t bytes, int type, int device_id);

/**
 * Record a graph-capture allocation. Same as malloc_gpu_virt_memory() with
 * MEMORY_TYPE_CAPTURE, but ties the record to the capturing graph so
 * free_gpu_virt_memory_by_graph() can retire it at cuStreamEndCapture.
 */
void malloc_gpu_virt_memory_captured(CUdeviceptr dptr, size_t bytes, CUgraph graph, int device_id);

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
