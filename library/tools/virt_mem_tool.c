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

// 虚拟显存分配
CUresult vmm_alloc(void **ptr, size_t size, int device_id) {
    CUresult rs;
    char *err = NULL;
    // 校验设备是否支持虚拟内存管理
    int deviceSupportsVmm;
    rs = cuDeviceGetAttribute(&deviceSupportsVmm, CU_DEVICE_ATTRIBUTE_VIRTUAL_MEMORY_MANAGEMENT_SUPPORTED, device_id);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuDeviceGetAttribute failed: %s\n", err);
        return rs;
    }
    if (deviceSupportsVmm != 0) {
        // `device` 
        printf("device %d supports Virtual Memory Management\n", device_id);
    } else {
        return CUDA_ERROR_NOT_SUPPORTED;
    }

    // 获取设备显存粒度
    CUmemAllocationProp prop = {};
    prop.type          = CU_MEM_ALLOCATION_TYPE_PINNED;
    prop.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    prop.location.id   = device_id;
 
    size_t granularity = 0;

    rs = cuMemGetAllocationGranularity(&granularity, &prop, CU_MEM_ALLOC_GRANULARITY_MINIMUM);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuMemGetAllocationGranularity failed: %s\n", err);
        return rs;
    }
 
    size = ((size - 1) / granularity + 1) * granularity;
    
    // 虚拟地址分配
    CUdeviceptr dptr;
    rs = cuMemAddressReserve(&dptr, size, 0, 0, 0);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuMemAddressReserve failed: %s\n", err);
        return rs;
    }
    // 申请物理地址
    CUmemGenericAllocationHandle allocationHandle;
    rs = cuMemCreate(&allocationHandle, size, &prop, 0);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuMemCreate failed: %s\n", err);
        return rs;
    }
    // 虚拟地址和物理地址映射
    rs = cuMemMap(dptr, size, 0, allocationHandle, 0);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuMemMap failed: %s\n", err);
        return rs;
    }
    // 释放物理地址handle 此处并不会真正释放物理地址
    rs = cuMemRelease(allocationHandle);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuMemRelease failed: %s\n", err);
        return rs;
    }
    
    // 设置访问权限
    CUmemAccessDesc accessDescriptor = {};
    accessDescriptor.location.id   = prop.location.id;
    accessDescriptor.location.type = prop.location.type;
    accessDescriptor.flags         = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
    rs = cuMemSetAccess(dptr, size, &accessDescriptor, 1);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuMemSetAccess failed: %s\n",  err);
        return rs;
    }
 
    *ptr = (void *)dptr;
 
    return CUDA_SUCCESS;
}


CUresult vmm_free(void *ptr, size_t size, int device_id) {
    if (!ptr) {
        return CUDA_SUCCESS;
    }
 
    CUmemAllocationProp prop = {};

    prop.type          = CU_MEM_ALLOCATION_TYPE_PINNED;
    prop.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    prop.location.id   = device_id;
 
    size_t granularity = 0;
    CUresult rs;
    char *err = NULL;
    rs = cuMemGetAllocationGranularity(&granularity, &prop, CU_MEM_ALLOC_GRANULARITY_MINIMUM);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuMemGetAllocationGranularity failed: %s\n", err);
        return rs;
    }
 
    size = ((size - 1) / granularity + 1) * granularity;
    rs = cuMemUnmap((CUdeviceptr)ptr, size);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuMemUnmap failed: %s\n", err);
        return rs;
    }
    rs = cuMemAddressFree((CUdeviceptr)ptr, size);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuMemAddressFree failed: %s\n", err);
        return rs;
    }

    return CUDA_SUCCESS;
}

int main(int argc, char **argv)
{
    if (argc != 3) {
        printf("wrong arguments: mem_managed_tool device_index size(MB)");
        return -1;
    }
    int device_id = str2int(argv[1]);
    int size_mb = str2int(argv[2]);
    
    CUresult rs;
    char *err = NULL;
    
    //初始化设备
    rs = cuInit(0);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuInit failed: %s\n", err);
        return -1;
    }

    //获得设备句柄
    CUdevice cuDevice;
    rs = cuDeviceGet(&cuDevice, device_id);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuDeviceGet failed: %s\n", err);
        return -1;
    }

    //创建上下文
    CUcontext cuContext;
    rs = cuCtxCreate_v2(&cuContext, 0, cuDevice);
    if (rs != CUDA_SUCCESS) {
        cuGetErrorString(rs, &err);
        printf("cuCtxCreate_v2 failed: %s\n", err);
        return -1; 
    }

    //显存中分配向量空间
    size_t size = size_mb;
    size *= 1024 * 1024;
    CUdeviceptr d;
    cuCtxSetCurrent(cuContext);
    if (vmm_alloc((void **)&d, size, device_id) != CUDA_SUCCESS) {
        printf("vmm_alloc failed\n");
        return -1;
    }
    // memset(d, 0, size);
    while (1){
    }
    if (vmm_free((void *)d, size, device_id) != CUDA_SUCCESS) {
        printf("vmm_free failed\n");
        return -1;
    }
    return 0;
}