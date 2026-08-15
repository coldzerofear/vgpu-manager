# 跨节点远程 GPU 资源池 —— 内存/核心隔离方案研究与设计（v0.5）

> 状态：**Phase 1 + Phase 2 已实现，待真机验收**
> 关联代码：本仓库 `library/`（vGPU 硬隔离库，本地与远程共用）、`D:\WorkSpace\GoCode\src\lupine`（开源远程 GPU 转发）
> 配套阅读：仓库根目录 `AGENTS.md`（面向后续 AI 的任务与复查指南）

> ## v0.5 重要变更：`library-remote/` 已合并回 `library/` 并删除
>
> 本文档 §4 之后多处以 `library-remote` 指代远程适配层，那是 v0.3～v0.4 的形态。**现已不存在该目录**：
> 两棵树 94% 代码相同，独立维护意味着每个 bug 要修两遍且容易漂移，收益不抵成本。
>
> 远程能力现在是 `library/` 内部由 env 门控的分支：`VGPU_CONFIG_SESSION_PATH` 未设时，所有路径与行为
> **逐字回退**到合并前的本地语义（有回归测试钉住），本地部署不受影响。产物仍是单一 `libvgpu-control.so`，
> 它同时充当 LD_PRELOAD hook 库与 lupine 的 checkpoint provider。
>
> AIMD/auto 控制器曾随合并移除，**已于 2026-08-14 恢复**——delta 的增量按 sm² 缩放而池容量线性于 sm，
> 大 SM 卡上限不住核心（HAMi-core #274 同源，详见 docs/sm_controller_aimd.md 沿革节）；默认仍为 delta。
> 容器级共享令牌桶**默认开启**。
>
> 阅读下文时请把 `library-remote/xxx` 一律理解为 `library/xxx`。

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

v0.3.1 的增补（来自评审）：
- **单 lupine-server 多容器并发判别**：按连接读各自 session 头定配置区，session id 消毒 + 控制面签发令牌 +
  fail-closed（§6.2.1）。
- **lupine Python 客户端分析**：`connect()` 适配器 vs sidecar 机制的本质与适用范围，k8s pod 集成方式
  （§2.4、§8.2）。

v0.3.2 的增补（来自评审）：
- **确定 C 落地形态**：改造集中在 lupine-server（fork 后、请求前注入 env）+ `library-remote` 裁剪适配器
  （专责远程虚拟化），`library` 继续专责本地（§4.3.1）。
- **逐环可行性确认**：客户端会话传递/session 判别现成；设备隔离靠 `cuDeviceGetCount/Get` 重映射（handler 直调
  已核实）；nvml 路径靠库的 dlsym 拦截器（**必须保留**）；per-session 配置依赖 `loader_child_after_fork` 的
  once-guard 重置（已核实，不能裁剪）。
- **补充 10 条实施边界**（令牌签发、agent 落盘通道、fail-closed、fork 安全、env 时机、无 session 客户端、设备重
  映射、裁剪回归等）。

v0.3.3 的增补（来自实测）：
- **实测暴露"客户端本地路由"陷阱**：client/server 同机时 lupine-client 把设备 0 路由到本地 GPU（`routing.cpp:226-244`
  本地设备优先），`cuMemAlloc` 在客户端进程内执行，服务端 library 从未收到 → 分配 3000MB 成功但 nvidia-smi 显示
  2GB 的"假象"。**测试必须用无 GPU 客户端或 `LUPINE_DISABLE_LOCAL=1`**（§4.3.2）。
- **fork 安全补遗（方案 C 阻塞项）**：`load_controller_configuration` 守卫 `if (g_vgpu_config == NULL)`，
  `loader_child_after_fork` 不重置 `g_vgpu_config` → 子进程继承父配置，per-session 配置不生效 →
  必须在 atfork 中置 NULL 或改守卫（§4.3.2 关键修正 1）。
- **HOST 模式记账口径不适用**：子进程记账须用 SESSION 模式（会话进程表过滤），不能用 HOST 全机求和
  （§4.3.2 关键修正 2）。

v0.3.4 的增补（来自评审）：
- **新增方案 C-2：借 checkpoint provider 注入会话配置，零 lupine 源码改动**（§4.3.3）。lupine 的
  `liblupinecr.so` 扩展点在每个连接子进程的**首个 RPC 前**调用 `restore(connection_id)`（`connection_id`=
  `LUPINE_SESSION`），自研 provider 在此 `setenv VGPU_CONFIG_SESSION_PATH` + 注册 pid 进 `<session>/pids.config`，
  靠返回值做 fail-closed；`stop()` 清理 pid。
- 明确：LD_PRELOAD 仍须 server 进程级设置（运行时 setenv 不可行）；库的 fork 安全修正（`g_vgpu_config` 置 NULL）
  仍必须做。
- **实现进展**：provider 已落地 `library/include/checkpoint_provider.h`（vendor）+ `src/checkpoint_provider.c`
  （restore 注册 pid / stop 清理 / `VGPU_CONFIG_SESSION_PATH`），随 `libvgpu-control.so` 构建；
  会话目录布局见 §4.3.3（`config/vgpu.config`、`.vgpu_lock`、`.vmem_node`、`pids.config`、共享 `watcher/sm_util.config`）。
- 风险：依赖 provider ABI 调用时机契约；provider 缺失时靠库 fail-closed 兜底（§4.3.3 风险 1/2）。
- **单制品合并评估（§4.3.3.1）**：provider 可**内置于 `libvgpu-control.so`**（同一 .so 同时做 hook 库 + 注入
  provider）——glibc 按 realpath 去重、dlopen 返回已加载句柄；只需在导出脚本 `global:` 加
  `lupinecr_get_lupine_provider_v1`（否则被 `local: *` 藏掉）并 vendor 头文件。推荐。

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
- checkpoint（可选）：lupine 通过 `dlopen` 加载外部 provider（`liblupinecr.so`，ABI 见 `checkpoint_provider.h`）实现
  **连接级优雅排空/恢复**（SIGTERM 时 quiesce 在途 CUDA RPC → `checkpoint(connection_id)`；重连前 `restore(id)`）。
  **仓库不构建/发布该 provider**（仅测试 no-op 实现），vgpu-manager 不依赖它。其对设计的价值：`server.cpp:421-429`
  的 `lupine_server_checkpoint_connection_ready(session)` 正是我们方案 C 的 env 注入点（§4.3.1 边界 #6）。
- **完整环境变量参考**（语义、默认值、代码位置、对设计的使用矩阵）见 `docs/lupine_env_reference.md`。

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

### 2.4 lupine Python 客户端（`python/lupine`，PyTorch 适配层）

`D:\WorkSpace\GoCode\src\lupine\python\lupine\` 是**可选的 Python/PyTorch 辅助包**，不是远程链路的必需部分。
分三块：

| 模块 | 作用 |
|---|---|
| `__init__.py` | `lupine.connect()` 会话适配器：设置 `LUPINE_SERVER` env + `ctypes.CDLL(...RTLD_GLOBAL)` **提前加载** lupine `libcuda.so.1`，然后返回普通 `torch.device("cuda:N")`。**不注册新 torch backend**（README 明确：真正的 `torch.device("lupine")` 需要 PrivateUse1 内核，LUPINE 不这么做）——PyTorch 走原生 CUDA dispatch，shim 在驱动层劫持。必须在任何 PyTorch CUDA 操作前调用。 |
| `sidecar.py` + `worker.py` + `container.py` | **sidecar 机制**：当宿主进程是"没有 CUDA backend 的 PyTorch"（典型是 macOS 的 CPU-only torch build）时，用 Docker/Podman/nerdctl/Apple Container 拉起一个**容器化的 CUDA PyTorch worker**（镜像 `lupinemachines/lupine-pytorch-worker`），宿主经 stdin/stdout 管道 + JSON 帧 + 二进制 tensor 流做 op RPC（`upload/call/download/copy_from_cpu/release/ping`）。worker 镜像按 server 的 `x-lupine-cuda-version` HEAD 响应选择"镜像 CUDA 版本 ≤ server 版本"（`_worker_image_for_server`）。 |
| `tensor.py` | sidecar 的二进制 tensor 传输层 + `SidecarTensor`/`SidecarDispatchMode`（torch dispatch mode 劫持 op 转发）。 |

**与版本兼容分析的印证（§7）**：lupine 自己的 sidecar 选镜像策略正是"worker(CUDA) ≤ server(CUDA)"——佐证 §7.2 的
**兼容方向 = client ≤ server** 结论。

**对 vgpu-manager 的意义**：
- `connect()` 的本质只是"设 env + 提前加载 shim"，这些在 k8s 里**完全可以由注入层（device-plugin/DRA Allocate）
  完成，应用代码无需 import lupine**（见 §8.2）。
- sidecar 为 macOS/CPU-only 宿主设计，**k8s Linux pod 场景不适用**（pod 镜像可自装 CUDA 版 PyTorch，无意义再套一层
  容器 worker）。
- `LUPINE_SESSION` 被 sidecar 自动继承（`container.py:19` `_INHERITED_ENV`）——会话 id 的传递语义与 §6.2.1 一致。

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

**做法（进程级 LD_PRELOAD 是前提，§4.0.1；注入方式默认用 C-2 checkpoint provider，§4.3.3；C-1 为等价补丁路径）**：
1. **lupine-server 侧**：
   - 整个 `lupine_driver_server` 进程 `LD_PRELOAD libvgpu-control.so` 启动（**必须 PRELOAD，dlopen 后置无效**，
     §4.0.1）。父进程不碰 CUDA，无副作用；子进程经 fork 继承。
   - 注入由 **C-2 provider 的 `restore(session)`** 完成（§4.3.3）：消毒 → 校验 `<session>/config/vgpu.config` 存在 →
     `setenv(VGPU_CONFIG_SESSION_PATH=<base>/<session>)` + 注册 pid 进 `<session>/pids.config`；库在首次 CUDA 调用
     时惰性读取（`load_necessary_data`）。**配置内容与更新机制见 §6**（非 per-child 独立 env，而是会话级共享目录）。
2. **library 小改（`SESSION` 兼容模式，容器级记账）**：
   - 关键修正：**不能只按子进程自身（getpid）记账**。一个容器多个进程 = 多个子进程，必须按**会话全部子进程**
     聚合（§6.5）。新增 `SESSION` 兼容模式：`accumulate_used_memory()`（`cuda_hook.c:2342`）与利用率过滤按
     "pid ∈ `<session>/pids.config`"判断。
   - 配置从会话目录读取（`VGPU_CONFIG_SESSION_PATH`，§6）。
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

### 4.3.1 实施路径确认（v0.3.2）：lupine-server 补丁 + `library-remote` 裁剪适配器

**采纳的落地形态（评审确认）**：对 lupine 的**源码零改动**——注入由内置在 `libvgpu-control.so` 的 checkpoint
provider 完成（C-2，§4.3.3/§4.3.3.1）；`library-remote` 是 `library/` 的**裁剪适配器**，专责远程 GPU 虚拟化；
`library/` 继续专责本地 GPU 虚拟化。两者职责分离。

**完整链路**：
```
DRA Allocate 注入 LUPINE_SESSION=<控制面令牌>  (客户端 pod，所有进程共享)
   ↓
Pod 应用 CUDA → lupine-client shim (libcuda.so.1)
   ↓ HTTP/2 (x-lupine-session 头)
lupine-server accept → fork 子进程 per 连接
   ↓ 子进程:
   │  1. fork 后 load_provider() → provider.start() (server.cpp:719)
   │  2. rpc_http2_server_init (解析本连接 session_id, h2.cpp:525/850-857)
   │  3. provider.restore(session) (server.cpp:421-429, 首个 RPC 前):
   │       消毒 → 校验 <session>/config/vgpu.config → setenv(VGPU_CONFIG_SESSION_PATH)
   │       → 注册 getpid() 进 <session>/pids.config; 配额不存在返回非 0 = 拒连
   │  4. 库首次 cu*/nvml* 调用 → load_necessary_data → 读会话配置 (fork 安全, 见 B)
   ↓ 库(library-remote, 进程级 LD_PRELOAD) 在子进程内拦截/虚拟化
GPU 节点真实驱动
```

**可行性确认（逐环代码证据）**：

| 环节 | 证据 | 结论 |
|---|---|---|
| 客户端会话传递 | `h2.cpp:711` 读 `getenv("LUPINE_SESSION")`；注入层给容器所有进程同一 env | ✅ 现成 |
| server 判别连接→session | 每连接独立 transport，`rpc_http2_session_id(&conn)`（`h2.cpp:850-857`） | ✅ 现成 |
| 设备隔离/重映射 | 客户端设备表经 `cuDeviceGetCount`/`cuDeviceGet` 两个 RPC 枚举（`routing.cpp:217-277`）→ **provider setenv `CUDA_VISIBLE_DEVICES`，由驱动裁剪与重排，无需 hook**；NVML 不受该 env 约束，另用 hook（§6.6） | ✅ 已实现 |
| 直调 cu* 被拦截 | 进程级 PRELOAD → 库的 `cu*` 先于真 libcuda 入全局符号表（§4.0.1）；handler 直调（`gen_server.cpp:1760`）走 PLT → 库 hook | ✅ |
| nvml 路径被拦截 | lupine-server 的 NVML 桩经 `nvml_symbol<>()` **dlsym 句柄查询**（`nvml_server.cpp:46-56`）；库的 dlsym 拦截器服务端唯一导出、遮蔽该查询 → 返回库的 nvml hook | ✅ **库必须保留 dlsym 拦截器**（不可裁剪） |
| per-session 配置 fork 安全 | `loader_child_after_fork`（`loader.c:2994`）重置 `g_controller_config_init`/`g_cuda_lib_init`/`g_nvml_lib_init`/`g_reset_cuda_index_init` 等全部 once-guard → 每个子进程用**自己的 env** 重新初始化配置 | ✅ 现成机制，远程必须保留 |
| 记账口径 | 会话进程表（§6.5）+ NVML 过滤（SESSION 模式） | ✅ 库改造 |

**`library-remote` 裁剪设计（减少 hook 数、缩小影响面）**：

- **导出面 = 只导出需要的 hook**（不再像本地库导出全部 `cu*`+passthrough）：未导出的符号在子进程里直接绑定真驱动，零拦截开销。
- **Phase 1（内存）保留的 hook 族**：
  - CUDA：`cuMemAlloc(_v2/Pitch/Async/FromPoolAsync/Managed)`、`cuMemFree(_v2/Async)`、`cuMemGetInfo(_v2)`、
    `cuDeviceTotalMem(_v2)`、`cuArray*Create`/`cuMipmappedArrayCreate`（预算）、`cuMemHostAlloc/HostRegister`、
    **`cuCtxGetDevice`**（取当前设备）、`cuInit`、`cuDriverGetVersion`。设备重映射不在此列——CUDA 侧走 `CUDA_VISIBLE_DEVICES`（§6.6.1）。
  - NVML：`nvmlInit(_v2/WithFlags)`、`nvmlDeviceGetMemoryInfo(_v2)`、`nvmlDeviceGet*RunningProcesses(_v2/_v3)`、
    以及设备可见性族 `nvmlDeviceGetCount(_v2)`/`GetHandleByIndex(_v2)`/`GetHandleByUUID`/`GetHandleByPciBusId(_v2)`/
    `GetHandleBySerial`/`GetIndex`（§6.6.2）。
  - `dlsym` 拦截器（**必须保留**，nvml 路径依赖，见上表）。
- **裁剪掉（Phase 1 不需要，减少影响面）**：`cuLaunchKernel*` 系列（核心限速，Phase 2）、利用率 watcher、
  UVA 超卖/`vmem` ledger（约束禁用）、`sm_node` 共享桶、`cuGetProcAddress_v2` 路由（lupine-server 不调用）、
  Vulkan layer、外部 SM watcher 等。
- **注意**：`cuGetProcAddress_v2` 在本地库 hook 是因为客户端应用会用它；lupine-server 的 handler **不调用**它，
  所以远程可裁剪。但**客户端**若走方案 B 仍需，C 则无关（客户端是普通 lupine client）。

**需要补充的边界（评审新增）**：

1. **设备隔离（v0.4 定案，与本条原文不同）**：CUDA 侧**不 hook** `cuDeviceGetCount`/`cuDeviceGet`，改由 provider
   `setenv CUDA_VISIBLE_DEVICES=<会话设备 UUID 列表>` 让驱动裁剪并重排；NVML 侧因不受该 env 约束，必须 hook
   枚举族。两侧共用同一排序函数保证序号一致。详见 §6.6。**这是"每个设备配置匹配"的前提，不能省。**
2. **session 令牌必须是控制面签发**，DRA Allocate 注入的 `LUPINE_SESSION` 不是裸 pod UID（防容器自改冒用，
   §6.2.1）；同时 provider 对 session id **消毒**（`[A-Za-z0-9_.-]`，防路径穿越）。
3. **agent 落盘 `<session>/config/vgpu.config` 的时机与通道**：DRA 分配在 worker 节点（kubelet 权威），GPU 节点 agent
   需经**控制面通道**（scheduler/controller 或 agent 轮询）拿到 "session → devices+limits" 再落盘。必须在 pod 首个
   CUDA 调用前完成。此通道是新的 Go 侧工作项（§6.3 来源 (a)）。
4. **fail-closed**：provider `restore()` 校验 `<session>/config/vgpu.config` 不存在（agent 未登记/过期/伪造 session）
   → 返回非 0 拒绝连接；库侧在 `VGPU_REMOTE_MODE` 下无有效会话配置同样拒绝（双保险，§6.2.1）。
5. **fork 安全依赖 atfork handler**：`loader_child_after_fork` 重置 once-guard 是 per-session 配置生效的前提；
   **但只重置 once-guard 不够** —— `load_controller_configuration` 守卫 `if (g_vgpu_config == NULL)` 会让子进程
   继承父配置而跳过重读（§4.3.2 关键修正 1），**必须同时把 `g_vgpu_config` 置 NULL**。`library-remote` 必须保留
   该 handler（含 vmem ledger 清理、mutex 重建），不能因"裁剪"而删掉。
6. **env 注入时机**：必须在 `client_handler` 内、**首个 RPC dispatch 之前**（`server.cpp:421-429` 之后、`459` 之前）。
   客户端首个 RPC 恒为 `cuInit`，库在 `cuInit` hook 里首次 `load_necessary_data`，顺序成立。
7. **无 session 的客户端**：若容器未被 vgpu-manager 注入（无 `LUPINE_SESSION`）→ 子进程无配置 → fail-closed 拒绝。
   这要求"预载 library-remote 的 lupine-server"是**远程 vGPU 专属部署**；纯 lupine 部署不预载库，行为不变。
8. **`cuCtxGetDevice` 是 alloc 预算判定的前置**：`cuMemAlloc` 不带 device，库需经 `cuCtxGetDevice` 取当前物理设备
   再映射 host_index（现有本地库已如此，远程保留）。
9. **`nvmlProcessInfo_v3` 结构体大小**在 lupine 透传的 ABI 需 spike 核对（S6/S3）。
10. **裁剪的回归**：`library-remote` 必须复用 `library/` 的 `hack/check_cuda_hook_consistency.py` 等一致性检查
    （钩子表/导出面/直通实现三处同步），防止裁剪后钩子面与实际不符。

### 4.3.2 实测验证与两条关键修正（v0.3.3）

**实测现象**：GPU 节点上 `LD_PRELOAD libvgpu-control.so`（2GB 限额）启动 `lupine_driver_server`；**同节点**用
lupine 客户端跑 `mem_occupy_tool 0 3000`：分配 3000MB **成功**（无 OOM），但客户端 nvidia-smi 显示 2GB。

**根因：客户端把 CUDA 调用路由到本地 GPU，服务端根本没收到分配。**

- lupine-client 构建设备表时**先探测本地 GPU**：`lupine_real_cuda_fn("cuDeviceGetCount")`（`routing.cpp:226-244`）
  在同机（有真 libcuda）返回成功 → 本地设备**排在虚拟设备表前面**，远程设备在其后。
- 工具 `cuDeviceGet(0)` → 虚拟序号 0 = **本地 GPU** → `cuCtxCreate_v2(0)`+`cuMemAlloc_v2` 在**客户端进程内**用真实
  驱动执行（客户端只 preload lupine shim，无 vgpu 库，无限制）→ 3000MB 直接成功，**从未到达 lupine-server**。
- 佐证（两条路径自洽）：
  - server 日志的 `hooking cuDeviceGetCount/cuDeviceGet` 来自 lupine-client **构建设备表**的 RPC
    （`routing.cpp:217-277`），与工具自身路由无关——无论本地/远程，设备表枚举总是查 server。
  - nvidia-smi 显示 2GB 来自 **nvml 路径总是走 server**（`nvml_symbol` dlsym → 库 hook 虚拟化），是服务端拦截
    生效的证据。两条路径不一致 → 造成"拦截失效"假象。
- **服务端拦截机制本身没问题，只是该实验没走到它。**

**正确验证方法（测试方法论，重要）**：
1. 客户端放**无 GPU 节点**（真实远程场景）；或
2. 客户端设 **`LUPINE_DISABLE_LOCAL=1`**（`routing.cpp:651`）强制设备表只有远程设备。
这样设备 0 才是远程 → `cuMemAlloc_v2` 作为 RPC 到达 server 子进程 → `_cuMemAlloc` 预算判定 → 3000MB>2GB → OOM。
**所有远程验收测试必须满足其一，否则远程路径不被执行、结果无意义。**

**关键修正 1（fork 安全，v0.4 更正：不是阻塞项，但必须做）**：
- 机制属实：`load_controller_configuration` 的守卫是 **`if (g_vgpu_config == NULL)`**，而
  `loader_child_after_fork` 原先只重置 once-guard、不重置 `g_vgpu_config`。若父进程已加载配置，子进程会继承它并
  跳过重读 → per-session 配置不生效。
- **但 v0.3.3 称其为"阻塞项"是不准确的**（v0.4 复核 lupine 源码）：`server.cpp` 的 `main()` 只做
  socket/bind/listen/fork，全文**没有任何 `dlsym`/`dlopen`/`cu*`/`nvml*` 调用**，`x-lupine-cuda-version` 是编译期
  常量（`h2.cpp:421-426`）。父进程从不触发 `load_necessary_data()`，fork 时 `g_vgpu_config` 恒为 NULL，
  子进程本就会各自加载。**当前不会失效。**
- **仍然必须修**，因为它守的失效模式恶劣且静默：dlsym 拦截器在 `cu` 分支里会调 `load_necessary_data()`
  （`loader.c:2192`），父进程只要因任何原因解析一次 `cu*` 符号就会加载配置；而此时没有
  `VGPU_CONFIG_SESSION_PATH`，会回退 `init_g_vgpu_config_by_env()` —— 那条路径把**所有设备 `activate = 1`**
  并写盘，于是所有子进程继承一份 permissive 配置 → **静默 fail-open**。这依赖的是 lupine 的实现细节，不是我们能
  控制的契约。
- **已实现**：`loader_child_after_fork` 置 `g_vgpu_config = NULL` + `session_paths_reset()`；并且 **§4.3.2 的
  fail-closed 不只校验"配置存在"，而是校验"配置确实来自会话目录"**（`session_remote_mode() && !session_enabled()`
  即拒绝），这样即使上面那行漏掉也不会 fail-open。

**关键修正 2（HOST 模式记账口径）**：实测是 `compatibility_mode=0`（HOST），`accumulate_used_memory` 求全机
进程总和（`cuda_hook.c:2398-2403`）。方案 C 的子进程记账必须用 **SESSION 模式**（按会话进程表过滤，§6.5），
不能依赖 HOST 模式（会把其他租户/节点进程计入）。

### 4.3.3 方案 C-2：借 checkpoint provider 注入会话配置（零 lupine 源码改动）（v0.3.4）

**思路**：lupine 已有 **checkpoint provider 扩展点**（`liblupinecr.so`，§2.2/`docs/lupine_env_reference.md`
`LUPINE_CHECKPOINT_LIBRARY`）：每个连接子进程在**首个 CUDA 调用/RPC 前**会调用外部 provider 的
`start()`/`restore(connection_id)`（`server_checkpoint.cpp`）。我们自研一个 provider，把 **`restore()` 当作
per-session 的 env 注入 + 会话进程注册钩子**——派生 `VGPU_CONFIG_SESSION_PATH` 并 `setenv`、把本子进程 PID
写进会话 `pids.config`，**不改 lupine 一行源码**。

**机制验证（逐环，代码级）**：
| 时机 | 位置 | 说明 |
|---|---|---|
| `provider->start()` | fork 后、`client_handler` 前（`server.cpp:719` → `server_checkpoint.cpp:78-126`） | 此时**尚无 session id**（http2 未初始化）；可做进程级初始化（通常不需要） |
| `provider->restore(connection_id)` | **首个 RPC dispatch 之前**（`server.cpp:421-429` → `server_checkpoint.cpp:211-236`） | `connection_id` = `rpc_http2_session_id`（= `LUPINE_SESSION`），http2 已解析完成。**这是注入点** |
| restore 返回值 | `server_checkpoint.cpp:228-233` | 返回非 0 → `connection_ready=false` → `break` **关闭连接**（fail-closed 现成） |
| 无 session | `server_checkpoint.cpp:220` | `connection_id` 为空 → restore 被跳过 → 由库 fail-closed 兜底 |
| provider 加载 | 每子进程内 `dlopen`（RTLD_LOCAL），只导出 `lupinecr_get_lupine_provider_v1` | 不与库的 `cu*`/`nvml*`/`dlsym` 冲突 |

**注入后的顺序**：fork → `start()` → http2 init（解析 session）→ `restore(id)` [setenv `VGPU_CONFIG_SESSION_PATH` +
注册 pid 进 `pids.config`] → 首个 cu*/nvml RPC → 库首次 `load_necessary_data` → 读会话配置。✓

**与 C-1（lupine-server 补丁）的关系**：
- **C-2 替代 C-1 的注入方式**（provider `restore()` 代替 `client_handler` 里 setenv），**lupine 源码零改动**；
  只要 checkpoint provider ABI 稳定，lupine 升级无需改我们的 provider。
- **LD_PRELOAD 仍必须在 server 启动时进程级设置**（库最先入全局符号表，§4.0.1）。**运行时 setenv LD_PRELOAD
  不可行也不必要**——库已 preload，provider 只负责 setenv `VGPU_CONFIG_SESSION_PATH` + 注册 pid。
- **库的 fork 安全修正仍必须做**（§4.3.2 关键修正 1：`loader_child_after_fork` 把 `g_vgpu_config` 置 NULL），
  否则子进程继承父配置、`VGPU_CONFIG_SESSION_PATH` 不生效。
- `VGPU_REMOTE_MODE=1` 建议 **server 进程级 env**（所有子进程继承）；库在远程模式下无有效会话配置即 fail-closed。

**liblupinecr.so 开发（兼容 ABI 的做法）**：
1. **头**：`checkpoint_provider.h`（`lupine_checkpoint_provider_v1`：`struct_size`/`abi_version`/`start`/`restore`/
   `checkpoint`/`stop`；符号 `lupinecr_get_lupine_provider_v1`，ABI version 1）。已在 vgpu-manager **vendor**
   （`library/include/checkpoint_provider.h`，40 行），构建不依赖 lupine 检出。
2. **四个函数**（完整实现见 `library/src/checkpoint_provider.c`）：
   - `start()` → 0。
   - `restore(connection_id)` → **核心**：消毒 session（`[A-Za-z0-9_.-]` 防路径穿越）→ 校验
     `<base>/<session>/config/vgpu.config` 存在（不存在返回非 0 = fail-closed）→ `setenv(VGPU_CONFIG_SESSION_PATH
     =<base>/<session>)` + `setenv(VGPU_REMOTE_MODE=1)` → **把 `getpid()` 追加进 `<session>/pids.config`**
     （`flock` EX + O_APPEND，格式与库 `get_container_pids_by_filepath` 一致，`util.c:528`）。
   - `checkpoint()` → 0（no-op，我们不保存 GPU 状态；SIGTERM 排空照常）。
   - `stop()` → **子进程退出清理**：从 `<session>/pids.config` 移除本 pid（重写文件，`flock` EX）。
3. **构建**：随 `library-remote` 一起编译进 `libvgpu-control.so`（单制品，§4.3.3.1）；导出符号
   `lupinecr_get_lupine_provider_v1` 已在 exports.ld `global:`。
4. **部署**：GPU 节点 server 启动 env：
   `LD_PRELOAD=/opt/vgpu/lib/libvgpu-control.so`、`VGPU_REMOTE_MODE=1`、
   `LUPINE_CHECKPOINT_LIBRARY=/opt/vgpu/lib/libvgpu-control.so`、可选 `VGPU_CONFIG_SESSION_BASE=<base>`。

**会话目录布局（VGPU_CONFIG_SESSION_PATH=<base>/<session>，库按此 env 派生所有 per-session 路径）**：
```
<base>/<session>/config/vgpu.config   会话配额（resource_data_t，agent 落盘）
<base>/<session>/.vgpu_lock           每会话 GPU lock 目录
<base>/<session>/.vmem_node           每会话 vmem ledger（内存记账）
<base>/<session>/.sm_node             每会话 SM 共享桶
<base>/<session>/pids.config          会话容器 PID 列表（provider 注册/清理，保持有序，SESSION 记账据此二分过滤 NVML）
<base>/watcher/sm_util.config         共享 SM watcher（外部 watcher 程序写入，所有会话共用）
```
> provider 的 `restore()` 会**幂等创建** `<session>` 及其子目录（`config`/`.vgpu_lock`/`.vmem_node`/`.sm_node`，
> 已存在不报错）；`pids.config` 注册时去重并按 PID 升序重写、`stop()` 清理时同样保持有序。

**代码骨架（provider，C；完整见 `library/src/checkpoint_provider.c`）**：
```c
#include "checkpoint_provider.h"
#include "hook.h"

static int valid_session_id(const char *id) { /* 仅 [A-Za-z0-9_.-]，非空，限长 */ }

static int session_register_pid(const char *session_path, pid_t pid) {
  /* open(pids_path, O_WRONLY|O_CREAT|O_APPEND), flock(LOCK_EX), write "<pid>\n" */
}
static void session_unregister_pid(const char *session_path, pid_t pid) {
  /* flock(LOCK_EX), 读全部行, 重写文件去掉本 pid */
}

static int checkpoint_restore(const char *connection_id) {
  if (!valid_session_id(connection_id)) return -1;              /* fail-closed */
  char session_path[PATH_MAX];
  snprintf(session_path, sizeof(session_path), "%s/%s", base, connection_id);
  if (access("<session>/config/vgpu.config", R_OK) != 0) return -1;  /* 配额不存在 */
  setenv("VGPU_CONFIG_SESSION_PATH", session_path, 1);
  setenv("VGPU_REMOTE_MODE", "1", 0);
  return session_register_pid(session_path, getpid());
}
static void checkpoint_stop(void) {
  const char *p = getenv("VGPU_CONFIG_SESSION_PATH");
  if (p && *p) session_unregister_pid(p, getpid());             /* 退出清理 */
}
static int checkpoint_start(void) { return 0; }
static int checkpoint_checkpoint(const char *id) { (void)id; return 0; }

static const lupine_checkpoint_provider_v1 provider = {
    sizeof(provider), LUPINE_CHECKPOINT_PROVIDER_ABI_VERSION,
    checkpoint_start, checkpoint_restore, checkpoint_checkpoint, checkpoint_stop};
const lupine_checkpoint_provider_v1 *lupinecr_get_lupine_provider_v1(void) {
  return &provider;
}
```

**风险/边界（C-2 新增）**：
1. **依赖 provider ABI 的调用时机契约**（restore 在首个 RPC 前）。lupine 升级若改变契约 → 注入失效 → 库
   fail-closed 兜底（远程模式无配置即拒绝），**安全不破、功能停摆**，升级需回归测试。
2. **provider 缺失时 lupine 静默禁用 checkpoint 但继续服务**（`server_checkpoint.cpp:93-97`）→ 若无库 fail-closed
   会无限制放行。因此必须：(a) server 部署保证 provider 在位；(b) 库在 `VGPU_REMOTE_MODE` 下无有效会话配置即拒绝。
3. **无 session 的未托管客户端**：restore 被跳过 → 靠库 fail-closed（同 C-1 边界 #7）。
4. **多 server/多容器判别**、session 令牌签发与消毒、配置区生命周期：同 §6.2.1/§6.4，不因 C-2 改变。
5. restore 返回非 0 关闭连接：客户端表现为连接断开/首个 RPC 失败（CUDA_ERROR_DEVICE_UNAVAILABLE），符合 fail-closed 预期。
6. **C-2 与真实 checkpoint 共存**：若未来要真 checkpoint，本 provider 可再加一层转发链到真实 provider（当前不必要）。

#### 4.3.3.1 单制品合并：provider 内置于 `libvgpu-control.so`（可行性评估，v0.3.4）

**结论：可行，且推荐** —— 单一制品（一个 `.so` 同时做 hook 库和注入 provider），构建/部署/版本同步都更简单。

**机制（同一 `.so` 双重角色）**：
- `libvgpu-control.so` 经 **LD_PRELOAD** 进程级加载（最先入全局符号表，§4.0.1）；又被 lupine 每子进程
  `load_provider()` **dlopen**（`server_checkpoint.cpp:83-92`）。glibc 对已加载文件按 **realpath/SONAME 去重**，
  `dlopen` 返回同一句柄、**不二次初始化**、不重复跑构造器。
- 库当前**未设 SONAME**（`library/CMakeLists.txt` 无 VERSION/SOVERSION，产出 `libvgpu-control.so`）→ 按 realpath
  去重成立；`library-remote` 若将来加 SONAME，确保 dlopen 路径与 SONAME 一致。

**必须做的两处配套**：
1. **导出符号**：`deploy/libvgpu-control.exports.ld` 的 `global:` 加 **`lupinecr_get_lupine_provider_v1;`**
   —— 否则 `local: *` 把它藏出 .dynsym，lupine 的 `dlsym(handle, ...)` 返回 NULL、provider 被静默忽略。
   同时更新 `hack/check_exported_symbols.sh` 正/负断言（见 `libvgpu-control.exports.ld` 注释约定）。
2. **vendor 头 + 源文件**：`include/checkpoint_provider.h`（40 行，`lupine_checkpoint_provider_v1` + 符号名 +
   ABI version 1）+ `src/checkpoint_provider.c`（§4.3.3 骨架），进 CMake 源列表。

**兼容性逐点核对**：
- provider 符号 `lupinecr_get_lupine_provider_v1` 不匹配 `cu[A-Z]*`/`nvml[A-Z]*`/`cudbg*` → 库的 `dlsym` 拦截器
  （`loader.c:2167-2210`）**不吞它**，回退 glibc 真 dlsym 返回（§4.0.3）。✓
- `start()/restore()/checkpoint()/stop()` 只在子进程主线程跑 `setenv`/`access`/`flock`/`pids.config` 读写，
  **不触 CUDA、不碰 hook 路径**。✓
- 库的 no-constructor 规则（`hack/check_no_constructors.sh`）：provider 只有静态 struct + 导出函数，无构造器。✓
- 时序：fork → atfork（`loader_child_after_fork` 重置 once-guard/`g_vgpu_config`）→ `load_provider()`（dlopen 去重 +
  `start()`）→ http2 init → `restore(session)` setenv `VGPU_CONFIG_SESSION_PATH` + 注册 pid 进 `pids.config` →
  首个 RPC → 库 `load_necessary_data` 按 `VGPU_CONFIG_SESSION_PATH` 读配置、按 `pids.config` 做 SESSION 记账。✓
- 裁剪后的 `library-remote` 导出面 = 内存 hook 族 + `cuCtxGetDevice` + nvml 内存/进程表/设备可见性 hook +
  `dlsym` + **`lupinecr_get_lupine_provider_v1`**。

**部署（GPU 节点 server 启动 env）**：
```
LD_PRELOAD=/opt/vgpu/lib/libvgpu-control.so \
VGPU_REMOTE_MODE=1 \
LUPINE_CHECKPOINT_LIBRARY=/opt/vgpu/lib/libvgpu-control.so \
# 可选：会话目录基址（默认 /etc/vgpu-manager/remote-sessions）
VGPU_CONFIG_SESSION_BASE=/etc/vgpu-manager/remote-sessions \
./lupine_driver_server
```
> `VGPU_CONFIG_SESSION_PATH` 不是 server 启动 env——它由 provider 的 `restore()` 按本连接 session 派生并 `setenv`
> （每子进程一份），库据此定位 `<session>/config/vgpu.config`、`.vgpu_lock`、`.vmem_node`、`pids.config`
> （§4.3.3 会话目录布局）。

**边界/提示**：
- `LUPINE_CHECKPOINT_LIBRARY` **必须显式设置**（制品不叫 `liblupinecr.so`，lupine 默认路径找不到）。
- 单一 .so 使 hook 库与注入 provider **版本严格同步**；若未来需独立提供 checkpoint（其他场景），再拆分出单独
  `.so` 即可，不影响本设计。

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
  │         ├─ child(进程B) ─┼─ 同一容器(S) 的多个进程 → 共享同一会话目录
  │         └─ child(进程C) ─┘
  └─ 会话目录 <base>/<session>/（VGPU_CONFIG_SESSION_PATH，provider restore() 幂等创建）
       ├─ config/vgpu.config   resource_data_t（device_t[16]，seqlock）← 本容器每设备限额（§6.3）
       ├─ .vgpu_lock           每会话 GPU lock 目录
       ├─ .vmem_node           每会话 vmem ledger（内存记账）
       ├─ .sm_node             每会话 SM 共享桶
       └─ pids.config          会话进程表：容器子进程 PID 列表（provider 注册/清理，有序，§4.3.3/§6.5）
  └─ <base>/watcher/sm_util.config   共享 SM watcher（外部程序写入，所有会话共用）
```

- **会话 id = `LUPINE_SESSION`**：客户端握手头 `x-lupine-session`（`h2.cpp:710-724`），子进程在 `client_handler`
  内 `rpc_http2_session_id()` 读取（`server.cpp:421-429`）。provider 的 `restore()` 据其派生会话目录并
  `setenv(VGPU_CONFIG_SESSION_PATH=<base>/<session>)`（§4.3.3）。
- **多 server**：session id 全局唯一（如 pod UID）；每 GPU 节点各持该容器**在本节点**的设备切片（本节点会话目录
  只含本节点设备）。

### 6.2.1 单 lupine-server 多容器并发：按连接区分容器、按 session 读配置（评审补充）

一个 `lupine_driver_server` 会同时服务**多个不同容器**，判别与取配置的关键：

1. **判别单位 = 连接（fork 的子进程）**：每个连接携带各自独立的 `x-lupine-session` 头
   （`h2.cpp:525` 解析进各自 transport 的 `session_id`）。子进程经 `rpc_http2_session_id(&conn)`
   （`h2.cpp:850-857`）读到的**只属于自己这条连接**的 session id —— 这是多容器并发的判别基础。
   **不需要也不应该有任何全局 session→child 查表**：判别完全由"每个子进程自己的连接头"决定。
2. **会话路径 = 由本连接 session id 推导**：provider 的 `restore()` 消毒后
   `setenv(VGPU_CONFIG_SESSION_PATH=<base>/<sanitized-session>)` 并把本子进程 PID 写进 `<session>/pids.config`
   （§4.3.3），再放行首个 CUDA RPC。同一容器所有进程的 `LUPINE_SESSION` 相同 → 子进程读同一会话目录（一致性）；
   不同容器 session 不同 → 读不同会话目录（隔离）。**无全局配置、无跨容器串读**。
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
  角色或新 agent）。scheduler 分配时经控制面下发 "session S → devices[UUIDs+limits]"；agent 创建会话目录并落盘
  `<base>/<session>/config/vgpu.config`（复用 `WriteVGPUConfigFile`/`ModifyDevice`，`pkg/config/vgpu/vgpu_config.go:477-627`）。
  子进程只读，**不信任客户端自报**（安全边界）。
- **(b) 客户端透传（仅可信集群试点）**：容器内 device-plugin 已注入 `CUDA_MEM_LIMIT_*`/`MANAGER_VISIBLE_DEVICES`
  等 env；lupine-client 经握手头/RPC 转发给 server，子进程写入 `config/vgpu.config`。
  **缺点：容器可自行篡改 env → 隔离可被绕过**；仅用于快速验证闭环。

### 6.4 应用 / 同步 / 更新机制

| 时机 | 动作 |
|---|---|
| 分配 | scheduler 下发 → agent 创建 `<base>/<session>/` 目录并落盘 `config/vgpu.config`（含空 `pids.config`） |
| 子进程启动 | provider `restore(session)`：校验配额存在 → `setenv(VGPU_CONFIG_SESSION_PATH=<base>/<session>)` → **把 `getpid()` 追加进 `<session>/pids.config`**（§4.3.3）；库首个 CUDA 调用按该 env 读配置 |
| 记账/限速 | 每个子进程算 `used = Σ NVML usedGpuMemory(pid ∈ pids.config, device)`（真实口径含隐式）；预算判定 `used + req > limit → OOM`；所有子进程读同一 `pids.config` → **容器级一致**（§6.5） |
| 更新 | agent 在原 `config/vgpu.config` 内 seqlock 改写（改 device_t 限额）→ 子进程每次 `get_device_snapshot()` 重读 → **热更新**，无需重启子进程 |
| 销毁 | pod 结束 → agent 删除会话目录；连接关闭 → 子进程退出 → provider `stop()` **从 `pids.config` 移除本 pid** |

- **热更新**依赖现有 seqlock 机制：库每次 `get_device_snapshot()`（`loader.c:1565-1590`）读快照，Go 侧
  `ModifyDevice`（`vgpu_config.go:533-559`）在原文件上改写。**只需把文件路径换成 per-session**：库按
  `VGPU_CONFIG_SESSION_PATH` 派生 `<session>/config/vgpu.config`（覆盖 `CONTROLLER_CONFIG_FILE_PATH`），
  `<session>/.vgpu_lock`、`<session>/.vmem_node` 同理；`loader.c:2856` 目前文件优先，需配合 §4.3.2 关键修正 1。
- **一次性初始化项**（compatibility_mode、设备映射、oversold 开关）只读一次；**限额类**（memory_limit 等）逐次读。
  静态配额场景足够；动态配额变化需子进程重新初始化会话区（Phase 2 再议）。

### 6.5 容器级记账（SESSION 兼容模式）与每设备配置匹配

- **库新增 `SESSION` 兼容模式**：`accumulate_used_memory()`（`cuda_hook.c:2342-2408`）与利用率过滤的 PID 归属
  判断改为 "pid ∈ `<session>/pids.config`"（复用现有 `CLIENT_COMPATIBILITY_MODE` 的
  `check_device_pid_in_ordered_container_pids` + `get_container_pids_by_filepath` 机制，只是文件路径换成
  `<session>/pids.config`）。子进程 = 容器的一个进程，`pids.config` = 容器全部子进程 → `used` 是**容器级真实口径**
  （含隐式内存，NVML 驱动值）。
- **会话进程表 = `pids.config`**：provider `restore()` 注册（追加 `getpid()`）、`stop()` 清理（§4.3.3）；
  死亡子进程因 NVML 不再上报其 PID 而自然失效，文件中的陈旧行无影响（`flock` SH/EX 保护读写，`util.c:528`）。
- **多进程竞态**：多子进程并发分配存在 NVML 滞后导致的 TOCTOU 窗口（与本地多进程容器一致），可接受。
- **每设备配置匹配**：`get_host_device_index_by_cuda_device`（`loader.c:2421-2461`）走 `cuDeviceGetUuid_v2` →
  UUID 匹配 `devices[].uuid`。**前提是子进程看到的设备与容器分配一致** → 必须配合设备级 allowlist + 序号重映射（§6.6）。
- **`nvmlDeviceGetComputeRunningProcesses` 等 hook**：返回 `pids.config` 内条目对应的进程（真实数据），PID 可改写
  （显示优化，可选）。
- **共享区 per-session 作用域**：`<session>/.vmem_node` 替代 `/tmp/.vmem_node`、`<session>/.vgpu_lock` 替代全局
  lock，`sm_util.config` 走 `<base>/watcher/`（共享）——避免跨租户串扰（Phase 1 禁超卖，ledger 仅预留）。

### 6.6 设备级访问控制 + 序号重映射（"每个设备配置匹配"的必要前提）（v0.4 重写，已实现）

客户端经 lupine 默认可见 server **全部设备**；若不限制，客户端可用未分配 GPU，且序号错位导致 UUID 匹配失败。
**CUDA 与 NVML 两条路径的机制不同，必须分别处理。**

#### 6.6.1 CUDA 路径：`CUDA_VISIBLE_DEVICES`（不 hook `cuDeviceGetCount`/`cuDeviceGet`）

provider 的 `restore()` 在 setenv 会话路径后，自行 mmap 会话配额，按 `devices[].uuid` 生成
`setenv("CUDA_VISIBLE_DEVICES", "<uuid1>,<uuid2>,...")`，由**驱动自己**完成裁剪与重映射。

成立条件（均已核实）：
- 驱动在 `cuInit` 时读取该 env，而 `restore()` 在首个 RPC（恒为 `cuInit`）之前调用；
- lupine-server 父进程不碰 CUDA，故 fork 时驱动未初始化。**这不是碰巧的实现细节，而是 lupine 的设计约束**
  （v1.5.1 更正归因）：父进程职责就是监听端口 + accept + fork，全部 CUDA 状态在子进程；而且 CUDA 上下文
  **本身不可跨 fork 继承**——父进程若先初始化 CUDA，子进程再用会直接报错，所以任何 per-connection-fork 的
  CUDA 转发器都必须让父进程远离 CUDA。源码事实（`server.cpp` 无 `cuInit`/`dlsym`/`cu*`；`x-lupine-cuda-version`
  为编译期常量 `h2.cpp:421-426`）是这一约束的体现，不是我们赖以成立的偶然。退一步，库的 atfork 处理
  （`loader_child_after_fork` 重置 once-guard/会话路径 + `config_source_moved()` 按来源重读）本身也防御性地
  覆盖了"父进程曾加载配置"的情形，不引入风险；
- provider 读配额走 `mmap_file_to_config_path`，**不触发 `load_necessary_data`**，注入前不加载任何 CUDA 库。

**优于自研 hook 的地方**：客户端设备表本就由 `cuDeviceGetCount`/`cuDeviceGet` 两个 RPC 枚举
（`routing.cpp:217-277`），驱动裁剪后客户端天然只看到允许设备且已重排为 0..n-1，链路上没有我们的 hook。
用 UUID 而非索引，因为配置以 UUID 命名设备，且 PCI 序在重启后不稳定。

**已知取舍（本阶段接受）**：CUDA 对 `CUDA_VISIBLE_DEVICES` 中的**无效条目是静默截断**——从该条目起后续设备全部
不可见，且不报错。因此**假定 agent 写入的 UUID 均合法有效**；配置陈旧时的表现是"容器看到的卡变少"而非报错，
排查设备数量异常时应先查这里。不做存在性校验是刻意的：校验意味着要在此处初始化 NVML，而这里是**唯一不能碰驱动**
的位置。

#### 6.6.2 NVML 路径：必须 hook（`CUDA_VISIBLE_DEVICES` 对 NVML 无效）

NVML **不受 `CUDA_VISIBLE_DEVICES` 约束**，永远枚举全部物理设备。不处理的话，远程容器内 `nvidia-smi` 会列出
GPU 节点上所有卡，并能读到其他租户的显存占用。lupine-server 只转发 5 个枚举类 NVML API
（`codegen/gen_nvml_server.inc`），对应 hook 如下（`library-remote/src/nvml_hook.c`，仅在会话模式生效）：

| hook 符号 | 语义 |
|---|---|
| `nvmlDeviceGetCount` / `_v2` | 返回 allowlist 数量 |
| `nvmlDeviceGetHandleByIndex` / `_v2` | 虚拟序号 → 允许设备的物理句柄 |
| `nvmlDeviceGetHandleByUUID` | 调真函数后校验结果在 allowlist 内，否则 `NVML_ERROR_NOT_FOUND` |
| `nvmlDeviceGetHandleByPciBusId` / `_v2` | 同上 |
| `nvmlDeviceGetHandleBySerial` | 同上（lupine 当前不转发，为防止 allowlist 依赖 lupine 的 RPC 暴露面而一并加固） |
| `nvmlDeviceGetIndex` | 返回**虚拟序号**，与 GetCount/GetHandleByIndex 自洽 |

要点：
- 库自身的枚举走 `NVML_INTERNAL_CALL`/`nvml_library_entry`（真驱动函数），**不会递归进自己的 hook**
  （`load_nvml_libraries` 用 `real_dlsym` 填表，`loader.c:1137`）。
- `nvmlDeviceGetIndex` 在 lupine 客户端是本地伪造的（`nvml_client.cpp:852-870`，不发 RPC），服务端 handler 对
  客户端等同死代码；仍然 hook，是为了两侧语义一致——调用方拿虚拟序号做判断时不会因为服务端返回物理序号而错乱。
- **不变量**：CUDA 虚拟序号 i 与 NVML 虚拟序号 i 必须是同一张卡。两侧共用唯一的排序函数
  `config_allowed_devices()`（`src/config_io.c`），按 `activate` 非零的 host_index 升序压缩（`activate` 允许稀疏）。

#### 6.6.3 方案 B 侧

adapter 必须按配额裁剪设备可见性（复用 `MANAGER_VISIBLE_DEVICES` 设备索引重映射），否则同一漏洞。

### 6.7 与方案 B 的对照

方案 B（客户端适配层）**没有"多子进程共享配置"问题**：虚拟化全在容器内，配置直接读容器内 `vgpu.config`/env
（现有机制）；多进程容器的容器级记账靠 pod 作用域 `remote_pid_map`（§5）聚合。
**B 的 `remote_pid_map` 与 C 的会话进程表是同一问题的两侧解法**：B 在客户端按容器聚合远端 PID，C 在 GPU 节点
按会话聚合子进程 PID。

### 6.8 多 server 设备聚合：lupine 机制与本方案的兼容性（v0.5，代码级核实）

`LUPINE_SERVER=server1,server2` 时客户端如何合并两台 server 的设备，以及"每节点独立 vgpu.config 屏蔽/重映射"
能否兼容。结论：**兼容**。机制与边界如下。

**lupine 聚合机制（客户端）**：
- CUDA 设备表（`routing.cpp:217-277` `lupine_ensure_device_table`）：先探测**本地** GPU 逐个入表，再按
  `LUPINE_SERVER` **从左到右**逐台调 `cuDeviceGetCount`+`cuDeviceGet(ordinal)`，`{conn_index, remote_device}`
  依次追加。虚拟序号 = 表下标（`cuDeviceGet(i)` 直接返回 i，`routing.cpp:305`）。每次 CUDA 调用经
  `lupine_route_for_device`（`routing.cpp:392-410`）把虚拟序号改写回该 server 的**本地序号**后发送——两台 server
  各自的"设备 0"由 `conn_index` 区分，不冲突。
- NVML 设备表（`nvml_client.cpp` `ensure_devices`）：同按连接顺序拼接，**不含本地设备**。NVML 与 CUDA 是**两套
  独立的连接数组**，但都用 `strsep(",")` 从左到右解析同一个 env（`client.cpp:8227` / `nvml_client.cpp:119`），
  正常情况下下标 i 指同一台 server。

**与本方案的兼容原理**：我们的裁剪/重映射完全发生在**每台 server 自己的编号空间内**（`CUDA_VISIBLE_DEVICES`
+ NVML hook 各自产出致密的 0..n-1），这正是 lupine 聚合层对每台 server 的全部要求——两层正交。
例：server1 配置槽位 2,3、server2 配置槽位 3,4，各分 2 卡 → 客户端看到 4 卡：

```
虚拟序号     0        1        2        3
           (s1,0)   (s1,1)   (s2,0)   (s2,1)
物理卡    s1:gpu2  s1:gpu3  s2:gpu3' s2:gpu4'
```

CUDA 与 NVML 聚合顺序一致，"cuda:i 就是 nvml:i" 不变量在聚合后仍成立。
注意：决定客户端可见顺序的是 **config 槽位的升序**（`config_allowed_devices` 压缩稀疏 activate），不是物理索引；
槽位=物理索引只是为了可读性，非必需。

**边界（必须知道）**：
1. **`LUPINE_DISABLE_LOCAL=1` 是硬性要求**（第二个理由，比测试方法论更硬）：CUDA 表含本地设备、NVML 表不含。
   客户端只要有一张本地卡，cuda:0 是本地卡而 nvml:0 是 server1 的卡——**两个 API 指向不同的卡**，且这发生在
   lupine 聚合层，服务端库无法修正。远程 pod 注入时必须设置。
2. **连接失败会静默跳过导致下标错位**：CUDA/NVML 两套连接独立建立，解析时坏 token/连不上的 server 被
   `continue`。若一侧成功另一侧失败，两表下标错开一位。排查"设备对不上"先查两侧连接数是否一致。
3. **同一 `LUPINE_SESSION` 发给所有 server**（`h2.cpp:711-720`）：每台 GPU 节点须有**同名**会话目录、各自只配
   本节点设备。`pids.config`/共享令牌桶/配额天然按节点独立，语义正确。
4. **fail-closed 客户端表现为全有全无**：任一 server 拒连（如缺会话配额），`lupine_ensure_device_table` 走
   `devices.clear(); return`——客户端看到 **0 张卡**，而非"跳过该 server 剩下的卡"。安全上正确；排查时勿误判为
   全部节点故障。
5. **服务端 `nvmlDeviceGetIndex` hook 的返回值到不了客户端**：lupine 客户端的 `nvmlDeviceGetIndex` 不发 RPC，
   用自己的表算全局序号（`nvml_client.cpp:852-870`）。服务端 hook 保留（语义自洽 + 防实现变化），但客户端编号
   由聚合层决定。
6. **真机验收项**：双 server 各 2 卡，验证 `cudaSetDevice(2)` 与 `nvmlDeviceGetHandleByIndex(2)` 落在同一张
   物理卡（比对 UUID）——整条链路唯一需实测确信的不变量。

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
- [ ] S4（C）PRELOAD + 会话配置可行性：`LD_PRELOAD libvgpu-control.so` + `LUPINE_CHECKPOINT_LIBRARY` 进
      `lupine_driver_server`，验证 provider `restore()` 注入 `VGPU_CONFIG_SESSION_PATH`/注册 pid、子进程内拦截生效、
      `pthread_atfork`/`nvml_symbol` 兼容（§4.0.3）。
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
- [ ] 部署：进程级 `LD_PRELOAD libvgpu-control.so` + `VGPU_REMOTE_MODE=1` +
      `LUPINE_CHECKPOINT_LIBRARY=.../libvgpu-control.so`；provider `restore()` 注入 `VGPU_CONFIG_SESSION_PATH` +
      注册 pid 进 `<session>/pids.config`、`stop()` 清理（§4.3.3，已落地 library-remote）。
- [x] 会话路径模块（`include/session.h` + `src/session.c`）：10 类路径由 `VGPU_CONFIG_SESSION_PATH` 运行时派生，
      env 只读一次；`sm_util.config` 挂 `<base>/watcher/`（跨会话共享）；未设 env 时逐字回退旧路径。
      实现手法是把 `hook.h` 的路径宏重定向为 `session_path()` 调用，68 处调用点零改动。
- [x] `loader_child_after_fork` 置 `g_vgpu_config=NULL` + `session_paths_reset()`（§4.3.2 关键修正 1）。
- [x] `SESSION_COMPATIBILITY_MODE = 300`：记账（`accumulate_used_memory`）与利用率过滤两处按
      `<session>/pids.config` 归属，复用现成的 `check_device_pid_in_ordered_container_pids`。
- [x] fail-closed：远程模式下"无会话目录"或"无可读配额"直接拒绝服务，**不回退 env 构造配置**（那条路径会把所有
      设备 `activate=1`，是静默 fail-open）。
- [x] 设备隔离：provider `setenv CUDA_VISIBLE_DEVICES` + NVML 可见性 hook 族（§6.6）。
- [x] provider `restore()`/`stop()` 更新 `pids.config` 时顺带 `pid_exist()` 剔除陈旧 PID（SIGKILL 下 `stop()` 不执行）。
- [x] 工具与回归：`make session-cli`（`vgpu-session-config` 手写会话配额）、`make test-nogpu`（路径派生/回退/
      配置区往返/设备序表）、`make check*`（复用 `library/hack` 的检查器，加 `--root` 参数双树共用）。
- [ ] 真机验收：GPU 节点起 server，无 GPU 客户端（或 `LUPINE_DISABLE_LOCAL=1`）跑通内存闭环。
- [ ] Go 侧：GPU 节点 agent 维护 session→配额（复用 `device_t`/`WriteVGPUConfigFile` 落盘
      `<base>/<session>/config/vgpu.config`），scheduler 分配时登记。
- [ ] 端到端：同 B 的验收标准，且**验证多进程容器容器级记账**（S5），客户端为**普通 lupine client**（无适配层）。

### Phase 2 —— 核心隔离（仅 C 支持）

> **前提澄清**：令牌桶（`rate_limiter`）、利用率 watcher、`sm_node` 共享桶全部在位；AIMD/auto 控制器曾被
> 裁剪、现已恢复（见 docs/sm_controller_aimd.md 沿革节）。Phase 2 不是"新增限速能力"，而是**让既有能力在
> 会话模型下语义正确**。

- [x] `rate_limiter` + 利用率 watcher 在子进程内启用：`initialization()` 由 `cuInit` hook 触发
      （`cuda_hook.c`），而 `cuInit` 正是 lupine 子进程的首个 RPC，故连接建立即完成映射与 watcher 启动。
      利用率归属已在 Phase 1 的 `SESSION` 模式中按 `pids.config` 过滤。
- [x] **共享令牌桶在会话模式下强制启用**（关键修正，见下）。
- [x] 限额热更新：watcher 每周期 `get_device_snapshot(host_index)` 重读 `hard_core`/`soft_core`，
      seqlock 保证不撕裂 → **核心限额缩容/调整无需重启子进程**。（`g_total_cuda_cores` 来自设备物理几何，
      与配额无关，不需要热更新。）
- [ ] 真机验证多租户 SM 抢占下各自限速正确。
- [ ] 显示层 PID 改写（把子进程 PID 映射回本地 PID，纯展示优化，可选）。

#### 关键修正：共享令牌桶不是可选项

`CUDA_SM_SHARED_BUCKET` 在本地库是**默认关闭的 opt-in**。远程模式下必须**强制开启**，否则有两个问题：

1. **容器级配额被突破**：一个远程容器 = 每条客户端连接一个 lupine-server 子进程。每个子进程有自己的
   `g_dev_hot[].cur_cuda_cores`，各自按完整 `hard_core` 限速 → 容器内 2 个 CUDA 进程拿到 **2×** 配额。
   这与内存侧"每子进程独立记账会超配额"（§6.1）是同一类缺陷。
2. **NVML 轮询放大**：每个子进程一个 watcher 线程各自高频轮询 NVML → N 个子进程 N 倍开销。

共享桶同时解决两者：桶落在 `<session>/.sm_node`（Phase 1 已做 per-session 作用域），
CAS 补给选举保证全会话每周期只有一次补给；**采样所有权**让 leader 之外的进程完全跳过 NVML、直接读其发布的样本
（`sm_publish_sample`），于是 N 个子进程只有 1 个轮询者。

实现：`sm_controller_init()` 在 `session_enabled()` 时把默认值置 1，且**显式 `CUDA_SM_SHARED_BUCKET=0` 会被拒绝
并告警**（放行它等于放弃配额）。映射失败时：本地模式照旧降级为进程内桶；**会话模式下若该会话确实配置了核心限额，
则 fail-closed 拒绝服务**（`session_has_core_limit()` 门控——没配核心限额时无配额可超，不必拒绝）。

### 8.2 Python/PyTorch 工作负载在 k8s 中的集成方式（基于 §2.4 分析）

lupine 的 shim 是**动态链接层**生效的，Python 的 `connect()` 只是"设 env + 提前加载 shim"的便利封装。因此在 k8s 里：

**推荐：注入层完成一切，应用零改动（不 import lupine）。**
- device-plugin / DRA Allocate 对远程 pod 注入（与现有本地 vGPU 注入并列，`pkg/deviceplugin/vgpu/vnum_plugin.go:757-803`）：
  - `LUPINE_SERVER=<gpu-node>:14833`（多 server 用逗号分隔）
  - `LUPINE_SESSION=<控制面签发的会话令牌>`（§6.2.1）
  - `LD_LIBRARY_PATH=/opt/lupine/lib` + 适配层/远程模式的 `LD_PRELOAD`（按方案 B/C）
  - `VGPU_REMOTE_MODE=1` + 现有 `CUDA_MEM_LIMIT_*`/`MANAGER_VISIBLE_DEVICES` 等
- pod 镜像：CUDA 版 PyTorch（`pip install torch --index-url ...cu12x`）+ lupine-client shim（`/opt/lupine/lib`）。
- 应用代码：直接用 `torch.device("cuda")`/`cudaMalloc`，**无需 import lupine**，shim 在驱动层劫持。

**何时才需要 `lupine` python 包：**
- 想在**代码里显式声明多 server / 控制连接时序**（`with lupine.connect(host=[...])`）时，可选择性安装，纯便利。
- 注意：`lupine.connect()` 要求**在任何 PyTorch CUDA 操作之前**调用，且 `LUPINE_SERVER` 已在 env 时以 env 为准；
  注入层已设置时，应用直接 `with lupine.connect()`（host 缺省读 `LUPINE_SERVER`）即可。

**明确不适用：** `sidecar` 机制是给 **macOS / CPU-only PyTorch 宿主**设计的容器化 worker，k8s Linux pod 内
**不需要、也不应该**用（pod 镜像自装 CUDA 版 PyTorch 即可；再套一层 worker 容器徒增开销与复杂度）。

**与安全/版本的一致性（必须由注入层保证）：**
- session 令牌由控制面签发（防冒用），不是 pod UID。
- client shim 版本 = 单基准制品（§7.3），server 驱动版本 ≥ pod cudart 版本。

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
| **多进程容器记账一致性（v0.3 新增）** | C 路径每子进程独立限速 → 容器级超配额 | 会话目录 + `pids.config` 聚合 `used`（SESSION 模式），S5 spike 验证（§6.1/§6.5） |
| 配置"文件优先 vs env" | C 路径 per-child env 被全局 config 覆盖 | `VGPU_CONFIG_SESSION_PATH` 会话目录（`<session>/config/vgpu.config`）；远程节点无全局 config（§6.4） |
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
