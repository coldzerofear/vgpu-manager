# VGPU-Controller

Cuda driver API interception library, implementing hard resource limitation.

Based on [vcuda-controller](https://github.com/tkestack/vcuda-controller) and [HAMi-core](https://github.com/Project-HAMi/HAMi-core) transformation.

Simplify gRPC within containers, and supporting cgroupv2 container environments.

## Project objectives:

* Simplify GRPC within containers
* Ensure hard isolation of gpu resources
* Support CUDA 12.x version drivers
* Support CGroupv1/CGroupv2 and Host compatibility mode
* Optimized multi card computing power and memory limitations
* Dynamically scale core limitations based on remaining resources
* Allow virtual memory to be used when memory limit is exceeded
* Allow GPU virtual memory based on UVA

## Building a dynamic link library

```
./build.sh
```

## Find new library functions

```bash
./find_new_lib.sh /lib/x86_64-linux-gnu/libcuda.so.1 /lib/x86_64-linux-gnu/libnvidia-ml.so.1
```

## Usage

environment variable

* VGPU_POD_NAME: current pod name
* VGPU_POD_NAMESPACE: current pod namespace
* VGPU_POD_UID: current pod uid
* VGPU_CONTAINER_NAME: current container name
* CUDA_MEM_LIMIT_<index>: gpu memory limit
* CUDA_CORE_LIMIT_<index>: gpu core limit
* CUDA_CORE_SOFT_LIMIT_<index>: gpu core soft limit
* CUDA_MEM_OVERSOLD_<index>: gpu memory oversold switch
* GPU_DEVICES_UUID: gpu uuids

## Log level

Use environment variable `LOGGER_LEVEL` to set the visibility of logs

| LOGGER_LEVEL       | description                                 |
| ------------------ |---------------------------------------------|
| 0                  | infos                                       |
| 1                  | errors,infos                                |
| 2                  | warnings,errors,infos                       |
| 3 (default)        | fatal,warnings,errors,infos                 |
| 4                  | verbose,fatal,warnings,errors,infos         |
| 5                  | details,verbose,fatal,warnings,errors,infos |

## CUDA/GPU support information

CUDA 12.2.x and before are supporteds

Any architecture of GPU after Kepler are supported