#!/usr/bin/bash
# Build entry point used by library/Dockerfile (and any direct invokers).
#
# BUILD_VULKAN_LAYER defaults to ON: the Vulkan implicit layer is part of
# the standard release artifact. Pure CUDA builds can override:
#
#   BUILD_VULKAN_LAYER=OFF ./build.sh
#
# Vulkan being on does NOT load the layer for CUDA-only workloads — the
# layer's manifest gates loading on the VGPU_VULKAN_ENABLE=1 environment
# variable, which device-plugin only sets for Pods that opt in. CUDA-only
# Pods see zero overhead.
set -e

rm -rf build
mkdir -p build

cd build

# cmake -D CUDA_TOOLKIT_ROOT_DIR=/usr/local/cuda-10. ..
cmake \
    -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_VULKAN_LAYER="${BUILD_VULKAN_LAYER:-ON}" \
    ..

make
