/*
 * src/dlsym_entry.S: does the exported dlsym stub keep its caller's identity?
 *
 * glibc resolves RTLD_NEXT relative to whoever called dlsym, which it takes
 * from the return address. An interceptor that forwards through a normal
 * call replaces that with its own address, and a library asking to skip its
 * own definition of a symbol gets that definition handed back -- LLVM
 * OpenMP's OMPT probe then calls itself until the process wedges. The stub
 * tail-jumps instead, so the return address stays the caller's.
 *
 * Layout matters: the interceptor is preloaded and libdlsym_caller.so is a
 * link-time dependency, so the caller loads after it. That is the order in
 * which a forwarding interceptor answers with the caller's own symbol.
 *
 * Self-re-execs once to install LD_PRELOAD. No GPU, no CUDA.
 */
#ifndef _GNU_SOURCE
#define _GNU_SOURCE
#endif
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

void *vgpu_test_own_selfsym(void);
void *vgpu_test_next_selfsym(void);

static int in_object(void *addr, const char *soname) {
  Dl_info info;
  if (!addr || !dladdr(addr, &info) || info.dli_fname == NULL) return 0;
  return strstr(info.dli_fname, soname) != NULL;
}

int main(int argc, char **argv) {
  (void)argc;
  if (getenv("VGPU_DLSYM_ENTRY_PRELOADED") == NULL) {
    setenv("LD_PRELOAD", VGPU_DLSYM_SHIM_PATH, 1);
    setenv("VGPU_DLSYM_ENTRY_PRELOADED", "1", 1);
    execv("/proc/self/exe", argv);
    perror("execv");
    return 1;
  }

  int failures = 0;

  /* The regression itself. */
  printf("[A] RTLD_NEXT keeps the caller's identity\n");
  void *own = vgpu_test_own_selfsym();
  void *next = vgpu_test_next_selfsym();
  if (next == own) {
    printf("  FAIL: the caller got its own definition back -- a library that\n"
           "        uses RTLD_NEXT to skip itself would call itself forever\n");
    failures++;
  }

  /* Driver symbols must not take the tail-jump path, or the vGPU limits
   * could be stepped around with one RTLD_NEXT lookup. */
  printf("[B] RTLD_NEXT still resolves driver symbols to our hook\n");
  if (!in_object(dlsym(RTLD_NEXT, "cuMemAlloc"), "dlsym_entry_shim")) {
    printf("  FAIL: cuMemAlloc escaped the interceptor\n");
    failures++;
  }

  printf("[C] cudbg* and nvml* are treated as driver symbols too\n");
  if (!in_object(dlsym(RTLD_NEXT, "cudbgApiInit"), "dlsym_entry_shim") ||
      !in_object(dlsym(RTLD_NEXT, "nvmlInit"), "dlsym_entry_shim")) {
    printf("  FAIL: a driver symbol family escaped the interceptor\n");
    failures++;
  }

  /* Non-RTLD_NEXT lookups never reach the stub's tail-jump branch. */
  printf("[D] RTLD_DEFAULT and explicit handles still work\n");
  void *strlen_p = dlsym(RTLD_DEFAULT, "strlen");
  void *libm = dlopen("libm.so.6", RTLD_LAZY);
  void *cos_p = libm ? dlsym(libm, "cos") : NULL;
  if (!strlen_p || in_object(strlen_p, "dlsym_entry_shim") || !cos_p) {
    printf("  FAIL: strlen=%p cos=%p\n", strlen_p, cos_p);
    failures++;
  }

  printf("[E] a repeated RTLD_NEXT lookup is stable, not NULL\n");
  void *first = dlsym(RTLD_NEXT, "open");
  void *second = dlsym(RTLD_NEXT, "open");
  if (!first || first != second) {
    printf("  FAIL: first=%p second=%p\n", first, second);
    failures++;
  }

  printf("[F] an unknown symbol still resolves to NULL\n");
  if (dlsym(RTLD_DEFAULT, "vgpu_no_such_symbol_at_all") != NULL) {
    printf("  FAIL: a missing symbol resolved to something\n");
    failures++;
  }

  printf("\nResult: %s\n", failures ? "FAIL" : "PASS");
  return failures ? 1 : 0;
}
