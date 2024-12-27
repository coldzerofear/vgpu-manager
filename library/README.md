# VGPU-Controller

Cuda driver API interception library, implementing hard resource limitation.

Based on [vcuda-controller](https://github.com/tkestack/vcuda-controller) and [HAMi-coer](https://github.com/Project-HAMi/HAMi-core) transformation.

Simplify gRPC within containers, capable of running without device plugins, and supporting cgroupv2 container environments.

## Building a dynamic link library

```
./build.sh
```

## Find new library functions
```bash
./find_new_lib.sh /lib/x86_64-linux-gnu/libcuda.so.535.54.03 /lib/x86_64-linux-gnu/libnvidia-ml.so.535.54.03
```

## Usage

environment variable

* VGPU_POD_NAME: current pod name
* VGPU_POD_UID: current pod uid
* VGPU_CONTAINER_NAME: current container name
* CUDA_MEM_LIMIT_<index>: gpu memory limit
* CUDA_CORE_LIMIT_<index>: gpu core limit
* GPU_DEVICES_UUID: gpu uuid

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