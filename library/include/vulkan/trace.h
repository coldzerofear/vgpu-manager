/*
 * Vulkan-path debug tracing, gated independently from the main LOGGER.
 *
 * Why a separate gate: the LOGGER framework in include/hook.h is shared
 * by CUDA and NVML hooks; turning it up to VERBOSE / DETAIL globally
 * floods stderr with every CUDA call in a CUDA+Vulkan workload. When
 * triaging a Vulkan-only issue we want fine-grained traces just for
 * the Vulkan path, with zero overhead when off.
 *
 * Design mirrors HAMi-core PR #182's HAMI_VK_TRACE so production
 * triage on either project's .so uses the same mental model:
 *   VGPU_VK_TRACE=1 in pod env  ==>  traces emitted on stderr
 *   anything else / unset       ==>  zero overhead (single int compare)
 *
 * Cost when off: vgpu_vk_trace_enabled() resolves to a cached
 * static-int compare on the hot path; the getenv() call only runs once
 * per TU per process. Cost when on: one fprintf+fflush per call site
 * — acceptable for triage, never enabled in production steady state.
 *
 * Cost when never reached: -O2 inlines vgpu_vk_trace_enabled to a
 * single int compare, and the fprintf branch is folded behind the
 * cold path; -DNDEBUG does not affect this (we don't use assert here).
 *
 * Each translation unit including this header gets its own static
 * cache. That is intentional — no cross-TU reachability, no shared
 * state, no link-time surface. The cached values converge across TUs
 * after their first VGPU_VK_TRACE call each; the race window is
 * benign (the env-var value is process-wide immutable).
 */
#ifndef VGPU_VULKAN_TRACE_H
#define VGPU_VULKAN_TRACE_H

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Cached env-var lookup. -1 sentinel for "uninitialized"; 0 / 1 after
 * first call. The strncmp for "1" specifically (not just non-empty)
 * mirrors HAMi PR #182's contract: VGPU_VK_TRACE=0 / =false / unset
 * all behave identically to "off". */
static inline int vgpu_vk_trace_enabled(void) {
  static int cached = -1;
  if (cached < 0) {
    const char *e = getenv("VGPU_VK_TRACE");
    cached = (e != NULL && e[0] == '1' && e[1] == '\0') ? 1 : 0;
  }
  return cached;
}

/* Trace macro. The do/while(0) is the standard wrapper that lets the
 * macro behave like a single statement under all C syntactic
 * positions (inside `if/else` without braces, in expression-statement
 * lists, etc.). Append "\n" inside the format so callers do not
 * have to remember it. */
#define VGPU_VK_TRACE(fmt, ...)                                       \
  do {                                                                \
    if (vgpu_vk_trace_enabled()) {                                    \
      fprintf(stderr, "VGPU_VK_TRACE: " fmt "\n", ##__VA_ARGS__);     \
      fflush(stderr);                                                 \
    }                                                                 \
  } while (0)

#ifdef __cplusplus
}
#endif

#endif /* VGPU_VULKAN_TRACE_H */
