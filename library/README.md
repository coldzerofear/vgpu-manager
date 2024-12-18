# vgpu-controller

服务于nvidia显卡的vgpu控制器，用于给容器设备划分vgpu资源，实现容器资源硬隔离。

通过动态连接拦截CUDA调用，基于[vcuda-controller](https://github.com/tkestack/vcuda-controller)及[HAMi-coer](https://github.com/Project-HAMi/HAMi-core)改造而来。

对比原版[vcuda-controller](https://github.com/tkestack/vcuda-controller),简化掉了rpc调用,容器脱离设备插件也能运行,兼容cgroupv2环境

## 构建

```
./build.sh
```

## 查找cuda新增函数
```bash
./find_new_lib.sh /lib/x86_64-linux-gnu/libcuda.so.535.54.03 /lib/x86_64-linux-gnu/libnvidia-ml.so.535.54.03
```

## 使用方式

环境变量：

* CUDA_MEM_LIMIT_<index>： 内存限制
* CUDA_CORE_LIMIT： 算力核心限制
* CUDA_CORE_SOFT_LIMIT： 算力核心软限制（允许超过硬限制）
* GPU_DEVICES_UUID： 设备uuid
* CUDA_MEMORY_OVERSUBSCRIBE： 设备内存超卖（虚拟内存）

Pod 例子

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    nvidia.com/use-gputype: a10
  name: gpu-test
spec:
  #schedulerName: gpu-scheduler
  terminationGracePeriodSeconds: 0
  containers:
    - name: test
      image: registry.tydic.com/cube-studio/gpu-player:v2
      command: ["/usr/bin/python", "/app/main.py", "--total=1", "--allocated=1"]
      #command: ["sleep", "9999999"]
      resources:
        limits:
          memory: 1Gi
      env:
      - name: VGPU_POD_NAME
        valueFrom:
          fieldRef:
            fieldPath: metadata.name
      - name: VGPU_POD_NAMESPACE
        valueFrom:
          fieldRef:
            fieldPath: metadata.namespace
      - name: VGPU_POD_UID
        valueFrom:
          fieldRef: 
            fieldPath: metadata.uid
      - name: VGPU_CONTAINER_NAME
        value: test
      - name: LD_PRELOAD
        value: /root/build/libvgpu-control.so
      - name: LOGGER_LEVEL # 日志等级
        value: "4"
      - name: CUDA_MEM_LIMIT_0 # 内存限制gpu0为1000m可用
        value: "1000m"
      - name: CUDA_MEM_LIMIT_1 # 内存限制gpu1为1000m可用
        value: "1000m"
      - name: CUDA_CORE_LIMIT # 算力限制利用率百分之50
        value: "50"
      - name: GPU_DEVICES_UUID # 设备uuid
        value: GPU-c496852d-f5df-316c-e2d5-86f0b322ec4c
      - name: NVIDIA_VISIBLE_DEVICES
        value: GPU-c496852d-f5df-316c-e2d5-86f0b322ec4c
      volumeMounts:
      - name: host-proc # 容器内使用需要将host proc目录mount到这个位置
        mountPath: /etc/vgpu-manager/host_proc
      - name: host-cgroup
        mountPath: /etc/vgpu-manager/host_cgroup
      - name: tools
        mountPath: /root/build
  volumes:
    - name: host-proc
      hostPath: 
        path: /proc
        type: Directory
    - name: host-cgroup
      hostPath: 
        path: /sys/fs/cgroup/devices/kubepods.slice/kubepods-burstable.slice
        type: Directory
    - name: tools
      hostPath:
        path: /root/library/build
        type: Directory
```

docker 例子

```bash	
docker run -it --net=host -v /root/libvgpu-control.so :/root/hook/libvgpu-control.so -v /proc:/var/run/vgpu/host_proc --name my-test -e NVIDIA_DRIVER_CAPABILITIES=compute,utility -e NVIDIA_VISIBLE_DEVICES=GPU-84ccfef5-2da2-1225-4836-7d347036f968,GPU-c4d72682-eded-81f0-557a-12e8e2c1d0cb,GPU-ca926cfb-4ba0-5ab1-d59e-4e033a6dd111 -e LD_PRELOAD=/root/hook/libvgpu-control.so -e LOGGER_LEVEL=5 -e  CUDA_MEM_LIMIT_0=1000m -e CUDA_MEM_LIMIT_1=1000m -e CUDA_MEM_LIMIT_2=1000m -e POD_NAME=test -e GPU_DEVICES_UUID=GPU-84ccfef5-2da2-1225-4836-7d347036f968,GPU-c4d72682-eded-81f0-557a-12e8e2c1d0cb,GPU-ca926cfb-4ba0-5ab1-d59e-4e033a6dd111  registry.tydic.com/cube-studio/gpu-player:v2 sleep 10000000
```


## Log level

Use environment variable `LOGGER_LEVEL` to set the visibility of logs

| LOGGER_LEVEL       | description                                 |
| ------------------ |---------------------------------------------|
| 0                  | infos                                       |
| 1                  | errors,infos                                |
| 2                  | warnings,errors,infos                       |
| 3 (default)        | fatal,warnings,errors,infos                 |
| 4                  | verbose,fatal,warnings,errors,infos         |
| 5                  | details,verbose,fatal,warnings,errors,infos |

## CUDA/GPU support information

CUDA 12.2.x and before are supporteds

Any architecture of GPU after Kepler are supported