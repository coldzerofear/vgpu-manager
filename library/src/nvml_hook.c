#include <assert.h>

#include "include/hook.h"
#include "include/nvml-helper.h"

extern entry_t nvml_library_entry[];
extern resource_data_t* g_vgpu_config;

extern int lock_gpu_device(int device);
extern void unlock_gpu_device(int fd);

nvmlReturn_t nvmlInitWithFlags(unsigned int flags);
nvmlReturn_t nvmlInit_v2(void);
nvmlReturn_t nvmlInit(void);
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
  return NVML_ENTRY_CALL(nvml_library_entry, nvmlInitWithFlags, flags);
}

nvmlReturn_t nvmlInit_v2(void) {
  load_necessary_data();
  return NVML_ENTRY_CALL(nvml_library_entry, nvmlInit_v2);
}

nvmlReturn_t nvmlInit(void) {
  load_necessary_data();
  return NVML_ENTRY_CALL(nvml_library_entry, nvmlInit);
}

nvmlReturn_t nvmlDeviceGetMemoryInfo(nvmlDevice_t device, nvmlMemory_t *memory) {
  nvmlReturn_t ret;
  int index;
  int fd = -1;
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetIndex, device, &index);
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetIndex call error, return %d", ret);
    goto DONE;
  }
  if (g_vgpu_config->devices[index].memory_limit) {
    fd = lock_gpu_device(index);
    size_t used = 0, vmem_used = 0;
    get_used_gpu_memory_by_device((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, index);

    size_t total_memory = g_vgpu_config->devices[index].total_memory;
    memory->total = total_memory;
    memory->used = used + vmem_used >= total_memory ? total_memory: used + vmem_used;
    memory->free = total_memory - memory->used;
    ret = NVML_SUCCESS;
    goto DONE;
  }
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetMemoryInfo, device, memory);
DONE:
  unlock_gpu_device(fd);
  return ret;
}

nvmlReturn_t nvmlDeviceGetMemoryInfo_v2(nvmlDevice_t device, nvmlMemory_v2_t *memory) {
  nvmlReturn_t ret;
  int index;
  int fd = -1;
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetIndex, device, &index);
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetIndex call error, return %d", ret);
    goto DONE;
  }
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetMemoryInfo_v2, device, memory);
  if (ret == NVML_SUCCESS && g_vgpu_config->devices[index].memory_limit) {
    fd = lock_gpu_device(index);
    size_t used = 0, vmem_used = 0;
    get_used_gpu_memory_by_device((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, index);

    size_t total_memory = g_vgpu_config->devices[index].total_memory;
    memory->total = total_memory;
    memory->used = used + vmem_used >= total_memory ? total_memory : used + vmem_used;
    //memory->free = (used + memory->reserved) > g_vcuda_config.gpu_memory ? 0 : g_vcuda_config.gpu_memory - used - memory->reserved;
    memory->free = total_memory - memory->used;
  }
DONE:
  unlock_gpu_device(fd);
  return ret;
}

nvmlReturn_t nvmlDeviceSetComputeMode(nvmlDevice_t device, nvmlComputeMode_t mode) {
  nvmlReturn_t ret;
  int index;
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetIndex, device, &index);
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetIndex call error, return %d", ret);
    goto DONE;
  }
  if (g_vgpu_config->devices[index].memory_limit || g_vgpu_config->devices[index].core_limit) {
    return NVML_ERROR_NOT_SUPPORTED;
  }
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceSetComputeMode, device, mode);
DONE:
  return ret;
}

nvmlReturn_t nvmlDeviceGetPersistenceMode(nvmlDevice_t device, nvmlEnableState_t *mode) {
  // fix: https://forums.developer.nvidia.com/t/nvidia-smi-uses-all-of-ram-and-swap/295639/15
  *mode = NVML_FEATURE_DISABLED;
  return NVML_SUCCESS;
}
