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

nvmlReturn_t nvmlInit(void);
nvmlReturn_t nvmlInit_v2(void);
nvmlReturn_t nvmlInitWithFlags(unsigned int flags);
nvmlReturn_t nvmlDeviceGetMemoryInfo(nvmlDevice_t device, nvmlMemory_t *memory);
nvmlReturn_t nvmlDeviceGetMemoryInfo_v2(nvmlDevice_t device, nvmlMemory_v2_t *memory);
nvmlReturn_t nvmlDeviceSetComputeMode(nvmlDevice_t device, nvmlComputeMode_t mode);
nvmlReturn_t nvmlDeviceGetPersistenceMode(nvmlDevice_t device, nvmlEnableState_t *mode);

entry_t nvml_hooks_entry[] = {
    {.name = "nvmlInit", .fn_ptr = nvmlInit},
    {.name = "nvmlInit_v2", .fn_ptr = nvmlInit_v2},
    {.name = "nvmlInitWithFlags", .fn_ptr = nvmlInitWithFlags},
    {.name = "nvmlDeviceGetMemoryInfo", .fn_ptr = nvmlDeviceGetMemoryInfo},
    {.name = "nvmlDeviceGetMemoryInfo_v2", .fn_ptr = nvmlDeviceGetMemoryInfo_v2},
    {.name = "nvmlDeviceSetComputeMode", .fn_ptr = nvmlDeviceSetComputeMode},
    {.name = "nvmlDeviceGetPersistenceMode", .fn_ptr = nvmlDeviceGetPersistenceMode},
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
