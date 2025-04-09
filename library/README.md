# VGPU-Controller

CUDA driver API dynamic library for GPU virtualization and resource hard isolation.

## Project objectives:

- [x] Simplify GRPC within containers
- [x] Ensure hard isolation of gpu resources
- [x] Support CUDA 12.x version drivers
- [x] Support cgroupv1/cgroupv2 and Host compatibility mode
- [x] Optimized multi card computing power and memory limitations
- [x] Dynamically scale core limitations based on remaining resources
- [x] Allow virtual memory to be used when memory limit is exceeded 
- [x] Allow GPU virtual memory based on UVA
- [x] support open-gpu-kernel-modules driver compatibility mode

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
* GPU_DEVICES_UUID: gpu device uuids
* ENV_COMPATIBILITY_MODE: environment compatibility mode

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

CUDA 12.x and before are supporteds

Any architecture of GPU after Kepler are supported