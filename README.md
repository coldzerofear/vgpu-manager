# VGPU-Manager

A k8s device plugin for managing and allocating vGPU devices.

The project forks based on [gpu-manager](https://github.com/tkestack/gpu-manager)

Will support multi card virtualization allocation

Project objectives:

* Simplify GRPC calls within containers
* support CUDA12 version drivers
* support cgroupv2
* dual scheduling strategy for nodes and devices
* Compatible with hot swappable devices and expansion capabilities

The project is currently underway.

// qos mount to /etc/vgpu-manager/cgroup/ ReadOnly
/sys/fs/cgroup/devices/kubepods.slice/kubepods-pod68bc7fad_794f_4c77_9eae_4120e76b29a4.slice 

// besteffort qos mount to /etc/vgpu-manager/cgroup/ ReadOnly
/sys/fs/cgroup/devices/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod68bc7fad_794f_4c77_9eae_4120e76b29a4.slice

// burstable qos mount to /etc/vgpu-manager/cgroup/ ReadOnly
/sys/fs/cgroup/devices/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod68bc7fad_794f_4c77_9eae_4120e76b29a4.slice

cgroupv2
/sys/fs/cgroup/kubepods.slice/kubepods-pod68bc7fad_794f_4c77_9eae_4120e76b29a4.slice

/sys/fs/cgroup/devices/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pode151a555_a8bc_406f_a249_9ca74724ad79.slice/d288a1e0952727ffbee274066206608e47cf78421eeea6d4b1af66eb35781754

宿主机目录
/etc/vgpu-manager/{pod-uid}-{container-name}/vgpu.config
/etc/vgpu-manager/vgpu.sock
/etc/vgpu-manager/vgpu-control.so
/etc/vgpu-manager/ld.so.preload

容器内目录
/etc/vgpu-manager/vgpu.config
/etc/vgpu-manager/host_cgroup/
/etc/vgpu-manager/host_proc/

动态均衡利用率 
当只有一个gpu进程，将所有的利用率分配（100%）
当发现新的gpu进程，调整利用率



