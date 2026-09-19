/*
Copyright 2024-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

#include <assert.h>

#include "include/hook.h"
#include "include/nvml-helper.h"

extern entry_t nvml_library_entry[];
extern resource_data_t* g_vgpu_config;

extern int lock_gpu_device(int host_index);
extern void unlock_gpu_device(int fd);
extern int get_nvml_device_index_by_host_index(int host_index);

nvmlReturn_t nvmlInit(void);
nvmlReturn_t nvmlInit_v2(void);
nvmlReturn_t nvmlInitWithFlags(unsigned int flags);
nvmlReturn_t nvmlDeviceGetMemoryInfo(nvmlDevice_t device, nvmlMemory_t *memory);
nvmlReturn_t nvmlDeviceGetMemoryInfo_v2(nvmlDevice_t device, nvmlMemory_v2_t *memory);
nvmlReturn_t nvmlDeviceSetComputeMode(nvmlDevice_t device, nvmlComputeMode_t mode);
nvmlReturn_t nvmlDeviceGetPersistenceMode(nvmlDevice_t device, nvmlEnableState_t *mode);
nvmlReturn_t nvmlDeviceGetCount(unsigned int *deviceCount);
nvmlReturn_t nvmlDeviceGetCount_v2(unsigned int *deviceCount);
nvmlReturn_t nvmlDeviceGetHandleByIndex(unsigned int index, nvmlDevice_t *device);
nvmlReturn_t nvmlDeviceGetHandleByIndex_v2(unsigned int index, nvmlDevice_t *device);
nvmlReturn_t nvmlDeviceGetHandleByUUID(const char *uuid, nvmlDevice_t *device);
nvmlReturn_t nvmlDeviceGetHandleByPciBusId(const char *pciBusId, nvmlDevice_t *device);
nvmlReturn_t nvmlDeviceGetHandleByPciBusId_v2(const char *pciBusId, nvmlDevice_t *device);
nvmlReturn_t nvmlDeviceGetIndex(nvmlDevice_t device, unsigned int *index);
nvmlReturn_t nvmlDeviceGetHandleBySerial(const char *serial, nvmlDevice_t *device);

entry_t nvml_hooks_entry[] = {
    {.name = "nvmlInit", .fn_ptr = nvmlInit},
    {.name = "nvmlInit_v2", .fn_ptr = nvmlInit_v2},
    {.name = "nvmlInitWithFlags", .fn_ptr = nvmlInitWithFlags},
    {.name = "nvmlDeviceGetMemoryInfo", .fn_ptr = nvmlDeviceGetMemoryInfo},
    {.name = "nvmlDeviceGetMemoryInfo_v2", .fn_ptr = nvmlDeviceGetMemoryInfo_v2},
    {.name = "nvmlDeviceSetComputeMode", .fn_ptr = nvmlDeviceSetComputeMode},
    {.name = "nvmlDeviceGetPersistenceMode", .fn_ptr = nvmlDeviceGetPersistenceMode},
    {.name = "nvmlDeviceGetCount", .fn_ptr = nvmlDeviceGetCount},
    {.name = "nvmlDeviceGetCount_v2", .fn_ptr = nvmlDeviceGetCount_v2},
    {.name = "nvmlDeviceGetHandleByIndex", .fn_ptr = nvmlDeviceGetHandleByIndex},
    {.name = "nvmlDeviceGetHandleByIndex_v2", .fn_ptr = nvmlDeviceGetHandleByIndex_v2},
    {.name = "nvmlDeviceGetHandleByUUID", .fn_ptr = nvmlDeviceGetHandleByUUID},
    {.name = "nvmlDeviceGetHandleByPciBusId", .fn_ptr = nvmlDeviceGetHandleByPciBusId},
    {.name = "nvmlDeviceGetHandleByPciBusId_v2", .fn_ptr = nvmlDeviceGetHandleByPciBusId_v2},
    {.name = "nvmlDeviceGetIndex", .fn_ptr = nvmlDeviceGetIndex},
    {.name = "nvmlDeviceGetHandleBySerial", .fn_ptr = nvmlDeviceGetHandleBySerial},
};

const int nvml_hook_nums = sizeof(nvml_hooks_entry) / sizeof(nvml_hooks_entry[0]);

nvmlReturn_t nvmlInitWithFlags(unsigned int flags) {
  load_necessary_data();
  return NVML_ENTRY_CHECK(nvml_library_entry, nvmlInitWithFlags, flags);
}

nvmlReturn_t nvmlInit_v2(void) {
  load_necessary_data();
  return NVML_ENTRY_CHECK(nvml_library_entry, nvmlInit_v2);
}

nvmlReturn_t nvmlInit(void) {
  load_necessary_data();
  return NVML_ENTRY_CHECK(nvml_library_entry, nvmlInit);
}

nvmlReturn_t nvmlDeviceGetMemoryInfo(nvmlDevice_t device, nvmlMemory_t *memory) {
  /* Ask NVML first, then override. The previous form returned NVML_SUCCESS from
   * the limited branch without ever calling NVML, so a device NVML would have
   * refused was reported as healthy with a configured size. Calling first also
   * hands NULL-argument policy back to NVML instead of us having to guess it.
   * This is what _v2 below already did; v1 is now aligned with it. */
  nvmlReturn_t ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetMemoryInfo, device, memory);
  if (unlikely(ret != NVML_SUCCESS)) {
    return ret;
  }
  /* Before the lookup, not after it: get_host_device_index_by_nvml_device falls
   * through to get_host_device_index_by_uuid on a cold cache, and that walks
   * g_vgpu_config->devices[] -- so the FIRST call is precisely the one that
   * would dereference it unset. Same ordering as the CUDA hooks. */
  load_necessary_data();
  int host_index = get_host_device_index_by_nvml_device(device);
  if (host_index < 0) {
    return ret;
  }
  device_t dsnap = get_device_snapshot(host_index);
  if (dsnap.memory_limit && memory != NULL) {
    int lock_fd = lock_gpu_device(host_index);

    size_t used = 0, vmem_used = 0;
    get_used_gpu_memory_by_device((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    size_t total_memory = dsnap.total_memory;
    memory->total = total_memory;
    memory->used = (used + vmem_used) >= total_memory ? total_memory : (used + vmem_used);
    memory->free = memory->total - memory->used;

    unlock_gpu_device(lock_fd);
  }
  return ret;
}

nvmlReturn_t nvmlDeviceGetMemoryInfo_v2(nvmlDevice_t device, nvmlMemory_v2_t *memory) {
  nvmlReturn_t ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetMemoryInfo_v2, device, memory);
  if (unlikely(ret != NVML_SUCCESS)) {
    return ret;
  }
  /* Before the lookup, not after it: get_host_device_index_by_nvml_device falls
   * through to get_host_device_index_by_uuid on a cold cache, and that walks
   * g_vgpu_config->devices[] -- so the FIRST call is precisely the one that
   * would dereference it unset. Same ordering as the CUDA hooks. */
  load_necessary_data();
  int host_index = get_host_device_index_by_nvml_device(device);
  if (host_index < 0) {
    return ret;
  }
  device_t dsnap = get_device_snapshot(host_index);
  if (dsnap.memory_limit && memory != NULL) {
    int lock_fd = lock_gpu_device(host_index);

    size_t used = 0, vmem_used = 0;
    get_used_gpu_memory_by_device((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    size_t total = dsnap.total_memory;
    size_t total_used = used + vmem_used;
    memory->total = total;
    memory->used = total_used >= total ? total : total_used;
    //memory->free = (memory->used + memory->reserved) >= memory->total ? 0 : memory->total - (memory->used + memory->reserved);
    memory->free = memory->total - memory->used;

    unlock_gpu_device(lock_fd);
  }
  return ret;
}

nvmlReturn_t nvmlDeviceSetComputeMode(nvmlDevice_t device, nvmlComputeMode_t mode) {
  nvmlReturn_t ret;
  int host_index = get_host_device_index_by_nvml_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  device_t dsnap = get_device_snapshot(host_index);
  if (dsnap.memory_limit || dsnap.core_limit) {
    ret = NVML_ERROR_NOT_SUPPORTED;
    goto DONE;
  }
CALL:
  ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceSetComputeMode, device, mode);
DONE:
  return ret;
}

nvmlReturn_t nvmlDeviceGetPersistenceMode(nvmlDevice_t device, nvmlEnableState_t *mode) {
  // fix: https://forums.developer.nvidia.com/t/nvidia-smi-uses-all-of-ram-and-swap/295639/15
  LOGGER(DETAIL, "hooking nvmlDeviceGetPersistenceMode");
  *mode = NVML_FEATURE_DISABLED;
  return NVML_SUCCESS;
}

/* ---- Device visibility ----
 *
 * CUDA_VISIBLE_DEVICES (published by the checkpoint provider) restricts the
 * CUDA driver, but NVML ignores it and always enumerates every physical GPU.
 * Left alone, a remote container's nvidia-smi would list the whole node and
 * could read another tenant's memory usage. These hooks close that: the
 * session's devices, numbered the same way CUDA numbers them.
 *
 * Only active for a remote session. In local mode the container runtime has
 * already restricted which devices exist, so filtering again could only ever
 * subtract from a correct view.
 *
 * The library's own enumeration is unaffected -- it goes through
 * NVML_INTERNAL_CALL / nvml_library_entry, i.e. the real driver functions, so
 * these hooks cannot recurse into themselves via init_devices_mapping(). */

/* A handle the caller obtained by any route is ours to serve only if it maps
 * back to an activated device in this session's config. */
static int device_is_visible(nvmlDevice_t device) {
  return config_visible_index_of(g_vgpu_config, get_host_device_index_by_nvml_device(device)) >= 0;
}

nvmlReturn_t _nvmlDeviceGetCount(unsigned int *deviceCount) {
  nvmlReturn_t ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetCount_v2))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetCount_v2, deviceCount);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetCount))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetCount, deviceCount);
  }
  return ret;
}

nvmlReturn_t nvmlDeviceGetCount_v2(unsigned int *deviceCount) {
  /* Session test first, and only then any initialisation: outside a session
   * this must be the passthrough it has always been, down to not pulling the
   * library's lazy init into a call that never did so before. Same shape in
   * every hook below. */
  if (likely(!session_enabled())) {
    return _nvmlDeviceGetCount(deviceCount);
  }
  load_necessary_data();
  if (deviceCount == NULL) {
    return NVML_ERROR_INVALID_ARGUMENT;
  }
  int host_indexes[MAX_DEVICE_COUNT];
  *deviceCount = (unsigned int)config_allowed_devices(g_vgpu_config, host_indexes, MAX_DEVICE_COUNT);
  LOGGER(DETAIL, "hooking nvmlDeviceGetCount_v2 -> %u", *deviceCount);
  return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetCount(unsigned int *deviceCount) {
  return nvmlDeviceGetCount_v2(deviceCount);
}

nvmlReturn_t _nvmlDeviceGetHandleByIndex_v2(unsigned int index, nvmlDevice_t *device) {
  nvmlReturn_t ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2, index, device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByIndex, index, device);
  }
  return ret;
}

nvmlReturn_t nvmlDeviceGetHandleByIndex_v2(unsigned int index, nvmlDevice_t *device) {
  if (likely(!session_enabled())) {
    return _nvmlDeviceGetHandleByIndex_v2(index, device);
  }
  load_necessary_data();
  if (device == NULL) {
    return NVML_ERROR_INVALID_ARGUMENT;
  }
  int host_index = config_allowed_device_at(g_vgpu_config, index);
  if (host_index < 0) {
    LOGGER(VERBOSE, "nvml index %u is outside the session's devices", index);
    return NVML_ERROR_INVALID_ARGUMENT;
  }
  int nvml_index = get_nvml_device_index_by_host_index(host_index);
  if (nvml_index < 0) {
    LOGGER(ERROR, "session device %d (host index %d) was not found on this node",
           index, host_index);
    return NVML_ERROR_NOT_FOUND;
  }
  LOGGER(DETAIL, "hooking nvmlDeviceGetHandleByIndex_v2 %u -> physical %d", index, nvml_index);
  return _nvmlDeviceGetHandleByIndex_v2((unsigned int)nvml_index, device);
}

nvmlReturn_t nvmlDeviceGetHandleByIndex(unsigned int index, nvmlDevice_t *device) {
  return nvmlDeviceGetHandleByIndex_v2(index, device);
}

/* Lookup by UUID / PCI id resolves against the whole node, so the result has
 * to be checked rather than the input. Without this a client could name a
 * device the index hooks would never hand out. */
nvmlReturn_t nvmlDeviceGetHandleByUUID(const char *uuid, nvmlDevice_t *device) {
  nvmlReturn_t ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByUUID, uuid, device);
  if (ret != NVML_SUCCESS || !session_enabled()) {
    return ret;
  }
  load_necessary_data();
  if (!device_is_visible(*device)) {
    LOGGER(VERBOSE, "refusing handle for uuid outside the session");
    return NVML_ERROR_NOT_FOUND;
  }
  return ret;
}

nvmlReturn_t _nvmlDeviceGetHandleByPciBusId(const char *pciBusId, nvmlDevice_t *device) {
  nvmlReturn_t ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByPciBusId_v2))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByPciBusId_v2, pciBusId, device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByPciBusId))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByPciBusId, pciBusId, device);
  }
  return ret;
}

nvmlReturn_t nvmlDeviceGetHandleByPciBusId_v2(const char *pciBusId, nvmlDevice_t *device) {
  nvmlReturn_t ret = _nvmlDeviceGetHandleByPciBusId(pciBusId, device);
  if (ret != NVML_SUCCESS || !session_enabled()) {
    return ret;
  }
  load_necessary_data();
  if (!device_is_visible(*device)) {
    LOGGER(VERBOSE, "refusing handle for pci bus id outside the session");
    return NVML_ERROR_NOT_FOUND;
  }
  return ret;
}

nvmlReturn_t nvmlDeviceGetHandleByPciBusId(const char *pciBusId, nvmlDevice_t *device) {
  return nvmlDeviceGetHandleByPciBusId_v2(pciBusId, device);
}

/* lupine does not currently forward this one, so no remote client can reach
 * it. Gated anyway: it is the same "name a device directly" shape as the two
 * above, and leaving one member of that family open would make the allowlist
 * depend on which RPCs lupine happens to expose today. */
nvmlReturn_t nvmlDeviceGetHandleBySerial(const char *serial, nvmlDevice_t *device) {
  nvmlReturn_t ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleBySerial, serial, device);
  if (ret != NVML_SUCCESS || !session_enabled()) {
    return ret;
  }
  load_necessary_data();
  if (!device_is_visible(*device)) {
    LOGGER(VERBOSE, "refusing handle for serial outside the session");
    return NVML_ERROR_NOT_FOUND;
  }
  return ret;
}

/* Report the index this device was handed out as. Returning the physical one
 * would contradict GetCount/GetHandleByIndex, and callers do round-trip an
 * index through a handle and back. */
nvmlReturn_t nvmlDeviceGetIndex(nvmlDevice_t device, unsigned int *index) {
  if (likely(!session_enabled())) {
    return NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetIndex, device, index);
  }
  load_necessary_data();
  if (index == NULL) {
    return NVML_ERROR_INVALID_ARGUMENT;
  }
  int visible = config_visible_index_of(g_vgpu_config, get_host_device_index_by_nvml_device(device));
  if (visible < 0) {
    return NVML_ERROR_NOT_FOUND;
  }
  *index = (unsigned int)visible;
  return NVML_SUCCESS;
}
