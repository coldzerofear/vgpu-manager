/*
 * VkPhysicalDevice -> host_index cache implementation.
 *
 * Storage: linked list of (phys, owner_instance, host_index) under a
 * pthread rwlock. Same shape as dispatch.c - a real process touches
 * one or two Vulkan instances and a handful of physical devices, so
 * O(N) lookup is fine and the data structure stays trivially correct.
 *
 * See physdev_index.h for the lifecycle contract.
 */
#include <pthread.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include <vulkan/vulkan.h>

#include "include/budget.h"   /* get_host_device_index_by_uuid_bytes */
#include "include/hook.h"     /* LOGGER */

#include "physdev_index.h"

typedef struct vgpu_vk_phys_node {
  VkPhysicalDevice           phys;
  VkInstance                 owner;
  int                        host_index;
  struct vgpu_vk_phys_node  *next;
} vgpu_vk_phys_node_t;

static vgpu_vk_phys_node_t *g_phys_cache = NULL;
static pthread_rwlock_t     g_phys_lock  = PTHREAD_RWLOCK_INITIALIZER;

/* Resolve a single physdev's UUID to host_index via the next-layer
 * GIPA. Returns -1 on any failure (non-NVIDIA device, pre-1.1 Vulkan
 * instance, UUID not in g_vgpu_config, etc.). */
static int resolve_phys_uuid(VkPhysicalDevice           phys,
                             VkInstance                 instance,
                             PFN_vkGetInstanceProcAddr  next_gipa) {
  /* vkGetPhysicalDeviceProperties2 was promoted to Vulkan 1.1 core; the
   * KHR-suffixed alias is the older extension entry point used by 1.0
   * instances that loaded VK_KHR_get_physical_device_properties2. NVIDIA
   * drivers expose at least 1.1 since 2017 so the KHR fallback is mostly
   * defensive. */
  PFN_vkGetPhysicalDeviceProperties2 pfn_gpdp2 =
      (PFN_vkGetPhysicalDeviceProperties2)
          next_gipa(instance, "vkGetPhysicalDeviceProperties2");
  if (pfn_gpdp2 == NULL) {
    pfn_gpdp2 = (PFN_vkGetPhysicalDeviceProperties2)
        next_gipa(instance, "vkGetPhysicalDeviceProperties2KHR");
  }
  if (pfn_gpdp2 == NULL) {
    LOGGER(VERBOSE,
           "vk physdev %p: GetPhysicalDeviceProperties2 unavailable "
           "(pre-1.1 instance without the KHR ext); cannot resolve UUID",
           (void *)phys);
    return -1;
  }

  VkPhysicalDeviceIDProperties id_props;
  memset(&id_props, 0, sizeof(id_props));
  id_props.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_ID_PROPERTIES;

  VkPhysicalDeviceProperties2 props2;
  memset(&props2, 0, sizeof(props2));
  props2.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROPERTIES_2;
  props2.pNext = &id_props;

  pfn_gpdp2(phys, &props2);

  int host_index = -1;
  get_host_device_index_by_uuid_bytes(id_props.deviceUUID, &host_index);
  return host_index;
}

void vgpu_vk_register_instance_physdevs(VkInstance                instance,
                                        PFN_vkGetInstanceProcAddr next_gipa) {
  if (instance == VK_NULL_HANDLE || next_gipa == NULL) {
    return;
  }

  PFN_vkEnumeratePhysicalDevices pfn_epd =
      (PFN_vkEnumeratePhysicalDevices)
          next_gipa(instance, "vkEnumeratePhysicalDevices");
  if (pfn_epd == NULL) {
    LOGGER(VERBOSE, "vk physdev register: vkEnumeratePhysicalDevices "
                    "not available on instance %p", (void *)instance);
    return;
  }

  uint32_t count = 0;
  if (pfn_epd(instance, &count, NULL) != VK_SUCCESS || count == 0) {
    return;
  }

  VkPhysicalDevice *phys_list =
      (VkPhysicalDevice *)calloc(count, sizeof(VkPhysicalDevice));
  if (phys_list == NULL) {
    LOGGER(ERROR, "vk physdev register: out of memory enumerating "
                  "%u physical devices on instance %p",
                  count, (void *)instance);
    return;
  }

  if (pfn_epd(instance, &count, phys_list) != VK_SUCCESS) {
    free(phys_list);
    return;
  }

  for (uint32_t i = 0; i < count; i++) {
    int host_index = resolve_phys_uuid(phys_list[i], instance, next_gipa);

    vgpu_vk_phys_node_t *node =
        (vgpu_vk_phys_node_t *)calloc(1, sizeof(*node));
    if (node == NULL) {
      LOGGER(ERROR, "vk physdev register: out of memory caching "
                    "phys %p", (void *)phys_list[i]);
      continue;
    }
    node->phys       = phys_list[i];
    node->owner      = instance;
    node->host_index = host_index;

    pthread_rwlock_wrlock(&g_phys_lock);
    node->next = g_phys_cache;
    g_phys_cache = node;
    pthread_rwlock_unlock(&g_phys_lock);

    if (host_index >= 0) {
      LOGGER(VERBOSE, "vk physical device %p (instance %p) => host device %d",
                      (void *)phys_list[i], (void *)instance, host_index);
    } else {
      LOGGER(DETAIL, "vk physical device %p (instance %p) => not tracked "
                     "(UUID not in vgpu_config)",
                     (void *)phys_list[i], (void *)instance);
    }
  }
  free(phys_list);
}

void vgpu_vk_unregister_instance_physdevs(VkInstance instance) {
  if (instance == VK_NULL_HANDLE) return;

  pthread_rwlock_wrlock(&g_phys_lock);
  vgpu_vk_phys_node_t **prev = &g_phys_cache;
  while (*prev != NULL) {
    if ((*prev)->owner == instance) {
      vgpu_vk_phys_node_t *target = *prev;
      *prev = target->next;
      free(target);
    } else {
      prev = &(*prev)->next;
    }
  }
  pthread_rwlock_unlock(&g_phys_lock);
}

int vgpu_vk_physdev_to_host_index(VkPhysicalDevice phys) {
  int result = -1;
  if (phys == VK_NULL_HANDLE) return -1;

  pthread_rwlock_rdlock(&g_phys_lock);
  for (vgpu_vk_phys_node_t *n = g_phys_cache; n != NULL; n = n->next) {
    if (n->phys == phys) {
      result = n->host_index;
      break;
    }
  }
  pthread_rwlock_unlock(&g_phys_lock);
  return result;
}
