ARG LIBRARY_IMG=coldzerofear/vgpu-controller:latest



FROM quay.io/jitesoft/ubuntu:24.04

#ENV NVIDIA_VISIBLE_DEVICES=all
#ENV NVIDIA_DRIVER_CAPABILITIES=utility,compute

COPY --from=build /go/src/volcano.sh/devices/volcano-vgpu-device-plugin /usr/bin/volcano-vgpu-device-plugin
COPY --from=build /go/src/volcano.sh/devices/volcano-vgpu-monitor /usr/bin/volcano-vgpu-monitor
COPY --from=build /go/src/volcano.sh/devices/nvml-monitor /k8s-vgpu/lib/nvidia/

RUN mkdir -p /installed
COPY --from=$LIBRARY_IMG /libvgpu-controller/build/libvgpu-control.so /installed/
RUN echo '/etc/vgpu-manager/libvgpu-control.so' > /installed/ld.so.preload