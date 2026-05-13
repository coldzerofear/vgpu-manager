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
- 为 [coldzerofear/device-mounter](https://github.com/coldzerofear/device-mounter) 风格的运行时热插拔提供合法、可维护的钩子。
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

[coldzerofear/device-mounter](https://github.com/coldzerofear/device-mounter) 当前作为独立工具，绕过 K8s 资源模型给运行中容器注入设备。这种"旁路工具"路线 fragile，且 K8s 不感知，资源核算对不上。

NRI 引入后的目标形态：

1. **vgpu-manager 的 NRI 插件成为节点上的"vGPU 设备网关"**：所有 vGPU 容器的 mount / env / cgroup device rule 都由它注入。
2. **device-mounter 通过本地 socket / gRPC 与 NRI 插件通信**：请求"给容器 X 增加设备 Y / 调整内存上限"，NRI 插件通过 `UpdateContainer` 钩子执行实际的 mount 和 cgroup 调整。
3. **资源核算回流到 K8s**：device-mounter 在请求成功后修改对应的 ResourceClaim（追加 request）或 pod annotation，让 scheduler 和后续运维工具感知。

这样 device-mounter 的角色从"绕过 K8s 的旁路工具"变成"K8s 资源模型的合法扩展"，长期可维护性大幅提升。

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

## 12. 参考资料

- [Kubernetes DRA KEP](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/4381-dra-structured-parameters)
- [Node Resource Interface (NRI)](https://github.com/containerd/nri)
- [kubernetes-sigs/dranet](https://github.com/kubernetes-sigs/dranet) — DRA + NRI 网络驱动
- [kubernetes-sigs/dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu) — DRA + NRI CPU 驱动
- [coldzerofear/device-mounter](https://github.com/coldzerofear/device-mounter) — 运行时设备热插拔
- 项目内：[dra_vgpu_multicontainer_claim_design.md](./dra_vgpu_multicontainer_claim_design.md)
- 项目内：[how_to_use_DRA_driver.md](./how_to_use_DRA_driver.md)
