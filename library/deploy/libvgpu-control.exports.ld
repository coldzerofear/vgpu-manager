/*
 * Linker version script for libvgpu-control.so.
 *
 * Pinpoints the public ABI surface so that -fvisibility=hidden combined
 * with `local: *;` below shrinks the .dynsym to exactly what production
 * consumers need:
 *
 *   - LD_PRELOAD'd CUDA applications resolve cu* / nvml* hooks.
 *   - LD_PRELOAD's libdl.so dlsym walk resolves our `dlsym` interceptor.
 *   - The Vulkan loader's dlsym for vkNegotiateLoaderLayerInterfaceVersion
 *     finds the layer entry point.
 *
 * Anything not listed in `global:` ends up matched by `local: *;` and
 * stays out of .dynsym entirely. Hides ~80 internal helpers (formatUuid,
 * accumulate_used_memory, cleanup_vmem_nodes, etc.) that prior to this
 * script leaked into the dynamic symbol table and were available for
 * NVIDIA-ICD / loader-side global-symbol resolution to collide with —
 * the exact class of risk that drove HAMi-core PR #182's libvgpu_vk.so
 * split (a heavier remediation we deliberately do not need given this
 * tighter visibility discipline).
 *
 * Conventions:
 *   - cu[A-Z]*   matches every CUDA driver entry point we hook. The
 *                explicit [A-Z] enforces CUDA's PascalCase function naming
 *                (cuInit, cuMemAlloc, cuGetProcAddress, ...). NOT prefixed
 *                with underscore: _cu* helpers in cuda_originals.c (e.g.
 *                _cuCtxCreate, _cuMemAlloc) are intentional INTERNAL
 *                multi-ABI fallback trampolines and MUST stay hidden.
 *                The [A-Z] discriminator is what keeps internal globals
 *                whose names share the "cu" prefix but use snake_case —
 *                cuda_hooks_entry, cuda_library_entry, cuda_hook_nums —
 *                from being accidentally promoted to .dynsym by the
 *                broader "cu*" glob (regression caught via nm -D).
 *   - cudbg*     the CUDA debugger API family (cudbgApiInit, cudbgGetAPI,
 *                ...). NVIDIA names these with a lowercase "dbg" rather
 *                than PascalCase, so cu[A-Z]* would miss them. Listed
 *                explicitly because they ARE legitimate hooks our loader
 *                registers in cuda_library_entry[]; verified that no
 *                internal symbol uses a cudbg_ snake_case prefix.
 *   - nvml[A-Z]* mirror of cu[A-Z]*, for NVML PascalCase entry points
 *                (nvmlInit, nvmlDeviceGetMemoryInfo, ...). Same
 *                discriminator excludes nvml_hooks_entry /
 *                nvml_library_entry / nvml_hook_nums.
 *   - dlsym      the LD_PRELOAD dlsym(3) interceptor in loader.c. Required
 *                for the LD_PRELOAD CUDA-hook mechanism to work at all.
 *   - vkNegotiateLoaderLayerInterfaceVersion is the only Vulkan symbol
 *                that ever appears in our .dynsym. We deliberately do NOT
 *                expose v1-fallback names (vkGetInstanceProcAddr,
 *                vkGetDeviceProcAddr) — see commentary in src/vulkan/layer.c.
 *
 * Adding a new public symbol means adding it here AND ideally extending
 * hack/check_exported_symbols.sh's positive/negative assertions.
 *
 * Removing a name from this list will silently break LD_PRELOAD or
 * Vulkan-loader integration with no test failure unless the negative
 * assertions in hack/check_exported_symbols.sh are kept comprehensive.
 */
{
  global:
    cu[A-Z]*;
    cudbg*;
    nvml[A-Z]*;
    dlsym;
    vkNegotiateLoaderLayerInterfaceVersion;

  local:
    *;
};
