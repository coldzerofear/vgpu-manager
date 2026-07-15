# DRA + NRI 协同架构设计（未来方向）

> 本文为 vgpu-manager DRA 驱动模式未来演进方向的设计稿，记录引入 NRI（Node Resource Interface）作为 DRA 互补层的动机、方案与分阶段路线。**本文档不包含已落地代码**，是后续 design review 与实施的依据。

## 1. 背景与动机

当前 vgpu-manager 的 DRA 驱动模式通过 webhook 强校验 + 连通分量算法为每个请求 vGPU 的容器挂载独立的设备路径，从而实现设备访问隔离。该方案在功能上可用，但存在若干设计层面的硬伤：

- **Prepare 与容器生命周期错位**：DRA 的 `NodePrepareResources` 是按 ResourceClaim 一次性准备的，不像设备插件那样在每个容器创建前触发。Prepare 阶段无法得知"哪些容器最终会消费哪些 request"。
- **跨容器/跨 Pod 信息缺失**：连通分量算法只能基于"当前 reservedFor 快照"推断 partition 边界，未来加入 reservedFor 的 pod 在 Prepare 时不可见，导致 partition 拆分不准。
- **多请求容器的 partition 冲突**（见 `pkg/claimresolve/partitions.go` 中 "bug-2" 注释）：一个未来容器引用多个 request 时，Prepare 时分别发出独立 partition，最终容器拿到多个 CDI device，env 变量在 `MANAGER_CLIENT_REGISTER_UUID` 上互相覆盖，挂载在 partition 目录上撞。
- **Webhook 规则越加越脆**：为了让 Prepare 那张图能算清楚，webhook 上加了一堆护栏（一个容器最多一个 vGPU claim、跨 Pod 不能共享 request 等），其中相当一部分是"算法约束"而非"语义约束"。

这些问题的本质是：**DRA 模型缺少"容器创建时刻 + 完整 pod/container 上下文"这一帧**。再多的 webhook 与算法补丁都是在用 Prepare 之外的信息补 Prepare 看不到的东西。

NRI（Node Resource Interface）恰好提供了这一帧：在容器创建/更新/销毁的关键时点，runtime 同步回调插件，插件能拿到完整的 pod spec、container spec、cgroup 路径等上下文，并可向 runtime 注入 mount、env、device、cgroup rule 等运行时调整。

社区已经验证了 DRA+NRI 的协作模式：

- [kubernetes-sigs/dranet](https://github.com/kubernetes-sigs/dranet)：网络资源 DRA 驱动，使用 NRI 在容器创建时注入网络配置。
- [kubernetes-sigs/dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu)：CPU 资源 DRA 驱动，类似模式。

## 2. 设计目标与非目标

### 2.1 目标

- 在容器创建时刻按容器粒度完成 partition 分配与设备路径注入，消除 Prepare 阶段过早决策导致的设计缺陷。
- 大幅简化连通分量逻辑与跨 partition 别名机制（`CandidatesByKey` / fallback key）。
- 为 [coldzerofear/device-mounter](https://github.com/coldzerofear/device-mounter) 风格的运行时热插拔提供标准化、可维护的钩子。
- 在不支持 NRI 的环境下保持当前 DRA-only 路径可用，**NRI 是互补不是替代**。

### 2.2 非目标

- 不抛弃当前 DRA 驱动模式与 webhook。两者长期并存。
- 不引入 NRI 插件对 ResourceClaim 的直接修改。NRI 仅作为运行时配置注入层，资源记账仍由 DRA / Scheduler 负责。
- 不在本设计阶段为 device-mounter 设计完整的资源回流方案，仅留出 NRI 钩子。

## 3. 目标架构

### 3.1 三层职责划分

| 层 | 职责 | 时机 |
|---|---|---|
| Webhook（保留） | 真正的语义校验（如 main request 只能被一个 init + 一个 app 引用） | API server admission |
| DRA kubelet plugin | 资源占位与 UUID 颁发；写入本地 partition 状态文件；返回最小化 CDI device | NodePrepareResources |
| NRI 插件（新增） | 容器创建时按容器粒度分配 partition、注入 mount/env/cgroup；容器删除时清理 | CreateContainer / UpdateContainer / RemoveContainer / Synchronize |

### 3.2 关键架构原则

1. **NRI 与 DRA 之间的事实源单一**：Prepare 是唯一写者，NRI 是只读消费者。NRI 永远不修改 claim 状态或 Prepare 写入的本地文件。
2. **NRI 时刻才是 partition 决策时刻**：partition 边界、register UUID 绑定容器的关系，全部推迟到 NRI CreateContainer 决定。
3. **DRA-only fallback 保留**：节点启动时探测 NRI socket；存在则注册 NRI 插件并启用 lazy 模式，否则回退到当前 eager Prepare 模式。
4. **接口而非分支**：partition 分配核心逻辑通过 `PartitionAssigner` 一类接口抽象，eager / lazy 两个实现并存，避免代码里散落 `if-nri-else`。

## 4. 状态与数据流设计

### 4.1 三种事实源方案对比

| 方案 | 优点 | 缺点 | 评价 |
|---|---|---|---|
| A. Claim annotation 作为唯一事实源 | 云端可见，方便排障 | NRI 路径引入 API server 依赖，慢且脆；annotation 大小有 256KB 上限 | 不推荐 |
| B. 本地文件 + claim annotation 索引 | 延续现有 `contPath/claims/<uid>/` 目录树，迁移成本低；本地读，NRI 路径无 API 依赖 | 排障需登录节点查文件 | **推荐** |
| C. NRI 插件自起 informer | 功能强 | 工程重；NRI hook 路径引入 informer cache sync 不确定性 | 不推荐 |

### 4.2 选定方案 B 下的契约

- **Prepare（DRA 端）写入**：
  - 在 `contPath/claims/<claimUID>/` 下创建 partition 占位目录与元数据文件（partition 候选集、UUID 颁发记录等）。
  - 在 claim annotation 上仅保留 `<driver>/<registerUUID>: <claimUID>` 这类索引信息，不写完整 partition key。
- **NRI 插件读取**：
  - 在 CreateContainer 时通过 pod spec 中的 podClaim ref 反查 claimUID，再读本地 `contPath/claims/<claimUID>/` 元数据获取候选 partition 集合。
  - 基于当前容器实际引用的 request 子集计算 partition key，分配 register UUID，注入 mount + env。
- **Unprepare（DRA 端）清理**：统一清理本地状态。NRI 的 RemoveContainer 不删除 claim 级别的状态，只清理容器级别的临时绑定记录。

### 4.3 NRI 插件重启后的 Synchronize

NRI runtime 在重连时会把"当前节点上所有 pod/container"通过 `Synchronize` 喂给插件。插件必须能从持久化状态重建出"这个容器对应哪个 partition、应该已经注入过什么"。dranet 在这块的实现可作为直接参考。

## 5. 现有模块影响

引入 NRI 后，**能简化甚至删除的部分比新增的还多**。

### 5.1 `pkg/claimresolve/partitions.go`

- 连通分量算法的角色从"决定 partition 边界"退化为"枚举 claim 的 request 集合"。
- `buildPartitionKey` 大幅简化甚至消失——partition key 由 NRI 在容器创建时按"该容器引用的 request 集合"计算。
- `CandidatesByKey` 和 fallback key 机制可以删除——NRI 时刻就是确定的时刻，没有"stale key"问题。
- bug-2（多 request 单容器 partition 冲突）从根上消失。

### 5.2 `pkg/kubeletplugin/clientregistry.go`

- `TargetByUUID` 这套"UUID → claim → partition key → 候选容器集合"的查找逻辑，在 NRI 模式下退化为"NRI 写一个 map（cgroup → partition）就够了"。
- 可能整个文件在 NRI 模式下不再需要，或仅保留作为 DRA-only fallback 专用。

### 5.3 `library/src/register.c` 与 RPC client

- 在 NRI 模式下可以**完全去掉 library 主动 register**。
- 原因：NRI 在 CreateContainer 时已经知道目标容器的 cgroup 路径与 PID 命名空间，pids.config 可以由 NRI 插件直接写。library 不再需要"主动告诉 manager 我是谁"。
- 这意味着 library 端的 cgo 桥接、`execl` RPC client 复杂度都可以退化。但 DRA-only 模式仍需保留 register 路径。

### 5.4 Webhook

当前 webhook 里那些约束，在 NRI 模式下需要逐条审视：

| 当前规则 | 在 NRI 模式下 |
|---|---|
| 同一 main request 只能被一个 init + 一个 app 容器引用 | **保留**，真语义约束 |
| 一个容器最多一个 vGPU claim | **可放宽**，NRI 可合并 partition |
| 跨 Pod 共享 request 的部分限制 | **可放宽**，NRI 知道每个容器要什么 |

Webhook 放宽是 Phase 2 的事，Phase 1 不动 webhook。

## 6. device-mounter 集成方向

### 6.1 device-mounter 当前工作方式

[coldzerofear/device-mounter](https://github.com/coldzerofear/device-mounter) 并非旁路工具，资源记账在 K8s 视图中是正确的：

- 通过"奴隶 Pod"（slave pod）走**标准 K8s 调度路径**占用目标设备的资源配额。
- 奴隶 Pod 调度成功（资源分配被 scheduler 确认）后，再将设备转移/挂载到目标运行中的容器，实现热插拔与扩容（追加设备、提升显存/算力上限等）。
- 设备占用通过奴隶 Pod 体现，scheduler 与 quota 都能正确感知，不存在"资源核算对不上"问题。

当前方案的工程代价集中在**设备转移环节**：从奴隶 Pod 把设备搬到目标容器这一步，需要直接和 runtime / CRI 接口打交道，缺少标准的"运行时调整容器"钩子，可观测性与可移植性都受限。

### 6.2 NRI 带来的改进

NRI 引入后，device-mounter 的资源占用机制保持不变，**只是把设备转移这一步从自定义低层操作改为走标准 NRI 钩子**：

1. **vgpu-manager 的 NRI 插件作为节点上的"vGPU 设备网关"**：所有 vGPU 容器的 mount / env / cgroup device rule 都由它注入。
2. **device-mounter 与 NRI 插件本地通信**（unix socket / gRPC）：奴隶 Pod 调度确认后，device-mounter 向 NRI 插件发起请求，由 NRI 插件通过 `UpdateContainer` 钩子完成实际的 mount 与 cgroup 调整。
3. **奴隶 Pod 机制保留**：作为资源占用的事实源，配额、quota、scheduler 视图与今天一致。NRI 只承担"将已批准的资源应用到容器"这一步。

这样 device-mounter 的设备转移环节从"绕过 runtime 接口直接操作"升级为"通过标准 NRI 钩子完成"，可观测性、错误回滚、与其他 NRI 插件的共存都有标准答案。资源记账模型不变。

## 7. 风险与失败处理

NRI 插件位于关键路径上，失败处理必须前置设计。

### 7.1 主要风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| NRI 插件挂掉/慢响应 | 新容器创建被阻塞 | 超时短 + fail-open；插件不可达时退化到 DRA-only 路径 |
| 与其他 NRI 插件（topology-manager、NUMA-aware 等）共存冲突 | 注入冲突、调用顺序问题 | 上线前在目标集群明确测试；遵循 NRI 插件最佳实践 |
| 状态漂移（NRI 内存视图与磁盘/runtime 实际不一致） | partition 误绑定 | 周期性自检 + 日志告警；不自动"修复"，由运维介入 |
| NRI 版本/runtime 兼容 | 老节点不可用 | DRA-only fallback 长期保留 |
| 双路径维护成本 | 代码与测试翻倍 | 接口化抽象（`PartitionAssigner`），单元测试针对接口跑两套实现 |

### 7.2 失败模式预案

- **NRI 插件无法启动**：节点 DRA 路径自动退化到 eager 模式，不拒绝调度。
- **NRI 插件运行中 panic**：containerd 默认重连，中间窗口期建议 fail-open——容器先起来，partition mount 缺失会在 library 启动时报错而不是 hang 住整个节点。
- **状态漂移**：日志 + metric 告警，不自动修复。

### 7.3 部署形态考虑

- **同进程 vs 分进程**：NRI 插件与现有 kubeletplugin 同进程共享状态简单，但 NRI 挂掉会把 DRA 也带挂。分进程更安全但要走 IPC。dranet 是同进程，可作为参考。Phase 0 spike 时先用同进程，Phase 1 再评估。

## 8. 可观测性

NRI 模式上线必须配套以下可观测能力：

### 8.1 指标

- NRI hook 延迟分布（CreateContainer / UpdateContainer / RemoveContainer 的 p50/p99）
- partition 分配冲突次数（理论恒为 0，> 0 即 bug）
- Synchronize 重建容器数 vs runtime 报告数（不一致即状态漂移）
- DRA-only fallback 触发次数（生产环境非 0 说明 NRI 路径有问题）

### 8.2 调试端点

NRI 插件应暴露本地 debug endpoint（unix socket / localhost http），可查询：

- 当前节点上 `partition → container` 的映射
- 已颁发但未消费的 register UUID
- 最近 N 次 hook 调用与结果

出问题时不用 attach 进程，`curl` 即可获取状态。

## 9. 测试策略

双路径的复杂度必须从一开始就管控好。

1. **接口化 + 双实现同测**：核心逻辑通过 `PartitionAssigner` 接口抽象，eager 与 lazy 实现各自实现接口，单元测试针对接口编写，两套实现跑同一套用例。
2. **CI 双轨**：kind 集群跑两套配置——启用 NRI 的 containerd 1.7+ 镜像，与禁用 NRI 模拟老节点。每个 PR 必须过两条。
3. **架构债回归用例**：`partitions.go` 注释里的 bug-2 场景（单容器多 request 撞 UUID）必须有集成测试，**在 NRI 模式打开后期望成功**。这是 Phase 1 完成的硬验收标准。
4. **NRI chaos testing**：插件强制 panic、慢响应、断连，验证 runtime 行为符合预期。
5. **复用社区测试夹具**：dranet 的 NRI 测试用例可直接借鉴。

## 10. 分阶段路线图

### Phase 0：Spike（1-2 周）

目标：**仅验证数据通路**，不动现有逻辑。

- 写一个**只读** NRI 插件，挂到 dev 集群上。
- CreateContainer 时打日志：能否拿到 pod 的 claim ref、能否通过 claim annotation 找到 Prepare 留下的 partition 信息、Synchronize 流程是否符合预期。
- 输出 design doc 走评审，锁定 Phase 1 接口形状。

### Phase 1：核心收益（一个月量级）

目标：partition path / env 注入从 CDI 移到 NRI。

- Prepare 改为仅做"资源占位"，不分配 UUID、不绑 path；CDI 返回最小化 device。
- NRI CreateContainer 时分配 register UUID、注入 `MANAGER_CLIENT_REGISTER_UUID`、mount 容器独占 partition 目录。
- 节点启动时探测 NRI socket：可用即 lazy 模式，否则保留现 eager 模式。
- 完成后 bug-2 应自然消失，`clientregistry.go` 的 fallback key / `CandidatesByKey` 大半可以删除。

### Phase 2：清理（两周量级）

- 基于 Phase 1 运行情况，**有依据地**放宽 webhook 规则。每条放宽前先问"DRA-only 模式怎么办"——若两路径需要不同规则集，要么放弃放宽，要么明确文档化"该约束仅在 NRI 模式下解除"。
- 删除 NRI 模式下不再需要的代码路径（清理而非保留）。

### Phase 3：扩展（两周量级）

- device-mounter 通过 NRI `UpdateContainer` 实现热插拔。
- Phase 1 的 lazy 分配框架已经在位，扩展面平。

### Phase 4：长期演进（视社区节奏）

- 观察社区方向（containerd / CRI-O / 托管平台对 NRI 的覆盖度），评估是否把 DRA-only 路径降级为遗留模式甚至 deprecate。
- 不急，至少 1-2 年后再看。

## 11. 开放问题

以下问题需要在 Phase 0 spike 后、Phase 1 实施前回答：

1. **NRI 插件与 kubeletplugin 同进程还是分进程？**
   倾向同进程（dranet 路线），但需评估故障隔离代价。

2. **Prepare 在 NRI 模式下到底返回什么 CDI device？**
   完全空 device + 标识符让 NRI 补全？还是返回"占位 mount"让 NRI 替换？这个直接决定接口形状。

3. **library 端的 register 路径是否需要保留？**
   NRI 模式下 NRI 已知 PID 命名空间和 cgroup，可直接写 pids.config，register 的意义只剩"library 自身的活性信号"。是否值得保留需要权衡。

4. **Pod 删除/容器重建的清理时机**
   NRI `RemoveContainer` / `StopPodSandbox` 触发清理时，与 DRA `Unprepare` 如何协调，避免一边删一边另一边还在用。

5. **NRI 模式跑通后，是否还需要 Prepare 阶段做 partition 决策？**
   如果 NRI 时刻已能拿到所有上下文，Prepare 可退化为"纯资源占位"，partition 边界与 UUID-to-container 映射全部由 NRI 决定。这样 DRA 管"配额和记账"、NRI 管"运行时配置注入"，架构上更对称。**此判断要等 Phase 1 跑出来才能验证，但 Phase 1 接口设计时应留出这个余地，不把 partition 边界写死成 Prepare 的输出。**

## 12. NRI 模式实现设计（收敛版，featuregate 门控）

> 本章是第 1–11 章的**收敛落地方案**，与前文的方向不冲突但更具体。前文（§4 事实源、§5 partition 简化、§7 部署形态等）在探讨"如何在 Prepare/NRI 之间搬移 partition 决策"；本章在实现层做了一个更彻底的简化：**NRI 模式下不再有跨容器共享的 partition，目录直接按容器隔离**。旧的 partition 共享设计（连通分量 + register UUID 模式）**整套保留、代码不动**，作为 NRI 不可用时的兼容路径。

### 12.1 双路径与门控

引入一个新的 featuregate（下称 `NRISupport`）。**依赖关系**：`NRISupport` 只要求 `VGPUSupport`，**不**要求 `DevicePluginClientMode`（后者即 client-register 模式，`MANAGER_COMPATIBILITY_MODE` 的 `200` 位）。NRI 插件的本职是"按容器注入 partition 目录 mount"，这与 register 模式正交。`DevicePluginClientMode` 只决定 library 用哪种方式获得 PID：

| NRISupport | DevicePluginClientMode | partition/目录 | PID 来源 | 缓存用途 |
|---|---|---|---|---|
| off | off/on | 连通分量 partition，跨容器共享 | 旧路径（UUID 模式 register 或 cgroup） | — |
| **on** | **on** | 按容器隔离，无 partition 图 | **pod-uid 模式 register**（library 走 pod-uid，registry server 读 NRI 缓存返回 target，§12.8） | NRI 缓存共享给 registry server 干预 pod-uid 路径 |
| **on** | **off** | 按容器隔离，无 partition 图 | **cgroup 模式**（library 从宿主 /proc 自解析 PID，无 register） | NRI 缓存仅内部使用（无 registry server） |

即：NRI 门控翻转的是"partition 决策与目录注入从 Prepare/CDI 移到 NRI CreateContainer"；`DevicePluginClientMode` 是否同开只影响 PID 获取方式与是否共享缓存给注册服务。保留旧 partition 路径的动机：**某些容器运行时不支持 NRI，或 NRI 配置成本高时可退回**。

### 12.2 核心简化与承重墙不变量

NRI 模式把"可注入目录"的作用域键从 **partition（连通分量）** 换成 **(claim-uid, pod-uid, container-name)**：每个容器一个独立目录。由此：

- 连通分量算法、`buildPartitionKey`、`CandidatesByKey`、fallback key、claim annotation 上的 `<driver>/<uuid>` 索引 —— 在 NRI 路径上**整块不参与**。
- `partitions.go` 注释里的 **bug-2**（单容器多 request 撞 UUID/mount）从根上消失：容器引用 r1+r2 时只拿到自己一个目录、一次注入；两个 request 的限额 env 是 `CUDA_MEM_LIMIT_<idx>` 带不同 idx 后缀，本就不冲突。

**承重墙不变量**：按容器隔离目录之所以安全，前提是 **"任何两个并发容器绝不共享同一个 vGPU request"**。这一点由 webhook 保证（一个 request ≤1 app、≤1 sidecar 且 sidecar 独占；唯一允许共享 request 的是"非重启 init + app"，而它们**顺序执行**，各自独立目录反而更干净）。**若未来放宽 webhook 允许并发容器共享 request，本简化即失效** —— 该约束必须作为显式不变量长期保留。

### 12.3 目录布局

```
Host:  <HostManagerDir>/claims/<claim-uid>/<pod-uid>_<container-name>/
Plugin/library 视图: /etc/vgpu-manager/claims/<claim-uid>/<pod-uid>_<container-name>/
    ├── config/       -> bind rw 到容器 /etc/vgpu-manager/config （含 pids.config）
    ├── vgpu_lock/    -> bind rw 到容器 /tmp/.vgpu_lock
    └── vmem_node/    -> bind rw 到容器 /tmp/.vmem_node
```

保留 `claims/<claim-uid>/` 这一层的价值：Unprepare 能 `rm -rf claims/<claim-uid>/` 做 claim 级批量兜底清理，即使 NRI `RemoveContainer` 在插件重启窗口漏删也不残留。

### 12.4 三层职责（NRI 模式版）

| 层 | NRI 模式下做什么 |
|---|---|
| Prepare（DRA） | 照常注入公共 edits + 设备级 env（`MANAGER_VISIBLE_DEVICE_<idx>`、`CUDA_*_<idx>`）；额外注入 `MANAGER_VGPU_CLAIM_UID=<claimUID>`；**跳过** partition mount 与 UUID 铸造/annotation。保留 `200` 位。 |
| NRI CreateContainer | 从 `container.Env` 读 `MANAGER_VGPU_CLAIM_UID`（并据 `LD_PRELOAD`/`MANAGER_COMPATIBILITY_MODE` 判定这是 vGPU 容器）→ 建 `claims/<claim-uid>/<pod-uid>_<container-name>/{config,vgpu_lock,vmem_node}` → 注入三个 rw bind mount + `VGPU_POD_UID` + `VGPU_CONTAINER_NAME` env → 向 register server 写入 **NRI 缓存** `(pod-uid, container-name) → (claim-uid, configDir)`。 |
| register server | 收到 pod-uid 模式请求（无 reg_uuid）→ 走 `getTargetByPodUid` → 返回该容器独占 `configDir` → 写 `pids.config`。 |

### 12.5 已落地的服务端地基（commit `583df02`）

下列改造已在 `feat/nri-support` 分支就位，NRI 插件可直接复用：

1. **register server 的 pod-uid 分支改为 resolver 驱动**：`GetTargetByPodUidFunc(ctx, podUid, contName) (*Target, error)`（`pkg/device/registry/server.go`）。`ConfigDir` 由注入的 resolver 决定，server 不再硬编码 `GetPodContainerManagerPath`。这是让 NRI 目录布局生效的关键。
2. **DRA 侧 `TargetByPodUID`**（`pkg/kubeletplugin/clientregistry.go`）已算出 `ConfigDir = <contPath>/claims/<claimUID>/<podUID>_<contName>/config`，与 §12.3 一致。**注意**：当前落地是用 `ClaimReservedForUid` 索引反查 + 取 `claims[0]` 的 interim 版本（见下条与 §12.8），收敛设计会把它改为"NRI 缓存命中 / device-plugin 回退"。
3. **`ClaimReservedForUid` 索引**（`claim.status.reservedFor.uid` → pod-uid → claims）：interim 阶段用来从 pod-uid 反查 claim。收敛设计用 NRI 缓存做权威来源后，此索引在 `TargetByPodUID` 路径上不再需要（可作清理项，除非另有他用）。
4. **`MANAGER_VGPU_CLAIM_UID` 常量**（`pkg/util/consts.go`）：CDI 注入 claim-uid 的载体，NRI 据此建缓存。
5. device-plugin 侧 resolver 仍返回旧目录 `<pod>_<container>/config`，两路径正确分岔；并在 `PreStartContainer` 增加了启动前清理旧 `pids.config` / `vmem_node.config`。
6. **contPath 一致性**：device-plugin 与 kubelet-plugin 的 registry server 都以 `util.ManagerRootPath`（`/etc/vgpu-manager`）为 contPath（`ContManagerDirectoryPath == ManagerRootPath`）。因此 §12.8 中"未命中回退到 device-plugin 式目录"落点与 device-plugin 自身完全一致，回退安全。

### 12.6 剩余待实现清单

1. **NRI 插件本体**：stub 注册 + `Configure`/`Synchronize`/`CreateContainer`/`RemoveContainer`/`StopPodSandbox`。参考 containerd `plugins/device-injector` 与 dranet `nri_hooks.go`。建议与 kubeletplugin **同进程**（复用现成 informer/registry server，dranet 路线）。
2. **`NRISupport` featuregate** 及 Prepare 分支：NRI 模式跳过 UUID 铸造 + partition mount，改经 CDI 注 `MANAGER_VGPU_CLAIM_UID`。旧路径（含 register UUID 机制）一行不改。
3. **NRI CreateContainer**：读 `MANAGER_VGPU_CLAIM_UID` → 建目录 + 注入三个 rw mount + `VGPU_POD_UID` + `VGPU_CONTAINER_NAME`（使 library 走 pod-uid 模式）→ 写 NRI 缓存。
4. **NRI 缓存 + `Synchronize`**：进程内 `(pod-uid, container-name) → (claim-uid, configDir)` 缓存，供 `TargetByPodUID` 查询；`Synchronize` 时从回放容器的 env 重建（见 §12.9）。
5. **`TargetByPodUID` 收敛为"缓存命中 / device-plugin 回退"**（见 §12.8），取代 interim 的 `ClaimReservedForUid` + `claims[0]`，同时关掉该 TODO。

### 12.7 register 模式切换机制（env 驱动）

library 从 env 读 `VGPU_POD_UID` / `VGPU_CONTAINER_NAME` / `MANAGER_CLIENT_REGISTER_UUID`（`library/src/loader.c`），`200` 位触发 `register_to_remote_with_data(pod_uid, container_name, reg_uuid)` 把三者全发；server 只按"reg_uuid 是否为空"分支（`server.go` `lookupTarget`）。因此模式切换很干净：

- **NRI 模式**：Prepare 不注入 `MANAGER_CLIENT_REGISTER_UUID`，NRI 注入 `VGPU_POD_UID`+`VGPU_CONTAINER_NAME` → library 发空 uuid → server 走 pod-uid 分支 → `TargetByPodUID` → `claims/<claim-uid>/...` 目录。
- **旧模式**：Prepare 注入 `MANAGER_CLIENT_REGISTER_UUID` → server 走 UUID 分支（`CandidatesByKey` + 逐候选试活 PID）。

### 12.8 `TargetByPodUID` 收敛设计：NRI 缓存命中 / device-plugin 回退（方案 A）

**背景缺口**：register 时服务端只拿到 `(pod-uid, container-name)`，缺"该容器引用哪个 claim"这一维。interim 版用 `ClaimReservedForUid` 索引把 pod-uid 反查为 claims，但**多 claim pod**（一个 pod 的不同容器各挂不同 vGPU claim —— webhook 只禁"单容器多 claim"，pod 级多 claim 是允许且应当允许的）会命中多个 claim，取 `claims[0]` 可能给错目录。reservedFor 索引只能定位到 **pod**、定位不到 **"该容器用哪个 claim"**。

**收敛方案（A）**：NRI 在 CreateContainer 时已知每容器**确切** claim-uid（来自 CDI 注入的 `MANAGER_VGPU_CLAIM_UID`），写入进程内 NRI 缓存 `(pod-uid, container-name) → (claim-uid, configDir)`。`TargetByPodUID(podUid, contName)` 逻辑变为：

1. **查 NRI 缓存**：命中 → 直接返回该 target（`ConfigDir = <ManagerRootPath>/claims/<claim-uid>/<pod-uid>_<container-name>/config`）。这是 NRI-DRA 容器的权威解析，无索引猜测、无 `claims[0]` 歧义。
2. **未命中 → 回退 device-plugin 式 target**：`ConfigDir = GetPodContainerManagerPath(ManagerRootPath, podUid, contName)/config` = `/etc/vgpu-manager/<pod-uid>_<container-name>/config`。用于**兼容一个节点同时安装 device-plugin 与 kubelet-plugin** 的情形 —— device-plugin 管理的 pod 其 library 若走到 kubelet-plugin 的注册服务，也能被正确服务（落点与 device-plugin 自身一致，见 §12.5 第 6 条）。

此设计**取代** interim 的 `ClaimReservedForUid` + `claims[0]`，并关掉其 TODO；`register` proto 不变（无需 `claim_uid` 字段），library/C 不改，NRI 本就是权威来源。

**缓存未命中的安全性**：NRI `CreateContainer` 在容器进程启动**之前**回调（早于 `StartContainer`），而 library 的 register 发生在进程启动 + CUDA 初始化**之后**。因此一个 NRI-DRA 容器发起 register 时，其缓存条目**必已写入** —— 未命中可可靠地判定为"非 NRI 容器" → 回退 device-plugin 目录是安全的。

**已知边界**：NRI 插件**运行中重启**的窗口内，缓存尚未由 `Synchronize` 重建时若有 register 到达，会误判未命中而回退到错误目录。缓解：`resolveTarget` 的轮询重试给 `Synchronize` 争取时间；且 library 对每个新进程都会重新 register，窗口过后自愈。此风险与 §12.11 的 fail 策略一并在 Phase 0 评估。

### 12.9 Synchronize（重建几乎免费）

NRI 重启后 runtime 回放全部容器，`container.Env` 里的 `MANAGER_VGPU_CLAIM_UID` + `VGPU_CONTAINER_NAME` + `pod.Uid` 都还在 → NRI 确定性重算 `configDir`，重建 §12.8 的映射即可。**CDI 注入的 env 本身即持久状态**，无需 NRI 自建权威存储（dranet 同款套路）。

### 12.10 清理职责划分

- NRI `RemoveContainer` / `StopPodSandbox`：删该容器 `claims/<claim-uid>/<pod>_<container>/` 子目录。
- DRA `Unprepare`：`rm -rf claims/<claim-uid>/` 兜底。注意 Prepare 的 `ensureClaimDirectories` 每次会 `RemoveAll` + 重建 `claims/<claim-uid>`；顺序上 Prepare 早于 CreateContainer，且 checkpoint 幂等保证不中途重跑，不会误删 NRI 子目录。

### 12.11 待决策：fail-open vs fail-closed（vGPU 必须先验证）

§7.2 假设 NRI 失败时 fail-open"容器先起来、mount 缺失由 library 报错"。**这必须在 Phase 0 验证**：partition 目录没挂上时，library 是 FATAL 退出还是**静默跑成无限制**。若是后者，fail-open 等于放一个不受显存/算力约束的容器压垮同卡邻居 —— 那对 vGPU 容器就必须 **fail-closed**。此判断决定整个失败模型。

### 12.12 Phase 0 验证清单（收敛版）

1. containerd 是否在 **CDI 注入之后**才回调 `CreateContainer`（决定 NRI 能否看到 CDI 注入的 env，如 `MANAGER_VGPU_CLAIM_UID`）。
2. NRI 是否能从 `container.Env` 稳定读到 CDI 注入的 env（而非仅 pod spec 的 env）。
3. `CreateContainer` 时容器进程尚未启动 → 确认 pids 仍靠 library 后续 register 写入（NRI 此刻拿不到 PID，不能自写 pids.config）。
4. §12.11 的 fail-open/closed 行为实测。
5. `CreateContainer` 回调 p99 延迟预算（在容器创建关键路径上）。

### 12.13 进程内部署形态、生命周期与故障恢复（生产级）

**形态确定**：NRI 插件**不作为独立二进制经 `/opt/nri/plugins` 预注册**，而是**内嵌在 kubelet-plugin 进程内**，由 `featuregates.Enabled(NRISupport)` 开关控制启动，主动 dial NRI socket 建 ttrpc 连接。这与 dranet / dra-driver-cpu 的做法一致（两者都是驱动进程内 `stub.New(...)` + 连接 socket，非预注册）。

**部署**：宿主 `nriRoot`（默认 `/var/run/nri`）挂进容器同路径，插件连 `/var/run/nri/nri.sock`。containerd ≥ 1.7 且 `[plugins."io.containerd.nri.v1.nri"] disable = false`。

#### 12.13.1 组件放置与启动顺序

新增 `pkg/kubeletplugin/nri`，由 `driver.Start` 在 **`NRISupport` 开启**时（独立于 `DevicePluginClientMode`）拉起。NRI 插件本身**不需要 informer** —— claim-uid 直接来自 `container.Env`（`MANAGER_VGPU_CLAIM_UID`），它只需 socket + 共享 `Cache`。当 `DevicePluginClientMode` 也开启时，NRI 启动放在 `startClientRegistry` **之后**，以保证读缓存的 registry server 先于写缓存的 NRI 就绪；`Cache` 在两个门控块之前创建，供二者共享（`podLister` 是 registry server / resolver 的依赖，不是 NRI 的）。关闭 `NRISupport` 时整个 NRI 组件不初始化，走旧 partition 路径。

```
driver.Start:
  ├─ kubeletplugin.Start (DRA socket)         # 不受 NRI 影响
  ├─ startClientRegistry (informers + registry server)
  └─ if NRISupport: startNRIPlugin(ctx, podLister, claimIndexer, registryServer)
```

#### 12.13.2 stub 配置

`stub.New(plugin, WithPluginName(DRADriverName), WithPluginIdx(<可配置,默认"00">), WithSocketPath(<nriRoot>/nri.sock), WithOnClose(onClose))`。

- 读自身 CDI 注入的 env（`MANAGER_VGPU_CLAIM_UID` 等）**不依赖 NRI idx** —— CDI 由 containerd 在所有 NRI `CreateContainer` 回调**之前**应用（§12.12 第 1 项验证此点）。故 idx 仅影响与**其他** NRI 插件的相对顺序，默认 `"00"` 即可，保留可配置。
- `WithOnClose` → 打点 `nri_connection_closed` metric + 日志，触发重连循环感知。

#### 12.13.3 两级失败模型：恢复优先，恢复无望才受控失败

参考实现要么 `Fatalf`（dranet，杀进程）要么 5 次即弃（都不合适）。本设计分两级：

**恢复级（tier 1）**：goroutine 内循环 `Run(ctx)`，断连即指数退避（cap 30s）后重连；`ctx.Done()` 才退出。此级**不 `Fatal`、不动 liveness** —— 瞬断/短时不可用期间 DRA 路径（informers / registry / Prepare / Unprepare）照常运行。

**升级级（tier 2）**：若**持续断连超过宽限期** `failureGracePeriod`（默认 3min，即"窗口内重连耗尽"）→ 置 `failed`（`Healthy()` 返回 false），但**循环继续重连**——能自愈就在下一个健康会话清除 `failed`（`recordDisconnect`：健康会话重置失败窗口）。`failed` 经 **healthcheck 报 `NOT_SERVING`（仅 NRISupport 开启时）→ kubelet liveness 探针失败 → 干净重启 pod**。

为何用 healthcheck 而非 `os.Exit`/`Fatalf`：
- **checkpoint 安全**：重启发生在进程边界，不在 `Prepare` 中途；且 checkpoint 走 kubelet `checkpointmanager`（temp+rename+checksum **原子写**）+ `cp.lock`/`pu.lock`，即便中途退出也不撕裂，`CheckpointCleanupManager` 重启后回收孤儿 `PrepareStarted`。
- **可自愈 + 可观测**：报 unhealthy 后仍在重连；若 NRI 先恢复则翻回 SERVING，否则 pod CrashLoopBackoff，管理员据日志定位 NRI 并修复。

```
for {
    start := now()
    err := stub.Run(ctx)                 // 阻塞至断连
    if ctx.Err() != nil { return }       // 优雅停机
    healthy := now()-start >= 30s
    recordDisconnect(healthy)            // 健康会话重置; 否则超宽限期置 failed
    if healthy { backoff.Reset() }
    sleep(backoff.Next())                // 指数退避 cap 30s
}
// Healthy() = !failed, 供 healthcheck 消费(仅 NRISupport 开启)
```

> 落地：`pkg/kubeletplugin/nri/plugin.go` 的 `Run`/`recordDisconnect`/`Healthy` 已实现该两级机制。healthcheck 消费 `Healthy()` 的接线是待做项（见 §12.13.6）。

#### 12.13.4 故障恢复：三类重启统一走 Synchronize

每次 (重)连成功，containerd 通过 `Synchronize(pods, containers)` 回放节点当前全部容器。NRI 据此从 `container.Env` 的 `MANAGER_VGPU_CLAIM_UID` **重建缓存** `(pod-uid, container-name) → (claim-uid, configDir)`。**env 即事实源，无需持久化**（同 dra-driver-cpu）。partition 目录下的 `pids.config` 在宿主 `hostPath` 上，重启不丢。

| 重启场景 | 现象 | 恢复 |
|---|---|---|
| containerd / nri.sock 重启 | ttrpc 断连，`Run` 返回 | 重连循环 → `Synchronize` 重建缓存 |
| kubelet-plugin(本进程)重启 | NRI + registry server + informers 全部重来 | 重启后连 socket → `Synchronize` 重建缓存；磁盘 pids.config 仍在 |
| kubelet 崩溃重启 | 不影响 NRI↔containerd 连接（NRI 走 containerd 非 kubelet） | 无需 NRI 侧动作；DRA 侧由 checkpoint 恢复 |

`Synchronize` 的 env 解析失败 → **fail-closed**（同 dra-driver-cpu，`return nil, err`），避免用错缓存静默污染；不返回 `ContainerUpdate`（运行中容器无法补挂 mount，只重建内存缓存）。

#### 12.13.5 启动就绪门：闭合 §12.8 的重启窗口

§12.8 的已知边界（缓存未由 `Synchronize` 重建时 register 到达 → 误判未命中而错误回退 device-plugin 目录）在此闭合。`Cache` 持有 `synced` 状态（首次 `Synchronize` 完成时经 `Replace` 置位，`Synced()` 查询；已在 `pkg/kubeletplugin/nri/cache.go` 落地）。`TargetByPodUID` 逻辑：

```
if NRISupport 开启:                       # 就绪门仅在 NRI 开启时生效
    if 命中 nriCache: return 该 target      # claims/<claim-uid>/<pod>_<container>/config
    if !nriCache.Synced(): return err      # 关键: 普通 error, 不能是 NotFound
    # 已 synced 仍未命中 → 确定是非 NRI 容器, 落到回退
return device-plugin target                # /etc/vgpu-manager/<pod>_<container>/config
```

**关键约束**：就绪门返回的错误**必须是非 `apierrors.NotFound` 的普通 error**。`resolveTarget` 只对 `apierrors.IsNotFound` 硬失败，其余错误一律 `lastErr=err; return false, nil` 继续轮询直到 `resolveTimeout`（60s）（`server.go:334-339`）。因此就绪门返回普通 error 时，ClientRegistry 会在超时周期内自动重试，等 NRI 首次 `Synchronize` 完成、缓存就绪后自然命中。**未开 NRISupport 时不走就绪门**，pod-uid 请求直接回退 device-plugin 目录（此时是 device-plugin 的 pod 撞到本服务，§12.8 第 2 分支）。

> 待定的小 nuance：断连（`onClose`）时是否把 `synced` 复位为 false。复位则断连窗口内所有未命中都返回可重试错误（等 NRI 回来重连+重放），对"断连期间新建的无 mount 容器"更保守；不复位则沿用旧缓存。因断连期间新容器本就无 mount（§12.11 失败态），倾向**不复位**，避免阻塞断连期间到达的 device-plugin pod 注册。Phase 0 后定。

#### 12.13.6 失败模型与 library 行为（已由作者确认）

NRI 未连接时 containerd 不调用其 `CreateContainer`，容器**在无 partition mount 的情况下被创建**。作者确认的 library 行为：

| register 模式 | partition mount 缺失时 library 行为 | 性质 |
|---|---|---|
| **cgroup 模式（无 200 位）** | 容器内**自建目录、继续运行，限额照常执行**（PID 从宿主 /proc 自解析）；仅 host daemonset 感知不到容器内缓存文件（**监控盲区**，非 enforcement 问题） | 安全降级 |
| **client 模式（200 位）** | 客户端注册调用失败 / `pids_size==0` → **`LOGGER(FATAL)` 退出** | fail-closed，可见 |

推论：
- 限额的**值**（`CUDA_MEM_LIMIT_<idx>` 等）来自设备级 CDI env（Prepare 注入，与 NRI 无关，恒在）；partition 目录只是跨进程锁 + 显存记账的共享落点。**每容器模型下单容器缺 `vgpu_lock`/`vmem_node` mount，enforcement 仍正确**，只丢失 host 侧监控可见性 → 这两个 mount 在功能上**可选、监控上需要**。
- 因此"NRI 未就绪"本身不会导致 vGPU 逃逸限额：client 模式直接 fail-closed（容器崩、可见），cgroup 模式安全降级。真正需要治理的是**插件持续不可用**，由 §12.13.3 的 tier-2 升级（healthcheck→liveness 重启）暴露给管理员。

**待接线（healthcheck 消费 `Healthy()`）**：`health.go` 的 `Check` 在 NRISupport 开启时，将 `nriPlugin.Healthy()` 纳入 SERVING 判定（`false` → `NOT_SERVING`）。注意启动顺序：healthcheck 与 NRI 插件的创建先后，需让 healthcheck 持有一个健康访问器（传 `func() bool` 或插件引用；NRISupport 关闭时该访问器恒为 healthy，不影响现有探测）。Prepare 侧**不**主动拦截（client 模式已 fail-closed，cgroup 模式安全降级，无需 Prepare 预判 NRI 可用性）。

#### 12.13.7 与参考实现的差异小结

| 维度 | dranet | dra-driver-cpu | 本设计 |
|---|---|---|---|
| 失败处理 | `Fatalf` 杀进程 | 降级不杀 | **两级：恢复优先，恢复无望经 healthcheck→liveness 干净重启**（checkpoint 安全，非 `Fatal`） |
| 重连 | 5 次无退避 | 5 次无退避 | **指数退避 + 持续重连**（宽限期后报 unhealthy 但不停重连） |
| 状态重建 | env 重放 | env 重放 | env 重放（`MANAGER_VGPU_CLAIM_UID`） |
| 就绪门 | — | — | **首次 Synchronize 前不回退**（闭合 §12.8 窗口） |

## 13. 参考资料

- [Kubernetes DRA KEP](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/4381-dra-structured-parameters)
- [Node Resource Interface (NRI)](https://github.com/containerd/nri)
- [kubernetes-sigs/dranet](https://github.com/kubernetes-sigs/dranet) — DRA + NRI 网络驱动
- [kubernetes-sigs/dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu) — DRA + NRI CPU 驱动
- [coldzerofear/device-mounter](https://github.com/coldzerofear/device-mounter) — 运行时设备热插拔
- 项目内：[dra_vgpu_multicontainer_claim_design.md](./dra_vgpu_multicontainer_claim_design.md)
- 项目内：[how_to_use_DRA_driver.md](./how_to_use_DRA_driver.md)
