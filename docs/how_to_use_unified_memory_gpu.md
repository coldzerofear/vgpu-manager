## Describe

Some NVIDIA GPUs have no framebuffer of their own: the GPU and the CPU share one
physical memory pool. NVIDIA's integrated parts work this way, the GB10 Superchip
(DGX Spark) being the current example. On such a device `nvmlDeviceGetMemoryInfo`
answers `Not Supported`, `nvidia-smi` prints `Memory-Usage: Not Supported`, and
nothing on the device can be asked how large it is - the number has to come from
the operator.

Without that number a GPU still registers, but with 0 memory:

- a pod that asks for `nvidia.com/vgpu-memory` is never scheduled on it (the node
  reports no memory to give);
- a pod that asks for none takes the whole device and runs **without memory
  isolation**, so several pods on one GPU can exhaust it;
- the device-level memory metrics read 0.

## Usage

Set the device memory size in MiB. It applies **only** to devices that cannot
report a size; a device that reports its own always wins, so the setting is safe
to leave in place on a mixed cluster.

Every component that looks at a device must agree about its size, so the value
goes on each of them: the device plugin and the device monitor on the classic
path, the kubelet plugin and the device monitor on the DRA path.

On the classic path the node configuration covers both containers at once (they
read the same file, see
[how_to_use_deviceplugin_nodeconfig.md](how_to_use_deviceplugin_nodeconfig.md)).
The DRA kubelet plugin does not read the node configuration, so there the flag
(or the chart value, which renders onto both containers) is the only route.

Node configuration (classic path only):

```yaml
version: v1
configs:
  - nodeName: dgx-spark-01
    deviceMemoryOverride: 65536   # MiB. Conservative: the GPU shares this pool with the host.
    deviceMemoryScaling: 1        # must stay 1, see below
```

Flags, with the Helm chart:

```shell
# classic (device plugin + device monitor)
helm upgrade --install vgpu-manager ./charts/vgpu-manager \
  --set devicePlugin.deviceMemoryOverride=65536

# DRA (kubelet plugin + device monitor)
helm upgrade --install vgpu-manager-dra ./charts/vgpu-manager-dra-driver \
  --set kubeletPlugin.deviceMemoryOverride=65536
```

Flags, editing a DaemonSet directly (`--device-memory-override`, or the
`DEVICE_MEMORY_OVERRIDE` environment variable, on every `device-plugin`,
`kubelet-plugin` and `device-monitor` container):

```yaml
  containers:
  - name: device-plugin
    command:
    - device-plugin
    - --device-memory-override=65536
```

The plugin logs which of the three states each device ended up in:

```
device 0 has no memory of its own (unified memory architecture), using the configured override of 65536 MiB
device 0 has no memory of its own (unified memory architecture) and no memory override is configured: ...
device 0 reports 24576 MiB of its own memory, ignoring the configured memory override of 65536 MiB
```

## Memory oversold is refused

`deviceMemoryScaling > 1` (classic) and `--device-memory-ratio > 100` (DRA) are
rejected at startup when the override is set. Oversold memory works by spilling
into host memory - and here the "device memory" already *is* host memory. There
is nothing to spill into, so overselling hands out memory the host also needs and
takes the node down instead of failing an allocation.

For the same reason the override is a bookkeeping ceiling, not a physical
guarantee: CPU-side allocations draw from the same pool. Leave headroom for the
host (64 GiB of a 128 GiB machine, say) rather than configuring the full size.

## What the metrics show

- `physical_gpu_device_total_memory_in_bytes` is the configured override.
- `physical_gpu_device_memory_usage_in_bytes` is the sum of the per-process memory
  the driver reports (NVML still lists that on these parts, which is what
  `nvidia-smi --query-compute-apps=used_memory` reads), capped at the override.
  The derived utilization rate is capped at 100%.
- The vGPU and per-pod metrics come from the node and pod annotations and from
  the library's own accounting, so they are exact on these devices as well.

## DRA claims

On a device whose size is unknown, the published memory capacity is 0, so a claim
must not request memory - a request of any size cannot be satisfied and the pod
stays pending. Configure the override, or write claims that only request cores.
