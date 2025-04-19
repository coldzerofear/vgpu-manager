## Describe

vgpu-manager supports GPU topology function. When a container requests multiple VGPU, it can allocate the best GPU according to the corresponding topology mode to provide high performance.

## Usage

The VGPU pod can use the annotation `nvidia.com/device-topology-mode` to select the topology mode to be used.

Annotation `nvidia.com/device-topology-mode` supports values:

* numa: When allocating multiple cards, recognize the affinity of numa and try to allocate them on GPUs of numa nodes on the same side.
* link: When allocating multiple cards, identify nvlinks and try to allocate them to the optimal nvlink topology to improve multi card performance.

### Numa topology

Pod example: 

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    nvidia.com/device-topology-mode: numa # Specify the use of numa topology allocation
  name: numa-pod-example
  namespace: default
spec:    
  schedulerName: vgpu-scheduler  # Specify scheduler (default: vgpu-manager)
  containers:
  - name: default
    image: nvidia/cuda:12.4.1-devel-ubuntu20.04
    command: ["sleep", "9999999"]
    resources:
      limits:
        cpu: 2
        memory: 4Gi
        nvidia.com/vgpu-number: 2 # Valid when applying for more than one card
        nvidia.com/vgpu-memory: 10000
```

### Link topology

To use the link topology mode, it is necessary to enable the GPUTopology function gating on the device plugin and scheduler plugin.

```bash
$ kubectl edit ds -n kube-system vgpu-manager-device-plugin
  containers
  - name: device-plugin
    command:
    - deviceplugin
    - --feature-gates=GPUTopology=true # Ensure that the feature gate is turned on

$ kubectl edit deployment -n kube-system vgpu-manager-scheduler
  containers
  - name: scheduler-extender
    command:
    - scheduler
    - --feature-gates=GPUTopology=true # Ensure that the feature gate is turned on
```

After restarting the device plugin, check the node comments to determine if the GPU topology function has been successfully started

```bash
$ kubectl get node master -o yaml | grep node-device-topology
    nvidia.com/node-device-topology: '[{"index":0,"links":....'
```

Pod example:

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    nvidia.com/device-topology-mode: link # Specify the use of link topology allocation
  name: link-pod-example
  namespace: default
spec:    
  schedulerName: vgpu-scheduler  # Specify scheduler (default: vgpu-manager)
  containers:
  - name: default
    image: nvidia/cuda:12.4.1-devel-ubuntu20.04
    command: ["sleep", "9999999"]
    resources:
      limits:
        cpu: 2
        memory: 4Gi
        nvidia.com/vgpu-number: 2 # Valid when applying for more than one card
        nvidia.com/vgpu-memory: 10000
```