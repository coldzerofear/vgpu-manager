/*
 * Kernel launch smoke test - exercises the rate-limiter path and
 * vgpu-manager's cuLaunchKernel interception.
 *
 * Ported from HAMi-core/test/test_runtime_launch.cu.
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <stdio.h>

#include "test_utils.h"

__global__ void add(float *a, float *b, float *c) {
  int idx = threadIdx.x;
  c[idx] = a[idx] + b[idx];
}

__global__ void compute_kernel(double *data, int N) {
  int tid = blockIdx.x * blockDim.x + threadIdx.x;
  if (tid < N) {
    double temp = sin(data[tid]) * cos(data[tid]);
    data[tid] = temp;
  }
}

int main(void) {
  float *a, *b, *c;
  CHECK_RUNTIME_API(cudaMalloc(&a, 1024 * sizeof(float)));
  CHECK_RUNTIME_API(cudaMalloc(&b, 1024 * sizeof(float)));
  CHECK_RUNTIME_API(cudaMalloc(&c, 1024 * sizeof(float)));
  add<<<1, 1024>>>(a, b, c);
  CHECK_RUNTIME_API(cudaDeviceSynchronize());

  int N = 1 << 27;
  double *d_data;
  CHECK_RUNTIME_API(cudaMalloc(&d_data, N * sizeof(double)));

  int threadsPerBlock = 256;
  int blocks = (N + threadsPerBlock - 1) / threadsPerBlock;
  int num_launches = 100;
  for (int i = 0; i < num_launches; ++i) {
    compute_kernel<<<blocks, threadsPerBlock>>>(d_data, N);
    CHECK_RUNTIME_API(cudaDeviceSynchronize());
  }
  CHECK_RUNTIME_API(cudaFree(d_data));
  CHECK_RUNTIME_API(cudaFree(a));
  CHECK_RUNTIME_API(cudaFree(b));
  CHECK_RUNTIME_API(cudaFree(c));

  printf("completed\n");
  return 0;
}
