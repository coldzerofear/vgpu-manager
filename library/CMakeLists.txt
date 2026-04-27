cmake_minimum_required(VERSION 3.5)
project(vgpu C CXX)

add_definitions(-D_GNU_SOURCE)
#add_compile_options(-Wall -Wshadow -Werror -Wno-format)
add_compile_options(-Wall -Wshadow -Werror=format-truncation=0)

include_directories(${CMAKE_SOURCE_DIR})

if (${ENABLE_DEBUG})
    add_compile_options(-g -O0)
else ()
    add_compile_options(-g -O2)
endif ()

if (NOT DEFINED ENV{CUDA_HOME})
    set(CUDA_HOME /usr/local/cuda)
else()
    set(CUDA_HOME $ENV{CUDA_HOME})
endif()


set(STATIC_C_LIBRARIES -static-libgcc -static-libstdc++ -pthread)

if (${USE_ORIGINAL})
    add_definitions(-DUSE_ORIGINAL)
endif ()

# controller related
add_library(vgpu-control SHARED
    include/hook.h
    include/list.h
    include/cuda-subset.h
    include/nvml-subset.h
    include/cuda-helper.h
    include/nvml-helper.h
    include/metrics.h
    include/vdpau/vdpau.h
    src/cuda_originals.c
    src/nvml_originals.c
    src/cuda_hook.c
    src/nvml_hook.c
    src/loader.c
    src/register.c
    src/util.c
    src/lock.c
    src/metrics.c)
target_link_libraries(vgpu-control ${STATIC_C_LIBRARIES})
target_compile_options(vgpu-control PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

# ---------------------------------------------------------------------------
# Build-time integrity checks for the CUDA hook layer.
#
# These scripts are intentionally split out of C code into Python/Bash so
# they can scan the entire dispatch table / enum / wrapper set without
# hand-maintaining a parallel list inside cuda-helper.h. See hack/*.
#
# VGPU_RUN_HOOK_CHECKS=OFF disables all three (useful for offline builds).
# ---------------------------------------------------------------------------
option(VGPU_RUN_HOOK_CHECKS "Run hack/check_*.* during build" ON)

if(VGPU_RUN_HOOK_CHECKS)
    find_package(Python3 COMPONENTS Interpreter)

    if(Python3_Interpreter_FOUND)
        # Pre-build: static consistency between dispatch table, enum order,
        # wrappers, and ABI-conflict family coverage. Always runs; failure
        # stops the build.
        add_custom_target(vgpu_check_hook_consistency ALL
            COMMAND ${Python3_EXECUTABLE}
                    ${CMAKE_CURRENT_SOURCE_DIR}/hack/check_cuda_hook_consistency.py
            WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}
            COMMENT "[vgpu] check CUDA hook registration consistency"
            VERBATIM)
        add_dependencies(vgpu-control vgpu_check_hook_consistency)

        # Pre-build: struct layout validation against the real <cuda.h>.
        # The script itself exits 0 with [SKIP] if no cuda.h is present, so
        # this never blocks builds in environments without a CUDA toolkit.
        add_custom_target(vgpu_check_struct_layout ALL
            COMMAND ${Python3_EXECUTABLE}
                    ${CMAKE_CURRENT_SOURCE_DIR}/hack/check_struct_layout.py
                    --cuda-home ${CUDA_HOME}
            WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}
            COMMENT "[vgpu] check cuda-subset.h struct layout vs real <cuda.h>"
            VERBATIM)
        add_dependencies(vgpu-control vgpu_check_struct_layout)
    else()
        message(WARNING "Python3 not found - skipping hack/check_*.py hook checks")
    endif()

    # Post-build: verify the linked .so exports BOTH ABI versions of every
    # conflict family. Only requires nm + bash.
    add_custom_command(TARGET vgpu-control POST_BUILD
        COMMAND bash
                ${CMAKE_CURRENT_SOURCE_DIR}/hack/check_exported_symbols.sh
                $<TARGET_FILE:vgpu-control>
        COMMENT "[vgpu] check ABI-conflict symbol exports in libvgpu-control.so"
        VERBATIM)
endif()

# ---------------------------------------------------------------------------
# Optional: compile the smoke tests in test/.
# Disabled by default to keep the main build light. Enable with:
#   cmake -DBUILD_TESTS=ON ..
# The test/ subdir self-skips if no CUDA toolkit is locatable.
# ---------------------------------------------------------------------------
option(BUILD_TESTS "Build smoke tests under test/" OFF)
if(BUILD_TESTS)
    add_subdirectory(test)
endif()

# ---------------------------------------------------------------------------
# Optional: Vulkan implicit layer module.
#
# When enabled, src/vulkan/*.c compiles as a CMake OBJECT library and links
# into libvgpu-control.so. The .so then doubles as a Vulkan implicit layer
# alongside its CUDA hook role - one .so, two roles, single mount point.
#
# CUDA-only consumers see no behaviour change either way:
#   - When OFF: .so does not export any vk* symbol; build does not
#     require Vulkan-Headers.
#   - When ON:  the Vulkan layer code is linked into the .so. Loading
#     of the layer at runtime is gated by the manifest's
#     enable_environment (VGPU_VULKAN_ENABLE=1), so even when ON the
#     CUDA-only Pods that don't set the env are not affected.
#
# Default:
#   - The CMake option below defaults to OFF for direct cmake invokers.
#   - The build.sh wrapper sets BUILD_VULKAN_LAYER=ON by default; set
#     the env var BUILD_VULKAN_LAYER=OFF on the wrapper to override.
#   - The standard release Docker image is built via build.sh, so the
#     shipped artifact has the layer compiled in.
#
# Requirements when enabled:
#   - Vulkan-Headers (e.g. apt install libvulkan-dev)
#   - CMake >= 3.7 for the bundled FindVulkan module
#
# To switch a build tree from OFF to ON or vice versa, prefer a fresh
# CMake configure step; reusing the same build/ directory with a
# different option value is supported but a clean is safer.
# ---------------------------------------------------------------------------
option(BUILD_VULKAN_LAYER "Build the Vulkan implicit layer module" OFF)
if(BUILD_VULKAN_LAYER)
    if(CMAKE_VERSION VERSION_LESS 3.7)
        message(FATAL_ERROR
            "BUILD_VULKAN_LAYER=ON requires CMake >= 3.7 for FindVulkan; "
            "current version is ${CMAKE_VERSION}. Either upgrade CMake or "
            "leave BUILD_VULKAN_LAYER off.")
    endif()
    find_package(Vulkan REQUIRED)
    message(STATUS "[vgpu] Vulkan implicit layer ENABLED "
                   "(headers: ${Vulkan_INCLUDE_DIRS})")
    add_library(vgpu_vulkan_mod OBJECT
        src/vulkan/layer.c
        src/vulkan/dispatch.c
        src/vulkan/physdev_index.c
        src/vulkan/queue_index.c
        src/vulkan/hooks_alloc.c
        src/vulkan/hooks_memory.c
        src/vulkan/hooks_submit.c)
    target_include_directories(vgpu_vulkan_mod
        PRIVATE ${Vulkan_INCLUDE_DIRS} ${CMAKE_SOURCE_DIR})
    # CMake's default for OBJECT libraries is POSITION_INDEPENDENT_CODE=OFF,
    # which fails to link into the SHARED parent target as soon as any
    # translation unit references a GOT-resident symbol like stderr (any
    # file using LOGGER triggers this). Set it explicitly to match the
    # parent vgpu-control target.
    set_target_properties(vgpu_vulkan_mod PROPERTIES
        POSITION_INDEPENDENT_CODE ON)
    target_sources(vgpu-control PRIVATE $<TARGET_OBJECTS:vgpu_vulkan_mod>)

    # Ship the Vulkan implicit-layer manifest alongside the .so.
    #
    # Copy at configure time into the build tree so the top-level Dockerfile
    # (which COPY --from=builder reads paths under build/) sees a stable
    # location regardless of source-tree layout. Using configure_file with
    # COPYONLY (no @VAR@ substitution) keeps the manifest exactly as
    # checked in - the library_path is hard-coded to the runtime mount
    # location (/etc/vgpu-manager/driver/libvgpu-control.so), matching
    # device-plugin's ContVGPUControlFilePath.
    configure_file(
        ${CMAKE_CURRENT_SOURCE_DIR}/deploy/vgpu_manager_implicit_layer.json
        ${CMAKE_CURRENT_BINARY_DIR}/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json
        COPYONLY)

    # CMake install rule. Not part of the current Docker-based release flow
    # (top-level Dockerfile does direct COPY --from=builder), but harmless
    # and useful for downstream packaging / `make install` consumers.
    install(FILES
        ${CMAKE_CURRENT_SOURCE_DIR}/deploy/vgpu_manager_implicit_layer.json
        DESTINATION etc/vulkan/implicit_layer.d)

    # Vulkan layer unit tests. Independent of CUDAToolkit_FOUND — these
    # tests run on any host with libvulkan-dev (no GPU, no CUDA, no
    # NVML), so devcontainers and CI runners without an NVIDIA stack
    # can still exercise them.
    if(BUILD_TESTS)
        add_subdirectory(test/vulkan)
    endif()
endif()

# find_package(CUDAToolkit REQUIRED)
add_executable(mem_occupy_tool tools/mem_occupy_tool.c)
target_include_directories(mem_occupy_tool PUBLIC ${C_INCLUDE_PATH} ${CUDA_HOME}/include)
target_link_libraries(mem_occupy_tool PRIVATE cuda ${STATIC_C_LIBRARIES})
target_compile_options(mem_occupy_tool PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

add_executable(mem_managed_tool tools/mem_managed_tool.c)
target_include_directories(mem_managed_tool PUBLIC ${C_INCLUDE_PATH} ${CUDA_HOME}/include)
target_link_libraries(mem_managed_tool PRIVATE cuda ${STATIC_C_LIBRARIES})
target_compile_options(mem_managed_tool PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

add_executable(mem_view_tool tools/mem_view_tool.c)
target_include_directories(mem_view_tool PUBLIC ${C_INCLUDE_PATH} ${CUDA_HOME}/include)
target_link_libraries(mem_view_tool PRIVATE cuda nvidia-ml ${STATIC_C_LIBRARIES})
target_compile_options(mem_view_tool PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

add_executable(virt_mem_tool tools/virt_mem_tool.c)
target_include_directories(virt_mem_tool PUBLIC ${C_INCLUDE_PATH} ${CUDA_HOME}/include)
target_link_libraries(virt_mem_tool PRIVATE cuda ${STATIC_C_LIBRARIES})
target_compile_options(virt_mem_tool PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

add_executable(mem_pool_tool tools/mem_pool_tool.c)
target_include_directories(mem_pool_tool PUBLIC ${C_INCLUDE_PATH} ${CUDA_HOME}/include)
target_link_libraries(mem_pool_tool PRIVATE cuda ${STATIC_C_LIBRARIES})
target_compile_options(mem_pool_tool PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

