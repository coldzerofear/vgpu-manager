/*
 * Async memory-pool allocation smoke test.
 *
 * Mirrors HAMi-core/test/test_alloc_pool_async.c, added in HAMi-core commit
 * 9ce21cc ("Track cuMemAllocFromPoolAsync allocations and free untracked async
 * pointers", PR #217) as a reproducer for issue #93.
 *
 * The two defects that commit fixed, and where vgpu-manager stands on each:
 *
 *   (1) cuMemAllocFromPoolAsync was a bare pass-through, so a pool-explicit
 *       allocation never counted against the vGPU memory limit. We already hook
 *       it through prepare_memory_allocation() (cuda_hook.c), so it is checked
 *       and accounted like every other allocation. Case [C] locks that in.
 *
 *   (2) cuMemFreeAsync() -> remove_chunk_async() walked an allocated-chunk list
 *       and, for a pointer that list never saw, returned -1 without performing
 *       the real free: the -1 surfaced to the caller as an "unrecognized error
 *       code" and the device allocation leaked. vgpu-manager keeps no such
 *       list -- cuMemFreeAsync() always forwards to the driver first and only
 *       then reconciles its virtual-memory bookkeeping, where an unknown
 *       pointer is a silent no-op (loader.c, free_gpu_virt_memory). We are
 *       structurally immune, and cases [A]/[B] keep it that way.
 *
 * Cases [D] and [E] cover a defect of the same family that is ours alone: the
 * oversold UVA fallback hands the caller a cuMemAllocManaged pointer that the
 * driver's cuMemFreeAsync refuses. [D] pins the ordinary release path; [E] pins
 * the mid-capture one, where the release cannot be performed at all and must be
 * reported rather than corrupt the graph capture.
 *
 * Cases [A]-[C] pass with or without LD_PRELOAD, so they double as a baseline
 * check against stock CUDA; [D] and [E] assert vgpu-manager behaviour and
 * self-skip when the library is not preloaded.
 *
 * Run:
 *   ./test_alloc_pool_async
 *   LD_PRELOAD=<build>/libvgpu-control.so CUDA_MEM_LIMIT=2048m ./test_alloc_pool_async
 */
#include <cuda.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "test_utils.h"

#define ALLOC_BYTES  (32ull * 1024 * 1024)  /* 32 MiB */
#define OVER_LIMIT   (1024ull * 1024 * 1024) /* how far past `free` case [C] asks */

/* Print a CUresult, flagging codes the driver itself does not recognise --
 * exactly what a caller sees when a hook leaks a bare -1 into a CUresult. */
static void describe(const char *label, CUresult res) {
  const char *name = NULL;
  CUresult q = cuGetErrorName(res, &name);
  if (q != CUDA_SUCCESS || name == NULL) {
    printf("  %-38s -> %d  <unrecognized error code>\n", label, (int)res);
  } else {
    printf("  %-38s -> %d  (%s)\n", label, (int)res, name);
  }
}

static CUresult create_device_pool(CUmemoryPool *pool, CUdevice dev) {
  CUmemPoolProps props;
  memset(&props, 0, sizeof(props));
  props.allocType = CU_MEM_ALLOCATION_TYPE_PINNED;
  props.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  props.location.id = (int)dev;
  return cuMemPoolCreate(pool, &props);
}

/* [A] Control: default-pool async path. */
static int default_pool_async_test(CUstream stream) {
  CUdeviceptr dptr = 0;
  CHECK_DRV_API(cuMemAllocAsync(&dptr, ALLOC_BYTES, stream));
  CUresult f = cuMemFreeAsync(dptr, stream);
  describe("cuMemFreeAsync (default pool)", f);
  CHECK_DRV_API(cuStreamSynchronize(stream));
  return f == CUDA_SUCCESS ? 0 : 1;
}

/* [B] The HAMi #93 shape: allocate from an explicit pool, free it async.
 * A hook that tracks allocations in a private list must either register this
 * pointer or fall through to the real free; either way the caller must see
 * CUDA_SUCCESS and the memory must actually be released. */
static int explicit_pool_async_test(CUdevice dev, CUstream stream) {
  CUmemoryPool pool;
  CHECK_DRV_API(create_device_pool(&pool, dev));

  CUdeviceptr dptr = 0;
  CHECK_DRV_API(cuMemAllocFromPoolAsync(&dptr, ALLOC_BYTES, pool, stream));
  CUresult f = cuMemFreeAsync(dptr, stream);
  describe("cuMemFreeAsync (explicit pool)", f);

  /* Best-effort teardown: a rejected free leaves the allocation live, and
   * destroying a pool with outstanding allocations is itself an error. */
  cuStreamSynchronize(stream);
  cuMemPoolDestroy(pool);
  return f == CUDA_SUCCESS ? 0 : 1;
}

/* [C] The pool path must be accounted against the vGPU memory limit.
 *
 * Ask for more than cuMemGetInfo() says is free and require a rejection. Under
 * LD_PRELOAD `free` is the limit-relative figure, so this exercises our OOM
 * check without ever committing the memory; unhooked, the driver rejects it on
 * its own. A pool allocation that bypassed the limiter -- HAMi defect (1) --
 * would be served straight from physical memory and return CUDA_SUCCESS here. */
static int explicit_pool_limit_test(CUdevice dev, CUstream stream) {
  size_t mem_free = 0, mem_total = 0;
  CHECK_DRV_API(cuMemGetInfo(&mem_free, &mem_total));
  printf("  reported free=%zu total=%zu\n", mem_free, mem_total);

  CUmemoryPool pool;
  CHECK_DRV_API(create_device_pool(&pool, dev));

  CUdeviceptr dptr = 0;
  CUresult a = cuMemAllocFromPoolAsync(&dptr, mem_free + OVER_LIMIT, pool, stream);
  describe("cuMemAllocFromPoolAsync (over limit)", a);

  int failed = 0;
  if (a == CUDA_SUCCESS) {
    printf("  allocation beyond the reported limit was accepted\n");
    failed = 1;
    cuMemFreeAsync(dptr, stream);
  } else if (a != CUDA_ERROR_OUT_OF_MEMORY) {
    printf("  expected CUDA_ERROR_OUT_OF_MEMORY (%d)\n", (int)CUDA_ERROR_OUT_OF_MEMORY);
    failed = 1;
  }

  cuStreamSynchronize(stream);
  cuMemPoolDestroy(pool);
  return failed;
}

/* [D] Async-freeing a pointer that came from the oversold UVA fallback.
 *
 * Under memory oversubscription cuMemAllocAsync silently serves the request
 * with cuMemAllocManaged (cuda_hook.c, ALLOCATED_TO_UVA). The caller has no way
 * to know, so it frees with cuMemFreeAsync -- which the driver rejects for a
 * managed pointer with CUDA_ERROR_NOT_SUPPORTED, leaking both the allocation and
 * our vmem accounting (the hook only reconciles the accounting on success).
 *
 * Reaching that fallback needs an oversold config plus a request larger than the
 * physical slice, which a test cannot size portably. cuMemAllocManaged with
 * CU_MEM_ATTACH_GLOBAL registers the pointer in exactly the same virtual-memory
 * list, so it is a faithful stand-in for the pointer the fallback hands out.
 *
 * vgpu-manager specific: unhooked, cuMemFreeAsync on a managed pointer is simply
 * invalid, so this case only asserts under LD_PRELOAD. */
static int uva_pointer_async_free_test(CUstream stream) {
  const char *preload = getenv("LD_PRELOAD");
  if (preload == NULL || strstr(preload, "libvgpu-control") == NULL) {
    printf("  SKIP (needs LD_PRELOAD=libvgpu-control.so)\n");
    return 0;
  }

  CUdeviceptr dptr = 0;
  CHECK_DRV_API(cuMemAllocManaged(&dptr, ALLOC_BYTES, CU_MEM_ATTACH_GLOBAL));
  CUresult f = cuMemFreeAsync(dptr, stream);
  describe("cuMemFreeAsync (managed/UVA pointer)", f);
  if (f != CUDA_SUCCESS) {
    printf("  the UVA fallback's pointer cannot be released by the API the\n"
           "  caller was given; allocation and vmem accounting both leak\n");
    return 1;
  }
  CHECK_DRV_API(cuStreamSynchronize(stream));
  return 0;
}

/* [E] The same free, but issued while the stream is capturing a CUDA graph.
 *
 * Releasing a UVA pointer needs the non-stream-ordered cuMemFree, which in turn
 * has to drain the stream -- and draining a capturing stream invalidates the
 * capture. So the hook must bail out before touching the stream. The pointer is
 * allocated before capture begins, which is the only way one can exist: the
 * alloc path declines to hand out UVA pointers mid-capture.
 *
 * Both assertions below are needed, and the second is the one with teeth. A hook
 * that drains the stream anyway *also* ends up returning 900, because that is
 * what the driver's own cuStreamSynchronize reports for a capturing stream -- so
 * checking the status alone cannot tell the two apart. What separates them is
 * that the driver has by then invalidated the capture, which surfaces as
 * CUDA_ERROR_STREAM_CAPTURE_INVALIDATED from cuStreamEndCapture.
 *
 * Note the allocation stays live either way: refusing the free is not releasing
 * it. This case pins the capture's survival and the accuracy of the status, not
 * the absence of a leak. */
static int uva_pointer_free_during_capture_test(CUstream stream) {
  const char *preload = getenv("LD_PRELOAD");
  if (preload == NULL || strstr(preload, "libvgpu-control") == NULL) {
    printf("  SKIP (needs LD_PRELOAD=libvgpu-control.so)\n");
    return 0;
  }

  CUdeviceptr dptr = 0;
  CHECK_DRV_API(cuMemAllocManaged(&dptr, ALLOC_BYTES, CU_MEM_ATTACH_GLOBAL));
  CHECK_DRV_API(cuStreamBeginCapture(stream, CU_STREAM_CAPTURE_MODE_GLOBAL));

  CUresult f = cuMemFreeAsync(dptr, stream);
  describe("cuMemFreeAsync (UVA, mid-capture)", f);

  /* Tear the capture down either way. If cuMemFreeAsync drained the stream, the
   * capture is already invalidated: cuStreamEndCapture then reports
   * CUDA_ERROR_STREAM_CAPTURE_INVALIDATED and yields no graph -- exactly the
   * outcome the early bail-out exists to avoid. */
  CUgraph graph = NULL;
  CUresult e = cuStreamEndCapture(stream, &graph);
  describe("cuStreamEndCapture", e);
  if (graph != NULL) cuGraphDestroy(graph);

  int failed = 0;
  if (f != CUDA_ERROR_STREAM_CAPTURE_UNSUPPORTED) {
    printf("  expected CUDA_ERROR_STREAM_CAPTURE_UNSUPPORTED (%d)\n",
           (int)CUDA_ERROR_STREAM_CAPTURE_UNSUPPORTED);
    failed = 1;
  }
  if (e != CUDA_SUCCESS) {
    printf("  the capture did not survive the rejected free\n");
    failed = 1;
  }

  cuStreamSynchronize(stream);
  cuMemFree(dptr); /* refused above, so it is still live */
  return failed;
}

int main(void) {
  CHECK_DRV_API(cuInit(0));
  CUdevice device;
  CHECK_DRV_API(cuDeviceGet(&device, TEST_DEVICE_ID));
  CUcontext ctx;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, device));
  CUstream stream;
  CHECK_DRV_API(cuStreamCreate(&stream, CU_STREAM_NON_BLOCKING));

  int failures = 0;

  printf("[A] cuMemAllocAsync + cuMemFreeAsync (default pool)\n");
  failures += default_pool_async_test(stream);

  printf("[B] cuMemAllocFromPoolAsync + cuMemFreeAsync (explicit pool)\n");
  failures += explicit_pool_async_test(device, stream);

  printf("[C] cuMemAllocFromPoolAsync honours the memory limit\n");
  failures += explicit_pool_limit_test(device, stream);

  printf("[D] cuMemFreeAsync accepts an oversold UVA pointer\n");
  failures += uva_pointer_async_free_test(stream);

  printf("[E] cuMemFreeAsync refuses a UVA pointer mid-capture\n");
  failures += uva_pointer_free_during_capture_test(stream);

  printf("\nResult: %s\n", failures ? "FAIL" : "PASS");

  cuStreamDestroy(stream);
  CHECK_DRV_API(cuCtxDestroy(ctx));
  return failures ? 1 : 0;
}
