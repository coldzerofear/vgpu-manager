/*
 * VkQueue -> VkDevice cache.
 *
 * vkQueueSubmit / vkQueueSubmit2[KHR] take a VkQueue, but the rate
 * limit is keyed by host_index — which we resolve via the queue's
 * VkDevice -> dispatch entry -> physical_device -> host_index chain.
 * Vulkan exposes no API for "which VkDevice owns this VkQueue" from
 * inside a layer, so we record the mapping at vkGetDeviceQueue /
 * vkGetDeviceQueue2 time and look it up at submit time. Same
 * architectural pattern as physdev_index for VkPhysicalDevice ->
 * host_index.
 *
 * Lifetime contract:
 *   - register at vkGetDeviceQueue / vkGetDeviceQueue2 hook (every
 *     queue acquisition, idempotent if the same handle is requested
 *     twice — Vulkan permits that)
 *   - bulk-unregister at vk_layer_DestroyDevice, before forwarding
 *     the destroy to the next layer (mirrors physdev_index's
 *     register/unregister pairing on instance lifecycle)
 *
 * Storage / locking shape matches dispatch.c: linked list under a
 * pthread_rwlock_t, since reads (per submit) vastly outnumber writes
 * (queue acquisition is rare).
 */
#ifndef VGPU_VULKAN_QUEUE_INDEX_H
#define VGPU_VULKAN_QUEUE_INDEX_H

#include <vulkan/vulkan.h>

#include "dispatch.h"   /* VGPU_VK_INTERNAL */

#ifdef __cplusplus
extern "C" {
#endif

/* Record (queue, device). Duplicate registrations of the same queue
 * are a no-op (Vulkan permits an app to re-query the same queue). */
VGPU_VK_INTERNAL void
vgpu_vk_register_queue(VkQueue queue, VkDevice device);

/* Lookup. Returns VK_NULL_HANDLE if the queue was never registered
 * (e.g., the app obtained it through a path we did not hook, or the
 * registration races with a concurrent submit — both are tolerated by
 * the submit hook's NULL check). */
VGPU_VK_INTERNAL VkDevice
vgpu_vk_queue_to_device(VkQueue queue);

/* Drop every (queue, *) entry whose device == `device`. Called by
 * vk_layer_DestroyDevice before the destroy fires, so the cache is
 * coherent with the dispatch table at all times. */
VGPU_VK_INTERNAL void
vgpu_vk_unregister_queues_for_device(VkDevice device);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_VULKAN_QUEUE_INDEX_H */
