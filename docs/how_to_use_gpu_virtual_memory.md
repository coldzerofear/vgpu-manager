## Describe

vgpu-manager supports virtual memory functionality, which allows the use of GPUs beyond the physical device memory limit without modifying any code. The principle of virtual memory functionality is to apply for the excess memory and allocate it using Global Unified Memory (UVA).

## Usage

Set a floating-point number greater than 1 using the startup parameter `--device-memory-scaling` of the device plugin.

```yaml
$ kubectl edit ds -n kube-system vgpu-manager-device-plugin
  containers:
  - name: device-plugin
    command:
    - deviceplugin
    - --device-memory-scaling=2.0 # Set the memory scaling ratio to 2 times
```

After waiting for the device plugin pod to run again, check the node annotation:

```yaml
$ kubectl get node master -o yaml | grep node-device-register
    # You can see that the device reported virtual memory that is twice as much as physical memory
    nvidia.com/node-device-register: '[{"id":0,"type":"NVIDIA GeForce GTX 1050 Ti","uuid":"GPU-49aa2e6a-33f3-99dd-e08b-ea4beb0e0d28","core":100,"memory":8192,"number":10,"numa":0,"mig":false,"busId":"0000:01:00.0","capability":6.1,"healthy":true}]'
```

At this point, create a pod that has applied for 4GB of GPU memory, which can use up to 2GB of GPU physical memory, with any excess using virtual memory (UVA).

> Note:  After enabling virtual memory, monitoring indicators cannot accurately reflect the specific GPU memory used by the container, and can only count the usage of physical memory.

## GPU Memory Overslod

When the physical memory of the GPU reaches its limit, allowing the allocation of virtual memory to achieve the effect of memory oversold.

Add the environment variable `CUDA_MEM_OVERSOLD` to the container configuration to enable this feature.

Pod example:

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
