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
#include "include/vulkan/trace.h"

/* The layer name advertised in our implicit-layer manifest at
 * library/deploy/vgpu_manager_implicit_layer.json. Used by the
 * vkEnumerate*Properties hooks below to recognise own-name queries
 * vs queries for other layers / unfiltered queries. MUST stay in
 * lockstep with the manifest "name" field. */
#define VGPU_VK_LAYER_NAME "VK_LAYER_VGPU_MANAGER_vgpu"

/* Forward declarations of the static hooks - referenced by the GetProcAddr
 * lookups before their bodies appear below. */
static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL
vk_layer_GetInstanceProcAddr(VkInstance instance, const char *pName);

static VKAPI_ATTR PFN_vkVoidFunction VKAPI_CALL
vk_layer_GetDeviceProcAddr(VkDevice device, const char *pName);

/* Cached "first successful" next-layer GIPA / GDPA — used as a fallback
 * when GIPA / GDPA is invoked with a handle we have not registered.
 *
 * Why this cache exists: NVIDIA driver and upper-layer wrappers (e.g.,
 * NVIDIA Carbonite under Isaac Sim — see HAMi PR #182's repro) can probe
 * our GIPA / GDPA with VkInstance / VkDevice handles that never appeared
 * in our register call (because an upper layer wrapped them, or because
 * the probe arrives before our register completes). The original code
 * returned NULL in that case; the caller dereferences the result while
 * assembling its dispatch table and SegFaults. Forwarding via the cached
 * next-layer GIPA / GDPA turns the crash into a benign chain walk.
 *
 * Correctness rationale: in any single loader instance, the chain that
 * follows our layer is invariant across VkInstance / VkDevice creations,
 * so the next-gipa captured during the first successful CreateInstance
 * is a valid forwarder for any later instance's queries. The cache is
 * write-once: only the first successful create publishes a value, via a
 * compare-and-swap so a concurrent second create cannot clobber it.
 *
 * Thread safety: the publishing CAS uses __atomic with seq_cst; the
 * read-side load on the GIPA / GDPA fall-through path uses atomic load.
 * No pthread mutex involvement — this path runs on every probe, must
 * stay lock-free. */
static PFN_vkGetInstanceProcAddr g_first_next_gipa = NULL;
static PFN_vkGetDeviceProcAddr   g_first_next_gdpa = NULL;

static inline void cache_first_next_gipa(PFN_vkGetInstanceProcAddr p) {
  if (p == NULL) return;
  PFN_vkGetInstanceProcAddr expected = NULL;
  /* Write-once. Subsequent CreateInstance calls are no-ops here. */
  __atomic_compare_exchange_n(&g_first_next_gipa, &expected, p,
                              /*weak=*/0,
                              __ATOMIC_SEQ_CST, __ATOMIC_SEQ_CST);
}

static inline void cache_first_next_gdpa(PFN_vkGetDeviceProcAddr p) {
  if (p == NULL) return;
  PFN_vkGetDeviceProcAddr expected = NULL;
  __atomic_compare_exchange_n(&g_first_next_gdpa, &expected, p,
                              /*weak=*/0,
                              __ATOMIC_SEQ_CST, __ATOMIC_SEQ_CST);
}

static inline PFN_vkGetInstanceProcAddr load_first_next_gipa(void) {
  return __atomic_load_n(&g_first_next_gipa, __ATOMIC_SEQ_CST);
}

static inline PFN_vkGetDeviceProcAddr load_first_next_gdpa(void) {
  return __atomic_load_n(&g_first_next_gdpa, __ATOMIC_SEQ_CST);
}

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

  /* Publish the next-layer GIPA into the unregistered-handle fallback
   * cache. Only the first successful CreateInstance wins (CAS). Done
   * AFTER register so that any concurrent probe with this handle is
   * already covered by the dispatch lookup; the cache only matters for
   * handles that never reach our register at all. */
  cache_first_next_gipa(next_gipa);

  VGPU_VK_TRACE("vk_layer_CreateInstance: registered instance=%p next_gipa=%p",
                (void *)*pInstance, (void *)next_gipa);

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
#if defined(VK_VERSION_1_3)
  /* Vulkan 1.3 core entry. Compiled out when the build's Vulkan-Headers
   * predate 1.3 (no PFN_vkQueueSubmit2 typedef). The KHR alias
   * fallback below is also gated because it shares the 1.3 PFN type. */
  entry.pfn_QueueSubmit2 =
      (PFN_vkQueueSubmit2)next_gdpa(*pDevice, "vkQueueSubmit2");
  if (entry.pfn_QueueSubmit2 == NULL) {
    entry.pfn_QueueSubmit2 =
        (PFN_vkQueueSubmit2)next_gdpa(*pDevice, "vkQueueSubmit2KHR");
  }
#endif

  vgpu_vk_register_device_dispatch(&entry);

  /* Publish the next-layer GDPA into the unregistered-handle fallback
   * cache. Same write-once semantics as the GIPA cache in
   * vk_layer_CreateInstance — see commentary near g_first_next_gipa. */
  cache_first_next_gdpa(next_gdpa);

  VGPU_VK_TRACE("vk_layer_CreateDevice: registered device=%p physdev=%p next_gdpa=%p",
                (void *)*pDevice, (void *)physicalDevice, (void *)next_gdpa);

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
/* Enumerate*Properties hooks (Vulkan 1.3 §38.3.1)                     */
/*                                                                     */
/* The Vulkan loader queries each layer's GIPA with VK_NULL_HANDLE     */
/* during its initialization handshake to discover which extensions    */
/* and layers each layer reports. The 4 hook names below are what the  */
/* loader probes. If GIPA returns NULL for any of them, the loader     */
/* (and downstream graphics frameworks like NVIDIA Carbonite, used by  */
/* Isaac Sim Kit) dereferences the NULL while assembling the enabled   */
/* extension list and SegFaults inside libvulkan.so during             */
/* vkCreateInstance — observed by HAMi PR #182 in production with     */
/* Isaac Sim Kit 6.0.0-rc.22.                                          */
/*                                                                     */
/* Spec-correct contract:                                              */
/*   - pLayerName == OUR layer name -> VK_SUCCESS, count = 0           */
/*     (we expose no instance/device extensions of our own)            */
/*   - pLayerName == NULL or any other layer's name                    */
/*     -> VK_ERROR_LAYER_NOT_PRESENT                                   */
/*     This is critical: returning VK_SUCCESS with count=0 here makes  */
/*     the loader treat our zero-count as authoritative for "all       */
/*     instance extensions", silently hiding the ICD's instance-level  */
/*     extensions and breaking any vkCreateInstance call that requests */
/*     driver extensions. LAYER_NOT_PRESENT lets the loader walk past  */
/*     us to the next layer / ICD, preserving the dispatch chain.     */
/* ------------------------------------------------------------------ */

static VKAPI_ATTR VkResult VKAPI_CALL
vgpu_vk_EnumerateInstanceExtensionProperties(const char           *pLayerName,
                                             uint32_t             *pPropertyCount,
                                             VkExtensionProperties *pProperties) {
  (void)pProperties;
  if (pLayerName != NULL && strcmp(pLayerName, VGPU_VK_LAYER_NAME) == 0) {
    if (pPropertyCount) *pPropertyCount = 0;
    return VK_SUCCESS;
  }
  return VK_ERROR_LAYER_NOT_PRESENT;
}

static VKAPI_ATTR VkResult VKAPI_CALL
vgpu_vk_EnumerateInstanceLayerProperties(uint32_t          *pPropertyCount,
                                         VkLayerProperties *pProperties) {
  /* The loader sources the layer list from manifest files itself; the
   * layer just reports its own count. Returning 0 is accepted by the
   * loader and matches HAMi PR #182's choice. */
  (void)pProperties;
  if (pPropertyCount) *pPropertyCount = 0;
  return VK_SUCCESS;
}

static VKAPI_ATTR VkResult VKAPI_CALL
vgpu_vk_EnumerateDeviceExtensionProperties(VkPhysicalDevice       physicalDevice,
                                           const char            *pLayerName,
                                           uint32_t              *pPropertyCount,
                                           VkExtensionProperties *pProperties) {
  /* Same own-name vs other-name rule as the instance variant. The
   * SegFault HAMi observed was on the instance variant; we apply the
   * same shape here for consistency with the spec and to avoid the
   * symmetric trap should some future caller probe the device path
   * with VK_NULL_HANDLE-equivalent uninitialized handles. */
  (void)physicalDevice;
  (void)pProperties;
  if (pLayerName != NULL && strcmp(pLayerName, VGPU_VK_LAYER_NAME) == 0) {
    if (pPropertyCount) *pPropertyCount = 0;
    return VK_SUCCESS;
  }
  return VK_ERROR_LAYER_NOT_PRESENT;
}

static VKAPI_ATTR VkResult VKAPI_CALL
vgpu_vk_EnumerateDeviceLayerProperties(VkPhysicalDevice    physicalDevice,
                                       uint32_t           *pPropertyCount,
                                       VkLayerProperties  *pProperties) {
  /* Deprecated since Vulkan 1.0.13 — the loader handles it itself.
   * Reporting count=0 keeps spec-conformant callers happy. */
  (void)physicalDevice;
  (void)pProperties;
  if (pPropertyCount) *pPropertyCount = 0;
  return VK_SUCCESS;
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

  /* Spec-required Enumerate*Properties hooks. These MUST resolve to a
   * non-NULL pfn when the loader probes our GIPA with VK_NULL_HANDLE
   * during initialization, otherwise downstream code (NVIDIA Carbonite
   * was the observed crash site in HAMi PR #182's Isaac Sim
   * deployment) dereferences NULL during extension-list assembly and
   * SegFaults. See the function bodies above for the spec-correct
   * own-name-vs-other-name return shape. */
  if (strcmp(pName, "vkEnumerateInstanceExtensionProperties") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_EnumerateInstanceExtensionProperties;
  }
  if (strcmp(pName, "vkEnumerateInstanceLayerProperties") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_EnumerateInstanceLayerProperties;
  }
  if (strcmp(pName, "vkEnumerateDeviceExtensionProperties") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_EnumerateDeviceExtensionProperties;
  }
  if (strcmp(pName, "vkEnumerateDeviceLayerProperties") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_EnumerateDeviceLayerProperties;
  }

  /* Anything else: forward to the next layer. We only do this when we
   * have an instance handle to look up; the few functions queryable at
   * instance == NULL (vkCreateInstance, the four vkEnumerate*Properties
   * above) are all handled in the explicit names above. */
  if (instance != VK_NULL_HANDLE) {
    vgpu_vk_instance_dispatch_t *d = vgpu_vk_get_instance_dispatch(instance);
    if (d != NULL && d->pfn_GetInstanceProcAddr != NULL) {
      return d->pfn_GetInstanceProcAddr(instance, pName);
    }
    /* Unregistered VkInstance: fall back to the GIPA captured at the
     * first successful CreateInstance. This handles upper-layer-wrapped
     * handles and pre-register probes that would otherwise SegFault the
     * caller (HAMi PR #182 Carbonite repro). The cache is invariant
     * within a single loader instance, so it is a valid forwarder for
     * any handle's queries. */
    PFN_vkGetInstanceProcAddr cached = load_first_next_gipa();
    if (cached != NULL) {
      VGPU_VK_TRACE("GIPA fallback: instance=%p pName=%s -> cached_gipa=%p",
                    (void *)instance, pName, (void *)cached);
      return cached(instance, pName);
    }
    VGPU_VK_TRACE("GIPA: instance=%p pName=%s NOT registered AND no cache -> NULL",
                  (void *)instance, pName);
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
#if defined(VK_VERSION_1_3)
  /* Same gate as the dispatch slot / hook function definition: the
   * 1.3 core entry and its KHR alias share PFN_vkQueueSubmit2, which
   * is undeclared on Vulkan-Headers <1.3. Without 1.3 headers the
   * loader simply falls through to the next layer for these names. */
  if (strcmp(pName, "vkQueueSubmit2") == 0 ||
      strcmp(pName, "vkQueueSubmit2KHR") == 0) {
    return (PFN_vkVoidFunction)vgpu_vk_QueueSubmit2;
  }
#endif

  if (device != VK_NULL_HANDLE) {
    vgpu_vk_device_dispatch_t *d = vgpu_vk_get_device_dispatch(device);
    if (d != NULL && d->pfn_GetDeviceProcAddr != NULL) {
      return d->pfn_GetDeviceProcAddr(device, pName);
    }
    /* Unregistered VkDevice: same fallback rationale as the GIPA tail
     * above. See g_first_next_gipa commentary. */
    PFN_vkGetDeviceProcAddr cached = load_first_next_gdpa();
    if (cached != NULL) {
      VGPU_VK_TRACE("GDPA fallback: device=%p pName=%s -> cached_gdpa=%p",
                    (void *)device, pName, (void *)cached);
      return cached(device, pName);
    }
    VGPU_VK_TRACE("GDPA: device=%p pName=%s NOT registered AND no cache -> NULL",
                  (void *)device, pName);
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
