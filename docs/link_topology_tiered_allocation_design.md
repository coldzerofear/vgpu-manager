# Link 拓扑分层分配重构设计

## 状态

设计定稿，分步实施中

## 1. 背景

当前 link 拓扑分配基于 NVIDIA `bestEffortPolicy` 的**划分枚举**（把节点上全部候选卡切分成若干个 size 大小的子集，对所有划分打分取最优）。这带来三个互相纠缠的问题：

### 1.1 组合爆炸，靠阈值硬压

| 节点卡数 / 请求 | 划分数 |
|---|---|
| 8 / 4 | 35 |
| 12 / 4 | 5,775 |
| 16 / 4 | 2,627,625 |
| 16 / 8 | 2,027,025 |
| 24 / 8 | ~5×10⁹ |

为此引入 `--best-effort-max-gpus`（默认 12），超过就切贪心。这是一个**把算法缺陷暴露成用户配置**的补丁：运维要理解"为什么 12"、"超了会怎样"，而这本该是实现细节。

### 1.2 设备策略被丢弃

`deviceStore` 是已按 binpack/spread 排好序的，`Filter(getDeviceUUIDs(deviceStore))` 也保序传入，但 `bestEffortPolicy.Allocate` **纯按链路分重新选择**，排序结果被丢弃。

为补救引入了 `AllocateLinkTopK` + `canonicalSetKey` 去重 + `candidateSetScore` + `selectLinkCandidateByDevicePolicy`，在链路等价的 top-K 集合间做二级排序。但 `K = linkTopKCandidates = 5` 是个窗口：**最优的 binpack 解落在 top-5 之外时，binpack 静默失效**。

对比 NUMA 路径：`CanNotCrossNumaNode` → 按 NUMA 分组（保持 deviceStore 顺序）→ 组内取前 N。全程保序，策略天然生效。**两条路径行为不一致，且 NUMA 那条才是对的。**

### 1.3 需要事后校验兜底

`bestEffortPolicy` 返回最高分划分但**不拒绝零分结果**，所以在部分 NVLink 的节点上可能返回跨岛集合。今天必须靠 `AreDevicesLinked` 做事后二次校验才能实现 strict 语义。

## 2. 目标

1. 删除 `--best-effort-max-gpus`，让算法自适应硬件形态，不靠配置
2. 设备策略与拓扑正交，且正交性是**结构保证**而非事后补丁
3. strict 判定成为选择过程的直接产物，消除"返回不连通集合"这一类失败模式
4. 单卡请求纳入拓扑路径（为跨 Pod rail 对齐铺路）
5. **不新增任何用户可见配置**

### 非目标

- 不改变 `link` / `link-strict` / `numa` / `numa-strict` 的用户可见语义
- 不改变节点排序（`LinkTopologyFitness` 保持不变）
- 不引入跨节点拓扑求解

## 3. 硬件形态调研

设计必须适配未知的客户硬件组合。以下矩阵均来自公开的实测 `nvidia-smi topo -m` 输出。

### 3.1 NVSwitch（DGX-2 / HGX A100 / H100 / B200）

全部 GPU 对为同一值（H100 为 `NV18`），NVSwitch 是非阻塞交叉开关，任意两卡单跳全带宽。

**特征：完全均匀。** 任意 N 张卡等价，选择问题不存在。

### 3.2 DGX-1 V100 混合立方网格

```
     GPU0 GPU1 GPU2 GPU3 GPU4 GPU5 GPU6 GPU7   CPU Affinity
GPU0  X   NV1  NV1  NV2  NV2  SYS  SYS  SYS    0-19,40-59
GPU1 NV1   X   NV2  NV1  SYS  NV2  SYS  SYS    0-19,40-59
GPU2 NV1  NV2   X   NV2  SYS  SYS  NV1  SYS    0-19,40-59
GPU3 NV2  NV1  NV2   X   SYS  SYS  SYS  NV1    0-19,40-59
GPU4 NV2  SYS  SYS  SYS   X   NV1  NV1  NV2    20-39,60-79
GPU5 SYS  NV2  SYS  SYS  NV1   X   NV2  NV1    20-39,60-79
GPU6 SYS  SYS  NV1  SYS  NV1  NV2   X   NV2    20-39,60-79
GPU7 SYS  SYS  SYS  NV1  NV2  NV1  NV2   X     20-39,60-79
```

**关键性质：分层阶梯分不开它。** NV2 边为 `0-3, 0-4, 1-2, 1-5, 2-3, 4-7, 5-6, 6-7`，并查集后 8 张卡仍是**一个分量**；NV1 阈值同理。这正是 NVIDIA 注释里 "the non-hierarchical nature of the various links" 的含义。

因此这是**唯一必须枚举**的形态。规模：C(8,2)=28、C(8,4)=70，最坏 420 次 pair 查表。

补充：GPU0-3 / GPU4-7 分属两个 NUMA，而这两个四卡组恰是 4 卡请求的最优解（组内 pair 分 900，跨组混合仅 620）。**NUMA 数据源能分开链路矩阵分不开的东西** —— 两个数据源互补，但本设计不围绕它做特殊逻辑。

### 3.3 NVLink Bridge（A100 / L40S PCIe + 桥接）

成对桥接的 GPU 显示 `NV12`，其余为 `PXB` / `PHB` / `SYS`。

**非均匀，但分层阶梯可干净分开**：NVLink 档给出若干 2 卡分量；N>2 时降到 Switch / NUMA 档拿到更大的组。**无需枚举。**

### 3.4 纯 PCIe（无 NVLink）

只有 `PIX` / `PXB` / `PHB` / `NODE` / `SYS`。这些值来自 `GetTopologyCommonAncestor`，**本质是一棵树**（PCIe 层级），因此可以精确分解。

### 3.5 结论

| 形态 | 链路图性质 | 处理方式 |
|---|---|---|
| NVSwitch | 均匀完全图 | 顺序取，零成本 |
| NVLink Bridge | 分层可分 | 树下降 |
| 纯 PCIe | 严格树 | 树下降 |
| **DGX-1 混合网格** | **非树、非均匀** | **分量内枚举（≤420 次）** |

分层的依据是**数据源的数学性质**，不是硬件型号：`GetP2PLink` 返回公共祖先层级（树），`GetNVLink` 返回链路条数（非树）。

## 4. 设计

### 4.1 核心规则

> **每一层排序：先比设备策略分，相等时比链路分，仍相等时比 deviceStore 顺序。**

`Score(u, p, NonePolicy)` **恒返回 0**，所以 `none` 不是特例分支，而是"策略分全部相等"的退化情形，自动落到链路分。这条规则同时用于**选分量**和**组内选卡**。

| 策略 | 策略分 | 实际生效判据 |
|---|---|---|
| `none` | 恒 0，全平 | 链路分 → deviceStore 序 |
| `binpack`/`spread`，节点有差异 | 有区分 | 策略分主导 |
| `binpack`/`spread`，节点全空 | 全平 | 链路分 → deviceStore 序（同 `none`） |

第三行很重要：**新节点上所有组利用率相同**，平局是常态而非边缘情况。没有链路分这一层，binpack 在最常见的场景下会丢掉全部拓扑信息。

### 4.2 算法

```
① tier 走查：NVLink → Switch → NUMA → Any
   找第一个「分量内候选 ≥ N」的档，记录落档
   └─ strict 判定 = 落档是否 ≥ NVLink（link）/ 同 NUMA（numa）
   └─ 任何档都找不到 → 分配失败

② 选分量（同档多组时）
   排序键：组策略分 ↓ → 组内最优 N 子集链路分 ↓ → 组 ordinal ↑

③ 组内选 N 张
   按策略分等值段（run）依次收取；跨不过去的那一段内部按链路分挑
   ├─ 段内均匀 → 全平 → 取 deviceStore 前几个
   └─ 段内非均匀 → 组合枚举（内部预算，超则贪心）

④ 单个分量装不下 N → 按子分量容量降序跨取
```

#### 为什么 ③ 用「等值段」而不是「集合策略分求和」

`deviceStore` 已按策略分排序，**策略分相等的设备必然相邻**，所以等值段是天然存在的结构。用段切分：

- 避免浮点求和相等判断（脆弱）
- 枚举被限制在**单个等值段内**，规模只会更小
- `none` 的策略分恒 0 → 整组是一个段 → 链路分决定全部，与今天一致

注意：`NewDevicePolicyPriority` 对 `NonePolicy` 用的是 `[ByNuma, ByDeviceIdAsc]`，没有策略分参与排序。实现时 `none` 必须**显式**当作"整组一个段"，不能从比较键反推。

#### 为什么 ④ 的贪心是最优的

设从子分量 i 取 aᵢ 张（Σaᵢ = k），总分 =

```
Σ 子分量内部得分(aᵢ) + 父层分 × ( C(k,2) − Σ C(aᵢ,2) )
```

父层分固定，且子分量内部得分 ≥ 父层分 × C(aᵢ,2)（子分量更紧），所以集中在少数子分量上必然更优 → **按容量降序取即最优，无需枚举**。此结论在树上严格成立。

### 4.3 不变式

| | 不变式 | 意义 |
|---|---|---|
| **I1** | 拓扑分支不得修改 `deviceStore`（不重排、不原地过滤） | 回退可靠 |
| **I2** | 拓扑分支的选择结果必须是 `deviceStore` 的**保序子序列** | 正交性是结构保证 |
| **I3** | 拓扑分配失败时，回退结果恒等于 `deviceStore[:N]` | 干净回退 |
| **I4** | 拓扑等价的候选之间，`deviceStore` 中靠前者优先 | 确定性 |

I2 是核心：它把"策略与拓扑正交"从一个需要论证的性质变成了一个可以断言的性质。**拓扑层只做筛选，不做排序。**

## 5. 行为兼容性

### 5.1 NVSwitch 机型零变化（已验证）

今天 `bestEffortPolicy.Allocate` 在均匀 fabric 上的行为：所有 pair 同分 → 所有划分总分相同 → `score > bestScore` 恒假、`bestPartition == nil` 仅首次成立 → **bestPartition = 第一个划分**；其内所有集合同分 → `bestSet = filteredBestPartition[0]` = `devices[0..N-1]` = `deviceStore[:N]`。

新算法在三条路径上均收敛到同一结果：

| 场景 | 路径 | 结果 |
|---|---|---|
| NVSwitch + `none` | 单组 → 一个段 → 链路分全平 → deviceStore 序 | `deviceStore[:N]` |
| NVSwitch + `binpack`（有占用） | 单组 → 段按序取（deviceStore 本就按 binpack 排） | `deviceStore[:N]` |
| NVSwitch + `binpack`（全空） | 单组 → 一个段 → 链路分全平 → deviceStore 序 | `deviceStore[:N]` |

**NVSwitch 机型上，无论什么设备策略，本次改造零行为变化。**

### 5.2 完整兼容矩阵

| 形态 | 走到哪 | 枚举 | 相比今天 |
|---|---|---|---|
| NVSwitch 8/16/32 卡 | NVLink 档单一分量，均匀 | ❌ | **逐字节一致** |
| NVLink bridge | NVLink 档 2 卡分量；N>2 降档 | ❌ | 更准 |
| **DGX-1 混合网格** | NVLink 档 8 卡分量，非均匀 | ✅ ≤420 次 | 见 5.3 |
| 纯 PCIe 8/16 卡 | NVLink 档全单例 → Switch → NUMA → Any | ❌ | 更准，且能说清降到哪档 |
| 无拓扑注解（gate 关 / 旧 plugin） | `HasGPUTopology()=false` | ❌ | **完全不变** |
| 无 NUMA 数据 | numa 模式失败 | ❌ | **完全不变** |
| 异构集群 | 每节点独立走查 | 按节点 | 更准 |
| MIG 卡 | `filterDevices` 已剔除 | — | 不变 |

### 5.3 会变的两处

**(a) `binpack` / `spread` 在同档内改由设备策略主导。** 今天 binpack 只能在链路等价的 top-5 集合内打破平局，窗口外的最优解静默失效。这是本次改造的目的之一，属于修复。

**(b) DGX-1 + `none`。** 今天取"最优划分内的最优集合"，新的取"分量内全局最优集合"。当前 Pod 的链路分只升不降，但剩余卡配置可能变差。

缓解：枚举时**主判据集合分数，平局时比较补集的最优分数**（≤8 卡，补集打分几乎免费）。DGX-1 的 N=4 场景下 `{0,1,2,3}` 与 `{4,5,6,7}` 同分 900、补集也同分 → 平局退到 deviceStore 序 → `{0,1,2,3}`，**与今天一致**。第一版即带此判据，把"可能变差"的窗口压到接近零。

## 6. 数据结构与开销

### 6.1 NodeInfo 新增

`NewNodeInfo` 对**每个候选节点每次 Filter 都执行**，大集群上是数千次/调度周期，所以数据结构必须廉价。

```
tierComponent  [4][]int   // 一块 [4*n]int backing，按设备索引取分量根
tierUniform    [4]bool    // 该档下所有分量内部链路是否均匀
gpuRail        map[string]string  // 保留（今天用完即弃），Step 3 需要
```

n=8 时 `tierComponent` 是 32 个 int、**一次分配**，相对现有 `deviceMap` / `deviceIndexMap` / `deviceList` 是噪音级。对比之下 4 个 `map[string]int` 会贵一个数量级。

均匀性判据："分量内所有 pair 的链路类型完全相同"。误判方向安全 —— 把均匀误判成非均匀只会多跑一次小枚举，无正确性影响。

### 6.2 分配路径开销

| 硬件 | 成本 |
|---|---|
| 均匀（NVSwitch） | O(n×4) 走查 + O(n) 取卡，**比今天的 35~70 个划分打分更便宜** |
| 树形（PCIe / bridge） | O(n×4) + O(n log n) 下降，无枚举 |
| DGX-1 | 上述 + ≤420 次 pair 查表 |

内部保留一个组合数预算常量做兜底（超预算 → 贪心），应对未知未来硬件。**它不是配置。**

## 7. 删除清单

| 文件 / 符号 | 原因 |
|---|---|
| `--best-effort-max-gpus` flag + chart 接线 | 算法自适应，不再需要 |
| `gpuallocator.AllocateLink` / `AllocateLinkTopK` 阈值分支 | 同上 |
| `greedy_policy.go` 的 `bestEffortMaxGPUs` 相关代码 | 同上（贪心本身作为内部兜底保留） |
| `bestEffortPolicy.AllocateTopK` + `canonicalSetKey` | top-K 窗口机制不再需要 |
| `linkTopKCandidates`（K=5 魔数） | 同上 |
| `candidateSetScore` / `selectLinkCandidateByDevicePolicy` | 策略正交由结构保证 |
| `allocateLink` 中 `policy == NonePolicy` 快路径分叉 | `none` 退化为策略分恒 0 |
| `AreDevicesLinked` 事后校验调用 | strict 判定改为落档比较 |
| `NodeInfo.AreDevicesLinked` 方法本体 | 同上，无调用方 |
| `NodeInfo.MaxLinkComponentSize` / `MaxNVLinkComponentSize` | 由 `LinkTierMaxComponentSize(tier)` 统一取代（旧的两个方法是同一问题的两个特例） |
| `NodeInfo.LinkComponentOf` | 分量归属改由 `ConnectedAtTier` 在候选集上现算（诱导子图），不再按全节点分量根判断 |
| `NodeInfo.maxLink/maxSwitch/maxNUMA/maxNVLinkComponentSize` 字段 | 合并进 `tiers.maxSize[tier]` |

**保留但不再被生产代码调用**：`gpuallocator` 的 `Allocator` / `Policy` / `bestEffortPolicy`。
它们是 `comparison_test.go` 的对照基线（"新算法不劣于 main"的证据本身），且与 NVIDIA 上游
逐字节兼容便于后续同步。原因写在 `pkg/device/gpuallocator/doc.go`，避免被后人当成疏漏清掉。

`--best-effort-max-gpus` 是**唯一不能硬删的**：pflag 遇到未知参数直接退出，硬删会让所有仍在
传该参数的存量部署 crash-loop。保留为已弃用的 no-op（解析、告警、忽略、从 help 隐藏），下个
版本再摘。

净代码量为负，净配置量 −1。

## 8. 分步落地

每步独立可发布、可回滚。

### Step 1 — 数据层（无行为变化）

`tierComponent` / `tierUniform` / 保留 `gpuRail`。纯新增字段，不接线到分配路径。

### Step 2 — 算法层（行为变化在此）

tier 走查 + 树下降 + 等值段选卡 + 分量内枚举，替换 `allocateLink`。执行第 7 节删除清单。

### Step 3 — 单卡与跨 Pod rail 对齐

移除 `allocateByTopologyMode` 的 `needNumber <= 1` 短路。N=1 时拓扑约束除 anchor 外**平凡满足**，不得产生 `TopologyFallback` 事件噪音。

跨 Pod 对齐改为两级降级：

```
L1  同 rail（有 node-gpu-domain → rail key；否则 → 设备索引）
     └─ 候选不足 ↓
L2  同分量签名（今天的跨节点对齐）
     └─ 仍不足 ↓
L3  不对齐（非 strict）/ 拒节点（strict）
```

N=1 在全连通节点上 L2 空转（整节点一个分量、签名处处相同），靠 L1 干活 —— 这正是今天缺失的能力。

依据：rail-optimized 组网的行业约定是 **Rail 0 连接每台服务器的 GPU 0、Rail 1 连接 GPU 1**，NCCL 亦有 `NCCL_CROSS_NIC=0` 专门避免跨 rail。所以无 rail map 时用**设备索引**回退与硬件约定一致，前提是各节点 GPU↔NIC 布局同构 —— 该假设现有 `ord:N` 回退路径已在使用。

### 正交项 — 抢占 victim 拓扑定向

`refineForNode` 只追加不回溯，link 请求下驱逐散落在不同分量的 Pod 永远凑不出连通集合 → 过度驱逐或误丢节点。在 `sortVictimsByPreference` 前置一个"是否占用目标分量"的键，目标分量复用 `Preempt` 已解析的 gang anchor。无 anchor 时不做定向（保持原排序）。

与 Step 1~3 正交，可独立合入。

## 9. 测试矩阵

### 9.1 机型 fixture（Step 2 正确性的唯一防线）

| fixture | 来源 | 验证 |
|---|---|---|
| NVSwitch 8 卡 | 全 `NV18` | 结果 == `deviceStore[:N]`，各策略均成立 |
| **DGX-1 8 卡** | 本文 3.2 实测矩阵 | 枚举命中；N=4 得 `{0,1,2,3}`；N=2 得 NV2 对 |
| NVLink bridge 8 卡 | 4 组 `NV12` + `PXB`/`SYS` | N=2 命中桥接对；N=4 降到 Switch/NUMA 档 |
| 纯 PCIe 8 卡 | `PIX`/`PXB`/`PHB`/`SYS` | 逐档降级，落档正确 |

⚠️ DGX-1 fixture 建议在真机上跑一次 `nvidia-smi topo -m` 对齐后再定稿，不同批次 / 驱动版本可能有差异。

### 9.2 不变式测试

I1~I4 各一组断言，尤其 I2（保序子序列）应对所有 fixture × 所有策略成立。

### 9.3 回归

- 无拓扑节点：行为与改造前逐字节一致
- `link-strict` 在各 fixture 上的拒绝/通过判定与改造前一致（NVSwitch、bridge、纯 PCIe）
- 单卡非 gang Pod：Step 3 后结果与改造前一致

## 10. 可观测性

指标由 extender 现有端口的 `/metrics` 暴露。**每个指标只有一个计数单位**，写在 Help 里 —— extender 有两个天然单位，混用会产出"看起来有意义但不是"的数字。

| 指标 | 单位 | 回答什么 |
|---|---|---|
| `topology_placement_total{mode,result}` | **每 Pod** | 申请 link/numa 的 Pod 实际拿到了什么连通性。`result != 满足值` 就是全部静默降级 |
| `pod_policy_total{node_policy,device_policy,topology_mode}` | **每 Pod** | 用户实际在申请什么，据此判断拓扑工作对本集群是否有价值 |
| `crosspod_alignment_total{result}` | **每 Pod** | 跨 Pod 对齐用的是 rail / component / 没对上 |
| `topology_strict_reject_total{mode}` | 每节点评估 | strict 契约拒了多少节点。与放置率对比即可判断是否过度约束 |
| `node_reject_total{code}` | 每节点评估 | 按结构化原因分桶的节点拒绝 |
| `link_search_total{algo}` + `link_search_candidates` | **每次搜索** | 分量内组合搜索是否真的被执行。均匀 fabric 恒为 0；非零即证明集群里有 DGX-1 类非均匀机型 |
| `filter_duration_seconds{stage}` | 每 Filter 调用 | `node` / `device_work` / `device_lock_wait` |

四条设计约束：

1. **每 Pod 的指标在 filter 里发**，不在 allocator 里 —— allocator 按节点运行，一个 Pod 可能评估多个节点才落地，在那里计数会把一个 Pod 报成多次放置。allocator 把结果记在该节点的 request 快照上，filter 在确定赢家后读取。
2. **抢占的 dry-run 完全不计**。`NewSimulationAllocator` 在源头关掉所有可观测副作用；一次抢占会跑多轮模拟，事后从看板里减是减不干净的。
3. **锁等待独立观测**。`SerializedNodeFilter` 默认开启，把排队和实际工作折在一起会让竞争看起来像"分配变慢"。两者是独立观测值而非相减后的单值 —— Filter 是并发的，把等待时间挂在共享的 `gpuFilter` 上做减法就是 data race。
4. **所有来自注解的 label 必须过白名单**（`metrics.PolicyLabel` / `metrics.TopologyLabel`，未知值归入 `other`）。注解解析器会把无法识别的值**原样透传**（`parseSchedulerPolicy` 返回 `SchedulerPolicy(raw)`，`BaseTopology` 的 default 分支返回原值），这对调度是正确的 —— 认不出的策略不匹配任何比较器，Pod 按默认顺序调度即可 —— 但作为指标 label 就意味着**任何能创建 Pod 的租户都可以在 scheduler 进程内无限制地生成时间序列**，而 Prometheus 客户端的 metric map 永不回收。白名单把这条路堵死，同时仍能通过 `other` 桶看见"有人写错了策略名"。

多容器 Pod 取**最差**的那个容器的结果：一个 Pod 的放置质量不会好过它最不走运的容器。

### 10.1 两条必须成立的口径不变式

1. **申请了 link/numa 的 Pod，必须都出现在 `topology_placement_total` 里。**
   filter 只在 outcome 非空时发这个指标，而 `pod_policy_total` 无条件发，所以只要
   有一条放置路径绕过了 `allocateByTopologyMode`，两个指标就会对不上，且**差值不可见** ——
   看板上表现为"拓扑放置量凭空变少"，而不是"有降级"。

   曾经踩过的坑：`pickDeviceClaims` 对**受迫集合**（`needNumber == len(deviceStore)`，
   即节点剩余候选数恰好等于请求数）有一条快路径，直接返回不走拓扑分发。省下的是一次
   长度等于 `needNumber` 的排序和一次只有唯一解的分档走查，代价却是这批放置完全不上报。
   而受迫集合恰恰是**节点已经没有余地挑好卡**的场景，把它悄悄丢掉会让指标系统性地偏乐观。
   现已删除该快路径，由 `Test_TopologyOutcome_AlwaysRecordedForTopologyPods` 钉住。

2. **没申请拓扑的 Pod 不能进 `topology_placement_total`。** 否则分母就不再是
   "申请了 link/numa 的 Pod"，比率失去意义。`none` 模式不调用 `recordOutcome`，
   同一个测试的最后一个子用例反向钉住这一点。

### 10.2 单卡请求的档位口径

单卡集合没有"对"，在任何档位上都是真空连通的。若照字面采纳，分档走查会在
`TierNVLink` 就停下并上报 `nvlink` —— 无论节点是 NVSwitch、纯 PCIe 还是完全无链路。
这既让 strict 形同虚设，也让 `topology_placement_total{result="nvlink"}` 混入大量
根本没有 NVLink 的放置。

因此单卡的档位改由**该卡自身的连通性**决定（`HasLinkPeerAtTier`，节点级、不看对端是否空闲）：
卡在某档上有真实对端才算落在该档。多卡集合不受影响 —— 其成员在该档上本就两两连通。

## 11. 风险

1. ~~**DGX-1 fixture 保真度**~~ —— **已消除**。改为直接引入 NVIDIA/go-gpuallocator 的官方
   fixture（`upstream_fixtures_test.go`），并用 `Test_Upstream_MatchesHandTranscribedDGX1`
   在 CI 里断言手抄矩阵与上游逐边一致。上游 fixture 还带有本地 fixture 近似掉的一类信息
   （同一对卡同时有 NVLink 与 PCIe 边），属于本地 fixture 结构上无法覆盖的保真度类别。
2. **5.3(b) 的剩余卡配置** —— 补集平局判据能覆盖常见请求规模，但非全覆盖
3. **均匀性判据的边界** —— 若某机型在同一分量内混用不同 NVLink 条数但期望被当作均匀处理，会多跑一次枚举（无正确性影响，仅开销）
4. **非均匀机型上的实测结论** —— 三组对照共 441 例（本地 fixture 48、NVIDIA 上游 fixture 64、
   随机任意拓扑 329）：**真实机型 112 例中 0 例变差**，4 例变好，其余持平；随机任意拓扑
   328/329 不劣，1 例低 10%。准确表述是**"从不更差、偶尔更好"**，而非一致占优。
