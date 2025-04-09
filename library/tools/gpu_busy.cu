#include <stdio.h>

__global__ void busyKernel() {
    while (true) {
        volatile int x = 1;
        x = x * 2;
    }
}

int main() {
    printf("Starting GPU busy kernel...\n");

    int numBlocks = 1;
    int threadsPerBlock = 1;

    busyKernel<<<numBlocks, threadsPerBlock>>>();

    cudaError_t err = cudaGetLastError();
    if (err != cudaSuccess) {
        printf("CUDA error: %s\n", cudaGetErrorString(err));
        return -1;
    }

    printf("GPU is now busy. Press Ctrl+C to terminate the program.\n");

    while (true) {}

    return 0;
}