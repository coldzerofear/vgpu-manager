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

extern resource_data_t g_vgpu_config;
extern char container_id[FILENAME_MAX];
extern entry_t cuda_library_entry[];
extern entry_t nvml_library_entry[];

static pthread_once_t g_init_set = PTHREAD_ONCE_INIT;

static volatile int g_cur_cuda_cores[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static volatile int g_total_cuda_cores[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

static int g_max_thread_per_sm[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};
static int g_sm_num[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

static int g_block_x[MAX_DEVICE_COUNT] = {1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1};
static int g_block_y[MAX_DEVICE_COUNT] = {1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1};
static int g_block_z[MAX_DEVICE_COUNT] = {1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1};
static uint32_t g_block_locker[MAX_DEVICE_COUNT] = {0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0};

static const struct timespec g_cycle = {
    .tv_sec = 0,
    .tv_nsec = TIME_TICK * MILLISEC,
};

static const struct timespec g_wait = {
    .tv_sec = 0,
    .tv_nsec = 120 * MILLISEC,
};

/** internal function definition */

static void active_utilization_notifier(int);

static void *utilization_watcher(void *);

static void get_used_gpu_utilization(void *, CUdevice);

static void initialization();

static void rate_limiter(int, int, int);

static void change_token(int, int);

static const char *nvml_error(nvmlReturn_t);

static const char *cuda_error(CUresult, const char **);

static int delta(int, int, int, int);

static int check_file_exist(const char *);

/** export function definition */
CUresult cuDriverGetVersion(int *driverVersion);
CUresult cuInit(unsigned int flag);
CUresult cuGetProcAddress(const char *symbol, void **pfn, int cudaVersion,
                          cuuint64_t flags); 
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
  int valid;
  uint64_t checktime;
  int sys_process_num;
} utilization_t;


const char *nvml_error(nvmlReturn_t code) {
  const char *(*err_fn)(nvmlReturn_t) = NULL;

  err_fn = nvml_library_entry[NVML_ENTRY_ENUM(nvmlErrorString)].fn_ptr;
  if (unlikely(!err_fn)) {
    LOGGER(FATAL, "can't find nvmlErrorString");
  }

  return err_fn(code);
}

const char *cuda_error(CUresult code, const char **p) {
  CUDA_ENTRY_CALL(cuda_library_entry, cuGetErrorString, code, p);
  return *p;
}

static int check_file_exist(const char *file_path) {
  if (access(file_path, F_OK) != -1) {
      return 0;
  } else {
      return 1;
  }
}


static void change_token(int delta, int device_id) {
  int cuda_cores_before = 0, cuda_cores_after = 0;

  LOGGER(DETAIL, "device: %d, delta: %d, curr: %d", device_id, delta, g_cur_cuda_cores[device_id]);
  do {
    cuda_cores_before = g_cur_cuda_cores[device_id];
    cuda_cores_after = cuda_cores_before + delta;

    if (unlikely(cuda_cores_after > g_total_cuda_cores[device_id])) {
      cuda_cores_after = g_total_cuda_cores[device_id];
    }
  } while (!CAS(&g_cur_cuda_cores[device_id], cuda_cores_before, cuda_cores_after));
}


static void rate_limiter(int grids, int blocks, int device_id) {
  int before_cuda_cores = 0;
  int after_cuda_cores = 0;
  int kernel_size = grids;

  LOGGER(VERBOSE, "device: %d, grid: %d, blocks: %d, ", device_id , grids, blocks);
  LOGGER(VERBOSE, "device: %d, launch kernel: %d, curr core: %d", device_id, kernel_size, g_cur_cuda_cores[device_id]);
  if (g_vgpu_config.devices[device_id].core_limit) {
    do {
    CHECK:
      before_cuda_cores = g_cur_cuda_cores[device_id];
      LOGGER(DETAIL, "device: %d, current core: %d", device_id, g_cur_cuda_cores[device_id]);
      if (before_cuda_cores < 0) {
        nanosleep(&g_cycle, NULL);
        goto CHECK;
      }
      after_cuda_cores = before_cuda_cores - kernel_size;
    } while (!CAS(&g_cur_cuda_cores[device_id], before_cuda_cores, after_cuda_cores));
  }
}

static int delta(int up_limit, int user_current, int share, int device_index) {
  int utilization_diff =
      abs(up_limit - user_current) < 5 ? 5 : abs(up_limit - user_current);
  int increment =
      g_sm_num[device_index] * g_sm_num[device_index] * g_max_thread_per_sm[device_index] * utilization_diff / 2560;

  /* Accelerate cuda cores allocation when utilization vary widely */
  if (utilization_diff > up_limit / 2) {
    increment = increment * utilization_diff * 2 / (up_limit + 1);
  }

  if (unlikely(increment < 0)) {
    LOGGER(FATAL, "overflow: %d, current sm: %d, thread_per_sm: %d, diff: %d",
           increment, g_sm_num[device_index], g_max_thread_per_sm[device_index], utilization_diff);
  }

  if (user_current <= up_limit) {
    share = share + increment > g_total_cuda_cores[device_index] ?
            g_total_cuda_cores[device_index] : share + increment;
  } else {
    share = share - increment < 0 ? 0 : share - increment;
  }

  return share;
}

// #lizard forgives
static void *utilization_watcher(void *arg) {
  int device_id = *(int *)arg;
  utilization_t top_result = {
      .user_current = 0,
      .sys_current = 0,
      .sys_process_num = 0,
  };
  int sys_free = 0;
  int share = 0;
  int i = 0;
  int avg_sys_free = 0;
  int pre_sys_process_num = 1;

  int up_limit = g_vgpu_config.devices[device_id].hard_core;
  LOGGER(VERBOSE, "current device %d, start %s", device_id, __FUNCTION__);
  LOGGER(VERBOSE, "device: %d, sm: %d, thread per sm: %d", device_id, g_sm_num[device_id], g_max_thread_per_sm[device_id]);
  while (1) {
    nanosleep(&g_wait, NULL);
    do {
      get_used_gpu_utilization((void *)&top_result, device_id);
    } while (!top_result.valid);
    // 设备最大利用率100% - 设备当前已使用 = 空闲利用率
    sys_free = MAX_UTILIZATION - top_result.sys_current;

    if (g_vgpu_config.devices[device_id].hard_limit) {
      /* Avoid usage jitter when application is initialized*/
      if (top_result.sys_process_num == 1 && top_result.user_current < up_limit / 10) {
        g_cur_cuda_cores[device_id] =
            delta(g_vgpu_config.devices[device_id].hard_core, top_result.user_current, share, device_id);
        continue;
      }
      share = delta(g_vgpu_config.devices[device_id].hard_core, top_result.user_current, share, device_id);
    } else {
      // 设备上的进程数发生变化时，重置初始值
      if (pre_sys_process_num != top_result.sys_process_num) {
        /* When a new process comes, all processes are reset to initial value*/
        if (pre_sys_process_num < top_result.sys_process_num) {
          share = g_max_thread_per_sm[device_id];
          up_limit = g_vgpu_config.devices[device_id].hard_core;
          i = 0;
          avg_sys_free = 0;
        }
        pre_sys_process_num = top_result.sys_process_num;
      }

      /* 1.Only one process on the GPU
       * Allocate cuda cores according to the limit value.
       *
       * 2.Multiple processes on the GPU
       * First, change the up_limit of the process according to the
       * historical resource utilization. Second, allocate the cuda
       * cores according to the changed limit value.*/
      if (top_result.sys_process_num == 1) {
        share = delta(g_vgpu_config.devices[device_id].soft_core, top_result.user_current, share, device_id);
      } else {
        i++;
        avg_sys_free += sys_free;
        if (i % CHANGE_LIMIT_INTERVAL == 0) {
          if (avg_sys_free * 2 / CHANGE_LIMIT_INTERVAL > USAGE_THRESHOLD) {
            up_limit = up_limit + g_vgpu_config.devices[device_id].hard_core / 10 > g_vgpu_config.devices[device_id].soft_core ?
                       g_vgpu_config.devices[device_id].soft_core : up_limit + g_vgpu_config.devices[device_id].hard_core / 10;
          }
          i = 0;
        }
        avg_sys_free = i % (CHANGE_LIMIT_INTERVAL / 2) == 0 ? 0 : avg_sys_free;
        share = delta(up_limit, top_result.user_current, share, device_id);
      }
    }
    change_token(share, device_id);
    LOGGER(DETAIL, "device: %d, util: %d, up_limit: %d, share: %d, cur: %d", device_id,
           top_result.user_current, up_limit, share, g_cur_cuda_cores[device_id]);
  }
}

static int device_ids[MAX_DEVICE_COUNT] = {0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15};

static void active_utilization_notifier(const int device_id) {
  pthread_t tid;
  pthread_create(&tid, NULL, utilization_watcher, &device_ids[device_id]);
  char thread_name[32] = {0};
  sprintf(thread_name, "utilization_watcher_gpu%d", device_id);
#ifdef __APPLE__
  pthread_setname_np(thread_name);
#else
  pthread_setname_np(tid, thread_name);
#endif
}

static void initialization() {
  int ret;
  const char *cuda_err_string = NULL;

  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuInit, 0);
  if (unlikely(ret)) {
    LOGGER(FATAL, "cuInit error %s", cuda_error((CUresult)ret, &cuda_err_string));
  }
  for (int i = 0; i < g_vgpu_config.device_count; i++) {
    if (!g_vgpu_config.devices[i].core_limit) {
      continue;
    }
    // 获取设备上的多处理器数量
    ret = CUDA_ENTRY_CALL(cuda_library_entry, cuDeviceGetAttribute, &g_sm_num[i],
                          CU_DEVICE_ATTRIBUTE_MULTIPROCESSOR_COUNT, i);
    if (unlikely(ret)) {
      LOGGER(FATAL, "can't get processor number, device %d, error %s", i,
            cuda_error((CUresult)ret, &cuda_err_string));
    }
    // 获取设备每个处理器的最大驻留线程数
    ret = CUDA_ENTRY_CALL(cuda_library_entry, cuDeviceGetAttribute, &g_max_thread_per_sm[i],
                          CU_DEVICE_ATTRIBUTE_MAX_THREADS_PER_MULTIPROCESSOR, i);
    if (unlikely(ret)) {
      LOGGER(FATAL, "can't get max thread per processor, device %d, error %s", 
                  i, cuda_error((CUresult)ret, &cuda_err_string));
    }
    // 处理器数量 * 最大驻留线程数 * 32 = 最大cuda核心数
    g_total_cuda_cores[i] = g_max_thread_per_sm[i] * g_sm_num[i] * FACTOR;
    LOGGER(VERBOSE, "device %d total cuda cores: %d", i, g_total_cuda_cores[i]);
    active_utilization_notifier(i);
  }
}

int split_str(char *line, char *key, char *value, char d) {
  int index = 0;
  for (index = 0; index < strlen(line) && line[index] != d; index++){
  }

  if (index == strlen(line)){
    key[0] = '\0';
    value = '\0';
    return 1;
  }

  int start = 0, i = 0;
  // trim head
  for (; start < index && (line[start] == ' ' || line[start] == '\t'); start++){
  }

  for (i = 0; start < index; i++, start++) {
    key[i] = line[start];
  }
  // trim tail
  for (; i > 0 && (key[i - 1] == '\0' || key[i - 1] == '\n' || key[i - 1] == '\t'); i--){
  }
  key[i] = '\0';

  start = index + 1;
  i = 0;

  // trim head
  for (; start < strlen(line) && (line[start] == ' ' || line[start] == '\t'); start++){
  }

  for (i = 0; start < strlen(line); i++, start++) {
    value[i] = line[start];
  }
  // trim tail
  for (; i > 0 && (value[i - 1] == '\0' || value[i - 1] == '\n' || value[i - 1] == '\t'); i--){
  }
  value[i] = '\0';
  return 0;
}


int read_cgroup(char *pidpath, char *cgroup_key, char *cgroup_value) {
  FILE *f = fopen(pidpath, "rb");
  if (f == NULL) {
    LOGGER(VERBOSE, "read file %s failed: %s\n", pidpath, strerror(errno));
    return 1;
  }
  char buff[255];
  while (fgets(buff, 255, f)) {
    int index = 0;
    for (; index < strlen(buff) && buff[index] != ':'; index++) {
    }
    if (index == strlen(buff))
      continue;
    char key[128], value[128];
    if (split_str(&buff[index + 1], key, value, ':') != 0)
      continue;
    if (strcmp(key, cgroup_key) == 0) {
      strcpy(cgroup_value, value);
      fclose(f);
      return 0;
    }
  }
  fclose(f);
  return 1;
}

int check_in_container() {
  int has_entries = 0;
  if (access(HOST_PROC_PATH, F_OK) != -1) {
    DIR *dir = opendir(HOST_PROC_PATH);
    if (dir == NULL) {
      return 0;
    }
    struct dirent *entry;
    while ((entry = readdir(dir)) != NULL) {
        // 跳过当前目录（.）和父目录（..）
        if (strcmp(entry->d_name, ".") == 0 || strcmp(entry->d_name, "..") == 0) {
            continue;
        }
        has_entries = 1;
        break; 
    }
    closedir(dir);
  }
  return has_entries ? 1 : 0;
}

int check_container_pid(unsigned int pid) {
  if (pid == 0) {
    goto DONE;
  }
  char pidpath[128] = "";
  sprintf(pidpath, HOST_CGROUP_PID_PATH, pid);

  char container_cg[256];
  char process_cg[256];

  if (!read_cgroup(PID_ONE_CGROUP_PATH, "memory", container_cg)
      && !read_cgroup(pidpath, "memory", process_cg)) {
    LOGGER(DETAIL, "\ncontainer cg: %s\nprocess cg: %s", container_cg, process_cg);
    if (strstr(process_cg, container_cg) != NULL) {
      LOGGER(VERBOSE, "cgroup match: %s", process_cg);
      return 1;
    }
  }
DONE:
  LOGGER(VERBOSE, "cgroup mismatch: %d", pid);
  return 0;
}

int check_container_pid_v2(unsigned int pid) {
  if (pid == 0) {
    goto DONE;
  }
  char pidpath[128] = "";
  sprintf(pidpath, HOST_CGROUP_PID_PATH, pid);
  if (check_file_exist(pidpath)) {
     goto DONE;
  }
  FILE *f = fopen(pidpath, "rb");
  if (f == NULL) {
    LOGGER(VERBOSE, "read file %s failed: %s\n", pidpath, strerror(errno));
    goto DONE;
  }
  char buff[FILENAME_MAX];
  while (fgets(buff, FILENAME_MAX, f)) {
    size_t len = strlen(buff);
    if (len > 0 && buff[len - 1] == '\n') {
      buff[len - 1] = '\0';
    }
    if (strcmp(buff, "0::/") == 0 || strstr(buff, container_id) != NULL) {
      fclose(f);
      LOGGER(VERBOSE, "cgroup match: %s", buff);
      return 1;
    }
  }
  fclose(f);
DONE:
  LOGGER(VERBOSE, "cgroup mismatch: %d", pid);
  return 0;
}

void get_used_gpu_memory_by_device(void *arg, nvmlDevice_t device) {
  size_t *used_memory = arg;
  // 记录gpu设备上运行的pid
  nvmlProcessInfo_t pids_on_device[MAX_PIDS];
  // 最大pid数1024
  unsigned int size_on_device = MAX_PIDS;
  int ret;

  unsigned int i;
  // 根据设备查找设备上运行的进程信息
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses,
                      device, &size_on_device, pids_on_device);
  if (unlikely(ret)) {
    LOGGER(WARNING, "nvmlDeviceGetComputeRunningProcesses can't get pids on device, "
           "return %d", ret);
    *used_memory = 0;
    return;
  }
  // 校验宿主机进程目录是否存在
  if (likely(check_in_container())) {
    // 当宿主机进程目录存在, 代表运行于容器中
    if (strlen(container_id) > 0) {
      LOGGER(VERBOSE, "use cgroupv2 compatible matching mode");
      for (i = 0; i < size_on_device; i++) {
        if (likely(check_container_pid_v2(pids_on_device[i].pid))) {
          LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid,
                 pids_on_device[i].usedGpuMemory);
          *used_memory += pids_on_device[i].usedGpuMemory;
        }
      }
    } else {
      // 校验pid是否是当前容器的pid，匹配上了就增加到已使用内存
      for (i = 0; i < size_on_device; i++) {
        if (likely(check_container_pid(pids_on_device[i].pid))) {
          LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid,
                pids_on_device[i].usedGpuMemory);
          *used_memory += pids_on_device[i].usedGpuMemory;
        }
      }
    }
  } else {
    // 没有找到宿主机进程目录，表示运行于物理机，添加所有进程的已使用内存
    for (i = 0; i < size_on_device; i++) {
      LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid,
             pids_on_device[i].usedGpuMemory);
      *used_memory += pids_on_device[i].usedGpuMemory;
    }
  }
  LOGGER(VERBOSE, "total used memory: %zu", *used_memory);
}

void get_used_gpu_memory(void *arg, CUdevice device_id) {
  size_t *used_memory = arg;
  nvmlDevice_t dev;
  // 记录gpu设备上运行的pid
  nvmlProcessInfo_t pids_on_device[MAX_PIDS];
  // 运行于gpu上的最大进程数1024
  unsigned int size_on_device = MAX_PIDS;
  int ret;

  unsigned int i;

  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlInit_v2);
  if (unlikely(ret)) {
    LOGGER(WARNING, "nvmlInit error, return %d, str: %s", ret, nvml_error(ret));
    *used_memory = 0;
    return;
  }
  // 根据设备号查找设备
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex, device_id, &dev);
  if (unlikely(ret)) {
    LOGGER(WARNING, "nvmlDeviceGetHandleByIndex can't find device %d, return %d, str: %s",
                    device_id, ret, nvml_error(ret));
    *used_memory = 0;
    return;
  }
  // 根据设备查找设备上运行的进程信息
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses,
                      dev, &size_on_device, pids_on_device);
  if (unlikely(ret)) {
    LOGGER(WARNING, "nvmlDeviceGetComputeRunningProcesses can't get pids on device %d, "
           "return %d, str: %s", device_id, ret, nvml_error(ret));
    *used_memory = 0;
    return;
  }
  // 校验宿主机进程目录是否存在, 存在表示运行于容器中
  if (likely(check_in_container())) {
        // 当宿主机进程目录存在, 代表运行于容器中
    if (strlen(container_id) > 0) {
      LOGGER(VERBOSE, "use cgroupv2 compatible matching mode");
      for (i = 0; i < size_on_device; i++) {
        if (likely(check_container_pid_v2(pids_on_device[i].pid))) {
          LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid,
                 pids_on_device[i].usedGpuMemory);
          *used_memory += pids_on_device[i].usedGpuMemory;
        }
      }
    } else {
      // 校验pid是否是当前容器的pid，匹配上了就增加到已使用内存
      for (i = 0; i < size_on_device; i++) {
        if (likely(check_container_pid(pids_on_device[i].pid))) {
          LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid,
                pids_on_device[i].usedGpuMemory);
          *used_memory += pids_on_device[i].usedGpuMemory;
        }
      }
    }
  } else {
    for (i = 0; i < size_on_device; i++) {
      LOGGER(VERBOSE, "pid[%d] use memory: %lld", pids_on_device[i].pid,
             pids_on_device[i].usedGpuMemory);
      *used_memory += pids_on_device[i].usedGpuMemory;
    }
  }
  LOGGER(VERBOSE, "total used memory: %zu", *used_memory);
}

static void get_used_gpu_utilization(void *arg, CUdevice device_id) {
  nvmlProcessUtilizationSample_t processes_sample[MAX_PIDS];
  int processes_num = MAX_PIDS;
  unsigned int running_processes = MAX_PIDS;
  nvmlProcessInfo_t pids_on_device[MAX_PIDS];
  nvmlDevice_t dev;
  utilization_t *top_result = (utilization_t *)arg;

  top_result->user_current = 0;
  top_result->sys_current = 0;

  nvmlReturn_t ret;
  struct timeval cur;
  size_t microsec;
  int codec_util = 0;

  int i;
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlInit_v2);
  if (unlikely(ret)) {
    LOGGER(WARNING, "nvmlInit error, return %d, str: %s", ret, nvml_error(ret));
    return;
  }
  // 获取设备句柄
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetHandleByIndex, device_id, &dev);
  if (unlikely(ret)) {
    LOGGER(WARNING, "nvmlDeviceGetHandleByIndex can't find device %d, "
                  "return %d, str: %s", device_id, ret, nvml_error(ret));
    return;
  }
  // 根据设备找到设备上运行中的进程
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetComputeRunningProcesses,
                      dev, &running_processes, pids_on_device);
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetComputeRunningProcesses can't get pids on device %d, "
           "return %d, str: %s", device_id, ret, nvml_error(ret));
    return;
  }
  top_result->sys_process_num = running_processes;

  gettimeofday(&cur, NULL);
  microsec = (cur.tv_sec - 1) * 1000UL * 1000UL + cur.tv_usec;
  top_result->checktime = microsec;
  // 获取当前设备的线程利用率
  ret = NVML_ENTRY_CALL(nvml_library_entry, nvmlDeviceGetProcessUtilization,
                        dev, processes_sample, &processes_num, microsec);
  if (unlikely(ret)) {
    LOGGER(VERBOSE, "nvmlDeviceGetProcessUtilization can't get process utilization on device: %d, "
          "return %d, str: %s", device_id, ret, nvml_error(ret));
    return;
  }

  if (likely(check_in_container())) {
    size_t isCgroupv2 = strlen(container_id);
    for (i = 0; i < processes_num; i++) {
      if (processes_sample[i].timeStamp >= top_result->checktime) {
        top_result->valid = 1;
        top_result->sys_current += GET_VALID_VALUE(processes_sample[i].smUtil);

        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) +
                    GET_VALID_VALUE(processes_sample[i].decUtil);
        top_result->sys_current += CODEC_NORMALIZE(codec_util);
        if (isCgroupv2 > 0) {
          if (likely(check_container_pid_v2(processes_sample[i].pid))) {
            top_result->user_current += GET_VALID_VALUE(processes_sample[i].smUtil);
            codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) +
                        GET_VALID_VALUE(processes_sample[i].decUtil);
            top_result->user_current += CODEC_NORMALIZE(codec_util); 
          }
        } else {
          if (likely(check_container_pid(processes_sample[i].pid))) {
            top_result->user_current += GET_VALID_VALUE(processes_sample[i].smUtil);
            codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) +
                        GET_VALID_VALUE(processes_sample[i].decUtil);
            top_result->user_current += CODEC_NORMALIZE(codec_util); 
          }
        }
      }
    }
  } else {
    for (i = 0; i < processes_num; i++) {
      if (processes_sample[i].timeStamp >= top_result->checktime) {
        top_result->valid = 1;
        top_result->sys_current += GET_VALID_VALUE(processes_sample[i].smUtil);

        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) +
                    GET_VALID_VALUE(processes_sample[i].decUtil);
        top_result->sys_current += CODEC_NORMALIZE(codec_util);
        top_result->user_current += GET_VALID_VALUE(processes_sample[i].smUtil);
        codec_util = GET_VALID_VALUE(processes_sample[i].encUtil) +
                     GET_VALID_VALUE(processes_sample[i].decUtil);
        top_result->user_current += CODEC_NORMALIZE(codec_util); 
      }
    }
  }
  LOGGER(VERBOSE, "device: %d, sys utilization: %d", device_id, top_result->sys_current);
  LOGGER(VERBOSE, "device: %d, used utilization: %d", device_id, top_result->user_current);
}

/** hook entrypoint */
CUresult cuDriverGetVersion(int *driverVersion) {
  CUresult ret;

  load_necessary_data();
  pthread_once(&g_init_set, initialization);

  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuDriverGetVersion, driverVersion);
  if (unlikely(ret)) {
    goto DONE;
  }

DONE:
  return ret;
}

CUresult cuInit(unsigned int flag) {
  CUresult ret;
  // 加载必要数据
  load_necessary_data();
  // 开启线程 初始化
  pthread_once(&g_init_set, initialization);
  // 调用官方api初始化驱动
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

  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuGetProcAddress, symbol, pfn,
                        cudaVersion, flags);
  if (ret == CUDA_SUCCESS) {
    for (i = 0; i < cuda_hook_nums; i++) {
      if (!strcmp(symbol, cuda_hooks_entry[i].name)) {
        LOGGER(DETAIL, "Match hook %s", symbol);
        *pfn = cuda_hooks_entry[i].fn_ptr;
        break;
      }
    }
  }

  return ret;
}

CUresult cuGetProcAddress_v2(const char *symbol, void **pfn, int cudaVersion,
                          cuuint64_t flags, void *symbolStatus) {
  CUresult ret;
  int i;

  load_necessary_data();
  pthread_once(&g_init_set, initialization);

  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuGetProcAddress_v2, symbol, pfn,
                        cudaVersion, flags, symbolStatus);
  if (ret == CUDA_SUCCESS) {
    for (i = 0; i < cuda_hook_nums; i++) {
      if (!strcmp(symbol, cuda_hooks_entry[i].name)) {
        LOGGER(DETAIL, "match hook %s", symbol);
        *pfn = cuda_hooks_entry[i].fn_ptr;
        break;
      }
    }
  }

  return ret;
}

CUresult cuMemAllocManaged(CUdeviceptr *dptr, size_t bytesize,
                           unsigned int flags) {
  size_t request_size = bytesize;
  CUresult ret;

  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    size_t used = 0;
    get_used_gpu_memory((void *)&used, ordinal);
    // 当开启了虚拟内存，当前已使用内存大于设备真实内存时，抛出oom
    if (g_vgpu_config.devices[ordinal].memory_oversold && 
        used > g_vgpu_config.devices[ordinal].device_memory) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
    // TODO 当开启了虚拟显存，在申请内存大于设备内存时，使用host内存分配
    if (g_vgpu_config.devices[ordinal].memory_oversold && 
        used + request_size > g_vgpu_config.devices[ordinal].device_memory) {
      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
      goto DONE;
    }
    // 没开启虚拟显存，设备内存不足抛出oom
    if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].total_memory)) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, flags);
DONE:
  return ret;
}

CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytesize) {
  size_t used = 0;
  size_t request_size = bytesize;
  CUresult ret;
  
  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  const char *cuda_err_string = NULL;
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    get_used_gpu_memory((void *)&used, ordinal);
    if (!g_vgpu_config.devices[ordinal].memory_oversold) {
      // 没开启虚拟现存正常校验和分配内存
      if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].total_memory)) {
        ret = CUDA_ERROR_OUT_OF_MEMORY;
        goto DONE;
      }
    } else if (unlikely(used > g_vgpu_config.devices[ordinal].device_memory)) {
      // 当开启了虚拟内存，当前已使用内存大于设备真实内存时，抛出oom
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    } else if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].device_memory)) {
      // 开启虚拟显存时，当申请内存大于设备真实分配的内存，使用host内存分配
FROM_HOST:
      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
      LOGGER(DETAIL, "[cuMemAlloc_v2] alloc mem from host, return %d, str: %s", ret, cuda_error(ret, &cuda_err_string));
      LOGGER(DETAIL, "[cuMemAlloc_v2] device %d: used %ld, request %ld, limit %ld", ordinal, used, request_size, g_vgpu_config.devices[ordinal].total_memory);
      goto DONE;
    } else {
      // 开启了虚拟内存，但是申请内存没超过可用内存，设备正常分配，当正常分配失败再使用本地内存分配
      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAlloc_v2, dptr, bytesize);
      if (ret == CUDA_SUCCESS) {
        goto DONE;
      }
      LOGGER(DETAIL, "[cuMemAlloc_v2] fail to alloc mem from device %d, "
            "return %d, str: %s", ordinal, ret, cuda_error(ret, &cuda_err_string));
      goto FROM_HOST;
    }
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAlloc_v2, dptr, bytesize);
  if (ret == CUDA_ERROR_OUT_OF_MEMORY && g_vgpu_config.devices[ordinal].memory_oversold) {
    goto FROM_HOST;
  }
DONE:
  return ret;
}


CUresult cuMemAlloc(CUdeviceptr *dptr, size_t bytesize) {
  size_t used = 0;
  size_t request_size = bytesize;
  CUresult ret;

  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  const char *cuda_err_string = NULL;
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    
    get_used_gpu_memory((void *)&used, ordinal);
    if (!g_vgpu_config.devices[ordinal].memory_oversold) {
      if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].total_memory)) {
        ret = CUDA_ERROR_OUT_OF_MEMORY;
        goto DONE;
      }
    } else if (unlikely(used > g_vgpu_config.devices[ordinal].device_memory)) {
      // 当开启了虚拟内存，当前已使用内存大于设备真实内存时，抛出oom
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    } else if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].device_memory)) {
      // 开启虚拟显存时，当申请内存大于设备真实分配的内存，使用host内存分配
FROM_HOST:
      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, bytesize, CU_MEM_ATTACH_GLOBAL);
      LOGGER(DETAIL, "[cuMemAlloc] alloc mem from host, return %d, str: %s", ret, cuda_error(ret, &cuda_err_string));
      LOGGER(DETAIL, "[cuMemAlloc] device %d: used %ld, request %ld, limit %ld", ordinal, used, request_size, g_vgpu_config.devices[ordinal].total_memory);
      goto DONE;
    } else {
      // 开启了虚拟内存，但是申请内存没超过可用内存，设备正常分配，当正常分配失败再使用本地内存分配
      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAlloc, dptr, bytesize);
      if (ret == CUDA_SUCCESS) {
        goto DONE;
      }
      LOGGER(DETAIL, "[cuMemAlloc] fail to alloc mem from device %d, "
            "return %d, str: %s", ordinal, ret, cuda_error(ret, &cuda_err_string));
      goto FROM_HOST;
    }
    
  }

  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAlloc, dptr, bytesize);
  if (ret == CUDA_ERROR_OUT_OF_MEMORY && g_vgpu_config.devices[ordinal].memory_oversold) {
    goto FROM_HOST;
  }
DONE:
  return ret;
}

CUresult cuMemAllocPitch_v2(CUdeviceptr *dptr, size_t *pPitch,
                            size_t WidthInBytes, size_t Height,
                            unsigned int ElementSizeBytes) {
  size_t used = 0;
  size_t request_size = ROUND_UP(WidthInBytes * Height, ElementSizeBytes);
  CUresult ret;

  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  const char *cuda_err_string = NULL;
  if (g_vgpu_config.devices[ordinal].memory_limit) {

    get_used_gpu_memory((void *)&used, ordinal);
    if (!g_vgpu_config.devices[ordinal].memory_oversold) {
      if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].total_memory)) {
        ret = CUDA_ERROR_OUT_OF_MEMORY;
        goto DONE;
      }
    } else if (unlikely(used > g_vgpu_config.devices[ordinal].device_memory)) {
      // 当开启了虚拟内存，当前已使用内存大于设备真实内存时，抛出oom
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    } else if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].device_memory)) {
      // 开启虚拟显存时，当申请内存大于设备真实分配的内存，使用host内存分配
FROM_HOST:
      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, request_size, CU_MEM_ATTACH_GLOBAL);
      LOGGER(DETAIL, "[cuMemAllocPitch_v2] alloc mem from host, return %d, str: %s", ret, cuda_error(ret, &cuda_err_string));
      LOGGER(DETAIL, "[cuMemAllocPitch_v2] device %d: used %ld, request %ld, limit %ld", ordinal, used, request_size, g_vgpu_config.devices[ordinal].total_memory);
      goto DONE;
    } else {
      // 开启了虚拟内存，但是申请内存没超过可用内存，设备正常分配，当正常分配失败再使用本地内存分配
      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocPitch_v2, dptr, pPitch,
                            WidthInBytes, Height, ElementSizeBytes);
      if (ret == CUDA_SUCCESS) {
        goto DONE;
      }
      LOGGER(DETAIL, "[cuMemAllocPitch_v2] fail to alloc mem from device %d, "
            "return %d, str: %s", ordinal, ret, cuda_error(ret, &cuda_err_string));
      goto FROM_HOST;
    }
  }

  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocPitch_v2, dptr, pPitch,
                        WidthInBytes, Height, ElementSizeBytes);
  if (ret == CUDA_ERROR_OUT_OF_MEMORY && g_vgpu_config.devices[ordinal].memory_oversold) {
    goto FROM_HOST;
  }
DONE:
  return ret;
}

CUresult cuMemAllocPitch(CUdeviceptr *dptr, size_t *pPitch, size_t WidthInBytes,
                         size_t Height, unsigned int ElementSizeBytes) {
  size_t used = 0;
  size_t request_size = ROUND_UP(WidthInBytes * Height, ElementSizeBytes);

  CUresult ret;
  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  const char *cuda_err_string = NULL;
  if (g_vgpu_config.devices[ordinal].memory_limit) {

    get_used_gpu_memory((void *)&used, ordinal);
    if (!g_vgpu_config.devices[ordinal].memory_oversold) {
      if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].total_memory)) {
        ret = CUDA_ERROR_OUT_OF_MEMORY;
        goto DONE;
      }
    } else if (unlikely(used > g_vgpu_config.devices[ordinal].device_memory)) {
      // 当开启了虚拟内存，当前已使用内存大于设备真实内存时，抛出oom
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    } else if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].device_memory)) {
      // 开启虚拟显存时，当申请内存大于设备真实分配的内存，使用host内存分配
FROM_HOST:
      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocManaged, dptr, request_size, CU_MEM_ATTACH_GLOBAL);
      LOGGER(DETAIL, "[cuMemAllocPitch] alloc mem from host, return %d, str: %s", ret, cuda_error(ret, &cuda_err_string));
      LOGGER(DETAIL, "[cuMemAllocPitch] device %d: used %ld, request %ld, limit %ld", ordinal, used, request_size, g_vgpu_config.devices[ordinal].total_memory);
      goto DONE;
    } else {
      // 开启了虚拟内存，但是申请内存没超过可用内存，设备正常分配，当正常分配失败再使用本地内存分配
      ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocPitch, dptr, pPitch,
                            WidthInBytes, Height, ElementSizeBytes);
      if (ret == CUDA_SUCCESS) {
        goto DONE;
      }
      LOGGER(DETAIL, "[cuMemAllocPitch] fail to alloc mem from device %d, "
            "return %d, str: %s", ordinal, ret, cuda_error(ret, &cuda_err_string));
      goto FROM_HOST;
    }
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocPitch, dptr, pPitch,
                        WidthInBytes, Height, ElementSizeBytes);
  if (ret == CUDA_ERROR_OUT_OF_MEMORY && g_vgpu_config.devices[ordinal].memory_oversold) {
    goto FROM_HOST;
  }
DONE:
  return ret;
}

CUresult cuMemAllocAsync(CUdeviceptr *dptr, size_t bytesize, CUstream hStream) {
  size_t used = 0;
  size_t request_size = bytesize;
  CUresult ret;
  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    get_used_gpu_memory((void *)&used, ordinal);
    if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].total_memory)) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocAsync, dptr, bytesize, hStream);
DONE:
  return ret;
}

CUresult cuMemAllocAsync_ptsz(CUdeviceptr *dptr, size_t bytesize, CUstream hStream) {
  size_t used = 0;
  size_t request_size = bytesize;
  CUresult ret;
  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    get_used_gpu_memory((void *)&used, ordinal);
    if (unlikely(used + request_size > g_vgpu_config.devices[ordinal].total_memory)) {
      ret = CUDA_ERROR_OUT_OF_MEMORY;
      goto DONE;
    }
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemAllocAsync_ptsz, dptr,
                                 bytesize, hStream);
DONE:
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

static CUresult
cuArrayCreate_helper(const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  size_t used = 0;
  size_t base_size = 0;
  size_t request_size = 0;
  CUresult ret = CUDA_SUCCESS;

  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }

  if (g_vgpu_config.devices[ordinal].memory_limit) {
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
  return ret;
}

CUresult cuArrayCreate_v2(CUarray *pHandle,
                          const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  CUresult ret;

  ret = cuArrayCreate_helper(pAllocateArray);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }

  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuArrayCreate_v2, pHandle,
                        pAllocateArray);
DONE:
  return ret;
}

CUresult cuArrayCreate(CUarray *pHandle,
                       const CUDA_ARRAY_DESCRIPTOR *pAllocateArray) {
  CUresult ret;

  ret = cuArrayCreate_helper(pAllocateArray);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }

  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuArrayCreate, pHandle,
                        pAllocateArray);
DONE:
  return ret;
}

static CUresult
cuArray3DCreate_helper(const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  size_t used = 0;
  size_t base_size = 0;
  size_t request_size = 0;
  CUresult ret = CUDA_SUCCESS;

  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }

  if (g_vgpu_config.devices[ordinal].memory_limit) {
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
  return ret;
}

CUresult cuArray3DCreate_v2(CUarray *pHandle,
                            const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  CUresult ret;

  ret = cuArray3DCreate_helper(pAllocateArray);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }

  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuArray3DCreate_v2, pHandle,
                        pAllocateArray);
DONE:
  return ret;
}

CUresult cuArray3DCreate(CUarray *pHandle,
                         const CUDA_ARRAY3D_DESCRIPTOR *pAllocateArray) {
  CUresult ret;

  ret = cuArray3DCreate_helper(pAllocateArray);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuArray3DCreate, pHandle,
                        pAllocateArray);
DONE:
  return ret;
}

CUresult
cuMipmappedArrayCreate(CUmipmappedArray *pHandle,
                       const CUDA_ARRAY3D_DESCRIPTOR *pMipmappedArrayDesc,
                       unsigned int numMipmapLevels) {
  size_t used = 0;
  size_t base_size = 0;
  size_t request_size = 0;
  CUresult ret;

  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }

  if (g_vgpu_config.devices[ordinal].memory_limit) {
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
  return ret;
}

CUresult cuDeviceTotalMem_v2(size_t *bytes, CUdevice dev) {
  // 当开启了vgpu，则返回受限制的显存大小
  if (g_vgpu_config.devices[dev].memory_limit) {
    *bytes = g_vgpu_config.devices[dev].total_memory;

    return CUDA_SUCCESS;
  }
  // 否则，直接调用api查询实际大小
  return CUDA_ENTRY_CALL(cuda_library_entry, cuDeviceTotalMem_v2, bytes, dev);
}

CUresult cuDeviceTotalMem(size_t *bytes, CUdevice dev) {
  if (g_vgpu_config.devices[dev].memory_limit) {
    *bytes = g_vgpu_config.devices[dev].total_memory;

    return CUDA_SUCCESS;
  }

  return CUDA_ENTRY_CALL(cuda_library_entry, cuDeviceTotalMem, bytes, dev);
}

CUresult cuMemGetInfo_v2(size_t *free, size_t *total) {
  size_t used = 0;
  CUresult ret;
  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }

  if (g_vgpu_config.devices[ordinal].memory_limit) {
    // 获取已使用的显卡内存
    get_used_gpu_memory((void *)&used, ordinal);

    *total = g_vgpu_config.devices[ordinal].total_memory;
    // 当已使用大于分配量，可用量为0，否则为 分配的显存总量 - 已使用的显存
    *free = used > g_vgpu_config.devices[ordinal].total_memory ? 0 : g_vgpu_config.devices[ordinal].total_memory - used;
    LOGGER(VERBOSE, "[cuMemGetInfo_v2] device %d, used %lu, free %lu, total %lu", ordinal, used, *free, *total);
    return CUDA_SUCCESS;
  }
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemGetInfo_v2, free, total);
DONE:
  return ret;
}

CUresult cuMemGetInfo(size_t *free, size_t *total) {
  size_t used = 0;
  CUresult ret;
  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  if (g_vgpu_config.devices[ordinal].memory_limit) {
    get_used_gpu_memory((void *)&used, ordinal);
    *total = g_vgpu_config.devices[ordinal].total_memory;
    *free = used > g_vgpu_config.devices[ordinal].total_memory ? 0 : g_vgpu_config.devices[ordinal].total_memory - used;
    LOGGER(VERBOSE, "[cuMemGetInfo] device %d, used %lu, free %lu, total %lu", ordinal, used, *free, *total);
    return CUDA_SUCCESS;
  }
  // 当没有开启vcuda，直接调用原生的cuda api
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuMemGetInfo, free, total);
DONE:
  return ret;
}

CUresult cuLaunchKernel_ptsz(CUfunction f, unsigned int gridDimX,
                             unsigned int gridDimY, unsigned int gridDimZ,
                             unsigned int blockDimX, unsigned int blockDimY,
                             unsigned int blockDimZ,
                             unsigned int sharedMemBytes, CUstream hStream,
                             void **kernelParams, void **extra) {
  CUresult ret;
  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  // 计算资源限制，根据网格数 更新当前cuda核心数
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
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  // 计算资源限制，根据网格数 更新当前cuda核心数
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
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
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
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }    
  // TODO 利用率限制         
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
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }    
  // 计算资源限制，将当前cuda核心数减1
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
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }    
  // 计算资源限制，根据网格数 更新当前cuda核心数
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
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
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
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
    goto DONE;
  }
  rate_limiter(grid_width * grid_height, g_block_x[ordinal] * g_block_y[ordinal] * g_block_z[ordinal], ordinal);
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuLaunchGrid, f, grid_width,grid_height);
DONE:
  return ret;
}

CUresult cuLaunchGridAsync(CUfunction f, int grid_width, int grid_height,
                           CUstream hStream) {
  CUresult ret;  
  CUdevice ordinal;
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
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
  ret = CUDA_ENTRY_CALL(cuda_library_entry, cuCtxGetDevice, &ordinal);
  if (ret != CUDA_SUCCESS) {
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