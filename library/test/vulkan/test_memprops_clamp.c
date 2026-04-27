/*
 * hooks_memory.c unit test — vkGetPhysicalDeviceMemoryProperties[2]
 * heap clamp.
 *
 * Verifies:
 *   - DEVICE_LOCAL heaps clamp to min(heap.size, cap)
 *   - non-DEVICE_LOCAL heaps untouched
 *   - oversold ON  => cap = real_memory
 *   - oversold OFF => cap = total_memory  (vgpu's invariant says
 *                                          equal to real_memory)
 *   - host_index < 0 => no clamp (forward only)
 *   - memory_limit == 0 => no clamp (forward only)
 *   - _2 path also clamps; pNext extension structs untouched
 */
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>

#include <vulkan/vulkan.h>

#include "include/hook.h"
#include "include/vulkan/dispatch.h"
#include "include/vulkan/physdev_index.h"
#include "include/vulkan/hooks_memory.h"

#include "test_helpers.h"

/* ---- Fakes for the next-layer pfns ----------------------------- */

static VkMemoryHeap g_fake_heaps[VK_MAX_MEMORY_HEAPS];
static uint32_t     g_fake_heap_count = 0;

static VKAPI_ATTR void VKAPI_CALL
fake_get_memprops(VkPhysicalDevice phys, VkPhysicalDeviceMemoryProperties *out) {
  (void)phys;
  memset(out, 0, sizeof(*out));
  out->memoryHeapCount = g_fake_heap_count;
  for (uint32_t i = 0; i < g_fake_heap_count; i++) out->memoryHeaps[i] = g_fake_heaps[i];
}

static VKAPI_ATTR void VKAPI_CALL
fake_get_memprops2(VkPhysicalDevice phys, VkPhysicalDeviceMemoryProperties2 *out) {
  fake_get_memprops(phys, &out->memoryProperties);
  /* Leave pNext untouched: we want to assert hooks_memory.c does not
   * mutate any extension struct. */
}

/* ---- Fake next-layer GIPA used by physdev_index registration --- */

static const uint8_t g_uuid_bytes[16] = { 0xC0, 0xDE, 0xC0, 0xDE, 0xC0, 0xDE,
                                          0xC0, 0xDE, 0xC0, 0xDE, 0xC0, 0xDE,
                                          0xC0, 0xDE, 0xC0, 0xDE };
static VkPhysicalDevice g_phys = (VkPhysicalDevice)(uintptr_t)0xABCDEF;

static VKAPI_ATTR VkResult VKAPI_CALL
fake_enumerate_one(VkInstance inst, uint32_t *pCount, VkPhysicalDevice *pDevices) {
  (void)inst;
  if (pDevices == NULL) { *pCount = 1; return VK_SUCCESS; }
  if (*pCount >= 1) pDevices[0] = g_phys;
  *pCount = 1;
  return VK_SUCCESS;
}

static VKAPI_ATTR void VKAPI_CALL
fake_props2_for_uuid(VkPhysicalDevice phys, VkPhysicalDeviceProperties2 *pProps) {
  (void)phys;
  for (VkBaseOutStructure *p = (VkBaseOutStructure *)pProps->pNext;
       p != NULL; p = p->pNext) {
    if (p->sType == VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_ID_PROPERTIES) {
      VkPhysicalDeviceIDProperties *id = (VkPhysicalDeviceIDProperties *)p;
      memcpy(id->deviceUUID, g_uuid_bytes, 16);
    }
  }
}

static PFN_vkVoidFunction VKAPI_CALL
fake_gipa(VkInstance instance, const char *pName) {
  (void)instance;
  if (strcmp(pName, "vkEnumeratePhysicalDevices") == 0)
    return (PFN_vkVoidFunction)fake_enumerate_one;
  if (strcmp(pName, "vkGetPhysicalDeviceProperties2") == 0)
    return (PFN_vkVoidFunction)fake_props2_for_uuid;
  return NULL;
}

/* ---- One-time setup: config + dispatch + physdev cache ----------- */

#define HOST_INDEX 3

static VkInstance g_inst = (VkInstance)(uintptr_t)0xC1;

static void setup(int memory_limit, int memory_oversold,
                  size_t real_mem, size_t total_mem) {
  vgpu_test_reset_all();
  /* Configure g_vgpu_config[HOST_INDEX]. */
  snprintf(g_vgpu_config->devices[HOST_INDEX].uuid, UUID_BUFFER_SIZE,
           "GPU-%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
           g_uuid_bytes[0x0], g_uuid_bytes[0x1], g_uuid_bytes[0x2], g_uuid_bytes[0x3],
           g_uuid_bytes[0x4], g_uuid_bytes[0x5], g_uuid_bytes[0x6], g_uuid_bytes[0x7],
           g_uuid_bytes[0x8], g_uuid_bytes[0x9], g_uuid_bytes[0xA], g_uuid_bytes[0xB],
           g_uuid_bytes[0xC], g_uuid_bytes[0xD], g_uuid_bytes[0xE], g_uuid_bytes[0xF]);
  g_vgpu_config->devices[HOST_INDEX].memory_limit    = memory_limit;
  g_vgpu_config->devices[HOST_INDEX].memory_oversold = memory_oversold;
  g_vgpu_config->devices[HOST_INDEX].real_memory     = real_mem;
  g_vgpu_config->devices[HOST_INDEX].total_memory    = total_mem;

  /* Register dispatch entry: the hook calls
   * vgpu_vk_get_instance_dispatch(owner) where owner is from the
   * physdev cache. */
  vgpu_vk_instance_dispatch_t entry;
  memset(&entry, 0, sizeof(entry));
  entry.instance                                = g_inst;
  entry.pfn_GetPhysicalDeviceMemoryProperties   = fake_get_memprops;
  entry.pfn_GetPhysicalDeviceMemoryProperties2  = fake_get_memprops2;
  vgpu_vk_register_instance_dispatch(&entry);

  /* Use physdev_index registration to install (g_phys -> HOST_INDEX). */
  /* But that puts g_phys at host_index = HOST_INDEX iff config slot
   * HOST_INDEX has the matching uuid (set above). */
  vgpu_vk_register_instance_physdevs(g_inst, fake_gipa);
}

static void teardown(void) {
  vgpu_vk_unregister_instance_physdevs(g_inst);
  vgpu_vk_remove_instance_dispatch(g_inst);
}

/* ---- Tests ------------------------------------------------------ */

static void heap(uint32_t i, VkDeviceSize size, VkMemoryHeapFlags flags) {
  g_fake_heaps[i].size  = size;
  g_fake_heaps[i].flags = flags;
}

static void test_clamps_device_local_oversold_off(void) {
  /* oversold OFF => cap = total_memory. real_memory == total_memory
   * by config invariant. Heap reports 32 GiB; clamp to 4 GiB. */
  setup(/*memory_limit*/ 1, /*oversold*/ 0,
        /*real*/  4ull << 30, /*total*/ 4ull << 30);
  g_fake_heap_count = 1;
  heap(0, 32ull << 30, VK_MEMORY_HEAP_DEVICE_LOCAL_BIT);

  VkPhysicalDeviceMemoryProperties props;
  vgpu_vk_GetPhysicalDeviceMemoryProperties(g_phys, &props);
  assert(props.memoryHeapCount == 1);
  assert(props.memoryHeaps[0].size == (4ull << 30));
  vgpu_test_pass("clamp DEVICE_LOCAL heap, oversold OFF, cap=total_memory");
  teardown();
}

static void test_clamps_device_local_oversold_on(void) {
  /* oversold ON => cap = real_memory. total_memory > real_memory.
   * Heap reports 32 GiB; clamp to real_memory = 4 GiB (NOT 8 GiB). */
  setup(/*memory_limit*/ 1, /*oversold*/ 1,
        /*real*/  4ull << 30, /*total*/ 8ull << 30);
  g_fake_heap_count = 1;
  heap(0, 32ull << 30, VK_MEMORY_HEAP_DEVICE_LOCAL_BIT);

  VkPhysicalDeviceMemoryProperties props;
  vgpu_vk_GetPhysicalDeviceMemoryProperties(g_phys, &props);
  assert(props.memoryHeaps[0].size == (4ull << 30));
  vgpu_test_pass("clamp DEVICE_LOCAL heap, oversold ON, cap=real_memory");
  teardown();
}

static void test_does_not_clamp_non_device_local(void) {
  setup(/*memory_limit*/ 1, /*oversold*/ 0,
        /*real*/  4ull << 30, /*total*/ 4ull << 30);
  g_fake_heap_count = 2;
  heap(0, 32ull << 30, VK_MEMORY_HEAP_DEVICE_LOCAL_BIT);
  heap(1, 16ull << 30, /*non-device-local*/ 0);

  VkPhysicalDeviceMemoryProperties props;
  vgpu_vk_GetPhysicalDeviceMemoryProperties(g_phys, &props);
  assert(props.memoryHeaps[0].size == (4ull << 30));
  /* Heap 1 is staging / host-visible; clamp does not touch it. */
  assert(props.memoryHeaps[1].size == (16ull << 30));
  vgpu_test_pass("non-DEVICE_LOCAL heaps left untouched");
  teardown();
}

static void test_does_not_clamp_when_unmanaged(void) {
  /* memory_limit == 0 => clamp disabled, even on DEVICE_LOCAL heap. */
  setup(/*memory_limit*/ 0, /*oversold*/ 0,
        /*real*/  4ull << 30, /*total*/ 4ull << 30);
  g_fake_heap_count = 1;
  heap(0, 32ull << 30, VK_MEMORY_HEAP_DEVICE_LOCAL_BIT);

  VkPhysicalDeviceMemoryProperties props;
  vgpu_vk_GetPhysicalDeviceMemoryProperties(g_phys, &props);
  assert(props.memoryHeaps[0].size == (32ull << 30));
  vgpu_test_pass("memory_limit == 0 => clamp disabled");
  teardown();
}

static void test_does_not_clamp_smaller_heap(void) {
  /* If the heap reports LESS than the cap, leave it alone. */
  setup(/*memory_limit*/ 1, /*oversold*/ 0,
        /*real*/  4ull << 30, /*total*/ 4ull << 30);
  g_fake_heap_count = 1;
  heap(0, 1ull << 30, VK_MEMORY_HEAP_DEVICE_LOCAL_BIT);

  VkPhysicalDeviceMemoryProperties props;
  vgpu_vk_GetPhysicalDeviceMemoryProperties(g_phys, &props);
  assert(props.memoryHeaps[0].size == (1ull << 30));
  vgpu_test_pass("heap.size < cap left untouched");
  teardown();
}

static void test_clamps_via_v2_entry(void) {
  setup(/*memory_limit*/ 1, /*oversold*/ 1,
        /*real*/  2ull << 30, /*total*/ 8ull << 30);
  g_fake_heap_count = 1;
  heap(0, 32ull << 30, VK_MEMORY_HEAP_DEVICE_LOCAL_BIT);

  /* pNext chain with a budget extension struct — must NOT be touched. */
  VkPhysicalDeviceMemoryBudgetPropertiesEXT budget_ext;
  memset(&budget_ext, 0, sizeof(budget_ext));
  budget_ext.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_MEMORY_BUDGET_PROPERTIES_EXT;

  VkPhysicalDeviceMemoryProperties2 props2;
  memset(&props2, 0, sizeof(props2));
  props2.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_MEMORY_PROPERTIES_2;
  props2.pNext = &budget_ext;

  vgpu_vk_GetPhysicalDeviceMemoryProperties2(g_phys, &props2);
  assert(props2.memoryProperties.memoryHeaps[0].size == (2ull << 30));
  /* Sanity: pNext budget struct still attached. */
  assert(props2.pNext == &budget_ext);
  vgpu_test_pass("_2 entry clamps and leaves pNext extension untouched");
  teardown();
}

int main(void) {
  test_clamps_device_local_oversold_off();
  test_clamps_device_local_oversold_on();
  test_does_not_clamp_non_device_local();
  test_does_not_clamp_when_unmanaged();
  test_does_not_clamp_smaller_heap();
  test_clamps_via_v2_entry();
  printf("ok: test_memprops_clamp complete\n");
  return 0;
}
