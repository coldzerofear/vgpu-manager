/*
 * hooks_alloc.c Phase 5b unit test — vk_alloc_is_import detection.
 *
 * The hook short-circuits vkAllocateMemory when pAllocateInfo->pNext
 * carries an "import existing external memory" structure with a
 * non-trivial handle. Verified here by checking that import calls:
 *   - skip vgpu_check_alloc_budget entirely (call count stays 0)
 *   - still forward to next-layer alloc (handle returned)
 *
 * Defense-in-depth: import structures with bogus handles (fd=-1 or
 * pHostPointer=NULL) MUST go through the normal budget path so they
 * cannot be used as a probing oracle.
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

static int g_alloc_calls = 0;

static VKAPI_ATTR VkResult VKAPI_CALL
fake_alloc(VkDevice d, const VkMemoryAllocateInfo *info,
           const VkAllocationCallbacks *cb, VkDeviceMemory *out) {
  (void)d; (void)cb;
  g_alloc_calls++;
  *out = (VkDeviceMemory)(uintptr_t)info->allocationSize;
  return VK_SUCCESS;
}

#define HOST_INDEX 2
static VkDevice         g_dev   = (VkDevice)        (uintptr_t)0xD200;
static VkPhysicalDevice g_phys  = (VkPhysicalDevice)(uintptr_t)0xD2FF;
static VkInstance       g_inst  = (VkInstance)      (uintptr_t)0xC2AA;

static const uint8_t g_uuid_bytes[16] = {
  0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
  0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00
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

static void setup(void) {
  vgpu_test_reset_all();
  g_alloc_calls = 0;

  snprintf(g_vgpu_config->devices[HOST_INDEX].uuid, UUID_BUFFER_SIZE,
           "GPU-11223344-5566-7788-99aa-bbccddeeff00");
  g_vgpu_config->devices[HOST_INDEX].memory_limit = 1;
  g_vgpu_config->devices[HOST_INDEX].real_memory  = 4ull << 30;
  g_vgpu_config->devices[HOST_INDEX].total_memory = 4ull << 30;

  vgpu_vk_register_instance_physdevs(g_inst, phys_gipa);

  vgpu_vk_device_dispatch_t dev_entry;
  memset(&dev_entry, 0, sizeof(dev_entry));
  dev_entry.device             = g_dev;
  dev_entry.physical_device    = g_phys;
  dev_entry.pfn_AllocateMemory = fake_alloc;
  vgpu_vk_register_device_dispatch(&dev_entry);
}

static void teardown(void) {
  vgpu_vk_remove_device_dispatch(g_dev);
  vgpu_vk_unregister_instance_physdevs(g_inst);
}

/* ---- Helper to drive one allocation with a given pNext chain --- */

static VkResult call_alloc_with_pnext(const void *pnext) {
  VkMemoryAllocateInfo info;
  memset(&info, 0, sizeof(info));
  info.sType          = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO;
  info.pNext          = pnext;
  info.allocationSize = 2ull << 30;
  VkDeviceMemory m;
  return vgpu_vk_AllocateMemory(g_dev, &info, NULL, &m);
}

/* ---- Tests ------------------------------------------------------ */

static void test_fd_import_short_circuits_budget(void) {
  setup();
  /* Make budget mock return OOM so we can prove the hook never reached
   * it: if the import detection works, we expect VK_SUCCESS despite
   * the OOM mock. */
  vgpu_test_budget_forced_path    = VGPU_PATH_OOM;
  vgpu_test_budget_lock_fd_to_set = 12;

  VkImportMemoryFdInfoKHR imp;
  memset(&imp, 0, sizeof(imp));
  imp.sType      = VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR;
  imp.handleType = VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD_BIT;
  imp.fd         = 42;

  assert(call_alloc_with_pnext(&imp) == VK_SUCCESS);
  assert(g_alloc_calls == 1);
  assert(vgpu_test_budget_call_count == 0);
  assert(vgpu_test_unlock_call_count == 0);
  vgpu_test_pass("VkImportMemoryFdInfoKHR(fd=42, type=OPAQUE_FD) short-circuits");
  teardown();
}

static void test_host_pointer_import_short_circuits_budget(void) {
  setup();
  vgpu_test_budget_forced_path    = VGPU_PATH_OOM;

  /* Use stack memory as a stand-in host pointer. */
  uint8_t buffer[16];
  VkImportMemoryHostPointerInfoEXT imp;
  memset(&imp, 0, sizeof(imp));
  imp.sType        = VK_STRUCTURE_TYPE_IMPORT_MEMORY_HOST_POINTER_INFO_EXT;
  imp.handleType   = VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT;
  imp.pHostPointer = buffer;

  assert(call_alloc_with_pnext(&imp) == VK_SUCCESS);
  assert(g_alloc_calls == 1);
  assert(vgpu_test_budget_call_count == 0);
  vgpu_test_pass("VkImportMemoryHostPointerInfoEXT short-circuits when valid");
  teardown();
}

static void test_fd_minus_one_does_not_short_circuit(void) {
  /* Defense-in-depth: a bogus import (fd=-1) must NOT short-circuit;
   * the request goes through normal budget enforcement. We force OOM
   * and assert VK_ERROR_OUT_OF_DEVICE_MEMORY (proving the budget path
   * was taken) and that the next-layer alloc was NOT called. */
  setup();
  vgpu_test_budget_forced_path    = VGPU_PATH_OOM;
  vgpu_test_budget_lock_fd_to_set = 22;

  VkImportMemoryFdInfoKHR imp;
  memset(&imp, 0, sizeof(imp));
  imp.sType      = VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR;
  imp.handleType = VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD_BIT;
  imp.fd         = -1;

  assert(call_alloc_with_pnext(&imp) == VK_ERROR_OUT_OF_DEVICE_MEMORY);
  assert(g_alloc_calls == 0);
  assert(vgpu_test_budget_call_count == 1);
  assert(vgpu_test_unlock_call_count == 1);
  vgpu_test_pass("fd=-1 falls through to budget (defense-in-depth)");
  teardown();
}

static void test_handle_type_zero_does_not_short_circuit(void) {
  setup();
  vgpu_test_budget_forced_path    = VGPU_PATH_OOM;
  vgpu_test_budget_lock_fd_to_set = 33;

  VkImportMemoryFdInfoKHR imp;
  memset(&imp, 0, sizeof(imp));
  imp.sType      = VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR;
  imp.handleType = 0;        /* invalid */
  imp.fd         = 5;        /* otherwise plausible */

  assert(call_alloc_with_pnext(&imp) == VK_ERROR_OUT_OF_DEVICE_MEMORY);
  assert(g_alloc_calls == 0);
  assert(vgpu_test_budget_call_count == 1);
  vgpu_test_pass("handleType=0 falls through to budget");
  teardown();
}

static void test_host_pointer_null_does_not_short_circuit(void) {
  setup();
  vgpu_test_budget_forced_path    = VGPU_PATH_OOM;
  vgpu_test_budget_lock_fd_to_set = 44;

  VkImportMemoryHostPointerInfoEXT imp;
  memset(&imp, 0, sizeof(imp));
  imp.sType        = VK_STRUCTURE_TYPE_IMPORT_MEMORY_HOST_POINTER_INFO_EXT;
  imp.handleType   = VK_EXTERNAL_MEMORY_HANDLE_TYPE_HOST_ALLOCATION_BIT_EXT;
  imp.pHostPointer = NULL;

  assert(call_alloc_with_pnext(&imp) == VK_ERROR_OUT_OF_DEVICE_MEMORY);
  assert(g_alloc_calls == 0);
  vgpu_test_pass("pHostPointer=NULL falls through to budget");
  teardown();
}

static void test_no_pnext_goes_through_budget(void) {
  setup();
  vgpu_test_budget_forced_path    = VGPU_PATH_GPU;
  vgpu_test_budget_lock_fd_to_set = 55;
  assert(call_alloc_with_pnext(NULL) == VK_SUCCESS);
  assert(g_alloc_calls == 1);
  assert(vgpu_test_budget_call_count == 1);
  vgpu_test_pass("no pNext (plain alloc) goes through budget");
  teardown();
}

static void test_export_struct_does_not_short_circuit(void) {
  /* VkExportMemoryAllocateInfo is *export*, not import. It is still
   * a new physical allocation and MUST go through budget. */
  setup();
  vgpu_test_budget_forced_path    = VGPU_PATH_GPU;
  vgpu_test_budget_lock_fd_to_set = 66;

  VkExportMemoryAllocateInfo exp;
  memset(&exp, 0, sizeof(exp));
  exp.sType       = VK_STRUCTURE_TYPE_EXPORT_MEMORY_ALLOCATE_INFO;
  exp.handleTypes = VK_EXTERNAL_MEMORY_HANDLE_TYPE_OPAQUE_FD_BIT;

  assert(call_alloc_with_pnext(&exp) == VK_SUCCESS);
  assert(g_alloc_calls == 1);
  assert(vgpu_test_budget_call_count == 1);
  vgpu_test_pass("VkExportMemoryAllocateInfo goes through budget (not import)");
  teardown();
}

int main(void) {
  test_fd_import_short_circuits_budget();
  test_host_pointer_import_short_circuits_budget();
  test_fd_minus_one_does_not_short_circuit();
  test_handle_type_zero_does_not_short_circuit();
  test_host_pointer_null_does_not_short_circuit();
  test_no_pnext_goes_through_budget();
  test_export_struct_does_not_short_circuit();
  printf("ok: test_alloc_import complete\n");
  return 0;
}
