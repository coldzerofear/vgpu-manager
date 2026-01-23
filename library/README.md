# VGPU-Controller

CUDA driver API dynamic library for GPU virtualization and resource hard isolation.

## Project objectives:

- [x] Ensure hard isolation of gpu resources
- [x] Support CUDA 13.x version drivers
- [x] Support cgroupv1 and cgroupv2 container environment
- [x] Optimized multi card computing power and memory limitations
- [x] Dynamically scale core limitations based on remaining resources
- [x] GPU Virtual Memory Overallocation Based on UVA
- [x] Support open-gpu-kernel-modules driver compatibility mode
- [x] Record GPU virtual memory allocation and limits
- [x] Support gpu utilization information provided by external observers to reduce driving overhead
- [x] Support client registration mode to improve container security

> Note: Checking indicates that the function has been completed, while unchecking indicates that the function has not been completed or is planned to be implemented.

## Building a dynamic link library

```
./build.sh
```

## Find new library functions

```bash
./find_new_lib.sh /lib/x86_64-linux-gnu/libcuda.so.1 /lib/x86_64-linux-gnu/libnvidia-ml.so.1
```

## Environment variable

* VGPU_POD_NAME: current pod name
* VGPU_POD_NAMESPACE: current pod namespace
* VGPU_POD_UID: current pod uid
* VGPU_CONTAINER_NAME: current container name
* CUDA_MEM_LIMIT_<index>: gpu memory limit
* CUDA_MEM_RATIO_<index>: gpu memory scaling ratio
* CUDA_CORE_LIMIT_<index>: gpu core limit
* CUDA_CORE_SOFT_LIMIT_<index>: gpu core soft limit
* CUDA_MEM_OVERSOLD_<index>: gpu memory oversold switch
* MANAGER_VISIBLE_DEVICES: List of GPU UUIDs visible to container
* MANAGER_VISIBLE_DEVICE_<index>: Single GPU UUID visible to the container
* MANAGER_COMPATIBILITY_MODE: Environment compatibility mode

## Log level

Use environment variable `LOGGER_LEVEL` to set the visibility of logs

| LOGGER_LEVEL       | description                                 |
| ------------------ |---------------------------------------------|
| 0                  | fatal                                       |
| 1                  | errors,fatal                                |
| 2                  | warnings,errors,fatal                       |
| 3 (default)        | infos,warnings,errors,fatal                 |
| 4                  | verbose,infos,warnings,errors,fatal         |
| 5                  | details,verbose,infos,warnings,errors,fatal |

## CUDA/GPU support information

CUDA 13.x and before are supporteds

Any architecture of GPU after Kepler are supported