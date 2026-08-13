# VGPU-Remote-Adapter

A remote vGPU adapter for implementing network-based remote vGPU hard isolation

## Project objectives:

- [x] Suitable for use with Lupine Server
- [x] Remote container GPU device quota configuration discovery
- [x] Remote container device PID list discovery/maintenance
- [x] Remote container device visibility isolation
- [x] Container level multi process shared token bucket (forced on in a session)
- [ ] Remote container level memory strict isolation (implemented, pending on-GPU validation)
- [ ] Remote container level SM core strict isolation (implemented, pending on-GPU validation)

> Note: Checking indicates that the function has been completed, while unchecking indicates that the function has not been completed or is planned to be implemented.

## Building a dynamic link library

```
./build.sh
```

## Development targets

```
make check          # static checks (no GPU, no CUDA toolkit)
make check-exports  # export-surface check on the built .so
make test-nogpu     # session path derivation + config region round-trip
make session-cli    # build vgpu-session-config
```

`vgpu-session-config` writes a session quota region the way the GPU-node agent
would, so the library can be exercised without Kubernetes:

```
vgpu-session-config --session <LUPINE_SESSION> \
    --device GPU-xxxx,mem=8192,core=50 \
    --device GPU-yyyy,mem=4096
```

## Deployment (lupine-server, GPU node)

```
LD_PRELOAD=/opt/vgpu/lib/libvgpu-remote.so \
VGPU_REMOTE_MODE=1 \
LUPINE_CHECKPOINT_LIBRARY=/opt/vgpu/lib/libvgpu-remote.so \
VGPU_CONFIG_SESSION_BASE=/etc/vgpu-manager/remote-sessions \
./lupine_driver_server
```

The same `.so` acts as both the LD_PRELOAD hook library and lupine's checkpoint
provider. `LUPINE_CHECKPOINT_LIBRARY` must be set explicitly since the artifact
is not named `liblupinecr.so`. In remote mode a connection without a valid
session quota is refused rather than served unrestricted.

> Remote testing must use a GPU-less client or `LUPINE_DISABLE_LOCAL=1`.
> Otherwise lupine-client routes device 0 to a local GPU and the server-side
> library never sees the allocation.

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
* CUDA_SM_SHARED_BUCKET: Enable SM shared token bucket. Opt-in locally, forced on in a session -- one container is
  served by several lupine-server children, and a per-process bucket would give each of them the full core quota
* VGPU_CONFIG_SESSION_BASE: Directory holding session directories (default /etc/vgpu-manager/remote-sessions)
* VGPU_REMOTE_MODE: Mark the process as serving remote sessions only (fail-closed without a session quota)
* VGPU_CONFIG_SESSION_PATH: This session's directory. Set by the checkpoint provider per connection, not by hand;
  every per-session path (quota, pids.config, .vgpu_lock, .vmem_node, .sm_node) is derived from it

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
