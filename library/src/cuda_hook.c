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

#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <dirent.h>
#include <unistd.h>
#include <time.h>
#include <pthread.h>

#include "include/hook.h"
#include "include/cuda-helper.h"
#include "include/metrics.h"
#include "include/nvml-helper.h"

#define INCREMENT_SCALE_FACTOR   2560
#define MAX_UTIL_DIFF_THRESHOLD  0.5
#define MIN_INCREMENT            5
#define DEVICE_BATCH_SIZE        4

extern resource_data_t* g_vgpu_config;
extern device_util_t* g_device_util;
extern device_vmemory_t* g_device_vmem;

extern entry_t cuda_library_entry[];
extern entry_t nvml_library_entry[];

extern int lock_gpu_device(int device);
extern void unlock_gpu_device(int fd);

extern int device_util_read_lock(int ordinal);
extern void device_util_unlock(int fd, int ordinal);

extern fp_dlsym real_dlsym;
extern void *lib_control;

extern int pid_exist(int pid);
extern int file_exist(const char *);
extern int device_pid_in_same_container(unsigned int pid);
extern int library_exists_in_process_maps(char const *libName, unsigned int pid);
extern int get_container_pids_by_filepath(const char *file_path, int *pids, int *pids_size, int sort_pids);

/* SM throttle controller selection (see util.c). Defaults preserve stock
 * behaviour when env is unset. */
extern int get_sm_controller_kind(int *kind);
extern int get_aimd_md_divisor(double *out);
extern int get_aimd_eff_ratio(int *out);
extern int get_aimd_ai_base_div(int *out);
extern int get_sm_auto_debounce_cycles(int *out);
extern int get_sm_auto_external_util_threshold(int *out);
extern int get_aimd_deadband_ratio(int *out);
extern int get_aimd_md_cooldown_cycles(int *out);
extern int get_usage_threshold(int *out);
extern int get_delta_ramp_floor_divisor(int *out);
extern int get_uva_advise(int *out);
extern int get_sm_shared_bucket(int *out);

/* fork() child handler implemented in loader.c -- re-inits the four
 * library-internal mutexes (g_memory_node_lock, tid_dlsym_lock,
 * device_index_mutex, init_config_mutex) and clears the tid_dlsyms
 * recursion-guard cache. Called from this file's child_after_fork(). */
extern void loader_child_after_fork(void);

static void graph_cost_after_fork(void);

static pthread_once_t g_init_set = PTHREAD_ONCE_INIT;

/* Per-device state that application threads write on *every* kernel launch while
 * SM limiting is on: rate_limiter() CASes cur_cuda_cores, gap_begin() stamps
 * last_launch_ns. Both are gated on the same devices[].core_limit flag, so they
 * are touched together or not at all -- which is why they belong in one line.
 *
 * These used to be two plain int64_t[MAX_DEVICE_COUNT] arrays. At 16 * 8 = 128
 * bytes, devices 0-7 sat in one 64B cache line and 8-15 in the next. An atomic
 * RMW needs *exclusive* ownership of its line, so a thread launching on GPU 0 and
 * a thread launching on GPU 3 -- different devices, different counters, no
 * logical sharing at all -- ping-ponged the same line between cores on every
 * launch. Textbook false sharing, and vgpu-manager does hand one container
 * several GPUs. Giving each device its own aligned slot removes it, and
 * co-locating the two fields makes a launch touch one line instead of two.
 *
 * Padded to 128 rather than 64: Intel's L2 adjacent-line prefetcher pulls lines
 * in 128B-aligned pairs, so 64B padding can still leave two devices effectively
 * sharing, and some ARM64 parts use a 128B granule. 16 * 128 = 2KB total.
 *
 * This does NOT address *true* sharing: N threads on the SAME device still
 * contend on that device's counter, which is inherent to a shared token bucket.
 * Fixing that needs thread-local token batching, tracked separately.
 *
 * CACHELINE_SIZE itself now lives in hook.h: the shared sm_node_dev_t is built
 * on the same granule and is a cross-version ABI, so it must be pinned in one
 * place rather than defined per translation unit.
 */

typedef struct {
  volatile int64_t cur_cuda_cores;  /* token bucket, CAS'd per launch    */
  volatile int64_t last_launch_ns;  /* gap detector, stamped per launch  */
} __attribute__((aligned(CACHELINE_SIZE))) dev_hot_t;

/* Zero-initialized as a static, matching the old `= {0}` arrays. */
static dev_hot_t g_dev_hot[MAX_DEVICE_COUNT];

_Static_assert(sizeof(dev_hot_t) == CACHELINE_SIZE,
               "dev_hot_t must occupy exactly one padded cache line");
_Static_assert(_Alignof(dev_hot_t) == CACHELINE_SIZE,
               "dev_hot_t must be cache-line aligned or false sharing returns");

/* `= {0}` relies on C's rule that any explicit initializer zero-fills the
 * remainder, so these scale automatically with MAX_DEVICE_COUNT. */
static volatile int64_t g_total_cuda_cores[MAX_DEVICE_COUNT] = {0};

/* Set by rate_limiter() (any app thread) whenever it actually throttles a
 * launch (bucket ran negative), read-and-cleared once per cycle by the watcher.
 * Lets the watcher's hard-limit anti-jitter bypass tell "util is low because
 * the workload is idle" (no throttle -> keep the clamp) from "util is low
 * because the clamp is starving a workload that wants more" (throttled -> fall
 * through to the accumulating path so the bucket recovers -- otherwise a
 * small-SM GPU stays pinned near 0% util forever). Cross-thread, so accessed
 * with __atomic; RELAXED suffices -- it is a plain demand flag with no ordering
 * dependency on other state. */
static int32_t g_throttled_since_watch[MAX_DEVICE_COUNT] = {0};

/* ---------------------------------------------------------------------- *
 *  Shared token bucket (CUDA_SM_SHARED_BUCKET)
 *
 *  NULL means "use the per-process state above" -- either the feature is off
 *  (the default) or the region could not be mapped. There is deliberately no
 *  third state: every failure to map degrades to exactly the historical
 *  behaviour, because container-wide SM isolation is an optimisation and must
 *  never be able to stop a container from running.
 *
 *  Set once in initialization() under pthread_once, before any watcher thread
 *  exists and before any launch can reach rate_limiter, then only ever read.
 *  MAP_SHARED survives fork, so a child inherits the same region and joins the
 *  same bucket -- which is precisely what we want, and why nothing in the fork
 *  handlers may reset it (see child_after_fork).
 * ---------------------------------------------------------------------- */
static sm_node_region_t *g_sm_node = NULL;

/* Sampling-ownership lock descriptor, held for the process lifetime and never
 * closed (see SM_NODE_LOCK_PATH). -1 means unavailable, which degrades to
 * every process sampling for itself -- exactly today's behaviour. Declared up
 * here rather than beside its helpers because child_after_fork, far above
 * them, must close it. */
static int g_sm_lock_fd = -1;
/* Per-device: does this process own sampling? Each watcher thread owns a
 * disjoint device slice, so plain ints suffice -- same convention as the
 * exclusivity FSM state. */
static int g_sm_sampling_mine[MAX_DEVICE_COUNT] = {0};
/* This process's previous publish time per device, used only to measure the
 * cadence it publishes. Per-process and reset on fork with the ownership flags,
 * so a child never reports an interval spanning the fork. */
static int64_t g_sm_last_publish_ns[MAX_DEVICE_COUNT] = {0};

/* Identity of the region file we mapped, so we can notice it being replaced.
 *
 * Deleting the region inside the container is not hypothetical -- the mount has
 * to be writable for the library to create the region at all, and a root
 * container can unlink what it can write (a recursive sweep of /tmp does it
 * without meaning to). The mount point itself survives, so this is invisible:
 * `rm -rf` empties the directory and only fails on the mountpoint.
 *
 * Why it matters more than losing a file: processes that already mapped the
 * region keep working, because the mmap holds the inode alive. But the next
 * process to start calls open(O_CREAT), gets a NEW inode, and maps a DIFFERENT
 * region -- so the container ends up with two shared buckets, each refilling on
 * its own last_refill_ns, and the aggregate limit is twice as loose. That is
 * precisely the silent split §4.5.4 rejected unlink+rename to avoid, reintroduced
 * from inside the container. It deserves a log line rather than nothing. */
static ino_t g_sm_node_ino;
static dev_t g_sm_node_dev;
/* CAS'd, not plain-written: unlike every other per-device static in this file,
 * the identity check is global rather than per-device, so the "each watcher
 * thread owns a disjoint host_index slice" invariant that lets those be plain
 * ints does NOT hold here -- several watcher threads reach it concurrently. The
 * stamp doubles as the claim: whoever swaps it does the check, so ino/dev below
 * still have a single writer. */
static int64_t g_sm_ident_checked_ns;
/* Suppresses repeats of the SAME complaint. Written only by the thread that won
 * the stamp CAS. */
static int g_sm_ident_warned;

static inline int64_t monotonic_ns(void);

/* Hot path. One predictable branch, no validation: the region was checked once
 * at map time and is immutable thereafter. Nothing else may be added here --
 * this runs on every kernel launch. */
static inline volatile int64_t *sm_bucket_of(int host_index) {
  return g_sm_node ? (volatile int64_t *)&g_sm_node->devices[host_index].cur_cuda_cores
                   : &g_dev_hot[host_index].cur_cuda_cores;
}

/* Shared, the flag's meaning widens from "did THIS PROCESS throttle" to "did
 * ANYONE in this container throttle" -- which is the question the anti-jitter
 * bypass actually needs to ask once one bucket serves every process. Left
 * per-process, process A could throttle while winner B reads its own zero flag,
 * conclude the workload is idle and clamp the bucket A is starving on. */
static inline int32_t *sm_throttled_of(int host_index) {
  return g_sm_node ? &g_sm_node->devices[host_index].throttled_since_watch
                   : &g_throttled_since_watch[host_index];
}

static int g_sm_num[MAX_DEVICE_COUNT]            = {0};
static int g_max_thread_per_sm[MAX_DEVICE_COUNT] = {0};

/* Block-dim defaults of 1 are set in load_necessary_data(); declaring with
 * {0} here is fine because those paths read them only after initialisation. */
static int g_block_x[MAX_DEVICE_COUNT] = {0};
static int g_block_y[MAX_DEVICE_COUNT] = {0};
static int g_block_z[MAX_DEVICE_COUNT] = {0};
static uint32_t g_block_locker[MAX_DEVICE_COUNT] = {0};

static const struct timespec g_cycle = {
  .tv_sec = 0,
  .tv_nsec = TIME_TICK * MILLISEC,
};

/* ---- GAP-path SM throttle (event-timed duty cycle) -------------------- *
 * A large kernel whose grid fits under the token budget slips past
 * rate_limiter() and runs unthrottled until the NVML watcher reacts ~100ms
 * later. For a launch that follows a >GAP_THRESHOLD_NS idle gap (the
 * synchronize-heavy pattern) we instead time the kernel with cuEvents and
 * inject a duty-cycle sleep. The target percentage follows the watcher's
 * live limit (gap_effective_dc), so this cooperates with both hard_limit and
 * soft (elastic) modes. The whole path is gated by per-device core_limit, so
 * it costs a couple of comparisons when limiting is off.
 * Design: docs/sm_core_limit_gap_throttle_design.md  (esp. §8 on locking). */
#define GAP_THRESHOLD_NS   (200LL * 1000000LL)   /* idle > 200ms => a "gap"   */
#define GAP_MAX_SLEEP_MS   500.0                 /* clamp pathological spikes */

/* Per-device thread mutex: protects the process-private cuEvent pair and the
 * g_gap_dc snapshot from concurrent launcher threads. NOT a cross-process
 * lock by design (see §8) -- the events are process-local CUDA objects. */
static pthread_mutex_t g_gap_lock[MAX_DEVICE_COUNT] = {
  [0 ... MAX_DEVICE_COUNT - 1] = PTHREAD_MUTEX_INITIALIZER,
};
static CUevent          g_gap_start[MAX_DEVICE_COUNT]      = {0};
static CUevent          g_gap_end[MAX_DEVICE_COUNT]        = {0};
static int              g_gap_evt_ready[MAX_DEVICE_COUNT]  = {0};
static int              g_gap_dc[MAX_DEVICE_COUNT]         = {0};

/* Per-device decimation tick for the periodic watcher utilization log.
 * The SM watcher samples each device ~every 80ms (~12 lines/s/device); at
 * VERBOSE that floods the log. Emit only 1 in WATCHER_UTIL_LOG_STRIDE
 * samples (~once per 5s/device) -- same "sample, don't spam" spirit as
 * metrics.c, but fixed-stride rather than power-of-two so a long-running
 * watcher keeps a steady cadence instead of going silent over time.
 * Single-writer per index (each watcher thread owns a disjoint device
 * set), so a plain int needs no atomics. */
#define WATCHER_UTIL_LOG_STRIDE 64
static unsigned int g_util_log_tick[MAX_DEVICE_COUNT] = {0};
/* Separate tick for the watcher main-loop DETAIL line (share/up_limit/curr
 * core). Same cadence and stride as the util log, but its own counter so
 * the two lines decimate independently. Single-writer per index too. */
static unsigned int g_share_log_tick[MAX_DEVICE_COUNT] = {0};

/* ---- fork() child handler ------------------------------------------------ *
 * Python multiprocessing / torch.multiprocessing / subprocess+fork patterns
 * fork() after the parent has already triggered initialization(). Without a
 * handler, the child inherits:
 *
 *   - g_init_set marked as completed -> pthread_once() in launch wrappers
 *     skips initialization() entirely. balance_batches() never re-spawns
 *     the watcher threads (fork() only copies the calling thread), so the
 *     SM controller stops running in the child -- no throttling at all.
 *     Same bug shape as Project-HAMi/HAMi-core PR #199.
 *
 *   - GAP-path cuEvent handles (g_gap_start/end[]) from the parent's CUDA
 *     context, which are invalid in the child's context. g_gap_evt_ready=1
 *     means gap_events_ensure() skips lazy re-creation, so the first
 *     gap_begin() in the child records into a stale handle and fails.
 *
 *   - g_gap_lock[] possibly in the "held" state if a parent thread was
 *     inside the critical section at the instant of fork (the owner thread
 *     vanishes in the child but the mutex bit stays set, so every trylock
 *     in the child returns EBUSY forever).
 *
 * The fix: pthread_atfork(NULL, NULL, child_after_fork) registered lazily
 * from loader.c's load_necessary_data() entry path (which is called by
 * every CUDA/NVML/dlsym hook -- much broader coverage than initialization()
 * which only fires on cuLaunchKernel*). We deliberately do NOT use a
 * constructor attribute -- the build-time check_no_constructors.sh forbids
 * it, since .init_array entries fire during dynamic-linker setup of the
 * host process, in the same window where libvulkan/libGLX/libEGL are being
 * initialized; activity there is a known crash vector (cf. HAMi-core PR
 * #182 Step C).
 *
 * Coverage is complete: any fork in a process that has ever called any
 * vgpu-hooked API has the handler registered; a fork before any hook call
 * inherits an unmodified g_init_set (still PTHREAD_ONCE_INIT) so the child
 * runs initialization() normally on its first launch -- no atfork handler
 * needed for that path (nothing to reset).
 *
 * Made extern (not static) so loader.c's register_atfork_handler can take
 * its address. Linker version script (libvgpu-control.exports.ld) keeps
 * this symbol out of .dynsym so no NVIDIA-ICD/loader collision risk.
 *
 * The child handler reverts every fork-unsafe piece of process-local
 * state we own; the next hook caller then sees g_init_set fresh and
 * re-runs initialization() naturally. Note we intentionally do NOT
 * reset loader.c's g_atfork_init flag: the pthread_atfork registration
 * itself survives fork via COW of glibc's internal atfork handler list,
 * so the child already has the handler; resetting would multiply-register
 * on every fork generation. */
void child_after_fork(void) {
  g_init_set = (pthread_once_t)PTHREAD_ONCE_INIT;
  /* Drop the inherited sampling-lock descriptor. Both failure modes it causes
   * are real and neither is obvious:
   *
   *  - An OFD lock belongs to the open file description, and fork SHARES the
   *    description. So the child inherits ownership of every device the parent
   *    owned. Resetting g_init_set (above) makes the child re-run
   *    initialization(), which respawns its own watcher threads -- so parent
   *    and child would both sample and both consider themselves owner.
   *
   *  - Worse in the other direction: the description stays alive as long as
   *    ANY descriptor references it. A child that keeps this fd open holds the
   *    parent's lock after the parent exits, so no standby can ever take over
   *    and the bucket stops being replenished for the whole container. The
   *    "kernel releases the lock when the holder dies" property that justifies
   *    this whole design silently stops holding.
   *
   * close() here drops only the child's reference: the parent's descriptor
   * keeps the description (and its locks) intact. initialization() then
   * reopens for the child, which contends for ownership normally.
   *
   * close() is async-signal-safe, which a pthread_atfork child handler must be. */
  if (g_sm_lock_fd >= 0) {
    close(g_sm_lock_fd);
    g_sm_lock_fd = -1;
  }
  memset(g_sm_sampling_mine, 0, sizeof(g_sm_sampling_mine));
  memset(g_sm_last_publish_ns, 0, sizeof(g_sm_last_publish_ns));
  for (int i = 0; i < MAX_DEVICE_COUNT; i++) {
    g_gap_evt_ready[i]  = 0;
    g_gap_start[i]      = NULL;
    g_gap_end[i]        = NULL;
    /* last_launch_ns only: the child's idle-gap timing genuinely restarts.
     *
     * Do NOT add cur_cuda_cores here, and do NOT clear g_sm_node. Both fork
     * handlers around this one discard inherited state -- loader_child_after_fork
     * frees the inherited memory ledger, and the lines above drop the parent's
     * CUDA event handles -- so the reflex is to discard the bucket too. That
     * would be wrong. Those are per-process records that mean nothing in the
     * child; the shared bucket is container-wide state that the child JOINS as
     * one more consumer, which is the whole point of sharing it. The MAP_SHARED
     * mapping is inherited intact, and initialization() re-runs here but skips
     * re-mapping precisely because g_sm_node is still set. */
    g_dev_hot[i].last_launch_ns = 0;
    /* Re-init in case a parent thread held the lock at the moment of fork.
     * POSIX leaves the inherited mutex state undefined; glibc re-init on a
     * never-destroyed mutex is safe (no resource leak for a non-robust
     * default mutex) and clears any phantom held-state. */
    pthread_mutex_init(&g_gap_lock[i], NULL);
  }
  /* Same held-at-fork hazard for the graph cost cache lock; parent CUgraph
   * Exec handles are also invalid in the child (CUDA contexts don't survive
   * fork) so the helper also wipes the cache to prevent stale-pointer
   * collisions with newly allocated execs that happen to hit the same
   * address. Forward-declared because the cache statics are defined later
   * (near the CUDA Graph hooks) to keep that subsystem co-located. */
  graph_cost_after_fork();
  /* Loader-level mutexes have the exact same held-at-fork hazard --
   * delegated since those mutexes are file-scope static in loader.c. The
   * most dangerous one is init_config_mutex (taken from load_necessary_data
   * on every launch hook entry); the others (tid_dlsym_lock, device_index_
   * mutex, g_memory_node_lock) are also hot enough to warrant the reset. */
  loader_child_after_fork();
}

typedef enum {
  MEMORY_PATH_GPU = 0,
  MEMORY_PATH_UVA = 1,
  MEMORY_PATH_OOM = 2,
} memory_path_t;

static void get_used_gpu_memory(void *, CUdevice);
static size_t get_array_base_size(int format);
static int load_limited_memory_view(CUdevice device, int *host_index,
                                    int *lock_fd, size_t *used,
                                    size_t *vmem_used);

static memory_path_t prepare_memory_allocation(CUdevice device,
                                               size_t request_size,
                                               int allow_uva,
                                               int *host_index,
                                               int *lock_fd) {
  size_t used = 0;
  size_t vmem_used = 0;

  if (!load_limited_memory_view(device, host_index, lock_fd, &used, &vmem_used)) {
    return MEMORY_PATH_GPU;
  }

  /* One tear-free snapshot for the whole decision: total_memory, memory_oversold
   * and real_memory must be read as a consistent set or the OOM/UVA routing can
   * mix a new total with an old oversold flag. host_index is valid here --
   * load_limited_memory_view returned non-zero. */
  device_t d = get_device_snapshot(*host_index);
  if ((used + vmem_used + request_size) > d.total_memory) {
    // It is necessary to check and clean up virtual memory usage
    if (vmem_used > 0 && (used + request_size) <= d.total_memory) {
      check_cleanup_vmem_nodes_by_device(*host_index);
      get_used_gpu_virt_memory((void *)&vmem_used, *host_index);
    }
    if ((used + vmem_used + request_size) > d.total_memory) {
      return MEMORY_PATH_OOM;
    }
  }

  if (allow_uva && d.memory_oversold && (used + request_size) > d.real_memory) {
    /* Recorded here rather than at the call sites because this is the only
     * place that holds the numbers describing the decision: what was asked
     * for, what the device already holds, and the physical ceiling being
     * exceeded. The call sites see only the chosen path. */
    metrics_record_uva_oversold(*host_index, (uint64_t)request_size, (uint64_t)used,
                                (uint64_t)g_vgpu_config->devices[*host_index].real_memory);
    return MEMORY_PATH_UVA;
  }

  return MEMORY_PATH_GPU;
}

static int load_limited_memory_view(CUdevice device,
                                    int *host_index,
                                    int *lock_fd,
                                    size_t *used,
                                    size_t *vmem_used) {
  *lock_fd = -1;
  *host_index = get_host_device_index_by_cuda_device(device);
  if (*host_index < 0) {
    return 0;
  }
  if (!get_device_flag(*host_index, memory_limit)) {
    return 0;
  }

  *lock_fd = lock_gpu_device(*host_index);
  get_used_gpu_memory((void *)used, device);
  get_used_gpu_virt_memory((void *)vmem_used, *host_index);
  return 1;
}

static size_t get_array_request_size(const CUDA_ARRAY_DESCRIPTOR *desc) {
  size_t base_size = get_array_base_size(desc->Format);
  return base_size * desc->NumChannels * desc->Height * desc->Width;
}

static size_t get_array3d_request_size(const CUDA_ARRAY3D_DESCRIPTOR *desc) {
  size_t base_size = get_array_base_size(desc->Format);
  return base_size * desc->NumChannels * desc->Height * desc->Width * desc->Depth;
}

/** internal function definition */

static void active_utilization_notifier(int);

static void *utilization_watcher(void *);

static nvmlReturn_t get_process_utilization_samples(nvmlDevice_t, nvmlProcessUtilizationSample_t *, unsigned int *, uint64_t);

static nvmlReturn_t get_gpu_process_from_external_watcher(utilization_t *, nvmlProcessUtilizationSample_t *, unsigned int *, int, int, nvmlDevice_t);

static nvmlReturn_t get_gpu_process_from_local_nvml_driver(utilization_t *, nvmlProcessUtilizationSample_t *, unsigned int *, int, nvmlDevice_t);

static void get_used_gpu_utilization(void *, int, int, nvmlDevice_t);

static void init_device_cuda_cores(int *device_count);

static void initialization();

static void rate_limiter(int grids, int blocks, int host_index);

static void change_token(int64_t, int);

static int64_t delta(int up_limit, int user_current, int64_t share, int host_index);

/** export function definition */
CUresult cuDriverGetVersion(int *driverVersion);
CUresult cuInit(unsigned int flag);
CUresult cuGetProcAddress(const char *symbol, void **pfn, int cudaVersion,
                          cuuint64_t flags);
CUresult cuGetProcAddress_v2(const char *symbol, void **pfn, int cudaVersion,
                          cuuint64_t flags, void *symbolStatus);           
CUresult cuMemAllocManaged(CUdeviceptr *dptr, size_t bytesize,
                           unsigned int flags);
CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytesize);
CUresult cuMemAlloc(CUdeviceptr *dptr, size_t bytesize);
CUresult cuMemAllocPitch_v2(CUdeviceptr *dptr, size_t *pPitch,
                            size_t WidthInBytes, size_t Height,
                            unsigned int ElementSizeBytes);
CUresult cuMemAllocPitch(CUdeviceptr *dptr, size_t *pPitch, size_t WidthInBytes,
                         size_t Height, unsigned int ElementSizeBytes);
CUresult cuArrayCreate_v2(CUarray *pHandle,
                          const CUDA_ARRAY_DESCRIPTOR *pAllocateArray);
CUresult cuArrayCreate(CUarray *pHandle,
                       const CUDA_ARRAY_DESCRIPTOR *pAllocateArray);
CUresult cuArray3DCreate_v2(CUarray *pHandle,
                            const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray);
CUresult cuArray3DCreate(CUarray *pHandle,
                         const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray);
CUresult cuMipmappedArrayCreate(CUmipmappedArray *pHandle,
                       const CUDA_ARRAY3D_DESCRIPTOR *pMipmappedArrayDesc,
                       unsigned int numMipmapLevels);
CUresult cuDeviceTotalMem_v2(size_t *bytes, CUdevice dev);
CUresult cuDeviceTotalMem(size_t *bytes, CUdevice dev);
CUresult cuMemGetInfo_v2(size_t *free, size_t *total);
CUresult cuMemGetInfo(size_t *free, size_t *total);
CUresult cuLaunchKernel_ptsz(CUfunction f, unsigned int gridDimX,
                        unsigned int gridDimY, unsigned int gridDimZ,
                        unsigned int blockDimX, unsigned int blockDimY,
                        unsigned int blockDimZ, unsigned int sharedMemBytes,
                        CUstream hStream, void **kernelParams, void **extra);
CUresult cuLaunchKernel(CUfunction f, unsigned int gridDimX,
                        unsigned int gridDimY, unsigned int gridDimZ,
                        unsigned int blockDimX, unsigned int blockDimY,
                        unsigned int blockDimZ, unsigned int sharedMemBytes,
                        CUstream hStream, void **kernelParams, void **extra);
CUresult cuLaunchKernelEx(CUlaunchConfig *config, CUfunction f,
                        void **kernelParams, void **extra);
CUresult cuLaunchKernelEx_ptsz(CUlaunchConfig *config, CUfunction f, 
                        void **kernelParams, void **extra);
CUresult cuLaunch(CUfunction f);
CUresult cuLaunchCooperativeKernel_ptsz(CUfunction f, unsigned int gridDimX, 
                                  unsigned int gridDimY, unsigned int gridDimZ, 
                                  unsigned int blockDimX, unsigned int blockDimY,
                                  unsigned int blockDimZ, unsigned int sharedMemBytes, 
                                  CUstream hStream, void **kernelParams);
CUresult cuLaunchCooperativeKernel(CUfunction f, unsigned int gridDimX,
                                  unsigned int gridDimY, unsigned int gridDimZ,
                                  unsigned int blockDimX, unsigned int blockDimY,
                                  unsigned int blockDimZ, unsigned int sharedMemBytes,
                                  CUstream hStream, void **kernelParams);
CUresult cuLaunchGrid(CUfunction f, int grid_width, int grid_height);
CUresult cuLaunchGridAsync(CUfunction f, int grid_width, int grid_height,
                           CUstream hStream);
CUresult cuLaunchCooperativeKernelMultiDevice(CUDA_LAUNCH_PARAMS *launchParamsList,
                                              unsigned int numDevices,
                                              unsigned int flags);
CUresult cuGraphInstantiate(CUgraphExec *phGraphExec, CUgraph hGraph,
                            CUgraphNode *phErrorNode, char *logBuffer,
                            size_t bufferSize);
CUresult cuGraphInstantiate_v2(CUgraphExec *phGraphExec, CUgraph hGraph,
                               CUgraphNode *phErrorNode, char *logBuffer,
                               size_t bufferSize);
CUresult cuGraphInstantiateWithFlags(CUgraphExec *phGraphExec, CUgraph hGraph,
                                     unsigned long long flags);
CUresult cuGraphInstantiateWithParams(CUgraphExec *phGraphExec, CUgraph hGraph,
                                      CUDA_GRAPH_INSTANTIATE_PARAMS *instantiateParams);
CUresult cuGraphInstantiateWithParams_ptsz(CUgraphExec *phGraphExec, CUgraph hGraph,
                                           CUDA_GRAPH_INSTANTIATE_PARAMS *instantiateParams);
CUresult cuGraphLaunch(CUgraphExec hGraphExec, CUstream hStream);
CUresult cuGraphLaunch_ptsz(CUgraphExec hGraphExec, CUstream hStream);
CUresult cuGraphExecDestroy(CUgraphExec hGraphExec);
CUresult cuGraphExecUpdate(CUgraphExec hGraphExec, CUgraph hGraph,
                           CUgraphNode *hErrorNode_out,
                           CUgraphExecUpdateResult *updateResult_out);
CUresult cuGraphExecUpdate_v2(CUgraphExec hGraphExec, CUgraph hGraph,
                              CUgraphExecUpdateResultInfo *resultInfo);
CUresult cuFuncSetBlockShape(CUfunction hfunc, int x, int y, int z);
CUresult cuMemAllocAsync(CUdeviceptr *dptr, size_t bytesize, CUstream hStream);
CUresult cuMemAllocAsync_ptsz(CUdeviceptr *dptr, size_t bytesize, CUstream hStream);
CUresult cuMemCreate(CUmemGenericAllocationHandle *handle, size_t size,
                     const CUmemAllocationProp *prop, unsigned long long flags);
CUresult cuMemAllocFromPoolAsync(CUdeviceptr *dptr, size_t bytesize,
                                 CUmemoryPool pool, CUstream hStream);
CUresult cuMemAllocFromPoolAsync_ptsz(CUdeviceptr *dptr, size_t bytesize,
                                 CUmemoryPool pool, CUstream hStream);
CUresult cuMemFree_v2(CUdeviceptr dptr);
CUresult cuMemFree(CUdeviceptr dptr);
CUresult cuMemFreeAsync(CUdeviceptr dptr, CUstream hStream);
CUresult cuMemFreeAsync_ptsz(CUdeviceptr dptr, CUstream hStream);
CUresult cuStreamEndCapture(CUstream hStream, CUgraph *phGraph);
CUresult cuStreamEndCapture_ptsz(CUstream hStream, CUgraph *phGraph);
CUresult cuGraphDestroy(CUgraph hGraph);
CUresult cuMemHostRegister(void *p, size_t bytesize, unsigned int Flags);
CUresult cuMemHostRegister_v2(void *p, size_t bytesize, unsigned int Flags);
CUresult cuMemHostAlloc(void **pp, size_t bytesize, unsigned int Flags);

entry_t cuda_hooks_entry[] = {
    {.name = "cuDriverGetVersion", .fn_ptr = cuDriverGetVersion},
    {.name = "cuInit", .fn_ptr = cuInit},
    {.name = "cuGetProcAddress", .fn_ptr = cuGetProcAddress},
    {.name = "cuGetProcAddress_v2", .fn_ptr = cuGetProcAddress_v2},
    {.name = "cuMemAllocManaged", .fn_ptr = cuMemAllocManaged},
    {.name = "cuMemAlloc_v2", .fn_ptr = cuMemAlloc_v2},
    {.name = "cuMemAlloc", .fn_ptr = cuMemAlloc},
    {.name = "cuMemAllocPitch_v2", .fn_ptr = cuMemAllocPitch_v2},
    {.name = "cuMemAllocPitch", .fn_ptr = cuMemAllocPitch},
    {.name = "cuArrayCreate_v2", .fn_ptr = cuArrayCreate_v2},
    {.name = "cuArrayCreate", .fn_ptr = cuArrayCreate},
    {.name = "cuArray3DCreate_v2", .fn_ptr = cuArray3DCreate_v2},
    {.name = "cuArray3DCreate", .fn_ptr = cuArray3DCreate},
    {.name = "cuMipmappedArrayCreate", .fn_ptr = cuMipmappedArrayCreate},
    {.name = "cuDeviceTotalMem_v2", .fn_ptr = cuDeviceTotalMem_v2},
    {.name = "cuDeviceTotalMem", .fn_ptr = cuDeviceTotalMem},
    {.name = "cuMemGetInfo_v2", .fn_ptr = cuMemGetInfo_v2},
    {.name = "cuMemGetInfo", .fn_ptr = cuMemGetInfo},
    {.name = "cuLaunchKernel_ptsz", .fn_ptr = cuLaunchKernel_ptsz},
    {.name = "cuLaunchKernel", .fn_ptr = cuLaunchKernel},
    {.name = "cuLaunchKernelEx_ptsz", .fn_ptr = cuLaunchKernelEx_ptsz},
    {.name = "cuLaunchKernelEx", .fn_ptr = cuLaunchKernelEx},
    {.name = "cuLaunch", .fn_ptr = cuLaunch},
    {.name = "cuLaunchCooperativeKernel_ptsz", .fn_ptr = cuLaunchCooperativeKernel_ptsz},
    {.name = "cuLaunchCooperativeKernel", .fn_ptr = cuLaunchCooperativeKernel},
    {.name = "cuLaunchGrid", .fn_ptr = cuLaunchGrid},
    {.name = "cuLaunchGridAsync", .fn_ptr = cuLaunchGridAsync},
    {.name = "cuLaunchCooperativeKernelMultiDevice", .fn_ptr = cuLaunchCooperativeKernelMultiDevice},
    {.name = "cuGraphInstantiate", .fn_ptr = cuGraphInstantiate},
    {.name = "cuGraphInstantiate_v2", .fn_ptr = cuGraphInstantiate_v2},
    {.name = "cuGraphInstantiateWithFlags", .fn_ptr = cuGraphInstantiateWithFlags},
    {.name = "cuGraphInstantiateWithParams", .fn_ptr = cuGraphInstantiateWithParams},
    {.name = "cuGraphInstantiateWithParams_ptsz", .fn_ptr = cuGraphInstantiateWithParams_ptsz},
    {.name = "cuGraphLaunch", .fn_ptr = cuGraphLaunch},
    {.name = "cuGraphLaunch_ptsz", .fn_ptr = cuGraphLaunch_ptsz},
    {.name = "cuGraphExecDestroy", .fn_ptr = cuGraphExecDestroy},
    {.name = "cuGraphExecUpdate", .fn_ptr = cuGraphExecUpdate},
    {.name = "cuGraphExecUpdate_v2", .fn_ptr = cuGraphExecUpdate_v2},
    {.name = "cuFuncSetBlockShape", .fn_ptr = cuFuncSetBlockShape},
    {.name = "cuMemAllocAsync", .fn_ptr = cuMemAllocAsync},
    {.name = "cuMemAllocAsync_ptsz", .fn_ptr = cuMemAllocAsync_ptsz},
    {.name = "cuMemCreate", .fn_ptr = cuMemCreate},
    {.name = "cuMemAllocFromPoolAsync", .fn_ptr = cuMemAllocFromPoolAsync},
    {.name = "cuMemAllocFromPoolAsync_ptsz", .fn_ptr = cuMemAllocFromPoolAsync_ptsz},
    {.name = "cuMemFree_v2", .fn_ptr = cuMemFree_v2},
    {.name = "cuMemFree", .fn_ptr = cuMemFree},
    {.name = "cuMemFreeAsync", .fn_ptr = cuMemFreeAsync},
    {.name = "cuMemFreeAsync_ptsz", .fn_ptr = cuMemFreeAsync_ptsz},
    {.name = "cuStreamEndCapture", .fn_ptr = cuStreamEndCapture},
    {.name = "cuStreamEndCapture_ptsz", .fn_ptr = cuStreamEndCapture_ptsz},
    {.name = "cuGraphDestroy", .fn_ptr = cuGraphDestroy},
    {.name = "cuMemHostRegister", .fn_ptr = cuMemHostRegister},
    {.name = "cuMemHostRegister_v2", .fn_ptr = cuMemHostRegister_v2},
    {.name = "cuMemHostAlloc", .fn_ptr = cuMemHostAlloc},
};

const int cuda_hook_nums =
    sizeof(cuda_hooks_entry) / sizeof(cuda_hooks_entry[0]);

/* SOFT_ADJUST_INTERVAL: how many watcher cycles (~80ms each) elapse
 * between successive periodic up_limit re-evaluations in soft mode.
 * Previously dynamic_config_t.change_limit_interval; promoted to a
 * compile-time constant because (1) no consumer ever wanted to tune
 * it, (2) it appears in the avg_sys_frees denominator and the every-
 * other-cycle reset condition (i % (INTERVAL/2)), so user-facing
 * tuning would need to expose two coupled knobs to be safe.
 * 30 cycles ~ 2.4s at the default ~80ms cadence.  */
#define SOFT_ADJUST_INTERVAL    30

/* DELTA_ERROR_RECOVERY_STEP: fallback share-increment value if the
 * delta() controller computes an out-of-range increment (negative or
 * > INT_MAX, both indicating an overflow path). Previously
 * dynamic_config_t.error_recovery_step; promoted to a constant
 * because it is purely defensive and irrelevant to behaviour outside
 * the overflow path. */
#define DELTA_ERROR_RECOVERY_STEP   10

/* delta()'s ramp-floor divisor is env-tunable via g_dynamic_config
 * .delta_ramp_floor_divisor (CUDA_SM_DELTA_RAMP_FLOOR_DIVISOR, default 64);
 * see the block comment at the use site in delta() and the loader in
 * sm_controller_init(). */

/* Defaults match the prior file-static initialisers; each field is
 * re-loaded from its env in sm_controller_init() under pthread_once.
 * The initialiser exists so a read before init still produces sane
 * behaviour (mostly relevant if a CUDA-side caller races with very
 * early library load -- the watcher proper does not start until after
 * init). */
dynamic_config_t g_dynamic_config = {
  .usage_threshold              = 5,
  .sm_controller_kind           = 0,     /* delta */
  .aimd_md_divisor              = 3.0,
  .aimd_eff_ratio               = 875,
  .aimd_ai_base_div             = 400,
  .aimd_deadband_ratio          = 800,
  .aimd_md_cooldown_cycles      = 3,
  .auto_debounce_cycles         = 10,
  .auto_external_util_threshold = 1,
  .delta_ramp_floor_divisor     = 64,
  .sm_shared_bucket             = 0,     /* per-process bucket (historical) */
};

/* The ceiling stays the per-process g_total_cuda_cores[]: it is derived from
 * device properties, so every process computes the same number, and reading it
 * locally removes any ordering dependency on the shared region being populated
 * (a transiently-zero shared ceiling would clamp the bucket to 0 and throttle
 * the whole container). The region's total_cuda_cores is observability only. */
static void change_token(int64_t delta, int host_index) {
  int64_t cuda_cores_before = 0, cuda_cores_after = 0;
  volatile int64_t *bucket = sm_bucket_of(host_index);

  LOGGER(DETAIL, "host device: %d, delta: %ld, curr: %ld", host_index, delta, *bucket);
  do {
    cuda_cores_before = *bucket;
    cuda_cores_after = cuda_cores_before + delta;

    if (unlikely(cuda_cores_after > g_total_cuda_cores[host_index])) {
      cuda_cores_after = g_total_cuda_cores[host_index];
    } else if (unlikely(cuda_cores_after < 0)) {
      cuda_cores_after = 0;
    }
  } while (!CAS(bucket, cuda_cores_before, cuda_cores_after));
}

static void rate_limiter(int grids, int blocks, int host_index) {
  (void)blocks;
  if (host_index < 0) {
    return;
  }
  if (get_device_flag(host_index, core_limit)) {
    int64_t before_cuda_cores = 0;
    int64_t after_cuda_cores = 0;
    int64_t kernel_size = (int64_t) grids;
    /* Resolved once per launch, not per CAS retry. With the shared bucket on,
     * this is the ONLY change the consumer side needs: a CAS is a CPU
     * instruction and does not care whether the word is process-private or in
     * MAP_SHARED memory, so N processes CASing one counter serialise into
     * physically correct deductions with no lock and no extra instruction. */
    volatile int64_t *bucket = sm_bucket_of(host_index);

    do {
    CHECK:
      before_cuda_cores = *bucket;
      if (before_cuda_cores < 0) {
        metrics_record_rate_limit_hit(host_index);
        /* Signal the watcher that this workload wanted more than its current
         * bucket allowed -- this is genuine demand, not idleness. Slow path
         * (we are about to sleep), so the atomic store is free. */
        __atomic_store_n(sm_throttled_of(host_index), 1, __ATOMIC_RELAXED);
        nanosleep(&g_cycle, NULL);
        goto CHECK;
      }
      after_cuda_cores = before_cuda_cores - kernel_size;
    } while (!CAS(bucket, before_cuda_cores, after_cuda_cores));
  }
}

static int64_t delta(int up_limit, int user_current, int64_t share, int host_index) {
  // 1. Using wider data types to prevent computation overflow
  int64_t sm_num = (int64_t)g_sm_num[host_index];
  int64_t max_thread = (int64_t)g_max_thread_per_sm[host_index];

  // 2. Calculate the difference in utilization rate
  int utilization_diff = abs(up_limit - user_current);
  if (utilization_diff < MIN_INCREMENT) {
    utilization_diff = MIN_INCREMENT;
  }

  // 3. Calculate increment (using 64 bit operation to prevent overflow)
  int64_t increment = sm_num * sm_num * max_thread * (int64_t)(utilization_diff) / INCREMENT_SCALE_FACTOR;

  // 4. Accelerate adjustment logic (using floating-point thresholds instead of hard coding)
  if ((float)utilization_diff / (float)(up_limit) > MAX_UTIL_DIFF_THRESHOLD) {
    increment = increment * utilization_diff * 2 / (up_limit + 1);
  }

  // 5. Error handling optimization: When the increment is negative,
  //    the process is no longer terminated, but rolled back to a safe value
  if (unlikely(increment < 0 || increment > INT_MAX)) {
    LOGGER(ERROR, "host device %d, increment overflow: %ld, current sm: %ld, thread_per_sm: %ld, diff: %d",
           host_index, increment, sm_num, max_thread, utilization_diff);
    increment = DELTA_ERROR_RECOVERY_STEP;
  }

  /* Ramp-speed floor, applied SYMMETRICALLY (before the grow/cut split) and
   * scaled by the distance from the setpoint. `increment` is sm^2-scaled while
   * the share must travel ~g_total (∝ sm) to track the limit, so the raw step
   * ∝ 1/sm -- on small-SM GPUs / MIG slices the ramp and the cut-back on an
   * overshoot both crawl for minutes.
   *
   * The floor is g_total * diff / (up_limit * DIVISOR): at cold start (diff ==
   * up_limit) it is g_total/DIVISOR so the bulk ramp completes in ~DIVISOR
   * cycles regardless of SM count, and it shrinks to ~0 as util approaches the
   * limit so the fine control near the setpoint reverts to delta's proportional
   * step -- keeping the tight limit tracking that large GPUs already had (a flat
   * floor would coarsen it to +/- g_total/DIVISOR everywhere). Symmetric so it
   * cannot ratchet: flooring only grow made grow >> cut and pushed util far past
   * the limit (observed: hard_core=8 pinned at 15, hard_core=50 at 65-89). Uses
   * the MIN_INCREMENT-floored utilization_diff, which conveniently keeps a small
   * residual floor near the setpoint on tiny slices (where even the near-limit
   * raw step is too small) while staying below the raw step on large GPUs.
   *
   * divisor <= 0 is the "disable" sentinel (CUDA_SM_DELTA_RAMP_FLOOR_DIVISOR set
   * to 0 or less): skip the floor -- and its division -- so delta uses its raw
   * sm^2-scaled step, exactly the pre-floor behaviour. */
  int ramp_divisor = g_dynamic_config.delta_ramp_floor_divisor;
  if (ramp_divisor > 0) {
    int64_t floor_up_limit = up_limit > 0 ? up_limit : 1; /* guard integer div-by-zero */
    int64_t ramp_floor = g_total_cuda_cores[host_index] * (int64_t)utilization_diff
                         / (floor_up_limit * ramp_divisor);
    if (increment < ramp_floor) {
      increment = ramp_floor;
    }
  }
  if (user_current <= up_limit) {
    share = (share + increment) > g_total_cuda_cores[host_index] ?
            g_total_cuda_cores[host_index] : (share + increment);
  } else {
    share = (share - increment) < 0 ? 0 : (share - increment);
  }

  return share;
}

/* ====================================================================== *
 *  Pluggable SM throttle controller                                       *
 *                                                                         *
 *  The per-cycle share update in the watcher loop is delegated to a       *
 *  controller selected once at init by CUDA_SM_CONTROLLER:                *
 *                                                                         *
 *    "delta" (default)  -- the stock symmetric proportional controller    *
 *                          (function `delta` above), keeps behaviour      *
 *                          identical to the pre-AIMD build.               *
 *    "aimd"             -- additive-increase / multiplicative-decrease,   *
 *                          ported from midokura/HAMi-core branch          *
 *                          ablation/orig-aimd-v5 (Stock MAE ~20% ->       *
 *                          AIMD MAE ~3% on RTX 4080 measurements).        *
 *                                                                         *
 *  The controller is a drop-in for delta(): same signature, same call     *
 *  sites, no change to the watcher's soft/hard mode logic or to the      *
 *  GAP path (which reads up_limits[], not shares[]).                      *
 * ====================================================================== */

typedef int64_t (*sm_controller_fn)(int up_limit, int user_current,
                                    int64_t share, int host_index);

enum {
  SM_CONTROLLER_DELTA = 0,
  SM_CONTROLLER_AIMD  = 1,
  SM_CONTROLLER_AUTO  = 2,   /* experimental: route per device per cycle */
};

/* All SM-controller tunables now live in g_dynamic_config (hook.h).
 * The 8 file-static globals that used to mirror them were removed in
 * favour of direct reads, so there is exactly one source of truth and
 * the boot log can dump them in a single line. The non-tunable
 * per-device cooldown counter stays local because it is RUNTIME STATE,
 * not configuration, and changes every watcher cycle. */

/* Per-device remaining cooldown counter. Watcher-thread-only access
 * (each watcher thread owns a disjoint host_index slice via
 * balance_batches; auto_routed_controller runs in the same watcher
 * thread). No volatile / atomics needed. fork() safety: child watcher
 * restarts and the static-zero initializer applies on first dispatch. */
static int g_aimd_md_cooldown[MAX_DEVICE_COUNT] = {0};

/* ---- Shared exclusivity FSM (V2.1) -------------------------------------- *
 * The watcher's "are we exclusively using this device" decision is needed
 * in three places:
 *
 *   1. soft_core burst gate         (watcher main loop, soft mode)
 *   2. hard_limit jitter-init gate  (watcher main loop, hard_limit mode)
 *   3. AUTO controller routing      (auto_routed_controller via g_sm_controller)
 *
 * Pre-V2.1 these called host_index_is_exclusive() (raw, no smoothing),
 * which caused two problems:
 *   - soft_core burst could be triggered by a single-cycle dip in
 *     external util (NVML jitter / external Pod inter-batch idle),
 *     wrongly grabbing soft_core and squeezing the real competing Pod.
 *   - AUTO routing kept its own private FSM (g_auto_current /
 *     g_auto_pending_streak) duplicating the same debounce idea, with
 *     a separate code path that could drift from the watcher's view.
 *
 * V2.1 introduces a SINGLE debounced FSM shared by all three callers:
 *
 *   raw       observation        -> host_index_is_exclusive_raw()
 *   debounced (state machine)    -> host_index_is_exclusive() (was renamed,
 *                                   keeps same public-call shape)
 *
 * The debounced FSM is updated at most ONCE per watcher cycle via a memo
 * (g_excl_memo_cycle), so when the watcher main loop calls the predicate
 * twice in the same iteration (jitter-init then burst), the second call
 * is O(1) and the streak counter doesn't double-update.
 *
 * Initial value g_is_exclusive_debounced[i] = 0 ("not exclusive") is
 * intentionally conservative: at startup we wait g_dynamic_config.auto_debounce_cycles
 * (~400ms) of observed exclusivity before allowing soft_core burst. The
 * cost is at most ~400ms of slower ramp-up; the benefit is never
 * accidentally squeezing an external Pod that briefly happened to be at
 * 0% util during our first watcher cycles.
 *
 * Threading: every field below is written and read exclusively by the
 * watcher thread that owns the corresponding host_index (each watcher
 * thread owns a disjoint slice via balance_batches()). No cross-thread
 * read; no volatile needed. fork() safety: see child_after_fork() --
 * post-fork the watcher restarts and the file-scope zero-init defaults
 * apply on its next entry to the main loop. */
static int      g_is_exclusive_debounced[MAX_DEVICE_COUNT] = {0};
static int      g_exclusive_pending_streak[MAX_DEVICE_COUNT] = {0};
/* One-shot flag set by the debounced FSM when it flips true->false; the
 * watcher main loop consumes it to perform a "lost exclusivity" reset
 * (down to hard_core) on the next cycle. */
static int      g_lost_exclusivity_pending[MAX_DEVICE_COUNT] = {0};
/* Per-cycle memoization so multiple calls in the same watcher iteration
 * don't double-advance the streak. The watcher main loop sets memo_valid=0
 * at the top of each per-device iteration; the first call to the
 * debounced predicate that cycle advances the FSM and stores its answer +
 * sets memo_valid=1; subsequent calls return the memoized value. */
static int      g_excl_memo_valid[MAX_DEVICE_COUNT] = {0};
static int      g_excl_memo_value[MAX_DEVICE_COUNT] = {0};

static int64_t aimd_controller(int up_limit, int user_current,
                               int64_t share, int host_index);

/* Function pointer set at init. Stays at delta if env is unset, so an
 * uninitialised call before sm_controller_init() runs is still safe. */
static sm_controller_fn g_sm_controller = delta;

/* AIMD v5: additive increase (gap-proportional) when under the buffered
 * limit, fast multiplicative cut otherwise. Asymmetric response is what
 * pulls down the steady-state MAE compared to delta()'s symmetric step.
 * Algorithm shape matches midokura's ablation; constants are env-tunable
 * because the original was calibrated on RTX 4080 and big datacenter
 * GPUs (A100/H100) likely need a less aggressive MD factor.
 *
 * Two fidelity fixes vs. the initial port (had broken small-hard_core
 * cases -- utilization stuck at 0-1 against hard_core=5 because share
 * could not recover after MD):
 *
 *   - base now multiplies by 3 to match Midokura. The original port
 *     omitted this and ran with 1/3 the AI step.
 *   - ai_step floor: after MD divides share by g_dynamic_config.aimd_md_divisor (default
 *     3), the next cycle could be left with a share so small that AI
 *     would need many cycles to recover. Midokura clamps `if (share <
 *     ai_step) share = ai_step;` so MD only ever cuts to a "current AI
 *     unit" floor, keeping utilization tracking the target instead of
 *     drifting to a fraction of it. This floor was the load-bearing
 *     piece in their small-setpoint behaviour. */
static int64_t aimd_controller(int up_limit, int user_current,
                               int64_t share, int host_index) {
  /* Three regions around the target (P1 hysteresis, see
   * sm_controller_aimd_sawtooth_analysis.md):
   *
   *      user_current
   *   0 +------ AI ------+---- deadband ----+------ MD ------+ 100
   *                  deadband_low        eff_limit
   *
   *   AI region        : share += gap-proportional step (grow)
   *   deadband region  : hold share (kills sawtooth)
   *   MD region        : share /= md_divisor, then arm cooldown (cut)
   *
   * Defaults: deadband_ratio=800 (low=80% of up_limit), eff_ratio=875
   * (high=87.5%); so deadband width = 7.5% of up_limit (scales with
   * setpoint, no manual tuning per workload).
   *
   * sm_controller_init clamps deadband_ratio < eff_ratio so deadband_low
   * < eff_limit always; we still guard at runtime as a defence in depth
   * (g_total_cuda_cores etc. live in shared memory and could theoretically
   * be perturbed by future code). */
  int eff_limit = (int)((int64_t)up_limit * g_dynamic_config.aimd_eff_ratio / 1000);
  if (eff_limit < 1) eff_limit = 1;
  int deadband_low = (int)((int64_t)up_limit * g_dynamic_config.aimd_deadband_ratio / 1000);
  if (deadband_low < 0) deadband_low = 0;
  if (deadband_low >= eff_limit) deadband_low = eff_limit - 1;   /* defence */

  int64_t sm_num     = (int64_t)g_sm_num[host_index];
  int64_t max_thread = (int64_t)g_max_thread_per_sm[host_index];
  /* base * 3: matches Midokura's `g_sm_num * g_max_thread_per_sm * 3`.
   * ai_step is computed unconditionally because we need it for both the
   * AI step itself and the post-MD floor below. */
  int64_t ai_step = sm_num * max_thread * 3 * (int64_t)eff_limit
                    / (int64_t)g_dynamic_config.aimd_ai_base_div;
  if (ai_step < 1) ai_step = 1;

  /* Cooldown drains every cycle (TCP Reno time-based semantics, not
   * event-based: an AI run shouldn't keep a stale cooldown alive). The
   * counter is bumped to (cycles + 1) when MD fires below, then this
   * decrement immediately knocks it down to `cycles`, so the post-MD
   * cycles 1..N all see counter > 0 and stay blocked, then cycle N+1
   * sees 0 and is free to MD again. This +1/decrement-first dance is
   * the cheapest way to make "cooldown=N" gate exactly N post-MD
   * cycles with a single integer of state. */
  if (g_aimd_md_cooldown[host_index] > 0) {
    g_aimd_md_cooldown[host_index]--;
  }

  if (user_current < deadband_low) {
    /* AI: gap-proportional step (gap >= 5 floor to keep moving when very
     * close to target). */
    int gap = up_limit - user_current;
    if (gap < 5) gap = 5;
    int64_t step = ai_step * (int64_t)gap / 5;
    share += step;
    if (share > g_total_cuda_cores[host_index]) {
      share = g_total_cuda_cores[host_index];
    }
  } else if (user_current > eff_limit) {
    /* MD region. Honour cooldown: a cycle right after an MD must NOT
     * MD again, even if util still over-shoots -- NVML's ~80ms sample
     * + share-take-effect lag (~200-400ms total) means consecutive
     * MD cuts share by md_divisor^N before the first cut's effect
     * surfaces, hence "MD avalanche". Cooldown breaks the chain. */
    if (g_aimd_md_cooldown[host_index] == 0) {
      /* md_divisor is a double so users can pick 1.5 etc for softer cuts.
       * share is int64_t; cast through double for the divide, then floor
       * back to int64. share never exceeds g_total_cuda_cores (a few
       * million), far below double's 2^53 mantissa limit, so no
       * precision loss. md_divisor is clamped >= 1.01 at load time so
       * division by ~zero is impossible. */
      share = (int64_t)((double)share / g_dynamic_config.aimd_md_divisor);
      if (share < 0) share = 0;
      /* +1 because the decrement at the top of NEXT cycle takes us
       * from cycles+1 to cycles, then cycles..1 = N cycles blocked. */
      g_aimd_md_cooldown[host_index] = g_dynamic_config.aimd_md_cooldown_cycles + 1;
      metrics_record_aimd_event(host_index, METRICS_AIMD_MD_FIRED);
    } else {
      /* In cooldown, hold share, do not MD. */
      metrics_record_aimd_event(host_index, METRICS_AIMD_MD_BLOCKED);
    }
  } else {
    /* Deadband region, hold share (no change). */
    metrics_record_aimd_event(host_index, METRICS_AIMD_DEADBAND_HIT);
  }

  /* Floor: never let share fall below one AI step. Without this, after
   * MD the next AI would need many cycles to bring share back into the
   * useful range, especially when up_limit (and therefore ai_step) is
   * small. This fixes the "utilization stuck near 0 against
   * hard_core=5" regression. */
  if (share < ai_step) share = ai_step;
  return share;
}

/* Tentative declaration so auto_routed_controller / host_index_is_exclusive
 * can reference the watcher's top_results[] which is fully defined ~80 lines
 * below alongside the other watcher state arrays. C merges multiple tentative
 * defs of the same static object into a single allocation; the only one with
 * an initializer wins. */
static utilization_t top_results[MAX_DEVICE_COUNT];

/* "Is this device exclusively used by our container?" predicate, shared by
 * AUTO routing and the watcher's soft_core burst path.
 *
 * Returns true (= exclusive) when no NON-self-container process is actually
 * burning SM cycles on this card. Built on the existing user/sys split
 * already computed by get_used_gpu_utilization():
 *
 *   user_current = sum of SM util for PIDs identified as in-our-container
 *                  (using whichever discriminator the active compatibility
 *                  mode provides -- CLIENT PID file, cgroup v1/v2 membership,
 *                  or OPEN_KERNEL PID-namespace match);
 *   sys_current  = sum of SM util across ALL PIDs on the device.
 *
 * external_util = max(0, sys_current - user_current) is therefore the SM
 * util attributable strictly to processes outside our container. A real
 * competing Pod will report >= a few percent here; the NVIDIA driver's
 * always-resident threads (nvidia-persistenced, MPS server, X server)
 * report 0% because they never launch kernels.
 *
 * Why util-based and not PID-count-based: counting "external PIDs"
 * (sys_process_num - matched-self-PIDs) would still be tricked by the
 * driver threads (they ARE non-self PIDs); util filtering naturally
 * ignores them. Same predicate also correctly handles intra-container
 * fork (e.g. torch.distributed N workers) -- all child PIDs belong to
 * our container, so user_current absorbs them and external_util stays 0.
 *
 * HOST_COMPATIBILITY_MODE special case: no container isolation exists,
 * so get_used_gpu_utilization sets user_current = sys_current. The
 * subtraction is always 0 -> the predicate always reports "exclusive".
 * That matches the historical sys_process_num==1 behaviour for this mode
 * (the old check was meaningless there too, but defaulted to "exclusive"
 * whenever the operator was the sole CUDA user). No regression.
 *
 * Threading: only the watcher thread that owns host_index reads
 * top_results[host_index] (where it was written upstream this same
 * cycle). No cross-thread access; no atomics needed. */
/* Raw observation: a single watcher cycle's view of "are we exclusive".
 * No smoothing. Used directly by hard_limit jitter-init bypass (which
 * MUST react instantly to a freshly-started workload's util ramp) and
 * read by the debounced FSM as its input. */
static int host_index_is_exclusive_raw(int host_index) {
  if (top_results[host_index].external_process_num <= 0) return 1;
  int sys  = top_results[host_index].sys_current;
  int user = top_results[host_index].user_current;
  int external = sys - user;
  if (external < 0) external = 0;   /* defensive: NVML race may leave user > sys briefly */
  return external < g_dynamic_config.auto_external_util_threshold;
}

/* Debounced exclusivity FSM. Reads top_results for the host_index,
 * updates the streak / flip state, returns the smoothed answer.
 *
 * Idempotent within a watcher cycle: the watcher main loop clears
 * g_excl_memo_valid[host_index] at the top of each per-device iteration;
 * the first call inside that iteration advances the FSM and caches the
 * result; subsequent calls in the same iteration return the cached value
 * without touching the streak. */
static int host_index_is_exclusive_debounced(int host_index) {
  if (g_excl_memo_valid[host_index]) {
    return g_excl_memo_value[host_index];
  }
  int observed = host_index_is_exclusive_raw(host_index);
  int current  = g_is_exclusive_debounced[host_index];
  if (observed == current) {
    g_exclusive_pending_streak[host_index] = 0;
  } else if (++g_exclusive_pending_streak[host_index] >= g_dynamic_config.auto_debounce_cycles) {
    /* Flip survived debounce -- commit. */
    LOGGER(INFO, "exclusivity changed: host_device=%d sys=%d user=%d ext=%d %s -> %s",
           host_index,
           top_results[host_index].sys_current,
           top_results[host_index].user_current,
           top_results[host_index].sys_current - top_results[host_index].user_current,
           current  ? "exclusive" : "shared",
           observed ? "exclusive" : "shared");
    if (current == 1 && observed == 0) {
      /* Lost exclusivity: signal the watcher loop's else-branch to reset
       * up_limits back to hard_core on its next pass through (so we
       * return the soft_core burst headroom we'd grabbed). One-shot --
       * the watcher clears the flag after consuming it. */
      g_lost_exclusivity_pending[host_index] = 1;
      metrics_record_exclusivity_flip(host_index, METRICS_EXCLUSIVITY_FLIP_LOST);
    } else {
      /* current == 0 && observed == 1: shared -> exclusive (gained). */
      metrics_record_exclusivity_flip(host_index, METRICS_EXCLUSIVITY_FLIP_GAINED);
    }
    g_is_exclusive_debounced[host_index] = observed;
    g_exclusive_pending_streak[host_index] = 0;
    current = observed;
  }
  g_excl_memo_valid[host_index] = 1;
  g_excl_memo_value[host_index] = current;
  return current;
}

/* AUTO mode: per-device router between delta (single Pod, throughput) and
 * aimd (multi-Pod, fairness). Reads the SHARED debounced exclusivity FSM
 * (host_index_is_exclusive_debounced) so AUTO routing and the watcher's
 * burst gating always agree -- there is no second private FSM to drift
 * out of sync. shares[host_index] is shared across both controllers and
 * both algorithms are iterators on it, so flips carry over cleanly. */
static int64_t auto_routed_controller(int up_limit, int user_current,
                                      int64_t share, int host_index) {
  if (host_index_is_exclusive_debounced(host_index)) {
    return delta(up_limit, user_current, share, host_index);
  }
  return aimd_controller(up_limit, user_current, share, host_index);
}

/* Dump g_dynamic_config to the INFO log in a single deterministic line.
 * One source of truth: operators see exactly what the running process
 * actually believes its config is (after env load + clamp), not what
 * the docs claim the defaults should be. */
static void dump_dynamic_config(void) {
  static const char *kind_name[] = { "delta", "aimd", "auto" };
  int k = g_dynamic_config.sm_controller_kind;
  LOGGER(INFO,
    "+ DynamicConfig  : controller=%s usage_threshold=%d "
    "aimd[md_div=%.3f eff_ratio=%d/1000 ai_base_div=%d deadband_ratio=%d/1000 md_cooldown=%d] "
    "auto[debounce=%d ext_util_threshold=%d%%] "
    "shared_bucket=%d "
    "internal[soft_adjust_interval=%d delta_recovery_step=%d]",
    (k >= 0 && k <= 2) ? kind_name[k] : "?",
    g_dynamic_config.usage_threshold,
    g_dynamic_config.aimd_md_divisor,
    g_dynamic_config.aimd_eff_ratio,
    g_dynamic_config.aimd_ai_base_div,
    g_dynamic_config.aimd_deadband_ratio,
    g_dynamic_config.aimd_md_cooldown_cycles,
    g_dynamic_config.auto_debounce_cycles,
    g_dynamic_config.auto_external_util_threshold,
    g_dynamic_config.sm_shared_bucket,
    SOFT_ADJUST_INTERVAL,
    DELTA_ERROR_RECOVERY_STEP);
}

/* Called once from initialization() before watcher threads spawn (guarded
 * by pthread_once g_init_set). After this returns, g_dynamic_config is
 * read-only at runtime; the watcher thread is the only reader. fork()
 * safety: child_after_fork resets g_init_set so the child's first launch
 * re-runs initialization() which re-runs this -- env values are
 * re-applied, defaults are re-applied, no stale state survives. */
static void sm_controller_init(void) {
  /* === Load every algo env into g_dynamic_config (one place) ===
   * Each getter returns its parsed value or its default; we then clamp
   * to enforce algorithm-level invariants. Keep the load-then-clamp
   * pattern even when env is unset because some defaults could be
   * mismatched in future edits (the clamp is cheap insurance). */

  /* Controller selection (delta/aimd/auto). */
  int kind = SM_CONTROLLER_DELTA;
  (void)get_sm_controller_kind(&kind);
  if (kind < SM_CONTROLLER_DELTA || kind > SM_CONTROLLER_AUTO) {
    kind = SM_CONTROLLER_DELTA;
  }
  g_dynamic_config.sm_controller_kind = kind;

  /* Soft-mode periodic adjust threshold. Range >= 0. */
  (void)get_usage_threshold(&g_dynamic_config.usage_threshold);
  if (g_dynamic_config.usage_threshold < 0) g_dynamic_config.usage_threshold = 0;

  /* Exclusivity FSM tunables. Consulted in EVERY controller mode (used by
   * the watcher's burst gate / jitter-init), so loaded unconditionally. */
  (void)get_sm_auto_external_util_threshold(&g_dynamic_config.auto_external_util_threshold);
  if (g_dynamic_config.auto_external_util_threshold < 1) g_dynamic_config.auto_external_util_threshold = 1;
  (void)get_sm_auto_debounce_cycles(&g_dynamic_config.auto_debounce_cycles);
  if (g_dynamic_config.auto_debounce_cycles < 1) g_dynamic_config.auto_debounce_cycles = 1;

  /* delta ramp-floor divisor. Loaded unconditionally: delta runs as the default
   * controller and as an AUTO dispatch target. NO clamp here on purpose: a value
   * <= 0 is the user's explicit "disable the floor" sentinel, which delta()
   * honours by skipping the floor (and its division) entirely. */
  (void)get_delta_ramp_floor_divisor(&g_dynamic_config.delta_ramp_floor_divisor);

  /* Container-wide shared bucket. Read here so the dump line reports it and so
   * initialization() can act on it; the mapping itself happens there, once the
   * device geometry this region is seeded from is known. */
  g_dynamic_config.sm_shared_bucket = 0;
  (void)get_sm_shared_bucket(&g_dynamic_config.sm_shared_bucket);

  /* AIMD tunables. Loaded unconditionally because AUTO can dispatch to
   * aimd_controller when the device becomes shared, even if the user
   * picked CUDA_SM_CONTROLLER=auto without setting AIMD env vars. */
  (void)get_aimd_md_divisor(&g_dynamic_config.aimd_md_divisor);
  (void)get_aimd_eff_ratio(&g_dynamic_config.aimd_eff_ratio);
  (void)get_aimd_ai_base_div(&g_dynamic_config.aimd_ai_base_div);
  (void)get_aimd_deadband_ratio(&g_dynamic_config.aimd_deadband_ratio);
  (void)get_aimd_md_cooldown_cycles(&g_dynamic_config.aimd_md_cooldown_cycles);
  /* md_divisor clamp: /1 is no-op, /<1 INVERTS the algorithm (would
   * amplify share on overshoot). Floor 1.01 leaves room for FP rounding. */
  if (g_dynamic_config.aimd_md_divisor < 1.01) g_dynamic_config.aimd_md_divisor = 1.01;
  /* deadband must sit strictly below eff so AI/deadband/MD regions stay
   * well-ordered. */
  if (g_dynamic_config.aimd_deadband_ratio < 0) {
    g_dynamic_config.aimd_deadband_ratio = 0;
  } else if (g_dynamic_config.aimd_deadband_ratio >= g_dynamic_config.aimd_eff_ratio) {
    LOGGER(WARNING, "AIMD deadband_ratio (%d) must be < eff_ratio (%d); clamping to eff_ratio-1",
           g_dynamic_config.aimd_deadband_ratio, g_dynamic_config.aimd_eff_ratio);
    g_dynamic_config.aimd_deadband_ratio = g_dynamic_config.aimd_eff_ratio - 1;
  }
  if (g_dynamic_config.aimd_md_cooldown_cycles < 0) g_dynamic_config.aimd_md_cooldown_cycles = 0;

  /* === Bind the dispatch pointer + label metrics === */
  switch (kind) {
    case SM_CONTROLLER_AUTO:
      g_sm_controller = auto_routed_controller;
      metrics_set_controller_label("auto");
      break;
    case SM_CONTROLLER_AIMD:
      g_sm_controller = aimd_controller;
      metrics_set_controller_label("aimd");
      break;
    case SM_CONTROLLER_DELTA:
    default:
      g_sm_controller = delta;
      metrics_set_controller_label("delta");
      break;
  }

  /* Single deterministic dump line -- replaces the three prior controller-
   * specific INFO lines so operators have ONE place to grep. */
  dump_dynamic_config();
}

static int64_t shares[MAX_DEVICE_COUNT]    = {0};
static int sys_frees[MAX_DEVICE_COUNT]      = {0};
static int avg_sys_frees[MAX_DEVICE_COUNT]  = {0};
static int is[MAX_DEVICE_COUNT]             = {0};
/* pre_external_process_nums tracks the previously-observed count of
 * processes on this device that are NOT in our container. The watcher
 * uses it to decide whether the cycle's `external_process_num` indicates
 * a new EXTERNAL Pod has joined (in which case it resets up_limits to
 * hard_core). Pre-V2.1 this tracked total sys_process_num, which was
 * fooled by our own intra-container fork (e.g. DataLoader workers) and
 * by NVIDIA driver threads coming and going from the NVML process list.
 *
 * Initial value 0: at process start we have observed no external PIDs
 * yet, so the first "real" cycle's value -- whatever it is -- becomes
 * the reference. Subsequent cycles see no change (driver threads stay
 * resident, our own fork doesn't move this number) unless a genuinely
 * new external process arrives. */
static int pre_external_process_nums[MAX_DEVICE_COUNT] = {0};
static utilization_t top_results[MAX_DEVICE_COUNT] = {};
/* volatile: written by the watcher thread, read cross-thread by the GAP path
 * (gap_effective_dc). Matches g_dev_hot[].cur_cuda_cores' convention -- forces a real
 * load (no register caching) of this single aligned int. */
static volatile int up_limits[MAX_DEVICE_COUNT] = {0};
static nvmlDevice_t nvml_devices[MAX_DEVICE_COUNT] = {};

/* ---------------------------------------------------------------------- *
 *  Refill election + shared controller state
 *
 *  With the shared bucket on, every process still runs its own watcher. If
 *  they all refilled, the container would be supplied N times over and the
 *  limit would end up N times looser than before -- worse than the problem
 *  being fixed. So each cycle, per device, exactly one process wins the right
 *  to run the controller and refill.
 *
 *  This is an election per cycle, not a leader: no term, no liveness probe, no
 *  failover. A winner that dies mid-cycle simply does not refill that cycle;
 *  next cycle the stamp is old enough again and somebody else takes it. Worst
 *  case is one skipped refill (~100ms), and recovery needs no code.
 * ---------------------------------------------------------------------- */

/* The watcher's DESIGN per-device period, and the single base every timing
 * constant below is expressed against.
 *
 * It is derived, not guessed: the loop sleeps wait = 100ms/dev_count before
 * each device and visits dev_count devices per pass, so a device is revisited
 * every 100ms no matter how many devices the batch holds. Naming it once means
 * the rest are stated as "N periods" rather than as free-floating milliseconds
 * that nobody can later justify. */
#define SM_WATCHER_NOMINAL_PERIOD_NS (100LL * 1000000LL)

/* Slightly under one period, so ordinary jitter cannot push a tick past the
 * threshold and cost the device a whole refill interval, while staying close
 * enough that two processes cannot both refill within one period. */
#define SM_REFILL_PERIOD_NS (SM_WATCHER_NOMINAL_PERIOD_NS * 9 / 10)

/* Past this multiple of the nominal period the watcher is missing its own
 * design contract by an order of magnitude. Scaling patience further would let
 * one pathological interval -- a suspend, a clock artefact, an owner
 * descheduled for seconds -- park the limits somewhere they never fire again,
 * so beyond this we stop adapting and treat the slowness as a fault. */
#define SM_CADENCE_MAX_PERIODS 10

/* Beats of the owner's cadence before a standby stops trusting its sample, and
 * before a standby will claim a refill out from under it. Stated as beats, not
 * as durations, precisely so they stay correct when a beat is not 100ms. */
#define SM_SAMPLE_STALE_BEATS   3
#define SM_STANDBY_REFILL_BEATS 2

/* How fast the owner of this device is ACTUALLY cycling.
 *
 * Everything a standby decides has to be measured against this rather than
 * against the nominal period, because the watcher's period is not a constant.
 * When per-device processing overruns its slot the loop stops sleeping to the
 * absolute-time grid and falls back to a 10ms floor, so an iteration costs
 * (processing + 10ms) and a device is revisited only every dev_count
 * iterations. Slow NVML on a four-device batch puts a device's period past
 * 400ms.
 *
 * Against thresholds fixed for a 100ms beat that is not an edge case, it is
 * every cycle: standbys would permanently judge the owner stale, all of them
 * would resume calling NVML, and the added load would slow the owner further --
 * a feedback loop whose fixed point is "centralisation delivers nothing",
 * reached exactly when the machine is already struggling.
 *
 * Below nominal (including 0, meaning the owner has not published twice yet)
 * we use nominal: an owner cannot beat the design cadence, and treating a
 * not-yet-known cadence as infinitely fast would make standbys hair-trigger
 * during startup. */
static int64_t sm_owner_cadence_ns(const sm_node_dev_t *d) {
  int64_t interval = __atomic_load_n(&d->sample_interval_ns, __ATOMIC_RELAXED);
  if (interval < SM_WATCHER_NOMINAL_PERIOD_NS) {
    return SM_WATCHER_NOMINAL_PERIOD_NS;
  }
  int64_t cap = SM_WATCHER_NOMINAL_PERIOD_NS * SM_CADENCE_MAX_PERIODS;
  return interval > cap ? cap : interval;
}

/* Standbys wait longer than the sampling owner before claiming a refill, which
 * makes the owner win in normal operation.
 *
 * Without this the two claims are fully independent, and which process refills
 * depends on nothing but the relative phase of the watchers. A standby whose
 * cycle happens to land just after the threshold takes the refill and runs the
 * controller on the sample the owner published up to a period ago, while the
 * owner -- which sampled moments earlier -- loses its own CAS and its fresh
 * sample waits a cycle to be used. Never wrong, since the sample is a 1-second
 * NVML average and the actuation lag is already 200-400ms, but it makes "which
 * sample fed this refill" depend on process start order, which is a poor thing
 * to have to reason about when reading a trace.
 *
 * With the owner claiming at 1x and standbys at 2x, the steady state is simply
 * "whoever sampled also refills, immediately". Standbys revert to what they
 * are for: a backstop that fires only if the owner misses a full extra period.
 * The takeover path is unaffected -- an owner that DIES releases its lock, so a
 * standby becomes the owner on its next cycle and claims at 1x from then on.
 * The 2x threshold only governs the narrow window before that, and the
 * alive-but-stuck case. */
/* Warn if the region file at SM_NODE_FILE_PATH is no longer the one we mapped.
 *
 * Rate-limited to roughly once a second and only reached on the refill path, so
 * it costs one stat per second per process -- irrelevant next to the NVML call
 * beside it, and nothing on the launch path.
 *
 * Remembers the new identity after warning, so a replacement produces one line
 * rather than a stream, and a later replacement is reported again. It does NOT
 * remap: the processes that mapped the old inode are still using it, so
 * adopting the new one here would only add a third view. There is no in-process
 * recovery -- the container has to restart -- and saying so is the useful part. */
static void shared_regions_check_identity(void) {
  /* Note what is NOT guarded here. An earlier version bailed out on
   * g_sm_node == NULL, which silently disabled the vmem_node check in the
   * DEFAULT configuration -- the shared bucket is opt-in, the ledger is not, so
   * the common case was the one left unchecked. Each region is guarded
   * individually below instead. */
  if (g_sm_node == NULL && g_device_vmem == NULL) {
    return;
  }
  int64_t last = __atomic_load_n(&g_sm_ident_checked_ns, __ATOMIC_RELAXED);
  int64_t now  = monotonic_ns();
  if (now - last < 1000000000LL) {
    return;
  }
  /* Claim the interval. A thread that loses simply skips -- the check is
   * whole-container, so one per interval is exactly what is wanted, and this
   * keeps the state below single-writer. */
  if (!CAS(&g_sm_ident_checked_ns, last, now)) {
    return;
  }

  /* Same interval, same claim: the vmem_node ledger is exposed to exactly the
   * same in-container deletion, and its split is worse (an under-enforced memory
   * limit rather than a loose SM limit). Checked here so both regions share one
   * stat-per-second budget and one call site. */
  vmem_node_check_identity();          /* self-guards on g_device_vmem */

  if (g_sm_node == NULL || g_sm_node_ino == 0) {
    return;                             /* shared bucket off, or identity unknown */
  }
  struct stat sb;
  if (stat(SM_NODE_FILE_PATH, &sb) != 0) {
    if (!g_sm_ident_warned) {
      LOGGER(WARNING, "%s has been deleted; processes started from now on will create a "
                      "SEPARATE region and the container's aggregate SM limit will be "
                      "enforced per group, not container-wide. Restart the container.",
                      SM_NODE_FILE_PATH);
      g_sm_ident_warned = 1;
    }
    /* g_sm_node_ino is deliberately left intact rather than zeroed. Zeroing it
     * would trip the `g_sm_node_ino == 0` guard just above and disable this
     * check permanently, so a file that later reappears under a new inode --
     * the moment the bucket actually splits -- would never be reported. */
    return;
  }
  if (sb.st_ino != g_sm_node_ino || sb.st_dev != g_sm_node_dev) {
    LOGGER(WARNING, "%s was replaced (inode %llu -> %llu); this process is still mapped "
                    "to the old region while newer processes use the new one, so the "
                    "shared bucket has split. Restart the container.",
                    SM_NODE_FILE_PATH,
                    (unsigned long long)g_sm_node_ino, (unsigned long long)sb.st_ino);
    g_sm_node_ino = sb.st_ino;     /* report each replacement once */
    g_sm_node_dev = sb.st_dev;
    g_sm_ident_warned = 0;         /* a further replacement is a new complaint */
    return;
  }
  g_sm_ident_warned = 0;           /* healthy again */
}

/* Returns 1 if this process owns this device's controller for this cycle.
 * Always 1 when the bucket is per-process -- there is nobody to contend with,
 * and the caller's code path stays exactly what it has always been. */
static int sm_try_claim_refill(int host_index) {
  if (g_sm_node == NULL) {
    return 1;
  }
  int64_t *stamp = &g_sm_node->devices[host_index].last_refill_ns;
  int64_t last = __atomic_load_n(stamp, __ATOMIC_RELAXED);
  int64_t now  = monotonic_ns();
  /* g_sm_lock_fd < 0 means ownership does not exist at all (no lock file), so
   * there is no owner to defer to and every process must keep claiming at the
   * owner threshold. Deferring here instead would push the whole container onto
   * the standby threshold -- nobody would hold the short ticket -- and cut the
   * refill rate, starving the bucket.
   *
   * The standby threshold is measured in beats of the OWNER's cadence, not in
   * fixed milliseconds. A fixed 2x90ms would be exceeded constantly by an owner
   * legitimately cycling at 440ms, so standbys would take its refills every
   * 180ms and the "whoever samples also refills" property would quietly hold
   * only for fast owners. */
  int64_t threshold;
  if (g_sm_lock_fd < 0 || g_sm_sampling_mine[host_index]) {
    threshold = SM_REFILL_PERIOD_NS;
  } else {
    threshold = sm_owner_cadence_ns(&g_sm_node->devices[host_index])
                * SM_STANDBY_REFILL_BEATS;
  }
  if (now - last < threshold) {
    return 0;
  }
  /* CAS decides it: several processes can pass the staleness test at once, but
   * only the one that swaps the stamp proceeds. The asymmetric thresholds only
   * bias WHO gets here; the CAS is what still guarantees at most one refill,
   * which is why a stale g_sm_sampling_mine could never cause over-supply. */
  return CAS(stamp, last, now) ? 1 : 0;
}

/* Copy the container's controller state into this process's working arrays.
 *
 * Doing it this way -- snapshot in, publish out, around the winner's block --
 * rather than rewriting every access site keeps the control algorithm textually
 * identical, which matters: the algorithm is not changing, only where its state
 * lives. It is also safe by construction, because only the winner runs between
 * the load and the publish.
 *
 * `share` carries the acquire/release pairing for the whole block: it is loaded
 * first with ACQUIRE and stored last with RELEASE, so every plain field written
 * before the release store is visible to the next winner that acquire-loads it. */
static void sm_ctl_load(int host_index) {
  if (g_sm_node == NULL) {
    return;
  }
  sm_node_dev_t *d = &g_sm_node->devices[host_index];
  shares[host_index]                     = __atomic_load_n(&d->share, __ATOMIC_ACQUIRE);
  up_limits[host_index]                  = d->up_limit;
  is[host_index]                         = d->is_cnt;
  avg_sys_frees[host_index]              = d->avg_sys_free;
  pre_external_process_nums[host_index]  = d->pre_external_proc;
  /* Without these three the exclusivity FSM would advance once per N cycles in
   * each process and fracture; lost_excl_pending especially, being a one-shot
   * flag, would hang set in every non-winner and fire a stale reset cycles late. */
  g_is_exclusive_debounced[host_index]   = d->excl_debounced;
  g_exclusive_pending_streak[host_index] = d->excl_streak;
  g_lost_exclusivity_pending[host_index] = d->lost_excl_pending;
  /* Without this, AIMD re-fires MD every cycle -- each winner sees its own
   * never-advanced cooldown of 0 -- and share collapses by md_divisor^N. That
   * avalanche is the exact failure the cooldown exists to prevent. */
  g_aimd_md_cooldown[host_index]         = d->md_cooldown;
}

static void sm_ctl_publish(int host_index) {
  if (g_sm_node == NULL) {
    return;
  }
  sm_node_dev_t *d = &g_sm_node->devices[host_index];
  d->up_limit          = up_limits[host_index];
  d->is_cnt            = is[host_index];
  d->avg_sys_free      = avg_sys_frees[host_index];
  d->pre_external_proc = pre_external_process_nums[host_index];
  d->excl_debounced    = g_is_exclusive_debounced[host_index];
  d->excl_streak       = g_exclusive_pending_streak[host_index];
  d->lost_excl_pending = g_lost_exclusivity_pending[host_index];
  d->md_cooldown       = g_aimd_md_cooldown[host_index];
  __atomic_store_n(&d->share, shares[host_index], __ATOMIC_RELEASE);  /* publish */
}

/* ---------------------------------------------------------------------- *
 *  Sampling ownership
 *
 *  Refill admission (above) and sampling ownership (here) are deliberately
 *  DIFFERENT mechanisms, and the split is what makes this safe:
 *
 *    last_refill_ns CAS  -- HARD. Bounds how often the bucket may be topped
 *                           up, always, no matter what else goes wrong.
 *    byte-range lock     -- SOFT. Decides who pays for NVML. If it fails, is
 *                           lost, or is somehow held twice, the worst outcome
 *                           is redundant sampling; it can never over-supply
 *                           the bucket, because the CAS still gates that.
 *
 *  So correctness never rests on the lock. That matters because a lock CAN be
 *  held by a process that is alive but stuck, and because fork() shares the
 *  descriptor the lock lives on.
 * ---------------------------------------------------------------------- */


/* Returns 1 if this process should sample this device. Already-owner is the
 * steady-state answer and costs nothing; otherwise ONE non-blocking fcntl.
 * That is the entire per-cycle cost of standing by -- no NVML call, no spin,
 * no sleep-poll loop. */
static int sm_sampling_claim(int host_index) {
  if (g_sm_node == NULL || g_sm_lock_fd < 0) {
    return 1;                       /* not shared, or no lock: sample as always */
  }
  if (g_sm_sampling_mine[host_index]) {
    return 1;
  }
  struct flock fl;
  memset(&fl, 0, sizeof(fl));
  fl.l_type   = F_WRLCK;
  fl.l_whence = SEEK_SET;
  fl.l_start  = host_index;         /* one byte per device -> devices are
                                     * independent, which is required: with
                                     * differing CUDA_VISIBLE_DEVICES two
                                     * processes may see disjoint device sets,
                                     * so there is no single owner for all. */
  fl.l_len    = 1;
  if (ofd_fcntl(g_sm_lock_fd, /*wait=*/0, &fl) == -1) {
    return 0;                       /* someone else owns it -- stand by */
  }
  g_sm_sampling_mine[host_index] = 1;
  __atomic_store_n(&g_sm_node->devices[host_index].leader_pid,
                   (int32_t)getpid(), __ATOMIC_RELAXED);
  LOGGER(VERBOSE, "host device %d: acquired sampling ownership", host_index);
  return 1;
}

/* Publish the sample just taken so standbys can skip NVML entirely. The stamp
 * goes last with release ordering; a reader acquire-loads it first. */
static void sm_publish_sample(int host_index) {
  if (g_sm_node == NULL) {
    return;
  }
  sm_node_dev_t *d = &g_sm_node->devices[host_index];
  int64_t now = monotonic_ns();

  /* Publish the cadence we are actually achieving, so standbys can scale their
   * staleness limit to it rather than to an assumed ~100ms (see
   * sm_owner_cadence_ns). Measured from this process's own previous publish;
   * skipped on the first one, and whenever ownership has just changed hands,
   * because the gap across a handover reflects the takeover, not the cadence. */
  int64_t prev = g_sm_last_publish_ns[host_index];
  if (prev != 0 && now > prev) {
    __atomic_store_n(&d->sample_interval_ns, now - prev, __ATOMIC_RELAXED);
  }
  g_sm_last_publish_ns[host_index] = now;

  d->s_user_current      = top_results[host_index].user_current;
  d->s_sys_current       = top_results[host_index].sys_current;
  d->s_sys_process_num   = top_results[host_index].sys_process_num;
  d->s_external_proc_num = top_results[host_index].external_process_num;
  __atomic_store_n(&d->sample_published_ns, now, __ATOMIC_RELEASE);
}

/* Load the owner's sample into this process's top_results. Returns 0 if there
 * is no sample yet or it is too old to trust, in which case the caller samples
 * for itself -- that is the backstop for an owner that holds the lock but has
 * stopped making progress, which a lock alone cannot detect. */
static int sm_load_published_sample(int host_index) {
  if (g_sm_node == NULL) {
    return 0;
  }
  sm_node_dev_t *d = &g_sm_node->devices[host_index];
  int64_t published = __atomic_load_n(&d->sample_published_ns, __ATOMIC_ACQUIRE);
  if (published == 0 ||
      monotonic_ns() - published > sm_owner_cadence_ns(d) * SM_SAMPLE_STALE_BEATS) {
    return 0;
  }
  /* Plain reads: these are heuristic counters feeding a control loop that is
   * already fed by a 1-second NVML average, so a torn-by-one-cycle field is
   * indistinguishable from ordinary sampling jitter. */
  top_results[host_index].user_current         = d->s_user_current;
  top_results[host_index].sys_current          = d->s_sys_current;
  top_results[host_index].sys_process_num      = d->s_sys_process_num;
  top_results[host_index].external_process_num = d->s_external_proc_num;
  top_results[host_index].valid                = 1;
  return 1;
}

static void *utilization_watcher(void *arg) {
  batch_t *batch = (batch_t *)arg;
  LOGGER(VERBOSE, "start %s batch code %d", __FUNCTION__, batch->batch_code);
  LOGGER(VERBOSE, "batch code %d, start index %d, end index %d", batch->batch_code, batch->start_index, batch->end_index);

  int host_indexes[MAX_DEVICE_COUNT] = {0};

  int host_index, cuda_index;
  for (cuda_index = batch->start_index; cuda_index < batch->end_index; cuda_index++) {
    host_index = get_host_device_index_by_cuda_device(cuda_index);
    host_indexes[cuda_index] = host_index;

    is[host_index] = 0;
    shares[host_index] = 0;
    sys_frees[host_index] = 0;
    avg_sys_frees[host_index] = 0;
    pre_external_process_nums[host_index] = 0;
    up_limits[host_index] = get_device_flag(host_index, hard_core);
    top_results[host_index].user_current = 0;
    top_results[host_index].sys_current = 0;
    top_results[host_index].valid = 0;
    top_results[host_index].sys_process_num = 0;
    top_results[host_index].external_process_num = 0;
  }
  int dev_count = batch->end_index - batch->start_index;
  struct timespec wait = {
    .tv_sec = 0,
    .tv_nsec = 100 / dev_count * MILLISEC,
  };
  /* Minimum sleep when the abstime deadline has already passed (overrun), to
   * stop a busy loop. Kept below `wait` (dev_count <= MaxBatchSize=4 => wait >=
   * 25ms) so it never throttles the normal cadence; matches TIME_TICK. */
  const int64_t MIN_WATCHER_SLEEP_NS = 10 * (int64_t)MILLISEC;
  /* Absolute-time cadence: clock_nanosleep(TIMER_ABSTIME) against a monotonic
   * grid keeps the sampling period drift-free -- a relative nanosleep(&wait)
   * silently adds each cycle's processing time to the period. first_cycle runs
   * the whole batch immediately (no sleep) so a freshly-started watcher
   * publishes its first sample without a startup delay; the grid is anchored to
   * "now" right after that first pass. */
  struct timespec next_wakeup;
  int first_cycle = 1;
  while (1) {
    for (cuda_index = batch->start_index; cuda_index < batch->end_index; cuda_index++) {
      if (likely(!first_cycle)) {
        struct timespec now_ts;
        clock_gettime(CLOCK_MONOTONIC, &now_ts);
        int64_t remaining_ns = (int64_t)(next_wakeup.tv_sec - now_ts.tv_sec) * 1000000000LL
                             + (next_wakeup.tv_nsec - now_ts.tv_nsec);
        if (unlikely(remaining_ns < MIN_WATCHER_SLEEP_NS)) {
          /* At/behind the deadline: sleep a fixed minimum instead of letting
           * clock_nanosleep(TIMER_ABSTIME) return immediately on a past deadline
           * -- a persistently overrunning watcher would otherwise busy-loop and
           * burn CPU. next_wakeup stays on the grid, so once processing catches
           * up remaining_ns goes positive again and the drift-free cadence
           * re-syncs on its own (no one-shot catch-up burst). */
          struct timespec floor_ts = { .tv_sec = 0, .tv_nsec = MIN_WATCHER_SLEEP_NS };
          nanosleep(&floor_ts, NULL);
        } else {
          clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &next_wakeup, NULL);
        }
        next_wakeup.tv_nsec += wait.tv_nsec;
        if (next_wakeup.tv_nsec >= 1000000000L) {
          next_wakeup.tv_sec += next_wakeup.tv_nsec / 1000000000L;
          next_wakeup.tv_nsec %= 1000000000L;
        }
      }
      host_index = host_indexes[cuda_index];
      /* One tear-free config snapshot per device per cycle: every core_limit /
       * hard_limit / hard_core / soft_core read in this loop body uses it, so a
       * concurrent Go ModifyDevice can never split this cycle's decisions across
       * an old and a new config. One-cycle staleness is fine for a watcher. */
      device_t dcfg = get_device_snapshot(host_index);

      /* Before every `continue` below, deliberately. Region identity has
       * nothing to do with sampling, core_limit, or who won the refill, and
       * gating it on any of them leaves real configurations unchecked: a
       * container with memory_limit but core_limit off skips this loop body
       * entirely, and that is exactly the case where the vmem ledger matters
       * and SM limiting does not. Self-rate-limited to one stat per second per
       * process, so calling it per device costs an atomic load and a compare.
       * Not on the launch path. */
      shared_regions_check_identity();

      // Skip GPU without core limit enabled
      if (!dcfg.core_limit) continue;

      /* Sample only if we own sampling for this device, or if the owner's
       * published sample has gone stale. A standby in steady state pays one
       * non-blocking fcntl plus one shared read here, and no NVML call at all
       * -- which is the point: N processes polling nvmlDeviceGetProcessUtilization
       * every ~100ms is exactly the call pattern the driver handles worst. */
      if (sm_sampling_claim(host_index)) {
        get_used_gpu_utilization((void *)&top_results[host_index], cuda_index, host_index, nvml_devices[host_index]);
        if (unlikely(!top_results[host_index].valid)) continue;
        sm_publish_sample(host_index);
      } else if (!sm_load_published_sample(host_index)) {
        /* Owner holds the lock but has stopped publishing -- alive and stuck,
         * which the lock cannot express. Sample for ourselves so the bucket
         * keeps being replenished. Refill stays gated by the CAS below, so
         * this cannot double-supply even if several standbys do it at once. */
        get_used_gpu_utilization((void *)&top_results[host_index], cuda_index, host_index, nvml_devices[host_index]);
        if (unlikely(!top_results[host_index].valid)) continue;
      }

      /* Invalidate per-cycle exclusivity memo so the FIRST debounced
       * predicate call this iteration recomputes from fresh sampling
       * data (and advances the streak / flip FSM at most once per
       * watcher cycle, regardless of how many code paths consult it). */
      g_excl_memo_valid[host_index] = 0;

      /* Sampling above happens in every process (metrics and logs want it);
       * from here down is the controller, and exactly one process per cycle
       * gets to run it. A no-op when the bucket is per-process. */
      if (!sm_try_claim_refill(host_index)) {
        continue;
      }
      sm_ctl_load(host_index);

      sys_frees[host_index] = MAX_UTILIZATION - top_results[host_index].sys_current;

      /* Read-and-clear once per cycle (before either branch, so it never goes
       * stale): did any launch on this device throttle since we last looked?
       * Must sit inside the winner's block -- a non-winner clearing the shared
       * flag would swallow the signal the winner needs to see. */
      int throttled = __atomic_exchange_n(sm_throttled_of(host_index), 0, __ATOMIC_RELAXED);

      if (dcfg.hard_limit) {
        /* Anti-jitter soft-start. Use the RAW predicate (no debounce) here: this
         * bypass must respond instantly to a freshly-started workload's util
         * ramp, and the hard_limit controller cannot exceed hard_core anyway so
         * a misfire here is not a safety hazard (worst case: a few cycles of
         * slightly different share computation).
         *
         * The bypass SETs g_cur to a single controller step and freezes shares[]
         * (via continue, skipping change_token) so the bucket does NOT
         * pre-accumulate to a full g_total during an idle/model-load phase --
         * otherwise the first heavy kernels would burst from a full bucket to
         * ~100% and get cut, causing startup jitter. But gate it on `!throttled`:
         * if the workload is actually hitting the throttle, its low util is
         * starvation, not idleness, and clamping would pin it near zero forever
         * (fatal on small-SM GPUs where the sm^2-scaled clamp is a fraction of a
         * percent -- observed util stuck <1%). In that case fall through to the
         * accumulating path so the bucket ramps up to serve the demand.
         *
         * Threshold floored at 1: up_limit/10 truncates to 0 for hard_core < 10,
         * which would disable the bypass entirely -- and then the idle-phase
         * accumulate path (util 0 <= hard_core, so delta keeps growing) fills the
         * bucket to g_total before any demand arrives, so the first kernels burst
         * far past the limit (observed: hard_core=8, util jumps to ~15 from a
         * pre-filled bucket). Keeping the clamp alive for small limits stops the
         * idle pre-fill. */
        int low_util_thr = up_limits[host_index] / 10;
        if (low_util_thr < 1) low_util_thr = 1;
        if (host_index_is_exclusive_raw(host_index)
            && top_results[host_index].user_current < low_util_thr
            && !throttled) {
          int64_t bypass_target =
              g_sm_controller(dcfg.hard_core, top_results[host_index].user_current, shares[host_index], host_index);
          if (g_sm_node != NULL) {
            /* A plain store would erase tokens other processes CAS-deducted
             * between our read and our write -- tokens conjured back into the
             * bucket, i.e. over-launch. Express the same intent ("bring the
             * bucket to target") as a delta applied through change_token's CAS
             * loop, which commutes with concurrent deductions.
             *
             * The per-process branch below keeps the literal store: with one
             * bucket per process the switch is off, and off must mean bit-for-
             * bit the old behaviour. */
            volatile int64_t *bucket = sm_bucket_of(host_index);
            int64_t cur = __atomic_load_n(bucket, __ATOMIC_RELAXED);
            change_token(bypass_target - cur, host_index);
          } else {
            g_dev_hot[host_index].cur_cuda_cores = bypass_target;
          }
          /* The controller ran, so it may have advanced md_cooldown or the
           * exclusivity FSM. Publish before the continue or those advances are
           * lost and the next winner recomputes from stale state. */
          sm_ctl_publish(host_index);
          continue;
        }
        shares[host_index] = g_sm_controller(dcfg.hard_core, top_results[host_index].user_current, shares[host_index], host_index);
      } else {
        if (pre_external_process_nums[host_index] != top_results[host_index].external_process_num) {
          /* A NEW external process arrived (count grew) -> reset to
           * hard_core so all competitors negotiate from the same floor.
           * V2.1: compare external_process_num instead of sys_process_num
           * so our own intra-container fork (DataLoader workers,
           * torch.distributed ranks) does NOT trigger a reset against
           * ourselves. Strict counting in get_used_gpu_utilization means
           * NVIDIA driver threads ARE counted as external -- but they
           * appear once at startup and stay, so they only ever trigger
           * a single initial reset (which is harmless: we'd just been
           * at hard_core anyway). */
          if (pre_external_process_nums[host_index] < top_results[host_index].external_process_num) {
            shares[host_index] = (int64_t) g_max_thread_per_sm[host_index];
            up_limits[host_index] = dcfg.hard_core;
            is[host_index] = 0;
            avg_sys_frees[host_index] = 0;
          }
          pre_external_process_nums[host_index] = top_results[host_index].external_process_num;
        }

        /* 1. Device is exclusively used by us (no external Pod competing).
         *    Allocate cuda cores up to soft_core for burst headroom.
         *
         * 2. Another Pod is actively burning SM on this device. First,
         *    change up_limit of this process according to historical
         *    resource utilization. Second, allocate cuda cores according
         *    to the changed limit value.
         *
         * Use the DEBOUNCED predicate: the burst grants exclusive access
         * to all of soft_core which directly squeezes any external Pod,
         * so we MUST be confident (g_dynamic_config.auto_debounce_cycles consecutive
         * agreeing observations) before flipping into burst mode. The
         * debounced FSM also drops g_lost_exclusivity_pending when it
         * flips back, which the else branch consumes to give the burst
         * headroom back via reset. */
        if (host_index_is_exclusive_debounced(host_index)) {
          up_limits[host_index] = dcfg.soft_core;
          shares[host_index] = g_sm_controller(up_limits[host_index], top_results[host_index].user_current, shares[host_index], host_index);
        } else {
          /* Lost-exclusivity reset (V2.1 option ①). When the debounced
           * FSM flipped true->false (we no longer have the card to
           * ourselves), give back any soft_core burst headroom we'd
           * grabbed by resetting up_limits to hard_core and re-starting
           * the elastic ramp. One-shot: consume the flag. */
          if (g_lost_exclusivity_pending[host_index]) {
            shares[host_index] = (int64_t) g_max_thread_per_sm[host_index];
            up_limits[host_index] = dcfg.hard_core;
            is[host_index] = 0;
            avg_sys_frees[host_index] = 0;
            g_lost_exclusivity_pending[host_index] = 0;
          }
          is[host_index]++;
          avg_sys_frees[host_index] += sys_frees[host_index];
          if (is[host_index] % SOFT_ADJUST_INTERVAL == 0) {
            /* Symmetric ramp: if we've been seeing headroom, climb toward
             * soft_core (V1 behaviour, kept); if we've been seeing pressure
             * AND we previously climbed past hard_core, give some back. */
            int avg = avg_sys_frees[host_index] * 2 / SOFT_ADJUST_INTERVAL;
            int step = dcfg.hard_core / 10;
            /* Integer division truncates to 0 for hard_core < 10, which would
             * freeze up_limits (never climbs to soft_core, never steps back).
             * Floor at 1 so the elastic ramp still moves for small limits. */
            if (step < 1) step = 1;
            int soft = dcfg.soft_core;
            int hard = dcfg.hard_core;
            if (avg > g_dynamic_config.usage_threshold) {
              /* Headroom available -> climb (capped at soft_core). */
              up_limits[host_index] = up_limits[host_index] + step > soft
                                      ? soft
                                      : up_limits[host_index] + step;
            } else if (avg < g_dynamic_config.usage_threshold
                       && up_limits[host_index] > hard) {
              /* Sustained pressure AND we'd previously climbed past
               * hard_core -> step back down. Strictly floored at
               * hard_core: never go below the user's guaranteed share,
               * even briefly. The "> hard" gate matters because before
               * V2.1 there was no down path, so up_limits is always
               * >= hard_core; we keep that invariant explicit. */
              up_limits[host_index] = up_limits[host_index] - step < hard
                                      ? hard
                                      : up_limits[host_index] - step;
            }
            is[host_index] = 0;
          }
          avg_sys_frees[host_index] = is[host_index] % (SOFT_ADJUST_INTERVAL / 2) == 0 ? 0 : avg_sys_frees[host_index];
          shares[host_index] = g_sm_controller(up_limits[host_index], top_results[host_index].user_current, shares[host_index], host_index);
        }
      }
      change_token(shares[host_index], host_index);
      sm_ctl_publish(host_index);
      if ((g_share_log_tick[host_index]++ % WATCHER_UTIL_LOG_STRIDE) == 0) {
        g_share_log_tick[host_index] = 1;
        LOGGER(DETAIL, "cuda device: %d, host device: %d, user util: %d, up_limit: %d, share: %ld, curr core: %ld (1/%d sampled)", cuda_index, host_index,
               top_results[host_index].user_current, up_limits[host_index], shares[host_index], *sm_bucket_of(host_index), WATCHER_UTIL_LOG_STRIDE);
      }
    }
    if (unlikely(first_cycle)) {
      /* First pass ran with no sleeps; anchor the steady-state grid to now so
       * the next pass sleeps a whole interval rather than racing to catch up. */
      first_cycle = 0;
      clock_gettime(CLOCK_MONOTONIC, &next_wakeup);
      next_wakeup.tv_nsec += wait.tv_nsec;
      if (next_wakeup.tv_nsec >= 1000000000L) {
        next_wakeup.tv_sec += next_wakeup.tv_nsec / 1000000000L;
        next_wakeup.tv_nsec %= 1000000000L;
      }
    }
  }
}

/* ====================================================================== *
 *  GAP-path SM throttle helpers                                           *
 *  See docs/sm_core_limit_gap_throttle_design.md                          *
 * ====================================================================== */

static inline int64_t monotonic_ns(void) {
  struct timespec ts;
#ifdef CLOCK_MONOTONIC_COARSE
  // Compile time: Only attempt COARSE on platforms with defined macros
  // Runtime: Very old kernel may have defined macros but syscall returns EINVAL
  if (__builtin_expect(clock_gettime(CLOCK_MONOTONIC_COARSE, &ts) == 0, 1)) {
    return (int64_t)ts.tv_sec * 1000000000LL + ts.tv_nsec;
  }
#endif
  // Fallback path: COARSE unavailable or failed
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (int64_t)ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

/* Current effective duty-cycle target (percent) for the GAP path:
 *   0      -> limiting disabled, no throttle
 *   1..99  -> throttle to this percentage
 *   >=100  -> currently allowed to burst at full speed, no throttle
 * In soft mode it follows the watcher's live elastic limit (up_limits[]), so
 * the GAP path inherits soft_core burst/back-off behaviour; in hard mode it
 * is the static hard_core. Lock-free read of an int snapshot is intentional. */
static int gap_effective_dc(int host_index) {
  device_t d = get_device_snapshot(host_index);
  if (!d.core_limit) return 0;
  if (d.hard_limit)  return d.hard_core;
  /* With a shared bucket the controller runs in ONE process per cycle, so this
   * process's private up_limits[] is only as fresh as the last cycle it won --
   * and a process that keeps losing may never refresh it at all. Reading the
   * container-wide value keeps every process's GAP throttle on the same limit
   * the controller actually converged to. Relaxed: a stale-by-one-cycle int is
   * fine for a duty-cycle target, which is why the private read was already
   * lock-free. */
  int t = g_sm_node ? __atomic_load_n(&g_sm_node->devices[host_index].up_limit,
                                      __ATOMIC_RELAXED)
                    : up_limits[host_index];
  if (t <= 0) t = d.hard_core;   /* watcher not warmed up yet -> guarantee floor */
  return t;
}

/* Lazily create the per-device cuEvent pair. Requires a current context
 * (guaranteed at a launch site, cuCtxGetDevice has succeeded). Caller holds
 * g_gap_lock[host_index]. */
static int gap_events_ensure(int host_index) {
  if (g_gap_evt_ready[host_index]) return 1;
  CUresult r1 = CUDA_INTERNAL_CALL(cuda_library_entry, cuEventCreate,
                                   &g_gap_start[host_index], CU_EVENT_DEFAULT);
  CUresult r2 = CUDA_INTERNAL_CALL(cuda_library_entry, cuEventCreate,
                                   &g_gap_end[host_index], CU_EVENT_DEFAULT);
  if (r1 != CUDA_SUCCESS || r2 != CUDA_SUCCESS) {
    LOGGER(VERBOSE, "host device %d: gap cuEventCreate failed (%d/%d)",
           host_index, r1, r2);
    return 0;
  }
  g_gap_evt_ready[host_index] = 1;
  return 1;
}

/* Determine whether the stream is in capture, return 1 if yes, return 0 if no.
 *
 * `ptsz` selects which cuStreamIsCapturing entry point to query through, and
 * must match the family of the hook that received hStream: the two disagree on
 * which default stream hStream==0 denotes, so a _ptsz hook querying through the
 * legacy entry point would inspect the wrong stream. Callers that are compiled
 * per-build pass __CUDA_API_IS_PTSZ; the fixed _ptsz hooks pass 1. */
static int stream_is_capturing(CUstream stream, int ptsz) {
  cuda_entry_enum_t sym = ptsz ? CUDA_ENTRY_ENUM(cuStreamIsCapturing_ptsz)
                               : CUDA_ENTRY_ENUM(cuStreamIsCapturing);
  /* Prefer the matching family, but fall back to its sibling if the driver only
   * exports one: the two differ solely in how hStream==0 is resolved, so an
   * imprecise answer still beats assuming "not capturing" and injecting a UVA
   * allocation into a live capture. */
  if (!cuda_library_entry[sym].fn_ptr) {
    sym = ptsz ? CUDA_ENTRY_ENUM(cuStreamIsCapturing)
               : CUDA_ENTRY_ENUM(cuStreamIsCapturing_ptsz);
  }
  // The old driver cuStreamIsCapturing does not exist, and there is no capture function at this time.
  if (!cuda_library_entry[sym].fn_ptr) {
    return 0;
  }
  CUstreamCaptureStatus cap = CU_STREAM_CAPTURE_STATUS_NONE;
  CUresult ret = ((CUresult (*)(CUstream, CUstreamCaptureStatus *))
                  cuda_library_entry[sym].fn_ptr)(stream, &cap);
  if (ret != CUDA_SUCCESS) {
    // When the call result is unsuccessful, it is conservatively judged as being in capture
    return 1;
  }
  return (cap != CU_STREAM_CAPTURE_STATUS_NONE) ? 1 : 0;
}

/* Graph currently being captured into by hStream, or NULL if that cannot be
 * established. `ptsz` selects the entry-point family, for the same reason as in
 * stream_is_capturing().
 *
 * The graph IS available mid-capture: cuStreamGetCaptureInfo documents graph_out
 * as "the graph being captured into ... while the capture sequence is in
 * progress", and the documented way to splice a node into a running capture
 * (get info -> cuGraphAddNode -> cuStreamUpdateCaptureDependencies) depends on
 * exactly that. Only _v2 and _v3 report it; the original entry point yields a
 * status and an id only, so a driver exporting nothing newer leaves the capture
 * unidentifiable. NULL is returned then, and the caller MUST decline to charge
 * the allocation: a charge that cannot be attributed to a graph is one
 * cuStreamEndCapture can never retire, and a permanently overstated usage fails
 * every later allocation. Losing the charge only costs accumulation within that
 * one capture.
 *
 * The status is accepted as anything but NONE, though the API only guarantees
 * graph_out for CU_STREAM_CAPTURE_STATUS_ACTIVE. That is deliberate: this handle
 * is used purely as an identity key to match records and is never dereferenced,
 * and a capture that has gone INVALIDATED still has to have its charge retired.
 * Tightening this to ACTIVE would strand the charge of every capture that failed
 * before its cuStreamEndCapture. */
static CUgraph stream_capture_graph(CUstream stream, int ptsz) {
  CUstreamCaptureStatus cap = CU_STREAM_CAPTURE_STATUS_NONE;
  cuuint64_t id = 0;
  CUgraph graph = NULL;
  const CUgraphNode *deps = NULL;
  size_t num_deps = 0;
  CUresult ret;

  cuda_entry_enum_t v3 = ptsz ? CUDA_ENTRY_ENUM(cuStreamGetCaptureInfo_v3_ptsz)
                              : CUDA_ENTRY_ENUM(cuStreamGetCaptureInfo_v3);
  if (cuda_library_entry[v3].fn_ptr) {
    const CUgraphEdgeData *edges = NULL;
    ret = ((CUresult (*)(CUstream, CUstreamCaptureStatus *, cuuint64_t *, CUgraph *,
                         const CUgraphNode **, const CUgraphEdgeData **, size_t *))
           cuda_library_entry[v3].fn_ptr)(stream, &cap, &id, &graph, &deps, &edges, &num_deps);
    if (ret == CUDA_SUCCESS && cap != CU_STREAM_CAPTURE_STATUS_NONE) {
      return graph;
    }
    return NULL;
  }

  cuda_entry_enum_t v2 = ptsz ? CUDA_ENTRY_ENUM(cuStreamGetCaptureInfo_v2_ptsz)
                              : CUDA_ENTRY_ENUM(cuStreamGetCaptureInfo_v2);
  if (cuda_library_entry[v2].fn_ptr) {
    ret = ((CUresult (*)(CUstream, CUstreamCaptureStatus *, cuuint64_t *, CUgraph *,
                         const CUgraphNode **, size_t *))
           cuda_library_entry[v2].fn_ptr)(stream, &cap, &id, &graph, &deps, &num_deps);
    if (ret == CUDA_SUCCESS && cap != CU_STREAM_CAPTURE_STATUS_NONE) {
      return graph;
    }
  }
  return NULL;
}

/* Returns 1 if this launch enters the GAP path -- the caller MUST then call
 * gap_end() after the real launch (it now owns g_gap_lock[host_index]).
 * Returns 0 to fall through to the token bucket only. */
static int gap_begin(int host_index, CUstream stream) {
  if (host_index < 0) return 0;

  int dc = gap_effective_dc(host_index);
  if (dc <= 0 || dc >= 100) return 0;          /* disabled or full-speed burst */

  int64_t now  = monotonic_ns();
  int64_t last = g_dev_hot[host_index].last_launch_ns;
  if (last != 0 && (now - last) < GAP_THRESHOLD_NS) {
    g_dev_hot[host_index].last_launch_ns = now;        /* BATCH region: token bucket handles it */
    return 0;
  }

  /* Concurrent gap on the same device: don't stack -- let the other thread
   * own the measurement, this launch falls back to the token bucket. */
  if (pthread_mutex_trylock(&g_gap_lock[host_index]) != 0) return 0;

  dc = gap_effective_dc(host_index);           /* re-snapshot under lock */
  if (dc <= 0 || dc >= 100) {
    pthread_mutex_unlock(&g_gap_lock[host_index]);
    return 0;
  }

  if (stream_is_capturing(stream, __CUDA_API_IS_PTSZ)) {
    /* capturing (or query failed) -> don't inject events; if the symbol is
     * absent the driver predates CUDA graphs, so there is nothing to guard. */
    pthread_mutex_unlock(&g_gap_lock[host_index]);
    return 0;
  }
  g_gap_dc[host_index] = dc;

  if (!gap_events_ensure(host_index) ||
      CUDA_INTERNAL_CALL(cuda_library_entry, __CUDA_API_PTSZ(cuEventRecord),
                         g_gap_start[host_index], stream) != CUDA_SUCCESS) {
    pthread_mutex_unlock(&g_gap_lock[host_index]);
    return 0;
  }
  return 1;
}

/* Records the end marker, measures GPU time, injects the duty-cycle sleep.
 * Always stamps last-launch and releases the lock. delay = gpu_ms*(100/dc-1). */
static void gap_end(int host_index, CUstream stream, CUresult launch_ret) {
  uint64_t gpu_us = 0, sleep_us = 0;   /* stay 0 if measurement fails */
  if (launch_ret == CUDA_SUCCESS &&
      CUDA_INTERNAL_CALL(cuda_library_entry, __CUDA_API_PTSZ(cuEventRecord),
                         g_gap_end[host_index], stream) == CUDA_SUCCESS &&
      CUDA_INTERNAL_CALL(cuda_library_entry, cuEventSynchronize,
                         g_gap_end[host_index]) == CUDA_SUCCESS) {
    float gpu_ms = 0.0f;
    if (CUDA_INTERNAL_CALL(cuda_library_entry, cuEventElapsedTime, &gpu_ms,
                           g_gap_start[host_index], g_gap_end[host_index]) == CUDA_SUCCESS
        && gpu_ms > 0.0f) {
      int dc = g_gap_dc[host_index];
      double sleep_ms = (double)gpu_ms * (100.0 / (double)dc - 1.0);
      if (sleep_ms > GAP_MAX_SLEEP_MS) sleep_ms = GAP_MAX_SLEEP_MS;
      gpu_us = (uint64_t)((double)gpu_ms * 1000.0);
      if (sleep_ms > 0.0) {
        sleep_us = (uint64_t)(sleep_ms * 1000.0);
        usleep((useconds_t)sleep_us);
      }
    }
  }
  /* Record before unlocking so the metric reflects this device's gap event.
   * Called unconditionally: count == number of GAP-path entries, even the
   * ones where the cuEvent measurement failed (gpu_us == sleep_us == 0). */
  metrics_record_gap_throttle(host_index, gpu_us, sleep_us);
  g_dev_hot[host_index].last_launch_ns = monotonic_ns();
  pthread_mutex_unlock(&g_gap_lock[host_index]);
}

static batch_t batches[MAX_DEVICE_COUNT / DEVICE_BATCH_SIZE] = {};

static void active_utilization_notifier(int batch_code) {
  pthread_t tid;
  /* If pthread_create fails (typical case: glibc 2.35+ rseq registration
   * blocked by container seccomp profile) the watcher silently never runs,
   * tokens are never replenished, every rate_limiter() goes to sleep, and
   * the symptom looks like "vgpu hangs all CUDA calls". Surface it. */
  int rc = pthread_create(&tid, NULL, utilization_watcher, &batches[batch_code]);
  if (unlikely(rc != 0)) {
    LOGGER(ERROR, "failed to spawn SM watcher for batch %d: %s -- "
                  "compute isolation will not work for devices in this batch",
                  batches[batch_code].batch_code, strerror(rc));
    return;
  }
  char thread_name[32] = {0};
  sprintf(thread_name, "watch_util_bt_%d", batches[batch_code].batch_code);
#ifdef __APPLE__
  pthread_setname_np(thread_name);
#else
  pthread_setname_np(tid, thread_name);
#endif
}

static void init_device_cuda_cores(int *device_count) {
  CUresult ret = CUDA_INTERNAL_CALL(cuda_library_entry, cuDeviceGetCount, device_count);
  if (unlikely(ret)) {
    LOGGER(FATAL, "cuDeviceGetCount call failed, return %d, str: %s", ret, CUDA_ERROR(cuda_library_entry, ret));
  }
  CUdevice device;
  nvmlReturn_t rt;
  for (int cuda_index = 0; cuda_index < *device_count; cuda_index++) {
    ret = CUDA_INTERNAL_CALL(cuda_library_entry, cuDeviceGet, &device, cuda_index);
    if (unlikely(ret)) {
      LOGGER(FATAL, "cuDeviceGet call failed, cuda device %d, return %d, str %s",
            cuda_index, ret, CUDA_ERROR(cuda_library_entry, ret));
    }
    int host_index = get_host_device_index_by_cuda_device(device);
    if (host_index < 0) {
      LOGGER(FATAL, "cuda device %d cannot find the corresponding host device", device);
    }
    int nvml_index = get_nvml_device_index_by_cuda_device(device);
    if (nvml_index < 0) {
      LOGGER(FATAL, "cuda device %d cannot find the corresponding nvml device", device);
    }

    if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2))) {
      rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2, nvml_index, &nvml_devices[host_index]);
    } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex))) {
      rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex, nvml_index, &nvml_devices[host_index]);
    } else {
      rt = NVML_ERROR_FUNCTION_NOT_FOUND;
    }
    if (unlikely(rt)) {
      LOGGER(FATAL, "nvmlDeviceGetHandleByIndex call failed, nvml device %d, return %d, str %s",
                     nvml_index, rt, NVML_ERROR(nvml_library_entry, rt));
    }

    ret = CUDA_INTERNAL_CALL(cuda_library_entry, cuDeviceGetAttribute, &g_sm_num[host_index],
                          CU_DEVICE_ATTRIBUTE_MULTIPROCESSOR_COUNT, device);
    if (unlikely(ret)) {
      LOGGER(FATAL, "can't get processor number, cuda device %d, return %d, str %s",
                     device, ret, CUDA_ERROR(cuda_library_entry, ret));
    }

    ret = CUDA_INTERNAL_CALL(cuda_library_entry, cuDeviceGetAttribute, &g_max_thread_per_sm[host_index],
                          CU_DEVICE_ATTRIBUTE_MAX_THREADS_PER_MULTIPROCESSOR, device);
    if (unlikely(ret)) {
      LOGGER(FATAL, "can't get max thread per processor, cuda device %d, return %d, str %s",
                     device, ret, CUDA_ERROR(cuda_library_entry, ret));
    }
    g_total_cuda_cores[host_index] = (int64_t)g_max_thread_per_sm[host_index] * (int64_t)(g_sm_num[host_index]) * FACTOR;

    LOGGER(VERBOSE, "cuda device %d total cuda cores: %ld", cuda_index, g_total_cuda_cores[host_index]);
  }
}

static void balance_batches(int device_count) {
  if (device_count <= 0) return;
  int batch_size = DEVICE_BATCH_SIZE;
  // When sm watcher is turned on, all devices are merged into one batch, reducing the number of monitoring threads.
  if (g_vgpu_config->sm_watcher) {
    batch_size = MAX_DEVICE_COUNT / 2;
  }
  int batch_count = (device_count + batch_size - 1) / batch_size;
  int base_size = device_count / batch_count;
  int remainder = device_count % batch_count;
  int current_index = 0;
  int current_size = 0;
  for (int i = 0; i < batch_count; i++) {
    current_size = base_size;
    if (i < remainder) {
      current_size++;
    }
    batches[i].start_index = current_index;
    batches[i].end_index = current_index + current_size;
    batches[i].batch_code = i;
    current_index += current_size;
    active_utilization_notifier(i);
  }
}

static void initialization() {
  int ret;
  ret = CUDA_INTERNAL_CALL(cuda_library_entry, cuInit, 0);
  if (unlikely(ret)) {
    LOGGER(ERROR, "cuInit error %s", CUDA_ERROR(cuda_library_entry, (CUresult)ret));
    LOGGER(ERROR, "initialization of sm watcher failed");
    return;
  }
  /* Note: pthread_atfork(child_after_fork) is no longer registered here.
   * It moved to loader.c's load_necessary_data() so that NVML-only and
   * dlsym-only entry paths also register the handler -- covering parent
   * processes that fork before any cuLaunchKernel*. See the block comment
   * above child_after_fork() in this file for the full rationale. */
  int device_count;
  init_device_cuda_cores(&device_count);
  /* Select the SM throttle controller (delta/aimd) before watcher threads
   * start so the function pointer is already pointing at the right impl by
   * the time they hit the dispatch. Safe to run on every initialization()
   * call because pthread_once guards the whole function. */
  sm_controller_init();

  /* Attach the container-wide bucket, if enabled. Deliberately here: after
   * init_device_cuda_cores (so g_total_cuda_cores is known) and after
   * sm_controller_init (so the switch has been read), but before
   * balance_batches spawns any watcher -- so g_sm_node is final before a
   * watcher or a launch can observe it, and no synchronisation is needed on the
   * pointer itself.
   *
   * Guarded on NULL because child_after_fork resets g_init_set, so a forked
   * child re-runs this function. The MAP_SHARED mapping is inherited across
   * fork and is exactly the one we want the child to keep using; mapping again
   * would leak the first mapping and, worse, is simply unnecessary.
   *
   * Failure is not an error. Directory not mounted (an older plugin, or one of
   * the mount sites missed), mounted read-only, /tmp shadowed -- all land here,
   * and all mean "run the way we always have". */
  if (g_dynamic_config.sm_shared_bucket && g_sm_node == NULL) {
    if (map_sm_node_region(&g_sm_node) != 0 || g_sm_node == NULL) {
      g_sm_node = NULL;
      LOGGER(WARNING, "sm_node unavailable, falling back to per-process token bucket");
    } else {
      for (int i = 0; i < MAX_DEVICE_COUNT; i++) {
        /* Observability only -- change_token clamps against the local copy. */
        __atomic_store_n(&g_sm_node->devices[i].total_cuda_cores,
                         g_total_cuda_cores[i], __ATOMIC_RELAXED);
      }
      /* Remember which file we mapped, so shared_regions_check_identity() can notice it
       * being replaced under us. */
      struct stat sb;
      if (stat(SM_NODE_FILE_PATH, &sb) == 0) {
        g_sm_node_ino = sb.st_ino;
        g_sm_node_dev = sb.st_dev;
      }
      /* Name the file. "attached" alone sent the first user of this feature
       * looking for it in the wrong place; the region is an ordinary file and
       * the fastest way to confirm which one is to say where it is. */
      LOGGER(INFO, "sm_node attached at %s (%d bytes, inode %llu): container-wide shared token bucket enabled",
             SM_NODE_FILE_PATH, SM_NODE_FILE_SIZE, (unsigned long long)g_sm_node_ino);
    }
  }

  /* Guarded separately from the region above, and that separation is required.
   * A forked child KEEPS the inherited mapping (it is the same container-wide
   * bucket, which is what we want) but must NOT keep the inherited lock
   * descriptor, so child_after_fork closes it -- and this is where it gets
   * reopened. Folding this into the region's `g_sm_node == NULL` guard would
   * leave every forked child permanently without a lock fd, silently
   * un-centralising sampling for the processes that fork the most. */
  if (g_sm_node != NULL && g_sm_lock_fd < 0) {
    g_sm_lock_fd = open_sm_node_lock();
    if (g_sm_lock_fd < 0) {
      /* Not fatal, not even degraded correctness: every process falls back to
       * sampling for itself, which is what it does today. Refill stays gated
       * by the CAS, so the bucket cannot be over-supplied either way. */
      LOGGER(WARNING, "sm_node sampling lock unavailable; each process will sample independently");
    }
  }

  balance_batches(device_count);
}

int split_str(char *line, char *key, char *value, char d) {
  int index = 0;
  for (index = 0; index < strlen(line) && line[index] != d; index++) {}

  if (index == strlen(line)){
    key[0] = '\0';
    value = '\0';
    return 1;
  }

  int start = 0, i = 0;
  // trim head
  for (; start < index && (line[start] == ' ' || line[start] == '\t'); start++) {}

  for (i = 0; start < index; i++, start++) {
    key[i] = line[start];
  }
  // trim tail
  for (; i > 0 && (key[i - 1] == '\0' || key[i - 1] == '\n' || key[i - 1] == '\t'); i--) {}

  key[i] = '\0';

  start = index + 1;
  i = 0;

  // trim head
  for (; start < strlen(line) && (line[start] == ' ' || line[start] == '\t'); start++) {}

  for (i = 0; start < strlen(line); i++, start++) {
    value[i] = line[start];
  }
  // trim tail
  for (; i > 0 && (value[i - 1] == '\0' || value[i - 1] == '\n' || value[i - 1] == '\t'); i--) {}

  value[i] = '\0';
  return 0;
}

int read_cgroup(char *pid_path, char *cgroup_key, char *cgroup_value) {
  int ret = -1;
  FILE *f = fopen(pid_path, "re");  /* "e" = O_CLOEXEC, prevent fork inheritance */
  if (f == NULL) {
    return ret;
  }
  char buff[256];
  while (fgets(buff, 256, f)) {
    int index = 0;
    for (; index < strlen(buff) && buff[index] != ':'; index++) {}
    if (index == strlen(buff)) {
      continue;
    }
    char key[128], value[128];
    if (split_str(&buff[index + 1], key, value, ':') != 0) {
      continue;
    }
    if (strcmp(key, cgroup_key) == 0) {
      strcpy(cgroup_value, value);
      ret = 0;
      break;
    }
  }
  fclose(f);
  return ret;
}

// Container PID matching method in cgroupv1 mode
int check_device_pid_in_cgroupv1_container(unsigned int device_pid) {
  int ret = -1;
  if (device_pid == 0) {
    return ret;
  }
  char host_pid_path[128];
  char cont_process_cg[256];
  char host_process_cg[256];
  sprintf(host_pid_path, HOST_PROC_CGROUP_PID_PATH, device_pid);
  if (!read_cgroup(PID_SELF_CGROUP_PATH, "memory", cont_process_cg) &&
      !read_cgroup(host_pid_path, "memory", host_process_cg)) {
    LOGGER(DETAIL, "\ncontainer process cgroup: %s\nhost process cgroup: %s", cont_process_cg, host_process_cg);
    if (strstr(host_process_cg, cont_process_cg) != NULL) {
      ret = 0;
    }
  }
  return ret;
}

// Container PID matching method in cgroupv2 mode
int check_device_pid_in_cgroupv2_container(unsigned int device_pid) {
  int ret = -1;
  if (device_pid == 0) {
    return ret;
  }
  // TODO May I need to check if there are zombie processes?
  char host_pid_path[128];
  sprintf(host_pid_path, HOST_PROC_CGROUP_PID_PATH, device_pid);
  if (file_exist(host_pid_path) != 0) {
    return ret;
  }

  FILE *fp = fopen(host_pid_path, "re");  /* "e" = O_CLOEXEC, prevent fork inheritance */
  if (!fp) {
    LOGGER(VERBOSE, "read host pid path %s failed: %s", host_pid_path, strerror(errno));
    return ret;
  }

  char buff[FILENAME_MAX];
  while (fgets(buff, FILENAME_MAX, fp)) {
    size_t len = strlen(buff);
    if (len > 0 && buff[len - 1] == '\n') {
      buff[len - 1] = '\0';
    }
    if (strcmp(buff, "0::/") == 0) {
      ret = 0;
      break;
    }
  }
  fclose(fp);
  return ret;
}

static int int_compare(const void *a, const void *b) {
  const int *pa = (const int *)a;
  const int *pb = (const int *)b;
  return (*pa > *pb) - (*pa < *pb);
}

#define PID_LINEAR_SEARCH_THRESHOLD 20

static inline int pid_linear_search(unsigned int key, const int *arr, int size) {
  for (int i = 0; i < size; i++) {
    if ((unsigned int)arr[i] == key) {
      return 1;
    }
  }
  return 0;
}

int check_device_pid_in_ordered_container_pids(unsigned int device_pid, const int *container_pids, int pids_size) {
  if (device_pid == 0 || !container_pids || pids_size <= 0) {
    return -1;
  }
  int found;
  if (pids_size <= PID_LINEAR_SEARCH_THRESHOLD) {
    found = pid_linear_search(device_pid, container_pids, pids_size);
  } else {
    found = (bsearch(&device_pid, container_pids, (size_t)pids_size, sizeof(int), int_compare) != NULL);
  }
  return found ? 0 : -1;
}

int check_device_pid_in_local_container_pid(unsigned int device_pid) {
  int ret = -1;
  if (device_pid == 0) {
    return ret;
  }
  // Check if PID exists in the container.
  if (pid_exist(device_pid) != 0) {
    return ret;
  }
  // Check if PID exists in the current container namespace.
  if (device_pid_in_same_container(device_pid) != 0) {
    return ret;
  }
  // Check if the process is using GPU.
  if (library_exists_in_process_maps("nvidia", device_pid) == 0) {
    ret = 0;
  }
  return ret;
}

void accumulate_used_memory(size_t *used_memory, nvmlProcessInfo_t *pids_on_device, unsigned int size_on_device) {
  unsigned int i;
  int matchOpenKernel = 0;
  int openKernelMode = (g_vgpu_config->compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE;

  if (size_on_device == 0) {
    // If there are no processes running on the device, quickly skip them.
  } else if ((g_vgpu_config->compatibility_mode & CLIENT_COMPATIBILITY_MODE) == CLIENT_COMPATIBILITY_MODE) {
    int pids_size = MAX_PIDS;
    int pids_on_container[pids_size];
    // Normally, the server has already sorted the PID list during device registration, so there is no need to sort it again here.
    get_container_pids_by_filepath(CONTAINER_PIDS_CONFIG_FILE_PATH, pids_on_container, &pids_size, 0);
    if (unlikely(pids_size == 0)) {
      LOGGER(FATAL, "unable to find registered container process");
    }
    int matchClientMode = 0;
    for (i = 0; i < size_on_device; i++) {
      if (!matchOpenKernel && check_device_pid_in_ordered_container_pids(pids_on_device[i].pid, pids_on_container, pids_size) == 0) {
        LOGGER(VERBOSE, "process id %d use gpu memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        matchClientMode = 1;
        *used_memory += pids_on_device[i].usedGpuMemory;
      } else if (!matchClientMode && openKernelMode && check_device_pid_in_local_container_pid(pids_on_device[i].pid) == 0) {
        LOGGER(VERBOSE, "process id %d use gpu memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        matchOpenKernel = 1;
        *used_memory += pids_on_device[i].usedGpuMemory;
      }
    }
  } else if ((g_vgpu_config->compatibility_mode & CGROUPV2_COMPATIBILITY_MODE) == CGROUPV2_COMPATIBILITY_MODE) {
    int matchCGroupV2Mode = 0;
    for (i = 0; i < size_on_device; i++) {
      if (!matchOpenKernel && check_device_pid_in_cgroupv2_container(pids_on_device[i].pid) == 0) {
        LOGGER(VERBOSE, "process id %d use gpu memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        matchCGroupV2Mode = 1;
        *used_memory += pids_on_device[i].usedGpuMemory;
      } else if (!matchCGroupV2Mode && openKernelMode && check_device_pid_in_local_container_pid(pids_on_device[i].pid) == 0) {
        LOGGER(VERBOSE, "process id %d use gpu memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        matchOpenKernel = 1;
        *used_memory += pids_on_device[i].usedGpuMemory;
      }
    }
  } else if ((g_vgpu_config->compatibility_mode & CGROUPV1_COMPATIBILITY_MODE) == CGROUPV1_COMPATIBILITY_MODE) {
    int matchCGroupV1Mode = 0;
    for (i = 0; i < size_on_device; i++) {
      if (!matchOpenKernel && check_device_pid_in_cgroupv1_container(pids_on_device[i].pid) == 0) {
        LOGGER(VERBOSE, "process id %d use gpu memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        matchCGroupV1Mode = 1;
        *used_memory += pids_on_device[i].usedGpuMemory;
      } else if (!matchCGroupV1Mode && openKernelMode && check_device_pid_in_local_container_pid(pids_on_device[i].pid) == 0) {
        LOGGER(VERBOSE, "process id %d use gpu memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        matchOpenKernel = 1;
        *used_memory += pids_on_device[i].usedGpuMemory;
      }
    }
  } else if (openKernelMode) {
    for (i = 0; i < size_on_device; i++) {
      if (check_device_pid_in_local_container_pid(pids_on_device[i].pid) == 0) {
        LOGGER(VERBOSE, "process id %d use gpu memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        matchOpenKernel = 1;
        *used_memory += pids_on_device[i].usedGpuMemory;
      }
    }
  } else if (g_vgpu_config->compatibility_mode == HOST_COMPATIBILITY_MODE) {
    // Host Mode does not verify PID
    for (i = 0; i < size_on_device; i++) {
      LOGGER(VERBOSE, "process id %d use gpu memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
      *used_memory += pids_on_device[i].usedGpuMemory;
    }
  } else {
    LOGGER(FATAL, "unsupported environment compatibility mode: %d", g_vgpu_config->compatibility_mode);
  }

}

void get_used_gpu_memory_by_device(void *arg, nvmlDevice_t device) {
  size_t *used_memory = arg;
  nvmlProcessInfo_t pids_on_device[MAX_PIDS];
  unsigned int size_on_device = MAX_PIDS;
  nvmlReturn_t ret = NVML_ERROR_FUNCTION_NOT_FOUND;

  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses,
                             device, &size_on_device, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2,
                             device, &size_on_device, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3,
                             device, &size_on_device, pids_on_device);
  }
  if (unlikely(ret)) {
    *used_memory = 0;
    LOGGER(ERROR, "nvmlDeviceGetComputeRunningProcesses call failed, return: %d, str: %s",
                   ret, NVML_ERROR(nvml_library_entry, ret));
    return;
  }
  accumulate_used_memory(used_memory, pids_on_device, size_on_device);
  unsigned int compute_pids_count = size_on_device;

  size_on_device = MAX_PIDS;
  nvmlProcessInfo_t graphic_pids_on_device[MAX_PIDS];

  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses,
                           device, &size_on_device, graphic_pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v2))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v2,
                           device, &size_on_device, graphic_pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v3))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v3,
                           device, &size_on_device, graphic_pids_on_device);
  } else {
    ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  }
  if (unlikely(ret)) {
    LOGGER(ERROR, "nvmlDeviceGetGraphicsRunningProcesses call failed, return: %d, str: %s",
                   ret, NVML_ERROR(nvml_library_entry, ret));
    goto DONE;
  }

  /* Deduplicate compute vs graphics PIDs.
   *
   * NVML's nvmlProcessInfo_t.usedGpuMemory is the per-PROCESS total memory
   * usage on the device, not per-context. A process that owns BOTH a compute
   * context (CUDA / OpenCL) and a graphics context (Vulkan / OpenGL / DirectX)
   * shows up in both lists with the SAME usedGpuMemory value. Without dedup,
   * accumulating both lists doubles that process's contribution and inflates
   * `used` to ~2x reality.
   *
   * This is a real production hazard for CUDA + render mixed apps such as
   * Isaac Sim, Omniverse, UE5 with CUDA inference plugins, or any app that
   * pairs CUDA work with an OpenGL/Vulkan visualisation. Filter out graphics
   * entries whose PID is already accounted for in the compute list. */
  unsigned int graphic_unique_count = 0;
  for (unsigned int i = 0; i < size_on_device; i++) {
    int already_seen = 0;
    for (unsigned int j = 0; j < compute_pids_count; j++) {
      if (graphic_pids_on_device[i].pid == pids_on_device[j].pid) {
        already_seen = 1;
        break;
      }
    }
    if (!already_seen) {
      if (graphic_unique_count != i) {
        graphic_pids_on_device[graphic_unique_count] = graphic_pids_on_device[i];
      }
      graphic_unique_count++;
    } else {
      LOGGER(DETAIL, "process id %d also owns a graphics context; skipped to "
                     "avoid double-counting", graphic_pids_on_device[i].pid);
    }
  }
  accumulate_used_memory(used_memory, graphic_pids_on_device, graphic_unique_count);

DONE:
  LOGGER(VERBOSE, "total used memory: %zu", *used_memory);
}

void get_used_gpu_memory(void *arg, CUdevice device) {
  size_t *used_memory = arg;

  int nvml_index = get_nvml_device_index_by_cuda_device(device);
  if (nvml_index < 0) {
    *used_memory = 0;
    LOGGER(ERROR, "cuda device %d cannot find the corresponding nvml devices", device);
    return;
  }

  nvmlDevice_t dev;
  nvmlReturn_t ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2, nvml_index, &dev);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex, nvml_index, &dev);
  }
  if (unlikely(ret)) {
    *used_memory = 0;
    LOGGER(ERROR, "nvmlDeviceGetHandleByIndex call failed, nvml device: %d, return: %d, str: %s",
                   nvml_index, ret, NVML_ERROR(nvml_library_entry, ret));
    return;
  }

  get_used_gpu_memory_by_device((void *)used_memory, dev);
}

static nvmlReturn_t get_process_utilization_samples(
  nvmlDevice_t device, nvmlProcessUtilizationSample_t *processes_sample,
  unsigned int *out_count, uint64_t last_seen) {

  unsigned int processes_num = *out_count;
  nvmlReturn_t res = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetProcessUtilization,
                                        device, processes_sample, &processes_num, last_seen);
  if (res == NVML_SUCCESS) {
    *out_count = processes_num;
    return NVML_SUCCESS;
  }
  if (res != NVML_ERROR_NOT_SUPPORTED) {
    return res;
  }
  // Try using nvmlDeviceGetProcessesUtilizationInfo when nvmlDeviceGetProcessUtilization is not supported to improve compatibility.
  if (!NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetProcessesUtilizationInfo)){
    return res;
  }

  nvmlProcessUtilizationInfo_v1_t local_samples[*out_count];
  nvmlProcessesUtilizationInfo_v1_t info;
  info.version = nvmlProcessesUtilizationInfo_v1;
  info.processSamplesCount = 0;
  info.lastSeenTimeStamp = last_seen;
  info.procUtilArray = NULL;

  res = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetProcessesUtilizationInfo, device, (nvmlProcessesUtilizationInfo_t *)&info);
  if (res == NVML_ERROR_INSUFFICIENT_SIZE) {
    info.procUtilArray = local_samples;
    info.processSamplesCount = *out_count;
    res = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetProcessesUtilizationInfo, device, (nvmlProcessesUtilizationInfo_t *)&info);
  }
  if (res != NVML_SUCCESS) {
    return res;
  }

  if (info.processSamplesCount > *out_count) {
    info.processSamplesCount = *out_count;
  }
  int i;
  for (i = 0; i < (int)info.processSamplesCount; i++) {
    processes_sample[i].pid = local_samples[i].pid;
    processes_sample[i].timeStamp = local_samples[i].timeStamp;
    processes_sample[i].smUtil = local_samples[i].smUtil;
    processes_sample[i].memUtil = local_samples[i].memUtil;
    processes_sample[i].encUtil = local_samples[i].encUtil;
    processes_sample[i].decUtil = local_samples[i].decUtil;
  }
  *out_count = info.processSamplesCount;
  return NVML_SUCCESS;
}

static nvmlReturn_t get_gpu_process_from_local_nvml_driver(
  utilization_t *top_result, nvmlProcessUtilizationSample_t *processes_sample,
  unsigned int *processes_size, int cuda_index, nvmlDevice_t dev) {
  struct timeval cur, prev;
  nvmlProcessInfo_t pids_on_device[MAX_PIDS];
  unsigned int running_processes = MAX_PIDS;
  int host_index = get_host_device_index_by_cuda_device(cuda_index);

  metrics_record_nvml_fallback(host_index);

  nvmlReturn_t ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses,
                             dev, &running_processes, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2,
                             dev, &running_processes, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3,
                             dev, &running_processes, pids_on_device);
  }
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetComputeRunningProcesses can't get pids on cuda device %d, "
                 "return %d, str: %s", cuda_index, ret, NVML_ERROR(nvml_library_entry, ret));
    return ret;
  }

  top_result->sys_process_num = running_processes;

  if (running_processes == 0) {
    running_processes = MAX_PIDS;
    nvmlProcessInfo_t graphic_pids_on_device[MAX_PIDS];
    if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses))) {
      ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses,
                               dev, &running_processes, graphic_pids_on_device);
    } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v2))) {
      ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v2,
                               dev, &running_processes, graphic_pids_on_device);
    } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v3))) {
      ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v3,
                               dev, &running_processes, graphic_pids_on_device);
    } else {
      ret = NVML_ERROR_FUNCTION_NOT_FOUND;
    }
    if (likely(ret == NVML_SUCCESS)) {
      top_result->sys_process_num = running_processes;
    }
  }

  gettimeofday(&cur, NULL);
  struct timeval temp = {1, 0};
  timersub(&cur, &temp, &prev);
  uint64_t microsec = (uint64_t)prev.tv_sec * 1000000ULL + prev.tv_usec;
  top_result->checktime = microsec;

  ret = get_process_utilization_samples(dev, processes_sample, processes_size, microsec);
  if (unlikely(ret)) {
    // Frequent calls to nvmlDeviceGetProcessUtilization may result in the return of NVML_ERROR_NOT_FOUND,
    // which is a normal phenomenon and should be avoided from printing these invalid logs.
    if (ret != NVML_ERROR_NOT_FOUND) {
      LOGGER(VERBOSE, "nvmlDeviceGetProcessUtilization can't get process utilization on cuda device %d, "
                      "return: %d, str: %s", cuda_index, ret, NVML_ERROR(nvml_library_entry, ret));
    }
    return ret;
  }

  // When using open source kernel modules, nvmlDeviceGetComputeRunningProcesses can only
  // query processes in the container namespace, while nvmlDeviceGetProcessUtilization
  // can query global processes, so it may need to be updated to the global process count here.
  if ((g_vgpu_config->compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
    if (*processes_size > running_processes) {
       top_result->sys_process_num = *processes_size;
    }
  }

  return NVML_SUCCESS;
}

int is_expired(unsigned long long lastTs) {
    struct timeval cur;
    gettimeofday(&cur, NULL);
    unsigned long long cur_us = cur.tv_sec * 1000000 + cur.tv_usec;
    if (cur_us <= lastTs) return 0;
    return (cur_us - lastTs) >= 5000000ULL; // 5,000,000 microsecond
}

static nvmlReturn_t get_gpu_process_from_external_watcher(
  utilization_t *top_result, nvmlProcessUtilizationSample_t *processes_sample,
  unsigned int *processes_size, int cuda_index, int host_index, nvmlDevice_t dev) {
  int fd = device_util_read_lock(host_index);
  if (fd < 0) {
    metrics_record_watcher_miss(host_index, METRICS_WATCHER_LOCK_MISS);
    LOGGER(WARNING, "failed to acquire read lock for host device %d, fallback to nvml driver", host_index);
    return get_gpu_process_from_local_nvml_driver(top_result, processes_sample, processes_size, cuda_index, dev);
  }
  int expired = is_expired(g_device_util->devices[host_index].lastSeenTimeStamp);
  if (expired) {
    goto DONE;
  }
  unsigned int actual_size = g_device_util->devices[host_index].process_util_samples_size;
  unsigned int copy_size = (*processes_size < actual_size) ? *processes_size : actual_size;
  if (copy_size > 0 && processes_sample != NULL) {
    memcpy(processes_sample, g_device_util->devices[host_index].process_util_samples, copy_size * sizeof(nvmlProcessUtilizationSample_t));
  }
  *processes_size = copy_size;

  if (g_device_util->devices[host_index].compute_processes_size >= g_device_util->devices[host_index].graphics_processes_size) {
    top_result->sys_process_num = g_device_util->devices[host_index].compute_processes_size;
  } else {
    top_result->sys_process_num = g_device_util->devices[host_index].graphics_processes_size;
  }
  top_result->checktime = (uint64_t)g_device_util->devices[host_index].lastSeenTimeStamp;

DONE:
  device_util_unlock(fd, host_index);
  if (expired) {
     metrics_record_watcher_miss(host_index, METRICS_WATCHER_EXPIRED);
     LOGGER(VERBOSE, "host device %d process utilization time window timeout detected, fallback to nvml driver", host_index);
     return get_gpu_process_from_local_nvml_driver(top_result, processes_sample, processes_size, cuda_index, dev);
  }
  return NVML_SUCCESS;
}

static void get_used_gpu_utilization(void *arg, int cuda_index, int host_index, nvmlDevice_t dev) {
  utilization_t *top_result = (utilization_t *)arg;
  nvmlProcessUtilizationSample_t processes_sample[MAX_PIDS];
  unsigned int processes_num = MAX_PIDS;

  nvmlReturn_t ret;
  if (g_vgpu_config->sm_watcher && g_device_util != NULL) {
    ret = get_gpu_process_from_external_watcher(top_result, processes_sample, &processes_num, cuda_index, host_index, dev);
  } else {
    ret = get_gpu_process_from_local_nvml_driver(top_result, processes_sample, &processes_num, cuda_index, dev);
  }
  if (unlikely(ret)) return;

  top_result->user_current = 0;
  top_result->sys_current = 0;
  // In some versions of the driver, it is not possible to rely on nvmlDeviceGetComputeRunningProcesses
  // to obtain the number of processes in the entire namespace. Therefore, it is necessary to aggregate
  // the current container process number and calculate the difference with the global process number
  // top_result ->sys_process_num to obtain the accurate number of external processes.
  unsigned int current_processes_num = 0;

  int sm_util = 0;
  int codec_util = 0;

  int i;
  int matchOpenKernel = 0;
  int openKernelMode = (g_vgpu_config->compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE;

  if (processes_num == 0) {
    // If there are no processes running on the device, quickly skip them.
  } else if ((g_vgpu_config->compatibility_mode & CLIENT_COMPATIBILITY_MODE) == CLIENT_COMPATIBILITY_MODE) {
    int pids_size = MAX_PIDS;
    int pids_on_container[MAX_PIDS];
    // Normally, the server has already sorted the PID list during device registration, so there is no need to sort it again here.
    get_container_pids_by_filepath(CONTAINER_PIDS_CONFIG_FILE_PATH, pids_on_container, &pids_size, 0);
    if (likely(pids_size > 0)) {
      int matchClientMode = 0;
      for (i = 0; i < processes_num; i++) {
        if (processes_sample[i].timeStamp >= top_result->checktime) {
          top_result->valid = 1;
          sm_util = GET_VALID_VALUE(processes_sample[i].smUtil);
          codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) + GET_VALID_VALUE(processes_sample[i].decUtil);
          codec_util = CODEC_NORMALIZE(codec_util);
          top_result->sys_current += sm_util + codec_util;
          if (!matchOpenKernel && check_device_pid_in_ordered_container_pids(processes_sample[i].pid, pids_on_container, pids_size) == 0) {
            matchClientMode = 1;
            top_result->user_current += sm_util + codec_util;
            current_processes_num++;
          } else if (!matchClientMode && openKernelMode && check_device_pid_in_local_container_pid(processes_sample[i].pid) == 0) {
            matchOpenKernel = 1;
            top_result->user_current += sm_util + codec_util;
            current_processes_num++;
          }
        }
      }
    }
  } else if ((g_vgpu_config->compatibility_mode & CGROUPV2_COMPATIBILITY_MODE) == CGROUPV2_COMPATIBILITY_MODE) {
    int matchCGroupV2Mode = 0;
    for (i = 0; i < processes_num; i++) {
      if (processes_sample[i].timeStamp >= top_result->checktime) {
        top_result->valid = 1;
        sm_util = GET_VALID_VALUE(processes_sample[i].smUtil);
        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) + GET_VALID_VALUE(processes_sample[i].decUtil);
        codec_util = CODEC_NORMALIZE(codec_util);
        top_result->sys_current += sm_util + codec_util;
        if (!matchOpenKernel && check_device_pid_in_cgroupv2_container(processes_sample[i].pid) == 0) {
          matchCGroupV2Mode = 1;
          top_result->user_current += sm_util + codec_util;
          current_processes_num++;
        } else if (!matchCGroupV2Mode && openKernelMode && check_device_pid_in_local_container_pid(processes_sample[i].pid) == 0) {
          matchOpenKernel = 1;
          top_result->user_current += sm_util + codec_util;
          current_processes_num++;
        }
      }
    }
  } else if ((g_vgpu_config->compatibility_mode & CGROUPV1_COMPATIBILITY_MODE) == CGROUPV1_COMPATIBILITY_MODE) {
    int matchCGroupV1Mode = 0;
    for (i = 0; i < processes_num; i++) {
      if (processes_sample[i].timeStamp >= top_result->checktime) {
        top_result->valid = 1;
        sm_util = GET_VALID_VALUE(processes_sample[i].smUtil);
        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) + GET_VALID_VALUE(processes_sample[i].decUtil);
        codec_util = CODEC_NORMALIZE(codec_util);
        top_result->sys_current += sm_util + codec_util;
        if (!matchOpenKernel && check_device_pid_in_cgroupv1_container(processes_sample[i].pid) == 0) {
          matchCGroupV1Mode = 1;
          top_result->user_current += sm_util + codec_util;
          current_processes_num++;
        } else if (!matchCGroupV1Mode && openKernelMode && check_device_pid_in_local_container_pid(processes_sample[i].pid) == 0) {
          matchOpenKernel = 1;
          top_result->user_current += sm_util + codec_util;
          current_processes_num++;
        }
      }
    }
  } else if (openKernelMode) {
    for (i = 0; i < processes_num; i++) {
      if (processes_sample[i].timeStamp >= top_result->checktime) {
        top_result->valid = 1;
        sm_util = GET_VALID_VALUE(processes_sample[i].smUtil);
        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) + GET_VALID_VALUE(processes_sample[i].decUtil);
        codec_util = CODEC_NORMALIZE(codec_util);
        top_result->sys_current += sm_util + codec_util;
        if (check_device_pid_in_local_container_pid(processes_sample[i].pid) == 0) {
          matchOpenKernel = 1;
          top_result->user_current += sm_util + codec_util;
          current_processes_num++;
        }
      }
    }
  } else if (g_vgpu_config->compatibility_mode == HOST_COMPATIBILITY_MODE) {
    for (i = 0; i < processes_num; i++) {
      if (processes_sample[i].timeStamp >= top_result->checktime) {
        top_result->valid = 1;
        sm_util = GET_VALID_VALUE(processes_sample[i].smUtil);
        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) + GET_VALID_VALUE(processes_sample[i].decUtil);
        codec_util = CODEC_NORMALIZE(codec_util);
        top_result->sys_current += sm_util + codec_util;
        top_result->user_current += sm_util + codec_util;
        current_processes_num++;
      }
    }
  } else {
    LOGGER(FATAL, "unknown environment compatibility mode: %d", g_vgpu_config->compatibility_mode);
  }

  top_result->external_process_num = current_processes_num >= top_result->sys_process_num ? 0 : top_result->sys_process_num - current_processes_num;
  if ((g_util_log_tick[host_index]++ % WATCHER_UTIL_LOG_STRIDE) == 0) {
    g_util_log_tick[host_index] = 1;
    LOGGER(VERBOSE, "cuda device: %d, host device: %d, sys util: %d, user util: %d (1/%d sampled)",
           cuda_index, host_index, top_result->sys_current, top_result->user_current,
           WATCHER_UTIL_LOG_STRIDE);
  }
}

/** hook entrypoint */
CUresult cuDriverGetVersion(int *driverVersion) {
  CUresult ret;

  load_necessary_data();
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDriverGetVersion, driverVersion);
  if (unlikely(ret)) {
    goto DONE;
  }
  init_devices_mapping();
//  pthread_once(&g_init_set, initialization);
DONE:
  return ret;
}

CUresult cuInit(unsigned int flag) {
  CUresult ret;

  load_necessary_data();
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuInit, flag);
  if (unlikely(ret)) {
    goto DONE;
  }
  init_devices_mapping();
  pthread_once(&g_init_set, initialization);
DONE:
  return ret;
}

CUresult cuGetProcAddress(const char *symbol, void **pfn, int cudaVersion,
                          cuuint64_t flags) {
  CUresult ret;
  load_necessary_data();
  LOGGER(DETAIL, "cuGetProcAddress symbol: %s, version: %d", symbol, cudaVersion);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGetProcAddress, symbol, pfn,
                         cudaVersion, flags);
  if (likely(ret == CUDA_SUCCESS)) {
    init_devices_mapping();
    pthread_once(&g_init_set, initialization);

    /*
     * Primary routing. *pfn is the entry point real libcuda chose from
     * `cudaVersion` and `flags`; naming it identifies that function exactly, so
     * the hook we substitute carries its ABI by construction.
     *
     * This runs ahead of the self-lookup case below, and covers it: when the
     * caller asks for cuGetProcAddress, the driver answers with whichever
     * version its own rules select, and routing follows that choice instead of
     * assuming this hook's arity. That is strictly more accurate than a name
     * comparison, which cannot see which version was picked.
     */
    const char *resolved = NULL;
    void *hook_fn = lookup_cuda_hook_ptr(*pfn, symbol, &resolved);
    if (hook_fn) {
      LOGGER(VERBOSE, "cuGetProcAddress: %s (v=%d) -> %s hook", symbol, cudaVersion, resolved);
      *pfn = hook_fn;
      goto DONE;
    }

    /*
     * Self-lookup safety net, reached only when routing produced no hook for
     * cuGetProcAddress -- an unrecognised pointer, or a version of the resolver
     * we do not wrap.
     *
     * This one symbol cannot be allowed to fall through to either of the paths
     * below. Handing back the driver's own resolver means every later lookup
     * the caller performs through it bypasses cuda_hooks_entry[] entirely, and
     * the whole hook layer goes quiet with nothing to show for it. Substituting
     * our 4-arg wrapper keeps the chain alive; matching this hook's own arity
     * is the safe choice precisely because routing has already declined to name
     * a version. (The 5-arg case is handled in cuGetProcAddress_v2 below.)
     */
    if (strcmp(symbol, "cuGetProcAddress") == 0) {
      LOGGER(VERBOSE, "cuGetProcAddress: substitute self-lookup with our "
                      "4-arg wrapper to preserve hook coverage");
      *pfn = (void *)cuGetProcAddress;
      goto DONE;
    }

    if (resolved) {
      /* Identified, and we hook no version of it. Keep the driver's pointer:
       * falling through would let a base-named hook be substituted for a
       * variant whose ABI it does not have. */
      note_unhooked_symbol(resolved);
      goto DONE;
    }

    /*
     * Name-based fallback. Reached whenever the driver did NOT hand back the
     * pointer this build knows under the requested name -- because it picked a
     * different version (cuCtxCreate -> _v4), because flags selected a variant
     * (cuLaunchKernel -> _ptsz), or because the pointer is not in our table at
     * all. Those are exactly the cases where the name alone cannot tell us the
     * ABI, so the older, more cautious rules below decide.
     *
     * ABI-conflict defense (MUST run before the lib_control / hooks_entry
     * substitution below). Real libcuda's cuGetProcAddress has already
     * written an ABI-correct function pointer to *pfn based on `cudaVersion`
     * (e.g. cuCtxCreate_v4 for cudaVersion >= 13000, cuCtxCreate_v2 for
     * cudaVersion < 13000). If we then overwrite *pfn with whatever our
     * libvgpu-control.so exports under the unversioned base name, we bind
     * the CUDA 13 caller to our 3-arg v2 wrapper (or vice-versa) and the
     * parameter frame is misaligned on the very next call.
     *
     * Safe for these conflict families because vgpu-manager does not hook
     * them - the unversioned wrappers in cuda_originals.c are forward-only,
     * so "keep the real libcuda pointer" does not lose any instrumentation.
     * Contrast with cuGetProcAddress above which MUST be substituted with
     * our wrapper to keep the hook chain alive for subsequent lookups.
     */
    if (is_abi_conflict_base(symbol)) {
      LOGGER(VERBOSE, "cuGetProcAddress: keep libcuda pointer for ABI-conflict "
                      "family %s (cudaVersion=%d)", symbol, cudaVersion);
      goto DONE;
    }

    /*
     * Version-aware substitution: cuGraphExecUpdate has two ABIs (4-arg v1
     * pre-12.0, 3-arg _v2 from 12.0+). The real cuGetProcAddress picked the
     * caller-appropriate variant based on cudaVersion; we MUST substitute
     * the matching ABI's hook so the frame the caller pushes lines up with
     * what our hook pops. Substituting the wrong ABI hook would corrupt the
     * stack on the very next call. Unlike is_abi_conflict_base() (which
     * gives up and keeps the libcuda pointer), here we WANT interception so
     * the cache-refresh logic runs -- we just route to the right hook.
     */
    if (strcmp(symbol, "cuGraphExecUpdate") == 0) {
      *pfn = (cudaVersion >= 12000)
               ? (void *)cuGraphExecUpdate_v2
               : (void *)cuGraphExecUpdate;
      LOGGER(VERBOSE, "cuGetProcAddress: cuGraphExecUpdate -> %s hook "
                      "(cudaVersion=%d)",
                      (cudaVersion >= 12000) ? "v2(3-arg)" : "v1(4-arg)",
                      cudaVersion);
      goto DONE;
    }

    if (lib_control) {
      hook_fn = real_dlsym(lib_control, symbol);
      if (likely(hook_fn)) {
        LOGGER(DETAIL, "cuGetProcAddress matched symbol: %s", symbol);
        *pfn = hook_fn;
        goto DONE;
      }
    } else {
      for (int i = 0; i < cuda_hook_nums; i++) {
        if (!strcmp(symbol, cuda_hooks_entry[i].name)) {
          LOGGER(VERBOSE, "cuGetProcAddress matched symbol: %s", symbol);
          *pfn = cuda_hooks_entry[i].fn_ptr;
          goto DONE;
        }
      }
    }

    /* Known symbol, no hook of ours: leave the driver pointer in place
     * and leave a trail, since this is how a version we do not intercept
     * shows up once a driver starts handing it out. */
    note_unhooked_symbol(symbol);
  }
DONE:
  return ret;
}

CUresult cuGetProcAddress_v2(const char *symbol, void **pfn, int cudaVersion,
                             cuuint64_t flags, void *symbolStatus) {
  CUresult ret;
  load_necessary_data();
  LOGGER(DETAIL, "cuGetProcAddress_v2 symbol: %s, version: %d", symbol, cudaVersion);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGetProcAddress_v2, symbol, pfn,
                         cudaVersion, flags, symbolStatus);
  if (likely(ret == CUDA_SUCCESS)) {
    init_devices_mapping();
    pthread_once(&g_init_set, initialization);

    /* Same driver-pointer routing as in cuGetProcAddress above, and for the
     * same reason it comes first: the driver's choice is more precise than any
     * name comparison we could make here. */
    const char *resolved = NULL;
    void *hook_fn = lookup_cuda_hook_ptr(*pfn, symbol, &resolved);
    if (hook_fn) {
      LOGGER(VERBOSE, "cuGetProcAddress_v2: %s (v=%d) -> %s hook", symbol, cudaVersion, resolved);
      *pfn = hook_fn;
      goto DONE;
    }

    /*
     * Self-lookup safety net - see cuGetProcAddress (4-arg hook) above for the
     * full rationale. Here the caller is in the 5-arg v2 hook, so their
     * PFN_cuGetProcAddress type has the v2 5-arg ABI (or they explicitly
     * declared 5-arg). Substitute our own v2 entry point so the caller's
     * subsequent indirect lookups re-enter our hook.
     */
    if (strcmp(symbol, "cuGetProcAddress") == 0 && cudaVersion >= 12000) {
      LOGGER(VERBOSE, "cuGetProcAddress_v2: substitute cuGetProcAddress "
                      "self-lookup with our 5-arg _v2 wrapper");
      *pfn = (void *)cuGetProcAddress_v2;
      goto DONE;
    }

    if (resolved) {
      note_unhooked_symbol(resolved);
      goto DONE;
    }

    /* Same ABI-conflict defense as in cuGetProcAddress above - must run
     * BEFORE the lib_control / hooks_entry substitution. See the comment
     * there for the full rationale. */
    if (is_abi_conflict_base(symbol)) {
      LOGGER(VERBOSE, "cuGetProcAddress_v2: keep libcuda pointer for "
                      "ABI-conflict family %s (cudaVersion=%d)", symbol, cudaVersion);
      goto DONE;
    }

    /* Same version-aware substitution as in cuGetProcAddress above -- see
     * the comment there for the full rationale. cuGraphExecUpdate's v1/v2
     * ABIs differ in arity (4 vs 3), so the substituted hook must match
     * the variant the real cuGetProcAddress_v2 selected by cudaVersion. */
    if (strcmp(symbol, "cuGraphExecUpdate") == 0) {
      *pfn = (cudaVersion >= 12000)
               ? (void *)cuGraphExecUpdate_v2
               : (void *)cuGraphExecUpdate;
      LOGGER(VERBOSE, "cuGetProcAddress_v2: cuGraphExecUpdate -> %s hook "
                      "(cudaVersion=%d)",
                      (cudaVersion >= 12000) ? "v2(3-arg)" : "v1(4-arg)",
                      cudaVersion);
      goto DONE;
    }

    if (lib_control) {
      hook_fn = real_dlsym(lib_control, symbol);
      if (likely(hook_fn)) {
        LOGGER(DETAIL, "cuGetProcAddress_v2 matched symbol: %s", symbol);
        *pfn = hook_fn;
        goto DONE;
      }
    } else {
      for (int i = 0; i < cuda_hook_nums; i++) {
        if (!strcmp(symbol, cuda_hooks_entry[i].name)) {
          LOGGER(VERBOSE, "cuGetProcAddress_v2 matched symbol: %s", symbol);
          *pfn = cuda_hooks_entry[i].fn_ptr;
          goto DONE;
        }
      }
    }

    /* Known symbol, no hook of ours: leave the driver pointer in place
     * and leave a trail, since this is how a version we do not intercept
     * shows up once a driver starts handing it out. */
    note_unhooked_symbol(symbol);
  }
DONE:
  return ret;
}

CUresult cuMemAllocManaged(CUdeviceptr *dptr, size_t bytesize, unsigned int flags) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  int host_index = -1;
  memory_path_t path;
  /* NULL-arg fast path; same rationale as _cuMemAlloc above. */
  if (unlikely(dptr == NULL)) {
    goto CALL;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  path = prepare_memory_allocation(device, bytesize, 1, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
  if (path == MEMORY_PATH_UVA) {
    flags = CU_MEM_ATTACH_GLOBAL;
  }
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, flags);
  if (likely(ret == CUDA_SUCCESS)) {
    if (flags == CU_MEM_ATTACH_GLOBAL) {
      malloc_gpu_virt_memory(*dptr, bytesize, MEMORY_TYPE_UVA_SYNC, host_index);
    }
  } else if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

/* ---------------------------------------------------------------------- *
 *  Oversold managed memory: residency hint
 *
 *  Applied ONLY to allocations this library redirected to managed memory
 *  because they would not fit in physical memory. An application that called
 *  cuMemAllocManaged itself is expressing its own intent about where the data
 *  should live, and second-guessing it is not our business.
 *
 *  Off by default. The hint is free to issue and cannot fail the allocation,
 *  but it is a POLICY change and its sign depends on the workload: for a
 *  buffer the GPU reads repeatedly it keeps pages from drifting to the host,
 *  while for one the CPU also touches it can add migrations, since each host
 *  access pulls the page over and the preference pulls it back. Past the point
 *  where the working set genuinely exceeds the card, no hint makes the pages
 *  fit and only the eviction order changes.
 *
 *  Applied uniformly rather than, say, above some oversubscription ratio. A
 *  ratio test would be evaluated against how much happened to be allocated at
 *  the time, so the same buffer would get a different policy depending on
 *  unrelated allocation timing -- effectively "whoever allocated first stays
 *  resident", which is allocation order, not access pattern. It would also
 *  make an A/B unreadable, since each run would be a mixture.
 * ---------------------------------------------------------------------- */
/* -1 not yet read, 0 off, 1 on. Resolved lazily by whichever thread arrives
 * first.
 *
 * Deliberately NOT a pthread_once. A once-control caught mid-initialisation by
 * a fork leaves the child with an indeterminate control, where a later
 * pthread_once can block forever waiting on an initialiser that no longer
 * exists -- which is exactly why child_after_fork resets g_init_set. A second
 * once would mean a second reset obligation for a future edit to remember. This
 * initialiser is a getenv into an int: every thread derives the same value from
 * the same environment, so racing is idempotent and carries no fork obligation
 * at all -- the child inherits an answer that is already correct. */
static int g_uva_advise_mode = -1;
static int g_uva_advise_logged = 0;

static int uva_advise_mode(void) {
  int mode = __atomic_load_n(&g_uva_advise_mode, __ATOMIC_RELAXED);
  if (unlikely(mode < 0)) {
    mode = 0;
    (void)get_uva_advise(&mode);
    __atomic_store_n(&g_uva_advise_mode, mode, __ATOMIC_RELAXED);
    /* Stated once so a run can be attributed to an arm of the A/B rather than
     * inferred from throughput. Exchange, not a plain flag: several threads can
     * be resolving this at the same moment. */
    if (mode && !__atomic_exchange_n(&g_uva_advise_logged, 1, __ATOMIC_RELAXED)) {
      LOGGER(INFO, "oversold managed allocations: advise mode %d (%s)", mode,
             mode >= 2 ? "prefer device, and read remotely rather than migrate back"
                       : "prefer device");
    }
  }
  return mode;
}

/* Per-device CU_DEVICE_ATTRIBUTE_CONCURRENT_MANAGED_ACCESS: 0 unknown, 1 yes,
 * -1 no. Relaxed atomics because several allocating threads may resolve it at
 * once; they all write the same value, but a plain int written from multiple
 * threads is a race whether or not the value agrees. */
static int g_managed_access[MAX_DEVICE_COUNT] = {0};
static int g_managed_access_warned[MAX_DEVICE_COUNT] = {0};

static int uva_device_has_concurrent_managed_access(CUdevice device, int host_index) {
  int cached = __atomic_load_n(&g_managed_access[host_index], __ATOMIC_RELAXED);
  if (cached != 0) {
    return cached > 0;
  }
  int supported = 0;
  /* CUDA_INTERNAL_CALL does not null-check the entry, so a missing symbol would
   * be a call through NULL. cuDeviceGetAttribute is a core symbol and the other
   * call site does not guard it either, but the guard costs nothing and removes
   * the class rather than trusting the symbol to always be present. */
  if (unlikely(!CUDA_FIND_ENTRY(cuda_library_entry, cuDeviceGetAttribute))) {
    return 0;
  }
  CUresult r = CUDA_INTERNAL_CALL(cuda_library_entry, cuDeviceGetAttribute, &supported,
                                  CU_DEVICE_ATTRIBUTE_CONCURRENT_MANAGED_ACCESS, device);
  if (r != CUDA_SUCCESS) {
    /* Unknown, not "no": leave the cache empty so a later call can resolve it,
     * and do not warn on what may be a transient failure. */
    return 0;
  }
  __atomic_store_n(&g_managed_access[host_index], supported ? 1 : -1, __ATOMIC_RELAXED);
  if (!supported && !__atomic_exchange_n(&g_managed_access_warned[host_index], 1, __ATOMIC_RELAXED)) {
    /* Worth saying out loud, independently of the hint. Without concurrent
     * managed access there is no demand paging: managed memory is migrated
     * wholesale at launch and an allocation larger than the card simply fails.
     * Oversubscription cannot work on such a device, and until now we neither
     * checked nor said so. */
    LOGGER(WARNING, "host device %d does not support concurrent managed access; "
                    "memory oversubscription cannot demand-page on this device",
                    host_index);
  }
  return supported != 0;
}

/* cuMemAdvise and cuMemAdvise_v2 are an ABI-conflict pair -- the last argument
 * is a CUdevice in one and a CUmemLocation struct in the other -- so they must
 * never fall through to each other (see cuda_originals.c). v2 is preferred
 * where the driver exports it; v1 is deprecated from CUDA 12.2. cuda_sym_t is
 * unprototyped, so nothing checks the arity below. */
static void uva_advise_one(CUdeviceptr ptr, size_t bytes, CUdevice device,
                           int host_index, CUmem_advise advice) {
  CUresult r;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAdvise_v2))) {
    CUmemLocation loc;
    memset(&loc, 0, sizeof(loc));
    loc.type = CU_MEM_LOCATION_TYPE_DEVICE;
    loc.id   = (int)device;
    r = CUDA_INTERNAL_CALL(cuda_library_entry, cuMemAdvise_v2, ptr, bytes, advice, loc);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAdvise))) {
    r = CUDA_INTERNAL_CALL(cuda_library_entry, cuMemAdvise, ptr, bytes, advice, device);
  } else {
    return;
  }
  /* A refused hint changes nothing about an allocation that has already
   * succeeded -- log it and carry on rather than failing the caller. */
  if (unlikely(r != CUDA_SUCCESS)) {
    LOGGER(VERBOSE, "cuMemAdvise(%d) on host device %d returned %d: %s",
           (int)advice, host_index, r, CUDA_ERROR(cuda_library_entry, r));
  }
}

static void uva_apply_residency_hint(CUdeviceptr ptr, size_t bytes,
                                     CUdevice device, int host_index) {
  if (bytes == 0 || host_index < 0 || host_index >= MAX_DEVICE_COUNT) {
    return;
  }
  /* Resolved even when the hint is disabled: the warning above is about
   * oversubscription being impossible, not about this hint. */
  int can_page = uva_device_has_concurrent_managed_access(device, host_index);

  int mode = uva_advise_mode();
  if (mode == 0 || !can_page) {
    return;
  }

  uva_advise_one(ptr, bytes, device, host_index, CU_MEM_ADVISE_SET_PREFERRED_LOCATION);
  if (mode >= 2) {
    /* Establishes a device mapping so an evicted page is read over the
     * interconnect instead of faulting back in and evicting something else.
     * SET_ACCESSED_BY names the processor that gets the mapping -- passing the
     * device means the GPU, and says nothing about the CPU. The CPU variant
     * would be pointless here anyway: the caller asked for device memory, so in
     * its own model this pointer is not host-dereferenceable and it never
     * touches it from the host. */
    uva_advise_one(ptr, bytes, device, host_index, CU_MEM_ADVISE_SET_ACCESSED_BY);
  }
}

CUresult _cuMemAlloc(CUdeviceptr *dptr, size_t bytesize) {
  size_t hint_bytes = 0;   /* non-zero => advise after unlocking */
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  int host_index = -1;
  size_t request_size = bytesize;
  memory_path_t path;
  /* NULL-arg fast path: defensive early return matching HAMi PR #182
   * commit 88143ab4. Existing logic is already structurally safe
   * (the *dptr deref via malloc_gpu_virt_memory is gated on
   * ret==SUCCESS, which the driver won't return for NULL dptr), but
   * forwarding immediately avoids the budget check + lock acquisition
   * for what is always a probe / fallback / invalid call. */
  if (unlikely(dptr == NULL)) {
    goto ALLOCATED_TO_GPU;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  path = prepare_memory_allocation(device, request_size, 1, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
  if (path == MEMORY_PATH_UVA) {
    goto ALLOCATED_TO_UVA;
  }
ALLOCATED_TO_GPU:
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAlloc_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAlloc_v2, dptr, bytesize);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAlloc))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAlloc, dptr, bytesize);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY)) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
    if (host_index >= 0 && get_device_flag(host_index, memory_oversold)) {
      metrics_record_uva_fallback(host_index);
      LOGGER(VERBOSE, "cuMemAlloc OOM, try using unified memory allocation (oversold), size: %zu, ret: %d, str: %s",
                       request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
    } else {
      goto DONE;
    }
  } else {
    goto DONE;
  }
ALLOCATED_TO_UVA:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
  LOGGER(VERBOSE, "cuMemAllocManaged to allocate unified memory (oversold), size: %zu, ret: %d, str: %s",
                   request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  if (likely(ret == CUDA_SUCCESS)) {
    malloc_gpu_virt_memory(*dptr, bytesize, MEMORY_TYPE_UVA_SYNC, host_index);
    /* Deferred past the unlock below: cuMemAdvise is a driver call, and
     * holding the cross-process per-device lock across it would make every
     * other allocator in the container wait on it for no reason. */
    hint_bytes = bytesize;
  } else if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  if (hint_bytes) {
    uva_apply_residency_hint(*dptr, hint_bytes, device, host_index);
  }
  return ret;
}

CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytesize) {
  return _cuMemAlloc(dptr, bytesize);
}

CUresult cuMemAlloc(CUdeviceptr *dptr, size_t bytesize) {
  return _cuMemAlloc(dptr, bytesize);
}

CUresult _cuMemAllocPitch(CUdeviceptr *dptr, size_t *pPitch, size_t WidthInBytes,
                          size_t Height, unsigned int ElementSizeBytes) {
  size_t hint_bytes = 0;   /* non-zero => advise after unlocking */
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  int host_index = -1;
  // size_t request_size = ROUND_UP(WidthInBytes * Height, ElementSizeBytes);
  size_t guess_pitch = (((WidthInBytes - 1) / ElementSizeBytes) + 1) * ElementSizeBytes;
  size_t request_size = guess_pitch * Height;
  memory_path_t path;
  /* NULL-arg fast path. The UVA fallback below dereferences *pPitch
   * unconditionally; *dptr deref is guarded by ret==SUCCESS but cheap
   * to short-circuit anyway. Forward to driver for INVALID_VALUE. */
  if (unlikely(dptr == NULL || pPitch == NULL)) {
    goto ALLOCATED_TO_GPU;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  path = prepare_memory_allocation(device, request_size, 1, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
  if (path == MEMORY_PATH_UVA) {
    goto ALLOCATED_TO_UVA;
  }
ALLOCATED_TO_GPU:
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAllocPitch_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocPitch_v2, dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAllocPitch))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocPitch, dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY)) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
    if (host_index >= 0 && get_device_flag(host_index, memory_oversold)) {
      metrics_record_uva_fallback(host_index);
      LOGGER(VERBOSE, "cuMemAllocPitch OOM, try using unified memory allocation (oversold), size: %zu, ret: %d, str: %s",
                       request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
    } else {
      goto DONE;
    }
  } else {
    goto DONE;
  }
ALLOCATED_TO_UVA:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocManaged, dptr, request_size, CU_MEM_ATTACH_GLOBAL);
  LOGGER(VERBOSE, "cuMemAllocManaged to allocate unified memory (oversold), size: %zu, ret: %d, str: %s",
                  request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  if (likely(ret == CUDA_SUCCESS)) {
    *pPitch = guess_pitch;
    malloc_gpu_virt_memory(*dptr, request_size, MEMORY_TYPE_UVA_SYNC, host_index);
    /* Deferred past the unlock below: cuMemAdvise is a driver call, and
     * holding the cross-process per-device lock across it would make every
     * other allocator in the container wait on it for no reason. */
    hint_bytes = request_size;
  } else if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  if (hint_bytes) {
    uva_apply_residency_hint(*dptr, hint_bytes, device, host_index);
  }
  return ret;
}


CUresult cuMemAllocPitch_v2(CUdeviceptr *dptr, size_t *pPitch, size_t WidthInBytes,
                            size_t Height, unsigned int ElementSizeBytes) {
  return _cuMemAllocPitch(dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
}

CUresult cuMemAllocPitch(CUdeviceptr *dptr, size_t *pPitch, size_t WidthInBytes,
                         size_t Height, unsigned int ElementSizeBytes) {
  return _cuMemAllocPitch(dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
}

CUresult cuMemAllocAsync(CUdeviceptr *dptr, size_t bytesize, CUstream hStream) {
  size_t hint_bytes = 0;   /* non-zero => advise after unlocking */
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  int host_index = -1;
  size_t request_size = bytesize;
  memory_path_t path;
  /* NULL-arg fast path; same rationale as _cuMemAlloc above. */
  if (unlikely(dptr == NULL)) {
    goto ALLOCATED_TO_GPU;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  path = prepare_memory_allocation(device, request_size, 1, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
  // TODO Do not disrupt graph capture due to UVA path destruction
  if (path == MEMORY_PATH_UVA && !stream_is_capturing(hStream, __CUDA_API_IS_PTSZ)) {
    goto ALLOCATED_TO_UVA;
  }
ALLOCATED_TO_GPU:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemAllocAsync), dptr, bytesize, hStream);
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY)) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
    // TODO Do not disrupt graph capture due to UVA path destruction
    if (host_index >= 0 && get_device_flag(host_index, memory_oversold) && !stream_is_capturing(hStream, __CUDA_API_IS_PTSZ)) {
      metrics_record_uva_fallback(host_index);
      LOGGER(VERBOSE, "cuMemAllocAsync OOM, try using unified memory allocation (oversold), size: %zu, ret: %d, str: %s",
                    request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
    } else {
      goto DONE;
    }
  } else if (ret == CUDA_SUCCESS && lock_fd >= 0) {
    if (!stream_is_capturing(hStream, __CUDA_API_IS_PTSZ)) {
      /* Bridge the allocation into the shared view before the lock is dropped,
       * so a concurrent allocator counts it while it is still in flight. */
      malloc_gpu_virt_memory(*dptr, bytesize, MEMORY_TYPE_ASYNC_BRIDGE, host_index);
      unlock_gpu_device(lock_fd);
      lock_fd = -1;
      /* Switching from capture to synchronous operation, off the device lock. */
      CUDA_INTERNAL_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamSynchronize), hStream);
      /* Release internal accounting only once NVML is guaranteed to see it. */
      free_gpu_virt_memory(*dptr);
    } else {
      /* Charge the capture so that several allocations inside one capture
       * accumulate against the limit. Chargeable only while the owning graph is
       * known, because that is what lets cuStreamEndCapture retire it. */
      CUgraph graph = stream_capture_graph(hStream, __CUDA_API_IS_PTSZ);
      if (graph != NULL) {
        malloc_gpu_virt_memory_captured(*dptr, bytesize, graph, host_index);
      }
    }
    goto DONE;
  } else {
    goto DONE;
  }
ALLOCATED_TO_UVA:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
  LOGGER(VERBOSE, "cuMemAllocManaged to allocate unified memory (oversold), size: %zu, ret: %d, str: %s",
                  request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  if (likely(ret == CUDA_SUCCESS)) {
    malloc_gpu_virt_memory(*dptr, bytesize, MEMORY_TYPE_UVA_ASYNC, host_index);
    /* Deferred past the unlock below: cuMemAdvise is a driver call, and
     * holding the cross-process per-device lock across it would make every
     * other allocator in the container wait on it for no reason. */
    hint_bytes = bytesize;
  } else if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  if (hint_bytes) {
    uva_apply_residency_hint(*dptr, hint_bytes, device, host_index);
  }
  return ret;
}

CUresult cuMemAllocAsync_ptsz(CUdeviceptr *dptr, size_t bytesize, CUstream hStream) {
  size_t hint_bytes = 0;   /* non-zero => advise after unlocking */
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  int host_index = -1;
  size_t request_size = bytesize;
  memory_path_t path;
  /* NULL-arg fast path; same rationale as _cuMemAlloc above. */
  if (unlikely(dptr == NULL)) {
    goto ALLOCATED_TO_GPU;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  path = prepare_memory_allocation(device, request_size, 1, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
  if (path == MEMORY_PATH_UVA && !stream_is_capturing(hStream, 1)) {
    goto ALLOCATED_TO_UVA;
  }
ALLOCATED_TO_GPU:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocAsync_ptsz, dptr, bytesize, hStream);
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY)) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
    // TODO Do not disrupt graph capture due to UVA path destruction
    if (host_index >= 0 && get_device_flag(host_index, memory_oversold) && !stream_is_capturing(hStream, 1)) {
      metrics_record_uva_fallback(host_index);
      LOGGER(VERBOSE, "cuMemAllocAsync_ptsz OOM, try using unified memory allocation (oversold), size: %zu, ret: %d, str: %s",
                    request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
    } else {
      goto DONE;
    }
  } else if (ret == CUDA_SUCCESS && lock_fd >= 0) {
    if (!stream_is_capturing(hStream, 1)) {
      /* Bridge the allocation into the shared view before the lock is dropped,
       * so a concurrent allocator counts it while it is still in flight. */
      malloc_gpu_virt_memory(*dptr, bytesize, MEMORY_TYPE_ASYNC_BRIDGE, host_index);
      unlock_gpu_device(lock_fd);
      lock_fd = -1;
      /* Switching from capture to synchronous operation, off the device lock. */
      CUDA_INTERNAL_CHECK(cuda_library_entry, cuStreamSynchronize_ptsz, hStream);
      /* Release internal accounting only once NVML is guaranteed to see it. */
      free_gpu_virt_memory(*dptr);
    } else {
      /* Charge the capture so that several allocations inside one capture
       * accumulate against the limit. Chargeable only while the owning graph is
       * known, because that is what lets cuStreamEndCapture retire it. */
      CUgraph graph = stream_capture_graph(hStream, 1);
      if (graph != NULL) {
        malloc_gpu_virt_memory_captured(*dptr, bytesize, graph, host_index);
      }
    }
    goto DONE;
  } else {
    goto DONE;
  }
ALLOCATED_TO_UVA:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
  LOGGER(VERBOSE, "cuMemAllocManaged to allocate unified memory (oversold), size: %zu, ret: %d, str: %s",
                  request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  if (likely(ret == CUDA_SUCCESS)) {
    malloc_gpu_virt_memory(*dptr, request_size, MEMORY_TYPE_UVA_ASYNC, host_index);
    /* Deferred past the unlock below: cuMemAdvise is a driver call, and
     * holding the cross-process per-device lock across it would make every
     * other allocator in the container wait on it for no reason. */
    hint_bytes = request_size;
  } else if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  if (hint_bytes) {
    uva_apply_residency_hint(*dptr, hint_bytes, device, host_index);
  }
  return ret;
}

static size_t get_array_base_size(int format) {
  size_t base_size = 0;

  switch (format) {
  case CU_AD_FORMAT_UNSIGNED_INT8:
  case CU_AD_FORMAT_SIGNED_INT8:
    base_size = 8;
    break;
  case CU_AD_FORMAT_UNSIGNED_INT16:
  case CU_AD_FORMAT_SIGNED_INT16:
  case CU_AD_FORMAT_HALF:
    base_size = 16;
    break;
  case CU_AD_FORMAT_UNSIGNED_INT32:
  case CU_AD_FORMAT_SIGNED_INT32:
  case CU_AD_FORMAT_FLOAT:
    base_size = 32;
    break;
  default:
    base_size = 32;
  }

  return base_size;
}


CUresult _cuArrayCreate(CUarray *pHandle, const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  memory_path_t path;
  /* Declared before the NULL-arg `goto CALL` below: that jump skips any
   * initializer between it and the label, and the driver-OOM metric at CALL
   * reads host_index. */
  int host_index = -1;
  /* NULL-arg fast path: forward to the driver so the caller sees the
   * canonical CUDA_ERROR_INVALID_VALUE instead of a SegFault inside
   * get_array_request_size (which dereferences pAllocateArray->Format
   * etc. with no NULL check). NVIDIA OptiX / Aftermath fallback init
   * paths probe with NULL during early init; without this guard
   * libvgpu-control.so crashes Isaac Sim Kit at startup. Mirrors
   * HAMi PR #182 commit 88143ab4 / 275ba3db / 01a58f13 patterns.
   * Keeps the CUDA-side semantics (NULL -> INVALID_VALUE) intact. */
  if (unlikely(pAllocateArray == NULL)) {
    goto CALL;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t request_size = get_array_request_size(pAllocateArray);
  path = prepare_memory_allocation(device, request_size, 0, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
CALL:
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArrayCreate_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayCreate_v2, pHandle, pAllocateArray);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArrayCreate))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayCreate, pHandle, pAllocateArray);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuArrayCreate_v2(CUarray *pHandle, const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  return _cuArrayCreate(pHandle, pAllocateArray);
}

CUresult cuArrayCreate(CUarray *pHandle, const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  return _cuArrayCreate(pHandle, pAllocateArray);
}

CUresult _cuArray3DCreate(CUarray *pHandle, const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  memory_path_t path;
  /* Declared before `goto CALL`; see _cuArrayCreate. */
  int host_index = -1;
  /* NULL-arg fast path; same rationale as _cuArrayCreate above. */
  if (unlikely(pAllocateArray == NULL)) {
    goto CALL;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t request_size = get_array3d_request_size(pAllocateArray);
  path = prepare_memory_allocation(device, request_size, 0, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
CALL:
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArray3DCreate_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArray3DCreate_v2, pHandle, pAllocateArray);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArray3DCreate))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArray3DCreate, pHandle, pAllocateArray);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuArray3DCreate_v2(CUarray *pHandle, const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  return _cuArray3DCreate(pHandle, pAllocateArray);
}

CUresult cuArray3DCreate(CUarray *pHandle, const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  return _cuArray3DCreate(pHandle, pAllocateArray);
}

CUresult cuMipmappedArrayCreate(CUmipmappedArray *pHandle,
                                const CUDA_ARRAY3D_DESCRIPTOR *pMipmappedArrayDesc,
                                unsigned int numMipmapLevels) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  memory_path_t path;
  /* Declared before `goto CALL`; see _cuArrayCreate. */
  int host_index = -1;
  /* NULL-arg fast path; same rationale as _cuArrayCreate above. */
  if (unlikely(pMipmappedArrayDesc == NULL)) {
    goto CALL;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t request_size = get_array3d_request_size(pMipmappedArrayDesc);
  path = prepare_memory_allocation(device, request_size, 0, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMipmappedArrayCreate, pHandle,
                         pMipmappedArrayDesc, numMipmapLevels);
  if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuMemCreate(CUmemGenericAllocationHandle *handle, size_t size,
                    const CUmemAllocationProp *prop, unsigned long long flags) {
  CUresult ret;
  int lock_fd = -1;
  memory_path_t path;
  /* Declared before the `goto CALL`s below: a jump skips any initializer
   * between it and the label, and the driver-OOM metric at CALL reads
   * host_index. Both bypasses (NULL handle, host-typed allocation) leave it
   * at -1, which metrics_record_oom() correctly ignores as out-of-range. */
  int host_index = -1;

  /* NULL-arg fast path: same rationale as _cuMemAlloc above (forward
   * to driver so the caller sees its canonical INVALID_VALUE instead
   * of crashing inside our hook). */
  if (unlikely(handle == NULL)) {
    goto CALL;
  }

  /* Skip budget bookkeeping entirely for non-DEVICE allocations.
   *
   * cuMemCreate's prop->location.type can be:
   *   - CU_MEM_LOCATION_TYPE_DEVICE          -> GPU VRAM, count toward budget
   *   - CU_MEM_LOCATION_TYPE_HOST            -> pinned host RAM
   *   - CU_MEM_LOCATION_TYPE_HOST_NUMA       -> NUMA-pinned host RAM
   *   - CU_MEM_LOCATION_TYPE_HOST_NUMA_CURRENT -> ditto, current NUMA node
   *
   * Host-typed allocations live in pinned host RAM and are governed
   * by the K8s pod memory cgroup, not by our GPU-VRAM budget. The
   * pre-merge code (and main commit 43a7bae, and HAMi PR #182's
   * pre-#188 code) ran prepare_memory_allocation for these, which
   * over-charged the device budget and risked falsely OOM'ing later
   * true-VRAM allocations. Mirrors HAMi PR #188's `do_oom_check`
   * gating but applied at the entry instead of around the post-
   * success tracking call (vgpu does pre-alloc budget checks; HAMi
   * does post-alloc tracking — same outcome, different shape).
   *
   * NULL prop is also forwarded straight to the driver so the
   * caller sees the canonical INVALID_VALUE error instead of us
   * silently treating it as DEVICE. */
  if (prop == NULL || prop->location.type != CU_MEM_LOCATION_TYPE_DEVICE) {
    goto CALL;
  }

  /* Use prop->location.id (the VMM target device) rather than the
   * current context's device. They differ in cross-device VMM on
   * multi-GPU pods: the app may hold a context on device A while
   * explicitly creating VMM allocations on device B. Tracking
   * against context-device would charge the wrong host_index's
   * budget. CUDA defines location.id as the device ordinal for
   * DEVICE-typed allocations, identical to a CUdevice value, so
   * the cast is safe. We do not call cuCtxGetDevice here at all —
   * cross-device VMM is the correctness target, and avoiding the
   * call also sidesteps the cuCtxGetDevice-failure SegFault path
   * that main commit 43a7bae had to guard against. */
  CUdevice device = (CUdevice)prop->location.id;

  size_t request_size = size;
  path = prepare_memory_allocation(device, request_size, 0, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemCreate, handle, size, prop, flags);
  if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult _cuDeviceTotalMem(size_t *bytes, CUdevice dev) {
  CUresult ret = CUDA_ERROR_NOT_FOUND;
  /* Ask the driver first, with the caller's own pointer. Two things fall out
   * of that ordering. A NULL `bytes` is handled by whatever policy the driver
   * has -- we neither have to know it nor guess it -- and a device the driver
   * would have refused (lost, not initialised) still produces the driver's
   * error instead of us reporting a configured size for a device that is not
   * answering. The previous form returned CUDA_SUCCESS from the limited-view
   * branch without ever consulting the driver. */
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDeviceTotalMem_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceTotalMem_v2, bytes, dev);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDeviceTotalMem))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceTotalMem, bytes, dev);
  }
  if (unlikely(ret != CUDA_SUCCESS)) {
    return ret;
  }
  /* Before the index lookup: get_host_device_index_by_cuda_device resolves via
   * UUID against g_vgpu_config->devices[] on a cold cache, so the FIRST call is
   * the one that would dereference it unset. Only load_necessary_data() --
   * deliberately NOT init_devices_mapping(), whose nvmlInit failure path is
   * LOGGER(FATAL) and would turn a memory query into an exit(1). */
  load_necessary_data();
  int host_index = get_host_device_index_by_cuda_device(dev);
  if (unlikely(host_index < 0)) {
    return ret;
  }
  device_t dsnap = get_device_snapshot(host_index);
  if (dsnap.memory_limit && bytes != NULL) {
    size_t configured = dsnap.total_memory;
    if (dsnap.memory_oversold) {
      *bytes = configured;
    } else {
      CUresult sub = CUDA_ERROR_NOT_FOUND;
      size_t real_total = 0, real_free_unused = 0;
      if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetInfo_v2))) {
        sub = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetInfo_v2, &real_free_unused, &real_total);
      } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetInfo))) {
        sub = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetInfo, &real_free_unused, &real_total);
      }
      *bytes = (sub == CUDA_SUCCESS && real_total > 0 && real_total < configured) ? real_total : configured;
    }
  }
  return ret;
}

CUresult cuDeviceTotalMem_v2(size_t *bytes, CUdevice dev) {
  return _cuDeviceTotalMem(bytes, dev);
}

CUresult cuDeviceTotalMem(size_t *bytes, CUdevice dev) {
  return _cuDeviceTotalMem(bytes, dev);
}

CUresult _cuMemGetInfo(size_t *free, size_t *total) {
  CUdevice device;
  int lock_fd = -1;
  CUresult ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetInfo_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetInfo_v2, free, total);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetInfo))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetInfo, free, total);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  /* Before the index lookup: get_host_device_index_by_cuda_device resolves via
   * UUID against g_vgpu_config->devices[] on a cold cache, so the FIRST call is
   * the one that would dereference it unset. Only load_necessary_data() --
   * deliberately NOT init_devices_mapping(), whose nvmlInit failure path is
   * LOGGER(FATAL) and would turn a memory query into an exit(1). */
  load_necessary_data();
  int host_index = -1;
  size_t used = 0, vmem_used = 0;
  if (load_limited_memory_view(device, &host_index, &lock_fd, &used, &vmem_used)) {
    device_t dsnap = get_device_snapshot(host_index);
    size_t configured = dsnap.total_memory;
    size_t actual_total = 0, actual_free = 0;
    if (dsnap.memory_oversold) {
      actual_total = configured;
      actual_free  = (used + vmem_used) >= actual_total ? 0 : (actual_total - used - vmem_used);
    } else {
      if (total != NULL) {
        actual_total = (*total > 0 && *total < configured) ? *total : configured;
      } else if (free != NULL) {
        CUresult sub = CUDA_ERROR_NOT_FOUND;
        size_t real_total = 0, real_free_unused = 0;
        if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetInfo_v2))) {
          sub = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetInfo_v2, &real_free_unused, &real_total);
        } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetInfo))) {
          sub = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetInfo, &real_free_unused, &real_total);
        }
        actual_total = (sub == CUDA_SUCCESS && real_total > 0 && real_total < configured) ? real_total : configured;
      }
      actual_free  = (used + vmem_used) >= actual_total ? 0 : (actual_total - used - vmem_used);
    }
    if (total != NULL) *total = actual_total;
    if (free != NULL) *free = actual_free;
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuMemGetInfo_v2(size_t *free, size_t *total) {
  return _cuMemGetInfo(free, total);
}

CUresult cuMemGetInfo(size_t *free, size_t *total) {
  return _cuMemGetInfo(free, total);
}

CUresult cuLaunchKernel_ptsz(CUfunction f, unsigned int gridDimX,
                             unsigned int gridDimY, unsigned int gridDimZ,
                             unsigned int blockDimX, unsigned int blockDimY,
                             unsigned int blockDimZ,
                             unsigned int sharedMemBytes, CUstream hStream,
                             void **kernelParams, void **extra) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  rate_limiter(gridDimX * gridDimY * gridDimZ,
              blockDimX * blockDimY * blockDimZ, host_index);
  int gap = gap_begin(host_index, hStream);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchKernel_ptsz, f, gridDimX,
                         gridDimY, gridDimZ, blockDimX, blockDimY, blockDimZ,
                         sharedMemBytes, hStream, kernelParams, extra);
  if (gap) gap_end(host_index, hStream, ret);
DONE:
  return ret;
}

CUresult cuLaunchKernel(CUfunction f, unsigned int gridDimX,
                        unsigned int gridDimY, unsigned int gridDimZ,
                        unsigned int blockDimX, unsigned int blockDimY,
                        unsigned int blockDimZ, unsigned int sharedMemBytes,
                        CUstream hStream, void **kernelParams, void **extra) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  rate_limiter(gridDimX * gridDimY * gridDimZ,
              blockDimX * blockDimY * blockDimZ, host_index);
  int gap = gap_begin(host_index, hStream);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuLaunchKernel), f, gridDimX,
                         gridDimY, gridDimZ, blockDimX, blockDimY, blockDimZ,
                         sharedMemBytes, hStream, kernelParams, extra);
  if (gap) gap_end(host_index, hStream, ret);
DONE:
  return ret;
}

CUresult cuLaunchKernelEx(CUlaunchConfig *config, CUfunction f, 
                          void **kernelParams, void **extra) {
  CUresult ret;
  CUdevice device;
  int gap = 0;
  int host_index = -1;
  if (unlikely(config == NULL)) {
    goto CALL;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  host_index = get_host_device_index_by_cuda_device(device);
  rate_limiter(config->gridDimX * config->gridDimY * config->gridDimZ,
               config->blockDimX * config->blockDimY * config->blockDimZ, host_index);
  gap = gap_begin(host_index, config->hStream);
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuLaunchKernelEx),
                         config, f, kernelParams, extra);
  if (gap) gap_end(host_index, config->hStream, ret);
DONE:
  return ret;
}

CUresult cuLaunchKernelEx_ptsz(CUlaunchConfig *config, CUfunction f, 
                               void **kernelParams, void **extra) {
  CUresult ret; 
  CUdevice device;
  int gap = 0;
  int host_index = -1;
  if (unlikely(config == NULL)) {
    goto CALL;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  host_index = get_host_device_index_by_cuda_device(device);
  rate_limiter(config->gridDimX *config->gridDimY * config->gridDimZ,
               config->blockDimX * config->blockDimY * config->blockDimZ, host_index);
  gap = gap_begin(host_index, config->hStream);
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchKernelEx_ptsz,
                         config, f, kernelParams, extra);
  if (gap) gap_end(host_index, config->hStream, ret);
DONE:
  return ret;
}

CUresult cuLaunch(CUfunction f) {
  CUresult ret; 
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  rate_limiter(1, g_block_x[host_index] * g_block_y[host_index] * g_block_z[host_index], host_index);
  int gap = gap_begin(host_index, CU_STREAM_LEGACY);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunch, f);
  if (gap) gap_end(host_index, CU_STREAM_LEGACY, ret);
DONE:
  return ret;
}

CUresult cuLaunchCooperativeKernel_ptsz(
    CUfunction f, unsigned int gridDimX, unsigned int gridDimY,
    unsigned int gridDimZ, unsigned int blockDimX, unsigned int blockDimY,
    unsigned int blockDimZ, unsigned int sharedMemBytes, CUstream hStream,
    void **kernelParams) {
  CUdevice device;
  CUresult ret;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  rate_limiter(gridDimX * gridDimY * gridDimZ,
              blockDimX * blockDimY * blockDimZ, host_index);
  int gap = gap_begin(host_index, hStream);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchCooperativeKernel_ptsz, f,
                         gridDimX, gridDimY, gridDimZ, blockDimX, blockDimY,
                         blockDimZ, sharedMemBytes, hStream, kernelParams);
  if (gap) gap_end(host_index, hStream, ret);
DONE:
  return ret;
}

CUresult cuLaunchCooperativeKernel(CUfunction f,
    unsigned int gridDimX, unsigned int gridDimY, unsigned int gridDimZ,
    unsigned int blockDimX, unsigned int blockDimY, unsigned int blockDimZ,
    unsigned int sharedMemBytes, CUstream hStream, void **kernelParams) {
  CUdevice device;
  CUresult ret; 
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }    
  int host_index = get_host_device_index_by_cuda_device(device);
  rate_limiter(gridDimX * gridDimY * gridDimZ,
              blockDimX * blockDimY * blockDimZ, host_index);
  int gap = gap_begin(host_index, hStream);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuLaunchCooperativeKernel), f,
                         gridDimX, gridDimY, gridDimZ, blockDimX, blockDimY,
                         blockDimZ, sharedMemBytes, hStream, kernelParams);
  if (gap) gap_end(host_index, hStream, ret);
DONE:
  return ret;
}

CUresult cuLaunchGrid(CUfunction f, int grid_width, int grid_height) {
  CUresult ret;  
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  rate_limiter(grid_width * grid_height, g_block_x[host_index] * g_block_y[host_index] * g_block_z[host_index], host_index);
  int gap = gap_begin(host_index, CU_STREAM_LEGACY);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchGrid, f, grid_width,grid_height);
  if (gap) gap_end(host_index, CU_STREAM_LEGACY, ret);
DONE:
  return ret;
}

CUresult cuLaunchGridAsync(CUfunction f, int grid_width, int grid_height, CUstream hStream) {
  CUresult ret;  
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  rate_limiter(grid_width * grid_height, g_block_x[host_index] * g_block_y[host_index] * g_block_z[host_index], host_index);
  int gap = gap_begin(host_index, hStream);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchGridAsync, f, grid_width, grid_height, hStream);
  if (gap) gap_end(host_index, hStream, ret);
DONE:
  return ret;
}

extern CUresult _cuCtxPushCurrent(CUcontext ctx);
extern CUresult _cuCtxPopCurrent(CUcontext *pctx);

/* Multi-device cooperative launch: applies the per-device throttle by
 * extracting host_index from each entry's stream context. Rare API; we keep
 * the simple approach of throttling per entry rather than aggregating, so
 * each per-device token bucket sees its own portion of the work. */
CUresult cuLaunchCooperativeKernelMultiDevice(CUDA_LAUNCH_PARAMS *launchParamsList,
                                              unsigned int numDevices,
                                              unsigned int flags) {
  if (likely(launchParamsList != NULL)) {
    for (unsigned int i = 0; i < numDevices; i++) {
      CUDA_LAUNCH_PARAMS *p = &launchParamsList[i];
      CUresult ret;
      CUcontext sctx = NULL;
      /* CUDA_INTERNAL_CHECK only adds a log line around CUDA_INTERNAL_CALL,
       * which does NOT null-check the entry -- a driver that does not export
       * the symbol would be called through a NULL pointer. Hence the explicit
       * guards. cuStreamGetCtx_v2 is preferred where the driver has it: it
       * additionally reports a green context, which we do not need (a green
       * context partitions one device, so the device is the same either way),
       * but routing through the newer entry keeps this working if the older
       * one is ever retired. The two take a different number of arguments and
       * cuda_sym_t is unprototyped, so the compiler cannot check the call --
       * the arity below is load-bearing. */
      if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetCtx_v2)))) {
        ret = CUDA_INTERNAL_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetCtx_v2),
                                  p->hStream, &sctx, NULL);
      } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetCtx)))) {
        ret = CUDA_INTERNAL_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamGetCtx),
                                  p->hStream, &sctx);
      } else {
        ret = CUDA_ERROR_NOT_FOUND;
      }
      if (unlikely(ret != CUDA_SUCCESS || sctx == NULL)) {
        continue;
      }
      CUcontext prev = NULL;
      ret = _cuCtxPushCurrent(sctx);
      if (unlikely(ret != CUDA_SUCCESS)) {
        continue;
      }
      CUdevice device;
      ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
      if (likely(ret == CUDA_SUCCESS)) {
        int host_index = get_host_device_index_by_cuda_device(device);
        rate_limiter(p->gridDimX * p->gridDimY * p->gridDimZ,
                     p->blockDimX * p->blockDimY * p->blockDimZ, host_index);
      }
      _cuCtxPopCurrent(&prev);
    }
  }
  return CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchCooperativeKernelMultiDevice,
                          launchParamsList, numDevices, flags);
}

/* =========================================================================
 * CUDA Graph throttling
 *
 * cuLaunchKernel-style hooks consult rate_limiter per launch. CUDA Graphs
 * batch many kernels into one cuGraphLaunch call -- without intercepting
 * them, every kernel inside a graph bypasses our token bucket. Walking the
 * graph on every launch would defeat the latency win that motivates graphs
 * in the first place (PyTorch 2.x replays graphs hundreds of times per sec).
 *
 * Strategy: walk the source graph once at cuGraphInstantiate time, sum the
 * gridDim products across every kernel node (recursing into child graphs),
 * cache it keyed by the resulting CUgraphExec. cuGraphLaunch then consults
 * the cache in O(1) and calls rate_limiter once. cuGraphExecDestroy frees
 * the cache slot.
 *
 * cuGraphExecUpdate AND cuGraphExecUpdate_v2 are both hooked (rewalk +
 * replace cache entry) because PyTorch dynamic-shape inference relies on
 * Update; CUDA 12+ headers redirect callers from v1 to v2 at compile time,
 * so both ABIs surface in practice.
 *
 * Edge cases left intentionally as imprecision (KNOWN LIMITATIONS):
 *   - cuGraphExecKernelNodeSetParams / _v2 replaces a kernel node's
 *     grid/block after instantiation. We DO NOT hook these -- correctly
 *     delta-updating the cached cost would require either (a) maintaining
 *     a per-exec shadow copy of every node's params (~1KB per exec, complex
 *     concurrency), or (b) invalidating the entry which silently disables
 *     throttling for that exec. Neither is appealing. Frameworks that use
 *     SetParams (rare; mainly low-level CUDA Graph users, not PyTorch's
 *     typical path) will see the cached cost age. Accepted.
 *   - cuGraphExecChildGraphNodeSetParams (replaces a subgraph node) and
 *     cuGraphExecMemcpy/Memset/HostNodeSetParams (do not affect kernel
 *     cost) similarly not hooked.
 *   - Conditional nodes (CUDA 12.4+) and batched memop nodes don't
 *     contribute to walk cost so changes to them don't matter.
 * ========================================================================= */

#define GRAPH_COST_CACHE_SIZE 256
typedef struct {
  CUgraphExec exec;       /* NULL = unused slot */
  int total_grids;        /* sum of gridDimX*Y*Z over all kernel nodes */
  int max_blocks;         /* max blockDim product across kernel nodes */
} graph_cost_entry_t;
static graph_cost_entry_t g_graph_cost_cache[GRAPH_COST_CACHE_SIZE];
static pthread_mutex_t g_graph_cost_lock = PTHREAD_MUTEX_INITIALIZER;
static int g_graph_cost_full_warned = 0;

/* Called from child_after_fork. Reinit the lock (POSIX leaves inherited
 * mutex state undefined) and wipe the cache -- parent CUgraphExec handles
 * are invalid here because CUDA contexts don't survive fork. */
static void graph_cost_after_fork(void) {
  pthread_mutex_init(&g_graph_cost_lock, NULL);
  memset(g_graph_cost_cache, 0, sizeof(g_graph_cost_cache));
  g_graph_cost_full_warned = 0;
}

extern CUresult _cuGraphKernelNodeGetParams(CUgraphNode hNode, CUDA_KERNEL_NODE_PARAMS *nodeParams);

static void walk_graph_cost(CUgraph graph, int64_t *total_grids, int *max_blocks) {
  if (unlikely(!graph)) return;
  /* Driver-availability guard: CUDA_INTERNAL_CHECK calls fn_ptr directly
   * without a NULL check, so an old driver lacking these graph APIs would
   * SEGV here. cuGraphInstantiate having succeeded does not guarantee
   * cuGraphGetNodes is loaded in our cuda_library_entry. */
  /* Bail if cuGraphGetNodes or cuGraphNodeGetType is absent, OR if BOTH
   * KernelNodeGetParams variants are absent. _cuGraphKernelNodeGetParams
   * dispatches v2-preferred-v1 internally, so we only need at least one
   * of the two to be loaded. Old driver: only v1 -> pass. CUDA 12+: both
   * present -> pass. Pre-CUDA-10 graphless driver: neither -> bail. */
  if (unlikely(!CUDA_FIND_ENTRY(cuda_library_entry, cuGraphGetNodes) ||
               !CUDA_FIND_ENTRY(cuda_library_entry, cuGraphNodeGetType) ||
               (!CUDA_FIND_ENTRY(cuda_library_entry, cuGraphKernelNodeGetParams) &&
                !CUDA_FIND_ENTRY(cuda_library_entry, cuGraphKernelNodeGetParams_v2)))) {
    return;
  }
  size_t num_nodes = 0;
  CUresult r = CUDA_INTERNAL_CHECK(cuda_library_entry, cuGraphGetNodes,
                                   graph, NULL, &num_nodes);
  if (r != CUDA_SUCCESS || num_nodes == 0) return;
  CUgraphNode *nodes = (CUgraphNode*)malloc(num_nodes * sizeof(CUgraphNode));
  /* cppcheck-suppress memleak ; false positive: nodes is NULL on this path */
  if (unlikely(!nodes)) return;
  r = CUDA_INTERNAL_CHECK(cuda_library_entry, cuGraphGetNodes,
                          graph, nodes, &num_nodes);
  if (r == CUDA_SUCCESS) {
    for (size_t i = 0; i < num_nodes; i++) {
      CUgraphNodeType type;
      if (CUDA_INTERNAL_CHECK(cuda_library_entry, cuGraphNodeGetType, nodes[i], &type) != CUDA_SUCCESS) {
        continue;
      }
      if (type == CU_GRAPH_NODE_TYPE_KERNEL) {
        CUDA_KERNEL_NODE_PARAMS params = {0};
        if (_cuGraphKernelNodeGetParams(nodes[i], &params) != CUDA_SUCCESS) continue;
        *total_grids += (int64_t)params.gridDimX * params.gridDimY * params.gridDimZ;
        int b = params.blockDimX * params.blockDimY * params.blockDimZ;
        if (b > *max_blocks) *max_blocks = b;
      } else if (type == CU_GRAPH_NODE_TYPE_GRAPH) {
        if (unlikely(!CUDA_FIND_ENTRY(cuda_library_entry, cuGraphChildGraphNodeGetGraph))) {
          continue;
        }
        CUgraph child = NULL;
        if (CUDA_INTERNAL_CHECK(cuda_library_entry, cuGraphChildGraphNodeGetGraph,
                                nodes[i], &child) == CUDA_SUCCESS) {
          walk_graph_cost(child, total_grids, max_blocks);
        }
      }
    }
  }
  free(nodes);
}

static void graph_cost_remember(CUgraphExec exec, int grids, int blocks) {
  if (unlikely(!exec) || grids <= 0) return;
  pthread_mutex_lock(&g_graph_cost_lock);
  int free_slot = -1;
  for (int i = 0; i < GRAPH_COST_CACHE_SIZE; i++) {
    if (g_graph_cost_cache[i].exec == exec) {
      g_graph_cost_cache[i].total_grids = grids;
      g_graph_cost_cache[i].max_blocks  = blocks;
      pthread_mutex_unlock(&g_graph_cost_lock);
      return;
    }
    if (g_graph_cost_cache[i].exec == NULL && free_slot < 0) free_slot = i;
  }
  if (free_slot >= 0) {
    g_graph_cost_cache[free_slot].exec        = exec;
    g_graph_cost_cache[free_slot].total_grids = grids;
    g_graph_cost_cache[free_slot].max_blocks  = blocks;
  } else if (!g_graph_cost_full_warned) {
    g_graph_cost_full_warned = 1;
    LOGGER(WARNING, "graph cost cache full (%d entries); newly-instantiated "
                    "graphs will not be throttled at cuGraphLaunch",
                    GRAPH_COST_CACHE_SIZE);
  }
  pthread_mutex_unlock(&g_graph_cost_lock);
}

static int graph_cost_lookup(CUgraphExec exec, int *grids, int *blocks) {
  if (unlikely(!exec)) return 0;
  pthread_mutex_lock(&g_graph_cost_lock);
  for (int i = 0; i < GRAPH_COST_CACHE_SIZE; i++) {
    if (g_graph_cost_cache[i].exec == exec) {
      *grids  = g_graph_cost_cache[i].total_grids;
      *blocks = g_graph_cost_cache[i].max_blocks;
      pthread_mutex_unlock(&g_graph_cost_lock);
      return 1;
    }
  }
  pthread_mutex_unlock(&g_graph_cost_lock);
  return 0;
}

static void graph_cost_forget(CUgraphExec exec) {
  if (unlikely(!exec)) return;
  pthread_mutex_lock(&g_graph_cost_lock);
  for (int i = 0; i < GRAPH_COST_CACHE_SIZE; i++) {
    if (g_graph_cost_cache[i].exec == exec) {
      g_graph_cost_cache[i].exec        = NULL;
      g_graph_cost_cache[i].total_grids = 0;
      g_graph_cost_cache[i].max_blocks  = 0;
      break;
    }
  }
  pthread_mutex_unlock(&g_graph_cost_lock);
}

static void capture_graph_cost(CUgraphExec exec, CUgraph src_graph) {
  if (unlikely(!exec || !src_graph)) return;
  int64_t total = 0;
  int max_blocks = 1;
  walk_graph_cost(src_graph, &total, &max_blocks);
  if (total > 0) {
    if (total > INT_MAX) total = INT_MAX;
    graph_cost_remember(exec, (int)total, max_blocks);
    return;
  }
  /* Walk found no kernel work in this graph. For cuGraphInstantiate this
   * is a no-op (no prior entry to clear). For cuGraphExecUpdate / _v2,
   * leaving any prior entry in place would lock the exec to the OLD
   * graph's cost forever -- a strict regression vs the previous
   * forget-then-remember design when the update degenerates the graph
   * to zero kernel work. Explicit forget keeps graph_cost_remember's
   * in-place overwrite contract complete: every successful instantiate /
   * update path settles the cache to a state that reflects the current
   * graph. */
  graph_cost_forget(exec);
}

/* cuGraphInstantiate and cuGraphInstantiate_v2 share an identical body:
 * dispatch via the internal _cuGraphInstantiate helper in cuda_originals.c
 * (which already does the "_v2 preferred, fall back to v1" probe), then
 * capture the graph cost on success. We keep two distinct exported symbols
 * because the dynamic linker treats them as separate dlsym targets -- a
 * framework calling either name has to be intercepted on the right symbol. */
extern CUresult _cuGraphInstantiate(CUgraphExec *phGraphExec, CUgraph hGraph,
                                    CUgraphNode *phErrorNode, char *logBuffer,
                                    size_t bufferSize);

static CUresult instantiate_and_capture(CUgraphExec *phGraphExec, CUgraph hGraph,
                                        CUgraphNode *phErrorNode, char *logBuffer,
                                        size_t bufferSize) {
  CUresult ret = _cuGraphInstantiate(phGraphExec, hGraph, phErrorNode,
                                     logBuffer, bufferSize);
  if (ret == CUDA_SUCCESS && phGraphExec) {
    capture_graph_cost(*phGraphExec, hGraph);
  }
  return ret;
}

CUresult cuGraphInstantiate(CUgraphExec *phGraphExec, CUgraph hGraph,
                            CUgraphNode *phErrorNode, char *logBuffer,
                            size_t bufferSize) {
  return instantiate_and_capture(phGraphExec, hGraph, phErrorNode,
                                 logBuffer, bufferSize);
}

CUresult cuGraphInstantiate_v2(CUgraphExec *phGraphExec, CUgraph hGraph,
                               CUgraphNode *phErrorNode, char *logBuffer,
                               size_t bufferSize) {
  return instantiate_and_capture(phGraphExec, hGraph, phErrorNode,
                                 logBuffer, bufferSize);
}

CUresult cuGraphInstantiateWithFlags(CUgraphExec *phGraphExec, CUgraph hGraph,
                                     unsigned long long flags) {
  CUresult ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphInstantiateWithFlags,
                                  phGraphExec, hGraph, flags);
  if (ret == CUDA_SUCCESS && phGraphExec) {
    capture_graph_cost(*phGraphExec, hGraph);
  }
  return ret;
}

CUresult cuGraphInstantiateWithParams(CUgraphExec *phGraphExec, CUgraph hGraph,
                                      CUDA_GRAPH_INSTANTIATE_PARAMS *instantiateParams) {
  CUresult ret = CUDA_ENTRY_CHECK(cuda_library_entry,
                                  __CUDA_API_PTSZ(cuGraphInstantiateWithParams),
                                  phGraphExec, hGraph, instantiateParams);
  if (ret == CUDA_SUCCESS && phGraphExec) {
    capture_graph_cost(*phGraphExec, hGraph);
  }
  return ret;
}

CUresult cuGraphInstantiateWithParams_ptsz(CUgraphExec *phGraphExec, CUgraph hGraph,
                                           CUDA_GRAPH_INSTANTIATE_PARAMS *instantiateParams) {
  CUresult ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphInstantiateWithParams_ptsz,
                                  phGraphExec, hGraph, instantiateParams);
  if (ret == CUDA_SUCCESS && phGraphExec) {
    capture_graph_cost(*phGraphExec, hGraph);
  }
  return ret;
}

CUresult cuGraphLaunch(CUgraphExec hGraphExec, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) goto DONE;
  int host_index = get_host_device_index_by_cuda_device(device);
  int grids = 1, blocks = 1;
  graph_cost_lookup(hGraphExec, &grids, &blocks);
  rate_limiter(grids, blocks, host_index);
  int gap = gap_begin(host_index, hStream);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuGraphLaunch),
                         hGraphExec, hStream);
  if (gap) gap_end(host_index, hStream, ret);
DONE:
  return ret;
}

CUresult cuGraphLaunch_ptsz(CUgraphExec hGraphExec, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) goto DONE;
  int host_index = get_host_device_index_by_cuda_device(device);
  int grids = 1, blocks = 1;
  graph_cost_lookup(hGraphExec, &grids, &blocks);
  rate_limiter(grids, blocks, host_index);
  int gap = gap_begin(host_index, hStream);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphLaunch_ptsz,
                         hGraphExec, hStream);
  if (gap) gap_end(host_index, hStream, ret);
DONE:
  return ret;
}

CUresult cuGraphExecDestroy(CUgraphExec hGraphExec) {
  CUresult ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecDestroy, hGraphExec);
  if (ret == CUDA_SUCCESS) {
    graph_cost_forget(hGraphExec);
  }
  return ret;
}

/* cuGraphExecUpdate / _v2 replace an existing exec's contents with a new
 * source graph (PyTorch dynamic-shape inference is the canonical caller).
 * Without a refresh, our cached cost for `hGraphExec` would describe the
 * OLD graph forever. We get the new graph as the second arg, so re-walk
 * and overwrite the cache entry.
 *
 * Two ABIs intercepted independently:
 *   - v1 (CUDA 11.2-11.7): 4 args, errNode_out + result_out as separate
 *     output pointers.
 *   - _v2 (CUDA 12.0+): 3 args, both outputs packed into CUgraphExecUpdate
 *     ResultInfo*. Same exec + graph at positions 1,2 so the cache refresh
 *     is identical -- the helper below absorbs the common tail.
 *
 * cuGetProcAddress("cuGraphExecUpdate", ..., cudaVersion, ...) routes
 * version-by-version to the right ABI's hook -- see substitute_version_aware
 * in cuGetProcAddress / cuGetProcAddress_v2.
 *
 * Note we deliberately do NOT call graph_cost_forget() before the walk:
 * graph_cost_remember() overwrites an existing entry in place, and dropping
 * the entry first would create a ~100µs window (covering the new graph's
 * walk) during which concurrent cuGraphLaunch on this exec would miss the
 * cache and degrade to grids=1 (effectively unthrottled). cuGraphExecUpdate
 * only succeeds when the topology is unchanged, so the OLD cost is a close
 * approximation to the NEW cost -- much better than grids=1 for that window. */
static void refresh_cost_on_update(CUresult ret, CUgraphExec hGraphExec, CUgraph hGraph) {
  if (ret == CUDA_SUCCESS) capture_graph_cost(hGraphExec, hGraph);
}

CUresult cuGraphExecUpdate(CUgraphExec hGraphExec, CUgraph hGraph,
                           CUgraphNode *hErrorNode_out,
                           CUgraphExecUpdateResult *updateResult_out) {
  CUresult ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecUpdate,
                                  hGraphExec, hGraph, hErrorNode_out,
                                  updateResult_out);
  refresh_cost_on_update(ret, hGraphExec, hGraph);
  return ret;
}

CUresult cuGraphExecUpdate_v2(CUgraphExec hGraphExec, CUgraph hGraph,
                              CUgraphExecUpdateResultInfo *resultInfo) {
  CUresult ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphExecUpdate_v2,
                                  hGraphExec, hGraph, resultInfo);
  refresh_cost_on_update(ret, hGraphExec, hGraph);
  return ret;
}

CUresult cuFuncSetBlockShape(CUfunction hfunc, int x, int y, int z) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  if (get_device_flag(host_index, core_limit)) {
    while (!CAS(&g_block_locker[host_index], 0, 1)) {}

    g_block_x[host_index] = x;
    g_block_y[host_index] = y;
    g_block_z[host_index] = z;

    LOGGER(VERBOSE, "cuda device %d => host device %d, set block shape: %d, %d, %d", device, host_index, x, y, z);

    while (!CAS(&g_block_locker[host_index], 1, 0)) {}
  }
CALL:
  ret =  CUDA_ENTRY_CHECK(cuda_library_entry, cuFuncSetBlockShape, hfunc, x, y, z);
DONE:
  return ret;
}

CUresult cuMemAllocFromPoolAsync(CUdeviceptr *dptr, size_t bytesize,
                                 CUmemoryPool pool, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  memory_path_t path;
  /* Declared before `goto CALL`; see _cuArrayCreate. */
  int host_index = -1;
  /* NULL-arg fast path; same rationale as _cuMemAlloc above. */
  if (unlikely(dptr == NULL)) {
    goto CALL;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t request_size = bytesize;
  path = prepare_memory_allocation(device, request_size, 0, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemAllocFromPoolAsync), dptr, bytesize, pool, hStream);
  if (ret == CUDA_SUCCESS && lock_fd >= 0) {
    if (!stream_is_capturing(hStream, __CUDA_API_IS_PTSZ)) {
      /* Bridge the allocation into the shared view before the lock is dropped,
       * so a concurrent allocator counts it while it is still in flight. */
      malloc_gpu_virt_memory(*dptr, bytesize, MEMORY_TYPE_ASYNC_BRIDGE, host_index);
      unlock_gpu_device(lock_fd);
      lock_fd = -1;
      /* Switching from capture to synchronous operation, off the device lock. */
      CUDA_INTERNAL_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuStreamSynchronize), hStream);
      /* Release internal accounting only once NVML is guaranteed to see it. */
      free_gpu_virt_memory(*dptr);
    } else {
      /* Charge the capture so that several allocations inside one capture
       * accumulate against the limit. Chargeable only while the owning graph is
       * known, because that is what lets cuStreamEndCapture retire it. */
      CUgraph graph = stream_capture_graph(hStream, __CUDA_API_IS_PTSZ);
      if (graph != NULL) {
        malloc_gpu_virt_memory_captured(*dptr, bytesize, graph, host_index);
      }
    }
  } else if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuMemAllocFromPoolAsync_ptsz(CUdeviceptr *dptr, size_t bytesize,
                                      CUmemoryPool pool, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  memory_path_t path;
  /* Declared before `goto CALL`; see _cuArrayCreate. */
  int host_index = -1;
  /* NULL-arg fast path; same rationale as _cuMemAlloc above. */
  if (unlikely(dptr == NULL)) {
    goto CALL;
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t request_size = bytesize;
  path = prepare_memory_allocation(device, request_size, 0, &host_index, &lock_fd);
  if (path == MEMORY_PATH_OOM) {
    metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
    ret = CUDA_ERROR_OUT_OF_MEMORY;
    goto DONE;
  }
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocFromPoolAsync_ptsz, dptr, bytesize, pool, hStream);
  // Confirm successful locking and waive the cost of unopened containers
  if (ret == CUDA_SUCCESS && lock_fd >= 0) {
    if (!stream_is_capturing(hStream, 1)) {
      /* Bridge the allocation into the shared view before the lock is dropped,
       * so a concurrent allocator counts it while it is still in flight. */
      malloc_gpu_virt_memory(*dptr, bytesize, MEMORY_TYPE_ASYNC_BRIDGE, host_index);
      unlock_gpu_device(lock_fd);
      lock_fd = -1;
      /* Switching from capture to synchronous operation, off the device lock. */
      CUDA_INTERNAL_CHECK(cuda_library_entry, cuStreamSynchronize_ptsz, hStream);
      /* Release internal accounting only once NVML is guaranteed to see it. */
      free_gpu_virt_memory(*dptr);
    } else {
      /* Charge the capture so that several allocations inside one capture
       * accumulate against the limit. Chargeable only while the owning graph is
       * known, because that is what lets cuStreamEndCapture retire it. */
      CUgraph graph = stream_capture_graph(hStream, 1);
      if (graph != NULL) {
        malloc_gpu_virt_memory_captured(*dptr, bytesize, graph, host_index);
      }
    }
  } else if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
    metrics_record_oom(host_index, METRICS_OOM_DRIVER_RETURN);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult _cuMemFree(CUdeviceptr dptr) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemFree_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemFree_v2, dptr);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemFree))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemFree, dptr);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  if (likely(ret == CUDA_SUCCESS)) {
    free_gpu_virt_memory(dptr);
  }
DONE:
  return ret;
}

CUresult cuMemFree_v2(CUdeviceptr dptr) {
  return _cuMemFree(dptr);
}

CUresult cuMemFree(CUdeviceptr dptr) {
  return _cuMemFree(dptr);
}

/* Release a pointer that the oversold UVA fallback allocated with
 * cuMemAllocManaged: the driver's async free rejects it, which would leave both
 * the device allocation and our virtual-memory accounting leaked. cuMemFree is
 * not stream-ordered, so drain the stream first to preserve the ordering the
 * caller asked cuMemFreeAsync for. */
/* ptsz selects which default stream hStream==0 denotes, so both the capture
 * query and the synchronize must come from the same family as the
 * cuMemFreeAsync entry point we intercepted. */
static CUresult free_virt_memory_uva_on_stream(CUdeviceptr dptr, CUstream hStream, int ptsz) {
  CUresult ret;
  /* The pointer reached us from the oversold UVA fallback, so the only way to
   * release it is the non-stream-ordered cuMemFree, which has to drain the
   * stream first -- and there is nothing equivalent to record into a graph.
   *
   * Draining a capturing stream does not merely fail: measured against the
   * driver, cuStreamSynchronize returns CUDA_ERROR_STREAM_CAPTURE_UNSUPPORTED
   * *and* invalidates the capture, so the app's later cuStreamEndCapture fails
   * with CUDA_ERROR_STREAM_CAPTURE_INVALIDATED and it loses the graph. Bailing
   * out before touching the stream is therefore what saves the capture; the
   * status code below is the same one the driver would have produced anyway.
   *
   * The allocation is left live. Nothing can free it here, so the caller gets a
   * status that names the reason instead of a corrupted capture or the driver's
   * bare CUDA_ERROR_INVALID_VALUE: oversold UVA and graph capture are simply
   * incompatible. The alloc side already declines to hand out UVA pointers
   * mid-capture, so this only fires for one allocated before capture began. */
  if (unlikely(stream_is_capturing(hStream, ptsz))) {
    LOGGER(ERROR, "cuMemFreeAsync on unified memory (oversold) inside a graph "
                  "capture is not supported, dptr %lld (allocation retained)", dptr);
    return CUDA_ERROR_STREAM_CAPTURE_UNSUPPORTED;
  }
  if (ptsz) {
    ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuStreamSynchronize_ptsz, hStream);
  } else {
    ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuStreamSynchronize, hStream);
  }
  if (unlikely(ret != CUDA_SUCCESS)) {
    return ret;
  }
  LOGGER(VERBOSE, "cuMemFreeAsync on unified memory (oversold), dptr %lld, "
                  "falling back to cuMemFree", dptr);
  return _cuMemFree(dptr);
}

CUresult cuMemFreeAsync(CUdeviceptr dptr, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  int type = get_gpu_virt_memory_type(dptr);
  if (type == MEMORY_TYPE_UVA_ASYNC) {
    // If the memory type is asynchronous UVA memory record, go this branch
    return free_virt_memory_uva_on_stream(dptr, hStream, __CUDA_API_IS_PTSZ);
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemFreeAsync), dptr, hStream);
  if (likely(ret == CUDA_SUCCESS)) {
    if (type != 0) {
      free_gpu_virt_memory(dptr);
    }
  }
DONE:
  return ret;
}

CUresult cuMemFreeAsync_ptsz(CUdeviceptr dptr, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  int type = get_gpu_virt_memory_type(dptr);
  if (type == MEMORY_TYPE_UVA_ASYNC) {
    // If the memory type is asynchronous UVA memory record, go this branch
    return free_virt_memory_uva_on_stream(dptr, hStream, 1);
  }
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemFreeAsync_ptsz, dptr, hStream);
  if (likely(ret == CUDA_SUCCESS)) {
    if (type != 0) {
      free_gpu_virt_memory(dptr);
    }
  }
DONE:
  return ret;
}

/* End of capture is where a capture charge is retired: past this point the
 * allocations belong to the graph, and once it is launched NVML reports them.
 *
 * Two things are deliberately unconditional here.
 *
 * The graph is resolved BEFORE ending the capture, because a capture that has
 * been invalidated makes cuStreamEndCapture fail and leave *phGraph NULL, and
 * the charge still has to come off in that case.
 *
 * The discharge itself ignores the driver's verdict. Whether the graph was
 * built or thrown away, no memory is held on its behalf yet, so there is
 * nothing a failure could justify keeping. Any path that skips the discharge
 * strands the charge in the shared counter for the life of the process, which
 * is a false OOM waiting to happen -- strictly worse than briefly under-counting
 * memory that does not exist yet. free_gpu_virt_memory_by_graph() therefore
 * needs no device argument and cannot fail. */
static CUresult end_capture_and_discharge(CUstream hStream, CUgraph *phGraph, int ptsz) {
  CUgraph capturing = stream_capture_graph(hStream, ptsz);
  CUresult ret = ptsz
      ? CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamEndCapture_ptsz, hStream, phGraph)
      : CUDA_ENTRY_CHECK(cuda_library_entry, cuStreamEndCapture, hStream, phGraph);
  free_gpu_virt_memory_by_graph(capturing);
  return ret;
}

CUresult cuStreamEndCapture(CUstream hStream, CUgraph *phGraph) {
  return end_capture_and_discharge(hStream, phGraph, __CUDA_API_IS_PTSZ);
}

CUresult cuStreamEndCapture_ptsz(CUstream hStream, CUgraph *phGraph) {
  return end_capture_and_discharge(hStream, phGraph, 1);
}

/* Second, deterministic discharge point for capture charges.
 *
 * cuStreamEndCapture retires the normal case, but a capture that was
 * invalidated may not report its graph any more, and CUDA offers no way to test
 * whether a CUgraph is still alive -- handles are recycled and probing a
 * destroyed one is undefined. Destruction, on the other hand, is something the
 * application tells us, and it names the graph exactly. Discharging here as well
 * is safe because free_gpu_virt_memory_by_graph() is idempotent: the normal path
 * has already emptied the graph's records by the time it is destroyed, so this
 * only ever fires for charges nothing else claimed. */
CUresult cuGraphDestroy(CUgraph hGraph) {
  CUresult ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGraphDestroy, hGraph);
  free_gpu_virt_memory_by_graph(hGraph);
  return ret;
}

CUresult _cuMemHostRegister(void *p, size_t bytesize, unsigned int Flags) {
  int lock_fd = -1;
  CUresult ret = CUDA_ERROR_NOT_FOUND;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemHostRegister_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemHostRegister_v2, p, bytesize, Flags);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemHostRegister))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemHostRegister, p, bytesize, Flags);
  }
  if (likely(ret == CUDA_SUCCESS)) {
    CUdevice device;
    if (unlikely(CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device) != CUDA_SUCCESS)) {
      goto DONE;
    }
    // Post check for exceeding memory limit.
    int host_index = -1;
    if (prepare_memory_allocation(device, 0, 0, &host_index, &lock_fd) == MEMORY_PATH_OOM) {
      metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
      CUDA_INTERNAL_CHECK(cuda_library_entry, cuMemHostUnregister, p);
      ret = CUDA_ERROR_OUT_OF_MEMORY;
    }
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuMemHostRegister_v2(void *p, size_t bytesize, unsigned int Flags) {
  return _cuMemHostRegister(p, bytesize, Flags);
}

CUresult cuMemHostRegister(void *p, size_t bytesize, unsigned int Flags) {
  return _cuMemHostRegister(p, bytesize, Flags);
}

CUresult cuMemHostAlloc(void **pp, size_t bytesize, unsigned int Flags) {
  int lock_fd = -1;
  CUresult ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemHostAlloc, pp, bytesize, Flags);
  if (likely(ret == CUDA_SUCCESS)) {
    CUdevice device;
    if (unlikely(CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device) != CUDA_SUCCESS)) {
      goto DONE;
    }
    // Post check for exceeding memory limit.
    int host_index = -1;
    if (prepare_memory_allocation(device, 0, 0, &host_index, &lock_fd) == MEMORY_PATH_OOM) {
      metrics_record_oom(host_index, METRICS_OOM_TOTAL_LIMIT);
      if (CUDA_INTERNAL_CHECK(cuda_library_entry, cuMemFreeHost, *pp) == CUDA_SUCCESS) {
        *pp = NULL;
      }
      ret = CUDA_ERROR_OUT_OF_MEMORY;
    }
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

