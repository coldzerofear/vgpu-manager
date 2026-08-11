# 跨节点远程 GPU 资源池 —— 内存/核心隔离方案研究与设计（v0.3）

> 状态：**研究 / 设计阶段（可行性论证），PID 映射结构为 v0.2 草案（待定）**
> 关联代码：本仓库 `library/`（本地 vGPU 硬隔离库）、`D:\WorkSpace\GoCode\src\lupine`（开源远程 GPU 转发）
> 配套阅读：仓库根目录 `AGENTS.md`（面向后续 AI 的任务与复查指南）

---

## 0. 文档目的与阅读对象

本仓库当前已实现：Kubernetes 中 GPU 设备调度/分配（Device Plugin 与 DRA 两条路径），并通过 `library/` 构建的
`libvgpu-control.so`（LD_PRELOAD 劫持 CUDA/NVML API）实现 GPU 内存/核心的**硬隔离**（内存限额、SM 令牌桶限速）。

新任务目标：**跨节点远程 GPU 调用** —— 将 GPU 与 Pod 节点解耦，Pod 通过 TCP 访问其他节点上的 GPU（类似 NFS 之于
本地磁盘），构建真正的跨节点 GPU 资源池。底层远程转发选用开源项目 **lupine**（API 劫持 + TCP 转发）。但 lupine
**只做远程访问、不做虚拟化（内存/核心隔离）**，需要在其之上叠加本项目 `library/` 的隔离能力。

v0.2 相对 v0.1 的增补（来自评审意见）：
1. PID 映射结构改为**按设备**组织的 v0.2 草案（§5），兼容多 server 场景。
2. 深入分析 **dlsym 双导出 / dlopen 后置无法拦截 / 是否需要合并成一个 so**（§4.0）。
3. 方案 C 的**配置下发（服务端权威）与设备级访问控制**（§6）。
4. **lupine 多版本/多制品分发**问题与策略（§7）。

v0.3 的修正（来自评审）：
- **推翻了 v0.2 的"每子进程 setenv 独立注入配置"假设**：一个容器有多个使用 CUDA 的进程 → 多个 server 子进程，
  必须共享同一份**会话级配置**并做**容器级记账**（§6 全文重写）。
- 新增"会话模型"（session 配置区 + 会话进程表）与配置**同步/更新/应用**机制（§6.4）。
- 方案 C 的记账从 `SELF_PID` 改为 `SESSION` 兼容模式（容器级口径，§4.3/§6.5）。
- 补充设备级 allowlist + 序号重映射，保证"每个设备配置匹配"（§6.6）。

---

## 1. 任务目标与约束

| 目标 | 说明 |
|---|---|
| 远程 GPU 池 | Pod 在本节点（无 GPU）通过 TCP 使用远端 GPU 节点上的 GPU，像本地设备一样编程 |
| 内存硬隔离（Phase 1） | 每个 Pod 有独立内存限额；超额分配返回 OOM；容器内看到"自己的"虚拟化视图（nvidia-smi/cuMemGetInfo 正确） |
| 核心硬隔离（Phase 2） | SM 占用率限速（复用本地库的令牌桶 + 利用率控制器） |
| 尽量少的改动 | 尤其避免对 lupine 大改；lupine 升级成本可控 |
| 与现有库解耦（优先） | 现有 `library/` 本地能力尽量复用，不因远程方案而破坏 |

**已确认的约束/决策（来自负责人）：**
- 适配层实现形态：复制 `library/` 为独立子工程（`library-remote/`），而非在现有库内加运行时分支。
- 第一阶段设备模型：**单节点多 GPU**（一个 Pod 只连一个 GPU 服务器，可分配该节点多张远程 GPU）。
- 第一阶段**禁用内存超卖**（`CUDA_MEM_OVERSOLD`/UVA 路径关闭），物理上限由远端真实分配兜底。
- 内存隔离优先；核心隔离评估后进入第二阶段。
- **待定**：PID 映射结构（§5）与 lupine 制品分发策略（§7）仍在评审中。

---

## 2. 现状架构

### 2.1 vgpu-manager `library/`（本地隔离，C 语言，LD_PRELOAD）

- 拦截面：CUDA **Driver API**（`cu*`）+ 少量 NVML（`nvml*`）。`cudaMalloc/cudaMemcpy/cudaLaunchKernel` 由 cudart
  内部转成 `cu*`，因此被覆盖。
- 导出符号：`cu*`, `nvml*`, `cudbg*`, `dlsym`（`library/deploy/libvgpu-control.exports.ld`）。
- 真函数解析：`load_necessary_data()` 从 `/proc/driver/nvidia/version` 读取驱动版本，`dlopen("libcuda.so.<ver>")`
  和 `libnvidia-ml.so.<ver>` 填充 `cuda_library_entry[]`/`nvml_library_entry[]` 表
  （`loader.c:1116-1203, 3040-3055`）。**所有 hook 通过该 entry 表调用真函数** —— 这是远程化改造的关键枢纽。
- 内存硬隔离：
  - `prepare_memory_allocation()`（`cuda_hook.c:321-379`）：`used + request > total_memory` → `CUDA_ERROR_OUT_OF_MEMORY`。
  - `used` 来自 `get_used_gpu_memory_by_device()`（`cuda_hook.c:2480-2530`）：调 `nvmlDeviceGetComputeRunningProcesses`
    + `nvmlDeviceGetGraphicsRunningProcesses`，按**容器 PID 归属模式**过滤（CLIENT/CGROUPV1/CGROUPV2/OPEN_KERNEL/HOST），
    累加本容器的 `usedGpuMemory`。
  - `vmem_used` 来自 UVA 超卖记账 ledger（`/tmp/.vmem_node/vmem_node.config`，共享 mmap，按设备+本地 PID）。
  - 上报虚拟化：`cuMemGetInfo`（`cuda_hook.c:3839-3892`）、`cuDeviceTotalMem`（3788-3829）、
    `nvmlDeviceGetMemoryInfo(_v2)`（`nvml_hook.c:63-132`）把 total/used/free 改写为"限额视图"。
- 核心硬隔离：
  - 令牌桶 `rate_limiter()`（`cuda_hook.c:642-673`）在 `cuLaunchKernel` 前按 `gridDim*blockDim` 扣减令牌，不足则休眠重试（物理限速）。
  - 利用率 watcher 线程（`cuda_hook.c:1459-1742`）**本地高频轮询** `nvmlDeviceGetProcessUtilization`/利用率，
    按容器 PID 过滤得到本容器的真实 SM 利用率，驱动令牌桶补给（delta/AIMD/auto 控制器）。
- 配置：`/etc/vgpu-manager/config/vgpu.config`（mmap，`device_t` 含 memory_limit/core_limit/hard_core/oversold 等，
  seqlock 版本化）。`load_controller_configuration()`（`loader.c:2856`）**文件优先**，无文件则回退
  `init_g_vgpu_config_by_env()`（`loader.c:2723`，从 `CUDA_MEM_LIMIT_*`/`CUDA_CORE_LIMIT_*`/`MANAGER_VISIBLE_DEVICES` 等 env 构建）。

### 2.2 lupine（远程转发，C++）

- 客户端 shim：`libcuda.so.1`（导出 `cu*`、`dlsym`、`lupine_checkpoint_*`）+ `libnvidia-ml.so.1`（导出 `nvml*`），
  通过 HTTP/2 把 `cu*`/`nvml*` 调用转发到 `LUPINE_SERVER`（TCP:14833）。
- 设备模型：`lupine_virtual_device_for_ordinal()` 把虚拟设备序号直接作为 CUdevice 返回
  （`routing.cpp:292-307`）；内部路由把序号映射回 (conn, remote_device)（`routing.cpp:217-337`）。
- 服务端：每连接 `fork` 一个子进程承载该客户端的全部 CUDA 状态
  （`server.cpp:668-727`）。父进程只做 accept/IPC 代理，绝不初始化 CUDA。
- 会话：客户端把 env `LUPINE_SESSION` 作为 HTTP/2 请求头 `x-lupine-session` 发送（`h2.cpp:710-724`），服务端在
  子进程内用 `rpc_http2_session_id()` 读取（`h2.cpp:850-857`），**目前仅用于 checkpoint 恢复**（`server.cpp:421-429`）。

### 2.3 结合点

```
Pod 内应用 (cudaMalloc/...)
   ↓ (远程模式) 适配层 hook → 校验/拦截 → 转发
lupine-client (libcuda.so.1 / libnvidia-ml.so.1)
   ↓ HTTP/2 TCP
lupine-server (每连接 fork 子进程)
   ↓ 子进程内真实调用 CUDA/NVML
GPU 服务器真实驱动
```

---

## 3. 代码级事实（证据）

### 3.1 lupine server 全部透传，无任何按 client 的虚拟化

**结论：`nvmlDeviceGetComputeRunningProcesses`（及全部 NVML/CUDA 服务端处理）都是原始透传。**

- `nvml_server.cpp:58-97` `handle_processes()`：读 `device + requested_count + has_infos` → `dlsym` 真 NVML →
  调用 → 把返回的 `nvmlProcessInfo_t` 数组**原样**写回。**无过滤、无 PID 重写、无内存记账、无进程注册表。**
  返回的是 GPU 服务器宿主机上所有使用该 GPU 的真实进程（含本连接 fork 的子进程和其他租户的子进程）。
- `codegen/gen_server.cpp:1722-1826`：`cuMemGetInfo_v2`/`cuMemAlloc_v2`/`cuMemFree_v2` 均直接调真驱动，原样回包。
- `codegen/gen_nvml_server.inc`：`nvmlDeviceGetMemoryInfo`/`nvmlDeviceGetUtilizationRates` 等同样透传宿主值。
- `LUPINE_SESSION` 不参与任何记账/过滤，仅用于 checkpoint（`server.cpp:421-429`）。

**推论（影响方案设计）：**
1. 远端返回的进程表是**宿主机聚合视图**，本地容器 PID 与远端 PID 不匹配，无法直接过滤出"本 Pod"的用量。
2. 若想拿到"本 Pod 真实已用内存（含上下文等隐式占用）"，必须让客户端能识别"自己的远端 PID"——
   这需要 lupine 提供一个**极小补丁**（见方案 B/C）。
3. `cuMemGetInfo`/`cuDeviceTotalMem`/`nvmlDeviceGetMemoryInfo` 的虚拟化上报可以在客户端完成（现有库已实现），
   不依赖 server 配合。

### 3.2 远端 PID 生命周期与本地 PID 的关系：**1:1 重叠，不是 1:N**

推理链：
1. lupine client 连接状态是**进程级**全局（`nvml_client.cpp:47-49` 的 `conns[16]/nconns/connected`；
   `client.cpp` 的 `rpc_open()` 同）。一个本地进程 = 一条 TCP 连接（单 server 场景）。
2. server 为**每条连接 fork 一个子进程**（`server.cpp:668-727`），该子进程承担此连接的全部 CUDA 调用。
3. NVML 进程表里本连接对应的 PID = 该子进程的 PID（远端 PID）。
4. 因此：**本地进程 PID ↔ 远端子进程 PID = 1:1，生命周期重叠**（连接随进程首次 CUDA 调用建立、进程退出时关闭，
   子进程随之被回收）。
5. N 个本地进程 → N 对映射，仍逐一配对。**不存在一个远端 PID 对应多个本地进程的情形**（连接不跨进程共享）。

例外与刷新点（建模必须处理）：
- **断线重连**：连接重建后 server 重新 fork 子进程，远端 PID 变化 → 映射需按需刷新。
- **多 server**（`LUPINE_SERVER=a,b`）：一个本地进程有 N 条连接、N 个远端 PID（每 server 一个）。这是 v0.2 映射
  结构按设备组织的直接原因（§5）。

### 3.3 隐式内存占用问题 与 HAMI 探测的局限

- **记账口径差异**：`cuMemAlloc/cuMemFree` 记账只能记录**显式分配**。但 CUDA 上下文、module 加载、JIT 缓存、
  stream 临时缓冲等会**隐式占用**设备内存。仅记账会出现 `记录值 < 实际占用`，导致超卖/漏管。
- 本地库**没有此问题**：`get_used_gpu_memory_by_device()` 用 NVML 的 `usedGpuMemory`（驱动按进程统计的真实值，
  含全部隐式占用）。
- **HAMI 思路（进程初始化时创建一个上下文探测上下文开销）在本远程场景不成立**：即使探测上下文，经 lupine 透传的
  NVML 查询返回的是**宿主机 PID 列表**，容器内无法把"这个上下文多占的内存"归属到本进程——因为不知道自己的远端 PID。
- **关键点：一旦能拿到自己的远端 PID（方案 B/C），HAMI 式探测就完全不需要了** —— 直接按远端 PID 过滤 NVML
  进程表即可拿到含隐式内存的真实用量，无需估算上下文开销。

### 3.4 现有 library 的记账模型（本地 = NVML 真实值口径）

本地模式下 `prepare_memory_allocation()` 的 `used` = NVML 进程表按容器 PID 过滤后的真实 `usedGpuMemory`
（含隐式占用）。**任何远程方案若偏离此口径（如纯记账），都是退化**，必须避免或明确接受代价。

---

## 4. 候选方案

### 4.0 前置：符号解析机制深入（决定方案形态的关键）

#### 4.0.1 dlopen 后置无法拦截已解析引用

进程内对 `cuMemAlloc_v2` 的引用经 **PLT 惰性绑定**解析：按"加载顺序 + 全局作用域"搜索符号，一旦绑定到真
`libcuda.so` 的符号就**粘住**，之后 `dlopen` 的任何库都不会改变该绑定。lupine-server 的生成 handler 是**直接引用**
（`codegen/gen_server.cpp:1760` 直接调用 `cuMemAlloc_v2`）。因此：

> **在 server 进程内"事后 dlopen library-remote 来加载 hook"的方案不可行** —— 库晚于真 libcuda 进入全局作用域，
> 无法替换已解析的引用。**必须 LD_PRELOAD**（库最先进入全局符号表，父进程不碰 CUDA，子进程经 fork 继承）。

这正是本地 vGPU 的注入方式（`/etc/ld.so.preload`，`scripts/install_files.sh`）。

#### 4.0.2 "库收录的 API 才 hook、未收录直通驱动" —— 是 entry 表设计天然具备的

用户设想的"library-remote 只 hook 收录的 API，未收录的回退 nvidia 驱动"不需要额外机制：现有库的
`cuda_library_entry[]`/`nvml_library_entry[]` 表只填库关注的函数，hook 只对表内函数生效；未 hook 的符号对应用
直接绑定真驱动（或 lupine shim）。**入口必须是 PRELOAD，而不是 dlopen**（见 4.0.1）。

#### 4.0.3 dlsym 双导出：分场景分析

先澄清：**LD_PRELOAD 支持冒号分隔的多个 so，问题不是"不能同时 preload"，而是重复符号的解析顺序。**

- **客户端（方案 B：adapter + lupine-client 都导出 `dlsym`）**：
  - library 的 dlsym 拦截器用 `dlvsym(RTLD_NEXT,...)` 自发现 glibc 的真 dlsym（`loader.c:1073-1110`），
    回退走 glibc、**不调用 lupine 的 dlsym**，因此无递归。
  - PRELOAD 顺序 adapter 在前 → 应用/其他库的 `dlsym("cuMemAlloc")` 先命中 adapter 的 dlsym → 返回 adapter hook；
    未收录名字回退 glibc 真 dlsym → 全局作用域找 lupine 的 `cu*` → 返回 lupine 指针（直通）。
  - **spike 必须实测**：双方都 PRELOAD 后 adapter 的 dlsym 是否被优先调用；其回退是否在全局作用域命中 lupine。
  - 备选：adapter 不导出 dlsym → 所有 `dlsym("cu*")` 走 lupine 的 dlsym、返回 lupine 指针 → 绕过 adapter 拦截。
    **不可取**（对 dlsym 型加载方失效）。
- **服务端（方案 C：库 PRELOAD 进 lupine_driver_server）**：
  - lupine_driver_server 只依赖真 `libcuda`/`libnvidia-ml`，**不导出 dlsym**；库是进程内唯一的 dlsym 导出者
    （遮蔽 glibc 的 dlsym）—— 与本地 vGPU 模式完全一致，**无新的冲突问题**。
  - 注意：lupine-server 的 NVML 桩经 `nvml_symbol<Fn>()`（`nvml_server.cpp:46-56`）用 `dlsym(handle,name)` 取真函数。
    PRELOAD 的库会遮蔽该 `dlsym` → 对库已 hook 的 nvml 函数返回库的 hook（虚拟化自然生效），未 hook 的返回真函数。
    需 spike 验证所有路径（直接引用、句柄 dlsym）均按预期拦截或直通。

#### 4.0.4 "合并 lupine + library 成一个 so"（方案 D 客户端融合）

- 若把 library 逻辑**编入 lupine-client**（客户端融合），仍需在客户端 hook 并转发，核心隔离仍受
  "远端 NVML 高频轮询"限制 → 与方案 B 同病，且改造量大、维护成本高。
- 服务端侧（方案 C）用 **PRELOAD（C-1）即可，无需合并工程**。合并成单 so 不解决任何 C-1 解决不了的问题。
- 结论：**不推荐合并工程；方案 C 采用 C-1（进程级 LD_PRELOAD 库进 lupine_driver_server）。**

### 4.1 方案 A：纯客户端记账适配层（`library-remote`，零 lupine 改动）

**做法**：复制 `library/` → `library-remote/`；远程模式改三处：
1. `loader.c`：真函数解析改为 dlopen lupine 的 `libcuda.so.1`/`libnvidia-ml.so.1`（或 RTLD_NEXT）。
2. `cuda_hook.c`：`used` 不再走 NVML 扫描，全部显式分配/回收计入 vmem ledger；OOM 判定 `ledger + request > total`。
3. `nvml_hook.c`：新增 `nvmlDeviceGetComputeRunningProcesses` 等 hook，进程表用 ledger 按本地 PID 重建。
   （进程表为**预留量口径**的合成数据。）

**优点**
- 对 lupine **零改动**，解耦最彻底（仅依赖导出的 `cu*`/`nvml*` 标准符号）。
- 客户端侧全部完成，无服务端部署变更。

**缺点 / 利害**
- **隐式内存丢失**：`记录值 < 实际占用`，会低估用量、可能绕过隔离（上下文大小时不可控）。这是硬伤。
- 进程表是"预留量"合成值，非 NVML 真实口径，nvidia-smi 显示的是近似值。
- **核心隔离不可行**：利用率 watcher 需高频轮询 NVML（本地 100ms 级），远程下每个采样是一次 RPC 往返，
  网络开销不可接受；且远端利用率是宿主机聚合值，无法归属到本 Pod。
- 需要新写"全量记账"逻辑（现有 ledger 只为 UVA 服务），改动其实不小。

**结论：仅作对比基准，不推荐。** 偏离了本地库已验证的 NVML 真实口径，且没有通往核心隔离的路径。

---

### 4.2 方案 B：小改 lupine 暴露远端 PID + 适配层过滤真实 NVML 内存

**做法**：
1. **lupine 极小补丁（约几十行）**：新增一个 RPC（或在握手响应头里加字段），让客户端查询"本连接对应的 server
   子进程 PID"。子进程内 `getpid()` 即可（`server.cpp` 的 `client_handler` 里已能取到 conn 与 session）。
   - 推荐形态：新增导出符号 + 手动 RPC handler（lupine 已有 `lupine_manual_handlers` 机制，`server.cpp:100-288`）。
   - 同时保持 `client.exports`/协议兼容，升级影响面最小。
2. **`library-remote` 适配层**：
   - 首次 `cuInit` 后查询自己的远端 PID，写入 **pod 作用域共享映射表**（§5）。
   - `used` 口径：调转发后的 `nvmlDeviceGetComputeRunningProcesses`+`nvmlDeviceGetGraphicsRunningProcesses`，
     按共享表中本 Pod 的远端 PID 集合过滤并累加 `usedGpuMemory` → **真实用量（含隐式内存）**。
   - 新增 `nvmlDeviceGet*RunningProcesses` hook：把远端列表过滤为**本 Pod 条目**后返回（PID 改写为本地 PID）。
   - **设备可见性裁剪**：经 lupine，客户端可见 server 全部设备。adapter 需按配额裁剪（只暴露已分配设备），
     否则可绕过配额使用未分配设备。复用现有 `MANAGER_VISIBLE_DEVICES` 设备索引重映射机制（`loader.c:2376-2492`）。

**优点**
- **拿到真实 NVML 口径内存**（含上下文等隐式占用），与本地库一致，无 HAMI 探测必要。
- 进程表可从真实数据过滤重建，nvidia-smi 显示正确、语义清晰。
- 远端 PID 映射为 1:1，建模简单（§5）。
- lupine 改动极小、机制化（新增一个只读 RPC），不破坏 lupine 升级主线。

**缺点 / 利害**
- 仍是对 lupine 的**私有扩展**：lupine 上游升级需重新打该小补丁（维护成本低但存在）。
- **仍只解决内存**：核心隔离同样受"高频 NVML 轮询走网络"约束，不可行。
- 需要一个 mmap 共享映射表 + 生命周期管理（新增模块）。
- 客户端需处理 dlsym 双导出（§4.0.3）。

**结论：内存隔离的正确最小方案，适合快速跑通 Phase 1 闭环。核心仍待方案 C。**

---

### 4.3 方案 C：改造 lupine-server，在子进程内加载 `libvgpu-control.so`（内存 + 核心全量）

**核心洞察**：lupine server 为每个连接 fork 的子进程，就是"一个只属于该客户端的、直接面对真驱动的进程"——
这恰好是 `libvgpu-control.so` 设计的使用场景。把现有库**放进 server 子进程**，等于把"每个客户端"当成
"一个本地 vGPU 容器"，隔离逻辑天然就地生效。

**做法（C-1：进程级 LD_PRELOAD，基于 §4.0 分析）**：
1. **lupine-server 补丁**：
   - 整个 `lupine_driver_server` 进程 `LD_PRELOAD libvgpu-control.so` 启动（**必须 PRELOAD，dlopen 后置无效**，
     §4.0.1）。父进程不碰 CUDA，无副作用；子进程经 fork 继承。
   - 子进程在 `client_handler` 读到 `LUPINE_SESSION` 后（`server.cpp:421-429` 已有该时机），
     `setenv(VGPU_CONFIG_PATH=<该 session 的配置区路径>)`，并自注册 PID 进会话进程表；库在首次 CUDA 调用时
     惰性读取会话配置区（`load_necessary_data`）。**配置内容与更新机制见 §6**（v0.3 修正：非 per-child 独立 env，
     而是会话级共享配置区）。
2. **library 小改（`SESSION` 兼容模式，容器级记账）**：
   - 关键修正：**不能只按子进程自身（getpid）记账**。一个容器多个进程 = 多个子进程，必须按**会话全部子进程**
     聚合（§6.5）。新增 `SESSION` 兼容模式：`accumulate_used_memory()`（`cuda_hook.c:2342`）与利用率过滤按
     "pid ∈ 会话进程表"判断。
   - 配置从 per-session 配置区读取（`VGPU_CONFIG_PATH`，§6）。
   - 新增 `nvmlDeviceGetComputeRunningProcesses` 等 hook：过滤为仅本会话条目（§6.5）。
   - 配置读取需保证 per-child env 优先于 GPU 节点全局 `vgpu.config`（`load_controller_configuration` 目前文件优先，§6.4）。
3. **Go 侧（配额下发，服务端权威 + 设备访问控制）**：见 §6。

**优点**
- **内存 + 核心一次到位**：
  - 内存：库在子进程内做预算校验 + 虚拟化上报（`cuMemGetInfo`/`cuDeviceTotalMem`/`nvmlDeviceGetMemoryInfo`），
    客户端收到的是限额视图。
  - 核心：`rate_limiter` 拦截的是**真实 kernel 启动**（子进程内）；利用率 watcher 在 **GPU 节点本地**轮询 NVML，
    无网络开销，且按**会话进程表**过滤得到**本容器真实 SM 利用率**（§6.5）——与本地 vGPU 语义完全一致。
- **复用现成库逻辑最多**：内存预算、虚拟化上报、令牌桶、利用率控制器全部原样复用，新增代码极少。
- **客户端零改动**：Pod 内只是普通 lupine client，无需适配层、无需远端 PID 映射表、无 dlsym 双导出问题（§4.0.3）。
- 每个客户端一个子进程 = 天然隔离边界（与 lupine 现有 fork 模型一致）。

**缺点 / 利害**
- **必须维护 lupine-server 分叉**（fork 或补丁）。补丁面很小（PRELOAD + session→env + 设备 allowlist），但升级要跟着打。
- 每连接一个子进程 + 每子进程一个利用率 watcher 线程：连接数较多时资源开销线性增长（与 lupine 现状一致）。
- 配额下发机制需设计（session→配置，服务端权威），涉及 Go 侧新组件/接口（§6）。
- 兼容性风险需 spike 验证：库的 `pthread_atfork` 处理器与 lupine 的 fork/signal 处理是否冲突；库的 dlsym 遮蔽
  `nvml_symbol<>()`（§4.0.3）；`/proc/driver/nvidia/version`、版本化 libcuda 的解析在 GPU 节点天然满足。
- 共享/ledger 路径需 per-session 作用域（子进程间不能共用 `/tmp/.sm_node`/`/tmp/.vmem_node`，否则互相串）。

**结论：唯一能同时覆盖内存 + 核心的完整方案，是最终目标的正确架构。**

---

### 4.4 方案 D：合并 lupine + library 成一个 so / 源码融合

为完整性列出。**不推荐**：
- 客户端融合（library 编入 lupine-client）：核心隔离仍受远端 NVML 轮询限制（同 B），改造量大。
- 服务端融合（library 编入 lupine_driver_server）：可用 C-1（PRELOAD）等价达成，无需合并。

---

### 4.5 方案对比总表

| 维度 | A 纯记账适配层 | B 远端PID + 适配层 | C server 子进程加载库 |
|---|---|---|---|
| 内存隔离 | 可，但**预留量口径**（丢隐式内存） | 可，**NVML 真实口径**（含隐式） | 可，**NVML 真实口径** |
| 核心隔离 | 不可行（网络轮询） | 不可行（网络轮询） | **可行**（节点本地轮询） |
| lupine 改动 | 无 | 极小（只读 RPC 暴露子进程 PID） | 小（PRELOAD + session→env + 设备 allowlist） |
| 客户端改动 | 适配层（最大） | 适配层（中等，含 dlsym 双导出处理） | **无**（普通 lupine client） |
| 复用现有库 | 部分（记账逻辑要新写） | 大部分（内存逻辑） | **几乎全部**（加 SESSION 模式 + 会话配置区） |
| 新模块 | 全量记账 + 进程表重建 | 远端PID映射表 + 进程表过滤 | session→配额下发 + 设备 allowlist（Go） |
| 升级维护 | 无 lupine 依赖 | 需重打小补丁 | 需维护 server 分叉 |
| 建议定位 | 不采用 | **Phase 1 快速闭环** | **最终方案（Phase 1+2）** |

---

## 5. 关键设计：本地 ↔ 远端 PID 映射（v0.2 草案，待定）

> 结构按评审意见改为**按设备组织**，天然兼容多 server（一个本地进程在多 server 下有多条远端 PID 记录）。

### 5.1 结构（v0.2 草案）

```c
struct pid_map_entry {
    pid_t   local_pid;      // 本地进程 PID
    pid_t   remote_pid;     // 该进程连接的 server 子进程 PID
    uint32_t conn_index;    // 连接序号：多 server 时区分；单 server 恒 0
    uint32_t gen;           // 版本号：规避本地 PID 复用
};

struct device_proc_entry {
    char    uuid[48];                           // GPU 设备 UUID（经 lupine 透传的真实 UUID）
    struct  pid_map_entry entries[1024];        // 该设备上的本地→远端 PID 映射
    uint32_t entry_count;                       // 有效条目数（遍历上限）
    uint64_t seq;                               // 设备级 seqlock：读无锁、写加锁
};

struct remote_pid_map {
    struct  device_proc_entry devices[16];      // 按设备记录（上限对齐 MAX_DEVICE_COUNT）
    uint64_t seq;                               // 表级 seqlock：devices[] 结构变更时
};
```

### 5.2 语义与设计注记

- **映射按设备、注册按进程**：一个本地进程连接一个 server 后，在其使用的**每个设备**条目下各注册一条
  `{local_pid, remote_pid(同值), conn_index, gen}`。单节点多 GPU 时同一 `remote_pid` 出现在多个设备条目
  （子进程 PID 全设备共用）——按设备冗余存储，换取过滤 O(1)。多 server 时同一 `local_pid` 在不同设备/
  `conn_index` 下有多条不同 `remote_pid` 记录。
- **为何按设备**：NVML 进程表是 per-device 的。过滤 `device X` 的列表只需取 `devices[i].entries[].remote_pid`
  集合，无需全局遍历。
- **注册时机**：本地进程首次 CUDA 调用（adapter `cuInit` 之后）查询 lupine 新导出符号，逐设备注册/刷新。
- **读取/过滤**：`used` 统计与进程表 hook 时，按设备取 `remote_pid` 集合，过滤 NVML 列表；返回给应用时把
  `remote_pid` 改写为对应 `local_pid`（进程表显示本地 PID）。
- **失效与刷新**：
  - 进程退出 → 连接关闭 → server 子进程回收 → NVML 不再出现该 PID，天然失效。
  - 本地 PID 复用 → `gen` 递增覆盖；旧条目 gen 低忽略。
  - 断线重连 → `remote_pid` 更新 + `gen` 递增。
  - 死亡条目惰性清理：读取时校验 `local_pid` 是否存活（`kill(pid,0)`）可延迟剔除。
- **锁**：设备级 `seq` 覆盖 entries 读写；表级 `seq` 覆盖 devices[] 结构变更。读写规范参考现有
  `/tmp/.vmem_node`（`loader.c:2504-2720`）与 `vgpu.config` seqlock（`docs/resource_data_seqlock_versioning_design.md`）。
- **容量**：`devices[16]`/`entries[1024]` 固定上限，与现有 `MAX_DEVICE_COUNT`/`MAX_PIDS` 风格一致；超限策略待定。
- **待定项**：是否记录 server 标识（UUID 全局唯一，一般无需）；是否 per-(device,conn) 索引；扩容策略。
- **方案 C 不需要该表（客户端侧）**：C 在 GPU 节点做容器级记账，用的是**服务端会话进程表**（§6.2/§6.5，
  子进程自注册），与 §5 的客户端映射表是同一问题的两侧解法。该表服务于方案 B，或后续"显示层 PID 回写"的
  可选优化（把子进程 PID 映射回本地 PID）。

---

## 6. 配置下发与应用（方案 C 重点，v0.3 重写）

### 6.1 问题重述：为什么"每子进程独立注入配置"是错的

v0.2 曾假设"子进程 `setenv(CUDA_MEM_LIMIT_*=...)` 独立注入即可"。**这是错误的**，原因：

1. **一个容器可能对应多个 server 子进程**：lupine client 连接是**进程级**状态（`nvml_client.cpp:47-49`），
   容器内每个使用 CUDA 的进程各自建立连接 → server 各 fork 一个子进程（`server.cpp:668-727`）。
   一个容器 N 个进程 = N 个子进程；多 server（`LUPINE_SERVER=a,b`）时更多。
2. **隔离单位是"容器"，不是"进程"**：配额（如某设备 16GB）属于整个容器。若每个子进程只按自己的用量限速
   （v0.2 的 `SELF_PID`），多进程容器会**超配额**——两个进程各用 10GB（配额 16GB），各自都以为合法，实际 20GB。
3. **配置必须对服务同一容器的全部子进程一致**，且**每设备配置匹配**（同一设备同一限额、同一可见性）。

因此配置应用**不是**"每子进程独立 env"，而是 **"GPU 节点上的会话级（session）配置区，所有子进程共享"**。

### 6.2 会话模型：把"容器"概念落到 GPU 节点

```
GPU 节点（lupine-server 所在）
  ├─ lupine_driver_server（进程级 LD_PRELOAD libvgpu-control.so）
  │    └─ fork 子进程 per 连接
  │         ├─ child(进程A) ─┐
  │         ├─ child(进程B) ─┼─ 同一容器(S) 的多个进程 → 共享同一会话配置区
  │         └─ child(进程C) ─┘
  └─ 会话配置区 /etc/vgpu-manager/remote-sessions/<session>/session.config
       ├─ resource_data_t（device_t[16]，seqlock）    ← 本容器的每设备限额（§6.3）
       └─ 会话进程表 child_pid[64]（seqlock）          ← 本容器的服务子进程集合（§6.5）
```

- **会话 id = `LUPINE_SESSION`**：客户端握手头 `x-lupine-session`（`h2.cpp:710-724`），子进程在 `client_handler`
  内 `rpc_http2_session_id()` 读取（`server.cpp:421-429`）。
- **多 server**：session id 全局唯一（如 pod UID）；每 GPU 节点各持该容器**在本节点**的设备切片（本节点
  `session.config` 只含本节点设备）。

### 6.2.1 单 lupine-server 多容器并发：按连接区分容器、按 session 读配置（评审补充）

一个 `lupine_driver_server` 会同时服务**多个不同容器**，判别与取配置的关键：

1. **判别单位 = 连接（fork 的子进程）**：每个连接携带各自独立的 `x-lupine-session` 头
   （`h2.cpp:525` 解析进各自 transport 的 `session_id`）。子进程经 `rpc_http2_session_id(&conn)`
   （`h2.cpp:850-857`）读到的**只属于自己这条连接**的 session id —— 这是多容器并发的判别基础。
   **不需要也不应该有任何全局 session→child 查表**：判别完全由"每个子进程自己的连接头"决定。
2. **配置路径 = 由本连接 session id 推导**：子进程 `setenv(VGPU_CONFIG_PATH=/etc/vgpu-manager/remote-sessions/
   <sanitized-session>/session.config)`，再放行首个 CUDA RPC。同一容器所有进程的 `LUPINE_SESSION` 相同 →
   子进程读同一配置区（一致性）；不同容器 session 不同 → 读不同配置区（隔离）。**无全局配置、无跨容器串读**。
3. **安全（session id 是客户端提供的，不可信）**：
   - **路径消毒**：session id 必须只允许安全字符（`[A-Za-z0-9_.-]`），拒绝 `/`、`..` 等，防路径穿越。
   - **防伪/防冒用**：session id 不应是裸 pod UID（容器可自设 `LUPINE_SESSION` 冒用他人 session 窃配额），应由
     控制面/scheduler 签发**不可预测的令牌**（随机 token），agent 登记；或由 agent 校验连接来源合法性。
   - **fail-closed**：若子进程首个 CUDA 调用时 session 配置区不存在（agent 未登记 / 已过期 / 伪造 session），
     必须**拒绝 CUDA**（返回 `CUDA_ERROR_*`），绝不无限制放行。
4. **生命周期**：agent 在分配时建配置区、pod 结束删除；孤儿/过期 session 由 fail-closed 拦截。

> 对方案 B（客户端适配层）无此问题：适配层在容器内，天然只读本容器配置；但 B 的适配层同样要校验
> 服务端是否允许该 session（防越权），见 §6.3 来源 (a)/(b)。

### 6.3 配置内容与来源（服务端权威）

**内容 = `resource_data_t`**（复用现成 ABI，`include/hook.h:245-269`）：`devices[MAX_DEVICE_COUNT]` 每个
`device_t` 含 `uuid`/`total_memory`/`real_memory`/`hard_core`/`soft_core`/`core_limit`/`hard_limit`/
`memory_limit`/`memory_oversold`/`activate`（`hook.h:219-235`），外加 `compatibility_mode`、pod 身份等。

**来源二选一：**
- **(a) GPU 节点 agent 权威（生产推荐）**：GPU 节点运行 vgpu-manager 组件（现有 device-plugin 的"远程 server"
  角色或新 agent）。scheduler 分配时经控制面下发 "session S → devices[UUIDs+limits]"；agent 落盘
  `session.config`（复用 `WriteVGPUConfigFile`/`ModifyDevice`，`pkg/config/vgpu/vgpu_config.go:477-627`）。
  子进程只读，**不信任客户端自报**（安全边界）。
- **(b) 客户端透传（仅可信集群试点）**：容器内 device-plugin 已注入 `CUDA_MEM_LIMIT_*`/`MANAGER_VISIBLE_DEVICES`
  等 env；lupine-client 经握手头/RPC 转发给 server，子进程写入 session.config。
  **缺点：容器可自行篡改 env → 隔离可被绕过**；仅用于快速验证闭环。

### 6.4 应用 / 同步 / 更新机制

| 时机 | 动作 |
|---|---|
| 分配 | scheduler 下发 → agent 落盘 session.config（含空会话进程表） |
| 子进程启动 | lupine 补丁读到 session 后 `setenv(VGPU_CONFIG_PATH=<session.config>)`；首个 CUDA 调用时库 mmap 会话区、**自注册 PID 进会话进程表** |
| 记账/限速 | 每个子进程算 `used = Σ NVML usedGpuMemory(pid ∈ 会话进程表, device)`（真实口径含隐式）；预算判定 `used + req > limit → OOM`；所有子进程读同一文件 → **容器级一致**（§6.5） |
| 更新 | agent 在原文件内 seqlock 改写（改 device_t 限额）→ 子进程每次 `get_device_snapshot()` 重读 → **热更新**，无需重启子进程 |
| 销毁 | pod 结束 → agent 删除会话区；连接关闭 → 子进程回收 |

- **热更新**依赖现有 seqlock 机制：库每次 `get_device_snapshot()`（`loader.c:1565-1590`）读快照，Go 侧
  `ModifyDevice`（`vgpu_config.go:533-559`）在原文件上改写。**只需把文件路径换成 per-session**（库新增
  `VGPU_CONFIG_PATH` 覆盖 `CONTROLLER_CONFIG_FILE_PATH`，`loader.c:2856` 目前文件优先）。
- **一次性初始化项**（compatibility_mode、设备映射、oversold 开关）只读一次；**限额类**（memory_limit 等）逐次读。
  静态配额场景足够；动态配额变化需子进程重新初始化会话区（Phase 2 再议）。

### 6.5 容器级记账（SESSION 兼容模式）与每设备配置匹配

- **库新增 `SESSION` 兼容模式**：`accumulate_used_memory()`（`cuda_hook.c:2342-2408`）与利用率过滤的 PID 归属
  判断改为 "pid ∈ 会话进程表"（替代本地模式的容器 cgroup/pid 过滤）。子进程 = 容器的一个进程，会话表 = 容器
  全部进程 → `used` 是**容器级真实口径**（含隐式内存，NVML 驱动值）。
- **会话进程表**：子进程自注册（首个 CUDA 调用时把自己的 PID 写入会话区）；死亡子进程因 NVML 不再上报其 PID
  而自然失效，表项可惰性清理。
- **多进程竞态**：多子进程并发分配存在 NVML 滞后导致的 TOCTOU 窗口（与本地多进程容器一致），可接受。
- **每设备配置匹配**：`get_host_device_index_by_cuda_device`（`loader.c:2421-2461`）走 `cuDeviceGetUuid_v2` →
  UUID 匹配 `devices[].uuid`。**前提是子进程看到的设备与容器分配一致** → 必须配合设备级 allowlist + 序号重映射（§6.6）。
- **`nvmlDeviceGetComputeRunningProcesses` 等 hook**：返回会话进程表内的条目（真实数据），PID 可改写为本地 PID
  （显示优化，可选）。
- **共享区 per-session 作用域**：`/tmp/.sm_node`/`/tmp/.vmem_node`（`loader.c:1873, 1736`）必须按 session 分目录，
  否则跨租户串扰（Phase 1 禁超卖，ledger 仅预留）。

### 6.6 设备级访问控制 + 序号重映射（"每个设备配置匹配"的必要前提）

客户端经 lupine 默认可见 server **全部设备**；若不限制，客户端可用未分配 GPU，且序号错位导致 UUID 匹配失败。
两层一起做：

- **allowlist（限制可用设备集合）**：
  - (a) **lupine-server 补丁**：子进程内过滤 `cuDeviceGetCount`/`cuDeviceGet`，只返回本 session 允许的设备
    （隔离边界在服务端，推荐）。
  - (b) **库 SESSION 模式 hook `cuDeviceGetCount`/`cuDeviceGet`**：当前 `cuda_hooks_entry`（`cuda_hook.c:517`）
    未 hook 这两个函数（本地模式靠容器运行时 `NVIDIA_VISIBLE_DEVICES` 裁剪），远程需新增；按 session.config
    `devices[].uuid` 回落到物理序号返回。
- **序号重映射**：容器看到的序号 = 允许设备顺序；`cuDeviceGet(i)` 返回**物理序号**，后续按物理序号执行。

**方案 B 侧同理**：adapter 必须按配额裁剪设备可见性（复用 `MANAGER_VISIBLE_DEVICES` 设备索引重映射，
`loader.c:2376-2492`），否则同一漏洞。

### 6.7 与方案 B 的对照

方案 B（客户端适配层）**没有"多子进程共享配置"问题**：虚拟化全在容器内，配置直接读容器内 `vgpu.config`/env
（现有机制）；多进程容器的容器级记账靠 pod 作用域 `remote_pid_map`（§5）聚合。
**B 的 `remote_pid_map` 与 C 的会话进程表是同一问题的两侧解法**：B 在客户端按容器聚合远端 PID，C 在 GPU 节点
按会话聚合子进程 PID。

---

## 7. lupine 版本/制品矩阵与分发（评审新增）

### 7.1 问题背景

lupine 按 **CUDA 版本 × OS/glibc × 客户端/服务端/平台** 打多个制品（如
`lupine-client-cuda-12.9.1-ubuntu24.04-x86_64.zip`、`lupine-server-cuda-13.1.0-...`、windows 变体等）。
原因：lupine 的 codegen 从**真实 CUDA 头**生成 RPC 桩，wire 结构与导出符号面随 CUDA 版本变化；且依赖 glibc 版本。

vgpu `library/` 是**单制品**：手维护 `cuda-subset.h`/`nvml-subset.h` + 运行时 dlopen 版本化 `libcuda.so.<ver>`
（`loader.c:1116-1203`），天然跨版本。两者模式不同，不能照搬 vgpu 的"一库打天下"。

### 7.2 兼容方向（关键事实）

1. **符号面子集方向**：客户端 shim 编译的 CUDA 版本决定其导出的 `cu*` 面。**client ≤ server** 时，客户端只调用
   server 一定有的函数 → 兼容；**client > server** 时可能调用 server 缺失的新 API → 失败。即兼容方向是
   **"客户端版本 ≤ 服务端版本"**。
2. **wire 结构布局**：常用函数（`cuMemAlloc_v2`/`cuMemGetInfo_v2`/`cuLaunchKernel` 等）的入参结构体在相邻 CUDA
   版本间基本稳定；`nvmlProcessInfo_t` v1/v2/v3 大小需核对。跨版本兼容需 spike 实测（如 client-12.9 ↔ server-12.4/12.6/12.8）。
3. **驱动版本 vs 应用 cudart**：pod 的 cudart 经 `cuDriverGetVersion` 看到的是 **server 驱动版本**；cudart 有最小
   驱动校验 → **pod cudart 必须 ≤ server 驱动支持的 CUDA**（与直接在 GPU 节点跑应用一致）。这是远程 GPU 的固有
   约束，也让选型简化：**server 驱动版本应 ≥ 所有 pod 的 cudart 版本**。

### 7.3 分发策略（推荐：收敛到单一基准制品）

- **服务端**：GPU 节点驱动版本确定（一般全集群统一）→ 打一个 `lupine-server-cuda-<节点驱动CUDA>` 制品，经
  daemonset/init 容器分发到节点。用最新驱动（如 CUDA 12.9/13.x）以覆盖所有 pod cudart。
- **客户端**：选基准 = **预期最高 pod cudart**（通常 ≤ server 基准），打一个 `lupine-client` 制品，随 vgpu-manager
  镜像/注入层分发。所有 cudart ≤ 该基准的 pod 都可用。
- **spike 验证跨版本 wire 兼容**：若 `client(12.9) ↔ server(12.4/12.6/12.8)` 全兼容 → **单制品成立，矩阵彻底消除**。
  验证点：同 RPC op id（CRC32 of name，名稳定）、常用结构体布局一致、新旧 API 子集关系。
- **若逐版本不可避免**：退化为**版本目录**——chart values 维护 `lupine.cudaVersion` 映射；pod 经 annotation
  （如 `nvidia.com/vgpu-remote-cuda-version`）声明所需 client 版本；scheduler/plugin 据此注入对应 shim。开销大，
  仅作 fallback。
- **构建对齐**：`versions.mk`/`Dockerfile.base` 增加 `LUPINE_CUDA_VERSION`、`LUPINE_ARTIFACT_BASE_URL`、sha256 校验
  （制品自带 SHA256SUMS）；GPU 节点与 pod 注入层各自引用对应制品。
- **OS/glibc**：client .so 的 glibc 基线 ≤ pod 运行时的 glibc；server 二进制与 GPU 节点基础镜像匹配
  （ubuntu22.04/24.04 对齐）。

### 7.4 对方案的影响

- 方案 B：版本问题集中在 lupine shim 本身；adapter/`library-remote` 保持单制品（运行时 dlopen shim，不关心版本）。
- 方案 C：server 制品按节点驱动定版本；客户端普通 lupine client 制品同样按 §7.3 选基准。两方案都**没有额外的
  vgpu 库版本矩阵**。

---

## 8. 推荐路线与分阶段实施计划

### 总体判断
- 只做内存且要"最少改动快速验证" → **方案 B**。
- 最终要做内存 + 核心、且不想做两套客户端适配 → **方案 C**（B 是其 Phase 1 的可选快速探针，可被 C 吸收）。

### 推荐（折中）
**先做方案 B 的快速验证闭环（Phase 1 内存），随即迁移到方案 C（Phase 1 内存 + Phase 2 核心）。**
B 阶段积累的 lupine 补丁经验（session/RPC 接入点）与 Go 侧配额下发模型，可直接平移到 C。

### Phase 0 —— Spike 验证（1 周内）
- [ ] S1 符号链（B）：`LD_PRELOAD=adapter:lupine-libcuda:lupine-libnvidia-ml` 下最小 CUDA 程序走通
      adapter→lupine→server；验证 **dlsym 双导出顺序**（§4.0.3）、RTLD_NEXT/dlopen 链、`cuGetProcAddress_v2` 路由。
- [ ] S2 远端 PID 补丁可行性：在 lupine server 新增只读 RPC 返回子进程 PID，客户端实测拿到的 PID 与
      NVML 进程表对得上。
- [ ] S3 记账口径：确认经远端过滤的 `usedGpuMemory` 包含上下文等隐式内存（对比显式分配和 NVML 值）。
- [ ] S4（C）PRELOAD + 会话配置可行性：`LD_PRELOAD libvgpu-control.so` 进 `lupine_driver_server`，验证子进程内
      拦截生效、`pthread_atfork`/`nvml_symbol` 兼容（§4.0.3）、`VGPU_CONFIG_PATH` 会话配置生效。
- [ ] S5（C）多进程容器记账：同一容器 2 个进程 → 2 个子进程，验证会话进程表聚合的 `used` 为容器级口径
      （2×10GB > 16GB 配额必须被拒绝）。
- [ ] S6（版本）跨版本 wire 兼容：`client-12.9 ↔ server-12.4/12.6/12.8` 冒烟（§7.3）。

### Phase 1 —— 内存隔离（方案 B 或 C 其一）
**若走 B：**
- [ ] `library-remote/` 独立子工程（复制 `library/`，改真函数解析为 lupine）。
- [ ] lupine 补丁：新增远端 PID 查询 RPC/导出符号。
- [ ] mmap 共享映射表模块（§5）+ 注册/刷新/失效逻辑。
- [ ] `cuda_hook.c`：`used` 改为按远端 PID 过滤 NVML；预算判定不变。
- [ ] `nvml_hook.c`：新增 `nvmlDeviceGet*RunningProcesses` 过滤 hook。
- [ ] adapter 设备可见性裁剪（`MANAGER_VISIBLE_DEVICES` 重映射）。
- [ ] Go 侧注入：`LD_PRELOAD`/`LD_LIBRARY_PATH`/`LUPINE_SERVER`/`VGPU_REMOTE_MODE`/限额 env；远端 GPU 发现写 config。
- [ ] 端到端：单节点多 GPU，nvidia-smi/`cuMemGetInfo` 显示限额视图，超限 OOM，多 Pod 互不干扰。

**若走 C（推荐直接做）：**
- [ ] lupine-server 补丁：进程级 `LD_PRELOAD libvgpu-control.so`；子进程读到 session 后设 `VGPU_CONFIG_PATH`
      并自注册 PID 进会话进程表；设备 allowlist + 序号重映射。
- [ ] library 小改：`SESSION` 兼容模式（会话进程表过滤记账/利用率）；`VGPU_CONFIG_PATH` 支持；
      `/tmp` 路径 per-session 作用域；可选 hook `cuDeviceGetCount`/`cuDeviceGet` 做设备重映射。
- [ ] Go 侧：GPU 节点 agent 维护 session→配额（复用 `device_t`/`WriteVGPUConfigFile` 落盘 session.config），
      scheduler 分配时登记。
- [ ] 端到端：同 B 的验收标准，且**验证多进程容器容器级记账**（S5），客户端为**普通 lupine client**（无适配层）。

### Phase 2 —— 核心隔离（仅 C 支持）
- [ ] 库的 `rate_limiter` + 利用率 watcher 在子进程内启用（本地轮询，`SESSION` 过滤）。
- [ ] 验证多租户 SM 抢占下各自限速正确；可选 `CUDA_SM_SHARED_BUCKET` 在节点级多客户端间共享（per-session 桶）。
- [ ] 动态配额更新（缩容/限速热调整）的子进程重新初始化机制。
- [ ] 权衡是否回补 B 的显示层 PID 改写（把子进程 PID 映射回本地 PID，纯展示优化，可选）。

---

## 9. 风险与未决问题

| 风险/问题 | 影响 | 缓解/决策点 |
|---|---|---|
| dlsym 双导出（adapter 与 lupine-client 都导出 `dlsym`） | B 路径符号解析不确定性 | §4.0.3：adapter 保留 dlsym 导出、回退走 glibc；S1 spike 实测顺序 |
| dlopen 后置无法拦截已解析引用 | C 路径拦截失效 | 必须进程级 LD_PRELOAD（库最先入全局符号表），父进程不碰 CUDA（§4.0.1） |
| 库遮蔽 `nvml_symbol<>()` 的 dlsym | C 路径 NVML 桩行为 | S4 spike 验证已 hook/未 hook 函数均按预期拦截或直通（§4.0.3） |
| lupine `pthread_atfork` 与 server fork/signal 交互 | C 路径子进程稳定性 | S4 spike；库的 atfork 处理器是独立 init，预期可兼容 |
| 配额下发安全边界 | C 路径客户端可伪造配额 | 服务端权威（GPU 节点 agent session→配额），不信任客户端自报（§6.3） |
| **多容器并发判别 + session 安全（v0.3 新增）** | 单 server 多容器时错读/冒用他人配置 | 按连接读各自 session 头定配置区；session id 消毒 + 控制面签发令牌 + fail-closed（§6.2.1） |
| **多进程容器记账一致性（v0.3 新增）** | C 路径每子进程独立限速 → 容器级超配额 | 会话级配置区 + 会话进程表聚合 `used`（SESSION 模式），S5 spike 验证（§6.1/§6.5） |
| 配置"文件优先 vs env" | C 路径 per-child env 被全局 config 覆盖 | per-session 配置区 + `VGPU_CONFIG_PATH`；远程节点无全局 config（§6.4） |
| 配置热更新 | C 路径限额变化不生效 | 复用 seqlock 在原文件改写 + `get_device_snapshot` 重读（§6.4） |
| 设备可见性不受限 | B/C 路径可绕过配额用未分配 GPU | 服务端设备 allowlist + 序号重映射（C）/adapter 裁剪（B）（§6.6） |
| `/tmp/.sm_node`/`/tmp/.vmem_node` 跨租户串扰 | C 路径多客户端 | per-session 作用域路径（§6.5） |
| `nvmlProcessInfo_v3` 结构体在 lupine 透传的 ABI 大小 | B/C 路径进程表 | spike 验证 v1/v2/v3 结构大小一致 |
| 断线重连时远端 PID 变化 | B 路径映射陈旧 | `gen` 版本号 + 惰性刷新（§5） |
| lupine 版本/制品矩阵 | 分发复杂、版本不匹配 | 单基准制品 + 跨版本 wire 兼容 spike；必要时版本目录 + annotation（§7） |
| 多 server（LUPINE_SERVER 多节点） | 索引/UUID/映射复杂度 | 本阶段限定单节点多 GPU；v0.2 映射结构已预留 conn_index（§5） |
| 核心利用率口径（Phase 2） | 客户端自算 vs 节点侧真实值 | C 用节点侧真实值（会话进程表过滤）；B 无解，故核心必须走 C |
| 性能 | 额外一跳函数指针 + TCP | 函数指针跳转开销可忽略；TCP 是 lupine 固有成本，与本方案无关 |

---

## 10. 附录：关键代码位置索引

### lupine（`D:\WorkSpace\GoCode\src\lupine`）
| 主题 | 位置 |
|---|---|
| 每连接 fork 子进程 | `server.cpp:668-727` |
| 子进程 handler / session 时机 | `server.cpp:346-475`，`421-429`（checkpoint_connection_ready） |
| 手动 handler 机制 | `server.cpp:100-288`（`lupine_manual_handlers`） |
| 进程表透传 handler | `nvml_server.cpp:58-97`（`handle_processes`），`103-125` |
| NVML 符号获取（dlsym 句柄查询） | `nvml_server.cpp:36-56`（`nvml_library`/`nvml_symbol`） |
| NVML 生成桩（透传） | `codegen/gen_nvml_server.inc`（GetMemoryInfo/GetUtilizationRates/...） |
| CUDA 生成桩（透传，直接引用） | `codegen/gen_server.cpp:1722-1826`（GetInfo/Alloc/Free） |
| 会话头 x-lupine-session | `h2.cpp:422, 710-724, 850-857` |
| 客户端连接状态（进程级） | `nvml_client.cpp:47-49`；`client.cpp` rpc_open |
| 虚拟设备序号= CUdevice | `routing.cpp:217-337`（`lupine_virtual_device_for_ordinal:292-307`） |
| 本地真 cuda 解析 | `client.cpp:647-686`（`lupine_local_libcuda_handle`） |
| 导出符号 | `client.exports`（`cu*`/`dlsym`/`lupine_checkpoint_*`），`nvml.exports`（`nvml*`） |
| dlsym 导出实现 | `client.cpp:8461-8520` |

### vgpu-manager `library/`
| 主题 | 位置 |
|---|---|
| 真函数解析（dlopen 版本化 libcuda/libnvidia-ml） | `src/loader.c:1116-1203`；`load_necessary_data:3040-3055` |
| dlsym 拦截器（glibc 真 dlsym 自发现） | `src/loader.c:1073-1110`（`init_real_dlsym`），`2167-2210` |
| 内存预算门 | `src/cuda_hook.c:321-379`（`prepare_memory_allocation`） |
| NVML 进程表→used（容器 PID 过滤） | `src/cuda_hook.c:2342-2530`（`accumulate_used_memory`/`get_used_gpu_memory_by_device`） |
| UVA ledger（按设备+本地PID） | `src/loader.c:2504-2720`（`malloc/free/get_used_gpu_virt_memory`） |
| 虚拟化上报 | `src/cuda_hook.c:3788-3892`（TotalMem/GetInfo）；`src/nvml_hook.c:63-132`（MemoryInfo） |
| 令牌桶 + 利用率 watcher | `src/cuda_hook.c:642-673`（rate_limiter）；`1459-1742`（utilization_watcher） |
| NVML hook 表 | `src/nvml_hook.c:36-44` |
| 设备索引/UUID 映射 | `src/loader.c:2376-2492` |
| 配置加载（文件优先→env 回退） | `src/loader.c:2856-2867`（`load_controller_configuration`），`2723-2854`（`init_g_vgpu_config_by_env`），`1478-1537`（mmap） |
| 配置 ABI（device_t） | `include/hook.h:219-269`；sm_node_region_t:529-609；memory_node_t:406-421 |
| 导出符号 | `deploy/libvgpu-control.exports.ld` |
| 构建/检查 | `build.sh`、`Makefile`（build/check/check-exports/test/test-nogpu） |

### Go 侧（vgpu-manager）
| 主题 | 位置 |
|---|---|
| Allocate 注入（.so + ld.so.preload） | `pkg/deviceplugin/vgpu/vnum_plugin.go:872-902` |
| DRA 注入（LD_PRELOAD env） | `pkg/kubeletplugin/vgpu.go:207-247, 295-330` |
| 限额 env 注入 | `pkg/deviceplugin/vgpu/vnum_plugin.go:757-803` |
| 配置写入（seqlock） | `pkg/config/vgpu/vgpu_config.go:477-627` |
