/*
 * cuMemAllocManaged smoke test - exercises vgpu-manager's UVA fallback path.
 *
 * Ported from HAMi-core/test/test_alloc_managed.c.
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <stdio.h>

#include "test_utils.h"

static size_t usage = 0;

static int alloc_managed(CUdeviceptr *dptr, size_t bytes) {
  CHECK_DRV_API(cuMemAllocManaged(dptr, bytes, CU_MEM_ATTACH_GLOBAL));
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
  CUdeviceptr dptr[84];
  for (int k = 0; k < 84; ++k) {
    if (alloc_managed(&dptr[k], arr[k]) != 0) {
      fprintf(stderr, "Test alloc %zu bytes failed\n", arr[k]);
      return -1;
    }
  }
  for (int k = 0; k < 84; ++k) cuMemFree(dptr[k]);

  CHECK_NVML_API(nvmlShutdown());
  CHECK_DRV_API(cuCtxDestroy(ctx));
  return 0;
}
