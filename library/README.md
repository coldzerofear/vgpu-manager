# VGPU-Controller

CUDA driver API dynamic library for GPU virtualization and resource hard isolation.

Using [LUPINE](https://github.com/lupinemachines/lupine) to unlock remote GPU virtualization and achieve session level resource hard isolation

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
- [x] Automatic SM current limiting algorithm routing
- [x] Multi process shared token bucket to prevent SM utilization fluctuation
- [x] Remote session device visibility isolation
- [x] Remote Session level multi process shared token bucket
- [x] Remote Session level memory strict isolation
- [x] Remote Session level SM core strict isolation

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
* EXTERNAL_SM_WATCHER_ENABLED: Enable external SM util watcher
* VMEMORY_NODE_ENABLED: Enable virtual memory node tracing
* CUDA_SM_CONTROLLER: Core limit algorithm: delta (default) | aimd | auto. delta scales its correction
  by sm_num^2 against a pool linear in sm_num, so on very high-SM cards (188-SM Blackwell class) it
  cannot hold the limit -- use aimd (or auto) there; see docs/sm_controller_aimd.md
* CUDA_SM_AIMD_MD_DIVISOR / _EFF_RATIO / _AI_BASE_DIV / _DEADBAND_RATIO / _MD_COOLDOWN_CYCLES: AIMD tunables
* CUDA_SM_DELTA_RAMP_FLOOR_DIVISOR: Accelerate delta utilization rate climb speed - default 64
* CUDA_SM_SHARED_BUCKET: Container-wide shared SM token bucket. On by default; set to 0 to opt out
  (ignored in a remote session, where a per-process bucket would give every connection the full core quota)

### Remote session (lupine-server) only

Set on the GPU node, not inside a pod. See `docs/remote_gpu_pool_research_design.md`.

* VGPU_REMOTE_MODE: Mark the process as serving remote sessions only. Without a valid session quota the
  library refuses to serve rather than falling back to a permissive config
* VGPU_CONFIG_SESSION_BASE: Directory holding session directories (default /etc/vgpu-manager/remote-sessions)
* VGPU_CONFIG_SESSION_PATH: This session's directory. Set per connection by the checkpoint provider, never by
  hand; the quota, pids.config, .vgpu_lock, .vmem_node and .sm_node all derive from it

## Remote GPU deployment (lupine-server)

The same `libvgpu-control.so` is both the LD_PRELOAD hook library and lupine's
checkpoint provider, which is what injects the per-connection session:

```
LD_PRELOAD=/opt/vgpu/lib/libvgpu-control.so \
VGPU_REMOTE_MODE=1 \
LUPINE_CHECKPOINT_LIBRARY=/opt/vgpu/lib/libvgpu-control.so \
VGPU_CONFIG_SESSION_BASE=/etc/vgpu-manager/remote-sessions \
./lupine_driver_server
```

`LUPINE_CHECKPOINT_LIBRARY` must be set explicitly since the artifact is not
named `liblupinecr.so`. Nothing here changes local behaviour: with none of these
set the library resolves every path to its historical location and no session
code runs.

`vgpu-session-config` writes a session quota the way the GPU-node agent would,
so the paths can be exercised without Kubernetes:

```
vgpu-session-config --session <LUPINE_SESSION> --device GPU-xxxx,mem=8192,core=50
```

> Remote testing must use a GPU-less client or `LUPINE_DISABLE_LOCAL=1`.
> Otherwise lupine-client routes device 0 to a local GPU and the server-side
> library never sees the allocation.

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

CUDA 13.x and before are supporteds

Any architecture of GPU after Kepler are supported