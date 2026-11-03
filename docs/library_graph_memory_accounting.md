# CUDA Graph 显存记账（library）

本文记录 `libvgpu-control.so` 对 CUDA Graph 相关显存的限额检查与记账：设计依据、真机实测的驱动行为、测试结果与已知边界。
起因是对照 HAMi-core [#387](https://github.com/Project-HAMi/HAMi-core/issues/387) / [#388](https://github.com/Project-HAMi/HAMi-core/pull/388)
（graph 分配不计入限额、捕获中 `cuMemPoolGetAttribute` 打断捕获）与 [#389](https://github.com/Project-HAMi/HAMi-core/pull/389)（`cuDeviceTotalMem`）。

## 1. 记账口径回顾

显存限额检查 = NVML 报告的进程实际占用 + 本进程"尚未被 NVML 看到"的在途记账（共享账本 `/tmp/.vmem_node`）。
在途记账有两类：

| 类型 | 何时记 | 何时释放 |
|---|---|---|
| `MEMORY_TYPE_ASYNC_BRIDGE` | 非捕获的 `cuMemAllocAsync` / `cuMemAllocFromPoolAsync` 调用后 | 同步该 stream 后（NVML 已可见） |
| `MEMORY_TYPE_CAPTURE` | 捕获期间的异步分配，以及加入捕获中 graph 的 alloc node | 捕获结束（成功或被作废）、graph 销毁、按指针释放、所属 context 销毁/重置、fork 子进程作废、进程退出清理 |

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
  目标 graph 正处于捕获中时，按捕获记账（随捕获结束释放）；手工构建的 graph 不记账。
- **捕获登记表**：hook `cuStreamBeginCapture` / `_v2` / `_ptsz` / `_v2_ptsz` / `cuStreamBeginCaptureToGraph` / `_ptsz`，开始捕获成功后记下
  "发起 stream → graph"（默认 per-thread stream 再按线程区分），64 个槽位，满了退回到结束时查询。`cuStreamBeginCaptureToGraph(_ptsz)` 原先不在驱动函数表里，现已加入。
  `cuStreamBeginCapture` 属 ABI 冲突族，已按先例加入 `R5_ALLOWLIST`（每个包装都调用自己同名的入口）。
- **`cuStreamEndCapture`**：graph 取自登记表（缺失时现场查询），在返回 `CUDA_SUCCESS` 或 `CUDA_ERROR_STREAM_CAPTURE_INVALIDATED` 时释放；
  其他失败（如全局模式下换线程结束）捕获仍在进行，不释放。
- **context 拆除兜底**：记账记录增加 `ctx` 字段（进程私有，不涉及共享 ABI）。`cuCtxDestroy(_v2)`、`cuDevicePrimaryCtxReset(_v2)`、
  释放最后一个引用的 `cuDevicePrimaryCtxRelease(_v2)` 成功后按 context 清理记账与登记表。这 6 个入口从透传改为注册进 hook 表，
  `cudaDeviceReset` 等经 `cuGetProcAddress` 获取的调用也能覆盖。primary context 只在活跃时获取句柄（对非活跃的 retain 会创建它）。
- fork 子进程清空登记表并重新初始化其锁。

## 4. 测试

GPU 用例在 `library/test/test_alloc_async_accounting.c`（同源编译出 `_ptsz` 变体），直接读共享账本断言：

| 用例 | 内容 |
|---|---|
| [E] | 手工 graph：超限 alloc node 返回 OOM；限额内成功且不记账 |
| [F] | 加入捕获中 graph 的 alloc node 记账，捕获结束后释放 |
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

- **空窗 B**：捕获结束（或手工 graph 加完节点）到第一次 upload/launch 之间，graph 的显存既不在记账里也不在 NVML 里，
  这段时间里的普通分配加上随后 launch 映射的显存可能超限。见第 6 节。
- 手工 graph 内多个 alloc node 各自检查、不累加。
- 登记表满（同时超过 64 个进行中的捕获）时退回到结束时查询，此时被作废的捕获仍会遗留记账，直到 context 拆除或进程退出。
- `cuDevicePrimaryCtxRelease` 的"查状态 → retain"之间若另一线程释放了最后一个引用，retain 会重新创建 primary context，随即被撤销的 release 销毁；记账结果不受影响。

## 6. 空窗 B 的修复研究

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

### 6.3 结论与待决事项

- 这个方案能覆盖空窗 B 的主场景（捕获后首次 launch、单 stream 顺序回放），不会误报 OOM，稳态零额外检查；
  多 stream 并发 launch 和"分配不释放的 graph"仍可能超出限额，只是超出量受 NVML 事后计入约束（之后的分配会被拒）。
- 它把超限从"静默超出"变成"launch 返回 OOM"，应用会看到错误（与 `cuMemAlloc` 超限同等语义）。
- 尚未实现，待评审：是否接受 launch 返回 OOM 的语义；并发/不释放两种低估是否需要再引入自维护的"活跃 graph 分配"表。
