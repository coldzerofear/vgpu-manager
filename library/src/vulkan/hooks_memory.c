/*
 * vkGetPhysicalDeviceMemoryProperties / _2 / _2KHR clamp hooks.
 *
 * For every device-local heap (VK_MEMORY_HEAP_DEVICE_LOCAL_BIT) reported
 * by the next layer, replace heap.size with min(heap.size, real_memory)
 * where real_memory is g_vgpu_config[host_index].real_memory.
 *
 * Non-clamp paths (forward only):
 *   - host_index < 0  (non-NVIDIA device or not allocated to this Pod)
 *   - memory_limit  == 0 in g_vgpu_config (no per-pod cap configured)
 *   - heap is host-visible / staging / non-device-local
 *
 * See hooks_memory.h header for the design rationale, especially the
 * deliberate divergence from cuMemGetInfo's oversold-aware clamp:
 * Vulkan has no UVA equivalent, so the only useful clamp here is the
 * physical-slice cap, regardless of memory_oversold flag.
 */
#include <stddef.h>
#include <stdint.h>

#include <vulkan/vulkan.h>

#include "include/hook.h"   /* g_vgpu_config, resource_data_t */

#include "dispatch.h"
#include "physdev_index.h"
#include "hooks_memory.h"

extern resource_data_t *g_vgpu_config;

/* Decide the clamp cap for this physical device, in bytes.
 * Returns 0 to indicate "do not clamp" (no host_index, no limit
 * configured, or g_vgpu_config not loaded). */
static size_t clamp_cap_for_phys(VkPhysicalDevice phys) {
  if (g_vgpu_config == NULL) return 0;

  int host_index = vgpu_vk_physdev_to_host_index(phys);
  if (host_index < 0) return 0;
  if (!g_vgpu_config->devices[host_index].memory_limit) return 0;

  return g_vgpu_config->devices[host_index].real_memory;
}

/* Apply the cap to every device-local heap in `props`. Caller has
 * already populated `props` from the next layer. */
static void clamp_device_local_heaps(VkPhysicalDeviceMemoryProperties *props,
                                     size_t cap) {
  if (cap == 0 || props == NULL) return;
  for (uint32_t i = 0; i < props->memoryHeapCount; i++) {
    if ((props->memoryHeaps[i].flags & VK_MEMORY_HEAP_DEVICE_LOCAL_BIT) == 0) {
      continue;
    }
    if (props->memoryHeaps[i].size > cap) {
      props->memoryHeaps[i].size = (VkDeviceSize)cap;
    }
  }
}

/* ------- Vulkan 1.0 entry ------- */

VKAPI_ATTR void VKAPI_CALL
vgpu_vk_GetPhysicalDeviceMemoryProperties(
    VkPhysicalDevice                  physicalDevice,
    VkPhysicalDeviceMemoryProperties *pMemoryProperties) {
  /* Find the dispatch table of the instance that owns `physicalDevice`.
   * The Vulkan loader does not deliver an unknown phys to a layer hook
   * in normal operation, so a NULL owner / dispatch indicates either a
   * caller that obtained the handle outside the loader-tracked path
   * (uncommon) or our own ordering bug. Either way, returning early
   * without touching the output is the safe choice - the caller would
   * see an uninitialised struct, which Vulkan validation layers will
   * flag. We never want to fabricate properties out of thin air. */
  VkInstance owner = vgpu_vk_physdev_owner(physicalDevice);
  if (owner == VK_NULL_HANDLE) return;

  vgpu_instance_dispatch_t *d = vgpu_get_instance_dispatch(owner);
  if (d == NULL || d->pfn_GetPhysicalDeviceMemoryProperties == NULL) {
    return;
  }

  /* Forward first. */
  d->pfn_GetPhysicalDeviceMemoryProperties(physicalDevice, pMemoryProperties);

  /* Then clamp. */
  clamp_device_local_heaps(pMemoryProperties, clamp_cap_for_phys(physicalDevice));
}

/* ------- Vulkan 1.1 / KHR entry ------- */

VKAPI_ATTR void VKAPI_CALL
vgpu_vk_GetPhysicalDeviceMemoryProperties2(
    VkPhysicalDevice                   physicalDevice,
    VkPhysicalDeviceMemoryProperties2 *pMemoryProperties) {
  VkInstance owner = vgpu_vk_physdev_owner(physicalDevice);
  if (owner == VK_NULL_HANDLE) return;

  vgpu_instance_dispatch_t *d = vgpu_get_instance_dispatch(owner);
  if (d == NULL || d->pfn_GetPhysicalDeviceMemoryProperties2 == NULL) {
    return;
  }

  d->pfn_GetPhysicalDeviceMemoryProperties2(physicalDevice, pMemoryProperties);

  /* The Vulkan 1.1 _2 struct embeds the V1 properties in
   * `memoryProperties`. The pNext chain is left untouched - we have
   * no opinion on extension structures (e.g. VkPhysicalDeviceMemoryBudgetPropertiesEXT)
   * yet; if a future requirement needs us to clamp those too, that is
   * a follow-up. */
  if (pMemoryProperties != NULL) {
    clamp_device_local_heaps(&pMemoryProperties->memoryProperties,
                              clamp_cap_for_phys(physicalDevice));
  }
}
