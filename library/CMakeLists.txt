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

set(STATIC_C_LIBRARIES -static-libgcc -static-libstdc++ -pthread)

if (${USE_ORIGINAL})
    add_definitions(-DUSE_ORIGINAL)
endif ()

# controller related
add_library(vgpu-control SHARED
    include/hook.h
    include/cuda-subset.h
    include/nvml-subset.h
    include/cuda-helper.h
    include/nvml-helper.h
    include/vdpau/vdpau.h
    src/cuda_originals.c
    src/nvml_originals.c
    src/cuda_hook.c
    src/nvml_hook.c
    src/loader.c
    src/util.c)
target_link_libraries(vgpu-control ${STATIC_C_LIBRARIES})
target_compile_options(vgpu-control PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

# find_package(CUDAToolkit REQUIRED)
add_executable(mem_occupy_tool tools/mem_occupy_tool.c)
target_include_directories(mem_occupy_tool PUBLIC ${C_INCLUDE_PATH} "/usr/local/cuda/include")
target_link_libraries(mem_occupy_tool PRIVATE cuda ${STATIC_C_LIBRARIES})
target_compile_options(mem_occupy_tool PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

add_executable(mem_managed_tool tools/mem_managed_tool.c)
target_include_directories(mem_managed_tool PUBLIC ${C_INCLUDE_PATH} "/usr/local/cuda/include")
target_link_libraries(mem_managed_tool PRIVATE cuda ${STATIC_C_LIBRARIES})
target_compile_options(mem_managed_tool PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

add_executable(mem_view_tool tools/mem_view_tool.c)
target_include_directories(mem_view_tool PUBLIC ${C_INCLUDE_PATH} "/usr/local/cuda/include")
target_link_libraries(mem_view_tool PRIVATE cuda ${STATIC_C_LIBRARIES})
target_compile_options(mem_view_tool PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)

add_executable(virt_mem_tool tools/virt_mem_tool.c)
target_include_directories(virt_mem_tool PUBLIC ${C_INCLUDE_PATH} "/usr/local/cuda/include")
target_link_libraries(virt_mem_tool PRIVATE cuda ${STATIC_C_LIBRARIES})
target_compile_options(virt_mem_tool PUBLIC $<$<COMPILE_LANGUAGE:CXX>:-std=c++11>)
