/*
 * Vulkan implicit layer entry point (Phase 1 stub).
 *
 * Real implementation lands in Phase 2:
 *   - vkNegotiateLoaderLayerInterfaceVersion
 *   - vk_layer_CreateInstance / vk_layer_DestroyInstance
 *   - vk_layer_CreateDevice   / vk_layer_DestroyDevice
 *   - vk_layer_GetInstanceProcAddr / vk_layer_GetDeviceProcAddr
 *
 * See docs/how_to_develop_vulkan_implicit_layer.md.
 *
 * The unused-attribute static const lets this translation unit produce
 * a valid object file under -Wall -Wshadow without "empty translation
 * unit" complaints from stricter toolchains.
 */
__attribute__((unused))
static const int __vgpu_vulkan_layer_stub = 0;
