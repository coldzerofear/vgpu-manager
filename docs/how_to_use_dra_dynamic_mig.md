# DRA 动态 MIG 使用指南

本文说明如何在 vgpu-manager DRA 驱动上启用动态 MIG、如何申请 MIG 资源，以及如何与 vGPU 同时启用。

> 状态：`DynamicMIG` 是 Alpha 功能。vGPU 与动态 MIG 同开的逻辑已用单元测试和真实调度分配器仿真验证，**尚未在真机上验证**，上线前请先在测试节点走完第 6 节。

## 1. 工作方式

开启后不需要预先切分 MIG。驱动发布每张卡所有可能的 MIG 规格，Pod 被调度后，由 kubelet-plugin 在 Prepare 阶段创建对应实例，claim 释放后销毁。

与 vGPU 同开时，一张卡在同一时刻只能处于一种模式：

| 卡的状态 | 可分配 vGPU | 可分配 MIG |
| --- | --- | --- |
| 空闲 | 是 | 是 |
| 已有 vGPU claim | 是 | 否 |
| 已有 MIG claim | 否 | 是 |

最后一个 claim 释放后，卡回到空闲。vGPU 不会运行在 MIG 实例里面，一个 Pod 也不要在同一容器里同时申请 vGPU 和 MIG。

## 2. 前提条件

### 通用要求

* 已按 [how_to_use_DRA_driver.md](./how_to_use_DRA_driver.md) 满足 DRA 驱动的基础要求，并启用 CDI。
* 不要同时部署 device-plugin 方案。
* 节点上**没有需要保留的 MIG 实例**。插件启动时会销毁自己没有记录的 MIG 实例。
* 插件运行期间不要手工执行 `nvidia-smi mig` 或 `mig-parted`。
* 节点需为裸金属或 GPU 直通虚拟机。NVIDIA vGPU 虚拟机（guest）不支持动态 MIG。

### Kubernetes 版本

| 使用方式 | 最低版本 | 需要的 K8s 特性门控 |
| --- | --- | --- |
| 只用动态 MIG（`VGPUSupport=false`） | 1.34 | 1.34、1.35 需在 kube-apiserver 和 kube-scheduler 开启 `DRAPartitionableDevices`；1.36 起默认开启 |
| vGPU + 动态 MIG | **1.36.2** | `DRAPartitionableDevices`、`DRAConsumableCapacity` 默认开启，无需设置 |

低于 1.36.2 时调度器不把 vGPU 计入共享 counter，可能把 MIG 分到正在运行 vGPU 的卡上，因此插件会拒绝启动。该检查读取的是 API server 版本，kube-scheduler 也必须不低于 1.36.2。

### GPU 型号

| GPU | 启动时 MIG 状态 | 同开时发布的设备 | 能否自动切换模式 |
| --- | --- | --- | --- |
| Hopper（H100/H200）及更新 | 任意 | vGPU 和 MIG | 能 |
| Ampere（A100/A30） | 已关闭 | 只有 vGPU | 不能 |
| Ampere（A100/A30） | 已开启 | 只有 MIG | 不能 |
| 不支持 MIG 的卡 | 无 | 只有 vGPU | 不涉及 |

Ampere 切换 MIG 模式需要 GPU reset，驱动不会自动执行。要在 Ampere 上用 MIG，先在节点上开启，再启动插件：

```shell
nvidia-smi -i 0 -mig 1
nvidia-smi -i 0 --gpu-reset    # 或重启节点
nvidia-smi -i 0 --query-gpu=mig.mode.current --format=csv
```

### 特性门控互斥

`DynamicMIG` 不能与 `RemoteGPUSupport`、`PassthroughSupport`、`MPSSupport` 同时开启，否则插件启动失败。`ConsumableShares` 与 `VGPUSupport` 互斥。

## 3. 启用

### Helm（推荐）

只改 kubelet-plugin 的门控。device-monitor 使用另一套门控，不要写到 `monitor.featureGates` 下。

```yaml
# values-dynamic-mig.yaml
kubeletPlugin:
  containers:
    plugin:
      featureGates:
        VGPUSupport: true          # 只用 MIG 时设为 false
        DynamicMIG: true
        NVMLDeviceHealthCheck: true
        # VGPUSupport=false 时必须同时关闭，它依赖 VGPUSupport
        SharedSMUtilizationWatcher: true
```

```shell
helm upgrade --install vgpu-manager-dra-driver ./charts/vgpu-manager-dra-driver \
  -n kube-system -f values-dynamic-mig.yaml
```

只用 MIG 时，`SharedSMUtilizationWatcher`、`DevicePluginClientMode`、`NRISupport` 都要设为 `false`，否则启动校验失败。

### YAML 部署

编辑 [vgpu-manager-dra-driver.yaml](../deploy/dra-local/vgpu-manager-dra-driver.yaml)，在 `FEATURE_GATES` 里追加 `DynamicMIG=true`：

```yaml
- name: FEATURE_GATES
  value: NVMLDeviceHealthCheck=true,SharedSMUtilizationWatcher=true,DevicePluginClientMode=false,NRISupport=false,TopologyDeviceAttributes=true,DynamicMIG=true
```

```shell
kubectl apply -f deploy/dra-local/vgpu-manager-deviceclass.yaml
kubectl apply -f deploy/dra-local/vgpu-manager-dra-driver.yaml
```

修改门控后 DaemonSet 会重启插件。已有的 vGPU 业务不需要停止，已 Prepare 的 MIG 实例按 checkpoint 保留。

## 4. 确认启用成功

```shell
# DeviceClass 存在
kubectl get deviceclass mig-manager vgpu-manager

# 每张卡发布了 vgpu-N 和 gpu-N-mig-* 设备
kubectl get resourceslice -o yaml | grep -E 'name: (vgpu-|gpu-[0-9]+-mig-)'

# 设备带有共享 counter
kubectl get resourceslice -o yaml | grep -c consumesCounters
```

`consumesCounters` 计数为 0，说明 API server 没有开启分区设备，或该节点没有可用的 MIG 卡。

查看节点支持的规格，下文的 `profile` 取值以此为准：

```shell
nvidia-smi mig -lgip
```

`profile` 的格式为 `1g.10gb`。设备名由规格和位置组成，例如 `gpu-0-mig-1g10gb-19-0`，无需手写。

## 5. 申请资源

所有 MIG 申请都通过 ResourceClaim，DeviceClass 使用 `mig-manager`。以下示例都能直接 `kubectl apply`，把 `1g.10gb` 换成节点实际支持的规格。

### 5.1 申请指定规格

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: mig-1g
  namespace: default
spec:
  spec:
    devices:
      requests:
        - name: mig
          exactly:
            deviceClassName: mig-manager
            selectors:
              - cel:
                  expression: "device.attributes['manager.nvidia.com'].profile == '1g.10gb'"
---
apiVersion: v1
kind: Pod
metadata:
  name: mig-pod
  namespace: default
spec:
  restartPolicy: Never
  containers:
    - name: ctr0
      image: ubuntu:24.04
      command: ["bash", "-c", "nvidia-smi -L; sleep 9999"]
      resources:
        claims:
          - name: mig
  resourceClaims:
    - name: mig
      resourceClaimTemplateName: mig-1g
```

### 5.2 按显存大小申请

不想绑定规格名时，用显存容量筛选。分配器按设备顺序取第一个匹配，不保证是最小规格，需要控制规格时请用 5.1。

```yaml
selectors:
  - cel:
      expression: "device.capacity['manager.nvidia.com'].memory.compareTo(quantity('20Gi')) >= 0"
```

### 5.3 同一张卡上申请多个 MIG

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: mig-same-gpu
  namespace: default
spec:
  spec:
    devices:
      requests:
        - name: small
          exactly:
            deviceClassName: mig-manager
            selectors:
              - cel:
                  expression: "device.attributes['manager.nvidia.com'].profile == '1g.10gb'"
        - name: medium
          exactly:
            deviceClassName: mig-manager
            selectors:
              - cel:
                  expression: "device.attributes['manager.nvidia.com'].profile == '2g.20gb'"
      constraints:
        - requests: []
          matchAttribute: "manager.nvidia.com/parentUUID"
```

### 5.4 与 vGPU 共存

vGPU 的申请方式不变，同一节点上两类 Pod 可以同时运行，由调度器分到不同的卡：

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: vgpu-small
  namespace: default
spec:
  spec:
    devices:
      requests:
        - name: vgpu
          exactly:
            deviceClassName: vgpu-manager
            capacity:
              requests:
                cores: 50
                memory: 4Gi
```

确认两者落在不同的卡上：

```shell
kubectl get resourceclaim -o custom-columns=NAME:.metadata.name,DEVICE:.status.allocation.devices.results[*].device
```

输出中 `vgpu-N` 与 `gpu-M-mig-*` 的 N、M 不应相同。

### 5.5 规划节点

调度器没有设备级调度策略，这会影响同开时的容量：

* 一张卡只要有一个小 vGPU，就不能再分配 MIG。
* 大 vGPU 会占满靠前的卡后继续占用下一张空卡，所有卡都进入 vGPU 模式后，MIG 申请会失败。
* 调度器不会把 Pod 优先放到已是同模式卡的节点上。

需要稳定保证 MIG 容量时，把节点分成 vGPU 和 MIG 两组，用 `nodeSelector` 或亲和性分别调度。

## 6. 上线验证清单

在测试节点依次确认：

1. 申请 5.1 的 Pod，`nvidia-smi -L` 能看到 MIG 设备。
2. 删除 Pod 后，`nvidia-smi mig -lgi` 无残留实例，`nvidia-smi --query-gpu=mig.mode.current --format=csv` 回到 `Disabled`（Hopper 及更新）。
3. 只有一张卡时，先运行 vGPU Pod，再申请 MIG：MIG Pod 保持 `Pending`；vGPU Pod 删除后 MIG Pod 启动。
4. 反过来先 MIG 后 vGPU，结果对称。
5. 运行 MIG 业务时重启插件，业务不中断，实例仍在。
6. 卡上有 vGPU 业务时重启插件，业务不中断。
7. 删除所有 Pod 后，同一张卡能再次用于 vGPU，且 `nvidia-smi` 显示的 GPU 总显存和 vGPU 限额正确。

## 7. 限制

* MIG 的隔离由硬件提供，`cores` 和 `memory` 软限制不适用。一个容器不要混用 vGPU 和 MIG。
* DRA 路径目前不上报 MIG 指标，切到 MIG 的卡上的容器监控为空。物理卡指标不受影响。
* `TimeSlicingSettings` 对 MIG 设备不起作用。
* Shared SM watcher 不采样处于 MIG 模式的卡。
* 致命 XID 会给该卡的 vGPU 和全部 MIG 设备打 NoSchedule 污点，已运行的业务不被驱逐。污点不会自动清除，修复硬件后需要重启插件。
* 卡被打污点后 watcher 停止采样，vGPU 业务退回本地 NVML 采样，并在业务日志中周期性输出 WARNING：`process utilization time window timeout detected, fallback to nvml driver`。看到它说明该卡需要报修。

## 8. 排障

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 插件启动失败：`together require Kubernetes 1.36.2 or later` | 集群版本过低 | 升级集群，或关闭 `VGPUSupport` / `DynamicMIG` 之一 |
| 插件启动失败：`mutually exclusive with` | 门控冲突 | 按第 2 节调整 |
| 插件启动失败：`unrecognized feature gate` | 门控写到了 device-monitor | 写到 `plugin.featureGates` |
| 日志 `no MIG capable GPU found` | 节点没有 MIG 卡 | 该节点退回普通模式，属正常 |
| Pod `Pending`，事件为 `cannot allocate all claims` | 没有空闲规格，或卡都在 vGPU 模式 | 查 `kubectl get resourceclaim -A`，等待释放或按 5.5 分组 |
| Pod 卡 `ContainerCreating`：`shares a physical GPU with device` | 上一个 Pod 被强删，节点上旧 claim 尚未清理 | 等待 kubelet 重试 |
| Pod 卡 `ContainerCreating`：`is in MIG mode` | vGPU 分到的卡关闭 MIG 失败 | 看卡上是否有残留实例或进程 |
| Pod 卡 `ContainerCreating`，错误含 `activation failed` | 卡上有进程占用，MIG 模式切换失败 | 用 `nvidia-smi` 或 `fuser -v /dev/nvidia*` 查进程并停止 |
| 容器看不到 GPU，节点上 MIG 实例存在 | CDI 未生效 | 检查容器运行时 CDI 配置 |

问题上报时提供 kubelet-plugin 日志、`kubectl get resourceslice -o yaml`、`kubectl get resourceclaim -A -o yaml` 和 `nvidia-smi mig -lgi` 输出。
