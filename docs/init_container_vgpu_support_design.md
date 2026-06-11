# Init 容器 vGPU 设备调度与分配支持 — 设计文档

> 状态:设计稿(改造已在 webhook 层起步,commit `350b971`)
> 目标:让 scheduler / deviceplugin / metrics 三个仍只识别业务容器的模块,正确识别并分配
> init 容器的 vGPU 资源,且**按 K8s 原生语义对 init 与业务容器取"峰值"而非简单累加**。

---

## 1. 背景与问题

vGPU-manager 通过 scheduler extender(Filter/Bind/Preempt)+ device-plugin + webhook + metrics
四件套,为 Pod 调度并分配 vGPU(`<domain>/vgpu-number` / `vgpu-cores` / `vgpu-memory` 三类扩展资源)。

当前除 webhook 外,整条分配链路**只遍历 `pod.Spec.Containers`,完全忽略 `pod.Spec.InitContainers`**。
若 init 容器声明了 vGPU 资源:调度器不会为其分配设备、device-plugin 找不到对应容器、metrics 也不统计。

直接的"把 init 容器也加进去累加"是**错误的**:init 容器与业务容器**生命周期不重叠**
(普通 init 容器顺序运行、全部结束后业务容器才启动),累加会把同一物理 GPU 的资源重复占用,
导致虚假的容量不足、密度骤降。K8s 原生调度器对此的处理是**取最大值**而非求和。

本项目的难点在于:vGPU 不是一个标量资源,而是**必须落到某张具体物理 GPU 上、按显存/算力切片**的
异构资源。因此"取 max"不是对一个数字取 max,而是要**对每一张物理 GPU 计算其生命周期内的并发占用峰值**。
这正是本设计要解决的核心。

---

## 2. K8s 原生 init 容器资源核算语义(精确)

K8s `PodRequests`(`k8s.io/component-helpers` / `pkg/api/v1/resource`)对每一种资源 R 的有效请求:

```
effective(R) = max(
    Σ over 普通业务容器 request(R)            // 业务容器并发运行 → 求和
        + Σ over 可重启 init 容器(sidecar) request(R),   // sidecar 与业务容器同时在跑 → 计入求和
    max over 每个普通 init 容器 ( request(R)
        + Σ 在它之前启动的 sidecar request(R) )            // 每个普通 init 容器独占运行,但其前序 sidecar 仍在跑
)
```

要点:
- **普通业务容器之间**:并发 → **求和(sum)**。
- **普通 init 容器(`restartPolicy` 非 `Always`)之间**:顺序、互不重叠 → **取最大(max)**。
- **普通 init 容器 与 业务容器之间**:不重叠 → **两组各自聚合后再取 max**。
- **sidecar(可重启 init 容器,`restartPolicy: Always`)**:从启动后持续运行,与业务容器**并发** → 计入"求和组"。

本设计落地时:**普通 init 容器精确按 max 处理;sidecar 归入并发求和组**(语义正确,且实现简单)。

---

## 3. 核心挑战:从"标量 max"到"每物理 GPU 的时间相位峰值"

K8s 的 max 作用在标量资源上。vGPU 必须落到具体物理卡上,所以我们需要把 Pod 的生命周期看成一串
**时间相位快照(phase snapshot)**:

```
相位 init-1 :  仅 initContainer[0] 运行
相位 init-2 :  仅 initContainer[1] 运行
   ...
相位 app    :  全部业务容器 + 全部 sidecar 并发运行
```

约束:**在每一个相位,任何一张物理 GPU 的并发占用都不得超过其容量。**

Pod 对某张物理 GPU `g` 的**预留量(footprint)** = 该 GPU 在所有相位中的占用峰值:

```
reserve(g) = max(
    Σ_{c ∈ 并发组(业务+ sidecar), 且 c 用到 g}  claim(c, g),   // app 相位:并发 → 求和
    max_{c ∈ 普通 init, 且 c 用到 g}            claim(c, g)    // 各 init 相位:取最大
)
```

其中 `claim(c, g)` 是容器 c 落在 GPU g 上的 `(cores, memory)` 切片(及是否占 1 个 number slot)。

**关键洞察 —— 设备复用(device reuse):** 由于 init 容器与业务容器永不同时运行,init 容器**可以复用
业务容器将要使用的同一张物理 GPU 的资源**(init 跑完释放、业务再启动)。因此理想分配是让 init 容器
尽量落到 Pod 业务容器已选中的那几张 GPU 上 —— 只要 init 在某卡上的需求 ≤ 业务相位在该卡的占用,
init 就**零额外成本**,完全匹配 K8s "取 max" 的密度收益。仅当 init 在某卡(或某张额外卡)上的需求
**超过**业务相位时,超出的增量 `max(0, init - app)` 才消耗节点额外容量,并把 `reserve(g)` 抬高到 init 的需求。

> 这条公式是整个改造的"单一真相":调度器分配算法产出的 footprint、节点占用核算、metrics 统计
> **三者必须用同一公式**,否则会出现核算漂移(过量分配或资源饿死)。见 §6.4 的共享 helper 与不变量。

---

## 4. 现状代码盘点:所有"遍历容器 / 累加资源"的落点

| # | 位置 | 现状 | 问题 |
|---|------|------|------|
| A1 | `pkg/device/allocator/request.go:124` `BuildAllocationRequest` | `for i := range pod.Spec.Containers`,`req.Total += sum`,`req.Max = max` | **源头**:init 容器根本不进入 `req.Containers`;`Total` 纯求和;`Max` 不含 init |
| A2 | `pkg/device/allocator/allocator.go:64` `Allocate` | 按 `req.Containers` 顺序逐容器分配,`addContainerAllocate` 跨容器累加占用 | 累加模型对并发业务容器正确,但对 init 会叠加(应复用) |
| A3 | `pkg/device/allocator/profile.go:73` `NewRequestProfile` | 打分权重只遍历 app 容器并累加 | 权重漏算 init |
| B1 | `pkg/device/types.go:1042` `AddPodUsedResources` | 双重 range,把 annotation 里**每个**容器的每个 claim 都累加到物理卡 | 一旦 init 进入 annotation → **节点核算双重计数** |
| B2 | `pkg/device/types.go:307` `GetCurrentPreAllocateContainerDevice` / `checkExistCont:317` | 只在 `pod.Spec.Containers` 里按容器名校验存在 | init 容器名 → "container does not exist" 报错 |
| C1 | `pkg/deviceplugin/vgpu/vnum_plugin.go:608` `Allocate` 容器循环 | 按 PreAlloc 游标逐容器消费、注入 env/挂载 | 不含 init → init 容器拿不到设备与拦截库 |
| C2 | `pkg/deviceplugin/vgpu/vnum_plugin.go:735` `PodContainerEnvEnabled` 等 | 只看 app 容器 | init-only Pod 判定失败 |
| D1 | `pkg/util/util.go:154` `GetResourceOfPod` / `:165` `IsVGPUResourcePod` / `:338` `IsSingleContainerMultiGPUs` / `:457` `PodContainerEnvEnabled` | 只遍历 app 容器 | **init-only Pod** 被判为非 vGPU Pod;`FilterAllocatingPods` 在 device-plugin 侧找不到当前 Pod |
| E1 | `pkg/metrics/collector/node_gpu.go:508` `FlattenDevicesEach` 累加段 | 展开 annotation 所有容器 claim 累加 | **改造后双重计数**(number/cores/memory 翻倍) |
| E2 | `pkg/metrics/collector/node_gpu.go:523` 实时统计 `range pod.Spec.Containers` | 只遍历 app | init 容器实时用量漏统计(多数情况可接受,init 已退出) |
| E3 | `pkg/metrics/lister/container_lister.go:115` `collectContainerKey` | 只收 app 容器目录键 | init 容器 `devices.json` 目录被当孤儿清理 |

**已有可复用的"正确工具"与参考实现:**
- `pkg/util/types.go:18` `GetAllPodContainers(pod)`:已统一返回 init(`ContainerKindInit`)+ app(`ContainerKindApp`),按 **init 在前、app 在后** 排序。
- `pkg/webhook/pod/validate/pod_validate.go:288-348`:DRA 路径已实现 init↔app 共享/不重叠的校验矩阵
  (init-init 禁止共享、app-app 禁止共享、init-app 允许共享且一个 init 至多与一个 app 重叠)——
  这正是"取 max / 设备复用"的业务规则来源。
- `pkg/claimresolve/partitions.go:114`:DRA 路径已用 `GetAllPodContainers` 把 init+app 按"共享 request 连通分量"分区,是设备复用的实现范本。
- commit `350b971`:webhook(mutate 默认值填充、DRA 转换、validate 校验)已同时处理 init+app 容器。

---

## 5. 改造总体方案

原则:**保持 annotation 的"每容器 claim"编码不变**(device-plugin 仍按容器逐个注入,init 与 app 条目可以
引用同一组物理 GPU UUID);把"init 与 app 取峰值"的语义集中到**两处**:
(1) 调度分配算法(两遍法:并发组求和 + 独占 init 复用放置);
(2) 一个**共享的 footprint reduce helper**,供节点核算与 metrics 统一调用。

```
            ┌─ scheduler (filter/preempt) ─┐
 Pod spec ─▶│ BuildAllocationRequest (含init, Total=有效峰值, Max含init)
            │ Allocator (两遍: app 求和累加 → init 复用放置, 记录每容器 claim)
            └─ 写 PreAlloc annotation: cont(init/app)[uuid_..]; ... ──┐
                                                                      ▼
 device-plugin: 按 GetAllPodContainers 顺序消费 PreAlloc 游标,逐容器注入 env/挂载/拦截库
                                                                      │
 节点占用核算 AddPodUsedResources ─┐                                  │
 metrics FlattenDevices           ├─▶ ReducePodFootprint(pod, claim) ◀┘  (§3 峰值公式, 单一真相)
```

---

## 6. 分模块改造设计

### 6.1 统一容器遍历入口(request.go)

把 `BuildAllocationRequest` 的循环从 `pod.Spec.Containers` 改为 `util.GetAllPodContainers(pod)`,
为每个 `ContainerNeed` 增加 `Kind`(init / app)与 `Restartable bool`(sidecar 标记,读
`InitContainers[i].RestartPolicy == Always`)。`req.Containers` 顺序保持 **init 在前、app 在后**
(与 device-plugin 游标、kubelet 调用顺序一致)。

```go
type ContainerNeed struct {
    Name        string
    Kind        util.ContainerKind // init / app
    Restartable bool               // sidecar: 计入并发求和组
    Number      int
    Cores       int64
    Memory      int64
}
```

### 6.2 聚合量语义:`req.Total` 改为"有效峰值",`req.Max` 纳入 init

`req.Total`(节点级容量必要条件门控,见 `filter_predicate.go:529-558`)按 §2 公式逐维度计算有效峰值:

```
并发组 = 业务容器 ∪ sidecar
Total.X = max( Σ_{并发组} X ,  max_{普通 init} X )      // X ∈ {Number, Cores(×number), Memory(×number)}
```

`req.Max`(单设备结构门控)对 **所有** 容器(含 init)逐字段取 max —— 仅需把 init 容器纳入现有
`max(...)` 比较即可。这两者仍是**必要条件**(只会放宽不会误拒),allocator 会精确复核。

> 注意:`Total` 三个维度各自独立取 max,不要把"哪个容器是峰值"耦合起来 —— number 维度的峰值容器
> 与 memory 维度的峰值容器可以不同,这与 K8s 的逐资源 max 一致。

### 6.3 分配算法:两遍法(allocator.go)

`Allocate` 改为两遍,核心是利用"NodeInfo 构造时已排除当前 Pod"(`WithExcludedPods(pod.UID)`),
即 NodeInfo 初始 used = **其他 Pod 的占用(base)**,本 Pod 的预留在分配过程中逐步累积。

**Pass 1 — 并发组(业务 + sidecar):** 完全沿用现有逐容器 `addContainerAllocate` 累加逻辑。
产出每个业务/sidecar 容器的 `ContainerDeviceClaim`,以及本 Pod 的 **app footprint**(每 UUID 的求和占用)。

**Pass 2 — 普通 init 容器(逐个,顺序):** 每个 init 容器独占运行,其在 GPU g 上的可用量 =
`total(g) - base(g) - Σ sidecar(g)`(**不减去 app footprint**,因为 init 运行时业务容器尚未启动,
可复用其预留)。放置时**优先选择本 Pod 业务容器已占用的 GPU**(传入 preference set,复用 → 零额外成本);
放置后按 §3 公式更新 Pod 预留:`podPeak(g) = max(podPeak(g), 本 init 在 g 的占用)`,
节点 committed(g) = `base(g) + podPeak(g)`。

实现要点:
- Pass 2 在计算 init 可用量时,需要把"本 Pod 的 app footprint 视为可复用空闲"。最直接的做法是在
  Pass 2 内部对每个 init 容器:临时把 NodeInfo.used 重置为 `base + sidecar`(可复用空闲 = 释放本 Pod app 预留),
  分配该 init,再把 used 恢复为 `base + 取 max 后的 podPeak`。`pkg/device/types.go` 已有
  `ResetResourceUsage` / `AddPodsUsedResources`(preempt 路径在用),可复用同类原语构造这个临时视图。
- 设备选择器 `filterDevices`/`pickDeviceClaims`(allocator.go:586/203)增加一个**偏好集合**参数:
  优先命中 Pod 已选 GPU;命中即复用,未命中再扩到节点空闲卡(此时 footprint 增长,属正确的峰值上升)。
- init-only Pod(并发组为空):Pass 1 跳过,footprint 完全由 Pass 2 定义。

产出的 `PodDeviceClaim` 仍是"每容器一条"(init 条目在前),init 与 app 条目**可以引用相同 UUID**。
写回 annotation 的格式不变(`cont[id_uuid_cores_mem,...];...`,`types.go:247-287`)。

### 6.4 节点占用核算:共享 reduce helper(单一真相)

新增 helper,**节点核算与 metrics 共用**,把"每容器 claim"折叠成"每物理 GPU 峰值":

```go
// ReducePodFootprint 按 init/app 生命周期把逐容器 claim 折叠为每物理 GPU 的占用峰值:
// 并发组(业务 + sidecar)在同一 UUID 上求和;普通 init 组在同一 UUID 上取最大;两组再取最大。
// 返回 map[uuid] -> 聚合后的 (number, cores, memory)。
func ReducePodFootprint(pod *corev1.Pod, claim device.PodDeviceClaim) map[string]device.DeviceClaim
```

- 容器分类:用 `pod.Spec.InitContainers`(及其 `RestartPolicy`)判断每个 `ContainerDeviceClaim.Name`
  属于 并发组 / 普通 init 组(找不到的容器名按 app 处理或告警)。
- `AddPodUsedResources`(`types.go:1027-1049`)改为:先 `ReducePodFootprint`,再对结果按 UUID 累加到物理卡
  —— **不再** 直接双重 range 累加每个容器 claim。

**关键不变量(必须有单测守护):**
> allocator 两遍法产出的 footprint(`base` 之上 Pod 的 `podPeak`)
> == `ReducePodFootprint(pod, 该 Pod 写入 annotation 的 PodDeviceClaim)`。
> 二者不一致即意味着调度核算与运行时核算漂移,会造成过量分配或资源饿死。

### 6.5 device-plugin(vnum_plugin.go)

- **游标存在性校验** `checkExistCont`(`types.go:317`):改用 `util.GetAllPodContainers(pod)`
  (或同时查 `InitContainers`),接受 init 容器名。
- **Allocate 容器循环**(`vnum_plugin.go:608`):无需大改 —— kubelet 会**为每个**申请了 vGPU 的容器
  (含 init,且先 init 后 app)分别调用 Allocate;现有"消费 PreAlloc 里第一个尚未进入 RealAlloc 的容器"
  游标模型,只要 PreAlloc 顺序为 init-first(§6.3 已保证)就**天然对齐**。需确认 init 与 app 注入路径一致
  (env `NVIDIA_VISIBLE_DEVICES`、`CUDA_DEVICE_MEMORY_LIMIT_*`、`CUDA_DEVICE_SM_LIMIT_*`、HAMi-core
  `libvgpu-control.so` / `ld.so.preload` 挂载,见 `vnum_plugin.go:635-742`),init 容器同样需要这些注入。
- `PreStartContainer` / `getRealContainerDeviceClaim`(`vnum_plugin.go:882`)按容器名查 RealAlloc,
  **对 init 天然可用**,只要 RealAlloc 含该 init 条目。
- `pkg/client/pod_resources.go` 平铺遍历 kubelet pod-resources 上报的容器名、不读 spec,**对 init 天然友好**,
  无需改动。

### 6.6 Pod 级判定(util.go)

`IsVGPUResourcePod`(`util.go:165`)、`GetResourceOfPod`(`:154`)、`IsSingleContainerMultiGPUs`(`:338`)、
`PodContainerEnvEnabled`(`:457`)改用 `GetAllPodContainers` 遍历(`IsVGPUResourcePod` 只需"任一容器申请即为真",
不涉及 sum/max 取舍)。否则 **init-only Pod** 会被 `FilterAllocatingPods` 判为非 vGPU Pod,device-plugin
`Allocate` 阶段 `GetCurrentPodByAllocatingPods` 找不到当前 Pod。

### 6.7 metrics(node_gpu.go / container_lister.go)

- `FlattenDevicesEach` 累加段(`node_gpu.go:508`)改为先 `ReducePodFootprint` 再按 UUID 统计,
  消除 init/app 双重计数;`sharedContainersMap`(共享容器计数)同样基于 reduce 后的结果。
- `collectContainerKey`(`container_lister.go:115`)纳入 init 容器名,避免 init 的
  `<podUID>_<initName>` 目录被 1 分钟孤儿清理逻辑误删、其 `vgpu.config` 不被加载。
- 实时用量遍历(`node_gpu.go:523`)可选纳入 init 容器(多数 init 已退出,优先级低;若支持长跑 sidecar 则需要)。

---

## 7. 设计决策与取舍

| 决策点 | 选项 | 建议 |
|--------|------|------|
| init 与 app 资源核算 | (a) 仍累加(安全但密度差,违背需求) / (b) **取峰值复用** | **(b)** —— 需求明确要求,且匹配 K8s 语义 |
| 复用实现层次 | (a) 仅在核算层去重(allocator 仍按 sum 试分配,过严) / (b) **allocator 两遍法 + 核算层 reduce** | **(b)** —— 仅核算层去重无法获得调度密度收益(filter 仍按 sum 卡容量) |
| annotation 编码 | (a) 保持每容器 claim,init/app 引用同 UUID / (b) 新增"footprint" annotation | **(a)** —— 编码不变,reduce 在读取时实时计算,device-plugin 注入路径零改动 |
| sidecar(可重启 init) | (a) 精确按"前序 sidecar 叠加" / (b) **并入并发求和组** | **(b)** —— 语义正确(sidecar 确与业务并发)、实现简单;普通 init 精确取 max |
| 多 init 共享同卡 | 复用 webhook 已有校验矩阵(`pod_validate.go:288-348`) | 沿用:init-init 禁共享、init-app 允许且一对一 |

---

## 8. 边界条件与测试

- **init-only Pod**(仅 init 申请 vGPU):footprint 由 init 组定义;Pod 级判定、metrics、device-plugin 全链路。
- **init 比 app 更大**:init 在某卡需求 > app → footprint 抬升;init 需要比 app 更多 GPU → Pod 卡数增长。
- **sidecar + 普通 init 混合**:sidecar 入并发组求和,普通 init 取 max。
- **多业务容器并发**:维持现有 sum 语义不回归。
- **抢占路径**(`preempt_predicate.go:425` `canAllocate`):`ResetResourceUsage` + `AddPodsUsedResources`
  重算时,被抢占 victim 的 footprint 也走 `ReducePodFootprint`,确保释放量一致。
- **核心不变量单测**:allocator footprint == `ReducePodFootprint(annotation)`(§6.4)。
- **回归**:无 init 容器的 Pod 行为与改造前**逐位一致**(reduce 对 app-only 退化为纯 sum)。

---

## 9. 分阶段落地建议

> **重要不变量(实施前必读)** —— 调度链路依赖一个隐藏不变量:
> `IsVGPUResourcePod(pod)` 的真假必须与 `BuildAllocationRequest(pod).Containers` 是否非空**保持一致**。
> 因为 `filter_predicate.go:141` 仅当 `req.Containers` 非空时才写 `predicate-node` annotation,
> 而 `bind_predicate.go:84` 仅当 `IsVGPUResourcePod` 为真时才**强制校验** `predicate-node`、缺失即拒绝绑定。
> 二者当前都只看业务容器,故一致。**若单独把 `IsVGPUResourcePod` 改为含 init、而 `BuildAllocationRequest`
> 仍只看业务容器,则 init-only Pod 会被判为 vGPU Pod → filter 跳过不写 predicate-node → bind 拒绝绑定(回归)。**
> 因此 `IsVGPUResourcePod` 与 `BuildAllocationRequest` 的容器识别**必须同步修改**(同在 P2),不可拆分。

1. **P0 — 安全无回归的前置铺路(已实施)**:仅做"按唯一容器名查找"的纯增量扩展,在 allocator 尚未产出
   init claim 前**今天恒为 no-op**,为 P2 铺路:
   - `checkExistCont`(`types.go`,6.5):校验 PreAlloc 容器名时同时检索 `InitContainers`。
   - `PodContainerEnvEnabled`(`util.go`,6.6):按名检索 env 开关时同时检索 `InitContainers`。
   - **不在 P0 改** `IsVGPUResourcePod`(受上述不变量约束,移至 P2);**不动** `GetResourceOfPod` /
     `IsSingleContainerMultiGPUs`(当前无调用方的求和语义死代码,真正需要时随 P1/P2 处理)。
2. **P1 — 核算层 reduce(已实施)**:落地 `ReducePodFootprint` + `PodDeviceFootprint`(`pkg/device/types.go`),
   改 `AddPodUsedResources`(加 `podHasVGPUInitContainer` 门控:无 vGPU init 容器走原逐-claim 快路径不变,
   否则走 reduce 路径)与 metrics 累加段(6.4/6.7)。共享 helper 对 app-only 退化为纯求和,行为逐位一致;
   配套 `Test_ReducePodFootprint` 10 例。

   **每物理 GPU 峰值公式(逐维)**:`reserve(g) = sidecarSum(g) + max(regularSum(g), initMax(g))`。
   - `regularSum`/`sidecarSum`:业务容器 / sidecar(可重启 init)的逐卡求和;
   - `initMax`:对每个**顺序 init 容器自身的逐卡求和**再跨容器取 max(顺序 init 互不重叠;"先容器内 sum、
     再容器间 max",不再 per-claim 硬编码 number=1,消除对"单容器多 vGPU 必落不同卡"不变量的隐式依赖);
   - sidecar 全程运行,作为常数加项叠加到两个相位上 —— 这修正了早期"sidecar 仅归并发组"会在
     sidecar 与顺序 init 同卡重叠时**少算**的问题(永不少算)。
   - 保守简化:认为 sidecar 与**每个**顺序 init 相位都重叠(实际只与其之前启动的 sidecar 重叠),
     在"sidecar 声明于 vGPU init 之后"的罕见顺序下会**略微多算**(安全方向)。

3. **P1.5 — sidecar 共享校验修复(已实施)**:webhook 的 vGPU request 共享校验矩阵原先把**所有** init 容器
   按"与 app 不重叠"放行 init↔app 共享,**漏了 sidecar**(可重启 init 与 app **并发**)。新增
   `util.IsRestartableInitContainer` + `ContainerRef.Restartable`;两处校验矩阵
   (`pkg/webhook/pod/validate/pod_validate.go`、`pkg/webhook/resourceclaim/validate/resourceclaim.go`)
   把 sidecar 按 app 类(并发)处理 —— sidecar↔app / sidecar↔sidecar 共享同一 vGPU request 现按 app-app 冲突**拒绝**,
   仅非重启 init↔app 仍允许跨相位复用。配套 webhook 测试 2 例。`GetAllPodContainers` 的 `Kind` 不变
   (sidecar 仍标 `init`),`claimresolve/partitions.go` 等不读 `Restartable` 的消费者不受影响。
4. **P2 — 分配算法两遍法 + 识别同步**:`request.go` 聚合量与容器遍历(6.1/6.2)+ `allocator.go` 两遍复用放置(6.3)
   + **同步**把 `IsVGPUResourcePod`(及必要的 Pod 级判定)改为含 init,保持上述不变量。配套不变量单测。
   这是密度收益的核心,风险最高,最后做。**allocator 的 footprint 必须等于 `ReducePodFootprint(annotation)`**
   (含 §P1 的 sidecar 保守叠加语义),否则核算漂移。
5. **P3 — metrics 实时使用指标 + 生命周期(排在 P2 之后)**:监控分两套指标,生命周期语义相反:
   - **assigned/预留**(`*_assigned_*`、`vgpu_device_shared_containers_number`,源自 annotation→`ReducePodFootprint`):
     已 init-aware 去重(P1),是 pod 全生命周期**静态峰值预留**,**不随容器运行态变化**,无需再改。
   - **used/实时使用**(6 个 `container_vgpu_*` usage/util,源自 libvgpu 共享内存+cgroup PID+NVML):当前
     `node_gpu.go:518` 与 `container_lister.go:115`(`collectContainerKey`)**只遍历 `pod.Spec.Containers`,完全忽略 init**。
   - 改造(两处必须配套,否则"加载了不读 / 要读已被孤儿清理"不一致):
     a. 采集与 keySet 都纳入 init 容器(可用 `util.GetAllPodContainers`)。
     b. **按 `pod.Status.InitContainerStatuses` 状态门控**:顺序 init 仅在 `Running` 时纳入 keySet/上报,
        `Terminated` 即移出→目录回收→usage 自然停止(避免上报已退出 init 容器的陈旧"幽灵使用值");
        sidecar(可重启 init)按 app 处理、全程上报。
     c. **(已实施)** 共享容器数拆为两个指标:`vgpu_device_peak_shared_containers_number`(峰值=
        全生命周期并发共享峰值,源自 `ReducePodFootprint` 的 `fp.Number`,原 `..._shared_containers_number` 重命名而来)
        + `vgpu_device_current_shared_containers_number`(当前=此刻 Running 且持 claim 的容器数,源自
        `device.CurrentSharedContainers`,按 `IsContainerRunning` 门控,顺序 init 退出即不计)。保证 peak ≥ current;
        二者均带 help text。app-only 时 current==peak,init 落地(P2)后才分化。配套 `Test_CurrentSharedContainers`。
     d. help text 注明 assigned≥used 的背离属正常(预留含 init 峰值,app 阶段 used 仅 app)。
   - 依赖:used 侧 init 数据只有 P2(allocator 产出 init claim + deviceplugin 向 init 容器注入拦截库/`vgpu.config`)
     之后才存在,故 P3 排在 P2 之后;在此之前该路径休眠。

> 落地顺序遵循"先让 init 不被忽略且不出错(P0/P1),再追求 K8s 级密度(P2)",每步独立可回归。
