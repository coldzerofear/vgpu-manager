/*
 * vkAllocateMemory / vkFreeMemory hooks (Phase 1 stub).
 *
 * Real implementation in Phase 5 (+ Phase 5b for VK_KHR_external_memory
 * import detection). Will call prepare_memory_allocation with kind =
 * VGPU_BUDGET_KIND_PHYSICAL and allow_uva = 0 - Vulkan has no UVA
 * equivalent so the cap is real_memory and the UVA fallback is forbidden.
 *
 * See docs/how_to_develop_vulkan_implicit_layer.md.
 */
__attribute__((unused))
static const int __vgpu_vulkan_hooks_alloc_stub = 0;
