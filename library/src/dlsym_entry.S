/*
Copyright 2025-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
 * The exported dlsym(3) entry point.
 *
 * Why assembly: glibc resolves RTLD_NEXT relative to whoever called dlsym,
 * and it identifies that caller by the return address on the stack. Calling
 * the real dlsym from a C function overwrites that with our own address, so
 * the lookup silently answers "what comes after libvgpu-control" instead of
 * "what comes after the caller". Only a tail jump -- transferring control
 * without pushing a new return address -- keeps the caller's identity.
 * C has no way to guarantee a tail call, hence these two stubs.
 *
 * There is no policy here. The stub calls vgpu_dlsym_target() (loader.c),
 * which decides everything, then either tail-jumps to what it returned or
 * falls through to vgpu_dlsym_dispatch(). Both callees are hidden, so both
 * calls bind directly with no PLT.
 *
 *   dlsym(handle, symbol)
 *     target = vgpu_dlsym_target(handle, symbol)
 *     if (target) jump target(handle, symbol)   <- caller preserved
 *     else        jump vgpu_dlsym_dispatch(handle, symbol)
 */

#if defined(__x86_64__)

    .text
    .globl  dlsym
    .type   dlsym, @function
    .align  16
dlsym:
    .cfi_startproc
    /* Save the arguments across the call. Entry rsp is 8 mod 16; the two
     * pushes plus the sub bring it to 0 mod 16, which the ABI requires at
     * a call boundary. */
    pushq   %rdi                      /* handle */
    .cfi_adjust_cfa_offset 8
    pushq   %rsi                      /* symbol */
    .cfi_adjust_cfa_offset 8
    subq    $8, %rsp
    .cfi_adjust_cfa_offset 8

    call    vgpu_dlsym_target         /* rdi/rsi already hold the arguments */

    addq    $8, %rsp
    .cfi_adjust_cfa_offset -8
    popq    %rsi
    .cfi_adjust_cfa_offset -8
    popq    %rdi
    .cfi_adjust_cfa_offset -8
    /* rsp is back to its entry value, so [rsp] is once again the caller's
     * return address and either jump below looks like a direct call. */

    testq   %rax, %rax
    jz      .Ldispatch_x86
    jmp     *%rax                     /* pass-through, caller preserved */
.Ldispatch_x86:
    jmp     vgpu_dlsym_dispatch
    .cfi_endproc
    .size   dlsym, .-dlsym

#elif defined(__aarch64__)

    .text
    .globl  dlsym
    .type   dlsym, %function
    .align  4
dlsym:
    .cfi_startproc
    /* 32 bytes keeps sp 16-byte aligned: 16 for the frame record, 16 for
     * the saved arguments. */
    stp     x29, x30, [sp, #-32]!
    .cfi_def_cfa_offset 32
    .cfi_offset 29, -32
    .cfi_offset 30, -24
    mov     x29, sp
    stp     x0, x1, [sp, #16]         /* handle, symbol */

    bl      vgpu_dlsym_target         /* x0/x1 already hold the arguments */

    mov     x2, x0                    /* keep the target out of x0 */
    ldp     x0, x1, [sp, #16]
    ldp     x29, x30, [sp], #32       /* x30 is the caller's return address */
    .cfi_def_cfa_offset 0
    .cfi_restore 29
    .cfi_restore 30

    cbz     x2, .Ldispatch_a64
    br      x2                        /* pass-through, caller preserved */
.Ldispatch_a64:
    b       vgpu_dlsym_dispatch
    .cfi_endproc
    .size   dlsym, .-dlsym

#else
#error "dlsym_entry.S supports x86_64 and aarch64 only. Forwarding RTLD_NEXT \
from C would corrupt the caller identity glibc derives from the return \
address, so there is no safe fallback -- add a stub for this architecture."
#endif

    /* Non-executable stack, same as every compiler-generated object here. */
    .section .note.GNU-stack,"",%progbits
