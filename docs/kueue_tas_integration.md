# Kueue TAS 与 vgpu-manager 集成使用文档

> 本文说明如何用 **Kueue 的拓扑感知调度(Topology-Aware Scheduling, TAS)** 为 vgpu-manager 补齐**多机多卡跨节点拓扑亲和**能力:让一组分布式训练 Pod 收敛到同一网络拓扑域(rack/block),同时**完全保留 vgpu-manager 现有的 extender 与节点内 GPU 拓扑分配**。结论:**二者可良好配合,vgpu-manager 调度器基本无需额外开发**(见 §7)。
>
> 关联:[`multinode_topology_aware_scheduling_analysis.md`](./multinode_topology_aware_scheduling_analysis.md)(总体分层)、[`cross_pod_nvlink_topology_design.md`](./cross_pod_nvlink_topology_design.md)(节点内 L0)。

## 1. 为什么 Kueue TAS 能与 vgpu-manager 配合(三个已验证的前提)

| 前提 | 验证 | 含义 |
|---|---|---|
| **Kueue 不替换调度器** | Kueue 文档:TAS "finds a fixed assignment of pods to nodes and **injects a NodeSelector**",不接管调度 | 注入 NodeSelector 后仍由 kube-scheduler 调度 → **vgpu-manager extender 原样被调用**,不冲突 |
| **Kueue 按 `.status.allocatable` 统计扩展资源** | Kueue 文档:容量基于 "Node allocatable capacity (`.status.allocatable`)",GPU 示例用 `nvidia.com/gpu` 作 `coveredResources` | 只要资源在节点 allocatable 里,Kueue 就能按拓扑域统计 |
| **`nvidia.com/vgpu-number` 是真实扩展资源** | device plugin 经 ListAndWatch 注册进 kubelet;[`deploy/vgpu-manager-scheduler.yaml:164`](../deploy/vgpu-manager-scheduler.yaml#L164) `ignoredByScheduler: false` | **Kueue 能按 rack/block 统计 vgpu-number 容量**,正确选域 ✓ 这是集成命门 |

> 注:`nvidia.com/vgpu-cores` / `nvidia.com/vgpu-memory` 在 extender 里是 `ignoredByScheduler: true`,kube-scheduler 与 Kueue 的容量计算都不看它们(仅 extender 精算)。因此 **TAS 场景下建议以 `vgpu-number` 为绑定维度(整卡申请)**,见 §6 与 §7 的注意事项。

## 2. 分层职责(谁管哪一层)

```
Workload(Job/JobSet/LWS/MPIJob…),Pod 模板:schedulerName: vgpu-scheduler
  + 注解 kueue.x-k8s.io/podset-preferred-topology: <rack 标签>
  + requests nvidia.com/vgpu-number: N
        │
        ▼  ① 跨节点域选择 + Gang 准入        ← Kueue TAS(L2/L3)
Kueue
  - ClusterQueue/LocalQueue 配额校验 + 整个 Workload 原子准入(gang)
  - TAS:按 node .status.allocatable 统计各拓扑域的 vgpu-number 余量,
         选出能装下整组的最低层域(rack,装不下回退 spine);最细层=rack,
         给每个 Pod 注入 rack 级 NodeSelector(不钉具体节点),记录于
         status.admission.podSetAssignments[].topologyAssignment
        │
        ▼  ② rack 内选节点 + 在该节点选具体 GPU  ← kube-scheduler + vgpu-manager extender(L0/L1)
kube-scheduler(profile: vgpu-scheduler)
  - rack 级 NodeSelector 把每个 Pod 限定到该 rack 的节点集
  - 基础 PodFitsResources 计 vgpu-number;调用 vgpu-manager extender
        │
        ▼
vgpu-manager(节点级策略 + extender)
  - 节点级:binpack/spread + 拓扑 fitness 在 rack 内选最佳节点
  - 设备级:在该节点上选具体物理卡(节点内拓扑):
    · 单 Pod 整机多卡 → 现有单 Pod link 拓扑
    · 多 Pod 共节点   → 跨 Pod NVLink anchor(已落地,注解 nvidia.com/cross-pod-topology)
    · 跨节点子域对齐  → 按兄弟域签名对齐(rail-set 或 ordinal 回退,已落地,§7.2)
  - device-scheduler-policy: spread → 在连通分量内优先选空闲卡(间接反共置)
```

**一句话**:**Kueue 决定"哪些节点"(跨节点 rack 域),vgpu-manager 决定"哪几张卡"(节点内 NVLink 域)。** 二者正交互补,无重叠、无冲突。

## 3. 前置条件

- Kueue **v0.14+**(`TopologyAwareScheduling` feature gate 默认开启;更早版本需手动开)。
- vgpu-manager 已部署,Pod 使用 `schedulerName: vgpu-scheduler`。
- 节点已打**网络拓扑层级 label**(Kueue 只消费、不发现;来源见 multinode 文档 §4:云厂商 label / NVIDIA Topograph / LLDP / 静态 ConfigMap)。
- 训练镜像统一(NCCL/CUDA 版本一致)。

## 4. 部署步骤

### 4.1 给节点打拓扑 label(前置,非 Kueue 职责)

```bash
# 示例:两层 spine + rack(粗→细)。spine 用作 rack 装不下时的回退层。
kubectl label node gpu-node-1 topology.example.com/spine=spine-1 topology.example.com/rack=rack-a
kubectl label node gpu-node-2 topology.example.com/spine=spine-1 topology.example.com/rack=rack-a
kubectl label node gpu-node-3 topology.example.com/spine=spine-1 topology.example.com/rack=rack-b
# 不需要给 hostname 单独建一层(见 §4.2):我们要 Kueue 只约束到 rack、
# 把"rack 内选哪个节点"留给 vgpu-manager。
```

### 4.2 Topology CRD(层级从粗到细;最细层 = rack,不用 hostname)

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: Topology
metadata:
  name: gpu-topology
spec:
  levels:
  - nodeLabel: topology.example.com/spine  # 粗:同 spine(rack 装不下时回退到这一层)
  - nodeLabel: topology.example.com/rack   # 细:最细可调度域 = rack
```

**为什么不放 `kubernetes.io/hostname`**:hostname 在 Kueue 里是**可选**的(规则是"若使用则必须是最细层",并非必须存在)。两种取舍:

| 最细层 | Kueue 行为 | 谁选节点 |
|---|---|---|
| `kubernetes.io/hostname` | TAS 做**逐 Pod→节点固定分配**,注入 hostname NodeSelector **把 Pod 钉死到具体节点** | Kueue(我们失去节点策略) |
| **`rack`(本文采用)** | TAS 只保证整组在一个 rack、注入 **rack 级 NodeSelector**,**rack 内选哪个节点交给 kube-scheduler + vgpu-manager** | **vgpu-manager**(节点 binpack/spread 生效) |

本文按"我们做节点选择"的设计取 **rack 为最细层**。⚠️ **代价**:Kueue 只核对 rack 级总容量,不保证节点级可放(节点碎片可能使某 Pod 装不下 → Pending,参见 Kueue issue #5243)。缓解:**整卡申请**让 rack 总量÷8≈可用节点数、碎片小;或对碎片敏感的作业改用 hostname 最细层(让 Kueue 兜底精确放置,放弃我们的节点策略)。

### 4.3 ResourceFlavor 绑定 Topology + 选 GPU 节点(含同构精确筛选)

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: gpu-flavor
spec:
  topologyName: gpu-topology               # ← 不设此字段则该 flavor 不启用 TAS
  nodeLabels:
    vgpu-manager: device-plugin            # 选中 vgpu-manager GPU 节点(= device-plugin DaemonSet 的 nodeSelector)
    # —— 同构精确筛选(让第一层筛选更准)——
    # 跨节点对齐:同构(GPU↔rail 布局一致)走 ordinal 回退即可;异构布线需发布
    # node-gpu-domain rail 注解(§7.2.1)。混代际还会引发 NCCL/CUDA 版本错配,
    # 故仍建议用 device-plugin 自动发布的标签把 flavor 锁到单一代际、按代际拆多个 flavor。
    # nvidia.com/node-cuda-version: "12080"
    # nvidia.com/node-driver-version: "570.86.15"
```

### 4.4 ClusterQueue / LocalQueue(把 vgpu-number 纳入配额)

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: gpu-cq
spec:
  namespaceSelector: {}
  resourceGroups:
  - coveredResources: ["nvidia.com/vgpu-number"]   # ← 必须覆盖 vgpu-number
    flavors:
    - name: gpu-flavor
      resources:
      - name: nvidia.com/vgpu-number
        nominalQuota: 64                             # 全集群 vGPU 配额
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: LocalQueue
metadata:
  name: gpu-lq
  namespace: default
spec:
  clusterQueue: gpu-cq
```

### 4.5 提交分布式训练 Job(关键三要素)

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: nccl-train
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: gpu-lq          # ① 进 Kueue 队列(触发准入+TAS)
spec:
  parallelism: 4                                # 4 个 Pod(多机)
  completions: 4
  suspend: true                                 # Kueue 要求:由它来 resume
  template:
    metadata:
      annotations:
        # ② rack 最优先,装不下自动回退到 spine(逐级向上);要"必须同 rack 否则等"
        #    改用 podset-required-topology
        kueue.x-k8s.io/podset-preferred-topology: topology.example.com/rack
    spec:
      schedulerName: vgpu-scheduler             # ③ 仍走 vgpu-manager extender
      restartPolicy: Never
      containers:
      - name: trainer
        image: <your-nccl-image>
        resources:
          limits:
            nvidia.com/vgpu-number: 8           # 整机 8 卡(见 §6 整卡建议)
            nvidia.com/vgpu-memory: 80000       # 整卡显存(extender 精算,Kueue 不计)
        # 节点内拓扑(可选):整机 8 卡时单 Pod link 即可
```

**注解选择(层级回退靠 levels 顺序 + preferred 表达)**:
- `podset-preferred-topology: <rack>` —— **软**:rack 最优先,装不下**沿 levels 向上逐级回退**(rack→spine→更高),最终仍可跨域。**"rack 最优先、spine 次之"就是这样体现的**:annotation 指到 rack,levels 里 spine 在 rack 之上即自动成为下一回退层。
- `podset-required-topology: <rack>` —— **硬**:必须整组同一个 rack,装不下就一直等(**不回退到 spine**),适合性能敏感、宁等勿散的训练。
- `podset-unconstrained-topology: "true"` —— 不约束拓扑,仅看容量(最小碎片)。
- 多 PodSet(如 LWS 的 leader+worker)要求同域:`podset-group-name`。

### 4.6 仓库示例与"让第一层筛选更准"的三个抓手

仓库 [`example/kueue/`](../example/kueue/) 已提供可直接套用的配置:

| 文件 | 作用 |
|---|---|
| [`configuration.yaml`](../example/kueue/configuration.yaml) | Kueue `Configuration` 的 **resource transformation**:把 per-vGPU 的 `vgpu-cores`/`vgpu-memory` ×`vgpu-number` 折算成 `total-vgpu-cores`/`total-vgpu-memory`。**这让 Kueue 的配额/域容量计数对"分片"语义也准确**(否则按 per-vGPU 值算会偏小)。 |
| [`sample.yaml`](../example/kueue/sample.yaml) | 基础配额示例(无 TAS):ResourceFlavor(`vgpu-manager: device-plugin`)+ ClusterQueue/LocalQueue + 一个 gpu-burn Deployment。 |
| [`topology-aware.yaml`](../example/kueue/topology-aware.yaml) | **拓扑感知示例(本文方案)**:Topology(spine/rack)+ `topologyName` flavor + 整卡 gang Job(`preferred-topology` + `device-topology-mode: link` + `cross-pod-topology`)。 |

**让 Kueue 第一层(节点)筛选更准,vgpu-manager 这边能贡献的三点**:

1. **精确选中 GPU 节点**:flavor 用 `vgpu-manager: device-plugin`(device-plugin DaemonSet 的 nodeSelector)选节点——**恰好就是装了 vgpu-manager 的那批节点**,不多不少。
2. **同构精确化(§4.3)**:device-plugin **自动**给节点打 `nvidia.com/node-driver-version`、`nvidia.com/node-cuda-version`。把它们加进 flavor 的 `nodeLabels`(或按代际拆多个 flavor),让 Kueue 只在**同构**节点里选域——既满足跨节点 rail 对齐的同构前提,又避免 NCCL/CUDA 版本错配。
3. **容量计数准确**:`vgpu-number` 是真实 allocatable 扩展资源(Kueue 直接按域统计);`configuration.yaml` 的 transformation 让 cores/memory 维度也准确。整卡作业仅申请 `vgpu-number`,计数与实际占用一一对应。

> 这三点都是"喂给 Kueue 更准的输入"(节点集、同构性、容量),Kueue 的第一层筛选随之更准;**vgpu-manager 调度器本身无需为此改代码**(标签是 device-plugin 现成发布的)。

## 5. 运行时交互时序

1. 用户提交 `suspend: true` 的 Job(带 queue-name + TAS 注解)。
2. Kueue 校验配额 → TAS 按 `.status.allocatable` 的 `vgpu-number` 统计各 **rack** 余量 → 选能装下 4×8=32 卡的 rack(装不下则按 preferred 回退到 spine)→ 写 `topologyAssignment` → resume Job。
3. Job 创建 4 个 Pod,每个 Pod 被 Kueue 注入 **rack 级** `nodeSelector: topology.example.com/rack=<选中的 rack>`(最细层=rack,**不钉具体节点**)。
4. 每个 Pod 经 `vgpu-scheduler` 调度:NodeSelector 限定到该 rack 的节点集 → **vgpu-manager 按节点策略(binpack/spread + 拓扑 fitness)在 rack 内选最佳节点** → extender 在该节点选具体物理卡(单 Pod link 保证节点内 NVLink 连通;多 Pod/节点时 anchor;跨节点子域对齐见 §7.2)。
5. 全部绑定 → 训练启动,跨节点走同 rack 的 RDMA,节点内走 NVLink。

## 6. 整卡语义建议(TAS 场景重要)

Kueue 的域容量统计只看 `vgpu-number`(`vgpu-cores`/`vgpu-memory` 它不计)。因此 **TAS 训练作业应按"整卡"申请**:`vgpu-number=N` + `vgpu-memory=整卡显存`(或不切分),让 Kueue 的"卡数"统计与实际占用一致。若用分片(多 Pod 共卡),Kueue 仍按 vGPU 槽位数统计——容量层面正确,但分布式训练本不该共卡,建议整卡。可在 vgpu-manager webhook 加校验对"TAS 注解 + 分片"组合告警(可选)。

## 7. 关键问题:vgpu-manager 调度器是否需要额外开发?

**结论:常见场景(整机整卡)不需要额外开发;唯一需要扩展的是"子卡分配 + 单节点多 NVLink 域"下的跨节点子域对齐(§7.2)。** 分三层看"具体设备是否在最佳拓扑域":

| 维度 | 谁保证 | 是否需开发 |
|---|---|---|
| **跨节点:设备所在节点同 rack** | Kueue TAS 选节点,设备随节点继承域 | ❌ 不需要 |
| **节点内:具体卡 NVLink 连通** | vgpu-manager:单 Pod link(整机 1 Pod)/ 跨 Pod anchor(多 Pod 共节点,已落地) | ❌ 已实现 |
| **跨节点子域对齐(rail 对齐)** | 每 Pod 取节点子集(如 4/8)且节点有多个 NVLink 域时,各 Pod 须选**跨节点对应的同一域** | ✅ **已实现(§7.2),由 `cross-pod-topology` 注解开启** |

前两层:Kueue 管到节点、vgpu-manager 管到卡,**整机整卡场景下已完整覆盖,无需新代码**。第三层是 §7.2 要解决的精细问题。

### 7.1 但有一个必须知道的边界:粗粒度 vs 细粒度计数不一致

Kueue 的统计是**节点级、按 vgpu-number 整数**,它**看不到节点内的 NVLink 连通分量、也不精算 per-card 显存/算力碎片**。而 Kueue 用 **NodeSelector 硬钉节点**(无兜底回退)。于是存在一种失配:

> Kueue 按 rack 级总量认为"这个 rack 装得下"→ 注入 rack 级 NodeSelector;但 rack 内每个节点的 8 张卡都分属两个不连通的 4 卡 NVLink 域,Pod 要 8 连通卡时 vgpu-manager 的 **link-strict** 逐个拒绝 → Pod 被限定在该 rack 内、又出不去 → **Pending**。(hostname-最细层时则是钉死到单节点后被拒。)

**缓解(按推荐度)**:
1. **整机整域**:DGX/HGX 单节点 8 卡本就是一个 NVSwitch 域(不分裂)→ 此问题不存在。绝大多数训练机型如此。**⚠️ 该结论依赖 NVSwitch 上的 NVLink 能被探测到**——修复前 `GetNVLink` 只按"远端 BusID == 对端 GPU"匹配,而 NVSwitch 的远端是交换机,导致这类节点被判成 8 个孤岛(此缓解手段随之失效)。已修复,见 `cross_pod_nvlink_topology_design.md` §9.6;需重新部署 device-plugin 才生效。
2. **用非 strict link**(`device-topology-mode: link` 而非 `link-strict`):装不下时 vgpu-manager 降级而非拒绝,避免钉死 Pending。
3. **整卡申请**(§6):让 Kueue 的卡数统计 = 实际占用,消除显存/算力维度的失配。
4. **(可选未来开发,非必须)** 若要硬保证:vgpu-manager 把"节点最大 NVLink 连通分量可用卡数"暴露为节点 label / 扩展资源,并把 NVLink 域建模为 Kueue Topology 的**最细一层**(替代 hostname)。这样 Kueue 的域统计直接以连通分量为单位,失配消失。**这是唯一可能的额外开发项,且仅在"单节点多 NVLink 域 + 要硬保证"时才需要**——属于进阶,默认不做。

### 7.2 跨节点子域对齐(rail 对齐)——子卡分配场景的解法

> **状态(2026-06-24):已实现并泛化为 domain key(Phase A,提交 `53e5486`)。** 由 per-pod 注解 `nvidia.com/cross-pod-topology: "true"` + `device-topology-mode: link` 开启。对齐 key 从"位置 ordinal"泛化为 **domain 签名(string)**:节点发布可选的 GPU→rail 映射(`nvidia.com/node-gpu-domain`)时按**物理 rail-set** 精确对齐(异构正确);**不发布时自动退回位置 ordinal**(同构集群行为不变)。兄弟的子域仍用其 `PodVGPUPreAllocAnnotation` 里的 **UUID** 在兄弟所在节点解析(稳定身份,非可能过时的 device id)。数据源配置见 **§7.2.1**。

**这是和 §7.1 不同的、更精细的问题。** §7.1 是"装不下"(节点 8 卡分两域,Pod 要 8 连通卡 → 拒绝);本节是"**装得下、但选错域**":

> 节点有两个不连通 4 卡域(a / b,通常对应两条 rail / 两组 NVSwitch)。Pod1 在 node1 选了 **a 域**;Pod2 到 node2 时,a、b 两域**都有 4 张空闲卡都装得下**。但若 Pod2 选 node2 的 **b 域**,跨节点 NCCL 会在 rail-a 与 rail-b 之间穿越(经 spine / host bridge),带宽掉档。**最佳应让 Pod2 也选 node2 的 a 域**,使整组 rail 对齐。

**为什么 Kueue 和现有 anchor 都覆盖不了**:
- **Kueue 只到节点/机架粒度**:它注入 rack(或 host)级 `NodeSelector`,**对"节点内是哪个子域"完全无感**——k8s 调度本就是节点粒度,子卡域选择天然是设备分配器(vgpu-manager)的职责。Kueue 无解。
- **现有跨 Pod anchor 只管同节点**:[`GangAnchorComponent`](../pkg/device/types.go) 只扫**本节点** `nodePods`,返回的是 union-find 的**节点本地 root**;跨节点兄弟在别的节点,其 root 在本节点的 union-find 里**无意义**(不同节点的本地索引不可比)。

**解法:把 anchor 从"同节点 root"升级为"跨节点稳定的域签名(domain key)"。** 三个构件,均已落地(Phase A 把签名从纯位置 ordinal 泛化为"有 rail 用 rail-set、无 rail 退回 ordinal"):

1. **稳定域签名(NodeInfo)**:给每个 NVLink 连通分量一个**确定性签名**。有 `node-gpu-domain`(§7.2.1)时 = `"rail:<排序 rail 集>"`(全局可比);无时 = `"ord:N"`,N 按"分量内最小 `Device.Index`"排序(含 GPU index 0 的分量 = `ord:0`)。复用现成 `componentToUUIDs` + `Device.Index`。同构节点上 `ord:k` 每台都指同一条 rail;异构需 rail 数据。

2. **用 UUID 反查兄弟域(无新 per-pod 注解、严谨)**:`PodVGPUPreAllocAnnotation` 编码 `id_uuid_cores_memory`,里面的 `id`(`Device.Index`)可能因驱动重载/枚举变化而过时,**UUID 才是稳定身份**。`PodPreAllocatedUUIDs` 取出并**去重**(多容器可共卡 → UUID 会重复),再用 `DomainOfUUIDs` 解析签名。但跨节点兄弟的 UUID **只在它自己那台节点有效**,所以必须在**兄弟所在节点的 NodeInfo** 上解析(`nvlinkComponentByUUID`→`nvlinkComponentDomain`,多数派)。

3. **跨节点 anchor 解析(携带通用值、消费时节点特化)**:
   - `filter`(`FindGangSiblingDomain`):在 `nodeInfoList` 就绪后,**复用 `nodePodsMap`**(全节点桶,**不额外 List**)找同 gang 已分配兄弟,用其**所在候选节点**的 NodeInfo 解析出域签名 → 写入**单个**节点无关值 `req.GangDomainKey`(默认 `""`)。兄弟节点非本轮候选则跳过(best-effort,对齐是优化不是正确性闸门)。
   - `allocateLink` 解析优先级:**同节点** anchor(`GangAnchorComponent`,UUID,连通是硬约束)> **跨节点** `ComponentByDomain(req.GangDomainKey)`(各节点把同一签名映射到**自己的** root;签名在本节点不存在 → 无匹配,安全跳过)> **首 Pod** 不约束(走现有最优选择)。
   - **首 Pod**:无兄弟 → 现有 allocateLink 选最优集,其分量签名自然成为全组目标(后续兄弟反查它的预分配 UUID 即可对齐),无需显式记录。

**正确性条件与边界**:
- **同构(无 rail 数据,默认回退)**:不发布 `node-gpu-domain` 时,域签名 = 位置 `"ord:N"`,"ordinal=同 rail"**仅在节点 GPU↔rail 物理布局一致时成立**(同构 DGX/HGX 集群即如此,是常态)。异构上 node1 的 `ord:0` 与 node2 的 `ord:0` 可能不是同一条 rail。
- **异构严格对齐(发布 rail 数据,Phase A 已支持消费)**:发布 `node-gpu-domain`(GPU→rail)后,域签名 = `"rail:<排序 rail 集>"`,**全局有意义**——同一组 rail 在任何节点产生相同签名,枚举顺序/布线差异不影响;若某节点根本没有该 rail-set,`ComponentByDomain` 返回无匹配→**安全跳过对齐(不强行错落到错误域)**。数据来源:device-plugin 自动发现(Phase B,未做)或运维/网络组件声明(§7.2.1)。
- **整机整卡直接绕过**:每 Pod 取满 8 卡时,两条 rail 都用上,NCCL 按 GPU 各走各 rail 自动对齐,**没有子域选择问题** → 本节整套机制都不需要。**这是最强且零成本的规避**。
- **best-effort / strict**:目标 ordinal 的分量空闲卡不足时,非 strict 降级到其它域(回到次优但能跑),strict 拒绝节点。
- **首 Pod 选域**:任意一致的 ordinal 都能对齐;若还想选**最大**域(容量),首 Pod 应挑最大分量——这点与已暂缓的 Lookahead 相关。

**落地定位**:跨 Pod anchor 从同节点扩展到跨节点,复用 `nodePodsMap` + 兄弟预分配注解里的 **UUID**,**不额外 List、不引入 CRD**;rail 数据走**可选** `node-gpu-domain` 节点注解(无则回退 ordinal)。改动:`types.go`(`buildComponentDomains` 出 ordinal+domain 双映射、`ComponentByDomain`/`DomainOfUUIDs`、gather 解析 rail)、`request.go`(`GangDomainKey`)、`filter_predicate.go`(`FindGangSiblingDomain`)、`allocator.go`(对齐分支)、`consts.go`(`NodeGPUDomainAnnotation`)。详见 [`cross_pod_nvlink_topology_design.md`](./cross_pod_nvlink_topology_design.md) §5.5。

**建议**:
- 默认/常见:**整机整卡**,本节不需要任何开发,也不用配 rail。
- 同构集群 + 子卡跨节点对齐:开 `cross-pod-topology` 即可,**走 ordinal 回退**,无需 rail 数据。
- 异构集群 + 子卡跨节点对齐:**额外发布 `node-gpu-domain`(§7.2.1)**,按 rail-set 精确对齐。
- 所以对"调度器是否需要额外开发"的完整回答:**整机整卡=否;子卡多域对齐=是(已落地 opt-in 的 domain-anchor);异构精确=再发布一份 rail 节点注解,无需改调度器。**

### 7.2.1 数据源(source②):节点注解 `node-gpu-domain` 配置详解

> 仅当你的集群是**异构布线**(各节点 GPU↔rail 物理布局不一致,例如枚举顺序反转、不同代际混布)且要做**子卡跨节点对齐**时才需要。**同构集群什么都不用配**,调度器自动用位置 ordinal。

**注解键**:`nvidia.com/node-gpu-domain`(`util.NodeGPUDomainAnnotation`),打在 **Node 对象**上。

**取值**:一个 JSON 对象,把**本节点每张 GPU 的 UUID** 映射到一个 **rail key 字符串**:

```json
{
  "GPU-3a1b...": "rail-0",
  "GPU-7c2d...": "rail-1",
  "GPU-9e4f...": "rail-2",
  "GPU-5b8a...": "rail-3",
  "GPU-1f6c...": "rail-4",
  "GPU-2d9e...": "rail-5",
  "GPU-8a3b...": "rail-6",
  "GPU-4c7d...": "rail-7"
}
```

设置示例:
```bash
kubectl annotate node <node> \
  'nvidia.com/node-gpu-domain={"GPU-3a1b...":"rail-0","GPU-7c2d...":"rail-1", ... }'
```

**rail key 的语义(最关键)**:它必须是**全局可比的物理子域标识**——

- **跨节点一致性是唯一要求**:连到**同一条物理 rail / 同一个 leaf 交换机**的 GPU,在**每台节点**都必须写**相同的 rail key**。`rail-0` 在 node1 和 node2 必须指同一条 rail。
- **节点内只需区分不同 rail**:同节点不同 rail 用不同 key 即可。
- 调度器**不解析 key 的内容**,只做字符串相等比较:一个 NVLink 岛的"域签名" = 其成员 GPU 的 rail key **排序去重后拼接**(`rail:rail-0,rail-1,rail-2,rail-3`)。两个岛"同子域" ⟺ **rail-set 签名相等**。

**key 怎么取(任选其一,只要全局一致)**:
- **leaf 交换机 GUID / 名称**:最严谨,直接用 GPU 所连 NIC 上联交换机的 GUID(`rail-<switch-guid>`)。
- **rail 序号约定**:rail-optimized 集群通常 GPU_i→NIC_i→leaf_i,直接用 `rail-<i>`(i = 该 GPU 在 rail 平面里的序号,**按物理 rail 而非本地 GPU index**)。
- **IB**:`<子网前缀>-<端口/LID 推导的 rail>`。

**全 or 无(重要约束)**:注解必须**覆盖本节点每一张 GPU**。只要有一张 GPU 缺 rail key(或注解缺失/JSON 非法),该节点**整体回退**到位置 ordinal(`ord:N`),不会"部分 rail 部分 ordinal"——保证一个节点的所有岛在**同一基准**上比较。

**生效条件**:还需 `device-topology-mode: link`(否则没有 NVLink 岛的概念)。注解在 `NewNodeInfo` 解析拓扑时一并读取(仅 `gpuTopologyEnabled` 时)。

**谁来写这个注解**:
- **运维/网络组件(现在就能用)**:同型号节点布线一致 → **按硬件 flavor 写一份 `index→rail` 映射**,用脚本/控制器展开成各节点的 `uuid→rail`(UUID 从 `node-device-register` 注解读)。也可直接消费已有 network-operator / Topograph 打的 rail 标签转译。
- **device-plugin 自动(Phase B,未做)**:自动发现 GPU→NIC(PCIe sysfs)→ rail(IB SM / LLDP),写**同一个注解**。届时消费侧零改动。
- **优先级**:运维声明与自动发现写同一键时以实际写入为准;如需运维覆盖自动值,可约定一个 override 注解(尚未实现,需要再加)。

**验证生效**:
```bash
# 看注解是否在
kubectl get node <node> -o jsonpath='{.metadata.annotations.nvidia\.com/node-gpu-domain}'
# 调度日志(V>=4)里跨节点对齐分支会用 domain 签名;
# 也可在 e2e 测试 Test_CrossPod_NVLink_HeterogeneousRail 看到 "rail:rA,rB,rC,rD" 形态。
```

**异构正确性回顾**(为什么 rail key 比 ordinal 强):node1 岛a 与 node2 岛b 若物理同 rail,二者 rail-set 签名相同 → 对齐到正确的岛;**位置 ordinal 会因 index 位置不同而错配**。详见 `cross_pod_nvlink_topology_design.md` §9.5/§5.5 与 `Test_CrossPod_NVLink_HeterogeneousRail`。

### 7.3 其它兼容性注意

- **schedulerName**:Pod 必须用 `vgpu-scheduler`;Kueue 不改 schedulerName,仅注入 NodeSelector,二者不冲突。
- **webhook**:vgpu-manager 的 `fixSpecifiedNodeName` 仅在 `spec.nodeName` 已设时动作;Kueue 用 NodeSelector(rack/host 标签)而非 `spec.nodeName`,且 webhook 是 append 不覆盖 → 不冲突。
- **抢占**:Kueue 有自己的 Workload 级抢占;vgpu-manager extender 的 preempt 仍在节点内生效。两者层级不同(Workload vs 设备),注意配额/优先级策略避免互相打架(建议训练作业走 Kueue 配额抢占,关闭或弱化 extender 抢占对 Kueue 管理 Pod 的干预)。
- **Gang 死锁**:`required-topology` + 配额不足会一直 Pending;靠 Kueue 的 `preemption` + `borrowing` 缓解,不要把 nominalQuota 设得过满。

## 8. 验证

```bash
# 1. 拓扑 label 就位
kubectl get nodes -L topology.example.com/spine -L topology.example.com/rack

# 2. Kueue 准入与拓扑分配
kubectl get workload -n default
kubectl get workload <wl> -o jsonpath='{.status.admission.podSetAssignments[*].topologyAssignment}'

# 3. Pod 确实被钉到同 rack 节点 + 落到了具体卡
kubectl get pods -o wide                          # 看 4 个 Pod 的节点是否同 rack
kubectl get pod <p> -o jsonpath='{.metadata.annotations.nvidia\.com/pre-allocated}'  # extender 选的卡

# 4. NCCL 实测:DEBUG=INFO 应显示节点内 NVLink、跨节点 IB/GDRDMA;非 TCP
#    nccl-tests all_reduce 带宽对比理论值
```

## 9. 适用边界与替代

- **适合**:已用或愿引入 Kueue 做配额/队列的环境;要 gang + 跨节点 rack 亲和但**不想自研、想保留 extender**。这是对 vgpu-manager 最友好的跨节点方案(共存成本最低)。
- **不适合 / 需评估**:超大集群(Kueue TAS 会跟踪全量 Pod+Node,内存与调度延迟上升,见官方 drawback);需要 GB200 跨节点 NVLink(那是 DRA ComputeDomain 路线,见 multinode 文档 §7)。
- **不引入 Kueue 时**:跨节点亲和退回 multinode 文档的"node label + webhook podAffinity"轻量方案(best-effort,无 gang 硬保证)。

## 10. 三句话总结

1. **Kueue TAS 管跨节点 rack 域 + gang 准入;vgpu-manager extender 管节点内具体 NVLink 卡。** 二者正交,Kueue 不替换调度器,extender 原样存活。
2. **能配合的命门已验证**:`nvidia.com/vgpu-number` 是真实 allocatable 扩展资源,Kueue 能按域统计。
3. **常见场景(整机整卡)零额外开发**;两个边界都源于"Kueue 节点级计数看不到节点内 NVLink 域":§7.1 装不下(用整机整域 + 非 strict link + 整卡申请规避);§7.2 子卡分配时的跨节点子域对齐**已实现**(跨节点稳定**域签名**,用兄弟 UUID 在其所在节点解析、去重,携带单值、消费时各节点特化,不加 CRD;同构走 ordinal 回退、异构发布可选 `node-gpu-domain` 节点注解走 rail-set 精确对齐,见 §7.2.1)。整机整卡可同时绕过这两个边界。

---

**Sources**:
- [Kueue: Topology Aware Scheduling (concepts)](https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/)
- [Kueue: Run Workloads with Topology-Aware Scheduling (task)](https://kueue.sigs.k8s.io/docs/tasks/run/topology_aware_scheduling/)
- [Kueue: Labels and Annotations](https://kueue.sigs.k8s.io/docs/reference/labels-and-annotations/)
- [GKE: Schedule workloads with TAS](https://docs.cloud.google.com/ai-hypercomputer/docs/workloads/schedule-gke-workloads-tas)
