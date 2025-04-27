## Describe

The vgpu-manager supports configuring NodeConfig and mounting it to the device plugin container through configmap, allowing for differentiated configuration for different nodes.

## Usage

Create nodeConfig.json configmap

Example:

```yaml
apiVersion: v1
data:
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
        configMap:    # Add nodeconfig cm
          name: vgpu-manager-device-plugin-config
```

## NodeConfig format

NodeConfig is a JSON formatted array, with each array element having the following structural parameters

| Parameter Name      | Value Format                               | Describe                                                                               |
|---------------------|--------------------------------------------|----------------------------------------------------------------------------------------|
| nodeName            | string (support regular matching)          | Configure k8s node name (required)                                                     |
| cgroupDriver        | Supported values: "cgroupfs"/"systemd"     | Specify the cgroup driver used (optional)                                              |
| deviceListStrategy  | supported values: "envvar"/"volume-mounts" | The desired strategy for passing the device list to the underlying runtime (optional)  |
| deviceSplitCount    | int                                        | The maximum number of VGPU that can be split per physical GPU (optional)               |
| deviceMemoryScaling | float                                      | The ratio for NVIDIA device memory scaling (optional)                                  |
| deviceMemoryFactor  | int                                        | The default gpu memory block size is 1MB (optional)                                    |
| deviceCoresScaling  | float                                      | The ratio for NVIDIA device cores scaling (optional)                                   |
| excludeDevices      | example: "0,1,2"/"0-2"                     | Specify the GPU IDs that need to be excluded (optional)                                |
| gdsEnabled          | bool                                       | Ensure that containers are started with NVIDIA_GDS=enabled (optional)                  |
| mofedEnabled        | bool                                       | Ensure that containers are started with NVIDIA_MOFED=enabled (optional)                |
| openKernelModules   | bool                                       | If using the open-gpu-kernel-modules, open it and enable compatibility mode (optional) |
