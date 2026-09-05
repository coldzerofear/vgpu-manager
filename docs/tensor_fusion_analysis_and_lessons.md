# tensor-fusion 源码分析与对本项目远程 GPU 管理/调度的借鉴

> 状态：分析记录（2026-08-21）。分析对象：本地 `tensor-fusion/`（Apache-2.0，Go，operator + 自定义调度器 +
> 多 CRD）。它是 gpu-go（`docs/gpu_go_analysis_and_lessons.md`）背后的**开源控制面全貌**——gpu-go 是其客户端，
> tensor-fusion 是其 operator/scheduler/CRD。
> 对照对象：`docs/remote_gpu_k8s_integration_design.md`（本项目 k8s 接入 v1.5.2）。
> 结论先行：**它的远程 GPU 架构与我们 D13 的判断高度吻合，且在几处我们标注"待定/后置"的地方给出了已验证的成熟解法。**
> 三条最值得吸收：①端点交付用**长轮询 HTTP + TokenReview 鉴权**（比我们的"令牌进 env / claim status"更强）；
> ②调度是**GPU 宿主中心**的、无跨节点可达图（印证我们的 endpoint 是 IP+port 而非拓扑）；③分配正确性靠
> **内存权威 store + Assume/Commit/Rollback 乐观账本 + TTL 清扫**。

## 1. 架构总览（对照我们的三平面）

| 平面 | tensor-fusion | 本项目对应 |
|---|---|---|
| 控制面 | operator（多 controller）+ **自定义 in-tree 调度器插件** `GPUResourcesFit` | helm/operator + DRA allocator（或 extender） |
| GPU 主机 | worker pod（`ComponentWorker`，跑 `tensor-fusion-worker -p <port>`）+ 每节点 hypervisor（运行时限额权威） | lupine-server + libvgpu-control（会话记账/限速） |
| 消费侧 | client pod：webhook 注入 client 库 + 连接 env；shim 长轮询取 endpoint | 消费 pod：kubelet-plugin 注入 lupine-client + `LUPINE_SERVER/SESSION` |

**关键澄清（重构了我此前对"远程"的理解）**：tensor-fusion **从不把 A 节点的 GPU 调度给 B 节点的 pod**。
worker 永远与其物理 GPU **同节点**；"远程 GPU"= **client pod 与 worker pod 解耦**，client 跑在任意节点、
经 TCP 连到 GPU 主机上的 worker。所以调度器里**没有跨节点 GPU 可达图，也不需要**（`gpuresources.go` 的
`Filter` 拒绝任何不在 `NodeGPUs` 的节点；`SameNodeFilter` 强制多卡同节点）。这正是我们设计 §6.8/§1.6 的判断
——远程可达性是 IP+port 层面的事，不进调度器。

## 2. 远程 GPU 的对象模型（对照我们 D13/D7）

```
WorkloadProfileSpec.IsLocalGPU  →  拓扑开关
  false（默认，远程）: client 任意节点调度 + worker 另起，TCP 连
  true（本地）      : client 直接 GPU 调度，embedded worker 走共享内存
  SidecarWorker    : worker 同 pod，shmem

TensorFusionWorkload（= WorkloadProfileSpec）  → 拥有/创建 worker pod
   └─ Replicas == nil  →  动态：★1:1 专属 worker/连接（worker 名 = 连接名，连接 owner）
   └─ Replicas 固定    →  N:1 共享池（least-connections 负载均衡，maxSkew=1）
TensorFusionConnection{Spec:{WorkloadName,ClientPod}, Status:{Phase,ConnectionURL,WorkerName}}
   = 把 client pod 绑到某 worker 的 join 对象；endpoint 在 status
```

**与我们的对应关系**：
- 它的 `Replicas==nil → 1:1 专属` vs `Replicas 固定 → N:1 共享池`，正是我们 D13 的 **1:1 / 1:N 两拓扑**——
  而且它证明了**两者可以藏在同一 CRD 后面、由一个字段隐式选择**，与我们"`RemoteGPUServer` 原语 + `RemoteGPUPool`
  编排"（§1.6.7）殊途同归。它们把选择放在 `Replicas` 是否为 nil；我们放在"手工建 Server vs Pool 生成"。
- 但它的 worker **始终与 GPU 同节点**（worker 就是"在 GPU 上开一个转发进程"），而我们的 lupine-server 也天然如此。
  两边一致：远程性只在 client 侧。

## 3. 三条最值得吸收的做法

### 3.1 端点交付 = 长轮询 HTTP + TokenReview 鉴权（分析；⚠️ 我们不能直接照搬）

> **约束更正（2026-08-21，见 k8s 设计 v1.6.1 §5.1）**：下面是 tensor-fusion 的做法与其价值分析，但**它需要
> 改 lupine client**（让 shim 主动拿 pod SA token HTTP 调控制面），违反我们"不改 lupine 源码"约束。**我们采用
> 的是约束内等价方案**：端点走**域名注入 + 控制面 DNS**（被动可解析，不需 client 回调），鉴权走**签发时绑定 pod
> + provider `restore()` 服务端校验**（`LUPINE_SESSION` 是唯一不改 lupine 的通道，provider 是我们的代码）。
> 就绪屏障回到 D2。本节保留原始分析作对照。

tensor-fusion **不把 endpoint 塞进 env / claim status 让消费者被动读**，而是：
- webhook 给 client 容器注入 `TENSOR_FUSION_OPERATOR_GET_CONNECTION_URL`（指向 operator 的
  `GET /api/connection?name=..&namespace=..`）+ 连接名/命名空间；
- client shim 启动后调该端点；operator 的 `ConnectionRouter.Get` **阻塞（长轮询，5 分钟超时）直到该连接
  `Status.Phase==WorkerRunning`**，再返回 `ConnectionURL`；返回前先订阅 k8s watch 以免错过 Running 事件。
- **鉴权**：该端点用 **k8s TokenReview** 校验调用者 SA token，并把 token 的 pod UID 与连接的
  `OwnerReferences[0].UID` 比对（`connection.go:210-265`）——**一个 pod 只能取到属于自己的 GPU 端点**，
  外加 LRU token 缓存。

对我们的意义（**这解决了我们两个"待定/较弱"点**）：
1. **同时是就绪屏障**：消费者进程在 worker Running 前拿不到可用 endpoint（长轮询挂着），无需 k8s init 容器
   gate。我们 D2 用的是"NodePrepare 内同步 EnsureSession"——长轮询是**另一种等价屏障**，且对 1:1"endpoint
   分配后才有"的场景更自然（§1.6.5 我们让 operator 回填 CR status，消费侧仍要等；长轮询把"等"收进一次 HTTP 调用）。
2. **鉴权模型比我们强**：我们现在是"控制面签发随机令牌进 pod env"（D8），泄露面 = env。tensor-fusion 的
   TokenReview + ownerUID 比对**不依赖任何共享秘密**——pod 用自己的 SA token 证明身份，operator 查 owner 关系。
   建议把它**列为我们令牌方案的强化选项**（尤其多租户/明文链路下，见我们 §6.1 令牌明文泄露缺口）。
3. **late binding + 重连**：endpoint 不写死在 env，worker 换 IP/重启后 shim 重新长轮询即可拿到新地址——
   与我们"server 重启会话不可恢复、应用重连"的现状契合，且给了一个干净的重连拉取点。
- **`ConnectionURL` 内嵌 `resourceVersion`**（`native+<podIP>+8000+<name>-<rv>`）：client 用它检测 worker
  重启/换代，触发重连。这是个便宜的 staleness 信号，我们的注入面可以照抄（endpoint 带一个 generation）。

### 3.2 调度正确性 = 内存权威 store + 乐观 Assume/Commit/Rollback 账本（借鉴给 extender 兼容路径）

- **真相源是 operator/scheduler 进程内的内存 `gpuStore`**，周期性 flush 到 `GPU` CRD 的 `.status.available`
  （`SyncGPUsToK8s`，dirty-queue 驱动）。CRD 是持久/可观测副本，内存 store 是调度期权威。
- **两阶段乐观分配**：`Reserve→Assume`（记账到 `assumedAllocation`，不动已提交可用量，后续周期在副本上叠加）
  → `PreBind→Commit`（原子临界区，真正扣减，最多重试 3 次，失败 `Rollback`）→ `PostBind→NotifyBound`。
- **陈旧预留安全**：`assumedAllocationTTL` + `sweepStaleAssumedAllocationsLocked` 回收 operator 在
  Reserve/PreBind 间崩溃留下的孤儿预留（gang-aware，不误扫等待中的 gang）。
- 不变量：**没有 pod 在缺少有效 `gpu-device-ids` annotation 时到达 Bind**；分配提交与 pod annotation patch
  要么都成功要么都回滚。

对我们的意义：**这正是我们设计 §3 里"extender 兼容路径要自建记账/恢复/回收"点名的那套小型 allocator**。
DRA 主路径我们靠原生 allocator 免掉了它；但 K3 的 extender 路径若真要做，tensor-fusion 的
Assume/Commit/Rollback + TTL 清扫 + 内存权威/CRD 持久分离，是一份**可直接照搬的成熟蓝图**。记为 extender
路径的实现参考。

### 3.3 一 worker 一端口 = 每节点位图端口分配器（1:1 落地的现成件）

`internal/portallocator/portallocator.go`：位图分配器，两个 scope——**每节点**（`AssignHostPort(nodeName)`，
`PortRangeStartNode..EndNode`，给每个 worker 一个**该节点上唯一的 hostPort**）+ **集群级**（`host-port: auto` 标签）。
worker 因此可寻址为 `nodeIP:hostPort`；端口在 pod 确认删除后才惰性释放（`releaseNodePortUntilPodDeleted`）；
leader 从存活 pod 重建位图（重启恢复）。配套 `indexallocator` 给每 worker 一个每节点隔离槽 index。

对我们的意义：**这填上了我们 D13 §1.6.4 "1:1 下 N 个端口要分配与放行"那句话的具体机制**。我们此前只说"端口分配是
代价"，tensor-fusion 给了完整实现：每节点位图 + 惰性释放 + leader 重建。如果我们上 1:1（operator 阶段的
`RemoteGPUServer`），端口分配器直接照这个模式做。注意它是 **hostPort**，不是 ClusterIP——印证我们 hostNetwork/
underlay 优先的判断（§6/§1.6.4）。

## 4. 其余可借鉴点（按价值排序）

| 点 | tensor-fusion 做法 | 对我们 |
|---|---|---|
| **限额出带外交付** | 软隔离：limits 不进 env，`libcuda_limiter.so`（LD_PRELOAD）从 **hypervisor 写的共享内存**读实时 limits；硬隔离：绝对值进 env（`HardSMLimiterEnv`/`HardMemLimiterEnv`）。native GPU limits 从 client 容器**剥离** | 与我们"配额进会话目录、不进 pod env"（服务端权威）同构；他们的**共享内存热更 limits** 比我们 seqlock 文件热更是另一种实现，可对比 |
| **多维配额** | `GPUResourceQuota`：`Total`（命名空间级 requests/limits/`MaxWorkers`/告警阈值）+ `Single`（每 workload max/default）；乐观 shadow 记账，PreFilter+Assume 两次校验 | 我们设计**完全没有配额系统**——多租户上生产前需要。这是一份现成的维度设计（命名空间总量 + 单 workload 上限 + 默认值 + 告警阈值） |
| **超卖/VRAM 扩展** | 池级 `Oversubscription`：TFlops 超卖 500%、VRAM 扩到主机内存 50%/磁盘 70%；只膨胀**节点级虚拟容量**，per-GPU fit 仍用真实 available | 我们 Phase1 明确禁用超卖；将来若做，"虚拟容量只影响节点级记账、真实 fit 用物理量"这个**分层**值得抄，避免超卖污染硬隔离判定 |
| **QoS + 单卡多进程公平** | `SchedulingConfigTemplate` 带 hypervisor QoS 调度（单 GPU 多进程排队公平）、垂直伸缩、cron 伸缩、rebalancer | 我们的共享令牌桶只做限速不做 QoS 优先级；跨会话公平/优先级是未来方向，这里有参考 |
| **GPU 隔离模式锁** | `GPU.Status`：`IsolationPolicy`(Static/Dynamic)、`ActiveIsolationMode`(shared/soft/hard 锁)、`DynamicIsolationConflict`（多模式冲突→fail-closed） | 一张物理卡被多租户切分时，**锁定隔离模式 + 冲突 fail-closed** 是我们没有的安全保护；1:N 单卡多会话若混用不同隔离强度，值得引入类似锁 |
| **gang 调度** | `GangSchedulingConfig` + 调度器 Permit 阶段 all-or-nothing | 多卡分布式训练场景需要；我们暂无 |
| **client 库注入** | webhook + **init 容器拷贝到 EmptyDir** + LD_PRELOAD/`ld.so.preload`（SubPath 挂载）+ 池级 `PatchToPod`/`PatchToContainer` 逃生门 | 与我们 D12"init 容器铺制品到 hostPath"同族；他们用 **per-pod EmptyDir**（我们 §4.4 否掉了 per-pod 拷贝，因为准入先于分配无法选版本）——差异根因：他们 client 库**不按 CUDA 版本分**（见 gpu-go 分析 §3.4），所以能 per-pod 拷固定制品；我们按版本分，必须节点级预铺。**这条差异再次印证：推动 lupine wire 与 CUDA 头解耦能让我们也退化到更简单的 per-pod 模型** |

## 5. 明确不适用 / 与我们不同的取舍

- **它不做 GPU-over-IP 的数据面**（那是 gpu-go 闭源 worker 的事）；调度器只管"worker 落在哪张 GPU 上"，
  数据面转发是 worker 内部。我们的数据面是 lupine（开源），职责边界不同。
- **网络类型（IB/RoCE/Ethernet）不进调度**：拓扑插件只建模**节点内** GPU 互联层级（NVLink/NUMA tier），
  跨节点 fabric 完全不建模（`LinkType`/`Bandwidth` 仅 observability）。**这是它的一个缺口**：想让 client 优先
  靠近其 remote worker 的快速 fabric，它做不到。我们的 `net-zone` 标签（§6）反而在这点上更前一步——记为
  我们相对它的一个领先点，但也提醒我们 zone 只是粗粒度可达性，真要做 fabric 亲和仍需扩展。
- **endpoint 走 Pod-IP:8000 overlay**（不是 hostPort/hostNetwork），代码 TODO 才提到 hostNetwork/IB 优化。
  我们从一开始就定 hostNetwork/underlay 优先（§6.1/§1.6.4）——在高性能网络这点上我们的默认更激进/更对。

## 6. 对设计文档的具体更新（已同步）

见 `docs/remote_gpu_k8s_integration_design.md` v1.6 的 D15/D16/D17 与 §5.1/§10：
- **D15（v1.6.1 修订）**：长轮询+TokenReview 需改 lupine client，**作废**；改为域名注入+控制面 DNS + 签发时绑定
  +provider `restore()` 服务端校验（§5.1）。
- **D18**：远程 GPU 控制面**不新开仓库**，留在 vgpu-manager（复用 DRA/informer/webhook 底座，避免重造 Go 双树）。
- **D16**：extender 兼容路径的自建记账，采用 Assume/Commit/Rollback + TTL 清扫蓝图（§3 补注）。
- **D17**：多租户前需引入配额系统（借 `GPUResourceQuota` 的 Total/Single 双维度）+ 单卡隔离模式锁；记入 §10 待办。
