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

int main(int argc, char **argv)  {
    if (argc != 5) {
      printf("wrong arguments: %s [device_index] [memory_size(MiB)] [type: 0(device) | 1(host)] [time(Second)]\n", argv[0]);
      return -1;
    }
    int device_id = str2int(argv[1]);
    int size_mb = str2int(argv[2]);
    int type = str2int(argv[3]);
    int time = str2int(argv[4]);

    if (cuInit(0) != CUDA_SUCCESS) {
      printf("cuInit failed\n");
      return -1;
    }

    CUdevice device;
    if (cuDeviceGet(&device, device_id) != CUDA_SUCCESS) {
        printf("cuDeviceGet failed\n");
        return -1;
    }

    CUcontext ctx;
    if (cuCtxCreate_v2(&ctx, 0, device) != CUDA_SUCCESS) {
        printf("cuCtxCreate_v2 failed\n");
        return -1;
    }

    cuDevicePrimaryCtxRetain(&ctx, device);

    CUmemoryPool pool;
    CUmemPoolProps poolProps = {0};
    poolProps.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
    poolProps.handleTypes = CU_MEM_HANDLE_TYPE_NONE;
    switch (type) {
      case 0:
        poolProps.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
        poolProps.location.id = device;
        break;
      case 1:
        poolProps.location.type = CU_MEM_LOCATION_TYPE_HOST;
        break;
      default:
        printf("Type value only supports 0:(device) 1:(host)");
        return -1;
    }

    CUresult result = cuMemPoolCreate(&pool, &poolProps);
    if (result != CUDA_SUCCESS) {
        fprintf(stderr, "cuMemPoolCreate failed: %d\n", result);
        return -1;
    }

    cuDeviceSetMemPool(device, pool);

    CUstream stream;
    cuStreamCreate(&stream, 0);

    CUdeviceptr d_ptr;
    size_t size = size_mb;
    size *= 1024 * 1024;
    cuMemAllocFromPoolAsync(&d_ptr, size, pool, stream);
    cuStreamSynchronize(stream);

    printf("Allocated size: %zu bytes\n", size);
    // memset((void *)d_ptr, 0, size);

    sleep(time);

    cuMemFreeAsync(d_ptr, stream);
    cuStreamSynchronize(stream);

    cuStreamDestroy(stream);
    cuMemPoolDestroy(pool);
    cuDevicePrimaryCtxRelease(device);

    return 0;
}