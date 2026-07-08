# 异步分配 NVML 滞后窗口的 in-flight 预留计数设计

> 本文为 vgpu-manager library(`libvgpu-control.so`)在 **异步显存分配(stream-ordered allocation)场景下收紧显存限额** 的设计稿。现状在同步分配下限额准确;本文只解决 `cuMemAllocAsync` / `cuMemAllocFromPoolAsync` 这一类**异步路径因 NVML 计量滞后而产生的瞬时超分窗口**,且在**不改动同步路径、不引入逐指针账本、不强制流同步**的前提下给出最小改造。**本文档不包含已落地代码**,是后续 design review 与实施的依据。
>
> 关联:async-pool free 的对照分析(HAMi-core PR #217,结论是我们架构性免疫)见对话记录;NVML `usedGpuMemory == N/A` 的独立小修见本文 §8;**捕获处理已在 `fix/stream` 落地**(见 §11.1);**最终决策见 §11**。

## 0. 适用边界(先划清楚)

**本设计只在"容器内负载密集使用异步显存分配、且中间不做 stream 同步"时才有价值。** 大多数负载感知不到这个窗口。

| 场景 | 是否受益 |
|---|---|
| 纯同步 `cuMemAlloc` / `cuMemAllocManaged` | ❌ 不涉及,同步分配返回即落地,下一次闸门读 NVML 已准确 |
| 偶发异步分配、每笔之间有 `cuStreamSynchronize` / kernel 依赖 | ❌ 基本无窗口,NVML 已追平 |
| 训练循环 / CUDA Graph 每 iteration 高频 `cuMemAllocAsync`+`cuMemFreeAsync`,长时间不同步 | ✅ **有真实瞬时超分窗口**,本设计针对此 |

**硬前提:软限额始终是 best-effort。** 物理硬止损永远是驱动自身的 OOM;本设计只是把"软限额被异步窗口绕过"的概率压到可忽略,不承诺字节级精确。

## 1. 背景:异步分配的 NVML 滞后窗口

现状物理显存限额闸门在 [`prepare_memory_allocation`](../library/src/cuda_hook.c#L231):分配**发起前**读一次 NVML 实测占用

```
load_limited_memory_view → get_used_gpu_memory → get_used_gpu_memory_by_device
  → nvmlDeviceGetComputeRunningProcesses/Graphics → accumulate_used_memory(Σ 本容器 PID 的 usedGpuMemory)
```

判定 `used + vmem_used + request > total_memory ? OOM`。

`cuMemAllocAsync` / `cuMemAllocFromPoolAsync` 是**流序异步**:物理占用要等 stream 执行到该点才发生。于是:

1. 进程连续甩出 N 笔异步分配,中间不 `cuStreamSynchronize`;
2. 每一笔闸门读到的 `used` 都还没包含前面几笔(NVML 没采样到);
3. 每一笔都以陈旧的低 `used` 通过闸门 → **在软限额之上瞬时超分**,直到 NVML 采样刷新才被拦回。

根因是**纯 NVML 轮询模型有采样延迟**,而闸门缺一份"已发起但 NVML 尚未体现"的即时账。这与 HAMi-core 用内部 chunk 计数(`add_chunk` 即时权威)相比,是我们用"无逐指针账本 / 不会有 #217 那类 free 未登记指针 bug"换来的代价。

## 2. 为什么不用"分配成功后强制同步"

在 async 调用返回 success 后强制 `cuStreamSynchronize`/`cuCtxSynchronize` 让 NVML 追平——功能上可行,但作为通用方案有三条硬伤,已否决:

1. **打断 CUDA Graph 捕获(硬错误)。** 异步分配自 CUDA 11.4 起可被 stream capture(PyTorch/TF CUDA Graph 大量在用)。在捕获中调用同步 API 直接返回 `CUDA_ERROR_STREAM_CAPTURE_UNSUPPORTED`,凡用 graph 的负载全崩。
2. **抹掉异步语义 → 严重性能回退。** async 存在的意义就是不阻塞 host、允许重叠;每笔后强制 drain 整条流,比普通 `cuMemAlloc` 还差。
3. **容器内自锁串行(注:影响面是容器局部,非跨容器)。** 闸门持 `lock_gpu_device`,但该锁文件是 `/tmp/.vgpu_lock/<device_index>`([hook.h](../library/include/hook.h#L135),`TMP_DIR VGPU_LOCK_DIR`),位于**容器私有 `/tmp`**,只序列化**本容器内**的进程,**不影响其他容器**(每个容器按自己 PID 列表闭环限额、各锁各的)。因此持锁 sync 的危害是容器局部的:把整条 stream drain 塞进持锁临界区,会显著拉长本容器自身的锁等待、拖慢自己的异步流水线(自己拖自己,不殃及邻居);若移到解锁后 sync,记账窗口又照样敞开。故此路仍不可取,但**"跨容器锁饥饿"是早期误述,应更正为容器内自我串行**。

> 综上,强制同步的真正否决理由收敛为第 1、2 条(捕获与性能),第 3 条降级为容器局部的性能问题。

## 3. 为什么不用"逐指针 chunk 账本"

即 HAMi-core / PR #217 那套 `add_chunk`/`remove_chunk_async` 逐指针登记。不采用,因为:

- **`cuMemFreeAsync` 只拿到 `dptr`、拿不到 size**,要按指针精确减账就必须维护 `dptr → bytes` 映射,正是我们当前架构刻意避开的逐指针链表,复杂度和 #217 一类"free 未登记指针"的 bug 面都会回来。
- 我们的物理限额本来就以 **NVML 实测为权威**,不需要一份长期精确的内部账本;只需要一个**短命的桥接账**去覆盖"发起到 NVML 可见"这段延迟。

**结论:预留计数应当是短命的、向 NVML 收敛的桥接账,而不是对称加减的长期账本。** 这直接决定了 §4 的形态。

## 4. 方案:in-flight 预留计数 + NVML 对账

### 4.1 核心思想

维护一个**每设备、每容器**的"在途预留字节数"`pending`,含义严格限定为:**已成功发起异步物理分配、但 NVML 采样尚未体现的字节**。闸门判定改为

```
effective_used = nvml_used + vmem_used + pending
if (effective_used + request > total_memory) → OOM
```

`pending` **只加不按指针减**,靠两条收敛机制归零,保证短命、不长期漂移:

- **NVML 追平即清偿**:一旦 NVML 实测把这些字节采样进来,`pending` 里对应部分立即减掉(避免双算)。
- **TTL 安全阀**:每笔预留带一个单调时钟 deadline,超过 TTL(NVML 必然已刷新的时长)无条件过期,防止进程异常/漏减导致 `pending` 永久虚高。

### 4.2 数据结构(共享内存)

限额是**按容器**(该容器所有 PID 之和)强制的,故 `pending` 必须放**跨进程共享内存**,与现有 `device_vmemory_t`([hook.h](../library/include/hook.h#L315))同段或并列。建议新增并列结构,避免改动既有 `device_vmem_used_t` 的 ABI:

```c
// 每设备保存少量在途预留条目;聚合读取时求和 + 过期清理
typedef struct {
  size_t   bytes;         // 本条预留字节
  uint64_t deadline_ns;   // CLOCK_MONOTONIC 到期时刻;0 表示空槽
  uint64_t nvml_base;     // 预留时刻的 nvml_used 快照,用于 NVML 追平清偿
} pending_resv_t;

typedef struct {
  pending_resv_t entries[PENDING_RESV_SLOTS]; // 环形槽位,建议 64~256
  unsigned int   head;                        // 环形写指针
  unsigned char  lock_byte;                   // 与 device_vmem_used_t 同风格的自旋位
} device_pending_t;

typedef struct {
  device_pending_t devices[MAX_DEVICE_COUNT];
} device_pending_memory_t;   // 与 g_device_vmem 同法映射的独立共享段
```

环形槽满时的策略:**丢弃最旧一条并把它的字节并入一个 per-device `overflow_bytes` 累加项**(该累加项同样受 TTL 统一过期,取最近一次并入时刻为准),保证极端高频下不丢账、只损失一点精度。**槽位数是可调常量**,不是硬限制。

> 若不愿新增共享段,可退而在 `device_vmem_used_t` 尾部追加字段——但那会改既有共享文件布局,需版本号协商(见 §6 ABI 说明),不推荐。

### 4.3 加账时机(仅异步物理成功路径)

**只在异步分配真正走物理路径且驱动返回 success 后加账**,且必须在**持有 `lock_gpu_device` 期间**完成(闸门本就持该锁到 `DONE`,天然一致):

| 路径 | 是否加 `pending` | 说明 |
|---|---|---|
| `cuMemAllocAsync` / `_ptsz` 正常物理成功 | ✅ 加 `request_size` | 目标场景 |
| `cuMemAllocAsync` 走 UVA/oversold 回退(`cuMemAllocManaged` + `malloc_gpu_virt_memory`) | ❌ 不加 | 已进 `vmem_used` 账,再加会**双算** |
| `cuMemAllocFromPoolAsync` / `_ptsz` 成功 | ✅ 加 `request_size` | `allow_uva=0`,恒物理路径,恒加 |
| 同步 `cuMemAlloc` / `cuMemAllocManaged` / `cuArrayCreate` | ❌ 不加 | 返回即落地,无滞后窗口 |
| **stream capture 捕获期内的任何异步分配** | ❌ 不加,且**不闸门、直接透传** | 见 §4.6,捕获期不消耗物理显存 |
| 任何返回非 `CUDA_SUCCESS` | ❌ 不加 | 未占用 |

加账即:在环形槽写入 `{bytes=request_size, deadline_ns=now+TTL, nvml_base=当次闸门读到的 nvml_used}`。

### 4.4 闸门读取与对账

`load_limited_memory_view` 在读 `nvml_used`、`vmem_used` 之后、判定之前,增加一步 `reconcile_pending(host_index, nvml_used)`,在**同一把 `lock_gpu_device` 下**执行:

```
pending_sum = 0
for each live entry e in device_pending[host_index]:
    if now >= e.deadline_ns:                 // TTL 过期
        清空该槽; continue
    if nvml_used > e.nvml_base:              // NVML 已开始体现
        absorbed = min(e.bytes, nvml_used - e.nvml_base)
        e.bytes -= absorbed                  // 清偿已被 NVML 采样的部分
        if e.bytes == 0: 清空该槽; continue
    pending_sum += e.bytes
return pending_sum   // 计入 effective_used
```

`nvml_base` 清偿是保守近似(NVML 上涨可能来自别的分配),但方向安全:**宁可少清偿(多算 pending)→ 偶发早 OOM**,不会少算导致超分。TTL 是最终兜底。

`nvmlDeviceGetMemoryInfo` / `_v2`([nvml_hook.c](../library/src/nvml_hook.c#L47))同样持 `lock_gpu_device`,可复用同一 `effective_used`,让对外汇报的 used/free 与闸门口径一致(可选,默认可只影响闸门)。

### 4.5 与 virt_memory 账本的关系(正交)

| 账本 | 记什么 | 生命周期 | 减账方式 |
|---|---|---|---|
| `g_device_vmem`(virt) | managed/UVA 超卖内存 | 长期,直到 free | `free_gpu_virt_memory` 按 `dptr` 精确减 |
| `pending`(本文) | 异步物理分配的**在途可见性延迟** | 短命(TTL 内) | NVML 追平清偿 + TTL 过期,**不按指针减** |

两者相加进 `effective_used`,互不覆盖。**唯一双算风险点**是异步 UVA 回退——§4.3 已明确该路径不加 `pending`,只进 virt 账,规避。

### 4.6 CUDA Graph 捕获处理(必备约束)

**stream capture 期间,流上的分配只被录制成图节点,捕获那一刻不发生物理占用**;真正的分配延迟到 `cuGraphLaunch`。因此闸门与 `pending` 在捕获期都必须**整体跳过**:

- **不能闸门**:捕获期没消耗显存,对着当前 NVML 做 OOM 判定是提前误判,还会把此刻不需要内存的捕获整失败;
- **不能加 `pending`**:捕获期加账会虚增,且那笔物理分配从未在捕获时发生,NVML 对账永远清偿不掉,反害后续真实分配误 OOM;
- **图 launch 不会重新进入我们的内存 hijack**。`cuGraphLaunch` 本身是被劫持的(见下),但它执行的是驱动内部**预编译好的图节点**,不会再回调 `cuMemAllocAsync` 等公开 API 符号——这正是 CUDA Graph 省掉逐次 API 开销的原因。所以图内的分配节点在 launch 时**拿不到我们的显存闸门**。

实现:在异步分配 hijack 入口用 `cuStreamIsCapturing(hStream, &status)`(或 `cuStreamGetCaptureInfo`)探测,若处于捕获态则**直接透传真实函数**,不进 `prepare_memory_allocation`、不写环形槽。

**关于 `cuGraphLaunch` 的 hook**:我们劫持了 `cuGraphLaunch` 本身,用 `graph_cost_lookup` 估算整张图的 grids/blocks,在 launch 时对**整张图做一次** SM 限速(不是逐 kernel)。但它**只做 SM 限速,不做显存记账**——因为图内分配节点不回调内存 hook,这里也拿不到每个节点的 size。

**残留限制(可接受、自愈)**:图在 launch 时分配的真实 footprint 不会被"即时 gate",但它会体现到 NVML,**下一次普通(非图)分配的闸门自然把它计入并自我纠偏**,窗口有界。此限制与 HAMi-core 同类(均不拦图内分配),记录在案即可。**推论**:若某负载把全部显存分配都塞进图捕获,则显存软限额对它基本失效——实践中框架通常把大缓冲的分配放在图外(或用内存池预分配),故风险低,但需知悉。

### 4.7 配置开关

- 新增配置项(沿用 `g_vgpu_config->compatibility_mode` 或独立 flag),**默认关闭**,opt-in 开启,保证对现网零行为变更。
- TTL、槽位数作为可配参数,给出保守默认(见 §7)。

## 5. 正确性与边界分析

- **安全方向**:任何近似都偏向"多算 pending → 偶发提前 OOM",绝不少算,不会因本机制引入超分。提前 OOM 的负载可通过调大 TTL 之外的余量或申请更多显存缓解。
- **进程崩溃自愈**:持有在途预留的进程异常退出,TTL 到期自动清偿,`pending` 不会永久虚高(优于长期账本的泄漏风险;virt 账本另有 [`check_cleanup_vmem_nodes`](../library/src/loader.c#L1691) 兜底,机制不同但目的一致)。
- **锁开销**:`reconcile_pending` 与加账都在**已持有的** `lock_gpu_device` 内,不引入新锁、不新增持锁时长量级(只是环形槽的常数遍历)。
- **硬止损不变**:软限额被极端时序绕过的残余概率,最终仍由驱动 OOM 兜底。

## 6. ABI / 兼容性说明

- 采用**新增独立共享段** `device_pending_memory_t`(推荐),不触碰既有 `device_vmemory_t` 布局,老版本 server 端无感;新段缺失时 library 视为"未启用"直接跳过,退回现状行为。
- 若走"在 `device_vmem_used_t` 尾部追加字段"的路线,必须给共享文件加版本号并双端协商,风险更高,不推荐。

## 7. 参数选择(TTL 权衡)

| TTL | 风险 |
|---|---|
| 过短(如 <20ms) | NVML 尚未刷新窗口就提前放行 → 保护失效 |
| 过长(如 >1s) | 已 free 的异步分配在 TTL 内仍计入 → 过度保守、误 OOM 偏多 |

建议默认 **100~300ms** 量级(需按目标驱动/NVML 采样周期实测校准),并可配置。槽位默认 **128**,溢出走 `overflow_bytes` 兜底。

## 8. 附:`usedGpuMemory == N/A` 守卫(独立小修)

与本设计正交、可单独先行的一处健壮性修复。[`accumulate_used_memory`](../library/src/cuda_hook.c#L1614) 目前裸加:

```c
*used_memory += pids_on_device[i].usedGpuMemory;   // 无守卫
```

当某进程 `usedGpuMemory == NVML_VALUE_NOT_AVAILABLE`(= `0xFFFFFFFFFFFFFFFF`)时,64 位无符号 `+=` 等价 `-= 1`:

- **有真实占用的混合场景**:该 N/A 进程真实显存被整体丢弃(再 `-k` 字节)→ **低估 → 可超分**;
- **全 N/A 且真实≈0**:回绕成巨值 → 一律 OOM(fail-closed,代价低)。

出现条件:WDDM(Windows,非本项目目标)、部分 NVIDIA vGPU 客户机、某些 MIG/机密计算配置、老驱动。Linux 裸金属整卡切分基本恒有值,现实暴露面低,但代码不安全。

修复:加守卫,并对"该平台确实拿不到 per-process 显存"给出明确语义(fail-closed 当满 / 告警),而非静默回绕:

```c
unsigned long long m = pids_on_device[i].usedGpuMemory;
if (m == NVML_VALUE_NOT_AVAILABLE) { /* 跳过或将该设备判为不可精确计量 */ continue; }
*used_memory += m;
```

## 9. 测试计划

- **单测**:`reconcile_pending` 的 TTL 过期、NVML 追平清偿、环形槽溢出并入 `overflow_bytes`、双算规避(UVA 回退不加 pending)。
- **并发**:多进程同容器共享设备,交叉发起异步分配,校验 `effective_used` 单调一致、无负值/回绕。
- **端到端**:构造"高频 `cuMemAllocAsync` 不同步"负载,对比开关前后是否还能突破软限额;校验 CUDA Graph 捕获负载不受影响(未插入任何同步)。
- **回归**:开关默认关时,所有既有显存限额/汇报单测与行为不变。

## 10. 不做本设计的备选(接受现状)

若评估认为目标负载不含"高频异步分配 + 长时间不同步"模式,可仅落地 §8 的 N/A 守卫,**异步窗口维持现状**——因为它本质是 soft-limit 的固有近似,硬止损由驱动 OOM 保证。是否投入 §4 取决于是否有真实负载证据表明该窗口被触发。

---

## 11. 最终决策(综合捕获处理落地 + nvshare 对照)

### 11.1 捕获处理:已落地,结论固化

`fix/stream` 上 `62bd4ae` → `05a44b7` → `f812200` 三次迭代后,捕获处理已定型,**结论正确**:

| 函数 | 可否被图捕获 | 处理 | 依据 |
|---|---|---|---|
| `cuMemAllocAsync(_ptsz)` / `cuMemAllocFromPoolAsync(_ptsz)` / `cuMemFreeAsync(_ptsz)` | **可**(CUDA 11.4+ 流序分配录成 `CU_GRAPH_NODE_TYPE_MEM_ALLOC/FREE`) | 捕获期透传 | 头文件 `include/cuda-subset.h` 里 `MEM_ALLOC=10`/`MEM_FREE=11` 节点类型即反证 |
| `cuLaunchKernel(Ex)(_ptsz)` | **可** | 捕获期透传 | kernel 是典型可捕获操作 |
| `cuGraphLaunch(_ptsz)` | **可**(嵌套成 child-graph) | 捕获期透传 | — |
| `cuLaunchCooperativeKernel(_ptsz)` / `cuLaunchGridAsync` | **不可**(协作启动捕获返回 `STREAM_CAPTURE_UNSUPPORTED`;后者为废弃 API) | 无需守卫 | 中途一版曾误移除内存函数守卫,`f812200` 已还原 |

**教训固化**:曾出现"内存 Async 不可捕获,所以可移除守卫 + success 后强制同步"的判断,**前提是错的**——内存 Async 在 CUDA 11.4+ 可被捕获,届时真实调用返回 `CUDA_SUCCESS`(录节点),强制同步会触发 `cuStreamSynchronize` → `STREAM_CAPTURE_UNSUPPORTED` → **崩掉捕获**。故:**捕获期只能透传,绝不可在捕获路径注入任何同步。**

**`_ptsz` 入口一致性(已修复,`cdc7a55`)**:`f812200` 一度把 `cuMemAllocAsync_ptsz` / `cuMemFreeAsync_ptsz` 的捕获透传写成 `__CUDA_API_PTSZ(cuMemAllocAsync)`,默认构建(非 PTSZ)下会解析为**非 `_ptsz`** 驱动入口,与同函数 NULL 路径/真实调用用的 `cuMemAllocAsync_ptsz` 不一致。`cdc7a55` 已改回直接调用 `cuMemAllocAsync_ptsz` / `cuMemFreeAsync_ptsz`,与 `62bd4ae` 的原始正确写法对齐,该 nit 已消除。

### 11.2 nvshare 对照:它为什么没有这个问题

nvshare(`grgalex/nvshare`,LD_PRELOAD 劫持)的模型与我们**根本不同**:

1. **全量 UVM**:把所有分配 hook 成 `cudaMallocManaged`(统一内存),用 GPU page fault 让系统内存当 swap,从而**软性超分**;
2. **全 GPU 时间片**:`nvshare-scheduler` 以 FCFS 把**整卡 + 整个物理显存**独占授予**单个**进程,每次至多 TQ 秒;
3. **不做 per-process 显存配额**:只阻止单进程一次性申请超过整卡容量(可 `NVSHARE_ENABLE_SINGLE_OVERSUB=1` 放开),没有硬性限额。

对照 issue #4(问 `cudaMallocAsync` 是否兼容其 managed 替换法):nvshare 的处理思路也是**把 async 分配一并转成 managed**,丢掉 pool 语义换取模型统一。

**关键结论**:nvshare **不存在**我们担心的"异步 NVML 滞后超分窗口",因为它用两招把问题消解掉了——**(a) 时间片消除并发**(一个 TQ 内只有一个进程在跑,没有并发分配去 race 一个瞬时闸门);**(b) UVM 让显存变"软"**(超了就换页到内存,而非撞一个硬闸门)。它**根本不依赖一个精确的即时显存闸门**。

而我们的模型正相反:**并发容器 + 基于 NVML 实测的硬性 per-container 限额**。nvshare 的两招都不能直接搬(我们要的就是并发 + 硬隔离)。但它给出一个强信号:**让异步安全的正道是"降低对精确即时闸门的依赖",而不是"用强制同步去逼精确"。** 而我们其实**已经有半只脚在这条路上**——`cuMemAllocAsync` 在 OOM+超卖时已回退到 `cuMemAllocManaged`([cuda_hook.c](../library/src/cuda_hook.c#L2463) 的 UVA 路径),与 nvshare 的 managed 替换同源。

### 11.3 最终方案(三层,按证据递进)

综合以上,**强制同步彻底出局**(崩捕获 / drain 整流 + 持锁串行 / 窄死锁,三重代价),最终方案分层:

- **第 0 层(必做,已落地/低风险):**
  1. **捕获处理**——保持 `fix/stream` 现状(全部可捕获的 async/launch/graph 透传,协作/legacy 不守卫),顺手修 §11.1 的 `_ptsz` nit;
  2. **N/A 守卫**(§8)——独立小修,防 `usedGpuMemory==0xFFFF...` 回绕导致的低估超分。

- **第 1 层(默认策略:接受软限额近似):** 异步 NVML 滞后窗口**维持现状不额外处理**。理由:它是 soft-limit 的固有近似(nvshare 级别的工具干脆用 UVM+时间片绕开精确闸门),且**自愈**(下次普通分配的闸门读到刷新后的 NVML 即纠偏)、**有硬止损**(驱动 OOM)。这是**推荐默认**。

- **第 2 层(可选加固,opt-in,仅当有真实证据):** 若线上确有"并发 + 高频异步分配 + 逼近上限"的负载被观测到超分,再启用 §4 的 **pending 计数 + NVML 对账**(default off 的配置开关)。它**只在非捕获路径**加账、无性能损失、无死锁,是比强制同步稳得多的收紧手段。

**一句话决策**:捕获处理已正确落地,保持并修 nit;异步窗口**默认接受**(与 nvshare 一类工具的软限额哲学一致,自愈 + 驱动兜底);**强制同步不采用**;`pending` 计数作为**有证据时才开的 opt-in 加固**保留在 §4。
