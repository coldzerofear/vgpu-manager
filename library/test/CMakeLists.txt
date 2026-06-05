# library/test/CMakeLists.txt
#
# Builds smoke-test executables that link against the real CUDA toolkit.
# The library/ CMakeLists.txt invokes this only when BUILD_TESTS=ON
# AND a CUDA toolkit is located.
#
# Tests are intended to be run with
#   LD_PRELOAD=<build>/libvgpu-control.so ./<test>
# via the sibling run_all_tests.sh script.

set(TEST_SRC_DIR ${CMAKE_CURRENT_SOURCE_DIR})
set(TEST_BIN_DIR ${CMAKE_CURRENT_BINARY_DIR})
set(CMAKE_RUNTIME_OUTPUT_DIRECTORY ${TEST_BIN_DIR})

# ---- locate CUDA ----------------------------------------------------------
# Prefer the modern CUDAToolkit package; fall back to the legacy FindCUDA
# module (for .cu files). CUDA 12 / 13 both ship both modules.
find_package(CUDAToolkit QUIET)
find_package(CUDA QUIET)

if(NOT CUDAToolkit_FOUND AND NOT CUDA_FOUND)
    message(WARNING "[vgpu] CUDA toolkit not found - skipping test/ subdir")
    return()
endif()

if(CUDAToolkit_FOUND)
    set(_CUDA_INCLUDE ${CUDAToolkit_INCLUDE_DIRS})
    # CUDAToolkit_LIBRARY_DIR may not exist in CUDA 13; derive from known libs
    if(CUDAToolkit_LIBRARY_DIR)
        set(_CUDA_LIBDIR ${CUDAToolkit_LIBRARY_DIR})
    elseif(CUDAToolkit_rt_LIBRARY)
        get_filename_component(_CUDA_LIBDIR ${CUDAToolkit_rt_LIBRARY} DIRECTORY)
    elseif(CUDAToolkit_CUDART_LIBRARY)
        get_filename_component(_CUDA_LIBDIR ${CUDAToolkit_CUDART_LIBRARY} DIRECTORY)
    else()
        # Fallback: try common CUDA 12/13 paths
        if(EXISTS "/usr/local/cuda/targets/x86_64-linux/lib")
            set(_CUDA_LIBDIR "/usr/local/cuda/targets/x86_64-linux/lib")
        elseif(EXISTS "/usr/local/cuda/lib64")
            set(_CUDA_LIBDIR "/usr/local/cuda/lib64")
        endif()
    endif()
    # CUDA 12/13: cudart is found via CUDAToolkit_CUDART_LIBRARY
    if(CUDAToolkit_CUDART_LIBRARY)
        set(_CUDART_LIBRARY ${CUDAToolkit_CUDART_LIBRARY})
    endif()
else()
    set(_CUDA_INCLUDE ${CUDA_INCLUDE_DIRS})
    set(_CUDA_LIBDIR  ${CUDA_TOOLKIT_ROOT_DIR}/lib64)
endif()

message(STATUS "[vgpu] test/ using CUDA include=${_CUDA_INCLUDE} libdir=${_CUDA_LIBDIR}")

# ---- TEST_DEVICE_ID passthrough -------------------------------------------
if(DEFINED TEST_DEVICE_ID)
    add_definitions(-DTEST_DEVICE_ID=${TEST_DEVICE_ID})
endif()

# ---- C tests --------------------------------------------------------------
file(GLOB C_TESTS "${TEST_SRC_DIR}/test_*.c")
foreach(src ${C_TESTS})
    get_filename_component(name ${src} NAME_WE)
    add_executable(${name} ${src})
    target_include_directories(${name} PRIVATE ${_CUDA_INCLUDE} ${TEST_SRC_DIR})
    target_link_directories(${name} PRIVATE ${_CUDA_LIBDIR})
    # Link cuda (driver) and cudart (runtime) explicitly since CUDA 12/13
    # cmake packages may not correctly resolve cudart from 'cuda' target
    if(_CUDART_LIBRARY)
        target_link_libraries(${name} PRIVATE cuda "${_CUDART_LIBRARY}" nvidia-ml pthread rt)
    else()
        target_link_libraries(${name} PRIVATE cuda cudart nvidia-ml pthread rt)
    endif()
endforeach()

# ---- CUDA tests (kernel launches) -----------------------------------------
file(GLOB CU_TESTS "${TEST_SRC_DIR}/test_*.cu")
if(CU_TESTS)
    if(CUDA_FOUND)
        foreach(src ${CU_TESTS})
            get_filename_component(name ${src} NAME_WE)
            cuda_add_executable(${name} ${src})
            target_include_directories(${name} PRIVATE ${TEST_SRC_DIR})
            target_link_libraries(${name} cuda nvidia-ml pthread rt)
        endforeach()
    else()
        message(WARNING "[vgpu] legacy FindCUDA not available - skipping .cu tests")
    endif()
endif()

# ---- copy python smoke tests + runner into build dir ----------------------
add_custom_target(vgpu_copy_test_scripts ALL
    COMMAND ${CMAKE_COMMAND} -E copy_directory
            ${TEST_SRC_DIR}/python ${TEST_BIN_DIR}/python
    COMMAND ${CMAKE_COMMAND} -E copy
            ${TEST_SRC_DIR}/run_all_tests.sh ${TEST_BIN_DIR}/run_all_tests.sh
    COMMAND ${CMAKE_COMMAND} -E copy
            ${TEST_SRC_DIR}/run_tests_with_env.sh ${TEST_BIN_DIR}/run_tests_with_env.sh
    COMMENT "[vgpu] stage python/ + run_all_tests.sh into test build dir"
    VERBATIM)
