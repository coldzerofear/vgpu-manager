/*
 * Shared helpers for vgpu-manager library smoke tests.
 *
 * These tests are intended to run with LD_PRELOAD=libvgpu-control.so so the
 * allocation, kernel launch, and context creation paths exercise our hook
 * layer. Tests link directly against the real CUDA toolkit; vgpu-control is
 * injected at runtime.
 *
 * Ported (and trimmed) from HAMi-core/test/test_utils.h.
 */
#ifndef VGPU_TEST_UTILS_H
#define VGPU_TEST_UTILS_H

/* Prevent <nvml.h> from rewriting unversioned symbols to _v2 in-source; we
 * want our tests to call the unversioned names so vgpu-manager's wrappers
 * get a chance to forward them. */
#ifndef NVML_NO_UNVERSIONED_FUNC_DEFS
#define NVML_NO_UNVERSIONED_FUNC_DEFS
#endif

#include <nvml.h>
#include <pthread.h>
#include <stdio.h>
#include <stdint.h>

#ifndef TEST_DEVICE_ID
#define TEST_DEVICE_ID 0
#endif

#define CHECK_RUNTIME_API(f)                                                  \
  do {                                                                        \
    cudaError_t _status = (f);                                                \
    if (_status != cudaSuccess) {                                             \
      fprintf(stderr, "CUDA runtime error at %s:%d: %d\n",                    \
              __FILE__, __LINE__, _status);                                   \
      return _status;                                                         \
    }                                                                         \
  } while (0)

#define CHECK_DRV_API(f)                                                      \
  do {                                                                        \
    CUresult _status = (f);                                                   \
    if (_status != CUDA_SUCCESS) {                                            \
      fprintf(stderr, "CUDA driver api error at %s:%d: %d\n",                 \
              __FILE__, __LINE__, _status);                                   \
      return _status;                                                         \
    }                                                                         \
  } while (0)

#define CHECK_NVML_API(f)                                                     \
  do {                                                                        \
    nvmlReturn_t _status = (f);                                               \
    if (_status != NVML_SUCCESS) {                                            \
      fprintf(stderr, "NVML api error at line %d: %d\n", __LINE__, _status);  \
      return _status;                                                         \
    }                                                                         \
  } while (0)

#define CHECK_ALLOC_TEST(f)                                                   \
  do {                                                                        \
    fprintf(stderr, "Testing %s\n", #f);                                      \
    (f);                                                                      \
    fprintf(stderr, "Test %s succeed\n", #f);                                 \
  } while (0)

/*
 * CUDA 11/12 ship cuCtxCreate with a 3-arg (pctx, flags, dev) signature
 * (really an alias for cuCtxCreate_v2). CUDA 13 redefines the unversioned
 * name to cuCtxCreate_v4 with a 4-arg (pctx, CUctxCreateParams*, flags, dev)
 * signature. Centralise the branch here so every test file stays compact.
 */
#if CUDA_VERSION >= 13000
  #define CUCTX_CREATE(ctx_pp, flags, dev) cuCtxCreate((ctx_pp), NULL, (flags), (dev))
#else
  #define CUCTX_CREATE(ctx_pp, flags, dev) cuCtxCreate((ctx_pp), (flags), (dev))
#endif

/* NVML singleton for memory-usage observation. Lazily initialised on first
 * call to get_current_memory_usage() so tests need not worry about ordering
 * relative to cuInit(). */
static pthread_once_t __status_initialize_nvml = PTHREAD_ONCE_INIT;
static nvmlDevice_t __current_nvml_device;
static int __nvml_init_rc;

static void __nvml_init(void) {
  nvmlReturn_t r = nvmlInit();
  if (r != NVML_SUCCESS) { __nvml_init_rc = (int)r; return; }
  r = nvmlDeviceGetHandleByIndex(TEST_DEVICE_ID, &__current_nvml_device);
  __nvml_init_rc = (int)r;
}

static nvmlDevice_t get_nvml_device(void) {
  pthread_once(&__status_initialize_nvml, __nvml_init);
  return __current_nvml_device;
}

static nvmlReturn_t get_current_memory_usage(size_t *usage) {
  nvmlDevice_t dev = get_nvml_device();
  if (__nvml_init_rc != NVML_SUCCESS) return (nvmlReturn_t)__nvml_init_rc;
  nvmlMemory_t memory;
  CHECK_NVML_API(nvmlDeviceGetMemoryInfo(dev, &memory));
  int64_t diff = memory.used > (*usage)
      ? (int64_t)(memory.used - (*usage))
      : -(int64_t)((*usage) - memory.used);
  printf("Current usage: %llu bytes, diff: %lld bytes, total: %llu bytes\n",
         (unsigned long long)memory.used, (long long)diff,
         (unsigned long long)memory.total);
  *usage = memory.used;
  return NVML_SUCCESS;
}

#endif /* VGPU_TEST_UTILS_H */
