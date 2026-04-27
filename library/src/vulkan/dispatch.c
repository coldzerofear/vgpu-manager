/*
 * Per-VkInstance / per-VkDevice dispatch table cache (Phase 1 stub).
 *
 * Real implementation in Phase 2: hash table keyed by VkInstance /
 * VkDevice, populated at vkCreateInstance / vkCreateDevice time using
 * the next-layer pfn returned by the loader, queried by every other
 * hook to forward calls down the chain.
 *
 * See docs/how_to_develop_vulkan_implicit_layer.md.
 */
__attribute__((unused))
static const int __vgpu_vulkan_dispatch_stub = 0;
