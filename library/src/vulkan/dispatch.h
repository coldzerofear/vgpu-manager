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
 * Phase 2 only populates GetProcAddr + Destroy fields because Phase 2
 * only intercepts create/destroy. Phase 3 and later add fields as the
 * corresponding hooks land - struct layout is forward-compatible by
 * design (extra fields default to NULL via calloc).
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

  /* Phase 4: clamp device-local heap size to vgpu real_memory.
   * pfn_GetPhysicalDeviceMemoryProperties is required (Vulkan 1.0 core);
   * pfn_GetPhysicalDeviceMemoryProperties2 is Vulkan 1.1 core and may
   * fall back to the KHR alias on pre-1.1 instances - the populator in
   * vk_layer_CreateInstance tries both names. NULL means "next layer
   * does not provide it"; the corresponding hook returns gracefully. */
  PFN_vkGetPhysicalDeviceMemoryProperties    pfn_GetPhysicalDeviceMemoryProperties;
  PFN_vkGetPhysicalDeviceMemoryProperties2   pfn_GetPhysicalDeviceMemoryProperties2;
} vgpu_instance_dispatch_t;

typedef struct {
  VkDevice                  device;
  VkPhysicalDevice          physical_device;
  PFN_vkGetDeviceProcAddr   pfn_GetDeviceProcAddr;
  PFN_vkDestroyDevice       pfn_DestroyDevice;
  /* Phase 5+: PFN_vkAllocateMemory / FreeMemory
   * Phase 6+: PFN_vkQueueSubmit / Submit2[KHR] */
} vgpu_device_dispatch_t;

/* Lookup. Returns NULL if not registered. Result pointer is stable for
 * the lifetime of the corresponding VkInstance / VkDevice (until matching
 * vgpu_remove_*); callers may read fields without further locking. */
VGPU_VK_INTERNAL vgpu_instance_dispatch_t *vgpu_get_instance_dispatch(VkInstance instance);
VGPU_VK_INTERNAL vgpu_device_dispatch_t   *vgpu_get_device_dispatch  (VkDevice   device);

/* Register a snapshot of the dispatch table. Caller-owned struct is
 * copied; pointer parameter does not need to outlive the call. */
VGPU_VK_INTERNAL void vgpu_register_instance(const vgpu_instance_dispatch_t *entry);
VGPU_VK_INTERNAL void vgpu_register_device  (const vgpu_device_dispatch_t   *entry);

/* Remove a previously registered dispatch entry. No-op if not registered. */
VGPU_VK_INTERNAL void vgpu_remove_instance(VkInstance instance);
VGPU_VK_INTERNAL void vgpu_remove_device  (VkDevice   device);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_VULKAN_DISPATCH_H */
