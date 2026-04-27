/*
 * vgpu-manager Vulkan implicit layer - core skeleton (Phase 2).
 *
 * Implements the loader<->layer negotiation protocol, the dispatch
 * chain wiring at vkCreateInstance / vkCreateDevice time, and the
 * per-layer GetProcAddr lookups. NO business logic - every call is
 * forwarded transparently to the next layer in the chain. Memory
 * budget enforcement (Phase 5), heap clamping (Phase 4), queue
 * throttling (Phase 6) layer on top of this skeleton.
 *
 * Symbol export discipline (very important, see layer.h):
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

#include "layer.h"
#include "dispatch.h"

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
  vgpu_instance_dispatch_t entry;
  memset(&entry, 0, sizeof(entry));
  entry.instance                = *pInstance;
  entry.pfn_GetInstanceProcAddr = next_gipa;
  entry.pfn_DestroyInstance     =
      (PFN_vkDestroyInstance)next_gipa(*pInstance, "vkDestroyInstance");
  vgpu_register_instance(&entry);

  return VK_SUCCESS;
}

static VKAPI_ATTR void VKAPI_CALL
vk_layer_DestroyInstance(VkInstance instance,
                         const VkAllocationCallbacks *pAllocator) {
  vgpu_instance_dispatch_t *d = vgpu_get_instance_dispatch(instance);
  PFN_vkDestroyInstance next  = (d != NULL) ? d->pfn_DestroyInstance : NULL;
  /* Remove the entry first so a racing concurrent lookup that arrives
   * after vkDestroyInstance returns can't see a stale dispatch table. */
  vgpu_remove_instance(instance);
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

  vgpu_device_dispatch_t entry;
  memset(&entry, 0, sizeof(entry));
  entry.device                = *pDevice;
  entry.physical_device       = physicalDevice;
  entry.pfn_GetDeviceProcAddr = next_gdpa;
  entry.pfn_DestroyDevice     =
      (PFN_vkDestroyDevice)next_gdpa(*pDevice, "vkDestroyDevice");
  vgpu_register_device(&entry);

  return VK_SUCCESS;
}

static VKAPI_ATTR void VKAPI_CALL
vk_layer_DestroyDevice(VkDevice device,
                       const VkAllocationCallbacks *pAllocator) {
  vgpu_device_dispatch_t *d = vgpu_get_device_dispatch(device);
  PFN_vkDestroyDevice next  = (d != NULL) ? d->pfn_DestroyDevice : NULL;
  vgpu_remove_device(device);
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

  /* Anything else: forward to the next layer. We only do this when we
   * have an instance handle to look up; the few functions queryable at
   * instance == NULL (vkCreateInstance, vkEnumerateInstance*) are all
   * handled in the explicit names above. */
  if (instance != VK_NULL_HANDLE) {
    vgpu_instance_dispatch_t *d = vgpu_get_instance_dispatch(instance);
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

  if (device != VK_NULL_HANDLE) {
    vgpu_device_dispatch_t *d = vgpu_get_device_dispatch(device);
    if (d != NULL && d->pfn_GetDeviceProcAddr != NULL) {
      return d->pfn_GetDeviceProcAddr(device, pName);
    }
  }
  return NULL;
}

/* ------------------------------------------------------------------ */
/* Loader<->layer negotiation - the only ELF-exported entry point.    */
/* ------------------------------------------------------------------ */

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
  /* No physical-device-level hook in Phase 2; Phase 4 will provide one
   * via the dispatch chain rather than this back-channel. */
  pVersionStruct->pfnGetPhysicalDeviceProcAddr = NULL;
  return VK_SUCCESS;
}
