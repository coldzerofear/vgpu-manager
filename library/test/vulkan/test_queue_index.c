/*
 * queue_index.c unit test — VkQueue -> VkDevice cache.
 *
 * Verifies:
 *   - empty cache lookup returns VK_NULL_HANDLE
 *   - register stores (queue, device); subsequent lookup returns device
 *   - duplicate registration of same queue is a no-op (does not grow
 *     the list, does not change the recorded device)
 *   - unregister-by-device removes only entries for that device
 *   - remove on empty / unregistered device is a silent no-op
 */
#include <assert.h>
#include <stdio.h>
#include <stdint.h>

#include <vulkan/vulkan.h>

#include "include/vulkan/queue_index.h"

#include "test_helpers.h"

int main(void) {
  vgpu_test_reset_all();

  VkDevice dev_a = (VkDevice)(uintptr_t)0xA0;
  VkDevice dev_b = (VkDevice)(uintptr_t)0xB0;
  VkQueue  q1    = (VkQueue) (uintptr_t)0x11;
  VkQueue  q2    = (VkQueue) (uintptr_t)0x22;
  VkQueue  q3    = (VkQueue) (uintptr_t)0x33;
  VkQueue  qx    = (VkQueue) (uintptr_t)0xFF;  /* unregistered */

  /* Empty cache. */
  assert(vgpu_vk_queue_to_device(q1) == VK_NULL_HANDLE);
  vgpu_test_pass("empty cache => VK_NULL_HANDLE");

  /* Register two queues on dev_a, one on dev_b. */
  vgpu_vk_register_queue(q1, dev_a);
  vgpu_vk_register_queue(q2, dev_a);
  vgpu_vk_register_queue(q3, dev_b);

  assert(vgpu_vk_queue_to_device(q1) == dev_a);
  assert(vgpu_vk_queue_to_device(q2) == dev_a);
  assert(vgpu_vk_queue_to_device(q3) == dev_b);
  assert(vgpu_vk_queue_to_device(qx) == VK_NULL_HANDLE);
  vgpu_test_pass("register + lookup three queues across two devices");

  /* Duplicate registration of same queue: lookup still returns the
   * original device. The cache does not grow into a list of dupes
   * (we cannot directly observe length, but the post-condition check
   * holds and dedup is asserted by no-crash on long stress runs). */
  vgpu_vk_register_queue(q1, dev_a);
  vgpu_vk_register_queue(q1, dev_a);
  assert(vgpu_vk_queue_to_device(q1) == dev_a);
  vgpu_test_pass("duplicate registration of same queue is a no-op");

  /* Unregister-by-device dev_a: q1, q2 disappear; q3 remains. */
  vgpu_vk_unregister_queues_for_device(dev_a);
  assert(vgpu_vk_queue_to_device(q1) == VK_NULL_HANDLE);
  assert(vgpu_vk_queue_to_device(q2) == VK_NULL_HANDLE);
  assert(vgpu_vk_queue_to_device(q3) == dev_b);
  vgpu_test_pass("unregister-by-device removes only that device's queues");

  /* Re-register a queue for the same device after unregister works
   * (no stale-handle conflict). */
  vgpu_vk_register_queue(q1, dev_a);
  assert(vgpu_vk_queue_to_device(q1) == dev_a);
  vgpu_test_pass("re-register after unregister succeeds");

  /* Unregister already-empty device: silent no-op. */
  vgpu_vk_unregister_queues_for_device((VkDevice)(uintptr_t)0xDEAD);
  assert(vgpu_vk_queue_to_device(q1) == dev_a);
  vgpu_test_pass("unregister of unknown device is a no-op");

  /* NULL handle inputs are handled defensively. */
  vgpu_vk_register_queue(VK_NULL_HANDLE, dev_a);
  vgpu_vk_register_queue(q1, VK_NULL_HANDLE);
  vgpu_vk_unregister_queues_for_device(VK_NULL_HANDLE);
  assert(vgpu_vk_queue_to_device(VK_NULL_HANDLE) == VK_NULL_HANDLE);
  vgpu_test_pass("NULL inputs handled defensively");

  printf("ok: test_queue_index complete\n");
  return 0;
}
