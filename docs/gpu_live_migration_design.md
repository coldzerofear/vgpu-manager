# GPU 热迁移（Live Migration）能力规划与设计

> 状态：**分析 + 方案规划（2026-09-23 初版，2026-09-24 并入 mncr 实机结论）**，尚未动代码。
> 分析对象：`cuda-checkpoint/`（NVIDIA 官方，驱动能力的唯一权威）、`GPU-CR/`（FAST'26 GCR 的开源实现）、
> `cudackpt/`（社区 LD_PRELOAD + CRIU 方案）、`tensor-fusion/`（同类产品控制面）、`live-pod-migration/`
> （CRIU 容器迁移 operator）、`lupine/`（我们远程 GPU 的底座）、**`cuda-checkpoint-aburan28/`（`mncr`，跨节点 C/R 实机验证，见 §2.7）**、本仓库 `library/` 与 Go 侧。
> 外部参考：CRIU 4.0 `plugins/cuda`、CRIUgpu（arXiv 2502.16631）、趋动科技 OrionX 热迁移公开资料。
> 关联设计：`remote_gpu_pool_research_design.md`、`remote_gpu_k8s_integration_design.md`、
> `resource_data_seqlock_versioning_design.md`、`sm_multiproc_shared_bucket_design.md`。

---

## 0. 结论先行

**（1）不要自己实现 GPU 检查点。** NVIDIA 驱动 r550+ 已经把 `cuCheckpointProcess{GetState,Lock,Checkpoint,
Restore,Unlock}` 做成了驱动内能力，r570 起有 CRIU 集成与进程树支持，r580 起支持**跨 GPU 迁移**
（`CUcheckpointRestoreArgs.gpuPairs` / CLI `--device-map "oldUUID=newUUID,..."`）与容器部分透传。
`cudackpt` 那条"用户态追踪 + 重建"的路线在原理上就不健全（见 §2.3），**不采纳**。

**（2）我们 library 的定位不是"再造一个 C/R 库"，而是"迁移感知的旁路"（migration-aware）。**
它占着 `LD_PRELOAD` 这个位置，**不参与就一定会出错**：设备 UUID→序号映射被 `pthread_once` 钉死、NVML 句柄失效、
SM watcher 与共享令牌桶的采样所有权、检查点期间 NVML `used` 归零导致配额被别人吃掉。
所以答案是**集成而非合并**：调用驱动 API + 增加"迁移前后"的状态维护，**不把 cudackpt/GPU-CR 的拦截层搬进来**
（重复且冲突）。GPU-CR 的数据面加速（§2.4）可作为**后置可选**优化，因为我们已有 `cuMemAlloc`/VMM 入口。

**（3）真正的机会在远程 vGPU。** 趋动 OrionX 能做热迁移，根因不是它的检查点多强，而是它的 **client-server
架构让"应用进程根本不用动"**——GPU 上下文全在服务端。我们的 lupine 是同构架构，而且 lupine **上游已经把
迁移所需的静默栅栏和 provider 插件 ABI 写好了**（`checkpoint.h` 的 drain gate、`checkpoint_provider.h` 的
`start/restore/checkpoint/stop`），我们还**已经占住了这个 provider**（`library/src/checkpoint_provider.c`，
其中 `checkpoint()` 目前是空实现）。这是别人没有的起跑线。

**（4）分四层交付，每层都能独立产生价值，不必等全套落地：**

| 层 | 能力 | 是否需要 CRIU | 是否需要重建 Pod | 依赖 |
|---|---|---|---|---|
| **L0** | 进程级 GPU 检查点/恢复原语（挂起→显存清零→恢复） | 否 | 否 | 驱动 r570+ |
| **L1** | **同节点跨卡**热迁移（本地 vGPU，Pod 不重启） | 否 | 否 | L0 + r580 `--device-map` |
| **L3a** | **远程 vGPU 同节点跨卡**（客户端零感知，**零 lupine 改动**） | 否 | 否 | L0（作用于 lupine-server 子进程） |
| **L2** | 整 Pod 跨节点迁移 | 是 | 是 | CRIU 4.0 cuda 插件 + 独立 operator |
| **L3b** | 远程 vGPU **跨节点**会话迁移 | 是（仅服务端子进程） | 否（应用 Pod 不动） | L2 技术 + lupine 客户端重连 |

**建议先做 L0 + L1 + L3a**：它们不碰 CRIU、不碰 Pod 生命周期、不需要新 operator，却直接解决"GPU 碎片整理、
坏卡（XID/ECC）预警腾挪、高优先级抢占让位"这三个真实痛点——而这三件事今天在 vgpu-manager 上**只能靠杀 Pod**。

**（5）整 Pod 迁移（L2）确实应该独立成项目**，与用户设想一致。边界见 §6：
vgpu-manager 只提供 **GPU 侧原语 + 配额预留 + 配置重绑**三件事，CRIU/kubelet/OCI/Pod 重建全部交给外部 operator
（`live-pod-migration/` 是现成的骨架），且优先走 **CRIU 上游 cuda 插件**，让对方连"GPU"这个词都不用知道。

**（6）跨节点 GPU 迁移已经被开源实现跑通过一次，我们不必从零摸索。**
`aburan28/cuda-checkpoint` 的 `mncr`（§2.7）在双节点上实测完成了 rank 互换迁移，
**恢复到进程从未见过 UUID 的 GPU 上**，并留下两份实机测量文档与七个坑的记录。
最值得立刻吸收的四条：
- CRIU 的 CUDA 插件不支持设备重映射，但**在 `PATH` 前置一个 15 行 shim 包住 `cuda-checkpoint` 二进制**
  就能给那一次 restore 调用追加 `--device-map`（§2.7.5 坑 7）——这补上了我们原以为是死路的缺口；
- **提交点在 `lock` 与 `checkpoint` 之间**：之前可回滚，`checkpoint` 自身失败即**终态**。
  我们原来写的"任何一步失败都回滚"是错的，已按此修正（§2.7.3）；
- **CUDA IPC 的导入方 checkpoint 成功但 restore 必败且进程彻底丢失**，且失败发生在提交点之后
  （§2.7.4）——这条直接决定了我们的准入必须拦死 IPC 导入方，含我们自己的跨 Pod NVLink/IMEX；
- **主机可用内存必须 ≥ 在用显存**，否则任何驱动版本都不可检查点（§2.7.7）。

---

## 1. 术语与问题边界

一次"GPU 热迁移"实际是三件互相独立的事，混为一谈是大多数方案失败的原因：

| # | 子问题 | 谁能解决 |
|---|---|---|
| ① | **GPU 状态**：显存内容、上下文、流、模块、事件、内核队列 | 只有驱动（`cuCheckpointProcess*`）。用户态无法完整重建 |
| ② | **CPU 进程状态**：地址空间、线程、fd、socket | CRIU |
| ③ | **编排状态**：Pod 对象、卷、网络身份、设备注入、我们的配额与配置 | K8s + vgpu-manager |

- **L0/L1/L3a 只涉及 ①（+ 我们的 ③ 配置面）**：进程一直活着，不需要 ②。这是它们代价低的根本原因。
- **L2/L3b 才需要 ①+②+③ 全套。**

另有一个常被忽略的第四件事：**迁移窗口内的资源保留**。检查点完成后，进程在 NVML 里"消失"、显存占用归零
（这正是 cuda-checkpoint 的设计目的）。在我们的记账口径下（NVML 进程表求和，`cuda_hook.c` 的
`used` 计算），这意味着**同卡上的其他容器会立刻认为有空闲显存并把它吃掉**，随后 restore 必然 OOM。
而 NVIDIA 明确写了"检查点/恢复过程中出错**不保证进程还能用**"。所以**配额预留不是优化项，是正确性前提**。

---

## 2. 上游与同类方案盘点（事实核实）

### 2.1 NVIDIA `cuda-checkpoint` / 驱动 API —— 唯一可信的底座

能力与版本门槛（`cuda-checkpoint/README.md`，本地仓库已核实）：

| 驱动 | 新增能力 |
|---|---|
| r550 | `cuda-checkpoint` 工具可用；lock/checkpoint/restore/unlock 四动作 |
| r570 | **NVML 支持**；**与 CRIU 4.0+ 集成、支持进程树**；`cuCheckpointProcess*` 驱动接口与 CLI 等价；lock 带超时 |
| r580 | **GPU 迁移**（跨卡恢复）；**容器部分透传（partial passthrough）** |
| r595 | ARM CPU |
| r610 | `cuIpcGetMemHandle` 形式的 CUDA IPC 支持（`--launch-job` / `CUDA_CHECKPOINT_JOB_FILE`） |

挂起（suspend）语义（README 原文要点）：
1. 所有会提交工作/管理资源/影响 GPU 状态的 driver API 被**加锁**；
2. 已提交的工作（含 stream callback）**跑完**；
3. 设备显存**拷到主机**（由驱动自己管理的分配里）；
4. 释放全部 GPU 资源 —— 此时进程在 OS 层面不再引用任何 GPU 硬件，才可被 CRIU dump。

**CPU 线程不被挂起**，可以继续调 CUDA（会阻塞）、可以继续访问 host pinned 内存。

恢复：重新获取 GPU → 显存拷回并**在原虚拟地址重建映射** → 恢复 stream/context 等对象 → 解锁。

跨卡迁移（r580，`src/r580-migration-api.c`）：
```c
CUcheckpointRestoreArgs restore_args = {0};
CUcheckpointGpuPair *pairs = calloc(dev_count, sizeof pairs[0]);
for (i...) { cuDeviceGetUuid(&pairs[i].oldUuid, i); cuDeviceGetUuid(&pairs[i].newUuid, (i+1)%dev_count); }
restore_args.gpuPairsCount = dev_count; restore_args.gpuPairs = pairs;
cuCheckpointProcessLock(pid,&lock); cuCheckpointProcessCheckpoint(pid,&ck);
cuCheckpointProcessRestore(pid,&restore_args); cuCheckpointProcessUnlock(pid,&unlock);
```
CLI 等价形式（`src/r580-migration-cli.c`）：`cuda-checkpoint --action restore --pid <pid> --device-map "GPU-xxx=GPU-yyy,..."`。

> ⚠️ 注释里写明：**"CUDA 能看见的每一张 GPU 都必须出现在 pair 列表里，哪怕应用没用它"**。这条直接决定了
> §4.4 的容器设备可见性方案。

已知限制（README "Functionality" 节）：
- 不支持 **UVM 内存**、不支持 `cuMemExportToShareableHandle()` 创建的 IPC 内存；
- 检查点会**等已提交的工作跑完**（长 kernel = 长停顿）；
- **出错不保证进程仍可用**（没有事务性回滚）。

> 对我们的直接影响：**内存超卖（UVA/managed）与热迁移互斥**。`how_to_use_gpu_virtual_memory.md` 那条路径下
> 的容器必须在准入时就被判定为"不可迁移"。这是 fail-closed 的硬约束，不是警告。

### 2.2 CRIU 4.0 `plugins/cuda` —— 整进程迁移的上游正解

核实要点（`checkpoint-restore/criu` criu-dev 分支 `plugins/cuda/cuda_plugin.c`）：
- 钩子映射：`PAUSE_DEVICES` → `--action lock`（进程还在跑时静默）；`CHECKPOINT_DEVICES` → `--action checkpoint`
  （进程已被 seize/freeze 后）；`RESUME_DEVICES_LATE` → `restore` + `unlock`；
- 实现方式是 **fork 执行 `cuda-checkpoint` 二进制**（不是链驱动 API）→ **节点上必须有这个二进制且在 `$PATH`**；
- **不支持设备重映射**：代码里没有任何 device map / UUID 转换 / 相关环境变量；
- `PRE_DUMP` 阶段**强制禁用** CUDA 检查点（即不能做迭代式预拷贝）；
- 注释记录了 `cuInit()` 竞态与 UVM 指针在映射恢复前访问会崩的问题。

**价值**：runc/crun → CRI → kubelet `/checkpoint/<ns>/<pod>/<container>` 这条链路上，只要节点装了 CRIU 4.0 +
`cuda-checkpoint` + 驱动 r570+，**GPU 状态会被自动带上**，上层 operator 完全不需要感知 GPU。这是"云原生"
最优解，L2 必须走这条路。

**缺口**（外部资料核实，见 §9 待验证）：
- 社区共识是 **kubelet checkpoint API 只能 checkpoint，不能 restore**：恢复要靠"把 checkpoint tar 转成 OCI 镜像
  再当作容器镜像拉起"这个既定技巧（`live-pod-migration/` 正是这么做的）；
- **不支持 NCCL**（挂起会摧毁 communicator）、多 GPU 需要额外同步；
- ~~插件不做设备重映射 → 跨节点落到不同 UUID 的卡上能否恢复取决于容器设备注入~~
  → **已有经实机验证的解法（§2.7.5 坑 7）**：插件通过 `PATH` 解析 `cuda-checkpoint`，
  在 `PATH` 前置一个 15 行 shell shim，只对 `--action restore` 且未带 `--device-map` 的那一次调用
  追加 `--device-map`，其余调用透传。mncr 用它把两个 rank 迁到了**镜像从未见过 UUID 的 GPU** 上。
  这也是**我们能在 CRIU 路径上插手的那个点**（§4.6.2 说"截不到具名符号"仍然成立——
  这里包的是**二进制**，不是符号）。

### 2.3 `cudackpt` —— 路线不健全，不采纳

架构（本地 `cudackpt/`）：`LD_PRELOAD` 的 `libcudackpt.so` 拦截 Driver API，追踪
allocation/stream/module/symbol/event/context（`shim/tracker.hpp`），检查点时把显存 `cuMemcpyDtoH` 到
`device.bin`，再调 CRIU dump 进程；恢复时 CRIU restore → 用 VMM 在**原虚拟地址**重建显存
（`shim/restore.cu:36` `map_at()`：`cuMemAddressReserve(&addr, aligned, gran, want, 0)` + `cuMemCreate` +
`cuMemMap` + `cuMemSetAccess`）→ 拷回数据。

**为什么不健全**：
1. **不透明句柄没有翻译层**。`CUcontext`/`CUstream`/`CUmodule`/`CUfunction`/`CUevent` 都是驱动给的不透明值，
   CRIU 恢复的进程内存里存着**旧值**，而恢复时是重新创建的**新值**。`shim/` 里**没有任何 old→new 的转换**
   （`interpose.c` 里查不到 remap/translate 之类的路径）。只有在驱动恰好按同样顺序发回同样句柄时才"碰巧对"。
   它们自己的 README 也承认：*"GPU restore relies on deterministic reallocation or fixed virtual-address remap;
   bit-exact resume is workload-dependent."*
2. **拦截不到的状态一律丢失**：内核队列、graph exec、cuBLAS/cuDNN 的内部 workspace 与句柄、JIT 缓存、
   texture/surface 对象、P2P 映射。用户态无论拦多少个符号都补不齐这个洞——这正是 NVIDIA 把它做进驱动的原因。
3. **与我们的 library 正面冲突**：两个 `LD_PRELOAD` 库都要拦 `cuMemAlloc`/`dlsym`、都要维护分配表，
   顺序敏感、语义打架（我们要的是预算拒绝，它要的是记录），且 `cuMemAlloc` 会被换成 VMM 分配，
   直接破坏我们基于 NVML 真实 `usedGpuMemory` 的记账口径。

**可以借鉴的只有一点**：它的**镜像格式工程化**（manifest + CRC32C + 稀疏页 + 去重 + 增量快照 + 保留/回收 GC +
`inspect`/`validate`/`report` 子命令）值得抄进我们的 checkpoint artifact 设计（§6.4）。

### 2.4 `GPU-CR` —— 数据面/控制面分离，值得选择性吸收

架构（本地 `GPU-CR/`，对应 FAST'26 论文 GCR）：
- `LD_PRELOAD` 的 `vGPU-NVIDIA.so` 接管 `cudaMalloc`/`cuMem*`，把应用分配**换成 VMM 支撑的分配**，
  于是它能单独控制"虚拟地址"与"物理页"的生命周期；
- 检查点（`src/vGPU.cpp` 信号处理路径）：`syncAllKernels()` → 用**双缓冲 + event 重叠**把显存拷进
  hugepage staging（`/mnt/huge-ckpt`）→ 逐指针 `releasePhysicalMemory()`（`src/vGPU.cpp:195`）
  **释放物理页但保留虚拟地址** → 关闭 P2P → 然后才由 `cr_client` 调
  `cuda-checkpoint --toggle`（`coordinator/cr_client.cpp:137`，实现在 `src/GPUs/NVIDIA/nv.cpp:246`）
  去处理**剩下的控制状态**；
- 恢复：先 `cuda-checkpoint --toggle` 恢复控制状态 → 信号通知进程 → `remapPhysicalMemory()`
  （`src/vGPU.cpp:239`）→ 拷回数据 → 重开 P2P。

**核心洞察（README 的 Data / Control 延迟拆分就是这个意思）**：
把**数据面**从驱动手里抢过来自己做（重叠拷贝 + hugepage，远快于驱动串行拷到 pageable 主机内存），
驱动只剩**控制面**要处理（很小很快）。附带效果是"显存降到 0 但进程还活着"，可以让别的负载切进来。

**读代码补上的机制细节（比 README 精确）**：
- 它**替换 `cudaMalloc` 为 VMM 分配**（`src/GPUs/NVIDIA/nv.cpp:268-384`：`cuMemAddressReserve` →
  `cuMemCreate` → `cuMemMap` → `cuMemSetAccess`），并维护 `global_handle_map` 与
  `allocated_memory_type`（0=cudaMalloc / 1=VMM）；
- 检查点时 `cuMemUnmap` + `cuMemRelease` 但**不释放保留的虚拟地址**（`nv.cpp:175-193`），
  恢复时在同一 VA 上 `cuMemCreate`+`cuMemMap`（`nv.cpp:222-235`）——这就是"显存降到 0 而指针仍有效"；
- 控制通道是**共享内存 + 信号**（`src/comm/comm.h`：`ShareMemComm` + `INIT/CKPT/RESTORE/FINISH_MSG`
  与 `IPC_TEARDOWN/EXPORT/IMPORT_MSG`），`cr_client` 发信号、进程内信号处理器干活——
  与我们 §4.7.8 设想的"信号 + 控制线程"同构；
- 它**在库里自己处理 IPC**（`src/ipc_hooks.h`）：`ipc_teardown_all_imports()`、
  `ipc_save_and_teardown_all_exports()`、`ipc_teardown_all_events()`、`ipc_disable_all_peer_access()`，
  恢复侧对称重建。**这正是 mncr 要求应用自己做的事，GPU-CR 放进了库里**——代价是接管分配语义；
- 多 GPU 编排（`coordinator/multi_cr_client.cpp`）：`lock` 全部并行 → 失败则 `unlock` 全部（可回滚）
  → `checkpoint` 全部并行。**比 mncr 宽松**：它在 checkpoint 失败后也去 unlock，
  而 mncr 的混沌矩阵把 checkpoint 失败判为终态（§2.7.3）——**以 mncr 为准**。

**一处与 mncr 冲突、需按驱动版本探测的结论**：`ipc_hooks.h:155-160` 写明
*"the driver cannot restore cuMem VMM allocations with IPC handle types"*——即只要
`requestedHandleTypes` 设了 IPC 类型、**哪怕从未导出**就不能恢复，所以它连非导出的 cuMem 分配
都要拆掉重建。而 mncr 在 595 上实测"导出并持有 fd 也没事"。两者驱动版本不同（580.95.05 vs 595.91.07）
→ 见 §2.7.4 第 3 条与 spike **S9**。

**我们该吸收什么**：
- ✅ **理念**：Data/Control 分离、以及"显存让渡"这个独立于迁移的能力（对我们的超卖/抢占场景极有价值）；
- ✅ **可行性**：我们的 `library/` **已经**拦了 `cuMemAlloc*`、`cuMemCreate`，且 `cuMemAddressReserve/
  cuMemCreate/cuMemMap/cuMemSetAccess/cuMemUnmap/cuMemRelease` 全在入口表里（`include/cuda-helper.h:1041-1062`，
  `src/loader.c:490-500`），改造点比从零做小得多；
- ❌ **不吸收**：接管分配语义会与我们的**预算门 + NVML 真实口径记账 + UVA 超卖账本**三者同时耦合，
  风险极高。**必须后置到 P5，且默认关闭**。

### 2.5 `tensor-fusion` —— 开源里只有"能力位"与"相位"，实现是闭源

核实结论（避免误判）：README 第 78 行 *"GPU live-migration, snapshot and restore GPU context cross cluster"*
列在 **Enterprise Features** 下，**开源仓库里没有实现**。仓库里 `migrat` 的命中绝大多数是
"从 NVIDIA device-plugin 栈渐进迁移"（`internal/webhook/v1/pod_webhook.go:135` 的 `IsProgressiveMigration`），
与 GPU 热迁移无关。

**真正可借鉴的是它的控制面建模**（这部分开源）：
- `internal/gpuallocator/filter/virtualization_capabilities.go`：GPU 能力以 **annotation 里的一个 JSON**
  声明——`SupportsPartitioning / SupportsSoftIsolation / SupportsHardIsolation / **SupportsSnapshot** /
  SupportsMetrics / SupportsRemoting / MaxPartitions / MaxWorkersPerDevice`，调度器 filter 直接读它；
- `api/v1/constants.go:17` 定义 `PhaseMigrating`，`GPU` 与 `GPUNode` 的 phase 枚举里都有 `Migrating`
  （`api/v1/gpu_types.go:240,249`、`api/v1/gpunode_types.go:104,109`）。

→ 我们应照搬这两点：**节点/设备级"是否支持快照"的能力位**（驱动版本/CRIU/是否开了超卖共同决定），
以及**设备与节点的 `Migrating` 相位**（迁移期间把设备置为该相位，天然把调度器挡在外面）。

### 2.6 OrionX（趋动）—— 证明了"API 转发架构 = 迁移友好"

公开资料要点：client-server 架构；**"Server 端保存上下文和数据，Client 端存储相关信息"**，恢复时
client 申请新资源并恢复上下文；支持同节点跨卡与跨节点（推荐 RDMA）；实测**同节点约 16s / 跨节点约 19s**；
有环境检查与**失败自动回退**（"对业务不造成影响"）；限制：**仅支持同型号 GPU**、时间随显存线性增长、
依赖网络质量、大规模分布式训练（集合通信）仍在研究。

**对我们的直接映射**：lupine 就是这个架构。所以：
- "同型号 GPU"这条限制我们同样适用（驱动检查点镜像与硬件强相关）→ 进准入校验；
- "失败自动回退"必须做：`Lock → Checkpoint` 之后若 `Restore` 失败，要能原地 `Restore` 回源卡再 `Unlock`；
- "Client 端存储相关信息"对应我们的 `LUPINE_SESSION` + 会话目录，**已经有了**。

---

### 2.7 `aburan28/cuda-checkpoint` 的 `mncr` —— **唯一一个把跨节点 GPU 迁移真正跑通的开源实现**

> 分析对象：`cuda-checkpoint-aburan28/`（NVIDIA 官方仓库的 fork）。
> **这是本文档最有价值的外部参考**：它有两份**实机测量**文档（不是推断），跑通了双节点 rank 互换迁移，
> 并把过程中踩的坑逐条记录。下面凡标"实机"的都是它测出来的，不是我们推的。

#### 2.7.1 与官方仓库的差异

`diff -rq` 的结果很干净——**官方那 5976 字节的二进制与 `src/` 下的 demo 一字未改**，新增的是：

| 新增 | 是什么 |
|---|---|
| `mncr/` | 约 3200 行 Python + C：多节点 C/R 的完整实现（agent / coordinator / imagestore / k8s controller / NCCL seam / verify） |
| `docs/gds-rdma-transport-design.md` | 提案：给检查点数据面加 GDS/RDMA 直通（**需要改显示驱动，目前只是提案**） |
| `src/gds-transport-benchmark.cu` | 对照 benchmark：今天的 host-staged 路径 vs `cuFileRead/Write` 直通 |
| `src/Makefile` | 构建上面那个 benchmark |

→ 所以它的价值**全在 `mncr/`**，那个 GDS 提案对我们近期无用（等驱动）。

#### 2.7.2 它的核心论点，与我们的设计独立地收敛到了同一点

> *"The premise is that everything the driver cannot checkpoint can be destroyed before the checkpoint
> and rebuilt after it. That is what makes this buildable today — and it is also what you give up: transparency."*

这正是我们 §4.5/§4.6 给 library 设计的 quiesce/rebind 模式。差别在**施加对象**：

| | 对谁做 teardown/rebuild | 代价 |
|---|---|---|
| **mncr** | **应用**（要求训练脚本挂 `torchckpt.on_quiesce` 钩子里 `destroy_process_group()`） | **牺牲透明性**，应用必须"检查点感知" |
| **GPU-CR** | **库自己**（LD_PRELOAD 跟踪并重建 IPC export/import/event/peer-access） | 要接管分配语义 |
| **我们** | **只对 library 自己的状态**做 | 透明性保住；但应用自身的不可检查点资源要靠**准入拦截**而非重建 |

→ 我们的定位是三者中最保守也最安全的一档：**不碰应用状态，靠能力位把不可迁移的工作负载挡在门外。**

#### 2.7.3 commit point：一条我们文档里写错了的语义

> *"There is a commit point, and it sits between `lock` and `checkpoint`.
> Before it, a failure costs one drained step: unlock, resume, retry.
> After it, the ranks have released their GPU resources and the driver offers no rollback."*

它的混沌测试矩阵把这条钉死了（每条都实测 pass）：

```
CASE                 SIDE           RAISED     RESULT
dirty-rank           before-commit  abortable  aborted
lock-failure         before-commit  abortable  aborted
rank-killed          before-commit  abortable  aborted
agent-unreachable    before-commit  abortable  aborted
checkpoint-failure   after-commit   terminal   failed     ← 注意
dump-failure         after-commit   terminal   failed
resume-failure       after-commit   terminal   failed
unlock-failure       after-commit   terminal   failed
```

**我们 §4.1 M8 原来写的"任何一步失败 → 回滚到源卡 restore + unlock"是错的**，必须按提交点分开：

- **`Lock` 失败 / `Lock` 成功但决定放弃** → 可回滚（`Unlock` 即可，代价是一次排空）；
- **`Checkpoint` 调用本身失败** → **终态**。NVIDIA 明确"出错不保证进程仍可用"，实机也印证
  （见 2.7.4 的 import 案例：checkpoint 成功、restore 拒绝、随后连 unlock 都返回
  `"the operation cannot be performed in the present state"`，进程彻底丢失）；
- **`Checkpoint` 全部成功后决定放弃** → 可以 `Restore`（不带 device map）+ `Unlock` 回到原卡。

所以正确的说法是：**回滚窗口是"lock 之后、checkpoint 之前"，加上"checkpoint 全部成功之后"；
checkpoint 自身失败没有回滚。**

#### 2.7.4 实机能力矩阵（驱动 595.91.07，RTX PRO 6000 Blackwell）

| 分配方式 | checkpoint | restore | 备注 |
|---|---|---|---|
| `cuMemAlloc` | ✅ | ✅ | 对照组，校验和验证过 |
| `cuMemCreate` + `cuMemMap`（持有不共享） | **✅** | **✅** | **VMM 路径可检查点** |
| ＋`cuMemExportToShareableHandle`（POSIX fd 持续打开） | ✅ | ✅ | **仅导出不算违规** |
| **`cuMemImportFromShareableHandle` 导入方** | ✅ | **❌** | `invalid argument`，且**此后无法 unlock，进程彻底丢失** |
| `cuMemAllocManaged`（UVM） | ❌ | — | `operation not supported` |
| `cuIpcGetMemHandle` | ❌ | — | 610 才支持 |

**三条对我们直接有用的推论：**

1. **PyTorch 的 expandable segments 不必禁用**——它走 `cuMemCreate`/`cuMemMap`，实测可检查点。
   官方文档那句"不支持 `cuMemExportToShareableHandle` 创建的 IPC 内存"实测**要一分为二**：
   **导出方没事，导入方致命**。
2. **导入方致命发生在提交点之后**——这是 2.7.3 那条不对称性在真实硬件上的实例。
   → 对我们意味着：**任何使用 CUDA IPC 导入的容器都不可迁移**（多进程共享显存的推理框架、
   NCCL rank 之间、以及**我们自己的跨 Pod NVLink / IMEX 通道**），必须在准入期拦死，
   不能等到迁移时才发现。
3. **⚠️ 与 GPU-CR 的结论冲突，且很可能是驱动版本差异。** GPU-CR（实测于 580.95.05）的
   `ipc_hooks.h:155-160` 写明*"the driver cannot restore cuMem VMM allocations with IPC handle types"*
   —— 即**只要 `requestedHandleTypes` 设了 IPC 类型、哪怕从未导出**就不能恢复，因此它在检查点前
   把这类分配全部拆掉重建。而 mncr 在 595 上测出"导出并持有 fd 也没事"。
   → **结论：不要把"什么可检查点"写成静态表，必须按驱动版本探测。**
   mncr 的做法值得照搬：用一个 canary（`verify/vmm_probe.cu`）**跑一遍工作负载实际用的分配方式再试检查点**，
   拿驱动自己的判决当准入依据。

#### 2.7.5 跨节点实测结果与七个坑（**这一节是纯金**）

双节点（各 1 张 Blackwell，62 GiB 内存，socket 互联），torch 2.13.0+cu130，NCCL 2.29.7，CRIU 4.2.1：

```
SCENARIO    RESULT   SECONDS
continue    pass        6.22    检查点后继续跑；NCCL 拆掉重建
restore     pass        7.19    检查点后停；两个 rank 都被杀；criu 拉回来
migrate     pass       16.15    rank0 A→B、rank1 B→A；带非恒等 device map 恢复
```

**migrate 那一行是关键**：*"resumed on a GPU whose UUID the process had never seen"*——
**跨节点 + 换卡的 GPU 热迁移，开源实现里跑通了。**

| # | 坑 | 对我们的意义 |
|---|---|---|
| **1** | 请求文件在不同节点落到不同训练 step，而 NCCL 按**顺序**匹配集合通信 → 需要每个安全点做一次控制集合（`MAX` over `[epoch, lookahead]`）。另外：**终态失败后没人清请求文件**，被新起的 rank捡到 | 跨节点协调必须有屏障；**终态失败必须清理残留状态**（我们的 `VGPUMigration` status 要有终态清理） |
| **2** | **libfabric 在插件初始化时打开 `/dev/gdrdrv` 就再也不放**（aws-ofi-nccl 找不到 EFA、退回 socket，fd 仍留着）。CRIU 无法 dump 它，`destroy_process_group()` 也关不掉 | **只能在启动时预防**：`FI_HMEM_CUDA_USE_GDRCOPY=0` 或 `NCCL_NET_PLUGIN=none`。→ 我们的 **webhook 应对可迁移 Pod 注入这类 env** |
| **3** | **NCCL RAS 每进程留两个 LISTEN socket**（2.24 起默认开，per-process 不 per-communicator，永不关闭）。CRIU 能 dump，restore 时重新 bind → 同节点恢复撞 `Address already in use`，**发生在提交点之后** | `NCCL_RAS_ENABLE=0`（顺带把 communicator 初始化从 10.5s 降到 0.3s）。→ webhook 注入 + **进程扫描把任何 TCP socket 当作 before-lock 发现**，让它在可回滚侧便宜地中止 |
| **4** | **NCCL 把"出生所在节点的地址"缓存在静态内存里**，迁移后仍试图 bind 旧地址 → `Cannot assign requested address`。无 API 可重置、无法从 torch 底下重载 libnccl | 解法是 `libmncr_netmap.so`：**LD_PRELOAD 拦 `bind()`**，对 `EADDRNOTAVAIL` 的单播 IPv4 用本机路由地址重试；NCCL 的 listener 是从 `getsockname()` 上报的，所以对端能学到新地址。→ **同样的问题会出现在 lupine-server 跨节点恢复上**（L3b）。注意其局限：**IB verbs 的地址是 GID 不是 socket，这个 shim 到不了** |
| **5** | 清单（manifest）原来只写在本地目录，**没 dump 过该 rank 的节点读不到**（模拟器里所有"节点"共享一块盘所以没暴露） | 产物布局：**manifest 必须进共享存储**，且恢复方要被告知"这个 rank 是哪个节点 dump 的" |
| **6** | **CRIU 校验每个被映射文件的 build-id**。同一 AMI 相隔几分钟启动的两台机器，一小时后无法互相迁移：`libcrypto.so.3 has bad build-ID`（unattended-upgrades 跑过了）。它**还校验文件 mode**（抓到了不同 umask 下构建的 shim） | **比我们 M8 写的"同 library 版本 + 同驱动"严格得多**：**每个被映射文件**都要 build-id 一致。容器镜像天然满足，但**我们的 `libvgpu-control.so` 是从宿主机 bind-mount 进去的**（`vnum_plugin.go:807-810`）→ **两节点的 vgpu-manager 版本必须完全一致**。宿主侧路径带版本号（`HostVGPUControlFilePath` 有 `.<version>` 后缀）这点帮了我们，但容器内路径是固定的，CRIU 看的是容器内路径 + build-id。还要注意挂载 mode 一致（我们是 `ReadOnly: true`，一致） |
| **7** | **CRIU 的 CUDA 插件自己完成 CUDA 恢复，而且不带 device map**；插件不能绕过（CRIU 4.x 拒绝恢复一个 inventory 里点名了未加载插件的镜像），也无法告知迁移 | **解法：插件通过 `PATH` 解析 `cuda-checkpoint`，所以在 `PATH` 前面放一个 shim**，只对 `--action restore` 且尚未带 `--device-map` 的那一次调用追加 `--device-map "$MNCR_DEVICE_MAP"`，其余调用（`-h` 能力探测、`--get-restore-tid`、`--get-state`、lock、unlock）原样透传。**两个迁移的 rank 都是这么恢复成功的。** 实现见 `mncr/agent/criu.py:50-66`，一共 15 行 shell |

#### 2.7.6 另外四条实机细节（每条都能省我们一次真机翻车）

1. **CRIU 插件恢复完之后再调 `--action restore` 会失败**（`"the operation cannot be performed in the present state"`），
   因为已经没东西可恢复了。→ **恢复侧必须先 `--get-state` 再决定，并把"已经做完"当成成功**
   （`agent/driver.py:resume`）。这正好印证我们 §4.6.2.1 选的**被动纪元检测**是对路的：CRIU 路径下
   GPU 早就恢复好了，我们只需要重绑 library，**不该自己去调 Restore**。
2. **`criu dump` 默认会杀掉被 dump 的进程**，除非 `--leave-running`。他们的"检查点后继续跑"模式一度
   在真 CRIU 上变成"恢复一个已经不存在的进程"。→ 我们的 L0"显存让渡"场景必须显式 `--leave-running`。
3. **每个 CUDA 进程都持有 `/dev/nvidia-uvm` 的 fd 和映射**，哪怕只调过 `cudaMalloc`——runtime 无条件初始化 UVM。
   把它当 before-lock 阻塞条件会**拒掉所有工作负载**。它只在 **dump 门**才有意义（检查点后驱动已关闭所有 GPU fd，
   那时还在就说明检查点没生效）。→ **我们判断"是否用了 UVA 超卖"绝不能看 `/proc`**，
   要看我们自己的 `.vmem_node` 账本（我们恰好有精确答案，这是 library 的优势）。
4. **`lock` 16ms；`checkpoint` 一个 16 MiB 缓冲 286ms；但 `nvidia-smi` 要在调用返回后约 1 秒才不再列出该进程。**
   → **我们基于 NVML 的记账会有约 1 秒滞后**，配额预留的释放必须等到轮询确认，不能在调用返回就放；
   任何验收脚本都要带 deadline 轮询而不是查一次。

#### 2.7.7 两条硬性容量约束（实机算术，不是猜测）

- **主机内存 ≥ 在用显存**。驱动在 CRIU 介入之前就把显存拷进主机分配。他们那台 62 GiB 内存 / 97,887 MiB 显存的
  机器上，preflight 直接判 blocker：`host has 55,956 MiB available but device memory totals 97,887 MiB`。
  那台机器上的 vLLM 占了 93,494 MiB，**在任何驱动版本上都不可检查点——算术不允许**。
  → 我们的准入要**同时**校验两件事：容器 memory limit（驱动的拷贝计进本进程的 cgroup）
  **和**宿主机可用内存。
- **GPU 迁移要求 NVML persistence mode 开启**（`agent/preflight.py:117`，与 r580 demo 注释一致）。
  → 进节点能力位。

#### 2.7.8 一条给我们 library 的体检结论（顺手验证，结果是好的）

mncr 花了很大篇幅记录一个**它自己踩了的拦截陷阱**：

> *"PyTorch and NCCL `dlsym` exactly one symbol from libcuda, `cuGetProcAddress`, and resolve every other
> entry point through the pointer they get back. The hook redirected the entry points but handed back the
> driver's own resolver, so every lookup made through it landed inside libcuda and the redirect table was
> never consulted."*

结果是它的审计器在真 PyTorch 下**什么都没记录到**（torch 明明用了 2060 MiB 的 expandable segments）。
它给出的教训：**空的审计报告不能证明工作负载是干净的。**

**我们的 library 没有这个 bug**，已核实：`vgpu_dlsym_dispatch()`（`src/loader.c:1983-2016`）对
`symbol_is_cuda_api()` 为真的符号一律走 `resolve_local_hook()` 返回**我们自己的** hook，
注释也写明*"Driver symbols never take this path: they must keep resolving to our hooks whatever the handle says"*。
所以 `dlsym(handle, "cuGetProcAddress")` 拿到的是我们的 `cuGetProcAddress`，torch 之后经它解析的每个入口
都会过我们的 `g_routes` 替换。**这条顺带解释了为什么我们的库在 PyTorch 下一直有效。**

## 3. 分层能力模型（本文骨架）

```
                        ┌──────────────────────────────────────────────┐
   L2 整 Pod 跨节点     │  独立项目（live-pod-migration 形态）           │
   （CRIU + kubelet）   │  PodMigration/PodCheckpoint/PodRestore CRD     │
                        │  ↓ 只调用下面这层的契约，不懂 GPU              │
                        └───────────────┬──────────────────────────────┘
                                        │ gRPC: PreCheckpoint / PostRestore
                        ┌───────────────┴──────────────────────────────┐
   L1 同节点跨卡        │  vgpu-manager 节点侧 Migration Agent          │
   L3a 远程同节点跨卡   │  （device-plugin DaemonSet 内，hostPID+priv）  │
                        │  Lock/Checkpoint/Restore/Unlock/Abort + 预留   │
                        └───────────────┬──────────────────────────────┘
                                        │ 驱动 API / cuda-checkpoint
                        ┌───────────────┴──────────────────────────────┐
   L0 GPU C/R 原语      │  NVIDIA 驱动 cuCheckpointProcess*             │
                        └───────────────┬──────────────────────────────┘
                                        │ 必须让路 + 前后维护自身状态
                        ┌───────────────┴──────────────────────────────┐
                        │  libvgpu-control.so（迁移感知的旁路）          │
                        │  设备重绑定 / 记账冻结 / watcher 让位 / 句柄失效 │
                        └──────────────────────────────────────────────┘
```

**关键判断：L0/L1/L3a 完全落在 vgpu-manager 内部，不需要新 operator、不需要 CRIU、不需要动 Pod 生命周期。**
这是"先做"的理由，也是把 L2 推出去独立成项目的理由。

---

## 4. 本地 vGPU：library 需要怎么改

### 4.0 为什么 library 必须参与（而不是"agent 自己调 cuda-checkpoint 就完了"）

因为 library 在应用进程里缓存了一批**跨迁移必然失效**的状态，且它维护的账本在迁移窗口里会给出**危险的错误答案**：

| 现状锚点 | 迁移后会发生什么 |
|---|---|
| `src/loader.c:2941-2952` 一串 `pthread_once`：驱动版本、CUDA/NVML 库句柄、`reset_cuda_index_mapping`、`init_nvml_to_host_device_index` | 设备 UUID→序号映射**被钉死**。跨卡迁移后仍按旧卡记账、旧卡限速 |
| `src/loader.c:2376-2492` 设备索引/UUID 映射 | 同上 |
| `cuda_hook.c:2342-2530` NVML 进程表 → `used` | 检查点期间本进程从 NVML 消失 → `used` 归零 → **同卡其他容器把显存吃掉 → restore 必然 OOM 且进程不可恢复** |
| `cuda_hook.c:1459-1742` 利用率 watcher + 令牌桶；`sm_node` 共享桶的 CAS 补给选举 | 被迁移进程若正好是采样 owner，迁移期间整容器限速失准；进程"消失"又"回来"会让 AIMD/delta 控制器读到 0 利用率后爆冲 |
| `nvml_hook.c` 的 NVML 句柄 | 跨卡后句柄指向**旧设备**，`nvmlDeviceGetMemoryInfo` 虚拟化视图错卡 |
| `cuda_hook.c` 的 `cuMemHostAlloc` 后置预算校验 | 检查点期间 `used`=0，校验形同虚设（不致命，但说明账本在窗口内不可信） |
| `include/hook.h` `device_t.uuid` + seqlock 热更新 | **好消息**：配置面**已经**支持热更新设备 UUID，Go 侧改写 + 库重读即可，不需要新 IPC 通道 |

### 4.1 改造清单（M1–M9）

> 约定：全部挂在新 feature gate **`GPUCheckpointRestore`**（Core registry，默认 `false`）之下；
> 未开启时代码路径逐字回退到今天的行为（与 `VGPU_CONFIG_SESSION_PATH` 的门控风格一致）。
> **M1/M9 与 §4.5/§4.6 是同一件事的两面**：§4.6 定"对外长什么样"，这里定"内部改哪些文件"。

**M1 — 劫持 `cuCheckpointProcess*`（不是直通；语义见 §4.6）**
- 在 `cuda-subset.h` 补 `CUcheckpointLockArgs/CheckpointArgs/RestoreArgs/UnlockArgs/CUcheckpointGpuPair`
  与 `CUprocessState` 的类型子集（遵循"不 include 真 `cuda.h`"的约定）；
- `cuda-helper.h` 的 `cuda_entry_enum_t` 与 `loader.c` 的 `cuda_library_entry[]` **同序**新增 5 个符号
  （`check_cuda_hook_consistency.py` 的 R1/R2 会验证）；
- **进 `cuda_hooks_entry[]`**：这样经 `cuGetProcAddress` 解析的调用者也会拿到我们的钩子
  （`g_routes[]` 按 real_fn→hook_fn substitution，`loader.c:1200-1207`）；
- 真函数调用一律用 **`CUDA_ENTRY_CHECK_STRICT`**（`cuda-helper.h:119-129`）——老驱动上符号不存在时
  返回 `CUDA_ERROR_NOT_SUPPORTED`，与驱动自身行为一致，**不能走 `CUDA_ENTRY_CHECK` 的 NULL 兜底**；
- 钩子里先做 **PID 纪律判定**（§4.6.3）：`pid != getpid()` 一律纯直通 + WARNING；
- 导出：`global: cu[A-Z]*;` **已覆盖**，无需改导出脚本；但要扩 `hack/check_exported_symbols.sh` 正向断言；
- 同步维护 `hack/check_cuda_hook_consistency.py`（R6 要求每个入口都有 ELF wrapper）与 `cuda_originals.c`。

**M1b — 导出正交原语 `vgpu_library_quiesce/resume`（§4.6.5）**
- 供 agent / CRIU action-script 调用（L2 路径上 GPU 状态归 CRIU 插件，我们只做 library 那一半）；
- **必须手工加进** `deploy/libvgpu-control.exports.ld` 的 `global:`（`local: *` 会藏掉，
  `lupinecr_get_lupine_provider_v1` 踩过这个坑）。


**M2 — 设备绑定代次（binding generation）：把 `pthread_once` 变成可重入**
- `resource_data_t` 头部（或 `device_t.reserved[]`）新增 `bind_gen`（`uint32`，seqlock 保护，Go 侧 mirror 同步，
  按 `resource_data_seqlock_versioning_design.md` 的规矩 bump `CONFIG_LAYOUT_VERSION` 并跑 `make check`）；
- 库侧把 `reset_cuda_index_mapping` / `init_nvml_to_host_device_index` 从 `pthread_once` 改为
  **"once + gen 比对"**：热路径只读一个原子 `uint32`，不等于本进程缓存的 gen 时才走重建慢路径；
- 重建内容：CUDA 序号↔UUID 映射、NVML 句柄、`config_allowed_devices()` 排序结果、vmem/sm 共享区的设备槽位。
- **这是 L1（跨卡迁移）的必要条件**，也是"Go 侧改一行 UUID 就完成设备重绑"的关键。

**M3 — 迁移窗口的记账语义（正确性前提，不是优化）**
- 新增容器级状态 `migration_state ∈ {NONE, LOCKED, CHECKPOINTED, RESTORING}`，落在共享配置区（seqlock），
  由节点 agent 权威写入；
- `CHECKPOINTED` 期间：
  - `used` 计算**不再**采信 NVML 进程表的归零结果，而是**冻结在检查点前的水位**（保留一个 `frozen_used`），
    使同容器其他进程、以及 `cuMemGetInfo` 虚拟化视图保持稳定；
  - 跨容器的保护**不能**靠库（库只管自己容器）→ 必须由 **Go 侧账本预留**（§5.2）兜底。库这边只保证"自己不误判"。
- `RESTORING` 结束后按新设备重算水位。

**M4 — SM watcher / 共享令牌桶让位**
- 共享桶（`sm_multiproc_shared_bucket_design.md` 的 `.sm_node`）的补给 owner 选举增加**自愿退位**：
  进入 `LOCKED` 前若本进程是 owner，主动 CAS 让出，避免"owner 被冻住 → 整容器无人补给"；
- watcher 在 `LOCKED/CHECKPOINTED` 期间**跳过采样**，并在 `RESTORING` 结束后**重置控制器状态**
  （delta 的误差项与 AIMD 的窗口都要清零，否则会把"迁移期间利用率 0"当成需要爆冲的信号——
  `sm_controller_aimd_sawtooth_analysis.md` 记录过同类爆冲）。

**M5 — 共享内存区与 CRIU 的兼容（只影响 L2/L3b）**
- **好消息（已核实）**：容器内路径是**固定**的（`pkg/deviceplugin/vgpu/vnum_plugin.go:377` 起的
  `ContManagerDirectoryPath` 系列、`/tmp/.vgpu_lock`、`/tmp/.sm_node`、`/tmp/.vmem_node`），
  **pod UID 只出现在宿主机侧路径**（`vnum_plugin.go:748` `<host>/<pod-uid>_<cont-name>`）。
  所以 CRIU dump 出来的文件映射在目标容器里**路径一致**，这是能恢复的前提，我们不需要额外做路径稳定化。
- 待办：
  - 目标节点必须在 restore **之前**把这些文件建好且**大小一致**（`CONFIG_FILE_SIZE` 固定 8192 已经帮了大忙）；
  - 区域头部 magic/layout 校验在恢复后要能接受"内容变了"（新设备 UUID、新 gen）而不是 fail；
  - **`flock` 状态跨 CRIU 的行为需实测**（`src/lock.c`、registry 的 `LOCK_EX`）——列为 spike S4。

**M6 — 统一的"恢复后再初始化"钩子 `library_after_restore()`**
- CRIU 恢复的是**同一个进程镜像**，`pthread_once` 的已完成状态会被原样恢复 → 所有缓存都必须能**主动失效**；
- 复用 M2 的 gen 机制作为触发器（agent 在 restore 前 bump gen），**不引入新的进程间通道**；
- 与现有 `loader_child_after_fork`（`loader.c:2883-2888`）并列，但语义不同：fork 是"重置 once 让子进程重做"，
  restore 是"同一进程主动丢弃缓存"。两者共用一个 `invalidate_cached_state()` 内部函数。

**M7 — NVML 句柄失效与重取**：并入 M2 的重建集合，单列是因为 `nvml_hook.c` 的枚举族
（`nvmlDeviceGetHandleByIndex/UUID/PciBusId/Serial`）与 `config_allowed_devices()` 排序耦合，
跨卡后"cuda:i 就是 nvml i"这条不变式必须重新建立（见 AGENTS.md §2.2 v0.4 定案）。

**M8 — 护栏与 fail-closed**
- 容器若启用了**内存超卖（UVA/managed）**→ 标记为**不可迁移**（§2.1 驱动限制），准入期就拒绝；
- 检查点元数据里记录 **library 版本 + 驱动版本 + GPU 型号 + CUDA 版本**，恢复前逐项校验，任一不符 → 拒绝恢复
  （对应 OrionX 的"仅支持同型号 GPU"与"环境检查"）；
- 失败处理**按提交点分档**（§2.7.3，mncr 的混沌矩阵逐条实测）：
  - `Lock` 失败，或 `Lock` 成功后决定放弃 → **可回滚**，`Unlock` 即可，代价是一次排空；
  - **`Checkpoint` 调用自身失败 → 终态，没有回滚**。NVIDIA 明确"出错不保证进程仍可用"，
    实机印证：导入方 checkpoint 成功、restore 报 `invalid argument`、此后连 `Unlock` 都返回
    `"the operation cannot be performed in the present state"`，进程彻底丢失；
  - `Checkpoint` **全部成功**后决定放弃 → 可以 `Restore`（不带 device map）+ `Unlock` 回原卡。
  → 所以控制面必须区分 `abortable` 与 `terminal` 两种失败，终态失败要清理残留状态（§2.7.5 坑 1）
  并把 Pod 标记为不可恢复、上报事件，而不是重试。
- **准入必须拦死的四类（都来自 §2.7 的实机结论）**：
  1. **CUDA IPC 导入方**——导入方 checkpoint 成功但 restore 必败且进程不可恢复（§2.7.4）。
     包括多进程共享显存的推理框架、NCCL rank 之间，**以及我们自己的跨 Pod NVLink / IMEX 通道**；
  2. **UVA/managed 超卖**——驱动不支持（判据要用我们自己的 `.vmem_node` 账本，
     **绝不能看 `/dev/nvidia-uvm` 的 fd/映射**，那个每个 CUDA 进程都有，§2.7.6 第 3 条）；
  3. **主机可用内存 < 在用显存**——驱动在 CRIU 介入前就把显存拷进主机分配，算术不允许（§2.7.7）。
     要**同时**校验容器 memory limit 与宿主机可用内存；
  4. **NVML persistence mode 未开启**——GPU 迁移要求它开着（§2.7.7）。
- **能力位不能写成静态表，要探测**：GPU-CR（580）与 mncr（595）对"IPC handle type 的 VMM 分配能否恢复"
  给出了相反结论（§2.7.4 第 3 条）。照搬 mncr 的 canary 思路：**用工作负载实际使用的分配方式跑一次
  探针再试检查点，拿驱动自己的判决当准入依据**。
- **跨节点还要加一条比"同型号 GPU"严格得多的**：**CRIU 校验每个被映射文件的 build-id 与 mode**（§2.7.5 坑 6）。
  容器镜像天然满足，但 `libvgpu-control.so` 是从宿主机 bind-mount 的
  （`pkg/deviceplugin/vgpu/vnum_plugin.go:807-810`）→ **两节点 vgpu-manager 版本必须完全一致**。
  宿主侧路径带 `.<version>` 后缀有助于发现不一致；挂载 mode 保持 `ReadOnly: true` 一致。
- **NVML 记账有约 1 秒滞后**（`nvidia-smi` 在 checkpoint 调用返回后约 1s 才不再列出该进程，§2.7.6 第 4 条）
  → 配额预留的释放必须等轮询确认，不能在调用返回就放；验收脚本一律带 deadline 轮询。

**M9 — 进程内静默栅栏（新增，见 §4.6.7）**
- hook 入口 `enter()` / 出口 `leave()` 计数，`library_quiesce()` 关闸并等归零（带超时）；
- 不做这一步，quiesce 里的 munmap 会和正卡在 hook 里持有 flock / `g_memory_node_lock` 的线程撞成 UAF；
- 远程路径上游已经 drain 过（`lupine_checkpoint_drain_cuda_calls()`），**本地路径必须自己完整实现**——
  这是"本地优先"路线上最大的单项新增工作量。


### 4.2 明确不做的事

| 不做 | 理由 |
|---|---|
| 把 cudackpt 的追踪/重建层合并进 library | §2.3，原理不健全 + 与现有 hook 正面冲突 |
| 用 library 自己实现上下文/流/模块的序列化 | 用户态做不完整，驱动已提供 |
| 默认接管 `cuMemAlloc` 为 VMM 分配（GPU-CR 式） | 与预算门/NVML 口径/超卖账本三重耦合，风险过高；后置 P5 且默认关闭 |
| 在 library 里内嵌 CRIU 逻辑 | 进程自己不能 dump 自己；这是 agent 与外部 operator 的事 |

### 4.3 触发通道：不新增通道，复用已有的两条

1. **配置面（Go → 库）**：`vgpu.config` 的 seqlock 热更新，承载 `migration_state`、`bind_gen`、新设备 UUID。
   已经是双向验证过的 ABI（`resource_data_seqlock_versioning_design.md`），成本最低。
2. **控制面（agent → 驱动）**：节点 agent 直接对**容器内进程的宿主机 PID** 调 `cuCheckpointProcess*`。
   device-plugin DaemonSet **已经是 `hostPID: true`**（`charts/vgpu-manager/templates/device-plugin/daemonset.yaml:54`）
   且 privileged，容器 PID 列表也已经有（registry 写的 `pids.config`，`pkg/device/registry/`）。
   **不需要在库里开新的 RPC 服务端**，这点很重要——库是纯 C、且在应用进程里，暴露控制面会是安全面。

### 4.4 一个必须先定的问题：容器怎么「看见」目标卡

> **本节 2026-09-23 复核后重写。** 初稿推荐的"选项 A：注入迁移域全部设备"在当前代码基线上**会直接崩进程**，
> 而不是温和降级。下面是核实后的结论。

#### 4.4.1 现状：本地与远程是**两套完全不同**的可见性机制

| | 本地路径（device-plugin） | 远程路径（lupine + library） |
|---|---|---|
| 谁保证"可见 = 已分配" | **容器运行时**：`NVIDIA_VISIBLE_DEVICES`/CDI/volume-mounts + `DeviceSpec` 下发 `/dev/nvidiaN`（`pkg/deviceplugin/vgpu/vnum_plugin.go:402,424-509`） | **library 自己**：provider `restore()` → `apply_visible_devices()` setenv `CUDA_VISIBLE_DEVICES=<会话设备 UUID 列表>`（`library/src/checkpoint_provider.c:237-276`） |
| 强制层级 | **内核 cgroup**（device allowlist） | **用户态库 + 驱动自裁剪** |
| 我们是否设 `CUDA_VISIBLE_DEVICES` | **否**。`util.CudaVisibleDevices`（`pkg/util/consts.go:265`）在 Go 侧**没有任何使用点** | 是（服务端子进程内） |
| NVML 遮蔽族 | **不启用**。`nvmlDeviceGetCount/GetHandleBy*` 全部被 `session_enabled()` 门控（`library/src/nvml_hook.c:209-217,241-247` 等），本地是纯直通 | 启用 |
| 用户自设 CVD 的作用对象 | 真驱动，收敛到"注入设备"的子集 | **lupine 客户端**的聚合表（`lupine/routing.cpp:193-219`，lupine 自己实现了完整 CVD 语义，支持序号与 UUID） |

**两层 CVD 是正交组合，今天没有冲突。** 远程下是 `用户子集 ⊆ 会话设备（服务端 CVD） ⊆ 节点物理设备`，
两个 CVD 分别作用在**两个不同的进程**（消费容器里的 lupine 客户端 / GPU 节点上的 server 连接子进程），
用户改不到服务端那一个。本地下是 `用户子集 ⊆ 注入设备 = 分配设备`。两边方向都是收敛，安全。

#### 4.4.2 但本地模式对"配置里没有的卡"是 fail-open，且核心限额路径会 `exit(1)`

这两条今天都不会被触发（本地容器里不可能出现未分配的卡），但**迁移一旦引入多余的可见设备就会立刻踩中**：

| 锚点 | 行为 |
|---|---|
| `cuda_hook.c:358-372` `load_limited_memory_view()` | `host_index < 0` → `return 0` → `MEMORY_PATH_GPU` → **不限额放行** |
| `nvml_hook.c:97-99` `nvmlDeviceGetMemoryInfo()` | `host_index < 0` → 原样返回物理值（不虚拟化） |
| `cuda_hook.c:1984-2010` `init_device_cuda_cores()` | 遍历 `cuDeviceGetCount` 返回的每一张卡，任一张找不到 config 条目 → `LOGGER(FATAL, ...)`；而 `LOGGER` 的 FATAL 分支是 **`exit(1)`**（`include/hook.h:792-794`）。只在**开了核心限额**时走到 |

→ 初稿"选项 A"的实际后果是：**开核心限额的容器在 SM watcher 初始化时直接 `exit(1)`**；
没开核心限额的容器不崩，但未分配卡上的分配**完全无限额**——AGENTS.md §6.6 给远程路径提的那条
"可绕过配额用未分配 GPU"，会因为选项 A 在**本地模式第一次真的出现**。

而且**靠我们自己设 `CUDA_VISIBLE_DEVICES` 来兜底是不牢的**：CVD 是单值环境变量，用户在 pod spec /
entrypoint / 代码里重设就覆盖了（"设 CVD 子集"本来就是合法用法）。本地模式下我们既没有 NVML 遮蔽
（被 session 门控），也**明确定案不 hook `cuDeviceGetCount/Get`**（AGENTS.md §2.2 v0.4）——**没有第二道防线**。

#### 4.4.3 更关键的未知：驱动是否允许目标卡不在 CUDA 可见集里

NVIDIA 两个迁移 demo 的 `main()` **第一件事都是**：

```c
CHECK_OK(unsetenv("CUDA_VISIBLE_DEVICES"));
CHECK_OK(unsetenv("CUDA_DEVICE_ORDER"));
```
（`cuda-checkpoint/src/r580-migration-api.c` 与 `r580-migration-cli.c`，两份都有）

配合那句注释"**CUDA 能看见的每张 GPU 都必须出现在 pair 列表里，哪怕应用没用它**"，强烈暗示
**跨卡迁移是在"进程能看见全部物理卡"的前提下设计的**。而 CVD 是 `cuInit` 时消费的，
检查点之后进程的 CUDA 虽被拆掉，env 早已被读过——**恢复时驱动是否重读 CVD、目标卡能否在 CVD 之外，未知**。

这是 L1 形态的**唯一阻塞性未知**，必须由 spike **S2** 回答：

- **若目标卡必须在 CVD 内** → 迁移域的全部卡必须**从容器创建起**就 CUDA 可见 → 本地模式必须改造成
  "library 强制"（§4.4.4），没有别的路；
- **若 `--device-map` 按 UUID 全局解析、不受 CVD 约束** → 可以走"**窗口内临时可见**"（§4.4.5），
  隔离强度不降级，这是更好的结果。

#### 4.4.4 分支一（若目标卡必须常驻可见）：把远程那套复用到本地迁移模式

不发明新机制，**把远程模式已经设计/评审/落地过的那套原样启用到本地**：

1. 对 opt-in 迁移的 Pod，device-plugin 注入**迁移域内全部 GPU 设备节点**；
2. 由我们写 `CUDA_VISIBLE_DEVICES`，值取自 `config_allowed_devices()` 的同一排序
   （`library/src/config_io.c:238-254`），保住"cuda:i 就是 nvml i"这条不变式；
3. **把 NVML 遮蔽族的 `session_enabled()` 门控放宽**为"配置里有 allowlist 就遮蔽"——代码已经写好了，
   今天只是只对会话模式生效；
4. `load_limited_memory_view` 在**迁移模式下**把 `host_index < 0` 从 fail-open 改为 **fail-closed**（返回 OOM）；
5. `init_device_cuda_cores` 的 FATAL 改成"跳过并告警"——**这条无论走哪个分支都该改**：
   今天一张不在配置里的卡就能 `exit(1)` 掉用户进程，太脆；
6. webhook 对"opt-in 迁移 且 自设 `CUDA_VISIBLE_DEVICES`"的 Pod **直接拒绝**，并在文档写明
   迁移模式下 CVD 归我们所有。

**代价必须诚实写进文档**：迁移域内的设备隔离从**内核 cgroup 强制降级为 library 强制**，
等级与远程模式一致。对"同租户内碎片整理"可接受，对多租户强隔离场景不可接受 → 必须 Pod 级 opt-in。

#### 4.4.5 分支二（若目标卡可在 CVD 之外）：窗口内临时可见（**更优，优先验证**）

- 平时容器只注入已分配的卡，**现状与隔离强度都不变**；
- 迁移窗口内，特权 agent 临时把目标卡加进容器的 device cgroup（v2 下是 eBPF program）+ 在容器 mount ns 内
  `mknod`；迁移完成后把源卡移出；
- 同样需要 §4.4.4 的第 5 条（FATAL → 跳过告警），因为窗口内会短暂出现"可见但未激活"的卡。

代价：agent 要直接操作 cgroup（CRI 不暴露该能力），升级/重启的失配处理要小心。
但**换来隔离强度不降级 + 用户 CVD 语义不受影响 + fail-open 面不扩大**，值得优先验证。

#### 4.4.6 远程路径（L3a）不受这些问题影响

远程模式下迁移发生在 **GPU 节点的 lupine-server 连接子进程**里：那个进程的 CVD 本来就是我们在
`restore()` 里 setenv 的、用户碰不到；要让目标卡可见只需在会话配置里多激活一张卡再重设 CVD——
而子进程是**每连接新建**的，`cuInit` 尚未发生（`library/src/checkpoint_provider.c:217-222` 的注释
明确了这个时序）。所以 **L3a 走分支一的成本几乎为零**，这是它应该优先做的又一个理由。


---

### 4.5 library 状态跨 C/R 的生命周期矩阵

> 回答一个根本问题：**检查点时 library 的中间态会被保存吗？切卡恢复后会不会和新卡冲突？还是会重新加载？**

#### 4.5.1 两种场景，机制相反，结论相同

| | **场景 A：同节点跨卡**（只用 cuda-checkpoint，无 CRIU）= L1/L3a | **场景 B：跨节点**（CRIU）= L2 |
|---|---|---|
| library 中间态 | **既不保存也不恢复——它从头到尾没被碰过** | **被完整保存并完整恢复**（就是进程地址空间的一部分） |
| 为什么 | cuda-checkpoint 只动 GPU 侧，进程用户态内存一字节不改；README 明确"does not suspend CPU threads" | CRIU 恢复的是同一个进程镜像，`.data`/`.bss`/heap/线程栈逐字带回 |
| 会重新 dlopen library / 重跑构造函数吗 | **不会** | **不会**（`pthread_once` 的"已完成"标记也被恢复） |
| 危险形态 | **静默**：library 状态没变，底下的卡换了，没有任何事件通知它 | 额外约束：`.so` 是文件映射，目标节点上**路径/版本/大小必须逐字一致** |

**共同结论：library 永远不会自动重建，必须我们主动失效。** 这就是 M2/M6 存在的理由。

#### 4.5.2 好消息：80% 的重建代码已经有了——fork 复位路径

`child_after_fork()`（`src/cuda_hook.c:259-304`）+ `loader_child_after_fork()`（`src/loader.c:2880-2919`）
已经实现了"让 library 忘掉一切并重新初始化"。**C/R 重建 = fork 复位 + 6 处差异**：

| 要重建的状态 | 锚点 | fork 路径已覆盖 | C/R 的差异 |
|---|---|---|---|
| 全部 `pthread_once` / mutex | `loader.c:2883-2902` | ✅ | — |
| `cuda_to_host` / `cuda_to_nvml` 映射 | `loader.c:2183-2185` | ✅ `g_reset_cuda_index_init` 复位 | — |
| **`nvml_to_host_device_index[]`** | `loader.c:2187` | ❌ `reset_cuda_index_mapping()` **只清前两张表** | **① 必须补清**，否则新卡的 NVML 序号映到旧槽位 |
| `nvml_devices[]` / `g_sm_num[]` | `cuda_hook.c:1139,186` | ✅ `g_init_set` 复位 → `initialization()` → `init_device_cuda_cores()` | **② `init_device_cuda_cores` 需可重入**，且遇未知卡不能 `exit(1)` |
| **watcher 线程** | `cuda_hook.c:1449` | ✅ 隐式（fork 不继承线程） | **③ CRIU 会恢复全部线程** → 老 watcher 带旧 `host_indexes[]` 继续跑 + `initialization()` 又起一批 = 双份。**必须主动停并 join** |
| watcher 的 `host_indexes[]` | `cuda_hook.c:1454-1459` | — | **在线程栈上**，全局代次机制够不着，只能靠停线程重建 |
| `g_sm_lock_fd` / 采样所有权 | `cuda_hook.c:269-274` | ✅ | — |
| gap `CUevent` | `cuda_hook.c:276-278` | ✅ 置 NULL | **④ 置 NULL 不够，必须先 `cuEventDestroy`**，且必须在 `Lock` 之前 |
| graph cost cache | `cuda_hook.c:295` | ✅ | — |
| **`g_memory_node` 链表** | `loader.c:2906-2918` | ✅ 但**整个 free 掉** | **⑤ C/R 必须保留**（dptr 恢复后仍有效），只需把 `host_index` 翻译到新槽位 |
| `g_vgpu_config` 映射 | `config_io.c:296-310` | 条件性（`config_source_moved()` 按路径比对） | 路径没变 → 不会重映射，需**强制**重映射（新文件/新 inode） |
| `g_dev_hot[].cur_cuda_cores` | `cuda_hook.c:280-285` | 故意不清（共享桶语义） | C/R 要清 |
| `.vmem_node` / `.sm_node` / `pids.config` 里的账 | — | 不涉及 | **⑥ 跨节点必须注销 + 重注册**；容器内 PID 不变但**宿主 PID 变了** |
| `cuda_library_entry[]` 函数指针、`g_routes[]` | `loader.c:1206` | ✅（三个 once 复位 → 重 dlopen/dlsym） | 需驱动版本指纹校验（比"同型号 GPU"更严格） |
| 控制器历史（`shares[]`/`top_results[]`/AIMD 窗口/独占 FSM） | `cuda_hook.c:1461-1472` | ✅ 隐式（watcher 线程启动时重置） | 随 ③ 一起解决 |

→ 实现上只需把 `child_after_fork()` 拆成 `reset_common()` + `drop_memory_ledger()`：
**fork 调两个，C/R resume 只调第一个。** 不需要写一套平行的重建代码。

#### 4.5.3 穿越 C/R 的"信物"：把权威信息搬进自己的 .bss

场景 B 下共享内存会被 munmap（见 §4.6），所以"必须跨越 C/R 的权威信息"不能放在共享区，要放进 `.bss`——
CRIU 免费带走，不需要任何外部存储：

```c
static int  g_quiesced;                      /* 是否处于静默态，restore 侧的唯一触发器 */
static char g_pre_ckpt_slot_uuid[16][41];    /* slot -> uuid 快照，用于翻译 memory ledger */
static size_t g_pre_ckpt_vmem_used[16];      /* 本进程 UVA 用量，用于按新槽位写回 */
static char g_pre_ckpt_driver_version[32];   /* 环境指纹，恢复前校验，不符 fail-closed */
```

---

### 4.6 `cuCheckpointProcess*` 劫持：把驱动 API 本身当作我们的控制面

#### 4.6.1 核心判断

**采纳劫持，但它只覆盖"走公开具名 API 的同进程调用者"。** 两件事都要说清楚，
覆盖面的确切边界见 §4.6.2（结论比初稿更强也更窄）。

> **前提已确认（2026-09-23，实机）**：目标驱动的 `libcuda.so` **确实导出具名 `cuCheckpointProcess*`**。
> 这是整个劫持方案的成立条件，原 S8 就此关闭（剩余的 args 结构体布局核对转为 S8-b）。
> 注意这个事实**不能**从 `cuda-checkpoint` 二进制推出来——它走的是私有导出表（§4.6.2），
> 两者是各自独立的两条路径。

**为什么采纳**：驱动的四阶段 API 与我们需要的动作**天然一一对应**，把时序封装进库里，
外部调用者不可能搞错；而且完全不扩大 ABI 面。

| 驱动 API | 我们在钩子里做什么 | 时序要求 |
|---|---|---|
| `cuCheckpointProcessLock(pid)` | 先记环境指纹与 `slot→uuid` 快照进 `.bss`，再调真函数，成功后 `library_quiesce()` | quiesce **零 CUDA 调用**（§4.6.2.2），故放真函数之后也安全；真函数失败则不 quiesce |
| `cuCheckpointProcessCheckpoint(pid)` | 直通（此时已静默） | — |
| `cuCheckpointProcessRestore(pid, args)` | **先**校验环境指纹（不符直接拒绝，不调真函数），**再**调真函数（含 `gpuPairs` 跨卡重映射），**后** `library_rebind()` | rebind 必须在真函数之后：它要读的是迁移**之后**的设备 |
| `cuCheckpointProcessUnlock(pid)` | 调真函数；若本次是 `Lock → Unlock`（未经 Checkpoint/Restore），在此**撤销静默**；清 `g_quiesced` | 见 §4.6.4 状态机 |

watcher 不在钩子里重起——交给后续第一次 launch 的 `pthread_once(&g_init_set)` 自然拉起，
与现有语义一致（NVML-only / dlsym-only 的进程本来就不该有 watcher）。

另有一个白捡的好处：导出脚本里 `global: cu[A-Z]*;` **已经覆盖**这些符号名，不需要新增导出条目
（对比自定义 `vgpu_library_checkpoint` 那种方案）。

#### 4.6.2 覆盖面的真正边界：`cuda-checkpoint` 走的是**私有导出表**，不是具名符号

初稿说"截不到 CRIU 路径"的理由是 mount namespace——那个理由成立但**不是根本原因**。
对 `cuda-checkpoint/bin/x86_64_Linux/cuda-checkpoint`（5976 字节，动态链接）实测：

```
$ readelf -d cuda-checkpoint | grep NEEDED
 (NEEDED)  Shared library: [libcuda.so.1]
 (NEEDED)  Shared library: [libc.so.6]

$ readelf --dyn-syms cuda-checkpoint        # 只有两个 CUDA 符号
  UND cuDriverGetVersion
  UND cuGetExportTable
```

**它根本不导入 `cuCheckpointProcess*`。** 它用 `cuGetExportTable(&table, &uuid)` 向 libcuda 取一张
**未公开的私有导出表**，再从表里取函数指针调用——这样一个二进制就能同时支持 r550（公开 API 尚不存在）
到 r610。外部资料亦印证这一点（NTT docomo Engineers' Blog 对 CUDA 12.8 Checkpoint API 的分析）。

**所以真正的覆盖面是：**

| 调用方 | 走什么路径 | 钩子能截到吗 |
|---|---|---|
| 我们的控制线程 / agent 触发 | 公开具名 API（r570+） | ✅ |
| lupine provider（远程） | 公开具名 API | ✅ |
| 应用 / SDK 自检查点 | 公开具名 API | ✅ |
| **`cuda-checkpoint` 二进制** | **`cuGetExportTable` 私有表** | ❌ **具名符号劫持在原理上就截不到** |
| **CRIU cuda 插件** | fork/exec 上面那个二进制 | ❌ |

即使把库塞进那个 helper 进程（它跑在宿主机 mount ns，而我们的 `/etc/ld.so.preload` 只 bind-mount 进容器，
`vnum_plugin.go:810-811`——所以本来也塞不进去），**调用也不经过任何我们能 hook 的具名符号**。

**推论：不要试图 hook `cuGetExportTable`。** 那张表按 `CUuuid` 索引、布局完全未公开、随驱动版本变化，
包错一个函数指针就是整个 CUDA 崩掉。我们当前**既不 hook 也不解析**它（`loader.c:244`、
`cuda_originals.c:1735-1737`、`cuda-helper.h:557-558` 三处都被注释掉，我们的 `.so` 不定义该符号，
调用直通真 libcuda）——**这是对的，保持现状**。

#### 4.6.2.1 那"无缝对接任意工具"怎么办？答案是**被动纪元检测**

把问题拆成两半，两半的可达性完全不同：

| | 能不能被动做到 | 为什么 |
|---|---|---|
| **静默（checkpoint 侧）** | ❌ **不能可靠做到** | CRIU 的 `PAUSE_DEVICES`（lock）时进程确实还在跑，我们的 watcher 理论上能察觉，但紧接着就被 seize/freeze——这是**竞态**（watcher 周期 25–100ms，freeze 在毫秒级）。不可依赖 |
| **重绑（restore 侧）** | ✅ **完全可以** | 恢复后进程照常运行，**下一次进任何 hook** 就能发现"世界变了"并重建 |

所以架构是：

```
静默：必须显式协作（三条通道，共用同一份 library_quiesce() 实现）
        (a) 我们的 cuCheckpointProcess* 钩子（同进程调用者）
        (b) agent 信号 → 库内控制线程 → vgpu_library_quiesce()
        (c) CRIU action-script(pre-dump) → agent → (b)

重绑：天然无缝，对任何调用者都成立
        hook 入口的两级纪元检查 → library_rebind()
```

两级检查，都极廉价：

```c
/* load_necessary_data() 最前面 */
if (unlikely(__atomic_load_n(&g_quiesced, __ATOMIC_RELAXED))) {
    library_rebind();            /* 我们自己静默过：共享区已 munmap，凭 .bss 信物重建 */
} else if (unlikely(g_vgpu_config != NULL &&
                    __atomic_load_n(&g_vgpu_config->incarnation, __ATOMIC_RELAXED)
                        != g_cached_incarnation)) {
    library_rebind();            /* 外部工具路径：共享区仍映射着，但 agent 已写入新代次 */
}
```

第二条分支正是**兜底**：哪怕别人绕过我们用 `cuda-checkpoint` 直接操作，只要 agent 在恢复侧更新了配置，
我们下一次进 hook 就自愈。**若连 agent 都没参与**（纯手工操作），则 `incarnation` 不变、`g_quiesced` 为 0，
我们察觉不到——这种情况下应当由 §4.6.6 的能力令牌 + `cuCheckpointProcessGetState(getpid())` 抽查
做 **fail-closed**（发现"被静默过但没走我们的通道"就拒绝继续），而不是静默给出错误的限额。

#### 4.6.2.2 一处修正：quiesce **不应包含任何 CUDA API 调用**

初稿断言"`cuEventDestroy` 必须在 `Lock` 之前，否则死锁"，由此推出一条硬顺序约束。复核后**这条约束应当取消**：

- `cuCheckpointProcessCheckpoint` 的职责本来就是把**进程内全部 CUDA 对象**（含我们的 gap `CUevent`）
  一起搬走并在 restore 时还原；跨卡时 `gpuPairs` 会把驱动自己拥有的东西一并重映射。
  我们的事件按**配置槽位**索引（`g_gap_start[host_index]`），槽位语义在迁移前后不变，所以**大概率无需销毁**。
- 更重要的是设计上的收益：**只要 quiesce 里一个 CUDA 调用都没有**（只有 `munmap`/`pthread_join`/
  文件写/`close`），它就**不受 `Lock` 约束，可以在 lock 之前或之后的任意时刻执行**——鲁棒性大幅提升，
  也让通道 (b)/(c) 不必关心驱动状态。
- 因此：**quiesce 定义为"零 CUDA 调用"**；是否需要销毁 `CUevent` 降级为 spike（新增 S7），
  若实测确有必要，再作为**唯一**的例外放在 `Lock` 钩子调真函数之前。

#### 4.6.3 PID 纪律：只对自己负责，不越界

驱动 API 按 pid 操作，我们必须遵守同样的边界——库被 preload 进容器内**所有**进程，越界会伤到无辜进程：

```c
if (pid == getpid())  →  完整处理（quiesce / rebind）
else                  →  纯直通 + 一条 WARNING 日志
                         （目标进程的 library 状态没人处理，日志要说出来）
```

**不要试图跨进程代劳**：我们无法从 A 进程安全地操作 B 进程的 `.bss`、线程和映射。
需要跨进程时走 agent 通道，由目标进程自己执行。

多进程容器同理：静默是**按进程**的，被静默的进程从 `.vmem_node`/`pids.config` 注销后，
同容器其他进程的记账自然收缩——这是正确行为（它确实不再占用 GPU），但共享令牌桶的补给 owner 要让位（M4）。

#### 4.6.4 状态机与失败语义

```
   RUNNING ──Lock──▶ LOCKED(已静默) ──Checkpoint──▶ CHECKPOINTED
      ▲                   │                              │
      │                Unlock                         Restore
      │                   ▼                              ▼
      └──── resume ── RUNNING                     LOCKED ──Unlock──▶ RUNNING(已重绑)
```

**`Lock → Unlock`（不经 Checkpoint）是合法序列**（CRIU 中止、lock 超时回滚都会走到），
所以 `Unlock` 钩子必须能**撤销静默**（重映射共享区、重注册、重起 watcher），不能假设中间一定发生过 Checkpoint。

失败语义（NVIDIA 明确"C/R 出错不保证进程仍可用"，所以边界要严）：

| 时机 | 规则 |
|---|---|
| `Lock` 前的 quiesce 失败 | **不调真函数**，直接返回错误。失败在"还没造成伤害"的一侧 |
| `Restore` 后的 rebind 失败 | 真函数已经执行、进程已在新卡上，**不能让调用失败**；置降级标志 + `LOGGER(ERROR)` + 后续分配 fail-closed |
| 环境指纹不符（驱动/library 版本） | `Restore` 钩子里**在调真函数前**拒绝，fail-closed |
| 老驱动没有这些符号 | 用 `CUDA_ENTRY_CHECK_STRICT`（`cuda-helper.h:119-129`）返回 `CUDA_ERROR_NOT_SUPPORTED`，与驱动行为一致 |

#### 4.6.5 仍然需要的正交原语

劫持解决不了 L2——那条路径上 **GPU 状态归 CRIU 插件**，我们只该做 library 那一半。所以除了钩子，
还要导出一对**只管 library 自己**的原语，供 agent/action-script 调用：

```c
int vgpu_library_quiesce(void);   /* 只静默 library，不碰 GPU 状态 */
int vgpu_library_resume(void);    /* 只重绑 library，参数从配置区自取 */
```

三条路径的分工：

| 路径 | GPU 状态 | library 状态 | 入口 |
|---|---|---|---|
| L1 同节点跨卡 | 我们 | 我们 | `cuCheckpointProcess*` 钩子（agent 经控制线程触发） |
| **L2 跨节点（CRIU）** | **CRIU cuda 插件** | 我们 | `vgpu_library_quiesce/resume`（agent + action-script） |
| L3a 远程同节点跨卡 | 我们 | 我们 | `cuCheckpointProcess*` 钩子（provider 或 agent 触发） |

这两个符号**必须手工加进** `deploy/libvgpu-control.exports.ld` 的 `global:`（`local: *` 会藏掉，
`lupinecr_get_lupine_provider_v1` 踩过这个坑）与 `hack/check_exported_symbols.sh` 的正向断言。

#### 4.6.6 安全：新增入口带来的配额绕过，必须堵

库被 preload 进容器内所有进程，容器里任何代码都能调 `cuCheckpointProcessLock(getpid())`。
大部分影响仅限自身，但有一条是真的：**若 quiesce 顺手把自己从 `.vmem_node`/`pids.config` 注销，
它就"释放"了自己的记账，随后 resume 再申请 → 配额绕过。**

两道缓解，都要上：
1. **配额预留由 Go 侧账本持有，与进程内状态无关**（与 §4.1 M3"预留是正确性前提"合流）。
   导出入口只动"进程存活性"记录，碰不到配额上限；
2. **能力令牌**：agent 写进配置区的 nonce。无令牌时钩子只做纯 `quiesce`，**拒绝**驱动侧的
   Checkpoint/Restore 代理动作。

#### 4.6.7 M9（新增）：进程内静默栅栏

钩子被调用的那一刻，其他应用线程可能正卡在我们的 hook 里、持有 `lock_gpu_device` 的 flock 或
`g_memory_node_lock`。这时去 munmap 共享区就是 use-after-free。

所以 `library_quiesce()` 第一步必须是**排空自己的 hook**：入口 `enter()` / 出口 `leave()` 计数，
quiesce 时关闸并等归零（带超时）。lupine 为等价的事写了整个 `checkpoint.cpp`，这不是小工作量。

**分工**：远程路径上游已经 drain 过（`lupine_checkpoint_drain_cuda_calls()`），我们只需 drain 自己的
NVML/记账路径；**本地路径必须完整实现**——这是"本地优先"路线上最大的单项新增工作量。

### 4.7 实施步骤（逐步可交付、可验证、可回退）

> 原则：**每一步单独可合入、单独可回退、都有不需要 GPU 的验证手段**；任何一步不做后面都不会崩，
> 只是能力缺失。步骤 1–3 不依赖任何 spike 结论，**现在就能做**。

#### 4.7.0 先记两个会直接引入 bug 的坑（复核代码时发现）

**坑 A：恢复后若 `pids.config` 没有先重建，进程会 `exit(1)`。**
`load_container_pids()`（`cuda_hook.c:2328-2350`）在列表为空时走 `LOGGER(FATAL, ...)` → `exit(1)`
（注释写明这是刻意的：空列表会让限额变成空操作，等于把整张卡交出去）。而 CRIU 恢复后容器内 PID 虽被保留，
`pids.config` 是**宿主机侧写的文件**，跨节点后目标节点上是新建的空文件。
→ **硬性时序要求：agent 必须在进程 resume 之前写好 `pids.config`**（CRIU 的 `post-restore` 阶段，
不是 `post-resume`）。这条要写进 §6.2 的 `PostRestore` 契约。

**坑 B（好消息）：容器 PID ↔ 宿主 PID 的映射不会过期。**
`container_pid_cache_t` 是**每次调用在栈上新建**的（`cuda_hook.c:2514`：`container_pid_cache_t
container_pids = {.loaded = 0};`），`pids.config` 每次重读，宿主侧比对走 `.host_proc` 的 cgroup 字符串
（`cuda_hook.c:2213-2233`）也是每次现算。所以**跨节点后宿主 PID 变了也能自动对上**，
不需要任何额外的失效逻辑。

#### 4.7.1 步骤 1（P0，独立小 patch）：`reset_cuda_index_mapping()` 补清第三张表

- **文件**：`src/loader.c:2338-2346`
- **改法**：循环里补 `nvml_to_host_device_index[index] = -1;`
- **为什么现在做**：它今天就是一个潜在错卡源（fork 场景恰好设备不变所以没暴露）；
  一行改动，与后续所有步骤解耦
- **风险**：极低。`get_host_device_index_by_nvml_device()` 本来就有 `-1` 时的重建慢路径
- **验证**：`make check` + `make test-nogpu`；新增一个单测断言三张表在 reset 后全为 -1
- **回退**：直接 revert

#### 4.7.2 步骤 2（P0）：`init_device_cuda_cores()` 去 FATAL 化 + 拆出可重入 `rebind_devices()`

- **文件**：`src/cuda_hook.c:1984-2010`（及 `initialization()` 的调用点 `:2070`）
- **改法**：
  1. 拆成 `static int rebind_devices(int *device_count)`，`initialization()` 调它；
  2. 三处 `LOGGER(FATAL, ...)` 改为 `LOGGER(WARNING, ...) + continue`——**跳过这张卡**而不是杀进程
     （§4.4.2 已论证：今天一张不在配置里的卡就能 `exit(1)` 掉用户进程，这本身就是脆弱点）；
  3. 函数改为**幂等可重入**：进入时清 `nvml_devices[]`/`g_sm_num[]`/`g_max_thread_per_sm[]` 再重填，
     不假设首次调用
- **注意**：`cuDeviceGetCount` 失败仍应 FATAL——那是"驱动不可用"，与"多了一张不管的卡"性质不同
- **风险**：中。跳过某张卡意味着该卡上的 launch 走 `host_index < 0` 的 fail-open 路径。
  **必须同步**：在迁移特性开启（`GPUCheckpointRestore=true`）时，`load_limited_memory_view()`
  的 `host_index < 0` 分支改为 fail-closed（返回 OOM），见 §4.4.4 第 4 条
- **验证**：无 GPU 下用 stub 表驱动 `rebind_devices()` 跑两遍，断言第二遍结果与第一遍一致（幂等）
- **回退**：feature gate 关闭时保持 FATAL 行为（用编译期或运行期分支都可，推荐运行期）

#### 4.7.3 步骤 3（P0）：watcher 线程可停、可 join

- **文件**：`src/cuda_hook.c:1960-1980`（`active_utilization_notifier`）、`:1449-1492`（`utilization_watcher`）
- **改法**：
  1. `static pthread_t g_watcher_tids[MAX_DEVICE_COUNT / DEVICE_BATCH_SIZE];` + `static int g_watcher_n;`
     （现在 `pthread_t tid` 是局部变量，创建完就丢了，**根本没法 join**）；
  2. `static volatile int g_watcher_stop;`，`utilization_watcher` 的 `while (1)` 改为
     `while (!__atomic_load_n(&g_watcher_stop, __ATOMIC_RELAXED))`，并在 `clock_nanosleep` 前后各查一次
     （否则最坏要等一个完整周期）；
  3. 新增 `stop_watchers()`：置标志 → `pthread_join` 全部 → `g_watcher_n = 0` → 清 `g_watcher_stop`
- **风险**：中。join 可能被慢的 NVML 调用拖住 → **必须带超时**（`pthread_timedjoin_np`），
  超时则放弃 join 但保持 stop 标志（线程自己会退出），并记 WARNING
- **易错点**：`child_after_fork()` 里必须把 `g_watcher_tids`/`g_watcher_n` 清零——fork 的子进程
  **没有**继承这些线程，留着旧 tid 会让后续 `stop_watchers()` join 一个不存在的线程
- **验证**：`make test-nogpu` 起 stub watcher 反复 start/stop 1000 次，断言无泄漏无挂起
- **回退**：直接 revert（这一步不改变任何既有行为，只是新增能力）

#### 4.7.4 步骤 4：静默栅栏 M9（**关闸 + 排空**，不只是排空）

这是整条路线上最大的单项，也是最容易写出 bug 的一项。

- **为什么必须有**：quiesce 要 `munmap(g_vgpu_config)`。但**我们的 hook 在驱动之前**——即使驱动已 Lock，
  应用线程调 `cuMemAlloc` 仍会先进我们的 hook，`load_limited_memory_view()` → `get_device_snapshot()`
  就去读那块内存。指针不置 NULL 就是 **SIGSEGV**；置 NULL 则每个读点都要判空，而判空后的现状行为是
  **fail-open**（`host_index < 0` 直接放行）→ 静默期间限额完全失效。
- **所以语义必须是"关闸"**：quiesced 期间新进入的 hook **阻塞等待**而非放行。
  这与驱动 `Lock` 的语义一致（CUDA 调用阻塞到 unlock），对应用是自然的。
- **实现要点**：
  1. `static volatile int32_t g_gate_closed;` + `static volatile int32_t g_gate_inflight;`；
  2. hook 入口 `gate_enter()`：先 relaxed load `g_gate_closed`，为 0 则 `__atomic_add_fetch(&g_gate_inflight, 1)`
     后**再查一次** `g_gate_closed`（double-check，避免与关闸竞态）；为 1 则在条件变量上等；
  3. hook 出口 `gate_leave()`：`__atomic_sub_fetch` 并在归零时唤醒 quiesce；
  4. **重入**：用 TLS 计数器，同一线程嵌套进入只在最外层计数
     （`cuMemAllocManaged` 等路径是否会进到另一个 hook 需逐条核对，宁可用 TLS 兜底）；
  5. **超时**：排空等待带超时，超时则**放弃 quiesce 并开闸**，返回错误 —— 失败在"还没造成伤害"那一侧；
  6. **async-signal-safety**：若 quiesce 由信号触发，handler 只能 `write()` 一字节给控制线程，
     真正的关闸/排空在控制线程里做。
- **死锁风险**：被关在闸外的线程若持有应用层的锁，而触发方在等它 —— 驱动自身的 `Lock` 有同样问题，
  NVIDIA 给了 timeout，我们照做
- **验证**：纯状态机可在 `make test-nogpu` 里压测（N 个线程狂进出，主线程反复关闸/开闸，
  断言关闸完成时 inflight 恒为 0、无死锁、无计数泄漏）。**先把测试写出来再接进 hook**
- **回退**：gate 默认常开（`g_gate_closed` 恒 0），此时 `gate_enter/leave` 退化成两个 relaxed 原子操作

#### 4.7.5 步骤 5：`library_quiesce()` / `library_rebind()` + `.bss` 信物

- **前置**：步骤 1–4
- **`library_quiesce()`（零 CUDA 调用，§4.6.2.2）**，顺序：
  1. `gate_close()` + 排空（步骤 4）
  2. 快照进 `.bss`：`g_pre_ckpt_slot_uuid[][]`、`g_pre_ckpt_vmem_used[]`、`g_pre_ckpt_driver_version[]`
  3. `stop_watchers()`（步骤 3）
  4. 从 `.vmem_node` / `.sm_node` / `pids.config` 注销本进程；**让出共享桶采样所有权**；
     `close(g_sm_lock_fd)` —— 这一步**必须做**：被检查点的进程若还占着 flock，
     整个容器的令牌补给会停摆
  5. `munmap` 三个共享区并把指针置 NULL
  6. `g_quiesced = 1`（**最后一步**，它是 restore 侧的唯一触发器）
- **`library_rebind()`**，顺序：
  1. 校验环境指纹（驱动版本 / library 版本）→ 不符 **fail-closed**
  2. `reset_common()`（= `child_after_fork()` 拆出的、**不含** `drop_memory_ledger()` 的那一半）
  3. 强制重映射 `g_vgpu_config`（新文件/新 inode，不能走 `config_source_moved()` 的路径比对）
  4. 用 `g_pre_ckpt_slot_uuid[]` 把 `g_memory_node` 每条记录的 `host_index` 翻译到新槽位；
     **翻译不到的记录**（旧 UUID 在新配置里不存在）→ 记 ERROR 并丢弃该条，不能保留错误归属
  5. 按新槽位重注册 `.vmem_node`（写回 `g_pre_ckpt_vmem_used[]`）、`.sm_node`
  6. `g_cached_incarnation = cfg->incarnation`；`g_quiesced = 0`
  7. `gate_open()`
- **并发正确性（易错）**：多个线程可能同时发现纪元变化 → `library_rebind()` 内部用一把互斥锁 +
  进入后**重新判定**是否仍需重建；且整个函数必须**幂等**
- **验证**：无 GPU 下用 stub 共享区跑 quiesce→rebind 往返，断言 ledger 槽位翻译正确、
  共享区条目数守恒、重复调用幂等
- **回退**：feature gate 关闭则两个函数都不被调用

#### 4.7.6 步骤 6：`cuCheckpointProcess*` 钩子骨架

- **前置**：步骤 5（但**可以先只做骨架**：PID 纪律 + 直通 + 状态机 + 日志，不接 quiesce，用于真机摸底）
- **文件**：`include/cuda-subset.h`（类型子集）、`include/cuda-helper.h`（enum）、`src/loader.c`（入口表）、
  `src/cuda_hook.c`（钩子实现 + `cuda_hooks_entry[]`）、`hack/check_cuda_hook_consistency.py`
- **类型子集是最大的单点风险**：`CUcheckpointLockArgs/CheckpointArgs/RestoreArgs/UnlockArgs/
  CUcheckpointGpuPair/CUprocessState` 这些结构**没有 `struct_size` 自描述字段**，布局抄错就是把垃圾指针
  交给驱动。demo 只暴露了 `gpuPairsCount`/`gpuPairs` 两个字段，**完整布局必须对着 CUDA 13 的 `cuda.h` 抄**。
  → 建议：实施前先取一份 CUDA 13 header 核对，并在 `make check` 里加一条"仅在检测到 CUDA toolkit 时执行"
  的交叉校验（与现有 `hack/check_struct_layout.py` 同风格）
- **真函数调用一律用 `CUDA_ENTRY_CHECK_STRICT`**（`include/cuda-helper.h:119-129`）：
  老驱动上符号不存在时返回 `CUDA_ERROR_NOT_SUPPORTED`，与驱动行为一致；
  **绝不能**用 `CUDA_ENTRY_CHECK`（它的 NULL 兜底会把"符号不存在"伪装成别的错误）
- **PID 纪律**（§4.6.3）：`pid != getpid()` → 纯直通 + 一条 WARNING（说明目标进程的 library 状态无人处理）
- **`cuda_hooks_entry[]` 要加**：这样经 `cuGetProcAddress` 解析的调用者也拿到我们的钩子
- **导出无需改脚本**：`global: cu[A-Z]*;` 已覆盖；但要扩 `hack/check_exported_symbols.sh` 的正向断言
- **验证**：真机上 `cuda-checkpoint --action lock/unlock` 对一个挂着我们库的进程操作——
  注意**这条路走的是私有导出表，不会触发我们的钩子**（§4.6.2），所以钩子验证要用一个
  自己调公开 API 的小程序（照抄 `r580-migration-api.c`）
- **回退**：feature gate 关闭时钩子全部退化为直通

#### 4.7.7 步骤 7：被动纪元检测（restore 侧无缝）

- **文件**：`src/loader.c` 的 `load_necessary_data()` 最前面；`include/hook.h`（`resource_data_t` 加
  `bind_gen` / `incarnation`，用保留位，bump `CONFIG_LAYOUT_VERSION`）；Go 侧 `pkg/config/vgpu` mirror
- **两级检查**见 §4.6.2.1。热路径代价：一次 relaxed load + 一次高度可预测的分支
- **必须同步**：`pkg/config/vgpu/vgpu_config.go` 的结构体镜像与偏移断言，然后跑 `make check`
  （`hack/check_struct_layout.py`），否则 C/Go 两侧会悄悄错位
- **验证**：Go 侧写入 `incarnation++`，C 侧断言下一次 hook 触发 rebind

#### 4.7.8 步骤 8：正交原语 + agent 通道 + CRIU action-script

- 导出 `vgpu_library_quiesce()` / `vgpu_library_resume()`，**手工加进**
  `deploy/libvgpu-control.exports.ld` 的 `global:`（`local: *` 会藏掉，
  `lupinecr_get_lupine_provider_v1` 踩过这个坑）与 `hack/check_exported_symbols.sh`
- 库内控制线程：阻塞在 pipe 上，信号 handler 只 `write()` 一字节；
  **quiesce 完成后控制线程退出**，避免被 CRIU 连同一起 dump；rebind 时重建
- agent 侧新增 gRPC（§6.2）与 CRIU action-script（`pre-dump` / `post-restore`），
  `post-restore` 里**必须**先写 `pids.config` 与新 `vgpu.config`（坑 A）

#### 4.7.9 步骤 9：设备重映射（L1 跨卡）

依赖 spike **S2**（§4.4.3）的结论选 §4.4.4 / §4.4.5 分支；库侧改动已由步骤 1/2/5 铺好
（`bind_gen` 触发 → `rebind_devices()` 重填 → ledger 槽位翻译）。

#### 4.7.10 新增 spike

| # | 验证什么 |
|---|---|
| **S7** | gap `CUevent` 是否需要在 quiesce 里销毁：同 GPU 往返、跨 GPU `--device-map` 往返后事件是否仍可用（决定 §4.6.2.2 的例外要不要开） |
| **S8-a** | ~~目标驱动 `libcuda.so` 是否导出具名 `cuCheckpointProcess*`~~ → **已实机确认导出（2026-09-23）**，钩子方案成立 |
| **S8-b** | args 结构体（`CUcheckpointLockArgs/CheckpointArgs/RestoreArgs/UnlockArgs`、`CUcheckpointGpuPair`、`CUprocessState`）布局是否与 CUDA 13 header 逐字段一致 —— 这些结构**没有 `struct_size` 自描述字段**，抄错即向驱动传垃圾指针 |

## 5. 远程 vGPU（lupine + library）：反而是最容易、最有价值的一条

### 5.1 为什么容易

远程模式下，**CUDA 状态全部在 GPU 节点的 lupine-server 连接子进程里**，应用进程（消费侧 Pod）手里只有
RPC stub。这正是 OrionX 能做热迁移的结构性原因（§2.6）。而且 lupine 上游**已经把迁移脚手架写好了**：

| lupine 现成能力 | 位置 | 对迁移的意义 |
|---|---|---|
| 每连接 fork 一个子进程 | `server.cpp:443`（child `:466`） | **迁移单位 = 连接 = 子进程**，天然隔离 |
| 进程级 CUDA RPC 静默栅栏 | `checkpoint.h`：`lupine_checkpoint_drain_cuda_calls()` / `resume_cuda_calls()`、capture 栅栏 | 检查点前把所有在途 RPC 排空，**这是最难写的部分，已经有了** |
| provider 插件 ABI | `checkpoint_provider.h`：`start/restore/checkpoint/stop` | 迁移的标准挂载点 |
| SIGTERM 触发检查点 | `server_checkpoint.cpp` 的 `wait_for_shutdown`（`'T'`）→ `child_finish()` 里 `drain` + `provider->checkpoint()` | **外部发个信号就能触发迁出**，不需要改 lupine |
| 连接 id = `LUPINE_SESSION` | `h2.cpp:850-857` → `checkpoint_connection_ready` → `provider->restore(id)` | 迁入时按会话找回检查点，语义完全对得上 |
| 我们已占住 provider | `library/src/checkpoint_provider.c`（`checkpoint()` 目前是空实现，`:331`） | **零额外集成成本** |

> `checkpoint_provider.h` 的注释甚至写明了正确的失败语义：*"A missing checkpoint is success; malformed or
> unrestorable state fails the connection rather than allowing it to continue with empty GPU memory."*
> ——这就是我们要的 fail-closed。

### 5.2 L3a：远程同节点跨卡（**零 lupine 改动，客户端零感知**）

流程（全部发生在 GPU 节点内，TCP 连接**自始至终不断**）：

```
控制面选定 目标卡B（同节点，同型号）
  → agent 在 Go 侧账本上【预留】B 的显存/算力配额          ← 正确性前提
  → agent 对该连接子进程 PID: cuCheckpointProcessLock(timeout)
        （在途 RPC handler 自然阻塞在 cu* 调用里，客户端只感知延迟，不报错）
  → cuCheckpointProcessCheckpoint            （A 卡显存清零）
  → Go 侧改写 <session>/config/vgpu.config：device uuid = B，bind_gen++
  → cuCheckpointProcessRestore(gpuPairs = {A→B, 其余卡恒等})
  → cuCheckpointProcessUnlock                （RPC 恢复流动）
  → 释放 A 的配额预留；更新节点元数据/指标
失败任一步 → Restore 回 A + Unlock（自动回退），配额原样归还
```

- **不需要 `drain_cuda_calls()`**：`Lock` 本身就让后续 driver API 阻塞并等已提交工作跑完，效果等价且不用改 lupine；
- 库侧需要 M2（`bind_gen` 重绑）+ M4（watcher 让位）+ M7；
- **这是整套规划里性价比最高的一项**：远程池内的 GPU 碎片整理 / 坏卡腾挪，对客户端完全透明。

### 5.3 L3b：远程跨节点会话迁移（应用 Pod 仍然不动）—— **暂缓（2026-09-23 用户决定：先不推进）**

> 本节保留为背景分析。"不改 lupine 客户端"这条既定约束继续生效，L3b 不进当前路线图。

目标：把会话从 GPU 节点 N1 迁到 N2，消费侧 Pod **不重启、不重调度**。

```
N1: agent 向连接子进程发 SIGTERM
      → lupine child_finish(): drain_cuda_calls() + provider->checkpoint(session)
      → 我们的 provider 把 GPU 状态落到共享存储 <store>/<session>/
N2: 客户端重连（见下）→ server accept → fork child → connection_ready(session)
      → provider->restore(session) 从共享存储恢复 → 第一个 CUDA RPC 前状态就绪
```

**两条实现路线，各有取舍：**

| 路线 | provider 怎么实现 restore | 句柄一致性 | lupine 改动 |
|---|---|---|---|
| **B1 CRIU 服务端子进程** | 在 N1 对子进程做 `cuda-checkpoint lock/checkpoint` + `criu dump`；N2 上 `criu restore --inherit-fd fd[N]:socket:[...]` 把**新 accept 的 socket** 交给恢复出来的进程 | ✅ 天然一致（同一进程镜像，VA/句柄全保留） | 需要"父进程不 fork 而是 criu restore"的分支 |
| **B2 服务端句柄虚拟化** | server 为每个客户端维护 old→new 句柄翻译表，重建资源 | ❌ 要把整个 RPC 面的句柄都虚拟化，工作量巨大 | 极大 |

→ **选 B1**。`cuda-checkpoint/src/r570-features.c:111-119` 的 demo 正是
`criu dump --ext-unix-sk` / `criu restore --inherit-fd fd[N]:socket:[inode]` 这个用法，**上游已验证**。

**唯一真正缺失的能力：lupine 客户端不会重连。** 仓库里 `reconnect/failover` 无命中（只有 lz4 与
`cuda_client_memcpy.cpp` 的无关字样）。所以 L3b 必须：
- 给 lupine-client 加"连接断开 → 按 `LUPINE_SERVER` 列表/控制面下发的新端点重连 → 用同一 `LUPINE_SESSION`
  续接 → 重放未收到响应的那一个在途 RPC"。因为迁出前已 `drain`，**在途请求至多一个**，重放窗口很小；
- 这**违反了我们既定的"不改 lupine 源码"约束**（AGENTS.md §6.3），所以必须显式决策：
  要么作为上游贡献提 PR，要么我们维护一个小 patch。**建议提上游**——重连对 lupine 本身也是刚需。

**替代（不改 lupine 的降级方案）**：把"重连"下沉到 **DNS / 端点切换 + 应用重启**，即跨节点迁移时消费侧
容器需要重启（Pod 不重建）。价值大打折扣，仅作为 L3b 不可行时的兜底。

### 5.4 远程模式的额外好处

L3b 成立后，**消费侧 Pod 本身完全无状态可迁**（它没有 GPU 上下文），于是"整 Pod 跨节点迁移"在远程模式下
退化成"普通无 GPU Pod 的 CRIU 迁移" —— L2 的难度被架构直接消掉。这是远程 vGPU 相对本地 vGPU 的
**结构性优势**，值得写进对外材料。

---

## 6. 控制面设计与项目边界

### 6.1 职责切分（回答"哪些留在 vgpu-manager，哪些独立成项目"）

| 能力 | 归属 | 理由 |
|---|---|---|
| GPU 检查点/恢复原语（L0） | **vgpu-manager** | 必须与 library 的记账/限速状态机联动，外部项目做不了 |
| 迁移期配额预留与账本 | **vgpu-manager** | 分配权威在我们这 |
| 设备重绑定与配置热更新 | **vgpu-manager** | 是我们的 ABI |
| 同节点跨卡迁移编排（L1/L3a） | **vgpu-manager** | 不涉及 Pod 生命周期，放外面反而绕路 |
| 远程会话迁移（L3） | **vgpu-manager**（+ lupine 上游 PR） | 会话是我们的概念 |
| CRIU / kubelet checkpoint / OCI 镜像 / Pod 重建 / 共享存储 | **独立项目** | 与 GPU 无关的通用能力，且有成熟骨架可复用 |
| 迁移策略（何时迁、迁去哪、优先级、预算） | **独立项目 或 调度器扩展** | 策略与机制分离 |

### 6.2 vgpu-manager 对外暴露的契约（这是给独立项目用的唯一接口）

节点 agent（device-plugin DaemonSet 内新增 gRPC service，unix socket + 可选 TCP）：

```proto
service VGPUMigration {
  // 能力探测：驱动版本、是否有 cuda-checkpoint、CRIU 版本、该容器是否可迁移（超卖=否）
  rpc Capabilities(CapabilitiesRequest) returns (CapabilitiesResponse);

  // —— 给外部 Pod 迁移编排器用的两个钩子（L2 契约）——
  // 在 CRIU dump 之前调用：预留目标资源、冻结记账、静默 SM watcher、（可选）lock GPU
  rpc PreCheckpoint(PreCheckpointRequest) returns (PreCheckpointResponse);
  // 在目标节点 CRIU restore 之后、容器恢复运行【之前】调用（CRIU 的 post-restore 阶段，
  // 不是 post-resume）。必须先写 pids.config 再写 vgpu.config(bump incarnation/bind_gen)：
  // pids.config 为空时库会 exit(1)（cuda_hook.c:2328-2350 的刻意 FATAL），见 §4.7.0 坑 A。
  rpc PostRestore(PostRestoreRequest) returns (PostRestoreResponse);

  // —— 我们自己用的细粒度原语（L0/L1/L3a）——
  rpc Lock(LockRequest) returns (LockResponse);            // 含 timeout_ms
  rpc Checkpoint(CheckpointRequest) returns (CheckpointResponse);
  rpc Restore(RestoreRequest) returns (RestoreResponse);   // 含 device_map: repeated {old_uuid,new_uuid}
  rpc Unlock(UnlockRequest) returns (UnlockResponse);
  rpc Abort(AbortRequest) returns (AbortResponse);          // 尽力回滚到源卡
  rpc GetState(GetStateRequest) returns (GetStateResponse);
}
```

**关键设计点：作用单位是"容器"而不是"PID"。** agent 从 `pids.config` 取该容器的全部进程，
按**确定顺序**串行执行（NVIDIA 明确要求 job 内进程顺序处理，见 r610 的 `--launch-job` 说明），
任一进程失败即整体回滚。

> 注意：`PreCheckpoint` **默认不自己调 lock/checkpoint**——让 CRIU 的 cuda 插件去做（§2.2），
> 我们只做资源与配置侧。只有在节点没装 CRIU cuda 插件时才降级为自己调（`self_drive=true`）。
> 这是"最大化复用上游"的具体体现。

### 6.3 CRD 还是注解？

vgpu-manager 目前**零 CRD**（全部走注解 + 节点元数据）。迁移引入状态机、需要重试与可观测，注解会很难受。
建议**引入一个 CRD，且只引入一个**：

```yaml
apiVersion: vgpu.vgpu-manager.io/v1alpha1
kind: VGPUMigration
spec:
  podRef: {namespace, name}
  container: <name>            # 省略 = 全部使用 vGPU 的容器
  mode: InPlaceDevice | RemoteSession | PodMigration
  target:                      # 三种 mode 各取所需
    deviceUUID: GPU-xxxx       # InPlaceDevice
    nodeName: node-b           # RemoteSession / PodMigration
  policy:
    lockTimeoutSeconds: 30
    onFailure: Rollback | Abandon
status:
  phase: Pending|Reserving|Locking|Checkpointing|Restoring|Completed|RolledBack|Failed
  sourceDevice / targetDevice / conditions / timings
```

并照搬 tensor-fusion 的两个建模（§2.5）：
- **设备能力位**：节点/设备注解里增加 `SupportsSnapshot`（由 驱动版本 ∧ cuda-checkpoint 存在 ∧ 未开超卖 决定），
  调度器/迁移控制器直接读；
- **`Migrating` 相位**：迁移期间把源/目标设备置为 `Migrating`，天然把新分配挡在外面
  （比额外造一个"预留"概念更省事，但**预留仍然要做**，因为 `Migrating` 只挡调度器、挡不住同卡已有容器的 `cuMemAlloc`）。

### 6.4 检查点产物（artifact）格式

只有 L2/L3b 需要落盘。借鉴 cudackpt 的工程化（§2.3 末）：manifest（版本号 + 分块索引 + CRC32C）+
环境指纹（library 版本 / 驱动版本 / GPU 型号 / CUDA 版本 / 设备 UUID 列表）+ 保留策略与 GC +
`inspect`/`validate` 子命令。存储后端与 `live-pod-migration` 对齐（RWX PVC，POSIX 语义）。

### 6.5 与 `live-pod-migration` 的对接方式

它已有 PodMigration/PodCheckpoint/ContainerCheckpoint/PodRestore 四个 CRD、特权 DaemonSet agent、
走 kubelet `/checkpoint` API、把产物放 RWX PVC、用"checkpoint tar → OCI 镜像"做恢复（`OVERVIEW.md`）。
我们需要它加的只有两件事：
1. 在 ContainerCheckpoint 之前调我们的 `PreCheckpoint`，在 PodRestore 之后调 `PostRestore`；
2. 目标节点的 Pod 必须**先拿到等价的 vGPU 分配**才能 restore（否则 device-plugin 没跑过、配置文件不存在、
   CRIU 恢复文件映射就会失败）——这要求它的"预拉镜像"阶段扩展为"预分配资源"阶段。

---

## 7. 路线图

> 每个阶段都**独立可交付**，且前一阶段不成立时后一阶段自动降级而不是崩盘。

### P0 —— Spike（不交付功能，2~3 周，全部需要真机 GPU）

| # | 验证什么 | 为什么是阻塞项 |
|---|---|---|
| **S1** | 驱动能力矩阵：r550/570/580 × `cuCheckpointProcess*` × 容器内/外；agent 在宿主 PID 空间对容器进程操作是否可行 | 决定最低驱动基线 |
| **S2** | **目标卡是否必须在 `CUDA_VISIBLE_DEVICES` 内**（§4.4.3）；据此在 §4.4.4 / §4.4.5 两个分支里选一个；r580 "container partial passthrough" 的确切语义 | **决定 L1 的形态，是阻塞性未知** |
| **S3** | **library 在场时 cuda-checkpoint 是否正常**：`LD_PRELOAD` 干扰、`dlsym` 拦截器、watcher 线程、mmap 共享区、`cuGetProcAddress` hook | **最高优先级**；这条不过，后面全免谈 |
| **S4** | CRIU dump/restore 一个带 library 的 GPU 容器（先同节点原地）：共享区 mmap 恢复、`flock` 状态、`/tmp/.sm_node` 行为 | 决定 L2 可行性 |
| **S5** | lupine-server 连接子进程做 in-process 跨卡迁移（L3a 全流程） | 决定 L3a 可行性 |
| **S6** | 典型负载的停顿时间实测：PyTorch 训练 / vLLM 推理，不同显存规模 | 决定是否需要 P5 数据面加速 |
| **S7** | gap `CUevent` 是否需在 quiesce 里销毁（同卡 / 跨卡 `--device-map` 往返后是否仍可用） | 决定 §4.6.2.2 的"零 CUDA 调用"能否无例外成立。**已有旁证**：GPU-CR 明确 `ipc_teardown_all_events()` 在检查点前拆掉 IPC event、并 `ipc_disable_all_peer_access()`（`src/ipc_hooks.h:167-185`），说明事件/P2P 状态确实有风险 |
| **S9** | **能力探针**（照搬 mncr `verify/vmm_probe.cu`）：在目标驱动上逐一试 `cuMemAlloc` / VMM 持有 / VMM+导出 / VMM 导入 / managed / `cuIpcGetMemHandle` 的 C/R，产出**本节点的**能力矩阵 | 取代静态表；GPU-CR(580) 与 mncr(595) 结论相反（§2.7.4） |
| **S10** | `--device-map` 的 **PATH shim** 在我们的部署形态下是否可用（criu 由 runc 拉起，PATH 由谁决定；我们的 agent 能否在那个 PATH 前插目录） | 决定 L2 跨卡恢复可行性（§2.7.5 坑 7） |
| **S11** | 我们自己的 `libvgpu-control.so` 在两节点间的 **build-id/mode 一致性**：同版本 helm 部署出来的两台机器，CRIU 是否接受（含 `.<version>` 后缀路径与 ReadOnly 挂载） | 决定跨节点迁移的版本约束能否落地（§2.7.5 坑 6） |
| ~~**S8**~~ | 目标驱动 `libcuda.so` 导出具名 `cuCheckpointProcess*` | **已实机确认（2026-09-23）**：具名符号存在，劫持方案成立。剩余的结构体布局核对转为 S8-b（§4.7.10） |

### P1 —— L0：GPU C/R 原语 + library 兼容（交付："被 vgpu-manager 管理的 GPU 容器可以被安全挂起与恢复"）
- 库：M1、M3、M4、M6、M7；Go：agent gRPC（Lock/Checkpoint/Restore/Unlock/Abort/GetState/Capabilities）+ 配额预留；
- gate `GPUCheckpointRestore=false` 默认关闭；
- **独立价值**：即使不做迁移，这已经是"**显存让渡/抢占**"能力——低优先级任务挂起让出整卡显存给高优先级任务，
  跑完再恢复。对超卖场景价值极大，且是 GPU-CR 那篇论文的主打场景。
- 验收：容器内 vLLM 与 PyTorch 训练进程经 lock→checkpoint→（等待 N 秒，期间别的容器跑满该卡）→restore→unlock
  后结果正确；限额仍生效；预留未被吃掉；失败路径能回滚。

### P2 —— L1：本地 vGPU 同节点跨卡热迁移
- 依赖 S2 结论；库补 M2；Go 补迁移控制器 + `VGPUMigration` CRD + 设备 `Migrating` 相位；
- 验收：`nvidia-smi` 上进程换卡、业务只感知一次停顿、限额跟随新卡、源卡配额归还、失败自动回退。

### P3 —— L3a：远程 vGPU 同节点跨卡（L3b 暂缓）
- L3a 先做（零 lupine 改动，复用 P1/P2 的 agent，目标进程换成 lupine-server 子进程）；
- ~~L3b~~ **暂缓**（不改 lupine 客户端）。若将来解冻，provider 的 `checkpoint()`/`restore()` 填上真实实现
  （`library/src/checkpoint_provider.c`）。

### P4 —— L2：整 Pod 跨节点迁移（与独立项目对接）
- 我们只交付 `PreCheckpoint`/`PostRestore` 两个 RPC + 目标节点资源预分配保证；
- 优先让 CRIU 上游 cuda 插件承担 GPU 侧；没有插件的节点降级为 `self_drive`；
- 独立项目建议直接 fork/改造 `live-pod-migration`。

### P5（可选）—— GPU-CR 式数据面加速
- 仅当 S6 显示停顿时间不可接受时才做；默认关闭；复用已有 `cuMemAlloc`/VMM 入口；
- 必须先解决与预算门、NVML 真实口径记账、UVA 超卖账本的三重耦合。

---

## 8. 风险与限制（必须写进文档与准入校验）

| 风险 | 说明 | 缓解 |
|---|---|---|
| **超卖与迁移互斥** | 驱动不支持 UVM 内存的检查点 | 准入期判定不可迁移，`SupportsSnapshot=false` |
| **NCCL / 多卡分布式** | CRIU cuda 集成不支持 NCCL；挂起会摧毁 communicator | 首期只支持单卡单进程；多卡列为后续（参考 GPU-CR 的 `multi_cr_client` 分阶段编排 + NCCL adapter 思路） |
| **CUDA IPC / MPS** | r610 之前 IPC 不支持；MPS 未验证 | 准入拒绝 |
| **停顿时间随显存线性增长** | 驱动串行拷贝；OrionX 实测同节点 ~16s | S6 实测；必要时 P5 |
| **检查点期间主机内存暴涨** | 驱动把显存拷进**进程的主机内存** → 容器 memory limit 可能 OOM Kill | 准入校验 `container.memory.limit ≥ vGPU 显存 + 余量`；或 P5 改走 hugepage staging |
| **迁移窗口内配额被抢** | NVML `used` 归零 | §4.1 M3 + Go 侧预留（**正确性前提**） |
| **驱动/库/型号不一致** | 跨节点恢复失败且进程不可用 | M8 环境指纹校验，fail-closed |
| **设备隔离降级** | §4.4.4 分支一把 cgroup 强制降为库强制（仅当 S2 判定目标卡必须常驻可见时才走） | Pod 级 opt-in + webhook 拒绝用户自设 CVD + 文档明示；优先争取 §4.4.5 分支二 |
| **长 kernel 阻塞 lock** | lock 要等已提交工作跑完 | lock 带 timeout，超时即放弃并 unlock（CRIU 插件也是这么做的） |
| ~~改 lupine 违反既定约束~~ | L3b 需要客户端重连 | **已决策：L3b 暂缓，不改 lupine 客户端**（2026-09-23） |
| **kubelet 只能 checkpoint 不能 restore** | 上游现状 | 恢复走 OCI 镜像技巧，由独立项目承担 |

---

## 9. 待验证清单（spike 之外的开放问题）

1. （已升格为 S2，见 §4.4.3）`cuCheckpointProcessRestore` 的 `gpuPairs` 与 `CUDA_VISIBLE_DEVICES` 的关系：
   NVIDIA 两个 demo 都先 `unsetenv("CUDA_VISIBLE_DEVICES")`，目标卡能否在可见集之外是本方案的核心未知。
2. CRIU 恢复后 `flock`/`fcntl` 锁状态是否保留（影响 `src/lock.c` 与 registry 的 `pids.config` 写入）。
3. CRIU cuda 插件在**目标 GPU UUID 与源不同**时的实际表现（插件无 device map，但容器设备注入可能已经把
   序号对齐了）—— 决定 L2 跨节点是否需要我们额外介入。
4. lupine 连接子进程被 `criu dump` 时，HTTP/2 会话层状态（`h2.cpp`）是否需要额外处理，`--inherit-fd` 之后
   对端序列号能否直接续上（大概率需要客户端重放最后一个请求）。
5. `cuda-checkpoint` 二进制的分发与许可（NVIDIA 专有许可）—— 能否随我们的镜像分发，还是要求运维自备。
6. MIG 模式下的检查点/恢复行为。
7. 我们的 `dlsym` 拦截器（`loader.c:2167-2210`）是否会影响 CRIU cuda 插件 fork 出的 `cuda-checkpoint` 子进程
   （它不该继承我们的 `LD_PRELOAD`，但 `/etc/ld.so.preload` 部署形态下会）—— **这条要单独测**，
   `/etc/ld.so.preload` 是全局生效的。

---

## 10. 对用户三个核心问题的直接回答

**Q1：library 要不要合并 cudackpt / GPU-CR 的能力？**
不合并。库的角色是"**迁移感知的旁路**"：给驱动的 C/R 让路，并在迁移前后维护自己的设备绑定、记账、限速状态
（§4.1 的 M1–M8）。cudackpt 的重建式 C/R 原理不健全且与我们的 hook 正面冲突（§2.3）；GPU-CR 的数据面加速
理念正确、且我们已有 VMM 入口，但它要接管分配语义，与预算门/记账/超卖三重耦合，**后置到 P5 且默认关闭**。

**Q2：本地 vGPU 的 library 需要改造吗？**
需要，而且是**必要条件而非可选项**。最关键的三条：设备绑定代次（M2，`pthread_once` 必须变可重入，
否则跨卡迁移后仍按旧卡记账限速）、迁移窗口记账语义（M3，否则配额被抢、恢复必然 OOM 且进程不可恢复）、
恢复后缓存失效钩子（M6）。

**Q3：远程 vGPU（lupine + library）能不能好好支持？**
不仅能，而且**比本地更容易、价值更大**。lupine 上游已经把最难的静默栅栏与 provider ABI 写好，我们还已经
占住了 provider。**L3a（远程同节点跨卡）零 lupine 改动、客户端零感知**，应该优先做，且它不受 §4.4 那套
设备可见性难题影响（§4.4.6）。L3b（跨节点）唯一的真缺口是 lupine 客户端不会重连，**已决定暂缓、不改 lupine**。
更进一步：远程模式下消费侧 Pod 本身无 GPU 状态，
