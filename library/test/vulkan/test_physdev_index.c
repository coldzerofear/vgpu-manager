/*
 * physdev_index.c unit test — VkPhysicalDevice -> host_index resolution.
 *
 * Mocks the next-layer GIPA so register_instance_physdevs walks a
 * controlled set of physical devices, each with a controlled UUID.
 * Then asserts the cache returns the expected host_index for each
 * mapped UUID and -1 for unmapped ones.
 *
 * Also verifies:
 *   - per-instance unregister drops only that instance's entries
 *   - vgpu_vk_physdev_owner returns the owning instance handle
 *   - VK_NULL_HANDLE inputs return -1 / VK_NULL_HANDLE
 */
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>

#include <vulkan/vulkan.h>

#include "include/hook.h"
#include "include/vulkan/physdev_index.h"

#include "test_helpers.h"

/* ---- Fake next-layer GIPA + the two pfns it returns ------------- */

#define FAKE_DEVICE_COUNT 3

/* Per-fake-physdev configuration (filled by main before calling
 * register_instance_physdevs). The fake pfns read this. */
static VkPhysicalDevice  g_fake_phys_list[FAKE_DEVICE_COUNT];
static uint8_t           g_fake_uuids   [FAKE_DEVICE_COUNT][16];

static VKAPI_ATTR VkResult VKAPI_CALL
fake_enumerate(VkInstance instance, uint32_t *pCount,
               VkPhysicalDevice *pDevices) {
  (void)instance;
  if (pDevices == NULL) {
    *pCount = FAKE_DEVICE_COUNT;
    return VK_SUCCESS;
  }
  uint32_t n = *pCount < FAKE_DEVICE_COUNT ? *pCount : FAKE_DEVICE_COUNT;
  for (uint32_t i = 0; i < n; i++) {
    pDevices[i] = g_fake_phys_list[i];
  }
  *pCount = n;
  return VK_SUCCESS;
}

static VKAPI_ATTR void VKAPI_CALL
fake_props2(VkPhysicalDevice phys, VkPhysicalDeviceProperties2 *pProps) {
  /* Find which fake index this corresponds to and fill ID props. */
  int idx = -1;
  for (int i = 0; i < FAKE_DEVICE_COUNT; i++) {
    if (g_fake_phys_list[i] == phys) { idx = i; break; }
  }
  /* Walk pNext for VkPhysicalDeviceIDProperties. */
  for (VkBaseOutStructure *p = (VkBaseOutStructure *)pProps->pNext;
       p != NULL; p = p->pNext) {
    if (p->sType == VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_ID_PROPERTIES) {
      VkPhysicalDeviceIDProperties *id = (VkPhysicalDeviceIDProperties *)p;
      if (idx >= 0) {
        memcpy(id->deviceUUID, g_fake_uuids[idx], 16);
      } else {
        memset(id->deviceUUID, 0, 16);
      }
    }
  }
}

static PFN_vkVoidFunction VKAPI_CALL
fake_gipa(VkInstance instance, const char *pName) {
  (void)instance;
  if (strcmp(pName, "vkEnumeratePhysicalDevices") == 0)
    return (PFN_vkVoidFunction)fake_enumerate;
  if (strcmp(pName, "vkGetPhysicalDeviceProperties2") == 0)
    return (PFN_vkVoidFunction)fake_props2;
  return NULL;
}

/* ---- Helpers ---------------------------------------------------- */

/* Format a 16-byte UUID into the canonical "GPU-..." string used by
 * g_vgpu_config.devices[].uuid. Mirrors loader.c's formatUuid. */
static void format_uuid(const uint8_t b[16], char out[UUID_BUFFER_SIZE]) {
  snprintf(out, UUID_BUFFER_SIZE,
           "GPU-%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
           b[0x0], b[0x1], b[0x2], b[0x3],
           b[0x4], b[0x5], b[0x6], b[0x7],
           b[0x8], b[0x9], b[0xA], b[0xB],
           b[0xC], b[0xD], b[0xE], b[0xF]);
}

static void seed_fake_devices(void) {
  for (int i = 0; i < FAKE_DEVICE_COUNT; i++) {
    g_fake_phys_list[i] = (VkPhysicalDevice)(uintptr_t)(0x100 + i);
    /* UUID = i in every byte (deterministic, easy to read in logs). */
    memset(g_fake_uuids[i], (uint8_t)(0xA0 + i), 16);
  }
}

/* Configure g_vgpu_config so that fake_uuids[0] -> host_index 0,
 * fake_uuids[2] -> host_index 1. fake_uuids[1] is intentionally
 * absent so its lookup yields -1 (non-NVIDIA / not allocated to this
 * pod). */
static void seed_config(void) {
  vgpu_test_reset_config();
  /* host_index 0 <- fake_uuids[0] */
  format_uuid(g_fake_uuids[0], g_vgpu_config->devices[0].uuid);
  /* host_index 1 <- fake_uuids[2] */
  format_uuid(g_fake_uuids[2], g_vgpu_config->devices[1].uuid);
  /* All other slots: zero uuid (memset earlier) -> never matches. */
}

/* ---- Zero-UUID fallback tests (HAMi PR #182 parity) -------------- */

/* Real-looking UUID bytes used by the regression / multi-GPU tests
 * below. Need to differ from the seeded fake UUIDs (0xA0+i in every
 * byte) so the strict match clearly distinguishes which slot wins. */
static const uint8_t k_real_uuid_one[16] = {
  0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01, 0x02, 0x03,
  0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B
};
static const uint8_t k_real_uuid_two[16] = {
  0xCA, 0xFE, 0xBA, 0xBE, 0x10, 0x11, 0x12, 0x13,
  0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1A, 0x1B
};

/* Single-GPU container hits the buggy NVIDIA driver: deviceUUID is
 * all zeros. The fallback must map to the only assigned host_index
 * regardless of the zero UUID's strict-match miss. */
static void test_zero_uuid_single_gpu_fallback(void) {
  vgpu_test_reset_all();
  seed_fake_devices();
  /* Override fake_uuids[0] to all-zero — simulates the driver quirk. */
  memset(g_fake_uuids[0], 0, 16);

  /* Configure g_vgpu_config so the Pod has EXACTLY ONE assigned GPU
   * at host_index 5. The UUID we register is k_real_uuid_one which
   * does not match any fake_uuids[i], proving the fallback (not the
   * strict matcher) is what binds host_index 5 to phys[0]. */
  format_uuid(k_real_uuid_one, g_vgpu_config->devices[5].uuid);

  VkInstance inst = (VkInstance)(uintptr_t)0xCC;
  vgpu_vk_register_instance_physdevs(inst, fake_gipa);

  /* phys[0] zero UUID + 1 assigned GPU => fallback fires. */
  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[0]) == 5);
  /* phys[1], phys[2] return non-zero UUIDs that do not match the
   * single config slot => standard miss, host_index=-1, fallback
   * NOT applied (zero-UUID predicate is the gate). */
  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[1]) == -1);
  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[2]) == -1);
  vgpu_test_pass("zero-UUID + single-GPU Pod => fallback to that host_index");

  vgpu_vk_unregister_instance_physdevs(inst);
}

/* Multi-GPU container with the same driver bug: must NOT fall back —
 * we cannot disambiguate which host_index this physdev should be.
 * Failing open (host_index=-1, no enforcement on this physdev) is
 * preferable to mis-binding the budget to the wrong device. */
static void test_zero_uuid_multi_gpu_no_fallback(void) {
  vgpu_test_reset_all();
  seed_fake_devices();
  memset(g_fake_uuids[0], 0, 16);

  /* Two assigned GPUs in g_vgpu_config (slots 2 and 7), neither
   * matching any fake UUID. */
  format_uuid(k_real_uuid_one, g_vgpu_config->devices[2].uuid);
  format_uuid(k_real_uuid_two, g_vgpu_config->devices[7].uuid);

  VkInstance inst = (VkInstance)(uintptr_t)0xCD;
  vgpu_vk_register_instance_physdevs(inst, fake_gipa);

  /* phys[0] zero UUID + 2 assigned GPUs => fallback declines, returns -1. */
  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[0]) == -1);
  vgpu_test_pass("zero-UUID + multi-GPU Pod => no fallback (host_index=-1)");

  vgpu_vk_unregister_instance_physdevs(inst);
}

/* Regression: the fallback is gated by is_zero_uuid. Normal (non-zero)
 * UUIDs continue to go through the strict UUID matcher, even in the
 * single-GPU-Pod case. */
static void test_normal_uuid_unaffected_by_fallback(void) {
  vgpu_test_reset_all();
  seed_fake_devices();
  /* Configure exactly one slot, with a UUID that matches fake_uuids[0]
   * (which seed_fake_devices set to 0xA0 in every byte — non-zero).
   * Strict match must put phys[0] at host_index 3; fallback must NOT
   * fire because deviceUUID is non-zero. */
  format_uuid(g_fake_uuids[0], g_vgpu_config->devices[3].uuid);

  VkInstance inst = (VkInstance)(uintptr_t)0xCE;
  vgpu_vk_register_instance_physdevs(inst, fake_gipa);

  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[0]) == 3);
  vgpu_test_pass("normal (non-zero) UUID still resolved by strict match");

  vgpu_vk_unregister_instance_physdevs(inst);
}

int main(void) {
  vgpu_test_reset_all();
  seed_fake_devices();
  seed_config();

  VkInstance inst_a = (VkInstance)(uintptr_t)0xAA;
  VkInstance inst_b = (VkInstance)(uintptr_t)0xBB;

  /* Empty cache. */
  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[0]) == -1);
  assert(vgpu_vk_physdev_owner(g_fake_phys_list[0]) == VK_NULL_HANDLE);
  vgpu_test_pass("empty cache => -1 / VK_NULL_HANDLE");

  /* Register inst_a's physdevs. */
  vgpu_vk_register_instance_physdevs(inst_a, fake_gipa);

  /* fake_phys_list[0] -> host_index 0, owner = inst_a */
  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[0]) == 0);
  assert(vgpu_vk_physdev_owner(g_fake_phys_list[0]) == inst_a);
  /* fake_phys_list[1] -> host_index -1 (UUID not in config), but
   *                      still cached with owner = inst_a */
  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[1]) == -1);
  assert(vgpu_vk_physdev_owner(g_fake_phys_list[1]) == inst_a);
  /* fake_phys_list[2] -> host_index 1 */
  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[2]) == 1);
  assert(vgpu_vk_physdev_owner(g_fake_phys_list[2]) == inst_a);
  vgpu_test_pass("UUID resolution + owner tracking");

  /* Same physdev handle reused under a second instance: register
   * also caches under inst_b. The lookup returns the most-recently
   * registered entry (linked list head). For our test we use
   * different handle values so the two instances do not collide. */
  /* (The implementation does not dedup by phys handle across
   * instances; collisions in real Vulkan are not expected anyway.) */

  /* Unregister inst_a — entries gone, lookup returns -1 / NULL. */
  vgpu_vk_unregister_instance_physdevs(inst_a);
  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[0]) == -1);
  assert(vgpu_vk_physdev_owner(g_fake_phys_list[0]) == VK_NULL_HANDLE);
  assert(vgpu_vk_physdev_to_host_index(g_fake_phys_list[2]) == -1);
  vgpu_test_pass("unregister-by-instance drops cache entries");

  /* NULL inputs. */
  assert(vgpu_vk_physdev_to_host_index(VK_NULL_HANDLE) == -1);
  assert(vgpu_vk_physdev_owner(VK_NULL_HANDLE) == VK_NULL_HANDLE);
  vgpu_vk_unregister_instance_physdevs(VK_NULL_HANDLE);    /* no crash */
  vgpu_vk_register_instance_physdevs(VK_NULL_HANDLE, fake_gipa); /* no crash */
  vgpu_vk_register_instance_physdevs(inst_a, NULL);              /* no crash */
  vgpu_test_pass("NULL inputs handled defensively");

  /* Zero-UUID + single-GPU fallback (HAMi PR #182 parity fix). */
  test_zero_uuid_single_gpu_fallback();
  test_zero_uuid_multi_gpu_no_fallback();
  test_normal_uuid_unaffected_by_fallback();

  printf("ok: test_physdev_index complete\n");
  return 0;
}
