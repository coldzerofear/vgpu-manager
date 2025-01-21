# VGPU-Manager

A k8s device plugin for managing and allocating vGPU devices.

Support multi container and multi GPU virtualization allocation and rich scheduling strategies.

## Project objectives:

The project forks based on [gpu-manager](https://github.com/tkestack/gpu-manager) and has undergone multiple improvements.

* Efficient scheduling performance
* Ensure the security of container resource isolation
* Simplify GRPC within containers
* Support CUDA 12.x version drivers
* Support CGroup v1 and v2
* Dual scheduling strategy for nodes and devices
* Provide GPU monitoring indicators
* Compatible with hot swappable devices and expansion capabilities

The project is currently underway...

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

* deploy

```bash
kubectl apply -f deploy/vgpu-manager-scheduler.yaml
kubectl apply -f deploy/vgpu-manager-deviceplugin.yaml
```

* uninstall

```shell
kubectl delete -f deploy/vgpu-manager-scheduler.yaml
kubectl delete -f deploy/vgpu-manager-deviceplugin.yaml
```

* label nodes with `vgpu-manager-enable=enable`

```shell
kubectl label node <nodename> vgpu-manager-enable=enable
```

Note that the scheduler version needs to be modified according to the cluster version
```yaml
      containers:
        - image: registry.cn-hangzhou.aliyuncs.com/google_containers/kube-scheduler:v1.28.15
          imagePullPolicy: IfNotPresent
          name: scheduler
```

## Example of use

Submit a VGPU container application with 10% computing power and 1GB of memory

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-pod
  namespace: default
spec:    
  schedulerName: vgpu-scheduler # Specify scheduler
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
        nvidia.com/vgpu-number: 1     # one gpu
        nvidia.com/vgpu-core: 10      # Allocate 10% of computing power
        nvidia.com/vgpu-memory: 1024  # Allocate memory quantity
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

## Scheduling strategy 

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

## Select devices

Support selecting device type or uuid

Add annotations to vGPU pod to select or exclude device types to be scheduled: 
`nvidia.com/include-gpu-type` `nvidia.com/exclude-gpu-type`

If there are multiple devices separated by commas

Example: Choose to use A10 and exclude A100
```yaml
metadata:
  annotations:
    nvidia.com/include-gpu-type: "A10"  
    nvidia.com/exclude-gpu-type: "A100"
```

Add annotations to vGPU pod to select or exclude device uuids to be scheduled:
`nvidia.com/include-gpu-uuid` `nvidia.com/exclude-gpu-uuid`

If there are multiple devices separated by commas

Example: Select a GPU uuid
```yaml
metadata:
  annotations:
    nvidia.com/include-gpu-uuid: GPU-49aa2e6a-33f3-99dd-e08b-ea4beb0e0d28
```