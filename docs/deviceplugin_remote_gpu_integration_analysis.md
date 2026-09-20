# 设备插件 + 调度器路径的远程 GPU 集成：可行性与改造面分析 v0.5

> 状态：**以 §16 为准**（v0.5，2026-09-15：S1 调度器已按用户方向落地；init/sidecar、会话模型、监控分析与三项拍板）。
> §15.2 契约表与 §15.3 的 S1 表已被 §16.1 取代，保留作历史记录。
> v0.4：实施顺序定稿，见 §15（不做命名改造、不做抽象重构，直接实现 remote-gpu）
> v0.3（2026-09-14）：新增 §11 设备后端抽象、§12 分阶段落点、§13 待拍板决策、§14 命名规范化方案。
> 已拍板：消费侧走**方案 P（消费侧设备插件）**；**先做抽象重构再加远程**（§13 ⑦⑧）。
> 两条重要更正：① kubelet 准入会**丢弃**节点未上报的扩展资源（`removeMissingExtendedResources`），
> 所以"消费节点零上报 + `ignoredByScheduler`"在 kubelet 层面可行，决策②的原论据作废，消费侧形态重新列为决策⑦；
> ② 集群外接入不抄 HAMi 的 session stub，走我们自己的 D19（正常调度的远程 GPU pod 即中继）。
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
   出现在 GPU 节点的输出里。这与 DRA 远程路径的部署形态一致（monitor 只在 GPU 服务器那个 Pod 里）。
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
**就绪文件永不写出** → GPU 服务器 Pod 里 lupine-server 容器卡在 `until [ -f /run/vgpu/ready ]` →
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
--session-owner=auto    （两种会话同时服务，2026-09-16 用户拍板新增）
    两套 informer 都建；集群不提供 DRA API 时只打印日志并跳过 claim 那一半，不退出服务
```

配套要点：

1. **显式 flag 优先**：与既有 "gate + mode 两层正交"（设计文档 D21）一致。
   `RemoteGPUSupport` gate 仍表示"启用远程能力"，具体行为由进程角色 + 该 flag 决定（决策⑥）。
   原则仍然是"不静默切换语义"，但**降级的触发权交给运维**：只有显式配 `auto` 的 agent 才会在无 DRA API 时
   自己少服务一半；配 `claim` 的仍然启动即失败。之所以敢这么做，是因为两种会话的归属语义不再由进程模式决定
   —— 每个请求自己带 `owner`（见下），claim 与 pod 的鉴权、设备快照、清扫各走各的路，同时服务也不会串味。
2. **claim 模式加启动前置校验**，把情形 A/B/C 全部变成"启动即失败 + 可操作的提示"，而不是 hang：
   - 先发一次 typed 探测调用（如 `ResourceSlices().List(limit=1)`），它会走 draclient 的协商；
   - 组不存在 / NotFound → 失败，提示"本集群未提供 `resource.k8s.io`（版本过低或 apiserver 未开 DRA 特性门），
     请改用设备插件路径 `--session-owner=pod`"（覆盖情形 A、B）；
   - 成功但 `CurrentAPI()` 不是 `V1` → 失败，提示"集群协商到 `<版本>`，而 informer 仅支持 v1"（覆盖情形 C，
     并与 §6.2 的修法二选一）；
   - 成功且为 `V1` → 正常建 informer（今天的行为）；
   - `auto` 模式下这三种失败都只打印 `Skipping claim sessions: <原因>` 并继续，只服务 pod 会话。
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
| `deploy/classic-remote/` | 新部署形态 yaml（消费侧 DaemonSet；GPU 侧沿用 dra-remote 的 gpu-server 形态） | — |

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

---

## 11. 设备后端抽象（v0.3 新增，目标：调度器清晰可扩展）

### 11.1 现状的好消息：打包引擎已经是设备无关的

`pkg/device/allocator` 全包**没有任何 nvml / nvidia 依赖**，它只认 `device.NodeInfo` 里的一袋
`*device.Device`（核数、显存、份数、NUMA、链路拓扑、健康位）以及请求里的 uuid/type 过滤。
也就是说"怎么打包、怎么按拓扑对齐、怎么排优先级"这层**不需要为远程或 AMD 改动**。

设备专有的其实只有五件事：

| # | 设备专有的事 | 今天写死在哪 |
|---|---|---|
| ① | 哪些资源名/注解属于我，这个 Pod 是不是我的 | `allocator.BuildAllocationRequest`（读 `vgpu-number` 等） |
| ② | 候选节点够不够格 | `filter.CheckNode`（要求本节点有 GPU 注册注解） |
| ③ | 打包对象（库存）从哪来 | `device.NewNodeDeviceGatherInfo`（读本节点 `node-device-register`） |
| ④ | 定下来之后往 Pod 上写什么 | `allocator.Allocate` 尾部写 `pre-allocated` + `predicate-node` |
| ⑤ | 节点侧怎么兑现 | `vnum_plugin.Allocate`（CDI/挂载/env/写 vgpu.config） |

抽象就画在这五处，其余全部共享。这也是"逻辑清晰"的来源：**调度器主干只剩一条链路，
设备差异全部收敛到一个接口的实现里。**

### 11.2 调度器侧接口 `Backend`

```go
// pkg/device/backend/backend.go
//
// Backend 是调度器能放置的一类设备：本节点的 NVIDIA vGPU、同一批卡经 lupine 上网后的远程
// vGPU、将来的 AMD。打包引擎（pkg/device/allocator）共享且保持设备无关；后端只提供引擎无从
// 知道的四件事：哪些请求归我、哪些节点够格、往什么库存里打包、定了之后写什么。
type Backend interface {
    Name() string                       // 日志/指标/事件里的身份
    Resources() ResourceNames           // 本后端拥有的扩展资源名（注册表拒绝重名）
    Annotations() AnnotationKeys        // 本后端自己的注册/预分配/已分配注解键

    // Request 把 Pod 解析成本后端的分配请求；不是我的请求返回 nil。
    // 同一个 Pod 只能被一个后端认领（registry.Resolve 保证）。
    Request(pod *corev1.Pod) *allocator.AllocationRequest

    // Admit 是节点级门禁：这个候选节点能不能承载该 Pod。
    // 本地：节点自己注册了 GPU；远程：节点是消费节点且至少有一台健康服务器可达。
    Admit(node *corev1.Node, req *allocator.AllocationRequest) *reason.FilterReason

    // Inventory 给出候选节点上可以打包的库存。本地恰好一项（本节点自己）；
    // 远程是"这个消费节点可达的每台服务器各一项"，所以返回切片。
    Inventory(ctx context.Context, node *corev1.Node,
        req *allocator.AllocationRequest, snap *Snapshot) ([]*Placement, *reason.FilterReason, error)

    // Commit 把选中的放置结果变成节点侧读得回来的元数据补丁。
    Commit(pod *corev1.Pod, p *Placement, claims device.PodDeviceClaim) (map[string]*string, error)
}

// Placement 把"往哪份库存里打包"和"Pod 实际落在哪个节点"分开。
// 本地两者相同；远程两者不同——这正是这个类型存在的唯一理由。
type Placement struct {
    Inventory *device.NodeInfo  // 打包对象：本地 = 本节点，远程 = 某台服务器
    RunsOn    string            // Pod 实际落点，写进 predicate-node
    Context   map[string]string // 后端自带上下文（远程：server / agent endpoint）
}
```

`Snapshot` 是一次调度周期内共享的只读视图（全量 vGPU Pod 列表、节点列表、缓存的库存），
由 filter 构造一次传给所有后端，避免每个后端各自 list 一遍。**用量归属也交给后端**：
本地按 `PodPlanSchedulingNode` 归组，远程按 Pod 注解里的服务器节点归组——
这比在共享的 `podLister` 上加第二个归组函数更内聚（§3.1 的方案相应调整）。

filter 主干化简成一条可读的链路：

```go
be := registry.Resolve(pod)              // 没有后端认领 = 不是我们的 Pod，直接放行
req := be.Request(pod)
for _, node := range candidates {
    if r := be.Admit(node, req); r != nil { failed[node.Name] = r; continue }
    places, r, err := be.Inventory(ctx, node, req, snap)
    for _, p := range sortPlacements(req, places) {
        claims, rsn, err := allocator.New(p.Inventory).Allocate(req)   // 共享引擎
        if rsn != nil { continue }
        patch := be.Commit(pod, p, claims)
        client.PatchPodMetadata(...)                                   // 共享写入
    }
}
```

### 11.3 节点侧接口 `Realizer`（设备插件内）

`vnum_plugin.Allocate` 现在是 270 行的大函数，把"找到当前 Pod、解析预分配注解、逐容器循环、
写 real-alloc、patch 成功"这些**共享骨架**和"挂驱动、写 vgpu.config、CDI"这些**本地专有**混在一起。
拆成：

```go
// pkg/deviceplugin/realize/realize.go
type Realizer interface {
    Name() string
    // Devices 上报给 kubelet 的设备列表（ID 对我们只是计数用的占位符）
    Devices() []*pluginapi.Device
    // Realize 把一个容器已记录的 claim 兑现成 kubelet 应答：
    // 本地挂驱动/写配置；远程先 EnsureSession 再注入 LUPINE_SERVER/LUPINE_SESSION 与 client shim。
    Realize(ctx context.Context, pod *corev1.Pod, claim *device.ContainerDeviceClaim,
        resp *pluginapi.ContainerAllocateResponse) error
}
```

骨架（找 Pod、循环、状态机 patch）只此一份，两种设备各自实现 `Realize`。

### 11.4 借鉴 HAMi 什么、不借鉴什么

| HAMi 的做法 | 我们 | 理由 |
|---|---|---|
| 一个 `Devices` 接口 + 注册表按设备类型分发 | **借鉴** | 正是"以后接 AMD"要的形状 |
| 每个后端自带注解命名空间（`InRequestDevices` / `SupportDevices` 按类型注册） | **借鉴**（`Annotations()`） | AMD 的节点注册注解不该挤进 NVIDIA 的键 |
| 后端自己判断"这个容器的请求是不是我的" | **借鉴**（`Request` 返回 nil） | 比在主干里 if-else 判资源名清晰 |
| 资源名来自配置而非常量 | **借鉴** | 我们已有 `--domain` 改域名，思路一致 |
| `DeviceInfo.CustomInfo map[string]any` 逃生舱 | **暂不**，等第二家厂商真的需要再加 | 类型不安全；我们读写两端都自己控制，YAGNI |
| `Fit` 返回 `(bool, map[...], string)`，失败原因是字符串 | **不借鉴** | 我们已有结构化 `reason.FilterReason`（带 Code + 明细 + 指标归类），比字符串强 |
| `CheckHealth(devType, node) (bool, bool)` 两个匿名 bool | **不借鉴** | 语义靠注释猜；健康状态我们直接落在 `DeviceInfo.Healthy` 上 |
| `PatchAnnotations(pod, *map[string]string, ...)` 指针改 map | **不借鉴** | `Commit` 返回补丁，纯函数好测 |
| 包级可变全局注册表（`InRequestDevices` 等 `var`） | **不借鉴** | 改成显式 `Registry` 对象，单测不用清理全局状态 |
| `LockNode/ReleaseNodeLock` 挂在设备接口上 | **不借鉴** | 我们的串行化在 filter（`f.locker`）与 bind 层，与设备无关，不该下放 |

### 11.5 引入抽象的风险与边界

- **必须零行为变化**：本地 NVIDIA 后端只是把现有代码搬进接口实现，不改逻辑。
  `pkg/scheduler/filter`（跑 123s 的重测试）、`preempt`、`allocator` 三个包的既有用例全绿即为通过判据。
- **不动的部分**：allocator 内部（打包/拓扑/NUMA/优先级/gang/cross-pod）、bind、串行锁、事件与指标骨架。
- **`allocator.Allocate` 需要一处小改**：它现在把 `predicate-node` 写成 `nodeInfo.GetName()`，
  等于假设"设备所在节点 = Pod 落点"。远程下二者不同，且 `FilterAllocatingPods` 与 bind 都要求
  `predicate-node == pod.Spec.NodeName`。改法：把落点作为显式入参（`Placement.RunsOn`），
  默认仍是库存节点名——本地行为不变。

---

## 12. 实施落点（按阶段，替代 §9 的粗粒度版本）

> 阶段之间可独立验证；每阶段结束时全仓库 `go build / vet / test -race` 必须绿。

### P0 契约冻结（无代码）
拍板 §8 与 §13 的决策；定稿注解/编码：远程服务器节点名的载体、Pod 会话令牌注解键、
real-alloc 摘要（对应 DRA 的 allocation-id）、`metrics-node` 语义扩展、节点角色标签。

### P1 抽象骨架（**不改任何行为**）
| 落点 | 动作 |
|---|---|
| `pkg/device/backend/`（新包） | `Backend` / `Placement` / `Snapshot` / `Registry` / `ResourceNames` / `AnnotationKeys` |
| `pkg/device/backend/local/`（新包） | 把 ①②③④ 的现有实现搬进来：`Request` = 现 `BuildAllocationRequest`；`Admit` = 现 `CheckNode`；`Inventory` = 现 `NewNodeInfo`；`Commit` = 现 `Allocate` 尾部的注解拼装 |
| `pkg/scheduler/filter/filter_predicate.go` | `nodeFilter`/`deviceFilter` 改为经 `registry` 调用后端；主干只剩链路 |
| `pkg/device/allocator/allocator.go` | `Allocate` 不再自己写 `predicate-node`，由 `Commit` 决定（默认值不变） |
| `pkg/scheduler/preempt/preempt_predicate.go` | `refineForNode` 的节点门禁与库存重建改走后端 |
| 判据 | filter/preempt/allocator 既有测试全绿，无新行为 |

### P2 远程后端（调度器侧）
| 落点 | 动作 |
|---|---|
| `pkg/device/backend/remote/`（新包） | `Request`（accessMode=remote 才认领）、`Admit`（消费节点门禁 + 有健康服务器）、`Inventory`（按可达服务器各建一份 NodeInfo，用量按服务器归属聚合）、`Commit`（pre-alloc + 服务器节点 + endpoint + 会话令牌） |
| `pkg/scheduler/reason/reason.go` | 新增远程专用拒绝码（无可达服务器 / 服务器不健康 / 消费节点未启用等） |
| `pkg/scheduler/preempt/` | remote 请求在入口 `passthrough`（决策④） |
| 判据 | 单测：远程 Pod 选中服务器并写对注解；本地 Pod 行为不变；跨节点用量不重复计算 |

### P3 GPU 侧上报（remote-server 节点）
| 落点 | 动作 |
|---|---|
| `pkg/deviceplugin/`（新增 remote-serve 模式） | 节点带远程标签 → 不向 kubelet 注册，只写注册注解；按 agent `ServerInfo` 置 `DeviceInfo.Healthy`，并写入 server endpoint / agent endpoint / server CUDA 版本 / client bundle etag |
| `cmd/device-plugin/options` | 模式解析：**标签是唯一权威**，配置与标签冲突时报错（借鉴 HAMi `resolveOperatingMode`） |
| `deploy/` | GPU 节点沿用 `dra-remote/vgpu-manager-dra-gpu-server.yaml` 形态（agent + lupine-server + monitor） |

### P4 消费侧兑现（形态取决于决策⑦）
| 落点（方案 P：消费侧设备插件） | 动作 |
|---|---|
| `pkg/deviceplugin/realize/`（新包） | 抽出 `Realizer` 骨架，本地实现平移 |
| `pkg/deviceplugin/realize/remote/` | EnsureSession（5s 超时）+ 注入 env/挂载 + ld.so.preload |
| `pkg/deviceplugin/base/plugin_server.go` | `DeviceManager` 变为可选（仅 3 处耦合：`GetDeviceManager`/`AddNotifyChannel`/`RemoteNotifyChannel`） |
| `cmd/device-plugin/main.go` | remote-only 模式：跳过 NVML 初始化 |
| 方案 W（webhook + downward API + init 容器）若中选 | 落点改为 `pkg/webhook/pod/mutate` 注入 env/init 容器/emptyDir；EnsureSession 屏障移到 scheduler bind；分配状态机需要新的完成信号 |

### P5 agent Pod 模式与会话
| 落点 | 动作 |
|---|---|
| `pkg/api/remoteagent/api.proto` | 新增中立 `SessionOwner{kind,uid,namespace,name,resourceVersion}`，保留 `claim_*` 兼容 |
| `pkg/remoteagent/agent.go` | `--session-owner=pod` 时监听 Node（设备快照）+ Pod（会话归属，标签选择器收窄），**不建任何 DRA informer**（§6） |
| `pkg/remoteagent/session.go` | `Materialize` 入参中立化（owner + `[]DeviceClaim`）；标记文件记 owner UID/RV |
| `pkg/remoteagent/` | 新增"从 Node 注解构造 `NodeDevices`" |

### P6 监控
`pkg/metrics/collector/remote_session.go`（公共会话工具外提）+ `node_remote.go`（远程消费者反查、会话目录取 PID/vmem），
`pkg/client/kube_patch.go` 让远程 Pod 的 `metrics-node` 指向服务器节点且不被覆盖。

### P7 部署与文档
`deploy/classic-remote/`、`charts/vgpu-manager` 增补、README 与已知边界（含 §4 的抢占残留语义）。

---

## 13. 新增待拍板决策（v0.3）

| # | 决策 | 选项 | 我的建议 |
|---|---|---|---|
| ⑦ | **消费侧形态** | (P) 消费侧设备插件；(W) webhook + downward API + init 容器（HAMi 式，已确认 kubelet 准入可行） | ✅ **已拍板：P**（2026-09-14）。注入对用户透明（不改 Pod spec）、D2 屏障天然落在容器创建前、复用既有 Allocate 状态机与制品缓存；代价是消费节点要跑 DaemonSet。W 的优点是消费节点零组件、制品每次从 server 现拉不用缓存，但要把屏障搬到 bind、另找状态机完成信号，且 Pod spec 被注入 init 容器 |
| ⑧ | 抽象重构时机 | (a) 先做 P1 再加远程；(b) 边加远程边抽 | ✅ **已拍板：(a) 先重构后远程**（2026-09-14）。P1 零行为变化、有厚测试护栏；混在一起做会让"远程引入的 bug"和"重构引入的 bug"无法区分 |
| ⑨ | 后端注册键命名 | `nvidia-local` / `nvidia-remote` / 将来 `amd`；或单 `nvidia` 后端内部按 accessMode 分支 | **前者**。远程与本地的门禁、库存、写回都不同，合成一个后端会把 if-else 搬回主干 |
| ⑩ | filter 返回一个节点还是全部可行节点 | (a) 沿用本地行为：选中一个消费节点即返回；(b) 返回全部可行消费节点，交 kube-scheduler 打分，bind 时再定 `predicate-node` | **(a) 先行**。(b) 更符合 k8s 语义（CPU/内存/亲和性参与选点），但要改 bind 与预分配时序，留作后续 |
| ⑪ | 服务器选择策略 | 复用 `node-scheduler-policy`（binpack/spread）作用在服务器维度，`device-scheduler-policy` 作用在卡维度 | 同意复用，语义天然对应 |
| ⑫ | 消费节点候选顺序 | (a) 第一个通过门禁的；(b) 剩余远程配额最多的（spread） | **(b)**，避免所有远程 Pod 堆在同一个消费节点上 |

---

## 14. 命名规范化方案（v0.3 新增，待拍板）

### 14.1 问题定位

今天 `pkg/util/consts.go` 里 **54 个键全部由同一个 `globalDomainName` 派生，默认值是 `nvidia.com`**，
并且这个域名可以用 `--domain` 在运行时整体替换（`MustInitGlobalDomain` → `initConstants()` 重新赋值 54 个包级变量）。
另有 5 个键硬编码 `vgpu-manager.io/`、5 个硬编码 `nvidia.com/`。三套并存，没有规则。

**根因是把两类语义混成了一个域名：**

| 类别 | 语义归属 | 今天 | 应该 |
|---|---|---|---|
| 扩展资源名 | **厂商**的东西，用户和 kubelet 都按厂商理解 | `<domain>/vgpu-number` | 厂商域名 `nvidia.com/...`，且**按后端可配** |
| 注解 / 标签 | **我们项目**的契约（调度策略、分配记录、节点注册） | 同一个 `<domain>/...` | 项目域名 `<proj>/...`，**固定不可配** |

所以会出现 `nvidia.com/node-scheduler-policy` 这种键：策略是我们调度器的概念，对 AMD 一样适用，却占着 NVIDIA 的域名。

### 14.2 资源名方案（采纳你的提议，加一条限定）

```
<厂商域名>/<设备类>[.<维度>]
nvidia.com/vgpu             份数（原 vgpu-number）
nvidia.com/vgpu.core        算力（原 vgpu-cores）
nvidia.com/vgpu.memory      显存（原 vgpu-memory）
nvidia.com/mig-1g.5gb       MIG：保持与 NVIDIA 官方插件一致，不改
amd.com/vgpu                将来
ascend.com/vnpu             将来（用厂商自己的术语，不强行统一成 vgpu）
```

合法性已核对：k8s 的 `IsQualifiedName` 允许名字段含点（`[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?`，≤63），
`requests.nvidia.com/vgpu.core` 作为 ResourceQuota 键同样合法；NVIDIA 官方的 `nvidia.com/mig-1g.5gb` 就是先例。
点号读作"vgpu 的某个维度"，比连字符更贴切。

**限定一：资源名必须可按后端覆盖，不能写死。**（借鉴 HAMi：资源名来自调度器 ConfigMap）
理由有二：① `nvidia.com/vgpu` 是在 NVIDIA 的域名里定义我们自己的语义，属于轻度"抢注"——
HAMi 直接抢 `nvidia.com/gpu` 已经造成"同名不同义"的混乱（他们的是可切分，NVIDIA 的是整卡），
我们至少要用 `vgpu` 这种明确属于我们的名字，并给用户改名的余地；
② 同一集群里可能同时装着 NVIDIA 官方插件，留出改名口子才能避免撞名。

**限定二：远程不另起资源名。** 远程 Pod 仍请求 `nvidia.com/vgpu`，靠 accessMode 注解区分（决策②已定），
这样本地/远程的 Pod YAML 完全一致。

### 14.3 注解 / 标签方案（两种候选都不建议，给第三种）

你给的两种：

| 候选 | 问题 |
|---|---|
| `scheduling.device.manager.io/node-scheduler-policy` | 前缀里有 `device`、键里又是 `node`，两处对象名打架；且 `scheduler` 在 `scheduling.` 前缀下是冗余词 |
| `scheduling.node.manager.io/scheduler-policy` | 对象进前缀不冗余，但**把一个特性拆成多个前缀**：node/device/memory/topology 四个知悉同一件事的键分散在四个前缀里，文档、grep、RBAC、webhook 白名单都要写四遍 |

**建议：一个关注点一个前缀，对象放在键里**，这正是 k8s 自己的写法
（`topology.kubernetes.io/zone`、`scheduling.k8s.io/group-name`、`pod-security.kubernetes.io/enforce`）：

```
# ① 用户输入 · 影响“落在哪”          （公共 API，用户可写，webhook 校验）
scheduling.<proj>/node-policy              ← node-scheduler-policy
scheduling.<proj>/device-policy            ← device-scheduler-policy
scheduling.<proj>/memory-policy            ← memory-scheduler-policy
scheduling.<proj>/topology-mode            ← device-topology-mode
scheduling.<proj>/cross-pod-topology
scheduling.<proj>/stuck-grace-period
scheduling.<proj>/access-mode              ← vgpu-access-mode（local/remote 决定落点）
scheduling.<proj>/include-device-uuid      ← include-gpu-uuid（去掉 gpu，面向多设备）
scheduling.<proj>/exclude-device-uuid
scheduling.<proj>/include-device-type
scheduling.<proj>/exclude-device-type

# ② 用户输入 · 影响“怎么跑”
runtime.<proj>/compute-policy              ← vgpu-compute-policy
runtime.<proj>/memory-oversold             （现为容器 env，可一并规范）

# ③ 系统写入的 Pod 状态（用户不应写，webhook 可直接拒绝整个前缀）
state.<proj>/pre-allocated
state.<proj>/real-allocated
state.<proj>/predicate-node
state.<proj>/predicate-time
state.<proj>/assigned-phase        (label)
state.<proj>/metrics-node          (label)
state.<proj>/allocation-id         远程：分配摘要
state.<proj>/session.<hash16>      远程：会话令牌（对应 DRA 侧 session-<hash>）

# ④ 节点侧发布（设备插件写，按厂商分键，天然支持多设备共存）
node.<proj>/devices.nvidia         ← node-device-register
node.<proj>/config.nvidia          ← node-config-info
node.<proj>/topology.nvidia        ← node-device-topology
node.<proj>/domain.nvidia          ← node-gpu-domain
node.<proj>/remote.nvidia          远程：server/agent endpoint + server CUDA 版本 + bundle etag（新增）
node.<proj>/heartbeat

# ⑤ 内部
system.<proj>/ignore-webhook
system.<proj>/scheduler-role       (label)
```

三个要点：

1. **`scheduler` 一词在 `scheduling.` 前缀下删掉**（`node-policy` 而非 `node-scheduler-policy`），
   信息不丢、长度减半。
2. **厂商作为键的点号后缀**（`devices.nvidia` / `devices.amd`），不是前缀。
   这样"所有节点发布物"一个前缀可枚举，同时 AMD 插件写自己的键不碰 NVIDIA 的。
3. **按角色分前缀（输入 / 状态 / 节点发布）**，让"用户可以写什么"成为可执行规则：
   webhook 可以直接拒绝用户设置 `state.*`，恢复逻辑可以按前缀清理，文档可以按前缀成章。
   这是当前一锅端的命名做不到的。

### 14.4 项目域名怎么选

这是最贵、最难回退的一项，因为它是写进用户 YAML 的公共 API。

| 候选 | 评价 |
|---|---|
| `vgpu-manager.io` | 已在 5 个键里使用、与仓库/产品同名、零额外决策成本；缺点是将来纳管 NPU/TPU 时 "vgpu" 名不副实 |
| `device-manager.io` / `xpu-manager.io` | 面向多设备更贴切；但等于再做一次全量改名，且这类通用域名容易与他人概念撞车 |
| `manager.io`（你的示例里用的） | **不建议**：k8s 惯例是用自己控制的域名，`manager.io` 是个真实可注册的通用域名，语义上也太泛 |

**建议**：除非确定要改项目名，否则选 `vgpu-manager.io`。多花一次改名的代价，远高于"vgpu 三个字母不够泛"的代价；
而且 `vgpu` 也可以读作 "virtual device"（业界 vGPU 已泛指虚拟化设备）。这一项请你直接拍。

### 14.5 顺带能删掉的东西

固定项目域名后，`--domain` flag、`globalDomainName` 包级可变变量、`MustInitGlobalDomain`、
`initConstants()`（运行时重算 54 个变量）**整套机制可以删除**，54 个 `var` 变回真正的 `const`。
资源名则改由后端配置提供（§11.2 的 `Backend.Resources()`）。这既是命名规范化的收益，
也直接服务于"调度器清晰易懂"：没有运行时可变的全局键名，测试不用关心初始化顺序。

> 代价：`--domain` 是现有用户可见的 chart 值（`.Values.globalDomain`），删除是破坏性变更，需写进升级说明。
> 若要保留改名能力，建议只保留**资源名**可配（按后端），注解/标签域名固定。

### 14.6 迁移路径（改名是 API 破坏，必须有过渡）

| 步骤 | 内容 | 关键约束 |
|---|---|---|
| M1 | 新增 `pkg/util/naming`（或 `apis/`）集中定义新键，提供 `LookupAnnotation(obj, new, legacy...)` 双读助手 | 只加不改，先合入 |
| M2 | 全仓库改为**写新键、读新旧两套** | 节点注解由插件重启即完成重写；Pod 注解为短生命周期，升级期间双读即可 |
| M3 | **标签要特别处理**：`metrics-node` 被 monitor 的 informer 用作 **label selector**（selector 无法 OR 两个键），升级窗口内必须**新旧标签同时写** | 否则升级瞬间 monitor 会看不到存量 Pod |
| M4 | 资源名：**只上报新名**，由 webhook 在准入时把旧名请求重写成新名 | 不要新旧同时上报——同一份容量挂两个资源名会被 kubelet 和 ResourceQuota 重复计算 |
| M5 | 一到两个小版本后删除旧键与双读代码 | |

> 时机建议：命名规范化**与 §12 的 P1 抽象骨架同期、但作为独立提交**先落。
> 两者都是"零行为变化 + 有厚测试护栏"的机械改动，混在一起会让 review 无从下手；
> 但都必须赶在 P2 远程后端之前，否则远程代码要跟着改两遍。

---

## 15. 定稿：不做重构、不改命名的实施顺序（v0.4）

> 2026-09-14 用户拍板：**① 暂不做命名/域名改造**（沿用现有 `globalDomainName` 常量）；
> **② 暂不做设备后端抽象重构**（§11/§12-P1 推迟，不废弃，将来再做）；**③ 直接实现 remote-gpu**；
> **④ 消费侧设备插件只上报 `vgpu-number`，不上报 `vgpu-cores` / `vgpu-memory`**。

### 15.1 决策④ 为什么成立（已核实）

消费节点只上报 `vgpu-number`，而 Pod 仍然可以在 limits 里写 `vgpu-memory` / `vgpu-cores`：

- **kube-scheduler**：我们的 chart 对这两个资源已经设了 `ignoredByScheduler: true`
  （`charts/vgpu-manager/templates/scheduler/configmap.yaml`），in-tree NodeResourcesFit 本来就不看它们；
- **kubelet**：准入前先跑 `removeMissingExtendedResources`，**节点完全没上报的扩展资源会被丢弃**，不参与适配；
- **调度器**：`BuildAllocationRequest` 仍从容器 limits 读取 cores/memory，远程分配照常按显存/算力切分；
- **限额**：仍由服务器侧会话配置强制执行，与是否上报资源无关。

即：少上报两个资源**不影响任何功能**，只是少两份 kubelet 账本。反而更干净——远程容量不在消费节点上，
本来就不该由消费节点的 allocatable 去描述显存和算力。

**`vgpu-number` 必须上报**（不能也省掉），两个原因：① `IsVGPUEnabledNode` 看的就是它的 allocatable；
② chart 里它是 `ignoredByScheduler: false`，in-tree 要靠它识别抢占候选。它的数值语义 = **本节点远程并发上限**（§3.2）。

> 配套动作：消费侧插件要复用 `CycleCleanupNodeResources` 把 `vgpu-cores` / `vgpu-memory` 从本节点 status 里清掉，
> 避免节点曾经跑过本地插件时残留旧值。

### 15.2 契约冻结（P0，需你确认下列默认值）

> **v0.5：本表已被 §16.1 取代。** 实际落地没有新增任何 Pod 注解，`predicate-node` 直接等于服务器节点。

利用决策③（单容器单服务器），**`DeviceClaim` 的位置式文本格式完全不用动**：设备 UUID 不加节点前缀，
另用一个注解说明这批 UUID 属于哪台服务器。这样 §10 坑#1 自动消失。

| 用途 | 建议键（沿用现域名，默认 `nvidia.com`） | 值 |
|---|---|---|
| 远程服务器节点 | `<domain>/remote-server-node` | 节点名。**用量归属就读它**；`predicate-node` 保持 = 消费节点（`FilterAllocatingPods`/bind 都要求） |
| lupine-server 地址 | `<domain>/remote-server-endpoint` | `http://host:14833`，调度时从节点发布物快照下来，避免节点侧再查 |
| agent 地址 | `<domain>/remote-agent-endpoint` | `grpc://host:14834` |
| 会话令牌（每容器） | `<domain>/remote-session.<hash16(容器名)>` | 随机令牌，镜像 DRA 侧 `session-<hash16(partitionKey)>` 的做法 |
| 分配作用域摘要 | `<domain>/remote-allocation-id` | 服务器节点 + real-alloc 的摘要，令牌随分配作废（对应 DRA 的 `allocation-id`） |
| 节点发布的远程信息 | `<domain>/remote-endpoints`（Node 注解） | JSON：`{server, agent, serverCudaVersion, bundleEtag}`，由 GPU 侧插件按 agent `ServerInfo` 写 |
| 角色标签 | GPU 侧沿用 `vgpu-manager.io/remote-server=true`；消费侧 `vgpu-manager.io/remote-consumer=true` | **标签是唯一权威**：插件模式由标签决定，与配置冲突时报错退出（借鉴 HAMi `resolveOperatingMode`） |
| metrics-node | 远程 Pod 指向**服务器节点** | 且 `PatchPodAllocationSucceed` 不得用 `pod.Spec.NodeName` 覆盖（§5.1） |

### 15.3 实施顺序（每步独立可验证，全仓库测试须绿）

**S1 · 调度器：远程分配（最小闭环）**

| 落点 | 动作 |
|---|---|
| `pkg/device/remote/`（新包，调度器侧） | 远程池：从带 `remote-server=true` 标签的节点读 `node-device-register` + `remote-endpoints`，按服务器建 `device.NodeInfo`；用量按 Pod 的 `remote-server-node` 注解归属；不健康服务器（`DeviceInfo.Healthy=false` 或 endpoint 缺失）剔除 |
| `pkg/scheduler/filter/filter_predicate.go` | `nodeFilter`：`req.AccessMode==remote` 时走消费节点门禁（有 `remote-consumer` 标签 + `IsVGPUEnabledNode`），跳过 register/config 注解检查；`deviceFilter`：remote 时以服务器 NodeInfo 为打包对象，按 node-policy 排序选服务器，命中后写注解 |
| `pkg/device/allocator/allocator.go` | `Allocate` 尾部不再固定写 `predicate-node = nodeInfo.GetName()`，改为可传入落点（本地默认值不变，零行为变化） |
| `pkg/client/pod_lister.go` | 新增 `NodeMapByDeviceOwner`（远程按 `remote-server-node`，本地按 `PodPlanSchedulingNode`） |
| `pkg/client/kube_patch.go` | 远程 Pod 的 `metrics-node` 指向服务器节点且不被后续 patch 覆盖 |
| `pkg/scheduler/preempt/preempt_predicate.go` | 入口判 `AccessMode==remote` → `passthrough`（决策④，约 3 行） |
| `pkg/scheduler/reason/reason.go` | 新增远程拒绝码 |
| 判据 | 单测：远程 Pod 选中健康服务器、写对全部注解、跨节点用量不重复计；本地路径既有测试全绿 |

**S2 · GPU 侧设备插件：远程供给模式**

| 落点 | 动作 |
|---|---|
| `cmd/device-plugin/options` + `pkg/deviceplugin/factory.go` | 节点带 `remote-server` 标签 → 远程供给模式：**不向 kubelet 注册任何资源**，只写节点注册注解 |
| `pkg/deviceplugin/`（远程供给） | 周期调本机 agent `ServerInfo`（复用 `pkg/kubeletplugin/remote.ServerInfo`），写 `remote-endpoints` 注解；agent 不可用或 server 未监听 → 把该节点 `DeviceInfo.Healthy` 全部置 false |
| 判据 | GPU 节点不再上报 `vgpu-number`；杀掉 lupine-server 后调度器不再把远程 Pod 放到该节点 |

**S3 · agent：Pod 模式会话**

| 落点 | 动作 |
|---|---|
| `pkg/api/remoteagent/api.proto` | 新增中立 `SessionOwner{kind,uid,namespace,name,resourceVersion}`，保留 `claim_*` 字段兼容已部署的 DRA inject |
| `pkg/remoteagent/agent.go` | `--session-owner=pod`：监听 Node（设备快照）+ Pod（用 `metrics-node=<本节点>` 标签选择器收窄），**绝不建 DRA informer**；claim 模式保持现状并保留 §6 的 v1 启动校验 |
| `pkg/remoteagent/session.go` | `Materialize` 入参中立化（owner + `[]DeviceClaim`）；标记文件记 owner UID/RV；`NodeDevices` 增加"从 Node 注解构造" |
| `pkg/remoteagent/` | 清扫按 Pod：Pod 删除/终态 → 回收其令牌对应会话；令牌须匹配 `remote-allocation-id` |
| 判据 | 低版本集群（无 DRA）上 remote-server pod 正常就绪；Pod 删除后会话目录回收 |

**S4 · 消费侧设备插件（方案 P）**

| 落点 | 动作 |
|---|---|
| `pkg/deviceplugin/base/plugin_server.go` | `DeviceManager` 变可选（仅 3 处耦合：`GetDeviceManager` / `AddNotifyChannel` / `RemoveNotifyChannel`） |
| `pkg/deviceplugin/remote/`（新插件） | 只注册 `vgpu-number`（数量 = `--remote-vgpu-count`）；`Allocate`：复用**已经是导出函数**的骨架（`client.GetActivePodsOnNode` → `util.FilterAllocatingPods` → `util.GetCurrentPodByAllocatingPods` → `device.GetCurrentPreAllocateContainerDevice` → `device.UpdatePodRealContainerDeviceClaim` → `client.PatchPodAllocationSucceed`），中间换成：签发/读取会话令牌 → 调 agent `EnsureSession`（5s 超时）→ 注入 `LUPINE_SERVER`/`LUPINE_SESSION` + client shim 挂载 + `/etc/ld.so.preload` |
| `cmd/device-plugin/main.go` | 消费模式跳过 NVML 初始化（`NewDeviceManager` 必失败）；启动时若发现本节点有本地 GPU 注册注解则拒绝启动（角色互斥） |
| `pkg/kubeletplugin/remote/{artifacts,bundle,artifact_fetch}.go` | 复用制品选择与 bundle 下载；**下载不得放在 `Allocate` 里**（kubelet `HandlePodAdditions` 串行准入，会阻塞本节点其他 Pod），改为插件启动时/后台预热 |
| 判据 | 远程 Pod 在无 GPU 节点上跑起来，容器内 `nvidia-smi` 看到远程会话视图 |

**S5 · 监控** — 见 §5.3（公共会话工具外提 + `node_remote.go` 远程消费者反查）。
**S6 · 部署与文档** — `deploy/classic-remote/`、chart 增补、README 已知边界（含 §4 抢占残留语义、§3.2 配额语义）。

### 15.4 推迟但不废弃

§11 的 `Backend` / `Realizer` 抽象与 §14 的命名规范化都**保留在文档里**，等远程路径跑通后再单独立项。
为了将来重构成本可控，S1/S4 的新代码请尽量收进 `pkg/device/remote/` 与 `pkg/deviceplugin/remote/` 两个新包，
在既有文件里只留**薄分支**（判 `AccessMode` 后调用新包），不要把远程逻辑铺进 `filter_predicate.go` / `vnum_plugin.go` 的主干。

---

## 16. v0.5：S1 实际落地、init/sidecar 与会话/监控分析

> 2026-09-15。S1 按用户方向重写后落地（分支 `feat/remote-gpu-deviceplugin`：`d2d1afe`、`151224a`、`00123f5`、`3b98472`）。
> 本节是后续 S2–S6 的依据；与 §15 冲突时以本节为准。

### 16.1 S1 实际契约（取代 §15.2 表与 §15.3 S1 表）

原则：**复用已有链路与注解，不新增 Pod 注解**。

| 项 | 实际做法 |
|---|---|
| 设备归属 | 远程 Pod 在服务器节点上走原有 `nodeFilter` → `deviceFilter` → allocator，`predicate-node` = **服务器节点** |
| 用量归属 | `util.PodPlanSchedulingNode`：本地已绑定返回 `Spec.NodeName`；远程 Pod **始终返回 `predicate-node`**。节点 NodeInfo、pod lister 分组、监控 informer 索引都自动归到服务器 |
| metrics-node | 预分配、bind 时写 `predicate-node`（服务器）；`PatchPodAllocationSucceed` 对远程 Pod 保持服务器（§16.4 D2） |
| 节点发布物 | 服务器节点注解 `<domain>/remote-endpoints`，JSON `remotegpu.ServerEndpointInfo{serverEndpoint, agentEndpoint, serverCudaVersion, bundleEtag}`；地址须可路由、带端口，server 为 http/https、agent 为 grpc |
| 角色标签 | `<domain>/remote-server`、`<domain>/remote-consumer`（随 `globalDomainName`） |
| 服务器判定 | 标签为 `true` **且**存在 `remote-endpoints` 注解（`util.IsRemoteServerNode`）；注解不可解析时 `CheckNode` 报 `NodeBadRemoteEndpoint` |
| 消费节点判定 | 标签为 `true` **且** `vgpu-number` allocatable > 0（`util.IsRemoteConsumerNode`） |
| 设备插件取地址 | 由 `predicate-node` 查服务器 Node，读 `remote-endpoints` 注解 |

调度流程（`filter()` 远程分支）：

```
kube-scheduler 候选（Pod 可运行的节点）
   │ remoteConsumerNodes：保留消费节点，其余 NodeNotRemoteConsumer
   ▼
nodeLister（服务器标签）+ 本次候选 → remoteServerNodes：留 IsRemoteServerNode 且在役的，按名字排序
   │ 原有 nodeFilter → deviceFilter（全局锁、allocator、预分配，predicate-node=服务器）
   ▼
remoteFilterResult：
   有服务器放得下 → 返回全部消费节点，由 kube-scheduler 打分选定
   都放不下       → 每个消费节点报 NoRemoteServer / RemoteServerUnfit，详情为各服务器原因
```

- **本地 Pod 行为不变**：仍只返回预分配成功的那一个节点；`nodeFilter` 与 preempt 拒绝服务器节点（`NodeIsRemoteServer`）。
- **服务器在役判定（污点，2026-09-17）**：只看**集群自己打的**硬污点——`node.kubernetes.io/*` 与
  `node.cloudprovider.kubernetes.io/*` 前缀下的 `NoSchedule`/`NoExecute`（cordon/drain、not-ready、unreachable、
  out-of-service、各类资源压力、云侧 shutdown），未被 Pod 容忍即不再往该服务器放**新**会话，已有会话不受影响。
  刻意**不看**运维为隔离打的业务污点（如 `nvidia.com/gpu=true:NoSchedule`）：远程 Pod 根本不在服务器上运行，
  而且同一份 tolerations 还决定 Pod 自己落在哪个消费节点，用它来放行服务器会顺带放宽 Pod 的落点。
  `PreferNoSchedule` 是偏好不是拒绝，按 kube-scheduler 的 TaintToleration 同样过滤掉。
- **候选节点也参与服务器发现**：除 nodeLister 外，本次请求带来的候选节点里的服务器也会被纳入（同名以 lister 的副本为准），
  这样 dry-run（Cluster Autoscaler 扩容仿真）里还不存在的服务器节点也能被选中。
- **bind**：远程 Pod 只要求 `predicate-node` 非空（绑定到消费节点）；本地仍要求等于绑定节点。
- **抢占**：远程 Pod 原样透传。
- **跨 Pod 拓扑**：远程 Pod 关闭，仅 live Filter 发 `TopologyFallback` 事件；单服务器内 NVLink/NUMA 保留。
- **回归验证**：基线 `3f497be` 与 HEAD 各跑 5 遍 224 组策略组合（节点策略 × 设备策略 × 拓扑模式 × 显存策略 + 跨 Pod gang），
  每组 12 个 Pod，全部至少一次逐字一致（含具体卡号）；两边各有约 30 组因 `filterDevices` 遍历 map 而结果不稳定，属原有行为。

### 16.2 init / sidecar：调度侧已兼容

| 环节 | 事实 |
|---|---|
| 请求构建（`request.go:337-439`） | 先 init（sidecar 标 `Restartable`）后业务容器；`Total = sidecar 之和 + max(业务之和, 单个 init 最大)` |
| 分配（`allocator.go:100-235`） | 有顺序 init 时两轮：先 sidecar + 业务（累加），释放业务预留后逐个放 init（优先 Pod 已用的卡，失败再全节点）；`pre-allocated` 每个 vGPU 容器一段，init 在前 |
| 记账（`types.go:1500-1579`） | 每卡峰值 `sidecar + max(业务, init)`，只看 spec 与注解；远程 Pod 在服务器 NodeInfo 上同公式 |
| 消费节点 `vgpu-number` | kubelet device manager 把顺序 init 的设备 ID 留给后续容器复用，sidecar 的不复用，与上面的峰值一致 |

结论：调度侧对 init、sidecar、多容器、重启无需额外处理。

### 16.3 设备插件路径的会话模型（已拍板）

| 决策 | 内容 |
|---|---|
| 粒度 | **每个容器一个会话**，init 不复用业务容器会话（同 DRA NRI 模式） |
| 会话键 | `<podUID>_<containerName>`（即 `util.NRIPartitionKey`） |
| token | **对会话键做哈希得到，不写 Pod 注解**，算法与 DRA 侧会话键哈希保持一致；作为设备插件路径统一的 token 约束 |
| 建立时机 | Pod 的**第一次 `Allocate`** 按 `pre-allocated` 一次 RPC 建好该 Pod 全部容器的会话，后续容器只取结果 |
| 回收 | agent 侧 Pod informer：Pod 删除/终态 → 回收；周期清扫兜底；不依赖设备插件回调 |

为什么按容器、不复用：
1. `pre-allocated` 按容器给卡与 cores/memory，init 可能落在别的卡、限额也可能不同，合不进一份会话配额。
2. 服务器按会话汇总 `pids.config` 用量（`cuda_hook.c:2363`）；sidecar 与业务容器同时运行，共用会话会共享一份限额，与"sidecar 与业务相加"的调度记账不符。
3. 顺序 init 退出即断开连接，lupine 子进程退出、显存释放后业务容器才启动；同时存在的会话占用之和不超过调度峰值。
4. 容器重启会话键不变，直接复用。

为什么首次 `Allocate` 批量建：kubelet 在 Pod 准入时串行调用各容器 `Allocate`，每次 `EnsureSession` 最多 5s，
逐个建会让准入最坏耗时 5s × 容器数，并阻塞同节点其他 Pod 准入。

token 由会话键确定性派生，带来的约束（S3/S4 实现时遵守）：
- **token 不是秘密**：知道 Pod UID 与容器名即可算出。agent 签发/物化会话前必须从 Pod informer 校验：
  Pod 存在且未终止、`predicate-node` = 本节点、容器名在 `pre-allocated` 中，配额只取该容器的声明。
  lupine-server 与 agent 端口的访问控制仍需保留。
- **监控可直接算出会话目录**：`<sessionBase>/<hash(podUID_container)>`，无需额外元数据文件。
- **会话建好后 `pre-allocated` 不会再变**：会话只在 Pod 绑定后的 `Allocate` 里创建，绑定后调度器不再处理该 Pod；
  预分配只可能在绑定前变化，那时还没有会话。准入失败的 Pod 由 reschedule 控制器删除重建，UID 变了 token 也就变了；
  容器重启沿用原会话。所以 `Materialize` 做到"已存在即复用"即可。

### 16.4 设备插件侧必改点（S4，除 D2 外）

| # | 位置 | 问题与改法 |
|---|---|---|
| D1 | `util.FilterAllocatingPods`（`util.go:436-439`） | 要求 `predicate-node == Spec.NodeName`，远程 Pod 被丢弃，消费节点 `Allocate` 找不到 Pod。需按访问模式分支 |
| D2 | `PatchPodAllocationSucceed`（`kube_patch.go:112-115`） | 用 `Spec.NodeName` 覆盖 `metrics-node`，服务器监控随即丢失该 Pod。**远程 Pod 保持服务器**（已拍板，首个实施步骤） |
| D3 | `GetPreferredAllocation`（`vnum_plugin.go:445`） | 按 GPU UUID 映射本地设备 ID；消费节点只有虚拟 ID，应直接走默认选择 |
| D4 | `PreStartContainer`（`vnum_plugin.go:1148-1160`） | 重写本地 `vgpu.config` 并删除 `pids.config` 等；远程容器应跳过，绝不触碰服务器会话 |
| D5 | `Allocate` 返回内容 | 远程注入 `NVIDIA_VISIBLE_DEVICES=void`、`LUPINE_DISABLE_LOCAL=1`、`LUPINE_SERVER`、`LUPINE_SESSION`、远程 `ld.so.preload` 与客户端文件挂载。**客户端文件改为按需下载**（2026-09-16，取代"须预置"）：节点没有匹配 shim、或 etag 与服务器当前内嵌的不一致时，从该服务器的 agent 拉 bundle 装好再用，与 DRA 侧同一套逻辑 |
| D6 | reschedule 控制器（`reschedule.go:85`） | 判定分配失败的依据是否适用远程 Pod，S4 核对 |

`Allocate` 的容器游标（`GetCurrentPreAllocateContainerDevice`，取第一个未写入 `real-allocated` 的容器）与 kubelet
init 在前的调用顺序一致，对 init/sidecar 无需改动。

### 16.5 监控（S5）

| 指标 | 服务器监控 | 消费节点监控 |
|---|---|---|
| 节点/卡已分配量 | 已能统计（watch `metrics-node=服务器` + `PodPlanSchedulingNode`），前提 D2 | 不统计（无 GPU） |
| 卡级 `access_mode` | 写死 `local`（`node_gpu.go:593-620`），需按 Pod 区分 | — |
| 容器级实时用量 | **缺失**：`container_lister.go:135` 只留 `Spec.NodeName==本节点`；PID 取本机 cgroup；限额读本机 `<uid>_<容器>` 目录 | 无 |

S5 改法参照 DRA（`dra_remote.go`、`dra_gpu.go`）：远程 Pod 的 PID 从
`<sessionBase>/<token>/pids.config`（共享文件锁）读取后与本机 NVML 进程匹配；虚拟显存读 `<token>/.vmem_node`，
限额读 `<token>/config/vgpu.config`；label `node=服务器`、`pod_node=消费节点`、`access_mode=remote`。
init/sidecar 沿用 `CollectableContainerNames`（读 API 中的容器状态，跨节点有效）。

**已实施（2026-09-16）**：

- 会话目录布局收敛到 `remotegpu.SessionQuotaFile/SessionPidsFile/SessionVMemFile`（`vgpu.config` 文件名下沉到
  `util.VGPUConfigFile`）。agent 写、monitor 读、DRA 采集器都用这一组，不再各自拼路径。
- `ContainerLister` 增加会话来源：远程 Pod（`access_mode=remote` 且 `PodPlanSchedulingNode` 是本节点）按
  `<podUID>_<容器>` 这个既有 key 映射会话里的 `config/vgpu.config` 与 `.vmem_node/vmem_node.config`，
  和本地容器目录走同一套 mmap/reload 逻辑（已抽成 `syncResourceData`/`syncResourceVMem`）。
  **会话目录只读不删**——它归 agent，由 Pod 事件与周期 GC 回收；这里只在 Pod 不再由本节点服务时释放映射。
- `nodeGPUCollector`：容器级指标的 `access_mode` 改成按 Pod 取（`node=`本服务器、`pod_node=`消费节点）；
  远程容器的 PID 不再查本机 cgroup（那里根本没有这个容器），而是读会话的 `pids.config`。
- 卡级 `access_mode` 按节点角色给：`IsRemoteServerNode` 为真即 `remote`（本地 Pod 被调度器挡在服务器节点之外，
  所以一张卡不会同时有两种消费方式），与 DRA 侧 publish-only 节点的语义一致。
- 消费节点不跑这个采集器（无 GPU，`DetectionDeviceLib` 会失败）；部署时不要在纯消费节点上部署 monitor（S6 确认）。
- **顺带修掉两个原有的句柄问题**（2026-09-16）：①目录被从宿主机整个删掉时，`ContainerLister` 永远不会再访问那个 key
  （扫描只看还存在的目录项），fd 与 mmap 一直不释放、还会继续上报已经消失的容器 —— 现在两轮扫描都没碰到的 key 统一释放
  （`dropVanished`，回归测试在 `container_lister_remote_test.go`）。②`MmapDeviceVMemory` 缺 `closed` 标志（`MmapResourceData` 有）：
  `Close` 解除映射后读锁重新变为空闲，刚取到句柄的采集协程还能 `RLock` 成功并读到已 munmap 的内存 —— 现在 `RLock` 拒绝已关闭的映射，
  `Close` 幂等。

### 16.6 节点角色与资源上报（已拍板，取代 §15.3 S2"不注册任何资源"）

一个插件进程可同时承担服务器与消费两个角色，只注册一次 `vgpu-number`，`CheckNode` 无需改动。

| 节点 | 启动参数 | 注册资源 | 角色标签 |
|---|---|---|---|
| 纯 GPU 服务器 | `--remote-server` | 与本地一致：`vgpu-number`，按 feature gate 注册 cores/memory | `remote-server=true` |
| 服务器兼消费节点 | `--remote-server --remote-consumer` | 只注册 `vgpu-number`；数量取 `max(GPU数×切分数, 消费数量参数)`；这些设备对 kubelet 始终上报健康 | 两个都为 `true` |
| 纯 CPU 消费节点 | `--remote-consumer`（不初始化 NVML） | 只注册 `vgpu-number`，数量 = `--remote-consumer-vgpu-number`（默认 1000） | `remote-consumer=true` |

- **开启消费角色就不注册 cores/memory**：kubelet 准入会检查节点上报过的扩展资源，远程 Pod 按远程服务器的卡申请的
  显存/算力会被扣在本机总量上而遭拒（kube-scheduler 对这两项 `ignoredByScheduler`，照样调度过来，形成删除重建循环）。
- **数量与健康与本机 GPU 脱钩**：这类节点上 `vgpu-number` 只表示本节点远程并发上限；本机 GPU 的健康照旧经
  `node-device-register` 的 `Healthy` 告诉调度器。
- **标签由插件按启动参数写入**：开启的角色写 `true`，关闭的角色删除标签，插件退出时也删除。DaemonSet 不能再拿这两个标签当 nodeSelector。
- 纯服务器节点仍会被 kube-scheduler 当作本地 Pod 的候选，再由 extender 以 `NodeIsRemoteServer` 拒绝；抢占侧已跳过服务器。

**已实施（2026-09-16，option 模式）**：`pkg/deviceplugin/remote` 只有一个插件 `remote.Plugin`，四项上报按角色选项装配，
`remote.New(cfg, devManager, WithServerRole(...), WithConsumerRole(...))`：

| 选项 | 节点设备信息 | `vgpu-number` | server 标签 | consumer 标签 | Allocate |
|---|---|---|---|---|---|
| `WithServerRole` | 发布（随插件启停） | 发布（数量 = 本地槽位） | `true` | 不发 | 拒绝（`FailedPrecondition`） |
| `WithConsumerRole` | 不发 | 发布（数量 = `max(消费数量, 本地槽位)`） | 不发 | `true` | 准备 shim + 会话 |
| 两者 | 发布 | 发布一次 | `true` | `true` | 准备 shim + 会话 |

- **每个角色只管自己的标签与注解**（2026-09-18，上机发现后改定，取代"没配的角色主动摘除"）：server 角色发布
  `remote-server` 标签与 `remote-endpoints` 注解、consumer 角色发布 `remote-consumer` 标签，各自在**本进程**退出时删除，
  **从不碰另一个角色的**。原先"对没配的角色注册删除函数"是为了防过时标签残留，但同一节点把两个角色跑成两个进程时
  （GPU 节点顺带消费），纯 server 进程会每轮把 consumer 进程发布的标签删掉、纯 consumer 进程则会删掉 server 的标签与
  endpoints（后者让服务器直接从调度器视野里消失）。回归测试 `TestRoleMatrix` 断言"没跑的角色不注册任何发布/清理函数"。
- **残留由本地插件清理**：一个节点不会同时跑本地插件与远程插件（两者都注册 `vgpu-number`；远程标签挂在本地设备旁边
  会把远程 Pod 引到这台不能服务它们的节点上），所以本地路径（factory 判定本进程不带任何远程角色时，含只起 MIG 插件的节点）
  每轮注册都调用 `remote.RemoveRoles` 删掉两种角色的标签与注解——清的是异常退出的远程进程或改过角色的节点留下的东西。
  远程节点之间互相不清：一个远程进程异常退出后留下的标签，靠它重启后照常发布/退出时照常删除，或者节点改成本地后被清掉。
- **纯 server 也注册 `vgpu-number`**（2026-09-16 用户拍板）：`CheckNode` 的第一道门是 `IsVGPUEnabledNode`（可分配 > 0），
  所以注册它调度器才认这台机器是 vGPU 节点，**调度器侧零改动**。它不会招来本地 Pod（extender 以 `NodeIsRemoteServer` 拒），
  也不会被当成消费节点（没有 consumer 标签）。真有 Pod 落到这里只能是绕过了调度器，`Allocate` 直接拒。
- **节点设备信息跟着插件启停**（用户要求）：`Start` 里发布、`Stop` 里摘除，既不早于插件服务、也不晚于插件退出。
  角色标签则是进程级的（`New` 里装配），因为它描述的是"这个节点是什么"，与 kubelet 注册是否成功无关。
- **两个进程共存时单向让位**：kubelet 对同名资源只保留一个 endpoint（后注册者接管），所以纯 server 进程在探测到本节点
  有活着的 consumer 进程时不注册资源（`standDown`），只继续发布设备信息；consumer 进程消失后重新注册。判活是 dial
  `nvidia-vgpu-remote.sock` 而不是看文件是否存在（崩溃会留下残留文件），周期 10s，通过 `base.RestartNotifier` 让
  main 的启动循环重跑。两个角色的 socket 路径必须不同（`nvidia-vgpu-remote.sock` / `nvidia-vgpu-remote-server.sock`）：
  任一进程 `Stop` 都会 unlink 自己的 socket 路径，同路径会把对方的 socket 文件删掉。
  切换期间不会中断：kubelet 对已 stop 的 endpoint 有 5 分钟量级的宽限期，期内重新注册直接恢复。
- **消费侧的 lupine-server 地址以 agent 为准**（2026-09-16 用户要求）：节点注解只用来找到 agent，`EnsureSession`
  的应答里带着当前的 lupine-server 地址，`Allocate` 把它注入 `LUPINE_SERVER`，注解里的值只作为兜底（agent 没报时）；
  两者不一致时打 V(2) 日志说明用了 agent 的。与 DRA 的 inject 插件一致。服务不可用时的"摘除"沿用既有机制：
  探测失败即发布 `remotegpu.UnreachableServerEndpointInfo`（`{}`），`CheckNode` 解出 `ErrServerUnreachable`
  就以 `NodeRemoteServerUnreachable` 跳过该节点，不需要额外的健康字段或标签翻转。
- **退出时不主动清理 `vgpu-number`**（2026-09-16 用户拍板）：插件退出会删掉设备注册注解与驱动标签，调度器在
  `CheckNode` 里拿不到 `node-device-register` 就以 `NodeNoVGPURegister` 跳过该节点，不必等 kubelet 把可分配数归零。
- 注意与现有校验的出入：本节表格里"纯服务器按 feature gate 注册 cores/memory"目前做不到——`options.Validate()` 规定
  `RemoteGPUSupport` 与 `GPUCoreResourcePlugin`/`GPUMemoryResourcePlugin` 互斥（不分角色），所以远程节点一律不注册这两项。

### 16.7 后续实施顺序

1. **D2**（已完成）：`PatchPodAllocationSucceed` 对远程 Pod 保持 `metrics-node` = 服务器。
2. **S2 服务器角色**（已完成）：
   - 参数 `--remote-server`（需 `RemoteGPUSupport` gate）与 `--remote-agent-endpoint`（默认 `:14834`，空 host 取节点 InternalIP）。
   - `pkg/deviceplugin/remote` 挂到设备管理器已有的节点注册循环：写 `remote-server=true` 与 `remote-endpoints`；
     每 5s 经 agent `ServerInfo` 探测，值变化立即重新注册；关闭角色或插件退出时删除标签与注解。
   - lupine-server 不可达时发布 `{}`（`remotegpu.UnreachableServerEndpointInfo`）：节点仍是服务器、本地 Pod 不上，
     但不接远程 Pod，调度原因 `NodeRemoteServerUnreachable`。
   - 与 DRA 共用的 agent 客户端、地址解析与可发布校验下沉到 `pkg/device/remotegpu/agent.go`（`ProbeServer` 等），
     DRA 发布器与 remote-agent 改为调用它，原副本删除。
3. **S3 agent Pod 模式**（已完成）：
   - `--session-owner=claim|pod|auto`（进程默认 pod；配置项缺省仍按 claim 读）。pod / auto 模式下 DRA 预检
     不再是硬失败（auto 打日志跳过 claim），pod 模式只监听两样东西：
     带 `metrics-node=<本节点>` 标签的 Pod，和本节点的 Node 对象（设备快照来自 `node-device-register`
     + `node-config-info` + CUDA/驱动版本标签）。
   - 会话归属抽象为 `SessionOwner`（claim 或 pod），标记文件保留原文件名并增加 kind 行，缺省按 claim 读，
     所以升级后旧会话仍能识别。
   - `EnsureSession` 对 pod 会话的鉴权：Pod 仍在用本节点的 GPU（remote 访问模式、`predicate-node` 是本节点、
     未进入终态），且 token 等于它某个已预分配容器的 `SessionToken`。Pod 身份沿用 `claim_*` 字段携带。
   - **会话接口按请求里的 owner 路由**（2026-09-16）：proto 新增 `SessionOwner` 枚举，
     `EnsureSession` / `ReleaseSessions` / `FetchClientBundle` 各加一个 `owner` 字段（零值 = claim，
     老的 DRA 调用方语义不变）。agent 不再按进程模式分支，而是按请求的 owner 选鉴权对象、设备快照
     （claim 取 ResourceSlice，pod 取 Node 注册表，auto 模式两份并存互不覆盖）与清扫路径；
     请求的 owner 不在本 agent 服务范围时回 `FailedPrecondition`，绝不落到另一条路上。
   - 回收：Pod 事件与周期 GC。仅被删除（还有 DeletionTimestamp）的 Pod 保留会话，容器还在跑；对象消失或进入
     终态才清。周期 GC 按每个会话标记里的 owner kind 分流；本 agent 当前不服务的那种 owner 的残留会话
     （上一次配置留下的）直接清掉，因为这里既校验不了它、也不会有人来释放它。
4. **S4 消费角色**（进行中）：`--remote-consumer`、`--remote-consumer-vgpu-number` 与 §16.6 的资源规则。
   - **Allocate 不碰网络**（用户 2026-09-15 拍板，取代 §16.3 的"首次 Allocate 批量建会话"）：kubelet 准入是串行的，
     逐个容器等 gRPC 会拖住整个节点。`Allocate` 只做三件事：写本容器目录与 `devices.json`、注入环境变量
     （`LUPINE_SERVER`/`LUPINE_SESSION`/`NVIDIA_VISIBLE_DEVICES=void`/`LUPINE_DISABLE_LOCAL`）、声明两个挂载
     （客户端 shim 目录与 `ld.so.preload`，内容由 PreStart 填）。
   - **PreStartContainer 按当前容器做实事**：建会话（agent gRPC）、准备客户端 shim 与 `ld.so.preload`。
   - **全部在 `Allocate` 里做，不实现 `PreStartContainer`**（用户 2026-09-16 拍板）：`Allocate` 依次为每个容器
     准备客户端 shim、建会话，再返回环境变量和挂载；失败即准入失败并标记 Pod，不会让容器在没有会话的情况下起来。
   - **为什么不用 `PreStartContainer`**：它只带设备 ID，而 kubelet 会把顺序 init 容器的设备 ID 复用给业务容器，
     两个容器的 ID 集合可能完全相同，无法可靠判断当前是哪个容器（`vNumberDevicePlugin` 出现过这个 bug）。
     `Allocate` 靠预分配游标天然知道是哪个容器，所以这个歧义根本不存在。
   - **客户端 shim**：按服务器 CUDA 版本选出 shim 目录（客户端不能比服务器新），生成 preload 列表，直接把这两个
     宿主路径挂进容器（与 DRA 侧一致）。复用 DRA 的选择与 preload 生成逻辑，经 `remote.StageClientArtifact` 导出。
     服务器还没上报 CUDA 版本时报错等重试。
   - **shim 缺失时从 agent 下载**（2026-09-16 用户要求，与 DRA 一致）：选择/下载/重选这套流程收敛成
     `ensureArtifactSelection`（导出包装 `EnsureClientArtifact`），claim 侧保留"取 CUDA 最低的服务器 + 跨服务器 etag 告警"
     后复用它；pod 侧的下载凭证与会话同源（owner=pod + 由某个容器派生的 token，agent 用同一套鉴权）。
     代价：某节点/某服务器构建的**首个** Pod 会在 `Allocate` 里等下载（上限 40s）+ 建会话（5s），kubelet 准入串行，
     所以这段时间同节点其他 Pod 的准入会排队；仍可用 init 容器预置 shim 规避。
   - 代码放在新的 `pkg/deviceplugin/remote`，消费节点用它替换本地 vGPU 插件，不在 `vnum_plugin.go` 里加分支。
   - **节点设备注册已抽到 `pkg/deviceplugin/nodedevice`**：本地插件和远程插件都调用它，没有设备的节点自动不发布。
   - **三种角色组合收敛成一个 option 模式的插件**（2026-09-16，见 §16.6"已实施"）：`WithServerRole`/`WithConsumerRole`
     各自决定发布哪几项、各自只清理自己的（残留由本地路径清）；纯 server 也注册 `vgpu-number`（于是调度器零改动），
     两进程共存时纯 server 向 consumer 单向让位。
5. **S5 监控**（已完成）：§16.5 的"已实施"。服务器节点的节点/卡级用量本来就统计到了（靠 D2 与
   `PodPlanSchedulingNode`），这一步补的是容器级实时用量与 `access_mode` 标签。
6. **S6 部署与文档**（直铺部署集已完成）：`deploy/classic-remote/`——
   `vgpu-manager-remote-gpu-server.yaml`（agent + lupine-server + monitor，hostNetwork/hostPID，`SESSION_OWNER=pod`）、
   `vgpu-manager-deviceplugin-server.yaml`（`--remote-server`：节点设备注册 + 角色标签/endpoints + vgpu-number）、
   `vgpu-manager-deviceplugin-consumer.yaml`（`--remote-consumer`：槽位 + 会话 + 客户端 shim，可选预铺 init 容器）、
   调度器与 webhook（与 classic-local 相同，集群已有则跳过），外加一份 README（组件拓扑、标签分工、
   参数表、端口表、已知边界）。刻意把设备插件与数据面拆成两个 DaemonSet：滚动升级插件不该打断在跑的会话。
   剩余：chart 增补（`charts/vgpu-manager` 的远程角色开关）与主 README 的远程小节细化。
   - **按域名寻址（2026-09-20，实机发现后补）**：默认仍是 hostNetwork（节点 IP 稳定、数据面不过 CNI），
     但集群禁止 hostNetwork 时服务端 Pod 每次重建都换 IP，而 `LUPINE_SERVER` 是 `Allocate` 时烧进容器的、
     容器重启不重注入 —— 于是给每个节点的服务端一个稳定域名：headless Service（`publishNotReadyAddresses: true`）
     + 由 webhook 按目标节点名生成的 `spec.hostname`（新入口 `/pods/hostname`，Pod 模板标签
     `vgpu-manager.io/node-hostname=true` opt-in，`failurePolicy: Fail`，同时注入 `HOSTNAME` 供 `1000 1000VAR)` 展开）。
     agent 侧零改动地复用 `ADVERTISE_SERVER_ENDPOINT`，并新增 `ADVERTISE_AGENT_ENDPOINT`（否则 agent 自己的
     endpoint 仍是 Pod IP，重建后要等一轮注解刷新）。节点名→DNS label 的转换会在需要改写时追加节点名摘要，
     否则 `a.b` 与 `a-b` 会撞成同一个域名、把客户端引到另一台服务器。
     消费侧要求：校验 webhook 拒绝 `access-mode: remote` 且 `dnsPolicy: Default`、或 `hostNetwork` + `ClusterFirst`
     的 Pod（这两种解析不到集群域），`None` 要求自带 `dnsConfig.nameservers`。DRA 路径同一套机制、同样零代码改动。
