# 远程 GPU 的 k8s 控制面接入设计（上报/调度/分配/注入）v2.0

> 状态：**设计定稿（二十五项关键决策已确认），实施中（S1/agent/分区会话代码已落）**
> v2.2（2026-08-27）：**D8 修订——会话 = 分区，令牌随机并存于 claim annotation**；**D7 修订——独立 chart `vgpu-manager-remote-dra-driver`**（dra-driver chart 改动已回滚）；只允许 VGPUSupport+RemoteGPUSupport 同开；webhook/monitor 适配方案见 §11。
> v2.1（2026-08-25，用户提出）：**gate 开的 GPU 节点只发布不本地分配**，本节点消费者也统一走远程路径（D23 修订）；`--remote-node-selector` 取代固定 net-zone 标签（D23）。
> v2.0（2026-08-25，用户提出、评审采纳的**架构修订**）：**不再有独立的远程资源池**。GPU 节点的既有设备
> （VGPUSupport 开 = `type=vgpu`，关 = `type=gpu`）在 `RemoteGPUSupport` gate 开启时**原地多发**
> `accessMode=remote`/`endpoint`/`netZone` 属性，pool 节点范围由 `nodeName` 放宽为 nodeSelector（本节点 OR 可达节点）；
> pod 落 GPU 节点走既有本地路径、落可达节点走远程注入，**同一设备只有一份 DRA 记账**。agent 与 lupine-server
> 同 pod 两容器、ready 文件排序、kubelet-plugin 独立部署可单独升级；DNS 组件独立部署后置。
> 新增 D23/D24/D25，§0.1 给出对既有章节的覆盖说明；D1/D7/D10/§1/§1.5/§2 中与"独立远程池"相关的表述**以 §0.1 为准**。
> v1.7（2026-08-25，用户拍板四项实施边界）：**D20 EnsureSession 走 gRPC**（沿用 `pkg/api` proto 惯例）；
> **D21 角色开关 = `--mode` + `RemoteGPUSupport` feature gate 两层正交**；**D7 再修订：独立
> `vgpu-manager-remote-gpu` chart**（依赖 dra-driver chart，不并入）；**实施采取 spike 先行**——K1 工程化之前
> 先跑 S0（纯 YAML 调度链路验证）与 S1（最小 inject 注入链路 + 手工会话落盘），见 §8.1。
> v1.6.2（2026-08-24）：新增 **D19（集群外接入的中继边界）**——`publish: false` 的集群外消费不得直连共享 server
> 端口，须经每会话的单租户中继 pod 终止外部传输、校验令牌、补 TLS（§7.2）；§1.6.7 `publish: false` 交叉引用补齐。
> v1.6.1（2026-08-21）：**约束"不改 lupine 源码"下修订 D15**——长轮询+TokenReview 作废（需改 client），改为
> 域名注入+控制面 DNS + 签发时绑定+provider restore() 服务端校验（§5.1 重写）；新增 D18（代码组织：不新开仓库）。
> v1.6（2026-08-21）：tensor-fusion 对照分析（`docs/tensor_fusion_analysis_and_lessons.md`）引入 D15/D16/D17。
> gpu-go 对照见 `docs/gpu_go_analysis_and_lessons.md`。
> v1.5（2026-08-15）：**D13 服务端拓扑**——1:N 主线 + 1:1 第二拓扑（operator 阶段以 `RemoteGPUServer` CR 引入），
> 源自 gpu-go 对照分析；纠正"1:N 省 IP"的直觉（TensorFusion 也是单 IP 多端口）。§1.6 新增。
> v1.5.1（同日评审）：§1.6.4 k8s 落地四形态与三选二困境；§1.6.5 server-centric 分工；§1.6.6 库侧边界更正
> （**发现既有缺口：SESSION 记账要求 server pod `hostPID: true`，1:N 同样适用**，已补进 D9）。
> v1.5.2（同日评审）：§1.6.7 CR 层次收敛（`RemoteGPUServer` 原语 + `publish` 开关，`RemoteGPUPool` 为其多节点编排）；
> §1.6.8 外部 SM watcher 是 1:N 独有优势；核心设计 §6.6.1 更正"父进程不碰 CUDA"的归因为**设计约束**而非碰巧。
> v1.4（2026-08-15）：两项修订。**D7 修订**——**helm 编排替代 operator/CRD**（快速迭代期，operator 后置；
> RemoteGPUPool spec 降级为 values schema，§1.5）。**D2 修订**——EnsureSession 从注入 init 容器改为 NodePrepareResources 内同步调用：
> 屏障更强（严格先于 pod 一切容器，含用户自己的 init 容器）且**远程 pod 零 spec mutation**；
> webhook 退化为纯 UX 糖（可选）。代价：pod netns 数据面 pre-flight 不可得，由首个 CUDA 调用报错兜底（§5）。
> v1.3（2026-08-15）：**D4 重大修订**——client 制品改为**自包含静态构建**（D11，已在 fork 实现并本地全链路验证）
> + **镜像列表→节点版本目录**分发机制（D12）；新增 §4.4/§4.5。
> v1.2（2026-08-14）：引入 **operator + RemoteGPUPool CRD** 总体形态（D7–D10）——组件收敛为
> "1 CRD + 1 Operator + 2 DaemonSet"，agent 合并进 kubelet-plugin，controller 取消，会话目录改
> pod 内 emptyDir；§1.5 新增。
> v1.1（2026-08-14）：新增 §6.1 传输加密（TLS，D5）与 §7.1 高性能网络承载（D6），均为读 lupine 源码后的核查结论；
> D3 因 TLS 主机名校验被修正。
> 前置：`docs/remote_gpu_pool_research_design.md`（核心库 + lupine 方案 C-2，已实现并真机验证单节点会话隔离）
> 本文回答：设备如何上报、pod 如何调度到"网络可达但无 GPU"的节点、分配结果如何同时到达消费节点（注入）
> 与 GPU 节点（配额落盘）、lupine 版本如何匹配、server 如何被发现。

## 0. 已确认的决策（2026-08-14）

| # | 决策点 | 结论 |
|---|---|---|
| D1 | 技术路径 | **DRA 为主路径**，extender+device-plugin 为老集群兼容路径（后行） |
| D2 | 配额落盘时序 | **agent watch 推送（快路径）+ 消费侧 plugin 在 NodePrepareResources 内同步 EnsureSession（确定性屏障）**。<br>**v1.4 修订**：不再注入 init 容器——NodePrepare 严格先于 pod 一切容器，屏障更强且**远程 pod 零 mutation**（§5） |
| D3 | server 发现 | **endpoint attribute 同时允许 IP 或域名**，注入层不区分；按部署形态填值。<br>**修正（§6.1）**：attribute 需携带 scheme；启用 TLS 时只能是域名（主机名校验） |
| D4 | 客户端制品 | **直接上版本目录机制**（不等单基准制品 spike）；制品须校验带 TLS 编译（§6.1） |
| D5 | 传输加密 | lupine **服务端不支持 TLS**，需前置终止代理；K1 可明文，**多租户/跨信任域前必须启用**（§6.1） |
| D6 | 高性能网络 | **SR-IOV 优先**（透明、零 lupine 改动）；IB/RoCE 组网时叠 IPoIB 或 SMC-R；GPUDirect 属改 lupine 的长期项（§7.1） |
| D7 | 总体形态 | **v1.4：helm 编排 2 DaemonSet**，values 结构 = 原 RemoteGPUPool spec，`reachableNodeSelector`（= net-zone）一物两用：既圈定 slice 可调度范围，又是消费侧 DS 铺设范围。**operator/CRD 后置**至组件稳定、边界明确且 helm 不够用时（§1.5）。**v1.7 修订（用户拍板）：独立 `vgpu-manager-remote-gpu` chart，依赖 `vgpu-manager-dra-driver`**（不并入作子树；边界更清晰，代价是镜像/RBAC 声明有少量重复） |
| D8 | 会话令牌签发 | **v2.2：会话 = 分区（§11.1），每分区一个随机令牌，存 claim annotation `manager.nvidia.com/session-<hash>`**；原案"写入 `claim.status.devices[].data`"作废（依赖 KEP-4817 集群版本）。原文：**消费侧 kubelet-plugin 在 NodePrepareResources 生成随机令牌，写入 `claim.status.devices[].data`**；独立 controller 取消，pod 启动关键路径上没有任何集中式控制器（§1.5.3） |
| D9 | 会话目录 | **agent 与 lupine-server 同 pod，共享 emptyDir**；GPU 节点主机零安装，生命周期与会话语义天然对齐（§1.5.2） |
| D10 | 域名前缀 | **`vgpu-manager.io`**（CRD group 与 device attributes 同源）；不用 `nvidia.com` 后缀（`gpu.nvidia.com` 是 NVIDIA 官方 DRA 驱动的属性域，避撞） |
| D11 | 制品形态 | **自包含静态 client**（fork 增加 `LUPINE_STATIC_DEPS`：nghttp2/OpenSSL/libstdc++/libgcc 静态内嵌，运行时依赖仅 glibc；rockylinux8 统一底座 → glibc 2.28 地板全矩阵最低）。已实现并本地验证（§4.5） |
| D12 | 制品分发 | **镜像列表 → 节点版本目录**：cudaVersion→镜像地址映射在 values 里维护，helm 渲染进消费侧 DS 的 init 容器逐版本落盘 hostPath；增减版本 = 改列表，不重建我们的镜像。**逐 pod 选镜像不可行**（准入先于分配，§4.4） |
| D13 | 服务端拓扑（v1.5） | **1:N（单 server/节点 + session）为 k8s 池化主线；1:1（per-allocation server）为正式第二拓扑**。CR 层次：`RemoteGPUServer` 为节点级原语（含 `publish` 开关：发布给集群内 DRA / 不发布仅供集群外连接），`RemoteGPUPool` 是它的多节点编排层（§1.6.7）；1:N 的独有优势是外部 SM watcher 跨会话共享 NVML 采样（§1.6.8）。k8s 落地形态受"同 IP/高性能网卡/pod 隔离三选二"约束（§1.6.4）；库侧记账模式待定（SESSION+hostPID vs CGROUP+host /proc，§1.6.6） |
| D15 | 端点交付与鉴权（v1.6.1 修订：不改 lupine） | tensor-fusion 的**长轮询+TokenReview 需改 lupine client，作废**（`docs/tensor_fusion_analysis_and_lessons.md` §3.1）。改为：**① endpoint 用域名注入 + 控制面维护 DNS**（name→IP，提升 D3/§6 的域名选项为主力，顺带满足 TLS 主机名/multus 次网卡/IP 漂移），就绪屏障回到 D2；**② 鉴权 = 签发时绑定 pod + provider `restore()` 服务端校验**（`LUPINE_SESSION` 是唯一不改 lupine 的 client→server 通道；provider 是我们的代码，可在此二次校验 token 绑定与存活）。详见 §5.1 |
| D16 | extender 记账蓝图（v1.6） | K3 extender 兼容路径的自建全局记账，采用 **内存权威 store + Assume/Commit/Rollback 乐观账本 + TTL 清扫孤儿预留**（照搬 tensor-fusion `gpuallocator`，§3 补注）；DRA 主路径无需，靠原生 allocator |
| D17 | 多租户前置能力（v1.6，待办） | 多租户上生产前需引入：**多维配额系统**（借 `GPUResourceQuota` 的 Total 命名空间级 + Single 每-workload 双维度 + 默认值 + 告警阈值）与**单卡隔离模式锁**（一张物理卡被多会话切分时锁定 shared/soft/hard、冲突 fail-closed）。记入 §10 |
| D18 | 代码组织（v1.6.1） | **远程 GPU 控制面不新开仓库，留在 vgpu-manager**：agent=kubelet-plugin `--mode`（D-§1.5.1，复用 DRA 底座 80%）、helm 并入 dra-driver chart（D-§1.4）、刚消灭 library 双树——新仓库会重造双树。DNS 控制器/会话签发校验控制器作为新 `cmd/` 入口 + controller 加入本仓库。仅当需独立发版节奏时，用 monorepo 内独立 Go module，仍不新 repo |
| D14 | 平台制品（v1.6） | **Windows/macOS 客户端打包进 GHCR 载体镜像**（内网 registry 作统一制品通道）：tag 按**平台家族**分 `cuda-<ver>-windows` / `cuda-<ver>-darwin`，**绝不按 distro 版本分**（D11 已把 Linux 制品做成 distro 无关，os-version 维度是倒退）。编译必须留在原生 runner（MSVC/Apple 工具链），Linux job 只做打包（§4.6） |
| D20 | EnsureSession 协议（v1.7，用户拍板） | **gRPC**（消费侧 plugin → GPU 节点 agent 的跨节点调用）：proto 放 `pkg/api/`（与 registry 服务同惯例），强类型、响应顺带携带 `cudaVersion` 供 §4.2 版本比对。监听 endpoint 同网卡的独立 TCP 端口；K1 明文，多租户前叠加 TLS/凭证（随 D5 节奏） |
| D21 | 角色开关形态（v1.7，用户拍板） | **`--mode` 标志 + `RemoteGPUSupport` feature gate 两层正交**：`--plugin-mode=server`（GPU 节点，本地 DRA 原职责 + gate 开启时叠加远程池职责）/ `--plugin-mode=inject`（消费节点，剥离 NVML/健康检查/NRI 等 GPU 依赖的初始化分支，仅注入面）；`RemoteGPUSupport` gate 控制远程功能开闸并按仓库惯例做启动期组合校验（要求 `VGPUSupport`；inject 模式隐含要求该 gate） |
| D22 | 实施顺序（v1.7，用户拍板） | **spike 先行，验证后再工程化**：S0 = 纯 YAML 手工 ResourceSlice 验证跨节点调度链路（零代码）；S1 = 最小 `--mode=inject` 注入链路 + GPU 节点 `vgpu-session-config` 工具手工落盘（跳过 server watch/EnsureSession）；S2 起进入 K1 工程化（server 模式、gRPC、chart）。详见 §8.1 |
| D23 | 统一设备模型（v2.0，用户提出） | **远程不是新池，是既有设备的一个发布属性**。每台 GPU 节点仍只有一个 pool（= 节点名），gate 开时：设备属性叠加 `accessMode=remote`、`endpoint`、`netZone`（驱动默认域非限定名），pool 用 nodeSelector（= **运维自定义的 `--remote-node-selector` label selector 表达式**，标准语法：`k=v,k2 in (a,b),!k3`，单 term 内 AND；**GPU 节点自身不再隐含加入**——v2.1 后本节点也走远程，是否允许 pod 落在 GPU 节点由运维在 selector 里决定；无 net-zone 约定、无 netZone 属性），helper 传 `ReconcilePoolWithName(节点名)`（上游为"节点拥有、集群可见"专设的选项）。gate 关 → `accessMode=local` + `nodeName`，与今天全等。**v2.1 修订（用户提出）：gate 开的节点只发布、不做分配**——server 模式插件以 helper 的 `DRAService(false)`+`RegistrationService(false)` 运行：只跑 ResourceSlice 发布，**不向 kubelet 注册 DRA 服务**；同一 GPU 节点上另起 `--mode=inject` 进程作为该驱动名的唯一注册插件承担全部 NodePrepare，pod 即使落在 GPU 节点也经 lupine（loopback）消费。（驱动名在 kubelet 侧是节点单例，server 若仍注册则 inject 无法注册，"返回错误"只会让节点永远 prepare 失败——因此必须是"不注册"而非"拒绝"。）理由：一个 claim/pod 可能同时含本节点与其他节点的设备，本地路径（LD_PRELOAD 真驱动）与远程路径（lupine shim + `LUPINE_DISABLE_LOCAL`）在一个容器内互斥，无法按设备拆分注入。代价：同节点消费多一跳 TCP 环回；收益：路径唯一、无混合冲突。**GPU 节点因此也必须铺 client 制品目录**（chart 注意）。**一台设备只有一个 accessMode 值**：class 只能选"未放开节点的设备"或"已放开节点的设备"，表达不了"同一设备但只本地"——只本地靠节点不开 gate 或 pod nodeAffinity。本地优先打分为后续专项（DRA 插件当前不打分） |
| D24 | 服务端组件形态（v2.0，用户提出） | **独立 `cmd/remote-agent` 二进制**，与 lupine-server **同 pod 两容器**：agent 起 → preflight 完成后向共享 emptyDir 写 ready 文件 → server 容器 bash 循环等到该文件再 exec `lupine_driver_server`（**不用 init/sidecar 容器，兼容旧 k8s**；server 崩溃重启时文件仍在、直接起）。agent 职责：claim watch 落盘/回收会话目录、EnsureSession gRPC（D20）、探测 server 端口就绪、维护本 server 的 DNS 记录（D25）。**kubelet-plugin 独立部署**（升级 DRA 插件不掐会话）；三者合一 pod 留作可选形态。VGPUSupport 关（type=gpu）时远程仍需库侧会话裁剪可见性（server 容器永远带 `libvgpu-control.so`；会话可"不限额只裁可见"） |
| D25 | DNS 组件（v2.0，用户提出） | **独立组件、pod 网络部署**，agent 主动维护 name→IP；接入代价 = 集群 CoreDNS 对 zone `forward` 到它（或它本身是带 hosts/file 插件的 CoreDNS 实例）。**K1 先节点 IP 直注（endpoint 缺省 = 节点 InternalIP:14833），DNS 作独立阶段接**（硬理由 = multus 次网卡 IP / IP 漂移 / TLS 主机名） |
| D26 | lupine-server 状态的唯一来源（v2.3，用户提出） | **remote-agent 是 lupine-server 状态的唯一来源，dra-server 只需知道本机 agent 怎么连**（`--remote-agent-endpoint`，grpc:// 或 unix://；`--remote-server-endpoint` 已删除）。agent 每 5s 探测 server（HTTP GET 读 `x-lupine-cuda-version`），`ServerInfo` 暴露 `listening` / `cuda_driver_version` / `endpoint`（server 对外地址）/ `agent_endpoint`（agent 自身对外地址），`EnsureSession` 回包也带 `server_endpoint`。对外 host 的来源：探测地址 host 可路由则原样；是回环时在本机地址中发现（候选序 = 节点 InternalIP > 物理网卡 > 虚拟网卡，逐个用同一 GET 验证，命中后粘住直到失效；lupine 绑 `INADDR_ANY`，监听表反查不出 IP）；`--advertise-server-endpoint` 可钉死 server 对外地址（DNS/网关）。**agent 未应答或无可路由地址时，设备照常发布但不带 serverEndpoint/agentEndpoint/serverCudaVersion 属性，并打 NoSchedule 污点 `<driver>/remote-unavailable`**（需 DRADeviceTaints 生效才拦调度）；inject 侧兜底：serverEndpoint 属性缺失时以 EnsureSession 回包的 `server_endpoint` 为准，两者都没有则 prepare 失败。agent `--listen-server-endpoint` 支持逗号分隔的 grpc:// + unix:// 多监听（unix 只服务同节点、不发布）。为 D25（DNS）预留：agent 已持有对外 endpoint |
| D19 | 集群外接入的中继边界（v1.6.2） | **`publish: false` 的集群外消费（开发者本机 `vgpu use`、跨集群）不得直连共享 lupine-server 端口**：集群外主体没有 pod 身份（D15 的"签发时绑定 pod"不成立）、lupine 服务端无 TLS/无逐连接身份（D5），直连等于把多租户共享端口面裸暴露。改为**每会话一个单租户中继 pod**：作为外部连接的唯一落点（`kubectl port-forward` 目标 / 专用 Service），终止外部传输、承载 TLS 终止（补 D5）、校验会话令牌后经集群内网络转发到共享 server。一租户一中继 = 单租户信任/崩溃域隔离，生命周期绑定会话便于回收。集群内 DRA 消费者不经此路（它们有 pod 身份，走 §5.1）（§7.2） |

## 0.1 v2.0 架构修订：对既有章节的覆盖说明

| 既有表述 | v2.0 结论 |
|---|---|
| §1 "远程是新增 DeviceClass/资源池，现有本地分配不动" | **不新增池**。本地分配确实不动，但同一设备承担两种消费；远程只是属性 + nodeSelector 放宽（D23） |
| §1.5.1 "agent 合并进 kubelet-plugin `--mode=server`" | **agent 独立二进制 `cmd/remote-agent`**（D24）。kubelet-plugin 的 `--mode=server` 仅剩"发布时叠属性 + 放宽 nodeSelector"（改动量小）；`--mode=inject` 不变 |
| §1.5.4 "消费侧 DS 集群级归并" | 仍成立（驱动名节点单例），但归并对象是 `--mode=inject` DS 的 nodeAffinity（各 zone 可达标签并集，排除 GPU 节点） |
| §2.1 属性表（`vgpu-manager.io/type=remote-vgpu`、`cudaVersion` 等） | 改为驱动默认域非限定名：新增 `accessMode`/`endpoint`/`netZone`，`uuid`/`cudaDriverVersion` 复用既有属性；`type` 保持 `vgpu`/`gpu`。D10 的 `vgpu-manager.io` 前缀保留给**标签/注解/CRD group**，不用于设备属性（同驱动域内无撞名风险） |
| §2.1 "每 GPU 节点一个 pool" | 不变；pool 名 = 节点名，helper `ReconcilePoolWithName` 使其在无 `nodeName` 时仍被正确跟踪（性能注脚：每节点插件将 list/watch 本驱动全部 slice） |
| §2.4 "GPU 节点 agent = kubelet-plugin server 模式" | = `cmd/remote-agent`（D24），职责表不变，另加"探 server 端口就绪"与 DNS 记录维护 |
| D2/D8/D20（令牌、屏障、gRPC） | 不变；EnsureSession 的被调方是 remote-agent |
| D13（1:N 主线 / 1:1 第二拓扑） | 不变 |
| §8 改造面表 | kubelet-plugin server 模式：大 → **小**（已落）；新增 remote-agent：**大**（K1）；DNS 组件：中（K2） |

**DeviceClass 用法**（chart `deviceClasses.remoteGPU`）：启用后 `gpu-manager`/`vgpu-manager` class 钉 `accessMode == 'local'`，
新增 `remote-vgpu-manager` class 选 `accessMode == 'remote'`。升级窗口内旧插件发布的设备无 `accessMode`，会被钉住的 class 排除，
直到该节点插件升级——滚动升级期间注意。

## 1. 问题本质：三平面模型

远程 GPU 打破 "设备属于节点" 假设。所有问题都是三个平面之间的通信问题：

```
┌─ 控制面 ────────────────────────────────────────────────┐
│ scheduler(DRA allocator / extender)：全局池记账、版本/可达性匹配 │
│ helm 渲染：池 DS/消费侧 DS/DeviceClass（静态，无运行时控制器，§1.5）│
└──────────┬──────────────────────────┬───────────────────┘
           │ ①分配结果                  │ ①分配结果
┌─ GPU 节点(资源面) ─────────┐   ┌─ 消费节点(任意可达节点) ────────┐
│ lupine-server + libvgpu     │   │ kubelet + kubelet-plugin        │
│ agent：上报设备、落盘会话配额 │◄──│ 注入: LUPINE_SERVER/SESSION/     │
│   ②须先于容器首个 CUDA 调用  │ ③ │ DISABLE_LOCAL/客户端库/屏障      │
└────────────────────────────┘   └────────────────────────────────┘
```

- ① 分配结果的载体：DRA 下是 **ResourceClaim 对象本身**（两边都 watch 它）；extender 下是 pod annotation。
- ② 时序约束：provider fail-closed（库侧安全底线，不放松）要求配额先于首个 CUDA 调用落盘 → D2 的屏障。
- ③ 可达性约束：消费节点必须与 GPU 节点 underlay 网络可达 → 标签 + NodeSelector 进调度。

**本地 vGPU 路径零影响**：远程是新增 DeviceClass/资源池，现有 device-plugin/extender/DRA 本地分配不动。

## 1.5 总体形态：helm 编排的 2 DaemonSet（v1.4 修订；operator/CRD 后置）

> **v1.4 策略调整**：方案处于快速迭代期，一开始就上 operator/CRD 过于激进（开发/维护复杂度高，
> 且边界未全部明确时 CRD schema 会反复变）。改为 **helm 编排整个 remote-gpu 组件集**，与现有
> `charts/vgpu-manager` / `charts/vgpu-manager-dra-driver` 同一形态；待组件稳定、边界明确、
> 且 helm 在编排或简易性上确实不够用时，再引入 CRD/operator。
>
> **代价很小**——v1.2 分析里真正承重的部分本就不依赖 operator：agent 合并进 kubelet-plugin
> （§1.5.1）、令牌在消费侧签发（§1.5.3）、会话目录 emptyDir（§1.5.2）、节点级制品物化（§4.4）
> 全部原样保留。operator 承担的只是"把声明翻译成 DS 模板"，而这恰是 helm 的本职。
> `RemoteGPUPool` 的 spec 结构**降级为 values.yaml 的 schema**，字段一一对应，
> 将来升 CRD 时是机械映射。

部署者视角：`helm install` 一个 chart（建议并入 `vgpu-manager-dra-driver` 作 `remoteGPU:` 子树，
共享 kubelet-plugin 镜像/RBAC/webhook；或独立 `vgpu-manager-remote-gpu` chart 依赖前者）
→ 给节点打标签 → 完事。

```
用户视角：  helm install ... -f values.yaml       ← 部署 = 一次 helm + 节点标签
           pod 写熟悉的资源请求                    ← 使用 = 与本地 vGPU 一致（§1.5.6）

┌─ helm 渲染（静态，无运行时控制器）─────────────────────────────────┐
│  values.remoteGPU.pools[]  →  每 pool 一个 GPU 节点 DaemonSet          │
│  values.remoteGPU.consumer →  一个消费侧 DaemonSet（§1.5.4 归并由 helm │
│                               模板取各 pool selector 并集）           │
│  values.remoteGPU.clientImages → 消费侧 DS 的 init 容器列表（§4.4）    │
│  DeviceClass / RBAC / webhook 配置                                   │
└──────────────────────────────────────────────────────────────────┘
┌─ gpu-node DaemonSet (per pool) ─────┐ ┌─ consumer DaemonSet(归并后) ──┐
│ [c1] kubelet-plugin --mode=server    │ │ [c1] kubelet-plugin           │
│      发布 slice + watch claim 落盘 + │ │      --mode=inject            │
│      EnsureSession（原"agent"职责）   │ │   （注入/CDI/EnsureSession）  │
│ [c2] lupine-server                   │ │ [init×N] 逐版本铺制品到 host  │
│      （镜像内置 libvgpu-control.so）  │ └───────────────────────────────┘
│ [c3] tls-proxy（可选, envoy, §6.1）  │
│ 共享 emptyDir = 会话目录              │
└──────────────────────────────────────┘
```

**helm 阶段有意放弃、留给未来 operator 的能力**（都不是 K1/K2 必需）：
- 维护模式编排（撤 slice → 等 claim 排空 → 滚动重启 server）——helm 阶段靠运维手动
  cordon 池 + 等待，或 plugin 提供 `--drain` 开关；
- TLS 证书/DNS 记录的自动生命周期——helm 阶段用 cert-manager Certificate 模板 + 手工/外部 DNS；
- 池状态汇总（`status.servers[]`）——helm 阶段看 ResourceSlice + plugin 指标。
这些正是"helm 不够用"的判据；触发到再上 operator，届时 values schema 即 CRD schema。
### 1.5.1 agent 合并进 kubelet-plugin（同一二进制，`--mode` 区分）

§2.4 的 agent 三职责（发布 slice / watch claim 落盘 / EnsureSession）中，前两个在
kubelet-plugin 已有 80% 基建（`driver.go` 的 `GenerateDriverResources` 已产
`resourceslice.DriverResources{Pools}`，远程池 = 换 pool 名 + `NodeName` 换 `NodeSelector`；
informer 底座同一套）。独立 agent 等于重抄依赖。合并后：
`--plugin-mode=server`（GPU 节点，含本地 DRA 原职责 + 远程池职责 + inject 能力）、
`--plugin-mode=inject`（消费节点，仅注入，无 GPU 依赖）。

### 1.5.2 会话目录 = pod 内共享 emptyDir（D9）

lupine-server 与 agent 同 pod 后，会话目录用两容器共享的 emptyDir，收益链：
- **生命周期天然对齐**：server 容器重启 = 全部会话子进程死亡 = 会话作废；emptyDir 恰好活到
  pod 级，无跨 server 世代的陈旧目录清扫问题；
- **GPU 节点主机零安装**：`libvgpu-control.so` 烧在 server 容器镜像内，
  `LD_PRELOAD`/`LUPINE_CHECKPOINT_LIBRARY` 指容器内路径（本地 vGPU 才需要 host 安装）；
- 会话配额文件不暴露在主机文件系统。
库侧零改动（`VGPU_CONFIG_SESSION_BASE` 指 emptyDir 挂载点即可）。
**pod spec 必须 `hostPID: true`**（v1.5.1 补）：SESSION 记账把子进程 `getpid()` 写进 `pids.config`，而 NVML 返回
宿主 PID；不同 PID 命名空间无法对应（进程从内部得不到自己的宿主 PID）。server pod 无 PID 隔离需求，代价可接受。
替代是 CGROUP 模式 + 挂宿主 `/proc` 到 `.host_proc`（不需 hostPID），见 §1.6.6 第 4 条。

### 1.5.3 令牌签发在消费侧 plugin（D8）——关键路径卫生

独立 controller 的唯一职责是签令牌，取消之：消费侧 plugin 在 NodePrepareResources 生成随机令牌
→ 写入 `claim.status.devices[].data`（DRA 为 driver 私有数据设计的字段，需
`resourceclaims/status` update RBAC）→ 注入 pod env；GPU 侧 agent watch claim 同时拿到分配与令牌。
故障域对照（pod 启动关键路径上没有集中式组件——helm 阶段天然如此，将来上 operator 也须保持）：

| 组件挂了 | 影响 |
|---|---|
| helm/apiserver 侧（无运行时控制器） | 已部署的 DS 照常工作；仅新的部署变更受影响 |
| gpu-node DS | 该池不可用（本质如此） |
| consumer plugin | 该节点新 pod 无法 prepare（DRA 自身故障域） |

### 1.5.4 消费侧 DS 的归并约束（多池重叠，实现必须处理）

DRA 驱动名是**节点单例**（插件 socket 注册冲突），两个 pool 的可达域有交集时**不能**各铺一个
inject DS。helm 模板必须集群级归并：消费侧 DS 只有一个，nodeAffinity = 所有 pool
`reachableNodeSelector` 的**并集**（多 nodeSelectorTerms 即 OR 语义），且排除 server 节点
（server 模式插件本就兼具 inject 能力）。

### 1.5.5 values 结构（D7、D10）——将来的 RemoteGPUPool CRD schema

**v1.4：以下结构落在 chart values 里**（`remoteGPU.pools[]` 每项一个池），字段设计即为未来 CRD 的
spec；`network`/`transport` 字段是 D5/D6 的预留接缝——新增网络形态 = 模板加一个 profile 分支
（NAD annotation / hostNetwork / preload env / MTU），**values 结构不破坏、用户无迁移**：

```yaml
# values.yaml (remoteGPU.pools[0])；升 CRD 时逐字段映射为 RemoteGPUPool.spec
- name: pool-a
  nodeSelector: {vgpu-manager.io/remote-server: "true"}   # 哪些 GPU 节点入池
  devices: {}                            # 可选：uuid/index/型号过滤，默认全部
  consumer:                              # 消费侧（D7：也由本 CR 控制）
    reachableNodeSelector: ...           # 一物两用：slice 可调度范围 + 消费侧 DS 铺设范围
    # 缺省约定：匹配 vgpu-manager.io/net-zone.<zone>=reachable 标签
  network:                               # ← D6 预留接缝
    profile: hostNetwork                 # hostNetwork | multus（将来扩展）
    multus: {networkAttachment: sriov-net-a}
    zone: zoneA
  transport:                             # ← D5/D6 预留接缝
    tls: {enabled: false, issuerRef: ..., dnsSuffix: gpu.corp}
    preload: none                        # none | smc-r | rsocket（§7.1.3 spike 通过后开闸）
  lupine:                                # LUPINE_* 透传逃生门 + 制品映射（D12）
    image: ...                           # server 镜像
    port: 14833
    extraEnv: []
    clientImages:                        # cudaVersion → 自包含 client 制品镜像（§4.4）
      - {cudaVersion: "12.9", image: ghcr.io/.../lupine-client-static:cuda-12.9.1@sha256:...}
  scheduling: {deviceClassName: remote-vgpu-a}      # chart 渲染 DeviceClass
# status.servers[] 等运行时汇总：helm 阶段无；升 CRD 后由 operator 填
```

### 1.5.6 使用面：与本地 vGPU 对齐的两档 UX

- **默认档**：pod 上写熟悉声明（annotation/资源风格），现有 pod-mutate webhook 扩一条转换
  → 自动生成引用 DeviceClass 的 ResourceClaimTemplate。用户不需要懂 zone（slice nodeSelector
  自动圈定）、不懂版本（运行时兜底）、不懂 endpoint（注入层拼接）。
- **进阶档**：直接写 ResourceClaimTemplate + CEL（选 zone/版本/跨池约束），不经 webhook。

## 1.6 服务端拓扑：1:N 主线 + 1:1 第二拓扑（D13，v1.5）

对照 gpu-go/TensorFusion（`docs/gpu_go_analysis_and_lessons.md` §3.1）后的定案。先纠正一个直觉：
**TensorFusion 的 1:1 也是单主机 IP，只是每 worker 一个端口**（`WorkerInfo.ListenPort`，
`connection_ip` 是主机上选的一个 IP）——所以"1:N 省 IP/网卡"相对它并不成立，两者都单 IP；
真实差异只在**端口数**与**进程/隔离拓扑**。

### 1.6.1 两种拓扑的取舍

| 维度 | 1:N（单 server/节点 + session，现方案） | 1:1（per-allocation server） |
|---|---|---|
| 崩溃域 | server 父进程崩 = 节点全部会话连接断 | 一份分配一个进程，互不影响 |
| 升级/重启 | 全节点滚动，所有会话中断 | 逐分配 |
| quota/可见性交付 | provider restore() 注入 + session 目录 + 消毒 + fail-closed | spawn 时 env 注入（库的 `init_g_vgpu_config_by_env` 现成），进程 = 分配 |
| 就绪信号 | 需 EnsureSession 屏障 | 端口能连 = 配额就位（但 endpoint 分配后才有，DRA 下仍需回传等待，形状等价） |
| CUDA 版本 | 单 server 绑定节点驱动 | 同节点可混跑多版本 server 二进制，按分配选 |
| 端口面 | **1 个/节点**：TLS 代理/防火墙/DNS 一份 | N 个/节点：需端口分配 + 范围放行 + 代理按端口路由 |
| endpoint | 静态，进 ResourceSlice attribute | 动态，经 claim status / CR status 回传 |
| 空闲开销 | 无 | N 个监听进程（未连接不持 CUDA context，极轻） |
| 容器级记账 | SESSION 模式按 pids.config 聚合 | **仍需**（同一 worker 收同租户多进程连接），只是分组更简单 |
| 外部 SM watcher（跨会话共享 NVML 采样） | ✓ 节点一个 watcher，全部会话 mmap 同一缓存，驱动探测 O(1)/节点 | ✗ pod 隔离切断共享，O(server 数)/节点（§1.6.8） |
| 实现状态 | 已实现、真机验证 | 库侧零改动；需 reconciler（desired/actual 进程对账，gpu-go `hypervisor/reconciler.go` 是现成范式） |

### 1.6.2 场景归属（这是决定"两者都要"的依据）

| 场景 | 拓扑 | 原因 |
|---|---|---|
| k8s pod 批量调度（K1/K2 主线） | **1:N** | endpoint 静态进 slice；单端口好管；分配密度高、进程数敏感 |
| 用户专属/长期独占 GPU 服务 | **1:1** | 崩溃域、升级、CUDA 版本按用户定；本来就是一份长期分配 |
| 开发者本机 `vgpu use`（gpu-go 分析 §3.5） | **1:1** | 一人一 server，share code 语义天然对上 |
| 强隔离/合规（租户间进程级隔离） | **1:1** | 不共享父进程 |

### 1.6.3 与 operator 的关系

**1:1 是上 operator 的第一个硬理由**（比 v1.4 列的 drain/TLS 生命周期/status 汇总都硬）：
"申请 GPU 服务 → 拉起用户专属 server" 是运行时事件驱动的生命周期，helm 无法响应。落法：

- `RemoteGPUPool` CR/values → 1:N 池（helm 阶段即可，operator 阶段可平移）；
- `RemoteGPUServer` CR → 1:1：spec 写设备/quota/CUDA 版本/端口策略，operator 拉起
  `lupine_driver_server`（LD_PRELOAD 库 + `CUDA_VISIBLE_DEVICES` + `CUDA_MEM_LIMIT_*`/`CUDA_CORE_LIMIT_*`
  + `VGPU_CONFIG_SESSION_PATH=<per-server 目录>` 以隔离 `.sm_node`/`.vmem_node` 共享区），回填 endpoint
  到 status，管重启/回收；
- 两种拓扑对客户端**完全透明**：注入面相同（`LUPINE_SERVER`/`SESSION`/`DISABLE_LOCAL`/制品目录），
  仅 endpoint 来源不同（slice attribute vs CR status）。

阶段：1:N 继续主线；1:1 在 operator 阶段引入，K3 之后。库侧边界（记账模式选择、hostPID、
`VGPU_REMOTE_MODE` 不可设、per-server 目录只在一 pod 多 server 时才需要）见 §1.6.6，**记账模式待定**。

### 1.6.4 1:1 在 k8s 里怎么落：同 IP 多端口的三选二困境（v1.5.1，答复评审）

先说清 gpu-go 的事实：它的 agent 是**裸机 systemd 进程**，worker 是 agent 在主机上 spawn 的
**主机进程**（`hypervisor/process_unix.go`），所以"同主机 IP、每 worker 一个端口"对它是零成本——
根本不在 k8s 里。搬到 k8s，1:1 有四种形态，各自的代价必须摆清楚：

| 形态 | 同 IP | 高性能网卡（multus/SR-IOV/IB） | 进程/资源隔离 | 说明 |
|---|---|---|---|---|
| **(a) worker-per-pod，pod 网络** | ✗ 每 pod 一个 CNI IP | ✓ 每 pod 一个 VF/IPoIB 附着 | ✓ pod 级 | 消耗 IP 与 **VF（每 PF 通常 8–64 个，小分配多时不够）**；DNS/路由按 pod |
| **(b) worker-per-pod，hostNetwork** | ✓ 主机 underlay IP，端口由 operator 分配 | △ **hostNetwork pod 不能挂 multus 次网卡**；VF/IPoIB 必须是**主机拥有**的接口，server 绑主机 underlay IP，所有 worker 共享该网卡 | ✓ pod 级（进程/cgroup），网络命名空间共享 | 与 1:N 推荐形态同一张网；hostPort 端口冲突由 operator 管 |
| **(c) 单 server pod/节点，pod 内 spawn 多 worker 进程** | ✓ | 同 (b) 或该 pod 挂一个 multus 网卡 | △ 进程级崩溃域有，**pod 资源在创建后不可变**：spawn/退出 worker 无法让 k8s 资源记账跟着走，GPU 也必须预先全部挂进 pod | = gpu-go 裸机模型直接搬进一个 pod；k8s-native 程度最差，**不推荐** |
| **(e) worker-per-pod，pod 网络 + hostPort** | ✓ 客户端看到主机 IP:port | ✗ hostPort DNAT 走 pod **主 CNI** 接口，不经 multus 次网卡 | ✓ | 只在没有高性能网络需求时成立 |

**结论：同 IP / 高性能网卡 / pod 级隔离，三者最多取二。**
- 要同 IP + 高性能网卡 → (b) hostNetwork（网卡主机拥有，全部 worker 共享）——恰是 1:N 的推荐形态，1:1 只是多了端口分配；
- 要高性能网卡 + pod 级隔离 → (a) 每 pod 一个 VF/IP，代价是 VF 数量与 IP/DNS 管理；
- 要同 IP + pod 级隔离且不要高性能网卡 → (e) hostPort。
选哪个由部署的网络形态决定（对应 values 里 `network.profile`），**不是 1:1 本身能回避的问题**。
(c) 记录为"可行但不推荐"：它复刻 gpu-go 的裸机模型，但与 k8s 资源模型冲突。

### 1.6.5 1:1 与 DRA 的分工（答复：operator 建 server，DRA 只发布设备并注入客户端）

采纳评审意见，把 §1.6.1 表中"endpoint 分配后才有"的问题**从 DRA 里拿出去**：

- **server-centric**：用户/管理员创建 `RemoteGPUServer` CR → operator 拉起 server（形态按 §1.6.4）→
  status 回填 endpoint/token/cudaVersion。**GPU 归属由 operator 代为创建 ResourceClaim 占住**，
  DRA allocator 仍是设备记账的唯一真相源，不会与 1:N 池双分。
- **消费 pod 不再对 GPU 设备发 claim**：它引用某个 `RemoteGPUServer`（label/annotation/DeviceClass
  参数），注入层从 CR status 拿 endpoint+token 注入，与 1:N 的注入面完全相同；调度约束只剩
  "可达 server 所在 zone"（nodeSelector）。
- 于是 1:N 是**device-centric**（pod 要设备，调度器挑 server），1:1 是**server-centric**（先有 server，
  pod 挑 server）。两者并存不冲突，客户端透明。

### 1.6.6 库侧边界更正（答复评审，此前表述有误）

此前"库侧零改动，`VGPU_CONFIG_SESSION_PATH` 指 per-server 目录即可"说得过粗，逐条更正：

1. **`VGPU_CONFIG_SESSION_PATH` 与记账模式是两个独立开关**（源码：`session.c` 的 `session_enabled()`
   只看该 env 是否为合法绝对路径，与 `compatibility_mode` 无关）。设了它，**全部 10 类路径**
   （quota/`pids.config`/`.vgpu_lock`/`.vmem_node`/`.sm_node`）迁到该目录——在任何记账模式下都生效；
   但 **`pids.config` 只被 SESSION 模式读取**，其他模式设了它只是"路径隔离生效、记账走各自模式"。
   所以"非 session 模式下不生效"的准确说法是：**路径隔离生效，会话记账不生效**。
2. **谁需要它**：worker-per-pod（(a)/(b)/(e)）**不需要**——每 pod 自己的 `/tmp`，共享区天然隔离；
   只有 (c) 一 pod 多 server 才必须 per-server 目录，否则 `.sm_node`/`.vmem_node` 跨 server 串。
3. **`VGPU_REMOTE_MODE` 在 1:1 env 配置下不能设**：远程模式 fail-closed 会因"无会话配额文件"直接
   拒绝服务。1:1 走 `CUDA_MEM_LIMIT_*`/`CUDA_CORE_LIMIT_*` env 回退配置时保持它未设；provider 也不必挂载
   （无 session 头 → restore 被跳过，无害）。
4. **1:1 的记账模式二选一，待定**：
   - **SESSION 模式** → 服务端 pod 必须 **`hostPID: true`**：`pids.config` 记的是子进程 `getpid()`，
     NVML 返回的是宿主 PID，不同 PID 命名空间就对不上（进程无法从内部得知自己的宿主 PID）。
     server pod 本就没有 PID 隔离需求，代价可接受；
   - **CGROUPV1/V2 模式**（本地 vGPU 现行）→ 不要求 hostPID，改为挂载宿主 `/proc` 到
     `/etc/vgpu-manager/.host_proc`（库按 `.host_proc/<nvml-pid>/cgroup` 判归属，`cuda_hook.c:2216`），
     lupine 子进程与 server 同 cgroup 即命中；无 session 目录、无 provider。
   两者都能正确处理"同一物理卡被多个 1:1 server 切分"的情况（各自只算自己的进程）；**HOST 模式不行**
   （全机求和）。
5. **顺带发现的既有缺口（1:N 同样适用）**：SESSION 模式的 hostPID 要求此前两份设计文档都没写。
   1:N 的 gpu-node DaemonSet **必须 `hostPID: true`**，否则会话记账全部失效——补进 §1.5.2/D9 与 chart。
6. worker-per-pod 下**设备可见性由容器运行时完成**：CDI/`NVIDIA_VISIBLE_DEVICES` 只挂分配到的
   `/dev/nvidia*`，CUDA 与 NVML 都只看到它们，`CUDA_VISIBLE_DEVICES` 与 NVML 可见性 hook 在此形态下
   都是冗余的（保留无害）。这是 1:1 pod-per-worker 在库侧**严格更简单**的地方。

### 1.6.7 CR 层次收敛：`RemoteGPUServer` 是原语，`RemoteGPUPool` 是它的多节点编排（v1.5.2，采纳评审）

评审提议并采纳：不把 1:N 与 1:1 做成两个平行的 CR，而是**一个原语 + 一个编排层**：

- **`RemoteGPUServer`（原语，节点级）**：描述"某节点上一个 lupine-server 实例"——节点、设备集、CUDA 版本/镜像、
  端口策略、网络形态、quota 模式（session / env）、**`publish`（是否把设备发布给集群内调度）**。
  operator 由它物化该节点的 server 工作负载（形态按 §1.6.4）并回填 endpoint/token/状态。
- **`RemoteGPUPool`（编排层，多节点）**：按 `nodeSelector` 圈一批 GPU 节点，为每个节点**生成一个 `RemoteGPUServer`**
  （模板化：`serverTemplate` + per-node 覆盖），再叠加池级语义（可达域/消费侧 DS/DeviceClass/制品列表）。
  Pool 对 Server 是 owner；删 Pool 级联删 Server。

这样两种拓扑的关系变成："1:N 池"= Pool 生成的一组 `publish: true` 的 Server；"1:1 专属"= 单个手工建的 Server
（`publish` 按需）。**只有一套 server 物化逻辑**，1:N 与 1:1 的差别退化为"谁创建 Server、发不发布、每节点几个"。

**`publish` 开关（评审提出的重要用法）**：
- `publish: true` → operator 为该 Server 的设备发布 ResourceSlice（或代为持有 claim），**供集群内 pod 经 DRA 消费**；
- `publish: false` → **不进集群调度**，Server 只对外暴露 endpoint+token，供**集群外用户**（开发者本机 `vgpu use`、
  gpu-go 分析 §3.5 的场景；或跨集群消费）连接。会话/配额仍由 operator 落盘（session 模式）或 env 注入。
  这把"集群外消费"从一个独立产品路径变成了 Server 原语的一个布尔属性。
  **注意（D19）**："对外暴露 endpoint" **不等于直连共享 server 端口**——集群外主体无 pod 身份、lupine 端无 TLS/逐连接身份，
  必须经每会话的单租户中继 pod 终止外部传输并校验令牌后再转发（§7.2）。

**helm 阶段的对应**：`values.remoteGPU.pools[]` 就是 Pool 的 spec；helm 直接渲染 per-node DS 而不生成中间 Server 对象
（helm 无 owner 级联）。升 operator 时 Pool spec 原样、Server 原语新增——增量而非重构，与 §1.5 的判断一致。

### 1.6.8 边界：外部 SM watcher 是 1:N 的独有优势（v1.5.2，采纳评审）

`EXTERNAL_SM_WATCHER_ENABLED` + `<base>/watcher/sm_util.config`（节点级共享，见核心设计 §4.3.3 会话目录布局）
让**一个外部 watcher 进程**替所有会话采样 NVML 利用率，各会话子进程 mmap 同一份共享缓存——驱动探测开销
从 O(会话数) 降到 O(1)。这与共享令牌桶的"采样所有权"（同会话内 O(1)）是两个层次：桶解决**会话内**多进程，
外部 watcher 解决**跨会话**。

| | 1:N（单 server/节点） | 1:1（server-per-pod） |
|---|---|---|
| 外部 SM watcher | ✓ 节点上一个 watcher pod/sidecar，`<base>/watcher/` 经 hostPath 或同 pod 卷共享给全部会话 | **✗ 每个 server pod 各有自己的 NVML 路径**：pod 隔离恰恰切断了共享缓存的前提；每 pod 内的会话共享令牌桶仍可减到 pod 内 O(1)，但**跨 pod 是 O(server 数)**  |
| 驱动探测开销 | O(1)/节点 | O(1:1 server 数)/节点 |
| 结论 | 高密度小分配场景（很多会话/节点）的实质优势 | 分配数少（专属服务本就少）时代价可接受；若走 §1.6.4 形态 (c) 一 pod 多 server，则可回到 O(1)，但 (c) 不推荐 |

**这条要进 1:N vs 1:1 的取舍表**：它给"什么时候用 1:N"提供了一个此前没写的硬理由——**每节点会话越多，1:N
的 NVML 开销优势越大**；1:1 只适合"每节点 server 数少"的场景，与 §1.6.2 的场景归属一致（专属/独占本就不会
在一个节点上塞很多）。hostNetwork 形态 (b) 下的 1:1 理论上可以让多个 server pod 共享一个 hostPath 的
`watcher/`，但 watcher 需 hostPID 看到所有 server 的进程且各 server 的 pids 归属仍分开——**属于将来 1:1
落地时的可选优化，非默认**。

## 2. 主路径：DRA

### 2.1 设备上报（agent → ResourceSlice）

GPU 节点 agent（**v1.2：= kubelet-plugin `--mode=server`，见 §1.5.1，非独立组件**）发布远程池 ResourceSlice：

- `spec.nodeSelector`：**编码网络可达域**——匹配携带本 server 可达域标签的节点（见 §6），
  这是"资源只在 GPU 节点上报、pod 却能调度到别处"的核心机制；调度器原生理解，无需改调度逻辑。
- `spec.pool`：每 GPU 节点一个 pool，generation 随设备变化递增。
- 每设备 attributes（供 CEL 匹配与注入层读取）：

| attribute                        | 类型          | 用途                                        |
|----------------------------------|-------------|-------------------------------------------|
| `vgpu-manager.io/type`        | string      | 显卡类型 `remote-vgpu`                        |
| `vgpu-manager.io/uuid`        | string      | 物理卡 UUID，agent 落盘配额时按它填 `devices[].uuid`  |
| `vgpu-manager.io/memory`      | int         | 可分配显存                                     |
| `vgpu-manager.io/cudaVersion` | **version** | 节点驱动支持的 CUDA 上限，版本匹配用（§4）                 |
| `vgpu-manager.io/endpoint`    | string      | lupine-server 端点，**IP 或域名均可**（D3），注入层原样拼接 |
| `vgpu-manager.io/netZone`     | string      | 所属网络域（与 nodeSelector 用的标签一致，冗余供审计）        |

切分模型（一卡多份额）复用现有 DRA可消费设备 本地路径的 vgpu 机制（`pkg/kubeletplugin/vgpu.go`），
远程池按同一套份额语义发布。

### 2.2 调度与版本匹配

- 可达性：ResourceSlice.nodeSelector 完成，无自定义调度逻辑。
- 版本（client ≤ server）：DeviceClass/claim 的 CEL selector 匹配 `vgpu-manager.io/cudaVersion >= <pod 最低需求>`。
  pod 最低需求的三档来源见 §4。
- 记账/防双分：DRA allocator 原生。多设备 claim 可跨 pool（= 跨 server）满足，
  需要同节点约束时用 claim constraints（matchAttribute）。

### 2.3 消费节点注入（kubelet-plugin 远程分支）

kubelet-plugin daemonset **扩展到所有可调度节点**（消费节点上无 GPU 依赖，仅做注入）。
NodePrepareResources 发现 claim 命中远程池时：

1. env：`LUPINE_SERVER`（按分配结果确定顺序拼接 endpoint 列表，**顺序即客户端设备序号**，
   必须确定性生成——设计 §6.8 边界）、`LUPINE_SESSION=<控制面签发令牌>`、**`LUPINE_DISABLE_LOCAL=1`（硬性）**、
   `LD_LIBRARY_PATH=/opt/vgpu/lupine/<ver>/lib`。
2. CDI 挂载：按 server 的 `cudaVersion` 从版本目录选 lupine-client 制品（D4，§4.3）。
3. **同步 EnsureSession（D2，v1.4）**：在本回调内对每台分配到的 server 调 agent `EnsureSession(token)`，
   全部确认才返回成功——NodePrepare 失败 = kubelet 事件 + 自动退避重试，屏障语义与 init 容器等价且更强
   （严格先于 pod 一切容器，包括用户自己的 init 容器）。顺带在此做版本比对（agent 返回其 cudaVersion）。
   **远程 pod 因此不需要任何 pod spec mutation**。
4. 限额 env（`CUDA_MEM_LIMIT_*` 等）**不注入**——远程模式配额的唯一来源是 GPU 节点会话目录（服务端权威）。

### 2.4 GPU 节点 agent

- **watch ResourceClaim**（过滤本 pool）：分配 → 建会话目录 + `WriteVGPUConfigFile` 落盘
  `<base>/<session>/config/vgpu.config`（复用 `pkg/config/vgpu` seqlock 写盘，`devices[].uuid` 填本节点分到的卡）；
  释放/pod 删除 → 删除会话目录（幂等；孤儿目录由库 fail-closed 兜底，无安全风险）。
- **EnsureSession 端点**（HTTP/gRPC，监听 endpoint 同网卡）：入参会话令牌；用令牌反查 claim
  （令牌为消费侧 plugin 生成的随机值，本身即能力凭证，D8），已落盘直接返回 ready，未落盘则现场落盘。
  可选强化：bound SA token + TokenReview（device-mounter 式），第一阶段不做。
- 会话令牌（**v1.2 = D8**）：消费侧 plugin 在 NodePrepareResources 生成随机值，写入
  `claim.status.devices[].data` 并注入 pod env；GPU 侧 agent watch claim 读取。
  **不用 pod UID**（可预测、可冒用，设计 §6.2.1）。落盘/会话目录 = pod 内共享 emptyDir（D9，§1.5.2）。

## 3. 兼容路径：extender + device-plugin（老集群，后行）

不支持 DRA 的集群走现有 annotation 协议扩展。**明确此路径的两处"造假"与代价**：

1. **假资源触发注入**：所有可调度节点的 device-plugin 上报占位资源（如 `nvidia.com/remote-vgpu`，大数额），
   仅为触发 kubelet Allocate 拿到 env 注入点；真实记账完全不在 kubelet。
2. **extender 全局记账**：agent 把设备表写成 CR；extender Filter/Bind 对 CR 做全局 bin-packing
   （版本匹配、zone 标签匹配在 Filter 内自实现），结果写 pod annotation；并发、调度器重启恢复、
   泄漏回收全部自维护——等于手写小型 allocator。
3. agent 侧改为 watch pod annotation 落盘（EnsureSession 机制不变，两路径共用）。

结论：机制可行（本地 vGPU 的 annotation 协议是现成底座），但长期维护成本显著高于 DRA，
仅为老集群保留；**agent 的落盘/EnsureSession/回收逻辑设计为与载体无关**（输入是"session → 设备+限额"，
不关心来自 claim 还是 annotation），两路径共享。

## 4. 版本匹配（client ≤ server）

三层，缺一不可：

**4.1 调度时（尽力筛选）**：pod 最低 CUDA 需求 → CEL 匹配 server `cudaVersion`。需求来源三档：
1. pod spec 显式含 `NVIDIA_REQUIRE_CUDA`（如 `cuda>=12.4`）→ webhook 解析转 annotation。
   **注意**：CUDA 基础镜像把该 env 写在**镜像配置**里，pod spec 不可见；webhook 不做 registry 内省
   （重、外部依赖），所以这只是"用户显式声明时的便利通道"。
2. 显式 annotation `vgpu-manager.io/min-cuda: "12.4"`——**推荐的主要声明方式**。
3. 都没有 → 不筛选，靠下两层兜底。

**4.2 启动前（NodePrepare 屏障，v1.4）**：plugin 在 EnsureSession 往返中获得 agent 的 cudaVersion
（或 HEAD server 读 `x-lupine-cuda-version`，`h2.cpp:440`），与需求比对，不符则 NodePrepare 失败，
事件明确指向版本。

**4.3 运行时（权威兜底，天然存在）**：pod 内 cudart 经 lupine 看到 server 驱动版本
（`cuDriverGetVersion` 透传），不满足时 cudart 自报 `CUDA_ERROR_INSUFFICIENT_DRIVER`。

**客户端制品（D4 → v1.3 由 D11/D12 具体化）**：版本目录 + `max{ver : ver <= server_cudaVersion}` 选择逻辑不变；
制品形态与分发方式见 §4.4/§4.5。lupine shim 走动态链接器查找（远程 pod 无驱动，它的 libcuda 就该是 lupine 的），
**非 LD_PRELOAD**。多 server 版本不一时取交集最低者。

**v2.2 补（2026-09）：上限取 min(节点驱动上限, server 二进制版本)**。lupine 客户端已不再做版本 pre-flight
（`rpc_http2_client_probe` 只剩测试在用，`x-lupine-cuda-version` 成了纯信息头），client 比 server 新会在运行时以
未知 opcode 失败，所以制品选择是唯一的守门。lupine #660 起 RPC 端口兼答 HTTP/1.x 且任意响应都带
`x-lupine-cuda-version`，dra-server 据此定期 GET `http://<endpoint>/`（5s 直到首次成功，之后 60s），把结果发布为
设备属性 `serverCudaVersion`；inject 侧 `DeviceInfo.CUDAVersion` = min(`cudaDriverVersion`, `serverCudaVersion`)。
属性缺失（server 尚未应答）时退回驱动上限并打 warning。remote-agent 的就绪探测也换成同一个 GET。

### 4.4 制品分发：镜像列表 → 节点版本目录（D12）

**动机**：lupine CI 按 CUDA 版本持续产出 client 镜像。若把制品固化进我们的 install 镜像，每次增减版本都要重建发版；
改为**cudaVersion → 镜像地址的映射表**驱动，增减版本 = 改列表。

**关键技术约束（决定机制形态）**：制品版本的选择依赖**分配结果**（server 的 cudaVersion），而分配发生在调度时；
**准入（webhook 注入 init 容器）在调度之前**，此时不可能知道 pod 会分到哪台 server → "逐 pod 按需选制品镜像
注入 init 容器"**不可行**（pod spec 的 init image 事后不可靠变更）。因此落点必须是**节点级异步物化**：

```
RemoteGPUPool.spec.lupine.clientImages:          # cudaVersion → image 映射（建议 digest 固定）
  - {cudaVersion: "12.9", image: ghcr.io/.../lupine-client-static:cuda-12.9.1@sha256:...}
  - {cudaVersion: "12.4", image: ghcr.io/.../lupine-client-static:cuda-12.4.1@sha256:...}
        │ helm 模板取所有 pool 的并集，渲染进消费侧 DS 模板
        ▼
consumer DS: initContainers[i] = 各版本镜像（CMD 即 cp /artifacts → hostPath/<ver>/）
        │ kubelet 原生拉取（pull secret/镜像仓库代理/缓存/离线环境全部走标准机制）
        ▼
节点 /var/lib/vgpu-manager/lupine/<cuda-ver>/   ← 注入层 CDI 挂载，选择逻辑照旧
```

**分工必须读清楚（易误读点）**：init 容器有两处，角色完全不同——
- **消费侧 DS 自己的 init 容器**（本节）：全版本制品物化到节点 hostPath，**每节点每次 rollout 一次**，
  与任何用户 pod 无关；
- **目标 pod**（v1.4）：**不被注入任何容器**——EnsureSession 屏障在 NodePrepareResources 内同步完成（D2），
  制品经 CDI 挂载 hostPath 对应版本目录，版本选择发生在 NodePrepare（分配已知）。

- **不在 pod 启动关键路径上**：物化随 DS rollout 异步完成；pod NodePrepare 时目录缺失（新节点赶上 rollout）
  → 返回可重试错误，kubelet 自动重试。
- **天然支持一个 pod 多容器、各配不同 CUDA 版本**：CDI 挂载是 per-container 的，NodePrepare 按各容器
  claim 的分配结果分别选目录——这正是"节点级全版本物化 + 分配时选择"优于任何"pod 级带版本"方案的地方
  （后者必须在准入时预测落点，做不到）。
- 列表变更 → helm upgrade 更新 DS 模板 → 滚动 rollout 重物化；移除版本由 plugin 启动时对账 GC。
- **被否掉的替代**：给目标 pod 注入制品 init 容器（准入先于分配无法选版本，只能全版本都挂，pod 被塞进
  N 个 init 容器——不可接受）；运行时 registry 拉取（绕过 kubelet 的 pull secret/镜像代理体系，
  把 registry 可用性引入 pod 启动路径）。

### 4.5 制品形态：自包含静态构建（D11，已实现）

**问题背景（三个被搅在一起的正交轴，实验教训记录）**：
1. *文件分发*（host 挂载 vs sidecar/emptyDir 拷贝）——sidecar 只能改变文件来源，**共享文件 ≠ 共享运行时**，
   .so 始终由目标容器的 loader 用目标容器的 glibc/搜索路径加载；
2. *依赖解析*——动态版 client 依赖 libnghttp2/libssl/libstdc++；靠目标镜像=碰运气；挂载依赖 + `LD_LIBRARY_PATH`
   =**进程全局搜索路径污染**（实测：ollama 官方镜像里 llama-server 因此崩溃，"Bad address"——它的每次库解析
   也先搜我们的目录）；
3. *版本选择*——D4 已解决。
**所有失败都在轴 2**；解法是让制品**不需要任何搜索路径可见的依赖**。

**实现（fork 提交 `81c51a4`，lupine 源码零逻辑改动）**：
- CMake 新增 `LUPINE_STATIC_DEPS`：nghttp2（1.64.0）/OpenSSL（3.0.18）/libstdc++/libgcc 静态内嵌
  （lz4 上游本就静态 vendor——同一哲学的既有先例）；现有 `client.exports` 版本脚本天然把内嵌符号藏出
  `.dynsym`，与目标镜像同名库**零符号冲突**。
- **rockylinux8 统一构建底座**（用户确认：全矩阵统一，不为 11.x 单开 ubuntu18.04）：glibc 2.28 是 NVIDIA
  全矩阵（11.7–13.1，amd64+arm64，已逐 tag 核实）发布 devel 镜像的最低地板——12.x/13.x 无更低选择，
  11.x 的 ubuntu18.04（2.27）仅省 0.01 却带 gcc7/cmake3.10 问题，不值。CUDA 13 用 gcc-toolset-13
  （其 libstdc++ 增量设计上就是静态链接，地板不抬）。实测产物 glibc 上限 2.27/2.25，低于声明地板。
- **首轮 CI 踩坑（已修）**：Rocky 8 的 `libstdc++-static` 在默认禁用的 **PowerTools** 仓（Ubuntu 的
  libstdc++-dev 自带 .a），`-static-libstdc++` 因此 `cannot find -lstdc++` 全矩阵链接失败。BuildKit 只给
  "exit code 2"，靠 rockylinux:8 chroot + 真实 RH gcc 8.5/binutils 2.30 复现定位；
  fix `--enablerepo=powertools libstdc++-static`（fork commit af71914）。
- `deploy/check_static_client.sh` 构建期自检：DT_NEEDED 仅 glibc / .dynsym 无内嵌符号泄漏 /
  TLS 静态存在（防 `find_package(OpenSSL QUIET)` 静默降级）/ glibc 上限 ≤ 声明地板；
  另有 rocky8-minimal 裸镜像（=地板本身）RTLD_NOW 加载探针阶段。
- CI：`.github/workflows/publish-client-static-image.yml`，全矩阵产出
  `lupine-client-static:cuda-<ver>` 镜像（busybox 载体，`/artifacts` + cp 入口，即 §4.4 init 容器直接可用）。
- **本地已全链路验证**（CUDA 12.9 redist 工具链 + 真实源码编译）：DT_NEEDED 仅
  `libc.so.6`/`ld-linux`，466 个 `cu*` + dlsym 导出完好，TLS 静态在位，RTLD_NOW 加载通过，
  自检脚本对动态库正确拒绝。
- **注入面因此无害化**：挂载目录里只有 `libcuda.so.1`/`libnvidia-ml.so.1`，容器内被遮蔽的名字仅此二者——
  远程 pod 本无真驱动，这正是产品语义。边界：glibc-only（musl/alpine 镜像不支持，与库同一约束）；
  pod 镜像不得自带真 libcuda。

### 4.6 平台制品：Windows/macOS 载体镜像（D14，fork 已实现 workflow）

上游 2026-08 新增原生 Windows DLL（`nvcuda.dll`/`nvml.dll`）与 macOS dylib 客户端。它们服务**集群外接入**
（开发机直连 GPU 池，即 D13 `RemoteGPUServer publish=false` 场景）；**k8s 注入路径永远用不到**（Linux pod
只能加载 ELF），故不影响 D12 的镜像列表机制，是额外的分发面。

要点（fork `publish-client-platform-images.yml`）：
- **编译进不了容器**：Windows 要 MSVC（原生 runner）、macOS 要 Apple 工具链（SDK 许可实际排除容器内
  osxcross）。原生 runner 编译 → artifact 汇集 → Linux job 打包 busybox 载体（/artifacts + cp 入口，与
  Linux 载体同形态）推 GHCR。
- **头文件钉版本**：不用上游的无版本 pip wheel，改用与 Linux Dockerfile 相同的 NVIDIA redist 归档
  （cudart/nvml_dev/profiler_api，headers 平台无关），保证 client≤server 规则可执行。
- **自包含**：Windows 上游天然全静态（vcpkg *-windows-static + MT 运行时），直接复用；macOS 的 brew
  依赖是动态的（/opt/homebrew 路径会随制品泄漏），改为源码静态编 nghttp2/OpenSSL，workflow 内置
  `otool -L` 门禁（仅允许 /usr/lib 系统库）。
- **v1 范围**：Windows x86_64 + macOS arm64（Apple Silicon）；Windows arm64 / macOS x86_64 结构留位。
- **server 不做多平台**：隔离库是 glibc/LD_PRELOAD 专属，Windows server 无法承载 libvgpu-control，非产品路径。
  client 与 server 也不合并进一个镜像（可运行镜像 vs 文件载体，角色不同）。
- **验证边界**：Windows/macOS 客户端与隔离 server 的会话语义共享 h2.cpp 代码（LUPINE_SESSION env 同样生效），
  但不在现有验证矩阵内——wire 兼容、Windows CUdeviceptr 语义等列为集群外接入场景验收项，非默认可用。

## 5. 配额落盘时序（D2）

```
调度: allocator 绑定 claim
消费节点: kubelet-plugin NodePrepare ──► 生成随机令牌写入 claim.status (D8) + 注入 env
                                   │
GPU 节点 agent watch claim ────────┤ (主通道，通常 <1s 落盘)
                                   ▼
消费节点: kubelet NodePrepareResources ──► plugin: 生成令牌写 claim.status
                                   ├──► 逐 server 调 agent.EnsureSession(token)
                                   │      已落盘 → ready；未落盘 → 反查 claim 现场落盘 → ready
                                   │      任一失败 → NodePrepare 返错，kubelet 退避重试
                                   ▼
        NodePrepare 成功 → pod 一切容器才开始创建 → 首个 CUDA 调用（配额必定已就绪）
                                   ▼
        lupine 子进程 provider restore() → 读会话目录 → 隔离生效
```

- 确定性来自 NodePrepareResources 语义：它严格先于 pod 的**一切**容器（含用户自己的 init 容器——
  这点 init 容器屏障反而做不到：与用户 init 容器同队列，排序可被打破）。
- **损失与补偿（multus/SR-IOV 形态）**：plugin 在主机网络，无法从 pod netns 验证数据面连通性；
  EnsureSession 是控制面调用（plugin→agent 走集群管理网，agent 双网监听），数据面故障由首个 CUDA 调用
  清晰报错兜底；需要时可提供**可选**诊断 init 容器作排障工具，非常态链路。
- 竞态彻底消除后，provider fail-closed 从"常态防线"退为"纵深防线"（孤儿/伪造 session 仍拒）。
- 回收：claim 释放 → agent 删会话目录；agent 崩溃重启 → list+watch 全量对账（落盘幂等）。

## 5.1 端点交付与鉴权（D15，v1.6.1 修订：不改 lupine 源码）

> **v1.6 曾提议照搬 tensor-fusion 的"长轮询 HTTP + TokenReview"；v1.6.1 作废该提议**——它需要
> lupine client 主动拿 pod 的 SA token 去 HTTP 调控制面，**必须改 lupine client**，违反"尽量不改 lupine
> 源码"约束。下面是约束内可达的等价方案。tensor-fusion 原做法见 `docs/tensor_fusion_analysis_and_lessons.md` §3.1。

### 5.1.1 端点交付 = 域名注入 + 控制面 DNS（提升 D3/§6 的域名选项为主力）

lupine 唯一的 client→server 通道是 `LUPINE_SERVER`（端点）与 `LUPINE_SESSION`（会话头），都是 env，
我们能控制注入值但不能让 client 主动回调控制面。因此端点交付走**被动可解析**而非主动拉取：

- 注入层给 pod 注入**域名** endpoint（如 `gpu-server-<serverID>.<zone>.vgpu.internal:14833`），**不注入裸 IP**；
- 控制器（watch server pod / `RemoteGPUServer` CR）维护 **name→IP 映射**，写进控制面管理的 DNS
  （CoreDNS custom zone / external-dns / 自建轻量 DNS 组件，作为 vgpu-manager 的新 controller，D18）；
- lupine 两侧均 `getaddrinfo`（已核实），域名天然可用；解析发生在首次建连。server 换 IP / 重启后，会话本就
  作废、应用重连时**重新解析拿到新地址**——不改 lupine 一行。

这一路顺带满足三处此前的硬约束：**① TLS 主机名校验要求域名**（§6.1）；**② SR-IOV/multus 次网卡 IP 不被 k8s
Service 覆盖**（§6），自维护 DNS 正是补这个；**③ server IP 漂移**。
**就绪屏障回到 D2**（NodePrepare 内同步 EnsureSession）——长轮询本是 D2 的替代，替代作废即回到 D2。

### 5.1.2 鉴权 = 签发时绑定 pod + provider restore() 服务端校验（不改 lupine）

**为什么 TokenReview-at-connection 做不了**：tensor-fusion 成立是因为它的 client shim 拿 pod SA token 主动
HTTP 调 operator。我们不改 lupine client，而 **lupine 不转发任何 SA token**——client→server 只有
`LUPINE_SESSION`（`h2.cpp:737` 读 env → `x-lupine-session` 头 → provider `restore(connection_id)`，
`checkpoint_provider.c:282` 收到,`connection_id == LUPINE_SESSION`）。所以"连接时用 pod 身份 TokenReview"
被物理堵死。约束内可达的最强模型分两层：

1. **签发时绑定（mint-time binding）**：控制面创建会话目录时就知道该 session 发给哪个 pod，把
   `session → podUID/claim/过期时间` 记进会话记录。token 本身**随机、不可预测、单 pod、短时效**。
   泄露面 = pod env（同 D8），但多了绑定元数据可供服务端二次校验。
2. **服务端校验由 library checkpoint 触发**（provider `restore()` 是**我们的代码**，非 lupine）：现状仅
   "session 消毒 + 会话目录/配额存在即放行"；加强为——再校验控制面写下的会话记录**仍有效、未过期、绑定的 pod
   仍存活**（读会话目录里 agent 落下的签名/带过期记录，或 provider 回调控制面校验），不满足返回非 0 →
   fail-closed 拒连。**全程不碰 lupine**（provider 在 GPU 节点、是我们的 .so）。

**安全强度**：达不到 tensor-fusion "pod 用自己 SA token 自证"（那需改 client 转发 token），但显著强于裸
bearer token——token 绑 pod、带过期、服务端二次校验存活性。**可选增强（需上游改动）**：改 lupine client
把 pod SA token 作为额外握手头转发，则 provider 可做真正的 TokenReview——列为长期项，非当前范围。

### 5.1.3 与 D2 的关系

不互斥：**D2 的 NodePrepare EnsureSession 保证"配额已落盘 + session 已登记"**（GPU 侧就绪屏障），
**5.1.1 的 DNS 保证 endpoint 可解析**（网络侧）。1:N 主线 endpoint 仍可静态进 ResourceSlice attribute
（域名形态）；1:1 endpoint 动态，经 CR status 回填域名后由 DNS 控制器发布。两拓扑都不需要改 lupine。
## 6. 服务发现与网络可达（D3）

**endpoint 值按部署形态填，注入层不区分（attribute 原样拼接）**：

| server 部署形态 | endpoint 填法 | 说明 |
|---|---|---|
| hostNetwork + 节点 underlay 网卡（推荐先行） | 节点 underlay IP | IP 是节点属性，pod 重启不变，无 DNS 依赖 |
| multus/SR-IOV 独立网卡 pod | agent 维护的域名（CoreDNS custom / external-dns） | **k8s Service 不覆盖次网卡 IP，headless service 不可用**；须自维护 A 记录 |

- lupine 两侧均走 `getaddrinfo`，域名可用；解析发生在进程首次建连。server 重启杀死全部会话子进程
  （连接态不可恢复），应用重启后自然重新解析——DNS 的增量价值仅覆盖"server 换 IP 且 pod 之后才重启"窗口。
- **可达性进调度**：网络域标签规范——GPU 节点 `vgpu-manager.io/net-zone=<zone>`（server 所在域），
  可达节点 `vgpu-manager.io/net-zone.<zone>=reachable`；slice.nodeSelector 匹配后者。第一阶段运维人工标注，
  探活组件可选后补。

## 6.1 传输加密（TLS）——lupine 的支持是单边的

**核查结论（2026-08-14，读 lupine 源码）：客户端能说 TLS，服务端不能。**

| 组件 | TLS | 证据 |
|---|---|---|
| `libcuda.so.1` 客户端 shim | ✅ 链 OpenSSL，认 `https://` | `client.cpp:8237-8253` 解析 scheme，`583-605` 建 TLS |
| `libnvidia-ml.so.1`（`nvidia-smi` 走这条） | ✅ 独立但等价实现 | `nvml_client.cpp:125`、`178-199` |
| `lupine_driver_server` | ❌ **纯明文** | `CMakeLists.txt:40` 注释 "The server stays plaintext (front it with a TLS proxy); only the client links OpenSSL"，服务端 `target_link_libraries` 无 OpenSSL |

所以 lupine 的加密形态是**服务端前置 TLS 终止代理**，客户端 `https://` 连代理，代理明文连 server。

**配置面只有一个开关**——`LUPINE_SERVER` 的 URL scheme，server 侧无任何 TLS 配置项：

```
LUPINE_SERVER=gpu-node:14833            # 明文，默认端口 14833
LUPINE_SERVER=https://gpu.example.com   # TLS，默认端口 443
LUPINE_SERVER=https://a.example.com,http://b:14833   # 多 server 逐个独立指定，可混用
```

**校验行为是严格的，且没有关闭开关**（`client.cpp:585-596`）：`TLS1_2_VERSION` 下限 + 系统信任库
（`SSL_CTX_set_default_verify_paths`）+ `SSL_VERIFY_PEER` + SNI（`SSL_set_tlsext_host_name`）+ **主机名校验**
（`SSL_set1_host`）。没有 `LUPINE_TLS_INSECURE` 之类的逃生门。自签证书必须进节点系统信任库，否则握手失败直接拒连。

### 对本设计的三处影响

1. **决策 3（endpoint 形态）需要修正**：`SSL_set1_host` 做主机名校验，意味着**启用 TLS 后 endpoint 必须是域名**
   （除非证书签了 IP SAN）。这与"hostNetwork underlay IP 直注先行"直接冲突——§6 表格的推荐形态在 TLS 下不成立。
   落法：`vgpu.io/endpoint` attribute **携带 scheme**（`https://host` / `host:port`），注入层原样拼进
   `LUPINE_SERVER`，是否加密由 agent 发布 slice 时决定，注入层仍然不区分。
2. **令牌明文传输是当前安全模型的一个前提缺口**：`LUPINE_SESSION` 作为 HTTP/2 头 `x-lupine-session` 发送。
   不启用 TLS 时，**同网段抓包即可取得令牌并冒用他人配额**——§7 "泄露面 = pod env" 的表述不完整，明文链路上
   还有一条网络侧泄露面。这是 TLS 从"可选加固"变成"多租户场景必需"的理由。
3. **客户端制品必须是带 TLS 编译的**：`LUPINE_TLS_OPENSSL` 由 `find_package(OpenSSL QUIET)` 决定
   （`CMakeLists.txt:148/165`），**QUIET 意味着构建机没有 OpenSSL 时静默降级**，产出的客户端遇到 `https://`
   会报 "built without TLS support" 并拒连。版本目录（决策 4）铺设的制品需要校验这一点。

### 部署代理时的两个坑（源码级）

- **无 ALPN 协商**：全项目搜不到 ALPN 代码，客户端 TLS 握手完直接跑 HTTP/2。代理必须**无条件按 h2 处理**该端口，
  不能依赖 ALPN 协商。nginx 的 `listen ... ssl http2` 依赖 ALPN，可能握不上；envoy 显式
  `http2_protocol_options`、或 nginx `stream` 四层 + TLS 终止更稳。
- **`:scheme` 伪头恒为 `"http"`**（`h2.cpp:714`），走 TLS 时也不改。严格校验伪头与传输层一致性的代理会拒绝。

> 阶段建议：K1 先明文（单租户/可信网络验证闭环），**多租户或跨信任域前必须上 TLS**——否则 §7 的令牌能力模型
> 在网络层是空的。

## 7. 安全模型

- 会话令牌：消费侧 plugin 生成的随机值（非 pod UID，D8），是 EnsureSession 的能力凭证与 `LUPINE_SESSION` 本体；
  泄露面 = pod env + claim status 读权限（同租户信任边界），明文链路另有网络侧泄露面（§6.1 影响 2）；
  风险 = 冒用他人配额（需先拿到令牌）。
- 服务端权威：配额唯一来源是 agent 落盘的会话目录；客户端 env 不参与限额（§2.3 第 4 条）。
- fail-closed 链条不变：无 session/无配额/空 allowlist → 拒连（库侧已实现并测试）。

## 7.1 高性能网络承载（IB / SR-IOV / RoCE / DPDK）

远程 GPU 的体验上限就是网络，所以这一节先把**lupine 的传输实现**钉死，再谈方案——因为它决定了哪些是"改配置"、
哪些是"改 lupine"。

### 7.1.0 前提：lupine 的传输是什么（源码核查）

| 事实 | 证据 | 推论 |
|---|---|---|
| `socket(AF_INET, SOCK_STREAM)`，`getaddrinfo` 也钉 `AF_INET` | `server.cpp:539`、`rpc.cpp:35-36`、`nvml_client.cpp:150` | **纯 IPv4 TCP**。无 IPv6，无 `AF_RDMA`/`AF_SMC`。任何"换地址族"的方案要么靠 preload 劫持，要么要改源码 |
| 收发用 `sendmsg`/`recvmsg` + iovec，`TCP_NODELAY` 已设 | `lupine_platform.h:251`、`h2.cpp` 的 `h2_write_all` | 标准 socket 语义，没有 io_uring / 自定义栈 |
| HTTP/2 窗口已按高 BDP 调过：客户端 `0x7fffffff`（~2GB），服务端 64MB，最大帧 16MB−1 | `h2.cpp:25-31`、`rpc.h:36` | **流控不是高速链路上的瓶颈**，无需调优 |
| 设备传输经**锁页主机内存**中转（`cuMemAllocHost`，失败回退 `malloc`） | `manual_server.cpp:764-772` | 数据面是 `GPU → 锁页主机内存 → TCP → 主机内存 → GPU` |
| lupine 自身代码**无 verbs / RDMA / dmabuf** | 全仓库无 `ibv_*`；`cuFlushGPUDirectRDMAWrites` 只是被代理的 CUDA API，不是 lupine 用 GPUDirect | **没有 GPUDirect**，两次主机内存落地是架构性的 |

### 7.1.1 方案分层

按"要不要动 lupine"分成两类，这是选型的第一刀：

**A 类：透明加速（lupine 零改动，纯部署/运维）**

| 方案 | 机制 | 对 lupine | 评价 |
|---|---|---|---|
| **SR-IOV** | pod 直通 VF，绕过 CNI overlay 与 host 协议栈转发 | 无（VF 就是普通 netdev） | **性价比最高，首选**。省掉 overlay 封装与 veth/bridge 跳数；server 已 `INADDR_ANY` 监听（`server.cpp:566`），多网卡天然可用 |
| **IPoIB** | IB 网卡跑 IP 层 | 无（普通 netdev） | 有 IB 组网时的直接选择。带宽好，但仍走内核 TCP，延迟/CPU 不如原生 verbs |
| **RoCE + SMC-R** | 内核 SMC 协议族透明替换 TCP，走 RDMA | 无（`smc_run` 用 LD_PRELOAD 把 `AF_INET` 换成 `AF_SMC`） | 唯一"不改代码却能吃到 RDMA"的路子。需内核 SMC 模块 + RoCE 网卡；与我们的 preload 共存需验证（见下） |
| **rsocket preload** | `librspreload.so` 劫持 socket 调用到 RDMA | 无（同为 LD_PRELOAD） | 同上，成熟度与运维熟悉度通常不如 SMC-R |

**B 类：需要改 lupine 传输层（不是部署能解决的）**

| 方案 | 为什么不透明 |
|---|---|
| 原生 verbs（`ibv_*`） | 要重写 `rpc.cpp`/`h2.cpp` 的收发，且 HTTP/2 帧语义要重新映射到 RDMA 消息 |
| DPDK | 内核旁路用户态网络，应用得基于 DPDK 上的 TCP 栈（F-Stack/VPP/Seastar）重写；与容器网络、k8s Service 模型都不兼容 |
| **GPUDirect RDMA** | 需要 lupine 用 verbs 注册**显存**（nv_peer_mem / dmabuf）做零拷贝 |

### 7.1.2 关键判断：A 类是"更粗的管子"，只有 GPUDirect 是"换架构"

A 类方案能提升的是**主机内存之间**那一段。但数据面固有地要落两次主机内存（7.1.0 最后两行），所以：

- 对**控制面密集**型负载（大量小 RPC，如 kernel launch、事件查询）：A 类的延迟收益直接兑现，SR-IOV/RoCE 都有效。
- 对**数据面密集**型负载（大块 HtoD/DtoH，如模型加载、大 batch 输入）：A 类只能把网络那一跳变快，
  两次主机内存拷贝和 PCIe 往返仍在。**收益会撞上天花板。**
- 真正拆掉天花板的是 GPUDirect RDMA（显存直接进出网卡，不落主机内存），但它属于 B 类——**是给 lupine 提 PR 或
  维护分叉的量级，不是我们部署侧能决定的**。

结论：**优先 SR-IOV（+ IB/RoCE 组网时叠 IPoIB 或 SMC-R），把 GPUDirect 记为长期项而非选型项。**

### 7.1.3 与本项目的四个具体交互点

1. **preload 共存**：我们已经把 `libvgpu-control.so` 进程级 `LD_PRELOAD` 进 lupine-server。SMC-R/rsocket 的加速
   也是 LD_PRELOAD。两者拦截的符号集**不相交**（我们只导出 `cu*`/`nvml*`/`dlsym` + provider 入口；它们拦
   `socket`/`connect`/`send`/`recv`），且我们的 `dlsym` 拦截器对非 `cu`/`nvml` 前缀一律回退 glibc 真 dlsym
   （`loader.c` 拦截器），所以它们经 `dlsym(RTLD_NEXT, "socket")` 取真函数不受影响。**理论上可共存，但必须 spike 实测**——
   这是我们唯一需要亲自验证的交互。
2. **SR-IOV 与 endpoint 形态**：VF 的 IP 不在 k8s Service 覆盖范围内（§6 已记录 multus 形态的 DNS 自维护问题）。
   叠加 §6.1 的 TLS 主机名校验要求，**SR-IOV + TLS 组合下 endpoint 必须是自维护 DNS 的域名**。这两条约束是叠乘的，
   选型时要一起看。
3. **可达性标签**：§6 的 `net-zone` 标签语义天然覆盖高性能网络——IB/RoCE 域就是一个 zone，非该域节点不可达。
   不需要为高性能网络新增调度机制。
4. **MTU 与分片**：IPoIB/RoCE 通常配 jumbo frame。lupine 已设 `TCP_NODELAY`（小帧不等 Nagle），大块传输走
   iovec `sendmsg`，两者都不与 jumbo 冲突，无需改动。

### 7.1.4 验证方法（避免测出假象）

- **必须 `LUPINE_DISABLE_LOCAL=1` 或用无 GPU 客户端**，否则客户端本地路由会让测试完全不过网络（§6.8 边界 1，
  这个坑已经踩过一次）。
- 分开测两类负载：小 RPC 往返延迟（控制面）与大块 HtoD/DtoH 带宽（数据面）。A 类方案在两者上的收益形状不同，
  混在一起测会得出误导性结论。
- 基线用 `LUPINE_RPC_STATS`（见 `docs/lupine_env_reference.md`）取 RPC 计数与耗时分布，再对比网络方案。

## 7.2 集群外接入的中继边界（D19）

§7 的信任边界建立在**消费者是集群内 pod**这一前提上：它有 pod 身份（D15 签发时绑定 pod、provider `restore()`
服务端校验），令牌泄露面收敛在同租户 namespace。`publish: false`（§1.6.7）的**集群外消费**打破了这个前提：

| 集群内 DRA 消费者 | 集群外消费者（`vgpu use` / 跨集群） |
|---|---|
| 有 pod 身份，令牌签发时可绑定 podUID（§5.1.2） | **无 pod 身份**——签发时无可绑定的 k8s 主体 |
| 令牌泄露面 = 同租户 namespace 内的 env+claim status | 令牌走集群外链路，泄露面外扩 |
| 经集群内网络到达 server，端口不出集群 | 若直连 = 把多租户共享 server 端口**裸暴露给集群外** |

而 lupine 服务端**无 TLS、无逐连接身份**（D5、§6.1）：直连共享 server 端口意味着任何拿到 endpoint 的集群外主体
都能试连同一个多租户端口面，没有 k8s 原生 authN 挡在前面。

**决策：集群外消费一律经每会话的单租户中继 pod，不直连共享 server。**中继 pod 是外部连接的**唯一落点**
（`kubectl port-forward` 的目标，或专用 Service/LB endpoint），职责：

1. **终止外部传输**：外部只触达中继，永不触达共享 server 端口；
2. **TLS 终止**：中继作前置终止代理，补上 D5 缺口（server 侧无 TLS），集群内段可保持明文或内网 mTLS；
3. **令牌校验后转发**：中继持有本会话令牌，校验通过后经集群内网络（同 §5.1 的 `LUPINE_SESSION` 语义）转发到共享 server；
4. **单租户隔离**：一会话一中继 = 信任域/崩溃域按会话隔离，生命周期绑定会话，会话结束即回收。

这样集群外主体的"身份"退化为**"能否建起这条中继"**——中继由控制面按 `publish: false` Server 的授权创建，
等价于把"签发时绑定 pod"替换为"签发时绑定中继实例"，provider `restore()` 的服务端校验（§5.1.2）不变。
集群内 DRA 消费者**不经此路**：它们已有 pod 身份，直接走 §5.1，中继只服务集群外这一条边。

## 8. 改造面与阶段

| 组件 | 改造 | 量级 | 阶段 |
|---|---|---|---|
| kubelet-plugin `--mode=server` | 远程池 slice 发布 + claim watch 落盘/回收 + EnsureSession（吸收原 agent，§1.5.1） | **大** | K1 |
| kubelet-plugin `--mode=inject` | 注入（env/CDI/init 容器）+ 令牌生成写 claim status（D8） | 中 | K1 |
| **chart（helm 模板）** | 池 DS/消费侧 DS（含集群级归并 §1.5.4 与制品 init 列表 §4.4）/DeviceClass/RBAC；TLS 时 cert-manager 模板 | 中 | K1（先只 hostNetwork profile），随阶段增强 |
| operator + CRD | **后置**：组件稳定、边界明确且 helm 不够用时；values schema 即 CRD schema | 大 | 待定 |
| server 容器镜像 | lupine-server + libvgpu-control.so 打包（emptyDir 会话目录，D9） | 小 | K1 |
| install（消费侧 DS 的 init） | lupine-client 版本目录铺设 | 小 | K1 |
| webhook | 默认档 UX 转换（§1.5.6）+ `NVIDIA_REQUIRE_CUDA`/annotation → 版本需求 | 小 | K2 |
| extender 兼容路径 | 假资源 + CR 记账 + annotation 落盘 | 大 | K3（老集群需求明确后） |

- **K1（最小闭环）**：单 zone、单 server/pod，agent+注入+NodePrepare 屏障跑通端到端。
- **K2**：版本匹配三层、多 server 组合（验证 §6.8 边界 6 的 cuda:i==nvml:i 实测项）、回收对账。
- **K3**：extender 兼容路径（按集群版本分布决定是否启动）。

## 8.1 spike 先行（D22，v1.7）：K1 工程化前的两级验证

> 动机：K1 的最大不确定性集中在两处——**DRA 调度器是否如设计预期消费 NodeSelector 型远程池**、
> **注入面三件套 + 制品挂载能否让无 GPU pod 真正跑通远程 CUDA**。这两处都能以远小于 K1 的代价先行证真/证伪，
> 失败时修改的是设计而非已成型的工程代码。

**S0：调度链路验证（纯 YAML，零代码）**
- 手工创建：`DeviceClass`（CEL 匹配 `type=="remote-vgpu"`）+ **手工 ResourceSlice**（`spec.nodeSelector`
  编码 net-zone 标签；设备带 §2.1 全套 attributes 与 cores/memory capacity、`allowMultipleAllocations: true`，
  份额语义与本地 vgpu 对齐）+ ResourceClaim + 无 GPU 节点上的消费 pod。
- 验收：claim 被 allocator 绑定到手工 slice 的设备；pod 被调度到 net-zone 可达标签节点（而非 GPU 节点）。
- 已知边界：消费节点上无插件注册，NodePrepareResources 必然失败，pod 停在 ContainerCreating——**这正是 S0 的
  终点**（调度与记账已验证，注入属 S1）。注意手工 slice 需填 `spec.driver=manager.nvidia.com` 且避开真插件
  同 pool 名冲突（GPU 节点插件不感知该 pool，天然无冲突）。
- 附带验证项：`allowMultipleAllocations` 多 claim 份额扣减、CEL 版本匹配（`cudaVersion >= 需求`）。

**S1：注入链路验证（最小 `--mode=inject` + 手工会话落盘）**
- 代码面（刻意最小）：kubelet-plugin 新增 inject 初始化分支（无 NVML/健康检查/NRI）；NodePrepare 识别远程
  claim（allocation results 命中远程 pool / `type==remote-vgpu` attribute）后仅做：CDI env 注入
  （`LUPINE_SERVER`=slice `serverEndpoint` attribute 确定性排序拼接、`LUPINE_SESSION`=令牌、`LUPINE_DISABLE_LOCAL=1`；EnsureSession 走 `agentEndpoint` attribute；制品上限 = min(`cudaDriverVersion`, `serverCudaVersion`)）
  + CDI 挂载**手工铺好的** client 制品目录（版本目录布局同 D12，内容手工 cp）。spike 阶段令牌可取 claim UID
  派生值并**不写 claim status**（D8 的 status 写入与幂等留到 S2）。
- GPU 节点侧全手工：起 lupine-server（LD_PRELOAD libvgpu-control.so + `LUPINE_CHECKPOINT_LIBRARY` 指向它），
  用 `library/tools/session_config.c`（`vgpu-session-config --session <token> --device <uuid>,mem=..,core=..`）
  预先落盘会话配额——**跳过 claim watch、EnsureSession、chart 的全部工程量**。
- 验收（对应核心设计验收项）：无 GPU pod 内 CUDA 程序经远程执行；`cuMemGetInfo`/nvidia-smi 呈限额视图；
  超限 OOM；伪造/缺失 session fail-closed 拒连。
- 明确不验证：落盘时序屏障（session 是预先手工落的）、回收、多池归并、TLS。

**S2 起 = K1 工程化**：S0/S1 结论回填设计后，按 §8 表推进——server 模式（slice 发布 + claim watch 落盘/回收 +
EnsureSession gRPC 服务端，D20）、inject 模式补齐（令牌写 claim status + NodePrepare 内同步 EnsureSession，D2/D8）、
独立 chart（D7 v1.7）、server 镜像与制品 init 列表。

## 9. 风险与待定

| 风险 | 缓解 |
|---|---|
| DRA 版本门槛（k8s ≥1.32） | K3 兼容路径兜底；集群版本分布待盘点 |
| 多 server 时 LUPINE_SERVER 顺序不确定 → 设备序号漂移 | 注入层按 claim 结果排序后固定；写入测试 |
| CUDA/NVML 两表连接数不一致错位（§6.8 边界 2） | NodePrepare 内逐 server EnsureSession 全通才放行（控制面）；数据面错位由首个 CUDA 调用报错暴露（v1.4 取舍，§5） |
| server 重启会话不可恢复 | 固有约束（lupine 连接态）；文档明示，应用层重试/重启恢复 |
| agent 落盘与回收竞态（同名 session 快速重建） | 令牌随机不复用；目录以令牌命名，天然不撞 |
| multus 形态 DNS 自维护成本 | D3 允许 IP 直注先行，DNS 仅该形态启用；**但启用 TLS 后 IP 直注不成立**（主机名校验，§6.1），两者叠乘时必须上 DNS |
| 明文链路上会话令牌可被抓包冒用 | §6.1：多租户/跨信任域前必须启用 TLS；K1 单租户可明文 |
| lupine 客户端制品可能未带 TLS 编译（`find_package(OpenSSL QUIET)` 静默降级） | 版本目录铺设时校验制品含 TLS 支持（§6.1） |
| TLS 代理配置不当（无 ALPN、`:scheme` 恒为 http） | §6.1 部署坑；优先 envoy 显式 h2 或四层 TLS 终止 |
| **server pod 未开 hostPID → SESSION 记账静默失效**（`pids.config` 的容器 PID 与 NVML 宿主 PID 对不上，used 恒为 0 → 限额形同虚设） | 1:N gpu-node DS 与 1:1 SESSION 模式 server pod 均 `hostPID: true`（§1.5.2/§1.6.6）；或改用 CGROUP 模式 + `.host_proc` 挂载。**建议库侧加自检**：SESSION 模式下若首次记账发现 `pids.config` 中无任何 PID 出现在 NVML 进程表且本进程已建上下文，打 ERROR 提示 hostPID |
| 高性能网络 preload（SMC-R/rsocket）与我们的 LD_PRELOAD 共存未验证 | 符号集不相交且 dlsym 拦截器会回退（§7.1.3 第 1 条），但**必须 spike 实测**后才可用于生产 |
| 多池可达域重叠 → 同节点两个 inject 插件注册冲突（DRA 驱动名节点单例） | helm 模板集群级归并消费侧 DS（§1.5.4），并集 nodeAffinity + 排除 server 节点 |
| 消费侧 plugin 写 claim status 的 RBAC 与并发（多设备/重试幂等） | 令牌按 claim 幂等生成（已存在则复用）；resourceclaims/status update 权限入 chart RBAC |
| emptyDir 会话目录随 pod 删除，agent 容器单独重启时会话目录仍在但 watch 状态丢失 | agent 启动时 list+watch 全量对账（落盘幂等，§5 已有），emptyDir 内容与 claim 集合收敛 |
| 数据面两次主机内存落地限制高速网络收益上限 | 固有于 lupine 架构（无 GPUDirect）；选型时按负载类型预期收益，勿承诺线性提升（§7.1.2） |

## 10. 多租户前置能力（D17，v1.6，待办；借鉴 tensor-fusion）

以下是**多租户上生产前**需要补齐的能力，来自 tensor-fusion 对照（`docs/tensor_fusion_analysis_and_lessons.md` §4）。
当前设计（Phase 1/2）单租户可用，但缺这两块：

1. **多维配额系统**（我们目前完全没有）。借 `GPUResourceQuota` 的双维度：
   - `Total`（命名空间级）：requests/limits 总量、`MaxWorkers`、告警阈值百分比；
   - `Single`（每 workload）：max/default requests & limits、`MaxGPUCount`。
   - 记账用与设备分配同构的乐观 shadow（预留即计入），调度早期（准入/Filter）+ 分配时两次校验。
   - 落点：DRA 主路径可挂在 allocator 前的准入校验；extender 路径并入 D16 的记账蓝图。

2. **单卡隔离模式锁**（1:N 单卡多会话的安全保护，我们目前无）。一张物理卡被多会话切分时：
   - 锁定该卡的隔离强度（shared / soft / hard 三选一），首个分配确定、后续必须一致；
   - 冲突（同卡出现不同隔离模式）→ **fail-closed**，拒绝新分配直至冲突清除。
   - 对应 tensor-fusion 的 `GPU.Status.{IsolationPolicy, ActiveIsolationMode, DynamicIsolationConflict}`。

3. **（可选，方向）跨会话 QoS/公平**：我们的共享令牌桶只做限速不做优先级；tensor-fusion 有 hypervisor QoS
   单卡多进程公平排队。多租户抢占场景的未来方向，非前置。

> 注：网络 fabric 亲和（让消费者优先靠近其 remote worker 的快速 IB/RoCE）我们的 `net-zone` 标签（§6）已是粗粒度
> 起点，且**领先于 tensor-fusion**（后者跨节点 fabric 完全不建模）；真做细粒度 fabric 亲和仍需扩展 zone 语义。

## 11. v2.2：会话粒度、独立 chart、webhook 与 monitor 的适配（2026-08-27）

### 11.1 D8 修订：会话 = 分区（partition），令牌存 claim annotation

claim UID 作会话只适合"一容器一 claim"。会话是服务端的**记账单元**（一组共享同一配额的进程），它与本地路径
`pkg/claimresolve` 的**分区**定义完全重合：reserved pods 上"容器 ↔ 实际命中 request"二部图的连通分量
（单容器时键为 `pod-<hash>-<container>`）。落地：

- inject 的 NodePrepare 调 `claimresolve.ResolveClaimVGPUPartitionsFromAllocatedRequests`（reader = API GET，
  与本地路径一致，不需要 pod informer）；解析失败回退为"每 request 一分区"。
- **每分区一个随机令牌**（16 字节 hex），持久化到 claim annotation `manager.nvidia.com/session-<sha256(partitionKey)[:16]>`
  （分区键含 podUID/容器名会超 63 字符，故哈希）；重试/重启先查 annotation 再铸造，一次 merge patch 写入。
  RBAC：inject 需要 `resourceclaims` patch（本地 DevicePluginClientMode 已有同类先例）。
- EnsureSession 携带 `requests`（分区内主请求名）与 `partition`；agent 只把这些 request 的 result 写进会话。
- CDI：**每个 allocation result 一个 CDI 设备**（命名沿用本地 `<request>-<device>-share-<shareID>`），env 取自所属分区；
  kubelet 只把容器引用的 request 的设备挂给它，同分区 env 相同、不会互相覆盖。跨 claim（一容器引两个 claim）与本地路径同样不支持，
  由 webhook §3.4 规则拦截。
- **同一分区内同一物理卡被多次命中**（webhook 正常拦截，此为回退路径）：会话配额取**最大值**。

### 11.2 D7 修订：远程部署使用独立 chart `charts/vgpu-manager-remote-dra-driver`

本分支对 `charts/vgpu-manager-dra-driver` 的改动已全部回滚；远程组件（GPU 节点 pod = remote-agent + lupine-server（+ device-monitor）、
消费侧 inject DS、DeviceClass、RBAC、制品 init）由新 chart 承载。约束：**只允许 `VGPUSupport` + `RemoteGPUSupport` 同开**
（feature gate 校验已强制；不做普通 gpu 类型的远程），且 `RemoteGPUSupport` 与 SharedSMUtilizationWatcher /
DevicePluginClientMode / NRISupport 互斥。

### 11.3 webhook 适配（核查结论与改法）

现状核查（`pkg/webhook`）：转换由扩展资源 `<domain>/vgpu-number|cores|memory` 触发，不读任何 annotation 选类；
生成的 request 只写 CEL `type == "vgpu"`（不钉 `device.driver`），DeviceClass 名是单一全局 flag；
**webhook 不注入 env/挂载/亲和**，也不读 ResourceSlice——因此远程数据面与放宽的 nodeSelector 对它透明。
§3.4 的共享校验以 `<claim>/<mainRequest>` 为键，同容器重复引用同一 request 会被 set 折叠，容器最多引一个 vGPU claim，
这正是 11.1 依赖的前提。

需要的改动（都小）：
1. **模式选择**：新增 pod annotation `<domain>/vgpu-access-mode: local|remote`（缺省 local）。`BuildDeviceRequest` 在
   request selector 上追加 `device.attributes["manager.nvidia.com"].accessMode == "<mode>"`。这样 DeviceClass 保持单一、
   dra-driver chart 零改动，且未标注的 pod 仍只拿本地设备（否则 gate 开后它们可能被分到远程卡）。
2. **分类器**：`ExactLooksLikeVGPU`/`DeviceRequestLooksLikeVGPU` 若以后引入远程 DeviceClass 名，要把该名加入 vGPU 识别
   （或教它认 `accessMode`），否则 §3.4 校验静默失效。当前走 request selector 方案时无需改。
3. **远程 pod 不打拓扑/运行时类注解**：`setDefaultDeviceTopologyMode`、`setDefaultRuntimeClassName` 对 accessMode=remote
   的 pod 跳过（NUMA/链路拓扑与 vgpu runtimeClass 对远程无意义）；`node/device-scheduler-policy` 可保留。
4. 顺带发现的既有缺陷：`BuildResourceClaim` 在 validate 阶段调 `allocator.BuildAllocationRequest(pod)` 时扩展资源已被
   mutate 删除，`req.Containers` 为空 → `LinkTopology`/`NUMATopology` 的 MatchAttribute 约束是死代码。与远程无关，单列修。

### 11.4 DRA monitor 适配（核查结论与改法）

现状核查（`pkg/metrics`、`cmd/device-monitor`）：
- ResourceSlice informer 用 `spec.nodeName=<node>` 选择器，collector 再按 `Spec.NodeName` 过滤——远程池无 nodeName，
  **GPU 节点上的 monitor 将看不到任何设备**（informer.go 注释称 `spec.pool.name` 不受支持是错的，v1 有
  `ResourceSliceSelectorPoolName`）。
- pod→claim→device 的 join 是 pod 先行且 pod informer 按本节点过滤，远程消费者的 pod 在别的节点 → 所有 `assigned_*`、
  `*_shared_containers` 对远程卡恒为 0；container 级指标的 `node` 标签是采集节点。
- container 用量指标靠 **本机 cgroup 取容器 PID** 再与 NVML 进程表相交——远程消费者的 cgroup 在消费节点，PID 命名空间不同，
  四个 `container_vgpu_device_*usage/utilization` 指标对远程必为 0。
- DRA collector 不读 `claims/` 下任何文件（限额来自 ConsumedCapacity），虚拟显存账本是 TODO 桩。
- monitor 的 feature gate 是独立注册表，没有 `RemoteGPUSupport`；它作为 kubelet-plugin DS 的 sidecar 部署。

改法：
1. **发现**：informer 选择器改 `spec.pool.name=<node> AND spec.driver=<driver>`；collector 改按 `Spec.Pool.Name == node` 认领。
   本地/远程通用。
2. **标签**：设备级与容器级指标增加 `access_mode`（取自设备属性）；容器级指标增加 `consumer_node`（pod 所在节点），`node` 保持采集节点。
3. **远程分配归属（"这张卡分给了哪些 pod"）**：把 join 反转为 **claim 先行**——用已有的集群级 claim lister，按
   `Status.Allocation.Devices.Results[].Pool == 本节点` 找到消费本节点设备的 claim，经 `ReservedFor` 得到 pod（需 pod 的
   集群级 lister 或按需 GET），从 pod spec 解析容器↔request。新增 `vgpu_device_allocation_info{device_uuid, pod_namespace,
   pod_name, container_name, consumer_node, access_mode}=1`，并用同一结果修正 `assigned_*` 聚合。
4. **远程容器用量**：PID 来源改为会话的 `<base>/<token>/pids.config`（SESSION 记账列表，agent 目录里 `.claim-uid` 标记 +
   claim annotation 反推分区→容器）。会话目录是 agent/server pod 的 emptyDir，monitor 在另一个 pod 读不到 → 在新 chart 里
   **把 device-monitor 放进 GPU 节点 pod**（与 agent/server 共享 emptyDir，hostPID），加 `--remote-session-base` 与
   `RemoteGPUSupport` gate；或 agent 增 `ListSessions` gRPC 暴露 token/claim/pids（二选一，前者改动小）。
5. `SharedSMUtilizationWatcher` 与远程互斥（11.2），monitor 侧该 gate 在远程节点不开。

### 11.5 实施状态（2026-08-27）

- **webhook（已实现）**：pod annotation `<domain>/vgpu-access-mode: local|remote`（缺省 local，非法值在 mutate 阶段拒绝）；
  `BuildDeviceRequest` 的 request selector 变为 `type == "vgpu" && accessMode == "<mode>"`（DeviceClass 不变、不新增）；
  remote pod 跳过 `runtimeClassName` 与 `device-topology-mode` 的默认注入。volcano 任务模板经同一 helper 生效。
- **monitor（已实现）**：slice informer/collector 改按 `spec.pool.name == 节点名` 认领（本地/远程通用）；
  DRA 与 device-plugin 两条路径的 `vgpu_device_*`（8 个）与 `container_vgpu_device_*`（6 个）指标**统一新增
  `access_mode` 标签**（device-plugin 路径恒为 `local`，DRA 路径取设备属性，缺省 `local`）；
  `RemoteGPUSupport` gate（monitor 独立注册表）+ `--remote-session-base` 开启后：claim 先行发现其他节点上的远程消费者
  pod（API GET + 60s 缓存，本节点 pod 仍走 lister），与本地 pod 走**同一套**记账闭包；远程容器的 PID 来源改为
  `<session-base>/<token>/pids.config`（分区键→claim annotation→令牌），`container_*` 指标的 `node` 标签仍是采集节点，
  pod 归属由既有的 `pod_namespace/pod_name/container_name` 标签表达，不新增指标。monitor 与 remote-agent、lupine-server
  同 pod 部署（共享会话 emptyDir、hostPID），由新 chart 承载。
- **agent**：删除未用的 `sync.Map`；`--listen-server-port` 改为纯端口号。
- **pool 语义答复**：pool = 单一发布者管理的一致性单元（generation/resourceSliceCount 判完整性，设备名 pool 内唯一）；
  多节点共用一个 pool 名会让各节点 controller 互删/互覆盖、设备重名、调度器视 pool 不完整——除非改为单一中央发布者
  （网络附加设备形态，需 `perDeviceNodeSelection`），本项目无此收益，**保持 pool = 节点名**。

### 11.6 NRI 模式：容器粒度会话（2026-08-27，用户要求，已实现）

inject 侧开启 `NRISupport` 时，会话不再在 NodePrepare 按分区分配，而是在 **NRI `CreateContainer` 钩子**按
(podUID, containerName) 分配——钩子精确知道容器身份，即使多个容器共用同一 claim request 也各得一个会话、各自记账，
与本地 NRI 路径的 per-container 分区目录同粒度。分工：

| 阶段 | 注入内容 |
|---|---|
| NodePrepare（CDI，每 result 一设备，env 全同） | `LUPINE_DISABLE_LOCAL=1`、`LD_LIBRARY_PATH`、制品挂载、`MANAGER_VGPU_CLAIM_UID=<claimUID>`（与本地路径相同的关联 env） |
| CreateContainer（NRI，`pkg/kubeletplugin/remote/nri.go`） | 读 pod spec 求该容器引用的 request 集合 → 分区键 `<podUID>_<containerName>` → 令牌（claim annotation 复用/铸造）→ **EnsureSession 屏障**（先于容器进程启动，D2 成立）→ 注入 `LUPINE_SERVER` + `LUPINE_SESSION` |

- `IsClaimPrepared` 校验沿用本地做法（NodePrepare 登记的 claim 集合），防伪造 `MANAGER_VGPU_CLAIM_UID`。
- 语义差异：共用同一 request 的两个容器在分区模式下共享一份配额（一个会话），在 NRI 模式下各持一份（两个会话）。
- monitor 查令牌顺序：先 `<podUID>_<containerName>`（NRI 键），再分区解析键，再 per-request 回退键，三种模式统一。
- gate 组合：`RemoteGPUSupport` + `NRISupport` **仅 inject 模式允许**；server 模式（只发布）二者互斥，校验移至 `--plugin-mode` 分支。

### 11.7 分支整体审计（2026-08-28）：闭环项、已修缺口、剩余边界

**已修（本轮）**
| 组件 | 问题 | 处理 |
|---|---|---|
| inject（NRI 模式） | 插件重启后 `prepared` 集合为空；kubelet 不会为已 prepare 的 claim 重跑 NodePrepare，旧 pod 的容器（重启/init 后续容器）在 CreateContainer 得到"claim not prepared" | 增加 `<plugin-dir>/remote-prepared.json` 检查点（只存 claim 身份，重启时按 UID 校验并重新解析设备），`recordPrepared/forgetPrepared` 同步落盘 |
| inject（NRI 钩子） | 钩子内 pod GET / EnsureSession 无总超时 | 30s 有界 context（CreateContainer 期间阻塞容器创建） |
| inject / agent | informer 缓存全量对象 | 两侧 slice informer 去 managedFields；**agent 的 claim informer 加 `trimClaim` 变换**：只保留身份、request 名（含 firstAvailable 子名）、本驱动本池的 allocation results；不触及本节点的 claim 缩到几百字节。EnsureSession 在裁剪缓存陈旧时向 API 重取一次 |
| agent | claim 事件触发目录扫描（O(claims×dir)） | `SessionStore` 内存索引（token↔claimUID），启动从磁盘标记重建；GC 仍按周期走磁盘兜底孤儿 |
| agent | `--sm-watcher` 与 kubelet-plugin 的 `SharedSMUtilizationWatcher` 一致性无校验 | 每 30s 检查 `<session-base>/watcher/sm_util.config`，缺失告警（库侧会回退 NVML 采样，不致命） |
| server（只发布） | 与 inject 同节点且都开 `NRISupport` 时，本地 NRI 插件与 inject 的 NRI 插件同名注册冲突 | 只发布模式不再启动本地 NRI 插件（它从不 prepare，无事可做） |
| webhook | 非法 `vgpu-access-mode` 值被 mutate 静默当 local | validate 阶段拒绝 |
| 测试 | 签名变更后 remoteagent/cmd 测试失效 | 已修并新增索引/裁剪/检查点测试 |

**SharedSMUtilizationWatcher 与 lupine-server 的配合（部署契约）**
- 发布端 kubelet-plugin（server 模式）在 gate 开时照常运行 `SMUtilWatcherStart`，写 `<host-manager-dir>/watcher/sm_util.config`（hostPath）；
- 库在会话模式按 `VGPU_CONFIG_SESSION_BASE/watcher/sm_util.config` 读（`session.c` SPECS，`from_base=1`）；
- 因此 chart 必须把 hostPath `<manager-dir>/watcher` 挂进 lupine-server 容器的 `<session-base>/watcher`（与 agent 容器同路径），agent `--sm-watcher` 与插件 gate 同开同关；monitor 读同一 hostPath 文件。agent 的检查只能告警，无法替代挂载。

**剩余边界（有意保留）**
1. 分区模式下共用一个 request 的多个容器共享一个会话（一份配额、用量合并）；NRI 模式各持一份。
2. 会话令牌 annotation 在命名 claim 上跨分配复用（claim 对象不删则令牌不换）；随机性仍在，但一次泄露的令牌生命周期 = claim 生命周期。
3. NRI 模式下容器数 × EnsureSession 往返，每容器一次；分区模式每 claim 一次。
4. monitor 对远程消费者的 pod 用 API GET + 60s 缓存，`consumer` pod 变更最多延迟一分钟反映。
5. 只发布节点关闭 gRPC 健康检查（无 DRA 服务可探），chart 的探针需改为进程/HTTP 指标探针。
