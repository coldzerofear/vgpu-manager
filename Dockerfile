# Declared here for use in FROM directives below
ARG BASE_BUILD_IMAGE=unknown

FROM ${BASE_BUILD_IMAGE} AS builder

FROM quay.io/jitesoft/ubuntu:20.04

ENV NVIDIA_DISABLE_REQUIRE="true"

ARG BUILD_VERSION="N/A"
ARG GIT_COMMIT="unknown"
ARG BUILD_DATE="1970-01-01T00:00:00Z"

LABEL io.k8s.display-name="VGPU-Manager"
LABEL name="VGPU-Manager"
LABEL summary="Kubernetes manager for NVIDIA vGPU resources"
LABEL description="Manager binaries for scheduling, device plugin, monitoring, and webhook integration for NVIDIA vGPU resources"
LABEL org.opencontainers.image.title="VGPU-Manager"
LABEL org.opencontainers.image.description="Manager binaries for scheduling, device plugin, monitoring, and webhook integration for NVIDIA vGPU resources"
LABEL org.opencontainers.image.url="https://github.com/coldzerofear/vgpu-manager"
LABEL org.opencontainers.image.source="https://github.com/coldzerofear/vgpu-manager"
LABEL org.opencontainers.image.version="${BUILD_VERSION}"
LABEL org.opencontainers.image.revision="${GIT_COMMIT}"
LABEL org.opencontainers.image.created="${BUILD_DATE}"
LABEL org.opencontainers.image.vendor="coldzerofear"
LABEL org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /

COPY --from=builder /go/src/vgpu-manager/bin/device-scheduler /usr/local/bin/device-scheduler
COPY --from=builder /go/src/vgpu-manager/bin/device-plugin    /usr/local/bin/device-plugin
COPY --from=builder /go/src/vgpu-manager/bin/device-monitor   /usr/local/bin/device-monitor
COPY --from=builder /go/src/vgpu-manager/bin/device-webhook   /usr/local/bin/device-webhook

# Add top-level license (AL2) file into the container image
COPY --from=builder /LICENSE /LICENSE
COPY --chmod=755 scripts/install_files.sh scripts/install_files.sh

COPY --from=builder /vgpu-controller/build/libvgpu-control.so /installed/libvgpu-control.so
COPY --from=builder /vgpu-controller/build/mem_occupy_tool    /installed/tools/mem_occupy_tool
COPY --from=builder /vgpu-controller/build/mem_managed_tool   /installed/tools/mem_managed_tool
COPY --from=builder /vgpu-controller/build/mem_view_tool      /installed/tools/mem_view_tool
COPY --from=builder /go/src/vgpu-manager/bin/device-client    /installed/registry/device-client

# Vulkan implicit-layer manifest. Stays unconditional even on CUDA-only
# Pods: the layer's enable_environment gate (VGPU_VULKAN_ENABLE=1) is
# only set by device-plugin for Pods that opt in, so the loader does not
# probe the .so for non-Vulkan workloads.
COPY --from=builder /vgpu-controller/build/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json \
                    /installed/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json

RUN echo '/etc/vgpu-manager/driver/libvgpu-control.so' > /installed/ld.so.preload
