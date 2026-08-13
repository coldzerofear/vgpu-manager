# Copyright 2026 coldzerofear
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
add_library(vgpu-remote SHARED
    include/hook.h
    include/list.h
    include/cuda-subset.h
    include/nvml-subset.h
    include/cuda-helper.h
    include/nvml-helper.h
    include/metrics.h
    include/vdpau/vdpau.h
    include/checkpoint_provider.h
    include/session.h
    src/session.c
    src/cuda_originals.c
    src/nvml_originals.c
    src/cuda_hook.c
    src/nvml_hook.c
    src/loader.c
    src/checkpoint_provider.c
    src/util.c
    src/lock.c
    src/metrics.c)
target_link_libraries(vgpu-remote ${STATIC_C_LIBRARIES})
target_compile_options(vgpu-remote PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

set(VGPU_EXPORTS_SCRIPT ${CMAKE_CURRENT_SOURCE_DIR}/deploy/libvgpu-remote.exports.ld)
target_link_options(vgpu-remote PRIVATE "LINKER:--version-script=${VGPU_EXPORTS_SCRIPT}")

# Re-link if the version script changes — CMake does not infer this dependency from -Wl,--version-script.
set_property(TARGET vgpu-remote APPEND PROPERTY LINK_DEPENDS ${VGPU_EXPORTS_SCRIPT})
