#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <string.h>
#include <cuda.h>
#include <nvml.h>
#include <sys/time.h>

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
      printf("wrong arguments: %s [device_index]\n", argv[0]);
      return -1;
    }
    printf("current pid[%d]\n", getpid());

    int device_id = str2int(argv[1]);

    if (nvmlInit_v2() != NVML_SUCCESS) {
      printf("nvmlInit_v2 failed\n");
      return -1;
    }

    if (cuInit(0) != CUDA_SUCCESS) {
      printf("cuInit failed\n");
      return -1;
    }

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
    printf("device %d nvml memory, total: %lluMB, used: %lluMB, free: %lluMB\n", device_id, memory.total>>20, memory.used>>20, memory.free>>20);

    size_t free;
    size_t total;
    if (cuMemGetInfo_v2(&free, &total) != CUDA_SUCCESS) {
        printf("cuMemGetInfo_v2 failed\n");
        return -1;
    }
    size_t used = (total - free);

    printf("device %d cuda memory, total: %zuMB, used: %zuMB, free: %zuMB\n", device_id, total>>20, used>>20, free>>20);

    nvmlProcessInfo_t pids_on_device[1024];
    unsigned int size_on_device = 1024;
    if (nvmlDeviceGetComputeRunningProcesses(device, &size_on_device, pids_on_device) != NVML_SUCCESS) {
      printf("nvmlDeviceGetComputeRunningProcesses failed\n");
      return -1;
    }
    printf("---------------ComputeProcesses size %d---------------\n", size_on_device);
    int i;
    for (i = 0; i < size_on_device; i++) {
      printf("ComputeProcesses pid[%d] use memory: %lldMB\n", pids_on_device[i].pid, pids_on_device[i].usedGpuMemory>>20);
    }
    size_on_device = 1024;
    nvmlProcessInfo_t pids_on_device1[1024];
    if (nvmlDeviceGetGraphicsRunningProcesses(device, &size_on_device, pids_on_device1) != NVML_SUCCESS) {
      printf("nvmlDeviceGetGraphicsRunningProcesses failed\n");
      return -1;
    }
    printf("---------------GraphicProcesses size %d---------------\n", size_on_device);
    for (i = 0; i < size_on_device; i++) {
      printf("GraphicProcesses pid[%d] use memory: %lldMB\n", pids_on_device1[i].pid, pids_on_device1[i].usedGpuMemory>>20);
    }
    struct timeval cur, prev;
    struct timeval temp = {1, 0};
    gettimeofday(&cur, NULL);
    timersub(&cur, &temp, &prev);
    uint64_t microsec = (uint64_t)prev.tv_sec * 1000000ULL + prev.tv_usec;

    nvmlProcessUtilizationSample_t processes_sample[1024];
    unsigned int processes_num = 1024;
    if (nvmlDeviceGetProcessUtilization(device, processes_sample, &processes_num, microsec) != NVML_SUCCESS) {
       printf("nvmlDeviceGetProcessUtilization failed\n");
       return -1;
    }
    printf("--------------ProcessUtilization size %d--------------\n", processes_num);
    for (i = 0; i < processes_num; i++) {
      printf("ProcessUtilization pid[%d] sm_util: %d, mem_util: %d\n", processes_sample[i].pid, processes_sample[i].smUtil, processes_sample[i].memUtil);
    }

    // @Base 535.104
    nvmlProcessDetailList_t list = {};
    list.version = nvmlProcessDetailList_v1;
    list.mode = 0;
    list.numProcArrayEntries = 0;
    list.procArray= NULL;
    nvmlReturn_t ret = nvmlDeviceGetRunningProcessDetailList(device, &list);
    if (ret != NVML_SUCCESS && ret != NVML_ERROR_INSUFFICIENT_SIZE) {
        printf("nvmlDeviceGetRunningProcessDetailList failed: %s\n", nvmlErrorString(ret));
        return -1;
    }
    printf("--------------ProcessDetailList size %d---------------\n", list.numProcArrayEntries);
    list.procArray = (nvmlProcessDetail_v1_t*)malloc(list.numProcArrayEntries * sizeof(nvmlProcessDetail_v1_t));
    ret = nvmlDeviceGetRunningProcessDetailList(device, &list);
    if (ret != NVML_SUCCESS) {
       printf("nvmlDeviceGetRunningProcessDetailList failed: %s\n", nvmlErrorString(ret));
       return -1;
    }
    for (i = 0; i < list.numProcArrayEntries; i++) {
        printf("ProcessDetail pid[%d] use memory: %lldMB\n", list.procArray[i].pid, list.procArray[i].usedGpuMemory>>20);
    }
}