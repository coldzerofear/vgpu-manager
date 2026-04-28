/*
 * cudaMallocHost smoke test.
 * Ported from HAMi-core/test/test_runtime_alloc_host.c.
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <stdio.h>

#include "test_utils.h"

static size_t usage = 0;

static int test_one(size_t bytes) {
  void *hptr;
  CHECK_RUNTIME_API(cudaMallocHost(&hptr, bytes));
  CHECK_NVML_API(get_current_memory_usage(&usage));
  CHECK_RUNTIME_API(cudaFreeHost(hptr));
  CHECK_NVML_API(get_current_memory_usage(&usage));
  return 0;
}

int main(void) {
  CHECK_RUNTIME_API(cudaSetDevice(TEST_DEVICE_ID));
  CHECK_NVML_API(get_current_memory_usage(&usage));

  size_t arr[84] = {0};
  for (int k = 0; k < 28; ++k) {
    arr[3 * k]     = (size_t)2 << k;
    arr[3 * k + 1] = ((size_t)2 << k) + 1;
    arr[3 * k + 2] = ((size_t)2 << k) - 1;
  }
  for (int k = 0; k < 84; ++k) {
    if (test_one(arr[k]) != 0) {
      fprintf(stderr, "Test alloc %zu bytes failed\n", arr[k]);
      return -1;
    }
  }
  CHECK_NVML_API(nvmlShutdown());
  return 0;
}
