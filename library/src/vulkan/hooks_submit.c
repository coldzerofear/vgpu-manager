/*
 * vkGetDeviceQueue[2] / vkQueueSubmit[2|2KHR] hooks.
 *
 * Submit flow:
 *   1. lookup VkQueue -> VkDevice via queue_index cache
 *   2. lookup VkDevice -> dispatch entry (gives next-layer pfn +
 *      physical_device)
 *   3. resolve physical_device -> host_index via physdev_index cache
 *   4. consume one rate-limit token via vgpu_rate_limit_by_host_index
 *      (no-op if host_index < 0 or core_limit is off — the core helper
 *      enforces those checks)
 *   5. forward to next layer
 *
 * Acquisition flow (GetDeviceQueue / _2):
 *   1. lookup dispatch entry, forward to next layer
 *   2. on success, register (VkQueue -> VkDevice) for future submits
 */
#include <stddef.h>
#include <stdint.h>

#include <vulkan/vulkan.h>

#include "include/budget.h"   /* vgpu_rate_limit_by_host_index */
#include "include/hook.h"     /* LOGGER */

#include "include/vulkan/dispatch.h"
#include "include/vulkan/physdev_index.h"
#include "include/vulkan/queue_index.h"
#include "include/vulkan/hooks_submit.h"
#include "include/vulkan/trace.h"

/* Per-submit token claim. Vulkan submits are opaque from the layer's
 * perspective (cmdbuf/dispatch counts and per-thread workload buried
 * in pipeline state we cannot cheaply introspect). The watcher thread
 * provides accurate throttling by tuning share based on NVML
 * utilization; per-call magnitude only governs fairness baseline. */
#define VGPU_VK_SUBMIT_TOKEN_COST  1

/* ------------------------------------------------------------------ */
/* vkGetDeviceQueue / vkGetDeviceQueue2                                */
/* ------------------------------------------------------------------ */

VKAPI_ATTR void VKAPI_CALL
vgpu_vk_GetDeviceQueue(VkDevice  device,
                       uint32_t  queueFamilyIndex,
                       uint32_t  queueIndex,
                       VkQueue  *pQueue) {
  vgpu_vk_device_dispatch_t *d = vgpu_vk_get_device_dispatch(device);
  if (d == NULL || d->pfn_GetDeviceQueue == NULL) {
    /* No dispatch / next-layer pfn: leave *pQueue untouched. The
     * Vulkan loader does not normally route here without a registered
     * device; log at VERBOSE for diagnostics. */
    LOGGER(VERBOSE, "vkGetDeviceQueue: dispatch unavailable for device %p",
                    (void *)device);
    return;
  }
  d->pfn_GetDeviceQueue(device, queueFamilyIndex, queueIndex, pQueue);
  if (pQueue != NULL && *pQueue != VK_NULL_HANDLE) {
    vgpu_vk_register_queue(*pQueue, device);
  }
}

VKAPI_ATTR void VKAPI_CALL
vgpu_vk_GetDeviceQueue2(VkDevice                  device,
                        const VkDeviceQueueInfo2 *pQueueInfo,
                        VkQueue                  *pQueue) {
  vgpu_vk_device_dispatch_t *d = vgpu_vk_get_device_dispatch(device);
  if (d == NULL || d->pfn_GetDeviceQueue2 == NULL) {
    LOGGER(VERBOSE, "vkGetDeviceQueue2: dispatch unavailable for device %p",
                    (void *)device);
    return;
  }
  d->pfn_GetDeviceQueue2(device, pQueueInfo, pQueue);
  if (pQueue != NULL && *pQueue != VK_NULL_HANDLE) {
    vgpu_vk_register_queue(*pQueue, device);
  }
}

/* ------------------------------------------------------------------ */
/* vkQueueSubmit / vkQueueSubmit2                                      */
/* ------------------------------------------------------------------ */

VKAPI_ATTR VkResult VKAPI_CALL
vgpu_vk_QueueSubmit(VkQueue              queue,
                    uint32_t             submitCount,
                    const VkSubmitInfo  *pSubmits,
                    VkFence              fence) {
  VkDevice dev = vgpu_vk_queue_to_device(queue);
  vgpu_vk_device_dispatch_t *d = (dev != VK_NULL_HANDLE)
                              ? vgpu_vk_get_device_dispatch(dev) : NULL;
  if (d == NULL || d->pfn_QueueSubmit == NULL) {
    /* Match hooks_alloc.c's contract: an unroutable call surfaces a
     * canonical Vulkan failure rather than crashing the app. */
    VGPU_VK_TRACE("vkQueueSubmit: queue=%p unroutable -> INIT_FAILED",
                  (void *)queue);
    return VK_ERROR_INITIALIZATION_FAILED;
  }

  int host_index = vgpu_vk_physdev_to_host_index(d->physical_device);
  VGPU_VK_TRACE("vkQueueSubmit: queue=%p host_index=%d submitCount=%u",
                (void *)queue, host_index, submitCount);
  vgpu_rate_limit_by_host_index(VGPU_VK_SUBMIT_TOKEN_COST, host_index);

  return d->pfn_QueueSubmit(queue, submitCount, pSubmits, fence);
}

#if defined(VK_VERSION_1_3)
VKAPI_ATTR VkResult VKAPI_CALL
vgpu_vk_QueueSubmit2(VkQueue              queue,
                     uint32_t             submitCount,
                     const VkSubmitInfo2 *pSubmits,
                     VkFence              fence) {
  VkDevice dev = vgpu_vk_queue_to_device(queue);
  vgpu_vk_device_dispatch_t *d = (dev != VK_NULL_HANDLE)
                              ? vgpu_vk_get_device_dispatch(dev) : NULL;
  if (d == NULL || d->pfn_QueueSubmit2 == NULL) {
    VGPU_VK_TRACE("vkQueueSubmit2: queue=%p unroutable -> INIT_FAILED",
                  (void *)queue);
    return VK_ERROR_INITIALIZATION_FAILED;
  }

  int host_index = vgpu_vk_physdev_to_host_index(d->physical_device);
  VGPU_VK_TRACE("vkQueueSubmit2: queue=%p host_index=%d submitCount=%u",
                (void *)queue, host_index, submitCount);
  vgpu_rate_limit_by_host_index(VGPU_VK_SUBMIT_TOKEN_COST, host_index);

  return d->pfn_QueueSubmit2(queue, submitCount, pSubmits, fence);
}
#endif

