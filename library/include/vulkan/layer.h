/*
 * vgpu-manager Vulkan implicit layer - public ELF entry point.
 *
 * Only one symbol is exported from libvgpu-control.so for the Vulkan
 * loader: vkNegotiateLoaderLayerInterfaceVersion. Everything else
 * (vk_layer_CreateInstance, vk_layer_GetInstanceProcAddr, ...) is
 * file-static in layer.c, reachable only through the function pointers
 * we hand back via the negotiate struct or via our GetProcAddr returns.
 *
 * Why one symbol only: this .so is also LD_PRELOADed for CUDA hooking.
 * Exporting a function named e.g. vkAllocateMemory at default visibility
 * would let the dynamic linker resolve unrelated references in the
 * application or other .sos to our function, bypassing the Vulkan loader
 * entirely. Keeping every layer entry point static eliminates that risk.
 */
#ifndef VGPU_VULKAN_LAYER_H
#define VGPU_VULKAN_LAYER_H

#include <vulkan/vulkan.h>
#include <vulkan/vk_layer.h>

/* Newer Vulkan-Headers releases (>= ~1.3) dropped VK_LAYER_EXPORT from
 * vk_layer.h. Provide a portable local fallback so the same source builds
 * across distros. We default to visibility("default") on every compiler
 * that supports it; this only matters when callers compile us with
 * -fvisibility=hidden (we do not today, but layer code should be robust). */
#ifndef VK_LAYER_EXPORT
#if defined(__GNUC__) && __GNUC__ >= 4
#define VK_LAYER_EXPORT __attribute__((visibility("default")))
#else
#define VK_LAYER_EXPORT
#endif
#endif

#ifdef __cplusplus
extern "C" {
#endif

VK_LAYER_EXPORT VkResult VKAPI_CALL
vkNegotiateLoaderLayerInterfaceVersion(VkNegotiateLayerInterface *pVersionStruct);

#ifdef __cplusplus
}
#endif

#endif /* VGPU_VULKAN_LAYER_H */
