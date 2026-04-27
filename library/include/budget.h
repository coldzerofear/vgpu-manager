/*
 * Cross-API enforcement interface (memory budget + SM rate limit).
 *
 * vgpu-manager hooks more than one GPU API surface (CUDA today, Vulkan in
 * progress). The enforcement primitives — per-device memory budget and
 * core-time rate limiting — are API-neutral. This header is the single
 * public entry point for any hook layer that needs to consult them
 * before allowing an allocation or a queue submission.
 *
 * What lives here:
 *   - vgpu_path_t     : "what should the caller do" decision result
 *   - vgpu_budget_kind_t : whether the caller can use UVA-style oversold
 *                          capacity (CUDA) or only physical (Vulkan)
 *   - prepare_memory_allocation : CUDA-side memory budget entry
 *   - vgpu_check_alloc_budget   : host-index-keyed memory budget entry
 *                                  (Vulkan / any future non-CUDA path)
 *   - vgpu_rate_limit_by_host_index : host-index-keyed SM rate limit
 *                                      (shared core; CUDA wraps it via
 *                                      its CUdevice-keyed rate_limiter)
 *   - vgpu_ensure_sm_watcher_started : pthread_once-trigger for the
 *                                       SM utilisation watcher thread,
 *                                       so non-CUDA hook layers can
 *                                       front-load rate-limiter readiness
 *   - get_host_device_index_by_uuid_bytes : raw 16-byte UUID lookup,
 *                                           used by Vulkan's
 *                                           VkPhysicalDeviceIDProperties
 *                                           ::deviceUUID resolution
 *
 * Both the CUdevice-keyed (CUDA) and host-index-keyed (Vulkan) budget
 * checks share the same internal decision logic; see cuda_hook.c.
 */
#ifndef VGPU_BUDGET_H
#define VGPU_BUDGET_H

#include <stddef.h>
#include <stdint.h>

#include "cuda-subset.h"   /* CUdevice, CUdeviceptr */

#ifdef __cplusplus
extern "C" {
#endif

/*
 * Decision returned by the budget check.
 *
 * GPU : caller can proceed with a physical allocation
 * UVA : (CUDA only, oversold mode) caller should fall back to
 *       cuMemAllocManaged / managed memory; physical is full
 * OOM : the request would exceed the configured budget; reject
 */
typedef enum {
  VGPU_PATH_GPU = 0,
  VGPU_PATH_UVA = 1,
  VGPU_PATH_OOM = 2,
} vgpu_path_t;

/*
 * Upper-bound semantics requested by the caller.
 *
 * VIRTUAL : Cap = g_vgpu_config[host_index].total_memory.
 *           Includes oversold UVA capacity. The CUDA hooks pass this
 *           because cuMemAllocManaged can legitimately consume the
 *           oversold amount via host paging.
 *
 * PHYSICAL: Cap = g_vgpu_config[host_index].real_memory.
 *           The hard physical cap. Vulkan and any future
 *           physical-only API uses this. Forces allow_uva = 0
 *           internally - no UVA fallback can ever be returned.
 *           Heap-size reporting paths that drive direct allocation
 *           sizing should also use this.
 */
typedef enum {
  VGPU_BUDGET_KIND_VIRTUAL  = 0,
  VGPU_BUDGET_KIND_PHYSICAL = 1,
} vgpu_budget_kind_t;

/*
 * CUDA-side budget check.
 *
 * Resolves CUdevice -> host_index, takes the device lock, queries
 * NVML-derived used + tracked vmem_used, and compares against the cap
 * dictated by `kind`. Returns:
 *   - VGPU_PATH_GPU  : caller may proceed with physical alloc
 *   - VGPU_PATH_UVA  : (only when kind=VIRTUAL && allow_uva && oversold)
 *                       caller should fall back to managed memory
 *   - VGPU_PATH_OOM  : reject the alloc; caller should return OOM error
 *
 * On any non-OOM return the device lock_fd is held; caller must release
 * via unlock_gpu_device(*lock_fd) once done. On OOM the lock is also
 * held so the caller can perform metrics_record_oom() before unlocking.
 *
 * If the device is not under our memory_limit (host_index < 0 or
 * memory_limit == 0), returns VGPU_PATH_GPU and *lock_fd = -1.
 */
vgpu_path_t prepare_memory_allocation(CUdevice device,
                                      size_t request_size,
                                      vgpu_budget_kind_t kind,
                                      int allow_uva,
                                      int *host_index,
                                      int *lock_fd);

/*
 * Look up host_index by raw 16-byte device UUID.
 *
 * Vulkan's VkPhysicalDeviceIDProperties::deviceUUID is exactly this
 * format; NVML's nvmlDeviceGetUUID returns the same 16 bytes wrapped in
 * the "GPU-xxxxxxxx-..." string. This helper formats the bytes into the
 * canonical string form and dispatches to get_host_device_index_by_uuid().
 *
 * On miss (no matching device in g_vgpu_config or the UUID was never
 * registered), *host_index is left unchanged. Callers should pre-set it
 * to -1 if they care to detect the miss case.
 */
void get_host_device_index_by_uuid_bytes(const uint8_t uuid[16],
                                         int *host_index);

/*
 * Host-index-keyed budget check for non-CUDA hook layers (Vulkan today).
 *
 * Same decision shape as prepare_memory_allocation: lock the device,
 * read NVML-aggregated `used` plus tracked `vmem_used`, compare against
 * the cap dictated by `kind`, return GPU/UVA/OOM. Differs only in input
 * surface — caller already has a host_index (e.g. resolved from a
 * VkPhysicalDevice via deviceUUID), no CUdevice in scope.
 *
 * `used` view comes from NVML's compute+graphics process aggregation
 * (already PID-deduped in get_used_gpu_memory_by_device), so a process
 * mixing CUDA and Vulkan only contributes its physical footprint once.
 *
 * Lock ownership contract is identical to prepare_memory_allocation:
 *   - on any non-OOM return the device lock_fd is held; caller must
 *     unlock_gpu_device(*lock_fd) once forwarding completes
 *   - on OOM the lock is held so caller can metrics_record_oom() first
 *   - if the device is unmanaged (host_index out of range, memory_limit==0,
 *     or NVML resolution failed), returns VGPU_PATH_GPU with *lock_fd=-1
 *
 * Vulkan callers MUST pass kind=PHYSICAL and allow_uva=0 — Vulkan has no
 * UVA fallback path. The function defends against misuse internally
 * (PHYSICAL forces allow_uva=0).
 */
vgpu_path_t vgpu_check_alloc_budget(int host_index,
                                    size_t request_size,
                                    vgpu_budget_kind_t kind,
                                    int allow_uva,
                                    int *lock_fd);

/*
 * Host-index-keyed SM rate limit. Same CAS-decrement-on-token-bucket
 * core as cuda_hook.c's CUdevice-keyed rate_limiter; both routes call
 * into this function. CUDA call sites continue to use the static
 * wrapper that resolves CUdevice -> host_index first, so their
 * signature is unchanged.
 *
 * `kernel_size` is the number of tokens consumed by this submission.
 * For CUDA kernel launches, callers pass grids (gridDimX*Y*Z) so the
 * claim approximates compute volume. For Vulkan vkQueueSubmit /
 * vkQueueSubmit2[KHR] the layer passes 1 — vkQueueSubmit's command-
 * buffer payload size is opaque from the layer side, so we use a
 * minimal claim. Throttle precision still comes from the watcher
 * thread's per-window share adjustment, not from per-call magnitude.
 *
 * No-ops if:
 *   - host_index is out of range or unmanaged
 *   - g_vgpu_config[host_index].core_limit is false (no rate limit)
 *
 * Precondition: vgpu_ensure_sm_watcher_started has been called at
 * least once and the watcher thread is replenishing tokens; otherwise
 * the first decrement could push g_cur_cuda_cores negative and stall
 * the calling thread indefinitely. CUDA bootstrap entries
 * (cuInit / cuGetProcAddress / cuDriverGetVersion) and the Vulkan
 * vk_layer_CreateInstance both satisfy this precondition.
 */
void vgpu_rate_limit_by_host_index(int kernel_size, int host_index);

/*
 * pthread_once-guarded trigger for cuda_hook.c's initialization() —
 * which runs cuInit, populates g_total_cuda_cores / nvml_devices and
 * starts the SM utilization_watcher thread. Idempotent across all
 * call sites.
 *
 * CUDA hooks call this implicitly via their own pthread_once on the
 * same g_init_set; the Vulkan layer calls this explicitly at
 * vk_layer_CreateInstance success so a pure-Vulkan process gets the
 * watcher running before its first vkQueueSubmit.
 */
void vgpu_ensure_sm_watcher_started(void);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_BUDGET_H */
