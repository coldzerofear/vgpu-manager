/*
 * vgpu-manager Vulkan implicit layer - core wiring.
 *
 * Implements the loader<->layer negotiation protocol, the dispatch
 * chain wiring at vkCreateInstance / vkCreateDevice time, the
 * per-layer GetInstance/DeviceProcAddr lookups, and the bootstrap of
 * vgpu-manager's runtime state (config + NVML + SM watcher) on first
 * Vulkan instance creation. The actual hook bodies live in
 * hooks_memory.c (heap clamp), hooks_alloc.c (memory budget) and
 * hooks_submit.c (SM rate limit).
 *
 * Symbol export discipline (see layer.h for details):
 *   - the only ELF-exported symbol is vkNegotiateLoaderLayerInterfaceVersion
 *   - every other entry point in this file is static
 *   - the layer's GetInstance/DeviceProcAddr return our static pointers
 *     when asked, otherwise forward to the next layer's GIPA/GDPA
 *   - this prevents accidental ELF-resolution hijacking when the .so
 *     is also LD_PRELOADed for CUDA hooking
 */
#include <stdlib.h>
#include <string.h>

#include <vulkan/vulkan.h>
#include <vulkan/vk_layer.h>

#include "include/hook.h"     /* load_necessary_data, init_devices_mapping */
#include "include/budget.h"   /* vgpu_ensure_sm_watcher_started */

#include "include/vulkan/layer.h"
#include "include/vulkan/dispatch.h"
#include "include/vulkan/physdev_index.h"
#include "include/vulkan/queue_index.h"
#include "include/vulkan/hooks_memory.h"
#include "include/vulkan/hooks_alloc.h"
#include "include/vulkan/hooks_submit.h"

/* Forward declarations of the static hooks - referenced by the GetProcAddr
 * lookups before their bodies appear below. */
static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL
vk_layer_GetInstanceProcAddr(VkInstance instance, const char *pName);

static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL
vk_layer_GetDeviceProcAddr(VkDevice device, const char *pName);

/* ------------------------------------------------------------------ */
/* Chain helpers                                                       */
/* ------------------------------------------------------------------ */

/* Walk pCreateInfo->pNext for the VkLayerInstanceCreateInfo whose
 * `function` field matches `func` (typically VK_LAYER_LINK_INFO during
 * vkCreateInstance). Returns NULL if not found - which means the loader
 * did not provide a chain link, and we cannot construct the layer chain. */
static const VkLayerInstanceCreateInfo *
find_instance_chain_info(const VkInstanceCreateInfo *pCreateInfo,
                         VkLayerFunction func) {
  const VkBaseInStructure *p = (const VkBaseInStructure *)pCreateInfo->pNext;
  while (p != NULL) {
    if (p->sType == VK_STRUCTURE_TYPE_LOADER_INSTANCE_CREATE_INFO) {
      const VkLayerInstanceCreateInfo *li = (const VkLayerInstanceCreateInfo *)p;
      if (li->function == func) {
        return li;
      }
    }
    p = p->pNext;
  }
  return NULL;
}

static const VkLayerDeviceCreateInfo *
find_device_chain_info(const VkDeviceCreateInfo *pCreateInfo,
                       VkLayerFunction func) {
  const VkBaseInStructure *p = (const VkBaseInStructure *)pCreateInfo->pNext;
  while (p != NULL) {
    if (p->sType == VK_STRUCTURE_TYPE_LOADER_DEVICE_CREATE_INFO) {
      const VkLayerDeviceCreateInfo *li = (const VkLayerDeviceCreateInfo *)p;
      if (li->function == func) {
        return li;
      }
    }
    p = p->pNext;
  }
  return NULL;
}

/* ------------------------------------------------------------------ */
/* Instance create / destroy                                           */
/* ------------------------------------------------------------------ */

static VKAPI_ATTR VkResult VKAPI_CALL
vk_layer_CreateInstance(const VkInstanceCreateInfo  *pCreateInfo,
                        const VkAllocationCallbacks *pAllocator,
                        VkInstance                  *pInstance) {
  /* Ensure vgpu-manager's global state is loaded before we touch any of
   * it. For pure-Vulkan applications (no CUDA, no NVML calls) nothing
   * else triggers initialisation - the existing CUDA / NVML hook entry
   * points are dormant. Without this, vgpu_vk_register_instance_physdevs
   * below would dereference a NULL g_vgpu_config and crash.
   *
   * load_necessary_data is pthread_once-guarded internally so this is
   * a no-op after the first call regardless of which API surface
   * (CUDA, Vulkan, future) reaches it first. */
  load_necessary_data();

  const VkLayerInstanceCreateInfo *chain_info =
      find_instance_chain_info(pCreateInfo, VK_LAYER_LINK_INFO);
  if (chain_info == NULL || chain_info->u.pLayerInfo == NULL) {
    return VK_ERROR_INITIALIZATION_FAILED;
  }

  PFN_vkGetInstanceProcAddr next_gipa =
      chain_info->u.pLayerInfo->pfnNextGetInstanceProcAddr;
  if (next_gipa == NULL) {
    return VK_ERROR_INITIALIZATION_FAILED;
  }

  PFN_vkCreateInstance next_create =
      (PFN_vkCreateInstance)next_gipa(VK_NULL_HANDLE, "vkCreateInstance");
  if (next_create == NULL) {
    return VK_ERROR_INITIALIZATION_FAILED;
  }

  /* Advance the chain so the next layer sees only the rest of it. The
   * loader passes the same VkLayerInstanceCreateInfo down the chain;
   * each layer is expected to bump pLayerInfo before forwarding. The
   * cast away from const is sanctioned by the Vulkan loader spec. */
  ((VkLayerInstanceCreateInfo *)chain_info)->u.pLayerInfo =
      chain_info->u.pLayerInfo->pNext;

  VkResult result = next_create(pCreateInfo, pAllocator, pInstance);
  if (result != VK_SUCCESS) {
    return result;
  }

  /* Build our dispatch table snapshot for this instance. We capture the
   * next layer's GIPA so subsequent hooks can route forwarding calls,
   * and DestroyInstance so vk_layer_DestroyInstance can clean up. Other
   * pfn_ fields stay NULL until later phases register more hooks. */
  vgpu_vk_instance_dispatch_t entry;
  memset(&entry, 0, sizeof(entry));
  entry.instance                = *pInstance;
  entry.pfn_GetInstanceProcAddr = next_gipa;
  entry.pfn_DestroyInstance     =
      (PFN_vkDestroyInstance)next_gipa(*pInstance, "vkDestroyInstance");

  /* Heap-clamp pfns (hooks_memory.c). _2 falls back to _2KHR for
   * pre-1.1 instances that loaded VK_KHR_get_physical_device_properties2
   * - same fallback shape physdev_index uses for the UUID resolver. */
  entry.pfn_GetPhysicalDeviceMemoryProperties =
      (PFN_vkGetPhysicalDeviceMemoryProperties)
      next_gipa(*pInstance, "vkGetPhysicalDeviceMemoryProperties");
  entry.pfn_GetPhysicalDeviceMemoryProperties2 =
      (PFN_vkGetPhysicalDeviceMemoryProperties2)
      next_gipa(*pInstance, "vkGetPhysicalDeviceMemoryProperties2");
  if (entry.pfn_GetPhysicalDeviceMemoryProperties2 == NULL) {
    entry.pfn_GetPhysicalDeviceMemoryProperties2 =
        (PFN_vkGetPhysicalDeviceMemoryProperties2)
        next_gipa(*pInstance, "vkGetPhysicalDeviceMemoryProperties2KHR");
  }

  vgpu_vk_register_instance_dispatch(&entry);

  /* Eagerly populate the VkPhysicalDevice -> host_index cache for every
   * physdev this instance can see. Failures here are logged but never
   * propagate - vkCreateInstance must not fail just because we could
   * not classify a device. The lookup at hook time becomes a pure cache
   * read with no Vulkan loader round-trip. */
  vgpu_vk_register_instance_physdevs(*pInstance, next_gipa);

  /* Front-load NVML readiness before any vkAllocateMemory can arrive.
   * load_necessary_data() above only dlsym'd the NVML symbols; the
   * actual nvmlInit_v2 call lives inside init_nvml_to_host_device_index,
   * which init_devices_mapping invokes via pthread_once. Without this
   * the first vgpu_check_alloc_budget would resolve a nvmlDevice_t
   * against an uninitialised NVML library and fall through to "no
   * enforcement" — silently bypassing the budget on the first alloc.
   *
   * Deliberately deferred until after next_create succeeded: if the
   * NVIDIA ICD failed to come up (no GPU, broken driver), the next
   * layer's vkCreateInstance has already returned an error and we will
   * not reach here. Calling init_devices_mapping in that state would
   * trip the LOGGER(FATAL,...) inside init_nvml_to_host_device_index.
   *
   * pthread_once-guarded internally, so multiple instances or a CUDA
   * path that already triggered it are no-ops. */
  init_devices_mapping();

  /* Front-load SM rate-limiter readiness. cuda_hook.c's initialization()
   * runs cuInit, populates g_total_cuda_cores, and starts the
   * utilization_watcher thread that replenishes the per-device token
   * bucket. Without this, the first vkQueueSubmit would push
   * g_cur_cuda_cores below zero and stall in nanosleep forever (the
   * watcher would never run to refill it).
   *
   * pthread_once-guarded — a CUDA path that already triggered it is
   * a no-op; multiple Vulkan instances are also fine. */
  vgpu_ensure_sm_watcher_started();

  return VK_SUCCESS;
}

static VKAPI_ATTR void VKAPI_CALL
vk_layer_DestroyInstance(VkInstance instance,
                         const VkAllocationCallbacks *pAllocator) {
  vgpu_vk_instance_dispatch_t *d = vgpu_vk_get_instance_dispatch(instance);
  PFN_vkDestroyInstance next  = (d != NULL) ? d->pfn_DestroyInstance : NULL;
  /* Drop physdev cache entries first, then dispatch entry, before
   * forwarding the destroy. Order matters in case a racing concurrent
   * lookup arrives between our removals and the chain destroy: it
   * either sees a fully-registered instance (and gets a valid forward)
   * or no entry (and bails out). It never sees a stale phys -> host
   * mapping for an instance whose dispatch table is already gone. */
  vgpu_vk_unregister_instance_physdevs(instance);
  vgpu_vk_remove_instance_dispatch(instance);
  if (next != NULL) {
    next(instance, pAllocator);
  }
}

/* ------------------------------------------------------------------ */
/* Device create / destroy                                             */
/* ------------------------------------------------------------------ */

static VKAPI_ATTR VkResult VKAPI_CALL
vk_layer_CreateDevice(VkPhysicalDevice              physicalDevice,
                      const VkDeviceCreateInfo     *pCreateInfo,
                      const VkAllocationCallbacks  *pAllocator,
                      VkDevice                     *pDevice) {
  const VkLayerDeviceCreateInfo *chain_info =
      find_device_chain_info(pCreateInfo, VK_LAYER_LINK_INFO);
  if (chain_info == NULL || chain_info->u.pLayerInfo == NULL) {
    return VK_ERROR_INITIALIZATION_FAILED;
  }

  PFN_vkGetInstanceProcAddr next_gipa =
      chain_info->u.pLayerInfo->pfnNextGetInstanceProcAddr;
  PFN_vkGetDeviceProcAddr next_gdpa =
      chain_info->u.pLayerInfo->pfnNextGetDeviceProcAddr;
  if (next_gipa == NULL || next_gdpa == NULL) {
    return VK_ERROR_INITIALIZATION_FAILED;
  }

  /* The Vulkan loader spec allows querying vkCreateDevice from any
   * non-NULL layer GIPA via NULL instance. */
  PFN_vkCreateDevice next_create =
      (PFN_vkCreateDevice)next_gipa(VK_NULL_HANDLE, "vkCreateDevice");
  if (next_create == NULL) {
    return VK_ERROR_INITIALIZATION_FAILED;
  }

  /* Advance the chain. */
  ((VkLayerDeviceCreateInfo *)chain_info)->u.pLayerInfo =
      chain_info->u.pLayerInfo->pNext;

  VkResult result = next_create(physicalDevice, pCreateInfo, pAllocator, pDevice);
  if (result != VK_SUCCESS) {
    return result;
  }

  vgpu_vk_device_dispatch_t entry;
  memset(&entry, 0, sizeof(entry));
  entry.device                = *pDevice;
  entry.physical_device       = physicalDevice;
  entry.pfn_GetDeviceProcAddr = next_gdpa;
  entry.pfn_DestroyDevice     =
      (PFN_vkDestroyDevice)next_gdpa(*pDevice, "vkDestroyDevice");

  /* Memory budget pfns (hooks_alloc.c). NULL is tolerated by the
   * corresponding hooks (they degrade to a defensive error / no-op
   * rather than crashing). */
  entry.pfn_AllocateMemory =
      (PFN_vkAllocateMemory)next_gdpa(*pDevice, "vkAllocateMemory");
  entry.pfn_FreeMemory =
      (PFN_vkFreeMemory)next_gdpa(*pDevice, "vkFreeMemory");

  /* Queue acquisition + submit pfns (hooks_submit.c). _2 falls back to
   * the KHR synchronization2 alias when the 1.3 core entry is
   * unavailable — same fallback pattern used elsewhere for
   * promoted-extension APIs. */
  entry.pfn_GetDeviceQueue =
      (PFN_vkGetDeviceQueue)next_gdpa(*pDevice, "vkGetDeviceQueue");
  entry.pfn_GetDeviceQueue2 =
      (PFN_vkGetDeviceQueue2)next_gdpa(*pDevice, "vkGetDeviceQueue2");
  entry.pfn_QueueSubmit =
      (PFN_vkQueueSubmit)next_gdpa(*pDevice, "vkQueueSubmit");
  entry.pfn_QueueSubmit2 =
      (PFN_vkQueueSubmit2)next_gdpa(*pDevice, "vkQueueSubmit2");
  if (entry.pfn_QueueSubmit2 == NULL) {
    entry.pfn_QueueSubmit2 =
        (PFN_vkQueueSubmit2)next_gdpa(*pDevice, "vkQueueSubmit2KHR");
  }

  vgpu_vk_register_device_dispatch(&entry);

  return VK_SUCCESS;
}

static VKAPI_ATTR void VKAPI_CALL
vk_layer_DestroyDevice(VkDevice device,
                       const VkAllocationCallbacks *pAllocator) {
  vgpu_vk_device_dispatch_t *d = vgpu_vk_get_device_dispatch(device);
  PFN_vkDestroyDevice next  = (d != NULL) ? d->pfn_DestroyDevice : NULL;
  /* Drop queue cache entries first, then dispatch entry, before
   * forwarding the destroy. Same ordering rationale as
   * vk_layer_DestroyInstance: a racing concurrent submit either sees
   * a fully-registered device (and gets a valid forward) or no entry
   * (and bails out via VK_ERROR_INITIALIZATION_FAILED). It never
   * sees a stale queue -> device mapping for a device whose dispatch
   * table is already gone. */
  vgpu_vk_unregister_queues_for_device(device);
  vgpu_vk_remove_device_dispatch(device);
  if (next != NULL) {
    next(device, pAllocator);
  }
}

/* ------------------------------------------------------------------ */
/* GetInstance/DeviceProcAddr                                          */
/* ------------------------------------------------------------------ */

static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL
vk_layer_GetInstanceProcAddr(VkInstance instance, const char *pName) {
  if (pName == NULL) return NULL;

  /* Functions our layer hooks at instance scope. Returning our static
   * pointers here is what makes the dispatch chain re-enter us on
   * subsequent calls; without these the loader resolves directly to
   * the next layer. */
  if (strcmp(pName, "vkGetInstanceProcAddr") == 0) {
    return (PFN_vkVoidFunction)vk_layer_GetInstanceProcAddr;
  }
  if (strcmp(pName, "vkCreateInstance") == 0) {
    return (PFN_vkVoidFunction)vk_layer_CreateInstance;
  }
  if (strcmp(pName, "vkDestroyInstance") == 0) {
    return (PFN_vkVoidFunction)vk_layer_DestroyInstance;
  }
  if (strcmp(pName, "vkCreateDevice") == 0) {
    return (PFN_vkVoidFunction)vk_layer_CreateDevice;
  }
  /* vkGetDeviceProcAddr is queryable at instance scope per the spec. */
  if (strcmp(pName, "vkGetDeviceProcAddr") == 0) {
    return (PFN_vkVoidFunction)vk_layer_GetDeviceProcAddr;
  }

  /* Heap-clamp on memory-properties query. The Vulkan 1.1 _2 entry
   * and the original _2KHR alias have identical signatures and
   * identical semantics, so they share one hook function. */
  if (strcmp(pName, "vkGetPhysicalDeviceMemoryProperties") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_GetPhysicalDeviceMemoryProperties;
  }
  if (strcmp(pName, "vkGetPhysicalDeviceMemoryProperties2") == 0 ||
      strcmp(pName, "vkGetPhysicalDeviceMemoryProperties2KHR") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_GetPhysicalDeviceMemoryProperties2;
  }

  /* Anything else: forward to the next layer. We only do this when we
   * have an instance handle to look up; the few functions queryable at
   * instance == NULL (vkCreateInstance, vkEnumerateInstance*) are all
   * handled in the explicit names above. */
  if (instance != VK_NULL_HANDLE) {
    vgpu_vk_instance_dispatch_t *d = vgpu_vk_get_instance_dispatch(instance);
    if (d != NULL && d->pfn_GetInstanceProcAddr != NULL) {
      return d->pfn_GetInstanceProcAddr(instance, pName);
    }
  }
  return NULL;
}

static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL
vk_layer_GetDeviceProcAddr(VkDevice device, const char *pName) {
  if (pName == NULL) return NULL;

  if (strcmp(pName, "vkGetDeviceProcAddr") == 0) {
    return (PFN_vkVoidFunction)vk_layer_GetDeviceProcAddr;
  }
  if (strcmp(pName, "vkDestroyDevice") == 0) {
    return (PFN_vkVoidFunction)vk_layer_DestroyDevice;
  }

  /* Device-memory budget enforcement. */
  if (strcmp(pName, "vkAllocateMemory") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_AllocateMemory;
  }
  if (strcmp(pName, "vkFreeMemory") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_FreeMemory;
  }

  /* SM rate limit on queue submission, plus the queue acquisition
   * hooks that record VkQueue -> VkDevice for the submit hooks.
   * _2 and _2KHR share the same hook function (identical signatures,
   * identical semantics). */
  if (strcmp(pName, "vkGetDeviceQueue") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_GetDeviceQueue;
  }
  if (strcmp(pName, "vkGetDeviceQueue2") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_GetDeviceQueue2;
  }
  if (strcmp(pName, "vkQueueSubmit") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_QueueSubmit;
  }
  if (strcmp(pName, "vkQueueSubmit2") == 0 ||
      strcmp(pName, "vkQueueSubmit2KHR") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_QueueSubmit2;
  }

  if (device != VK_NULL_HANDLE) {
    vgpu_vk_device_dispatch_t *d = vgpu_vk_get_device_dispatch(device);
    if (d != NULL && d->pfn_GetDeviceProcAddr != NULL) {
      return d->pfn_GetDeviceProcAddr(device, pName);
    }
  }
  return NULL;
}

/* ------------------------------------------------------------------ */
/* Loader<->layer negotiation - the only ELF-exported entry point.    */
/* ------------------------------------------------------------------ */

/* Belt-and-suspenders: explicitly attach visibility=default to the
 * loader entry point regardless of what VK_LAYER_EXPORT happens to
 * expand to. HAMi PR #182 hit a production failure where a build
 * compiled with -fvisibility=hidden combined with a Vulkan-Headers
 * version where VK_LAYER_EXPORT had degenerated to empty caused this
 * symbol to be hidden. The Vulkan loader could see the .so via the
 * implicit-layer manifest but failed to dlsym this entry, silently
 * inserted the layer name into the chain WITHOUT wiring any function
 * pointers, and every vk* call then bypassed our hooks straight into
 * the ICD — heap reported the unclamped native size, vkAllocateMemory
 * exceeded budget, no error.
 *
 * Our build does not currently use -fvisibility=hidden and our
 * VK_LAYER_EXPORT fallback already produces visibility=default, so in
 * practice this is redundant today (verified by hack/check_vulkan_layer.sh
 * which greps `nm -D` on every CI build). The redundancy guards
 * against a future change to either the build flags or the headers
 * that would otherwise silently regress the layer. */
__attribute__((visibility("default")))
VK_LAYER_EXPORT VkResult VKAPI_CALL
vkNegotiateLoaderLayerInterfaceVersion(VkNegotiateLayerInterface *pVersionStruct) {
  if (pVersionStruct == NULL) {
    return VK_ERROR_INITIALIZATION_FAILED;
  }
  /* Layer interface version 2 is the modern protocol (loader provides
   * the chain via VkLayerInstance/DeviceCreateInfo, layer publishes
   * GIPA/GDPA via this struct). We do not support v1 (legacy). */
  if (pVersionStruct->loaderLayerInterfaceVersion < 2) {
    return VK_ERROR_INITIALIZATION_FAILED;
  }
  if (pVersionStruct->loaderLayerInterfaceVersion > 2) {
    pVersionStruct->loaderLayerInterfaceVersion = 2;
  }
  pVersionStruct->pfnGetInstanceProcAddr       = vk_layer_GetInstanceProcAddr;
  pVersionStruct->pfnGetDeviceProcAddr         = vk_layer_GetDeviceProcAddr;
  /* Physical-device-level hooks (vkGetPhysicalDeviceMemoryProperties[2])
   * are routed via the GIPA chain returned above; the v2 back-channel
   * is unused. */
  pVersionStruct->pfnGetPhysicalDeviceProcAddr = NULL;
  return VK_SUCCESS;
}
