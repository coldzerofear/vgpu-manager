
#include <cuda_runtime.h>
#include <iostream>
#include <csignal>
#include <cstdlib>
#include <cmath> // 引入数学库，用于增加计算复杂度

// 定义一个简单的信号处理函数，用于捕获Ctrl+C信号并退出程序
void signalHandler(int signum) {
    std::cout << "捕获到信号 " << signum << "，程序将退出..." << std::endl;
    cudaDeviceReset(); // 重置设备，释放所有资源
    exit(signum); // 退出程序
}

// 定义矩阵的大小
const int N = 2048; // 增大矩阵大小，增加计算量
const int NUM_ITERATIONS = 99999999; // 迭代次数，可以根据需要调整

// CUDA内核函数，用于矩阵加法和一些额外的复杂计算
__global__ void matrixAddAndCompute(float* A, float* B, float* C, int iterations) {
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    int j = blockIdx.y * blockDim.y + threadIdx.y;
    int index = i * N + j;

    for (int iter = 0; iter < iterations; iter++) {
       // if (i < N && j < N) {
            C[index] += A[index] + B[index]; // 多次累加，增加计算量

            // 添加一些额外的复杂计算，比如计算平方根和三角函数
            float temp = sqrt(C[index]) * sin(C[index]);
            // 这里我们并不使用temp的结果，只是为了增加计算复杂度
        //}
    }
}

int main() {
    // 注册信号处理函数
    signal(SIGINT, signalHandler);

    // 分配和初始化主机内存
    float* h_A = (float*)malloc(N * N * sizeof(float));
    float* h_B = (float*)malloc(N * N * sizeof(float));
    for (int i = 0; i < N * N; i++) {
        h_A[i] = static_cast<float>(rand()) / RAND_MAX;
        h_B[i] = static_cast<float>(rand()) / RAND_MAX;
    }

    // 分配设备内存
    float* d_A = nullptr;
    float* d_B = nullptr;
    float* d_C = nullptr;
    cudaMalloc((void**)&d_A, N * N * sizeof(float));
    cudaMalloc((void**)&d_B, N * N * sizeof(float));
    cudaMalloc((void**)&d_C, N * N * sizeof(float));

    // 初始化设备上的C矩阵为0
    cudaMemset(d_C, 0, N * N * sizeof(float));

    // 将数据从主机复制到设备
    cudaMemcpy(d_A, h_A, N * N * sizeof(float), cudaMemcpyHostToDevice);
    cudaMemcpy(d_B, h_B, N * N * sizeof(float), cudaMemcpyHostToDevice);

    // 定义块和网格的大小
    dim3 blockSize(1024, 1024);
    dim3 gridSize((N + blockSize.x - 1) / blockSize.x, (N + blockSize.y - 1) / blockSize.y);

    // 无限循环，直到捕获到Ctrl+C信号
    while (true) {
        // 执行矩阵加法运算和额外的复杂计算
        matrixAddAndCompute<<<gridSize, blockSize>>>(d_A, d_B, d_C, NUM_ITERATIONS);

        // 同步设备，确保计算完成
        cudaDeviceSynchronize();

        // 你可以在这里添加一些其他的计算或操作，但请注意，这仍然是一个无限循环
    }

    // 注意：由于存在无限循环，下面的代码实际上不会被执行，除非在循环中添加适当的退出条件
    // 释放设备内存
    // cudaFree(d_A);
    // cudaFree(d_B);
    // cudaFree(d_C);

    // 释放主机内存
    // free(h_A);
    // free(h_B);

    return 0;
}