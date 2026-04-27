/*
 * VkPhysicalDevice -> host_index mapping (Phase 1 stub).
 *
 * Real implementation in Phase 3: cache keyed by VkPhysicalDevice; on
 * first lookup query VkPhysicalDeviceIDProperties::deviceUUID via the
 * dispatch table and dispatch to get_host_device_index_by_uuid_bytes
 * (added to library/include/budget.h in Phase 0).
 *
 * See docs/how_to_develop_vulkan_implicit_layer.md.
 */
__attribute__((unused))
static const int __vgpu_vulkan_physdev_index_stub = 0;
