#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <string.h>
#include <cuda.h>

int str2int(char *str) {
    int l = strlen(str);
    int res = 0;
    for (int i = 0; i < l; i++) {
        res *= 10;
        res += str[i] - '0';
    }
    return res;
}

int main(int argc, char **argv) {
    if (argc != 4) {
        printf("wrong arguments: %s [device_index] [memory_size(MiB)] [time(Second)]\n", argv[0]);
        return -1;
    }
    int device_id = str2int(argv[1]);
    int size_mb = str2int(argv[2]);
    int time = str2int(argv[3]);

    //初始化设备
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

    //显存中分配向量空间
    size_t size = size_mb;
    size *= 1024 * 1024;
    CUdeviceptr d;
    cuCtxSetCurrent(cuContext);
    CUresult ret;
    ret = cuMemAlloc_v2(&d, size);
    if (ret != CUDA_SUCCESS) {
        const char *err = NULL;
        cuGetErrorString(ret, &err);
        printf("cuMemAlloc_v2 failed: %s\n", err);
        return -1;
    }
    while (1){
        sleep(time);
        break;
    }
    cuMemFree_v2(d);
    return 0;
}