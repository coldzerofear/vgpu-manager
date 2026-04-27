/*
 * vkAllocateMemory / vkFreeMemory hooks.
 *
 * Single concern: enforce the per-pod vGPU memory budget on Vulkan
 * device-memory allocations. Cap selection mirrors the heap clamp in
 * hooks_memory.c (real_memory under PHYSICAL kind), so the upper bound
 * the application sees via vkGetPhysicalDeviceMemoryProperties matches
 * the upper bound we will accept here.
 *
 * Import path short-circuit: when pAllocateInfo->pNext
 * carries an "import existing external memory" structure, the call is
 * not a new physical allocation — it is another VkDeviceMemory handle
 * pointing at memory that was already allocated (and already accounted
 * for) elsewhere (CUDA cuMemAlloc + export, or a prior Vulkan
 * vkAllocateMemory + export). Adding `request_size` to `used` for these
 * calls would double-count the underlying physical memory and falsely
 * report OOM. See vk_alloc_is_import in hooks_alloc.c for the bypass
 * analysis (false-positive cannot grant a budget bypass because the
 * Vulkan driver itself enforces that import requires a valid external
 * handle, which can only come from a previously-budgeted allocation).
 *
 * vkFreeMemory is a transparent forward — used view is NVML-derived
 * and reflects the release automatically once the driver returns.
 */
#ifndef VGPU_VULKAN_HOOKS_ALLOC_H
#define VGPU_VULKAN_HOOKS_ALLOC_H

#include <vulkan/vulkan.h>

#include "dispatch.h"   /* VGPU_VK_INTERNAL */

#ifdef __cplusplus
extern "C" {
#endif

VGPU_VK_INTERNAL VKAPI_ATTR VkResult VKAPI_CALL
vgpu_vk_AllocateMemory(VkDevice                     device,
                       const VkMemoryAllocateInfo  *pAllocateInfo,
                       const VkAllocationCallbacks *pAllocator,
                       VkDeviceMemory              *pMemory);

VGPU_VK_INTERNAL VKAPI_ATTR void VKAPI_CALL
vgpu_vk_FreeMemory(VkDevice                     device,
                   VkDeviceMemory               memory,
                   const VkAllocationCallbacks *pAllocator);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_VULKAN_HOOKS_ALLOC_H */
