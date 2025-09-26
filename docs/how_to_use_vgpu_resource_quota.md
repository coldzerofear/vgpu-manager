## Describe

vgpu-manager supports quota control of vGPU resources using `ResourceQuota`

## Example

Create a `ResourceQuota`, limit the default namespace to only apply for one vGPU.

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: vgpu-resource-quota
  namespace: default
spec:
  hard:
    requests.nvidia.com/vgpu-number: "1"
```

Check the resource quota, at this time, there are no pods using vGPU.

```bash
$ kubectl get quota vgpu-resource-quota
NAME                  AGE   REQUEST                                LIMIT
vgpu-resource-quota   10s   requests.nvidia.com/vgpu-number: 0/1  
 
$ kubectl describe quota vgpu-resource-quota
Name:                            vgpu-resource-quota
Namespace:                       default
Resource                         Used  Hard
--------                         ----  ----
requests.nvidia.com/vgpu-number  0     1
```

Create a vGPU pod. at this time, it can be created normally.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: vgpu-pod-test
  namespace: default
spec:
  schedulerName: vgpu-scheduler
  containers:
  - name: default
    image: quay.io/jitesoft/ubuntu:24.04
    command: ["sleep", "9999999"]
    resources:
      limits:
        nvidia.com/vgpu-number: 1 # apply for one
```

Check quota, vGPU has been used as 1.

```bash
$ kubectl describe quota vgpu-resource-quota
Name:                            vgpu-resource-quota
Namespace:                       default
Resource                         Used  Hard
--------                         ----  ----
requests.nvidia.com/vgpu-number  1     1
```

At this point, the available vGPU for the default namespace has reached the quota limit.

If you create vGPU pod again, the request will be rejected.

```bash
$ kubectl apply -f vgpu-pod.yaml 
Error from server (Forbidden): error when creating "vgpu-pod.yaml": pods "vgpu-pod-test1" is forbidden: exceeded quota: vgpu-resource-quota, requested: requests.nvidia.com/vgpu-number=1, used: requests.nvidia.com/vgpu-number=1, limited: requests.nvidia.com/vgpu-number=1
```

## HardLimit ResourceName

| ResourceName                    | Describe                                                       |
|---------------------------------|----------------------------------------------------------------|
| requests.nvidia.com/vgpu-number | Limit the number of VGPU applications that can be applied for. |
| requests.nvidia.com/vgpu-cores  | Limit the number of vGPU cores that can be applied for.        |
| requests.nvidia.com/vgpu-memory | Limit the amount of VGPU memory that can be applied for.       |
