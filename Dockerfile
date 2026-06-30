# Declared here for use in FROM directives below
ARG TOOLKIT_CONTAINER_IMAGE=unknown

ARG BASE_BUILD_IMAGE=unknown

FROM ${BASE_BUILD_IMAGE} AS builder

# Pull the nvidia-cdi-hook binary out of the relevant toolkit container (arch:
# TARGETPLATFORM). It is installed onto the host by the `install` init container
# (via install_files.sh) and referenced from the generated CDI specification when
# a cdi-* device-list-strategy is enabled. The explicit --platform keeps the
# binary matching the target node arch (and makes Dockerfile.cross skip it).
FROM --platform=$TARGETPLATFORM ${TOOLKIT_CONTAINER_IMAGE} AS toolkit

FROM quay.io/jitesoft/ubuntu:20.04

ENV NVIDIA_DISABLE_REQUIRE="true"

ARG BUILD_VERSION="v1.0.0"
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

COPY --from=builder /vgpu-controller/build/libvgpu-control.so /installed/libvgpu-control.so.$BUILD_VERSION
COPY --from=builder /vgpu-controller/build/mem_occupy_tool    /installed/tools/mem_occupy_tool
COPY --from=builder /vgpu-controller/build/mem_managed_tool   /installed/tools/mem_managed_tool
COPY --from=builder /vgpu-controller/build/mem_view_tool      /installed/tools/mem_view_tool
COPY --from=builder /go/src/vgpu-manager/bin/device-client    /installed/registry/device-client
# Bundled NVIDIA CDI hook; installed to the host (/etc/vgpu-manager/nvidia-cdi-hook)
# by the install init container and referenced from the generated CDI spec.
COPY --from=toolkit /artifacts/rpm/usr/bin/nvidia-cdi-hook    /installed/nvidia-cdi-hook

RUN echo '/etc/vgpu-manager/driver/libvgpu-control.so' > /installed/ld.so.preload
