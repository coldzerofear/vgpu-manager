#include <stdio.h>
#include <unistd.h>
#include <cuda_runtime.h>

#define N 1000000

// 初始化数据的核函数
__global__ void initKernel(int* data) {
    int idx = blockIdx.x * blockDim.x + threadIdx.x;
    if (idx < N) {
        data[idx] = idx;
    }
}

int main() {
    int *data;
    cudaStream_t stream;

    // 1. 分配统一内存
    cudaMallocManaged(&data, N * sizeof(int));

    printf("cudaMallocManaged\n");
    sleep(10);

    // 2. 创建CUDA流
    cudaStreamCreate(&stream);

    // 3. 预取数据到当前GPU (异步)
    cudaMemPrefetchAsync(data, N * sizeof(int), 0, stream); // 0 表示当前GPU

    // 4. 在流中执行初始化内核
    dim3 block(256);
    dim3 grid((N + block.x - 1) / block.x);
    initKernel<<<grid, block, 0, stream>>>(data);

    printf("cudaMemPrefetchAsync to gpu\n");
    sleep(10);

    // 5. 预取回CPU以便后续处理
    cudaMemPrefetchAsync(data, N * sizeof(int), cudaCpuDeviceId, stream);

    // 6. 同步流确保操作完成
    cudaStreamSynchronize(stream);

    printf("cudaMemPrefetchAsync to cpu\n");
    sleep(10);

    // 7. 验证结果 (CPU访问)
    for (int i = 0; i < 10; i++) {
        printf("data[%d] = %d\n", i, data[i]);
    }

    // 8. 清理资源
    cudaFree(data);
    cudaStreamDestroy(stream);

    return 0;
}