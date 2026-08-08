# 多机多卡拓扑感知调度设计(节点 label + podAffinity 方案)

> 本文给出 vgpu-manager 在 **多机多卡分布式训练** 场景下实现"一组 gang Pod 聚拢到邻近网络拓扑域"的**轻量级、保留 extender 模型、不影响现有功能**的设计。核心思路:**设备插件给节点打拓扑层级 label → webhook 给 gang Pod 注入分层 podAffinity → kube-scheduler 原生完成跨节点聚拢 → extender 在节点内做 GPU 连通分配**。**本文不包含已落地代码。**
>
> 配套:[`cross_pod_nvlink_topology_design.md`](./cross_pod_nvlink_topology_design.md)(节点内跨 Pod NVLink,即本文 L0 层)、[`scheduler_strategy_topology_refactor_design.md`](./scheduler_strategy_topology_refactor_design.md)(单 Pod 评分/拓扑底座)。

## 1. 目标与边界

| | 内容 |
|---|---|
| **目标** | 同一 gang(PodGroup)的多个 Pod 跨节点调度时,优先落到**同一网络拓扑域**(rack → spine → …),使 Pod 间 NCCL 走最短网络路径 |
| **手段** | 节点拓扑 label(生产)+ webhook 注入分层 podAffinity(消费)+ extender 节点内 GPU 连通(L0,可选) |
| **不做** | 不实现 gang 调度本身(All-or-Nothing 由外层或未来 Kueue 承接);不重写网络拓扑发现(尽量复用成熟组件) |
| **关键约束** | 全程**保留 extender 模型**,不替换调度器;所有新行为默认关闭或仅对 gang+vGPU Pod 生效,**升级无感** |

## 2. 核心机制:为什么是 podAffinity 而不是 nodeAffinity

跨节点训练**不要求同节点**,只要求"不同节点但同拓扑域"。这恰好是 podAffinity 的语义——它不等于"同节点",**域的粒度由 `topologyKey`(一个节点 label key)决定**:

| `topologyKey` | "同域"含义 |
|---|---|
| `kubernetes.io/hostname` | 同一台机器(hostname 唯一)|
| **`nvidia.com/topo-rack`** | **同一 rack,允许不同节点 ← 本方案所用** |
| `nvidia.com/topo-spine` | 同一 spine 交换组 |

- **nodeAffinity 是绝对约束**:要在 admission 时写死 `rack=X`,但那时整组 Pod 都没调度,根本不知道该选哪个 rack、有没有容量,也无法引用兄弟。
- **podAffinity 是相对约束**:"跟 gang 兄弟同 rack,具体哪个 rack 交给调度器动态定"——这是"域未知但要求一致"的唯一表达。

## 3. 总体数据流

```
device-plugin (DaemonSet, 每 GPU 节点)
  └─ TopologySource 适配器 ── 读外部来源(云/IB/LLDP/ConfigMap)
       └─ 归一化写节点 label: nvidia.com/topo-rack=<v>, nvidia.com/topo-spine=<v> ...
                                     │ (复用现有 patchNodeMetadata, registry.go:29)
                                     ▼
webhook (mutate)
  └─ PodHasGangName(pod) 命中 + 是 vGPU Pod
       ├─ 打归一化 label: nvidia.com/gang-name=<G>   (供兄弟互选)
       └─ 注入分层 podAffinity (preferred, 按 tier 加权)
                                     │
                                     ▼
kube-scheduler (原生 InterPodAffinity 插件)
  └─ 按 podAffinity 权重对节点筛选+打分 → 把后续兄弟吸引到首 Pod 所在 rack
                                     │  (extender 在此之后才被调用,透明无感)
                                     ▼
vgpu-manager extender (filter/bind)
  └─ 在已满足亲和的节点上分配 GPU
       └─ (可选 L0) 同节点多 gang Pod 时按 NVLink 连通分量选卡 = cross_pod 设计
```

---

## 4. 标签生产:选型决策树

> **关键认知:Kueue / Volcano 只"消费"拓扑 label,自身不"发现"拓扑。** 发现属于生产端,首选**复用成熟组件**,而非把它们源码编进镜像(见 §4.3)。

### 4.1 决策树

```
集群是否在公有云?
├─ 是 ──→ 直接用厂商拓扑 label(零开发)
│         device-plugin 适配器只做"厂商 label → nvidia.com/topo-* 归一化映射"
│         (AWS/GCP/Azure/OCI 都有 zone/rack/block 级 label)
└─ 否(私有云 / 裸金属)
   ├─ 网络是 InfiniBand? ──→ 查 SM/UFM(ibnetdiscover)推导
   │     首选: 跑 NVIDIA Topograph 打 label;或 device-plugin 内置 IB 适配器查 SM endpoint
   ├─ 网络是 RoCE / 以太? ──→ LLDP 邻居推 ToR
   │     首选: Topograph;或自研 LLDP 适配器(daemonset 读 /sys + lldpd)
   └─ 拓扑稳定 / 规模小? ──→ 静态 ConfigMap 映射(node→rack→spine)
         最省: device-plugin 适配器读 ConfigMap,无任何发现逻辑
```

### 4.2 三种生产路径对比

| 路径 | 适用 | 开发量 | 运维 | 备注 |
|---|---|---|---|---|
| **云厂商 label** | 公有云 | 仅归一化映射 | 无 | 最优,直接用 |
| **复用 Topograph** | IB/RoCE 私有云 | 0(装组件) | +1 组件 | NVIDIA 官方拓扑发现器,产出层级 label,本就是喂 Kueue TAS/Volcano HyperNode 用的 |
| **device-plugin 自带适配器** | 想零额外组件 | 小(几百行/源) | 无新增 | 复用现有 DaemonSet 与 node-patch 权限;发现源可插拔 |

### 4.3 为什么不把 Kueue/Topograph 源码编进镜像

- 它们是**独立控制器/二进制,不是库**:"复用逻辑"≈ 把整个 controller-runtime manager 嵌进来,比直接跑它的镜像还重。
- **依赖冲突**:Kueue 拉全量 controller-runtime + apimachinery,与本仓 [go.mod](../go.mod)(go 1.26)版本难调和,vendoring 后要替上游扛破坏性变更。
- **正确做法**:发现器**当组件原样跑**(选 Topograph),或**自己写聚焦的小适配器**;只**借鉴** Kueue 的 `Topology` CR 建模(把层级表达成"有序的节点 label key"),不借代码。

### 4.4 标签 schema

```
# 层级 label(供 topologyKey 使用,必须是 label,nodeAffinity/podAffinity 只匹配 label)
nvidia.com/topo-rack:  rack-a12      # tier 0, 最局部
nvidia.com/topo-spine: spine-3       # tier 1
nvidia.com/topo-zone:  zone-east     # tier 2, 最全局
# 调试用(可选,完整层级一目了然)
nvidia.com/node-network-topology: '{"rack":"rack-a12","spine":"spine-3","zone":"zone-east"}'
```
层级 key 顺序由配置声明(借鉴 Kueue Topology CR),tier 0 最局部、带宽最高。

---

## 5. device-plugin 拓扑适配器接口

发现源可插拔、可级联回退(契合"优先最新来源 + 优雅降级"原则):按顺序尝试,第一个成功者胜。

```go
// pkg/device/topology/source.go (新增)

// TopologySource 从外部来源发现本节点的网络拓扑域。实现可插拔:
// 云元数据 / IB SM / LLDP / 静态 ConfigMap 各为一个实现,互不耦合。
type TopologySource interface {
    // Name 返回来源标识,用于日志与指标。
    Name() string
    // Discover 返回本节点拓扑层级,从最局部(rack)到最全局(zone)有序。
    // 无法判定时返回 ErrTopologyUnavailable,调用方回退下一个来源。
    Discover(ctx context.Context, nodeName string) (NodeTopology, error)
}

type NodeTopology struct {
    // Tiers 局部→全局有序;Tiers[0] 为最局部(带宽最高)的域。
    Tiers []TopologyTier
}

type TopologyTier struct {
    Level  string // "rack" / "spine" / "zone" —— 成为 label key 后缀
    Domain string // "rack-a12" —— 成为 label value
}

// Chain 按序尝试多个来源,首个非 ErrTopologyUnavailable 的结果即采用。
type Chain []TopologySource
func (c Chain) Discover(ctx context.Context, node string) (NodeTopology, error) { /* 回退逻辑 */ }
```

内置实现(按部署环境装配):
- `cloudSource` —— 读节点已有云厂商 label,按映射表归一化。
- `ibSource` —— 查 IB SM/UFM endpoint(地址由 flag/env 给)。
- `lldpSource` —— 读本机 LLDP 邻居推 ToR。
- `configMapSource` —— 读 `node→tiers` 静态映射(最省、最稳)。

**写 label**:复用现有 [`patchNodeMetadata`](../pkg/device/manager/registry.go#L29)(已支持 label),把 `NodeTopology.Tiers` 渲染成 §4.4 的 label。与现有 `node-device-topology` 注册流程**同一条管道,顺手加 label**,不新增组件。

**配置入口**(`cmd/device-plugin`):`--topology-sources=cloud,ib,lldp,configmap`(有序),`--topology-label-prefix=nvidia.com/topo-`,`--topology-levels=rack,spine,zone`。默认空 = 不打拓扑 label(行为完全不变)。

---

## 6. webhook:分层 podAffinity 注入

在现有 [`pod_mutate.go`](../pkg/webhook/pod/mutate/pod_mutate.go)(已在变异 NodeSelector / 默认 topology 注解)里加一个变异函数。

**触发条件**(全部满足才注入):
1. `PodHasGangName(pod)` 命中(复用 [`util.go:685`](../pkg/util/util.go#L685),已支持 Volcano/Koordinator/coscheduling/原生);
2. 是 vGPU Pod(请求 `nvidia.com/vgpu-number`);
3. 开关开启:全局 `--enable-topology-affinity-injection`(默认关)或 Pod 注解 `nvidia.com/cross-node-topology` 显式 opt-in。

**注入内容**:

```yaml
# 1) 打归一化 gang label(供兄弟互选)
metadata:
  labels:
    nvidia.com/gang-name: <G>          # = PodHasGangName 结果

# 2) 分层软亲和(append,不覆盖用户已有 affinity)
spec:
  affinity:
    podAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100                     # tier 0 rack 最优先
        podAffinityTerm:
          topologyKey: nvidia.com/topo-rack
          labelSelector: { matchLabels: { nvidia.com/gang-name: <G> } }
      - weight: 50                      # tier 1 spine 次之
        podAffinityTerm:
          topologyKey: nvidia.com/topo-spine
          labelSelector: { matchLabels: { nvidia.com/gang-name: <G> } }
```

**软/硬切换**:注解 `nvidia.com/cross-node-topology: rack`(默认软)/`rack-required`(硬,转 `requiredDuringScheduling...`)。
> ⚠️ **硬亲和 + 无 gang = 装不下整组就死锁 Pending**。默认软;硬模式必须搭配外层 gang 或足够冗余容量,文档明确告警。

**合并规则**:用户已写 affinity 时只 append preferred terms,不动 required;权重避免与用户冲突。

**整卡闸门**(validate webhook,可选):拓扑/gang Pod 建议 `vgpu-cores=100 + 整卡 memory`,对"拓扑 + 分片"矛盾配置给 warning(训练要整卡,vGPU 分片在此场景无意义)。

---

## 7. 调度器(extender)是否需要改造?

**核心结论:跨节点聚拢这部分,extender 一行都不用改。**

| 能力层 | extender 是否改造 | 原因 |
|---|---|---|
| **跨节点聚拢(本方案核心)** | ❌ **不需要** | podAffinity 由 kube-scheduler 的 **InterPodAffinity 插件在调用 extender 之前**完成节点筛选与打分;extender 拿到的已是满足亲和的节点子集,**透明无感** |
| **节点内 GPU 连通(L0)** | ◐ **仅特定场景需要** | 见下 |
| **节点级域容量打分(lookahead)** | ❌ **不做** | extender 当前只有 filter/bind/preempt,**无 prioritize verb**([routes.go:27-29](../pkg/route/routes.go#L27));新增 verb 会把跨节点容量推理塞回 extender,违背"保持简单"。首 Pod 选域用软亲和 + 冗余容量即可接受 |

### 关键洞察:最常见的部署形态下,L0 也不用动

```
形态 A:1 节点 = 1 Pod = 8 卡(典型多机数据并行)
  ├─ 跨节点:本方案 podAffinity 把这些单 Pod 节点聚到同 rack            ✅ 无需改 extender
  └─ 节点内:每节点就一个 Pod 独占 8 卡,现有「单 Pod link 拓扑」已覆盖   ✅ 无需 L0
  ⇒ 仅靠 §4-§6(label + webhook),整条链路完整,extender 零改动

形态 B:1 节点 = 多个 gang Pod(MPI 风格 / 小作业打包)
  └─ 节点内多 Pod 需 NVLink 连通 → 需要 L0 = cross_pod_nvlink 设计
     (feature gate 包裹,默认关,不动现有路径)
```

**所以**:对绝大多数"整机整卡"训练,**本方案 = label + webhook 两处新增,调度器不改**;只有"单节点塞多个 gang Pod"才需要叠加已规划的 L0(且独立 feature gate)。

---

## 8. 不影响现有功能的保证

| 改动点 | 默认行为 | 启用方式 |
|---|---|---|
| device-plugin 拓扑 label | `--topology-sources` 空 → 不打 label,完全不变 | 配置来源后才打 |
| webhook podAffinity 注入 | 开关默认关 + 仅 gang+vGPU Pod | 全局 flag 或 Pod 注解 opt-in |
| webhook 只 append preferred | 不覆盖用户 affinity | —— |
| extender | **无改动**(核心场景) | —— |
| L0 跨 Pod NVLink | feature gate 默认关 | 仅多 Pod/节点场景按需 |

全部为**增量 + 默认关 + 仅对目标 Pod 生效**,存量部署升级无感。

---

## 9. 落地路线

```
P0 标签生产(1-2 周, 零行为变更)
  ├─ 按 §4 决策树选来源;实现对应 TopologySource(云=只归一化 / IB / LLDP / ConfigMap)
  ├─ device-plugin 装配 Chain + 写 label(复用 patchNodeMetadata)
  └─ 验证: kubectl get node -L nvidia.com/topo-rack 出现正确域值

P1 webhook 注入(1-2 周)
  ├─ pod_mutate 加触发判定 + gang label + 分层 podAffinity(软)
  ├─ 开关默认关,小集群 opt-in 灰度
  └─ 验证: 同 gang Pod 落同 rack(describe pod 看 node 的 topo-rack 一致)

P2(条件性)节点内 L0
  └─ 仅"多 Pod/节点"场景:落地 cross_pod_nvlink Phase 1/2(feature gate)

P3(可选)硬保证升级路径
  └─ 需要 All-or-Nothing 时:接 Kueue TAS(复用同一套 topo-* label,本方案平滑升级,非重写)
```

**验证基线**:`nccl-tests` all_reduce 实测带宽;`NCCL_DEBUG=INFO` 确认跨节点走 `NET/IB/GDRDMA`;调度断言同 gang Pod 节点的 `topo-rack` label 一致。

---

## 10. 风险与边界

| 风险 | 缓解 |
|---|---|
| 首 Pod 无锚点,可能选到装不下整组的域(软亲和散开 / 硬亲和死锁) | 默认软亲和 + 冗余容量;硬保证留给 Kueue TAS(P3) |
| 无 All-or-Nothing,部分调度浪费 GPU | 现有 stuck-pod grace([`ShouldCountPodDeviceAllocation`](../pkg/device/types.go))超时放卡兜底;接受 best-effort |
| podAffinity 在 kube-scheduler 中偏贵 | 官方建议 ≤几百节点;大集群压测,必要时降为 nodeSelector + Kueue 域指派 |
| rack/rail label 节点无法自查 | 由外部来源经适配器消费(§4),不指望节点自给 |
| 拓扑变更(热插拔/迁移)label 过期 | 适配器周期 reconcile;复用 device-plugin health 流程 |

---

## 11. 与既有文档关系

```
本文 = 跨节点聚拢(L2/L3): label 生产 + webhook podAffinity, 保留 extender、调度器基本不改
  └─ 可选叠加 → cross_pod_nvlink_topology_design (L0 节点内跨 Pod NVLink, 仅多 Pod/节点)
  └─ 底座     → scheduler_strategy_topology_refactor_design (单 Pod 评分/拓扑)
  └─ 升级路径 → Kueue TAS (复用同一 topo-* label, 获得 All-or-Nothing)
```

## 12. 三句话总结

1. **跨节点聚拢 = 节点拓扑 label + webhook 注入分层 podAffinity**;kube-scheduler 原生消化亲和,**调度器核心不改**。
2. **标签生产优先复用成熟组件**(云 label / Topograph),或 device-plugin 加可插拔适配器;**绝不 vendoring 大型控制器源码**。
3. **最常见的"1 节点 1 Pod 整机整卡"形态下,本方案两处新增即完整**;"多 Pod/节点"才叠加 L0;要硬保证再平滑升级到 Kueue TAS。
