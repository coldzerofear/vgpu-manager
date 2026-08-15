# 远程 GPU 的 k8s 控制面接入设计（上报/调度/分配/注入）v1.4

> 状态：**设计定稿（十二项关键决策已确认），待实施**
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
| D7 | 总体形态 | **v1.4：helm 编排 2 DaemonSet**（并入 dra-driver chart 或独立依赖 chart），values 结构 = 原 RemoteGPUPool spec，`reachableNodeSelector`（= net-zone）一物两用：既圈定 slice 可调度范围，又是消费侧 DS 铺设范围。**operator/CRD 后置**至组件稳定、边界明确且 helm 不够用时（§1.5） |
| D8 | 会话令牌签发 | **消费侧 kubelet-plugin 在 NodePrepareResources 生成随机令牌，写入 `claim.status.devices[].data`**；独立 controller 取消，pod 启动关键路径上没有任何集中式控制器（§1.5.3） |
| D9 | 会话目录 | **agent 与 lupine-server 同 pod，共享 emptyDir**；GPU 节点主机零安装，生命周期与会话语义天然对齐（§1.5.2） |
| D10 | 域名前缀 | **`vgpu-manager.io`**（CRD group 与 device attributes 同源）；不用 `nvidia.com` 后缀（`gpu.nvidia.com` 是 NVIDIA 官方 DRA 驱动的属性域，避撞） |
| D11 | 制品形态 | **自包含静态 client**（fork 增加 `LUPINE_STATIC_DEPS`：nghttp2/OpenSSL/libstdc++/libgcc 静态内嵌，运行时依赖仅 glibc；rockylinux8 统一底座 → glibc 2.28 地板全矩阵最低）。已实现并本地验证（§4.5） |
| D12 | 制品分发 | **镜像列表 → 节点版本目录**：cudaVersion→镜像地址映射在 values 里维护，helm 渲染进消费侧 DS 的 init 容器逐版本落盘 hostPath；增减版本 = 改列表，不重建我们的镜像。**逐 pod 选镜像不可行**（准入先于分配，§4.4） |

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
`--mode=server`（GPU 节点，含本地 DRA 原职责 + 远程池职责 + inject 能力）、
`--mode=inject`（消费节点，仅注入，无 GPU 依赖）。

### 1.5.2 会话目录 = pod 内共享 emptyDir（D9）

lupine-server 与 agent 同 pod 后，会话目录用两容器共享的 emptyDir，收益链：
- **生命周期天然对齐**：server 容器重启 = 全部会话子进程死亡 = 会话作废；emptyDir 恰好活到
  pod 级，无跨 server 世代的陈旧目录清扫问题；
- **GPU 节点主机零安装**：`libvgpu-control.so` 烧在 server 容器镜像内，
  `LD_PRELOAD`/`LUPINE_CHECKPOINT_LIBRARY` 指容器内路径（本地 vGPU 才需要 host 安装）；
- 会话配额文件不暴露在主机文件系统。
库侧零改动（`VGPU_CONFIG_SESSION_BASE` 指 emptyDir 挂载点即可）。

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
| 高性能网络 preload（SMC-R/rsocket）与我们的 LD_PRELOAD 共存未验证 | 符号集不相交且 dlsym 拦截器会回退（§7.1.3 第 1 条），但**必须 spike 实测**后才可用于生产 |
| 多池可达域重叠 → 同节点两个 inject 插件注册冲突（DRA 驱动名节点单例） | helm 模板集群级归并消费侧 DS（§1.5.4），并集 nodeAffinity + 排除 server 节点 |
| 消费侧 plugin 写 claim status 的 RBAC 与并发（多设备/重试幂等） | 令牌按 claim 幂等生成（已存在则复用）；resourceclaims/status update 权限入 chart RBAC |
| emptyDir 会话目录随 pod 删除，agent 容器单独重启时会话目录仍在但 watch 状态丢失 | agent 启动时 list+watch 全量对账（落盘幂等，§5 已有），emptyDir 内容与 claim 集合收敛 |
| 数据面两次主机内存落地限制高速网络收益上限 | 固有于 lupine 架构（无 GPUDirect）；选型时按负载类型预期收益，勿承诺线性提升（§7.1.2） |
