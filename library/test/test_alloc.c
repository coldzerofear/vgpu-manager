/*
 * cuMemAlloc / cuMemFree trim and oversubscription smoke test.
 * Exercises vgpu-manager's memory-limit path via the driver API.
 *
 * Ported from HAMi-core/test/test_alloc.c.
 */
#include <assert.h>
#include <cuda.h>
#include <cuda_runtime.h>
#include <stdio.h>

#include "test_utils.h"

static size_t usage = 0;

static int test_one(size_t bytes) {
  CUdeviceptr dptr;
  CHECK_DRV_API(cuMemAlloc(&dptr, bytes));
  CHECK_NVML_API(get_current_memory_usage(&usage));
  CHECK_DRV_API(cuMemFree(dptr));
  CHECK_NVML_API(get_current_memory_usage(&usage));
  return 0;
}

static int alloc_trim_test(void) {
  size_t arr[90] = {0};
  for (int k = 0; k < 30; ++k) {
    arr[3 * k]     = (size_t)2 << k;
    arr[3 * k + 1] = ((size_t)2 << k) + 1;
    arr[3 * k + 2] = ((size_t)2 << k) - 1;
  }
  for (int k = 0; k < 90; ++k) {
    printf("k=%d, arr[k]=%zu\n", k, arr[k]);
    assert(test_one(arr[k]) == 0);
  }
  return 0;
}

static int alloc_oversize_test(void) {
  size_t tmp_size = ((size_t)2 << 29) + 2;
  printf("size=%zu\n", tmp_size);
  CUdeviceptr dp[10];
  for (int k = 0; k < 10; k++) {
    cuMemAlloc(&dp[k], tmp_size);
  }
  CHECK_NVML_API(get_current_memory_usage(&usage));
  for (int k = 0; k < 10; k += 2) cuMemFree(dp[k]);
  CHECK_NVML_API(get_current_memory_usage(&usage));
  for (int k = 1; k < 10; k += 2) cuMemFree(dp[k]);
  CHECK_NVML_API(get_current_memory_usage(&usage));
  return 0;
}

int main(void) {
  CHECK_DRV_API(cuInit(0));
  CUdevice device;
  CHECK_DRV_API(cuDeviceGet(&device, TEST_DEVICE_ID));
  CUcontext ctx;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, device));
  CHECK_NVML_API(get_current_memory_usage(&usage));

  CHECK_ALLOC_TEST(alloc_trim_test());
  CHECK_ALLOC_TEST(alloc_oversize_test());

  CHECK_NVML_API(nvmlShutdown());
  CHECK_DRV_API(cuCtxDestroy(ctx));
  return 0;
}
