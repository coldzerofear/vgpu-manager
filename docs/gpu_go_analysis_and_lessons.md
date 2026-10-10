# gpu-go（TensorFusion "NFS for GPUs"）源码分析与对本项目远程 vGPU 的借鉴

> 状态：分析记录（2026-08-15）。分析对象：本地 `gpu-go/`（Apache-2.0，Go，约 2.8 万行），
> 是 TensorFusion GPU Go 产品的**开源客户端/CLI/agent**；其远程 GPU 数据面（`tensor-fusion-worker` +
> `libcuda.so` stub + `libteleport`）**闭源**，从 CDN 下载二进制。
> 对照对象：本项目 `docs/remote_gpu_pool_research_design.md`（核心库）与
> `docs/remote_gpu_k8s_integration_design.md`（k8s 接入 v1.4）。
> 结论先行：**架构惊人地同构**（三平面 + 服务端 per-allocation 隔离 + `CUDA_VISIBLE_DEVICES` 裁剪 + 令牌鉴权 +
> 客户端 stub 库注入），可借鉴的主要是**工程化细节与非 k8s 消费路径**，而非核心机制。

## 1. gpu-go 是什么（读 README + docs + 源码）

| 层 | 组件 | 位置 |
|---|---|---|
| 控制面 | tensor-fusion.ai SaaS（账号/PAT/agent 注册/worker 配置/share link/心跳/指标） | 闭源；客户端见 `internal/api` |
| GPU 主机 | `ggo agent`：注册、拉配置、**按配置拉起 per-worker 进程**、上报状态/连接/指标；`hypervisor` 子模块做设备发现（purego 调 NVML）与 worker 生命周期 reconcile | `internal/agent`、`internal/hypervisor` |
| GPU 主机 | `tensor-fusion-worker`（闭源二进制）：一个 worker = 一份 GPU 分配，独立端口，quota/可见性/鉴权全部由 env 注入 | `agent.go:455-505` |
| 消费侧 | `ggo use <share-code>`：给当前 shell 注入 env（`LD_PRELOAD` stub 库 + 连接串）；`ggo studio create`：本地 dev 容器（docker/colima/wsl/apple-container）+ sshd + 自动写 `~/.ssh/config`；`ggo launch`（Windows） | `internal/studio`、`cmd/ggo/use` |
| 消费侧 | VS Code 扩展：GUI 管 studio/worker/device/connection，本质是 CLI 的壳 | `vscode-extension/` |
| 制品分发 | `ggo deps`：三份 manifest（releases/deps/downloaded）+ CDN 下载 + 校验和缓存 + 7 天自动同步 | `internal/deps` |

## 2. 与本项目逐点对照

| 维度 | gpu-go | 本项目（lupine + libvgpu-control） | 结论 |
|---|---|---|---|
| 服务端隔离单元 | **per-allocation worker 进程**：agent 为每份分配拉起一个 `tensor-fusion-worker -p <port>`，quota（`TF_CUDA_SM_PERCENT_LIMIT`/`TF_GPU_MEMORY_LIMIT`）、可见性（`CUDA_VISIBLE_DEVICES`）、鉴权文件路径全部 env 注入 | **单 lupine-server per node，每连接 fork 子进程**，session 目录 + checkpoint provider 注入 quota；`CUDA_VISIBLE_DEVICES` 由 provider setenv | 两种合法拓扑，见 §3.1 |
| 设备可见性 | `buildGPUVisibilityEnv` → `CUDA_VISIBLE_DEVICES`（AMD 用 `HIP_/ROCR_VISIBLE_DEVICES`） | provider `restore()` setenv `CUDA_VISIBLE_DEVICES` + NVML hook 族 | **同一手法**，我们已实现；他们的 NVML 侧怎么处理未开源 |
| 鉴权 | share code 拼在连接串尾（`url+code`），worker 读 `TF_AUTHORIZED_KEY_PATH` 文件校验（`TF_ENABLE_URL_AUTH=1`）；share 有过期/最大使用次数 | `LUPINE_SESSION` 令牌 = 会话目录名，fail-closed | 同构；他们的**过期/次数**是我们没有的运营维度 |
| 容器级记账 | 未开源（在 worker 里）；单 worker 单分配，天然一致 | SESSION 模式 + `pids.config` 聚合多子进程 | — |
| 连接可观测 | worker 把 `clientIP,clientPort,clientPID` 逐行写 `TF_CONNECTION_INFO_PATH`，agent tail 后上报 | `pids.config`（服务端子进程 PID）；无客户端侧信息 | 可借鉴，见 §3.3 |
| 客户端注入 | `LD_PRELOAD` **只放 stub**（`libcuda.so`/`libnvidia-ml.so`），支撑库（`libteleport`/`libaccelerator`）走 `LD_LIBRARY_PATH` | 静态自包含 shim（D11），仅 glibc 依赖，`LD_LIBRARY_PATH` 指向只含两个 .so 的目录 | 他们暴露在与我们踩过的**同一依赖污染风险**下（§3.2） |
| 客户端制品版本 | **不按 CUDA 版本分**：CDN 路径 `{version}/{os}-{arch}/lib{name}.so`，`SelectRequiredDeps` 每类型只取最新 | lupine 按 CUDA 版本矩阵（codegen from headers）→ D12 版本目录 | **重要信号**，见 §3.4 |
| 消费路径 | 开发者本机/dev 容器（非 k8s 为主） | k8s pod（DRA） | 互补，见 §3.5 |
| 控制面 | SaaS | 集群内 DRA + helm | 不同产品形态 |

## 3. 对本项目有积极意义的部分

### 3.1 服务端拓扑的另一种选择：per-allocation server 进程

> **已定案为正式第二拓扑（D13）**，取舍表与场景归属见 k8s 接入设计 §1.6；本节保留原始分析。
> 纠正：TensorFusion 的 1:1 也是**单主机 IP + 每 worker 一个端口**（`ListenPort`），不是每分配一个 IP。

gpu-go 的 worker 模型证明了 **"一份分配 = 一个独立进程 + 独立端口 + env 注入 quota"** 是可运营的。
对我们的含义：lupine-server 本身无每客户端状态，完全可以**一份分配一个 `lupine_driver_server`**，
`LD_PRELOAD` 库 + `CUDA_VISIBLE_DEVICES` + `CUDA_MEM_LIMIT_*`/`CUDA_CORE_LIMIT_*` env 拉起——库的
env 回退配置路径（`init_g_vgpu_config_by_env`）现成，**不需要 session 目录、provider、session 消毒、
EnsureSession 屏障**（worker 不存在则连不上，reconciler 保证先起）。

| | 共享 server + session（现方案，已实现验证） | per-allocation server |
|---|---|---|
| 服务端复杂度 | provider/session 目录/pids.config/消毒/fail-closed | 一个 reconciler（desired vs actual 进程） |
| 端口 | 1 个/节点 | N 个/节点，需分配与放行 |
| 容器级记账 | SESSION 模式按 pids.config 聚合 | **仍需**（一个 server 多连接多子进程），且 `/tmp/.sm_node` 等要 per-server 作用域 |
| endpoint | 静态，可进 ResourceSlice attribute | **分配后才有**，须经 claim status 回传 → 与 EnsureSession 同形的等待 |
| 崩溃域 | 一个 server 死 = 节点全部会话死 | 一份分配一个进程，互不影响 |
| 更新/维护 | 全节点滚动 | 逐分配 |
不建议现在切换（现方案已真机验证），但**记录为正式备选**：崩溃域隔离与逐分配升级是它的真实优势；
若将来 lupine 单 server 的稳定性/升级成为痛点，这是现成的退路，库侧零改动。

### 3.2 客户端注入的四条实战教训（可直接采纳）

来自 `internal/studio/env.go`/`ssh_setup.go` 的注释，都是踩过坑的：
1. **`/etc/ld.so.preload` 不能挂进容器**——它对包括 sshd 在内的全部进程生效，会破坏 SSH 协议
   （他们改为写 `/etc/environment` 的 `LD_PRELOAD`，只影响用户 shell）。对我们：远程 pod 注入用
   `LD_LIBRARY_PATH` 而非 preload（shim 就是 libcuda 本身），天然规避；但**任何"dev studio"式产品**
   要记住这条。
2. **不要覆盖容器 PATH**——把单个二进制挂到 `/usr/local/bin/<name>`。对我们：`nvidia-smi` 应作为
   **单文件挂载**进 pod，而不是 prepend PATH。
3. `LD_PRELOAD` 只放 stub，支撑库放 `LD_LIBRARY_PATH`——他们由此暴露在依赖污染风险下
   （`libteleport` 等要在目标镜像可解析）。**这正是我们做 D11 静态自包含的理由**；同时提示我们
   在 static 制品自检里保持 "DT_NEEDED 仅 glibc" 的红线。
4. Windows 上 PATH 不足以让 DLL 优先加载（System32 优先）→ 需 `ggo launch` 包装器。非我们范围，备忘。

**可加的小事**：static 制品镜像里带上 `nvidia-smi`（lupine 客户端镜像已含；它动态链接
`libnvidia-ml.so.1` 会自然拿到我们的 shim），作为单文件挂载注入——用户对 `nvidia-smi` 的期待很强，
成本极低。

### 3.3 连接级可观测性

`clientIP,clientPort,clientPID` 逐行落文件 + agent tail 变化上报，让控制面能回答"谁从哪连着这块卡"。
我们的 `pids.config` 只有服务端子进程 PID。provider ABI 只传 `connection_id`（`checkpoint_provider.h:57`），
**不传对端地址**；但 `restore()` 跑在持有 `connfd` 的子进程里（`server.cpp:645` accept 后 fork 继承），
遍历本进程 fd 找到 TCP socket 后 `getpeername()` 即可拿到 `clientIP,clientPort`——不改 lupine，
provider 侧几十行。写入 `pids.config` 同一行，换来"谁从哪连着这块卡"的运维可见性
（对应 helm 阶段"池状态汇总"的缺口）。列入 K2 可选项。

### 3.4 关键战略信号：他们的客户端制品**不按 CUDA 版本分**

`libdownloader.go` 的 URL 是 `{version}/{os}-{arch}/lib{name}.so`，`deps.go` 按类型取最新版，
**没有 cudaVersion 维度**。说明 TensorFusion 的闭源 shim 与 CUDA 头版本解耦（推测：手维护
API 子集 + 运行时按名转发，与我们 `library/` 的 `cuda-subset.h`+dlopen 思路同源）。
对照 lupine 的 codegen-from-headers（wire 结构随 CUDA 头变 → 制品矩阵），这是他们最大的**结构性
优势**，也直接印证设计 §7.3 "单基准制品"在原理上可达。
含义：我们的 D12 版本目录是对 lupine 现状的正确工程化处理，但**长期看值得推动 lupine 上游
或 fork 做 wire 稳定化**（把 RPC 结构与 CUDA 头解耦），矩阵消失后 D12 退化为单目录。
另外他们 manifest 的 **required(deps) vs have(downloaded) 差分对账**是个干净的模式，
消费侧 plugin 对 hostPath 版本目录做 GC/补全时可以照用。

### 3.5 非 k8s 消费路径：我们几乎已经有了

gpu-go 的核心体验是 `ggo use <code>` 一条命令让本机/dev 容器用上远程 GPU。我们的服务端形态与之
完全对得上：`vgpu-session-config` 落会话 + `lupine_driver_server`（带库）+ static client 制品，
**今天就是一个可用的非 k8s 远程 GPU**，缺的只是客户端糖：
- 一个 `vgpu use <session-token>`：写 `LUPINE_SERVER`/`LUPINE_SESSION`/`LUPINE_DISABLE_LOCAL=1`/
  `LD_LIBRARY_PATH=<static 制品目录>` 到当前 shell 或 `~/.bashrc`；
- 会话签发从"agent watch claim"变成"CLI/控制面 mint"，`vgpu-session-config` 已能落盘。
这条路径服务的是**开发者本机 → 集群 GPU** 的场景（他们的主战场），与 k8s pod 路径共用全部服务端。
建议作为 K3 之后的独立小项立项，不影响主线；他们的 share 过期/次数控制可一并借鉴到令牌上。

### 3.6 工程化小项

- **reconciler 模式**（`hypervisor/reconciler.go`）：desired/actual 对账，GPU 集/端口变化才重启，
  仅 env 变化延迟到下次重启生效——若走 §3.1 拓扑，这是现成范式。
- **`cmd/mock-worker`**：无 GPU 测 agent 的假 worker。与我们 `vgpu-session-config` 同一精神；
  可考虑给 kubelet-plugin `--mode=server` 配一个 mock lupine endpoint 做 CI。
- 多厂商（AMD/Hygon `libamdhip64` preload）：备忘，非当前范围。

## 4. 不借鉴 / 不适用

- SaaS 控制面、license、PAT、心跳上报：产品形态不同。
- 闭源数据面：无法分析其 wire/记账/限速实现，仅能从 env 名（`TF_CUDA_SM_PERCENT_LIMIT` 等）推断
  与我们同为 SM 百分比 + 显存 MB 的 hard limiter。
- 他们的 studio 是"本地 dev 容器 + 远程 GPU"，与我们 pod 模型正交，不冲突。

## 5. 行动项汇总

| 项 | 阶段 | 量级 |
|---|---|---|
| static 制品镜像加 `nvidia-smi`，pod 内单文件挂载（§3.2） | K1 | 小 |
| provider 写入 `pid,clientIP,clientPort`，metrics/status 消费（§3.3） | K2 可选 | 小 |
| hostPath 版本目录 GC 采用 required/have 差分对账（§3.4） | K1 | 小 |
| per-allocation server 拓扑 → 已升格为正式第二拓扑 D13（k8s 设计 §1.6），operator 阶段以 `RemoteGPUServer` CR 落地 | operator 阶段 | 中 |
| 非 k8s `vgpu use` CLI（§3.5） | K3 后独立项 | 中 |
| 推动 lupine wire 与 CUDA 头解耦（§3.4 长期） | 长期 | 大 |
