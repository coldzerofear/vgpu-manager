/*
 * dispatch.c unit test — register / lookup / remove for both
 * VkInstance and VkDevice scopes.
 *
 * No mocks needed for the dispatch core itself; we just feed in fake
 * handles. PFN fields are NULL because this test does not invoke
 * any hook function.
 */
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>

#include <vulkan/vulkan.h>

#include "include/vulkan/dispatch.h"

#include "test_helpers.h"

static void test_instance_register_lookup(void) {
  VkInstance inst_a = (VkInstance)(uintptr_t)0xA1;
  VkInstance inst_b = (VkInstance)(uintptr_t)0xB2;

  /* Empty cache => lookup returns NULL. */
  assert(vgpu_vk_get_instance_dispatch(inst_a) == NULL);

  vgpu_vk_instance_dispatch_t entry_a;
  memset(&entry_a, 0, sizeof(entry_a));
  entry_a.instance = inst_a;
  vgpu_vk_register_instance_dispatch(&entry_a);

  vgpu_vk_instance_dispatch_t entry_b;
  memset(&entry_b, 0, sizeof(entry_b));
  entry_b.instance = inst_b;
  vgpu_vk_register_instance_dispatch(&entry_b);

  vgpu_vk_instance_dispatch_t *got_a = vgpu_vk_get_instance_dispatch(inst_a);
  vgpu_vk_instance_dispatch_t *got_b = vgpu_vk_get_instance_dispatch(inst_b);
  assert(got_a != NULL && got_a->instance == inst_a);
  assert(got_b != NULL && got_b->instance == inst_b);
  assert(got_a != got_b);

  vgpu_vk_remove_instance_dispatch(inst_a);
  assert(vgpu_vk_get_instance_dispatch(inst_a) == NULL);
  /* inst_b unaffected. */
  assert(vgpu_vk_get_instance_dispatch(inst_b) != NULL);

  /* remove non-registered: silent no-op. */
  vgpu_vk_remove_instance_dispatch(inst_a);
  assert(vgpu_vk_get_instance_dispatch(inst_b) != NULL);

  vgpu_vk_remove_instance_dispatch(inst_b);
  assert(vgpu_vk_get_instance_dispatch(inst_b) == NULL);

  vgpu_test_pass("instance dispatch register/lookup/remove");
}

static void test_device_register_lookup(void) {
  VkDevice dev_a = (VkDevice)(uintptr_t)0xD1;
  VkDevice dev_b = (VkDevice)(uintptr_t)0xD2;

  assert(vgpu_vk_get_device_dispatch(dev_a) == NULL);

  VkPhysicalDevice phys_a = (VkPhysicalDevice)(uintptr_t)0x10A;
  VkPhysicalDevice phys_b = (VkPhysicalDevice)(uintptr_t)0x10B;

  vgpu_vk_device_dispatch_t entry_a;
  memset(&entry_a, 0, sizeof(entry_a));
  entry_a.device          = dev_a;
  entry_a.physical_device = phys_a;
  vgpu_vk_register_device_dispatch(&entry_a);

  vgpu_vk_device_dispatch_t entry_b;
  memset(&entry_b, 0, sizeof(entry_b));
  entry_b.device          = dev_b;
  entry_b.physical_device = phys_b;
  vgpu_vk_register_device_dispatch(&entry_b);

  vgpu_vk_device_dispatch_t *got_a = vgpu_vk_get_device_dispatch(dev_a);
  assert(got_a != NULL);
  assert(got_a->device == dev_a);
  assert(got_a->physical_device == phys_a);

  vgpu_vk_remove_device_dispatch(dev_a);
  assert(vgpu_vk_get_device_dispatch(dev_a) == NULL);
  assert(vgpu_vk_get_device_dispatch(dev_b) != NULL);

  vgpu_vk_remove_device_dispatch(dev_b);
  vgpu_test_pass("device dispatch register/lookup/remove");
}

static void test_register_null_entry_is_safe(void) {
  /* dispatch.c skips NULL gracefully — sanity-check. */
  vgpu_vk_register_instance_dispatch(NULL);
  vgpu_vk_register_device_dispatch(NULL);
  vgpu_test_pass("NULL entry register is safe");
}

int main(void) {
  vgpu_test_reset_all();
  test_instance_register_lookup();
  test_device_register_lookup();
  test_register_null_entry_is_safe();
  printf("ok: test_dispatch complete\n");
  return 0;
}
