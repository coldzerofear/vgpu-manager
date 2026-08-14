# Copyright 2024-2026 coldzerofear
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

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
    include/session.h
    include/sm_delta.h
    include/checkpoint_provider.h
    include/vdpau/vdpau.h
    src/cuda_originals.c
    src/nvml_originals.c
    src/cuda_hook.c
    src/nvml_hook.c
    src/loader.c
    src/register.c
    src/session.c
    src/config_io.c
    src/checkpoint_provider.c
    src/util.c
    src/lock.c
    src/metrics.c)
target_link_libraries(vgpu-control ${STATIC_C_LIBRARIES})
target_compile_options(vgpu-control PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

# Tighten the dynamic-symbol surface of libvgpu-control.so.
#
# Without this, every non-static C function in the .so (~80 internal
# helpers like formatUuid / cleanup_vmem_nodes / accumulate_used_memory)
# leaks into .dynsym, where they are available for NVIDIA-ICD /
# loader-side global-symbol resolution to collide with. Keeping the
# export surface narrow at the source avoids that risk without needing
# a heavier remediation like splitting the .so.
#
# Implementation: linker version script alone. We deliberately do NOT
# pair it with -fvisibility=hidden — the compile-time hidden attribute
# forces STV_HIDDEN on every symbol, which the linker cannot promote
# back to global through `global:` patterns. Result of trying both: the
# .so ends up with only the explicitly tagged symbols (dlsym +
# vkNegotiate) and every cu*/nvml* hook becomes invisible — totally
# breaks LD_PRELOAD.
#
# Version-script-only is the standard approach: every symbol stays at
# default visibility through compilation, and the linker's `local: *;`
# below hides everything not matched by `global:`. This produces the
# desired narrow .dynsym (only cu* + nvml* + dlsym + vkNegotiate) AND
# preserves the LD_PRELOAD intercept path.
#
# Applied only to the vgpu-control library — the mem_*_tool /
# virt_mem_tool executables below are unaffected (they contain main()
# which must stay externally visible regardless).
set(VGPU_EXPORTS_SCRIPT ${CMAKE_CURRENT_SOURCE_DIR}/deploy/libvgpu-control.exports.ld)
target_link_options(vgpu-control PRIVATE
    "LINKER:--version-script=${VGPU_EXPORTS_SCRIPT}")
# Re-link if the version script changes — CMake does not infer this
# dependency from -Wl,--version-script.
set_property(TARGET vgpu-control APPEND PROPERTY
    LINK_DEPENDS ${VGPU_EXPORTS_SCRIPT})

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

    # Pre-build: forbid __attribute__((constructor)) / ((destructor)) in
    # library/src/. .init_array entries run at .so load time inside every
    # LD_PRELOAD'd process — concurrent with NVIDIA ICD bringup — and are
    # a known root-cause candidate for the unsolved Step C regression (see
    # note 2026-04-28-vk-trace-isaac-sim.md). We rely on (no constructors +
    # no NVIDIA DT_NEEDED + narrow .dynsym + manifest gating) to neutralize
    # that risk class without splitting the .so. This guard preserves
    # the no-constructors invariant.
    add_custom_target(vgpu_check_no_constructors ALL
        COMMAND bash
                ${CMAKE_CURRENT_SOURCE_DIR}/hack/check_no_constructors.sh
        WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}
        COMMENT "[vgpu] check no constructor/destructor attributes in src/"
        VERBATIM)
    add_dependencies(vgpu-control vgpu_check_no_constructors)

    # Pre-build: forbid paths concatenated at compile time from TMP_DIR /
    # VGPU_MANAGER_PATH. Those resolve to the pre-session location always, so
    # inside a session they name a file nothing creates -- the open() fails and
    # the caller carries on unprotected. Needs the preprocessor, since the
    # concatenation is written in macro names rather than in the path it makes.
    add_custom_target(vgpu_check_session_paths ALL
        COMMAND bash
                ${CMAKE_CURRENT_SOURCE_DIR}/hack/check_session_paths.sh
        WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}
        COMMENT "[vgpu] check session path routing in src/"
        VERBATIM)
    add_dependencies(vgpu-control vgpu_check_session_paths)
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
# Shared-region checks (sm_node token bucket, vmem_node ledger).
#
# Built and run separately from test/ because they need NEITHER CUDA nor a GPU:
# they fork processes over a real MAP_SHARED region and assert the properties
# those regions rest on -- that concurrent cross-process CAS deductions lose
# nothing, that the refill election admits at most one winner per period, that
# the vmem ledger keeps one slot per PID under concurrent registration, and that
# its per-device byte-range lock actually excludes at the offsets lock.c
# computes. All are exactly the kind of thing that cannot be checked by reading
# the code, and all can run in ordinary CI.
#
#   cmake -DBUILD_SM_NODE_TESTS=ON .. && make test_sm_node_shared
#   ./test_sm_node_shared
#   ./test_vmem_region_concurrency
# ---------------------------------------------------------------------------
#
# Live in test/nogpu/ rather than test/ on purpose: test/CMakeLists.txt does
# file(GLOB "test_*.c") and builds everything it finds against the CUDA
# includes only. A file there that includes hook.h fails to compile the moment
# anyone configures with -DBUILD_TESTS=ON. The subdirectory keeps the two test
# families apart -- test/ needs a GPU and links CUDA, test/nogpu/ needs neither.
#
# Sources that touch neither CUDA nor NVML, shared by the GPU-less test and
# the session-config tool so neither has to restate the library's own file
# handling (region layout, locking, path derivation).
set(VGPU_NODRIVER_SRCS
    src/session.c src/config_io.c src/util.c src/lock.c src/metrics.c)

option(BUILD_SM_NODE_TESTS "Build the GPU-less shared-region checks" OFF)
if(BUILD_SM_NODE_TESTS)
    add_executable(test_sm_node_shared test/nogpu/test_sm_node_shared.c)
    target_include_directories(test_sm_node_shared PRIVATE
        ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/include)

    add_executable(test_vmem_region_concurrency test/nogpu/test_vmem_region_concurrency.c)
    target_include_directories(test_vmem_region_concurrency PRIVATE
        ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/include)

    # Unlike the two above -- which reimplement the region semantics against
    # their own temp files -- this one exercises the real path derivation and
    # the real config writer, so it links the driver-free sources.
    add_executable(test_session_paths test/nogpu/test_session_paths.c ${VGPU_NODRIVER_SRCS})
    target_include_directories(test_session_paths PRIVATE
        ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/include)

    # Drives the provider ABI against a concurrent reader, so it needs the
    # provider too -- the rewrite it checks is the provider.s.
    add_executable(test_session_pids_concurrency
        test/nogpu/test_session_pids_concurrency.c src/checkpoint_provider.c ${VGPU_NODRIVER_SRCS})
    target_include_directories(test_session_pids_concurrency PRIVATE
        ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/include)

    # Header-only: exercises the inline sm_delta_step directly, plus a frozen
    # replica of the removed formula for the MAE comparison.
    add_executable(test_delta_step test/nogpu/test_delta_step.c)
    target_include_directories(test_delta_step PRIVATE
        ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/include)

    # The dominance campaign: 55 GPU models x limits 1-100 x 3 workloads x
    # 2 delays, new step vs frozen old, asserting zero regressed cells.
    # -O2 keeps the 33000-cell grid to ~10 seconds regardless of build type.
    add_executable(test_delta_campaign test/nogpu/test_delta_campaign.c)
    target_include_directories(test_delta_campaign PRIVATE
        ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/include)
    target_compile_options(test_delta_campaign PRIVATE -O2)
endif()

# Writes a session quota region the way the GPU-node agent would, so the
# session paths can be exercised without Kubernetes. Needs no CUDA toolkit,
# hence its placement outside the CUDA tool block below.
find_package(Threads REQUIRED)
add_executable(vgpu-session-config tools/session_config.c ${VGPU_NODRIVER_SRCS})
target_include_directories(vgpu-session-config PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/include)
target_link_libraries(vgpu-session-config PRIVATE Threads::Threads)

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

