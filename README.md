# VGPU-Manager

A k8s device plugin for managing and allocating vGPU devices.

Support multi container and multi GPU virtualization allocation and rich scheduling strategies.

## Project objectives:

The project forks based on [gpu-manager](https://github.com/tkestack/gpu-manager) and has undergone multiple improvements.

- [x] Efficient scheduling performance
- [x] Ensure the security of container resource isolation
- [x] Simplify GRPC within containers
- [x] Support CUDA 12.x version drivers
- [x] Support CGroupv1 and CGroupv2
- [x] Dual scheduling strategy for nodes and devices
- [x] Provide GPU monitoring indicators
- [x] Idle computing power of dynamic balancing equipment
- [x] GPU device uses virtual memory after exceeding memory limit
- [x] Rescheduling device allocation failed pod
- [ ] Webhook dynamic admission, fixing some non-standard pod configurations
- [ ] Compatible with hot swappable devices and expansion capabilities
- [ ] Compatible with Volcano Batch Scheduler

> Note: Checking indicates that the function has been completed, while unchecking indicates that the function has not been completed or is planned to be implemented.

## Prerequisite

* Kubernetes v1.23+ (other version not tested)
* Docker/Containerd (other version not tested)
* Nvidia Container Toolkit (Configure nvidia container runtime)

## Build

* compile binary
```shell
make build
```
> Note: After the program compilation is completed, three binary files will be generated in the /bin directory

* build docker image and push it
```shell
make docker-build docker-push IMG=<tag>
```

## Deploy

precondition: `nvidia-container-toolkit` must be installed and correctly configure the default container runtime

Label the node where the device plugin will be deployed: `vgpu-manager-enable=enable`

```shell
kubectl label node <nodename> vgpu-manager-enable=enable
```

### Deploy directly using YAML files

```bash
kubectl apply -f deploy/vgpu-manager-scheduler.yaml
kubectl apply -f deploy/vgpu-manager-deviceplugin.yaml
```

Note that the scheduler version needs to be modified according to the cluster version, Only supports scheduler versions v1.25.x and above.

```yaml
      containers:
        - image: registry.cn-hangzhou.aliyuncs.com/google_containers/kube-scheduler:v1.28.15
          imagePullPolicy: IfNotPresent
          name: scheduler
```

### Helm charts deployment

Modify the configuration in values.yaml according to the node environment and requirements

Use the following command for deployment

```shell
helm install vgpu-manager ./helm/ -n kube-system
```

Verify the installation of `vgpu-manager-device-plugin` and `vgpu-manager-scheduler`

```shell
kubectl get pods -n kube-system
```

## Uninstall

### Uninstall directly according to YAML

```shell
kubectl delete -f deploy/vgpu-manager-scheduler.yaml
kubectl delete -f deploy/vgpu-manager-deviceplugin.yaml
```

### Helm charts uninstallation

```shell
helm uninstall vgpu-manager -n kube-system 
```

## Example of use

Submit a VGPU container application with 10% computing power and 1GB of memory

> Note: VGPU pod requires specifying the scheduler name and the number of vGPU devices to be requested by the container.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-pod
  namespace: default
spec:    
  schedulerName: vgpu-scheduler  # Specify scheduler (default: vgpu-manager)
  terminationGracePeriodSeconds: 0
  containers:
  - name: default
    image: nvidia/cuda:12.4.1-devel-ubuntu20.04
    command: ["sleep", "9999999"]
    resources:
      requests:
        cpu: 1
        memory: 2Gi
      limits:
        cpu: 2
        memory: 4Gi
        nvidia.com/vgpu-number: 1     # Allocate one gpu
        nvidia.com/vgpu-core: 10      # Allocate 10% of computing power
        nvidia.com/vgpu-memory: 1024  # Allocate memory (default: Mib)
```

Check that the container meets expectations

```bash
root@gpu-pod1:/# nvidia-smi 
[vGPU INFO(34|loader.c|1043)]: loaded nvml libraries
[vGPU INFO(34|loader.c|1171)]: loaded cuda libraries
Sun Dec 22 19:20:47 2024       
+---------------------------------------------------------------------------------------+
| NVIDIA-SMI 535.183.01             Driver Version: 535.183.01   CUDA Version: 12.2     |
|-----------------------------------------+----------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id        Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |         Memory-Usage | GPU-Util  Compute M. |
|                                         |                      |               MIG M. |
|=========================================+======================+======================|
|   0  NVIDIA GeForce GTX 1050 Ti     Off | 00000000:01:00.0 Off |                  N/A |
| N/A   42C    P8              N/A / ERR! |      0MiB /  1024MiB |      0%      Default |
|                                         |                      |                  N/A |
+-----------------------------------------+----------------------+----------------------+
                                                                                         
+---------------------------------------------------------------------------------------+
| Processes:                                                                            |
|  GPU   GI   CI        PID   Type   Process name                            GPU Memory |
|        ID   ID                                                             Usage      |
|=======================================================================================|
+---------------------------------------------------------------------------------------+
```

## Scheduling Strategy 

Support scheduling strategies for both node and device dimensions

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

## Compute Strategy

Support the use of annotations on nodes or pods to configure the computing strategy to be used: `nvidia.com/vgpu-compute-policy`

Supported policy values:

* `fixed`: Fixed GPU core limit to ensure that task core utilization does not exceed the limit (Default strategy)
* `balance`: Allow tasks to run beyond the limit when there are still remaining resources on the GPU, improving the overall core utilization of the GPU
* `none`: No core restriction effect, competing for computing power on its own

> Note: If policies are configured on both Node and Pod, the configuration on Pod takes priority; otherwise, the policy on Node is used.

## GPU Memory Overslod (virtual memory)

When the physical memory of the GPU reaches its limit, allowing the allocation of virtual memory to achieve the effect of memory oversold.

Add the environment variable `CUDA_MEM_OVERSOLD` to the container configuration to enable this feature.

Example pod:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-example
  namespace: default
spec:
  schedulerName: vgpu-scheduler
  containers:
  - name: default
    image: quay.io/jitesoft/ubuntu:24.04
    command: ["sleep", "9999999"]
    env:
    - name: CUDA_MEM_OVERSOLD # Add environment variables to the container
      value: "true"
    resources:
      limits:
        cpu: 2
        memory: 2Gi
        nvidia.com/vgpu-number: 1
```

> Note: Only in the above example, defining environment variables in the env of the container is valid.

## Feature Gate

The device plugin of vgpu-manager has implemented some special functions that require adding the command-line parameter `--feature-gates` to enable.

### CorePlugin

Opening the core plugin will report the number of virtual cores to the kubelet node.

Use the command `--feature-gates=CorePlugin=true` to open the feature.

After opening the feature gate, check the status of the corresponding node to see the registered resource name `nvidia.com/vgpu-core`.

```yaml
status:
  allocatable:
    nvidia.com/vgpu-core: "200"
  capacity:
    nvidia.com/vgpu-core: "200"
```

> Tips: It may be useful in scenarios where node resource constraints such as `ResourceQuota` are required.

### MemoryPlugin

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

Opening the reschedule will rearrange nodes and devices for certain pods that have failed allocation.

Use the command `--feature-gates=Reschedule=true` to open the feature.

> Tips: In scenarios where multiple Pods are created and scheduled in parallel, device plugins may experience allocation errors. 
> Enabling this feature can restore the erroneous Pods.
