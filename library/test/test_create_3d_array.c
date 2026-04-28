/*
 * cuArray3DCreate smoke test - 3D CUDA array allocation.
 * Ported from HAMi-core/test/test_create_3d_array.c.
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <stdio.h>

#include "test_utils.h"

int main(void) {
  CHECK_DRV_API(cuInit(0));
  CUdevice device;
  CHECK_DRV_API(cuDeviceGet(&device, TEST_DEVICE_ID));
  CUcontext ctx;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, device));

  size_t usage = 0;
  CHECK_NVML_API(get_current_memory_usage(&usage));

  CUarray handle;
  CUDA_ARRAY3D_DESCRIPTOR desc = {2048, 2048, 0, CU_AD_FORMAT_UNSIGNED_INT32, 2, 0};
  CHECK_DRV_API(cuArray3DCreate(&handle, &desc));
  CHECK_NVML_API(get_current_memory_usage(&usage));
  CHECK_DRV_API(cuArrayDestroy(handle));
  CHECK_NVML_API(get_current_memory_usage(&usage));

  CHECK_NVML_API(nvmlShutdown());
  CHECK_DRV_API(cuCtxDestroy(ctx));
  return 0;
}
