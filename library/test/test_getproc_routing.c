/*
 * cuGetProcAddress routing: does the pointer the caller ends up with belong to
 * us or to the driver?
 *
 * A caller that resolves through cuGetProcAddress never names the variant it
 * gets. The driver picks it from `cudaVersion` and `flags` -- cuCtxCreate can
 * come back as _v2 or _v4, and any stream API asked for with
 * CU_GET_PROC_ADDRESS_PER_THREAD_DEFAULT_STREAM comes back as its _ptsz twin.
 * loader.c names the returned pointer and substitutes the hook registered under
 * that same name, so the substitution carries the right ABI without guessing
 * from cudaVersion.
 *
 * The property that matters is observable without a GPU kernel: after
 * cuGetProcAddress returns, whose object does the pointer live in? dladdr
 * answers that. Instrumented means it points into libvgpu-control.so.
 *
 * The per-thread case is the one worth guarding. Matching on the requested name
 * alone cannot see the flags argument, so it would hand a per-thread caller the
 * legacy-stream hook -- same signature, so nothing crashes, it just silently
 * instruments the wrong stream semantics. Here we require the two flag settings
 * to produce two DIFFERENT pointers, both ours.
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so ./test_getproc_routing
 */
#define _GNU_SOURCE
#include <cuda.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "test_utils.h"  /* VGPU_TEST_RC_SKIP */

/* CUDA 12 renamed the 4-arg entry point; cuda.h maps the plain name onto the
 * 5-arg _v2 from 12.0 on. Wrap so the test body reads the same either way. */
static CUresult getproc(const char *sym, void **pfn, int ver, cuuint64_t flags) {
#if CUDA_VERSION >= 12000
  CUdriverProcAddressQueryResult st;
  return cuGetProcAddress(sym, pfn, ver, flags, &st);
#else
  return cuGetProcAddress(sym, pfn, ver, flags);
#endif
}

/* Name of the object a pointer lives in, or NULL if it cannot be attributed. */
static const char *owner_of(void *p) {
  static Dl_info info;
  if (!p || !dladdr(p, &info) || !info.dli_fname) return NULL;
  const char *slash = strrchr(info.dli_fname, '/');
  return slash ? slash + 1 : info.dli_fname;
}

static int is_ours(void *p) {
  const char *o = owner_of(p);
  return o && strstr(o, "libvgpu-control") != NULL;
}

int main(void) {
  int failures = 0;

  /* Every assertion here checks that a resolved pointer lives inside
   * libvgpu-control.so. Without the library injected there is nothing to route
   * into, so the checks would read as failures rather than what they are --
   * untestable. Skip instead. */
  const char *preload = getenv("LD_PRELOAD");
  if (preload == NULL || strstr(preload, "libvgpu-control") == NULL) {
    printf("SKIP (needs LD_PRELOAD=libvgpu-control.so)\n");
    return VGPU_TEST_RC_SKIP;
  }

  if (cuInit(0) != CUDA_SUCCESS) {
    printf("SKIP (no usable CUDA driver)\n");
    return VGPU_TEST_RC_SKIP;
  }
  int drv = 0;
  if (cuDriverGetVersion(&drv) != CUDA_SUCCESS) {
    printf("SKIP (cannot read driver version)\n");
    return VGPU_TEST_RC_SKIP;
  }
  printf("driver version: %d\n\n", drv);

  /* [A] A plain hooked symbol comes back instrumented. If this fails nothing
   * else in the file is meaningful -- routing is not reaching the caller. */
  printf("[A] cuMemAlloc resolves to our hook\n");
  void *p_alloc = NULL;
  if (getproc("cuMemAlloc", &p_alloc, drv, 0) != CUDA_SUCCESS) {
    printf("  FAIL: cuGetProcAddress(cuMemAlloc) failed\n");
    failures++;
  } else {
    printf("      -> %p in %s\n", p_alloc, owner_of(p_alloc));
    if (!is_ours(p_alloc)) {
      printf("  FAIL: cuMemAlloc was left pointing at the driver\n");
      failures++;
    }
  }

  /* [B] The per-thread variant must be instrumented too, and must NOT be the
   * same pointer as the legacy-stream one. Equal pointers would mean the
   * per-thread caller got the legacy hook. */
  printf("\n[B] cuLaunchKernel: legacy and per-thread resolve to distinct hooks\n");
  void *p_legacy = NULL, *p_pt = NULL;
  CUresult r1 = getproc("cuLaunchKernel", &p_legacy, drv, 0);
  CUresult r2 = getproc("cuLaunchKernel", &p_pt, drv,
                        CU_GET_PROC_ADDRESS_PER_THREAD_DEFAULT_STREAM);
  if (r1 != CUDA_SUCCESS || r2 != CUDA_SUCCESS) {
    printf("  FAIL: cuGetProcAddress(cuLaunchKernel) failed (%d / %d)\n",
           (int)r1, (int)r2);
    failures++;
  } else {
    printf("      legacy     -> %p in %s\n", p_legacy, owner_of(p_legacy));
    printf("      per-thread -> %p in %s\n", p_pt, owner_of(p_pt));
    if (!is_ours(p_legacy)) {
      printf("  FAIL: legacy-stream cuLaunchKernel left pointing at the driver\n");
      failures++;
    }
    if (!is_ours(p_pt)) {
      printf("  FAIL: per-thread cuLaunchKernel left pointing at the driver "
             "-- the _ptsz variant is not being instrumented\n");
      failures++;
    }
    if (p_legacy == p_pt) {
      printf("  FAIL: both flag settings produced the SAME pointer -- the "
             "per-thread caller is bound to the legacy-stream hook\n");
      failures++;
    }
  }

  /* [C] An ABI-conflict family must never come back as a hook whose ABI
   * disagrees with the version the driver picked. Either outcome is
   * acceptable -- our correctly-versioned wrapper, or the driver's own pointer
   * -- so this case only records what happened. What it would catch is a
   * resolution failure or a null pointer. */
  printf("\n[C] cuCtxCreate (ABI-conflict family) resolves to something usable\n");
  void *p_ctx = NULL;
  if (getproc("cuCtxCreate", &p_ctx, drv, 0) != CUDA_SUCCESS || !p_ctx) {
    printf("  FAIL: cuGetProcAddress(cuCtxCreate) gave no pointer\n");
    failures++;
  } else {
    printf("      -> %p in %s (%s)\n", p_ctx, owner_of(p_ctx),
           is_ours(p_ctx) ? "our versioned wrapper" : "driver pointer kept");
  }

  printf("\nResult: %s\n", failures ? "FAIL" : "PASS");
  return failures ? 1 : 0;
}
