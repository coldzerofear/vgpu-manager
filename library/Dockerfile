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
