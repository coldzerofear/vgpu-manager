/*
 * vkAllocateMemory / vkFreeMemory hooks.
 *
 * Allocation flow:
 *   1. lookup device dispatch + physical_device
 *   2. resolve host_index via the physdev_index cache
 *   3. unmanaged short-circuit: host_index < 0 => forward
 *      (handled implicitly by vgpu_check_alloc_budget returning GPU
 *       with lock_fd = -1; we still call it for symmetry, but the path
 *       is effectively "no-op enforce")
 *   4. import short-circuit: pAllocateInfo->pNext carries an
 *      "import existing external memory" structure => forward without
 *      budget check (avoids the double-count failure mode described in
 *      vk_alloc_is_import below)
 *   5. budget check via vgpu_check_alloc_budget(host_index, size,
 *      PHYSICAL, allow_uva=0). PHYSICAL caps at real_memory and forces
 *      allow_uva=0 internally; UVA can never be returned.
 *   6. on OOM => unlock and return VK_ERROR_OUT_OF_DEVICE_MEMORY
 *   7. on GPU => forward to next layer; either way unlock at the end
 *
 * Free flow: transparent forward. NVML's per-process memory view will
 * pick up the release on the next query; we keep no per-allocation
 * state of our own.
 */
#include <stddef.h>
#include <stdint.h>

#include <vulkan/vulkan.h>

#include "include/budget.h"
#include "include/hook.h"   /* LOGGER, unlock_gpu_device */

#include "include/vulkan/dispatch.h"
#include "include/vulkan/physdev_index.h"
#include "include/vulkan/hooks_alloc.h"

extern void unlock_gpu_device(int fd);

/* ------------------------------------------------------------------ */
/* Import path detection.                                              */
/* ------------------------------------------------------------------ */

/*
 * Walk pAllocateInfo->pNext for any structure that turns this allocation
 * into "import an existing external memory handle" rather than a fresh
 * physical allocation.
 *
 * Why short-circuit imports:
 *   The `used` view in vgpu_check_alloc_budget comes from NVML's
 *   compute+graphics process aggregation, which is already PID-deduped
 *   (commit 20e9519). For an import the underlying physical memory is
 *   *already* reflected in `used` (the export side allocated it). The
 *   OOM formula `(used + request_size) > cap` would then count the same
 *   physical memory twice — once in `used`, once in `request_size` —
 *   and falsely reject CUDA-Vulkan interop allocations that fit
 *   physically (Isaac Sim, Omniverse, video render pipelines).
 *
 * Bypass-via-false-positive analysis:
 *   A malicious application cannot use a forged import to bypass the
 *   budget. The driver enforces that import requires a valid external
 *   handle whose attributes (handleType, allocationSize) match the
 *   originating allocation; otherwise vkAllocateMemory fails with
 *   VK_ERROR_INVALID_EXTERNAL_HANDLE without consuming any physical
 *   memory. Valid handles can only originate from a previously
 *   budgeted allocation (CUDA cuMemAlloc or Vulkan vkAllocateMemory).
 *   So skipping the budget check on import is correct: the cost was
 *   paid at export time.
 *
 * Defense-in-depth: we additionally require the import structure to
 * carry a non-trivial handle (fd >= 0 / pHostPointer != NULL) before
 * granting the short-circuit. A bogus import that the driver would
 * reject anyway falls through to normal budget enforcement so it does
 * not become a probing oracle for budget state.
 *
 * VkExportMemoryAllocateInfo is intentionally not in this list — export
 * is still a new physical allocation and must be budgeted.
 */
static int vk_alloc_is_import(const VkMemoryAllocateInfo *info) {
  if (info == NULL) return 0;
  for (const VkBaseInStructure *p = (const VkBaseInStructure *)info->pNext;
       p != NULL; p = p->pNext) {
    switch ((int)p->sType) {
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR: {
        const VkImportMemoryFdInfoKHR *imp =
            (const VkImportMemoryFdInfoKHR *)p;
        if (imp->fd >= 0 && imp->handleType != 0) return 1;
        break;
      }
#ifdef VK_EXT_external_memory_host
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_HOST_POINTER_INFO_EXT: {
        const VkImportMemoryHostPointerInfoEXT *imp =
            (const VkImportMemoryHostPointerInfoEXT *)p;
        if (imp->pHostPointer != NULL && imp->handleType != 0) return 1;
        break;
      }
#endif
      /* Out-of-platform sTypes: never expected on Linux containers but
       * harmless to accept. We do not deep-inspect their handle fields
       * to avoid pulling in cross-platform header dependencies.
       * False-positive bypass is impossible (see analysis above). */
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_WIN32_HANDLE_INFO_KHR:
#ifdef VK_USE_PLATFORM_ANDROID_KHR
      case VK_STRUCTURE_TYPE_IMPORT_ANDROID_HARDWARE_BUFFER_INFO_ANDROID:
#endif
#ifdef VK_USE_PLATFORM_FUCHSIA
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_ZIRCON_HANDLE_INFO_FUCHSIA:
#endif
        return 1;
      default:
        break;
    }
  }
  return 0;
}

/* ------------------------------------------------------------------ */
/* vkAllocateMemory                                                    */
/* ------------------------------------------------------------------ */

VKAPI_ATTR VkResult VKAPI_CALL
vgpu_vk_AllocateMemory(VkDevice                     device,
                       const VkMemoryAllocateInfo  *pAllocateInfo,
                       const VkAllocationCallbacks *pAllocator,
                       VkDeviceMemory              *pMemory) {
  vgpu_vk_device_dispatch_t *d = vgpu_vk_get_device_dispatch(device);
  if (d == NULL || d->pfn_AllocateMemory == NULL) {
    /* No dispatch entry / next-layer pfn missing — we cannot forward.
     * Return the same error a Vulkan loader would surface for an
     * unroutable call instead of crashing. */
    return VK_ERROR_INITIALIZATION_FAILED;
  }
  if (pAllocateInfo == NULL || pMemory == NULL) {
    /* Spec says these must be non-NULL; let the next layer surface the
     * canonical validation error. */
    return d->pfn_AllocateMemory(device, pAllocateInfo, pAllocator, pMemory);
  }

  int host_index = vgpu_vk_physdev_to_host_index(d->physical_device);

  /* Import path short-circuit. Logged at DETAIL so operators can
   * correlate "Vulkan alloc accepted with no budget delta" against
   * a specific call site if needed. */
  if (host_index >= 0 && vk_alloc_is_import(pAllocateInfo)) {
    LOGGER(DETAIL,
           "vkAllocateMemory: import path detected, skipping budget "
           "(host_index=%d size=%zu)",
           host_index, (size_t)pAllocateInfo->allocationSize);
    return d->pfn_AllocateMemory(device, pAllocateInfo, pAllocator, pMemory);
  }

  /* Budget check. Vulkan callers MUST pass PHYSICAL/allow_uva=0; the
   * helper enforces that internally too as a defensive guard. */
  int lock_fd = -1;
  vgpu_path_t path = vgpu_check_alloc_budget(host_index,
                                             (size_t)pAllocateInfo->allocationSize,
                                             VGPU_BUDGET_KIND_PHYSICAL,
                                             /*allow_uva*/ 0,
                                             &lock_fd);

  if (path == VGPU_PATH_OOM) {
    LOGGER(VERBOSE,
           "vkAllocateMemory: OOM at host_index=%d size=%zu, "
           "returning VK_ERROR_OUT_OF_DEVICE_MEMORY",
           host_index, (size_t)pAllocateInfo->allocationSize);
    if (lock_fd >= 0) unlock_gpu_device(lock_fd);
    return VK_ERROR_OUT_OF_DEVICE_MEMORY;
  }
  /* PATH_UVA is structurally unreachable here (PHYSICAL forces
   * allow_uva=0). If a future refactor breaks that invariant we still
   * proceed safely — there is no UVA fallback in Vulkan, so we just
   * forward as if it were a plain GPU grant. */

  VkResult r = d->pfn_AllocateMemory(device, pAllocateInfo, pAllocator, pMemory);
  if (lock_fd >= 0) unlock_gpu_device(lock_fd);
  return r;
}

/* ------------------------------------------------------------------ */
/* vkFreeMemory                                                        */
/* ------------------------------------------------------------------ */

VKAPI_ATTR void VKAPI_CALL
vgpu_vk_FreeMemory(VkDevice                     device,
                   VkDeviceMemory               memory,
                   const VkAllocationCallbacks *pAllocator) {
  vgpu_vk_device_dispatch_t *d = vgpu_vk_get_device_dispatch(device);
  if (d == NULL || d->pfn_FreeMemory == NULL) return;
  d->pfn_FreeMemory(device, memory, pAllocator);
}
