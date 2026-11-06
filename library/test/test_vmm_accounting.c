/*
 * VMM memory (cuMemCreate) against the limit.
 *
 * The limit is checked against what NVML reports, so nothing has to be charged
 * or released along create / map / release / unmap as long as NVML follows the
 * driver's rules: memory exists from cuMemCreate until every handle reference
 * is released AND every mapping is gone. These cases hold the library to that:
 *
 *   [A] a created handle counts at once, before it is mapped: cuMemCreate
 *       drops its device lock on return, so a later allocator must see it
 *   [B] released while mapped: still counted until unmapped
 *   [C] unmapped first: still counted until released
 *   [D] a retained handle keeps it counted after the first release
 *   [E] one handle mapped twice: counted until the second unmap
 *   ([A] also covers: never mapped, gone at release)
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so CUDA_MEM_LIMIT=4096m ./test_vmm_accounting
 */
#include <cuda.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "test_utils.h"

#define MIB(x) ((double)(x) / (1024.0 * 1024.0))

static CUmemAllocationProp g_prop;
static size_t g_big;   /* two of these do not fit under the limit */

static int preloaded(void) {
  const char *p = getenv("LD_PRELOAD");
  return p != NULL && strstr(p, "libvgpu-control") != NULL;
}

static size_t headroom(void) {
  size_t freeb = 0, total = 0;
  if (cuMemGetInfo(&freeb, &total) != CUDA_SUCCESS) return 0;
  return freeb;
}

/* Would another g_big fit? cuMemAlloc-free probe: create and release. */
static int big_fits(void) {
  CUmemGenericAllocationHandle h;
  CUresult r = cuMemCreate(&h, g_big, &g_prop, 0);
  if (r == CUDA_SUCCESS) {
    cuMemRelease(h);
    return 1;
  }
  return 0;
}

#define EXPECT_FITS(want, what) do {                                          \
    int _f = big_fits();                                                      \
    printf("  %-44s %s (free view %.0f MiB)\n", what,                         \
           _f ? "a second one fits" : "a second one refused", MIB(headroom())); \
    if (_f != (want)) {                                                       \
      printf("  FAIL: expected %s\n", (want) ? "it to fit" : "it to be refused"); \
      failed = 1;                                                             \
    }                                                                         \
  } while (0)

static int created_counts_at_once(void) {
  int failed = 0;
  CUmemGenericAllocationHandle h;
  CHECK_DRV_API(cuMemCreate(&h, g_big, &g_prop, 0));
  EXPECT_FITS(0, "created, never mapped");
  CHECK_DRV_API(cuMemRelease(h));
  EXPECT_FITS(1, "released");
  return failed;
}

static int release_while_mapped(CUdeviceptr va) {
  int failed = 0;
  CUmemGenericAllocationHandle h;
  CHECK_DRV_API(cuMemCreate(&h, g_big, &g_prop, 0));
  CHECK_DRV_API(cuMemMap(va, g_big, 0, h, 0));
  CHECK_DRV_API(cuMemRelease(h));
  EXPECT_FITS(0, "released while mapped");
  CHECK_DRV_API(cuMemUnmap(va, g_big));
  EXPECT_FITS(1, "then unmapped");
  return failed;
}

static int unmap_then_release(CUdeviceptr va) {
  int failed = 0;
  CUmemGenericAllocationHandle h;
  CHECK_DRV_API(cuMemCreate(&h, g_big, &g_prop, 0));
  CHECK_DRV_API(cuMemMap(va, g_big, 0, h, 0));
  CHECK_DRV_API(cuMemUnmap(va, g_big));
  EXPECT_FITS(0, "unmapped, not released");
  CHECK_DRV_API(cuMemRelease(h));
  EXPECT_FITS(1, "then released");
  return failed;
}

static int retained_handle(CUdeviceptr va) {
  int failed = 0;
  CUmemGenericAllocationHandle h, h2;
  CHECK_DRV_API(cuMemCreate(&h, g_big, &g_prop, 0));
  CHECK_DRV_API(cuMemMap(va, g_big, 0, h, 0));
  CHECK_DRV_API(cuMemRetainAllocationHandle(&h2, (void *)va));
  CHECK_DRV_API(cuMemRelease(h));
  CHECK_DRV_API(cuMemUnmap(va, g_big));
  EXPECT_FITS(0, "unmapped, one reference left");
  CHECK_DRV_API(cuMemRelease(h2));
  EXPECT_FITS(1, "last reference released");
  return failed;
}

static int mapped_twice(CUdeviceptr va, CUdeviceptr va2) {
  int failed = 0;
  CUmemGenericAllocationHandle h;
  CHECK_DRV_API(cuMemCreate(&h, g_big, &g_prop, 0));
  CHECK_DRV_API(cuMemMap(va, g_big, 0, h, 0));
  CHECK_DRV_API(cuMemMap(va2, g_big, 0, h, 0));
  CHECK_DRV_API(cuMemRelease(h));
  CHECK_DRV_API(cuMemUnmap(va, g_big));
  EXPECT_FITS(0, "first mapping removed");
  CHECK_DRV_API(cuMemUnmap(va2, g_big));
  EXPECT_FITS(1, "second mapping removed");
  return failed;
}

int main(void) {
  if (!preloaded()) {
    printf("SKIP (needs LD_PRELOAD=libvgpu-control.so and a memory limit)\n");
    return VGPU_TEST_RC_SKIP;
  }
  CUdevice dev;
  CUcontext ctx;
  CHECK_DRV_API(cuInit(0));
  CHECK_DRV_API(cuDeviceGet(&dev, TEST_DEVICE_ID));
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, dev));

  memset(&g_prop, 0, sizeof(g_prop));
  g_prop.type = CU_MEM_ALLOCATION_TYPE_PINNED;
  g_prop.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  g_prop.location.id = dev;
  size_t gran = 0;
  CHECK_DRV_API(cuMemGetAllocationGranularity(&gran, &g_prop, CU_MEM_ALLOC_GRANULARITY_MINIMUM));
  /* Two thirds of the room: one fits, a second does not. */
  g_big = (headroom() / 3 * 2) / gran * gran;
  if (g_big == 0) {
    printf("SKIP (limit too small)\n");
    return VGPU_TEST_RC_SKIP;
  }
  printf("each allocation %.0f MiB, free view %.0f MiB\n", MIB(g_big), MIB(headroom()));
  CUdeviceptr va, va2;
  CHECK_DRV_API(cuMemAddressReserve(&va, g_big, 0, 0, 0));
  CHECK_DRV_API(cuMemAddressReserve(&va2, g_big, 0, 0, 0));

  int failures = 0;
#define RUN_CASE(title, call) do { printf(title "\n"); failures += (call); } while (0)
  RUN_CASE("[A] a created handle counts before it is mapped", created_counts_at_once());
  RUN_CASE("[B] released while mapped: counted until unmapped", release_while_mapped(va));
  RUN_CASE("[C] unmapped first: counted until released", unmap_then_release(va));
  RUN_CASE("[D] a retained handle keeps it counted", retained_handle(va));
  RUN_CASE("[E] mapped twice: counted until the second unmap", mapped_twice(va, va2));
#undef RUN_CASE

  /* [F] is [A]'s second half: never mapped, gone at release. */
  cuMemAddressFree(va, g_big);
  cuMemAddressFree(va2, g_big);
  cuCtxDestroy(ctx);
  return vgpu_test_verdict(failures, 0);
}
