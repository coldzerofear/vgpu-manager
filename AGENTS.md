# AGENTS.md —— vgpu-manager 项目指南（供 AI 与工程师使用）

> 本文件让一位**没有本仓库上下文的新 AI / 新工程师**能够快速上手：理解项目、当前任务、设计决策、
> 构建/测试方法，以及如何**复查/实施**远程 GPU 虚拟化相关工作。
> 深度设计见 `docs/remote_gpu_pool_research_design.md`。

---

## 1. 项目是什么

`vgpu-manager`：Kubernetes GPU 虚拟化与调度项目。

- **调度/分配**：Device Plugin 与 DRA 两条路径，把 GPU 切成 vGPU（内存限额 + 核心限额）分配给 Pod。
- **硬隔离**：核心是 `library/` 构建的 **`libvgpu-control.so`**（C 语言），通过 `LD_PRELOAD`（或 `/etc/ld.so.preload`）
  劫持 CUDA Driver API（`cu*`）与少量 NVML（`nvml*`），实现：
  - 内存硬隔离：`cuMemAlloc` 前预算校验，超限返回 `CUDA_ERROR_OUT_OF_MEMORY`；
    `cuMemGetInfo`/`cuDeviceTotalMem`/`nvmlDeviceGetMemoryInfo` 改写为"限额视图"。
  - 核心硬隔离：`cuLaunchKernel` 前令牌桶限速（`gridDim*blockDim` 扣减），利用率 watcher 线程本地轮询 NVML
    驱动令牌补给（delta/AIMD/auto 控制器）。
  - 可选内存超卖（UVA/managed memory + vmem ledger）。
- Go 侧：调度器、插件、webhook、monitor、metrics、配置下发（`vgpu.config`，seqlock 版本化）。

## 2. 当前任务（重要）

**跨节点远程 GPU 资源池**：Pod 在无 GPU 节点通过 TCP 使用远端 GPU（底层用开源 **lupine**，位于
`D:\WorkSpace\GoCode\src\lupine`，C++）。lupine 只做远程转发（API 劫持 + HTTP/2 TCP），**不做虚拟化**。

我们要在其上叠加本项目的隔离能力。已完成可行性分析，结论摘要：

1. **lupine server 全部透传**：`nvmlDeviceGetComputeRunningProcesses`（`nvml_server.cpp:58-97`）等返回的是
   GPU 宿主机真实进程表，**无按 client 过滤/记账/虚拟化**。
2. **远端 PID 生命周期 = 1:1 重叠**：一个本地进程 = 一条连接 = server 一个 fork 子进程（`server.cpp:668-727`）。
   不是 1:N。断线重连会换远端 PID。
3. **记账缺陷**：纯 `cuMemAlloc` 记账丢上下文等隐式内存（记录值 < 实际占用）。本地库用 NVML 真实
   `usedGpuMemory` 口径（含隐式）。任何远程方案不应退化为纯记账。
4. **核心隔离的关键约束**：利用率 watcher 需**本地高频轮询 NVML**；远程下走网络轮询不可接受。因此
   "客户端适配层 + 远端 NVML" 无法做核心隔离。
5. **dlopen 后置无法拦截已解析引用**：lupine-server 生成 handler 直接引用 `cu*`（`gen_server.cpp:1760`），
   PLT 绑定粘住后 dlopen 的库无法替换。**方案 C 必须进程级 LD_PRELOAD**，不能靠 dlopen 加载 hook（设计 §4.0.1）。
6. **dlsym 双导出分场景**：客户端（B）adapter 与 lupine-client 都导出 `dlsym`——adapter 保留导出、回退走 glibc
   真 dlsym（`loader.c:1073-1110`），无递归，需 spike 验证顺序；服务端（C）lupine_driver_server 不导出 dlsym，
   **无冲突**（设计 §4.0.3）。
7. **lupine 按 CUDA 版本打多制品**：兼容方向是"客户端 ≤ 服务端"；推荐**单一基准制品 + 跨版本 wire 兼容 spike**，
   服务端制品绑定 GPU 节点驱动版本（设计 §7）。

**三种方案（详见设计文档第 4 节）：**
| 方案 | 一句话 | 内存 | 核心 | 推荐 |
|---|---|---|---|---|
| A 纯记账适配层 | 客户端 ledger 全量记账 | 丢隐式内存 | 不可行 | 不采用 |
| B 小改 lupine 暴露远端PID + 适配层 | 按远端 PID 过滤真实 NVML | ✅ 真实口径 | 不可行 | **Phase1 快速验证** |
| C lupine-server 子进程内加载本库 | 现有库跑在 server 子进程里 | ✅ 真实口径 | ✅ 节点本地轮询 | **最终方案** |

**已定约束**：适配层复制为独立子工程 `library-remote/`；第一阶段单节点多 GPU；第一阶段禁用内存超卖。
**待定项**：PID 映射结构 v0.2（按设备组织，见设计 §5）；lupine 制品分发策略（设计 §7）。

**推荐路线**：先 B 快速验证内存闭环，随即迁移到 C（内存 Phase1 + 核心 Phase2）。

## 2.1 关键结论（v0.2 增补）

- **PID 映射结构 v0.2（草案）**：按设备组织 `remote_pid_map{ devices[16]{ uuid, entries[1024]{local_pid,remote_pid,conn_index,gen}, entry_count, seq }, seq }`。
  一个本地进程在多 server 下有多个远端 PID → 按设备 + `conn_index` 区分；过滤 NVML 进程表时按设备取 `remote_pid` 集合。
  方案 C 不需要该表（用服务端会话进程表）。
- **方案 C 配置下发（v0.3.4 修正）**：一个容器多个使用 CUDA 的进程 = 多个 server 子进程，**不能每子进程独立
  setenv 注入**（会导致容器级超配额）。必须用 **GPU 节点上的会话目录** `<base>/<session>/`（provider `restore()`
  幂等创建 `config/`/`.vgpu_lock`/`.vmem_node`/`.sm_node`，agent 权威落盘 `config/vgpu.config` 的
  `resource_data_t`，seqlock；含 `pids.config`），所有子进程共享、容器级记账（`SESSION` 兼容模式按 `pids.config`
  过滤 NVML，列表保持有序可用二分）。provider `restore()` 设 `VGPU_CONFIG_SESSION_PATH`、`stop()` 清理 pid；
  更新经 seqlock 热更新。详见设计 §6/§4.3.3。
- **单 lupine-server 多容器并发（v0.3.1）**：判别单位是**连接**——每个子进程经 `rpc_http2_session_id()` 读自己
  连接的 `x-lupine-session` 头（`h2.cpp:850-857`）由 provider 推导 `VGPU_CONFIG_SESSION_PATH`，无需全局查表；
  session id 客户端可控 → 须消毒（防路径穿越）+ 控制面签发令牌（防冒用）+ fail-closed（`config/vgpu.config` 不存在
  即拒绝 CUDA）（设计 §6.2.1）。
- **设备访问控制必须做**：客户端经 lupine 可见 server 全部设备，须服务端 allowlist + 序号重映射（C）或 adapter 裁剪（B），
  否则可绕过配额用未分配 GPU（设计 §6.6）。

## 2.2 C 方案落地形态（v0.3.2 确认）

- **改造集中在 lupine-server**（fork 后、处理请求前识别 session 并注入 env），`library-remote` 为裁剪适配器专责
  远程虚拟化，`library` 继续专责本地。
- **可行性逐环确认**：
  - 客户端会话传递（`LUPINE_SESSION` env）+ server 判别连接（`rpc_http2_session_id`，`h2.cpp:850-857`）现成。
  - 设备隔离靠库 hook `cuDeviceGetCount/Get`（lupine-server handler 直调，`gen_server.cpp:102/79`；客户端设备表
    经这两个 RPC 枚举，`routing.cpp:217-277`）。
  - nvml 路径经 `nvml_symbol<>()` dlsym 句柄查询（`nvml_server.cpp:46-56`）→ **库必须保留 dlsym 拦截器**。
  - per-session 配置 fork 安全依赖 `loader_child_after_fork`（`loader.c:2994`）重置 once-guard，**不能裁剪**。
- **`library-remote` 裁剪**（更正）：实际只裁掉了 AIMD/auto 控制器（delta 保留）。`cuLaunchKernel*`、利用率
  watcher、`sm_node` 共享桶**都还在**——Phase 2 的工作是让它们在会话模型下语义正确，不是重新加回来。
- **共享令牌桶在会话模式下强制开启**：一个远程容器 = 每条连接一个子进程，进程内桶会让每个子进程各自按完整
  `hard_core` 限速 → 容器拿到 N× 配额；且 N 个 watcher 各自轮询 NVML。共享桶（`<session>/.sm_node`）的 CAS 补给
  选举 + 采样所有权同时解决两者（standby 完全跳过 NVML）。显式 `CUDA_SM_SHARED_BUCKET=0` 在会话模式下被拒绝；
  映射失败且该会话配了核心限额则 fail-closed。详见设计 Phase 2。
- **设备隔离不 hook `cuDeviceGetCount/Get`**（v0.4 定案）：CUDA 侧由 provider `setenv CUDA_VISIBLE_DEVICES=<会话
  设备 UUID 列表>`，驱动自己裁剪并重排为 0..n-1；NVML **不受该 env 约束**，另 hook `nvmlDeviceGetCount(_v2)`/
  `GetHandleByIndex(_v2)`/`GetHandleByUUID`/`GetHandleByPciBusId(_v2)`/`GetHandleBySerial`/`GetIndex`。
  两侧共用 `config_allowed_devices()` 排序，保证 "cuda:i 就是 nvml i"。详见设计 §6.6。
- 边界见设计 §4.3.1 的 10 条（令牌签发、agent 落盘通道、fail-closed、env 时机、无 session 客户端等）。
- **C-2 注入方式（v0.3.4，推荐）**：不改 lupine 源码——provider 内置于 `libvgpu-remote.so`（§4.3.3.1），在其
  `restore(connection_id)`（lupine 在每个连接子进程首个 RPC 前调用，`connection_id`=`LUPINE_SESSION`）里
  消毒 session → 校验 `<session>/config/vgpu.config` 存在 → `setenv(VGPU_CONFIG_SESSION_PATH)` + **注册 pid 进
  `<session>/pids.config`** → 不存在返回非 0（fail-closed）；`stop()`（子进程退出）**从 `pids.config` 移除 pid**。
  LD_PRELOAD 仍须 server 进程级设置；库的 fork 安全修正（`g_vgpu_config` 置 NULL）仍必须做。详见设计 §4.3.3。
- **单制品合并（§4.3.3.1）**：provider 可内置于 `libvgpu-remote.so`（同一 .so 既做 hook 库又被 lupine dlopen
  当 provider；glibc 按 realpath 去重返回已加载句柄）。必须把 `lupinecr_get_lupine_provider_v1` 加进导出脚本
  `global:`（否则 `local: *` 藏掉），并 vendor `checkpoint_provider.h`；server 部署设
  `LUPINE_CHECKPOINT_LIBRARY=/opt/vgpu/lib/libvgpu-remote.so`（制品不叫 liblupinecr.so，默认路径找不到）。

## 2.3 实测结论（v0.3.3，重要）

- **客户端本地路由陷阱**：client/server 同机测试时 lupine-client 把设备 0 路由到本地 GPU（`routing.cpp:226-244`
  本地设备优先），`cuMemAlloc` 在客户端进程内用真实驱动执行，服务端 library 收不到调用 → 分配不受限，但
  nvidia-smi（nvml 总是走 server）显示限额 → 假象。**远程验收测试必须用无 GPU 客户端或 `LUPINE_DISABLE_LOCAL=1`。**
- **fork 安全（v0.4 更正：不是阻塞项，但已修）**：`load_controller_configuration` 守卫 `if (g_vgpu_config == NULL)`，
  atfork 原先不重置它。但实测 lupine 父进程从不碰 CUDA/dlsym（`server.cpp` 全文无相关调用，cuda 版本头是编译期
  常量），fork 时该指针恒为 NULL，所以当前不会失效。仍已在 atfork 置 NULL —— 它守的是"父进程一旦解析过 `cu*`
  就回退 env 构造出 `activate=1` 的 permissive 配置"这条**静默 fail-open**路径（设计 §4.3.2）。
- **记账口径**：子进程必须用 SESSION 模式（会话进程表过滤，§6.5），不能是 HOST 全机求和（`cuda_hook.c:2398`）。

## 3. 关键决策与事实速查（含文件:行号）

### lupine 侧
- 每连接 fork 子进程：`server.cpp:668-727`
- 子进程内可取到 session：`server.cpp:421-429`（`rpc_http2_session_id`）
- 进程表透传：`nvml_server.cpp:58-97`
- NVML 真函数获取（dlsym 句柄查询，会被库遮蔽）：`nvml_server.cpp:36-56`（`nvml_symbol`）
- 会话头 `x-lupine-session`：`h2.cpp:422, 710-724, 850-857`（客户端 env `LUPINE_SESSION`）
- 虚拟设备序号直接作 CUdevice：`routing.cpp:292-307`
- 客户端导出：`client.exports`（`cu*`/`dlsym`/`lupine_checkpoint_*`）、`nvml.exports`
- lupine 客户端 libcuda 也导出 `dlsym`：`client.cpp:8461`
- Python 客户端 `python/lupine`：`connect()` 只做"设 `LUPINE_SERVER` + ctypes 提前加载 shim"，返回普通
  `torch.device("cuda")`，**不注册新 torch backend**；`sidecar` 是为 macOS/CPU-only 宿主设计的容器化 PyTorch worker，
  **k8s Linux pod 不适用**。k8s 集成 = 注入层设 env/libs，应用零改动（设计 §2.4/§8.2）。
- **lupine 环境变量全量参考**（语义/默认/位置/使用矩阵）：`docs/lupine_env_reference.md`。要点：
  `LUPINE_SERVER`（端点，逗号列表≤16）、`LUPINE_SESSION`（会话头=容器判别基础，§6.2.1）、`LUPINE_DISABLE_LOCAL`
  （**远程测试必设 1**，否则本地路由，§4.3.2）、`LUPINE_PORT`（默认14833）、`LUPINE_TRACE/LOG_LEVEL/DEBUG/RPC_STATS`
  （排查）、`LUPINE_DRIVER_VERSION_OVERRIDE`（伪造驱动版本，慎用）。

### library 侧
- 真函数解析枢纽（远程化改造关键）：`loader.c:1116-1203`（dlopen 版本化 `libcuda.so.<ver>`），
  `load_necessary_data:3040-3055`
- glibc 真 dlsym 自发现（dlsym 拦截器无递归的关键）：`loader.c:1073-1110`；dlsym 拦截：`loader.c:2167-2210`
- 内存预算门：`cuda_hook.c:321-379`
- NVML 进程表 → used（容器 PID 过滤）：`cuda_hook.c:2342-2530`
- UVA ledger（按设备+本地PID，共享 mmap）：`loader.c:2504-2720`
- 令牌桶 + 利用率 watcher：`cuda_hook.c:642-673` / `1459-1742`
- NVML hook 表：`nvml_hook.c:36-44`；`nvmlDeviceGetMemoryInfo` 虚拟化：`nvml_hook.c:63-132`
- 设备索引/UUID 映射：`loader.c:2376-2492`
- 配置加载（文件优先→env 回退）：`loader.c:2856-2867` / `2723-2854` / `1478-1537`
- 配置 ABI：`include/hook.h:219-269`（device_t）、`529-609`（sm_node_region_t）、`406-421`（memory_node_t）

## 4. 构建 / 测试 / 检查命令

### Go 侧
```bash
make build            # 编译 bin/device-* 等（含 fmt+vet）
make test             # go vet + go test ./...
make docker-build     # 构建镜像（Dockerfile.base 里先构建 library）
```

### library（libvgpu-control.so）
```bash
cd library
./build.sh                                  # 产出 build/libvgpu-control.so（cmake Release）
make check                                 # 静态检查（无需 GPU）：hook 一致性、结构体布局
make check-exports                         # 校验导出符号（ABI 冲突族）
make test-nogpu                            # 共享区无 GPU 测试（sm_node / vmem 并发）
make test                                  # 需 GPU：LD_PRELOAD 冒烟测试
```

### lupine（`../lupine`）
```bash
cmake -B build && cmake --build build      # 产出 client 的 libcuda.so.1 / libnvidia-ml.so.1 与 lupine_driver_server
```
lupine 环境变量：`LUPINE_SERVER=host:14833`（客户端）、`LUPINE_PORT=14833`（服务端）、`LUPINE_SESSION`（会话）、
`LUPINE_DISABLE_LOCAL`、`LUPINE_REAL_LIBCUDA`、`LUPINE_TRACE`。

## 5. 代码约定

- **library/ 是纯 C（glibc/GCC），不引入 C++/STL**（`include/hook.h:49-57` 强制）。手维护 CUDA/NVML 类型子集
  （`include/cuda-subset.h`/`nvml-subset.h`），**不要**直接 include 真 `<cuda.h>/<nvml.h>`。
- 所有 hook 通过 `cuda_library_entry[]`/`nvml_library_entry[]` + `CUDA_ENTRY_CHECK`/`NVML_ENTRY_CHECK` 宏调用真函数。
- 新增 hook 时同步维护：`cuda_hooks_entry[]`/`nvml_hooks_entry[]`、`*_originals.c` 的直通实现、
  `deploy/*.exports.ld` 导出符号、`hack/check_cuda_hook_consistency.py` 一致性表。
- Go 侧配置写入与 C 读取共用 seqlock ABI（`docs/resource_data_seqlock_versioning_design.md`），改结构体要跑
  `make check`（结构体布局校验）。
- 提交前跑 `make check`（library）与 `make fmt vet`（Go）。

## 6. 复查/实施指引（给下一位 AI）

1. **先读** `docs/remote_gpu_pool_research_design.md`（完整方案分析）再动手。
2. **不要改 lupine 之外假设已存在的东西**：以下均为现状，需先核实再引用：
   - lupine 无任何进程注册/记账；远端 PID 只有打补丁才能拿到（设计 §3.1）。
   - 远端 `nvmlDeviceGetMemoryInfo`/`cuMemGetInfo` 返回宿主机聚合值（透传）。
   - lupine-server 生成 handler 直接引用 `cu*`（`gen_server.cpp:1760`），dlopen 后置无法拦截（设计 §4.0.1）。
3. **涉及 lupine 的改动要极小且机制化**：优先只读 RPC / 握手头；保持 `client.exports` 兼容。
4. **验证路径**：
   - 远程连通性：无 GPU 节点 `LUPINE_SERVER=gpu-node:14833` 起 lupine client，GPU 节点起 `lupine_driver_server`。
   - 内存隔离验收：`cuMemGetInfo`/nvidia-smi 显示限额视图；分配超限返回 OOM；多 Pod 互不干扰；
     `usedGpuMemory` 含上下文隐式内存（对比显式分配）。
   - 核心隔离（仅方案 C）：多客户端同卡竞争时各自 SM 限速正确。
   - 版本兼容（S5）：`client-12.9 ↔ server-12.4/12.6/12.8` 冒烟，确认跨版本 wire 兼容后收敛单制品（设计 §7）。
5. **安全**：配额下发必须是服务端权威（GPU 节点 session→配额），不信任客户端自报；并做**设备级访问控制**
   （服务端 allowlist / adapter 裁剪），防止用未分配 GPU（设计 §6）。
6. **性能**：拦截只加一层本地函数指针跳转；TCP 转发是 lupine 固有成本。核心隔离的利用率采样必须在 GPU 节点本地。

## 7. 常见坑

- lupine 客户端 libcuda.so.1 也导出 `dlsym`：与 library 的 dlsym 拦截器共存需 spike 验证（设计 §4.0.3）。
- 方案 C 中 dlopen 后置无法拦截已解析引用 → 必须**进程级 LD_PRELOAD**（库最先入全局符号表），父进程不碰 CUDA。
- 方案 C 里库的 dlsym 会遮蔽 lupine-server 的 `nvml_symbol<>()` 句柄 dlsym（`nvml_server.cpp:36-56`）：
  spike 验证已 hook/未 hook 的 nvml 函数均按预期拦截或直通。
- 配置加载"文件优先"（`loader.c:2856`）：方案 C 靠 `VGPU_CONFIG_SESSION_PATH` 定位 `<session>/config/vgpu.config`，
  远程节点不放全局 config；配合 `loader_child_after_fork` 置 `g_vgpu_config=NULL`（设计 §4.3.2 关键修正 1）。
- 设备访问控制不可省略：客户端经 lupine 可见 server 全部设备，需服务端 allowlist 或 adapter 裁剪（设计 §6.3）。
- lupine 按 CUDA 版本打多制品（client/server × cuda-ver × os）：兼容方向 **client ≤ server**；推荐单基准制品 +
  跨版本 wire 兼容 spike；server 制品绑定节点驱动（设计 §7）。
- `/tmp/.sm_node` / `/tmp/.vmem_node` 在子进程模型下需 per-session 作用域，避免跨租户串扰。
- 远程 Pod 节点无 `/proc/driver/nvidia/version`：library 版本化 dlopen 逻辑在远程模式必须走 lupine 解析分支。
- 中文注释/文档为主，代码注释遵循仓库既有风格（Apache-2.0 头）。
