# Cluster Autoscaler extender 接入：Filter dry-run 设计

> 作用范围：`pkg/scheduler/filter/`、`pkg/scheduler/predicate/`、`pkg/route/`、`pkg/scheduler/metrics/`、`cmd/device-scheduler/`。
> 目标：让 vgpu-manager 的 extender Filter 能被 Cluster Autoscaler(CA)扩容仿真**只读地**调用，修复「CA 仿真看不到 extender 过滤 → vGPU 饱和时该扩不扩、pod 卡 Pending」的问题。
> 触发背景：k8s/autoscaler PR #9786（Integrate Extender-Managed Resources）把 HTTP extender 接进 CA 的扩容仿真链路。
> 状态：已实现（独立端点 `/scheduler/filter-dryrun`，默认注册但不影响现有调度路径），单测覆盖，未做真机 CA 端到端验证。

---

## 1. 背景与问题

### 1.1 CA #9786 做了什么

Cluster Autoscaler 决定要不要扩节点时，靠一套**内置调度仿真**判断「pending pod 能不能塞进候选节点 / nodegroup 模板节点」。此前该仿真**从不调用 HTTP scheduler extender**，所以对 vgpu-manager 这类「真正可行性判断在 extender 里」的方案是盲区：CA 只看 Node 的 `Allocatable` 标量聚合，无法感知单卡碎片、显存超卖、NUMA/NVLink 拓扑、MIG 等约束 → 误判节点还能塞 → 该扩不扩。

PR #9786 的做法（均为 **CA 侧**改动）：

- `utils/gpu/gpu.go` 新增 `RegisterGPUResourceNames()`，运行时把 extender 管理的资源名注册成 GPU vendor 资源。
- `core/autoscaler.go` `registerExtenderManagedResources()` 从 `KubeSchedulerConfiguration.Extenders` 读配置并注册。
- `simulator/framework/handle.go` 用 `scheduler.NewHTTPExtender()` 实例化 extender，存进 `Handle.Extenders`。
- `simulator/clustersnapshot/predicate/plugin_runner.go` 新增 `runExtenderFilters()`，在节点过滤时逐个调 extender 的 Filter HTTP 端点；`RunFiltersUntilPassingNode` 改为「先收集所有通过内置插件的节点，再交给 extender 做后置过滤」。

配置层面复用现有 `KubeSchedulerConfiguration.Extenders[].managedResources[].name`，**无新增 CLI flag**。上游已知局限：scale-from-0（冷启动空 nodegroup）尚未解决；extender 如何暴露「资源配比」无标准；该能力尚未进入 CA 正式发布版本。

### 1.2 vgpu-manager 正是这类系统

vgpu-manager 本身就是标准 HTTP scheduler extender：

- [cmd/device-scheduler/main.go](../cmd/device-scheduler/main.go) 起 extender 服务，[pkg/route/routes.go](../pkg/route/routes.go) 暴露 `POST /scheduler/filter`、`/scheduler/bind`、`/scheduler/preempt`。
- 管理三个资源：`nvidia.com/vgpu-number`、`nvidia.com/vgpu-memory`、`nvidia.com/vgpu-cores`（[pkg/util/consts.go](../pkg/util/consts.go)，域名 `globalDomainName` 可配，默认 `nvidia.com`）。
- 可行性判断全在 [pkg/scheduler/filter/filter_predicate.go](../pkg/scheduler/filter/filter_predicate.go)。

### 1.3 核心问题：Filter 有副作用，不能被仿真安全调用

真实调度的 Filter 把两件事耦合在一起：

1. **可行性判断（只读）**：`nodeFilter`（节点级门控：设备注册、config、显存策略）+ `deviceFilter` 前半段（构建 NodeInfo、容量预门控、按策略排序）。
2. **提交（写集群）**：`deviceFilter` 尾部对第一个能放下的节点做乐观预分配 —— `PatchPodPreAllocatedMetadata`（改真实 Pod）、`podLister.Mutation`（改本地缓存）、`recorder.Eventf`（发 Event）。

这是「filter 阶段乐观预分配」模式：把设备分配结果 patch 进真实 Pod，桥接 filter→bind 之间的竞态。

**为什么挡住 CA**：CA 仿真必须只读，而它会对多个候选节点 / 模板节点、跨多个 nodegroup、多轮调用 Filter。一旦调到 vgpu-manager，就会对真实 pending Pod 打预分配注解、污染 podLister 缓存、刷 Event，甚至**过早把 pod 提交到错误节点**。

> 关键事实：`allocator.Allocate` 在 `pod.DeepCopy()` 上运算（[allocator.go](../pkg/device/allocator/allocator.go)），是纯计算，**本身无写操作**。整个 Filter 路径真正的副作用只有 Patch / Mutation / Event 三处，加上 `CheckDeviceRequest` 与不支持的 NodePolicy 各一处告警 Event。

此外真实 Filter 语义上还有一处与 CA 需求不符：命中第一个可行节点后立即停止（因为要提交唯一节点），**只返回一个节点**。CA 想知道的是「**所有**能放下的节点 / 模板」，需要返回完整可行集。

---

## 2. 设计目标与非目标

**目标**
- G1：提供一个 extender Filter 的**只读**语义，供 CA 仿真调用，零集群副作用（不 Patch、不改缓存、不发 Event、不混入真实调度的指标）。
- G2：该只读语义返回**所有**通过可行性判断的节点，而非首个命中。
- G3：与真实 kube-scheduler 走的 `/scheduler/filter` 路径**协议隔离**，对现有调度行为零回归。
- G4：只读语义与真实提交路径**共享同一份可行性逻辑**，杜绝两条判定漂移。

**非目标**
- N1：scale-from-0（冷启动空 nodegroup 模板节点无设备注册注解）——见 §7。
- N2：多 Pod 连续仿真（CA binpacking estimator 的假设占用累积）——见 §7。
- N3：修改真实调度的乐观预分配 / bind 竞态语义。
- N4：CA 侧的 `RegisterGPUResourceNames` / nodegroup 相似度实现（上游范畴）。

---

## 3. 方案选型

### 3.1 dry-run 信号如何传入

extender 的 `ExtenderArgs`（`k8s.io/kube-scheduler/extender/v1`）是固定结构，**无自定义字段**可塞标记。可选：

| 方案 | 机制 | 评价 |
|---|---|---|
| **A. 独立端点** | 新增 `POST /scheduler/filter-dryrun`，CA 的 extender 配置 `filterVerb: filter-dryrun` 指向它；真实 scheduler 仍用 `filter` | ✅ 显式、对真实路径零风险、无需解析请求来源。**已采用** |
| B. Query 参数 | `/scheduler/filter?dryRun=true` | 与 A 等价但路由/日志略乱，CA extender urlPrefix 拼接 query 不如独立 verb 干净 |
| C. 靠 `Nodes != nil` 判定 | 请求带完整 Node 对象就当作仿真 | ❌ `Nodes` 只表示调用方 `nodeCacheCapable: false`，普通 kube-scheduler 同样配置也会发 `Nodes`，会把真实调度误判成仿真、跳过预分配 |
| D. 请求来源识别 | 靠 UA / pod 状态启发式判断是不是 CA | ❌ 脆弱、易误判，真实 pod 也在 Pending 态 |

**选 A**。关键前提：CA 读的是**自己的** scheduler 配置文件，与真实 kube-scheduler 的配置相互独立 —— 因此可以只给 CA 配 `filter-dryrun`，真实 scheduler 保持 `filter`，互不干扰。

### 3.2 两条路径如何共享判定

不复制第二套过滤逻辑。整条链路只有一份，用一个内部模式枚举 `filterMode`（[filter_predicate.go](../pkg/scheduler/filter/filter_predicate.go)）区分契约：

```go
const (
    liveFilter  filterMode = iota // kube-scheduler：提交预分配，返回单节点
    dryRunFilter                  // 扩容仿真：只读，返回全部可行节点
)
```

`Filter` 与 `FilterDryRun` 都只是 `f.filter(ctx, args, mode)` 的一行包装。mode 只决定三件事：**能否写集群**（预分配 / Event）、**命中后是否继续扫**、**候选节点从哪来**；指标则统一按 `verb` 标签分流。可行性判断本身逐字共享，满足 G4。

---

## 4. 详细改动

### 4.1 `predicate.FilterPredicate` 接口

[pkg/scheduler/predicate/predicate.go](../pkg/scheduler/predicate/predicate.go) 增加 dry-run 方法：

```go
type FilterPredicate interface {
    Name() string
    Filter(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult
    // FilterDryRun 与 Filter 共享可行性判断，但只读：返回所有可行节点，
    // 不做预分配、不 Patch Pod、不改缓存、不发 Event。供扩容仿真调用。
    FilterDryRun(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult
    IsReady(ctx context.Context) bool
}
```

### 4.2 过滤链（filter_predicate.go）

```
Filter        → filter(args, liveFilter)   ┐
                                           ├→ preFilterRequestNodes → [nodeFilter, deviceFilter(mode)] → buildFilterResult
FilterDryRun  → filter(args, dryRunFilter) ┘
```

各函数职责：

| 函数 | 职责 | 与 mode 的关系 |
|---|---|---|
| `preFilterRequestNodes` | 校验 Pod、解析 `AllocationRequest`、组织候选节点集 | dry-run **只接受 `args.Nodes`**；live 保持 `NodeNames` 优先 |
| `nodeFilter` | 节点级门控（vGPU 使能、设备注册注解、config、显存策略），把解码结果写进 CycleState | 无副作用，两模式完全一致 |
| `preFilterNodeInfos` | 构建 `NodeInfo`（叠加节点上已有 vGPU Pod 用量）、容量预门控、UUID/型号过滤、gang 子域解析 | 无副作用，两模式完全一致 |
| `sortNodeInfos` | 按 NodePolicy(binpack/spread/none) + 拓扑 tie-break 排序 | 仅 live 在策略非法时发 `PolicyInvalid` Event |
| `deviceFilter` | 逐节点跑 allocator | 见下 |
| `buildFilterResult` | 按请求形态回包（`NodeNames` 进则 `NodeNames` 出） | dry-run 恒走 `Nodes` 形态 |

`deviceFilter` 是唯一按 mode 分叉的地方：

- **live**：`IsScheduled` 短路（已预分配的 Pod 直接引回原节点）→ 取串行锁 → 逐节点 `NewAllocator(...).Allocate` → 首个成功即 Patch + Mutation + 成功 Event + 停扫，其余节点标 `AlreadyScheduledElsewhere`。
- **dry-run**：**不取串行锁**、**不走 `IsScheduled` 短路** → 逐节点 `NewSimulationAllocator(...).Allocate`（仓库既有的只读 allocator，preempt 也在用，天然不发 Event、不计入 allocator 搜索指标）→ **扫完全部候选**，可行的全部收进结果，**绝不把可行节点写进 FailedNodes**。

无论哪种模式，`preFilterNodeInfos` 收集到的失败原因都会随返回值上抛 —— 包括「一个节点都没剩下」的情况。这正是扩容决策最需要原因的场景。

### 4.3 失败原因的粒度

`reasonsToFailedNodesMap` 按 mode 选择措辞：

- live 用 `Short()`：喂给 kube-scheduler 合成的 `0/N nodes are available: ...` 行，同时 vgpu-manager 自己发一条聚合 `FilteringFailed` Event。
- dry-run 用 `Detailed()`：仿真结果下游没有任何东西会把原因聚合成 Event，`FailedNodes` 是调用方唯一能看到的诊断。

节点级门控本身就区分了 `NodeNotVGPUEnabled` / `NodeNoVGPURegister` / `NodeBadVGPURegister` / `NodeNoVGPUConfig` / `NodeBadVGPUConfig` 五种原因（[CheckNode](../pkg/scheduler/filter/filter_predicate.go)），「没注解」和「注解解析失败」不会被压成同一句话。

### 4.4 路由（routes.go）

一个 verb 绑一个 path，handler 不从 URL 反推自己在服务谁：

```go
func AddFilterPredicate(router *httprouter.Router, predicate predicate.FilterPredicate) {
    addFilterRoute(router, filterPerfix, predicate, predicate.Filter)
}

func AddFilterDryRunPredicate(router *httprouter.Router, predicate predicate.FilterPredicate) {
    addFilterRoute(router, filterDryRunPerfix, predicate, predicate.FilterDryRun)
}
```

两个端点共用同一份 `FilterPredicateRoute` 骨架（body 大小限制、JSON 解码、`IsReady` 门控、错误回包），只是注入的 `FilterFunc` 不同。在 [cmd/device-scheduler/main.go](../cmd/device-scheduler/main.go) 一并注册。

### 4.5 并发与指标隔离

- **不占串行锁**：真实 Filter 走 `serial.Locker`（`--serial-filter-node`）串行化预分配；dry-run 无预分配、无缓存写，**不获取该锁**，仿真突发不会排在线上调度前面或后面。
- **靠 `verb` 标签隔离指标**（[pkg/scheduler/metrics/metrics.go](../pkg/scheduler/metrics/metrics.go)）：所有按调用计数的指标都带同一个 `verb` 标签，dry-run 上报 `verb="filter_dryrun"`，真实调度上报 `verb="filter"`。仿真流量是无界的（每轮对每个候选 nodegroup 都探一次），同一条 series 会把真实调度的数字冲垮，分标签后两边各看各的：
  - `vgpu_scheduler_verb_total{verb, result}` —— dry-run 的 `result` 取 `fit` / `no_fit` / `error`。
  - `vgpu_scheduler_verb_duration_seconds{verb, stage}` —— dry-run 上报 `total` / `node` / `device` 三个阶段，不上报 `lock_wait`（它不加锁）。
  - `vgpu_scheduler_node_reject_total{verb, code}` —— 逐节点拒绝原因，同样按 verb 分开。
- **按 Pod 计数的指标一律不写**：`topology_placement_total`、`pod_policy_total`、`crosspod_alignment_total` 只在真实放置时记录；`topology_strict_reject_total`、`link_search_total` 由 allocator 内部记录，而 dry-run 用的 `NewSimulationAllocator` 天然抑制它们。仿真没有放置任何东西，不应出现在放置统计里。

---

## 5. CA 侧配置（部署，非改码）

CA 的 scheduler 配置文件里声明 extender，`filterVerb` 指向 dry-run 端点：

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: default-scheduler
extenders:
  - urlPrefix: "https://vgpu-scheduler.kube-system.svc:443/scheduler"
    filterVerb: "filter-dryrun"      # ← 仿真专用只读端点
    enableHTTPS: true
    tlsConfig:
      caFile: /etc/vgpu-manager/tls/ca.crt
    nodeCacheCapable: false          # ← 必须 false，理由见下
    httpTimeout: 30s
    ignorable: true                  # extender 异常时不阻断扩容决策
    managedResources:
      - name: nvidia.com/vgpu-number
        ignoredByScheduler: true
      - name: nvidia.com/vgpu-memory
        ignoredByScheduler: true
      - name: nvidia.com/vgpu-cores
        ignoredByScheduler: true
```

要点：

- **`nodeCacheCapable: false` 是硬性要求**。设为 `true` 时调用方只发 `NodeNames`，而 nodegroup 模板节点**在集群里根本不存在**，我们的 nodeLister 查不到 → 全部判为 `NodeCacheMiss` → 扩容永远不触发。dry-run 端点因此显式拒绝纯 `NodeNames` 请求，回一条可操作的错误（`dry-run filter requires extenderArgs.Nodes, set nodeCacheCapable=false on the extender`），而不是静默失败。真实 kube-scheduler 那一侧不受影响，可继续用 `nodeCacheCapable: true`。
- **`ignoredByScheduler: true`** 让这三个资源交给 extender 判定而非内置 `NodeResourcesFit`，否则模板节点可能在到达我们之前就被框架过滤掉。它**不会**给模板节点凭空造出设备清单。
- **资源域名**要与实际部署的 `globalDomainName` 对齐（默认 `nvidia.com`）。
- **`enableHTTPS: true` 本身不校验证书**：不配 `caFile`/`caData` 时调用方会跳过服务端证书校验。
- **`ignorable`** 决定 extender 报错时是中止本次可行性评估还是忽略继续。上例取 `true`（扩容判断失败不至于阻塞 CA 主循环）；要求 fail-closed 则设 `false`。
- **请求体上限 7MB**（`maxRequestBodySize`）。`nodeCacheCapable: false` 时请求体是完整 Node 对象数组，超大集群需评估；必要时靠 `managedResources` 收窄调用面。

### 5.1 就绪与 leader 语义

dry-run **不是无状态的**：`preFilterNodeInfos` 要读 podLister 统计节点上已有的 vGPU 用量。因此：

- `IsReady` 门控对 dry-run 同样成立（informer 缓存未同步时回错，由 `ignorable` 决定调用方怎么处理）。
- readyz 探针同时绑定了 leader 身份（`--watch-lease` / `--leader-elect`），非 leader 副本会被摘出 Service Endpoints，调用方经 Service 访问时只会打到 leader。

### 5.2 用量语义

| 请求里的节点 | 用量来源 |
|---|---|
| nodegroup 模板节点（集群中不存在同名 Node） | podLister 查不到该节点名下的 Pod → 天然零用量，即「一台完成设备注册、尚未跑任何业务的新节点」 |
| 请求里夹带的真实节点 | 使用该节点上真实的 vGPU Pod 用量 |

对真实节点使用真实用量，而不是一律清零 —— 否则会高估已有节点的剩余容量，从而低估需要扩的节点数。

---

## 6. 影响面与回归风险

| 维度 | 评估 |
|---|---|
| 真实调度路径 | `Filter`/`bind`/`preempt` 行为不变。`deviceFilter` 现在带 mode 参数，live 分支的语句顺序（`CheckDeviceRequest` → `IsScheduled` → 取锁 → ctx 检查 → 构建 NodeInfo → 排序 → 逐节点分配提交）与重构前一致 |
| 新增端点 | `/scheduler/filter-dryrun` 纯新增、只读；不配置调用方则完全无效果 |
| 串行锁 | dry-run 不占真实调度的 serial 锁，仿真不拖慢线上 |
| 指标 | 按调用计数的指标新增 `verb` 标签区分四个 verb；按 Pod 放置计数的指标仍只在真实放置时记录 |
| 依赖 | 无新依赖；复用现有 `ExtenderArgs`/`ExtenderFilterResult` |

**测试**（[pkg/scheduler/filter/filter_dryrun_test.go](../pkg/scheduler/filter/filter_dryrun_test.go)）：

1. 只读性：`FilterDryRun` 后无任何写操作（fake client actions 为空）、无 Event、Pod 无预分配注解、Pod 缓存未被写入。
2. 全集性：多节点可行时返回全部节点且 `FailedNodes` 为空；同请求走 `Filter` 仍只返回 1 个节点。
3. 失败原因透传：所有节点都被容量预门控刷掉时，`FailedNodes` 仍逐节点带原因。
4. 模板节点：不在 nodeLister 中的节点可被正常判定。
5. 契约校验：纯 `NodeNames` 请求被拒绝，错误信息指向 `nodeCacheCapable=false`。
6. 非法请求：dry-run 返回错误但不在 Pod 上留 Event；同请求走 `Filter` 会发 `ResourceInvalid` Event。
7. 指标隔离：dry-run 只推进 `verb="filter_dryrun"` 的计数，真实 verb 的计数不动；逐节点拒绝原因同样落在自己的 verb 上。

`go test -race ./pkg/scheduler/...` 通过。

**尚未做**：真机 CA 端到端验证（依赖 CA 侧 #9786 进入可用版本）。

---

## 7. 已知局限

### 7.1 scale-from-0（冷启动空 nodegroup）

节点级门控要求节点带 `nvidia.com/node-device-register` 注解，否则判 `NodeNoVGPURegister`。CA 从 0 扩时用的是 provider 生成的模板节点，没有该注解 → 一律被拒 → 冷启动扩容失效。这与上游 #9786 自陈的 scale-from-0 局限一致。

后续可选做法（本期不做）：

- 让 device-plugin / operator 把每类 GPU 机型的设备容量与拓扑「模板」以节点标签或 annotation 形式预置到 nodegroup 模板节点上；
- dry-run 识别模板节点，从模板容量合成一个「虚拟 NodeDeviceInfo」参与判定。

### 7.2 warm 模板节点本身也未必可信

即使 warm nodegroup 的模板复制了某台在役节点的注册注解，注解里的设备 ID、健康状态、NUMA 归属、MIG 配置、设备互联评分描述的是**被采样那台机器的运行时状态**。采样节点上有坏卡，或组内机型/拓扑/device-plugin 配置不一致，模板就会错报新节点的容量。需要 nodegroup → 设备 profile 的稳定映射、同构性与采样规则、以及新节点注册后与 profile 的对账，才能真正可靠。

### 7.3 多 Pod 连续仿真

CA 的 estimator 会往快照里连续假设放置多个 Pod，但标准 `ExtenderArgs` 只携带**当前这一个** Pod 和候选节点，不包含此前已被假设放置到这些节点上的 Pod 及其设备分配。我们每次请求都从当前真实状态重新计算，后一个 Pod 看不到前一个的假设占用 → **可能低估需要扩的节点数**。

要解决需要一份跨请求契约（仿真会话、模板节点、假设分配、重试与回滚），且必须与真实调度状态完全隔离；进程内匿名缓存做不到（生命周期无定义，无法与调用方的重试/回滚保持一致）。

### 7.4 部署面

只应让预期的调用方访问 dry-run 端点。TLS、NetworkPolicy、超时、限流当前均由部署侧负责，仓库内未提供样例清单。dry-run 虽不占串行锁，但每个请求会按 `GOMAXPROCS*2` 并行构建 NodeInfo，高并发仿真下建议在部署侧限流。

---

## 8. 小结

- CA #9786 使 vgpu-manager 这类 extender 能被扩容仿真感知，**前提是集群用 CA 且其版本已带该能力**。
- 唯一硬阻塞是 Filter 的乐观预分配副作用（Patch/Mutation/Event）—— 因 `allocator.Allocate` 本就是纯计算，抽出只读语义干净可行。
- 落地：新增独立只读端点 `/scheduler/filter-dryrun`（方案 A），与真实路径共享同一条过滤链、靠内部 `filterMode` 区分契约、不占串行锁、指标按 `verb` 标签隔离；调用方配 `filterVerb: filter-dryrun` + `nodeCacheCapable: false` + 三个资源名。
- scale-from-0、模板可信度、多 Pod 连续仿真为已知局限，见 §7。
