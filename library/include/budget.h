/*
 * Cross-API memory budget interface.
 *
 * vgpu-manager hooks more than one GPU API surface (CUDA today, Vulkan in
 * progress). The budget bookkeeping itself - per-device limits, used /
 * vmem_used accounting, oversold UVA fallback decision - is API-neutral.
 * This header is the single public entry point for any hook layer that
 * needs to consult the budget before allowing an allocation.
 *
 * What lives here:
 *   - vgpu_path_t     : "what should the caller do" decision result
 *   - vgpu_budget_kind_t : whether the caller can use UVA-style oversold
 *                          capacity (CUDA) or only physical (Vulkan)
 *   - prepare_memory_allocation : CUDA-side entry that resolves CUdevice
 *                                  to host_index, locks, and decides
 *   - get_host_device_index_by_uuid_bytes : raw 16-byte UUID lookup,
 *                                           used by Vulkan's
 *                                           VkPhysicalDeviceIDProperties
 *                                           ::deviceUUID resolution
 *
 * What does NOT live here yet:
 *   - The host_index-keyed budget check (vgpu_check_alloc_budget). Will
 *     be added in Phase 5 of the Vulkan layer rollout when the consumer
 *     side actually exists. See docs/how_to_develop_vulkan_implicit_layer.md.
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

#ifdef __cplusplus
}
#endif

#endif /* VGPU_BUDGET_H */
