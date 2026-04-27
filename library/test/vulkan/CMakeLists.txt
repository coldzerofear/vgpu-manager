# Vulkan layer unit tests — no GPU / no CUDA / no NVML required.
#
# Each test is a standalone executable that compiles the relevant
# src/vulkan/*.c files directly and links stubs.c for the external
# symbols normally provided by cuda_hook.c / loader.c / lock.c.
#
# Gated entirely on BUILD_VULKAN_LAYER=ON AND BUILD_TESTS=ON. Notably
# does NOT require CUDA toolkit (unlike the sibling test/CMakeLists.txt
# which compiles CUDA-driver smoke tests). This lets devs without a
# local CUDA install run the Vulkan unit tests in CI / devcontainers.

set(VK_TEST_SRC_DIR ${CMAKE_CURRENT_SOURCE_DIR})
set(VK_TEST_BIN_DIR ${CMAKE_CURRENT_BINARY_DIR})

# Common include paths every Vulkan test needs:
#   - ${CMAKE_SOURCE_DIR}/library/         for "include/X.h" / "include/vulkan/X.h"
#   - ${Vulkan_INCLUDE_DIRS}               for <vulkan/vulkan.h>
#   - ${VK_TEST_SRC_DIR}                   for "test_helpers.h"
set(VK_TEST_COMMON_INCLUDES
    ${CMAKE_SOURCE_DIR}
    ${Vulkan_INCLUDE_DIRS}
    ${VK_TEST_SRC_DIR})

# Source files of the layer that tests link in. Each test picks the
# subset it needs.
set(VK_LAYER_DISPATCH_SRC      ${CMAKE_SOURCE_DIR}/src/vulkan/dispatch.c)
set(VK_LAYER_PHYSDEV_SRC       ${CMAKE_SOURCE_DIR}/src/vulkan/physdev_index.c)
set(VK_LAYER_QUEUE_SRC         ${CMAKE_SOURCE_DIR}/src/vulkan/queue_index.c)
set(VK_LAYER_HOOKS_MEMORY_SRC  ${CMAKE_SOURCE_DIR}/src/vulkan/hooks_memory.c)
set(VK_LAYER_HOOKS_ALLOC_SRC   ${CMAKE_SOURCE_DIR}/src/vulkan/hooks_alloc.c)
set(VK_LAYER_HOOKS_SUBMIT_SRC  ${CMAKE_SOURCE_DIR}/src/vulkan/hooks_submit.c)

# Stubs file shared by every binary.
set(VK_TEST_STUBS ${VK_TEST_SRC_DIR}/stubs.c)

# Helper to declare one test target with the given extra layer sources.
# Usage: vgpu_vk_add_test(<name> [<src1> [<src2> ...]])
function(vgpu_vk_add_test name)
    set(layer_srcs ${ARGN})
    add_executable(${name}
        ${VK_TEST_SRC_DIR}/${name}.c
        ${VK_TEST_STUBS}
        ${layer_srcs})
    target_include_directories(${name} PRIVATE ${VK_TEST_COMMON_INCLUDES})
    target_link_libraries(${name} PRIVATE pthread dl)
    # Stubs.c expects to override symbols normally exported with
    # default visibility from cuda_hook.c. To avoid -fvisibility=hidden
    # interactions we keep the test binaries at default visibility.
    set_target_properties(${name} PROPERTIES C_VISIBILITY_PRESET default)
endfunction()

# ---- Test targets ---------------------------------------------------

# 1. Negotiate protocol: dlopen the built libvgpu-control.so.
add_executable(test_layer_init ${VK_TEST_SRC_DIR}/test_layer_init.c)
target_include_directories(test_layer_init PRIVATE ${VK_TEST_COMMON_INCLUDES})
target_link_libraries(test_layer_init PRIVATE dl)

# 2. Dispatch register / lookup / remove.
vgpu_vk_add_test(test_dispatch
    ${VK_LAYER_DISPATCH_SRC})

# 3. Queue index.
vgpu_vk_add_test(test_queue_index
    ${VK_LAYER_QUEUE_SRC})

# 4. Physdev index (UUID resolution).
vgpu_vk_add_test(test_physdev_index
    ${VK_LAYER_PHYSDEV_SRC})

# 5. Memory-properties heap clamp (oversold branches).
vgpu_vk_add_test(test_memprops_clamp
    ${VK_LAYER_DISPATCH_SRC}
    ${VK_LAYER_PHYSDEV_SRC}
    ${VK_LAYER_HOOKS_MEMORY_SRC})

# 6. AllocateMemory budget enforcement (excluding import detection).
vgpu_vk_add_test(test_alloc_budget
    ${VK_LAYER_DISPATCH_SRC}
    ${VK_LAYER_PHYSDEV_SRC}
    ${VK_LAYER_HOOKS_ALLOC_SRC})

# 7. AllocateMemory import-path detection (Phase 5b).
vgpu_vk_add_test(test_alloc_import
    ${VK_LAYER_DISPATCH_SRC}
    ${VK_LAYER_PHYSDEV_SRC}
    ${VK_LAYER_HOOKS_ALLOC_SRC})

# 8. QueueSubmit / GetDeviceQueue throttle + register.
vgpu_vk_add_test(test_submit_throttle
    ${VK_LAYER_DISPATCH_SRC}
    ${VK_LAYER_PHYSDEV_SRC}
    ${VK_LAYER_QUEUE_SRC}
    ${VK_LAYER_HOOKS_SUBMIT_SRC})

# ---- Stage runner script into the test build dir -------------------

add_custom_target(vgpu_vk_copy_runner ALL
    COMMAND ${CMAKE_COMMAND} -E copy
            ${VK_TEST_SRC_DIR}/run_unit_tests.sh
            ${VK_TEST_BIN_DIR}/run_unit_tests.sh
    COMMENT "[vgpu] stage Vulkan-test runner into build dir"
    VERBATIM)

message(STATUS "[vgpu] Vulkan unit tests ENABLED (8 binaries)")
