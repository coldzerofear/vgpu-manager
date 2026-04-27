/*
 * VkQueue -> VkDevice cache implementation. See queue_index.h for the
 * lifecycle contract; storage shape is identical to dispatch.c —
 * linked list under a pthread_rwlock — for the same reasoning (small
 * cardinality, read-heavy access pattern).
 */
#include <pthread.h>
#include <stdlib.h>

#include "include/vulkan/queue_index.h"

typedef struct vgpu_vk_queue_node {
  VkQueue                       queue;
  VkDevice                      device;
  struct vgpu_vk_queue_node    *next;
} vgpu_vk_queue_node_t;

static vgpu_vk_queue_node_t *g_queue_cache = NULL;
static pthread_rwlock_t      g_queue_lock  = PTHREAD_RWLOCK_INITIALIZER;

void vgpu_vk_register_queue(VkQueue queue, VkDevice device) {
  if (queue == VK_NULL_HANDLE || device == VK_NULL_HANDLE) return;

  /* Skip if already registered. Vulkan lets an app call
   * vkGetDeviceQueue with the same family/index twice and get the
   * same VkQueue back; without this dedup we would grow the list each
   * time. */
  pthread_rwlock_rdlock(&g_queue_lock);
  for (vgpu_vk_queue_node_t *n = g_queue_cache; n != NULL; n = n->next) {
    if (n->queue == queue) {
      pthread_rwlock_unlock(&g_queue_lock);
      return;
    }
  }
  pthread_rwlock_unlock(&g_queue_lock);

  vgpu_vk_queue_node_t *node =
      (vgpu_vk_queue_node_t *)calloc(1, sizeof(*node));
  if (node == NULL) return;
  node->queue  = queue;
  node->device = device;

  pthread_rwlock_wrlock(&g_queue_lock);
  /* Re-check under the writer lock in case a concurrent register
   * already added this queue between our reader-lock release above
   * and the writer-lock acquire here. */
  for (vgpu_vk_queue_node_t *n = g_queue_cache; n != NULL; n = n->next) {
    if (n->queue == queue) {
      pthread_rwlock_unlock(&g_queue_lock);
      free(node);
      return;
    }
  }
  node->next = g_queue_cache;
  g_queue_cache = node;
  pthread_rwlock_unlock(&g_queue_lock);
}

VkDevice vgpu_vk_queue_to_device(VkQueue queue) {
  VkDevice result = VK_NULL_HANDLE;
  if (queue == VK_NULL_HANDLE) return VK_NULL_HANDLE;

  pthread_rwlock_rdlock(&g_queue_lock);
  for (vgpu_vk_queue_node_t *n = g_queue_cache; n != NULL; n = n->next) {
    if (n->queue == queue) {
      result = n->device;
      break;
    }
  }
  pthread_rwlock_unlock(&g_queue_lock);
  return result;
}

void vgpu_vk_unregister_queues_for_device(VkDevice device) {
  if (device == VK_NULL_HANDLE) return;

  pthread_rwlock_wrlock(&g_queue_lock);
  vgpu_vk_queue_node_t **prev = &g_queue_cache;
  while (*prev != NULL) {
    if ((*prev)->device == device) {
      vgpu_vk_queue_node_t *target = *prev;
      *prev = target->next;
      free(target);
    } else {
      prev = &(*prev)->next;
    }
  }
  pthread_rwlock_unlock(&g_queue_lock);
}
