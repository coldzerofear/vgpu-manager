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

int main(int argc, char **argv)
{
    if (argc != 4) {
        printf("wrong arguments: mem_managed_tool device_index size(MB) FLAG\n");
        return -1;
    }
    int device_id = str2int(argv[1]);
    int size_mb = str2int(argv[2]);
    int flag = str2int(argv[3]);

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
    CUresult res;
    switch (flag)
    {
    case 0:
        res = cuMemAllocManaged(&d, size, CU_MEM_ATTACH_GLOBAL);
        break;
    case 1:
        res = cuMemAllocManaged(&d, size, CU_MEM_ATTACH_HOST);
        break;
    case 2:
        res = cuMemAllocManaged(&d, size, CU_MEM_ATTACH_SINGLE);
        break;
    default:
        printf("Flag value only supports 0:(CU_MEM_ATTACH_GLOBAL) 1:(CU_MEM_ATTACH_HOST) 2:(CU_MEM_ATTACH_SINGLE)");
        return -1;
    }
    if (res != CUDA_SUCCESS) {
        const char *msg = NULL;
        cuGetErrorString(res, &msg);
        printf("cuMemAllocManaged failed, error: %s\n", msg);
        return -1;
    }
    CUdeviceptr base;
    size_t query_size;
    cuMemGetAddressRange(&base, &query_size, d);
    printf("Allocated size: %zu bytes\n", query_size);
    memset((void *)d, 0, size);
    while (1){
    }
    cuMemFree_v2(d);
    return 0;
}