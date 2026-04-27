/*
 * vkQueueSubmit / vkQueueSubmit2[KHR] hooks.
 *
 * Plus VkQueue acquisition hooks (vkGetDeviceQueue / _2) — these do
 * not enforce anything on their own but record the (VkQueue ->
 * VkDevice) mapping that the submit hooks need. See queue_index.h.
 *
 * Throttle policy:
 *   - claim 1 token per submit (HAMi PR #182 parity; layer cannot
 *     estimate command-buffer compute volume cheaply, and the watcher
 *     thread tunes the share dynamically by NVML-observed utilization)
 *   - skip when host_index resolution fails or core_limit is off (the
 *     vgpu_rate_limit_by_host_index core enforces both checks)
 *   - SM watcher thread MUST already be running; vk_layer_CreateInstance
 *     calls vgpu_ensure_sm_watcher_started() at the success path to
 *     satisfy this precondition before any device / queue exists
 *
 * Failure modes outside our control:
 *   - dispatch entry missing -> return VK_ERROR_INITIALIZATION_FAILED
 *     (matches the equivalent path in hooks_alloc.c)
 *   - unregistered queue -> forward without throttle (degraded but
 *     not blocking; logged at VERBOSE)
 */
#ifndef VGPU_VULKAN_HOOKS_SUBMIT_H
#define VGPU_VULKAN_HOOKS_SUBMIT_H

#include <vulkan/vulkan.h>

#include "dispatch.h"   /* VGPU_VK_INTERNAL */

#ifdef __cplusplus
extern "C" {
#endif

VGPU_VK_INTERNAL VKAPI_ATTR void VKAPI_CALL
vgpu_vk_GetDeviceQueue(VkDevice  device,
                       uint32_t  queueFamilyIndex,
                       uint32_t  queueIndex,
                       VkQueue  *pQueue);

VGPU_VK_INTERNAL VKAPI_ATTR void VKAPI_CALL
vgpu_vk_GetDeviceQueue2(VkDevice                  device,
                        const VkDeviceQueueInfo2 *pQueueInfo,
                        VkQueue                  *pQueue);

VGPU_VK_INTERNAL VKAPI_ATTR VkResult VKAPI_CALL
vgpu_vk_QueueSubmit(VkQueue              queue,
                    uint32_t             submitCount,
                    const VkSubmitInfo  *pSubmits,
                    VkFence              fence);

/* Used for both vkQueueSubmit2 (Vulkan 1.3 core) and
 * vkQueueSubmit2KHR (VK_KHR_synchronization2 alias). Identical
 * signatures, identical semantics. */
VGPU_VK_INTERNAL VKAPI_ATTR VkResult VKAPI_CALL
vgpu_vk_QueueSubmit2(VkQueue              queue,
                     uint32_t             submitCount,
                     const VkSubmitInfo2 *pSubmits,
                     VkFence              fence);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_VULKAN_HOOKS_SUBMIT_H */
