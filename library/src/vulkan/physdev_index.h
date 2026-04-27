/*
 * VkPhysicalDevice -> vgpu-manager host_index resolution + cache.
 *
 * Vulkan apps refer to GPUs via VkPhysicalDevice handles obtained from
 * vkEnumeratePhysicalDevices. Our budget bookkeeping is keyed by
 * `host_index` (an index into g_vgpu_config->devices[]). To enforce
 * memory limits inside vk_layer_AllocateMemory (Phase 5+) we need a
 * fast, ABI-correct way to translate one to the other.
 *
 * Bridge: VkPhysicalDeviceIDProperties::deviceUUID, populated by the
 * NVIDIA Vulkan ICD, is exactly the same 16-byte UUID NVML reports via
 * nvmlDeviceGetUUID. get_host_device_index_by_uuid_bytes() (added in
 * Phase 0 in include/budget.h) does the final string-keyed lookup.
 *
 * Lifecycle:
 *   - vk_layer_CreateInstance calls vgpu_vk_register_instance_physdevs
 *     after the chain returns. We enumerate this instance's physical
 *     devices, query each one's UUID, resolve to host_index, cache the
 *     result. Errors are logged but not propagated - vkCreateInstance
 *     must not fail just because we could not classify a device.
 *   - vk_layer_DestroyInstance calls vgpu_vk_unregister_instance_physdevs
 *     to drop entries owned by the instance, before forwarding the
 *     destroy. Prevents stale handles being interpreted against the
 *     wrong host_index if Vulkan reuses handle values later.
 *   - vgpu_vk_physdev_to_host_index is the runtime hot path - O(N)
 *     under a read lock; populated entries make it cache-hit.
 */
#ifndef VGPU_VULKAN_PHYSDEV_INDEX_H
#define VGPU_VULKAN_PHYSDEV_INDEX_H

#include <vulkan/vulkan.h>

#include "dispatch.h"   /* VGPU_VK_INTERNAL */

#ifdef __cplusplus
extern "C" {
#endif

/* Eagerly enumerate physical devices from `instance` and cache each one's
 * (phys, owner=instance, host_index) tuple. Safe to call on Vulkan 1.0
 * instances that lack VkPhysicalDeviceProperties2 - those entries will
 * cache `host_index = -1` and not block vkCreateInstance. */
VGPU_VK_INTERNAL void
vgpu_vk_register_instance_physdevs(VkInstance                instance,
                                   PFN_vkGetInstanceProcAddr next_gipa);

/* Drop every cached entry whose owner is `instance`. Called from
 * vk_layer_DestroyInstance before the chain destroy fires. */
VGPU_VK_INTERNAL void
vgpu_vk_unregister_instance_physdevs(VkInstance instance);

/* Pure cache lookup. Returns host_index, or -1 if:
 *   - the handle was never registered (e.g. instance from before our
 *     layer was loaded - cannot happen in practice since the loader
 *     loads us before any instance is created)
 *   - the device's UUID is not in g_vgpu_config (non-NVIDIA, not
 *     allocated to this Pod, etc.)
 *   - UUID resolution failed at register time (pre-Vulkan 1.1 instance
 *     without VK_KHR_get_physical_device_properties2). */
VGPU_VK_INTERNAL int
vgpu_vk_physdev_to_host_index(VkPhysicalDevice phys);

/* Returns the VkInstance that owns `phys`, or VK_NULL_HANDLE if not
 * registered. Vulkan hooks that receive VkPhysicalDevice as input
 * (vkGetPhysicalDeviceMemoryProperties, vkEnumerateDeviceExtensionProperties,
 * etc.) need this to find the instance dispatch table for forwarding. */
VGPU_VK_INTERNAL VkInstance
vgpu_vk_physdev_owner(VkPhysicalDevice phys);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_VULKAN_PHYSDEV_INDEX_H */
