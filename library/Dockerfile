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

ARG NVIDIA_CUDA_IMAGE=nvidia/cuda:13.4.2-devel-rockylinux8

FROM $NVIDIA_CUDA_IMAGE

ARG DNF_MIRROR

RUN echo "Asia/Shanghai" > /etc/timezone && \
    ln -fs /usr/share/zoneinfo/Asia/Shanghai /etc/localtime

RUN if [ -n "$DNF_MIRROR" ]; then \
      sed -e 's|^mirrorlist=|#mirrorlist=|g' \
          -e "s|^#baseurl=http://dl\.rockylinux\.org/\$contentdir|baseurl=${DNF_MIRROR}|g" \
          -i.bak /etc/yum.repos.d/Rocky-*.repo; \
    fi && dnf makecache && dnf install -y make cmake gcc-c++ && dnf clean all

WORKDIR /vgpu-controller

COPY . .

RUN chmod +x build.sh && ./build.sh
