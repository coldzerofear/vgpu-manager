/*
 * Tencent is pleased to support the open source community by making TKEStack
 * available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not
 * use this file except in compliance with the License. You may obtain a copy of
 * the License at
 *
 * https://opensource.org/licenses/Apache-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OF ANY KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations under the License.
 */

#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <dirent.h>
#include <unistd.h>

#include "include/hook.h"
#include "include/cuda-helper.h"
#include "include/nvml-helper.h"

#define INCREMENT_SCALE_FACTOR   2560
#define MAX_UTIL_DIFF_THRESHOLD  0.5
#define MIN_INCREMENT            5
#define DEVICE_BATCH_SIZE        4

extern resource_data_t* g_vgpu_config;
extern device_util_t* g_device_util;

extern char container_id[FILENAME_MAX];
extern entry_t cuda_library_entry[];
extern entry_t nvml_library_entry[];

extern int lock_gpu_device(int device);
extern void unlock_gpu_device(int fd);

extern int device_util_read_lock(int ordinal);
extern void device_util_unlock(int fd, int ordinal);

extern fp_dlsym real_dlsym;
extern void *lib_control;

extern int extract_container_pids(char *base_path, int *pids, int *pids_size);

static pthread_once_t g_init_set = PTHREAD_ONCE_INIT;

static volatile int64_t g_cur_cuda_cores[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static volatile int64_t g_total_cuda_cores[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

static volatile int g_sm_num[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static volatile int g_max_thread_per_sm[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

static int g_block_x[MAX_DEVICE_COUNT] = {1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1};
static int g_block_y[MAX_DEVICE_COUNT] = {1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1};
static int g_block_z[MAX_DEVICE_COUNT] = {1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1};
static uint32_t g_block_locker[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

static const struct timespec g_cycle = {
  .tv_sec = 0,
  .tv_nsec = TIME_TICK * MILLISEC,
};

/** internal function definition */

static void active_utilization_notifier(int);

static void *utilization_watcher(void *);

static nvmlReturn_t get_gpu_process_from_external_watcher(utilization_t *, nvmlProcessUtilizationSample_t *, unsigned int *, int, int, nvmlDevice_t);

static nvmlReturn_t get_gpu_process_from_local_nvml_driver(utilization_t *, nvmlProcessUtilizationSample_t *, unsigned int *, int, nvmlDevice_t);

static void get_used_gpu_utilization(void *, int, int, nvmlDevice_t);

static void init_device_cuda_cores(int *device_count);

static void initialization();

static void rate_limiter(int grids, int blocks, CUdevice device);

static void change_token(int64_t, int);

static int64_t delta(int up_limit, int user_current, int64_t share, int host_index);

static int check_file_exist(const char *);

//static int int_compare(const void *a, const void *b);

/** export function definition */
CUresult cuDriverGetVersion(int *driverVersion);
CUresult cuInit(unsigned int flag);
CUresult cuGetProcAddress(const char *symbol, void **pfn, int cudaVersion,
                          cuuint64_t flags);
CUresult _cuGetProcAddress_v2(const char *symbol, void **pfn, int cudaVersion,
                          cuuint64_t flags, void *symbolStatus);
CUresult cuGetProcAddress_v2(const char *symbol, void **pfn, int cudaVersion,
                          cuuint64_t flags, void *symbolStatus);           
CUresult cuMemAllocManaged(CUdeviceptr *dptr, size_t bytesize,
                           unsigned int flags);
CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytesize);
CUresult cuMemAlloc(CUdeviceptr *dptr, size_t bytesize);
CUresult cuMemAllocPitch_v2(CUdeviceptr *dptr, size_t *pPitch,
                            size_t WidthInBytes, size_t Height,
                            unsigned int ElementSizeBytes);
CUresult cuMemAllocPitch(CUdeviceptr *dptr, size_t *pPitch, size_t WidthInBytes,
                         size_t Height, unsigned int ElementSizeBytes);
CUresult cuArrayCreate_v2(CUarray *pHandle,
                          const CUDA_ARRAY_DESCRIPTOR *pAllocateArray);
CUresult cuArrayCreate(CUarray *pHandle,
                       const CUDA_ARRAY_DESCRIPTOR *pAllocateArray);
CUresult cuArray3DCreate_v2(CUarray *pHandle,
                            const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray);
CUresult cuArray3DCreate(CUarray *pHandle,
                         const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray);
CUresult cuMipmappedArrayCreate(CUmipmappedArray *pHandle,
                       const CUDA_ARRAY3D_DESCRIPTOR *pMipmappedArrayDesc,
                       unsigned int numMipmapLevels);
CUresult cuDeviceTotalMem_v2(size_t *bytes, CUdevice dev);
CUresult cuDeviceTotalMem(size_t *bytes, CUdevice dev);
CUresult cuMemGetInfo_v2(size_t *free, size_t *total);
CUresult cuMemGetInfo(size_t *free, size_t *total);
CUresult cuLaunchKernel_ptsz(CUfunction f, unsigned int gridDimX,
                        unsigned int gridDimY, unsigned int gridDimZ,
                        unsigned int blockDimX, unsigned int blockDimY,
                        unsigned int blockDimZ, unsigned int sharedMemBytes,
                        CUstream hStream, void **kernelParams, void **extra);
CUresult cuLaunchKernel(CUfunction f, unsigned int gridDimX,
                        unsigned int gridDimY, unsigned int gridDimZ,
                        unsigned int blockDimX, unsigned int blockDimY,
                        unsigned int blockDimZ, unsigned int sharedMemBytes,
                        CUstream hStream, void **kernelParams, void **extra);
CUresult cuLaunchKernelEx(CUlaunchConfig *config, CUfunction f,
                        void **kernelParams, void **extra);
CUresult cuLaunchKernelEx_ptsz(CUlaunchConfig *config, CUfunction f, 
                        void **kernelParams, void **extra);
CUresult cuLaunch(CUfunction f);
CUresult cuLaunchCooperativeKernel_ptsz(CUfunction f, unsigned int gridDimX, 
                                  unsigned int gridDimY, unsigned int gridDimZ, 
                                  unsigned int blockDimX, unsigned int blockDimY,
                                  unsigned int blockDimZ, unsigned int sharedMemBytes, 
                                  CUstream hStream, void **kernelParams);
CUresult cuLaunchCooperativeKernel(CUfunction f, unsigned int gridDimX,
                                  unsigned int gridDimY, unsigned int gridDimZ,
                                  unsigned int blockDimX, unsigned int blockDimY,
                                  unsigned int blockDimZ, unsigned int sharedMemBytes,
                                  CUstream hStream, void **kernelParams);
CUresult cuLaunchGrid(CUfunction f, int grid_width, int grid_height);
CUresult cuLaunchGridAsync(CUfunction f, int grid_width, int grid_height,
                           CUstream hStream);
CUresult cuFuncSetBlockShape(CUfunction hfunc, int x, int y, int z);
CUresult cuMemAllocAsync(CUdeviceptr *dptr, size_t bytesize, CUstream hStream);
CUresult cuMemAllocAsync_ptsz(CUdeviceptr *dptr, size_t bytesize, CUstream hStream);
CUresult cuMemCreate(CUmemGenericAllocationHandle *handle, size_t size,
                     const CUmemAllocationProp *prop, unsigned long long flags);
CUresult cuMemAllocFromPoolAsync(CUdeviceptr *dptr, size_t bytesize,
                                 CUmemoryPool pool, CUstream hStream);
CUresult cuMemAllocFromPoolAsync_ptsz(CUdeviceptr *dptr, size_t bytesize,
                                 CUmemoryPool pool, CUstream hStream);
CUresult cuMemFree_v2(CUdeviceptr dptr);
CUresult cuMemFree(CUdeviceptr dptr);
CUresult cuMemFreeAsync(CUdeviceptr dptr, CUstream hStream);
CUresult cuMemFreeAsync_ptsz(CUdeviceptr dptr, CUstream hStream);

entry_t cuda_hooks_entry[] = {
    {.name = "cuDriverGetVersion", .fn_ptr = cuDriverGetVersion},
    {.name = "cuInit", .fn_ptr = cuInit},
    {.name = "cuGetProcAddress", .fn_ptr = cuGetProcAddress},
    {.name = "cuGetProcAddress_v2", .fn_ptr = cuGetProcAddress_v2},
    {.name = "cuMemAllocManaged", .fn_ptr = cuMemAllocManaged},
    {.name = "cuMemAlloc_v2", .fn_ptr = cuMemAlloc_v2},
    {.name = "cuMemAlloc", .fn_ptr = cuMemAlloc},
    {.name = "cuMemAllocPitch_v2", .fn_ptr = cuMemAllocPitch_v2},
    {.name = "cuMemAllocPitch", .fn_ptr = cuMemAllocPitch},
    {.name = "cuArrayCreate_v2", .fn_ptr = cuArrayCreate_v2},
    {.name = "cuArrayCreate", .fn_ptr = cuArrayCreate},
    {.name = "cuArray3DCreate_v2", .fn_ptr = cuArray3DCreate_v2},
    {.name = "cuArray3DCreate", .fn_ptr = cuArray3DCreate},
    {.name = "cuMipmappedArrayCreate", .fn_ptr = cuMipmappedArrayCreate},
    {.name = "cuDeviceTotalMem_v2", .fn_ptr = cuDeviceTotalMem_v2},
    {.name = "cuDeviceTotalMem", .fn_ptr = cuDeviceTotalMem},
    {.name = "cuMemGetInfo_v2", .fn_ptr = cuMemGetInfo_v2},
    {.name = "cuMemGetInfo", .fn_ptr = cuMemGetInfo},
    {.name = "cuLaunchKernel_ptsz", .fn_ptr = cuLaunchKernel_ptsz},
    {.name = "cuLaunchKernel", .fn_ptr = cuLaunchKernel},
    {.name = "cuLaunchKernelEx_ptsz", .fn_ptr = cuLaunchKernelEx_ptsz},
    {.name = "cuLaunchKernelEx", .fn_ptr = cuLaunchKernelEx},
    {.name = "cuLaunch", .fn_ptr = cuLaunch},
    {.name = "cuLaunchCooperativeKernel_ptsz", .fn_ptr = cuLaunchCooperativeKernel_ptsz},
    {.name = "cuLaunchCooperativeKernel", .fn_ptr = cuLaunchCooperativeKernel},
    {.name = "cuLaunchGrid", .fn_ptr = cuLaunchGrid},
    {.name = "cuLaunchGridAsync", .fn_ptr = cuLaunchGridAsync},
    {.name = "cuFuncSetBlockShape", .fn_ptr = cuFuncSetBlockShape},
    {.name = "cuMemAllocAsync", .fn_ptr = cuMemAllocAsync},
    {.name = "cuMemAllocAsync_ptsz", .fn_ptr = cuMemAllocAsync_ptsz},
    {.name = "cuMemCreate", .fn_ptr = cuMemCreate},
    {.name = "cuMemAllocFromPoolAsync", .fn_ptr = cuMemAllocFromPoolAsync},
    {.name = "cuMemAllocFromPoolAsync_ptsz", .fn_ptr = cuMemAllocFromPoolAsync_ptsz},
    {.name = "cuMemFree_v2", .fn_ptr = cuMemFree_v2},
    {.name = "cuMemFree", .fn_ptr = cuMemFree},
    {.name = "cuMemFreeAsync", .fn_ptr = cuMemFreeAsync},
    {.name = "cuMemFreeAsync_ptsz", .fn_ptr = cuMemFreeAsync_ptsz},
};

const int cuda_hook_nums =
    sizeof(cuda_hooks_entry) / sizeof(cuda_hooks_entry[0]);

dynamic_config_t g_dynamic_config = {
  .change_limit_interval = 30,
  .usage_threshold = 5,
  .error_recovery_step = 10
};

static int check_file_exist(const char *file_path) {
  int ret = 0;
  if (access(file_path, F_OK) == 0) {
      ret = 1;
  }
  return ret;
}


static void change_token(int64_t delta, int host_index) {
  int64_t cuda_cores_before = 0, cuda_cores_after = 0;

  LOGGER(DETAIL, "host device: %d, delta: %ld, curr: %ld", host_index, delta, g_cur_cuda_cores[host_index]);
  do {
    cuda_cores_before = g_cur_cuda_cores[host_index];
    cuda_cores_after = cuda_cores_before + delta;

    if (unlikely(cuda_cores_after > g_total_cuda_cores[host_index])) {
      cuda_cores_after = g_total_cuda_cores[host_index];
    } else if (unlikely(cuda_cores_after < 0)) {
      cuda_cores_after = 0;
    }
  } while (!CAS(&g_cur_cuda_cores[host_index], cuda_cores_before, cuda_cores_after));
}

static void rate_limiter(int grids, int blocks, CUdevice device) {
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    return;
  }
  if (g_vgpu_config->devices[host_index].core_limit) {
    int64_t before_cuda_cores = 0;
    int64_t after_cuda_cores = 0;
    int64_t kernel_size = (int64_t) grids;

    LOGGER(VERBOSE, "cuda device: %d, host device: %d, grid: %d, blocks: %d", device, host_index, grids, blocks);
    LOGGER(VERBOSE, "cuda device: %d, host device: %d, launch kernel: %ld, curr core: %ld", device, host_index, kernel_size, g_cur_cuda_cores[host_index]);
    do {
    CHECK:
      before_cuda_cores = g_cur_cuda_cores[host_index];
      LOGGER(DETAIL, "cuda device: %d, host device: %d, current core: %ld", device, host_index, before_cuda_cores);
      if (before_cuda_cores < 0) {
        nanosleep(&g_cycle, NULL);
        goto CHECK;
      }
      after_cuda_cores = before_cuda_cores - kernel_size;
    } while (!CAS(&g_cur_cuda_cores[host_index], before_cuda_cores, after_cuda_cores));
  }
}

static int64_t delta(int up_limit, int user_current, int64_t share, int host_index) {
  // 1. Using wider data types to prevent computation overflow
  int64_t sm_num = (int64_t)g_sm_num[host_index];
  int64_t max_thread = (int64_t)g_max_thread_per_sm[host_index];

  // 2. Calculate the difference in utilization rate
  int utilization_diff = abs(up_limit - user_current);
  if (utilization_diff < MIN_INCREMENT) {
    utilization_diff = MIN_INCREMENT;
  }

  // 3. Calculate increment (using 64 bit operation to prevent overflow)
  int64_t increment = sm_num * sm_num * max_thread * (int64_t)(utilization_diff) / INCREMENT_SCALE_FACTOR;

  // 4. Accelerate adjustment logic (using floating-point thresholds instead of hard coding)
  if ((float)utilization_diff / (float)(up_limit) > MAX_UTIL_DIFF_THRESHOLD) {
    increment = increment * utilization_diff * 2 / (up_limit + 1);
  }

  // 5. Error handling optimization: When the increment is negative,
  //    the process is no longer terminated, but rolled back to a safe value
  if (unlikely(increment < 0 || increment > INT_MAX)) {
    LOGGER(ERROR, "host device %d, increment overflow: %ld, current sm: %ld, thread_per_sm: %ld, diff: %d",
           host_index, increment, sm_num, max_thread, utilization_diff);
    increment = g_dynamic_config.error_recovery_step;
  }

  if (user_current <= up_limit) {
    share = (share + increment) > g_total_cuda_cores[host_index] ?
            g_total_cuda_cores[host_index] : (share + increment);
  } else {
    share = (share - increment) < 0 ? 0 : (share - increment);
  }

  return share;
}

static int64_t shares[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static int sys_frees[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static int avg_sys_frees[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static int is[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static int pre_sys_process_nums[MAX_DEVICE_COUNT] = {1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1};
static utilization_t top_results[MAX_DEVICE_COUNT] = {};
static int up_limits[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static volatile nvmlDevice_t nvml_devices[MAX_DEVICE_COUNT] = {};

static void *utilization_watcher(void *arg) {
  batch_t *batch = (batch_t *)arg;
  LOGGER(VERBOSE, "start %s batch code %d", __FUNCTION__, batch->batch_code);
  LOGGER(VERBOSE, "batch code %d, start index %d, end index %d", batch->batch_code, batch->start_index, batch->end_index);

  int host_indexes[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

//  CUdevice cuda_device;
//  CUresult result;
  int host_index;
  int cuda_index;
//  int need_limit = 0;
  for (cuda_index = batch->start_index; cuda_index < batch->end_index; cuda_index++) {
    host_index = get_host_device_index_by_cuda_device(cuda_index);
    host_indexes[cuda_index] = host_index;

    up_limits[host_index] = g_vgpu_config->devices[host_index].hard_core;
    top_results[host_index].user_current = 0;
    top_results[host_index].sys_current = 0;
    top_results[host_index].valid = 0;
    top_results[host_index].sys_process_num = 0;
//    if (g_vgpu_config->devices[host_index].core_limit) {
//      need_limit = 1;
//    }
  }
//  if (!need_limit) {
//    LOGGER(VERBOSE, "no need cuda core limit for batch %d", batch->batch_code);
//    return NULL;
//  }

  int dev_count = batch->end_index - batch->start_index;
  struct timespec wait = {
      .tv_sec = 0,
      .tv_nsec = 100 / dev_count * MILLISEC,
  };
  while (1) {
    for (cuda_index = batch->start_index; cuda_index < batch->end_index; cuda_index++) {
      nanosleep(&wait, NULL);
      host_index = host_indexes[cuda_index];

      // Skip GPU without core limit enabled
      if (!g_vgpu_config->devices[host_index].core_limit) continue;

      get_used_gpu_utilization((void *)&top_results[host_index], cuda_index, host_index, nvml_devices[host_index]);

      if (unlikely(!top_results[host_index].valid)) continue;

      sys_frees[host_index] = MAX_UTILIZATION - top_results[host_index].sys_current;

      if (g_vgpu_config->devices[host_index].hard_limit) {
        /* Avoid usage jitter when application is initialized*/
        if (top_results[host_index].sys_process_num == 1 && top_results[host_index].user_current < up_limits[host_index] / 10) {
          g_cur_cuda_cores[host_index] =
              delta(g_vgpu_config->devices[host_index].hard_core, top_results[host_index].user_current, shares[host_index], host_index);
          continue;
        }
        shares[host_index] = delta(g_vgpu_config->devices[host_index].hard_core, top_results[host_index].user_current, shares[host_index], host_index);
      } else {
        if (pre_sys_process_nums[host_index] != top_results[host_index].sys_process_num) {
          /* When a new process comes, all processes are reset to initial value*/
          if (pre_sys_process_nums[host_index] < top_results[host_index].sys_process_num) {
            shares[host_index] = (int64_t) g_max_thread_per_sm[host_index];
            up_limits[host_index] = g_vgpu_config->devices[host_index].hard_core;
            is[host_index] = 0;
            avg_sys_frees[host_index] = 0;
          }
          pre_sys_process_nums[host_index] = top_results[host_index].sys_process_num;
        }

        /* 1.Only one process on the GPU
         * Allocate cuda cores according to the limit value.
         *
         * 2.Multiple processes on the GPU
         * First, change the up_limit of the process according to the
         * historical resource utilization. Second, allocate the cuda
         * cores according to the changed limit value.*/
        if (top_results[host_index].sys_process_num == 1) {
          up_limits[host_index] = g_vgpu_config->devices[host_index].soft_core;
          shares[host_index] = delta(up_limits[host_index], top_results[host_index].user_current, shares[host_index], host_index);
        } else {
          is[host_index]++;
          avg_sys_frees[host_index] += sys_frees[host_index];
          if (is[host_index] % g_dynamic_config.change_limit_interval == 0) {
            if (avg_sys_frees[host_index] * 2 / g_dynamic_config.change_limit_interval > g_dynamic_config.usage_threshold) {
              up_limits[host_index] = up_limits[host_index] + g_vgpu_config->devices[host_index].hard_core / 10 > g_vgpu_config->devices[host_index].soft_core ?
                         g_vgpu_config->devices[host_index].soft_core : up_limits[host_index] + g_vgpu_config->devices[host_index].hard_core / 10;
            }
            is[host_index] = 0;
          }
          avg_sys_frees[host_index] = is[host_index] % (g_dynamic_config.change_limit_interval / 2) == 0 ? 0 : avg_sys_frees[host_index];
          shares[host_index] = delta(up_limits[host_index], top_results[host_index].user_current, shares[host_index], host_index);
        }
      }
      change_token(shares[host_index], host_index);
      LOGGER(DETAIL, "cuda device: %d, host device: %d, user util: %d, up_limit: %d, share: %ld, curr core: %ld", cuda_index, host_index,
             top_results[host_index].user_current, up_limits[host_index], shares[host_index], g_cur_cuda_cores[host_index]);
    }
  }
}

static batch_t batches[MAX_DEVICE_COUNT / DEVICE_BATCH_SIZE] = {};

static void active_utilization_notifier(int batch_code) {
  pthread_t tid;
  pthread_create(&tid, NULL, utilization_watcher, &batches[batch_code]);
  char thread_name[32] = {0};
  sprintf(thread_name, "watch_util_bt_%d", batches[batch_code].batch_code);
#ifdef __APPLE__
  pthread_setname_np(thread_name);
#else
  pthread_setname_np(tid, thread_name);
#endif
}

static void init_device_cuda_cores(int *device_count) {
  CUresult ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceGetCount, device_count);
  if (unlikely(ret)) {
    LOGGER(FATAL, "cuDeviceGetCount call failed, return %d, str: %s", ret, CUDA_ERROR(cuda_library_entry, ret));
  }
  CUdevice device;
  nvmlReturn_t rt;
  for (int cuda_index = 0; cuda_index < *device_count; cuda_index++) {
    ret = CUDA_INTERNAL_CALL(cuda_library_entry, cuDeviceGet, &device, cuda_index);
    if (unlikely(ret)) {
      LOGGER(FATAL, "cuDeviceGet call failed, cuda device %d, return %d, str %s",
            cuda_index, ret, CUDA_ERROR(cuda_library_entry, ret));
    }
    int host_index = get_host_device_index_by_cuda_device(device);
    if (host_index < 0) {
      LOGGER(FATAL, "cuda device %d cannot find the corresponding host device", device);
    }
    int nvml_index = get_nvml_device_index_by_cuda_device(device);
    if (nvml_index < 0) {
      LOGGER(FATAL, "cuda device %d cannot find the corresponding nvml device", device);
    }

    if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2))) {
      rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2, nvml_index, &nvml_devices[host_index]);
    } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex))) {
      rt = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex, nvml_index, &nvml_devices[host_index]);
    } else {
      rt = NVML_ERROR_FUNCTION_NOT_FOUND;
    }
    if (unlikely(rt)) {
      LOGGER(FATAL, "nvmlDeviceGetHandleByIndex call failed, nvml device %d, return %d, str %s",
                     nvml_index, rt, NVML_ERROR(nvml_library_entry, rt));
    }

    ret = CUDA_INTERNAL_CALL(cuda_library_entry, cuDeviceGetAttribute, &g_sm_num[host_index],
                          CU_DEVICE_ATTRIBUTE_MULTIPROCESSOR_COUNT, device);
    if (unlikely(ret)) {
      LOGGER(FATAL, "can't get processor number, cuda device %d, return %d, str %s",
                     device, ret, CUDA_ERROR(cuda_library_entry, ret));
    }

    ret = CUDA_INTERNAL_CALL(cuda_library_entry, cuDeviceGetAttribute, &g_max_thread_per_sm[host_index],
                          CU_DEVICE_ATTRIBUTE_MAX_THREADS_PER_MULTIPROCESSOR, device);
    if (unlikely(ret)) {
      LOGGER(FATAL, "can't get max thread per processor, cuda device %d, return %d, str %s",
                     device, ret, CUDA_ERROR(cuda_library_entry, ret));
    }
    g_total_cuda_cores[host_index] = (int64_t)g_max_thread_per_sm[host_index] * (int64_t)(g_sm_num[host_index]) * FACTOR;

    LOGGER(VERBOSE, "cuda device %d total cuda cores: %ld", cuda_index, g_total_cuda_cores[host_index]);

  }
}

static void balance_batches(int device_count) {
  if (device_count <= 0) return;
  int batch_size = DEVICE_BATCH_SIZE;
  // When sm watcher is turned on, all devices are merged into one batch, reducing the number of monitoring threads.
  if (g_vgpu_config->sm_watcher) {
    batch_size = MAX_DEVICE_COUNT;
  }
  int batch_count = (device_count + batch_size - 1) / batch_size;
  int base_size = device_count / batch_count;
  int remainder = device_count % batch_count;
  int current_index = 0;
  int current_size = 0;
  for (int i = 0; i < batch_count; i++) {
    current_size = base_size;
    if (i < remainder) {
      current_size++;
    }
    batches[i].start_index = current_index;
    batches[i].end_index = current_index + current_size;
    batches[i].batch_code = i;
    current_index += current_size;
    active_utilization_notifier(i);
  }
}

static void initialization() {
  int ret;
  ret = CUDA_INTERNAL_CALL(cuda_library_entry, cuInit, 0);
  if (unlikely(ret)) {
    LOGGER(ERROR, "cuInit error %s", CUDA_ERROR(cuda_library_entry, (CUresult)ret));
    LOGGER(ERROR, "initialization of sm watcher failed");
    return;
  }
  int device_count;
  init_device_cuda_cores(&device_count);
  balance_batches(device_count);
}

int split_str(char *line, char *key, char *value, char d) {
  int index = 0;
  for (index = 0; index < strlen(line) && line[index] != d; index++) {}

  if (index == strlen(line)){
    key[0] = '\0';
    value = '\0';
    return 1;
  }

  int start = 0, i = 0;
  // trim head
  for (; start < index && (line[start] == ' ' || line[start] == '\t'); start++) {}

  for (i = 0; start < index; i++, start++) {
    key[i] = line[start];
  }
  // trim tail
  for (; i > 0 && (key[i - 1] == '\0' || key[i - 1] == '\n' || key[i - 1] == '\t'); i--) {}

  key[i] = '\0';

  start = index + 1;
  i = 0;

  // trim head
  for (; start < strlen(line) && (line[start] == ' ' || line[start] == '\t'); start++) {}

  for (i = 0; start < strlen(line); i++, start++) {
    value[i] = line[start];
  }
  // trim tail
  for (; i > 0 && (value[i - 1] == '\0' || value[i - 1] == '\n' || value[i - 1] == '\t'); i--) {}

  value[i] = '\0';
  return 0;
}


int read_cgroup(char *pid_path, char *cgroup_key, char *cgroup_value) {
  FILE *f = fopen(pid_path, "rb");
  if (f == NULL) {
    return 1;
  }
  char buff[255];
  while (fgets(buff, 255, f)) {
    int index = 0;
    for (; index < strlen(buff) && buff[index] != ':'; index++) {}
    if (index == strlen(buff)) {
      continue;
    }
    char key[128], value[128];
    if (split_str(&buff[index + 1], key, value, ':') != 0) {
      continue;
    }
    if (strcmp(key, cgroup_key) == 0) {
      strcpy(cgroup_value, value);
      fclose(f);
      return 0;
    }
  }
  fclose(f);
  return 1;
}

int check_container_pid_by_cgroupv1(unsigned int pid) {
  int ret = 0;
  if (pid == 0) {
    return ret;
  }
  char pid_path[128] = "";
  sprintf(pid_path, HOST_PROC_CGROUP_PID_PATH, pid);

  char container_cg[256];
  char process_cg[256];

  if (!read_cgroup(PID_SELF_CGROUP_PATH, "memory", container_cg) && !read_cgroup(pid_path, "memory", process_cg)) {
    LOGGER(DETAIL, "\ncontainer cg: %s\nprocess cg: %s", container_cg, process_cg);
    if (strstr(process_cg, container_cg) != NULL) ret = 1;
  }
  return ret;
}

int check_container_pid_by_cgroupv2(unsigned int pid) {
  int ret = 0;
  if (pid == 0) {
    return ret;
  }
  char pid_path[128] = "";
  sprintf(pid_path, HOST_PROC_CGROUP_PID_PATH, pid);
  if (!check_file_exist(pid_path)) {
    return ret;
  }
  FILE *fp = fopen(pid_path, "rb");
  if (!fp) {
    LOGGER(VERBOSE, "read file %s failed: %s", pid_path, strerror(errno));
    return ret;
  }
  char buff[FILENAME_MAX];
  while (fgets(buff, FILENAME_MAX, fp)) {
    size_t len = strlen(buff);
    if (len > 0 && buff[len - 1] == '\n') {
      buff[len - 1] = '\0';
    }
    if (strcmp(buff, "0::/") == 0 || strstr(buff, container_id) != NULL) {
      ret = 1;
      break;
    }
  }
  fclose(fp);
  return ret;
}

static int int_compare(const void *a, const void *b) {
  const int *pa = (const int *)a;
  const int *pb = (const int *)b;
  return (*pa > *pb) - (*pa < *pb);
}

int check_container_pid_by_open_kernel(unsigned int pid, int *pids_on_container, int pids_size) {
  int ret = 0;
  if (pid == 0 || !pids_on_container || pids_size <= 0) {
    return ret;
  }
  if (bsearch(&pid, pids_on_container, (size_t)pids_size, sizeof(int), int_compare)) {
    ret = 1;
  }
  return ret;
}

void accumulate_used_memory(size_t *used_memory, nvmlProcessInfo_t *pids_on_device, unsigned int size_on_device) {
  unsigned int i;
  if (size_on_device == 0) {
  } else if ((g_vgpu_config->compatibility_mode & CLIENT_COMPATIBILITY_MODE) == CLIENT_COMPATIBILITY_MODE) {
    int pids_size = MAX_PIDS;
    int pids_on_container[MAX_PIDS];
    extract_container_pids(CONTAINER_PIDS_CONFIG_FILE_PATH, pids_on_container, &pids_size);
    if (unlikely(pids_size == 0)) {
      LOGGER(FATAL, "unable to find registered container process");
    }
    for (i = 0; i < size_on_device; i++) {
      if (check_container_pid_by_open_kernel(pids_on_device[i].pid, pids_on_container, pids_size)) {
        LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        *used_memory += pids_on_device[i].usedGpuMemory;
      }
    }
  } else if ((g_vgpu_config->compatibility_mode & CGROUPV2_COMPATIBILITY_MODE) == CGROUPV2_COMPATIBILITY_MODE) {
    //LOGGER(VERBOSE, "use cgroupv2 compatibility mode");
    int pids_size = 0;
    int pids_on_container[MAX_PIDS];
    for (i = 0; i < size_on_device; i++) {
      if (check_container_pid_by_cgroupv2(pids_on_device[i].pid)) {
        LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        *used_memory += pids_on_device[i].usedGpuMemory;
      } else if ((g_vgpu_config->compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
        if (unlikely(pids_size == 0)) {
          char proc_path[PATH_MAX];
          pids_size = MAX_PIDS;
          snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
          extract_container_pids(proc_path, pids_on_container, &pids_size);
        }
        if (check_container_pid_by_open_kernel(pids_on_device[i].pid, pids_on_container, pids_size)) {
          LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
          *used_memory += pids_on_device[i].usedGpuMemory;
        }
      }
    }
  } else if ((g_vgpu_config->compatibility_mode & CGROUPV1_COMPATIBILITY_MODE) == CGROUPV1_COMPATIBILITY_MODE) {
    //LOGGER(VERBOSE, "use cgroupv1 compatibility mode");
    int pids_size = 0;
    int pids_on_container[MAX_PIDS];
    for (i = 0; i < size_on_device; i++) {
      if (check_container_pid_by_cgroupv1(pids_on_device[i].pid)) {
        LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        *used_memory += pids_on_device[i].usedGpuMemory;
      } else if ((g_vgpu_config->compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
        if (unlikely(pids_size == 0)) {
          char proc_path[PATH_MAX];
          pids_size = MAX_PIDS;
          snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
          extract_container_pids(proc_path, pids_on_container, &pids_size);
        }
        if (check_container_pid_by_open_kernel(pids_on_device[i].pid, pids_on_container, pids_size)) {
          LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
          *used_memory += pids_on_device[i].usedGpuMemory;
        }
      }
    }
  } else if ((g_vgpu_config->compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
    //LOGGER(VERBOSE, "use open kernel driver compatibility mode");
    int pids_size = MAX_PIDS;
    int pids_on_container[MAX_PIDS];
    char proc_path[PATH_MAX];
    snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
    extract_container_pids(proc_path, pids_on_container, &pids_size);
    for (i = 0; i < size_on_device; i++) {
      if (check_container_pid_by_open_kernel(pids_on_device[i].pid, pids_on_container, pids_size)) {
        LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        *used_memory += pids_on_device[i].usedGpuMemory;
      }
    }
  } else if (g_vgpu_config->compatibility_mode == HOST_COMPATIBILITY_MODE) {
    //LOGGER(VERBOSE, "use host compatibility mode");
    for (i = 0; i < size_on_device; i++) { // Host mode does not verify PID
      LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
      *used_memory += pids_on_device[i].usedGpuMemory;
    }
  } else {
    LOGGER(FATAL, "unsupported environment compatibility mode: %d", g_vgpu_config->compatibility_mode);
  }

}

void get_used_gpu_memory_by_device(void *arg, nvmlDevice_t device) {
  size_t *used_memory = arg;
  nvmlProcessInfo_t pids_on_device[MAX_PIDS];
  unsigned int size_on_device = MAX_PIDS;
  nvmlReturn_t ret;

  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses,
                             device, &size_on_device, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2,
                             device, &size_on_device, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3,
                             device, &size_on_device, pids_on_device);
  } else {
    ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  }
  if (unlikely(ret)) {
    *used_memory = 0;
    LOGGER(ERROR, "nvmlDeviceGetComputeRunningProcesses call failed, return: %d, str: %s",
                   ret, NVML_ERROR(nvml_library_entry, ret));
    return;
  }
  accumulate_used_memory(used_memory, pids_on_device, size_on_device);

  // TODO　Increase the memory usage of intercepting graphic processes.
  size_on_device = MAX_PIDS;
  nvmlProcessInfo_t graphic_pids_on_device[MAX_PIDS];

  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses,
                           device, &size_on_device, graphic_pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v2))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v2,
                           device, &size_on_device, graphic_pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v3))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v3,
                           device, &size_on_device, graphic_pids_on_device);
  } else {
    ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  }
  if (unlikely(ret)) {
    LOGGER(ERROR, "nvmlDeviceGetGraphicsRunningProcesses call failed, return: %d, str: %s",
                   ret, NVML_ERROR(nvml_library_entry, ret));
    goto DONE;
  }
  accumulate_used_memory(used_memory, pids_on_device, size_on_device);

DONE:
  LOGGER(VERBOSE, "total used memory: %zu", *used_memory);
}

void get_used_gpu_memory(void *arg, CUdevice device) {
  size_t *used_memory = arg;

  int nvml_index = get_nvml_device_index_by_cuda_device(device);
  if (nvml_index < 0) {
    *used_memory = 0;
    LOGGER(ERROR, "cuda device %d cannot find the corresponding nvml devices", device);
    return;
  }

  nvmlDevice_t dev;
  nvmlReturn_t ret;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2, nvml_index, &dev);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex, nvml_index, &dev);
  } else {
    ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  }
  if (unlikely(ret)) {
    *used_memory = 0;
    LOGGER(ERROR, "nvmlDeviceGetHandleByIndex call failed, nvml device: %d, return: %d, str: %s",
                   nvml_index, ret, NVML_ERROR(nvml_library_entry, ret));
    return;
  }

  get_used_gpu_memory_by_device((void *)used_memory, dev);
}

static nvmlReturn_t get_gpu_process_from_local_nvml_driver(utilization_t *top_result, nvmlProcessUtilizationSample_t *processes_sample, unsigned int *processes_size, int cuda_index, nvmlDevice_t dev) {
  nvmlReturn_t ret;
  struct timeval cur, prev;
  nvmlProcessInfo_t pids_on_device[MAX_PIDS];
  unsigned int running_processes = MAX_PIDS;

  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses,
                             dev, &running_processes, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2,
                             dev, &running_processes, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3))) {
    ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3,
                             dev, &running_processes, pids_on_device);
  } else {
    ret = NVML_ERROR_FUNCTION_NOT_FOUND;
  }
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetComputeRunningProcesses can't get pids on cuda device %d, "
                 "return %d, str: %s", cuda_index, ret, NVML_ERROR(nvml_library_entry, ret));
    return ret;
  }

  top_result->sys_process_num = running_processes;

  gettimeofday(&cur, NULL);
  struct timeval temp = {1, 0};
  timersub(&cur, &temp, &prev);
  uint64_t microsec = (uint64_t)prev.tv_sec * 1000000ULL + prev.tv_usec;
  top_result->checktime = microsec;

  ret = NVML_INTERNAL_CALL(nvml_library_entry, nvmlDeviceGetProcessUtilization,
                           dev, processes_sample, processes_size, microsec);
  if (unlikely(ret)) {
    if (ret != NVML_ERROR_NOT_FOUND) {
      LOGGER(VERBOSE, "nvmlDeviceGetProcessUtilization can't get process utilization on cuda device: %d, "
                      "return %d, str: %s", cuda_index, ret, NVML_ERROR(nvml_library_entry, ret));
    }
    return ret;
  }

  // When using open source kernel modules, nvmlDeviceGetComputeRunningProcesses can only
  // query processes in the container namespace, while nvmlDeviceGetProcessUtilization
  // can query global processes, so it may need to be updated to the global process count here.
  if ((g_vgpu_config->compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
    if (*processes_size > running_processes) {
       top_result->sys_process_num = *processes_size;
    }
  }

  return NVML_SUCCESS;
}

int is_expired(unsigned long long lastTs) {
    struct timeval cur;
    gettimeofday(&cur, NULL);
    unsigned long long cur_us = cur.tv_sec * 1000000 + cur.tv_usec;
    return (cur_us - lastTs) >= 5000000; // 5,000,000 microsecond
}

static nvmlReturn_t get_gpu_process_from_external_watcher(utilization_t *top_result, nvmlProcessUtilizationSample_t *processes_sample, unsigned int *processes_size, int cuda_index, int host_index, nvmlDevice_t dev) {
  int fd = device_util_read_lock(host_index);
  if (fd < 0) {
    LOGGER(WARNING, "failed to acquire read lock for host device %d, fallback to nvml driver", host_index);
    return get_gpu_process_from_local_nvml_driver(top_result, processes_sample, processes_size, cuda_index, dev);
  }
  int expired = is_expired(g_device_util->devices[host_index].lastSeenTimeStamp);
  if (expired) {
    goto DONE;
  }
  unsigned int actual_size = g_device_util->devices[host_index].process_util_samples_size;
  unsigned int copy_size = (*processes_size < actual_size) ? *processes_size : actual_size;
  if (copy_size > 0 && processes_sample != NULL) {
    memcpy(processes_sample, g_device_util->devices[host_index].process_util_samples, copy_size * sizeof(nvmlProcessUtilizationSample_t));
  }
  *processes_size = copy_size;

  top_result->sys_process_num = g_device_util->devices[host_index].compute_processes_size;
  top_result->checktime = (uint64_t)g_device_util->devices[host_index].lastSeenTimeStamp;

DONE:
  device_util_unlock(fd, host_index);
  if (expired) {
     LOGGER(WARNING, "host device %d process utilization time window timeout detected, fallback to nvml driver", host_index);
     return get_gpu_process_from_local_nvml_driver(top_result, processes_sample, processes_size, cuda_index, dev);
  }
  return NVML_SUCCESS;
}

static void get_used_gpu_utilization(void *arg, int cuda_index, int host_index, nvmlDevice_t dev) {
  utilization_t *top_result = (utilization_t *)arg;
  nvmlProcessUtilizationSample_t processes_sample[MAX_PIDS];
  unsigned int processes_num = MAX_PIDS;

  nvmlReturn_t ret;
  if (g_vgpu_config->sm_watcher) {
    ret = get_gpu_process_from_external_watcher(top_result, processes_sample, &processes_num, cuda_index, host_index, dev);
  } else {
    ret = get_gpu_process_from_local_nvml_driver(top_result, processes_sample, &processes_num, cuda_index, dev);
  }
  if (unlikely(ret)) return;

  top_result->user_current = 0;
  top_result->sys_current = 0;

  int sm_util = 0;
  int codec_util = 0;

  int i;

  if (processes_num == 0) {
  } else if ((g_vgpu_config->compatibility_mode & CLIENT_COMPATIBILITY_MODE) == CLIENT_COMPATIBILITY_MODE) {
    int pids_size = MAX_PIDS;
    int pids_on_container[MAX_PIDS];
    extract_container_pids(CONTAINER_PIDS_CONFIG_FILE_PATH, pids_on_container, &pids_size);
    if (likely(pids_size > 0)) {
      for (i = 0; i < processes_num; i++) {
        if (processes_sample[i].timeStamp >= top_result->checktime) {
          top_result->valid = 1;
          sm_util = GET_VALID_VALUE(processes_sample[i].smUtil);
          codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) +
                       GET_VALID_VALUE(processes_sample[i].decUtil);
          codec_util = CODEC_NORMALIZE(codec_util);
          top_result->sys_current += sm_util + codec_util;
          if (check_container_pid_by_open_kernel(processes_sample[i].pid, pids_on_container, pids_size)) {
            top_result->user_current += sm_util + codec_util;
          }
        }
      }
    }
  } else if ((g_vgpu_config->compatibility_mode & CGROUPV2_COMPATIBILITY_MODE) == CGROUPV2_COMPATIBILITY_MODE) {
    int pids_size = 0;
    int pids_on_container[MAX_PIDS];
    for (i = 0; i < processes_num; i++) {
      if (processes_sample[i].timeStamp >= top_result->checktime) {
        top_result->valid = 1;
        sm_util = GET_VALID_VALUE(processes_sample[i].smUtil);
        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) +
                     GET_VALID_VALUE(processes_sample[i].decUtil);
        codec_util = CODEC_NORMALIZE(codec_util);
        top_result->sys_current += sm_util + codec_util;
        if (check_container_pid_by_cgroupv2(processes_sample[i].pid)) {
          top_result->user_current += sm_util + codec_util;
        } else if ((g_vgpu_config->compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
          if (unlikely(pids_size == 0)) {
            char proc_path[PATH_MAX];
            pids_size = MAX_PIDS;
            snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
            extract_container_pids(proc_path, pids_on_container, &pids_size);
          }
          if (check_container_pid_by_open_kernel(processes_sample[i].pid, pids_on_container, pids_size)) {
            top_result->user_current += sm_util + codec_util;
          }
        }
      }
    }
  } else if ((g_vgpu_config->compatibility_mode & CGROUPV1_COMPATIBILITY_MODE) == CGROUPV1_COMPATIBILITY_MODE) {
    int pids_size = 0;
    int pids_on_container[MAX_PIDS];
    for (i = 0; i < processes_num; i++) {
      if (processes_sample[i].timeStamp >= top_result->checktime) {
        top_result->valid = 1;
        sm_util = GET_VALID_VALUE(processes_sample[i].smUtil);
        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) +
                     GET_VALID_VALUE(processes_sample[i].decUtil);
        codec_util = CODEC_NORMALIZE(codec_util);
        top_result->sys_current += sm_util + codec_util;
        if (check_container_pid_by_cgroupv1(processes_sample[i].pid)) {
          top_result->user_current += sm_util + codec_util;
        } else if ((g_vgpu_config->compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
          if (unlikely(pids_size == 0)) {
            char proc_path[PATH_MAX];
            pids_size = MAX_PIDS;
            snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
            extract_container_pids(proc_path, pids_on_container, &pids_size);
          }
          if (check_container_pid_by_open_kernel(processes_sample[i].pid, pids_on_container, pids_size)) {
            top_result->user_current += sm_util + codec_util;
          }
        }
      }
    }
  } else if ((g_vgpu_config->compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
    int pids_size = 0;
    int pids_on_container[MAX_PIDS];
    char proc_path[PATH_MAX];
    snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
    extract_container_pids(proc_path, pids_on_container, &pids_size);
    for (i = 0; i < processes_num; i++) {
      if (processes_sample[i].timeStamp >= top_result->checktime) {
        top_result->valid = 1;
        sm_util = GET_VALID_VALUE(processes_sample[i].smUtil);
        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) +
                     GET_VALID_VALUE(processes_sample[i].decUtil);
        codec_util = CODEC_NORMALIZE(codec_util);
        top_result->sys_current += sm_util + codec_util;
        if (check_container_pid_by_open_kernel(processes_sample[i].pid, pids_on_container, pids_size)) {
          top_result->user_current += sm_util + codec_util;
        }
      }
    }
  } else if (g_vgpu_config->compatibility_mode == HOST_COMPATIBILITY_MODE) {
    for (i = 0; i < processes_num; i++) {
      if (processes_sample[i].timeStamp >= top_result->checktime) {
        top_result->valid = 1;
        sm_util = GET_VALID_VALUE(processes_sample[i].smUtil);
        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) +
                     GET_VALID_VALUE(processes_sample[i].decUtil);
        codec_util = CODEC_NORMALIZE(codec_util);
        top_result->sys_current += sm_util + codec_util;
        top_result->user_current += sm_util + codec_util;
      }
    }
  } else {
    LOGGER(FATAL, "unknown env compatibility mode: %d", g_vgpu_config->compatibility_mode);
  }

  LOGGER(VERBOSE, "cuda device: %d, host device: %d, sys util: %d, user util: %d",
         cuda_index, host_index, top_result->sys_current, top_result->user_current);
}

/** hook entrypoint */
CUresult cuDriverGetVersion(int *driverVersion) {
  CUresult ret;

  load_necessary_data();
//  pthread_once(&g_init_set, initialization);

  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDriverGetVersion, driverVersion);
  if (unlikely(ret)) {
    goto DONE;
  }

DONE:
  return ret;
}

CUresult cuInit(unsigned int flag) {
  CUresult ret;

  load_necessary_data();
  pthread_once(&g_init_set, initialization);

  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuInit, flag);
  if (unlikely(ret)) {
    goto DONE;
  }

DONE:
  return ret;
}

CUresult cuGetProcAddress(const char *symbol, void **pfn, int cudaVersion,
                          cuuint64_t flags) {
  CUresult ret;
  int i;

  load_necessary_data();
  pthread_once(&g_init_set, initialization);
  LOGGER(DETAIL, "cuGetProcAddress symbol: %s", symbol);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGetProcAddress, symbol, pfn,
                         cudaVersion, flags);
  if (ret == CUDA_SUCCESS) {
    if (lib_control) {
      void *f = real_dlsym(lib_control, symbol);
      if (likely(f)) {
        LOGGER(DETAIL, "cuGetProcAddress matched symbol: %s", symbol);
        *pfn = f;
        goto DONE;
      }
    }
    for (i = 0; i < cuda_hook_nums; i++) {
      if (!strcmp(symbol, cuda_hooks_entry[i].name)) {
        LOGGER(VERBOSE, "cuGetProcAddress matched symbol: %s", symbol);
        *pfn = cuda_hooks_entry[i].fn_ptr;
        goto DONE;
      }
    }
  }
DONE:
  return ret;
}

CUresult _cuGetProcAddress_v2(const char *symbol, void **pfn, int cudaVersion,
                             cuuint64_t flags, void *symbolStatus) {
  CUresult ret;
  int i;

  load_necessary_data();
  pthread_once(&g_init_set, initialization);
  LOGGER(DETAIL, "cuGetProcAddress_v2 symbol: %s", symbol);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuGetProcAddress_v2, symbol, pfn,
                         cudaVersion, flags, symbolStatus);
  if (ret == CUDA_SUCCESS) {
    if (lib_control) {
      void *f = real_dlsym(lib_control, symbol);
      if (likely(f)) {
        LOGGER(DETAIL, "cuGetProcAddress_v2 matched symbol: %s", symbol);
        *pfn = f;
        goto DONE;
      }
    }
    for (i = 0; i < cuda_hook_nums; i++) {
      if (!strcmp(symbol, cuda_hooks_entry[i].name)) {
        LOGGER(VERBOSE, "cuGetProcAddress_v2 matched symbol: %s", symbol);
        *pfn = cuda_hooks_entry[i].fn_ptr;
        goto DONE;
      }
    }
  }
DONE:
  return ret;
}

CUresult cuGetProcAddress_v2(const char *symbol, void **pfn, int cudaVersion,
                             cuuint64_t flags, void *symbolStatus) {
  CUresult ret;
  ret = _cuGetProcAddress_v2(symbol, pfn, cudaVersion, flags, symbolStatus);
  if (ret == CUDA_SUCCESS && strcmp(symbol,"cuGetProcAddress") == 0) {
    // Compatible with CUDA 12
    *pfn = _cuGetProcAddress_v2;
  }
  return ret;
}

CUresult cuMemAllocManaged(CUdeviceptr *dptr, size_t bytesize, unsigned int flags) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }

  size_t used = 0, vmem_used = 0, request_size = bytesize;
  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);
    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used + request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
    if (g_vgpu_config->devices[host_index].memory_oversold) {
      // Used memory exceeds device memory limit, return OOM
//      if (unlikely(used > g_vgpu_config->devices[host_index].real_memory)) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }
      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
        // The requested memory exceeds the device's memory limit, using global unified memory
        flags = CU_MEM_ATTACH_GLOBAL;
      }
    }
  }
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, flags);
  if (ret == CUDA_SUCCESS && flags == CU_MEM_ATTACH_GLOBAL) {
    malloc_gpu_virt_memory(*dptr, bytesize, host_index);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult _cuMemAlloc(CUdeviceptr *dptr, size_t bytesize) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t used = 0, vmem_used = 0, request_size = bytesize;
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto ALLOCATED_TO_GPU;
  }

  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);
    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used + request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }

    if (g_vgpu_config->devices[host_index].memory_oversold) {
      // Used memory exceeds device memory limit, return OOM
//      if (unlikely(used > g_vgpu_config->devices[host_index].real_memory)) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }

      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
        // The requested memory exceeds the device's memory limit, using global unified memory
        goto ALLOCATED_TO_UVA;
      } else {
        // The requested memory is within the device's memory limit, using device memory
        goto ALLOCATED_TO_GPU;
      }
    }
  }

ALLOCATED_TO_GPU:
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAlloc_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAlloc_v2, dptr, bytesize);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAlloc))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAlloc, dptr, bytesize);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY && host_index >= 0 && g_vgpu_config->devices[host_index].memory_oversold)) {
    LOGGER(VERBOSE, "cuMemAlloc OOM, try using unified memory allocation (oversold), size: %zu, ret: %d, str: %s",
                     request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  } else {
    goto DONE;
  }
ALLOCATED_TO_UVA:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
  LOGGER(VERBOSE, "cuMemAllocManaged to allocate unified memory (oversold), size: %zu, ret: %d, str: %s",
                   request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  if (likely(ret == CUDA_SUCCESS)) {
    malloc_gpu_virt_memory(*dptr, bytesize, host_index);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytesize) {
  return _cuMemAlloc(dptr, bytesize);
}

CUresult cuMemAlloc(CUdeviceptr *dptr, size_t bytesize) {
  return _cuMemAlloc(dptr, bytesize);
}

CUresult _cuMemAllocPitch(CUdeviceptr *dptr, size_t *pPitch, size_t WidthInBytes,
                         size_t Height, unsigned int ElementSizeBytes) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t used = 0, vmem_used = 0;
  // size_t request_size = ROUND_UP(WidthInBytes * Height, ElementSizeBytes);
  size_t guess_pitch = (((WidthInBytes - 1) / ElementSizeBytes) + 1) * ElementSizeBytes;
  size_t request_size = guess_pitch * Height;

  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto ALLOCATED_TO_GPU;
  }
  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);
    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used + request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }

    if (g_vgpu_config->devices[host_index].memory_oversold) {
      // Used memory exceeds device memory limit, return OOM
//      if (unlikely(used > g_vgpu_config->devices[host_index].real_memory)) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }

      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
        // The requested memory exceeds the device's memory limit, using global unified memory
        goto ALLOCATED_TO_UVA;
      } else {
        // The requested memory is within the device's memory limit, using device memory
        goto ALLOCATED_TO_GPU;
      }
    }
  }

ALLOCATED_TO_GPU:
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAllocPitch_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocPitch_v2, dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAllocPitch))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocPitch, dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY && host_index >= 0 && g_vgpu_config->devices[host_index].memory_oversold)) {
    LOGGER(VERBOSE, "cuMemAllocPitch OOM, try using unified memory allocation (oversold), size: %zu, ret: %d, str: %s",
                    request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  } else {
    goto DONE;
  }
ALLOCATED_TO_UVA:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocManaged, dptr, request_size, CU_MEM_ATTACH_GLOBAL);
  LOGGER(VERBOSE, "cuMemAllocManaged to allocate unified memory (oversold), size: %zu, ret: %d, str: %s",
                  request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  if (likely(ret == CUDA_SUCCESS)) {
    *pPitch = guess_pitch;
    malloc_gpu_virt_memory(*dptr, request_size, host_index);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}


CUresult cuMemAllocPitch_v2(CUdeviceptr *dptr, size_t *pPitch, size_t WidthInBytes,
                            size_t Height, unsigned int ElementSizeBytes) {
  return _cuMemAllocPitch(dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
}

CUresult cuMemAllocPitch(CUdeviceptr *dptr, size_t *pPitch, size_t WidthInBytes,
                         size_t Height, unsigned int ElementSizeBytes) {
  return _cuMemAllocPitch(dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
}

CUresult cuMemAllocAsync(CUdeviceptr *dptr, size_t bytesize, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t used = 0, vmem_used = 0, request_size = bytesize;
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto ALLOCATED_TO_GPU;
  }

  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);
    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used + request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }

    if (g_vgpu_config->devices[host_index].memory_oversold) {
      // Used memory exceeds device memory limit, return OOM
//      if (unlikely(used > g_vgpu_config->devices[host_index].real_memory)) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }

      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
        // The requested memory exceeds the device's memory limit, using global unified memory
        goto ALLOCATED_TO_UVA;
      } else {
        // The requested memory is within the device's memory limit, using device memory
        goto ALLOCATED_TO_GPU;
      }
    }
  }
ALLOCATED_TO_GPU:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemAllocAsync), dptr, bytesize, hStream);
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY && host_index >= 0 && g_vgpu_config->devices[host_index].memory_oversold)) {
    LOGGER(VERBOSE, "cuMemAllocAsync OOM, try using unified memory allocation (oversold), size: %zu, ret: %d, str: %s",
                    request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  } else {
    goto DONE;
  }
ALLOCATED_TO_UVA:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
  LOGGER(VERBOSE, "cuMemAllocManaged to allocate unified memory (oversold), size: %zu, ret: %d, str: %s",
                  request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  if (likely(ret == CUDA_SUCCESS)) {
    malloc_gpu_virt_memory(*dptr, bytesize, host_index);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuMemAllocAsync_ptsz(CUdeviceptr *dptr, size_t bytesize, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t used = 0, vmem_used = 0, request_size = bytesize;
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto ALLOCATED_TO_GPU;
  }

  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);
    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used + request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }

    if (g_vgpu_config->devices[host_index].memory_oversold) {
      // Used memory exceeds device memory limit, return OOM
//      if (unlikely(used > g_vgpu_config->devices[host_index].real_memory)) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }

      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
        // The requested memory exceeds the device's memory limit, using global unified memory
        goto ALLOCATED_TO_UVA;
      } else {
        // The requested memory is within the device's memory limit, using device memory
        goto ALLOCATED_TO_GPU;
      }

    }
  }
ALLOCATED_TO_GPU:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocAsync_ptsz, dptr, bytesize, hStream);
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY && host_index >= 0 && g_vgpu_config->devices[host_index].memory_oversold)) {
    LOGGER(VERBOSE, "cuMemAllocAsync_ptsz OOM, try using unified memory allocation (oversold), size: %zu, ret: %d, str: %s",
                    request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  } else {
    goto DONE;
  }
ALLOCATED_TO_UVA:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
  LOGGER(VERBOSE, "cuMemAllocManaged to allocate unified memory (oversold), size: %zu, ret: %d, str: %s",
                  request_size, ret, CUDA_ERROR(cuda_library_entry, ret));
  if (likely(ret == CUDA_SUCCESS)) {
    malloc_gpu_virt_memory(*dptr, request_size, host_index);
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

static size_t get_array_base_size(int format) {
  size_t base_size = 0;

  switch (format) {
  case CU_AD_FORMAT_UNSIGNED_INT8:
  case CU_AD_FORMAT_SIGNED_INT8:
    base_size = 8;
    break;
  case CU_AD_FORMAT_UNSIGNED_INT16:
  case CU_AD_FORMAT_SIGNED_INT16:
  case CU_AD_FORMAT_HALF:
    base_size = 16;
    break;
  case CU_AD_FORMAT_UNSIGNED_INT32:
  case CU_AD_FORMAT_SIGNED_INT32:
  case CU_AD_FORMAT_FLOAT:
    base_size = 32;
    break;
  default:
    base_size = 32;
  }

  return base_size;
}


CUresult _cuArrayCreate(CUarray *pHandle, const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  size_t used = 0, vmem_used = 0, base_size = 0, request_size = 0;
  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);
    base_size = get_array_base_size(pAllocateArray->Format);
    request_size = base_size * pAllocateArray->NumChannels *
                   pAllocateArray->Height * pAllocateArray->Width;

    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used + request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }

//    if (g_vgpu_config->devices[host_index].memory_oversold) {
//      // Used memory exceeds device memory limit, return OOM
//      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }
//    }
  }
CALL:
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArrayCreate_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayCreate_v2, pHandle, pAllocateArray);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArrayCreate))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayCreate, pHandle, pAllocateArray);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuArrayCreate_v2(CUarray *pHandle, const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  return _cuArrayCreate(pHandle, pAllocateArray);
}

CUresult cuArrayCreate(CUarray *pHandle, const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  return _cuArrayCreate(pHandle, pAllocateArray);
}

CUresult _cuArray3DCreate(CUarray *pHandle, const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  size_t used = 0, vmem_used = 0, base_size = 0, request_size = 0;
  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);
    base_size = get_array_base_size(pAllocateArray->Format);
    request_size = base_size * pAllocateArray->NumChannels *
                   pAllocateArray->Height * pAllocateArray->Width *
                   pAllocateArray->Depth;

    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used+ request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }

//    if (g_vgpu_config->devices[host_index].memory_oversold) {
//      // Used memory exceeds device memory limit, return OOM
//      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }
//    }
  }
CALL:
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArray3DCreate_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArray3DCreate_v2, pHandle, pAllocateArray);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArray3DCreate))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArray3DCreate, pHandle, pAllocateArray);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuArray3DCreate_v2(CUarray *pHandle, const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  return _cuArray3DCreate(pHandle, pAllocateArray);
}

CUresult cuArray3DCreate(CUarray *pHandle, const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  return _cuArray3DCreate(pHandle, pAllocateArray);
}

CUresult cuMipmappedArrayCreate(CUmipmappedArray *pHandle,
                                const CUDA_ARRAY3D_DESCRIPTOR *pMipmappedArrayDesc,
                                unsigned int numMipmapLevels) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  size_t used = 0, vmem_used = 0, base_size = 0, request_size = 0;
  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);

    base_size = get_array_base_size(pMipmappedArrayDesc->Format);
    request_size = base_size * pMipmappedArrayDesc->NumChannels *
                   pMipmappedArrayDesc->Height * pMipmappedArrayDesc->Width *
                   pMipmappedArrayDesc->Depth;

    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used + request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }

//    if (g_vgpu_config->devices[host_index].memory_oversold) {
//      // Used memory exceeds device memory limit, return OOM
//      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }
//    }
  }
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMipmappedArrayCreate, pHandle,
                        pMipmappedArrayDesc, numMipmapLevels);
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuMemCreate(CUmemGenericAllocationHandle *handle, size_t size,
                    const CUmemAllocationProp *prop, unsigned long long flags) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  size_t used = 0, vmem_used = 0, request_size = size;
  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);
    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used + request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }

//    if (g_vgpu_config->devices[host_index].memory_oversold) {
//      // Used memory exceeds device memory limit, return OOM
//      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }
//    }
  }
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemCreate, handle, size, prop, flags);
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult _cuDeviceTotalMem(size_t *bytes, CUdevice dev) {
  CUresult ret;
  int host_index = get_host_device_index_by_cuda_device(dev);
  if (host_index < 0) {
    goto CALL;
  }
  if (g_vgpu_config->devices[host_index].memory_limit) {
    *bytes = g_vgpu_config->devices[host_index].total_memory;
    return CUDA_SUCCESS;
  }
CALL:
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDeviceTotalMem_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceTotalMem_v2, bytes, dev);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDeviceTotalMem))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceTotalMem, bytes, dev);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  return ret;
}

CUresult cuDeviceTotalMem_v2(size_t *bytes, CUdevice dev) {
  return _cuDeviceTotalMem(bytes, dev);
}

CUresult cuDeviceTotalMem(size_t *bytes, CUdevice dev) {
  return _cuDeviceTotalMem(bytes, dev);
}

CUresult _cuMemGetInfo(size_t *free, size_t *total) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  size_t used = 0, vmem_used = 0;
  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);
    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    size_t total_memory = g_vgpu_config->devices[host_index].total_memory;
    *total = total_memory;
    *free = (used + vmem_used) >= total_memory ? 0 : (total_memory - used - vmem_used);
    ret = CUDA_SUCCESS;
    goto DONE;
  }
CALL:
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetInfo_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetInfo_v2, free, total);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetInfo))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetInfo, free, total);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuMemGetInfo_v2(size_t *free, size_t *total) {
  return _cuMemGetInfo(free, total);
}

CUresult cuMemGetInfo(size_t *free, size_t *total) {
  return _cuMemGetInfo(free, total);
}

CUresult cuLaunchKernel_ptsz(CUfunction f, unsigned int gridDimX,
                             unsigned int gridDimY, unsigned int gridDimZ,
                             unsigned int blockDimX, unsigned int blockDimY,
                             unsigned int blockDimZ,
                             unsigned int sharedMemBytes, CUstream hStream,
                             void **kernelParams, void **extra) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(gridDimX * gridDimY * gridDimZ,
              blockDimX * blockDimY * blockDimZ, device);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchKernel_ptsz, f, gridDimX,
                         gridDimY, gridDimZ, blockDimX, blockDimY, blockDimZ,
                         sharedMemBytes, hStream, kernelParams, extra);
DONE:
  return ret;
}

CUresult cuLaunchKernel(CUfunction f, unsigned int gridDimX,
                        unsigned int gridDimY, unsigned int gridDimZ,
                        unsigned int blockDimX, unsigned int blockDimY,
                        unsigned int blockDimZ, unsigned int sharedMemBytes,
                        CUstream hStream, void **kernelParams, void **extra) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(gridDimX * gridDimY * gridDimZ,
              blockDimX * blockDimY * blockDimZ, device);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuLaunchKernel), f, gridDimX,
                         gridDimY, gridDimZ, blockDimX, blockDimY, blockDimZ,
                         sharedMemBytes, hStream, kernelParams, extra);
DONE:
  return ret;
}

CUresult cuLaunchKernelEx(CUlaunchConfig *config, CUfunction f, 
                          void **kernelParams, void **extra) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(config->gridDimX * config->gridDimY * config->gridDimZ,
               config->blockDimX * config->blockDimY * config->blockDimZ, device);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuLaunchKernelEx),
                         config, f, kernelParams, extra);
DONE:
  return ret;
}

CUresult cuLaunchKernelEx_ptsz(CUlaunchConfig *config, CUfunction f, 
                               void **kernelParams, void **extra) {
  CUresult ret; 
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(config->gridDimX *config->gridDimY * config->gridDimZ,
               config->blockDimX * config->blockDimY * config->blockDimZ, device);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchKernelEx_ptsz, 
                         config, f, kernelParams, extra);
DONE:
  return ret;
}

CUresult cuLaunch(CUfunction f) {
  CUresult ret; 
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  rate_limiter(1, g_block_x[host_index] * g_block_y[host_index] * g_block_z[host_index], device);
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunch, f);
DONE:
  return ret;
}

CUresult cuLaunchCooperativeKernel_ptsz(
    CUfunction f, unsigned int gridDimX, unsigned int gridDimY,
    unsigned int gridDimZ, unsigned int blockDimX, unsigned int blockDimY,
    unsigned int blockDimZ, unsigned int sharedMemBytes, CUstream hStream,
    void **kernelParams) {
  CUdevice device;
  CUresult ret;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(gridDimX * gridDimY * gridDimZ,
               blockDimX * blockDimY * blockDimZ, device);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchCooperativeKernel_ptsz, f,
                         gridDimX, gridDimY, gridDimZ, blockDimX, blockDimY,
                         blockDimZ, sharedMemBytes, hStream, kernelParams);
DONE:
  return ret;
}

CUresult cuLaunchCooperativeKernel(CUfunction f,
    unsigned int gridDimX, unsigned int gridDimY, unsigned int gridDimZ,
    unsigned int blockDimX, unsigned int blockDimY, unsigned int blockDimZ,
    unsigned int sharedMemBytes, CUstream hStream, void **kernelParams) {
  CUdevice device;
  CUresult ret; 
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }    
  rate_limiter(gridDimX * gridDimY * gridDimZ,
               blockDimX * blockDimY * blockDimZ, device);
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuLaunchCooperativeKernel), f,
                         gridDimX, gridDimY, gridDimZ, blockDimX, blockDimY,
                         blockDimZ, sharedMemBytes, hStream, kernelParams);
DONE:
  return ret;
}

CUresult cuLaunchGrid(CUfunction f, int grid_width, int grid_height) {
  CUresult ret;  
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  rate_limiter(grid_width * grid_height, g_block_x[host_index] * g_block_y[host_index] * g_block_z[host_index], device);
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchGrid, f, grid_width,grid_height);
DONE:
  return ret;
}

CUresult cuLaunchGridAsync(CUfunction f, int grid_width, int grid_height, CUstream hStream) {
  CUresult ret;  
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  rate_limiter(grid_width * grid_height, g_block_x[host_index] * g_block_y[host_index] * g_block_z[host_index], device);
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuLaunchGridAsync, f, grid_width, grid_height, hStream);
DONE:
  return ret;
}

CUresult cuFuncSetBlockShape(CUfunction hfunc, int x, int y, int z) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  if (g_vgpu_config->devices[host_index].core_limit) {
    while (!CAS(&g_block_locker[host_index], 0, 1)) {}

    g_block_x[host_index] = x;
    g_block_y[host_index] = y;
    g_block_z[host_index] = z;

    LOGGER(VERBOSE, "cuda device %d => host device %d, set block shape: %d, %d, %d", device, host_index, x, y, z);

    while (!CAS(&g_block_locker[host_index], 1, 0)) {}
  }
CALL:
  ret =  CUDA_ENTRY_CHECK(cuda_library_entry, cuFuncSetBlockShape, hfunc, x, y, z);
DONE:
  return ret;
}

CUresult cuMemAllocFromPoolAsync(CUdeviceptr *dptr, size_t bytesize,
                                 CUmemoryPool pool, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }
  size_t used = 0, vmem_used = 0, request_size = bytesize;
  if (g_vgpu_config->devices[host_index].memory_limit) {

    lock_fd = lock_gpu_device(host_index);
    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used + request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }

//    if (g_vgpu_config->devices[host_index].memory_oversold) {
//      // Used memory exceeds device memory limit, return OOM
//      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }
//    }
  }
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemAllocFromPoolAsync), dptr, bytesize, pool, hStream);
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuMemAllocFromPoolAsync_ptsz(CUdeviceptr *dptr, size_t bytesize,
                                      CUmemoryPool pool, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  int lock_fd = -1;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) {
    goto CALL;
  }

  size_t used = 0, vmem_used = 0, request_size = bytesize;
  if (g_vgpu_config->devices[host_index].memory_limit) {
    lock_fd = lock_gpu_device(host_index);

    get_used_gpu_memory((void *)&used, device);
    get_used_gpu_virt_memory((void *)&vmem_used, host_index);

    // Exceeded total memory, return OOM
    if ((used + vmem_used + request_size) > g_vgpu_config->devices[host_index].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }

//    if (g_vgpu_config->devices[host_index].memory_oversold) {
//      // Used memory exceeds device memory limit, return OOM
//      if ((used + request_size) > g_vgpu_config->devices[host_index].real_memory) {
//        ret = CUDA_ERROR_OUT_OF_MEMORY;
//        goto DONE;
//      }
//    }
  }
CALL:
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocFromPoolAsync_ptsz, dptr, bytesize, pool, hStream);
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult _cuMemFree(CUdeviceptr dptr) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemFree_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemFree_v2, dptr);
  } else if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemFree))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemFree, dptr);
  } else {
    ret = CUDA_ERROR_NOT_FOUND;
  }
  if (likely(ret == CUDA_SUCCESS)) {
    free_gpu_virt_memory(dptr, get_host_device_index_by_cuda_device(device));
  }
DONE:
  return ret;
}

CUresult cuMemFree_v2(CUdeviceptr dptr) {
  return _cuMemFree(dptr);
}

CUresult cuMemFree(CUdeviceptr dptr) {
  return _cuMemFree(dptr);
}

CUresult cuMemFreeAsync(CUdeviceptr dptr, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuMemFreeAsync), dptr, hStream);
  if (likely(ret == CUDA_SUCCESS)) {
    free_gpu_virt_memory(dptr, get_host_device_index_by_cuda_device(device));
  }
DONE:
  return ret;
}

CUresult cuMemFreeAsync_ptsz(CUdeviceptr dptr, CUstream hStream) {
  CUresult ret;
  CUdevice device;
  ret = CUDA_INTERNAL_CHECK(cuda_library_entry, cuCtxGetDevice, &device);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemFreeAsync_ptsz, dptr, hStream);
  if (likely(ret == CUDA_SUCCESS)) {
    free_gpu_virt_memory(dptr, get_host_device_index_by_cuda_device(device));
  }
DONE:
  return ret;
}

