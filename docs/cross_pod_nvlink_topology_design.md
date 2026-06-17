# 跨 Pod NVLink 拓扑亲和设计(轻量版)

> 本文为 vgpu-manager 在 **同节点跨 Pod 边界保持 NVLink 连通** 的设计。单 Pod 内的链路分配已落地;本文在**不影响现有功能**的前提下,给出让"同 Gang 的多个 Pod 落在同一 NVLink 连通分量"的**最小改造方案**。**本文档不包含已落地代码。**
>
> 本文是 [`multinode_topology_aware_scheduling_analysis.md`](./multinode_topology_aware_scheduling_analysis.md) 中 **L0 层**(节点内)的详细设计;跨节点聚拢(L2/L3)见该文。底座见 [`scheduler_strategy_topology_refactor_design.md`](./scheduler_strategy_topology_refactor_design.md)。

## 0. 适用边界(先划清楚,避免过度设计)

**跨 Pod NVLink 只在"单节点上跑了同 Gang 的多个 Pod"时才有意义。** 两种部署形态:

| 形态 | L0 是否需要 |
|---|---|
| **1 节点 = 1 Pod = 整机 8 卡**(典型多机数据并行) | ❌ **不需要**。节点内只有一个 Pod 独占所有卡,现有「单 Pod link 拓扑」已覆盖;跨节点聚拢交给 L2/L3 |
| **1 节点 = 多个 Gang Pod**(MPI 风格 / 每 rank 一卡一 Pod / 小作业打包) | ✅ **需要**。否则 Pod-A 选 GPU0-1、Pod-B 选 GPU4-5,二者不连通,Pod 间 NCCL 退化到 PCIe |

所以本设计是**面向"多 Pod 共节点"场景的增量增强**,默认关闭、按需开启,对最常见的整机整卡形态零影响。

## 1. 现状盘点(对齐最新代码)

> ⚠️ 本节修订自旧版:`scheduler_strategy_topology_refactor` 的 strict 模式 / greedy 降级 / TopK / `AllocationRequest` 抽象**均已落地**,行号与 API 已变。下表为当前真实状态。

### 1.1 已具备、可直接复用的基础设施

| 能力 | 当前位置 | 说明 |
|---|---|---|
| Gang 身份识别 | [`pkg/util/util.go:685`](../pkg/util/util.go#L685) `PodHasGangName` | 已支持 coscheduling/Volcano/Koordinator/原生四套方言,返回 `(gangName, bool)` |
| 连通分量地图 | [`pkg/device/types.go:829`](../pkg/device/types.go#L829) `computeLinkComponents` | union-find,产出 `maxLinkComponentSize` + `linkComponentByUUID map[string]int`,`NewNodeInfo` 构建时算好 |
| 连通校验 | [`pkg/device/types.go:1556`](../pkg/device/types.go#L1556) `AreDevicesLinked(uuids)` | O(K) 判断一组 UUID 是否同分量,strict-link 已在用 |
| 最大分量大小 | [`pkg/device/types.go:1542`](../pkg/device/types.go#L1542) `MaxLinkComponentSize()` | 已用于节点级拓扑 fitness 排序 |
| 跨 Pod 资源视图 | [`filter_predicate.go:488`](../pkg/scheduler/filter/filter_predicate.go#L488) `NodeMapByIndexValue` | 按计划落点分桶全部 vGPU Pod,经 `WithNodePods` 注入每个 NodeInfo |
| 预分配持久化 | [`consts.go:80`](../pkg/util/consts.go#L80) `PodVGPUPreAllocAnnotation` | Filter 末尾 `PatchPodPreAllocatedMetadata` 写入([:699](../pkg/scheduler/filter/filter_predicate.go#L699)) |
| 预分配解析 | [`types.go:267`](../pkg/device/types.go#L267) `PodDeviceClaim.UnmarshalText` / [`:985`](../pkg/device/types.go#L985) `ShouldCountPodDeviceAllocation` | 解析兄弟 UUID + 判断其预分配是否仍有效 |
| MutationCache 实时可见 | [`filter_predicate.go:707`](../pkg/scheduler/filter/filter_predicate.go#L707) `podLister.Mutation` | 前一个 Pod 的预分配立刻对下一个 Pod 的 Filter 可见 |
| **全局串行锁** | [`filter_predicate.go:479`](../pkg/scheduler/filter/filter_predicate.go#L479) `f.locker.Lock()` | 整个 `deviceFilter` 串行,**同 Gang 兄弟不会并发进入分配**,跨 Pod 状态无竞态 |
| strict + 回退骨架 | [`allocator.go:393`](../pkg/device/allocator/allocator.go#L393) `allocateByTopologyMode` / [`:448`](../pkg/device/allocator/allocator.go#L448) `handleTopologyFallback` | strict→拒绝节点、非 strict→发 `TopologyFallback` 事件,**cross-pod 可直接挂这套** |
| link 分配本体 | [`allocator.go:473`](../pkg/device/allocator/allocator.go#L473) `allocateLink` | 已含 `AllocateLink`(快路径)+ `AllocateLinkTopK`+`selectLinkCandidateByDevicePolicy`(组合 binpack/spread)+ greedy 降级 |

### 1.2 与旧文档的关键差异(影响方案)

1. **`BuildAllocationRequest(pod)` 只吃 pod、不吃 node**([request.go:132](../pkg/device/allocator/request.go#L132))。`req` 是**节点无关、一次解析、所有节点共用**的,**不能**往 `req` 上挂 per-node 的 anchor。→ anchor 必须在 per-node 的 `NodeInfo` 或 per-node 的 allocator 上算。
2. **`NodeInfo` 当前不保留 `nodePods`**:`nodePods` 只是 `NodeInfoOption` 的字段([types.go:724](../pkg/device/types.go#L724)),`NewNodeInfo` 用它累加用量后即弃,没存进结构体。→ 要让 NodeInfo 自解析 anchor,需**补一行**把它留下。
3. **strict / greedy / TopK 已落地**:旧文档"阶段 2 做 greedy 降级、阶段 3 自建 strict"已无必要,**直接复用**。cross-pod 改造量因此大幅缩小。

## 2. 核心思路:anchor 自解析,filter 零改动

把"同 Gang 兄弟在本节点已预分配的连通分量"作为 **anchor**,让 `allocateLink` 在 **anchor 分量窗口内**选卡。关键洞察:**`NodeInfo` 既持有 `linkComponentByUUID`,又(补一行后)持有本节点 pod 列表——anchor 可完全在 NodeInfo 内部自解析,`deviceFilter` 一行都不用改。**

```
deviceFilter (现有, 不改) → 串行循环, 每节点 NewAllocator(nodeInfo).Allocate(req)
        │  nodeInfo 已含: linkComponentByUUID + 本节点同 Gang 兄弟(已 Mutation 的预分配)
        ▼
allocateByTopologyMode (req.Topology==link)
        │  若 req.GangName != "" && HasGPUTopology:
        │     anchorRoot, ok := nodeInfo.GangAnchorComponent(req.GangName, req.Pod.UID)
        ▼
allocateLink(anchorRoot 窗口)
        ├─ ok(有兄弟): 候选 devices 过滤到 linkComponentByUUID==anchorRoot, 再跑 AllocateLink/TopK
        │     装得下 → 选卡;装不下 → 非 strict 回退全节点(现有行为) / strict 拒绝(现有 handleTopologyFallback)
        └─ !ok(首 Pod): 走现有 allocateLink(为自己选最优分量) [+ 可选 lookahead, 见 §4]
```

## 3. 阶段一:Anchor-Aware(推荐先落地,filter 零改动)

### 3.1 改动清单(最小集)

| 文件 | 改动 | 量 |
|---|---|---|
| [`request.go`](../pkg/device/allocator/request.go) | `AllocationRequest` 加 `GangName string`;`BuildAllocationRequest` 里 `req.GangName, _ = util.PodHasGangName(pod)` | ~3 行 |
| [`types.go`](../pkg/device/types.go) | ① `NodeInfo` 加字段 `nodePods []*corev1.Pod`,`NewNodeInfo` 补 `ret.nodePods = infoOption.nodePods`<br>② `computeLinkComponents` 顺手产出 `componentToUUIDs map[int][]string` 并存到 NodeInfo<br>③ 新增方法 `GangAnchorComponent`、`ComponentUUIDs`<br>④ `DeepCopy` 同步浅拷 `nodePods`、克隆 `componentToUUIDs` | ~70 行 |
| [`allocator.go`](../pkg/device/allocator/allocator.go) | `allocateByTopologyMode` 解析 anchorRoot 传入;`allocateLink` 增 `anchorRoot int`(-1=无)参数并按分量过滤候选 | ~40 行 |
| 开关 | per-pod 注解 `nvidia.com/cross-pod-topology: "true"`(默认 off,可被 webhook 设默认),仅在开启 + `req.GangName!=""` + `Topology==link` 时启用 anchor 逻辑。**已用注解取代早期的 `CrossPodLinkTopology` feature gate**(per-pod 更灵活、免重启) | ~5 行 |

**filter_predicate.go 不改。** 这是相比旧文档(要改 filter 加 `collectGangAnchors`、clone req)的最大简化——因为 NodeInfo 已被 `WithNodePods` 注入了本节点的同 Gang 兄弟。

### 3.2 NodeInfo 新方法

```go
// GangAnchorComponent 返回同 Gang 兄弟 Pod 在本节点已预分配的 NVLink 连通分量根。
// 遍历 nodePods,筛 PodHasGangName==gangName && UID!=self && ShouldCountPodDeviceAllocation,
// 解析其 PodVGPUPreAllocAnnotation 的 UUID,经 linkComponentByUUID 映射到分量根。
// 多兄弟分属不同分量时(语义已降级)取多数派根并记 V(3) 告警。
// 无有效兄弟(首 Pod)返回 (-1, false)。
func (n *NodeInfo) GangAnchorComponent(gangName string, self types.UID) (root int, ok bool)

// ComponentUUIDs 返回某连通分量根下的全部 GPU UUID(供窗口过滤 / lookahead 选分量)。
func (n *NodeInfo) ComponentUUIDs(root int) []string
```

### 3.3 allocateLink 的窗口过滤

在 [`allocateLink`](../pkg/device/allocator/allocator.go#L473) 取得 `devices` 后,若 `anchorRoot >= 0`:把 `devices` 与对应 `deviceStore` 过滤到 `linkComponentByUUID[uuid]==anchorRoot` 的子集,再跑现有的 `AllocateLink` / `AllocateLinkTopK`。

- 窗口内卡数 `>= needNumber`:正常在窗口内选,后续兄弟自然收敛同分量。
- 窗口内不足:
  - **非 strict**:回退到过滤前的全节点 `devices`(= 完全等同现有行为),不拒绝节点。
  - **strict**:返回 `(nil, false)` → 现有 `handleTopologyFallback` 以 `LinkTopologyUnsatisfied` 拒绝本节点(复用,无新代码)。

### 3.4 为什么不影响现有功能

- `req.GangName==""`(非 Gang Pod)→ anchorRoot 永远 -1 → `allocateLink` 走原分支,**逐字节等同现状**。
- 注解 `cross-pod-topology` 未设(默认)→ 即便 Gang Pod 也走原路径。
- 有 anchor 但装不下时,非 strict 回退到原 `devices`,行为不变。
- 不新增 informer;anchor 解析复用已注入 NodeInfo 的 `nodePods` + 已有 `linkComponentByUUID`,**热路径零额外网络/缓存开销**。

## 4. 阶段二(可选):首 Pod Lookahead

> **状态(2026-06-16):已决定暂缓,不实现。** 原因:获取 Gang 总卡数要么引入 PodGroup CRD informer,要么按 Pod 求和(部分 Pod 未创建时漏算);团队明确不想为此引入更多 CRD 解析。当前首 Pod 走现有单 Pod 选最优分量逻辑——可能选到偏小分量,后续兄弟在非 strict 下降级、strict 下拒绝节点(由阶段一/三覆盖)。若将来要做,优先非 CRD 的轻量来源。以下为保留的设计备忘,非待办。

**问题**:首 Pod 无 anchor,现有 `allocateLink` 只为自己挑最优分量,可能选了只够自己的小分量,后续兄弟挤不进 → 散开(非 strict)/失败(strict)。

**轻量实现**(仅在"多 Pod 共节点"且需要硬收敛时才值得做):

1. `deviceFilter` 在构建 `nodePodsMap` 后,对 `req.GangName` 求和同 Gang 各 Pod 的 vGPU 总数,写入**节点无关**的 `req.GangTotalNumber`(放 `req` 上合理,Gang 总数与节点无关)。这是本阶段**唯一**的 filter 改动(~10 行)。
   - 来源用 podLister 现有视图(同 Gang 所有 Pod 求和),无需新 informer;**缺陷**:部分 Gang Pod 尚未创建时会漏算 → 文档要求"先建满 PodGroup 再放 Pod 调度"。更稳的 PodGroup CRD informer 作为后续可选项,不在轻量范围。
2. 首 Pod 的 `allocateLink`(anchor 不存在)改为:在 `MaxLinkComponentSize() >= req.GangTotalNumber` 的前提下,用 `ComponentUUIDs` 枚举满足 `size >= GangTotalNumber` 的分量,在其中分别跑 link 选优,取最佳。
3. 节点级 fast-skip:`MaxLinkComponentSize() < GangTotalNumber` 时直接拒绝(把现有"仅排序提示"的 `MaxLinkComponentSize` 提升为硬约束),Reason 复用 `LinkTopologyUnsatisfied`。

## 5. 阶段三(可选):strict 跨 Pod

复用已落地的 strict 机制,几乎无新代码:

- 语义:整个 Gang 必须同一连通分量。通过现有 `LinkTopologyStrict`(`device-topology-mode: link-strict`)叠加 `cross-pod-topology` 注解表达,无需额外注解。
- 后续 Pod:anchor 存在但窗口装不下 → strict 下 `allocateLink` 返回 false → `handleTopologyFallback` 拒绝节点(现成)。
- 与抢占协同(进阶,非必须):`preempt_predicate` 的 `findAdditionalVictims` 增加"只抢 anchor 分量内低优先级 Pod"的过滤,避免抢占后仍分散。**留作后续**,不在轻量范围。

## 5.5 阶段四:跨节点子域对齐(rail 对齐,已落地)

**场景**:每 Pod 取节点子集(如 4/8)且节点有多个不连通 NVLink 域时,跨节点的兄弟 Pod 应落到**各节点对应的同一子域(rail)**,否则跨节点 NCCL 走错 rail 掉档。这超出同节点 anchor(union-find root 节点本地、不可跨节点比)与 Kueue(只到节点粒度)的能力,由本阶段在 extender 内解决。详细配合见 [`kueue_tas_integration.md`](./kueue_tas_integration.md) §7.2。

**机制(不引入新注解)**:
- **稳定 ordinal**:`NodeInfo` 按"分量最小 `Device.Index`"给每个连通分量赋 ordinal(`rootByOrdinal` / `ordinalByDeviceID`);同构节点 ordinal-k = 同 rail。
- **反查兄弟 ordinal**:兄弟的 `PodVGPUPreAllocAnnotation` **已编码所选卡的 device id**;`PodPreAllocatedDeviceIDs` 取出,经本节点 `ordinalByDeviceID` 反查 ordinal(同构假设下成立;异构由 Kueue TAS 节点级保证)。**零新注解、零额外 List**。
- **跨节点兄弟查询**:`filter` 复用已有 `nodePodsMap`(全节点桶)找同 Gang 已分配兄弟,装入 `req.GangSiblingDeviceIDs`(节点无关)。
- **对齐**:`allocateLink` 解析优先级 = 同节点 anchor(连通硬约束)> 跨节点 `AlignedComponentRoot`(按 ordinal 对齐)> 首 Pod 不约束。

**落地**:`types.go`(ordinal 字段/构建/`ComponentByOrdinal`/`AlignedComponentRoot`/`PodPreAllocatedDeviceIDs`)、`request.go`(`GangSiblingDeviceIDs`)、`filter_predicate.go`(`findGangSiblingDeviceIDs`,~15 行)、`allocator.go`(对齐分支)。同 `cross-pod-topology` 注解开关;整机整卡(单分量)= ordinal 恒 0、窗口=全节点,无副作用。

## 6. 与 Volcano #5049(反共置)正交互补

[Volcano PR #5049](https://github.com/volcano-sh/volcano/pull/5049) 的 `vgpu-podgroup-policy: spread` 让**同 PodGroup 的 Pod 不共用同一物理卡**。它与本设计**方向相反但正交**:

| | #5049 spread(反共置) | 本设计(link 亲和) |
|---|---|---|
| 方向 | 兄弟落**不同**卡 | 兄弟落**同一** NVLink 分量 |
| 粒度 | 设备级 | 拓扑级 |

**二者可同时启用** → 兄弟落同一 NVLink mesh 的**不同卡**上,正是分布式训练理想分布(每 rank 独占一卡 + rank 间走 NVLink)。建议作为本设计完成后的独立小特性跟进(~80 LOC,在 `filterDevices` 加"该卡是否已有同 Gang 兄弟"过滤)。

## 7. 测试矩阵

| 用例 | 场景 | 期望 |
|---|---|---|
| `Anchor_FollowSibling` | 同 Gang 两 Pod 顺序到同节点(阶段一) | Pod-2 落在 Pod-1 所选 UUID 同 `linkComponentByUUID` 根的卡上 |
| `DifferentGangs_Isolated` | 不同 Gang 同节点 | 各自独立选,不强制同分量 |
| `NonGang_Unchanged` | 非 Gang Pod | 完全走原路径,逐字节等同现状 |
| `NoRoom_NonStrict_Fallback` | 锚定分量空 slot 不足,非 strict | 回退全节点,不拒绝节点 |
| `NoRoom_Strict_Reject` | 同上但 strict | 拒绝节点,走 `handleTopologyFallback` |
| `Lookahead_PicksBigComponent` | 多分量节点,首 Pod 只需 2 卡但 Gang 共需 8(阶段二) | 首 Pod 选 ≥8 卡容量的分量 |
| `AnchorSplit_Majority` | 兄弟 anchor 跨多分量 | 非 strict 取多数派 + V(3);strict 拒绝 |
| `AnnotationOff_Noop` | 未设 cross-pod-topology 注解 | 即便 Gang Pod 也走原路径 |

性能:在 `filter_perf_test` 增 Gang 维度,确认 anchor 解析(遍历本节点 pod + map 查询)成本 < 5%。

## 8. 工作量与里程碑

| 阶段 | 量(估) | 风险 | 价值 |
|---|---|---|---|
| 阶段一 Anchor | ~120 LOC(**filter 不改**) | 低 | ★★★★ 多 Pod 共节点场景核心收益 |
| 阶段二 Lookahead | ~60 LOC(filter +10) | 中(漏算缺陷) | ★★★ 提升首 Pod 选分量正确率 |
| 阶段三 strict | ~30 LOC(复用) | 低 | ★★ 硬保证场景 |
| 反共置(#5049) | ~80 LOC | 低 | ★★ 与亲和叠加=训练理想分布 |

**节奏(2026-06-16)**:阶段一已落地(`28cf038` + 边界修复 `d2e0f88`);开关已由 feature gate 改为 per-pod 注解 `nvidia.com/cross-pod-topology`、并落地**阶段四跨节点子域对齐**(`6462ae6`,见 §5.5);阶段三 strict 随阶段一已具备(复用 `handleTopologyFallback`);**阶段二 Lookahead 已决定暂缓**(避免引入 CRD 解析,见 §4);反共置(#5049)决定不自研,改用 `link + device spread` 间接实现(spread 在 anchor 分量窗口内优先选空闲卡)。

## 9. 不在本设计范围

- Gang 调度本身(同节点 + 连续出队 + 原子提交):依赖外层(见 multinode 文档 L2/L3);本设计假设外层已保证同节点。
- 跨节点 NVLink(NVLink 不跨主机;GB200 IMEX 例外,走 DRA 路线,见 multinode 文档 §7)。
- DRA 模式下的等价能力:DRA 设备分配是声明式 ResourceClaim,跨 Pod 拓扑需在 claim 级别做,独立 follow-up。

## 10. 一句话总结

> 借助 NodeInfo 已持有连通分量地图、并(补一行后)持有本节点同 Gang 兄弟,**anchor 可在 allocateLink 内自解析、filter 零改动**;strict/回退/greedy/TopK 已落地可直接复用。阶段一 ~120 LOC、默认关、对非 Gang 与整机整卡形态零影响,即可让"同 Gang 多 Pod 共节点"收敛到同一 NVLink 分量。
