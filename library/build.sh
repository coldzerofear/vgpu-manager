#!/usr/bin/bash

rm -rf build
mkdir -p build
cd build
# cmake -D CUDA_TOOLKIT_ROOT_DIR=/usr/local/cuda-10. ..
cmake -DCMAKE_BUILD_TYPE=Release ..
make