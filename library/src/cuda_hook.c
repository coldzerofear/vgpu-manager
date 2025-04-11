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
#include <pthread.h>
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

extern resource_data_t g_vgpu_config;
extern char container_id[FILENAME_MAX];
extern entry_t cuda_library_entry[];
extern entry_t nvml_library_entry[];

extern int lock_gpu_device(int device);
extern void unlock_gpu_device(int fd);

extern fp_dlsym real_dlsym;
extern void *lib_control;

extern int extract_container_pids(char *base_path, int **pids, int *pids_size);

static pthread_once_t g_init_set = PTHREAD_ONCE_INIT;
static pthread_once_t g_nvml_init = PTHREAD_ONCE_INIT;

static volatile int64_t g_cur_cuda_cores[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static volatile int64_t g_total_cuda_cores[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

static int g_sm_num[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static int g_max_thread_per_sm[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

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

static void get_used_gpu_utilization(void *, int, nvmlDevice_t);

static void init_device_cuda_cores(unsigned int *devCount);

static void initialization();

static void rate_limiter(int, int, int);

static void change_token(int64_t, int);

static const char *nvml_error(nvmlReturn_t);

static const char *cuda_error(CUresult, const char **);

static int64_t delta(int, int, int64_t, int);

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
};

const int cuda_hook_nums =
    sizeof(cuda_hooks_entry) / sizeof(cuda_hooks_entry[0]);

/** dynamic rate control */
typedef struct {
  int user_current;
  int sys_current;
  uint64_t checktime;
  int valid;
  int sys_process_num;
} utilization_t;

dynamic_config_t g_dynamic_config = {
  .change_limit_interval = 30,
  .usage_threshold = 5,
  .error_recovery_step = 10
};

const char *nvml_error(nvmlReturn_t code) {
  const char *(*err_fn)(nvmlReturn_t) = NULL;

  err_fn = nvml_library_entry[NVML_ENTRY_ENUM(nvmlErrorString)].fn_ptr;
  if (unlikely(!err_fn)) {
    LOGGER(ERROR, "can't find nvmlErrorString");
    static char fallback_error[32];
    snprintf(fallback_error, sizeof(fallback_error), "NVML Error (code=%d)", (int)code);
    return fallback_error;
  }

  return err_fn(code);
}

const char *cuda_error(CUresult code, const char **p) {
  CUDA_ENTRY_CALL(cuda_library_entry, cuGetErrorString, code, p);
  return *p;
}

static int check_file_exist(const char *file_path) {
  int ret = 0;
  if (access(file_path, F_OK) == 0) {
      ret = 1;
  }
  return ret;
}

static void nvml_init() {
  nvmlReturn_t rt;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlInitWithFlags))) {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlInitWithFlags, 0);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlInit_v2))) {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlInit_v2);
  } else {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlInit);
  }
  if (unlikely(rt)) {
    LOGGER(FATAL, "nvmlInit failed, return %d, str: %s", rt, nvml_error(rt));
  }
}

static void change_token(int64_t delta, int device_id) {
  int64_t cuda_cores_before = 0, cuda_cores_after = 0;

  LOGGER(DETAIL, "device: %d, delta: %ld, curr: %ld", device_id, delta, g_cur_cuda_cores[device_id]);
  do {
    cuda_cores_before = g_cur_cuda_cores[device_id];
    cuda_cores_after = cuda_cores_before + delta;

    if (unlikely(cuda_cores_after > g_total_cuda_cores[device_id])) {
      cuda_cores_after = g_total_cuda_cores[device_id];
    } else if (unlikely(cuda_cores_after < 0)) {
      cuda_cores_after = 0;
    }
  } while (!CAS(&g_cur_cuda_cores[device_id], cuda_cores_before, cuda_cores_after));
}

static void rate_limiter(int grids, int blocks, int device_id) {
  if (g_vgpu_config.devices[device_id].core_limit) {
    int64_t before_cuda_cores = 0;
    int64_t after_cuda_cores = 0;
    int64_t kernel_size = (int64_t) grids;

    LOGGER(VERBOSE, "device: %d, grid: %d, blocks: %d", device_id , grids, blocks);
    LOGGER(VERBOSE, "device: %d, launch kernel: %ld, curr core: %ld", device_id, kernel_size, g_cur_cuda_cores[device_id]);
    do {
    CHECK:
      before_cuda_cores = g_cur_cuda_cores[device_id];
      LOGGER(DETAIL, "device: %d, current core: %ld", device_id, before_cuda_cores);
      if (before_cuda_cores < 0) {
        nanosleep(&g_cycle, NULL);
        goto CHECK;
      }
      after_cuda_cores = before_cuda_cores - kernel_size;
    } while (!CAS(&g_cur_cuda_cores[device_id], before_cuda_cores, after_cuda_cores));
  }
}

static int64_t delta(int up_limit, int user_current, int64_t share, int device_index) {
  // 1. 使用更宽的数据类型防止计算溢出
  int64_t sm_num = (int64_t)g_sm_num[device_index];
  int64_t max_thread = (int64_t)g_max_thread_per_sm[device_index];

  // 2. 计算利用率差异
  int utilization_diff = abs(up_limit - user_current);
  if (utilization_diff < MIN_INCREMENT) {
    utilization_diff = MIN_INCREMENT;
  }

  // 3. 计算增量（使用64位运算防止溢出）
  int64_t increment = sm_num * sm_num * max_thread * (int64_t)(utilization_diff) / INCREMENT_SCALE_FACTOR;

  // 4. 加速调整逻辑（使用浮点阈值代替硬编码）
  if ((float)utilization_diff / (float)(up_limit) > MAX_UTIL_DIFF_THRESHOLD) {
    increment = increment * utilization_diff * 2 / (up_limit + 1);
  }

  // 5. 错误处理优化：负增量时不再终止进程，而是回退到安全值
  if (unlikely(increment < 0 || increment > INT_MAX)) {
    LOGGER(ERROR, "device %d, increment overflow: %ld, current sm: %ld, thread_per_sm: %ld, diff: %d",
           device_index, increment, sm_num, max_thread, utilization_diff);
    increment = g_dynamic_config.error_recovery_step;  // 使用安全步长
  }

  if (user_current <= up_limit) {
    share = (share + increment) > g_total_cuda_cores[device_index] ?
            g_total_cuda_cores[device_index] : (share + increment);
  } else {
    share = (share - increment) < 0 ? 0 : (share - increment);
  }

  return share;
}

static void *utilization_watcher(void *arg) {
  batch_t *batch = (batch_t *)arg;
  LOGGER(VERBOSE, "start %s batch code %d", __FUNCTION__, batch->batch_code);
  LOGGER(VERBOSE, "batch code %d, start index %d, end index %d", batch->batch_code, batch->start_index, batch->end_index);
  int64_t shares[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
  int sys_frees[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
  int avg_sys_frees[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
  int is[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
  int pre_sys_process_nums[MAX_DEVICE_COUNT] = {1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1};

  utilization_t top_results[MAX_DEVICE_COUNT] = {};
  int up_limits[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
  nvmlDevice_t devices[MAX_DEVICE_COUNT] = {};

  int device_id;
  int need_limit = 0;
  for (device_id = batch->start_index; device_id < batch->end_index; device_id++) {
    if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2))) {
      NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2, device_id, &devices[device_id]);
    } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex))) {
      NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByIndex, device_id, &devices[device_id]);
    } else {
      LOGGER(WARNING, "nvmlDeviceGetHandleByIndex function not found");
    }
    up_limits[device_id] = g_vgpu_config.devices[device_id].hard_core;
    top_results[device_id].user_current = 0;
    top_results[device_id].sys_current = 0;
    top_results[device_id].valid = 0;
    top_results[device_id].sys_process_num = 0;
    if (g_vgpu_config.devices[device_id].core_limit) {
      need_limit = 1;
    }
  }
  if (likely(!need_limit)) {
    LOGGER(VERBOSE, "no need cuda core limit for batch %d", batch->batch_code);
    return NULL;
  }
  int dev_count = batch->end_index - batch->start_index;
  struct timespec wait = {
      .tv_sec = 0,
      .tv_nsec = 100 / dev_count * MILLISEC,
  };
  while (1) {
    for (device_id = batch->start_index; device_id < batch->end_index; device_id++) {
      nanosleep(&wait, NULL);
      if (!g_vgpu_config.devices[device_id].core_limit) {
        continue; // Skip GPU without core limit enabled
      }

      get_used_gpu_utilization((void *)&top_results[device_id], device_id, devices[device_id]);
      if (unlikely(!top_results[device_id].valid)) {
        continue;
      }

      sys_frees[device_id] = MAX_UTILIZATION - top_results[device_id].sys_current;

      if (g_vgpu_config.devices[device_id].hard_limit) {
        /* Avoid usage jitter when application is initialized*/
        if (top_results[device_id].sys_process_num == 1 && top_results[device_id].user_current < up_limits[device_id] / 10) {
          g_cur_cuda_cores[device_id] =
              delta(g_vgpu_config.devices[device_id].hard_core, top_results[device_id].user_current, shares[device_id], device_id);
          continue;
        }
        shares[device_id] = delta(g_vgpu_config.devices[device_id].hard_core, top_results[device_id].user_current, shares[device_id], device_id);
      } else {
        if (pre_sys_process_nums[device_id] != top_results[device_id].sys_process_num) {
          /* When a new process comes, all processes are reset to initial value*/
          if (pre_sys_process_nums[device_id] < top_results[device_id].sys_process_num) {
            shares[device_id] = (int64_t) g_max_thread_per_sm[device_id];
            up_limits[device_id] = g_vgpu_config.devices[device_id].hard_core;
            is[device_id] = 0;
            avg_sys_frees[device_id] = 0;
          }
          pre_sys_process_nums[device_id] = top_results[device_id].sys_process_num;
        }

        /* 1.Only one process on the GPU
         * Allocate cuda cores according to the limit value.
         *
         * 2.Multiple processes on the GPU
         * First, change the up_limit of the process according to the
         * historical resource utilization. Second, allocate the cuda
         * cores according to the changed limit value.*/
        if (top_results[device_id].sys_process_num == 1) {
          up_limits[device_id] = g_vgpu_config.devices[device_id].soft_core;
          shares[device_id] = delta(up_limits[device_id], top_results[device_id].user_current, shares[device_id], device_id);
        } else {
          is[device_id]++;
          avg_sys_frees[device_id] += sys_frees[device_id];
          if (is[device_id] % g_dynamic_config.change_limit_interval == 0) {
            if (avg_sys_frees[device_id] * 2 / g_dynamic_config.change_limit_interval > g_dynamic_config.usage_threshold) {
              up_limits[device_id] = up_limits[device_id] + g_vgpu_config.devices[device_id].hard_core / 10 > g_vgpu_config.devices[device_id].soft_core ?
                         g_vgpu_config.devices[device_id].soft_core : up_limits[device_id] + g_vgpu_config.devices[device_id].hard_core / 10;
            }
            is[device_id] = 0;
          }
          avg_sys_frees[device_id] = is[device_id] % (g_dynamic_config.change_limit_interval / 2) == 0 ? 0 : avg_sys_frees[device_id];
          shares[device_id] = delta(up_limits[device_id], top_results[device_id].user_current, shares[device_id], device_id);
        }
      }
      change_token(shares[device_id], device_id);
      LOGGER(DETAIL, "device: %d, user util: %d, up_limit: %d, share: %ld, curr core: %ld", device_id,
             top_results[device_id].user_current, up_limits[device_id], shares[device_id], g_cur_cuda_cores[device_id]);
    }
  }
}

static batch_t batches[MAX_DEVICE_COUNT / DEVICE_BATCH_SIZE] = {};

static void active_utilization_notifier(int batch_code) {
  pthread_t tid;
  pthread_create(&tid, NULL, utilization_watcher, &batches[batch_code]);
  char thread_name[32] = {0};
  sprintf(thread_name, "utilization_watcher_batch_%d", batches[batch_code].batch_code);
#ifdef __APPLE__
  pthread_setname_np(thread_name);
#else
  pthread_setname_np(tid, thread_name);
#endif
}

static void init_device_cuda_cores(unsigned int *devCount) {
  nvmlReturn_t rt;
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetCount))) {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetCount, devCount);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetCount_v2))) {
    rt = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetCount_v2, devCount);
  } else {
    rt = NVML_ERROR_FUNCTION_NOT_FOUND;
    LOGGER(WARNING, "nvmlDeviceGetCount function not found");
  }
  if (unlikely(rt)) {
    LOGGER(FATAL, "nvmlDeviceGetCount failed, return %d, str: %s", rt, nvml_error(rt));
  }
  int ret;
  const char *cuda_err_string = NULL;
  for (int i = 0; i < *devCount; i++) {
    ret = CUDA_ENTRY_CALL(cuda_library_entry, cuDeviceGetAttribute, &g_sm_num[i],
                          CU_DEVICE_ATTRIBUTE_MULTIPROCESSOR_COUNT, i);
    if (unlikely(ret)) {
      LOGGER(FATAL, "can't get processor number, device %d, error %s", i,
            cuda_error((CUresult)ret, &cuda_err_string));
    }
    ret = CUDA_ENTRY_CALL(cuda_library_entry, cuDeviceGetAttribute, &g_max_thread_per_sm[i],
                          CU_DEVICE_ATTRIBUTE_MAX_THREADS_PER_MULTIPROCESSOR, i);
    if (unlikely(ret)) {
      LOGGER(FATAL, "can't get max thread per processor, device %d, error %s",
                  i, cuda_error((CUresult)ret, &cuda_err_string));
    }
    g_total_cuda_cores[i] = (int64_t)g_max_thread_per_sm[i] * (int64_t)(g_sm_num[i]) * FACTOR;
    LOGGER(VERBOSE, "device %d total cuda cores: %ld", i, g_total_cuda_cores[i]);
  }
}

static void initialization() {
  int ret;
  const char *cuda_err_string = NULL;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuInit, 0);
  if (unlikely(ret)) {
    LOGGER(FATAL, "cuInit error %s", cuda_error((CUresult)ret, &cuda_err_string));
  }
  pthread_once(&g_nvml_init, nvml_init);
  unsigned int deviceCount;
  init_device_cuda_cores(&deviceCount);
  int batch_count = (deviceCount + DEVICE_BATCH_SIZE - 1) / DEVICE_BATCH_SIZE;
  for (int i = 0; i < batch_count; i++) {
    batches[i].start_index = i * DEVICE_BATCH_SIZE;
    batches[i].end_index = (i + 1) * DEVICE_BATCH_SIZE;
    batches[i].batch_code = i;
    if (batches[i].end_index > deviceCount) {
      batches[i].end_index = deviceCount;
    }
    active_utilization_notifier(i);
  }
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
  if (pid == 0) {
    goto DONE;
  }
  char pid_path[128] = "";
  sprintf(pid_path, HOST_PROC_CGROUP_PID_PATH, pid);

  char container_cg[256];
  char process_cg[256];

  if (!read_cgroup(PID_SELF_CGROUP_PATH, "memory", container_cg) && !read_cgroup(pid_path, "memory", process_cg)) {
    LOGGER(DETAIL, "\ncontainer cg: %s\nprocess cg: %s", container_cg, process_cg);
    if (strstr(process_cg, container_cg) != NULL) {
      LOGGER(VERBOSE, "cgroup match pid=%d, cg=%s", pid, process_cg);
      return 1;
    }
  }
DONE:
  LOGGER(VERBOSE, "cgroup mismatch pid=%d", pid);
  return 0;
}

int check_container_pid_by_cgroupv2(unsigned int pid) {
  if (pid == 0) {
    goto DONE;
  }
  char pid_path[128] = "";
  sprintf(pid_path, HOST_PROC_CGROUP_PID_PATH, pid);
  if (!check_file_exist(pid_path)) {
    goto DONE;
  }
  FILE *fp = fopen(pid_path, "rb");
  if (!fp) {
    LOGGER(VERBOSE, "read file %s failed: %s", pid_path, strerror(errno));
    goto DONE;
  }
  char buff[FILENAME_MAX];
  while (fgets(buff, FILENAME_MAX, fp)) {
    size_t len = strlen(buff);
    if (len > 0 && buff[len - 1] == '\n') {
      buff[len - 1] = '\0';
    }
    if (strcmp(buff, "0::/") == 0 || strstr(buff, container_id) != NULL) {
      fclose(fp);
      LOGGER(VERBOSE, "cgroup match pid=%d, cg=%s", pid, buff);
      return 1;
    }
  }
  fclose(fp);
DONE:
  LOGGER(VERBOSE, "cgroup mismatch pid=%d", pid);
  return 0;
}

//static int int_compare(const void *a, const void *b) {
//  const int *pa = (const int *)a;
//  const int *pb = (const int *)b;
//  return (*pa > *pb) - (*pa < *pb);
//}

int check_container_pid_by_open_kernel(unsigned int pid, int *pids_on_container, int pids_size) {
  int ret = 0;
  if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) != OPEN_KERNEL_COMPATIBILITY_MODE) {
    return ret;
  }
  if (pid == 0 || !pids_on_container || pids_size <= 0) {
    goto DONE;
  }
//  if (bsearch(&pid, pids_on_container, (size_t)pids_size, sizeof(int), int_compare)) {
//    ret = 1;
//  }
  for (int i = 0; i < pids_size; i++) {
    if (pid == pids_on_container[i]) {
      ret = 1;
      break;
    }
  }
DONE:
  if (ret) {
     LOGGER(VERBOSE, "cgroup match pid=%d", pid);
  } else {
     LOGGER(VERBOSE, "cgroup mismatch pid=%d", pid);
  }
  return ret;
}

void get_used_gpu_memory_by_device(void *arg, nvmlDevice_t device) {
  size_t *used_memory = arg;
  nvmlProcessInfo_t pids_on_device[MAX_PIDS];
  unsigned int size_on_device = MAX_PIDS;
  nvmlReturn_t ret;

  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses,
                           device, &size_on_device, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2,
                           device, &size_on_device, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3,
                           device, &size_on_device, pids_on_device);
  } else {
    ret = NVML_ERROR_FUNCTION_NOT_FOUND;
    LOGGER(WARNING, "nvmlDeviceGetComputeRunningProcesses function not found");
  }
  if (unlikely(ret)) {
    *used_memory = 0;
    return;
  }

  unsigned int i;
  if ((g_vgpu_config.compatibility_mode & CGROUPV2_COMPATIBILITY_MODE) == CGROUPV2_COMPATIBILITY_MODE) {
    LOGGER(VERBOSE, "use cgroupv2 compatibility mode");
    int pids_size = MAX_PIDS;
    int *pids_on_container = NULL;
    for (i = 0; i < size_on_device; i++) {
      if (check_container_pid_by_cgroupv2(pids_on_device[i].pid)) {
        LOGGER(VERBOSE, "pid[%d] compute use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        *used_memory += pids_on_device[i].usedGpuMemory;
      } else if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
        if (!pids_on_container) {
          char proc_path[PATH_MAX];
          snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
          extract_container_pids(proc_path, &pids_on_container, &pids_size);
        }
        if (check_container_pid_by_open_kernel(pids_on_device[i].pid, pids_on_container, pids_size)) {
          LOGGER(VERBOSE, "pid[%d] compute use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
          *used_memory += pids_on_device[i].usedGpuMemory;
        }
      }
    }
    if (pids_on_container) {
      free(pids_on_container);
      pids_on_container = NULL;
    }
  } else if ((g_vgpu_config.compatibility_mode & CGROUPV1_COMPATIBILITY_MODE) == CGROUPV1_COMPATIBILITY_MODE) {
    LOGGER(VERBOSE, "use cgroupv1 compatibility mode");
    int pids_size = MAX_PIDS;
    int *pids_on_container = NULL;
    for (i = 0; i < size_on_device; i++) {
      if (check_container_pid_by_cgroupv1(pids_on_device[i].pid)) {
        LOGGER(VERBOSE, "pid[%d] compute use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        *used_memory += pids_on_device[i].usedGpuMemory;
      } else if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
        if (!pids_on_container) {
          char proc_path[PATH_MAX];
          snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
          extract_container_pids(proc_path, &pids_on_container, &pids_size);
        }
        if (check_container_pid_by_open_kernel(pids_on_device[i].pid, pids_on_container, pids_size)) {
          LOGGER(VERBOSE, "pid[%d] compute use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
          *used_memory += pids_on_device[i].usedGpuMemory;
        }
      }
    }
    if (pids_on_container) {
      free(pids_on_container);
      pids_on_container = NULL;
    }
  } else if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
    LOGGER(VERBOSE, "use open kernel driver compatibility mode");
    int pids_size = MAX_PIDS;
    int *pids_on_container = NULL;
    char proc_path[PATH_MAX];
    snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
    extract_container_pids(proc_path, &pids_on_container, &pids_size);
    for (i = 0; i < size_on_device; i++) {
      if (check_container_pid_by_open_kernel(pids_on_device[i].pid, pids_on_container, pids_size)) {
        LOGGER(VERBOSE, "pid[%d] compute use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
        *used_memory += pids_on_device[i].usedGpuMemory;
      }
    }
    if (pids_on_container) {
      free(pids_on_container);
      pids_on_container = NULL;
    }
  } else if (g_vgpu_config.compatibility_mode == HOST_COMPATIBILITY_MODE) {
    LOGGER(VERBOSE, "use host compatibility mode");
    for (i = 0; i < size_on_device; i++) { // Host mode does not verify PID
      LOGGER(VERBOSE, "pid[%d] compute use memory: %lld", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory);
      *used_memory += pids_on_device[i].usedGpuMemory;
    }
  } else {
    LOGGER(FATAL, "unknown env compatibility mode: %d", g_vgpu_config.compatibility_mode);
  }

  // TODO　Increase the memory usage of intercepting graphic processes.
  size_on_device = MAX_PIDS;
  nvmlProcessInfo_t graphic_pids_on_device[MAX_PIDS];

  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses,
                           device, &size_on_device, graphic_pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v2))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v2,
                           device, &size_on_device, graphic_pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v3))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetGraphicsRunningProcesses_v3,
                           device, &size_on_device, graphic_pids_on_device);
  } else {
    ret = NVML_ERROR_FUNCTION_NOT_FOUND;
    LOGGER(WARNING, "nvmlDeviceGetGraphicsRunningProcesses function not found");
  }
  if (unlikely(ret)) {
    goto DONE;
  }

  if ((g_vgpu_config.compatibility_mode & CGROUPV2_COMPATIBILITY_MODE) == CGROUPV2_COMPATIBILITY_MODE) {
    int pids_size = MAX_PIDS;
    int *pids_on_container = NULL;
    for (i = 0; i < size_on_device; i++) {
      if (check_container_pid_by_cgroupv2(graphic_pids_on_device[i].pid)) {
        LOGGER(VERBOSE, "pid[%d] graphics use memory: %lld", graphic_pids_on_device[i].pid, graphic_pids_on_device[i].usedGpuMemory);
        *used_memory += graphic_pids_on_device[i].usedGpuMemory;
      } else if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
        if (!pids_on_container) {
          char proc_path[PATH_MAX];
          snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
          extract_container_pids(proc_path, &pids_on_container, &pids_size);
        }
        if (check_container_pid_by_open_kernel(graphic_pids_on_device[i].pid, pids_on_container, pids_size)) {
          LOGGER(VERBOSE, "pid[%d] graphics use memory: %lld", graphic_pids_on_device[i].pid, graphic_pids_on_device[i].usedGpuMemory);
          *used_memory += graphic_pids_on_device[i].usedGpuMemory;
        }
      }
    }
    if (pids_on_container) {
      free(pids_on_container);
      pids_on_container = NULL;
    }
  } else if ((g_vgpu_config.compatibility_mode & CGROUPV1_COMPATIBILITY_MODE) == CGROUPV1_COMPATIBILITY_MODE) {
    int pids_size = MAX_PIDS;
    int *pids_on_container = NULL;
    for (i = 0; i < size_on_device; i++) {
      if (check_container_pid_by_cgroupv1(graphic_pids_on_device[i].pid)) {
        LOGGER(VERBOSE, "pid[%d] graphics use memory: %lld", graphic_pids_on_device[i].pid, graphic_pids_on_device[i].usedGpuMemory);
        *used_memory += graphic_pids_on_device[i].usedGpuMemory;
      } else if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
        if (!pids_on_container) {
          char proc_path[PATH_MAX];
          snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
          extract_container_pids(proc_path, &pids_on_container, &pids_size);
        }
        if (check_container_pid_by_open_kernel(graphic_pids_on_device[i].pid, pids_on_container, pids_size)) {
          LOGGER(VERBOSE, "pid[%d] graphics use memory: %lld", graphic_pids_on_device[i].pid, graphic_pids_on_device[i].usedGpuMemory);
          *used_memory += graphic_pids_on_device[i].usedGpuMemory;
        }
      }
    }
    if (pids_on_container) {
      free(pids_on_container);
      pids_on_container = NULL;
    }
  } else if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
    int pids_size = MAX_PIDS;
    int *pids_on_container = NULL;
    char proc_path[PATH_MAX];
    snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
    extract_container_pids(proc_path, &pids_on_container, &pids_size);
    for (i = 0; i < size_on_device; i++) {
      if (check_container_pid_by_open_kernel(graphic_pids_on_device[i].pid, pids_on_container, pids_size)) {
        LOGGER(VERBOSE, "pid[%d] graphics use memory: %lld", graphic_pids_on_device[i].pid, graphic_pids_on_device[i].usedGpuMemory);
        *used_memory += graphic_pids_on_device[i].usedGpuMemory;
      }
    }
    if (pids_on_container) {
      free(pids_on_container);
      pids_on_container = NULL;
    }
  } else if (g_vgpu_config.compatibility_mode == HOST_COMPATIBILITY_MODE) {
    for (i = 0; i < size_on_device; i++) {
      LOGGER(VERBOSE, "pid[%d] graphics use memory: %lld", graphic_pids_on_device[i].pid, graphic_pids_on_device[i].usedGpuMemory);
      *used_memory += graphic_pids_on_device[i].usedGpuMemory;
    }
  }

DONE:
  LOGGER(VERBOSE, "total used memory: %zu", *used_memory);
}

void get_used_gpu_memory(void *arg, CUdevice device_id) {
  size_t *used_memory = arg;
  nvmlDevice_t dev;
  nvmlReturn_t ret;
  pthread_once(&g_nvml_init, nvml_init);
  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByIndex_v2, device_id, &dev);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetHandleByIndex))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetHandleByIndex, device_id, &dev);
  } else {
    ret = NVML_ERROR_FUNCTION_NOT_FOUND;
    LOGGER(WARNING, "nvmlDeviceGetHandleByIndex function not found");
  }
  if (unlikely(ret)) {
    *used_memory = 0;
    return;
  }

  get_used_gpu_memory_by_device((void *)used_memory, dev);
}

static void get_used_gpu_utilization(void *arg, int device_id, nvmlDevice_t dev) {
  nvmlProcessUtilizationSample_t processes_sample[MAX_PIDS];
  unsigned int processes_num = MAX_PIDS;
  unsigned int running_processes = MAX_PIDS;
  nvmlProcessInfo_t pids_on_device[MAX_PIDS];
  utilization_t *top_result = (utilization_t *)arg;

  nvmlReturn_t ret;
  struct timeval cur, prev;
  int i;

  if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses,
                             dev, &running_processes, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v2,
                             dev, &running_processes, pids_on_device);
  } else if (likely(NVML_FIND_ENTRY(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3))) {
    ret = NVML_ENTRY_CHECK(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses_v3,
                             dev, &running_processes, pids_on_device);
  } else {
    ret = NVML_ERROR_FUNCTION_NOT_FOUND;
    LOGGER(WARNING, "nvmlDeviceGetComputeRunningProcesses function not found");
  }
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetComputeRunningProcesses can't get pids on device %d, "
           "return %d, str: %s", device_id, ret, nvml_error(ret));
    return;
  }

  top_result->sys_process_num = running_processes;

  gettimeofday(&cur, NULL);
  struct timeval temp = {1, 0};
  timersub(&cur, &temp, &prev);
  uint64_t microsec = (uint64_t)prev.tv_sec * 1000000ULL + prev.tv_usec;
  top_result->checktime = microsec;

  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetProcessUtilization,
                        dev, processes_sample, &processes_num, microsec);
  if (unlikely(ret)) {
    if (ret != NVML_ERROR_NOT_FOUND) {
      LOGGER(VERBOSE, "nvmlDeviceGetProcessUtilization can't get process utilization on device: %d, "
                "return %d, str: %s", device_id, ret, nvml_error(ret));
    }
    return;
  }

  // When using open source kernel modules, nvmlDevice∝mputeRunningProcesses can only
  // query processes in the container namespace, while nvmlDeviceVNet cessUtilization
  // can query global processes, so it may need to be updated to the global process count here.
  if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) ==
       OPEN_KERNEL_COMPATIBILITY_MODE && processes_num > running_processes) {
    top_result->sys_process_num = processes_num;
  }

  top_result->user_current = 0;
  top_result->sys_current = 0;

  int sm_util = 0;
  int codec_util = 0;

  if ((g_vgpu_config.compatibility_mode & CGROUPV2_COMPATIBILITY_MODE) == CGROUPV2_COMPATIBILITY_MODE) {
    int pids_size = MAX_PIDS;
    int *pids_on_container = NULL;
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
        } else if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
          if (!pids_on_container) {
            char proc_path[PATH_MAX];
            snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
            extract_container_pids(proc_path, &pids_on_container, &pids_size);
          }
          if (check_container_pid_by_open_kernel(processes_sample[i].pid, pids_on_container, pids_size)) {
            top_result->user_current += sm_util + codec_util;
          }
        }
      }
    }
    if (pids_on_container) {
      free(pids_on_container);
      pids_on_container = NULL;
    }
  } else if ((g_vgpu_config.compatibility_mode & CGROUPV1_COMPATIBILITY_MODE) == CGROUPV1_COMPATIBILITY_MODE) {
    int pids_size = MAX_PIDS;
    int *pids_on_container = NULL;
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
        } else if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
          if (!pids_on_container) {
            char proc_path[PATH_MAX];
            snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
            extract_container_pids(proc_path, &pids_on_container, &pids_size);
          }
          if (check_container_pid_by_open_kernel(processes_sample[i].pid, pids_on_container, pids_size)) {
            top_result->user_current += sm_util + codec_util;
          }
        }
      }
    }
    if (pids_on_container) {
      free(pids_on_container);
      pids_on_container = NULL;
    }
  } else if ((g_vgpu_config.compatibility_mode & OPEN_KERNEL_COMPATIBILITY_MODE) == OPEN_KERNEL_COMPATIBILITY_MODE) {
    int pids_size = MAX_PIDS;
    int *pids_on_container = NULL;
    char proc_path[PATH_MAX];
    snprintf(proc_path, sizeof(proc_path), HOST_CGROUP_PID_BASE_PATH, container_id);
    extract_container_pids(proc_path, &pids_on_container, &pids_size);
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
    if (pids_on_container) {
      free(pids_on_container);
      pids_on_container = NULL;
    }
  } else if (g_vgpu_config.compatibility_mode == HOST_COMPATIBILITY_MODE) {
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
    LOGGER(FATAL, "unknown env compatibility mode: %d", g_vgpu_config.compatibility_mode);
  }

  LOGGER(VERBOSE, "device: %d, sys util: %d, user util: %d", device_id,
                  top_result->sys_current, top_result->user_current);
}

/** hook entrypoint */
CUresult cuDriverGetVersion(int *driverVersion) {
  CUresult ret;

  load_necessary_data();
//  pthread_once(&g_init_set, initialization);

  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuDriverGetVersion, driverVersion);
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
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuInit, flag);

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
  CUdevice ordinal;
  int lock_fd = -1;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t used = 0, request_size = bytesize;
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    lock_fd = lock_gpu_device(ordinal);

    get_used_gpu_memory((void *)&used, ordinal);
    if (g_vgpu_config.devices[ordinal].memory_oversold) {
      // Used memory exceeds device memory limit, return OOM
      if (unlikely(used > g_vgpu_config.devices[ordinal].real_memory)) {
        ret = CUDA_ERROR_OUT_OF_MEMORY;
        goto DONE;
      }
      // The requested memory exceeds the device's memory limit,
      // using global unified memory
      if ((used + request_size) > g_vgpu_config.devices[ordinal].real_memory) {
        flags = CU_MEM_ATTACH_GLOBAL;
      }
    } else if (used + request_size > g_vgpu_config.devices[ordinal].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, flags);
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult _cuMemAlloc(CUdeviceptr *dptr, size_t bytesize) {
  CUresult ret;
  CUdevice ordinal;
  int lock_fd = -1;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  const char *mem_err_string = NULL;
  const char *uva_err_string = NULL;
  size_t used = 0, request_size = bytesize;
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    lock_fd = lock_gpu_device(ordinal);
    get_used_gpu_memory((void *)&used, ordinal);

    if (g_vgpu_config.devices[ordinal].memory_oversold) {
      // Used memory exceeds device memory limit, return OOM
      if (unlikely(used > g_vgpu_config.devices[ordinal].real_memory)) {
        ret = CUDA_ERROR_OUT_OF_MEMORY;
        goto DONE;
      }
      // The requested memory exceeds the device's memory limit,
      // using global unified memory
      if ((used + request_size) > g_vgpu_config.devices[ordinal].real_memory) {
FROM_UVA:
        ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
        LOGGER(VERBOSE, "cuMemAlloc call from UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &uva_err_string));
        goto DONE;
      }
      if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAlloc_v2))) {
        ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAlloc_v2, dptr, bytesize);
      } else {
        ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAlloc, dptr, bytesize);
      }
      if (likely(ret == CUDA_SUCCESS)) {
        goto DONE;
      }
      LOGGER(VERBOSE, "cuMemAlloc call failed, fallback to UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &mem_err_string));
      goto FROM_UVA;
    } else if ((used + request_size) > g_vgpu_config.devices[ordinal].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  }
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAlloc_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAlloc_v2, dptr, bytesize);
  } else {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAlloc, dptr, bytesize);
  }
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY && g_vgpu_config.devices[ordinal].memory_oversold)) {
    LOGGER(VERBOSE, "cuMemAlloc call failed, fallback to UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &mem_err_string));
    goto FROM_UVA;
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
  CUdevice ordinal;
  int lock_fd = -1;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  const char *mem_err_string = NULL;
  const char *uva_err_string = NULL;

  size_t used = 0;
  // size_t request_size = ROUND_UP(WidthInBytes * Height, ElementSizeBytes);
  size_t guess_pitch = (((WidthInBytes - 1) / ElementSizeBytes) + 1) * ElementSizeBytes;
  size_t request_size = guess_pitch * Height;

  if (g_vgpu_config.devices[ordinal].memory_limit) {
    lock_fd = lock_gpu_device(ordinal);
    get_used_gpu_memory((void *)&used, ordinal);

    if (g_vgpu_config.devices[ordinal].memory_oversold) {
      // Used memory exceeds device memory limit, return OOM
      if (unlikely(used > g_vgpu_config.devices[ordinal].real_memory)) {
        ret = CUDA_ERROR_OUT_OF_MEMORY;
        goto DONE;
      }
      // The requested memory exceeds the device's memory limit,
      // using global unified memory
      if ((used + request_size) > g_vgpu_config.devices[ordinal].real_memory) {
FROM_UVA:
        ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, request_size, CU_MEM_ATTACH_GLOBAL);
        LOGGER(VERBOSE, "cuMemAllocPitch call from UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &uva_err_string));
        if (likely(ret == CUDA_SUCCESS)) {
          *pPitch = guess_pitch;
        }
        goto DONE;
      }

      if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAllocPitch_v2))) {
        ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocPitch_v2, dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
      } else {
        ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocPitch, dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
      }
      if (likely(ret == CUDA_SUCCESS)) {
        goto DONE;
      }
      LOGGER(VERBOSE, "cuMemAllocPitch call failed, fallback to UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &mem_err_string));
      goto FROM_UVA;
    } else if ((used + request_size) > g_vgpu_config.devices[ordinal].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  }
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemAllocPitch_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocPitch_v2, dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
  } else {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemAllocPitch, dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
  }
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY && g_vgpu_config.devices[ordinal].memory_oversold)) {
    LOGGER(VERBOSE, "cuMemAllocPitch call failed, fallback to UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &mem_err_string));
    goto FROM_UVA;
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
  CUdevice ordinal;
  int lock_fd = -1;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  const char *mem_err_string = NULL;
  const char *uva_err_string = NULL;
  size_t used = 0, request_size = bytesize;

  if (g_vgpu_config.devices[ordinal].memory_limit) {
    lock_fd = lock_gpu_device(ordinal);
    get_used_gpu_memory((void *)&used, ordinal);

    if (g_vgpu_config.devices[ordinal].memory_oversold) {
      // Used memory exceeds device memory limit, return OOM
      if (unlikely(used > g_vgpu_config.devices[ordinal].real_memory)) {
        ret = CUDA_ERROR_OUT_OF_MEMORY;
        goto DONE;
      }
      // The requested memory exceeds the device's memory limit,
      // using global unified memory
      if ((used + request_size) > g_vgpu_config.devices[ordinal].real_memory) {
FROM_UVA:
        ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
        LOGGER(VERBOSE, "cuMemAllocAsync call from UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &uva_err_string));
        goto DONE;
      }

      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocAsync, dptr, bytesize, hStream);
      if (likely(ret == CUDA_SUCCESS)) {
        goto DONE;
      }
      LOGGER(VERBOSE, "cuMemAllocAsync call failed, fallback to UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &mem_err_string));
      goto FROM_UVA;
    } else if ((used + request_size) > g_vgpu_config.devices[ordinal].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocAsync, dptr, bytesize, hStream);
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY && g_vgpu_config.devices[ordinal].memory_oversold)) {
    LOGGER(VERBOSE, "cuMemAllocAsync call failed, fallback to UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &mem_err_string));
    goto FROM_UVA;
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult cuMemAllocAsync_ptsz(CUdeviceptr *dptr, size_t bytesize, CUstream hStream) {
  CUresult ret;
  CUdevice ordinal;
  int lock_fd = -1;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  const char *mem_err_string = NULL;
  const char *uva_err_string = NULL;
  size_t used = 0, request_size = bytesize;

  if (g_vgpu_config.devices[ordinal].memory_limit) {
    lock_fd = lock_gpu_device(ordinal);
    get_used_gpu_memory((void *)&used, ordinal);

    if (g_vgpu_config.devices[ordinal].memory_oversold) {
      // Used memory exceeds device memory limit, return OOM
      if (unlikely(used > g_vgpu_config.devices[ordinal].real_memory)) {
        ret = CUDA_ERROR_OUT_OF_MEMORY;
        goto DONE;
      }
      // The requested memory exceeds the device's memory limit,
      // using global unified memory
      if ((used + request_size) > g_vgpu_config.devices[ordinal].real_memory) {
FROM_UVA:
        ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
        LOGGER(VERBOSE, "cuMemAllocAsync_ptsz call from UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &uva_err_string));
        goto DONE;
      }
      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocAsync_ptsz, dptr, bytesize, hStream);
      if (likely(ret == CUDA_SUCCESS)) {
        goto DONE;
      }
      LOGGER(VERBOSE, "cuMemAllocAsync_ptsz call failed, fallback to UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &mem_err_string));
      goto FROM_UVA;
    } else if ((used + request_size) > g_vgpu_config.devices[ordinal].total_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocAsync_ptsz, dptr, bytesize, hStream);
  if (unlikely(ret == CUDA_ERROR_OUT_OF_MEMORY && g_vgpu_config.devices[ordinal].memory_oversold)) {
    LOGGER(VERBOSE, "cuMemAllocAsync_ptsz call failed, fallback to UVA (oversold), size: %zu, ret: %d, str: %s",
                        request_size, ret, cuda_error(ret, &mem_err_string));
    goto FROM_UVA;
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

static CUresult cuArrayCreate_helper(const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  CUresult ret = CUDA_SUCCESS;
  CUdevice ordinal;
  int lock_fd = -1;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t used = 0, base_size = 0, request_size = 0;
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    lock_fd = lock_gpu_device(ordinal);

    base_size = get_array_base_size(pAllocateArray->Format);
    request_size = base_size * pAllocateArray->NumChannels *
                   pAllocateArray->Height * pAllocateArray->Width;

    get_used_gpu_memory((void *)&used, ordinal);
    if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].total_memory)) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult _cuArrayCreate(CUarray *pHandle, const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  CUresult ret;
  ret = cuArrayCreate_helper(pAllocateArray);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArrayCreate_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayCreate_v2, pHandle, pAllocateArray);
  } else {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArrayCreate, pHandle, pAllocateArray);
  }
DONE:
  return ret;
}

CUresult cuArrayCreate_v2(CUarray *pHandle, const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  return _cuArrayCreate(pHandle, pAllocateArray);
}

CUresult cuArrayCreate(CUarray *pHandle, const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  return _cuArrayCreate(pHandle, pAllocateArray);
}

static CUresult cuArray3DCreate_helper(const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  CUresult ret;
  CUdevice ordinal;
  int lock_fd = -1;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t used = 0, base_size = 0, request_size = 0;
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    lock_fd = lock_gpu_device(ordinal);

    base_size = get_array_base_size(pAllocateArray->Format);
    request_size = base_size * pAllocateArray->NumChannels *
                   pAllocateArray->Height * pAllocateArray->Width *
                   pAllocateArray->Depth;
    get_used_gpu_memory((void *)&used, ordinal);
    if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].total_memory)) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  }
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult _cuArray3DCreate(CUarray *pHandle,
                          const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  CUresult ret;
  ret = cuArray3DCreate_helper(pAllocateArray);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuArray3DCreate_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArray3DCreate_v2, pHandle, pAllocateArray);
  } else {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuArray3DCreate, pHandle, pAllocateArray);
  }
DONE:
  return ret;
}

CUresult cuArray3DCreate_v2(CUarray *pHandle,
                            const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  return _cuArray3DCreate(pHandle, pAllocateArray);
}

CUresult cuArray3DCreate(CUarray *pHandle,
                         const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  return _cuArray3DCreate(pHandle, pAllocateArray);
}

CUresult cuMipmappedArrayCreate(CUmipmappedArray *pHandle,
                                const CUDA_ARRAY3D_DESCRIPTOR *pMipmappedArrayDesc,
                                unsigned int numMipmapLevels) {
  CUresult ret;
  CUdevice ordinal;
  int lock_fd = -1;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t used = 0, base_size = 0, request_size = 0;
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    lock_fd = lock_gpu_device(ordinal);

    base_size = get_array_base_size(pMipmappedArrayDesc->Format);
    request_size = base_size * pMipmappedArrayDesc->NumChannels *
                   pMipmappedArrayDesc->Height * pMipmappedArrayDesc->Width *
                   pMipmappedArrayDesc->Depth;

    get_used_gpu_memory((void *)&used, ordinal);
    if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].total_memory)) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMipmappedArrayCreate, pHandle,
                        pMipmappedArrayDesc, numMipmapLevels);
DONE:
  unlock_gpu_device(lock_fd);
  return ret;
}

CUresult _cuDeviceTotalMem(size_t *bytes, CUdevice dev) {
  if (g_vgpu_config.devices[dev].memory_limit) {
    *bytes = g_vgpu_config.devices[dev].total_memory;
    return CUDA_SUCCESS;
  }
  CUresult ret;
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuDeviceTotalMem_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceTotalMem_v2, bytes, dev);
  } else {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuDeviceTotalMem, bytes, dev);
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
  CUdevice ordinal;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  size_t used = 0;
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    get_used_gpu_memory((void *)&used, ordinal);
    size_t total_memory = g_vgpu_config.devices[ordinal].total_memory;
    *total = total_memory;
    *free = (used > total_memory) ? 0 : (total_memory - used);
    return CUDA_SUCCESS;
  }
  if (likely(CUDA_FIND_ENTRY(cuda_library_entry, cuMemGetInfo_v2))) {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetInfo_v2, free, total);
  } else {
    ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuMemGetInfo, free, total);
  }
DONE:
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
  CUdevice ordinal;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(gridDimX * gridDimY * gridDimZ,
              blockDimX * blockDimY * blockDimZ, ordinal);
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuLaunchKernel_ptsz, f, gridDimX,
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
  CUdevice ordinal;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(gridDimX * gridDimY * gridDimZ,
              blockDimX * blockDimY * blockDimZ, ordinal);
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuLaunchKernel, f, gridDimX,
                         gridDimY, gridDimZ, blockDimX, blockDimY, blockDimZ,
                         sharedMemBytes, hStream, kernelParams, extra);
DONE:
  return ret;
}

CUresult cuLaunchKernelEx(CUlaunchConfig *config, CUfunction f, 
                          void **kernelParams, void **extra) {
  CUresult ret;
  CUdevice ordinal;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(config->gridDimX * config->gridDimY * config->gridDimZ,
               config->blockDimX * config->blockDimY * config->blockDimZ, ordinal);
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuLaunchKernelEx, 
                         config, f, kernelParams, extra);
DONE:
  return ret;
}

CUresult cuLaunchKernelEx_ptsz(CUlaunchConfig *config, CUfunction f, 
                               void **kernelParams, void **extra) {
  CUresult ret; 
  CUdevice ordinal;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(config->gridDimX *config->gridDimY * config->gridDimZ,
               config->blockDimX * config->blockDimY * config->blockDimZ, ordinal);
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuLaunchKernelEx_ptsz, 
                         config, f, kernelParams, extra);
DONE:
  return ret;
}

CUresult cuLaunch(CUfunction f) {
  CUresult ret; 
  CUdevice ordinal;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }

  rate_limiter(1, g_block_x[ordinal] * g_block_y[ordinal] * g_block_z[ordinal], ordinal);
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuLaunch, f);
DONE:
  return ret;
}

CUresult cuLaunchCooperativeKernel_ptsz(
    CUfunction f, unsigned int gridDimX, unsigned int gridDimY,
    unsigned int gridDimZ, unsigned int blockDimX, unsigned int blockDimY,
    unsigned int blockDimZ, unsigned int sharedMemBytes, CUstream hStream,
    void **kernelParams) {
  CUdevice ordinal;
  CUresult ret; 
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(gridDimX * gridDimY * gridDimZ,
               blockDimX * blockDimY * blockDimZ, ordinal);
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuLaunchCooperativeKernel_ptsz, f,
                         gridDimX, gridDimY, gridDimZ, blockDimX, blockDimY,
                         blockDimZ, sharedMemBytes, hStream, kernelParams);
DONE:
  return ret;
}

CUresult cuLaunchCooperativeKernel(CUfunction f,
    unsigned int gridDimX, unsigned int gridDimY, unsigned int gridDimZ,
    unsigned int blockDimX, unsigned int blockDimY, unsigned int blockDimZ,
    unsigned int sharedMemBytes, CUstream hStream, void **kernelParams) {
  CUdevice ordinal;
  CUresult ret; 
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }    
  rate_limiter(gridDimX * gridDimY * gridDimZ,
               blockDimX * blockDimY * blockDimZ, ordinal);
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuLaunchCooperativeKernel, f,
                         gridDimX, gridDimY, gridDimZ, blockDimX, blockDimY,
                         blockDimZ, sharedMemBytes, hStream, kernelParams);
DONE:
  return ret;
}

CUresult cuLaunchGrid(CUfunction f, int grid_width, int grid_height) {
  CUresult ret;  
  CUdevice ordinal;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(grid_width * grid_height, g_block_x[ordinal] * g_block_y[ordinal] * g_block_z[ordinal], ordinal);
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuLaunchGrid, f, grid_width,grid_height);
DONE:
  return ret;
}

CUresult cuLaunchGridAsync(CUfunction f, int grid_width, int grid_height, CUstream hStream) {
  CUresult ret;  
  CUdevice ordinal;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  rate_limiter(grid_width * grid_height, g_block_x[ordinal] * g_block_y[ordinal] * g_block_z[ordinal], ordinal);
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuLaunchGridAsync, f, grid_width, grid_height, hStream);
DONE:
  return ret;
}

CUresult cuFuncSetBlockShape(CUfunction hfunc, int x, int y, int z) {
  CUresult ret;
  CUdevice ordinal;
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (unlikely(ret != CUDA_SUCCESS)) {
    goto DONE;
  }
  if (g_vgpu_config.devices[ordinal].core_limit) {
    while (!CAS(&g_block_locker[ordinal], 0, 1)) {}

    g_block_x[ordinal] = x;
    g_block_y[ordinal] = y;
    g_block_z[ordinal] = z;

    LOGGER(VERBOSE, "device %d set block shape: %d, %d, %d", ordinal, x, y, z);

    while (!CAS(&g_block_locker[ordinal], 1, 0)) {}
  }
  ret =  CUDA_ENTRY_CALL(cuda_library_entry, cuFuncSetBlockShape, hfunc, x, y, z);
DONE:
  return ret;
}