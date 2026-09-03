/*
 * The two callees src/dlsym_entry.S dispatches to, built into the test
 * interceptor. They mirror loader.c and share its symbol predicates from
 * hook.h, so the classification under test is the production one; only the
 * driver-hook lookup is stubbed, since that needs the real library.
 */
#ifndef _GNU_SOURCE
#define _GNU_SOURCE
#endif
#include <dlfcn.h>

#include "include/hook.h"

typedef void *(*fp)(void *, const char *);
static fp real_dlsym;

/* Exported so dladdr can attribute it to this object -- stands in for a
 * driver hook. */
void cuTestHook(void) {}

static void resolve_real_dlsym(void) {
  if (real_dlsym != NULL) return;
  const char *versions[] = {"GLIBC_2.34", "GLIBC_2.22", "GLIBC_2.17",
                            "GLIBC_2.2.5", NULL};
  for (int i = 0; versions[i] != NULL; i++) {
    real_dlsym = (fp)dlvsym(RTLD_NEXT, "dlsym", versions[i]);
    if (real_dlsym) return;
  }
}

FUNC_ATTR_HIDDEN void *vgpu_dlsym_target(void *handle, const char *symbol) {
  if (handle != RTLD_NEXT || symbol == NULL) return NULL;
  if (symbol_is_cuda_api(symbol) || symbol_is_nvml_api(symbol)) return NULL;
  resolve_real_dlsym();
  return (void *)real_dlsym;
}

FUNC_ATTR_HIDDEN void *vgpu_dlsym_dispatch(void *handle, const char *symbol) {
  resolve_real_dlsym();
  if (symbol != NULL && (symbol_is_cuda_api(symbol) || symbol_is_nvml_api(symbol))) {
    return (void *)cuTestHook;
  }
  return real_dlsym ? real_dlsym(handle, symbol) : NULL;
}
