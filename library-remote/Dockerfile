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

ARG NVIDIA_CUDA_IMAGE=nvidia/cuda:12.4.1-devel-ubuntu20.04

FROM $NVIDIA_CUDA_IMAGE

ARG APT_MIRROR

RUN echo "Asia/Shanghai" > /etc/timezone && \
    ln -fs /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN if [ -n "$APT_MIRROR" ]; then sed -i "s@http://archive.ubuntu.com@${APT_MIRROR}@g" /etc/apt/sources.list ; fi && \
    apt-get -y update && apt-get -y install make cmake && rm -rf /var/lib/apt/lists/*

WORKDIR /vgpu-controller

COPY . .

RUN chmod +x build.sh && ./build.sh
