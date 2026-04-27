/*
 * Per-VkInstance / per-VkDevice dispatch table cache.
 *
 * Vulkan dispatch chain rule: a layer cannot call any vk* function
 * directly - it must go through the next layer's pfn_GetInstanceProcAddr
 * (or pfn_GetDeviceProcAddr), which the loader exposes during the
 * VkCreateInstance / VkCreateDevice handshake. We capture those pfns at
 * create time and store them keyed by the resulting VkInstance / VkDevice
 * handle so subsequent hooks can find their next-layer trampolines.
 *
 * Each pfn field may be NULL if the next layer does not provide it; the
 * corresponding hook degrades to a transparent forward / no-op rather
 * than crashing.
 */
#ifndef VGPU_VULKAN_DISPATCH_H
#define VGPU_VULKAN_DISPATCH_H

#include <vulkan/vulkan.h>

/* Module-internal linkage. The Vulkan layer must export exactly one ELF
 * symbol (vkNegotiateLoaderLayerInterfaceVersion); these dispatch
 * helpers are reachable only across files inside the layer module so
 * mark them hidden. Avoids accidentally letting an LD_PRELOAD chain or
 * a stray dlsym pull module internals out of the .so. */
#if defined(__GNUC__) && __GNUC__ >= 4
#define VGPU_VK_INTERNAL __attribute__((visibility("hidden")))
#else
#define VGPU_VK_INTERNAL
#endif

#ifdef __cplusplus
extern "C" {
#endif

typedef struct {
  VkInstance                                 instance;
  PFN_vkGetInstanceProcAddr                  pfn_GetInstanceProcAddr;
  PFN_vkDestroyInstance                      pfn_DestroyInstance;

  /* Used by hooks_memory.c to clamp device-local heap size to the
   * per-pod cap. pfn_GetPhysicalDeviceMemoryProperties is Vulkan 1.0
   * core; pfn_GetPhysicalDeviceMemoryProperties2 is Vulkan 1.1 core
   * and falls back to its KHR alias on pre-1.1 instances - the
   * populator in vk_layer_CreateInstance tries both names. */
  PFN_vkGetPhysicalDeviceMemoryProperties    pfn_GetPhysicalDeviceMemoryProperties;
  PFN_vkGetPhysicalDeviceMemoryProperties2   pfn_GetPhysicalDeviceMemoryProperties2;
} vgpu_vk_instance_dispatch_t;

typedef struct {
  VkDevice                  device;
  VkPhysicalDevice          physical_device;
  PFN_vkGetDeviceProcAddr   pfn_GetDeviceProcAddr;
  PFN_vkDestroyDevice       pfn_DestroyDevice;

  /* Memory budget enforcement (hooks_alloc.c). */
  PFN_vkAllocateMemory      pfn_AllocateMemory;
  PFN_vkFreeMemory          pfn_FreeMemory;

  /* SM rate limit on queue submission (hooks_submit.c).
   *
   * GetDeviceQueue / GetDeviceQueue2 record (VkQueue -> VkDevice) at
   * acquisition time so the submit hooks (which receive a VkQueue,
   * not a VkDevice) can resolve back to a host_index. QueueSubmit2
   * covers both the Vulkan 1.3 core entry and the
   * VK_KHR_synchronization2 alias (vkQueueSubmit2KHR) — same
   * signature, same hook function. */
  PFN_vkGetDeviceQueue      pfn_GetDeviceQueue;
  PFN_vkGetDeviceQueue2     pfn_GetDeviceQueue2;
  PFN_vkQueueSubmit         pfn_QueueSubmit;
  PFN_vkQueueSubmit2        pfn_QueueSubmit2;
} vgpu_vk_device_dispatch_t;

/* Lookup. Returns NULL if not registered. Result pointer is stable for
 * the lifetime of the corresponding VkInstance / VkDevice (until matching
 * vgpu_remove_*); callers may read fields without further locking. */
VGPU_VK_INTERNAL vgpu_vk_instance_dispatch_t *vgpu_vk_get_instance_dispatch(VkInstance instance);
VGPU_VK_INTERNAL vgpu_vk_device_dispatch_t   *vgpu_vk_get_device_dispatch  (VkDevice   device);

/* Register a snapshot of the dispatch table. Caller-owned struct is
 * copied; pointer parameter does not need to outlive the call. */
VGPU_VK_INTERNAL void vgpu_vk_register_instance_dispatch(const vgpu_vk_instance_dispatch_t *entry);
VGPU_VK_INTERNAL void vgpu_vk_register_device_dispatch  (const vgpu_vk_device_dispatch_t   *entry);

/* Remove a previously registered dispatch entry. No-op if not registered. */
VGPU_VK_INTERNAL void vgpu_vk_remove_instance_dispatch(VkInstance instance);
VGPU_VK_INTERNAL void vgpu_vk_remove_device_dispatch  (VkDevice   device);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_VULKAN_DISPATCH_H */
