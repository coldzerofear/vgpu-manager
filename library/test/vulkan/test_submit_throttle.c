/*
 * hooks_submit.c unit test — vkQueueSubmit + vkQueueSubmit2 throttle
 * + forward, plus vkGetDeviceQueue[2] queue registration.
 *
 * Verifies:
 *   - Submit hook calls vgpu_rate_limit_by_host_index(1, host_index)
 *     exactly once per submit, then forwards to the next-layer pfn.
 *   - QueueSubmit2 path follows the same shape.
 *   - Missing dispatch / unregistered queue => VK_ERROR_INITIALIZATION_FAILED.
 *   - GetDeviceQueue / _2 register the resulting (queue, device) pair.
 */
#include <assert.h>
#include <stdio.h>
#include <stdint.h>
#include <string.h>

#include <vulkan/vulkan.h>

#include "include/vulkan/dispatch.h"
#include "include/vulkan/physdev_index.h"
#include "include/vulkan/queue_index.h"
#include "include/vulkan/hooks_submit.h"

#include "test_helpers.h"

/* ---- Mock pfns -------------------------------------------------- */

static int g_submit1_calls = 0;
static int g_submit2_calls = 0;
static int g_getq1_calls   = 0;
static int g_getq2_calls   = 0;

static VKAPI_ATTR VkResult VKAPI_CALL
fake_submit(VkQueue q, uint32_t n, const VkSubmitInfo *si, VkFence f) {
  (void)q; (void)n; (void)si; (void)f;
  g_submit1_calls++;
  return VK_SUCCESS;
}

static VKAPI_ATTR VkResult VKAPI_CALL
fake_submit2(VkQueue q, uint32_t n, const VkSubmitInfo2 *si, VkFence f) {
  (void)q; (void)n; (void)si; (void)f;
  g_submit2_calls++;
  return VK_SUCCESS;
}

static VkQueue g_returned_queue;
static VKAPI_ATTR void VKAPI_CALL
fake_getq(VkDevice d, uint32_t fam, uint32_t idx, VkQueue *out) {
  (void)d; (void)fam; (void)idx;
  g_getq1_calls++;
  *out = g_returned_queue;
}

static VKAPI_ATTR void VKAPI_CALL
fake_getq2(VkDevice d, const VkDeviceQueueInfo2 *info, VkQueue *out) {
  (void)d; (void)info;
  g_getq2_calls++;
  *out = g_returned_queue;
}

/* ---- Setup ------------------------------------------------------ */

#define HOST_INDEX 7
static VkDevice         g_dev   = (VkDevice)        (uintptr_t)0xDDD0;
static VkPhysicalDevice g_phys  = (VkPhysicalDevice)(uintptr_t)0xDDDF;
static VkInstance       g_inst  = (VkInstance)      (uintptr_t)0xC1FF;

static const uint8_t g_uuid_bytes[16] = {
  0x77, 0x77, 0x77, 0x77, 0x77, 0x77, 0x77, 0x77,
  0x77, 0x77, 0x77, 0x77, 0x77, 0x77, 0x77, 0x77
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
phys_gipa(VkInstance i, const char *name) {
  (void)i;
  if (strcmp(name, "vkEnumeratePhysicalDevices") == 0)
    return (PFN_vkVoidFunction)phys_enum_one;
  if (strcmp(name, "vkGetPhysicalDeviceProperties2") == 0)
    return (PFN_vkVoidFunction)phys_props2_uuid;
  return NULL;
}

static void setup(void) {
  vgpu_test_reset_all();
  g_submit1_calls = 0;
  g_submit2_calls = 0;
  g_getq1_calls   = 0;
  g_getq2_calls   = 0;

  snprintf(g_vgpu_config->devices[HOST_INDEX].uuid, UUID_BUFFER_SIZE,
           "GPU-77777777-7777-7777-7777-777777777777");
  g_vgpu_config->devices[HOST_INDEX].activate     = 1;
  g_vgpu_config->devices[HOST_INDEX].memory_limit = 1;

  vgpu_vk_register_instance_physdevs(g_inst, phys_gipa);

  vgpu_vk_device_dispatch_t dev_entry;
  memset(&dev_entry, 0, sizeof(dev_entry));
  dev_entry.device              = g_dev;
  dev_entry.physical_device     = g_phys;
  dev_entry.pfn_QueueSubmit     = fake_submit;
  dev_entry.pfn_QueueSubmit2    = fake_submit2;
  dev_entry.pfn_GetDeviceQueue  = fake_getq;
  dev_entry.pfn_GetDeviceQueue2 = fake_getq2;
  vgpu_vk_register_device_dispatch(&dev_entry);
}

static void teardown(void) {
  vgpu_vk_remove_device_dispatch(g_dev);
  vgpu_vk_unregister_instance_physdevs(g_inst);
}

/* ---- Tests ------------------------------------------------------ */

static void test_submit_throttles_then_forwards(void) {
  setup();
  VkQueue q = (VkQueue)(uintptr_t)0x9001;
  vgpu_vk_register_queue(q, g_dev);

  VkResult r = vgpu_vk_QueueSubmit(q, 0, NULL, VK_NULL_HANDLE);
  assert(r == VK_SUCCESS);
  assert(g_submit1_calls == 1);
  assert(vgpu_test_throttle_call_count == 1);
  assert(vgpu_test_throttle_last_host_index == HOST_INDEX);
  assert(vgpu_test_throttle_last_kernel_size == 1);
  vgpu_test_pass("vkQueueSubmit: throttle(1, host_index) then forward");
  teardown();
}

static void test_submit2_throttles_then_forwards(void) {
  setup();
  VkQueue q = (VkQueue)(uintptr_t)0x9002;
  vgpu_vk_register_queue(q, g_dev);

  VkResult r = vgpu_vk_QueueSubmit2(q, 0, NULL, VK_NULL_HANDLE);
  assert(r == VK_SUCCESS);
  assert(g_submit2_calls == 1);
  assert(vgpu_test_throttle_call_count == 1);
  assert(vgpu_test_throttle_last_host_index == HOST_INDEX);
  vgpu_test_pass("vkQueueSubmit2: throttle then forward");
  teardown();
}

static void test_unregistered_queue_returns_init_failed(void) {
  setup();
  VkQueue qx = (VkQueue)(uintptr_t)0xDEADBEEF;
  /* Did NOT register qx. */
  VkResult r = vgpu_vk_QueueSubmit(qx, 0, NULL, VK_NULL_HANDLE);
  assert(r == VK_ERROR_INITIALIZATION_FAILED);
  assert(g_submit1_calls == 0);
  assert(vgpu_test_throttle_call_count == 0);
  vgpu_test_pass("unregistered queue => INITIALIZATION_FAILED, no throttle, no forward");
  teardown();
}

static void test_get_device_queue_registers_pair(void) {
  setup();
  VkQueue out = VK_NULL_HANDLE;
  g_returned_queue = (VkQueue)(uintptr_t)0x9100;
  vgpu_vk_GetDeviceQueue(g_dev, /*fam*/ 0, /*idx*/ 0, &out);
  assert(g_getq1_calls == 1);
  assert(out == g_returned_queue);
  /* Cache must now resolve out -> g_dev. */
  assert(vgpu_vk_queue_to_device(out) == g_dev);
  vgpu_test_pass("vkGetDeviceQueue forwards then registers (queue, device)");
  teardown();
}

static void test_get_device_queue2_registers_pair(void) {
  setup();
  VkQueue out = VK_NULL_HANDLE;
  g_returned_queue = (VkQueue)(uintptr_t)0x9200;
  VkDeviceQueueInfo2 info;
  memset(&info, 0, sizeof(info));
  info.sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_INFO_2;
  vgpu_vk_GetDeviceQueue2(g_dev, &info, &out);
  assert(g_getq2_calls == 1);
  assert(out == g_returned_queue);
  assert(vgpu_vk_queue_to_device(out) == g_dev);
  vgpu_test_pass("vkGetDeviceQueue2 forwards then registers (queue, device)");
  teardown();
}

int main(void) {
  test_submit_throttles_then_forwards();
  test_submit2_throttles_then_forwards();
  test_unregistered_queue_returns_init_failed();
  test_get_device_queue_registers_pair();
  test_get_device_queue2_registers_pair();
  printf("ok: test_submit_throttle complete\n");
  return 0;
}
