/*
 * Per-VkInstance / per-VkDevice dispatch table storage.
 *
 * Implementation note: linked list under a pthread rwlock. A real Vulkan
 * application has at most a handful of VkInstance and VkDevice objects in
 * its lifetime (typically 1 of each), so O(N) lookup is fine and the data
 * structure stays trivially correct. If profiling ever shows this in a
 * hot path we can switch to a chained hash without touching the public
 * API in dispatch.h.
 *
 * Locking: rwlock so multiple concurrent hooks can read in parallel
 * (the common case) while register / remove (rare, only at create /
 * destroy) take the writer lock briefly.
 */
#include <pthread.h>
#include <stdlib.h>
#include <string.h>

#include "dispatch.h"

typedef struct vgpu_instance_node {
  vgpu_instance_dispatch_t   entry;
  struct vgpu_instance_node *next;
} vgpu_instance_node_t;

typedef struct vgpu_device_node {
  vgpu_device_dispatch_t   entry;
  struct vgpu_device_node *next;
} vgpu_device_node_t;

static vgpu_instance_node_t *g_instances = NULL;
static vgpu_device_node_t   *g_devices   = NULL;
static pthread_rwlock_t g_instance_lock = PTHREAD_RWLOCK_INITIALIZER;
static pthread_rwlock_t g_device_lock   = PTHREAD_RWLOCK_INITIALIZER;

vgpu_instance_dispatch_t *vgpu_get_instance_dispatch(VkInstance instance) {
  vgpu_instance_dispatch_t *result = NULL;
  pthread_rwlock_rdlock(&g_instance_lock);
  for (vgpu_instance_node_t *n = g_instances; n != NULL; n = n->next) {
    if (n->entry.instance == instance) {
      result = &n->entry;
      break;
    }
  }
  pthread_rwlock_unlock(&g_instance_lock);
  return result;
}

vgpu_device_dispatch_t *vgpu_get_device_dispatch(VkDevice device) {
  vgpu_device_dispatch_t *result = NULL;
  pthread_rwlock_rdlock(&g_device_lock);
  for (vgpu_device_node_t *n = g_devices; n != NULL; n = n->next) {
    if (n->entry.device == device) {
      result = &n->entry;
      break;
    }
  }
  pthread_rwlock_unlock(&g_device_lock);
  return result;
}

void vgpu_register_instance(const vgpu_instance_dispatch_t *entry) {
  if (entry == NULL) return;
  vgpu_instance_node_t *node = (vgpu_instance_node_t *)calloc(1, sizeof(*node));
  if (node == NULL) return;
  node->entry = *entry;

  pthread_rwlock_wrlock(&g_instance_lock);
  node->next = g_instances;
  g_instances = node;
  pthread_rwlock_unlock(&g_instance_lock);
}

void vgpu_register_device(const vgpu_device_dispatch_t *entry) {
  if (entry == NULL) return;
  vgpu_device_node_t *node = (vgpu_device_node_t *)calloc(1, sizeof(*node));
  if (node == NULL) return;
  node->entry = *entry;

  pthread_rwlock_wrlock(&g_device_lock);
  node->next = g_devices;
  g_devices = node;
  pthread_rwlock_unlock(&g_device_lock);
}

void vgpu_remove_instance(VkInstance instance) {
  pthread_rwlock_wrlock(&g_instance_lock);
  vgpu_instance_node_t **prev = &g_instances;
  while (*prev != NULL) {
    if ((*prev)->entry.instance == instance) {
      vgpu_instance_node_t *target = *prev;
      *prev = target->next;
      free(target);
      break;
    }
    prev = &(*prev)->next;
  }
  pthread_rwlock_unlock(&g_instance_lock);
}

void vgpu_remove_device(VkDevice device) {
  pthread_rwlock_wrlock(&g_device_lock);
  vgpu_device_node_t **prev = &g_devices;
  while (*prev != NULL) {
    if ((*prev)->entry.device == device) {
      vgpu_device_node_t *target = *prev;
      *prev = target->next;
      free(target);
      break;
    }
    prev = &(*prev)->next;
  }
  pthread_rwlock_unlock(&g_device_lock);
}
