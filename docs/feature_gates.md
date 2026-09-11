## Describe

vgpu-manager ships a number of optional behaviours behind feature gates. This document lists every
gate, which component it belongs to, its default value, and how to turn it on.

## Two independent gate registries

This is the most common source of confusion: vgpu-manager has **two separate feature gate
registries**, and a gate name is only valid in its own registry. Passing a gate to a component that
does not know it is a fatal error — the process exits at flag-parse time with
`unrecognized feature gate: <name>` rather than ignoring it.

| Registry | Components | Where it is defined |
| --- | --- | --- |
| Core | `device-plugin`, `scheduler-extender`, `device-monitor` | [pkg/util/consts.go](../pkg/util/consts.go) |
| DRA driver | `kubelet-plugin` | [pkg/kubeletplugin/featuregates](../pkg/kubeletplugin/featuregates/featuregates.go) |

Two names appear in both registries — `SharedSMUtilizationWatcher` and `DevicePluginClientMode` —
because both halves of the project implement the same behaviour. They are still registered
separately, so each component needs its own copy of the setting.

`device-monitor` deserves a special note: it is deployed as a sidecar in both the device-plugin
DaemonSet and the DRA kubelet-plugin DaemonSet, but in **both** cases it uses the Core registry and
only understands the two gates listed for it below. Handing it the kubelet-plugin's gates will stop
the container from starting.

## How to set

Core components take a `--feature-gates` flag:

```
--feature-gates=SharedSMUtilizationWatcher=true,VirtualMemoryTracking=true
```

The DRA kubelet-plugin is configured by the chart through the `FEATURE_GATES` environment variable
(the flag itself comes from the upstream `sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags` package):

```yaml
- name: FEATURE_GATES
  value: "VGPUSupport=true,NVMLDeviceHealthCheck=true"
```

With Helm, set the corresponding map instead of building the string by hand:

| Component | Chart | Values path |
| --- | --- | --- |
| device-plugin | `vgpu-manager` | `devicePlugin.devicePlugin.commands.featureGates` |
| device-monitor | `vgpu-manager` | `devicePlugin.deviceMonitor.commands.featureGates` |
| scheduler-extender | `vgpu-manager` | `scheduler.schedulerExtender.commands.featureGates` |
| kubelet-plugin | `vgpu-manager-dra-driver` | `kubeletPlugin.containers.plugin.featureGates` |
| device-monitor | `vgpu-manager-dra-driver` | `kubeletPlugin.containers.monitor.featureGates` |

```yaml
devicePlugin:
  devicePlugin:
    commands:
      featureGates:
        TopologyAwareGPUAllocation: true
        SharedSMUtilizationWatcher: true
```

## Reference

### device-plugin

| Gate | Default | Stage |
| --- | --- | --- |
| `GPUCoreResourcePlugin` | `false` | Alpha |
| `GPUMemoryResourcePlugin` | `false` | Alpha |
| `AllocationFailureReschedule` | `false` | Alpha |
| `TopologyAwareGPUAllocation` | `false` | Alpha |
| `SharedSMUtilizationWatcher` | `false` | Alpha |
| `VirtualMemoryTracking` | `false` | Alpha |
| `DevicePluginClientMode` | `false` | Alpha |
| `HonorPreAllocatedDeviceIDs` | `false` | Alpha |

### scheduler-extender

| Gate | Default | Stage |
| --- | --- | --- |
| `SerializedNodeBind` | `true` | Beta |
| `SerializedNodeFilter` | `true` | Beta |
| `TopologyAwareGPUAllocation` | `false` | Alpha |

### device-monitor

| Gate | Default | Stage |
| --- | --- | --- |
| `SharedSMUtilizationWatcher` | `false` | Alpha |
| `VirtualMemoryTracking` | `false` | Alpha |

### kubelet-plugin (DRA driver)

| Gate | Default | Stage |
| --- | --- | --- |
| `VGPUSupport` | `true` | Alpha |
| `NVMLDeviceHealthCheck` | `false` | Alpha |
| `IMEXDaemonsWithDNSNames` | `false` | Beta |
| `TimeSlicingSettings` | `false` | Alpha |
| `MPSSupport` | `false` | Alpha |
| `PassthroughSupport` | `false` | Alpha |
| `DynamicMIG` | `false` | Alpha |
| `DeviceMetadata` | `false` | Alpha |
| `SharedSMUtilizationWatcher` | `false` | Alpha |
| `DevicePluginClientMode` | `false` | Alpha |
| `NRISupport` | `false` | Alpha |
| `FabricManagerPartitioning` | `false` | Alpha |
| `DRAListTypeAttributes` | `false` | Alpha |

## Dependencies and mutual exclusions

The kubelet-plugin validates its gate combination at startup and refuses to start on a conflict.
The rules it enforces:

**Requires**

* `SharedSMUtilizationWatcher` → `VGPUSupport`
* `DevicePluginClientMode` → `VGPUSupport`
* `NRISupport` → `VGPUSupport`
* `DeviceMetadata` → `PassthroughSupport`

**Mutually exclusive**

* `VGPUSupport` ⊗ `MPSSupport`
* `VGPUSupport` ⊗ `PassthroughSupport`
* `VGPUSupport` ⊗ `IMEXDaemonsWithDNSNames`
* `DynamicMIG` ⊗ `PassthroughSupport`
* `DynamicMIG` ⊗ `NVMLDeviceHealthCheck`
* `DynamicMIG` ⊗ `MPSSupport`
* `PassthroughSupport` ⊗ `NVMLDeviceHealthCheck`

> Note: `DeviceMetadata` requires `PassthroughSupport`, which is mutually exclusive with
> `VGPUSupport`. Since `VGPUSupport` is on by default, enabling `DeviceMetadata` also means turning
> `VGPUSupport` off.

## Gate details

### GPUCoreResourcePlugin

* action scope: device-plugin

Opening the core plugin will report the number of virtual cores to the kubelet node.

Use the command `--feature-gates=GPUCoreResourcePlugin=true` to open the feature.

After opening the feature gate, check the status of the corresponding node to see the registered
resource name `nvidia.com/vgpu-cores`.

```yaml
status:
  allocatable:
    nvidia.com/vgpu-cores: "200"
  capacity:
    nvidia.com/vgpu-cores: "200"
```

> Tips: It may be useful in scenarios where node resource constraints such as `ResourceQuota` are required.

### GPUMemoryResourcePlugin

* action scope: device-plugin

Opening the memory plugin will report virtual memory to the kubelet node.

Use the command `--feature-gates=GPUMemoryResourcePlugin=true` to open the feature.

After opening the feature gate, check the status of the corresponding node to see the registered
resource name `nvidia.com/vgpu-memory`.

```yaml
status:
  allocatable:
    nvidia.com/vgpu-memory: "8192"
  capacity:
    nvidia.com/vgpu-memory: "8192"
```

> Tips: It may be useful in scenarios where node resource constraints such as `ResourceQuota` are required.

### AllocationFailureReschedule

* action scope: device-plugin

Opening the AllocationFailureReschedule will rearrange nodes and devices for certain pods that have
failed allocation.

Use the command `--feature-gates=AllocationFailureReschedule=true` to open the feature.

> Tips: In scenarios where multiple Pods are created and scheduled in parallel, device plugins may
> experience allocation errors. Enabling this feature can restore the erroneous Pods.

### TopologyAwareGPUAllocation

* action scope: scheduler-extender, device-plugin

Opening the GPU topology through the device plugin will reveal GPU topology information to the nodes.

When the scheduler opens the GPU topology, it will affect the device allocation of Pods in link
topology mode. `nvidia.com/device-topology-mode: link`

Use the command `--feature-gates=TopologyAwareGPUAllocation=true` to open the feature.

Both components must have it enabled: the device-plugin publishes the topology, the scheduler
consumes it. See [how_to_use_gpu_topology.md](./how_to_use_gpu_topology.md).

### SerializedNodeBind

* action scope: scheduler-extender

Enable serial binding of nodes to the scheduler, this will reduce the performance of the scheduler,
but it will increase the success rate of device allocation.

Use the command `--feature-gates=SerializedNodeBind=true` to open the feature.

### SerializedNodeFilter

* action scope: scheduler-extender

Serializes the filter step so concurrent scheduling cycles cannot act on inconsistent views of a
node's remaining device resources. Like `SerializedNodeBind` this trades scheduler throughput for
allocation accuracy, and is enabled by default.

Use the command `--feature-gates=SerializedNodeFilter=false` to turn it off.

### SharedSMUtilizationWatcher

* action scope: device-plugin, kubelet-plugin, device-monitor

Runs a single shared watcher that samples per-process SM utilization for every GPU on the node and
publishes it to a shared memory region, instead of every container starting its own watcher. This
keeps the cost of `nvmlDeviceGetProcessUtilization` at O(1) per device rather than O(N) per container.

Use the command `--feature-gates=SharedSMUtilizationWatcher=true` to open the feature.

> Note: enabling it on `device-monitor` alone does nothing useful — the monitor only *reads* the
> published samples. The producing side (`device-plugin`, or `kubelet-plugin` on the DRA path) must
> have the gate enabled too, otherwise the shared file never appears and container core utilization
> metrics stay at zero.

See [sm_multiproc_shared_bucket_design.md](./sm_multiproc_shared_bucket_design.md).

### VirtualMemoryTracking

* action scope: device-plugin, device-monitor

Tracks virtual (unified/oversubscribed) device memory allocation through local records, so memory
usage reporting accounts for memory that has been handed out beyond physical VRAM.

Use the command `--feature-gates=VirtualMemoryTracking=true` to open the feature.

> Note: on the DRA path `device-monitor` cannot reach the per-container virtual memory ledger yet,
> so `container_vgpu_device_memory_usage_in_bytes` equals
> `container_vgpu_device_physical_memory_usage_in_bytes` there regardless of this gate.

See [how_to_use_gpu_virtual_memory.md](./how_to_use_gpu_virtual_memory.md).

### DevicePluginClientMode

* action scope: device-plugin, kubelet-plugin

Allocated containers register themselves with the device plugin over a Unix gRPC socket instead of
relying on the plugin to discover them. Useful when container start-up ordering makes discovery
unreliable.

Use the command `--feature-gates=DevicePluginClientMode=true` to open the feature.

### HonorPreAllocatedDeviceIDs

* action scope: device-plugin

Makes the plugin's preferred-allocation response follow the device IDs the scheduler already picked
whenever possible, instead of choosing freely among the available IDs.

Use the command `--feature-gates=HonorPreAllocatedDeviceIDs=true` to open the feature.

### VGPUSupport

* action scope: kubelet-plugin

Mounts `libvgpu-control.so` into containers so a physical GPU can be shared by several claims. This
is what makes the DRA driver hand out *virtual* GPUs rather than whole ones, and it is on by default.

### NVMLDeviceHealthCheck

* action scope: kubelet-plugin

Watches NVML events (Xid, ECC) and applies DRA device taints to unhealthy devices so the scheduler
stops placing new claims on them. On by default.

### NRISupport

* action scope: kubelet-plugin

Moves per-container partition directory mounts from DRA Prepare/CDI to the NRI `CreateContainer`
hook. Requires `VGPUSupport`, and requires the NRI socket directory to be mounted into the plugin.

See [dra_nri_integration_design.md](./dra_nri_integration_design.md).

### DynamicMIG

* action scope: kubelet-plugin

Enables dynamic MIG device management: MIG partitions are created and destroyed to match claims
instead of being pre-configured on the node.

### PassthroughSupport

* action scope: kubelet-plugin

Allows GPUs to be bound to the `vfio-pci` driver for full passthrough into VMs. Requires extra host
mounts (host root, `/lib/modules`, sysfs, procfs), which the chart adds automatically when the gate
is on.

### FabricManagerPartitioning

* action scope: kubelet-plugin

Enables Fabric Manager (NVSwitch) partition management for full-GPU and VFIO devices. Prepare
activates the FM partition whose member set exactly matches the claim's allocated GPUs, and fails if
no partition matches. Requires Fabric Manager running with `FABRIC_MODE=1`.

### DeviceMetadata

* action scope: kubelet-plugin

Generates device metadata files inside the workload for prepared devices (KEP-5304 downward API).
Requires `PassthroughSupport`.

### DRAListTypeAttributes

* action scope: kubelet-plugin

Publishes list-valued DRA device attributes. The Kubernetes cluster must have the feature gate of
the same name enabled first, otherwise the API server rejects the ResourceSlice.

### TimeSlicingSettings / MPSSupport / IMEXDaemonsWithDNSNames

* action scope: kubelet-plugin

Inherited from the upstream NVIDIA DRA driver: customizable time-slicing settings, MPS
(Multi-Process Service) support, and using DNS names instead of raw IPs for IMEX daemons.
`MPSSupport` is mutually exclusive with `VGPUSupport`.

## Renamed gates

The Core registry gate names were made self-describing. The old names are **no longer accepted** —
a component started with one exits immediately with `unrecognized feature gate`. Update any custom
Helm values or manifests before upgrading.

| Old name | Current name |
| --- | --- |
| `CorePlugin` | `GPUCoreResourcePlugin` |
| `MemoryPlugin` | `GPUMemoryResourcePlugin` |
| `Reschedule` | `AllocationFailureReschedule` |
| `GPUTopology` | `TopologyAwareGPUAllocation` |
| `SMWatcher` | `SharedSMUtilizationWatcher` |
| `SerialBindNode` | `SerializedNodeBind` |
| `SerialFilterNode` | `SerializedNodeFilter` |
| `VMemoryNode` | `VirtualMemoryTracking` |
| `ClientMode` | `DevicePluginClientMode` |

The DRA driver gate names were not affected.
