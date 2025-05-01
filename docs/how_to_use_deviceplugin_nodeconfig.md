## Describe

The vgpu-manager supports configuring NodeConfig and mounting it to the device plugin container through configmap, allowing for differentiated configuration for different nodes.

## Parameters

NodeConfig is array, with each array element having the following structural parameters:

| Parameter Name      | Value Format                               | Describe                                                                               |
|---------------------|--------------------------------------------|----------------------------------------------------------------------------------------|
| nodeName            | string (support regular matching)          | K8s node name, Specify the nodes for configuring the application (required)            |
| cgroupDriver        | string (support: "cgroupfs"/"systemd")     | Specify the cgroup driver used on the specified node (optional)                        |
| deviceListStrategy  | string (support: "envvar"/"volume-mounts") | The desired strategy for passing the device list to the underlying runtime (optional)  |
| deviceSplitCount    | int                                        | The maximum number of VGPU that can be split per physical GPU (optional)               |
| deviceMemoryScaling | float                                      | The ratio for NVIDIA device memory scaling (optional)                                  |
| deviceMemoryFactor  | int                                        | The default gpu memory block size is 1MB (optional)                                    |
| deviceCoresScaling  | float                                      | The ratio for NVIDIA device cores scaling (optional)                                   |
| excludeDevices      | string (example: "0,1,2"/"0-2")            | Specify the GPU IDs that need to be excluded (optional)                                |
| gdsEnabled          | bool                                       | Ensure that containers are started with NVIDIA_GDS=enabled (optional)                  |
| mofedEnabled        | bool                                       | Ensure that containers are started with NVIDIA_MOFED=enabled (optional)                |
| openKernelModules   | bool                                       | If using the open-gpu-kernel-modules, open it and enable compatibility mode (optional) |

## Example

NodeConfig currently supports both JSON and YAML formats, Identify and use the corresponding parsing method through the file suffixes `.json` and `.yaml`

* JSON format

```json
[
  {
    "nodeName": "testNode",
    "cgroupDriver": "systemd",
    "deviceListStrategy": "envvar",
    "deviceSplitCount": 10,
    "deviceMemoryScaling": 1.0,
    "deviceMemoryFactor": 1,
    "deviceCoresScaling": 1.0,
    "excludeDevices": "0-2",
    "openKernelModules": true
  }
]
```

* YAML format

```yaml
version: v1
config:
 - nodeName: testNode
   cgroupDriver: systemd
   deviceListStrategy: envvar
   deviceSplitCount: 10
   deviceMemoryScaling: 1.0
   deviceMemoryFactor: 1
   deviceCoresScaling: 1.0
   excludeDevices: "0-2"
   openKernelModules: true
```

## Usage

Create a JSON formatted configmap

Configmap example:

```yaml
apiVersion: v1
data:
  # Note: JSON format with `.json` suffix, YAML format with `.yaml` or `.yml` suffix
  nodeConfig.json: |
    [
      {
        "nodeName": "master",
        "deviceSplitCount": 5,
        "deviceMemoryScaling": 2,
        "deviceMemoryFactor": 1,
        "deviceCoresScaling": 1
      }
    ]
kind: ConfigMap
metadata:
  name: vgpu-manager-device-plugin-config
  namespace: kube-system
```

Mount the created configmap to the device plugin and set startup parameters`--node-config-path`

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: vgpu-manager-device-plugin
  namespace: kube-system
spec:
  template:
    spec:
      containers:
      - name: device-plugin
        command:
        - deviceplugin
        - --node-config-path=/config/nodeConfig.json # Specify the path of the nodeconfig file
        volumeMounts: # Add Mount
        - name: node-config
          mountPath: /config        
      - name: node-config
        configMap:    # Add nodeConfig configmap
          name: vgpu-manager-device-plugin-config
```

The device plugin pod corresponding to the node will read and use the configuration file after restarting.
