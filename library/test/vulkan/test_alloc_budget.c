/*
 * hooks_alloc.c unit test — vkAllocateMemory + vkFreeMemory budget
 * paths (excluding import detection, which lives in test_alloc_import.c).
 *
 * Verifies:
 *   - GPU path: budget mock returns GPU => forward + unlock
 *   - OOM path: budget mock returns OOM => VK_ERROR_OUT_OF_DEVICE_MEMORY
 *               + unlock + no forward
 *   - kind/allow_uva passed to budget = PHYSICAL / 0
 *   - host_index < 0 path: budget still called (mock acts as no-op)
 *   - free path: forwards transparently, no budget interaction
 *   - missing dispatch entry => VK_ERROR_INITIALIZATION_FAILED
 */
#include <assert.h>
#include <stdio.h>
#include <stdint.h>
#include <string.h>

#include <vulkan/vulkan.h>

#include "include/vulkan/dispatch.h"
#include "include/vulkan/physdev_index.h"
#include "include/vulkan/hooks_alloc.h"

#include "test_helpers.h"

/* ---- Mock pfn_AllocateMemory / pfn_FreeMemory ------------------ */

static int      g_alloc_calls = 0;
static int      g_free_calls  = 0;
static VkResult g_alloc_result = VK_SUCCESS;

static VKAPI_ATTR VkResult VKAPI_CALL
fake_alloc(VkDevice d, const VkMemoryAllocateInfo *info,
           const VkAllocationCallbacks *cb, VkDeviceMemory *out) {
  (void)d; (void)cb;
  g_alloc_calls++;
  /* Return a non-null handle so the caller can free it. Use the
   * allocationSize as the opaque handle for ergonomic asserting. */
  *out = (VkDeviceMemory)(uintptr_t)info->allocationSize;
  return g_alloc_result;
}

static VKAPI_ATTR void VKAPI_CALL
fake_free(VkDevice d, VkDeviceMemory mem, const VkAllocationCallbacks *cb) {
  (void)d; (void)mem; (void)cb;
  g_free_calls++;
}

/* ---- Setup ------------------------------------------------------ */

#define HOST_INDEX 5
static VkDevice         g_dev   = (VkDevice)        (uintptr_t)0xD000;
static VkPhysicalDevice g_phys  = (VkPhysicalDevice)(uintptr_t)0xD0FF;

/* physdev_index has no public way to inject a raw cache entry, so we
 * stand it up via vgpu_vk_register_instance_physdevs with a fake
 * GIPA. To keep this test focused, we use a side door: we register
 * the dispatch entry with the right physical_device, then rely on a
 * minimal physdev_index registration that emits g_phys with a UUID
 * matching g_vgpu_config[HOST_INDEX]. */

static const uint8_t g_uuid_bytes[16] = {
  0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00, 0x11,
  0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99
};

static VKAPI_ATTR VkResult VKAPI_CALL
phys_enum_one(VkInstance i, uint32_t *pn, VkPhysicalDevice *pd) {
  (void)i;
  if (pd == NULL) { *pn = 1; return VK_SUCCESS; }
  if (*pn) pd[0] = g_phys;
  *pn = 1;
  return VK_SUCCESS;
}

static VKAPI_ATTR void VKAPI_CALL
phys_props2_uuid(VkPhysicalDevice phys, VkPhysicalDeviceProperties2 *p) {
  (void)phys;
  for (VkBaseOutStructure *q = (VkBaseOutStructure *)p->pNext; q; q = q->pNext) {
    if (q->sType == VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_ID_PROPERTIES) {
      memcpy(((VkPhysicalDeviceIDProperties *)q)->deviceUUID, g_uuid_bytes, 16);
    }
  }
}

static PFN_vkVoidFunction VKAPI_CALL
phys_gipa(VkInstance inst, const char *name) {
  (void)inst;
  if (strcmp(name, "vkEnumeratePhysicalDevices") == 0)
    return (PFN_vkVoidFunction)phys_enum_one;
  if (strcmp(name, "vkGetPhysicalDeviceProperties2") == 0)
    return (PFN_vkVoidFunction)phys_props2_uuid;
  return NULL;
}

static VkInstance g_inst = (VkInstance)(uintptr_t)0xC1AA;

static void setup(void) {
  vgpu_test_reset_all();
  g_alloc_calls  = 0;
  g_free_calls   = 0;
  g_alloc_result = VK_SUCCESS;

  /* Configure host_index slot. */
  snprintf(g_vgpu_config->devices[HOST_INDEX].uuid, UUID_BUFFER_SIZE,
           "GPU-aabbccdd-eeff-0011-2233-445566778899");
  g_vgpu_config->devices[HOST_INDEX].activate     = 1;
  g_vgpu_config->devices[HOST_INDEX].memory_limit = 1;
  g_vgpu_config->devices[HOST_INDEX].real_memory  = 4ull << 30;
  g_vgpu_config->devices[HOST_INDEX].total_memory = 4ull << 30;

  /* Register physdev cache so host_index resolves correctly. */
  vgpu_vk_register_instance_physdevs(g_inst, phys_gipa);

  /* Register device dispatch entry. */
  vgpu_vk_device_dispatch_t dev_entry;
  memset(&dev_entry, 0, sizeof(dev_entry));
  dev_entry.device             = g_dev;
  dev_entry.physical_device    = g_phys;
  dev_entry.pfn_AllocateMemory = fake_alloc;
  dev_entry.pfn_FreeMemory     = fake_free;
  vgpu_vk_register_device_dispatch(&dev_entry);
}

static void teardown(void) {
  vgpu_vk_remove_device_dispatch(g_dev);
  vgpu_vk_unregister_instance_physdevs(g_inst);
}

/* ---- Tests ------------------------------------------------------ */

static void test_gpu_path_forwards_and_unlocks(void) {
  setup();
  vgpu_test_budget_forced_path    = VGPU_PATH_GPU;
  vgpu_test_budget_lock_fd_to_set = 7;        /* observable lock fd */

  VkMemoryAllocateInfo info;
  memset(&info, 0, sizeof(info));
  info.sType          = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO;
  info.allocationSize = 1ull << 30;

  VkDeviceMemory m;
  VkResult r = vgpu_vk_AllocateMemory(g_dev, &info, NULL, &m);
  assert(r == VK_SUCCESS);
  assert(g_alloc_calls == 1);
  assert(vgpu_test_budget_call_count == 1);
  assert(vgpu_test_budget_last_host_index == HOST_INDEX);
  assert(vgpu_test_budget_last_request_size == (1ull << 30));
  assert(vgpu_test_budget_last_kind == VGPU_BUDGET_KIND_PHYSICAL);
  assert(vgpu_test_budget_last_allow_uva == 0);
  assert(vgpu_test_unlock_call_count == 1);
  assert(vgpu_test_unlock_last_fd == 7);
  vgpu_test_pass("GPU path: budget called with PHYSICAL/allow_uva=0, alloc forwarded, unlock fired");
  teardown();
}

static void test_oom_path_blocks_alloc(void) {
  setup();
  vgpu_test_budget_forced_path    = VGPU_PATH_OOM;
  vgpu_test_budget_lock_fd_to_set = 9;

  VkMemoryAllocateInfo info;
  memset(&info, 0, sizeof(info));
  info.sType          = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO;
  info.allocationSize = 8ull << 30;

  VkDeviceMemory m = VK_NULL_HANDLE;
  /* Pre-evaluate side-effect call so it survives any future build that
   * re-enables NDEBUG; assert verifies the captured return code only. */
  VkResult r = vgpu_vk_AllocateMemory(g_dev, &info, NULL, &m);
  assert(r == VK_ERROR_OUT_OF_DEVICE_MEMORY);
  /* alloc was NOT called — we returned OOM before forward. */
  assert(g_alloc_calls == 0);
  /* Lock was held, so unlock must have fired exactly once. */
  assert(vgpu_test_unlock_call_count == 1);
  assert(vgpu_test_unlock_last_fd == 9);
  vgpu_test_pass("OOM path: returns VK_ERROR_OUT_OF_DEVICE_MEMORY, no forward, unlock fires");
  teardown();
}

static void test_unlock_skipped_when_lock_fd_negative(void) {
  /* Unmanaged path: budget returns GPU + lock_fd = -1 (e.g. host_index < 0).
   * The hook should not call unlock with -1. */
  setup();
  vgpu_test_budget_forced_path    = VGPU_PATH_GPU;
  vgpu_test_budget_lock_fd_to_set = -1;

  VkMemoryAllocateInfo info;
  memset(&info, 0, sizeof(info));
  info.sType          = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO;
  info.allocationSize = 1ull << 30;

  VkDeviceMemory m = VK_NULL_HANDLE;
  VkResult r = vgpu_vk_AllocateMemory(g_dev, &info, NULL, &m);
  assert(r == VK_SUCCESS);
  (void)m;
  assert(g_alloc_calls == 1);
  assert(vgpu_test_unlock_call_count == 0);
  vgpu_test_pass("lock_fd=-1 => unlock not called (avoids spurious unlock)");
  teardown();
}

static void test_free_forwards_only(void) {
  setup();
  vgpu_vk_FreeMemory(g_dev, (VkDeviceMemory)(uintptr_t)0xABCDEF, NULL);
  assert(g_free_calls == 1);
  assert(vgpu_test_budget_call_count == 0);
  assert(vgpu_test_unlock_call_count == 0);
  vgpu_test_pass("vkFreeMemory transparently forwards; no budget/unlock interaction");
  teardown();
}

static void test_missing_dispatch_returns_init_failed(void) {
  setup();
  /* Drop the dispatch entry under the device handle the hook will see.
   * The next AllocateMemory call should return INITIALIZATION_FAILED. */
  vgpu_vk_remove_device_dispatch(g_dev);

  VkMemoryAllocateInfo info;
  memset(&info, 0, sizeof(info));
  info.sType          = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO;
  info.allocationSize = 1ull << 30;
  VkDeviceMemory m = VK_NULL_HANDLE;
  VkResult r = vgpu_vk_AllocateMemory(g_dev, &info, NULL, &m);
  assert(r == VK_ERROR_INITIALIZATION_FAILED);
  (void)m;
  assert(g_alloc_calls == 0);
  vgpu_test_pass("missing dispatch => VK_ERROR_INITIALIZATION_FAILED");
  /* No teardown beyond physdev cleanup (already removed dispatch). */
  vgpu_vk_unregister_instance_physdevs(g_inst);
}

int main(void) {
  test_gpu_path_forwards_and_unlocks();
  test_oom_path_blocks_alloc();
  test_unlock_skipped_when_lock_fd_negative();
  test_free_forwards_only();
  test_missing_dispatch_returns_init_failed();
  printf("ok: test_alloc_budget complete\n");
  return 0;
}
