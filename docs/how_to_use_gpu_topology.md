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
  schedulerName: vgpu-scheduler  # Specify scheduler (default: vgpu-scheduler)
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
  schedulerName: vgpu-scheduler  # Specify scheduler (default: vgpu-scheduler)
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

### Cross-pod topology (multi-pod gang training)

By default `link` topology only optimizes GPU connectivity **within a single pod**.
For distributed training where a gang (PodGroup) is split into multiple pods, add the
annotation `nvidia.com/cross-pod-topology: "true"` to keep the whole gang's GPUs in
the **same NVLink connected component on a node**, and — across nodes — aligned to the
**same component ordinal (rail)**. It takes effect only together with
`device-topology-mode: link` and a recognized gang (scheduler-plugins / Volcano /
Koordinator / native PodGroup); non-gang pods are unaffected.

```yaml
metadata:
  annotations:
    nvidia.com/device-topology-mode: link      # required: link topology
    nvidia.com/cross-pod-topology: "true"      # opt into cross-pod / cross-node alignment
  labels:
    scheduling.x-k8s.io/pod-group: my-training-gang   # gang identity (any supported dialect)
```

Notes:
- Strictness follows the topology mode: use `link-strict` to reject a node when the
  gang cannot stay connected, instead of degrading.
- Combine with `nvidia.com/device-scheduler-policy: spread` to also keep gang siblings
  on **different physical cards** within the component (indirect anti-co-location).
- Cross-node sub-domain (rail) alignment assumes homogeneous GPU↔rail layout across
  nodes; for whole-node (all cards) requests it is a no-op. For cross-node node-level
  topology (rack/rail) use Kueue TAS — see [kueue_tas_integration.md](./kueue_tas_integration.md).