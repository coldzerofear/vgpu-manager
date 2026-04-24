/*
 * cuMemHostRegister smoke test.
 * Ported from HAMi-core/test/test_host_register.c.
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <stdio.h>
#include <stdlib.h>

#include "test_utils.h"

static size_t usage = 0;

static int test_one(size_t bytes) {
  void *hptr = malloc(bytes);
  if (!hptr) return -1;
  CHECK_DRV_API(cuMemHostRegister(hptr, bytes, CU_MEMHOSTALLOC_DEVICEMAP));
  CHECK_NVML_API(get_current_memory_usage(&usage));
  CHECK_DRV_API(cuMemHostUnregister(hptr));
  CHECK_NVML_API(get_current_memory_usage(&usage));
  free(hptr);
  return 0;
}

int main(void) {
  CHECK_DRV_API(cuInit(0));
  CUdevice device;
  CHECK_DRV_API(cuDeviceGet(&device, TEST_DEVICE_ID));
  CUcontext ctx;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, device));
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
  CHECK_DRV_API(cuCtxDestroy(ctx));
  return 0;
}
