## Describe

vgpu-manager supports providing vGPU to POD through Kubernetes' latest dynamic resource allocation(DRA) mechanism

Using DRA requires the independent installation of the DRA driver for the vgpu-manager, which cannot work simultaneously with the vgpu-manager-device-plugin

## Prerequisite

* NVIDIA drivers >= 440
* nvidia-docker version > 2.0
* default runtime configured as nvidia for containerd/docker/cri-o container runtime
    - `accept-nvidia-visible-devices-as-volume-mounts` option should be set to `true` for NVIDIA Container Runtime
* Kubernetes version >= 1.34 wit DRAConsumableCapacity feature enabled
* glibc >= 2.17 & glibc < 2.35
* kernel version >= 3.10

> Note: DRAConsumableCapacity 作为 Kubernetes 1.34 中的 alpha 功能引入，功能门必须在 kubelet、kube-apiserver、kube-scheduler 和 kube-controller-manager 中启用。

## Install DRA driver

You can use the following command to deploy the vgpu-manager's DRA driver

```bash
kubectl apply -f deploy/dra/vgpu-manager-kubeletplugin.yaml
```

## Usage example

Submit a request for a single vGPU pod

```yaml
kubectl apply -f  example/dra/pod-single-vgpu.yaml
```

Submit a request for a multi vGPU pod

```yaml
kubectl apply -f  example/dra/pod-multi-vgpu.yaml
```

## Install DRA webhook

DRA Webhook supports converting the traditional vGPU resource request format of vgpu-manager into DRA request format, and is compatible with some annotations supported by vgpu-manager.

You can deploy a webhook using the following command

```yaml
kubectl apply -f deploy/dra/vgpu-manager-webhook.yaml
```

Request VGPU like using device plugins

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: vgpu-pod
  namespace: default
spec:    
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

Pod submissions will be converted into DRA request format by the webhook

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: vgpu-pod
  namespace: default
spec:    
  containers:
  - name: default
    image: nvidia/cuda:12.4.1-devel-ubuntu20.04
    command: ["sleep", "9999999"]
    resources:
      limits:
        cpu: 2
        memory: 4Gi
      claims:  # Convert to using resource claim request devices
        - name: vgpu-pod-default-92f8e6ab
  resourceClaims:
  - name: vgpu-pod-default-92f8e6ab
    resourceClaimName: vgpu-pod-default-92f8e6ab
```

The created resource claim is as follows

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: vgpu-pod-default-92f8e6ab
  namespace: default
spec:
  devices:
    constraints:
    - distinctAttribute: manager.nvidia.com/uuid
      requests:
      - vgpu
    requests:
    - exactly:
        allocationMode: ExactCount
        capacity:
          requests:
            cores: "10"
            memory: 1Gi
        count: 1
        deviceClassName: vgpu-manager
        selectors:
        - cel:
            expression: device.attributes["manager.nvidia.com"].type == "vgpu"
      name: vgpu
```

## about DRAExtendedResource

DRAExtendedResource is the ability of DRA to extend resources, allowing Pods to use DRA like requesting extended resources from device plugins.

DRAExtendedResource must be enabled simultaneously in these three components
* kube-apiserver
* kube-scheduler
* kubelet

After enabling, you can define the extension resource name in `DeviceClass.spec.sextendedResourceName`, for example:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: gpu-manager
spec:
  selectors:
    - cel:
        expression: "device.driver == 'manager.nvidia.com' && device.attributes['manager.nvidia.com'].type == 'gpu'"
  # Expanded resource names for mapping
  extendedResourceName: nvidia.com/gpu
```

Then the Pod can request like a traditional extended resource:

```yaml
resources:
  requests:
    nvidia.com/gpu: 2
  limits:
    nvidia.com/gpu: 2
```