# Declared here for use in FROM directives below
ARG BASE_BUILD_IMAGE=unknown

FROM ${BASE_BUILD_IMAGE} AS builder

FROM quay.io/jitesoft/ubuntu:20.04

ENV NVIDIA_DISABLE_REQUIRE="true"

WORKDIR /

COPY --from=builder /go/src/vgpu-manager/bin/device-scheduler /usr/local/bin/device-scheduler
COPY --from=builder /go/src/vgpu-manager/bin/device-plugin    /usr/local/bin/device-plugin
COPY --from=builder /go/src/vgpu-manager/bin/device-monitor   /usr/local/bin/device-monitor
COPY --from=builder /go/src/vgpu-manager/bin/device-webhook   /usr/local/bin/device-webhook

# Add top-level license (AL2) file into the container image
COPY LICENSE /
COPY --chmod=755 scripts/install_files.sh scripts/install_files.sh

COPY --from=builder /vgpu-controller/build/libvgpu-control.so /installed/libvgpu-control.so
COPY --from=builder /vgpu-controller/build/mem_occupy_tool    /installed/tools/mem_occupy_tool
COPY --from=builder /vgpu-controller/build/mem_managed_tool   /installed/tools/mem_managed_tool
COPY --from=builder /vgpu-controller/build/mem_view_tool      /installed/tools/mem_view_tool
COPY --from=builder /go/src/vgpu-manager/bin/device-client    /installed/registry/device-client

RUN echo '/etc/vgpu-manager/driver/libvgpu-control.so' > /installed/ld.so.preload