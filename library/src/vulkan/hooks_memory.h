/*
 * vkGetPhysicalDeviceMemoryProperties / _2 / _2KHR clamp hooks (Phase 4).
 *
 * Single concern: when a Vulkan application asks the driver "how much
 * memory does this physical device have?", we report the per-pod vGPU
 * physical slice (g_vgpu_config[host_index].real_memory) instead of
 * the raw card capacity, for any heap flagged DEVICE_LOCAL_BIT.
 *
 * Why real_memory and not total_memory:
 *   - total_memory may be larger than the physical slice when the user
 *     enables CUDA oversold (UVA fallback). Vulkan has NO equivalent of
 *     cuMemAllocManaged - vkAllocateMemory always returns physical
 *     memory or fails. Reporting an oversold heap size to a Vulkan app
 *     guarantees it OOMs near the top of "available" memory.
 *   - real_memory matches what the driver itself can hand out via
 *     vkAllocateMemory; what we report and what the driver delivers
 *     stay consistent.
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
