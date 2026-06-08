# 跨 Pod NVLink 拓扑分配设计

> 本文为 vgpu-manager 在 **跨 Pod 边界保持 NVLink 连通** 的设计稿。当前实现的链路拓扑分配仅在单 Pod 内生效;本文档分析典型分布式训练场景下的跨 Pod 需求、现有基础设施、设计方案与实施风险,作为后续实施的依据。**本文档不包含已落地代码**。

## 1. 背景与目标

### 1.1 场景

分布式训练作业(NCCL all-reduce / pipeline parallel)通常由 Gang(PodGroup)拆成 N 个 Pod,每个 Pod 申请若干张 GPU。当 Pod 之间的 GPU **不在同一 NVLink 连通域**时,Pod 间集合通信会退化到 PCIe(典型 32 GB/s)甚至跨 NUMA(更慢),NCCL 训练吞吐受限于此。

现状:vgpu-manager 仅在**单 Pod 内**调度多张 GPU 时通过 [`pkg/device/allocator/allocator.go:341`](../pkg/device/allocator/allocator.go#L341) 的 `allocateLink` 让 bestEffort 算法选 NVLink 连通最好的子集。一个 Pod 选完后,**下一个 Pod 进入 Filter 时不知道前一个 Pod 选了哪些卡**,只能在剩余资源里再做一次"局部最优",彼此连不连通完全是运气。

### 1.2 目标

- **跨 Pod 优先连通**:同 Gang 后续 Pod 优先选择与已分配兄弟 Pod 处于同一 NVLink 连通分量的 GPU。
- **首 Pod 前瞻**:Gang 第一个 Pod 选定连通分量时,考虑后续兄弟 Pod 的总卡数需求,挑选能装下整组的分量。
- **严格模式**:提供 `gpu-cross-pod-link-strict` 注解,强制 Gang 全程必须在同一连通分量,否则拒绝并触发抢占。

### 1.3 非目标

- 不在本项目内实现 Gang 调度本身。外层(scheduler-plugins / Volcano / Koordinator)负责保证 Gang 兄弟 Pod **同节点 + 连续出队**,vgpu-manager 在已 Gang 化的前提下做卡级最优。
- 不考虑跨节点 NVLink(NVLink 不跨主机)。
- 不动 NUMA 拓扑模式(`numa` / `numa-strict`)的语义,后续可类比扩展但不在本设计范围。

## 2. 现状盘点

### 2.1 已具备的基础设施

| 能力 | 位置 | 说明 |
|---|---|---|
| Gang 身份识别 | [`pkg/util/util.go:631`](../pkg/util/util.go) `PodHasGangName` | 同时支持 scheduler-plugins / Volcano / Koordinator 三套注解 |
| Gang 注解常量 | [`pkg/util/consts.go:29-32`](../pkg/util/consts.go#L29) | `CoschedulingPodGroupLabel` / `KoordinatorGangNameAnnotation` 等 |
| 节点连通分量地图 | [`pkg/device/types.go:660,791`](../pkg/device/types.go#L660) `computeLinkComponents` | NodeInfo 构建时跑 union-find,产出 `linkComponentByUUID map[string]int` 与 `maxLinkComponentSize int` |
| 连通分量校验 | [`pkg/device/types.go:1254`](../pkg/device/types.go#L1254) `AreDevicesLinked(uuids)` | O(K) 判断一组 UUID 是否同分量 |
| 最大分量大小 | [`pkg/device/types.go:1240`](../pkg/device/types.go#L1240) `MaxLinkComponentSize()` | 当前仅供 link 模式的节点级 fitness 排序使用,未参与分配决策 |
| 跨 Pod 资源视图 | [`pkg/client/pod_lister.go:64`](../pkg/client/pod_lister.go) `NodeMapByIndexValue` | 按"Pod 计划落到的节点"分桶 vGPU Pod;`PodPlanSchedulingNode` 优先用 `Spec.NodeName`,否则用 `PodPredicateNodeAnnotation` |
| 预分配 UUID 持久化 | [`pkg/util/consts.go:79`](../pkg/util/consts.go) `PodVGPUPreAllocAnnotation` | Filter 末尾 `PatchPodPreAllocatedMetadata` 写入,内含每容器 `[id_uuid_cores_memory]` 列表 |
| MutationCache 实时可见 | [`pkg/client/pod_lister.go`](../pkg/client/pod_lister.go) | 同 Filter 调用之间 patch 的 pod 立刻被下一个 Pod 的 `ListByIndexValue` 看到,无须等 informer watch |
| bestEffort 链路分配 | [`pkg/device/gpuallocator/greedy_policy.go:40,58`](../pkg/device/gpuallocator/greedy_policy.go) `AllocateLink` / `AllocateLinkTopK` | 在给定候选集内挑连通分最高的子集;Top-K 用于和 binpack/spread 组合 |
| 节点级抢占识别 Gang | [`pkg/scheduler/preempt/preempt_predicate.go:179`](../pkg/scheduler/preempt/preempt_predicate.go) | 抢占时已会聚合 Gang 名作为受影响指标 |

### 2.2 还缺什么

- **Filter 路径没有 Gang 感知**:`deviceFilter` 不区分 Gang 与非 Gang Pod,`allocateLink` 不接受"anchor / 已锁定连通分量"输入。
- **没有 Gang 总卡数信息**:首 Pod 无法判断当前分量够不够装整组。需要 PodGroup CRD 的 `MinResources` 或对同 Gang 兄弟 Pod 卡数求和。
- **没有 Gang 级共享状态**:严格模式需要把"已选定分量 ID"持久化到 Gang 维度,目前没有承载位置。
- **测试覆盖**:[`pkg/scheduler/filter/filter_perf_test.go`](../pkg/scheduler/filter/filter_perf_test.go) 与现有功能测试均无多 Pod 同 Gang 的链路连通场景。

## 3. 关键概念

- **Gang**:由 `PodHasGangName` 识别的同名 Pod 组,本文档假设外层调度器已保证它们**同节点 + 连续出队**。
- **连通分量(link component)**:节点上 GPU 经 NVLink 连接形成的等价类。`linkComponentByUUID[uuid] = root`,union-find 的根 ID 唯一标识分量;同 root ⇔ 任意两两 NVLink 可达(强度由 P2P 边权决定)。
- **Anchor**:同 Gang 已通过 Filter 的兄弟 Pod 在本节点上已经预分配(`PodVGPUPreAllocAnnotation`)的 UUID 集合。后续 Pod 应在 anchor 所在分量内选卡。
- **Anchor 分量**:`linkComponentByUUID[anchorUUID0]`,即 anchor 锁定的连通分量根 ID;通常整个 Gang 应该收敛到唯一一个 anchor 分量(否则前序兄弟已经裂开,语义降级)。
- **Lookahead 总需求**:同 Gang 所有 Pod 加起来需要的 vGPU 总数(`Σ container.VGPUNumber × replicas`)。

## 4. 设计方案

### 4.1 总体思路

把"已分配给同 Gang 兄弟 Pod 的 UUID"作为 anchor 传给 `allocateLink`,让 bestEffort 算法在**anchor 所在连通分量**的窗口内选卡;首 Pod 在没有 anchor 的情况下,用 lookahead 信息挑能装下整组的最大分量。

整体流程:

```
deviceFilter (调度第 N 个 Gang 兄弟 Pod):
  1. 取 nodePodsMap[node] 中同 Gang 兄弟 Pod
  2. 解析其 PodVGPUPreAllocAnnotation → anchorUUIDs
  3. 若 anchorUUIDs 非空:
       anchorComponent = linkComponentByUUID[anchorUUIDs[0]]
       (校验所有 anchor 都在同一分量,否则退化)
     若空(首 Pod):
       gangTotal = lookahead(同 Gang 总卡数)
       anchorComponent = 选满足 size(c) >= gangTotal 的最优分量
  4. allocateLink 时,把候选 deviceStore 过滤到只剩 anchorComponent 内的卡
  5. 在该窗口内跑 AllocateLink / AllocateLinkTopK
  6. strict 模式下若窗口装不下当前 Pod 需求 → 拒绝节点
     non-strict 下退回原 allocateLink 行为(分量内尽力,装不下回到全节点)
```

### 4.2 分三个阶段落地

| 阶段 | 内容 | 必要性 |
|---|---|---|
| **阶段 1** | Anchor-aware allocateLink + 同 Gang 兄弟查询 | 基础能力 |
| **阶段 2** | Lookahead:首 Pod 按 Gang 总卡数挑分量 | **关键**,无 lookahead 会大量失败 |
| **阶段 3** | Strict 模式:Gang 级共享分量 ID + 严格回退 | 高保证场景 |

#### 阶段 1:Anchor-Aware Allocation

**改动点**:

1. **`AllocationRequest` 扩展**:在 [`pkg/device/allocator/request.go`](../pkg/device/allocator/request.go) 的 `AllocationRequest` 上新增字段:

   - `GangName string` — `PodHasGangName(pod)` 解析结果,空表示非 Gang。
   - `GangAnchorUUIDs []string` — 同 Gang 兄弟在本节点已预分配的 UUID 列表。**注意**:本字段是 per-node 上下文,不在 `BuildAllocationRequest` 里计算,而在 deviceFilter 的 per-node 循环里填,然后克隆 `req` 传给 `NewAllocator`。或者另起一个 `AllocationContext` per-node struct,避免污染 `AllocationRequest`(更干净的做法)。

2. **同 Gang 兄弟扫描**:在 [`filter_predicate.go:452`](../pkg/scheduler/filter/filter_predicate.go) 拿到 `nodePodsMap` 之后,新增辅助函数 `collectGangAnchors(gangName, nodePods, selfUID) []string`:

   - 遍历 `nodePods`,筛 `PodHasGangName(p) == gangName && p.UID != selfUID`。
   - 解析每个兄弟的 `PodVGPUPreAllocAnnotation`(已有 `PodDeviceClaim.UnmarshalText`),抽出所有 UUID。
   - 去重返回。

3. **`allocateLink` 接受 anchor**:[`allocator.go:341`](../pkg/device/allocator/allocator.go#L341) 签名扩展为接收 `anchorUUIDs []string`:

   - 若 `anchorUUIDs` 非空,根据 `nodeInfo.linkComponentByUUID` 拿到 anchor 分量 root。
   - 把 `devices` 过滤到只剩同 root 的卡,再调 `gpuallocator.AllocateLink` / `AllocateLinkTopK`。
   - 若过滤后 `< needNumber`,non-strict 下退回过滤前的 `devices` 继续走(降级到当前行为),strict 下返回 `(nil, false)` 让上层拒绝。

4. **Anchor 分量一致性校验**:若 anchor 内 UUID 分属多个分量(意味着前序兄弟已经裂开,Gang 已经"破损"),记录 `klog.V(3)` 告警并选 anchor 中**多数派分量**作为本 Pod 的目标。

**特性**:

- 非 Gang Pod 路径完全不变(`anchorUUIDs` 为空,`allocateLink` 走原分支)。
- 不引入新 informer。Gang 兄弟查询直接复用 podLister 的 `nodePodsMap[node.Name]`,基本零额外成本(分桶天然限定到本节点)。
- MutationCache 保证刚预分配的兄弟在下一个兄弟进 Filter 时立刻可见;无须额外同步。

**失败场景(阶段 1 已知不足)**:首 Pod 没有 anchor 时退回单 Pod bestEffort,可能选到只够自己的小分量,后续兄弟装不下 → 退到其他节点或失败。**阶段 2 解决**。

#### 阶段 2:首 Pod Lookahead

**目标**:首 Pod 进 Filter 时,知道整个 Gang 一共要多少张卡,挑能装下所有 Gang GPU 的连通分量。

**Gang 总卡数获取(两条路径)**:

- **路径 A:PodGroup CRD**(推荐)
  - 新增 PodGroup informer(scheduler-plugins 的 `scheduling.x-k8s.io/v1alpha1`)。
  - 从 PodGroup spec 的 `MinResources[VGPUNumberResourceName]` 直接拿总数。
  - **优点**:权威、稳定、不依赖 Pod 是否已经创建。
  - **缺点**:多引入一个 CRD 依赖;只覆盖标准 PodGroup,Volcano / Koordinator 各自的 CRD 需要分别适配。
- **路径 B:Pod 求和**(备选)
  - 从 podLister 查所有同 Gang Pod(无论是否在本节点、是否已调度),对每个 Pod 求 `Σ container.VGPUNumber × max(1, replicaCount)`,得到 Gang 总数。
  - **优点**:无 CRD 依赖,降级路径。
  - **缺点**:若部分 Gang Pod 尚未创建出来,会漏算;首 Pod 来得过早时尤其不准。
  - **缓解**:对漏算敏感的部署,要求用户**先创建满 PodGroup 再放 Pod 调度**。

**实现建议**:优先走路径 A;若读不到 PodGroup(注解里只有 Volcano / Koordinator 风格,而项目暂未集成对应 informer),退回路径 B。

**节点级新增 fast-skip**(在 [`filter_predicate.go`](../pkg/scheduler/filter/filter_predicate.go) Tier-1 容量门附近):

- 首 Pod 进入 Gang 拓扑路径时,若 `nodeInfo.MaxLinkComponentSize() < gangTotal`,直接拒绝节点,Reason 用 `LinkTopologyUnsatisfied` + Detail `gang needs %d in one component, node max is %d`。
- 现成的 `MaxLinkComponentSize()` 字段在阶段 2 前只用于排序,本阶段把它从"排序提示"提升到"硬约束"。

**分量挑选(首 Pod)**:

- 在 `linkComponentByUUID` 上反向索引出 `componentToUUIDs map[int][]string`(可在 `NewNodeInfo` 时顺手算并缓存)。
- 筛 `len(componentUUIDs) >= gangTotal && 该分量内可用 vGPU slot >= 自己当前 Pod 需求` 的分量集合。
- 若多个候选分量满足:
  - **链路最优**:沿用 `gpuallocator.AllocateLink` 但限制在每个候选分量内分别跑,取分数最高的。
  - **policy 兼容**:与 binpack/spread 组合时,走 `AllocateLinkTopK` 在多个候选分量上分别产出 Top-K,统一交给 `selectLinkCandidateByDevicePolicy` 排序选优。

#### 阶段 3:Strict 模式与 Gang 级状态

**新增注解**:`gpu-cross-pod-link-strict`(在 `pkg/util/consts.go` 加常量),布尔语义,贴在 Pod 上(整个 Gang 应保持一致)。

**语义**:

- 整个 Gang 必须落在同一连通分量。
- 首 Pod 选定分量后,把分量 ID 写到 Gang 级共享状态:
  - **路径 A(若有 PodGroup informer)**:写在 PodGroup CR 的状态字段或注解(`vgpu-manager/gang-link-component`)。
  - **路径 B**:写在首 Pod 自己的注解,后续 Pod 通过 `PodHasGangName + 同 Gang 兄弟查询` 找到首 Pod,读分量 ID。
- 后续 Pod 进入 Filter 时:
  - 在 anchor 解析阶段先尝试读 Gang 级共享状态;若读到分量 ID,**强制**只用该分量,装不下就拒绝。
  - 若读不到(首 Pod 还没完成 Filter,或写入未同步),阶段 2 的 lookahead 兜底:同样按 gangTotal 选分量。

**抢占交互**:

- Strict 失败时,触发 vgpu-manager 自身的 preempt 路径([`pkg/scheduler/preempt/preempt_predicate.go`](../pkg/scheduler/preempt/preempt_predicate.go))。
- 优先抢占**目标分量内**优先级低于本 Gang 的 Pod;若仍不够,放弃节点而非抢占其他分量(防止把好不容易腾出的 GPU 又分散到不连通的分量)。
- `refineForNode` 里 `findAdditionalVictims` 候选挑选阶段加 link-component 维度过滤。

## 5. 跨 Pod 状态传递与数据流

### 5.1 状态传递路径

```
Pod-1 (Gang 首 Pod) 进入 deviceFilter
  ├─ 无 anchor → lookahead 选分量 C
  ├─ NewAllocator(req with anchorComponent=C).Allocate(req)
  ├─ 选出 N1 张卡,UUID 写入 PodVGPUPreAllocAnnotation
  └─ PatchPodPreAllocatedMetadata → podLister.Mutation(newPod)
                                       │
                                       ▼  立刻对下一个 Filter 调用可见
Pod-2 (Gang 兄弟) 进入 deviceFilter
  ├─ NodeMapByIndexValue 拿到本节点 Pod,含 Pod-1
  ├─ collectGangAnchors → 解出 Pod-1 的 N1 个 UUID
  ├─ anchorComponent = linkComponentByUUID[anchorUUIDs[0]] = C
  ├─ NewAllocator(req with anchor=C).Allocate(req)
  └─ allocateLink 内把 devices 过滤到 C,再选 N2 张
  ...
```

### 5.2 同一 Filter 调用内不存在并发

- `serialFilterNode` 锁保证同一节点的 Filter 串行,不存在两个 Gang 兄弟"同时进入 Filter"的竞态。
- MutationCache 是 in-process overlay,Mutation 调用后下一次 `ListByIndexValue` 立刻反映,无 informer watch 延迟。

### 5.3 跨节点不传状态

跨 Pod NVLink 的前提是 Gang 同节点;不同节点上的 Gang 兄弟从对方 anchor 读出的分量 ID 在自己的 union-find 里无意义,所以本设计**只在同节点 anchor 情况下生效**。这要求外层 gang scheduling 保证同节点。

## 6. 与外层 gang scheduling 的协同

vgpu-manager 单独搞不定 Gang 调度。这套设计假设外层提供以下保证:

| 外层职责 | 谁来做 | 失败后果 |
|---|---|---|
| Gang 兄弟同节点 | scheduler-plugins Coscheduling / Volcano / Koordinator | 跨节点 NVLink 无意义,本设计退化为单 Pod link |
| Gang 兄弟连续出队 | 同上 | 非 Gang Pod 抢占分量内空 slot 概率上升;部分 Gang 失败 |
| Gang 兄弟原子提交(全部成功或全部回滚) | 同上 | 部分 Pod 卡分配成功、部分失败时,vgpu-manager 已 patch 的预分配不会自动撤销;但 [`pkg/device/types.go:972`](../pkg/device/types.go#L972) 的 `ShouldCountPodDeviceAllocation` 通过 `PodPredicateTimeAnnotation` 的 grace 窗口会自动释放卡 |

vgpu-manager 这边专心做:

- "假设兄弟连续来,我把它们装到同一连通分量里"。
- 不假设兄弟必然全部来;若中途 Gang 取消、外层抛弃部分 Pod,vgpu-manager 通过 stuck-pod grace 机制自动放卡(参见 `ShouldCountPodDeviceAllocation` 的失败/超时分支),Gang 级状态(若存在)在 PodGroup 删除时跟随消失。

## 7. 失败模式与边界

### 7.1 首 Pod 选错分量

- **症状**:首 Pod 选了某分量 C1,但 C1 在阶段 1 还没 lookahead,只够装首 Pod 自己;Pod-2 进 Filter 时 C1 已无空 slot,被迫挑别的分量或退节点。
- **缓解**:**阶段 2 必须做**。阶段 1 单独上线会导致 Gang 成功率不达预期。

### 7.2 Anchor 分量 split

- **症状**:Pod-1 在 C1 选 2 张,Pod-2 因为 C1 满了,被迫(non-strict 降级)从 C2 选 2 张;Pod-3 进 Filter 时 anchor 内 UUID 分属 C1 与 C2。
- **处理**:`collectGangAnchors` 后做分量一致性检查,选**多数派分量**作为本 Pod 目标;同时记录 V(3) 告警。strict 模式下直接拒绝本 Pod(并触发 Gang 级抢占)。

### 7.3 Gang 总卡数误判

- 路径 A(PodGroup):若用户提交 PodGroup 后又改了 `MinResources`,可能与实际 Pod 数对不上。设计上以 PodGroup spec 为准;实际超出能力时 lookahead 失败 + 拒绝节点,触发外层重新调度。
- 路径 B(Pod 求和):若部分 Gang Pod 尚未创建,首 Pod 选了不够大的分量。**已知缺陷**,文档显式声明用户需"先 PodGroup 后 Pod"。

### 7.4 与抢占的交互

- vgpu-manager 现有抢占模块在 [`pkg/scheduler/preempt/preempt_predicate.go`](../pkg/scheduler/preempt/preempt_predicate.go) 已能识别 Gang 名作为受影响指标(`gangNameSet`),用于聚合事件。本设计要求扩展 `findAdditionalVictims`,在 strict 失败的回退路径上**只抢占目标连通分量内的低优先级 Pod**,避免抢占跨分量导致后续 Pod 仍然分散。

### 7.5 跨 Pod link + 单 Pod numa 组合

- 单 Pod 内若同时指定 `link-strict + numa-strict`,要求同时在同分量同 NUMA。该 Pod 的候选窗口先按 anchor 分量过滤,再按 NUMA 过滤,最后跑 bestEffort。通常会进一步缩小候选,无新风险。

### 7.6 测试覆盖

- 现有 `Test_FilterPerf` 和 `Test_DeviceFilter` 均无多 Pod 同 Gang 链路连通场景。
- 需要新增:
  - 单元测试:`Test_CrossPodLink_Anchor`(阶段 1)、`Test_CrossPodLink_Lookahead`(阶段 2)、`Test_CrossPodLink_Strict`(阶段 3)。
  - 集成测试:多 Pod 同 Gang 顺序调度,断言所选 UUID 全部在同一 `linkComponentByUUID` 根。
  - 性能基线:`Test_FilterPerf` 新增 Gang 维度,确认 anchor 解析成本可忽略(预期 < 5%)。

## 8. 工作量与里程碑

| 阶段 | 改动量(估算) | 主要文件 | 风险 |
|---|---|---|---|
| 阶段 1 | ~200 LOC | `request.go`, `allocator.go`, `filter_predicate.go`, `util.go`(`collectGangAnchors`) | 低 |
| 阶段 2 | ~250 LOC + PodGroup informer 接入(若走路径 A) | 上 + `pkg/scheduler/`(informer)+ 节点级 fast-skip | 中(新依赖)|
| 阶段 3 | ~200 LOC | 上 + `consts.go`(注解)+ `preempt_predicate.go`(同分量抢占) | 中 |
| 测试 | ~400 LOC | `*_test.go` + `filter_perf_test.go` | 低 |

总计 ~1050 LOC + 1 个 informer + 测试。

里程碑建议:

1. **阶段 1 独立上线**:仅做 anchor-aware,默认行为不变(无 anchor 时走原逻辑)。可灰度,不需要外层联动。
2. **阶段 2 在 PodGroup 集成验证后上线**:lookahead 是关键收益点,阶段 1 单独价值有限。
3. **阶段 3 作为可选硬保证**:严格模式与抢占同分量逻辑较复杂,等阶段 1+2 在生产稳态运行后再上。

## 9. 不在本设计范围内

- Gang 调度本身(Pod 同节点、连续出队、原子提交):依赖外层。
- NUMA 跨 Pod 拓扑(类比但不在本次)。
- 跨节点 NVLink(NVLink 不跨主机)。
- DRA 模式下的等价能力:DRA 的设备分配是声明式 ResourceClaim,跨 Pod 拓扑需要在 ResourceClaim 级别做,设计路径不同,作为独立 follow-up。
