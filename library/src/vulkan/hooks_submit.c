/*
 * vkQueueSubmit / vkQueueSubmit2 / vkQueueSubmit2KHR hooks (Phase 1 stub).
 *
 * Real implementation in Phase 6: feed each submit through rate_limiter
 * for SM-utilisation throttling, mirroring the cuLaunchKernel path.
 * Initial submit-cost estimate is 1 unit per submit; refine later if
 * benchmark data justifies it.
 *
 * See docs/how_to_develop_vulkan_implicit_layer.md.
 */
__attribute__((unused))
static const int __vgpu_vulkan_hooks_submit_stub = 0;
