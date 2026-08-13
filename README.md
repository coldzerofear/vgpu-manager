# VGPU-Manager

A Kubernetes device plugin for managing and allocating virtual GPU (vGPU) devices. Supports multi-container and multi-GPU virtualization with advanced scheduling strategies.

## Project objectives

- [x] Ensure the correctness of scheduling performance and device allocation
- [x] Ensure the security of container resource isolation
- [x] Support the latest CUDA 13.x driver version
- [x] Compatible with both cgroupv1 and cgroupv2 container environments
- [x] Dual-layer scheduling policies (node-level and device-level)
- [x] Provide multi-dimensional vGPU monitoring metrics
- [x] Idle computing power of dynamic balancing equipment
- [x] GPU device uses virtual memory after exceeding memory limit
- [x] Automatic rescheduling of pods with failed device allocations
- [x] Webhook dynamic admission, fixing some non-standard pod configurations
- [x] Provide the optimal topology allocation for NUMA and NVLink
- [x] Compatible with open-gpu-kernel-modules
- [x] Support MIG strategy device allocation
- [x] Add an independent core utilization Watcher to avoid frequent driver calls
- [x] Support gpu registration mode, reduce the exposed host information, and provide a safer gpu container environment
- [x] Support dynamic resource allocation (DRA)
- [x] NRI supported DRA multi container configuration path isolation security
- [x] Device resource monitoring under the DRA driver path
- [x] Multi process core speed limit for shared token bucket
- [ ] Provide a scheduler framework plugin to achieve high-performance scheduling
- [ ] Support device hot plugging and expansion ([device-mounter](https://github.com/coldzerofear/device-mounter))
- [ ] Compatible with Volcano Batch Scheduler
- [ ] Remote GPU resource pooling (GPU-over-IP)

> **describe**:
> :white_check_mark: Completed feature
> :black_square_button: Planned/In-progress feature

## Prerequisite

* Kubernetes v1.18+ (Install using helm chart method)
* Container runtime (docker / containerd / cri-o - others untested)
* Nvidia Container Toolkit (with NVIDIA container runtime configured)

## Build

**Compile Binaries:**

```shell
make build
```
> Note: The compiled file is stored in the bin directory

**Build and Push container image:**

```shell
make docker-build-base docker-build docker-push REGISTRY=<your-image-registry> TAG=<your-image-tag>
```

## Installation and Uninstallation

> Currently, DRA driver based GPU allocation is supported. For installation and usage details, please refer to [how_to_use_DRA_driver.md](./docs/how_to_use_DRA_driver.md)

Label GPU nodes that require vgpu-manager management: `vgpu-manager=device-plugin`

```shell
kubectl label node <nodename> vgpu-manager=device-plugin
```

Provide two methods for installing helm charts and YAML files, and recommend the helm charts method

### Helm charts (Recommended)

**Installation:**

Modify `charts/vgpu-manager/values.yaml` according to your environment requirements

```shell
helm install vgpu-manager ./charts/vgpu-manager -n kube-system
```

Verify installation

```shell
$ kubectl get pods -n kube-system 
vgpu-manager-device-plugin-dvlll                       2/2     Running   0          10s
vgpu-manager-scheduler-6949f5d645-g57fj                2/2     Running   0          10s
vgpu-manager-webhook-854c56bb97-5f4lm                  1/1     Running   0          10s
```

**Uninstallation**

Execute the following command to uninstall

```shell
helm uninstall vgpu-manager -n kube-system 
```

### YAML files

**Installation:**

Deploy the scheduler and device plugin using the following command

```bash
kubectl apply -f deploy/vgpu-manager-scheduler.yaml
kubectl apply -f deploy/vgpu-manager-deviceplugin.yaml
```

Note that the scheduler version needs to be modified according to the cluster version, 
If the scheduler version is v1.25.x or above, you can directly modify the imageTag for use, 
otherwise you need to modify the scheduler configuration file.

```yaml
      containers:
        - image: registry.cn-hangzhou.aliyuncs.com/google_containers/kube-scheduler:<your-k8s-version>
          imagePullPolicy: IfNotPresent
          name: scheduler
```

If you want to install the webhook service component, please ensure that the cluster has installed `cert-manager`.

The Webhook service requires the use of [cert-manager](https://github.com/cert-manager/cert-manager) to generate HTTPS certificates and manage certificate renewal policies.

```bash
kubectl apply -f deploy/vgpu-manager-webhook.yaml
```

**Installation:**

```shell
kubectl delete -f deploy/vgpu-manager-scheduler.yaml
kubectl delete -f deploy/vgpu-manager-deviceplugin.yaml
kubectl delete -f deploy/vgpu-manager-webhook.yaml
```

## Example of use

Submit a VGPU container application with 10% computing power and 1GB of memory

> Note: vGPU pod requires specifying the scheduler name and the number of vGPU devices to be requested by the container.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-pod
  namespace: default
spec:    
  schedulerName: vgpu-scheduler  # Specify scheduler (default: vgpu-scheduler)
  terminationGracePeriodSeconds: 0
  containers:
  - name: default
    image: nvidia/cuda:12.4.1-devel-ubuntu20.04
    command: ["sleep", "9999999"]
    resources:
      limits:
        cpu: 2
        memory: 4Gi
        nvidia.com/vgpu-number: 1     # Allocate one gpu
        nvidia.com/vgpu-cores: 10     # Allocate 10% of computing power
        nvidia.com/vgpu-memory: 1024  # Allocate memory (default: Mib)
```

Check that the container meets expectations

```bash
root@gpu-pod1:/# nvidia-smi 
[vGPU INFO(34|loader.c|1043)]: loaded nvml libraries
[vGPU INFO(34|loader.c|1171)]: loaded cuda libraries
Mon Mar  3 03:04:34 2025       
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 570.86.10              Driver Version: 570.86.10      CUDA Version: 12.8     |
|-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|                                         |                        |               MIG M. |
|=========================================+========================+======================|
|   0  NVIDIA GeForce GTX 1050 Ti     Off |   00000000:01:00.0 Off |                  N/A |
| N/A   41C    P8             N/A / 5001W |       0MiB /   1024MiB |      0%      Default |
|                                         |                        |                  N/A |
+-----------------------------------------+------------------------+----------------------+
                                                                                         
+-----------------------------------------------------------------------------------------+
| Processes:                                                                              |
|  GPU   GI   CI              PID   Type   Process name                        GPU Memory |
|        ID   ID                                                               Usage      |
|=========================================================================================|
|  No running processes found                                                             |
+-----------------------------------------------------------------------------------------+
```

## Scheduling Policy 

Support scheduling policies for both node and device dimensions

* `binpack`: Choose the busiest nodes or devices to improve resource utilization and reduce fragmentation.
* `spread`: Select the most idle node or device to distribute tasks and isolate faults.

### Usage

Add annotations on the vGPU pod: `nvidia.com/node-scheduler-policy` or  `nvidia.com/device-scheduler-policy`

```yaml
metadata:
  annotations:
    nvidia.com/node-scheduler-policy: spread
    nvidia.com/device-scheduler-policy: binpack
```

## Select Devices

Support using annotations to select the device type and uuid to be selected for the pod.

### Device TYPE

Add annotations to vGPU pod to select or exclude device types to be scheduled: 
`nvidia.com/include-gpu-type` `nvidia.com/exclude-gpu-type`

Example: Choose to use A10 and exclude A100
```yaml
metadata:
  annotations:
    nvidia.com/include-gpu-type: "A10"  
    nvidia.com/exclude-gpu-type: "A100"
```

> Note: If there are multiple devices separated by commas

Matching rules:

* An entry matches as a case-insensitive substring of the device type, so `A100` selects `NVIDIA A100-SXM4-80GB`.
* When both annotations are present, both apply: the device must be named by the include list and must not be named by the exclude list.
* Blank entries are ignored, so `"A10,"` means the same as `"A10"`, and an annotation with no usable entry (`""`, `"  "`, `","`) is treated as if it were not set.

### Device UUID

Add annotations to vGPU pod to select or exclude device uuids to be scheduled:
`nvidia.com/include-gpu-uuid` `nvidia.com/exclude-gpu-uuid`

Example: Select a GPU uuid
```yaml
metadata:
  annotations:
    nvidia.com/include-gpu-uuid: GPU-49aa2e6a-33f3-99dd-e08b-ea4beb0e0d28
```

Example: Excluded a GPU uuid
```yaml
metadata:
  annotations:
    nvidia.com/exclude-gpu-uuid: GPU-49aa2e6a-33f3-99dd-e08b-ea4beb0e0d28
```

> Note: If there are multiple devices separated by commas

The same matching rules as [Device TYPE](#device-type) apply, including that include and exclude are both honoured when both are set.

> Changed: earlier releases stopped at `include-gpu-uuid` and ignored `exclude-gpu-uuid` whenever both annotations were present. A Pod that lists the same UUID in both now has that device rejected instead of selected.

## Compute Policy

Support the use of annotations on nodes or pods to configure the computing policy to be used: `nvidia.com/vgpu-compute-policy`

Supported policy values:

* `fixed`: Fixed GPU core limit to ensure that task core utilization does not exceed the limit (Default strategy)
* `balance`: Allow tasks to run beyond the limit when there are still remaining resources on the GPU, improving the overall core utilization of the GPU
* `none`: No core restriction effect, competing for computing power on its own

> Note: If policies are configured on both Node and Pod, the configuration on Pod takes priority; otherwise, the policy on Node is used.

## Feature Gates

Several optional behaviours are guarded by feature gates. Core components (`device-plugin`,
`scheduler-extender`, `device-monitor`) take them through `--feature-gates`, while the DRA
`kubelet-plugin` reads them from the `FEATURE_GATES` environment variable:

```
--feature-gates=TopologyAwareGPUAllocation=true,SharedSMUtilizationWatcher=true
```

> Warning: the core components and the DRA driver keep **separate** gate registries, and an
> unknown gate is fatal rather than ignored — the process exits with `unrecognized feature gate`.
> Make sure a gate is valid for the component you are passing it to.

| Component | Gates |
| --- | --- |
| device-plugin | `GPUCoreResourcePlugin`, `GPUMemoryResourcePlugin`, `AllocationFailureReschedule`, `TopologyAwareGPUAllocation`, `SharedSMUtilizationWatcher`, `VirtualMemoryTracking`, `DevicePluginClientMode`, `HonorPreAllocatedDeviceIDs` |
| scheduler-extender | `SerializedNodeBind`, `SerializedNodeFilter`, `TopologyAwareGPUAllocation` |
| device-monitor | `SharedSMUtilizationWatcher`, `VirtualMemoryTracking` |
| kubelet-plugin (DRA) | `VGPUSupport`, `NVMLDeviceHealthCheck`, `IMEXDaemonsWithDNSNames`, `TimeSlicingSettings`, `MPSSupport`, `PassthroughSupport`, `DynamicMIG`, `DeviceMetadata`, `SharedSMUtilizationWatcher`, `DevicePluginClientMode`, `NRISupport`, `FabricManagerPartitioning`, `DRAListTypeAttributes` |

For per-gate defaults, what each one does, the dependency/mutual-exclusion rules the DRA driver
enforces at startup, the Helm values paths, and the old→new name mapping for gates that were
renamed, see [feature_gates.md](./docs/feature_gates.md).
