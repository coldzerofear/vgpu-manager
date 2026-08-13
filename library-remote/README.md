# VGPU-Remote-Adapter

A remote vGPU adapter for implementing network-based remote vGPU hard isolation

## Project objectives:

- [ ] Suitable for use with Lupine Server
- [ ] Remote container GPU device quota configuration discovery
- [ ] Remote container device PID list discovery/maintenance
- [ ] Remote container level memory strict isolation
- [ ] Remote container level SM core strict isolation
- [ ] Container level multi process shared token bucket

> Note: Checking indicates that the function has been completed, while unchecking indicates that the function has not been completed or is planned to be implemented.

## Building a dynamic link library

```
./build.sh
```

## Environment variable

* CUDA_MEM_LIMIT_<index>: gpu memory limit
* CUDA_MEM_RATIO_<index>: gpu memory scaling ratio
* CUDA_CORE_LIMIT_<index>: gpu core limit
* CUDA_CORE_SOFT_LIMIT_<index>: gpu core soft limit
* CUDA_MEM_OVERSOLD_<index>: gpu memory oversold switch
* MANAGER_VISIBLE_DEVICES: List of GPU UUIDs visible to container
* MANAGER_VISIBLE_DEVICE_<index>: Single GPU UUID visible to the container
* MANAGER_COMPATIBILITY_MODE: Environment compatibility mode
* EXTERNAL_SM_WATCHER_ENABLED: Enable external SM util watcher
* VMEMORY_NODE_ENABLED: Enable virtual memory node tracing
* CUDA_SM_DELTA_RAMP_FLOOR_DIVISOR: Accelerate delta utilization rate climb speed - default 64
* CUDA_SM_SHARED_BUCKET: Enable SM shared token bucket
* VGPU_CONFIG_SESSION_PATH: Define the limit configuration path for the session

## Log level

Use environment variable `LOGGER_LEVEL` to set the visibility of logs

| LOGGER_LEVEL       | description                                 |
| ------------------ |---------------------------------------------|
| 0                  | fatal                                       |
| 1                  | errors,fatal                                |
| 2 (default)        | warnings,errors,fatal                       |
| 3                  | infos,warnings,errors,fatal                 |
| 4                  | verbose,infos,warnings,errors,fatal         |
| 5                  | details,verbose,infos,warnings,errors,fatal |

## CUDA/GPU support information

CUDA 13.1 and before are supporteds
