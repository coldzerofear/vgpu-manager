/*
 * Unregistered-handle GIPA/GDPA fallback test (HAMi PR #182 Carbonite repro).
 *
 * Verifies the cache-first behavior added to vk_layer_GetInstanceProcAddr
 * and vk_layer_GetDeviceProcAddr: when the caller probes our GIPA/GDPA
 * with a VkInstance/VkDevice handle that has not been registered by our
 * layer (because an upper layer wrapped it, or because the probe arrived
 * before / after our register / unregister window), forward through the
 * "first successful next-gipa/gdpa" cache instead of returning NULL.
 *
 * Strategy:
 *   1. Pull our layer's GIPA via vkNegotiateLoaderLayerInterfaceVersion.
 *   2. Drive vk_layer_CreateInstance through a fake chain whose
 *      pfnNextGetInstanceProcAddr is a test stub. This seeds the
 *      GIPA cache (g_first_next_gipa) with our test stub pointer.
 *   3. Drive vk_layer_CreateDevice through a fake chain whose
 *      pfnNextGetDeviceProcAddr is also our test stub. Seeds the
 *      GDPA cache (g_first_next_gdpa).
 *   4. Probe layer GIPA / GDPA with a *different* handle (one not
 *      registered with our layer) and a name that none of our static
 *      handlers match. Assert the call forwards through to our stub.
 *
 * No Vulkan ICD or GPU is required — the chain is fully mocked.
 *
 * stubs.c provides load_necessary_data / init_devices_mapping /
 * vgpu_ensure_sm_watcher_started as no-ops, so vk_layer_CreateInstance's
 * front-loading calls do not block the test.
 */
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>

#include <vulkan/vulkan.h>
#include <vulkan/vk_layer.h>

#include "test_helpers.h"

/* The negotiate entry is the one ELF-visible export of layer.c. */
extern VkResult VKAPI_CALL
vkNegotiateLoaderLayerInterfaceVersion(VkNegotiateLayerInterface *pVersionStruct);

/* ---- Stub chain bookkeeping ---- */

/* Sentinel pointer the test stub returns for any unrecognised name —
 * recognisable in assertions to confirm the call reached the stub. */
static int g_sentinel_marker;
#define SENTINEL_PFN ((PFN_vkVoidFunction)&g_sentinel_marker)

static int g_stub_gipa_called_with_pName_count;
static int g_stub_gdpa_called_with_pName_count;
static const char *g_stub_gipa_last_pName;
static const char *g_stub_gdpa_last_pName;
static VkInstance  g_stub_gipa_last_instance;
static VkDevice    g_stub_gdpa_last_device;

/* Fake VkInstance / VkDevice handles produced by the next-layer stub
 * during CreateInstance / CreateDevice. Distinct from the
 * unregistered handles we use later in the assertions. */
static VkInstance       g_fake_inst       = (VkInstance)(uintptr_t)0xCA11AB1E;
static VkDevice         g_fake_dev        = (VkDevice)(uintptr_t)0xDECEA5ED;
static VkPhysicalDevice g_fake_phys       = (VkPhysicalDevice)(uintptr_t)0xC0FFEE;

/* Forward decls for stub chain functions referenced by stub_gipa. */
static VkResult VKAPI_CALL stub_create_instance(const VkInstanceCreateInfo *,
                                                const VkAllocationCallbacks *,
                                                VkInstance *);
static void     VKAPI_CALL stub_destroy_instance(VkInstance,
                                                 const VkAllocationCallbacks *);
static VkResult VKAPI_CALL stub_create_device(VkPhysicalDevice,
                                              const VkDeviceCreateInfo *,
                                              const VkAllocationCallbacks *,
                                              VkDevice *);
static void     VKAPI_CALL stub_destroy_device(VkDevice,
                                               const VkAllocationCallbacks *);

/* Next-layer GIPA stub. The layer queries this for vkCreateInstance,
 * vkDestroyInstance and a handful of phys-prop / memory-prop names
 * during CreateInstance; we hand back canned pointers for the
 * structural ones and SENTINEL_PFN for everything else so the test
 * later recognises forwarded calls. */
static PFN_vkVoidFunction VKAPI_CALL
stub_gipa(VkInstance instance, const char *pName) {
  g_stub_gipa_called_with_pName_count++;
  g_stub_gipa_last_pName    = pName;
  g_stub_gipa_last_instance = instance;
  if (pName == NULL) return NULL;
  if (strcmp(pName, "vkCreateInstance") == 0)
    return (PFN_vkVoidFunction)stub_create_instance;
  if (strcmp(pName, "vkDestroyInstance") == 0)
    return (PFN_vkVoidFunction)stub_destroy_instance;
  if (strcmp(pName, "vkCreateDevice") == 0)
    return (PFN_vkVoidFunction)stub_create_device;
  /* Anything else (memory-properties, phys-dev-properties, etc.):
   * sentinel so caller can recognise a forwarded call. */
  return SENTINEL_PFN;
}

static PFN_vkVoidFunction VKAPI_CALL
stub_gdpa(VkDevice device, const char *pName) {
  g_stub_gdpa_called_with_pName_count++;
  g_stub_gdpa_last_pName  = pName;
  g_stub_gdpa_last_device = device;
  if (pName == NULL) return NULL;
  if (strcmp(pName, "vkDestroyDevice") == 0)
    return (PFN_vkVoidFunction)stub_destroy_device;
  return SENTINEL_PFN;
}

/* The "next layer" CreateInstance: succeeds, hands back our fake
 * VkInstance. Note the layer code in vk_layer_CreateInstance also
 * subsequently calls vgpu_vk_register_instance_physdevs(*pInstance,
 * next_gipa) — physdev_index.c will issue
 * vkEnumeratePhysicalDevices via stub_gipa; we return SENTINEL_PFN
 * for that name (anything-not-recognised path). The first call to
 * pfnEnumeratePhysicalDevices from within physdev_index will then
 * be the SENTINEL pointer cast to a function — if invoked it would
 * crash. To avoid that, we shadow it: when stub_gipa is asked for
 * vkEnumeratePhysicalDevices, return a no-op that reports zero
 * physical devices. */
static VkResult VKAPI_CALL
stub_enum_physdevs(VkInstance inst,
                   uint32_t *pPhysicalDeviceCount,
                   VkPhysicalDevice *pPhysicalDevices) {
  (void)inst;
  (void)pPhysicalDevices;
  if (pPhysicalDeviceCount) *pPhysicalDeviceCount = 0;
  return VK_SUCCESS;
}

static VkResult VKAPI_CALL
stub_create_instance(const VkInstanceCreateInfo  *pCreateInfo,
                     const VkAllocationCallbacks *pAllocator,
                     VkInstance                  *pInstance) {
  (void)pCreateInfo; (void)pAllocator;
  if (pInstance) *pInstance = g_fake_inst;
  return VK_SUCCESS;
}

static void VKAPI_CALL
stub_destroy_instance(VkInstance instance,
                      const VkAllocationCallbacks *pAllocator) {
  (void)instance; (void)pAllocator;
}

static VkResult VKAPI_CALL
stub_create_device(VkPhysicalDevice physicalDevice,
                   const VkDeviceCreateInfo  *pCreateInfo,
                   const VkAllocationCallbacks *pAllocator,
                   VkDevice                  *pDevice) {
  (void)physicalDevice; (void)pCreateInfo; (void)pAllocator;
  if (pDevice) *pDevice = g_fake_dev;
  return VK_SUCCESS;
}

static void VKAPI_CALL
stub_destroy_device(VkDevice device,
                    const VkAllocationCallbacks *pAllocator) {
  (void)device; (void)pAllocator;
}

/* Override stub_gipa so that vkEnumeratePhysicalDevices resolves to a
 * working no-op pfn instead of SENTINEL. We can't add a special-case
 * inside stub_gipa above without forward-declaring it here, so do it
 * here. */
static PFN_vkVoidFunction VKAPI_CALL
stub_gipa_v2(VkInstance instance, const char *pName) {
  if (pName != NULL && strcmp(pName, "vkEnumeratePhysicalDevices") == 0) {
    g_stub_gipa_called_with_pName_count++;
    g_stub_gipa_last_pName    = pName;
    g_stub_gipa_last_instance = instance;
    return (PFN_vkVoidFunction)stub_enum_physdevs;
  }
  return stub_gipa(instance, pName);
}

/* ---- Test driver ---- */

static PFN_vkGetInstanceProcAddr negotiate_get_layer_gipa(void) {
  VkNegotiateLayerInterface iface;
  memset(&iface, 0, sizeof(iface));
  iface.sType = LAYER_NEGOTIATE_INTERFACE_STRUCT;
  iface.loaderLayerInterfaceVersion = 2;
  VkResult r = vkNegotiateLoaderLayerInterfaceVersion(&iface);
  assert(r == VK_SUCCESS);
  assert(iface.pfnGetInstanceProcAddr != NULL);
  assert(iface.pfnGetDeviceProcAddr != NULL);
  return iface.pfnGetInstanceProcAddr;
}

static PFN_vkGetDeviceProcAddr negotiate_get_layer_gdpa(void) {
  VkNegotiateLayerInterface iface;
  memset(&iface, 0, sizeof(iface));
  iface.sType = LAYER_NEGOTIATE_INTERFACE_STRUCT;
  iface.loaderLayerInterfaceVersion = 2;
  VkResult r = vkNegotiateLoaderLayerInterfaceVersion(&iface);
  assert(r == VK_SUCCESS);
  return iface.pfnGetDeviceProcAddr;
}

/* Step 1: empty cache + unregistered handle + name not in our static
 * handler set => NULL (today's behavior). Locks in the safe
 * boundary: the new fallback only widens behavior, never narrows. */
static void test_unregistered_handle_no_cache_returns_null(void) {
  PFN_vkGetInstanceProcAddr gipa = negotiate_get_layer_gipa();
  PFN_vkGetDeviceProcAddr   gdpa = negotiate_get_layer_gdpa();

  VkInstance unreg_inst = (VkInstance)(uintptr_t)0xBAD1;
  VkDevice   unreg_dev  = (VkDevice)(uintptr_t)0xBAD2;

  /* "vkAllocateMemory" is a device-scope name, not handled by GIPA
   * statics — so it falls through the lookup tail. With nothing
   * cached and nothing registered, returns NULL. */
  PFN_vkVoidFunction p1 = gipa(unreg_inst, "vkAllocateMemory");
  PFN_vkVoidFunction p2 = gdpa(unreg_dev,  "vkBindBufferMemory");
  assert(p1 == NULL);
  assert(p2 == NULL);

  vgpu_test_pass("empty cache + unregistered handle => NULL");
}

/* Step 2: drive CreateInstance through the stub chain to seed the
 * cache, then probe with an unregistered instance handle and assert
 * forwarding. */
static void test_unregistered_instance_after_create_forwards(void) {
  PFN_vkGetInstanceProcAddr gipa = negotiate_get_layer_gipa();

  /* Resolve our layer's CreateInstance (the loader does this with
   * VK_NULL_HANDLE). */
  PFN_vkCreateInstance layer_create =
      (PFN_vkCreateInstance)gipa(VK_NULL_HANDLE, "vkCreateInstance");
  assert(layer_create != NULL);

  /* Build a chain link node pointing pfnNextGetInstanceProcAddr at
   * stub_gipa_v2. The layer code reads this via find_instance_chain_info
   * and advances pLayerInfo before calling next_create. */
  VkLayerInstanceLink link;
  memset(&link, 0, sizeof(link));
  link.pfnNextGetInstanceProcAddr = stub_gipa_v2;

  VkLayerInstanceCreateInfo chain_node;
  memset(&chain_node, 0, sizeof(chain_node));
  chain_node.sType    = VK_STRUCTURE_TYPE_LOADER_INSTANCE_CREATE_INFO;
  chain_node.function = VK_LAYER_LINK_INFO;
  chain_node.u.pLayerInfo = &link;

  VkInstanceCreateInfo ci;
  memset(&ci, 0, sizeof(ci));
  ci.sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO;
  ci.pNext = &chain_node;

  VkInstance got = VK_NULL_HANDLE;
  VkResult r = layer_create(&ci, NULL, &got);
  assert(r == VK_SUCCESS);
  assert(got == g_fake_inst);

  /* Now probe with a DIFFERENT, unregistered handle. The static name
   * matches must not catch it; the dispatch lookup must miss; the
   * fallback must forward through the cached stub_gipa_v2. We use a
   * name that doesn't match any of our static handlers
   * ("vkSomeUnknownDeviceQuery"). The forward should land on stub_gipa
   * which returns SENTINEL_PFN for any unrecognised name. */
  VkInstance unreg_inst = (VkInstance)(uintptr_t)0xDEAD1;
  int before = g_stub_gipa_called_with_pName_count;
  PFN_vkVoidFunction pfn = gipa(unreg_inst, "vkSomeUnknownDeviceQuery");
  int after  = g_stub_gipa_called_with_pName_count;

  assert(after == before + 1);
  assert(pfn == SENTINEL_PFN);
  /* Stub recorded our handle, confirming it received the fallback
   * forward (instance is passed through verbatim). */
  assert(g_stub_gipa_last_instance == unreg_inst);
  assert(g_stub_gipa_last_pName != NULL);
  assert(strcmp(g_stub_gipa_last_pName, "vkSomeUnknownDeviceQuery") == 0);

  vgpu_test_pass("unregistered VkInstance forwards via cached next-gipa");
}

/* Step 3: drive CreateDevice through the stub chain to seed the GDPA
 * cache, then probe with an unregistered device handle. */
static void test_unregistered_device_after_create_forwards(void) {
  PFN_vkGetInstanceProcAddr gipa = negotiate_get_layer_gipa();

  PFN_vkCreateDevice layer_create_dev =
      (PFN_vkCreateDevice)gipa(VK_NULL_HANDLE, "vkCreateDevice");
  assert(layer_create_dev != NULL);

  VkLayerDeviceLink link;
  memset(&link, 0, sizeof(link));
  link.pfnNextGetInstanceProcAddr = stub_gipa_v2;
  link.pfnNextGetDeviceProcAddr   = stub_gdpa;

  VkLayerDeviceCreateInfo chain_node;
  memset(&chain_node, 0, sizeof(chain_node));
  chain_node.sType    = VK_STRUCTURE_TYPE_LOADER_DEVICE_CREATE_INFO;
  chain_node.function = VK_LAYER_LINK_INFO;
  chain_node.u.pLayerInfo = &link;

  VkDeviceCreateInfo ci;
  memset(&ci, 0, sizeof(ci));
  ci.sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO;
  ci.pNext = &chain_node;

  VkDevice got = VK_NULL_HANDLE;
  VkResult r = layer_create_dev(g_fake_phys, &ci, NULL, &got);
  assert(r == VK_SUCCESS);
  assert(got == g_fake_dev);

  /* Probe via the layer's GDPA: pull it from negotiate, exercise it
   * with an unregistered device handle. */
  PFN_vkGetDeviceProcAddr gdpa = negotiate_get_layer_gdpa();
  VkDevice unreg_dev = (VkDevice)(uintptr_t)0xDEAD2;

  int before = g_stub_gdpa_called_with_pName_count;
  PFN_vkVoidFunction pfn = gdpa(unreg_dev, "vkSomeUnknownDeviceCmd");
  int after  = g_stub_gdpa_called_with_pName_count;

  assert(after == before + 1);
  assert(pfn == SENTINEL_PFN);
  assert(g_stub_gdpa_last_device == unreg_dev);
  assert(g_stub_gdpa_last_pName != NULL);
  assert(strcmp(g_stub_gdpa_last_pName, "vkSomeUnknownDeviceCmd") == 0);

  vgpu_test_pass("unregistered VkDevice forwards via cached next-gdpa");
}

int main(void) {
  vgpu_test_reset_all();
  /* g_vgpu_config must be non-NULL during vk_layer_CreateInstance —
   * register_instance_physdevs reads through it. The reset above
   * publishes a zeroed vgpu_test_config. With zero physical-device
   * count returned by stub_enum_physdevs, no UUID resolution runs. */

  test_unregistered_handle_no_cache_returns_null();
  test_unregistered_instance_after_create_forwards();
  test_unregistered_device_after_create_forwards();

  printf("ok: test_layer_fallback complete\n");
  return 0;
}
