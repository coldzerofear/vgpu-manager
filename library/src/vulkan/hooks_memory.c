/*
 * vkGetPhysicalDeviceMemoryProperties / _2 clamp hooks (Phase 1 stub).
 *
 * Real implementation in Phase 4: clamp device-local heap.size to
 * g_vgpu_config[host_index].real_memory regardless of memory_oversold
 * setting. Vulkan never has access to oversold UVA capacity, so reporting
 * total_memory there would mislead applications into asking for memory
 * the driver cannot deliver.
 *
 * See docs/how_to_develop_vulkan_implicit_layer.md.
 */
__attribute__((unused))
static const int __vgpu_vulkan_hooks_memory_stub = 0;
