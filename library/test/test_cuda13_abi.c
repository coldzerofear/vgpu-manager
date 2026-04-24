/*
 * CUDA 13 ABI-conflict wrapper smoke test.
 *
 * Exercises the versioned ELF symbols that vgpu-manager exports for the
 * "unversioned name binds to different ABI across CUDA majors" families.
 * Every call below targets a versioned name directly (e.g. cuCtxCreate_v2
 * vs cuCtxCreate_v4), so the test is stable regardless of which CUDA SDK
 * the test binary was compiled against - CUDA 13 headers macro-redirect
 * the unversioned identifier to `_v4` / `_v2`, which would change the
 * parameter list if we wrote `cuCtxCreate(...)` directly.
 *
 * What is NOT validated here: actual GPU semantics. These are smoke tests
 * for the hook dispatch path - we accept CUDA_SUCCESS or CUDA_ERROR_NOT_*
 * but never a SIGSEGV from a NULL fn_ptr or an ABI mismatch crash.
 */
#include <cuda.h>
#include <cuda_runtime.h>
#include <stdio.h>

#include "test_utils.h"

static int expect_ok_or(CUresult got, const char *name) {
  /* Any well-defined CUresult is acceptable - we only fail on segfault
   * (which this line never reaches) or on unexpected status codes that
   * indicate a completely broken dispatch. */
  if (got == CUDA_SUCCESS
      || got == CUDA_ERROR_NOT_SUPPORTED
      || got == CUDA_ERROR_NOT_FOUND
      || got == CUDA_ERROR_INVALID_VALUE) {
    printf("  %-42s -> %d (ok)\n", name, got);
    return 0;
  }
  fprintf(stderr, "  %-42s -> %d (UNEXPECTED)\n", name, got);
  return 1;
}

static int test_ctx_create_variants(CUdevice device) {
  int fails = 0;
  printf("[ctx create]\n");

  {
    CUcontext ctx = NULL;
    CUresult r = cuCtxCreate_v2(&ctx, 0, device);
    fails += expect_ok_or(r, "cuCtxCreate_v2");
    if (ctx) cuCtxDestroy(ctx);
  }

#if CUDA_VERSION >= 12050
  {
    CUcontext ctx = NULL;
    CUctxCreateParams params = {0};
    CUresult r = cuCtxCreate_v4(&ctx, &params, 0, device);
    fails += expect_ok_or(r, "cuCtxCreate_v4");
    if (ctx) cuCtxDestroy(ctx);
  }
#endif
  return fails;
}

static int test_device_get_uuid_variants(CUdevice device) {
  int fails = 0;
  printf("[device uuid]\n");

  /* cuDeviceGetUuid and _v2 have identical signatures - safe to call either
   * name directly. Call the versioned names explicitly to avoid the CUDA 13
   * header macro that maps the unversioned identifier to _v2. */
#if CUDA_VERSION >= 11040
  CUuuid u2 = {0};
  CUresult r = cuDeviceGetUuid_v2(&u2, device);
  fails += expect_ok_or(r, "cuDeviceGetUuid_v2");
#else
  CUuuid u = {0};
  CUresult r = cuDeviceGetUuid(&u, device);
  fails += expect_ok_or(r, "cuDeviceGetUuid");
#endif
  return fails;
}

static int test_mem_advise_variants(void) {
  int fails = 0;
  printf("[mem advise]\n");
  CUdeviceptr dptr = 0;
  CUresult r = cuMemAllocManaged(&dptr, 4096, CU_MEM_ATTACH_GLOBAL);
  if (r != CUDA_SUCCESS) {
    fprintf(stderr, "  (skip) cuMemAllocManaged failed: %d\n", r);
    return 0;
  }

#if CUDA_VERSION >= 12020
  /* CUDA 13 ABI: (ptr, count, advice, CUmemLocation). Call the versioned
   * name explicitly - on CUDA 12 headers this resolves to the _v2 symbol
   * directly; on CUDA 13 headers the unversioned macro would alias it. */
  CUmemLocation loc = {0};
  loc.type = CU_MEM_LOCATION_TYPE_DEVICE;
  loc.id = 0;
  r = cuMemAdvise_v2(dptr, 4096, CU_MEM_ADVISE_SET_PREFERRED_LOCATION, loc);
  fails += expect_ok_or(r, "cuMemAdvise_v2");
#endif
  cuMemFree(dptr);
  return fails;
}

static int test_mem_prefetch_variants(void) {
  int fails = 0;
  printf("[mem prefetch]\n");
  CUdeviceptr dptr = 0;
  CUresult r = cuMemAllocManaged(&dptr, 4096, CU_MEM_ATTACH_GLOBAL);
  if (r != CUDA_SUCCESS) {
    fprintf(stderr, "  (skip) cuMemAllocManaged failed: %d\n", r);
    return 0;
  }
#if CUDA_VERSION >= 12020
  CUmemLocation loc = {0};
  loc.type = CU_MEM_LOCATION_TYPE_DEVICE;
  loc.id = 0;
  r = cuMemPrefetchAsync_v2(dptr, 4096, loc, 0, 0);
  fails += expect_ok_or(r, "cuMemPrefetchAsync_v2");
#endif
  cuMemFree(dptr);
  return fails;
}

static int test_graph_v2_variants(void) {
  int fails = 0;
  printf("[graph _v2 family]\n");

  CUgraph g;
  CUresult r = cuGraphCreate(&g, 0);
  if (r != CUDA_SUCCESS) {
    fprintf(stderr, "  (skip) cuGraphCreate failed: %d\n", r);
    return 0;
  }

#if CUDA_VERSION >= 12030
  size_t n = 0;
  r = cuGraphGetEdges_v2(g, NULL, NULL, NULL, &n);
  fails += expect_ok_or(r, "cuGraphGetEdges_v2");
#endif
  cuGraphDestroy(g);
  return fails;
}

int main(void) {
  CHECK_DRV_API(cuInit(0));

  CUdevice device;
  CHECK_DRV_API(cuDeviceGet(&device, TEST_DEVICE_ID));

  CUcontext ctx;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, device));

  int fails = 0;
  fails += test_ctx_create_variants(device);
  fails += test_device_get_uuid_variants(device);
  fails += test_mem_advise_variants();
  fails += test_mem_prefetch_variants();
  fails += test_graph_v2_variants();

  cuCtxDestroy(ctx);
  if (fails > 0) {
    fprintf(stderr, "[FAIL] %d ABI call(s) returned an unexpected error\n", fails);
    return 1;
  }
  printf("[PASS] all ABI-conflict wrappers dispatched cleanly\n");
  return 0;
}
