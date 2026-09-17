# 设备插件 + 调度器路径的远程 GPU 集成：可行性与改造面分析 v0.2

> 状态：**六项决策已拍板（§8），待实施细则定稿**
> v0.2（2026-09-11，用户拍板）：抢占不实现（远程 Pod 在钩子处原样透传）；消费节点沿用 `vgpu-number` 原名；
> 单容器单远程服务器；拓扑只保留单服务器内 NVLink/NUMA；继续复用 `RemoteGPUSupport` gate（跨进程无法校验，
> 靠 nodeSelector 部署互斥）；新增两节：**device-monitor 适配（§5）** 与 **DRA 不可用集群的兼容（§6，含"版本够但特性门未开"
> 的四种情形与一处预先存在的 informer 版本缺陷）**。
> v0.1（2026-09-11）：首版可行性与改造面分析。
>
> 目标：在不改变现有本地设备调度逻辑/功能的前提下，让 `accessMode=remote` 的 Pod 被调度到无 GPU 的消费节点、
> 消费远程 GPU 节点的算力，并兼容不支持 DRA 的低版本集群。remote-server 部署形态不变（agent + lupine-server + monitor）。
> 复用 DRA 远程路径（`docs/remote_gpu_k8s_integration_design.md` D24/D26/D27/D28）已验证的 agent / 会话 / 制品机制。

---

## 0. 结论摘要

**可行。**核心发现三条，风险集中在两处。

- **数据模型天然对齐**：本地路径写进 Pod 预分配注解的 `device.DeviceClaim{Id, Uuid, Cores, Memory}`，与远程 agent
  `SessionStore.Materialize`（`pkg/remoteagent/session.go:380`）内部产出的会话配置**是同一个类型**。调度器在本地路径
  已经在生产"会话配置"所需的全部信息，远程化的本质只是把这份信息送到另一台机器上物化。
- **两个挂钩点是现成的**：`allocator.AllocationRequest.AccessMode` 已从注解读入（`request.go:343`）但无人消费；
  `PodMetricsNodeLabel`（`vgpu-manager.io/metrics-node`）是调度器写、监控读的"这个 Pod 由哪个节点上报指标"标签，
  全仓库只有 `pkg/client/kube_patch.go` 写、`pkg/metrics/informer.go:79` 读（见 §5.1）——把它指向远程服务器节点，
  监控侧几乎零改动。
- **抢占按决策不实现**，因此 v0.1 里最难的一块被移出范围，但**残留语义必须写进文档**（§4）。
- **风险一**：跨节点用量核算（Pod 落消费节点、吃 GPU 节点的卡）是唯一带侵入性的改动。
- **风险二**：DRA 不可用时 remote-agent 监听 DRA 资源会**卡住整个 remote-server pod**（§6），必须在 P1 之前解决。
  "不可用"共四种情形（版本过低、apiserver 特性门未开、只到 beta 版本、以及只有 scheduler/kubelet 门未开的
  **完全静默**组合），前三种能自动检测并快速失败，第四种只能靠部署前提与可观测性。

---

## 1. 现状事实梳理

### 1.1 本地路径（设备插件 + 调度器）的既有契约

| 环节 | 载体 | 代码位置 |
|---|---|---|
| 节点设备上报 | Node 注解 `node-device-register`（`[]DeviceInfo`，`Id` 即 GPU index/minor，含 `Healthy`）、`node-config-info`、`node-device-topology`、`node-gpu-domain`；Node 标签 driver/cuda 版本 | `vnum_plugin.go:192`、`manager/device.go:482` |
| in-tree 资源账本 | kubelet 扩展资源 `vgpu-number` / `vgpu-memory` / `vgpu-cores` | `pkg/deviceplugin/factory.go:90` |
| 节点级过滤 | `CheckNode`：`IsVGPUEnabledNode`（allocatable vgpu-number>0）+ register/config 注解齐备 + split/factor 合法 + 内存策略 | `filter_predicate.go:485` |
| NodeInfo 重建 | 节点注解 + **落在该节点上的** vGPU Pod 注解（按 `PodPlanSchedulingNode` 归属） | `NewNodeDeviceGatherInfo`、`pod_lister.go:80` |
| 设备级分配 | `allocator.Allocate(req)` → 命中即 `PatchPodPreAllocatedMetadata` 写 `pre-allocated` + `predicate-node` + `metrics-node`，并 `podLister.Mutation` 兜住 informer 滞后 | `filter_predicate.go:926` |
| 并发保护 | `f.locker` 串行化 live 分配（dry-run 不占锁） | `filter_predicate.go:963` |
| 节点侧兑现 | 设备插件 `Allocate` 读 `pre-allocated` → 映射本地 GPU → env/CDI/挂载 + 写 `vgpu.config` → `PatchPodAllocationSucceed` | `vnum_plugin.go:678` |
| 抢占 | extender `Preempt` 收到「候选节点 → victims」，`refineForNode` 重建该节点 NodeInfo、扣除 victims 后试分配 | `preempt_predicate.go:168/406` |
| 监控 | `nodeGPUCollector`（需 NVML）+ `ContainerLister`（扫 `<managerRoot>/<podUID>_<container>` 的 mmap 文件，按 `pod.Spec.NodeName` 过滤） | `node_gpu.go:52`、`container_lister.go:131` |

### 1.2 DRA 远程路径可直接复用的资产

| 资产 | 现状 | 复用方式 |
|---|---|---|
| agent 探测/发现/ServerInfo | 5s 探测 lupine-server，得 CUDA 版本 + 可路由 host + agent endpoint + bundle etag | **原样复用，不依赖 DRA** |
| `EnsureSession`/`ReleaseSessions`/token 鉴权 | 凭证 = "token 已登记在 claim 当前分配上" | 凭证来源换成 **Pod 注解** |
| `SessionStore.Materialize` | 入参 `(token, *ResourceClaim, *NodeDevices, requests)`，内部产出 `[]device.DeviceClaim` | 抽出中立入参（owner + `[]DeviceClaim`），claim/pod 两种来源各自适配 |
| 会话清扫（RV 守卫 + 分配作用域） | claim informer 事件 + 周期 sweep，标记文件记 claim UID + RV | 同构换成 pod informer + pod UID/RV + real-alloc 摘要 |
| `NodeDevices` 设备快照 | 由本节点 ResourceSlice 构造 | 新增"从 Node 注解 + 版本标签构造"（`DeviceInfo.Id` 即 minor，信息完全够） |
| client 制品选择 + bundle 下载校验 + `LUPINE_CLIENT_ETAG` | `pkg/kubeletplugin/remote/{artifacts,bundle,artifact_fetch}.go` | 消费侧插件直接调用（只换 claim 相关入参；导入 DRA 类型不发 API 调用，低版本集群无影响） |
| 会话 PID 归账工具 | `GetPidsByFilepath` / `lockPidsConfigShared` / `readPidsConfig`（`dra_remote.go:212-290`） | **纯文件操作，与 DRA 无关**，抽到公共文件后两条路径共用 |

---

## 2. 目标形态

```
GPU 节点（标签 vgpu-manager.io/remote-server=true）
 ├─ remote-server pod（形态不变）
 │   ├─ remote-agent      ← 新增 Pod 模式（会话凭证/清扫按 Pod 注解）
 │   │                       新增设备快照来源 = 本节点 node-device-register 注解
 │   │                       【关键】按 mode 决定监听哪些资源，低版本集群绝不碰 DRA API（§6）
 │   ├─ lupine-server     ← 不变
 │   └─ device-monitor    ← 新增：远程消费者按 Pod 注解反查 + 会话目录取 PID（§5）
 └─ device-plugin（现有 DaemonSet，新增"远程供给"模式）
     └─ 上报 node-device-register（含 accessMode 标记 + agent endpoint），
        并按 agent 探测结果置 DeviceInfo.Healthy

消费节点（无 GPU，标签 vgpu-manager.io/remote-consumer=true）
 └─ device-plugin（同一二进制，remote-only 模式，不初始化 NVML）
     ├─ ListAndWatch：上报 vgpu-number/vgpu-memory/vgpu-cores（原名，见决策②）
     └─ Allocate：读 Pod pre-allocated（含远程节点 + UUID）
                 → 调远程 agent EnsureSession
                 → 注入 LUPINE_SERVER/LUPINE_SESSION + client shim + ld.so.preload
                 （不注入任何 NVIDIA 设备/CDI）

调度器（同一进程，按 accessMode 分流）
 ├─ nodeFilter：remote 走"消费节点门禁"（不要求本节点有 GPU 注册）
 ├─ deviceFilter：从**远程服务器节点**的 NodeInfo 分配；用量按"设备归属节点"聚合
 ├─ 预分配注解：额外记录远程服务器节点名 + agent endpoint；metrics-node 指向服务器节点
 └─ Preempt：remote 直接 passthrough（决策④，§4）
```

**一句话概括数据流变化**：本地路径是"Pod 落哪个节点 = 设备来自哪个节点"；远程路径把两者解耦，
调度器必须同时决定"Pod 落哪个消费节点"和"设备取自哪台 GPU 服务器"，并把后者写进 Pod 注解，
供消费节点的设备插件、GPU 节点的 agent 与 monitor 三方对齐。

---

## 3. 可行性逐项评估

| # | 能力 | 可行性 | 主要难点 | 风险 |
|---|---|---|---|---|
| 1 | 节点过滤按 accessMode 分流 | 高 | `CheckNode` 拆 local/remote 两套门禁；`IsVGPUEnabledNode` 对纯消费节点不成立 | 低（纯增量分支） |
| 2 | 远程池容量核算 | 中 | Pod 用量必须归属"设备所在的 GPU 节点" | **中**（改动共享核算入口） |
| 3 | 分配与预留 | 高 | 复用 `f.locker` + `pre-allocated` 注解 + `Mutation`，机制现成 | 低 |
| 4 | in-tree 扩展资源账本偏差 | 中 | 消费节点 allocatable 是"本节点并发配额"，非真实远程容量 | 中（语义写进文档） |
| 5 | 会话生命周期（Pod 模式） | 中 | 设备插件 API **无释放回调**，释放以 agent 侧 Pod watch 为主 | 低（比 DRA 更简单） |
| 6 | 抢占 | — | 决策④不实现，passthrough | 低（残留语义见 §4） |
| 7 | 远程健康/可用性 | 高 | 复用 `DeviceInfo.Healthy=false`（`allocator.go:838` 已判） | 低 |
| 8 | client 制品 / CUDA 版本 | 高 | 复用 `pkg/kubeletplugin/remote` 现成逻辑 | 低 |
| 9 | 监控归账 | 高 | `metrics-node` 标签即现成挂钩点（§5） | 低 |
| 10 | 拓扑策略范围 | 中 | 决策⑤只保留单服务器内 NVLink/NUMA；跨消费节点 cross-pod 亲和性不做 | 低（范围已收窄） |
| 11 | **DRA 不可用集群兼容** | 中 | agent 监听 DRA 资源会卡死 ready 链路；四种"不可用"情形行为不同（§6） | **中高**（P1 前置） |

### 3.1 #2 用量核算：唯一的侵入性改动

`podLister.NodeMapByIndexValue` 按 `util.PodPlanSchedulingNode(pod)` 归组（`pod_lister.go:80`）。
远程 Pod 落在消费节点，却吃 GPU 节点的卡，因此需要**第二种归属函数**：

```
归属节点(pod) = accessMode=remote ? Pod 注解里的远程服务器节点名 : PodPlanSchedulingNode(pod)
```

实现建议：不改 `NodeMapByIndexValue` 语义，新增 `NodeMapByDeviceOwner`；本地池用旧的、远程池用新的，
本地路径零行为变化。**没有额外 list 开销**——现有实现本来就是一次性拿全量 vGPU Pod 再分组，只是分组键不同。

### 3.2 #4 扩展资源账本：必须明确的语义

消费节点上报的 allocatable 会被 in-tree `NodeResourcesFit` 先过一遍，它不可能等于真实远程容量
（多个消费节点各自上报）。语义只能定义为：

> **消费节点上报的数量 = 该节点允许的远程 vGPU 并发消费上限（per-node quota）**，
> 全集群真实容量由 extender 依据远程池裁决；两者之和允许超发。

附带好处是天然的单节点远程用量限流。代价是 Pod 可能过了 in-tree 过滤后被 extender 拒绝（与本地路径行为一致）。

### 3.3 #5 会话释放：设备插件路径反而更简单

设备插件 API 没有 `NodeUnprepareResources` 这类释放回调（DRA 独有）。因此：

- **主路径**：agent 侧 Pod informer——Pod 删除/进入终态 → 清扫其 token 对应会话（与 claim watch 完全同构）。
- **加速路径（可选）**：消费侧插件监听本节点 Pod 删除，best-effort 调 `ReleaseSessions`。
- **兜底**：周期 sweep（现成）。

单一事实来源相应变为：**token 必须出现在 Pod 的会话注解里，且与 Pod 当前 real-alloc 摘要匹配**
（对应 DRA 的 `allocation-id` 作用域），RV 守卫原样沿用。

---

## 4. 抢占：不实现（决策④）及其残留语义

**实现方式**：`Preempt` 入口在 `BuildAllocationRequest` 之后（`AccessMode` 此时已可读）判断
`req.AccessMode == util.AccessModeRemote` → 直接 `return passthrough(args)`。约 3 行，`passthrough` 已存在
（`preempt_predicate.go:732`，非 vGPU Pod 走的就是它）。

**为什么远程抢占语义天生混乱**（记录背景，便于将来重启该议题）：
extender 抢占协议是节点内闭合的——上游把 `NodeNameToMetaVictims` 映射回候选节点的 Pod 列表来定位真实 Pod，
本仓库 `findAdditionalVictims` 也据此跳过 `candidate.Spec.NodeName != nodeName` 的 Pod
（`preempt_predicate.go:632`）。而远程场景的稀缺资源在 GPU 服务器上，占用它的 Pod 散落在**多个消费节点**，
协议无法表达"驱逐 B 节点的 Pod 让 Pod 落到 A 节点"。要做只能自研驱逐 + 短期容量预留，属于新增分布式状态，
不宜与本次集成同期。

**必须写进用户文档的残留行为**：
"不实现远程抢占"≠"远程 Pod 不会触发任何抢占"。消费节点上报的 `vgpu-number` 是一个**普通扩展资源**，
当它耗尽时 kube-scheduler 的 in-tree 抢占照常触发、照常选该消费节点上的 victim；我们 passthrough 就意味着
**原样采纳 in-tree 的决定**。于是存在一种无效驱逐：in-tree 以为驱逐能腾出配额，驱逐后我们的 filter 仍因
远程池已满而拒绝该节点 → 驱逐白做。

> 如需彻底规避无效驱逐，可把 passthrough 改为"对 remote Pod 返回空 victims 映射"（= 明确拒绝抢占，远程 Pod 只排队）。
> 一行之差，但偏离"原样透传"的字面语义，**留给你后续决定**；本文按拍板结论先记 passthrough。

---

## 5. device-monitor 适配

### 5.1 关键发现：`metrics-node` 标签就是现成的挂钩点

device-plugin 路径的监控 Pod informer 用的是**标签选择器**而不是 `spec.nodeName`：

```go
// pkg/metrics/informer.go:79
options.LabelSelector = labels.Set{util.PodMetricsNodeLabel: nodeName}.String()
```

而 `PodMetricsNodeLabel`（`vgpu-manager.io/metrics-node`）全仓库只有三处关系：
`pkg/client/kube_patch.go` 写（预分配/分配中/分配成功三处）、`pkg/controller/reschedule/recovery.go:154` 清理、
`pkg/metrics/informer.go:79` 读。它的语义本就是"**这个 Pod 由哪个节点上报指标**"。

**结论**：远程 Pod 的该标签指向**远程服务器节点**，GPU 节点的 monitor 就能自动把远程消费者纳入视野，
**informer 作用域零改动**。对应的改动点是 `kube_patch.go` 的三个补丁函数——尤其
`PatchPodAllocationSucceed`（`kube_patch.go:114`）目前会用 `pod.Spec.NodeName` 覆盖该标签
（注释写着"Covering to correct certain possible errors"），远程 Pod 必须跳过这次覆盖，否则标签被改回消费节点、
GPU 节点的 monitor 立刻丢失该 Pod。

### 5.2 远程消费者的指标来源

| 数据 | 本地 Pod | 远程消费 Pod |
|---|---|---|
| 容器枚举 | `ContainerLister` 扫 `<managerRoot>/<podUID>_<container>` 的 mmap 文件，按 `pod.Spec.NodeName` 过滤 | **不适用**（容器目录在消费节点上）。按 Pod 的 real-alloc 注解直接枚举容器 |
| GPU 进程 PID | `cgroup.GetContainerPidsFunc` 读容器 cgroup | `<sessionBase>/<token>/pids.config`，复用 `GetPidsByFilepath` |
| vmem 记账 | 容器目录下 `vmem_node` | `<sessionBase>/<token>/.vmem_node`（与 DRA 远程路径同一布局） |
| 限额（memory/cores） | mmap 资源文件 | Pod real-alloc 注解里的 `DeviceClaim.Cores/Memory` |
| 设备维度指标 | NVML（本节点有卡） | NVML（卡就在本节点，无差别） |

`ContainerLister` 按 `pod.Spec.NodeName == c.nodeName` 过滤（`container_lister.go:134`），远程消费 Pod 天然被排除，
**不会去 mmap 不存在的目录**，这一段无需改动；远程分支走会话目录，与 `dra_remote.go` 完全同构。

### 5.3 改造方式

1. 把 `dra_remote.go` 里与 DRA 无关的会话工具（`GetPidsByFilepath`/`lockPidsConfigShared`/`readPidsConfig`）
   抽到 `pkg/metrics/collector/remote_session.go`，两条路径共用。
2. 新增 `node_remote.go`（挂在 `nodeGPUCollector` 上）：
   - `remoteConsumerPods()`：按 Pod real-alloc 注解中"uuid 带本节点前缀"的 `DeviceClaim` 筛出远程消费者；
   - `remoteSessionPIDs()`：从 Pod 会话注解取 token → 读会话目录 PID 与 `.vmem_node`；
   - 在 `Collect` 里按 Pod 是否远程走两条分支（结构参照 `dra_gpu.go:914-935`）。
3. **消费节点不需要部署 monitor**：卡在 GPU 节点、NVML 也在 GPU 节点，远程消费 Pod 只作为指标的 label
   出现在 GPU 节点的输出里。这与 DRA 远程路径的部署形态一致（monitor 只在 `remote-server.yaml` 里）。
   顺带好处：消费节点无 GPU，`nodeGPUCollector` 依赖的 `nvidia.DetectionDeviceLib` 本来就会失败。

### 5.4 待验证

- 远程模式下库把 mmap 资源数据写在哪里（会话目录 vs 容器目录）。若在会话目录，则限额类指标可以不依赖 Pod 注解、
  直接读会话目录，精度更高；实测确认后再定。

---

## 6. 低版本 / DRA 未启用集群的兼容（新增，P1 前置）

### 6.1 问题：agent 在 DRA 不可用的集群上会卡死整个 remote-server pod

现状 agent 无条件建两个 DRA informer（`pkg/remoteagent/agent.go:174/188`）：

```go
cache.NewListWatchFromClient(a.cfg.ClientSets.Resource.RESTClient(), "resourceslices", ...)
cache.NewListWatchFromClient(a.cfg.ClientSets.Resource.RESTClient(), "resourceclaims", ...)
```

`resource.k8s.io` 不可用时，reflector 不会 panic，而是**无限重试 + 刷日志**，于是 `hasReady` 永不为真 →
**就绪文件永不写出** → `remote-server.yaml` 里 lupine-server 容器卡在 `until [ -f /run/vgpu/ready ]` →
**整个 pod 起不来**。这是本方案在此类集群上的**硬阻塞**。

### 6.1.1 "DRA 不可用"其实有四种，行为各不相同

| 情形 | API 组是否被 serve | agent informer | 能否自动检测 |
|---|---|---|---|
| **A. 集群版本太低**，无 `resource.k8s.io` | 否 | 404 无限重试 → **hang** | 能（discovery / 探测调用） |
| **B. apiserver 的 DRA 特性门未开** | 否（API 启用与特性门绑定） | 同 A → **hang** | 能，与 A 同一处理 |
| **C. DRA 已开但仍是 beta**（只 serve `v1beta1`/`v1beta2`） | 是，但非 v1 | **仍然 hang**，原因见 §6.2 | 能（但需读协商结果） |
| **D. apiserver 开了、scheduler / kubelet 的门未开** | 是（v1 正常） | 完全正常、agent 就绪 | **不能**——API 层面全健康 |

情形 D 是唯一静默的：claim 能创建能 watch，但**永远不会被分配**（scheduler 的 DRA 插件没开）或
**永远不会被 prepare**（kubelet 的 DRA manager 没开）。其中 kubelet 门未开还算有信号（我们的 kubelet-plugin
向 kubelet 注册 DRA 插件会失败，起不来）；**scheduler 门未开是完全静默的组合**，表现只是 Pod 一直 Pending。
任何 discovery / API 探测都查不出来，只能靠运维前提 + 可观测性（§6.3 第 4 条）。

### 6.2 顺带发现：`RESTClient()` 绕过版本协商（预先存在，**已按拍板修复**）

> **实施状态（2026-09-11）**：§6.2/§6.3 的启动校验与 informer 版本协商**已落代码**：
> - `pkg/client/dra_api.go`：`DRAServedVersions` + `DRAAPIRequirement.Check`（含单测）；
> - `cmd/remote-agent/main.go`：**声明只支持 v1**，启动即校验，覆盖情形 A/B/C（理由：驱动用 consumable
>   capacity，必须 1.34+，beta 版本对我们无用）；
> - `cmd/device-monitor/main.go`：`--enable-dra-monitor` 做 A/B 校验（任意版本可用即通过）；
> - `pkg/metrics/informer.go`：slice/claim informer 改为走 draclient 的版本协商（`draListerWatcher`），
>   并用上游 `cache.ToListWatcherWithWatchListSemantics` 保持 watch-list 语义判断不变。


`k8s.io/dynamic-resource-allocation/client`（v0.37.0）**本身有版本协商**：typed 方法
（`ResourceClaims()` / `ResourceSlices()`）从 v1 起，遇错逐级回落到 v1beta2 / v1beta1 并记住结果
（`client/generic.go:247-266`，`CurrentAPI()` 可读当前版本）。**但 `Client.RESTClient()` 永远返回
`clientSet.ResourceV1().RESTClient()`**（`client/client.go:71`），绕过了协商。

agent 的两个 informer 正是用 `RESTClient()` 建的 → 在情形 C（DRA 已开、但只到 beta，例如部分 1.32/1.33 集群）下，
**agent 的 typed 调用可用、informer 却会 404 hang**。即：当前 DRA 路径的真实版本门槛是"`resource.k8s.io/v1`
已 GA 的集群"，而不是"任何支持 DRA 的集群"。这条独立于本次改造，**需要单独拍板**是否修
（修法：informer 改用协商到的版本对应的 REST client，或显式声明只支持 v1 并在启动时校验）。

### 6.2 全组件 DRA 依赖审计（已核实）

| 进程 | 是否触碰 `resource.k8s.io` | 低版本集群风险 |
|---|---|---|
| **remote-agent** | **是（无条件）** | **硬阻塞，必须改** |
| device-monitor | 仅 `--enable-dra-monitor` 分支内（`main.go:178`） | 无（关掉即不碰） |
| device-webhook | 仅 DRA 转换相关开关下（claim/template/class 的 webhook 注册与 reader） | 低（需确认 reader/cache 只在开关打开时构建） |
| **device-scheduler** | **否** | 无 |
| **device-plugin** | **否** | 无 |
| kubelet-plugin（DRA 驱动） | 是（本体） | 低版本集群不部署它 |

**好消息**：调度器与设备插件这两个本次改造的主角完全不碰 DRA API，所以低版本兼容的工作量集中在 agent 一处。

### 6.3 方案：agent 按角色选择监听源

```
--session-owner=claim   （DRA 路径，默认，行为与今天完全一致）
    监听：ResourceSlice（设备快照） + ResourceClaim（会话归属/清扫）
--session-owner=pod     （设备插件路径）
    监听：Node（本节点 node-device-register 注解 = 设备快照） + Pod（会话归属/清扫）
    完全不建任何 resource.k8s.io 的 informer / 不发任何该组的 API 调用
```

配套要点：

1. **显式 flag 优先，不做隐式自动降级**：与既有 "gate + mode 两层正交"（设计文档 D21）一致。
   `RemoteGPUSupport` gate 仍表示"启用远程能力"，具体行为由进程角色 + 该 flag 决定（决策⑥）。
   **不要做"探测到没有 DRA 就自动切 pod 模式"**：两种模式的会话归属语义不同，静默切换会让一个配错的集群
   表面上跑起来、实际与调度侧对不上账。
2. **claim 模式加启动前置校验**，把情形 A/B/C 全部变成"启动即失败 + 可操作的提示"，而不是 hang：
   - 先发一次 typed 探测调用（如 `ResourceSlices().List(limit=1)`），它会走 draclient 的协商；
   - 组不存在 / NotFound → 失败，提示"本集群未提供 `resource.k8s.io`（版本过低或 apiserver 未开 DRA 特性门），
     请改用设备插件路径 `--session-owner=pod`"（覆盖情形 A、B）；
   - 成功但 `CurrentAPI()` 不是 `V1` → 失败，提示"集群协商到 `<版本>`，而 informer 仅支持 v1"（覆盖情形 C，
     并与 §6.2 的修法二选一）；
   - 成功且为 `V1` → 正常建 informer（今天的行为）。
3. **情形 D 不做硬门禁**：API 层面查不出来，误判代价高（空集群同样没有 claim）。只做两件事——
   写进部署前提文档；给 dra-server 加一条可观测性告警（发布 slice 超过 N 分钟却从未观察到本驱动的 claim 被分配
   → warning 事件/日志）。**是否要做这条告警留给你定。**
4. **Pod watch 用标签收窄**：pod 模式不需要全量 Pod。`metrics-node` 标签（§5.1）已由调度器指向 GPU 服务器节点，
   agent 可以直接用 `LabelSelector: metrics-node=<本节点>` 建 informer，watch 规模与该节点的远程消费者数量同阶。
   注意 `recovery.go:154` 会清理该标签，那会表现为一次 delete 事件 → 触发会话清扫，与"Pod 被回收重调度"的语义一致，
   属于期望行为（实施时补测试确认）。
5. **RBAC 可以不分家**：RBAC 规则不校验 API 组是否存在，remote-server 的 ClusterRole 继续同时写
   `resource.k8s.io` 与 `nodes/pods` 都无副作用；真正的差异只在"建不建 informer"。
6. **proto 兼容**：`EnsureSessionRequest` 目前带 `claim_*` 字段。建议新增中立的
   `SessionOwner{kind, uid, namespace, name, resource_version}` 字段，agent 优先读它、缺失时回落到 `claim_*`，
   这样已部署的 DRA inject 侧不受影响。

### 6.4 设备插件路径本身对集群版本的要求：没有新增门槛

这是本方案对低版本集群最重要的结论——远程消费路径**只用到设备插件 API 最古老、最普适的能力**：

| 依赖 | 引入版本 | 说明 |
|---|---|---|
| `Allocate` 返回 Envs + Mounts | device plugin v1beta1，很早 | 远程注入只需要这两样（LUPINE_SERVER/LUPINE_SESSION + shim 挂载） |
| `GetPreferredAllocation` | k8s 1.19 | 本地路径已在用，非新增 |
| **不需要 CDI** | — | 远程 Pod 没有 NVIDIA 设备要注入，绕过了 CDI 对 containerd 1.7+ / kubelet 1.28+ 的要求 |
| 调度器 extender | 很早 | 本地路径已在用 |
| kubelet pod-resources API（仅 monitor） | v1 自 1.20 | 本地路径已在用 |

即：**远程设备插件路径不引入任何高于本地路径的 k8s 版本要求**，这正是它相对 DRA 路径的核心价值。

其他注意事项：

- `pkg/kubeletplugin/remote` 里的制品/bundle 工具**导入** `k8s.io/api/resource/v1` 类型，
  但只要不发该组的 API 调用，低版本集群上无任何影响（编译期依赖 ≠ 运行期依赖），可以放心复用。
- 部署层面：低版本集群不安装 DRA 相关的 CRD/webhook/DeviceClass，chart 需要能整体关闭这一组对象；
  `device-webhook` 的 claim/template/class webhook 注册与 reader 必须确认只在 DRA 转换开关打开时构建。

---

## 7. 改造面清单

### 7.1 新增（不触碰现有逻辑）

| 位置 | 内容 | 规模估计 |
|---|---|---|
| `pkg/deviceplugin/remote/` | 消费侧插件：ListAndWatch 上报远程容量；Allocate 做远程注入（EnsureSession + shim + env）；无 NVML | ~600 行 |
| `pkg/deviceplugin/remote/` | GPU 侧"远程供给"上报增量：register 注解带 accessMode/agent endpoint、按 agent 探测置 `Healthy` | ~150 行 |
| `pkg/remoteagent/` | Pod 模式：凭证校验、Pod informer 清扫、会话注解常量、`--session-owner` 与 discovery 校验（§6） | ~450 行 |
| `pkg/remoteagent/` | `NodeDevices` 从 Node 注解构造 | ~120 行 |
| `pkg/scheduler/filter/` | 远程节点门禁 + 远程池 NodeInfo 构建 + 远程分配分支 | ~400 行 |
| `pkg/metrics/collector/node_remote.go` + `remote_session.go` | 远程消费者反查 + 会话 PID/vmem 归账（§5.3） | ~300 行 |
| `deploy/deviceplugin-remote/` | 新部署形态 yaml（消费侧 DaemonSet；GPU 侧沿用 remote-server.yaml） | — |

### 7.2 现有文件的增量改动点

| 文件/函数 | 改动 | 影响本地路径？ |
|---|---|---|
| `filter_predicate.go` `CheckNode`/`nodeFilter` | 按 `req.AccessMode` 选门禁集合；remote 分支不要求本节点 GPU 注册 | 否（新分支） |
| 同上 `preFilterNodeInfos` | remote 时以远程服务器节点为 NodeInfo 主体，用量按设备归属分组 | 否（新分支） |
| 同上 `deviceFilter` | 命中后额外写远程服务器节点名/endpoint | 否 |
| `pkg/client/pod_lister.go` | 新增 `NodeMapByDeviceOwner` | 否（新方法） |
| `pkg/client/kube_patch.go` | 远程 Pod 的 `metrics-node` 指向 GPU 服务器节点，且 `PatchPodAllocationSucceed` 不得覆盖（§5.1） | **是（需回归）** |
| `preempt_predicate.go` `Preempt` | remote → `passthrough`（§4） | 否（前置分支） |
| `pkg/remoteagent/session.go` `Materialize` | 入参抽成中立结构（owner + `[]DeviceClaim`） | 否（DRA 侧同步适配） |
| `pkg/remoteagent/agent.go` `Run` | 按 `--session-owner` 选 informer（§6.3） | 否（DRA 模式行为不变） |
| `pkg/api/remoteagent/api.proto` | 新增 `SessionOwner`，保留 `claim_*` 兼容 | 否 |
| `pkg/deviceplugin/base/plugin_server.go` | `DeviceManager` 变可选/接口，供无 NVML 插件复用注册骨架 | 低（重构，行为不变） |
| `pkg/metrics/collector/dra_remote.go` | 抽出公共会话工具 | 否（纯移动） |

### 7.3 护栏

`pkg/scheduler/filter`（既有测试跑 123s，覆盖很厚）、`preempt`、`pkg/device/allocator`、`pkg/client` 的既有测试
全绿即可证明本地路径未被破坏；`kube_patch.go` 的改动尤其要盯 `kube_patch_test.go`。远程分支各自补新测试。

---

## 8. 决策记录（已拍板）

| # | 决策 | 结论 | 说明 |
|---|---|---|---|
| ① | 节点本地/远程二选一 vs 同卡混合供给 | **先二选一** | 与 DRA 路径 v2.1 一致；混合供给在决策②下其实已变便宜（用量都按 uuid 聚合，`NodeMapByDeviceOwner` 天然能合并两个来源），留后续 |
| ② | 消费节点资源名 | **沿用 `vgpu-number` 等原名** | Pod YAML 完全不变；`IsVGPUResourcePod`/`BuildAllocationRequest`/索引器零改动。约束：一个节点只能有一个 device-plugin 进程注册该资源名 |
| ③ | 单容器能否跨多台远程服务器 | **先单服务器** | `<nodeName>/<uuid>` 编码不阻断将来扩展；跨服务器时各会话内 slot（host minor）可能撞号，语义需单独验证 |
| ④ | 抢占 | **不实现**，remote Pod 在钩子处 `passthrough` | 语义混乱（§4）；残留的 in-tree 无效驱逐风险需写进用户文档 |
| ⑤ | 远程池拓扑范围 | **只保留单服务器内 NVLink/NUMA** | 与决策③配套；跨消费节点 cross-pod 亲和性不做 |
| ⑥ | feature gate | **继续用 `RemoteGPUSupport`** | DRA 与设备插件是不同进程，无法跨进程校验；同名不产生歧义，行为由"进程角色 + flag"决定；部署靠 nodeSelector 互斥 |

> 关于决策⑥的残留风险与可选缓解：跨进程虽无法直接校验，但**可以通过 API 对象间接互检**——
> 设备插件的远程上报侧启动时若发现本节点已有本驱动发布的 `accessMode=remote` ResourceSlice，
> 或 dra-server 发现本节点 `node-device-register` 带远程标记，即判定"两条路径重叠"并拒绝启动/告警。
> 这是对"nodeSelector 配错"唯一能自动发现的护栏，成本约 50 行。**是否要做，留给你定。**

---

## 9. 分阶段计划

| 阶段 | 内容 | 产出判据 |
|---|---|---|
| **P0 契约冻结** | 注解/编码格式定稿（远程节点名编码、Pod 会话注解、real-alloc 摘要、`metrics-node` 语义扩展）；agent `--session-owner` 与 discovery 校验设计（§6） | 本文档升级为设计定稿 |
| **P1 数据面直通** | agent Pod 模式 + 低版本兼容 → GPU 侧上报 → 调度器远程分配 → 消费侧 Allocate 注入 → 单 Pod 端到端 | 低版本集群上 remote-server pod 正常就绪；Pod 里 `nvidia-smi` 看到远程会话视图 |
| **P2 正确性加固** | 跨节点用量核算、健康摘除、会话清扫/RV 守卫、制品版本与 bundle 下载 | 并发多 Pod 不超分；杀 lupine-server 后设备被摘除；Pod 删除后会话回收 |
| **P3 监控** | §5 的 monitor 远程分支 + `metrics-node` 语义改动回归 | GPU 节点导出远程消费 Pod 的 `container_vgpu_*` 指标，label 指向消费 Pod |
| **P4（可选）** | 同卡混合供给（决策①的另一半）、决策⑥的重叠互检护栏 | 同一张卡上本地与远程 Pod 共存且账目一致 |

工作量粗估：P1 约 1 周，P2 约 1 周，P3 约 2～3 天，P4 另议。

---

## 10. 已识别的坑（实施时逐条核对）

1. **`DeviceClaim` 文本格式是位置式四段** `id_uuid_cores_memory`（`types.go:234`），加第五段会让老解码器直接
   `len(split) != 4` 报错。远程节点名要么编码进 uuid 字段（`<nodeName>/<uuid>`，节点名不含 `_`，安全），
   要么另开 Pod 注解。**不要改位置式格式的字段数。**
2. **`IsVGPUEnabledNode` 看的是 allocatable**（`util.go:108`），纯消费节点在插件起来前不满足，
   remote 门禁要用别的判据（远程标签 + 远程资源 allocatable）。
3. **无 NVML 启动**：`NewDeviceManager` 必走 `nvidia.DetectionDeviceLib`（`manager/device.go:233`）并失败，
   消费侧插件不能复用 `DeviceManager`；`basePluginServerImpl` 需让 manager 可选。
4. **设备插件无释放回调**，会话回收必须以 agent 侧 Pod watch 为主。
5. **kubelet 设备 ID 不透明**：沿用本地路径 `fakeDeviceUUID` 的套路（ID 只做计数，真实映射看 Pod 注解），
   但要注意设备插件 checkpoint 与 `GetPreferredAllocation` 的交互。
6. **agent token 鉴权强度不变**：token 写在 Pod 注解上，集群内有 Pod 读权限者可见（挡网络访问者、不挡集群内读者），
   文档需照抄 DRA 路径的这条边界。
7. **`metrics-node` 语义扩展有回归面**：它同时被 `reschedule/recovery.go` 清理，改动后要确认恢复流程仍正确。
8. **dry-run filter 可复用**：`FilterDryRun`（`filter_predicate.go:213`）已为 CA extender 存在，
   将来若重启远程抢占议题，仿真判定直接借用，别另写一套。
9. **DRA 不可用集群**：见 §6。agent 是唯一硬阻塞点（调度器与设备插件本身完全不碰 `resource.k8s.io`）；
   远程设备插件路径不引入任何高于本地路径的 k8s 版本要求（§6.4）。
10. **`draclient.RESTClient()` 绕过版本协商**（`client.go:71` 恒返回 v1 REST client）。预先存在的缺陷，
   **已按拍板处理**：agent 声明 v1-only 并启动校验，device-monitor 的 informer 改走协商版本（§6.2）。
