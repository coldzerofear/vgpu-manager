# VGPU-Manager

A Kubernetes device plugin for managing and allocating virtual GPU (vGPU) devices. Supports multi-container and multi-GPU virtualization with advanced scheduling strategies.

## Project objectives

- [x] Ensure the correctness of scheduling performance and device allocation
- [x] Ensure the security of container resource isolation
- [x] Do not obtain host PID through gRPC to device plugin
- [x] Support the latest CUDA 12.x driver version
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
- [ ] Provide a scheduler framework plugin to achieve high-performance scheduling
- [ ] Support device hot plugging and expansion ([device-mounter](https://github.com/coldzerofear/device-mounter))
- [ ] Compatible with Volcano Batch Scheduler
- [ ] Support dynamic resource allocation (DRA)

> **describe**:
> :white_check_mark: Completed feature
> :black_square_button: Planned/In-progress feature

## Prerequisite

* Kubernetes v1.17+ (Install using helm chart method)
* Container runtime (docker / containerd / cri-o - others untested)
* Nvidia Container Toolkit (with NVIDIA container runtime configured)

## Build

**Compile binaries:**

```shell
make build
```
> Note: The compiled file is stored in the bin directory

**Build and push Docker image:**

```shell
make docker-build docker-push IMG=<your-image-tag>
```

## Deployment

precondition: `nvidia-container-toolkit` must be installed and correctly configure the default container runtime

Label the node where the device plugin will be deployed: `vgpu-manager-enable=enable`

```shell
kubectl label node <nodename> vgpu-manager-enable=enable
```

### Helm chart (Recommended)

Modify `values.yaml` according to your environment requirements

```shell
helm install vgpu-manager ./helm/ -n kube-system
```

Verify installation

```shell
$ kubectl get pods -n kube-system 
vgpu-manager-device-plugin-dvlll                       2/2     Running   0          10s
vgpu-manager-scheduler-6949f5d645-g57fj                2/2     Running   0          10s
vgpu-manager-webhook-854c56bb97-5f4lm                  1/1     Running   0          10s
```

### Deploy directly using YAML files

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

## Uninstall

### Helm charts uninstallation

```shell
helm uninstall vgpu-manager -n kube-system 
```

### Uninstall directly according to YAML

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

## Compute Policy

Support the use of annotations on nodes or pods to configure the computing policy to be used: `nvidia.com/vgpu-compute-policy`

Supported policy values:

* `fixed`: Fixed GPU core limit to ensure that task core utilization does not exceed the limit (Default strategy)
* `balance`: Allow tasks to run beyond the limit when there are still remaining resources on the GPU, improving the overall core utilization of the GPU
* `none`: No core restriction effect, competing for computing power on its own

> Note: If policies are configured on both Node and Pod, the configuration on Pod takes priority; otherwise, the policy on Node is used.

## Feature Gates

The device plugin of vgpu-manager has implemented some special functions that require adding the command-line parameter `--feature-gates` to enable.

### CorePlugin

* action scope: device-plugin

Opening the core plugin will report the number of virtual cores to the kubelet node.

Use the command `--feature-gates=CorePlugin=true` to open the feature.

After opening the feature gate, check the status of the corresponding node to see the registered resource name `nvidia.com/vgpu-cores`.

```yaml
status:
  allocatable:
    nvidia.com/vgpu-cores: "200"
  capacity:
    nvidia.com/vgpu-cores: "200"
```

> Tips: It may be useful in scenarios where node resource constraints such as `ResourceQuota` are required.

### MemoryPlugin

* action scope: device-plugin

Opening the memory plugin will report virtual memory to the kubelet node.

Use the command `--feature-gates=MemoryPlugin=true` to open the feature.

After opening the feature gate, check the status of the corresponding node to see the registered resource name `nvidia.com/vgpu-memory`.

```yaml
status:
  allocatable:
    nvidia.com/vgpu-memory: "8192"
  capacity:
    nvidia.com/vgpu-memory: "8192"
```

> Tips: It may be useful in scenarios where node resource constraints such as `ResourceQuota` are required.

### Reschedule

* action scope: device-plugin

Opening the reschedule will rearrange nodes and devices for certain pods that have failed allocation.

Use the command `--feature-gates=Reschedule=true` to open the feature.

> Tips: In scenarios where multiple Pods are created and scheduled in parallel, device plugins may experience allocation errors. 
> Enabling this feature can restore the erroneous Pods.

### SerialBindNode

* action scope: scheduler-extender

Enable serial binding of nodes to the scheduler, this will reduce the performance of the scheduler, but it will increase the success rate of device allocation.

Use the command `--feature-gates=SerialBindNode=true` to open the feature.

### GPUTopology

* action scope: scheduler-extender, device-plugin

Opening the GPU topology through the device plugin will reveal GPU topology information to the nodes.

When the scheduler opens the GPU topology, it will affect the device allocation of Pods in link topology mode. `nvidia.com/device-topology-mode: link`

Use the command `--feature-gates=GPUTopology=true` to open the feature.