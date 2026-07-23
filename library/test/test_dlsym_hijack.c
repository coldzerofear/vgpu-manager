/*
 * dlsym hijack: version-drift warning.
 *
 * The dlsym hook (loader.c) warns exactly once when a caller resolves a
 * VERSIONED variant that this library does not hook but whose BASE name it does
 * -- the signature of a newer driver adding e.g. cuFoo_v3 that our
 * instrumentation then silently stops covering. This test drives that path
 * directly; it needs no GPU because every case resolves to "not one of our
 * hooks" and asserts on the warning, not on any device operation.
 *
 * The warning goes to stderr via LOGGER at WARNING level, which is the default
 * level, so it is visible without LOGGER_LEVEL being set. Each case captures
 * stderr around the dlsym call and inspects it.
 *
 * Only meaningful with the library preloaded (otherwise dlsym is libc's and no
 * hook runs); self-skips otherwise.
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

/* Run dlsym(RTLD_DEFAULT, symbol) with stderr captured; return whether the
 * captured text contains `needle`. The returned symbol value is irrelevant
 * here -- every symbol used below is deliberately not one of our hooks. */
static int dlsym_emits(const char *symbol, const char *needle) {
  FILE *cap = tmpfile();
  if (!cap) { perror("tmpfile"); return -1; }
  int capfd = fileno(cap);

  fflush(stderr);
  int saved = dup(STDERR_FILENO);
  dup2(capfd, STDERR_FILENO);

  void *p = dlsym(RTLD_DEFAULT, symbol);
  (void)p;

  fflush(stderr);
  dup2(saved, STDERR_FILENO);
  close(saved);

  /* Slurp the capture. */
  char buf[8192];
  rewind(cap);
  size_t n = fread(buf, 1, sizeof(buf) - 1, cap);
  buf[n] = '\0';
  fclose(cap);

  int hit = (strstr(buf, needle) != NULL);
  /* Echo the captured line(s) so a failure shows what actually happened. */
  if (n > 0) {
    char *line = strtok(buf, "\n");
    while (line) { printf("      stderr: %s\n", line); line = strtok(NULL, "\n"); }
  }
  return hit;
}

int main(void) {
  const char *preload = getenv("LD_PRELOAD");
  if (preload == NULL || strstr(preload, "libvgpu-control") == NULL) {
    printf("SKIP (needs LD_PRELOAD=libvgpu-control.so)\n");
    return 0;
  }

  int failures = 0;

  /* [A] Versioned variant, hooked base: must warn, and name both the variant
   * and its base. cuMemAlloc is hooked; _v9 does not exist, so this resolves to
   * not-a-hook and should trip the drift warning. */
  printf("[A] unhooked variant of a hooked base warns\n");
  if (dlsym_emits("cuMemAlloc_v9", "cuMemAlloc_v9") != 1) {
    printf("  FAIL: expected a warning naming cuMemAlloc_v9\n");
    failures++;
  }

  /* [B] Dedup: a second resolution of the same symbol must stay silent. */
  printf("[B] the same variant does not warn twice\n");
  if (dlsym_emits("cuMemAlloc_v9", "cuMemAlloc_v9") != 0) {
    printf("  FAIL: the drift warning repeated for the same symbol\n");
    failures++;
  }

  /* [C] Versioned variant whose base we do NOT hook: silent. We never
   * instrumented that family, so nothing has drifted. */
  printf("[C] unhooked variant of an unhooked base stays silent\n");
  if (dlsym_emits("cuNoSuchFamilyXYZ_v2", "cuNoSuchFamilyXYZ") != 0) {
    printf("  FAIL: warned about a family we never hooked\n");
    failures++;
  }

  /* [D] A plain base name (not a version variant): silent, even if unhooked. */
  printf("[D] a plain base symbol does not warn\n");
  if (dlsym_emits("cuNoSuchFamilyXYZ", "cuNoSuchFamilyXYZ") != 0) {
    printf("  FAIL: warned about a non-versioned symbol\n");
    failures++;
  }

  printf("\nResult: %s\n", failures ? "FAIL" : "PASS");
  return failures ? 1 : 0;
}
