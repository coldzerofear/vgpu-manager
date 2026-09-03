/*
 * Stands in for a library that defines a symbol and then uses RTLD_NEXT to
 * reach past its own copy -- the pattern LLVM OpenMP uses to look for an
 * OMPT tool. It has to live in a shared object loaded AFTER the interceptor,
 * because the bug being guarded against is the interceptor answering with
 * this object's own definition.
 */
#ifndef _GNU_SOURCE
#define _GNU_SOURCE
#endif
#include <dlfcn.h>

void vgpu_test_selfsym(void) {}

void *vgpu_test_own_selfsym(void) { return (void *)vgpu_test_selfsym; }

/* Correct answer: whatever defines vgpu_test_selfsym after THIS object --
 * nothing does, so NULL. Answering with our own definition means the caller
 * identity was lost.
 *
 * The barrier is load-bearing. Written as a plain `return dlsym(...)`, -O2
 * turns it into a tail call, the return address on the stack becomes our
 * caller's, and the lookup starts from the executable instead of from here
 * -- which finds this object and makes the test pass whatever the
 * interceptor does. Consuming the result first keeps the call a real one. */
void *vgpu_test_next_selfsym(void) {
  void *next = dlsym(RTLD_NEXT, "vgpu_test_selfsym");
  __asm__ __volatile__("" : : "r"(next) : "memory");
  return next;
}
