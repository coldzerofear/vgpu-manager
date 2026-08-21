/*
 * dlsym(RTLD_NEXT, ...): repeated lookups, and driver symbols.
 *
 * Two properties of the RTLD_NEXT path are worth pinning:
 *
 *   - A non-driver symbol passes through, and resolving it twice from the
 *     same thread returns the same non-NULL pointer both times. An earlier
 *     implementation kept a per-thread history of resolved pointers and
 *     answered NULL on the second sighting, reading ordinary repetition as
 *     recursion.
 *
 *   - A driver symbol resolves to OUR hook, not to the driver. RTLD_NEXT
 *     normally asks to skip the wrapper, which would otherwise be a way
 *     around the vGPU limits, so the loader ignores it for cu.../nvml...
 *
 * No GPU needed: dladdr answers the ownership question without calling
 * anything.
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so ./test_dlsym_rtld_next
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <string.h>

#include "test_utils.h"  /* VGPU_REQUIRE_PRELOAD */

/* Is `p` a function inside libvgpu-control.so? */
static int owned_by_us(void *p) {
  Dl_info info;
  if (!dladdr(p, &info) || info.dli_fname == NULL) return 0;
  return strstr(info.dli_fname, "libvgpu-control") != NULL;
}

int main(void) {
  VGPU_REQUIRE_PRELOAD();

  int failures = 0;

  printf("[A] a non-driver symbol resolves through RTLD_NEXT\n");
  void *p1 = dlsym(RTLD_NEXT, "open");
  if (!p1) {
    printf("  FAIL: first call returned NULL\n");
    failures++;
  }

  printf("[B] resolving it again returns the same pointer, not NULL\n");
  void *p2 = dlsym(RTLD_NEXT, "open");
  if (!p2) {
    printf("  FAIL: second call returned NULL\n");
    failures++;
  } else if (p1 && p2 != p1) {
    printf("  FAIL: second call returned %p, first returned %p\n", p2, p1);
    failures++;
  }

  printf("[C] a driver symbol resolves to our hook, not the driver\n");
  void *hook = dlsym(RTLD_NEXT, "cuMemAlloc");
  if (!hook) {
    printf("  FAIL: cuMemAlloc did not resolve\n");
    failures++;
  } else if (!owned_by_us(hook)) {
    Dl_info info;
    printf("  FAIL: cuMemAlloc resolved outside our library (%s) -- the "
           "limits can be bypassed this way\n",
           dladdr(hook, &info) && info.dli_fname ? info.dli_fname : "?");
    failures++;
  }

  printf("[D] a non-driver symbol is NOT claimed by us\n");
  if (p1 && owned_by_us(p1)) {
    printf("  FAIL: \"open\" resolved into our library\n");
    failures++;
  }

  printf("\nResult: %s\n", failures ? "FAIL" : "PASS");
  return failures ? 1 : 0;
}
