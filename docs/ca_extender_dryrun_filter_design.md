# Cluster Autoscaler extender 接入：Filter dry-run 改造设计

> 作用范围：`pkg/scheduler/filter/`、`pkg/scheduler/predicate/`、`pkg/route/`、`cmd/device-scheduler/`。
> 目标：让 vgpu-manager 的 extender Filter 能被 Cluster Autoscaler(CA)扩容仿真**只读地**调用，修复「CA 仿真看不到 extender 过滤 → vGPU 饱和时该扩不扩、pod 卡 Pending」的问题。
> 触发背景：k8s/autoscaler PR #9786（Integrate Extender-Managed Resources）把 HTTP extender 接进 CA 的扩容仿真链路。
> 状态：设计稿（新增独立端点，默认不影响现有调度路径）。

---

## 1. 背景与问题

### 1.1 CA #9786 做了什么

Cluster Autoscaler 决定要不要扩节点时，靠一套**内置调度仿真**判断「pending pod 能不能塞进候选节点 / nodegroup 模板节点」。此前该仿真**从不调用 HTTP scheduler extender**，所以对 HAMi / vgpu-manager 这类「真正可行性判断在 extender 里」的方案是盲区：CA 只看 Node 的 `Allocatable` 标量聚合，无法感知单卡碎片、显存超卖、NUMA/NVLink 拓扑、MIG 等约束 → 误判节点还能塞 → 该扩不扩。

PR #9786 的做法（均为 **CA 侧**改动）：

- `utils/gpu/gpu.go` 新增 `RegisterGPUResourceNames()`，运行时把 extender 管理的资源名注册成 GPU vendor 资源。
- `core/autoscaler.go` `registerExtenderManagedResources()` 从 `KubeSchedulerConfiguration.Extenders` 读配置并注册。
- `simulator/framework/handle.go` 用 `scheduler.NewHTTPExtender()` 实例化 extender，存进 `Handle.Extenders`。
- `simulator/clustersnapshot/predicate/plugin_runner.go` 新增 `runExtenderFilters()`，在节点过滤时逐个调 extender 的 Filter HTTP 端点；`RunFiltersUntilPassingNode` 改为「先收集所有通过内置插件的节点，再交给 extender 做后置过滤」。

配置层面复用现有 `KubeSchedulerConfiguration.Extenders[].managedResources[].name`，**无新增 CLI flag**。已知局限：scale-from-0（冷启动空 nodegroup）尚未解决；extender 如何暴露「资源配比」无标准。

### 1.2 vgpu-manager 正是这类系统

vgpu-manager 本身就是标准 HTTP scheduler extender：

- [cmd/device-scheduler/main.go](../cmd/device-scheduler/main.go) 起 extender 服务，[pkg/route/routes.go:27-28](../pkg/route/routes.go#L27-L28) 暴露 `POST /scheduler/filter`、`/scheduler/bind`。
- 管理三个资源：`nvidia.com/vgpu-number`、`nvidia.com/vgpu-memory`、`nvidia.com/vgpu-cores`（[pkg/util/consts.go:45-47](../pkg/util/consts.go#L45-L47)，域名 `globalDomainName` 可配，默认 `nvidia.com`）。
- 可行性判断全在 [pkg/scheduler/filter/filter_predicate.go](../pkg/scheduler/filter/filter_predicate.go) 的 `Filter()` 里。

所以只要集群跑 CA，PR #9786 就直接使 vgpu-manager 受益 —— **但有一个必须先解决的硬伤**。

### 1.3 核心 Bug：Filter 有副作用，不能被仿真安全调用

现状 `Filter()` 把两件事**耦合**在一起：

1. **可行性判断（只读）**：`nodeFilter`（节点级门控：设备注册、config、显存策略）+ `deviceFilter` 前半段（构建 NodeInfo、容量预门控、按策略排序）。
2. **提交（写集群）**：`deviceFilter` 尾部对第一个能放下的节点做乐观预分配。

副作用集中在 [filter_predicate.go:828-865](../pkg/scheduler/filter/filter_predicate.go#L828-L865)：

```go
newPod, rsn, err := allocator.NewAllocator(nodeInfo.NodeInfo, recorder).Allocate(req) // 纯计算，返回 DeepCopy
...
if err = client.PatchPodPreAllocatedMetadata(f.kubeClient, newPod); err != nil { ... } // ← Patch 真实 Pod
f.podLister.Mutation(newPod)                                                            // ← 改本地缓存
...
f.recorder.Eventf(pod, corev1.EventTypeNormal, reason.EventFilteringSucceed, ...)       // ← 发 Event
```

这是 HAMi 系一贯的「filter 阶段乐观预分配」模式：把设备分配结果 patch 进真实 Pod，桥接 filter→bind 之间的竞态。

**为什么挡住 CA**：CA 仿真必须只读，而它会对多个候选节点 / 模板节点、跨多个 nodegroup、多轮调用 `/filter`。一旦调到 vgpu-manager，就会对真实 pending Pod 打预分配注解、污染 podLister 缓存、刷 Event，甚至**过早把 pod 提交到错误节点**。这在接入前必须解决。

> 关键事实（已核对）：`allocator.Allocate` 在 `pod.DeepCopy()` 上运算（[allocator.go:68](../pkg/device/allocator/allocator.go#L68)），是纯计算，**本身无写操作**。整个 Filter 路径真正的副作用只有上述三处（Patch / Mutation / Event）。这让「抽出只读可行性核心」在工程上是干净可行的。

此外 `Filter()` 语义上还有一处与 CA 需求不符：现状命中第一个可行节点后 `success=true` 立即停止（因为要提交唯一节点，见 [filter_predicate.go:817-862](../pkg/scheduler/filter/filter_predicate.go#L817-L862)），**只返回一个节点**。CA 想知道的是「**所有**能放下的节点 / 模板」，需要返回完整可行集。

---

## 2. 设计目标与非目标

**目标**
- G1：提供一个 extender Filter 的**只读**语义，供 CA 仿真调用，零集群副作用（不 Patch、不改缓存、不发 Event）。
- G2：该只读语义返回**所有**通过可行性判断的节点，而非首个命中。
- G3：与真实 kube-scheduler 走的 `/scheduler/filter` 路径**完全隔离**，对现有调度行为零回归。
- G4：只读核心与真实提交路径**共享同一份可行性逻辑**，杜绝两条判定漂移。

**非目标**
- N1：scale-from-0（冷启动空 nodegroup 模板节点无设备注册注解）—— 属 PR #9786 自身未解决项，本设计留作 Phase 2（见 §6）。
- N2：修改真实调度的乐观预分配 / bind 竞态语义。
- N3：CA 侧的 `RegisterGPUResourceNames` / nodegroup 相似度实现（上游范畴）。

---

## 3. 方案选型

### 3.1 dry-run 信号如何传入

extender 的 `ExtenderArgs`（`k8s.io/kube-scheduler/extender/v1`）是固定结构，**无自定义字段**可塞标记。可选：

| 方案 | 机制 | 评价 |
|---|---|---|
| **A. 独立端点** | 新增 `POST /scheduler/filter-dryrun`，CA 的 extender 配置 `filterVerb: filter-dryrun` 指向它；真实 scheduler 仍用 `filter` | ✅ 显式、对真实路径零风险、无需解析请求来源。**推荐** |
| B. Query 参数 | `/scheduler/filter?dryRun=true` | 与 A 等价但 httprouter 路由/日志略乱，CA extender urlPrefix 拼接 query 不如独立 verb 干净 |
| C. 请求来源识别 | 靠 UA / pod 状态启发式判断是不是 CA | ❌ 脆弱、易误判，真实 pod 也在 Pending 态 |

**选 A**。关键前提（已确认可行）：CA 读的是**自己的** scheduler 配置文件（经 `--scheduler-config-file` 之类传入），与真实 kube-scheduler 的配置相互独立 —— 因此可以只给 CA 配 `filter-dryrun`，真实 scheduler 保持 `filter`，互不干扰。

### 3.2 只读核心如何抽取

把现状 `deviceFilter` 拆成两段：

- **可行性核心 `feasibleNodes()`**（只读）：node 级门控 + NodeInfo 构建 + 容量预门控 + `allocator.Allocate`（纯计算）。对候选集里**每个**节点独立判断「能否分配」，返回可行节点全集 + 每节点失败原因。**不排序、不命中即停、不 Patch/Mutation/Event**。
- **提交段 `commitPreAllocation()`**（写）：仅真实路径调用。在可行集上按 NodePolicy 排序、取最优节点、`PatchPodPreAllocatedMetadata` + `podLister.Mutation` + Event。

两个入口复用同一 `feasibleNodes()`：

```
Filter (真实)         : feasibleNodes() → 排序取最优 → commitPreAllocation() → 返回单节点
FilterDryRun (CA)     : feasibleNodes()                                     → 返回全部可行节点
```

这样满足 G4（判定不漂移）、G2（dry-run 返回全集）、G3（真实路径行为字节级不变，只是把尾段抽成函数再调回来）。

---

## 4. 详细改动

### 4.1 `predicate.FilterPredicate` 接口

[pkg/scheduler/predicate/predicate.go](../pkg/scheduler/predicate/predicate.go) 增加 dry-run 方法：

```go
type FilterPredicate interface {
    Name() string
    IsReady(ctx context.Context) bool
    Filter(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult
    // FilterDryRun 与 Filter 共享可行性判断，但只读：返回所有可行节点，
    // 不做预分配、不 Patch Pod、不改缓存、不发 Event。供 Cluster Autoscaler
    // 扩容仿真调用。
    FilterDryRun(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult
}
```

### 4.2 `gpuFilter` 拆分（filter_predicate.go）

将 `Filter()` 与 `deviceFilter` 重构为共享核心：

```go
// feasibleNodes 只读地判断候选集中每个节点能否容纳该 pod。
// 不排序、不命中即停、无任何副作用（allocator.Allocate 在 DeepCopy 上运算）。
func (f *gpuFilter) feasibleNodes(
    ctx context.Context, req *allocator.AllocationRequest, candidates []corev1.Node, state CycleState,
) (passed []corev1.Node, failed map[string]*reason.FilterReason, err error) {
    // = 现 nodeFilter + deviceFilter 的「构建 NodeInfo + 容量预门控 + 逐节点 allocator.Allocate」
    //   但对每个节点独立判定并全部收集，去掉 success 短路与尾部 Patch/Mutation/Event。
}

// Filter：真实调度路径 —— 可行性 + 提交（保持现语义：返回首个/最优单节点并预分配）。
func (f *gpuFilter) Filter(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
    // ...解析 req、组织候选集（不变）...
    passed, failed, err := f.feasibleNodes(ctx, req, filteredNodes, state)
    if err != nil { return &extenderv1.ExtenderFilterResult{Error: err.Error()} }
    // 按 NodePolicy 排序取最优 + commitPreAllocation()（= 现 828-865 段）
    // 返回单节点（现语义不变）
}

// FilterDryRun：CA 仿真路径 —— 只读，返回全部可行节点。
func (f *gpuFilter) FilterDryRun(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
    // ...解析 req、组织候选集（复用同一段）...
    passed, failed, err := f.feasibleNodes(ctx, req, filteredNodes, state)
    if err != nil { return &extenderv1.ExtenderFilterResult{Error: err.Error()} }
    return buildResult(nodeCache, passed, failed) // 不 commit、不发 Event
}
```

要点：
- `commitPreAllocation()` 内聚现 [filter_predicate.go:826-865](../pkg/scheduler/filter/filter_predicate.go#L826-L865) 的 `Allocate → Patch → Mutation → Event`。真实路径仅对**选中的最优节点**调用它一次（现状是命中即停，等价）。
- dry-run 复用 §4.1 之外**不触碰** `f.kubeClient`、`f.podLister.Mutation`、`f.recorder`。
- `req := allocator.BuildAllocationRequest(pod)` 及「pod 未请求 vGPU 则原样放行」（[filter_predicate.go:175-182](../pkg/scheduler/filter/filter_predicate.go#L175-L182)）两条入口共享，行为一致。

### 4.3 路由（routes.go）

参照现 `FilterPredicateRoute`（[routes.go:96](../pkg/route/routes.go#L96)）新增 dry-run 路由，仅末尾改调 `FilterDryRun`：

```go
const filterDryRunPerfix = apiPrefix + "/filter-dryrun"

func AddFilterDryRunPredicate(router *httprouter.Router, p predicate.FilterPredicate) {
    router.POST(filterDryRunPerfix, DebugLogging(FilterDryRunPredicateRoute(p), filterDryRunPerfix))
}
// FilterDryRunPredicateRoute 与 FilterPredicateRoute 结构一致，
// 仅第 120 行 p.Filter(...) 换成 p.FilterDryRun(...)。
```

在 [cmd/device-scheduler/main.go:139](../cmd/device-scheduler/main.go#L139) 一并注册：

```go
route.AddFilterPredicate(handler, filterPlugin)
route.AddFilterDryRunPredicate(handler, filterPlugin) // 新增
route.AddBindPredicate(handler, bindPlugin)
```

### 4.4 并发/串行隔离

真实 Filter 走 `serial.Locker`（[filter_predicate.go:98](../pkg/scheduler/filter/filter_predicate.go#L98)，`serialFilterNode` 开关）以串行化预分配、避免并发误算。**dry-run 无预分配、无缓存写，不应与真实调度争同一把串行锁** —— 否则 CA 的仿真调用会阻塞线上调度。设计：`feasibleNodes()` 只读，dry-run 入口**不获取** serial 锁（或使用独立的只读并发上限）。真实 `Filter` 保持现有加锁语义不变。

---

## 5. CA 侧配置（部署，非改码）

CA 的 scheduler 配置文件里声明 extender，`filterVerb` 指向 dry-run 端点：

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
extenders:
  - urlPrefix: "http://vgpu-scheduler.kube-system:443/scheduler"
    filterVerb: "filter-dryrun"      # ← CA 专用只读端点
    enableHTTPS: false
    nodeCacheCapable: true
    managedResources:
      - name: nvidia.com/vgpu-number
      - name: nvidia.com/vgpu-memory
      - name: nvidia.com/vgpu-cores
    ignorable: true                  # extender 异常时不阻断 CA 决策
```

并确保 CA 的 `RegisterGPUResourceNames` 覆盖这三个资源名（PR #9786 机制）。注意资源域名要与实际部署的 `globalDomainName` 对齐（默认 `nvidia.com`）。

> `nodeCacheCapable: true` 时 CA 传 `NodeNames` 而非完整 `Nodes`，dry-run 走 `getNodesOnCache` 分支（[filter_predicate.go:197-199](../pkg/scheduler/filter/filter_predicate.go#L197-L199)），依赖 extender 端 nodeLister 缓存 —— 与真实路径一致，无额外改动。

---

## 6. Phase 2：scale-from-0（留待后续）

现 `feasibleNodes()` 对无 `nvidia.com/node-device-register` 注解的节点直接判「node has not registered any GPU devices」并拒绝（[filter_predicate.go:352-354](../pkg/scheduler/filter/filter_predicate.go#L352-L354)）。CA 从 0 扩时用的是 nodegroup **模板节点**，没有该注解 → 一律被拒 → 冷启动扩容失效。这与 PR #9786 自陈的 scale-from-0 局限一致。

后续可选做法（本期不做）：
- 让 device-plugin / operator 把每类 GPU 机型的设备容量与拓扑「模板」以节点标签或 annotation 形式，预置到 nodegroup 模板节点上；
- `feasibleNodes()` 在 dry-run 模式下识别模板节点，从模板容量合成一个「虚拟 NodeDeviceInfo」参与判定。

这块工程量与风险明显更大，且依赖上游对「extender 暴露资源配比」的标准化，故拆到独立设计。

---

## 7. 影响面与回归风险

| 维度 | 评估 |
|---|---|
| 真实调度路径 | `Filter`/`bind`/`preempt` 端点行为**不变**（仅把尾段抽成 `commitPreAllocation` 再调回）。需单测保证抽取前后 byte-for-byte 等价 |
| 新增端点 | `/scheduler/filter-dryrun` 纯新增、只读，不注册则完全无效果 |
| 串行锁 | dry-run 不占真实调度的 serial 锁，避免仿真拖慢线上 |
| 依赖 | 无新依赖；复用现有 `ExtenderArgs`/`ExtenderFilterResult` |

**测试计划**：
1. 重构等价性单测：同一 `ExtenderArgs` 下，重构前后 `Filter` 的返回节点 + FailedNodes + 是否 Patch 完全一致（复用 [filter_scale_correctness_test.go](../pkg/scheduler/filter/filter_scale_correctness_test.go)、[filter_predicate_test.go](../pkg/scheduler/filter/filter_predicate_test.go) 扩例）。
2. dry-run 只读性单测：断言 `FilterDryRun` 调用后**无** `kubeClient` PATCH、**无** `podLister.Mutation`、**无** Event（用 fake client 计数）。
3. dry-run 全集性单测：构造多节点可行场景，断言返回全部可行节点（对比 `Filter` 只返 1 个）。
4. 端到端：CA 配 `filter-dryrun`，制造 vGPU 显存饱和，验证 CA 正确触发 warm nodegroup 扩容且不误改 pending pod。

---

## 8. 小结

- CA #9786 使 vgpu-manager 这类 extender 能被扩容仿真感知，**前提是集群用 CA**。
- 唯一硬阻塞是 Filter 的乐观预分配副作用（Patch/Mutation/Event）—— 因 `allocator.Allocate` 本就是纯计算，抽出只读可行性核心 `feasibleNodes()` 干净可行。
- 落地：新增独立只读端点 `/scheduler/filter-dryrun`（方案 A），与真实路径共享判定、物理隔离、不占串行锁；CA 侧配 `filterVerb: filter-dryrun` + 注册三个资源名。
- scale-from-0 归入 Phase 2，依赖模板节点容量暴露的标准化。
