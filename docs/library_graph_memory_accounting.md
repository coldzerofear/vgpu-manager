# CUDA Graph 显存记账（library）

本文记录 `libvgpu-control.so` 对 CUDA Graph 相关显存的限额检查与记账：设计依据、真机实测的驱动行为、测试结果与已知边界。

## 1. 记账口径回顾

显存限额检查 = NVML 报告的进程实际占用 + 本进程"尚未被 NVML 看到"的在途记账（共享账本 `/tmp/.vmem_node`）。
在途记账有两类：

| 类型 | 何时记 | 何时释放 |
|---|---|---|
| `MEMORY_TYPE_ASYNC_BRIDGE` | 非捕获的 `cuMemAllocAsync` / `cuMemAllocFromPoolAsync` 调用后 | 同步该 stream 后（NVML 已可见） |
| `MEMORY_TYPE_CAPTURE` | 捕获期间的异步分配 | 捕获结束（成功或被作废）、graph 销毁、按指针释放、所属 context 销毁/重置、fork 子进程作废、进程退出清理 |

launch 之后显存由 NVML 计入，记账若继续持有会重复计数（误报 OOM），所以记账只覆盖"NVML 还看不到"的那段时间。

## 2. 真机实测的驱动行为（RTX 3080，驱动 595.84，CUDA 13.0，不加载本库）

探针程序直接调用驱动 API，逐步读取 NVML 进程占用和 `CU_GRAPH_MEM_ATTR_RESERVED_MEM_CURRENT`。

| 编号 | 问题 | 实测结果 |
|---|---|---|
| P1 | graph alloc node（1 GiB）何时被 NVML 看到 | `cuGraphAddMemAllocNode` 后 216 MiB（基线）；instantiate 后 216；**`cuGraphUpload` 后 1240**；launch 后 1240 |
| P1b | 不 upload 直接 launch | **launch 调用返回时（尚未同步）即 1240**，映射在 launch 调用内完成 |
| P1c | graph、exec 都销毁之后 | 仍 1240（graph 池保留物理页），`cuDeviceGraphMemTrim` 后回到 216 |
| P8 | 含内存节点的 graph 二次 instantiate | `CUDA_ERROR_NOT_SUPPORTED`：graph 与 exec 一对一 |
| P2 | 向捕获中的 graph 显式加 alloc node | `cuStreamBeginCaptureToGraph` 的 graph 与经 `cuStreamGetCaptureInfo` 拿到的捕获内部 graph **都允许** |
| P3 | 捕获进行中 instantiate 该 graph（`BeginCaptureToGraph`） | **成功**（与文档"捕获结束前不可 instantiate"不符） |
| P4 | 捕获被作废后 | `cuStreamGetCaptureInfo` 返回状态 2（INVALIDATED）、id=0，**graph 输出参数不写入**；`cuStreamEndCapture` 返回 `CUDA_ERROR_STREAM_CAPTURE_INVALIDATED`，graph 为 NULL |
| P9 | 经 event 接入捕获的 stream | 与发起 stream 的 capture id、graph 相同 |
| P10 | 接入的 stream 上做非法操作 | 整个捕获被作废，发起 stream 上 `EndCapture` 返回 INVALIDATED |
| P5 | primary context 状态 | Reset 后立即 `active=0`（即使仍持有引用）；最后一次 Release 后 `active=0`；重新 retain 后句柄值与之前相同 |
| P6 | 捕获未结束时 `cuCtxDestroy` | 成功 |

推论：
- alloc node 本身不分配物理显存，非捕获期间调用后 NVML 看不到；要等 upload/launch。
- 捕获被作废后无法再从驱动拿到 graph，必须在捕获**开始**时记下 graph，否则这次捕获里的记账永远没有释放键（main 上原有泄漏）。
- primary context 的句柄必须在 Reset/Release **之前**获取，之后它已是非活跃状态。

## 3. 实现

- **`cuGraphAddMemAllocNode`**（新 hook）：按 `poolProps.location`（device 类型）取设备，像 `cuMemAllocFromPoolAsync` 一样先做限额检查，超限返回 OOM。
  不记账（含加入捕获中 graph 的 alloc node，2026-11-04 起；见 6.5）：物理显存在 upload/launch 时映射，由那里的准入检查按整个 graph 检查。
- **捕获登记表**：hook `cuStreamBeginCapture` / `_v2` / `_ptsz` / `_v2_ptsz` / `cuStreamBeginCaptureToGraph` / `_ptsz`，开始捕获成功后记下
  "发起 stream → graph"（默认 per-thread stream 再按线程区分），64 个槽位，满了退回到结束时查询。`cuStreamBeginCaptureToGraph(_ptsz)` 原先不在驱动函数表里，现已加入。
  `cuStreamBeginCapture` 属 ABI 冲突族，已按先例加入 `R5_ALLOWLIST`（每个包装都调用自己同名的入口）。
- **`cuStreamEndCapture`**：graph 取自登记表（缺失时现场查询），在返回 `CUDA_SUCCESS`、`CUDA_ERROR_STREAM_CAPTURE_INVALIDATED`，
  或 End 失败但 stream 已不在捕获中（跨线程的 `WRONG_THREAD`、未汇合的 `UNJOINED`，驱动拒绝的同时已终止捕获，见 6.6）时释放；捕获还在（接入的 stream 上 End 的 `UNMATCHED`）不释放。
- **异步分配的设备**：`cuMemAllocAsync` 按 stream 所在的卡、`cuMemAllocFromPoolAsync` 按池所在的卡检查和记账，都不用当前 context（见 6.6、6.7）。
- **`cuStreamDestroy`**（新 hook）：销毁正在发起捕获的 stream 时释放该捕获的记账（见 6.7）。
- **context 拆除兜底**：记账记录增加 `ctx` 字段（进程私有，不涉及共享 ABI）。`cuCtxDestroy(_v2)`、`cuDevicePrimaryCtxReset(_v2)`、
  释放最后一个引用的 `cuDevicePrimaryCtxRelease(_v2)` 成功后按 context 清理记账与登记表。这 6 个入口从透传改为注册进 hook 表，
  `cudaDeviceReset` 等经 `cuGetProcAddress` 获取的调用也能覆盖。primary context 只在活跃时获取句柄（对非活跃的 retain 会创建它）。
- fork 子进程清空登记表并重新初始化其锁。

## 4. 测试

GPU 用例在 `library/test/test_alloc_async_accounting.c`（同源编译出 `_ptsz` 变体），直接读共享账本断言：

| 用例 | 内容 |
|---|---|
| [E] | 手工 graph：超限 alloc node 返回 OOM；限额内成功且不记账 |
| [F] | 加入捕获中 graph 的 alloc node：超限返回 OOM（捕获不受影响），限额内成功且不记账（2026-11-04 前为"记账、捕获结束释放"，见 6.5） |
| [G] | 捕获被作废：记账在 `EndCapture` 返回 INVALIDATED 后释放 |
| [H] | 捕获未结束即 `cuCtxDestroy`：记账释放 |
| [I] | 捕获未结束即 `cuDevicePrimaryCtxReset`：记账释放 |
| [J] | 捕获未结束即释放 primary context 最后一个引用：记账释放 |
| [K] | 分配在经 event 接入的 stream 上、在那里作废捕获，于发起 stream 上结束：记账释放 |
| [L] | `cuStreamBeginCaptureToGraph` 的捕获被作废：记账释放（作废后驱动连应用自己的 graph 一起销毁，再 `cuGraphDestroy` 返回 INVALID_VALUE，长时间运行的进程里会在驱动内死锁，所以测试不再销毁它） |

runtime 路径另有 `library/test/test_runtime_capture_accounting.cu`（runtime 经 `cuGetProcAddress` 取驱动入口，验证新 hook 的路由）：
[R1] `cudaStreamBeginCapture` + `cudaMallocAsync` 记账并在结束时释放；[R2] 作废的捕获释放；[R3] 捕获未结束时 `cudaDeviceReset` 释放。

RTX 3080 实测（限额 2048 MiB，每次 457 MiB），同一测试二进制，仅替换库：

| 用例 | 修正前（14e9131："仅成功时释放"，primary context 句柄在调用后获取） | 本次实现 |
|---|---|---|
| [E] | 通过 | 通过 |
| [F] | 失败：捕获中加的 node 未记账 | 通过 |
| [G] | 失败：作废后残留 457 MiB | 通过 |
| [H] | 受前序残留影响读数错乱 | 通过 |
| [I] | 失败：Reset 后残留 457 MiB | 通过 |
| [J] | 受前序残留影响读数错乱 | 通过 |
| [K] [L] | 失败（残留或读数错乱） | 通过 |
| [R1] | 通过 | 通过 |
| [R2] | 失败：作废后残留 403 MiB | 通过 |
| [R3] | 受前序残留影响读数错乱 | 通过 |

全量：`make test` 20 项无 GPU + 27 项 GPU 全部通过，普通与 `_ptsz` 两种编译都通过。
`python/limit_pytorch.py`（安装 PyTorch 2.14.1+cu130 后）失败是**既有的参数问题**：默认张量 1024³ float32 = 4 GiB，
而测试环境限额 `CUDA_MEM_LIMIT=2048m`，OOM 正是限额生效；改用 256×1024×1024（1 GiB）在修正前后两版都得到正确结果。脚本另需 numpy。

PyTorch 真实场景（`PYTORCH_CUDA_ALLOC_CONF=backend:cudaMallocAsync`，限额 2048 MiB）：

| 场景 | 不加载本库 | 14e9131 | 本次实现 |
|---|---|---|---|
| T1 capture 内分配临时张量 + replay（HAMi-core #387 的症状） | 正常 | 正常 | 正常，结果正确 |
| T2 先捕获 12 个 graph（每个 256 MiB 临时张量，合计 3072 MiB）再全部 replay | 正常 | 正常 | 正常，无误报 OOM，实际占用 1356 MiB |
| T3 capture 内申请超出限额 | 未拒（无限额） | 拒绝 | 拒绝，之后进程正常 |

T2 也是 6.1 节"不能把记账持有到首次 launch"的实证：持有的话 3072 MiB 的记账会在 2048 MiB 限额下误报 OOM。

## 5. 已知边界

- **空窗 B**（捕获结束/手工加完节点到首次 upload/launch 之间 graph 显存不在记账也不在 NVML）：由 upload/launch 时的准入检查覆盖（第 6 节），
  多 stream 并发 launch、graph 内依赖无关的并行分支与分配不释放的 graph 曾经会低估，6.15 已按驱动规则修正；其中不释放的块是可叠加的绕过。
- 手工 graph 的 alloc node 在创建时各自检查、不累加；upload/launch 时按整个 graph 的总量（扣除池已预留）再检查一次。
- alloc node 跨多个设备的 graph 逐卡检查、每张卡检查完立即放锁（6.5）：检查到 launch 之间别的进程若在已检查的卡上分配，会低估（仅多卡 graph 的首次映射）。
- 捕获登记表可增长（6.9），只有内存耗尽写不进去时才退回到结束时查询，此时被作废的捕获仍会遗留记账，直到 context 拆除或进程退出，捕获内的异步分配也按当前 context 查卡。接入捕获的 stream 同样按当前 context 查卡（6.6）。
- 导入的池、host 位置的池按当前 context 查卡（6.7）。
- `cuDevicePrimaryCtxRelease` 的"查状态 → retain"之间若另一线程释放了最后一个引用，retain 会重新创建 primary context，随即被撤销的 release 销毁；记账结果不受影响。

## 6. 空窗 B：研究与实现

### 6.1 原方案（用 event 把记账交接给 NVML）被实测否定

原设想：捕获结束后不释放记账，instantiate 时转挂到 exec，首次 launch/upload 后 record 一个 event，event 完成时释放。
RTX 3080 实测（不加载本库，`bprobe`）否定了它的两个前提：

| 编号 | 场景 | 实测 |
|---|---|---|
| Q1 | 捕获的 1 GiB alloc+free graph，launch 到被 host 回调卡住的 stream 上 | launch 调用返回时 NVML 216→1240 MiB、池预留 0→1024，此时 graph 还没开始执行 |
| Q2 | 同样卡住的 stream 上 `cuGraphUpload` | upload 返回时即 1240 MiB |
| Q3 | 两个 1 GiB alloc+free graph 在同一 stream 上先后 launch | 池预留只有 1024 MiB（复用） |
| Q3b | 同样两个 graph 在两个 stream 上并发 launch | 池预留 2048 MiB（不复用） |
| Q4 | 两个无 free node 的 1 GiB graph 先后 launch | 2048 MiB（显存一直被占着，不能复用） |
| Q5 | 先捕获 8 个 1 GiB alloc+free graph，再依次 launch | 物理显存只增加 1 GiB |

- 映射发生在 launch/upload **调用内部**（Q1/Q2），调用返回即对 NVML 可见，不需要 event。
- 在首次 launch 之前持有记账会**误报 OOM**（Q5）：vLLM 等先捕获几十个 graph 再回放的用法，记账总和远大于实际物理占用
  （8 个 graph 记 8 GiB，实际 1 GiB），在 4 GiB 限额下第 5 个捕获就会被拒。

### 6.2 改进方向：launch/upload 时准入检查

不在 launch 之前持有任何记账，改为在显存真正被映射的那一刻检查：
1. instantiate 时（已有的 `capture_graph_cost` 遍历节点）顺带统计该 exec 各设备的 alloc node 总量 `need`。
2. `cuGraphLaunch`/`cuGraphUpload` 前，对 `need > 0` 的 exec 估算池增长 `growth = max(0, need − 池已预留)`（`CU_GRAPH_MEM_ATTR_RESERVED_MEM_CURRENT`）。
3. `growth > 0` 时在设备锁下按 `growth` 做限额检查，超限返回 `CUDA_ERROR_OUT_OF_MEMORY` 不执行 launch；否则 launch，返回后释放锁（NVML 已可见）。
4. `growth == 0`（稳态回放）跳过检查，热路径只多一次属性查询。

估算准确度实测（`bprobe2`，每行：需求 | 预留变化 | 实际增长 / 估算）：

| 场景 | 实际增长 | 估算 | |
|---|---|---|---|
| 首次 1 GiB | 1024 | 1024 | 准确 |
| 已预留 1 GiB 后 2 GiB | 1024 | 1024 | 准确 |
| 512 MiB、另一个 2 GiB、回放 1 GiB/2 GiB | 0 | 0 | 准确 |
| 同一 stream 连续 launch 不同步 | 0 | 0 | 准确 |
| trim 之后 1 GiB | 1024 | 1024 | 准确 |
| 两个 stream 并发 2 GiB（第二个） | 2048 | 0 | **低估** |
| 无 free node 的第二个 1 GiB graph | 1024 | 0 | **低估** |

驱动的 `CU_GRAPH_MEM_ATTR_USED_MEM_CURRENT` 始终等于预留量，不能用来区分池里哪些显存还被占着，所以后两种低估无法靠属性修正。

### 6.3 实现（2026-11-03，已采纳："超限时 launch 返回 OOM"语义；两种低估作为已知边界）

- graph 成本缓存（按 exec，instantiate / `cuGraphExecUpdate` 时遍历节点填充，`cuGraphExecDestroy` 与 fork 子进程清空）
  新增每设备的 alloc node 总量 `mem_need`；遍历时识别 `CU_GRAPH_NODE_TYPE_MEM_ALLOC`（含子 graph）。
- `graph_mem_admit()`：对 `need > 0` 的设备查 `CU_GRAPH_MEM_ATTR_RESERVED_MEM_CURRENT`，增长 `need − reserved > 0` 时
  用 `prepare_memory_allocation` 在设备锁下检查，超限返回 `CUDA_ERROR_OUT_OF_MEMORY`；多设备按序号加锁。
- 调用点：`cuGraphLaunch(_ptsz)`、`cuGraphUpload(_ptsz)`（新 hook）、带 `CUDA_GRAPH_INSTANTIATE_FLAG_UPLOAD` 的
  `cuGraphInstantiateWithParams(_ptsz)`（实例化前对源 graph 遍历；拒绝时 `result_out = CUDA_GRAPH_INSTANTIATE_ERROR`）。
- 顺序：launch 先限流（`rate_limiter` 可能睡眠），再加锁检查，launch 返回后立即放锁，再走 `gap_end`（可能同步、睡眠）。
- 不写任何账本记录：捕获期间记账、捕获结束释放的原有语义不变；稳态 replay 估算增长为 0 直接跳过。
- 已知边界：多 stream 并发 launch、分配不释放的 graph（见 6.2 表）；成本缓存满（256 个存活 exec）时新 exec 不检查。

### 6.4 测试数据

`library/test/test_graph_mem_admission.c`（同源 `_ptsz` 变体），RTX 3080，限额 4096 MiB：

| 用例 | 7b36d05（无准入） | 本次 |
|---|---|---|
| [A] 捕获后普通分配占掉余量，launch | 成功，越限映射 1220 MiB | 返回 OOM、池未增长；释放后成功 |
| [B] 已映射 graph 在余量耗尽时连续 replay 50 次 | 通过 | 通过（无误拒） |
| [C] `cuGraphUpload` 同上 | 越限（池 1248 MiB） | OOM / 释放后成功 |
| [D] `InstantiateWithParams` + UPLOAD | 越限 | OOM 且 `result_out=ERROR` / 释放后成功 |
| [E] 手工 alloc node graph 的 launch | 越限 | OOM / 释放后成功 |
| [F] 池已有 small，再 launch 2×small 的 graph | 增长放得下与放不下都成功 | 放得下成功、放不下 OOM（只按增长检查） |

PyTorch（`library/test/python/limit_pytorch_graph.py`，`backend:cudaMallocAsync`，每项独立子进程）：

| 项 | 7b36d05 | 本次 |
|---|---|---|
| T1 capture + replay | 通过 | 通过 |
| T2 24 个 graph（临时张量合计 6144 MiB，限额 4096 MiB）先捕获后 replay | 通过 | 通过（无误报） |
| T4 捕获后其他分配占掉余量再 replay | **越限映射 1219 MiB** | replay 被拒；`empty_cache()` 后 replay 成功 |

PyTorch 侧的观察（与本库无关，不加载本库同样发生）：
- capture 内分配失败后 PyTorch 无法恢复：捕获作废，析构里 `cuMemFreeAsync` 报 INVALIDATED 后 `terminate`。所以 Python 测试不测
  "capture 内超限"（C 测试 [D] 覆盖）。
- 首次捕获前必须先在捕获外跑一次同样的操作（PyTorch 要求的 warm-up），否则懒初始化发生在捕获内使其作废。
- cudaMallocAsync 后端 `del` 张量后显存留在 PyTorch 的池里（驱动默认内存池，PyTorch 设为不自动释放），NVML 仍计入；
  graph 分配来自驱动的 graph 内存池，两池不能互用。所以释放张量后、graph replay 需要新显存时，要先 `torch.cuda.empty_cache()`
  把显存还给驱动。**这是 PyTorch 在真实硬件上的固有行为，不是本库引入的**——同一段代码（`ec.py`：占满显存 → `del` → replay）：

  | 环境 | `del` 后直接 replay | `empty_cache()` 后 replay |
  |---|---|---|
  | 不加载本库，真实 10 GiB GPU（`del` 后设备空闲 1534 MiB，PyTorch 预留 7776 MiB，graph 需 3122 MiB） | OOM | 成功 |
  | 加载本库，限额 4096 MiB（空闲 595 MiB，预留 3008 MiB，graph 需 1219 MiB） | OOM | 成功 |

  本库让一块 4 GiB 的 vGPU 与一块真实的 4 GiB GPU 表现一致；main 上同样的 replay 会静默越限（实测 1219 MiB）。
  只涉及显式启用 `cudaMallocAsync` 后端、在捕获内分配临时张量、且首次 replay（或 Trim 后）余量不足的组合；
  默认的原生缓存分配器在捕获期间分到的是真实 `cudaMalloc` 显存（NVML 立即可见），不产生 graph 分配节点，不受影响。
- uint8 张量 `.sum()` 先转 int64，会产生 8 倍临时张量。

并发与 fork（`test_alloc_async_accounting.c`）：[M] 8 线程 × 30 次捕获（1/3 作废）账本归零；[N] 同时 80 个捕获（超过登记表 64 槽）
全部释放；[O] 两线程同时在 per-thread 默认 stream 上捕获（一个作废）各自释放；[P] 后台线程持续捕获时 fork 40 次，子进程全部 10 s 内退出。

内存安全：ASan + UBSan 构建（因 ASan 初始化走 `dlsym`，测试构建不导出本库 `dlsym`；runtime 测试因此不经过本库，只作冒烟）跑全部 GPU 测试
零报告；`make test-sanitize`（无 GPU）20/20 通过。另发现 `VGPU_SANITIZE` CMake 选项不作用于 `libvgpu-control.so` 本身
（`add_compile_options` 位于目标定义之后），已在 6.11 修复。

热路径开销（`lbench`：带 1 MiB alloc+free 的 graph 稳态 replay 20000 次，10 轮中位数）：不加载本库 2.61 µs/次，
7b36d05 2.59 µs，本次 2.59 µs，准入检查无可测开销。

既有用例 [A]（在途异步分配必须记账）在 8 核满载下偶发失败：观察线程在 stream 被卡住后固定 sleep 50 ms 读一次账本，
而分配线程此时可能还在 hook 里记账之前（NVML 查询、设备锁）。交替对比 14e9131 与本次各 30 次：4/30 与 3/30，同为既有时序问题。
改为在 stream 被卡住期间轮询账本（最长 2 s）直到记账出现；满载下普通与 `_ptsz` 各 30 次 0 失败。

测试环境：`run_tests_with_env.sh` 限额 2048m → 4096m；框架脚本默认 4 GiB 张量加上 context 自身约 216 MiB 在 4096 MiB 下必然 OOM，
`run_all_tests.sh` 改为传 `--tensor_shape 512,1024,1024`（2 GiB，可用 `VGPU_TEST_TENSOR_SHAPE` 覆盖）。

### 6.5 简化：单设备准入（2026-11-04，分支 fix/graph-mem-admit-simplify）

起因：6.3 的实现对每个 exec 维护 `mem_need[16]`，launch 时遍历 16 个设备、按序号加多把设备锁，复杂度和多卡出错面都偏大。
核对官方文档：`The graph's nodes must all reside on a single device` 只约束条件节点的 body graph（§4.2.4.2）和设备端 launch 的
graph（§4.2.6.1.1），普通 graph 可以跨设备（kernel/memcpy 节点各带 context、跨设备 event、对端驻留的分配 §4.2.5.6），
所以不能假设单设备，也不能用 `cuCtxGetDevice` 代替分配所在设备。最终实现：

1. 每个 exec 只记**一个驻留设备 + 字节数**（来自 alloc node 的 `poolProps.location.id`，即 CUDA 序号，与 `cuMemAlloc` 的设备同一编号空间，
   无漂移）。alloc node 跨了多个设备时标记"多设备、不检查"，进程内只告警一次——已知边界，退回 main 之前的行为：不报错不卡死，之后 NVML 计入超出部分。
2. 准入只检查这一张卡、只拿一把设备锁（与 `cuMemAlloc` 经 `prepare_memory_allocation` 的模式相同）。
   每次 upload/launch 查一次池预留（~10 ns）；稳态回放估算增长为 0，不拿锁；不带 alloc node 的 graph 只判一次设备号即返回。
3. `cuGraphAddMemAllocNode` 去掉捕获期记账分支（只检查不记账），删除 `capture_reg_has_graph`；捕获登记表保留（作废捕获的泄漏修复依赖它）。

曾实现并否决的"方案 B"（只在首次 launch/upload 检查，之后凭"已映射"标记跳过，hook `cuDeviceGraphMemTrim` 清标记；提交 805fb48）：
驱动会在物理显存不足时**自行回收**空闲的 graph 池（见下），没有任何可 hook 的通知，"已映射就跳过"会漏检；而它省下的只是每次 launch ~10 ns
（一次 graph launch 约 2 µs，约 0.5%）。所以撤掉，`cuDeviceGraphMemTrim` 恢复透传。

检查顺序保持"先限流、再检查、launch、放锁"：检查超限要持跨进程设备锁直到 launch 返回（别的进程分配时才能在 NVML 看到这次映射），
而 `rate_limiter` 可能睡眠。若把检查挪到限流前，要么持锁睡眠（同卡所有进程的分配被卡住整个限流时长），要么检查后放锁再睡
（睡眠期间别的进程可分走余量，launch 照样越限映射，即空窗 B 换了个位置）。限流之后的检查在稳态只是一次 ~10 ns 查询，挪位置不减少开销。

#### 新发现：驱动会在物理显存不足时自行回收空闲的 graph 池（不经 trim）

不加载本库的探针（`probe_pool`，RTX 2080 Ti）：graph launch 后池预留 3552 MiB、设备空闲 7104 MiB，再 `cuMemAlloc(7104 + 1776)` **成功**，
池预留随即变为 0；之后再 launch 该 graph 驱动重新映射（显存不够时返回 OOM，并残留 1760 MiB 的部分映射）。`cuMemAllocAsync` 同样超出时
不回收（返回 OOM）。即"池里的页一直保留到 trim"只在没有物理显存压力时成立。

端到端复现（`reclaim.sh`：本库 SESSION 记账，只统计 A 自己的进程；另一个不加载本库的进程 B 占满物理显存只留 600 MiB；限额 4096 MiB）：

| 步骤 | 方案 B（805fb48） | main | 最终实现 |
|---|---|---|---|
| A1 graph launch，池预留 | 1216 MiB | 1216 MiB | 1216 MiB |
| A2 物理显存紧张时 `cuMemAlloc(1200 MiB)` | 成功，驱动回收池 → 0 | 同左 | 同左 |
| A3 B 释放后 A 分配到限额 | 剩余 64 MiB | 剩余 64 MiB | 剩余 64 MiB |
| A4 再 launch 该 exec | **跳过检查，成功并重新映射 1216 MiB** | 返回 OOM | 返回 OOM |
| A 的实际占用 | **5248 MiB，越限 1152 MiB** | 4032 MiB | 4032 MiB |

触发条件是同卡其他租户把物理显存用满（或超卖）；单租户且限额 ≤ 物理显存时不会发生（物理显存先于限额耗尽）。

复现注意：裸机上 `/etc/vgpu-manager/config/vgpu.config` 存在时优先于环境变量；HOST 模式会把同卡所有进程（含 B）计入；
CGROUP 模式在裸机 session scope 下识别不到本进程（占用为 0），都不能用来做这类多租户实验，要用 SESSION 模式（`MANAGER_COMPATIBILITY_MODE=300`）
+ `/etc/vgpu-manager/config/pids.config`。

#### 测试数据（RTX 2080 Ti 11 GiB，驱动 595.84，CUDA 13.0 工具链，限额 4096 MiB）

- 静态检查：hook 一致性 PASS（604 项，hook 69 个，与 main 相同）、ABI 签名 PASS、session 路径 PASS、导出符号 PASS；
  结构体布局检查在 13.0 头文件下有 4 处已知差异（`cuda-subset.h` 对齐 13.4），与本改动无关。`make test-nogpu` 全部通过。
- `make test`：29 项全部通过（python 框架脚本因未装框架 SKIP）。
- `test_graph_mem_admission`（普通与 `_ptsz`）：[A]–[G] 通过，[H]（两卡）本机单卡 SKIP。
  [G]"trim 后再次 launch 要重新检查"：在方案 B 版本上把 trim hook 去掉做变异构建，[G] 失败（越限映射 1312 MiB），说明用例能抓到"trim 后不再检查"。
- `test_alloc_async_accounting`（普通与 `_ptsz`）[A]–[P] 全部通过，[F] 改为"加入捕获中 graph 的 alloc node 超限返回 OOM、限额内不记账"
  （账本 0→0→0 MiB）；`test_runtime_capture_accounting` [R1]–[R3] 通过。
- 开销：`cuDeviceGetGraphMemAttribute` 单次 9.8 ns（`cuCtxGetDevice` 12.7 ns），一次 graph launch 约 2 µs。
  `bench_launch`（20000 次 × 5 轮取最好，4 组，单位 µs/次；launch 入队受 GPU 队列影响，有波动）：

  | | 普通 graph | 带 1 MiB alloc+free 的 graph |
  |---|---|---|
  | 不加载本库 | 1.15–1.71 | 2.02–2.20 |
  | main 841a111 | 2.04–2.09 | 2.29–2.72 |
  | 本分支（最终实现） | 1.97–2.00 | 2.18–2.32 |

  本分支与 main 持平，差异在波动范围内（带 alloc node 的 graph 4 组里 3 组略快：少了 16 设备循环）；相对不加载本库多出的 ~0.3 µs 是 launch hook 既有的限流/gap 路径。

  内存检查本身的开销（`bench2`：每批 256 次 launch 只计入队耗时、批间同步不计时，避开 GPU 队列满的阻塞；7 轮×200 批取中位数，
  5 组交替运行；对照 `nocheck` 为同一构建只把 `graph_mem_admit` 调用换成直接成功；限额 4096 MiB 生效、无算力限额），单位 ns/次：

  | 组 | 不加载本库 plain / alloc | nocheck plain / alloc | 本分支 plain / alloc | 检查开销（alloc：本分支 − nocheck） |
  |---|---|---|---|---|
  | 1 | 1721 / 2068 | 1941 / 2149 | 1927 / 2205 | +56 |
  | 2 | 1711 / 2123 | 2024 / 2239 | 1941 / 2350 | +111 |
  | 3 | 1715 / 2018 | 2030 / 2304 | 1977 / 2352 | +48 |
  | 4 | 1701 / 1992 | 2026 / 2280 | 1961 / 2287 | +7 |
  | 5 | 1693 / 2036 | 1933 / 2180 | 1994 / 2139 | −41 |

  带 alloc node 的 graph：检查开销平均 +36 ns、中位 +48 ns（约 +1.6%），与单独测得的属性查询 9.8 ns 同量级，组间噪声约 ±60 ns；
  普通 graph 不走查询，本分支与 nocheck 差值在 −83…+61 ns 之间随机分布，即无开销。

  内存限额关闭时（`vgpu.config` / `CUDA_MEM_LIMIT` 均未开，启动日志 `Memory Limit : disabled`）：当前实现对带 alloc node 的 graph
  仍会先查一次池预留（~10 ns），增长为 0 才返回；首次映射时进 `prepare_memory_allocation`，那里看到限额关闭，不拿锁、不查 NVML。
  不带 alloc node 的 graph 判 `dev < 0` 即返回。变体 `flag`：在查询前先用 `get_device_flag(host_index, memory_limit)`（一次原子读）
  判断，限额关闭直接返回。`bench2` 5 组交替（alloc graph 中位数，ns/次）：

  | 限额 | nocheck | 当前实现 | flag 变体 |
  |---|---|---|---|
  | 关 | 2279（2109–2354） | 2304（2154–2332） | 2241（1783–2319） |
  | 开 4096 MiB | 2142（1679–2272） | 2187（1548–2298） | 2272（2099–2289） |

  端到端噪声（±100 ns 以上，个别离群值）远大于两者 ~10 ns 的确定差别，测不出区分；确定的差别只有"一次驱动属性查询（9.8 ns）"
  对"一次数组查表 + 一次原子读（~1–2 ns）"。限额开启时 flag 变体多一次设备号换算（`prepare_memory_allocation` 内还会再做一次），可忽略。

  采纳 flag 变体（先判 `memory_limit` 再查池预留）。采纳后复测：`make test` 29/29、准入与记账测试（普通与 `_ptsz`）全部通过，
  隐式回收复现仍拒绝 relaunch（A 占用 4032 MiB），限额关闭时 `bench2` 正常运行（plain 2015 / alloc 2245 ns）。

#### 多卡 graph：按实际用到的卡逐卡检查（2026-11-04）

graph 可以跨卡（编程指南：单设备约束只针对条件节点 body 与设备端 launch 的 graph；alloc node 用 `poolProps.location.id` 指定驻留卡，
`accessDescs` 可映射给其他卡，§4.2.5.6；graph 内存池按设备，`cuDeviceGraphMemTrim` / `cuDeviceGetGraphMemAttribute` 都以设备为参数）。
所以把"多卡就不检查"改为：

- walk 时只登记实际分配到的卡：`graph_mem_t { n; d[n] = {CUDA 序号, 字节数} }`，同卡合并、饱和累加，序号超出 `MAX_DEVICE_COUNT` 的忽略
  （与 `cuMemAlloc` 一致，这些卡本来就不受限）。缓存查询只拷贝前 n 项：无 alloc node 的 graph 拷 0 项，单卡 graph 拷 1 项，热路径不变。
- 准入：`n == 0` 直接返回；`n == 1` 走原单卡路径（超限检查通过时持这一把锁到 launch 返回）；`n > 1` 逐卡"加锁 → 检查 → 立即放锁"，
  任一卡超限即返回 OOM。**任何时刻最多持有一把设备锁**，不存在加锁顺序，不可能死锁。
- 判断用分配所在的卡，不用 `cuCtxGetDevice`：限流用的当前 context 设备与分配驻留卡可以不同（手工 graph 的 `location.id` 指向别的卡，
  或在另一张卡的 stream 上 launch），按当前卡判断会完全漏检。
- 曾考虑多卡时按 host index 升序同时持锁到 launch 返回（更严格）：同容器内进程可设不同 `CUDA_VISIBLE_DEVICES` 顺序，按 CUDA 序号加锁会
  交叉等待（设备锁 10 s 超时后无锁继续，不会永久卡死）；为彻底排除死锁，采用"用完立即释放"，代价是上面 §5 的窄竞态。

验证（RTX 2080 Ti 单卡）：`make test` 29/29；准入与记账测试（普通与 `_ptsz`）通过；隐式回收复现仍拒绝 relaunch（4032 MiB）。
[H] 改为断言"当前卡余量被占、另一张卡余量被占时 launch 都返回 OOM，释放后成功"，单卡机 SKIP，**待双卡机器验证**
（另需确认：不配 `accessDescs` 时能否在设备 0 的 context 里向设备 1 加 alloc node）。

### 6.6 双卡真机验证与 VMM 可见时机（2026-11-05，两张 Tesla V100-PCIE-16GB，无 NVLink）

驱动 550.127.05（CUDA 12.4）与升级后的 580.178.04（CUDA 13.0）各测一遍，工具链 12.4；问题 2 的 API 行为和最终回归另测了 550.54.15、570.133.07（CUDA 12.8）。
老驱动的切换方法：官方 CDN 对这台机器返回 403，从 Ubuntu 软件包池下载 `nvidia-kernel-source-*`、`libnvidia-compute-*`，`dpkg -x` 解包、`make modules` 编内核模块，`rmmod` 后 `insmod`，用户态库走 `LD_LIBRARY_PATH`；不动 apt，`modprobe nvidia` 即可回到 580。

**graph 跨卡（`probe_multi`，不加载本库，每卡 1 GiB）**：graph 确实可以跨卡，每张卡一个池，且不要求配 `accessDescs`。

| 场景 | 550 | 580 |
|---|---|---|
| M1 当前 ctx=d0，alloc node 只在 d1 | 成功，池 d1=1024，NVML d1 +1024 | 同左 |
| M2 alloc node 分别在 d0、d1 | 成功，两池各 1024；relaunch 不增长；trim 后归零 | 同左 |
| M3 同上加 `accessDescs` 互相映射 | `CUDA_ERROR_PEER_ACCESS_UNSUPPORTED`（`cuDeviceCanAccessPeer`=0） | 成功（`CanAccessPeer`=1） |
| M4 memset 节点各带 ctx0/ctx1 | 成功 | 成功 |
| M5 捕获：d0 与 d1 的 stream 经 event 接入同一捕获，各分配一次 | 成功，graph 含 loc=0、loc=1 两个 alloc node，launch 后两池各 1024 | 同左 |
| M6 当前 ctx=d1，launch 到 d0 的 stream，分配在 d0 | 成功，映射在 d0 | 同左 |

M6 是"准入不能用 `cuCtxGetDevice`"的实证。M5 说明跨卡 graph 不止手工构建一种来源。

**本库双卡**：`test_graph_mem_admission` [H] 在 550、580 上都通过：当前卡或另一张卡余量被占时 launch 返回 OOM，释放后成功（用例不需要 `accessDescs`）。

**VMM 显存何时对 NVML 可见（`probe_vmm`，1 GiB）**，550 与 580 结果相同：

| 步骤 | NVML 进程占用 |
|---|---|
| 基线 | 306 |
| `cuMemCreate` 返回时（未映射） | 1330（连续 3 次都是首个采样即可见，create 耗时 0.03 ms） |
| map / setaccess / 写入 | 1330 |
| 仍映射时 `cuMemRelease` | 1330 |
| `cuMemUnmap` | 306 |
| 先 unmap 不 release | 1330，release 后 306 |
| retain（返回同一 handle 值）后 release 一次、unmap | 1330，最后一次 release 后 306 |
| 同一 handle 映射两次：release、unmap 一次 | 1330，第二次 unmap 后 306 |
| 从未映射：release | 306 |

结论：`cuMemCreate` 检查完放锁**没有空窗**（物理显存在 create 返回时已被 NVML 计入）；NVML 严格遵循"引用与映射都没了才释放"，
所以我们不写账本的做法不会出现 HAMi-core #390 的提前扣账。新增 `test_vmm_accounting`（[A]–[E]）把这些固定成回归测试，限额 4096 MiB 下 550、580 都通过。

**隐式回收（6.5）在 V100 上同样成立**：580 上池 5280 MiB、空闲 10552 MiB 时 `cuMemAlloc(13191 MiB)` 成功且池归 0；`reclaim.sh` 端到端拒绝 relaunch，占用 4032 MiB。

#### 问题 1：被拒绝的 End 造成记账泄漏（与 HAMi-core #393 同类，已修复）

`probe_end`（不加载本库，550 与 580 相同）：

| 操作 | 返回 | 之后的捕获状态 |
|---|---|---|
| GLOBAL / THREAD_LOCAL 捕获，另一线程 `EndCapture` | `WRONG_THREAD`（908），graph=NULL | **捕获已终止**（status=0）；发起线程再 End 返回 `ILLEGAL_STATE`（401） |
| RELAXED 捕获，另一线程 `EndCapture` | SUCCESS | 正常结束，本库已正确释放 |
| 在接入的 stream 上 `EndCapture` | `UNMATCHED`（903） | 捕获被作废（status=2）；发起 stream 上 End 返回 INVALIDATED，本库已正确释放 |

第 3 节"其他失败（如全局模式下换线程结束）捕获仍在进行，不释放"的判断是**错的**。本库实测（`probe_leak`，限额 4096 MiB）：捕获内分配 3588 MiB，
另一线程 End 后余量视图仍只有 200 MiB、`cuMemAlloc(3588 MiB)` 返回 OOM；5 s 后、其他 stream 上再捕获、同一 stream 上再捕获都清不掉，只能等 context 销毁或进程退出。
main 841a111 同样如此。新增用例 [Q] 复现（账本 0 / 947 / 947 MiB），[R] 覆盖接入 stream 的情况（已通过）。

修复：`end_capture_and_discharge` 在 SUCCESS、INVALIDATED 之外，End 失败后问 `cuStreamIsCapturing`（任何时候都合法），stream 已不在捕获中就释放。
不枚举错误码，因为 `UNJOINED`（904，有分支未汇合）同样是拒绝加终止，只写 `WRONG_THREAD` 会漏。[Q] 账本 0 / 947 / 0，`probe_leak` 余量恢复到 3788 MiB。
另外 `cuStreamEndCapture(s, NULL)` 三个版本都返回 SUCCESS 并结束捕获，不是拒绝（[S]）。

HAMi-core #393 的其余内容在本库不适用：它在捕获和 alloc node 上长期持有记账，要到 graph 销毁才释放，所以"从未运行的 graph 销毁时要退账"；
本库捕获结束即退账、alloc node 不记账，不存在这类残留（[B]、[E]、[F]）。作废捕获的释放本库已有（[G]、[K]、[L]）。

#### 问题 2：异步分配按当前 context 查卡，查错卡（既有，已修复；`cuMemAllocFromPoolAsync` 的修法以 6.7 为准）

`cuMemAllocAsync`、`cuMemAllocFromPoolAsync` 的显存落在 **stream 所在的卡**（`probe_end` A/A2/A3：ctx=d0、stream 在 d1，NVML d1 +1024，捕获得到的 alloc node loc=1），
而 hook 用 `cuCtxGetDevice` 检查和记账。`probe_cross`（本库，每卡限额 4096 MiB，550、580 相同）：

| 场景 | 结果 |
|---|---|
| d1 只剩 512 MiB，ctx=d0，`cuMemAllocAsync(1 GiB, d1 的 stream)` | **成功，d1 越限** |
| 同上 `cuMemAllocFromPoolAsync`（池和 stream 都在 d1） | **成功，d1 越限** |
| d0 只剩 512 MiB、d1 空闲，ctx=d0，`cuMemAllocAsync(1 GiB, d1 的 stream)` | **误报 OOM** |

能否在捕获中问 stream 的设备（`probe_sctx`/`probe_sctx2`，ns 为单次耗时）：

| API | 550.54.15（CUDA 12.4） | 570.133.07（12.8）、580（13.0） |
|---|---|---|
| 不在捕获中 `cuStreamGetCtx` | 正常 | 正常 |
| 不在捕获中 `cuStreamGetDevice` | 符号不存在 | 正常（22 ns） |
| 捕获中 `cuStreamGetCtx` | UNSUPPORTED（900）**并作废捕获**（每个 API 用独立的新捕获验证，End 返回 901） | 成功，捕获不受影响（23 ns） |
| 捕获中 `cuCtxPushCurrent` + `cuCtxGetDevice` + `cuCtxPopCurrent` | 成功，捕获不受影响 | 成功，捕获不受影响（63 ns） |
| 捕获中 `cuStreamGetDevice` | 符号不存在 | UNSUPPORTED（900）**并作废捕获** |
| `cuStreamGetCaptureInfo` | 合法 | 合法 |

所以"捕获中先调用、返回 UNSUPPORTED 再回退"不可行：拿到错误时捕获已被作废，老驱动上每个捕获内的异步分配都会坏掉；新驱动上 `cuStreamGetDevice` 也一样。
查接入捕获的 stream 同样不行（550：同卡接入，对照 End=0，查询后 End=901）。符号表（`nm -D`）：535、550 有 `cuStreamGetCtx`，无 `cuStreamGetDevice`、`_v2`；570 三个都有。
12.6、12.7 的 Ubuntu 包只是过渡空壳，查不到，`cuStreamGetDevice` 确切引入版本在 12.5 到 12.8 之间，官方发行说明里没查到明确记载。设计不依赖这个版本号。

实现（`async_alloc_device`）：
- 查设备的顺序：`cuStreamGetDevice` → `cuStreamGetCtx` 加 push/GetDevice/pop → `cuCtxGetDevice`，按符号是否存在选，不看驱动版本。`cuStreamGetDevice(_ptsz)` 为此加入驱动函数表（透传，不是 hook），表从 604 项增到 606 项。
- 捕获中绝不问驱动：`BeginCapture` 的 6 个 hook 在真正开始捕获之前查好设备号，存进捕获登记表，捕获中读登记表。stream 已经在捕获中（比如已接入别的捕获）时也不问。
- 回退到当前 context 的情况（已知边界）：登记表满（同时超过 64 个捕获）；接入捕获的 stream。后者的缺口基本是空的：接入的 stream 在别的卡且当前 context 没切过去时，驱动本身让捕获失败（End 返回 904，三个版本相同，`probe_join`）；切过去时当前 context 正好是对的卡（M5）。
- 默认 per-thread stream 就是当前 context 的，不查询。

修复后 `probe_cross`：d1 满时 X2、X3 返回 OOM，d0 满 d1 空时 X5 成功。新增用例 [T]（需要两张卡，单卡机不运行、不计 SKIP）覆盖普通、pool 和捕获内三种路径，并断言捕获不被作废、记账落在 stream 的卡上。

**测试环境注意**：`run_tests_with_env.sh` 只把第一张卡的 UUID 写进配置，裸机双卡上 CUDA 能看到第二张卡，库报 FATAL `cuda device 1 cannot find the corresponding host device`，所有测试 rc=1。
要么加 `CUDA_VISIBLE_DEVICES=0`，要么把两张卡的 UUID 都写进 `NVIDIA_VISIBLE_DEVICES` 再直接跑 `run_all_tests.sh`。这次两种都跑了。

**全量测试**（最终实现；C 测试用 CUDA 12.4 工具链编译）：

| 驱动 | 单卡（`CUDA_VISIBLE_DEVICES=0`） | 双卡（两张卡都配限额） |
|---|---|---|
| 550.54.15（12.4） | 30/30（torch 需要 12.6 以上驱动，python 用例 SKIP） | 30/30 |
| 570.133.07（12.8） | 32/32（含 PyTorch） | 32/32（含 PyTorch） |
| 580.178.04（13.0） | 32/32（含 PyTorch） | 32/32（含 PyTorch） |

另外 hook 一致性（606 项，hook 69 个）、ABI 签名、session 路径、导出符号检查通过，`make test-nogpu` 通过；`reclaim.sh` 拒绝 relaunch，占用 4032 MiB。修复前 [Q] 失败（账本残留 947 MiB），X2、X3 越限、X5 误报。PyTorch 2.14.1+cu126（`limit_pytorch_graph.py`，`backend:cudaMallocAsync`，限额 4096 MiB），`--device 0` 与 `--device 1` 都通过：
T2 24 个 graph（临时张量合计 6144 MiB）捕获加回放无误报；T4 占掉余量时回放被拒（`AcceleratorError`），释放后回放成功。
注意：torch 的 cu130 版不带 V100（CC 7.0）的 kernel，报 `no kernel image is available`，V100 要用 cu126 版。

### 6.7 复查发现（2026-11-05，针对提交 5a7c302；驱动 580.178.04，两张 V100）

**缺陷（5a7c302 引入，已修复）：`cuMemAllocFromPoolAsync` 不该按 stream 查卡。** 上文"问题 2"对这个 API 的结论不完整。`cuda.h` 写明"The specified memory pool may be from a device different
than that of the specified hStream"，`probe_pool2`（不加载本库）证实显存落在**池所在的卡**，与 stream、当前 context 都无关；捕获中得到的 alloc node 也是池的 location：

| 池 | stream | 当前 ctx | NVML 增长 | main 查的卡 | 5a7c302 查的卡 |
|---|---|---|---|---|---|
| d0 | d0 | d0 | d0 +1024 | d0 对 | d0 对 |
| d1 | d1 | d0 | d1 +1024 | d0 错 | d1 对 |
| d1 | d0 | d0 | d1 +1024 | d0 错 | d0 错 |
| d0 | d1 | d1 | d0 +1024 | d1 错 | d1 错 |
| d0 | d1 | d0 | 同上推断 d0 | d0 对 | **d1 错（回归）** |

`cuMemAllocAsync` 不受影响（落在 stream 的卡，见上文）。没有查询池所在设备的 API：`cuMemPoolGetAccess` 对所属卡和被授权的对端都返回 3（READWRITE），不能区分（`probe_edge`）。

修复：在池句柄的来源处记下它的设备。hook `cuMemPoolCreate`（device 类型的 location）、`cuDeviceGetDefaultMemPool`、`cuDeviceGetMemPool`，记入 64 槽的表，`cuMemPoolDestroy` 删除，fork 子进程清空并重新初始化锁。
`cuMemAllocFromPoolAsync` 查这张表，捕获中也不需要问驱动。查不到（导入的池、host 位置的池、表满）回退到当前 context，即 main 的行为。

**既有的窄残留（main 上同样存在）：**

| 场景（`probe_edge`，本库，限额 4096 MiB） | 结果 | 处理 |
|---|---|---|
| 捕获中 `cuStreamDestroy`（驱动返回 SUCCESS） | 记账留到 context 销毁或进程退出（free 2764） | 已修：hook `cuStreamDestroy(_v2)`，只针对登记表里的发起 stream；接入的 stream 销毁时捕获还在，不释放 |
| 当前 ctx=d0，捕获在 d1 的 stream 上并分配 1 GiB，然后 `cuDevicePrimaryCtxReset(d1)` | 记账留在 d1（free 2764），重新 retain 后仍在。记录的 ctx 是当前 context（c0），按 c1 清扫不到 | 不修，已知边界：等 c0 销毁或进程退出 |

**核实后没有问题的点：**
- `CU_STREAM_LEGACY`、`CU_STREAM_PER_THREAD`：`cuStreamGetDevice` 返回当前 context 的设备（ctx=d1 时都是 1），`cuStreamGetCtx(LEGACY)` 成功。
- `gap_begin` 在持设备锁时调用，但用 `trylock`，不会阻塞。
- 函数表未加载时 `fn_ptr` 为空：`stream_is_capturing`、`stream_device` 都判空，回退到 `cuCtxGetDevice`。
- 任何时刻最多一把设备锁；`capture_reg` 的互斥锁不跨驱动调用。
- fork：登记表（含新字段）被 `memset` 清空、锁重新初始化；graph 成本缓存同样。
- `make test-sanitize`（ASan+UBSan，无 GPU）20 项通过。注意 `VGPU_SANITIZE` 不作用于 `libvgpu-control.so` 本身（6.4），这项不覆盖 hook 代码。

捕获中 `cuMemPoolGetAttribute`、`cuMemPoolGetAccess` 在 580 上合法，捕获不受影响；老驱动未测。

**修复后的测试**：新增 [U]（捕获中销毁 stream）、[V]（池和 stream 不在同一张卡的组合，含默认池、当前池、捕获内）、[W]（80 个池超过表容量，销毁后可复用）；[T] 只留 `cuMemAllocAsync`。
同一批用例在 5a7c302 上失败 9 处，说明它们能抓到这两个问题。

| 驱动 | 单卡 | 双卡 |
|---|---|---|
| 550.54.15（12.4） | 30/30（torch 需要 12.6 以上驱动，python 用例 SKIP） | 30/30 |
| 570.133.07（12.8） | 32/32（含 PyTorch） | 32/32（含 PyTorch） |
| 580.178.04（13.0） | 32/32（含 PyTorch） | 32/32（含 PyTorch） |

hook 一致性（606 项，hook 75 个，比 main 多 6 个：4 个池相关、2 个 `cuStreamDestroy`）、ABI 签名、session 路径、导出符号通过，`make test-nogpu` 20 项通过。
`make cppcheck` 在这台机器（cppcheck 2.7）上报 2 处 `uninitvar`（`walk_graph_cost` 的 `type`、`loader.c` 的 `device_count`），main 上同样有且位置对应，不是这次引入的；CI 用的版本没有核对。
host 位置的池未实测，走回退。

### 6.8 槽位容量分析（2026-11-05，提交 a2538e2 上的数据；V100，驱动 580.178.04。改造见 6.9）

三张表都是固定大小、循环有界，写满**不会内存越界**，后果是功能退化：

| 表 | 占用方 | 写满后 |
|---|---|---|
| `GRAPH_COST_CACHE_SIZE`=256 | 每个有 kernel 或 alloc node 的存活 exec 一项 | 新 exec 不限流（tokens=1）、不做显存准入，只告警一次 |
| `CAPTURE_REG_SLOTS`=64 | 每个进行中的捕获一项 | 该捕获被作废时记账留到 context 销毁；捕获内异步分配按当前 context 查卡 |
| `POOL_DEV_SLOTS`=64 | 每个已知的内存池一项 | 该池回退到当前 context 查卡 |

**实测 `probe_cache`（限额 4096 MiB，1 GiB 的 graph，余量 512 MiB，launch）：**

| 存活 graph 数 | 结果 |
|---|---|
| 200、256 | 返回 OOM（正确） |
| 257、300 | **返回成功，越限映射**（告警 `graph cost cache full`） |

**缺陷：context 销毁不回收成本缓存槽位。** 缓存项只在 `cuGraphExecDestroy` 成功时释放，而 context 销毁时驱动直接回收 exec，应用不会再调用它。
循环"建 context、建 exec、销毁 context"：100 次后准入正常；300 次后没有任何存活 graph，表却已满，launch 同样越限。陈旧项本身无害（同地址的新 exec 会覆盖或清除），但占着槽位。

**线性扫描的开销**（`bench2` 式分批计时，中位数，ns/次；组间噪声约 ±100）：

| 表大小 / 存活数 | 命中第一项 | 命中最后一项 | 未命中（全表扫描） |
|---|---|---|---|
| 256 / 16 | 699 | 686 | 1740 |
| 256 / 64 | 674 | 733 | 1698 |
| 256 / 128 | 703 | 815 | 1581 |
| 256 / 256 | 673 | 847 | 1751 |
| 4096 / 0（变体，只改宏） | - | - | 3404（256 表同条件 1541） |

末项比首项多约 170 ns；只把宏改成 4096，**每个不进缓存的 graph 的每次 launch 多约 1.9 µs**，launch 本身才约 1.5 µs。所以不能只调大宏，要换成 O(1) 查找。缓存项现在约 280 字节，主要是 16 项的设备列表。

**实际用量：**
- vLLM 默认 `FULL_AND_PIECEWISE`：每个 piecewise 子模块（约每层一个）、每个 capture size 各捕获一个 graph，再加 full 模式的 decode graph。按源码推算上限：子模块数 × size 数 + decode size 数；
  默认 size 列表 `[1,2,4] + range(8,256,8) + range(256,513,16)` 共 51 项，32 层模型约 33×51+51 ≈ 1700，LoRA 还可能翻倍。子模块确切数量和分发键未逐行核实，是上限估算，未跑 vLLM 实测。
- 捕获是一个接一个进行的：PyTorch、vLLM 同时进行的捕获数约等于线程数。[N] 已验证同时 80 个捕获（超过 64）记账全部释放。

**驱动自身的限制**（`probe_many`）：每 graph 一个 2 MiB alloc node，第 1011 个 `cuGraphAddMemAllocNode` 返回 OOM，不加载本库时相同（free 15832 MiB），不是本库造成的，原因未查。

### 6.9 三张表改成可增长哈希表（2026-11-05）

`include/slot_table.h`：纯头文件、零配置静态初始化（库禁止 constructor，同 `vmem_table.h`），三张表共用。开放寻址 + 线性探测 + Fibonacci 哈希；负载超过 70% 翻倍；删除用向后移位，不留墓碑；
内存不足或到上限时继续用现有大小，满了才拒绝写入。fork 子进程放弃旧表（不读、不写、不释放）：别的线程可能正在扩容，继承的状态不可信；每代 fork 泄漏一份副本，换来不碰写时复制页。

| 表 | 初始槽数 | 上限 | 表项 |
|---|---|---|---|
| graph 成本（原 `GRAPH_COST_CACHE_SIZE`） | 4096 | 1 << 20 | 单卡信息内联，约 72 字节（原约 280）；跨卡 graph 的设备列表单独分配 |
| 捕获登记（原 `CAPTURE_REG_SLOTS`） | 64 | 1 << 16 | 键是 stream，per-thread 默认 stream 再加线程；NULL 映射成对应的特殊句柄 |
| 池（原 `POOL_DEV_SLOTS`） | 64 | 1 << 16 | 池 → 设备 |

回收 bug 的修复：成本表项记录 instantiate 时的当前 context，`cuCtxDestroy`、`cuDevicePrimaryCtxReset`、`cuDevicePrimaryCtxRelease`（最后一个引用）时一起清扫。

**已完成的验证：**
- `test_slot_table`（无 GPU，新增）：增长、key2、随机操作对比模型、高负载下的 `retire_if`、到上限和内存不足、句柄分散、abandon、8 线程。ASan+UBSan 通过。
  改坏 5 处（删除的移位条件、删除不清格、`retire_if` 不重看当前格、不按 70% 扩容、不留空格）全部能抓到；其中 `retire_if` 那处最初抓不到（等差句柄不冲突），补了高负载随机句柄的用例。
  随机对齐句柄 2800 个放进 4096 槽，最长探测 40。
- 静态检查：hook 一致性（606 项，hook 75 个）、ABI 签名、session 路径、导出符号通过；`make test-nogpu` 21 项通过；`make test-sanitize` 21 项通过；cppcheck 仍是 6.7 的 2 处。
- 用例有效性（驱动 580）：新增的 [I]（600 个存活 graph）、[J]（300 次 context 循环）在旧提交 a2538e2 上都失败（graph 未被检查）。
- launch 开销（`probe_cache`，驱动 580，3 组，ns/次）：

  | 存活 graph | a2538e2 未命中 / 首项 / 末项 | 本次 未命中 / 首项 / 末项 |
  |---|---|---|
  | 0 | 1690–1760 / - / - | 1570–1593 / - / - |
  | 256 | 1774–1822 / 671–705 / 845–878 | 1621–1664 / 689–703 / 693–721 |
  | 900 | 1696–1764 / 685–710 / 813–868 | 1579–1626 / 676–694 / 676–693 |

- 真实 kernel（`probe_kern`，驱动 580，`CUDA_CORE_LIMIT=10`，6000 个存活 graph，一边 instantiate 一边另一线程并发 launch，单次运行）：

  | | 第一个 graph | 第 6000 个 graph |
  |---|---|---|
  | a2538e2 | 4373 次/s | **32339 次/s（未限流）** |
  | 本次 | 12615 次/s | 13491 次/s（与第一个一致） |

  两次运行都没有崩溃，随后 fork 子进程正常退出、父进程继续 launch。"第一个 graph"的差距（4373 对 12615）的原因见下面的重复测量。

**全量 GPU 回归（提交 0286792，第一台测试机中途回收，在另一台同配置的双 V100 上跑完）：**

| 驱动 | 单卡 | 双卡 | 单独跑（两张卡可见） |
|---|---|---|---|
| 550.127.05（12.4） | 30/30（未装 torch，python 用例 SKIP） | 30/30 | 全部 rc=0，0 处 FAIL |
| 570.133.07（12.8） | 32/32（含 PyTorch） | 32/32（含 PyTorch） | 同上 |
| 580.178.04（13.0） | 32/32（含 PyTorch） | 32/32（含 PyTorch） | 同上 |

"单独跑"指 `test_graph_mem_admission(_ptsz)`、`test_alloc_async_accounting(_ptsz)`、`test_vmm_accounting`，含 [H]、[I]、[J]、[N]（80 个捕获，账本 0 -> 1894 -> 0）、[Q]、[V]、[W]（80 个池）。
三个驱动上跨卡 X2、X3 返回 OOM、X5 成功；`probe_leak` 跨线程 End 后余量恢复；`reclaim.sh` 拒绝 relaunch，占用 4032 MiB。静态检查和 `make test-nogpu`（21 项）同样通过。C 测试用 CUDA 12.4 工具链、PyTorch 2.14.1+cu126。

**`probe_kern` 重复测量**（`CUDA_CORE_LIMIT=10`，6000 个存活 graph，次/s；探针改成"首、末、首、末"的顺序，每驱动每版本多次）：

| 驱动 | 版本 | 首 | 末 | 首 | 末 |
|---|---|---|---|---|---|
| 550.127 | a2538e2 | 12677 | 32252 | 4985 | 32735 |
| 550.127 | 本次 | 12959 | 12683 | 12703 | 12881 |
| 570.133 | a2538e2 | 12928 | 31187 | 5010 | 33122 |
| 570.133 | 本次 | 12849 | 12408 | 12403 | 12399 |
| 580.178 | a2538e2 | 12799 | 27832 | 5568 | 27547 |
| 580.178 | 本次 | 12939 | 12739 | 12735 | 12738 |
| 不加载本库（580） | | 129582 | 130563 | 129747 | 130599 |

上面单次运行里"第一个 graph"的差距（4373 对 12615）查清了：a2538e2 上第一个 graph 首次测量同样约 12700，掉到 4000～5000 是在第 6000 个 graph 之后，因为后者没被计费、超发了算力，共享的令牌桶让下一个有计费的 graph 还账。
本次所有 graph 都稳定在约 12500 次/s，与首个 graph 一致，不影响限流精度。a2538e2 上未计费的 graph 比本次快约 2.5 倍，即修复前的限流绕过幅度；不限流是 10 倍。

**仍未做的：** 没有实际跑 vLLM（`cu130` 以上的 torch 不带 V100 的 kernel，vLLM 对 V100 的支持也没确认），graph 数量仍是按源码推算的上限。没有在 595 驱动上跑（V100 不适用）。

### 6.10 长时间运行的回收、内存安全与遗留项（2026-11-05，双 V100）

**三张表会不会无休止增长。** 用带统计的变体（进程退出时打印容量和项数）把每条回收路径单独循环：

| 路径（`probe_churn`，每种一个进程） | 次数 | 修复前 | 修复后 |
|---|---|---|---|
| exec 创建 + 销毁 | 3 万 | cost 容量 4096，项数 0 | 同左 |
| 跨卡 exec（设备列表单独分配） | 5000 | cost 4096 / 0 | 同左 |
| context 销毁（当前、非当前、带跨卡 exec） | 100–150 | 0 项 | 同左 |
| primary reset、release 最后一个引用 | 60 | 0 项 | 同左 |
| 捕获 + End | 20 万 | reg 容量 64，项数 0 | 同左 |
| 捕获被作废、跨线程 End、捕获中销毁 stream | 3000–1 万 | reg 64 / 0 | 同左 |
| 捕获中销毁 context | 150 | reg 64 / 0 | 同左 |
| **捕获在 X 的 stream 上、当前 context 是 Y，销毁 X** | 150 | **reg 容量 256，项数 150，记账残留** | reg 64 / 0，记账释放 |
| **线程带着打开的捕获退出** | 1 万 | 项数 1（线程 id 复用），但**被替换的旧 graph 记账再也退不掉** | 项数 1，旧记账在替换时释放 |
| 池创建 + 销毁 | 2 万 | pool 容量 64，项数 0 | 同左 |
| 反复取默认池、当前池 | 20 万 | pool 64 / 1 | 同左 |

修复：登记表记下 stream 自己的 context（`BeginCapture` 之前读），任一 context 销毁都清掉记录并按 graph 退记账；同一键被新捕获替换时退掉旧 graph 的记账。

有两条路径项数会涨，核实后是对的，项数等于应用真实持有的数量：
- 建池不销毁、然后 reset 设备（60 项）：池在 reset 后仍有效，`cuMemPoolDestroy` 返回 0。
- graph 建在 X、当前 context 为 Y 时 instantiate，然后销毁 X（150 项）：exec 归属 Y，X 销毁后 exec 仍能 launch 和销毁（`probe_ownr`）。

表只增不缩，容量上限是历史峰值的约 2 倍：表项小（成本 72 字节，其余约 48），不值得为缩容增加复杂度。RSS 在高频循环下不涨（exec 3 万次 154656 → 155424 kB）；涉及 context 或作废捕获的路径多约 15～30 MB，是驱动自身的，是否随循环次数线性增长没有单独测量。

**内存安全。** 之前 `VGPU_SANITIZE` 不覆盖 `libvgpu-control.so`，这次用测试专用构建（`-fsanitize=address,undefined` 作用于 hook 代码，导出表去掉 `dlsym`，`protect_shadow_gap=0`）在真机跑：
`test_graph_mem_admission(_ptsz)`、`test_alloc_async_accounting(_ptsz)`、`test_vmm_accounting`、7 种循环全部 0 个 ASan、0 个 UBSan 报告。runtime（`.cu`）测试因不导出 `dlsym` 不经过本库，不在覆盖内。

**遗留项：**

| 项 | 结果 |
|---|---|
| `cuGraphLaunch` 限流按当前 context 查卡 | **真 bug，已修**。`CUDA_CORE_LIMIT_0=10`，d0 stream 上的 graph：当前 context 切到 d1 时从约 15500 涨到 110820 次/s；修复后 12715、13318、12508。普通 kernel 不受影响，驱动直接返回 `invalid resource handle`。成本表项记下 instantiate 时的设备（`cuCtxGetDevice` 仍保留，见 6.12） |
| 捕获 stream 的 context 被销毁时的记账残留 | 已修（上表） |
| `run_tests_with_env.sh` 裸机双卡只配第一张 | 已改成配上全部 GPU；双卡上标准 `make test` 30/30，单卡机行为不变（只有一个 UUID） |
| 13.x 头文件 | 用 CUDA 13.4 的 `cuda.h`：结构体布局、ABI 签名检查通过；`test_graph_mem_admission`、`test_alloc_async_accounting`、`test_vmm_accounting`（含 `_ptsz`）编译通过，未运行 |
| cppcheck | 2.7 版只有 2 处 `uninitvar`（main 上同样有）；CI 用 ubuntu-24.04 自带版本，本次未核对 |

已知边界：kernel 节点跨卡的 graph 只计到 instantiate 时的卡；不在成本表里的 exec（没有 kernel）仍按当前 context。

**PyTorch 大量 graph**（`torch_many.py`，真实 `torch.cuda.CUDAGraph`，模拟 vLLM piecewise 的存活数量，驱动 580）：

| 分配器 | graph 数 | 限额 | 结果 |
|---|---|---|---|
| 默认 | 1700 | 8192 MiB | 存活、回放、释放，再捕获一遍，退出时 cost 容量 4096、项数 0 |
| 默认 | 5000 | 15500 MiB | 同上，容量自动扩到 8192，项数 0 |
| 默认 | 5000 | 8192 MiB | 第 3691 个 OOM，不加载本库能过。每个 graph 约占 2.2 MiB 显存，算下来正好撞限额，是限额在正常工作 |
| `cudaMallocAsync` | 1700 | 不加载本库 | 第 1010 个失败，即 6.8 的驱动 alloc node 上限，与本库无关 |

捕获 + 回放 + 释放循环 3000 次：cost 容量 4096、项数 0。按上面的显存占用，1700 个 graph 约 3.7 GiB，vLLM 实际用量要结合它的模型层数看，这里没有跑 vLLM。

**fork 压力**（`probe_fork`）：4 个线程持续改动三张表（含 800 个 graph 的增减），主线程反复 fork，每个子进程走一遍 hook 再 fork 一层：60 次 0 挂死 0 错误；ASan 构建下 25 次 0 报告。

**回归**（提交 7b72dd0 / d40a89b，两者只差 cppcheck 写法）：

| 驱动 | 单卡 | 双卡 |
|---|---|---|
| 550.54.15（12.4） | 30/30 | 30/30，单独跑的准入、记账（含 [X]、[Y]）全部通过 |
| 580.178.04（13.0） | 未跑完（节点到期前优先其他项） | 32/32（含 PyTorch），标准 `make test` 30/30 |
| 570.133.07（12.8） | 这两个提交未跑；上一提交 0286792 为 32/32 | 同左 |

静态检查、`make test-nogpu`（21 项）、`make test-sanitize`（21 项）通过；跨卡 X2、X3 返回 OOM、X5 成功；`reclaim.sh`（550.54）拒绝 relaunch，占用 4032 MiB；限流速率见上。
探针修正：`probe_fork` 最初在子进程传伪造句柄 `0x4321`，段错误在驱动内部，属于非法调用，已改用真实句柄。

### 6.11 探针入库与 `VGPU_SANITIZE`（2026-11-05，无 GPU 环境，未在真机运行）

**探针**：`library/test/probes/`，`make test-probes` 运行，`run_all_tests.sh` 不跑；用法和判定标准见其 `README.md`。上文提到的探针名与入库版本的对应：

| 上文 | 入库 | 差别 |
|---|---|---|
| `probe_churn` | `probe_churn.c` | 模式重命名，期望值写在 `run_probes.sh` 里并断言 |
| `probe_fork` | `probe_fork.c` | |
| `reclaim.sh` + `reclaim.c` | `probe_reclaim.c` + `run_probes.sh` | 自带断言，驱动没回收时跳过 |
| `probe_sctx3` | `probe_capture_apis.c` | 新驱动上要重跑：捕获中不问驱动的规则依赖它 |
| `probe_kern` | `probe_graph_throttle.cu` | |
| `probe_rate` | `probe_launch_device.cu` | |
| `torch_many.py` | `torch_many_graphs.py` | |

其余（`probe_multi`、`probe_vmm`、`probe_end`、`probe_join`、`probe_pool2`、`probe_cross`、`probe_leak`、`probe_cache`、`bench2` 等）一次性的驱动事实探针或已被 [H]、[Q]、[T]、[V] 等用例覆盖的，没有入库。

表统计不再靠临时变体：`check_no_constructors.sh` 禁止 destructor，改由现有退出处理打一行 VERBOSE 日志（`table stats: ...`，`LOGGER_LEVEL=4` 可见），不加载调试构建也能查。

入库时为了可读重写过，**重写后的版本没有上 GPU 跑过**，只验证了编译（C 探针用 CUDA 13.4 头文件；`.cu` 探针本机没有 nvcc，未编译）。逻辑和跑过的版本相同，第一次 `make test-probes` 时注意区分探针自身的问题。写的过程中查出一处：`cuCtxCreate` 在 13.x 头文件下是 `_v4` 的 4 参数形式，已改用 `CUCTX_CREATE`。

**`VGPU_SANITIZE`**：
- 原来 `add_compile_options` 在 `add_library(vgpu-control)` 之后，`.so` 根本没被插桩，`make test-sanitize` 只覆盖无 GPU 测试自己的代码。挪到目标定义之前。
- sanitize 构建的 `.so` 不导出 `dlsym`：ASan 启动时调 `dlsym`，导出的话会进到被插桩的 hook，此时 shadow 还没建好就崩了（6.4 的现象）。代价是经 `dlsym` 找驱动的代码（CUDA runtime 等）绕过本库，所以只用于测试。
- 新增 `make test-gpu-sanitize`（`test/run_gpu_sanitize.sh`）：测试程序不插桩，只有库插桩，预加载 ASan 运行时，跑 5 个直接调驱动 API 的 GPU 测试。方法和 6.10 手工构建的一样，但这个入口本身还没在 GPU 上跑过。

cppcheck：CI 用的 2.13（ubuntu-24.04）在全部代码上没有报告；6.9、6.10 记的 2 处 `uninitvar` 只出现在 2.7 上，是旧版本的误报。

### 6.12 合并前的独立审查（2026-11-05，无 GPU，只验证了构建和无 GPU 测试）

对整个分支做了一次独立审查，下面都已修复。**这些修复没有上过 GPU**，上面的真机数据都来自修复之前的提交（到 d40a89b 为止）。

| 问题 | 后果 | 修复 |
|---|---|---|
| launch 改用 exec 的设备后，当前 context 在别的卡时 `gap_events_ensure` 仍用当前 context 建事件，事件却按 exec 的设备索引，而且每卡只建一次 | 该卡之后每次 `cuEventRecord` 跨 context 失败，占空比 gap 被静默关掉，只剩令牌桶（读代码推断） | 当前 context 不是 exec 的卡时这次 launch 不走 gap。`cuCtxGetDevice` 因此保留，开销与 main 相同 |
| `cuGraphExecUpdate` 走 `graph_cost_remember` 时覆盖了表项的 context 和设备 | 在别的 context 下 update 后，令牌扣错卡，context 清扫也按错的 context | 更新时保留原值，新 exec 才写入 |
| 池来源漏了 `cuMemGetDefaultMemPool`、`cuMemGetMemPool` | 经它们拿到别的卡的池，查卡回退到当前 context，正是 6.7 要堵的洞 | 补 hook，函数表 608 项、hook 77 个 |
| 每个 context 有自己的 per-thread stream，登记表的键只有线程 | 同一线程在两个 context 上各开一个捕获时，第二个把第一个判为已结束并退账（只会少计） | 键加上当前 context |
| 按 begin 时的当前 context 清扫 | 销毁 X，而捕获在 Y 的 stream 上：记录被提前清掉 | 优先按 stream 的 context，未知时才用当前 context |
| `cuStreamGetDevice` 在别的 stream 上有 GLOBAL 捕获时是否安全，没有数据 | 可能把应用的捕获弄坏（`cuStreamGetCtx` 已实测安全） | 进程内有捕获时走 `cuStreamGetCtx` + push/pop，探针增加这一项，待上机确认后可放开 |
| `log_graph_table_stats()` 只声明、没有调用（改退出处理的替换没匹配上） | `make test-probes` 必然失败 | 补上调用 |
| `make test-sanitize` 的断言用 `grep -q`，配 `pipefail` 在 `nm` 收到 SIGPIPE 时误判 | CI 随机变红 | 改 `grep -c` |
| `probe_reclaim` 等待没有上限 | 对方进程死掉时 `run_probes.sh` 永远挂住 | 150 s 超时退出 |
| [J]、[W]、[Y] 断言不到声称的内容 | [J] 在可增长的表上看不出记录泄漏；[W] 丢记录时回退结果相同；[Y] 没记账也算过 | [J] 改名说明它测什么（泄漏靠 `probe_churn`）；[W] 增加另一张卡上的池；[Y] 检查返回值并断言确实记账 |
| sanitize 导出表未声明配置依赖、脚本未处理 `nvidia-smi` 失败 | 改导出表不重新生成；错误信息被当成 GPU 列表 | `CMAKE_CONFIGURE_DEPENDS`；检查退出码 |

仍是已知边界：销毁 begin 时的当前 context 会清掉记在它名下的记账，即使捕获在别的 context 的 stream 上还在进行（只会少计）；host 位置的池和导入的池按当前 context 查卡。

审查里没有发现问题的部分：`slot_table.h`（另有 2 万轮 `retire_if` 模糊测试）、加锁和锁顺序、设备锁在所有返回路径的释放、fork 重置、`mem_more` 的所有权。

### 6.13 双 RTX 5090 全量验证与补测（2026-11-05，两张 RTX 5090 32 GiB，无 NVLink，驱动 595.71.05，CUDA 13.2 工具链，PyTorch 2.14.1+cu130）

这是 6.12 的修复第一次上 GPU。先按审查里没覆盖到的项补了用例和探针，再跑全量。13.2 的头文件比 `cuda-subset.h`（13.4）旧，布局检查失败属预期，所以这台机器用 `-DVGPU_RUN_HOOK_CHECKS=OFF`；本地用 13.4 头文件跑了 `make check`。

#### 这轮改的两处库代码

| 问题 | 修法 | 依据 |
|---|---|---|
| `current != device` 的 graph launch 跳过 `gap_begin()`，也就不更新 `last_launch_ns`（评审指出）。下一次本设备 launch 被当成刚结束空闲，多走一次 GAP 路径，多一次同步和占空比 sleep。偏严、一次性，但不对 | `graph_gap_begin()`：跨设备 launch 只打活动戳，仍走令牌桶 | `probe_launch_device`，见下 |
| `stream_device()` 优先用 `cuStreamGetDevice`，有捕获时走 `cuStreamGetCtx`，多一条分支和一个"别的流有 GLOBAL 捕获时是否安全"的未知项 | **按"优先沿用旧驱动的处理方式"**：一律走 `cuStreamGetCtx` + push + `cuCtxGetDevice` + pop，它在 550/570/580/595 上都实测可用；只有它失败才回退 `cuStreamGetDevice`。删掉 `other_capture` 参数和 `capture_reg_any()` | `probe_capture_apis`，见下 |

#### 补的用例（缺口 → 用例）

| 缺口 | 用例 | 595 上的结果 |
|---|---|---|
| 捕获进行中对另一个流做异步分配（同卡、跨卡、捕获在另一张卡） | `test_alloc_async_accounting` [Z] | 捕获都正常结束，账本归零 |
| per-thread stream 在同一线程的两个 context 上各开一个捕获 | [AA] | 结束第二个或销毁其 context：96 → 64 → 0 MiB，互不影响 |
| begin 时的当前 context 先销毁，stream 的 context 还在 | [AB]，`probe_churn capture_begin_ctx_destroyed` | 捕获继续有效，End 成功，账本归零（没有重复退账），表 count=0 |
| `cuMemGetDefaultMemPool`、`cuMemGetMemPool` 按位置取别的卡的池 | [V]（`dlsym` 取符号，旧驱动没有则跳过；先于 `cuDevice*` 取池，否则同一句柄已被登记，测不到） | 该卡满时都返回 OOM |
| `cuGraphExecUpdate` 在别的 context 下更新后，记录仍归原 context | `test_graph_mem_admission` [K]，`probe_churn exec_update` | 那个 context 销毁后 launch 仍被拦截，表 count=0 |
| update 后 launch 仍扣原设备 | `probe_launch_device` | 见下 |
| 第一次 launch 就是跨设备时 GAP 事件不进错 context；跨设备 launch 算活动 | `probe_launch_device`（改成当前设备 1 先 launch） | 见下 |
| 库有没有改变捕获能承受的操作 | `run_probes.sh`：`probe_capture_apis` 裸跑和挂库跑，输出做 diff | 完全一致 |

`probe_capture_apis` 不再要求两张卡，单卡时用第二个 context 代替。

#### `probe_capture_apis`（595，裸跑和挂库输出相同；当前 context 与被捕获 stream 不同）

| 操作（GLOBAL 捕获中） | 返回 | End |
|---|---|---|
| 无 | 0 | 0 |
| `cuStreamGetCtx`（捕获中的流） | 0 | 0 |
| `cuStreamGetDevice`（捕获中的流） | 900 | **901，捕获被破坏** |
| `cuStreamGetCtx`（另一个空闲流） | 0 | 0 |
| `cuStreamGetDevice`（另一个空闲流） | 0 | 0（只有 595 的数据，库只把它当回退） |
| push + `cuCtxGetDevice` + pop | 0 | 0 |
| `cuCtxGetDevice` | 0 | 0 |
| `cuMemAllocAsync`（另一个空闲流） | 900 | 901：驱动的 GLOBAL 语义，裸跑相同，不是库造成的 |
| `cuMemAllocAsync`（捕获中的流） | 0 | 0 |

#### `probe_launch_device`（kernel 约 22 ms，设备 0 限 10%、设备 1 限 100%；稳定性测 3 轮）

| 指标 | 限 10/100 | 对照 100/100 |
|---|---|---|
| 空闲后 launch 调用耗时 | 222 ms（22 ms GPU + 200 ms 占空比，GAP 事件没被污染） | 0.007 ms |
| 紧跟跨设备 launch 的 launch 调用耗时 | 0.003 ms（取多轮最快；个别轮碰到令牌桶 sleep，为 60 ms） | 0.003 ms |
| 当前设备 0 / 1 / 0，launches/s | 4.6 / 4.6 / 4.6 | 44.3 / 44.1 / 43.9 |
| 设备 1 当前下 update 后，launches/s | 4.6 | 43.8 |

这个探针改了三次才可靠，记下来免得再踩：空 kernel 和 memset 在 5090 上压不出利用率，launch 速率反映不出限额（memset 图的 launch 调用本身就 25 ms）；kernel 大小的校准不能放在受限设备上（`rate_limiter` 的 sleep 落在墙钟和事件计时里，会选出过小的 n）；校准前要用相同网格预热一次，否则首次的模块加载和资源分配也会选出过小的 n。

#### 变异验证（故意改坏库，确认用例变红）

| 变异 | 结果 |
|---|---|
| M1 跨设备 launch 不打活动戳 | `probe_launch_device` 失败：紧跟的 launch 222 ms |
| M2 跨设备 launch 也进 `gap_begin` | `probe_launch_device` 失败：空闲后只有 0.59 ms，GAP 事件进了错的 context，占空比永久失效（证实 6.12 那条是真问题） |
| M3 update 覆盖记录的 context | [K] 失败 |
| M4 per-thread 键不含 context | [AA] 失败（期望 96/64/0） |
| M5 两个按位置取池的 hook 都不登记 | [V] 失败（期望 OOM，实际成功） |
| 只去掉其中一个 | 不变红：两个 API 这里返回同一句柄，互相掩盖。第一版用例在分配前调了 `cuDevice*` 取池，连都去掉也不变红，已调整顺序 |

没有做变异的：[Z]。它要防的是"对捕获中的流发查询"，而 595 上 `cuStreamGetCtx` 对捕获中的流是安全的，只有 550 才会破坏捕获，这台机器区分不出。

#### 全量结果（HEAD 为本节提交）

| 项 | 结果 |
|---|---|
| `make test`（双卡，含 PyTorch；两轮） | 32/32，跳过 3 个（mxnet、tensorflow×2 未安装）；第二轮含全部新用例 |
| `CUDA_VISIBLE_DEVICES=0` 单卡视图，accounting 和 `_ptsz`、graph 准入 | 通过；需要两张卡的用例打印未运行 |
| `make test-gpu-sanitize` | 5/5，无 ASan/UBSan 报告 |
| `make test-nogpu`、`make test-sanitize` | 各 21 项通过 |
| `make check`（本地，13.4 头文件）、cppcheck 2.13 | 通过；608 项、77 个 hook |
| `probe_fork` | 60 次 fork，0 挂死，0 异常 |
| `probe_reclaim` | 池被驱动回收（1216→0 MiB），重新 launch 被拒（返回 2） |
| `torch_many_graphs`（1700 个真实 torch CUDAGraph） | 全部捕获、重捕获、释放；cost capacity=4096 count=0，capture 64/0 |
| 6 个进程并发跑 accounting、graph 准入、VMM、pool 用例 3 轮（抢设备锁） | 无挂死（没有 timeout）。部分用例失败是预期的：账本共享，彼此算进用量 |

表回收（`make test-probes`，容量和数量都达到期望）：

| 模式 | N | 表 | capacity | count |
|---|---|---|---|---|
| exec / exec_multi / exec_update | 20000 / 3000 / 3000 | cost | 4096 | 0 |
| ctx / ctx_not_current / ctx_multi | 100 | cost | 4096 | 0 |
| primary_release / primary_reset | 50 | cost | 4096 | 0 |
| capture | 100000 | capture | 64 | 0 |
| capture_invalidated / _stream_destroyed | 5000 | capture | 64 | 0 |
| capture_wrong_thread | 3000 | capture | 64 | 0 |
| capture_ctx_destroyed / _other_ctx_destroyed / _begin_ctx_destroyed | 100 | capture | 64 | 0 |
| capture_thread_exit | 5000 | capture | 64 | 1（线程 id 复用，稳定在 1） |
| pool / pool_get | 10000 / 100000 | pool | 64 | 0 / 1 |

`probe_graph_throttle`（6000 个存活 graph，空 kernel）：不加载库和限 100 都是 244256 launches/s；限 10 时约 38770，首尾一致（首 41173，末 38774）。

#### TSan（一次性，未入库：把 `-fsanitize=address,undefined` 换成 `thread` 构建库，只有库被插桩）

`test_graph_mem_admission` 和 `test_alloc_async_accounting` 全部用例通过，`detect_deadlocks=1` 下**没有锁顺序或死锁报告**。各有一条 data race，都不是这个分支引入的（`git blame`）：

| 位置 | 判断 |
|---|---|
| `rate_limiter` 读令牌桶 vs watcher 的 CAS | 有意的无锁设计，读到旧值只影响一轮 |
| `get_used_gpu_virt_memory` vs `retire_capture_records` | 误报：两边都持跨进程文件锁，TSan 不认识 |

TSan 只覆盖这两个测试程序，不包含 fork，也不覆盖多进程。

#### 同一台机器切到驱动 570.133.07（CUDA 12.8）

5090 必须用开源内核模块和 GSP 固件（`nvidia-kernel-source-570-open`、`nvidia-firmware-570-570.133.07`），闭源模块报 "requires use of the NVIDIA open kernel modules"。550 不支持这张卡。

| 项 | 结果 |
|---|---|
| `probe_capture_apis` 裸跑和挂库 | 输出完全相同，也和 595 的表相同（同样 900→901） |
| accounting、`_ptsz`，graph 准入、`_ptsz`，VMM，test_alloc，pool async、`_ptsz`，test_mem_create | 全部 rc=0；[Z]、[AA]、[AB]、[K] 的数值和 595 相同 |
| [V] 按位置取池 | 570 没有这两个 API（返回 500），打印未运行；hook 不崩，只打告警 |
| `probe_churn`：exec_update、capture_begin_ctx_destroyed、capture_invalidated、capture_other_ctx_destroyed、ctx_multi、pool_get | 无失败，count 为 0（pool_get 为 1） |
| ASan+UBSan | 只跑完 `test_graph_mem_admission`，无报告。其余四项没有结果：100 秒的上限是这次时间不够设的，补跑时命令写错，没来得及再跑 |

这台机器上没有 CUDA 12 工具链，所以测试是用 13.2 头文件编译的，有两个限制：

- `.cu` 测试和 PyTorch cu130 在 570 上跑不了（"driver version is insufficient"）。所以 `probe_launch_device`、GAP 相关和 runtime 测试只有 595 的数据。
- 13.x 头文件把 `cuCtxCreate` 映射成 `cuCtxCreate_v4`，测试传 NULL 参数；570 的 `_v4` 不接受 NULL，直接段错误（最小程序不加载本库也复现，和库无关）。这次临时把测试里的建 context 改成 `_v2` 才跑通，未提交。用与驱动匹配的工具链编译的程序碰不到这个问题。

#### 没有验证的

- 550、580 上这次的提交（550 不支持 5090）。`stream_device()` 现在走的路径就是 550 上一直走的路径，这些查询在 6.6 已于 550/570/580 上测过，但这几个提交没有在这两个驱动上整体回归。
- 570 上的 `make test` 全量：缺 12.x 工具链，只跑了上表这些 C 用例和探针；570 上的 ASan 只覆盖一项。
- vLLM 本身。

### 6.14 graph 池复用与两处误报 OOM（2026-11-06，RTX 3080 10 GiB，驱动 595.84，CUDA 13.0，main c405ed7，torch 2.15.0+cu132）

复查同类问题时发现两个疑点，这里是实测结果。"每个存活 graph 都按完整大小记账"这种累加我们没有，见 6.4 的 T2；这两处是相近的症状：物理显存够，却报 OOM。两处都已修复，见本节末尾。下面"问题 1""问题 2"的表是修复前 main 的行为。

探针：`test/probes/probe_pool_reuse.c`（手工图和捕获）、`test/probes/torch_pool_reuse.py`（真实 torch graph，从源码目录直接运行）。

#### 驱动的池怎么复用（不加载库，`probe_pool_reuse inner / shapes / blocks`）

每行先 trim，读 `CU_GRAPH_MEM_ATTR_RESERVED_MEM_CURRENT`。B = 256 MiB；a/f 是一条依赖链上的 alloc/free 节点，`|` 表示另起一条没有依赖的线。

| 形状 | 总和 | 峰值 | 池实际 MiB | 说明 |
|---|---|---|---|---|
| 16×B 依次分配释放（手工、捕获都一样） | 4096 | 256 | **256** | 同一 graph 内会复用 |
| 8×1024 依次分配释放 | 8192 | 1024 | **1024** | |
| 16×B 同时持有 | 4096 | 4096 | 4096 | 对照 |
| 4×B 同时持有 | 1024 | 1024 | 1024 | 对照 |
| 同时最多两块 a0 a1 f0 a2 f1 f2（各 B） | 768 | 512 | 512 | |
| 两条无依赖的线 a0f0 \| a1f1 | 512 | 256 | **512** | 没有依赖就不复用 |
| 四条无依赖的线 | 1024 | 256 | **1024** | |
| 递增 B、2B、4B | 1792 | 1024 | **1792** | 等于总和，不是峰值 |
| 递减 4B、2B、B | 1792 | 1024 | 1024 | |
| 混合 B、2B、B/2、4B、B/4 | 1984 | 1024 | 1792 | |
| 2B，然后 B 和 B 同时持有 | 1024 | 512 | 512 | 空闲块能切分 |
| 4B，然后 4×B 同时持有 | 2048 | 1024 | 1024 | |
| B 和 B 都释放，然后 2B | 1024 | 512 | 512 | 相邻空闲块能合并 |
| B，然后 B + 2 MiB | 514 | 258 | 544 | 空闲块不能原地扩展；按 32 MiB 取整（258 → 288） |
| B，然后 B + 64 MiB | 576 | 320 | 576 | |
| B、2B、B | 1024 | 512 | 768 | |
| B、2B、B、2B | 1536 | 512 | 768 | |

结论：同一 graph 内的行为像一个按依赖顺序工作、可切分可合并、不能原地扩展的空闲块分配器，只有 alloc 依赖于 free 时才复用。**"峰值"不是正确的估算**：递增序列的实际值等于总和，改成峰值会低估。总和是上界（只差 32 MiB 取整）。这个模型只在 595.84 上测过。

#### 问题 1：捕获时不考虑空闲的 graph 池

捕获内分配和 `cuGraphAddMemAllocNode` 按完整大小检查 `NVML + 账本 + 请求`，而 NVML 已含空闲的池，之后 launch 能复用这部分。

| 场景（`probe_pool_reuse`） | 结果 |
|---|---|
| 不加载库，recapture 2048 | 第二次捕获成功，第二次 launch 池 2048 → 2048，不增长 |
| 限额 4096，recapture 2048 | **第二次捕获的 `cuMemAllocAsync` 返回 2**（误拒） |
| 限额 4096，recapture 1800（对照） | 通过，池 1824 → 1824 |
| 限额 4096，refill 2048 | `cuGraphAddMemAllocNode`、捕获内 `cuMemAllocFromPoolAsync` 也都返回 2 |
| 限额 4096，refill 1800（对照） | 都通过，池不增长 |

真实 torch（`PYTORCH_CUDA_ALLOC_CONF=backend:cudaMallocAsync`，`torch_pool_reuse.py recapture`，捕获后立刻回放，张量 1500 MiB + 同样大小的临时块）：

| 配置 | 结果 |
|---|---|
| 不加载库，3 次 | 全部成功，池稳定在 1504 MiB |
| 限额 4096，3×1500 | 第 1 次成功；**第 2 次捕获失败**，应用看到 `cudaErrorStreamCaptureInvalidated`，而张量和池合计约 3004 MiB，放得下 |
| 限额 4096，3×1000（对照） | 全部成功，池 1024 MiB |

触发条件：捕获后回放、再捕获，且临时块大于可用额度的大约一半。先全部捕获再回放的用法（6.4 的 T2、vLLM）不会触发。

#### 问题 2：launch 的 `need` 是总和

`graph_mem_add` 把所有 alloc 节点相加。捕获时 free 会退账，所以能通过；到首次 launch 才按总和检查。

| 场景 | 结果 |
|---|---|
| 不加载库，`inner 16 256` | 依次分配释放 256 MiB，同时持有 4096 MiB |
| 限额 2048，`inner 16 256` | 手工、捕获的依次分配释放都**返回 2（误拒，实际只要 256 MiB）**；同时持有返回 2（正确，修的时候不能放过这一行） |
| 限额 8192，`inner 16 256`（对照） | 都通过，池 256 / 4096 MiB |
| 不加载库，torch 链式 12×256（`y = y * 1.0001`） | 回放成功，池 512 MiB |
| 限额 2048，torch 链式 12×256 | **回放 OOM**，池 0 |
| 限额 2048，torch 链式 6×256 | **回放 OOM**，池 0 |
| 限额 6144，torch 链式 12×256（对照） | 成功，池 512 MiB |
| 限额 2048，torch 链式 12×256，默认 allocator | 成功，池 0 MiB：临时块不进 graph |

#### 影响范围

只影响 graph 内有 alloc 节点的负载：`cudaMallocAsync` 后端、直接用 `cuMemAllocAsync` 或内存节点的程序。PyTorch 默认 allocator 不受影响，上面最后一行就是证据。两者都是偏严的错误（拒绝合法请求），不会让容器超限。Warp、mjlab 是否属于这一类没有验证。

#### 修复（分支 fix/graph-pool-false-oom）

取舍：多算会让没超限的任务 OOM、服务不可用；少算的部分映射后 NVML 能看到，再次分配和新进程都会被限住。所以**宁可少算**，但能准就准。

| | 修法 | 代价 |
|---|---|---|
| 问题 1 | 捕获内的 `cuMemAllocAsync`、`cuMemAllocFromPoolAsync` 和 `cuGraphAddMemAllocNode` 改为检查 `NVML + 账本 + 请求 − min(池预留, 本进程捕获记账 + 请求)`。普通分配不变 | 捕获内每次分配多一次属性查询和一次记账表扫描，只在池非空时扫描 |
| 问题 2 | `include/graph_pool_sim.h`：instantiate 遍历节点时按节点顺序模拟驱动的分配器（选能放下的最小空闲块、切分、合并、不原地扩展、按 32 MiB 取整），得到 `need`。内存不足时退回总和；驱动没有 `cuGraphMemFreeNodeGetParams` 时同样是总和 | 只在 instantiate 走一次，launch 热路径不变；没有 alloc 节点的 graph 没有额外开销 |

没有做依赖可达性分析（代价大，收益只在并行分支）。

**模型准确度**，`probe_pool_reuse model`，随机序列（最多同时 4 GiB，大小 1–900 MiB，一条依赖线），600 个序列离线比较各种策略：

| 估算 | 完全一致 | 偏高（平均） | 偏低（平均，最差） |
|---|---|---|---|
| 总和（修复前） | 35 | 565 例（2405 MiB） | 0 |
| 峰值 | 81 | 0 | 519 例（336，1568 MiB） |
| first-fit | 424 | 67 例（137 MiB） | 109 例（231，1312 MiB） |
| **best-fit（采用）** | **440** | **52 例（87 MiB）** | **108 例（226，1312 MiB）** |
| worst-fit | 405 | 97 例（212 MiB） | 98 例（251，1312 MiB） |
| last-fit | 424 | 75 例（183 MiB） | 101 例（223，1312 MiB） |
| 从高地址切分（各策略） | 373–418 | 更多 | 相近 |
| 取整的余量不可复用（各策略） | 362–404 | 更多 | 略少 |

上机复核 300 个序列：219 个完全一致，21 个偏高（平均 58，最差 256 MiB），60 个偏低（平均 237，最差 1312 MiB）。这是专门挑的碎片化序列；上面表里的结构化形状全部精确，已写入 `test/nogpu/test_graph_pool_sim.c`。

#### 修复后的结果

| 场景 | 修复前 | 修复后 |
|---|---|---|
| 限额 4096，recapture 2048 | 第二次捕获返回 2 | 通过，池 2048 → 2048 |
| 限额 4096，refill 2048（节点、池内分配） | 都返回 2 | 都通过，池不增长 |
| 限额 2048，`inner 16 256` 依次分配释放 | 返回 2 | 通过，池 256 MiB |
| 限额 2048，`inner 16 256` 同时持有 | 返回 2 | **仍返回 2**（正确） |
| torch 限额 4096，recapture 3×1500 | 第 2 次捕获失败 | 全部成功，池 1504 MiB |
| torch 限额 2048，链式 12×256 / 6×256 | 回放 OOM | 成功，池 512 MiB |
| torch 对照（recapture 3×1000、限额 6144） | 成功 | 成功，池不变 |

新增用例：`test_graph_mem_admission` [L]（空闲池算数，但只到池的大小：同一捕获的第二块、超过池加余量的请求仍被拒）、[M]（链式不按总和；同时持有和递增序列仍被拒）、[N]（本捕获已记的也算在池内）；`test_graph_pool_sim`（实测表逐项比对，2000 轮随机序列的不变量：块不重叠且总量等于 top，top 介于峰值和取整总和之间）。

变异验证（`test_graph_mem_admission`）：

| 变异 | 结果 |
|---|---|
| 不折扣（等于 main） | [L]、[N] 失败 |
| 忽略捕获记账 C | [N] 失败 |
| 吸收量不以池大小为上限 | [L] 失败：第二块放过了 |
| 不看 free 节点（等于总和） | [M] 失败 |

#### 全量结果（RTX 3080 10 GiB，单卡）

| 项 | 595.84（CUDA 13.0 工具链，torch 2.15.0+cu132） | 580.178.04（同一工具链，无 torch） |
|---|---|---|
| `make test` / `run_tests_with_env.sh` | 32/32，跳过 3 个未安装的框架 | 30/30，torch 也跳过：cu132 需要更新的驱动 |
| `make test-gpu-sanitize` | 5/5，无报告 | 5/5，无报告 |
| `make test-sanitize`（无 GPU） | 22 项 | — |
| `probe_capture_apis`，含新增的 `cuDeviceGetGraphMemAttribute` | 捕获不受影响，挂库输出相同 | 相同 |
| `probe_pool_reuse` 形状表、300 个随机序列 | 见上 | **全部和 595 相同** |
| `make check`（本地 13.4 头文件）、cppcheck | 通过；608 项，77 个 hook | — |

TSan（一次性，换 `-fsanitize=thread` 构建；这台 7.0 内核要 `vm.mmap_rnd_bits=28` 和 `setarch -R`）：`test_graph_mem_admission`、`test_alloc_async_accounting` 全部通过，没有锁顺序或死锁报告。data race 报告都在已有位置：令牌桶的无锁读，以及 ledger（读写都持跨进程文件锁，TSan 不认识）。

这轮还修了两处测试本身的问题，都不是库的问题：

- `test_graph_mem_admission` [I] 写死 600 个存活 graph。驱动对存活 alloc 节点的限制因卡而异：不加载库时 RTX 3080 在 618 个 1 MiB 的 graph 处返回 2，518 个时 256 MiB 的节点就失败，418 个时 3000 MiB 仍能成功（V100 约 1010）。main 的库在这张卡上同样失败。改成 300，仍超过当初固定表的 256。
- `probe_churn primary_release` 单卡时不是最后一个引用，execs 留着是对的（count=50）。改成需要两张卡。

顺带确认的：`PYTORCH_CUDA_ALLOC_CONF=backend:cudaMallocAsync` 下，捕获中真正超限时 torch 会 terminate（析构里的 free 抛 `cudaErrorStreamCaptureInvalidated`）。main 和修复后相同，是 torch 的行为。

#### 已知边界和没有验证的

- 依赖无关的并行分支、并发 launch：按节点顺序模拟会低估（两条线各 256，实际 512，估算 256）。映射后 NVML 能看到。**6.15 已修正，并发现不释放的块会造成可叠加的绕过。**
- 捕获打开期间的**普通**分配（别的线程的 `cuMemAlloc`）还是 `NVML + 捕获记账`，和池重复计算：只在捕获窗口内，可能偏严。
- 空闲的 graph 池对普通分配仍然算占用，那是真实占用（6.5）。
- **570、550 没有验证**：这台机器内核 7.0，这两个驱动的模块编不过。捕获中查 `cuDeviceGetGraphMemAttribute` 的安全性和复用模型只在 580、595 上测过。如果旧驱动的模型不同，偏差方向由上表决定，少算有 NVML 兜底。
- 跨卡路径没有上 GPU（这台只有一张卡）：[H]、`exec_multi`、`ctx_multi` 打印跳过。每张卡一个模拟器，逻辑上互不相关，但没有跑过。
- Warp、mjlab、vLLM 本身没有跑。

### 6.15 持有、并发与依赖：把三处少算也修准（2026-11-08，两张 RTX 2080 Ti 11 GiB，无 NVLink，驱动 595.84，CUDA 13.0，torch 2.15.0+cu132）

6.14 修的是多算。按"宁可少算"处理的三处少算，这轮在双卡机上逐条测了驱动的规则，结果其中一处是**可以无限叠加的绕过**，所以都改成按规则计算。数据来自 `exp`、`exp2`、`exp3` 三个探针（不加载库，规则部分已整理进 `probe_pool_reuse` 和用例），以及 `hc` 探针（借鉴 HAMi-core #393/#396 的测试场景）。

#### 发现的问题（修复前的 main 与 6.14 的分支都有）

| 问题 | 规则 | 后果 |
|---|---|---|
| 不释放的块 | graph 分配后不释放的块，launch 后一直归进程，exec、graph 销毁也不还，**context 销毁、primary reset 也不还**；只有任意 context 里的 `cuMemFree`/`cuMemFreeAsync`、或一个含它的 free 节点的 graph 运行后才还 | 池预留包含它，下一个 graph 以为池够大，增长算成 0。**限额 4096 下第二个不释放的 graph launch 成功，池到 5248 MiB，可以一直叠加**（`hc`） |
| 别的 stream 上还在跑的 graph | 驱动只在它在 GPU 上**执行完**后才复用它的池内存，和 host 是否同步无关；用 event 让后者等它也不复用；同一 stream 上复用 | 并发 launch 时池增长算成 0，N 个 stream 就能把池撑到 N 倍，池会留着这些页 |
| graph 内并行分支 | 只复用"祖先 free"释放的块；按**创建顺序**处理（后补的依赖不算）；互不先后的两个 free 释放的相邻块**不合并** | 两条无依赖的线各 x，按节点顺序模拟算成 x，实际 2x |

DAG 规则（B = 256 MiB，`exp dag`）：

| 形状 | 池 MiB |
|---|---|
| 链 a0 f0 a1 | 256 |
| 两条并行线 | 512 |
| 两条线的 free 之后分配 2B | 1024（不合并） |
| 只依赖其中一条线的 free 分配 B | 512 |
| 同一 free 之后的两个兄弟分配 | 512 |
| 依赖 a0、与 f0 并行 | 512 |
| 经空节点传递依赖 | 256 |
| 依赖 free 和另一个根 | 256 |
| 菱形（free 在一侧，分配在另一侧） | 512 |
| a1 先创建，之后补上对 f0 的依赖 | 512（按创建顺序） |

并发规则（1 GiB，`exp conc`）：

| 情况 | 池 MiB（之前 → 之后） |
|---|---|
| A 在 s1 跑完并同步，B 在 s2 | 1024 → 1024 |
| A 在 s1 被挂住，B 在 s2 | 1024 → **2048** |
| A 在 s1 已在 GPU 上跑完，host 没同步，B 在 s2 | 1024 → 1024 |
| A 在 s1 被挂住，s2 用 event 等它，B 在 s2 | 1024 → **2048** |
| A 在 s1 被挂住，B 也在 s1 | 1024 → 1024 |
| 同一个 exec 在 s1 挂住时又在 s2 launch | 1024 → 1024 |

不释放的块（1 GiB，`exp unfreed`、`exp2`、`exp3`）：

| 情况 | 结果 |
|---|---|
| U 不释放已 launch，F（分配加释放）launch | 池 1024 → 2048 |
| `cuMemFree(U 的块)` 后 F | 池不变，复用 |
| `cuMemFreeAsync` 后 F | 复用 |
| U 的块还被持有时再 launch U | 驱动返回 1 |
| AUTO_FREE_ON_LAUNCH 的 U 再 launch | 复用自己的块 |
| AUTO_FREE 的 U launch 后 F | 2048：直到 U 再 launch 都持有 |
| exec（无论 AUTO_FREE 与否）和 graph 销毁后 F | 2048：仍持有，之后 `cuMemFree` 成功 |
| 只 upload 不 launch，F | 1024：映射了但不算持有；此时 `cuMemFree` 返回 1 |
| 另一个 graph 的 free 节点运行后 F | 复用 |
| context 销毁、primary reset | 仍持有；从别的 context `cuMemFree` 成功，池随即可 trim 到 0 |
| 持有时 trim | 不还 |


580.178.04 上三组规则与 595.84 逐行相同。

#### 修复（分支 fix/graph-pool-false-oom）

| 部分 | 做法 | 代价 |
|---|---|---|
| 依赖 | instantiate 遍历时，graph 既有分配又有 free 才算：`cuGraphGetEdges`（失败或 `LOSSY` 再用 `_v2`）取边，按拓扑序传播"依赖了哪些 free 节点"的位图。模拟器只复用祖先 free 释放的块，只合并一方依赖另一方的相邻块，仍按创建顺序处理。位图超过 1M 个 64 位字或取不到边，退回按节点顺序 | 只在 instantiate 时 |
| 持有 | `dptr -> 卡、字节` 的表（可增长哈希表）。launch 成功后加入 graph 不释放的块（地址来自模拟器），`cuMemFree`、不在捕获中的 `cuMemFreeAsync`、含其 free 节点的 graph launch 移除；context 拆除**不**移除。可用池 = 预留 − 持有 + 本 exec 自己正被持有的块（按地址逐个核对，AUTO_FREE 的 exec 再 launch 时拿回来）。捕获时的折扣（6.14 问题 1）也扣掉持有 | free 热路径只多一次原子读；只有带不释放块的 exec 才查表 |
| 在途 | 每个 stream 一条记录。某张卡只见过一个 stream 时不做别的；见到第二个 stream 起，每次 launch 后记 event，之前没有 event 的记录问一次 `cuStreamQuery`（先确认不在捕获中）。可用池再扣掉别的 stream 上未完成 graph 的需要量 | 见下面的开销 |

没有做的：在途只看 graph 的第一张卡；per-thread 默认流在线程之间当作同一个 stream；子 graph 里的 free 节点不被任何分配看作祖先（只会多算，子 graph 里有内存节点本来就少见）。

#### 修复前后（`hc`，限额 4096 MiB）

| 场景 | 6.14 的分支 | 本节 |
|---|---|---|
| 第二个不释放的 graph（先 launch 第一个再建第二个） | launch 成功，池 5248 MiB，可叠加 | 节点创建时就被拒 |
| 三种捕获模式下，池能容纳第一块、不能容纳第二块 | 正确 | 正确 |
| `cuStreamQuery` 作废捕获后余量 | 恢复 | 恢复 |
| 同时打开的捕获拿到相同地址 | 200 对中 0 次 | 同左（按 `dptr` 做键是安全的） |

main 上若两个不释放的 graph 都先建好、再依次 launch，同样绕过（`[O]` 就是这个顺序）。

#### 新用例（`test_graph_mem_admission`）与变异验证

| 用例 | 内容 |
|---|---|
| [L]、[N] | 改为三种捕获模式各跑一次 |
| [O] | 第二个不释放的 graph 被拒；`cuMemFree`、`cuMemFreeAsync`、另一个 graph 的 free 节点之后放行；只 upload 不算持有；AUTO_FREE 再 launch 不误拒；context 销毁后块仍持有，从别的 context 释放后放行 |
| [P] | 另一 stream 上的 graph 未运行时拒绝，运行完或同一 stream 时放行 |
| [Q] | 两条并行线拒绝，链式放行 |

`probe_churn` 新增 `held`（1 万轮）、`held_ctx`（100 轮，context 销毁后从主 context 释放）两种模式，持有表都回到 0。

| 变异 | 打红的用例 |
|---|---|
| 持有不计入 | [O]（6 处） |
| `cuMemFree` 不移除持有 | [O] |
| 在途永远视为已完成 | [P] |
| 不算依赖 | [Q] |
| 不加回本 exec 自己的持有 | [O]（AUTO_FREE 再 launch 被误拒） |
| 忽略外来 free 节点 | [O] |

#### 模型准确度（随机 DAG，最多三条线、带跨线边，`probe_pool_reuse dagmodel 600`，595.84）

| 估算 | 完全一致 | 偏高（平均，最差） | 偏低（平均，最差） |
|---|---|---|---|
| 总和（6.14 之前） | 38 | 562 例（2239，6784 MiB） | 0 |
| 按节点顺序（6.14） | 197 | 27 例（146，448 MiB） | 376 例（910，3712 MiB） |
| **带依赖（本节）** | **335** | 124 例（316，1376 MiB） | 141 例（389，1440 MiB） |

580.178.04 上 400 个图：222 / 126 / 20 例完全一致，比例相同。一条线的随机序列（`model 300`）两个驱动都是 219 例一致。

离线比较了 24 种变体（可见性、合并规则、块选择、按拓扑序处理、只对总量取整、余量不与带标签块合并、可跨块取段），都不比现在好。随机 DAG 上剩下的偏差没有找到规律，就此打住：规整形状（链、并行线、菱形、兄弟、传递依赖）全部精确，写进了 `test_graph_pool_sim`。取 `min(带依赖, 按节点顺序)` 等于退回按节点顺序。

#### 热路径开销（`exp/bench`，单位 µs/launch，三轮，单 stream 与双 stream 在不同进程里测）

| | 不加载库 | main | 本节 |
|---|---|---|---|
| 空节点 graph | 0.23 | 0.32 | 0.33 |
| alloc+free graph，单 stream | 0.51–0.55 | 0.68–0.70 | 0.80–0.83 |
| alloc+free graph，两个 stream 交替 | 0.80–1.24 | 0.97–1.51 | 1.63–2.31 |

第一版每次 launch 都记 event，单 stream 是 1.43 µs，改成"见到第二个 stream 才记"后降到上表。一个进程见过两个 stream 后不会再回到单 stream 的做法。

#### 全量结果（两张 RTX 2080 Ti，本节最终代码）

| 项 | 595.84（torch 2.15.0+cu132） | 580.178.04 |
|---|---|---|
| `make test` / `run_tests_with_env.sh`（双卡） | 32/32，跳过 3 个未安装的框架 | 32/32 |
| `make test-gpu-sanitize` | 5/5，无报告 | 5/5，无报告 |
| `make test-sanitize`（无 GPU） | 22 项 | — |
| `make test-probes` | 全部通过；churn 各表回到 0（`capture_thread_exit` 1、`pool_get` 1 是预期的）；fork 60 次无挂死；驱动隐式回收后重新 launch 被拒 | — |
| `probe_launch_device` | 本节与 main 各 3 次全部通过 | — |
| torch（`cudaMallocAsync` 后端）：recapture 3×1500/3×1000、链式 12×256/6×256 限额 2048、对照 6144 | 全部成功，池 1504/1024/512 MiB | — |
| `make check`（本地 13.4 头文件）、cppcheck | 通过；608 项、77 个 hook | — |
| TSan（一次性，`vm.mmap_rnd_bits=28` 加 `setarch -R`） | 两个测试全部通过，**没有锁顺序或死锁报告**；data race 都在已有位置（令牌桶无锁读、TSan 不认识的跨进程文件锁） | — |
| 6 个进程并发 3 轮 | 18 个进程都没有超时；失败是共用账本互相算进用量，预期之内 | — |

这轮还修了两处测试本身的问题：`probe_launch_device` 的校准改为同一尺寸连续两次都 ≥20 ms 才采用（空闲卡起步时钟低），从设备 1 发起的那次 launch 改用极小的 graph（原来的大 kernel 会掏空设备 0 那 10% 的令牌桶，使下一次 launch 等令牌，测出 50–100 ms）。改之前 main 和本节在这台卡上都间歇失败。

记一个过程上的教训：网络断开后，旧的会话进程没有退出，和新会话同时用同一台 GPU 机器和同一个工作区跑了约半小时。这期间并发跑的结果（包括一次开销测量和 `probe_launch_device` 的失败）已全部作废，上面的数据都是在机器只有一个会话时重跑的。

#### 已知边界和没有验证的

- 随机 DAG 上模型仍有约 20% 偏高、约 24% 偏低（见上表），规整形状精确。
- 在途只看 graph 的第一张卡；per-thread 默认流跨线程视为同一个 stream。
- 捕获打开期间的**普通**分配仍会把捕获记账和池重复计算（只在捕获窗口内偏严）。
- 570、550 没有验证：这两台机器的内核是 7.0，模块编不过。
- Warp、mjlab、vLLM 本身没有跑。
