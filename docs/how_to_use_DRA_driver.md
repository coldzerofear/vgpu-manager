## Describe

vgpu-manager supports providing vGPU to POD through Kubernetes' latest dynamic resource allocation(DRA) mechanism

Using DRA requires the independent installation of the DRA driver for the vgpu-manager, which cannot work simultaneously with the vgpu-manager-device-plugin

## Prerequisite

* NVIDIA drivers >= 440
* nvidia-docker version > 2.0
* default runtime configured as nvidia for containerd/docker/cri-o container runtime
    - `accept-nvidia-visible-devices-as-volume-mounts` option should be set to `true` for NVIDIA Container Runtime
* Kubernetes version >= 1.34 wit DRAConsumableCapacity feature enabled
* glibc >= 2.17 & glibc < 2.30
* kernel version >= 3.10

> Note: DRAConsumableCapacity作为 Kubernetes 1.34 中的 alpha 功能引入。功能门必须在 kubelet、kube-apiserver、kube-scheduler 和 kube-controller-manager 中启用。DRAConsumableCapacity

## Install

```bash
kubectl apply -f deploy/vgpu-manager-kubeletplugin.yaml
```

## Usage

