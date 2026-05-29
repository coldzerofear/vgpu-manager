# 调度策略与拓扑分配重构设计

> 本文为 vgpu-manager 调度器 binpack/spread 策略与 NUMA/NVLink 拓扑分配能力的重构设计稿。当前实现已具备完整功能但存在若干精度、性能与边界缺陷,本文档拆解问题、提出三阶段改造方案,作为后续实施的依据。**本文档不包含已落地代码**。

## 1. 背景

vgpu-manager 调度器支持两级 binpack/spread 策略 + NUMA / NVLink 拓扑亲和分配,实现分布在以下文件:

| 文件 | 职责 |
|---|---|
| [`pkg/scheduler/filter/filter_predicate.go`](../pkg/scheduler/filter/filter_predicate.go) | extender Filter,节点级 binpack/spread + 拓扑节点排序 |
| [`pkg/device/allocator/allocator.go`](../pkg/device/allocator/allocator.go) | 容器级设备分配入口,设备级排序 + 拓扑路径分发 |
| [`pkg/device/allocator/priority.go`](../pkg/device/allocator/priority.go) | LessFunc 工厂与节点/设备评分函数 |
| [`pkg/device/allocator/numa.go`](../pkg/device/allocator/numa.go) | NUMA 节点分组与回调式策略选择 |
| [`pkg/device/gpuallocator/`](../pkg/device/gpuallocator/) | NVIDIA bestEffort 算法,基于 P2P 链路打分 |

整体架构为**两级调度**:

```
节点级 (deviceFilter)
  └─ NodeSchedulerPolicy (binpack/spread) + 拓扑节点偏好 → 排序节点列表
       └─ 设备级 (allocator.allocateOne, 逐容器)
            └─ DeviceSchedulerPolicy (binpack/spread) → 排序设备列表
                 └─ allocateByTopologyMode: link / numa / none → 选定 N 张卡
```

两级用独立 annotation 控制:`nvidia.com/node-scheduler-policy`、`nvidia.com/device-scheduler-policy`、`nvidia.com/device-topology-mode`。

## 2. 现有实现盘点

### 2.1 评分函数

[`priority.go:159-176`](../pkg/device/allocator/priority.go) 节点级评分公式:

```go
GetBinpackNodeScore(info) = 100 × (numUsed% + memUsed% + coreUsed%) / 3
GetSpreadNodeScore(info)  = 100 × (numFree% + memFree% + coreFree%) / 3
```

降序排序,binpack 选"用得满"的节点,spread 选"空得多"的节点。`BySpreadNodeScore` / `ByBinpackNodeScore` 内置 `nodeScoreMap` 缓存,避免排序比较时重复计算评分。

[`priority.go:217-236`](../pkg/device/allocator/priority.go) 设备级排序(字典序多级):

```go
Binpack: ByAllocatableMemoryAsc → ByAllocatableCoresAsc → ByDeviceIdAsc
Spread:  ByAllocatableMemoryDes → ByAllocatableNumberDes → ByAllocatableCoresDes → ByDeviceIdAsc
None:    ByNuma → ByDeviceIdAsc
```

### 2.2 NUMA 拓扑路径

[`numa.go:109-117`](../pkg/device/allocator/numa.go) `CanNotCrossNumaNode`:

- 若 `needNumber > 1` 且**存在**一个 NUMA 节点能容纳 `needNumber` 张卡,返回 NumaNodeDevice 并 `true`
- 否则返回 `false`,allocator 静默回退 `allocateByNumbers`(跨 NUMA)

NUMA 节点之间的选择走 `SchedulerPolicyCallback(policy, callback)`,根据 binpack/spread 排序 NUMA 节点并依次试匹配。

### 2.3 NVLink 拓扑路径

[`besteffort_policy.go`](../pkg/device/gpuallocator/besteffort_policy.go) 是 NVIDIA `gpuallocator` 标准实现,枚举所有"把可用 GPU 划分为 size 大小子集"的划分,对每个划分按链路质量打分,选最高分划分里最高分子集。链路打分细分 18 个等级,从 PCIe 跨 CPU(10 分)到 18 路 NVLink(1800 分)。

调用入口在 [`allocator.go:160-211`](../pkg/device/allocator/allocator.go):

```go
case string(util.LinkTopology):
    if alloc.nodeInfo.HasGPUTopology() {
        devices, _ := alloc.nodeInfo.GetDeviceList().Filter(getDeviceUUIDs(deviceStore))
        devices = gpuallocator.NewBestEffortPolicy().Allocate(devices, nil, needNumber)
        if len(devices) == needNumber {
            return allocateByDevices(deviceStore, devices, needCores, needMemory)
        }
        klog.Warningf("LinkTopology allocation failed, fallback to normal allocation mode")
    }
```

### 2.4 节点级拓扑偏好

[`priority.go:91-115`](../pkg/device/allocator/priority.go):

```go
ByNodeGPUTopology / ByNodeNUMATopology:
  通过 HasGPUTopology() / HasNUMATopology() 二元判断
  有拓扑信息的节点排前面,没有的排后面
```

由 `ApplyTopologyMode` 插到节点排序最前面(最高优先级)。

## 3. 已识别问题

### 3.1 问题汇总表

| 编号 | 问题 | 影响 |
|---|---|---|
| **#1** | 节点评分采用三维(num/mem/core)简单平均,与 pod 实际请求无关 | binpack/spread 精度损失,极端场景会选错节点 |
| **#2** | Link 拓扑路径完全忽略 device-scheduler-policy 排序 | 用户同时配 link + binpack/spread 时 binpack/spread 静默失效 |
| **#3** | NUMA 不满足时**静默**降级跨 NUMA,无 strict 模式 | 延迟敏感任务无感降速,无法表达"必须 NUMA 对齐"契约 |
| **#4** | NUMA 节点评分同样资源无关 | 同 #1 |
| **#5** | bestEffort 是组合爆炸算法,大节点性能差 | 16+ GPU 节点上单次 filter 可能耗时百毫秒到秒级 |
| **#6** | Link 拓扑同样静默降级 | 同 #3,Link 路径下表现一致 |
| **#7** | 节点级拓扑偏好仅判断"有没有",不评估"能不能满足" | 选了拓扑节点但设备级降级,排序失去意义 |
| **#8** | 拓扑仅对单容器多卡 pod 生效,多容器跨容器拓扑不优化 | 多容器训练任务无法享受拓扑收益 |

### 3.2 问题详细分析

#### 3.2.1 评分模型问题(#1, #4)

当前评分:

```go
score = 100 × (numUsed% + memUsed% + coreUsed%) / 3
```

三个维度等权平均。但:

- pod 主要请求 memory 的场景:节点 memory 90% 使用率、卡数 10% 使用率 → 平均 33%。binpack 应优先选这个节点,但平均分把它压低
- pod 仅请求计数的场景:节点 memory 0% 使用率反而拉低评分

业界主流(k8s `NodeResourcesBalancedAllocation` / `RequestedToCapacityRatio`、Volcano binpack 插件)都引入了请求加权。

#### 3.2.2 Link 路径忽略设备级 policy(#2)

当前 `allocateByTopologyMode` 的 Link 分支:

```go
devices, _ := alloc.nodeInfo.GetDeviceList().Filter(getDeviceUUIDs(deviceStore))
devices = gpuallocator.NewBestEffortPolicy().Allocate(devices, nil, needNumber)
```

`deviceStore` 是已经被 binpack/spread 排过序的设备列表,但 `BestEffortPolicy.Allocate` 内部按链路分数完全重新选择,**排序结果被丢弃**。

NUMA 路径则不同——`CanNotCrossNumaNode(needNumber, deviceStore)` 保留 deviceStore 顺序;NUMA 节点内 `allocateByNumbers` 取前 N 个,binpack/spread 被尊重。**两条路径行为不一致**。

#### 3.2.3 strict 模式缺失(#3, #6)

```go
case string(util.NUMATopology):
    if alloc.nodeInfo.HasNUMATopology() {
        numaNode, notCrossNuma := CanNotCrossNumaNode(needNumber, deviceStore)
        if notCrossNuma {
            ...
        }
    }
// 跳过 if 块,直接 fall through 到 allocateByNumbers(跨 NUMA)
return allocateByNumbers(deviceStore, needNumber, needCores, needMemory)
```

任何不满足条件(节点无拓扑信息、单 NUMA 卡数不够、Link 算法找不到解)都静默回退到无拓扑分配。用户**无任何感知**。

#### 3.2.4 bestEffort 性能(#5)

`iterateGPUPartitions` 枚举所有"把 n 张卡划分成 size 大小子集"的划分。规模估算:

| 节点 GPU 数 | size | 划分数量 | 量级 |
|---|---|---|---|
| 8 | 2 | 105 | 可接受 |
| 8 | 4 | 35 | 可接受 |
| 12 | 4 | 5,775 | 边界 |
| 16 | 4 | 2,627,625 | 慢 |
| 16 | 8 | 2,027,025 | 慢 |
| 24 | 8 | ~5×10⁹ | 不可接受 |

每个划分还要对每个 set 算 set score(O(size²) 的 pair 枚举)。**每次 filter 调用每个候选节点都跑一遍**,大节点场景下整次调度延迟显著。

#### 3.2.5 节点级拓扑偏好乐观(#7)

```go
ByNodeGPUTopology = func(p1, p2 *device.NodeInfo) bool {
    return p1.HasGPUTopology() && !p2.HasGPUTopology()
}
```

只看节点"是否上报了 topology annotation",不看拓扑实际能不能满足 pod 需求。结果:一个**有 topology 但实际链路质量很差或卡不够**的节点会排在**无 topology 信息但其实够用**的节点前面。设备级到位才知道用不上,回退 normal,**节点排序的"拓扑优先"承诺空头**。

#### 3.2.6 多容器拓扑(#8)

[`allocator.go:42`](../pkg/device/allocator/allocator.go) `Allocate(pod)` 逐容器调用 `allocateOne(pod, container)`,每个容器独立做拓扑分配。多容器场景下:

- 容器 A 要 2 卡,在 GPU 0-1(NVLink 直连)
- 容器 B 要 2 卡,选剩余 GPU 中链路最优,可能落在 GPU 4-5
- **A 和 B 之间没有跨容器拓扑亲和**,即便它们逻辑上属于同一训练任务

## 4. 改造目标与原则

### 4.1 目标

- **评分精度**:binpack/spread 真正反映 pod 资源需求,而非维度无关平均
- **可组合性**:Link 拓扑与 binpack/spread 可叠加
- **可契约化**:用户可表达"必须满足拓扑否则不调度"
- **可扩展性**:大节点(16+ GPU)调度延迟可控
- **完整性**:多容器同 pod 享受跨容器拓扑亲和

### 4.2 设计原则

1. **向后兼容默认**:新行为靠新 annotation/flag 启用,默认行为不变。已有部署升级后无感
2. **每阶段独立可发布**:Phase A/B/C 之间无强耦合
3. **接口而非分支**:用 `AllocationRequest` / `Scorer` 等抽象消除散落的 `if topology == link else if ...`
4. **可观测**:fallback / strict 失败等关键决策点必须发 Event 与 V(3) 日志

## 5. 总体方案

```
Phase A:安全 & 边界 (低风险,立即收益)
  ├─ A1. 拓扑 strict 模式 (修 #3, #6)
  ├─ A2. bestEffort 规模保护 + greedy 降级 (修 #5)
  ├─ A3. Link + binpack/spread 可组合 (修 #2)
  └─ A4. 节点级拓扑可行性预判 (修 #7)

Phase B:评分模型重构 (中风险,精度提升)
  ├─ B1. RequestProfile 抽象与加权评分 (修 #1, #4)
  ├─ B2. Score(Utilization, Profile, Policy) 统一接口
  └─ B3. 评分缓存 key 改造

Phase C:Allocator pod 级化 (高风险,功能完整)
  ├─ C1. AllocationRequest 抽象
  ├─ C2. Pod 级拓扑分配 (修 #8)
  └─ C3. 容器级分配从 pod 级结果派生
```

---

## 6. Phase A:安全 & 边界

### 6.1 A1. 拓扑 strict 模式

**[`pkg/util/consts.go`](../pkg/util/consts.go)** 扩展 `TopologyMode` 枚举:

```go
const (
    NoneTopology       TopologyMode = "none"
    NUMATopology       TopologyMode = "numa"        // 现有,best-effort
    NUMATopologyStrict TopologyMode = "numa-strict" // 新增
    LinkTopology       TopologyMode = "link"        // 现有,best-effort
    LinkTopologyStrict TopologyMode = "link-strict" // 新增
)
```

**[`pkg/device/allocator/allocator.go`](../pkg/device/allocator/allocator.go)** `allocateByTopologyMode` 改签名返回 error:

```go
func (alloc *allocator) allocateByTopologyMode(
    pod *corev1.Pod, deviceStore []*device.Device,
    policy util.SchedulerPolicy, needNumber int,
    needCores, needMemory int64,
) ([]device.DeviceClaim, error) {

    if needNumber <= 1 {
        return allocateByNumbers(deviceStore, needNumber, needCores, needMemory), nil
    }
    topo, strict := parseTopologyMode(pod)

    switch topo {
    case util.LinkTopology:
        claims := alloc.allocateLink(deviceStore, needNumber, needCores, needMemory)
        if len(claims) == needNumber {
            return claims, nil
        }
        if strict {
            return nil, &TopologyUnsatisfiedError{
                Mode: util.LinkTopology,
                Node: alloc.nodeInfo.GetName(),
            }
        }
        alloc.sendEventf(pod, corev1.EventTypeWarning, "TopologyFallback",
            "Link topology unsatisfiable, falling back to normal allocation")

    case util.NUMATopology:
        claims := alloc.allocateNUMA(deviceStore, policy, needNumber, needCores, needMemory)
        if len(claims) == needNumber {
            return claims, nil
        }
        if strict {
            return nil, &TopologyUnsatisfiedError{
                Mode: util.NUMATopology,
                Node: alloc.nodeInfo.GetName(),
            }
        }
        alloc.sendEventf(pod, corev1.EventTypeWarning, "TopologyFallback",
            "NUMA topology unsatisfiable, falling back to cross-NUMA allocation")
    }
    return allocateByNumbers(deviceStore, needNumber, needCores, needMemory), nil
}

type TopologyUnsatisfiedError struct {
    Mode util.TopologyMode
    Node string
}

func (e *TopologyUnsatisfiedError) Error() string {
    return fmt.Sprintf("%s topology unsatisfiable on node %s", e.Mode, e.Node)
}
```

调用方(`allocateOne`)拿到 `TopologyUnsatisfiedError` 当前节点直接 fail,filter 会跳过这个节点。多节点候选时,strict 失败节点被排除,选其他节点;若所有节点都 strict 失败 → pod Unschedulable + Event 写明原因。

**用户体验**:strict 模式下能从 pod Event 看到清晰原因 `"link topology unsatisfiable on node X"`,而非莫名调度成功后性能掉档。

### 6.2 A2. bestEffort 规模保护

**[`pkg/device/gpuallocator/allocator.go`](../pkg/device/gpuallocator/allocator.go)** 新增上限保护与 greedy 降级:

```go
// 命令行 flag,默认 12
var bestEffortMaxGPUs = 12

func (a *Allocator) AllocateWithLimit(num int) []*Device {
    available := a.remaining.SortedSlice()
    if len(available) > bestEffortMaxGPUs {
        klog.V(2).InfoS("Available devices exceed bestEffort threshold, using greedy",
            "available", len(available), "threshold", bestEffortMaxGPUs)
        return greedyLinkAllocate(available, num)
    }
    return a.policy.Allocate(available, nil, num)
}
```

**新增 [`pkg/device/gpuallocator/greedy_policy.go`](../pkg/device/gpuallocator/greedy_policy.go)**:

```go
// greedyLinkAllocate: O(n²·k) 替代 bestEffort 的 O(C(n,k)·partitions)
// 算法:
//   1. 计算每个 GPU 的"邻居链路总分"作为起点排名
//   2. 选 rank 最高的 GPU 作为种子
//   3. 迭代:每次加入与当前 set 配对分数最高的 GPU
// 性质:每张卡只考虑一次"边际增益",不保证全局最优但接近,且复杂度可控
func greedyLinkAllocate(devices []*Device, size int) []*Device {
    if size <= 0 || size > len(devices) {
        return nil
    }
    if size == len(devices) {
        return devices
    }

    rank := make([]int, len(devices))
    for i := range devices {
        for j := range devices {
            if i != j {
                rank[i] += calculateGPUPairScore(devices[i], devices[j])
            }
        }
    }
    seed := 0
    for i := 1; i < len(devices); i++ {
        if rank[i] > rank[seed] {
            seed = i
        }
    }
    selected := []*Device{devices[seed]}
    selectedIdx := map[int]bool{seed: true}

    for len(selected) < size {
        bestIdx, bestScore := -1, -1
        for i, cand := range devices {
            if selectedIdx[i] {
                continue
            }
            score := 0
            for _, s := range selected {
                score += calculateGPUPairScore(s, cand)
            }
            if score > bestScore {
                bestScore = score
                bestIdx = i
            }
        }
        if bestIdx < 0 {
            break
        }
        selected = append(selected, devices[bestIdx])
        selectedIdx[bestIdx] = true
    }
    return selected
}
```

**配置入口**:`cmd/device-scheduler/options/options.go` 加 flag:

```go
fs.IntVar(&opt.BestEffortMaxGPUs, "best-effort-max-gpus", 12,
    "When the candidate GPU count exceeds this threshold, fall back to greedy "+
        "link allocation instead of exhaustive bestEffort search.")
```

**预期效果**:8/12 卡节点行为不变(走 bestEffort,全局最优);16+ 卡节点走 greedy,延迟降至毫秒级。

### 6.3 A3. Link + binpack/spread 可组合

**[`pkg/device/gpuallocator/besteffort_policy.go`](../pkg/device/gpuallocator/besteffort_policy.go)** 新增 `AllocateTopK`:

```go
// AllocateTopK returns up to k candidate sets sorted by link score (highest first).
// Allows the caller to apply secondary ordering (e.g. binpack/spread on device
// utilization) when multiple link-topology-equivalent sets exist.
func (p *bestEffortPolicy) AllocateTopK(available, required []*Device, size, k int) [][]*Device {
    type scoredSet struct {
        set   []*Device
        score int
    }
    seen := make(map[string]bool) // dedupe sets by their sorted UUID concatenation
    var candidates []scoredSet

    iterateGPUPartitions(available, size, func(partition [][]*Device) {
        if !gpuPartitionContainsSetWithAll(partition, required) {
            return
        }
        for _, set := range partition {
            if !gpuSetContainsAll(set, required) || gpuSetCountPadding(set) > 0 {
                continue
            }
            key := setKey(set)
            if seen[key] {
                continue
            }
            seen[key] = true
            candidates = append(candidates, scoredSet{set, calculateGPUSetScore(set)})
        }
    })
    sort.SliceStable(candidates, func(i, j int) bool {
        return candidates[i].score > candidates[j].score
    })
    if len(candidates) > k {
        candidates = candidates[:k]
    }
    out := make([][]*Device, len(candidates))
    for i, c := range candidates {
        out[i] = c.set
    }
    return out
}
```

**[`pkg/device/allocator/allocator.go`](../pkg/device/allocator/allocator.go)** Link 分支:

```go
func (alloc *allocator) allocateLink(deviceStore []*device.Device,
    devicePolicy util.SchedulerPolicy, needNumber int, needCores, needMemory int64,
) []device.DeviceClaim {
    devices, _ := alloc.nodeInfo.GetDeviceList().Filter(getDeviceUUIDs(deviceStore))
    topK := 5
    if devicePolicy == util.NonePolicy {
        topK = 1 // 不需要二次排序时省力
    }
    candidates := gpuallocator.NewBestEffortPolicy().AllocateTopK(devices, nil, needNumber, topK)
    if len(candidates) == 0 {
        return nil
    }
    chosen := selectByDevicePolicy(candidates, deviceStore, devicePolicy)
    return allocateByDevices(deviceStore, chosen, needCores, needMemory)
}

// selectByDevicePolicy applies binpack/spread to choose among link-topology-
// equivalent candidate sets. Candidates are already sorted by link score
// (highest first); we keep the order stable when policy is None so the
// existing "highest link score" behavior is preserved.
func selectByDevicePolicy(candidates [][]*gpuallocator.Device,
    deviceStore []*device.Device, policy util.SchedulerPolicy,
) []*gpuallocator.Device {
    if policy == util.NonePolicy || len(candidates) == 1 {
        return candidates[0]
    }
    type scored struct {
        set   []*gpuallocator.Device
        score float64
    }
    out := make([]scored, len(candidates))
    for i, set := range candidates {
        out[i] = scored{set, candidateSetUtilization(set, deviceStore)}
    }
    sort.SliceStable(out, func(i, j int) bool {
        if policy == util.BinpackPolicy {
            return out[i].score > out[j].score // 利用率高优先
        }
        return out[i].score < out[j].score // spread:利用率低优先
    })
    return out[0].set
}

// candidateSetUtilization computes the average "already used resource" ratio
// across the devices in the set. Used to compare candidate sets for binpack
// (high = pack tightly into already-used devices) or spread (low = avoid
// already-used devices).
func candidateSetUtilization(set []*gpuallocator.Device, deviceStore []*device.Device) float64 {
    sum := 0.0
    for _, d := range set {
        for _, sd := range deviceStore {
            if sd.GetUUID() == d.UUID {
                sum += deviceUsedRatio(sd)
                break
            }
        }
    }
    return sum / float64(len(set))
}
```

**用户体验**:配 `link + binpack` 时,在链路质量等效的候选 set 中优先选已用率高的(继续打满);配 `link + spread` 优先选已用率低的(避免热点)。两种策略**首次真正叠加生效**。

### 6.4 A4. 节点级拓扑可行性预判

**[`pkg/device/types.go`](../pkg/device/types.go)** `NodeInfo` 新增缓存字段:

```go
type NodeInfo struct {
    ...
    maxLinkComponentSize int // GPU link 拓扑下的最大连通分量大小
    maxNUMAGroupSize     int // 同一 NUMA 节点下最大 GPU 数
}
```

构造时一次性算好:

```go
func NewNodeInfo(node *corev1.Node, pods []*corev1.Pod, excludedPods ...types.UID) (*NodeInfo, error) {
    ...
    ret.maxLinkComponentSize = computeMaxLinkComponentSize(gatherInfo.DeviceList)
    ret.maxNUMAGroupSize = computeMaxNUMAGroupSize(gatherInfo.DeviceMap)
    ...
}

// computeMaxLinkComponentSize 并查集 O(V + E) 求最大连通分量
func computeMaxLinkComponentSize(devices gpuallocator.DeviceList) int {
    if len(devices) == 0 {
        return 0
    }
    uf := newUnionFind(len(devices))
    for i, d := range devices {
        for j := range d.Links {
            if len(d.Links[j]) > 0 {
                uf.Union(i, j)
            }
        }
    }
    return uf.MaxComponentSize()
}
```

**[`pkg/device/allocator/priority.go`](../pkg/device/allocator/priority.go)** 替换原 ByNodeGPUTopology:

```go
// ByNodeGPUTopologyFitness ranks nodes by their ability to satisfy the
// requested link topology size.
//   fitness 2 = has topology AND max connected component >= needNumber
//   fitness 1 = has topology BUT cannot fully satisfy
//   fitness 0 = no topology info
func ByNodeGPUTopologyFitness(needNumber int) LessFunc[*device.NodeInfo] {
    return func(p1, p2 *device.NodeInfo) bool {
        return topologyFitness(p1, needNumber) > topologyFitness(p2, needNumber)
    }
}

func topologyFitness(n *device.NodeInfo, needNumber int) int {
    if !n.HasGPUTopology() {
        return 0
    }
    if n.MaxLinkComponentSize() >= needNumber {
        return 2
    }
    return 1
}
```

类似的 `ByNodeNUMATopologyFitness(needNumber)`。

**`ApplyTopologyMode` 签名调整**接收 needNumber:

```go
func ApplyTopologyMode(mode util.TopologyMode, needNumber int,
    less []LessFunc[*device.NodeInfo]) []LessFunc[*device.NodeInfo] {
    switch mode {
    case util.LinkTopology, util.LinkTopologyStrict:
        less = slices.Insert(less, 0, ByNodeGPUTopologyFitness(needNumber))
    case util.NUMATopology, util.NUMATopologyStrict:
        less = slices.Insert(less, 0, ByNodeNUMATopologyFitness(needNumber))
    }
    return less
}
```

调用方([`filter_predicate.go`](../pkg/scheduler/filter/filter_predicate.go) `deviceFilter`)需要计算 needNumber 总和后传入。

---

## 7. Phase B:评分模型重构

### 7.1 B1. RequestProfile 抽象

**新增 [`pkg/device/allocator/profile.go`](../pkg/device/allocator/profile.go)**:

```go
// RequestProfile is the normalized resource demand profile of a pod, used
// to weight the dimensions (number / memory / core) of binpack/spread scoring
// so the score reflects what the pod actually needs.
//
// Weights sum to 1.0. Dimensions the pod doesn't request get weight 0,
// effectively removing them from the score calculation. If the pod requests
// nothing (theoretically impossible for a vGPU pod but defensively handled),
// weights fall back to uniform 1/3 each.
type RequestProfile struct {
    NumWeight  float64
    MemWeight  float64
    CoreWeight float64
}

func NewRequestProfile(pod *corev1.Pod, node *corev1.Node) RequestProfile {
    var totalNum, totalCore, totalMem int64
    for i := range pod.Spec.Containers {
        c := &pod.Spec.Containers[i]
        if !util.IsVGPURequiredContainer(c) {
            continue
        }
        totalNum += util.GetResourceOfContainer(c, util.VGPUNumberResourceName)
        totalCore += util.GetResourceOfContainer(c, util.VGPUCoreResourceName)
        totalMem += util.GetResourceOfContainer(c, util.VGPUMemoryResourceName)
    }

    // 用节点配置的单卡基准做归一化,把不同量纲拉到同尺度
    nodeCfg := parseNodeConfig(node)
    memPerCard := defaultMemoryPerCard(node, nodeCfg)

    rNum := float64(totalNum)
    rMem := float64(totalMem) / memPerCard
    rCore := float64(totalCore) / util.HundredCore
    sum := rNum + rMem + rCore
    if sum == 0 {
        return RequestProfile{1.0 / 3, 1.0 / 3, 1.0 / 3}
    }
    return RequestProfile{rNum / sum, rMem / sum, rCore / sum}
}
```

### 7.2 B2. Score 统一接口

```go
// ResourceUtilization 0..1 normalized usage of each dimension.
type ResourceUtilization struct {
    Num, Mem, Core float64
}

func NodeUtilization(info *device.NodeInfo) ResourceUtilization {
    return ResourceUtilization{
        Num:  safeDiv(float64(info.GetUsedNumber()), float64(info.GetTotalNumber())),
        Mem:  safeDiv(float64(info.GetUsedMemory()), float64(info.GetTotalMemory())),
        Core: safeDiv(float64(info.GetUsedCores()), float64(info.GetTotalCores())),
    }
}

func DeviceUtilization(d *device.Device) ResourceUtilization { ... }
func NumaUtilization(devices []*device.Device) ResourceUtilization { ... }

// Score 是所有评分的统一入口:
//   Binpack: 加权使用率 (高 = 用得满 = 优先)
//   Spread:  加权空闲率 (高 = 空 = 优先)
func Score(u ResourceUtilization, p RequestProfile, mode util.SchedulerPolicy) float64 {
    switch mode {
    case util.BinpackPolicy:
        return p.NumWeight*u.Num + p.MemWeight*u.Mem + p.CoreWeight*u.Core
    case util.SpreadPolicy:
        return p.NumWeight*(1-u.Num) + p.MemWeight*(1-u.Mem) + p.CoreWeight*(1-u.Core)
    }
    return 0
}
```

原 `GetBinpackNodeScore` / `GetSpreadNodeScore` / `GetDeviceScore` / `GetNumaNodeScore` 全部废弃,改为这套接口。

### 7.3 B3. 评分缓存改造

`NewWeightedBinpackPriority` 工厂在闭包内捕获 profile,缓存仍以 nodeName 为 key(profile 在单次 filter 调用内不变):

```go
func NewWeightedBinpackNodePriority(profile RequestProfile) LessFunc[*device.NodeInfo] {
    cache := make(map[string]float64)
    return func(p1, p2 *device.NodeInfo) bool {
        s1 := cachedScore(cache, p1, profile, util.BinpackPolicy)
        s2 := cachedScore(cache, p2, profile, util.BinpackPolicy)
        return s1 > s2
    }
}

func cachedScore(cache map[string]float64, n *device.NodeInfo,
    p RequestProfile, mode util.SchedulerPolicy,
) float64 {
    if s, ok := cache[n.GetName()]; ok {
        return s
    }
    s := Score(NodeUtilization(n), p, mode)
    cache[n.GetName()] = s
    return s
}
```

### 7.4 调用方改造

`deviceFilter` 在选 LessFunc 时构造 profile:

```go
profile := allocator.NewRequestProfile(pod, /* 用第一个 candidate node 算 memPerCard */)

switch policy := strings.ToLower(nodePolicy); policy {
case string(util.BinpackPolicy):
    allocator.NewNodeBinpackPriority(profile, PodUsedGPUTopologyMode(pod), totalNeed)
case string(util.SpreadPolicy):
    allocator.NewNodeSpreadPriority(profile, PodUsedGPUTopologyMode(pod), totalNeed)
}
```

### 7.5 行为差异对照

| 场景 | 旧评分 | 新评分(加权) | 说明 |
|---|---|---|---|
| pod 只请求 1 vGPU,无 memory/cores | 三维平均 | 仅 num 维度参与 | 排序结果可能不同 |
| pod 请求 1 vGPU + 8GB memory | 三维平均 | memory 维度权重远大于 num | 倾向选 memory 高利用率节点 |
| pod 请求 4 vGPU + 100% cores | 三维平均 | core 与 num 维度均衡 | 排序更准确 |

---

## 8. Phase C:Allocator pod 级化

### 8.1 C1. AllocationRequest 抽象

**新增 [`pkg/device/allocator/request.go`](../pkg/device/allocator/request.go)**:

```go
// AllocationRequest captures every piece of information the allocator needs
// to make a decision for a pod, parsed once from annotations and containers.
// This eliminates scattered util.HasAnnotation(pod, ...) calls and makes
// the allocator amenable to pod-level (vs container-level) decisions.
type AllocationRequest struct {
    Pod *corev1.Pod

    // Per-container needs
    Containers []ContainerNeed

    // Aggregated across containers, for pod-level topology decisions
    Total ContainerNeed

    // Scheduling
    NodePolicy   util.SchedulerPolicy
    DevicePolicy util.SchedulerPolicy

    // Topology
    Topology       util.TopologyMode
    TopologyStrict bool

    // Filters
    GPUType TypeFilter
    GPUUUID UUIDFilter

    // For scoring
    Profile RequestProfile
}

type ContainerNeed struct {
    ContainerName string
    Number        int
    Cores         int64
    Memory        int64
}

func BuildAllocationRequest(pod *corev1.Pod, node *corev1.Node) *AllocationRequest {
    req := &AllocationRequest{Pod: pod}

    for i := range pod.Spec.Containers {
        c := &pod.Spec.Containers[i]
        if !util.IsVGPURequiredContainer(c) {
            continue
        }
        need := ContainerNeed{
            ContainerName: c.Name,
            Number:        int(util.GetResourceOfContainer(c, util.VGPUNumberResourceName)),
            Cores:         util.GetResourceOfContainer(c, util.VGPUCoreResourceName),
            Memory:        util.GetResourceOfContainer(c, util.VGPUMemoryResourceName),
        }
        req.Containers = append(req.Containers, need)
        req.Total.Number += need.Number
        req.Total.Cores += need.Cores
        req.Total.Memory += need.Memory
    }
    req.NodePolicy = parseNodePolicy(pod)
    req.DevicePolicy = parseDevicePolicy(pod)
    req.Topology, req.TopologyStrict = parseTopologyMode(pod)
    req.GPUType = parseTypeFilter(pod)
    req.GPUUUID = parseUUIDFilter(pod)
    req.Profile = NewRequestProfile(pod, node)
    return req
}
```

所有 allocator 方法签名改为接受 `*AllocationRequest`。原来 `allocateOne(pod, container)` 这种"逐容器从 pod 反复抽 annotation"的写法消失。

### 8.2 C2. Pod 级拓扑分配

```go
func (alloc *allocator) Allocate(req *AllocationRequest) (*corev1.Pod, error) {
    needPodLevel := req.Topology != util.NoneTopology &&
        len(req.Containers) > 1 &&
        req.Total.Number > 1

    if !needPodLevel {
        return alloc.allocatePerContainer(req) // 老路径,保持兼容
    }
    return alloc.allocatePodLevel(req)
}

func (alloc *allocator) allocatePodLevel(req *AllocationRequest) (*corev1.Pod, error) {
    // Step 1: 节点上能用的设备(应用 type / uuid / 健康度过滤)
    deviceStore, _ := alloc.filterDevicesForPod(req)
    if len(deviceStore) < req.Total.Number {
        return nil, errors.New("insufficient GPU devices for pod-level allocation")
    }

    // Step 2: 一次性按 Total.Number 做拓扑分配
    podDevices, err := alloc.selectByTopology(req, deviceStore, req.Total.Number)
    if err != nil {
        return nil, err
    }

    // Step 3: 按容器需求把 podDevices 切片分给各容器
    return alloc.distributeToContainers(req, podDevices)
}
```

### 8.3 C3. 容器级分配派生

```go
// distributeToContainers slices the pod-level selected devices to each
// container in order. The current model is "no GPU sharing across containers
// within a pod" (each container gets its own dedicated GPU slots), so we
// simply consume from the head of the slice per container.
//
// Future:if a "share GPUs across containers" mode is introduced, this is
// the place to implement the per-card splitting logic.
func (alloc *allocator) distributeToContainers(
    req *AllocationRequest, podDevices []*device.Device,
) (*corev1.Pod, error) {
    if len(podDevices) != req.Total.Number {
        return nil, fmt.Errorf("internal error: pod-level allocation returned %d devices, expected %d",
            len(podDevices), req.Total.Number)
    }
    newPod := req.Pod.DeepCopy()
    var pdc device.PodDeviceClaim
    cursor := 0
    for _, c := range req.Containers {
        slice := podDevices[cursor : cursor+c.Number]
        cursor += c.Number

        claims := make([]device.DeviceClaim, len(slice))
        for i, d := range slice {
            mem := c.Memory
            if mem == 0 {
                mem = d.GetTotalMemory()
            }
            claims[i] = device.DeviceClaim{
                Id: d.GetID(), Uuid: d.GetUUID(),
                Cores: max(c.Cores, util.HundredCore), Memory: mem,
            }
        }
        pdc = append(pdc, device.ContainerDeviceClaim{
            Name:         c.ContainerName,
            DeviceClaims: claims,
        })
        // 更新 NodeInfo 已用资源
        if err := alloc.addAllocateOne(&pdc[len(pdc)-1]); err != nil {
            return nil, err
        }
    }
    preAlloc, err := pdc.MarshalText()
    if err != nil {
        return nil, err
    }
    util.InsertAnnotation(newPod, util.PodVGPUPreAllocAnnotation, preAlloc)
    util.InsertAnnotation(newPod, util.PodPredicateNodeAnnotation, alloc.nodeInfo.GetName())
    return newPod, nil
}
```

**示例收益**:训练 pod 有 worker 容器(4 卡)+ 通信 sidecar(2 卡),pod 级拓扑分配会确保 6 张卡都在同一个 NVLink 域,worker 与 sidecar 之间的 GPU 通信也享受拓扑收益。

---

## 9. 向后兼容矩阵

| 用户当前配置 | Phase A 后 | Phase B 后 | Phase C 后 |
|---|---|---|---|
| 不写任何调度 annotation | 不变 | 不变 | 不变 |
| `topology-mode: numa` | 不变(best-effort) | 不变 | 不变 |
| `topology-mode: numa-strict` | **新支持** | 同 A | 同 B |
| `node-scheduler-policy: binpack` | 不变(三维平均) | **改加权评分** | 同 B |
| `topology-mode: link` + `device-scheduler-policy: binpack` | **组合首次生效** | 评分改加权 | 同 B |
| 多容器 pod + topology | 容器独立(同前) | 同 A | **集体拓扑优化** |

所有变更都是 strict 添加新行为或精度提升,**不破坏现有部署**。Phase B 评分模型变化只在已显式配 binpack/spread 时影响节点排序结果,属于"按用户意图更精确",一般是改善。

---

## 10. 测试策略

### 10.1 Phase A

- **A1 strict 模式**:table-driven 单测覆盖 `numa-strict` / `link-strict` / 普通 numa/link 的失败/成功路径;e2e 测试 strict 不满足时 pod Pending + Event 内容
- **A2 bestEffort 性能**:benchmark 8/12/16/24 卡场景的 bestEffort vs greedy 延迟对比,确认 16+ 卡降级触发
- **A3 Link + 策略组合**:构造链路等效的多 set 场景,验证 binpack/spread 在 top K 内做出预期选择
- **A4 节点拓扑预判**:构造连通分量小于需求量的节点,验证排序把它降级到无拓扑节点之后

### 10.2 Phase B

- **B1 加权评分**:对照测——同样节点集合 + 不同 pod 请求,验证加权后排序差异符合"高权重维度高利用率节点排前"
- **B2/B3 重构**:全量回归现有 binpack/spread 测试,保证基础语义不变

### 10.3 Phase C

- **C1 AllocationRequest**:单测覆盖各种 annotation 组合下的 parse 正确性
- **C2 pod 级拓扑**:e2e 测试多容器 pod 与单容器 pod 的拓扑分配差异
- **C3 distribute**:单测验证按容器需求量切片正确,各容器 cores/memory 独立设置

---

## 11. 风险与工作量评估

| Phase | 工作量 | 风险 | 价值 |
|---|---|---|---|
| A | 1-1.5 周 | 低 (纯增量) | ★★★★ 生产可见的稳定性与性能提升 |
| B | 1-2 周 | 中 (改评分会改节点排序结果) | ★★★ binpack/spread 真正生效 |
| C | 2-3 周 | 高 (重大重构) | ★★ 覆盖多容器场景,影响面取决于客户用例 |

**建议节奏**:

- Phase A 立即做(收益大、风险低,可以一个 PR 全部完成或分 4 个 PR)
- Phase B 跟在 A 后面,需要更充分的回归测试
- Phase C 看用户场景——如果多容器训练任务多,值得;否则可暂缓

---

## 12. Open Questions / Future Work

### 12.1 Open Questions

1. **`bestEffortMaxGPUs` 阈值设多少合适?** 12 是基于"组合数 < 100 万"的粗估,需要 benchmark 验证真实延迟数据
2. **strict 模式失败是否要触发 Reschedule controller?** 当前 stuck pod 处理逻辑(`pkg/controller/reschedule/`)对 strict 失败的语义可能需要调整
3. **Phase C 中"多容器是否共享同一物理卡"** 当前不共享。如果未来引入共享语义,distribute 逻辑要重写
4. **PriorityClass + strict topology 的契约语义** 高优先级 pod 在 strict 失败时是否触发抢占?当前抢占逻辑不考虑拓扑

### 12.2 Future Work

- **GPU 代际感知**:annotation `nvidia.com/include-gpu-generation: H100` 类的"按代际过滤",作为 binpack/spread 的二级排序维度
- **碎片化感知 binpack**:不仅看利用率,还看碎片度(small free chunks vs large free chunk),减少 vGPU 碎片
- **跨节点拓扑(InfiniBand / NCCL 优化)**:本设计仅涉及单节点拓扑,跨节点高性能通信是更大工程
- **评分 plugin 化**:把 Score 函数抽象成插件接口,允许用户自定义评分公式(类似 K8s scheduler framework score plugin)

---

## 13. 关键文件改动清单

为方便实施时定位,以下是预期的文件改动汇总。

### Phase A
- `pkg/util/consts.go`:新增 `NUMATopologyStrict` / `LinkTopologyStrict`
- `pkg/device/allocator/allocator.go`:`allocateByTopologyMode` 签名 + strict 处理 + Link/NUMA 函数拆分
- `pkg/device/gpuallocator/besteffort_policy.go`:新增 `AllocateTopK`
- `pkg/device/gpuallocator/greedy_policy.go`:**新增**,greedy 算法
- `pkg/device/gpuallocator/allocator.go`:`AllocateWithLimit` + 阈值判断
- `pkg/device/types.go`:`NodeInfo` 新增 `maxLinkComponentSize` / `maxNUMAGroupSize` 字段与构造
- `pkg/device/allocator/priority.go`:`ByNodeGPUTopologyFitness` / `ByNodeNUMATopologyFitness` 替换原 LessFunc;`ApplyTopologyMode` 接受 needNumber
- `pkg/scheduler/filter/filter_predicate.go`:调用 `ApplyTopologyMode` 传 needNumber
- `cmd/device-scheduler/options/options.go`:新增 `--best-effort-max-gpus` flag

### Phase B
- `pkg/device/allocator/profile.go`:**新增**,`RequestProfile` 与 `NewRequestProfile`
- `pkg/device/allocator/score.go`:**新增**,`Score` / `Utilization` 统一接口
- `pkg/device/allocator/priority.go`:`NewNodeBinpackPriority` / `NewNodeSpreadPriority` 接收 `RequestProfile`;原 `GetBinpackNodeScore` / `GetSpreadNodeScore` 改为内部调 `Score`
- `pkg/device/allocator/numa.go`:`GetNumaNodeScore` / `GetDeviceScore` 改用统一 `Score`
- `pkg/scheduler/filter/filter_predicate.go`:`deviceFilter` 构造 profile 并下发

### Phase C
- `pkg/device/allocator/request.go`:**新增**,`AllocationRequest` 与解析函数
- `pkg/device/allocator/allocator.go`:`Allocate` 重构成 pod 级入口,拆分 `allocatePerContainer` / `allocatePodLevel`
- 调用方([`filter_predicate.go`](../pkg/scheduler/filter/filter_predicate.go) 等)切到 `AllocationRequest` 入参
