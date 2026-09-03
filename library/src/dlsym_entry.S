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

/* Branch Target Identification. Our dlsym is reached through the PLT, i.e.
 * by an indirect branch, so on a BTI-enforcing kernel it needs a landing pad
 * at entry. `hint #34` is `bti c` spelled so that assemblers predating BTI
 * still accept it, and it decodes as a NOP on pre-ARMv8.5 parts.
 *
 * The property note below is what actually turns enforcement on, and the
 * linker only keeps it if every input object carries it -- so it is emitted
 * exactly when the compiler says the C objects have it too. Claiming it
 * unconditionally would be a lie on a build without -mbranch-protection;
 * omitting it on a build with one would silently disable BTI for the whole
 * library. */
#if defined(__ARM_FEATURE_BTI_DEFAULT) && __ARM_FEATURE_BTI_DEFAULT == 1
    .section .note.gnu.property, "a"
    .balign 8
    .long 4                      /* name size            */
    .long 0x10                   /* descriptor size      */
    .long 5                      /* NT_GNU_PROPERTY_TYPE_0 */
    .asciz "GNU"
    .long 0xc0000000             /* GNU_PROPERTY_AARCH64_FEATURE_1_AND */
    .long 4
    .long 1                      /* ..._BTI */
    .long 0                      /* padding */
#endif

    .text
    .globl  dlsym
    .type   dlsym, %function
    .align  4
dlsym:
    .cfi_startproc
    hint    #34                       /* bti c */
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

#elif defined(VGPU_ALLOW_C_DLSYM_FALLBACK)

/* Nothing to emit: loader.c defines dlsym in C for this architecture. See
 * the note there for what that costs. */

#else
#error "dlsym_entry.S has entry stubs for x86_64 and aarch64 only, and the \
CUDA driver ships for those two. Forwarding RTLD_NEXT from C cannot preserve \
the caller identity glibc derives from the return address, so building \
elsewhere is a deliberate choice: configure with \
-DVGPU_ALLOW_C_DLSYM_FALLBACK to take the degraded C path, or add a stub."
#endif

    /* Non-executable stack, same as every compiler-generated object here. */
    .section .note.GNU-stack,"",%progbits
