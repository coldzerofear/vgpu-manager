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
| `TopologyDeviceAttributes` | `false` | Alpha |
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

`--plugin-mode=inject` additionally refuses `SharedSMUtilizationWatcher` and
`TopologyDeviceAttributes`: an inject process publishes no devices.

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

Enables Fabric Manager (NVSwitch) partition management. Requires Fabric Manager running with
`FABRIC_MODE=1`. What it does depends on whether `VGPUSupport` is on:

* **`VGPUSupport` off** (full-GPU and VFIO devices): Prepare activates the FM partition whose member
  set exactly matches the claim's allocated GPUs, and fails if no partition matches. Unprepare
  releases it.
* **`VGPUSupport` on**: the node activates one partition covering all of its GPUs at startup, and
  Prepare/Unprepare never touch Fabric Manager. Per-claim partitioning cannot work here — vGPU
  devices allow multiple allocations, so claims hold overlapping rather than identical GPU sets and
  FM refuses to activate an overlapping partition. Fabric isolation between claims is given up,
  which vGPU cannot provide anyway since two claims may share one physical GPU. A node whose GPU
  count is not a supported partition size logs a warning and leaves the fabric untouched; the
  `partitionN` attributes are still published either way. If Fabric Manager refuses the activation,
  the plugin fails to start and is retried by its restart backoff. A refusal that persists is
  nearly always an overlapping partition that is still active — for example one left by per-claim
  partitioning before `VGPUSupport` was turned on — and has to be deactivated with Fabric Manager.
  The node partition is never deactivated, so a node that ran this combination keeps it active until
  it reboots.

### TopologyDeviceAttributes

* action scope: kubelet-plugin

Publishes each device's interconnect as three attributes:

* `clique` — the hardware fabric identity (`clique:<clusterUUID>.<cliqueID>`, from NVML). On MNNVL
  systems one clique spans several nodes.
* `nvlinkDomain` — equal for the GPUs of one NVLink-connected component **on this node**. Always
  node-local, also on MNNVL: a claim can draw devices from several nodes (remote-GPU pools), and a
  plain claim cannot use a cross-node NVLink path. The fabric identity is still used — it keeps two
  independent NVSwitch fabrics in one chassis apart — but it never becomes the key. Match on
  `clique` instead when cross-node fabric membership is what a claim means. Works on NVLink hardware
  without NVSwitch too.
* `pcieDomain` — equal for the GPUs of one component connected by peer-to-peer DMA **without
  crossing a PCIe host bridge**. Always node-local. The tiers nest, so NVLink links count as well
  and a GPU normally carries both domains.

Both domains are connected components, so two GPUs share one when a chain of qualifying links joins
them. Neither says anything about NUMA: NVLink crosses CPU sockets, so one `nvlinkDomain` or
`pcieDomain` routinely spans two NUMA nodes.

A GPU with no peer at a level publishes no attribute for that level, so a `matchAttribute`
constraint correctly refuses it. The gate never stops the plugin from starting: a single-GPU node or
one without NVLink simply publishes fewer attributes, a GPU pair whose link cannot be read is treated
as unconnected, and if the topology cannot be read at all the node publishes none — each logged as a
warning.

`pcieDomain` is deliberately not the standard `resource.kubernetes.io/pcieRoot`: a root complex also
groups GPUs that have to cross the host bridge to talk. `pcieRoot` stays untouched and is the right
key for hand-written ResourceClaims that need one topology key shared with another DRA driver — for
example pinning GPUs and CPUs to the same root complex together with
[dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu).

The webhook turns a multi-GPU request of a pod with a topology mode into `matchAttribute`
constraints on these attributes:

| Mode | `-strict` | Non-strict (`firstAvailable`, tried in order) |
|---|---|---|
| `link` | `nvlinkDomain` | `nvlink` (`nvlinkDomain`) → `pcie` (`pcieDomain`) → `numa` (`numaNode`) → `any` |
| `pcie` | `pcieDomain` | `pcie` (`pcieDomain`) → `numa` (`numaNode`) → `any` |
| `numa` | `numaNode` | `numa` (`numaNode`) → `any` |

`-strict` keeps one exact request pinned to its tier, so a node that cannot offer it is refused (a
strict single-GPU request is pinned too, so its card must carry that tier's attribute) — and
a node without this gate can never offer `nvlinkDomain` or `pcieDomain`, so **enable this gate on
every node that should satisfy `link-strict` or `pcie-strict`**. A non-strict mode becomes a
`firstAvailable` list, one subrequest per tier plus an unconstrained `any`: the allocator takes the
first one a node can satisfy, and the scheduler scores a node higher the earlier that subrequest is,
so the tightest available tier wins without ever leaving the pod unschedulable. The subrequest
names show up in the allocation result (`<request>/pcie`), which tells which tier a request got.
This needs the Kubernetes `DRAPrioritizedList` feature, on by default since 1.34 and locked on since
1.37. Pods without a topology mode are left as one exact, unconstrained request.

For remote GPUs (`nvidia.com/vgpu-access-mode: remote`) a request with a topology mode is also kept
on one GPU server, through the `nodeName` attribute remote devices carry: topology is not defined
across nodes. Without a topology mode a request may span servers.

**Limit with `--combined-resource-claim`.** Kubernetes allows at most 32 constraints per
ResourceClaim, and each multi-GPU request takes one per tier above, one for uuid exclusivity, and
one more for remote access. Without the flag every container gets its own claim and never comes
near the limit; with it, all of a pod's containers share one claim, which caps how many multi-GPU
containers a pod (or a Volcano task) can have:

| Topology mode | Constraints per multi-GPU container (local / remote) | Max multi-GPU containers (local / remote) |
|---|---|---|
| `link` | 4 / 5 | 8 / 6 |
| `pcie` | 3 / 4 | 10 / 8 |
| `numa` | 2 / 3 | 16 / 10 |
| any `-strict` mode | 2 / 3 | 16 / 10 |
| none | 1 / 1 | 32 / 32 |

Single-GPU containers take none, or one under a `-strict` mode. A workload beyond its limit is
refused at admission with an error saying so; the webhook does not drop tiers to make it fit, since
what a topology mode guarantees would then depend on how many containers share the claim. Use fewer
multi-GPU containers, a `-strict` mode, or turn the flag off. Well inside the limit, many multi-GPU
containers in one claim still multiply the combinations the scheduler has to try (one ladder per
container), so a pod with many of them schedules faster with a `-strict` mode or without the flag.

**How this compares with the scheduler-extender path.** The connectivity thresholds are the same, so
`link` means NVLink and `pcie` means the PCIe switch tier on both paths, but the grouping is not
identical. The domains are components of the whole node, while the extender checks connectivity
among the cards still free; on a board without full NVLink connectivity (for example a hybrid
cube-mesh) a DRA request can be given two GPUs of one component that have no direct link. The NUMA
tier also differs in source: `numaNode` here, the CPU-affinity link type on the extender path, which
can disagree with sub-NUMA clustering on.

It reads fabric identities from NVML and never talks to Fabric Manager. The topology is read once,
at plugin start, so it reflects the NVLinks that are up at that moment: with Fabric Manager in
`FABRIC_MODE=1`, links may not be trained until a partition is activated, and a node using
per-claim partitions (`FabricManagerPartitioning` without `VGPUSupport`) may then publish no
`nvlinkDomain` at all. Under `VGPUSupport` the node partition is activated before the topology is
read.

**Upgrading.** These changes reach pods through the webhook, which relies on what the kubelet
plugins publish, so upgrade the kubelet plugins (with this gate) before the webhook:

* A non-strict mode used to be one hard constraint (`link` on `pcieRoot`, `numa` on `numaNode`) and
  is now the fallback ladder above, ending unconstrained.
* `link-strict` used to match `pcieRoot`, which every node publishes; it now needs `nvlinkDomain`, so
  nodes without this gate stop satisfying it.
* A remote request with a topology mode needs the GPU servers to publish `nodeName`, which they do
  from this release on.
* The shipped manifests and chart now default the webhook to `numa`, so remote multi-GPU containers
  stay on one GPU server unless the pod sets `nvidia.com/device-topology-mode: none`.
* With `FabricManagerPartitioning` and `VGPUSupport` both on, the plugin now activates the node
  partition at startup and fails to start if Fabric Manager refuses it (see above).

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
