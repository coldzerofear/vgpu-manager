/*
 * cuMemCreate / cuMemRelease concurrent smoke test.
 *
 * Exercises the virtual-memory-management path with CUmemAllocationProp,
 * which embeds CUmemLocation. The CUmemLocation struct is one of the
 * ABI-critical types audited by hack/check_struct_layout.py - if its
 * layout diverges from the host CUDA toolkit, this test is the quickest
 * way to hit it at runtime.
 *
 * Ported from HAMi-core/test/test_mem_create.c.
 */
#include <cuda.h>
#include <pthread.h>
#include <stdio.h>

#include "test_utils.h"

#define NUM_THREADS 4
#define ALLOC_SIZE  (64 * 1024 * 1024)

static CUdevice g_device;

static void *thread_func(void *arg) {
  int tid = *(int *)arg;

  CUmemAllocationProp prop = {0};
  prop.type = CU_MEM_ALLOCATION_TYPE_PINNED;
  prop.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  prop.location.id = g_device;

  size_t granularity = 0;
  CUresult res = cuMemGetAllocationGranularity(&granularity, &prop,
                                               CU_MEM_ALLOC_GRANULARITY_MINIMUM);
  if (res != CUDA_SUCCESS) {
    fprintf(stderr, "thread %d: cuMemGetAllocationGranularity failed: %d\n", tid, res);
    return NULL;
  }
  size_t size = ((ALLOC_SIZE + granularity - 1) / granularity) * granularity;

  CUmemGenericAllocationHandle handle;
  res = cuMemCreate(&handle, size, &prop, 0);
  printf("thread %d: cuMemCreate returned %d (size=%zu)\n", tid, res, size);
  if (res == CUDA_SUCCESS) {
    cuMemRelease(handle);
    return (void *)1;
  }
  return NULL;
}

int main(void) {
  CHECK_DRV_API(cuInit(0));
  CHECK_DRV_API(cuDeviceGet(&g_device, TEST_DEVICE_ID));
  CUcontext ctx;
  CHECK_DRV_API(CUCTX_CREATE(&ctx, 0, g_device));

  pthread_t threads[NUM_THREADS];
  int tids[NUM_THREADS];
  for (int i = 0; i < NUM_THREADS; i++) {
    tids[i] = i;
    pthread_create(&threads[i], NULL, thread_func, &tids[i]);
  }

  int ok = 0;
  for (int i = 0; i < NUM_THREADS; i++) {
    void *ret = NULL;
    pthread_join(threads[i], &ret);
    if (ret != NULL) ok++;
  }
  printf("%d/%d threads succeeded\n", ok, NUM_THREADS);
  if (ok != NUM_THREADS) {
    fprintf(stderr, "expected all threads to succeed\n");
    return 1;
  }
  CHECK_DRV_API(cuCtxDestroy(ctx));
  return 0;
}
