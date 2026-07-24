/*
 * dlsym hijack: the unhooked-symbol trail.
 *
 * The dlsym hook (loader.c) records, once per symbol and at VERBOSE level, every
 * cu.../nvml... symbol that passed through it uninstrumented. That trail is what
 * makes a driver growing a variant we do not intercept -- cuFoo_v3 and the like
 * -- visible after the fact instead of silent.
 *
 * Two properties are worth pinning: the note appears at all, and it appears only
 * once no matter how often the symbol is resolved. The second is what keeps a
 * VERBOSE run readable, and it is the one a naive implementation gets wrong.
 *
 * No GPU is needed: every symbol used here resolves to "not one of our hooks",
 * and the assertions are about the log, not about any device operation.
 *
 * The level must be active before the library caches it, so the test
 * re-executes itself once with LOGGER_LEVEL set rather than hoping nothing has
 * logged yet.
 *
 * Run:
 *   LD_PRELOAD=<build>/libvgpu-control.so ./test_dlsym_hijack
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* Resolve `symbol` with stderr captured; return 1 if the capture contains the
 * unhooked-symbol note for it. The resolved value is irrelevant.
 *
 * Matching the full note text rather than the bare symbol name matters: the
 * library mentions a symbol's name in several unrelated lines -- "loading
 * cuMemAlloc:12" while it populates the driver table, "search found cuda hook
 * cuMemAlloc" on the hit path -- and a substring search for the name alone
 * reports those as if the symbol had been recorded as unhooked. */
static int dlsym_notes(const char *symbol) {
  char needle[256];
  snprintf(needle, sizeof(needle), "unhooked driver symbol '%s'", symbol);

  FILE *cap = tmpfile();
  if (!cap) { perror("tmpfile"); return -1; }

  fflush(stderr);
  int saved = dup(STDERR_FILENO);
  dup2(fileno(cap), STDERR_FILENO);

  void *p = dlsym(RTLD_DEFAULT, symbol);
  (void)p;

  fflush(stderr);
  dup2(saved, STDERR_FILENO);
  close(saved);

  /* Generous, because the first resolution in the process also drags the whole
   * driver-table load through this capture. */
  static char buf[1 << 20];
  rewind(cap);
  size_t n = fread(buf, 1, sizeof(buf) - 1, cap);
  buf[n] = '\0';
  fclose(cap);

  return strstr(buf, needle) != NULL;
}

int main(int argc, char **argv) {
  (void)argc;
  const char *preload = getenv("LD_PRELOAD");
  if (preload == NULL || strstr(preload, "libvgpu-control") == NULL) {
    printf("SKIP (needs LD_PRELOAD=libvgpu-control.so)\n");
    return 0;
  }

  /* The library caches its log level on first use, so the level has to be in
   * the environment from process start. Re-exec once to guarantee that.
   *
   * VERBOSE, not DETAIL: the note sits at VERBOSE precisely so it is readable,
   * and DETAIL additionally turns on a per-symbol line for every entry in the
   * driver table as it loads -- hundreds of lines that bury what we came to
   * look at. */
  if (getenv("LOGGER_LEVEL") == NULL) {
    setenv("LOGGER_LEVEL", "4", 1);   /* VERBOSE */
    execv("/proc/self/exe", argv);
    perror("execv");                  /* only reached on failure */
    return 1;
  }

  int failures = 0;

  /* [A] An unhooked symbol leaves a note. cuMemAlloc_v9 does not exist in any
   * driver, which keeps the case independent of the CUDA version installed. */
  printf("[A] an unhooked symbol is recorded\n");
  if (dlsym_notes("cuMemAlloc_v9") != 1) {
    printf("  FAIL: expected a DETAIL note naming cuMemAlloc_v9\n");
    failures++;
  }

  /* [B] The same symbol stays quiet afterwards. Without dedup a DETAIL run
   * would repeat a line for every resolution of every unhooked symbol. */
  printf("[B] the same symbol is not recorded twice\n");
  if (dlsym_notes("cuMemAlloc_v9") != 0) {
    printf("  FAIL: the note repeated for a symbol already recorded\n");
    failures++;
  }

  /* [C] A different unhooked symbol still gets its own note -- dedup must be
   * per symbol, not a one-shot latch. */
  printf("[C] a different unhooked symbol gets its own note\n");
  if (dlsym_notes("cuNoSuchFamilyXYZ_v2") != 1) {
    printf("  FAIL: expected a note naming cuNoSuchFamilyXYZ_v2\n");
    failures++;
  }

  /* [D] A symbol we DO hook is resolved by the hook path and never reaches the
   * recorder, so it must not appear in the trail. */
  printf("[D] a hooked symbol leaves no note\n");
  if (dlsym_notes("cuMemAlloc") != 0) {
    printf("  FAIL: a hooked symbol was recorded as unhooked\n");
    failures++;
  }

  printf("\nResult: %s\n", failures ? "FAIL" : "PASS");
  return failures ? 1 : 0;
}
