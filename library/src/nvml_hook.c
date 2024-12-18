#include <assert.h>

#include "include/hook.h"
#include "include/nvml-helper.h"

extern entry_t nvml_library_entry[];
extern resource_data_t g_vgpu_config;

nvmlReturn_t nvmlInitWithFlags(unsigned int flags);
nvmlReturn_t nvmlInit_v2(void);
nvmlReturn_t nvmlInit(void);
nvmlReturn_t nvmlDeviceGetMemoryInfo(nvmlDevice_t device, nvmlMemory_t *memory);
nvmlReturn_t nvmlDeviceGetMemoryInfo_v2(nvmlDevice_t device, nvmlMemory_v2_t *memory);
nvmlReturn_t nvmlDeviceSetComputeMode(nvmlDevice_t device, nvmlComputeMode_t mode);

entry_t nvml_hooks_entry[] = {
    {.name = "nvmlInit", .fn_ptr = nvmlInit},
    {.name = "nvmlInit_v2", .fn_ptr = nvmlInit_v2},
    {.name = "nvmlInitWithFlags", .fn_ptr = nvmlInitWithFlags},
    {.name = "nvmlDeviceGetMemoryInfo", .fn_ptr = nvmlDeviceGetMemoryInfo},
    {.name = "nvmlDeviceGetMemoryInfo_v2", .fn_ptr = nvmlDeviceGetMemoryInfo_v2},
    {.name = "nvmlDeviceSetComputeMode", .fn_ptr = nvmlDeviceSetComputeMode},
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


// TODO 再此拦截要显示的设备内存
nvmlReturn_t nvmlDeviceGetMemoryInfo(nvmlDevice_t device,
                                     nvmlMemory_t *memory) {
  nvmlReturn_t ret;
  int index;
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetIndex, device, &index);
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetIndex call error, return %d", ret);
    goto DONE;
  }
  if (g_vgpu_config.devices[index].memory_limit) {
    size_t used = 0;
    get_used_gpu_memory_by_device((void *)&used, device);
    // 最大可用显存限制为 vcuda 分配时的大小
    memory->total = g_vgpu_config.devices[index].total_memory;
    memory->used = used;
    memory->free = memory->used > memory->total ? 0 : memory->total - memory->used;
    return NVML_SUCCESS;
  }
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetMemoryInfo, device,
                         memory);
DONE:
  return ret;
}

// TODO 再此拦截要显示的设备内存
nvmlReturn_t nvmlDeviceGetMemoryInfo_v2(nvmlDevice_t device, 
                                        nvmlMemory_v2_t *memory) {
  nvmlReturn_t ret;
  int index;
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetIndex, device, &index);
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetIndex call error, return %d", ret);
    goto DONE;
  }
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetMemoryInfo_v2, device, memory);
  if (g_vgpu_config.devices[index].memory_limit && ret == NVML_SUCCESS) {
    size_t used = 0;
    get_used_gpu_memory_by_device((void *)&used, device);
    // 最大可用显存限制为 vcuda 分配时的大小
    memory->total = g_vgpu_config.devices[index].total_memory;
    memory->used = used;
    // v2 版本接口used和reserved是分开计算的
    //memory->free = (used + memory->reserved) > g_vcuda_config.gpu_memory ? 0 : g_vcuda_config.gpu_memory - used - memory->reserved;
    memory->free = used > memory->total ? 0 : memory->total - memory->used;
  }
DONE:
  return ret;
}

nvmlReturn_t nvmlDeviceSetComputeMode(nvmlDevice_t device,
                                      nvmlComputeMode_t mode) {
  nvmlReturn_t ret;
  int index;
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetIndex, device, &index);
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetIndex call error, return %d", ret);
    goto DONE;
  }
  if (g_vgpu_config.devices[index].memory_limit || g_vgpu_config.devices[index].core_limit) {
    return NVML_ERROR_NOT_SUPPORTED;
  }
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceSetComputeMode, device, mode);
DONE:
  return ret;
}

