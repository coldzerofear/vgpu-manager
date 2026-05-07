/*
 * vkGetPhysicalDeviceMemoryProperties / _2 / _2KHR clamp hooks.
 *
 * For every device-local heap (VK_MEMORY_HEAP_DEVICE_LOCAL_BIT) reported
 * by the next layer, replace heap.size with min(heap.size, cap), where
 * cap is selected by memory_oversold:
 *   - oversold ON  : cap = real_memory
 *   - oversold OFF : cap = total_memory  (== real_memory by config invariant)
 *
 * On the _2 entry, also walks the pNext chain for
 * VkPhysicalDeviceMemoryBudgetPropertiesEXT (VK_EXT_memory_budget) and
 * inflates heapUsage[] so engines that compute "available = heapBudget
 * - heapUsage" see the partition limit instead of the host GPU's free
 * memory. See clamp_budget_pnext below for why we cannot simply clamp
 * heapBudget directly.
 *
 * Non-clamp paths (forward only):
 *   - host_index < 0  (non-NVIDIA device or not allocated to this Pod)
 *   - memory_limit  == 0 in g_vgpu_config (no per-pod cap configured)
 *   - heap is host-visible / staging / non-device-local
 *
 * See clamp_cap_for_phys below for the rationale of the oversold branch
 * and how it mirrors cuMemGetInfo's structure.
 */
#include <stddef.h>
#include <stdint.h>

#include <vulkan/vulkan.h>

#include "include/hook.h"   /* g_vgpu_config, resource_data_t */

#include "include/vulkan/dispatch.h"
#include "include/vulkan/physdev_index.h"
#include "include/vulkan/hooks_memory.h"
#include "include/vulkan/trace.h"

extern resource_data_t *g_vgpu_config;

/* Decide the clamp cap for this physical device, in bytes. Returns 0
 * to indicate "do not clamp" (no host_index, no limit configured, or
 * g_vgpu_config not loaded).
 *
 * Branching on memory_oversold mirrors the cuMemGetInfo path's shape
 * for code-review symmetry:
 *   - oversold ON : total_memory is the configured UVA-inclusive size
 *                   (may be larger than the physical slice). Vulkan
 *                   has no UVA equivalent, so it must see only
 *                   real_memory (the physical-allocatable amount).
 *   - oversold OFF: g_vgpu_config invariant guarantees
 *                   total_memory == real_memory (no over-provisioning),
 *                   so either field gives the same cap. We pick
 *                   total_memory to keep the structural mirror of
 *                   cuMemGetInfo's non-oversold branch
 *                   (`actual_total = total_memory`).
 *
 * Behaviour is bit-identical to "always use real_memory" as long as
 * the invariant holds; the explicit branch is purely stylistic. */
static size_t clamp_cap_for_phys(VkPhysicalDevice phys) {
  if (g_vgpu_config == NULL) return 0;

  int host_index = vgpu_vk_physdev_to_host_index(phys);
  if (host_index < 0) return 0;
  if (!g_vgpu_config->devices[host_index].memory_limit) return 0;

  if (g_vgpu_config->devices[host_index].memory_oversold) {
    return g_vgpu_config->devices[host_index].real_memory;
  }
  return g_vgpu_config->devices[host_index].total_memory;
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
      VGPU_VK_TRACE("heap clamp: heap[%u] %llu -> %zu (DEVICE_LOCAL)",
                    i, (unsigned long long)props->memoryHeaps[i].size, cap);
      props->memoryHeaps[i].size = (VkDeviceSize)cap;
    }
  }
}

/* Reflect the partition limit through VK_EXT_memory_budget's
 * heapBudget / heapUsage pair when the caller queried it via the
 * pNext chain on _2 / _2KHR.
 *
 * Why this is "inflate heapUsage" rather than the obvious "clamp
 * heapBudget":
 *
 *   Carbonite / Omniverse / Isaac Sim Kit consume heapBudget through
 *   paths beyond the simple "available = heapBudget - heapUsage"
 *   subtraction that drives their memory-pressure overlay. HAMi-core
 *   PR #182 commit 58d304f documents that any clamp of heapBudget
 *   below ICD's reported value (whether to partition_limit, to
 *   partition_limit-heapUsage, or to any value below heap.size)
 *   deadlocks omni.physx.tensors during its plugin init on Isaac Sim
 *   Streaming 6.0 + NVIDIA driver 580.142. PhysX is unaffected by
 *   heapUsage changes.
 *
 *   So the workaround is: leave heapBudget at the ICD-reported value
 *   (host GPU's free memory) and inflate heapUsage by
 *   (icd_budget - cap). Engines compute "available" as
 *   heapBudget - heapUsage = icd_budget - (icd_usage + delta) =
 *   cap - icd_usage, exactly matching the partition. PhysX's other
 *   consumers of heapBudget see the unchanged ICD value and stay
 *   happy.
 *
 *   We deliberately do NOT alter heapBudget here. If a future
 *   regression observation contradicts the deadlock evidence, this
 *   function is the single place to revisit.
 *
 * `heaps` and `heap_count` come from the same _2 query whose pNext
 * we are walking; the index space matches one-to-one with
 * heapBudget[] / heapUsage[]. */
static void clamp_budget_pnext(VkPhysicalDevice phys,
                               const VkMemoryHeap *heaps,
                               uint32_t heap_count,
                               void *pnext_chain) {
  if (pnext_chain == NULL) return;
  size_t cap = clamp_cap_for_phys(phys);
  if (cap == 0) return;
  VkDeviceSize cap_vk = (VkDeviceSize)cap;

  VkBaseOutStructure *cur = (VkBaseOutStructure *)pnext_chain;
  while (cur != NULL) {
    if (cur->sType == VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_MEMORY_BUDGET_PROPERTIES_EXT) {
      VkPhysicalDeviceMemoryBudgetPropertiesEXT *bud =
          (VkPhysicalDeviceMemoryBudgetPropertiesEXT *)cur;
      uint32_t cnt = heap_count;
      if (cnt > VK_MAX_MEMORY_HEAPS) cnt = VK_MAX_MEMORY_HEAPS;
      for (uint32_t i = 0; i < cnt; i++) {
        if ((heaps[i].flags & VK_MEMORY_HEAP_DEVICE_LOCAL_BIT) == 0) {
          continue;
        }
        VkDeviceSize icd_budget = bud->heapBudget[i];
        VkDeviceSize icd_usage  = bud->heapUsage[i];
        if (icd_budget <= cap_vk) {
          /* ICD already reports a budget within partition; nothing to
           * inflate. Common when partition_limit happens to exceed
           * current free memory. */
          continue;
        }
        VkDeviceSize delta     = icd_budget - cap_vk;
        VkDeviceSize new_usage = icd_usage + delta;
        /* Defend against the (uint64) overflow wraparound the addition
         * could cause if the ICD ever reported a pathological value.
         * Same shape HAMi uses; cheap belt-and-suspenders. */
        if (new_usage > icd_usage) {
          VGPU_VK_TRACE("budget pnext: heap[%u] usage %llu -> %llu "
                        "(cap=%zu icd_budget=%llu)",
                        i,
                        (unsigned long long)icd_usage,
                        (unsigned long long)new_usage,
                        cap,
                        (unsigned long long)icd_budget);
          bud->heapUsage[i] = new_usage;
        }
      }
      /* VK_EXT_memory_budget appears at most once per pNext chain
       * per spec; stop walking. */
      break;
    }
    cur = (VkBaseOutStructure *)cur->pNext;
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

  vgpu_vk_instance_dispatch_t *d = vgpu_vk_get_instance_dispatch(owner);
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

  vgpu_vk_instance_dispatch_t *d = vgpu_vk_get_instance_dispatch(owner);
  if (d == NULL || d->pfn_GetPhysicalDeviceMemoryProperties2 == NULL) {
    return;
  }

  d->pfn_GetPhysicalDeviceMemoryProperties2(physicalDevice, pMemoryProperties);

  /* Two-step clamp on _2 / _2KHR: first the embedded V1 heap.size
   * array, then any VK_EXT_memory_budget extension struct in the
   * pNext chain. Order matters only weakly — clamp_budget_pnext reads
   * the (post-clamp) heap.size array's flags but not its size, and
   * uses ICD-original heapBudget / heapUsage values regardless.
   * Doing heap.size first keeps the two transformations independent
   * of any future change. */
  if (pMemoryProperties != NULL) {
    clamp_device_local_heaps(&pMemoryProperties->memoryProperties,
                             clamp_cap_for_phys(physicalDevice));
    clamp_budget_pnext(physicalDevice,
                       pMemoryProperties->memoryProperties.memoryHeaps,
                       pMemoryProperties->memoryProperties.memoryHeapCount,
                       pMemoryProperties->pNext);
  }
}
