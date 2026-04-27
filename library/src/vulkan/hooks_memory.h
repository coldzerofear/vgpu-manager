/*
 * vkGetPhysicalDeviceMemoryProperties / _2 / _2KHR clamp hooks (Phase 4).
 *
 * Single concern: when a Vulkan application asks the driver "how much
 * memory does this physical device have?", we report the per-pod vGPU
 * cap instead of the raw card capacity, for any heap flagged
 * DEVICE_LOCAL_BIT.
 *
 * Cap selection mirrors the cuMemGetInfo hook so the two paths read in
 * the same shape:
 *   - oversold ON  -> real_memory  (Vulkan has no UVA equivalent of
 *                                    cuMemAllocManaged, so the
 *                                    UVA-inclusive total_memory is
 *                                    unreachable from Vulkan)
 *   - oversold OFF -> total_memory (== real_memory by config invariant)
 * See clamp_cap_for_phys in hooks_memory.c for the full rationale.
 *
 * These hooks do NOT touch budget enforcement, lock_gpu_device, NVML
 * `used` view, or any cross-process state. Phase 5 introduces those
 * paths via vk_layer_AllocateMemory. Phase 4 is a pure read + clamp,
 * cheap enough that per-call cost is negligible.
 */
#ifndef VGPU_VULKAN_HOOKS_MEMORY_H
#define VGPU_VULKAN_HOOKS_MEMORY_H

#include <vulkan/vulkan.h>

#include "dispatch.h"   /* VGPU_VK_INTERNAL */

#ifdef __cplusplus
extern "C" {
#endif

VGPU_VK_INTERNAL VKAPI_ATTR void VKAPI_CALL
vgpu_vk_GetPhysicalDeviceMemoryProperties(
    VkPhysicalDevice                  physicalDevice,
    VkPhysicalDeviceMemoryProperties *pMemoryProperties);

/* Used for both vkGetPhysicalDeviceMemoryProperties2 (Vulkan 1.1 core)
 * and vkGetPhysicalDeviceMemoryProperties2KHR (the original extension).
 * The two have identical signatures and identical semantics. */
VGPU_VK_INTERNAL VKAPI_ATTR void VKAPI_CALL
vgpu_vk_GetPhysicalDeviceMemoryProperties2(
    VkPhysicalDevice                   physicalDevice,
    VkPhysicalDeviceMemoryProperties2 *pMemoryProperties);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_VULKAN_HOOKS_MEMORY_H */
