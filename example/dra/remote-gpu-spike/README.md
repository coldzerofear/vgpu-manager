# S0 spike：远程 GPU 调度链路验证（纯 YAML，零代码）

> 对应 `docs/remote_gpu_k8s_integration_design.md` §8.1（D22）。
> 目的：在写任何 Go 代码之前，证实 **DRA allocator 会消费 nodeSelector 放宽后的节点池**——
> 即"设备由 GPU 节点声明、pod 却被调度到网络可达的无 GPU 节点"这一核心机制（§2.1/§2.2）。

## 模型（v2.0）

不存在独立的"远程池"：GPU 节点插件开启 `RemoteGPUSupport` 后，**同一批设备**多发 `accessMode=remote`、
`endpoint`、`netZone` 属性，并把 pool 的节点范围从 `nodeName` 放宽为 nodeSelector（本节点 OR 可达节点）。
**所有消费者统一走远程路径——即使 pod 恰好落在 GPU 节点上**（v2.1：同一 pod 可能混合本节点与其他节点的卡，本地/远程两条注入路径无法共存，故 gate 开的节点的 server 插件只发布、不注册 DRA 服务，同节点另跑 `--mode=inject` 承担分配）。未开 gate 的节点发布 `accessMode=local`，
行为与今天完全一致；`vgpu-manager` class 加 `accessMode == "local"` 即"只要本地专属节点的设备"。

## 前置条件

- k8s ≥ 1.34（`resource.k8s.io/v1` 可用；本仓库 go.mod 已对齐 1.37 API）。
- 集群里已部署 vgpu-manager dra-driver **或者完全没有**都可以：手工 slice 不带
  `spec.nodeName`，真插件的 resourceslice controller 按 nodeName 过滤，不会回收它。
- 一个"消费节点"（无 GPU 即可）。

## 步骤

1. 给消费节点打可达性标签（标签任意，真插件用 `--remote-node-selector` 接同样的 label selector 表达式）：

   ```bash
   kubectl label node <consumer-node> topology.kubernetes.io/zone=az1   # 任意标签，与 slice 的 nodeSelector 一致
   ```

2. 编辑 `10-resourceslice.yaml`：把 `uuid`/`endpoint`/`memory`/`cudaDriverVersion`/GPU 节点名换成目标
   GPU 节点的真实值（S0 阶段 endpoint 只是被记录、不被连接，占位值也可）。

3. 依次 apply：

   ```bash
   kubectl apply -f 00-deviceclass.yaml
   kubectl apply -f 10-resourceslice.yaml
   kubectl apply -f 20-claim-pod.yaml
   ```

## 验收判据

| 检查 | 期望 |
|---|---|
| `kubectl get resourceclaim spike-remote-vgpu-0 -o yaml` | `status.allocation.devices.results[0]` 命中 pool `remote-spike-pool` 的 `vgpu-0`，`consumedCapacity` 反映 cores/memory 请求 |
| `kubectl get pod spike-remote-pod -o wide` | 被调度到匹配可达性标签的节点（而非 GPU 节点） |
| pod 状态 | **停在 ContainerCreating，事件报 DRA 驱动未注册/prepare 失败——这是 S0 的预期终点**（注入属 S1） |
| 份额语义（可选） | 再 apply 一份改名的 claim+pod：两个 claim 同时绑到 `vgpu-0`（`allowMultipleAllocations`），容量扣减正确；把请求撑到超过剩余容量则第二个 claim 不可分配 |
| 版本匹配（可选） | 给 claim 加 CEL selector `device.attributes["manager.nvidia.com"].cudaDriverVersion.isGreaterThan(semver("99.0.0"))`，应不可分配 |

## 清理

```bash
kubectl delete -f 20-claim-pod.yaml -f 10-resourceslice.yaml -f 00-deviceclass.yaml
kubectl label node <consumer-node> topology.kubernetes.io/zone-
```

## S0 之后：S1（注入链路，代码已就绪）

S1 用同一套 YAML，把"pod 停在 ContainerCreating"变成"pod 内远程 CUDA 跑通"。代码侧已实现
`--mode=inject`（`pkg/kubeletplugin/remote/`，需 feature gate `RemoteGPUSupport`），其余全手工：

1. **GPU 节点**：起 `lupine_driver_server`（进程级 `LD_PRELOAD=libvgpu-control.so` +
   `LUPINE_CHECKPOINT_LIBRARY` 指向同一 .so），用 `vgpu-session-config --session <claim-uid>
   --device <uuid>,mem=<MiB>,core=<pct>` 预先落盘会话配额（S1 令牌 = claim UID，创建 claim 后
   `kubectl get resourceclaim -o jsonpath='{.metadata.uid}'` 取值）。
2. **消费节点**：铺制品目录 `/var/lib/vgpu-manager/lupine/<cuda-ver>/`（放静态 client 的
   `libcuda.so.1`/`libnvidia-ml.so.1`，或镜像内 /artifacts 的 cp 产物）；以 host 网络运行
   `kubelet-plugin --mode=inject --feature-gates=RemoteGPUSupport=true --node-name=<node>`
   （挂 `/var/lib/kubelet/plugins_registry`、`/var/lib/kubelet/plugins`、`/var/run/cdi`）。
3. **用真插件替代手工 slice**：GPU 节点插件以 `--feature-gates=RemoteGPUSupport=true --remote-node-selector=topology.kubernetes.io/zone=az1`
   运行（endpoint 缺省 = 节点 InternalIP:14833；此时它只发布不注册），**GPU 节点上也要同时跑一个 `--mode=inject`**；删除手工 slice，
   DeviceClass 不变，再建 claim+pod。

验收：pod 内 CUDA 程序远程执行、`nvidia-smi`/`cuMemGetInfo` 呈限额视图、超限 OOM、
伪造 session 被 fail-closed 拒绝。S1 明确不验证：落盘时序屏障、回收、多池归并、TLS（见设计 §8.1）。
