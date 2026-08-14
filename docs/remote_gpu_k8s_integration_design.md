# 远程 GPU 的 k8s 控制面接入设计（上报/调度/分配/注入）v1.0

> 状态：**设计定稿（四项关键决策已确认），待实施**
> 前置：`docs/remote_gpu_pool_research_design.md`（核心库 + lupine 方案 C-2，已实现并真机验证单节点会话隔离）
> 本文回答：设备如何上报、pod 如何调度到"网络可达但无 GPU"的节点、分配结果如何同时到达消费节点（注入）
> 与 GPU 节点（配额落盘）、lupine 版本如何匹配、server 如何被发现。

## 0. 已确认的决策（2026-08-14）

| # | 决策点 | 结论 |
|---|---|---|
| D1 | 技术路径 | **DRA 为主路径**，extender+device-plugin 为老集群兼容路径（后行） |
| D2 | 配额落盘时序 | **agent watch 推送 + 注入 init 容器屏障**（EnsureSession） |
| D3 | server 发现 | **endpoint attribute 同时允许 IP 或域名**，注入层不区分；按部署形态填值 |
| D4 | 客户端制品 | **直接上版本目录机制**（不等单基准制品 spike） |

## 1. 问题本质：三平面模型

远程 GPU 打破 "设备属于节点" 假设。所有问题都是三个平面之间的通信问题：

```
┌─ 控制面 ────────────────────────────────────────────────┐
│ scheduler(DRA allocator / extender)：全局池记账、版本/可达性匹配 │
│ controller：会话令牌签发、生命周期回收                          │
└──────────┬──────────────────────────┬───────────────────┘
           │ ①分配结果                  │ ①分配结果
┌─ GPU 节点(资源面) ─────────┐   ┌─ 消费节点(任意可达节点) ────────┐
│ lupine-server + libvgpu     │   │ kubelet + kubelet-plugin        │
│ agent：上报设备、落盘会话配额 │◄──│ 注入: LUPINE_SERVER/SESSION/     │
│   ②须先于容器首个 CUDA 调用  │ ③ │ DISABLE_LOCAL/客户端库/init屏障  │
└────────────────────────────┘   └────────────────────────────────┘
```

- ① 分配结果的载体：DRA 下是 **ResourceClaim 对象本身**（两边都 watch 它）；extender 下是 pod annotation。
- ② 时序约束：provider fail-closed（库侧安全底线，不放松）要求配额先于首个 CUDA 调用落盘 → D2 的屏障。
- ③ 可达性约束：消费节点必须与 GPU 节点 underlay 网络可达 → 标签 + NodeSelector 进调度。

**本地 vGPU 路径零影响**：远程是新增 DeviceClass/资源池，现有 device-plugin/extender/DRA 本地分配不动。

## 2. 主路径：DRA

### 2.1 设备上报（agent → ResourceSlice）

GPU 节点 agent（新组件，可并入 device-monitor 部署形态）发布远程池 ResourceSlice：

- `spec.nodeSelector`：**编码网络可达域**——匹配携带本 server 可达域标签的节点（见 §6），
  这是"资源只在 GPU 节点上报、pod 却能调度到别处"的核心机制；调度器原生理解，无需改调度逻辑。
- `spec.pool`：每 GPU 节点一个 pool，generation 随设备变化递增。
- 每设备 attributes（供 CEL 匹配与注入层读取）：

| attribute                        | 类型          | 用途                                        |
|----------------------------------|-------------|-------------------------------------------|
| `namager.nvidia.com/type`        | string      | 显卡类型 `remote-vgpu`                        |
| `namager.nvidia.com/uuid`        | string      | 物理卡 UUID，agent 落盘配额时按它填 `devices[].uuid`  |
| `namager.nvidia.com/memory`      | int         | 可分配显存                                     |
| `namager.nvidia.com/cudaVersion` | **version** | 节点驱动支持的 CUDA 上限，版本匹配用（§4）                 |
| `namager.nvidia.com/endpoint`    | string      | lupine-server 端点，**IP 或域名均可**（D3），注入层原样拼接 |
| `namager.nvidia.com/netZone`     | string      | 所属网络域（与 nodeSelector 用的标签一致，冗余供审计）        |

切分模型（一卡多份额）复用现有 DRA可消费设备 本地路径的 vgpu 机制（`pkg/kubeletplugin/vgpu.go`），
远程池按同一套份额语义发布。

### 2.2 调度与版本匹配

- 可达性：ResourceSlice.nodeSelector 完成，无自定义调度逻辑。
- 版本（client ≤ server）：DeviceClass/claim 的 CEL selector 匹配 `namager.nvidia.com/cudaVersion >= <pod 最低需求>`。
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
3. 注入 **init 容器**（D2）：对每台分配到的 server 调 agent `EnsureSession(token)`，
   全部 ready 才退出；顺带 HEAD 探测连通性 + 比对 `x-lupine-cuda-version` 做版本 pre-flight，
   把"配额未就绪/不可达/版本不符"三类错误拦在主容器启动前。
4. 限额 env（`CUDA_MEM_LIMIT_*` 等）**不注入**——远程模式配额的唯一来源是 GPU 节点会话目录（服务端权威）。

### 2.4 GPU 节点 agent

- **watch ResourceClaim**（过滤本 pool）：分配 → 建会话目录 + `WriteVGPUConfigFile` 落盘
  `<base>/<session>/config/vgpu.config`（复用 `pkg/config/vgpu` seqlock 写盘，`devices[].uuid` 填本节点分到的卡）；
  释放/pod 删除 → 删除会话目录（幂等；孤儿目录由库 fail-closed 兜底，无安全风险）。
- **EnsureSession 端点**（HTTP/gRPC，监听 endpoint 同网卡）：入参会话令牌；用令牌反查 claim
  （令牌为 controller 签发的随机值，本身即能力凭证），已落盘直接返回 ready，未落盘则现场落盘。
  可选强化：bound SA token + TokenReview（device-mounter 式），第一阶段不做。
- 会话令牌：controller 在分配时签发，写入 claim（status/annotation），注入层读取。
  **不用 pod UID**（可预测、可冒用，设计 §6.2.1）。

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
2. 显式 annotation `namager.nvidia.com/min-cuda: "12.4"`——**推荐的主要声明方式**。
3. 都没有 → 不筛选，靠下两层兜底。

**4.2 启动前（init 屏障 pre-flight）**：HEAD server 读 `x-lupine-cuda-version`（`h2.cpp:440`，编译期常量），
与 annotation 需求比对，不符则 init 失败，错误信息明确指向版本。

**4.3 运行时（权威兜底，天然存在）**：pod 内 cudart 经 lupine 看到 server 驱动版本
（`cuDriverGetVersion` 透传），不满足时 cudart 自报 `CUDA_ERROR_INSUFFICIENT_DRIVER`。

**客户端制品（D4：版本目录）**：install daemonset 在全节点铺
`/opt/vgpu/lupine/<cuda-ver>/lib/{libcuda.so.1,libnvidia-ml.so.1}`；
注入层选 `max{ver : ver <= server_cudaVersion}` 的目录挂载 + `LD_LIBRARY_PATH`。
lupine shim 走动态链接器查找（远程 pod 无驱动，它的 libcuda 就该是 lupine 的），**非 LD_PRELOAD**。
多 server 版本不一时取交集最低者；`versions.mk` 增加制品清单与 sha256。

## 5. 配额落盘时序（D2）

```
调度: allocator 绑定 claim ──► controller 签发令牌写入 claim
                                   │
GPU 节点 agent watch claim ────────┤ (主通道，通常 <1s 落盘)
                                   ▼
消费节点: kubelet 起 init 容器 ──► agent.EnsureSession(token)
                                   │  已落盘 → ready
                                   │  未落盘 → 反查 claim 现场落盘 → ready
                                   ▼
        init 退出 → 主容器启动 → 首个 CUDA 调用（此时配额必定已就绪）
                                   ▼
        lupine 子进程 provider restore() → 读会话目录 → 隔离生效
```

- 确定性来自 init 容器语义：主容器保证在配额就绪后才启动，同时覆盖"agent 比 pod 起得晚"的场景。
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
- **可达性进调度**：网络域标签规范——GPU 节点 `namager.nvidia.com/net-zone=<zone>`（server 所在域），
  可达节点 `namager.nvidia.com/net-zone.<zone>=reachable`；slice.nodeSelector 匹配后者。第一阶段运维人工标注，
  探活组件可选后补。

## 7. 安全模型

- 会话令牌：controller 签发的随机值（非 pod UID），是 EnsureSession 的能力凭证与 `LUPINE_SESSION` 本体；
  泄露面 = pod env（仅 pod 所有者可见），风险 = 冒用他人配额（需先拿到令牌）。
- 服务端权威：配额唯一来源是 agent 落盘的会话目录；客户端 env 不参与限额（§2.3 第 4 条）。
- fail-closed 链条不变：无 session/无配额/空 allowlist → 拒连（库侧已实现并测试）。

## 8. 改造面与阶段

| 组件 | 改造 | 量级 | 阶段 |
|---|---|---|---|
| agent（新） | slice 发布 + claim watch 落盘/回收 + EnsureSession | **大** | K1 |
| controller | 令牌签发/写入 claim | 中 | K1 |
| kubelet-plugin | 远程分支注入（env/CDI/init 容器） | 中 | K1 |
| install daemonset | lupine-client 版本目录铺设；扩全节点 | 小 | K1 |
| webhook | `NVIDIA_REQUIRE_CUDA`/annotation → 版本需求 | 小 | K2 |
| chart | server+库部署（README 已有 env 清单）、zone 标签规范 | 中 | K1 |
| extender 兼容路径 | 假资源 + CR 记账 + annotation 落盘 | 大 | K3（老集群需求明确后） |

- **K1（最小闭环）**：单 zone、单 server/pod，agent+注入+init 屏障跑通端到端。
- **K2**：版本匹配三层、多 server 组合（验证 §6.8 边界 6 的 cuda:i==nvml:i 实测项）、回收对账。
- **K3**：extender 兼容路径（按集群版本分布决定是否启动）。

## 9. 风险与待定

| 风险 | 缓解 |
|---|---|
| DRA 版本门槛（k8s ≥1.32） | K3 兼容路径兜底；集群版本分布待盘点 |
| 多 server 时 LUPINE_SERVER 顺序不确定 → 设备序号漂移 | 注入层按 claim 结果排序后固定；写入测试 |
| CUDA/NVML 两表连接数不一致错位（§6.8 边界 2） | init 屏障逐 server 探测，全通才放行 |
| server 重启会话不可恢复 | 固有约束（lupine 连接态）；文档明示，应用层重试/重启恢复 |
| agent 落盘与回收竞态（同名 session 快速重建） | 令牌随机不复用；目录以令牌命名，天然不撞 |
| multus 形态 DNS 自维护成本 | D3 允许 IP 直注先行，DNS 仅该形态启用 |
