#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <string.h>
#include <cuda.h>
#include <nvml.h>

int str2int(char *str) {
    int l = strlen(str);
    int res = 0;
    for (int i = 0; i < l; i++) {
        res *= 10;
        res += str[i] - '0';
    }
    return res;
}
//　gcc mem_view_tool.c -o mem_view_tool -I/usr/local/cuda/include -L/usr/local/cuda/lib64 -l:libnvidia-ml.so.1 -l:libcuda.so.1
int main(int argc, char **argv) {
    if (argc != 2) {
      printf("wrong arguments: %s [device_id]\n", argv[0]);
      return -1;
    }
    int device_id = str2int(argv[1]);

    if (nvmlInit_v2() != NVML_SUCCESS) {
      printf("nvmlInit_v2 failed\n");
      return -1;
    }

    if (cuInit(0) != CUDA_SUCCESS) {
      printf("cuInit failed\n");
      return -1;
    }
    nvmlDevice_t device;
    if (nvmlDeviceGetHandleByIndex_v2(device_id, &device) != NVML_SUCCESS) {
      printf("nvmlDeviceGetHandleByIndex_v2 failed\n");
      return -1;
    }
    nvmlMemory_t memory = {};
    if (nvmlDeviceGetMemoryInfo(device, &memory) != NVML_SUCCESS) {
      printf("nvmlDeviceGetMemoryInfo_v2 failed\n");
      return -1;
    }
    printf("device %d nvml memory, total: %llu, used: %llu, free: %llu\n", device_id, memory.total>>20, memory.used>>20, memory.free>>20);

    //获得设备句柄
    CUdevice cuDevice;
    if (cuDeviceGet(&cuDevice, device_id) != CUDA_SUCCESS) {
      printf("cuDeviceGet failed\n");
      return -1;
    }
    //创建上下文
    CUcontext cuContext;
    if (cuCtxCreate_v2(&cuContext, 0, cuDevice) != CUDA_SUCCESS) {
      printf("cuCtxCreate_v2 failed\n");
      return -1;
    }
    cuCtxSetCurrent(cuContext);
    size_t free;
    size_t total;
    if (cuMemGetInfo_v2(&free, &total) != CUDA_SUCCESS) {
        printf("cuMemGetInfo_v2 failed\n");
        return -1;
    }
    size_t used = (total - free);
    printf("device %d cuda memory, total: %llu, used: %llu, free: %llu\n", device_id, total>>20, used>>20, free>>20);

    nvmlProcessInfo_t pids_on_device[1024];
    unsigned int size_on_device = 1024;
    if (nvmlDeviceGetComputeRunningProcesses(device, &size_on_device, pids_on_device) != NVML_SUCCESS) {
      printf("nvmlDeviceGetComputeRunningProcesses failed\n");
      return -1;
    }
    int i;
    for (i = 0; i < size_on_device; i++) {
      printf("ComputeProcesses pid[%d] use memory: %lld\n", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory>>20);
    }

    size_on_device = 1024;
    nvmlProcessInfo_t pids_on_device1[1024];
    if (nvmlDeviceGetGraphicsRunningProcesses(device, &size_on_device, pids_on_device1) != NVML_SUCCESS) {
      printf("nvmlDeviceGetGraphicsRunningProcesses failed\n");
      return -1;
    }
    for (i = 0; i < size_on_device; i++) {
      printf("GraphicsProcesses pid[%d] use memory: %lld\n", pids_on_device1[i].pid, pids_on_device1[i].usedGpuMemory>>20);
    }
}