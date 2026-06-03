#!/usr/bin/bash
set -euo pipefail

rm -rf build && mkdir -p build

# cmake -D CUDA_TOOLKIT_ROOT_DIR=/usr/local/cuda-10. ..
cd build && cmake -DCMAKE_BUILD_TYPE=Release ..

make