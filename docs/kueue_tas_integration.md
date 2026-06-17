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
  + 注解 kueue.x-k8s.io/podset-required-topology: <rack 标签>
  + requests nvidia.com/vgpu-number: N
        │
        ▼  ① 跨节点域选择 + Gang 准入        ← Kueue TAS(L2/L3)
Kueue
  - ClusterQueue/LocalQueue 配额校验 + 整个 Workload 原子准入(gang)
  - TAS:按 node .status.allocatable 统计各拓扑域的 vgpu-number 余量,
         选出能装下整组的最低层域(rack);算出 pod→node 固定分配,
         给每个 Pod 注入 NodeSelector(hostname),记录于
         status.admission.podSetAssignments[].topologyAssignment
        │
        ▼  ② 节点确定后,在该节点选具体 GPU   ← kube-scheduler + vgpu-manager extender(L0/L1)
kube-scheduler(profile: vgpu-scheduler)
  - NodeSelector 把每个 Pod 限定到 Kueue 指定节点
  - 基础 PodFitsResources 计 vgpu-number;调用 vgpu-manager extender
        │
        ▼
vgpu-manager extender
  - 在该节点上选具体物理卡(节点内拓扑):
    · 单 Pod 整机多卡 → 现有单 Pod link 拓扑
    · 多 Pod 共节点   → 跨 Pod NVLink anchor(已落地,feature gate CrossPodLinkTopology)
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
# 示例:两层 rack + host(host 用内置 hostname)
kubectl label node gpu-node-1 topology.example.com/rack=rack-a
kubectl label node gpu-node-2 topology.example.com/rack=rack-a
kubectl label node gpu-node-3 topology.example.com/rack=rack-b
# kubernetes.io/hostname 由 K8s 自动注入,无需手工
```

### 4.2 Topology CRD(层级从粗到细,最细一层用 hostname)

```yaml
apiVersion: kueue.x-k8s.io/v1beta1
kind: Topology
metadata:
  name: gpu-topology
spec:
  levels:
  - nodeLabel: topology.example.com/rack   # 粗:同 rack
  - nodeLabel: kubernetes.io/hostname      # 细:同节点(必须是最后一层,TAS 才能定到具体节点)
```

### 4.3 ResourceFlavor 绑定 Topology + 选 GPU 节点

```yaml
apiVersion: kueue.x-k8s.io/v1beta1
kind: ResourceFlavor
metadata:
  name: gpu-flavor
spec:
  topologyName: gpu-topology               # ← 不设此字段则该 flavor 不启用 TAS
  nodeLabels:
    node-role.kubernetes.io/gpu: "true"    # 选中 vgpu-manager 管理的 GPU 节点
```

### 4.4 ClusterQueue / LocalQueue(把 vgpu-number 纳入配额)

```yaml
apiVersion: kueue.x-k8s.io/v1beta1
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
apiVersion: kueue.x-k8s.io/v1beta1
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
        kueue.x-k8s.io/podset-required-topology: topology.example.com/rack  # ② 硬约束:整组同 rack
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
      # 如需"软"亲和改用 podset-preferred-topology;允许跨 rack 兜底用 podset-unconstrained-topology
```

**注解选择**:
- `podset-required-topology: <rack>` —— **硬**:装不下就一直等(配合 gang,适合性能敏感训练)。
- `podset-preferred-topology: <rack>` —— **软**:优先同 rack,装不下逐级向上(spine→更高),最终可跨域。
- `podset-unconstrained-topology: "true"` —— 不约束拓扑,仅看容量(最小碎片)。
- 多 PodSet(如 LWS 的 leader+worker)要求同域:`podset-group-name`。

## 5. 运行时交互时序

1. 用户提交 `suspend: true` 的 Job(带 queue-name + TAS 注解)。
2. Kueue 校验配额 → TAS 按 `.status.allocatable` 的 `vgpu-number` 统计各 rack 余量 → 选能装下 4×8=32 卡的 rack → 算出 4 个 Pod 落到哪 4 台节点 → 写 `topologyAssignment` → resume Job。
3. Job 创建 4 个 Pod,每个 Pod 被 Kueue 注入 `nodeSelector: kubernetes.io/hostname=<分配的节点>`。
4. 每个 Pod 经 `vgpu-scheduler` 调度:NodeSelector 限定到指定节点 → extender 在该节点选 8 张物理卡(单 Pod link 拓扑保证 NVLink 连通)。
5. 全部绑定 → 训练启动,跨节点走同 rack 的 RDMA,节点内走 NVLink。

## 6. 整卡语义建议(TAS 场景重要)

Kueue 的域容量统计只看 `vgpu-number`(`vgpu-cores`/`vgpu-memory` 它不计)。因此 **TAS 训练作业应按"整卡"申请**:`vgpu-number=N` + `vgpu-memory=整卡显存`(或不切分),让 Kueue 的"卡数"统计与实际占用一致。若用分片(多 Pod 共卡),Kueue 仍按 vGPU 槽位数统计——容量层面正确,但分布式训练本不该共卡,建议整卡。可在 vgpu-manager webhook 加校验对"TAS 注解 + 分片"组合告警(可选)。

## 7. 关键问题:vgpu-manager 调度器是否需要额外开发?

**结论:常见场景下不需要额外开发。** 分两层看"具体设备是否在最佳拓扑域":

| 维度 | 谁保证 | 是否需开发 |
|---|---|---|
| **跨节点(设备所在节点同 rack)** | Kueue TAS 选节点,设备随节点继承域 | ❌ 不需要 |
| **节点内(具体卡 NVLink 连通)** | vgpu-manager:单 Pod link(整机 1 Pod)/ 跨 Pod anchor(多 Pod 共节点,已落地) | ❌ 已实现 |

所以**"跨节点 pod 组确保分配的具体设备处于最佳拓扑域"这件事,Kueue 管到节点、vgpu-manager 管到卡,两者已完整覆盖,无需新代码。**

### 7.1 但有一个必须知道的边界:粗粒度 vs 细粒度计数不一致

Kueue 的统计是**节点级、按 vgpu-number 整数**,它**看不到节点内的 NVLink 连通分量、也不精算 per-card 显存/算力碎片**。而 Kueue 用 **NodeSelector 硬钉节点**(无兜底回退)。于是存在一种失配:

> Kueue 认为某节点"有 8 张空闲卡"→ 把 Pod 钉上去;但这 8 张分属两个不连通的 4 卡 NVLink 域,vgpu-manager 的 **link-strict** 会拒绝 → Pod 被 NodeSelector 钉死、无法换节点 → **Pending**。

**缓解(按推荐度)**:
1. **整机整域**:DGX/HGX 单节点 8 卡本就是一个 NVSwitch 域(不分裂)→ 此问题不存在。绝大多数训练机型如此。
2. **用非 strict link**(`device-topology-mode: link` 而非 `link-strict`):装不下时 vgpu-manager 降级而非拒绝,避免钉死 Pending。
3. **整卡申请**(§6):让 Kueue 的卡数统计 = 实际占用,消除显存/算力维度的失配。
4. **(可选未来开发,非必须)** 若要硬保证:vgpu-manager 把"节点最大 NVLink 连通分量可用卡数"暴露为节点 label / 扩展资源,并把 NVLink 域建模为 Kueue Topology 的**最细一层**(替代 hostname)。这样 Kueue 的域统计直接以连通分量为单位,失配消失。**这是唯一可能的额外开发项,且仅在"单节点多 NVLink 域 + 要硬保证"时才需要**——属于进阶,默认不做。

### 7.2 其它兼容性注意

- **schedulerName**:Pod 必须用 `vgpu-scheduler`;Kueue 不改 schedulerName,仅注入 NodeSelector,二者不冲突。
- **webhook**:vgpu-manager 的 `fixSpecifiedNodeName` 仅在 `spec.nodeName` 已设时动作;Kueue 用 NodeSelector(hostname)而非 nodeName,且 webhook 是 append 不覆盖 → 不冲突。
- **抢占**:Kueue 有自己的 Workload 级抢占;vgpu-manager extender 的 preempt 仍在节点内生效。两者层级不同(Workload vs 设备),注意配额/优先级策略避免互相打架(建议训练作业走 Kueue 配额抢占,关闭或弱化 extender 抢占对 Kueue 管理 Pod 的干预)。
- **Gang 死锁**:`required-topology` + 配额不足会一直 Pending;靠 Kueue 的 `preemption` + `borrowing` 缓解,不要把 nominalQuota 设得过满。

## 8. 验证

```bash
# 1. 拓扑 label 就位
kubectl get nodes -L topology.example.com/rack -L kubernetes.io/hostname

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
3. **常见场景(整机整卡)零额外开发**;唯一边界是 Kueue 节点级计数看不到节点内 NVLink 分裂,用"整机整域 + 非 strict link + 整卡申请"规避,硬保证才需要把 NVLink 域建模为 Kueue 最细拓扑层(可选进阶)。

---

**Sources**:
- [Kueue: Topology Aware Scheduling (concepts)](https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/)
- [Kueue: Run Workloads with Topology-Aware Scheduling (task)](https://kueue.sigs.k8s.io/docs/tasks/run/topology_aware_scheduling/)
- [Kueue: Labels and Annotations](https://kueue.sigs.k8s.io/docs/reference/labels-and-annotations/)
- [GKE: Schedule workloads with TAS](https://docs.cloud.google.com/ai-hypercomputer/docs/workloads/schedule-gke-workloads-tas)
