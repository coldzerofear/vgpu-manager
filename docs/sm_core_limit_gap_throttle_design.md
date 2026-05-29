# 算力切分 GAP 路径节流设计（与 soft_core 弹性模式协同）

> 作用范围：`library/`（LD_PRELOAD 运行时库）。
> 目标：修复"大 kernel 绕过令牌桶导致算力切分失效"的问题，并与现有 `soft_core` 弹性模式协同工作。
> 状态：设计稿（默认关闭，环境变量灰度开启）。

---

## 1. 背景与问题

### 1.1 当前算力切分机制

运行时库通过 LD_PRELOAD 劫持 `cuLaunchKernel` 系列入口，对每次 kernel 下发做令牌桶节流。所有启动点统一为：

```c
rate_limiter(grids, blocks, device);   // 扣减令牌
ret = REAL_LAUNCH(...);                 // 真正下发 kernel
```

- [`rate_limiter()`](../library/src/cuda_hook.c#L308)：把 `g_cur_cuda_cores[]` 减去 `grids`，**只有当桶里令牌已为负时才 `nanosleep` 阻塞**（[line 322](../library/src/cuda_hook.c#L322)）。
- [`utilization_watcher`](../library/src/cuda_hook.c#L380)：后台线程，按约 80ms/设备 的节奏用 NVML 采样真实利用率，经 [`delta()`](../library/src/cuda_hook.c#L332) 反馈控制后用 `change_token()` 补充/回收令牌。

### 1.2 核心 Bug：大 kernel 绕过节流

#### 1.2.1 直接症状

`rate_limiter` 是"发射后不管"。一个大 kernel 只要进来时桶里令牌 ≥ 0,就**永远不会触发阻塞分支**,扣完不管多负也立即放行,在 GPU 上想跑多久跑多久。watcher 要 ~80ms 才采样一次,对单个大的、带同步的 kernel 来说太慢。

典型失效场景(对标 HAMi 商业版测试报告 Task3):`N=8192` 矩阵乘 + `cuCtxSynchronize` 循环 —— NVML/令牌桶模式守不住目标算力占比,实测 SM 利用率冲到 ~100%。

#### 1.2.2 根因:`before < 0` 而不是 `after < 0`

关键在 [line 322](../library/src/cuda_hook.c#L322) 的判断条件:

```c
before_cuda_cores = g_cur_cuda_cores[host_index];
if (before_cuda_cores < 0) {          // ← 检查"扣减前",不是"扣减后"
  nanosleep(&g_cycle, NULL);          //   只有进来时已经欠债才睡
  goto CHECK;
}
after_cuda_cores = before_cuda_cores - kernel_size;   // 无论多大,直接扣
CAS(&g_cur_cuda_cores[host_index], before, after);    // 即使 after 极负也立即返回
```

**只要进来时桶里 ≥ 0,就放行**;扣完是不是负数无所谓。设计上 `rate_limiter` 是一个**"平均消费率"控制器**,假设很多小 kernel 持续来,**靠后续 kernel 替前面的负债买单**。但单个大 kernel + sync + 等待这种模式打破了"持续小 kernel"假设。

#### 1.2.3 具体数字示例(A100,`hard_core=30`)

`g_total_cuda_cores ≈ max_thread_per_sm × sm_num × FACTOR = 2048 × 108 × 32 ≈ 7M`。一发 `N=8192` 矩阵乘 `grids = 524288`:

- 进来时 `before = 7M ≥ 0` → 不睡
- 扣完 `after = 7M − 524288 = 6.5M`,CAS 写回,**立即返回**
- kernel 上 GPU 跑 ~500ms,期间 watcher 周期(~80ms × 6 次)早把令牌补回去了
- 下一次迭代再来:`before` 又是 ~7M,又不睡 —— **永远不被节流**

#### 1.2.4 哪些模式会被绕过 / 哪些不会

| 工作负载模式 | rate_limiter 是否生效 | 原因 |
|---|---|---|
| **单个大 kernel + sync 循环**(本设计目标场景) | ❌ 永远不被 | 进来时桶常满,扣完即放行,kernel 跑期间 watcher 已补满 |
| **连续大量小 kernel**(BATCH/流式推理) | ✅ 正常工作 | 桶被多个小 kernel 逐渐扣到负,后续 kernel 排队 nanosleep |
| **极大 kernel(grids > total)** | ❌ 自身不被 | 同上;自己只付一次 CAS,欠的债"由下个调用者偿还" |

**触发绕过的精确条件**:`g_cur_cuda_cores[host_index] >= 0 && 启动间隔 ≥ watcher 补满所需时间(~80ms)`。这正是同步/等待型工作负载的特征,也正是 GAP 路径精确覆盖的区间。

---

## 2. 当前 hard / soft 两种限速模式

理解结合方案前，必须先厘清现有两种模式。配置在 [`loader.c:2026-2049`](../library/src/loader.c#L2026-L2049) 加载，行为在 [`cuda_hook.c:422-465`](../library/src/cuda_hook.c#L422-L465) 的 watcher 主循环里。

`device_t` 关键字段（[hook.h:161](../library/include/hook.h#L161)）：

| 字段 | 含义 |
|---|---|
| `core_limit` | 总开关：是否启用算力限制 |
| `hard_limit` | 1=硬限速模式；0=软（弹性）模式 |
| `hard_core` | 保底算力占比（0~100） |
| `soft_core` | 弹性上限占比（仅软模式有效，> hard_core） |

### 2.1 配置加载逻辑

```
get_core_limit -> hard_cores
  若 hard_cores > 0:
      core_limit = 1; hard_limit = 1; hard_core = hard_cores
      get_core_soft_limit -> soft_cores
      若 soft_cores > 0 且 soft_cores > hard_cores:
          hard_limit = 0          # 切换为软模式
          soft_core  = soft_cores
  否则:
      core_limit = 0; hard_limit = 0   # 完全不限速
```

### 2.2 watcher 运行期行为

- **hard_limit 模式（`hard_limit==1`）**：`up_limit` 恒为 `hard_core`，令牌按 `delta(hard_core, ...)` 补充，**严格硬顶 hard_core**。
- **软模式（`hard_limit==0` 且 `soft_core>hard_core`）**：
  - **独占 GPU**（`sys_process_num==1`）：`up_limits = soft_core` —— 允许一路冲到 soft_core，吃满空闲算力；
  - **多进程竞争**：从 `hard_core` 起步；每隔 `change_limit_interval` 检测系统空闲，若 `avg_sys_frees*2/interval > usage_threshold`（确有空余），则 `up_limit += hard_core/10`，逐步爬升、上限 `soft_core`；新进程进入时重置回 `hard_core`。

**一句话总结软模式**：保底 `hard_core`，GPU 有空余时弹性突发到 `soft_core`，竞争加剧时回落。

watcher 把"此刻允许用多少"实时写在文件作用域静态数组 [`up_limits[host_index]`](../library/src/cuda_hook.c#L377) 中。

---

## 3. 设计原则：watcher 定策略，GAP 路径做执行

原始 P0 草案在节流时**直接用静态 `hard_core` 作为 duty cycle 目标**，这会在软模式下把大 kernel 摁死在 hard_core，即使 GPU 空闲、watcher 本应放行到 soft_core —— **直接废掉软模式弹性**。

结合方案的核心：**GAP 路径不再读静态 `hard_core`，而是读 watcher 的实时弹性上限 `up_limits[host_index]`**。分工如下：

| 组件 | 角色 | 依据 |
|---|---|---|
| `utilization_watcher` | 策略大脑：决定此刻允许的算力占比（含竞争检测、软模式弹性爬升） | 全局 NVML 视角 |
| GAP 路径（本设计） | 快速执行器：在大 kernel 时间尺度上精确执行 watcher 的当前裁决 | 进程本地 cuEvent 计时 |
| 令牌桶 `rate_limiter` | 稳态执行器：处理密集小 kernel（BATCH） | 现状保留 |

三者互补：大 kernel 走 GAP 路径精确节流；小 kernel 流走令牌桶；弹性/竞争决策始终集中在 watcher。

---

## 4. 范围界定

| 场景 | 本设计（P0） | 后续 |
|---|---|---|
| 空闲 >200ms 后的大 kernel（同步模式） | ✅ cuEvent 计时 + 注入 sleep | |
| 持续小 kernel 流（BATCH） | 维持现有令牌桶 | P1：按批次摊销的 event 计时 |
| 与 soft_core 弹性模式协同 | ✅ 跟随 watcher `up_limits` | |
| 多进程 duty-cycle 协同 | ❌ 各进程各自节流（可能合计偏低） | P2：`/tmp` 共享内存协同睡眠 |
| 默认行为 | **直接启用**，由每设备 `core_limit` 自然门控（限速关闭时仅几次比较的开销，无需环境变量开关） | — |

**异步语义说明**：GAP 路径中的 `gap_end()` 会做一次 `cuEventSynchronize`，使该次启动阻塞主机线程直到 kernel 完成 + sleep 结束。这只在应用空闲 >200ms 后触发（此时应用几乎必然本就在同步），因此安全；BATCH 小 kernel 流不受影响。

---

## 5. 详细设计

### 5.1 新增 include 与全局变量

顶部 include（[line 26 附近](../library/src/cuda_hook.c#L26)）：

```c
#include <time.h>      /* clock_gettime, CLOCK_MONOTONIC */
#include <pthread.h>
```

全局变量（`g_cycle` 之后，[line 79 附近](../library/src/cuda_hook.c#L79)）。无环境变量开关:特性由 `core_limit` 门控;`g_gap_lock` 用 GNU 区间指定初始化器**静态初始化**,因此**无需任何 init 函数**:

```c
/* ---- GAP 路径算力节流（基于 cuEvent 计时的 duty cycle）---------------- */
#define GAP_THRESHOLD_NS   (200LL * 1000000LL)   /* 空闲 >200ms 视为一次 gap */
#define GAP_MAX_SLEEP_MS   500.0                 /* 钳制异常尖峰 */

/* 每设备线程锁:保护进程私有 cuEvent 对与 g_gap_dc 快照,详见 §8。
 * 静态初始化,免去运行期 init。 */
static pthread_mutex_t g_gap_lock[MAX_DEVICE_COUNT] = {
  [0 ... MAX_DEVICE_COUNT - 1] = PTHREAD_MUTEX_INITIALIZER,
};
static CUevent         g_gap_start[MAX_DEVICE_COUNT]  = {0};
static CUevent         g_gap_end[MAX_DEVICE_COUNT]    = {0};
static int             g_gap_evt_ready[MAX_DEVICE_COUNT]  = {0};
static int             g_gap_dc[MAX_DEVICE_COUNT]     = {0};      /* begin->end 一致的 dc 快照 */
static volatile int64_t g_last_launch_ns[MAX_DEVICE_COUNT] = {0};
```

> **头文件改动**：本地 `CUlaunchConfig` 子集([nvml-subset.h](../library/include/nvml-subset.h))原先只声明了 grid/block 维度,缺 `hStream`。为让 `cuLaunchKernelEx` 取到 kernel 所在 stream,补上 `sharedMemBytes` + `struct CUstream_st *hStream`(用底层 tag 拼写,因为 hook.h 在 cuda-subset.h 之前包含 nvml-subset.h;`sharedMemBytes` 使 `hStream` 落在与真实 CUDA 一致的偏移)。该结构体仅用于透传应用指针,扩展声明不改变 ABI。

### 5.2 辅助函数

```c
static inline int64_t monotonic_ns(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (int64_t)ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

/* 懒创建每设备 event 对，需已有 current context（启动点天然满足），调用方持锁 */
static int gap_events_ensure(int host_index) {
  if (g_gap_evt_ready[host_index]) return 1;
  CUresult r1 = CUDA_INTERNAL_CALL(cuda_library_entry, cuEventCreate,
                                   &g_gap_start[host_index], CU_EVENT_DEFAULT);
  CUresult r2 = CUDA_INTERNAL_CALL(cuda_library_entry, cuEventCreate,
                                   &g_gap_end[host_index], CU_EVENT_DEFAULT);
  if (r1 != CUDA_SUCCESS || r2 != CUDA_SUCCESS) {
    LOGGER(VERBOSE, "host %d: gap cuEventCreate failed (%d/%d)", host_index, r1, r2);
    return 0;
  }
  g_gap_evt_ready[host_index] = 1;
  return 1;
}
```

### 5.3 关键：与 soft_core 协同的目标函数

这是结合方案的核心 —— GAP 路径的 duty cycle 目标随模式动态变化：

```c
/* 返回当前生效的 duty cycle 目标百分比：
 *   0    => 不限速（无需节流）
 *   1..99 => 需按此占比节流
 *   >=100 => 当前允许满速突发（不节流）
 * 软模式下跟随 watcher 的实时弹性上限 up_limits[]，从而与 soft_core 协同。 */
static int gap_effective_dc(int host_index) {
  device_t *d = &g_vgpu_config->devices[host_index];
  if (!d->core_limit) return 0;            /* 完全不限速 */
  if (d->hard_limit)  return d->hard_core; /* 硬模式：严格 hard_core */
  /* 软模式：跟随 watcher 当前弹性上限（hard_core..soft_core） */
  int t = up_limits[host_index];
  if (t <= 0) t = d->hard_core;            /* watcher 尚未预热 -> 兜底到保底值 */
  return t;
}
```

> **内存可见性**：`up_limits[]` 原为 watcher 线程私有,本设计首次引入跨线程读(launch 线程)。因此把它从 `static int` 改为 **`static volatile int`**,与 `g_cur_cuda_cores`(同样被 watcher 写、launch 线程读)保持一致 —— `volatile` 强制每次从内存加载(不被寄存器缓存),写方仍是单一 watcher 线程。这是对一个对齐 `int` 的无锁读:在 x86_64/aarch64 上加载原子(无撕裂),且目标值滞后一个 watcher 周期(~80ms)对启发式节流无害。`volatile` 不提供内存序,但此处只读单个独立标量,无需序保证。

### 5.4 gap_begin / gap_end

```c
/* 返回 1 表示进入 GAP 路径，调用方在真正下发后必须调用 gap_end()（此时持有锁）。
 * 返回 0 => 仅走令牌桶。 */
static int gap_begin(int host_index, CUstream stream) {
  if (host_index < 0) return 0;

  int dc = gap_effective_dc(host_index);
  if (dc <= 0 || dc >= 100) return 0;       /* 不限速 / 当前允许满速突发 */

  int64_t now  = monotonic_ns();
  int64_t last = g_last_launch_ns[host_index];
  if (last != 0 && (now - last) < GAP_THRESHOLD_NS) {
    g_last_launch_ns[host_index] = now;     /* BATCH 区间：刷新窗口，交给令牌桶 */
    return 0;
  }

  if (pthread_mutex_trylock(&g_gap_lock[host_index]) != 0) return 0; /* 同设备并发：不叠加 */

  /* 锁内重新快照，保证 begin/end 使用同一个 dc */
  dc = gap_effective_dc(host_index);
  if (dc <= 0 || dc >= 100) { pthread_mutex_unlock(&g_gap_lock[host_index]); return 0; }
  g_gap_dc[host_index] = dc;

  if (!gap_events_ensure(host_index) ||
      CUDA_INTERNAL_CALL(cuda_library_entry, __CUDA_API_PTSZ(cuEventRecord),
                         g_gap_start[host_index], stream) != CUDA_SUCCESS) {
    pthread_mutex_unlock(&g_gap_lock[host_index]);
    return 0;
  }
  return 1;
}

/* 记录结束标记 -> 测量 GPU 用时 -> 按 duty cycle 注入 sleep；总会刷新时间戳并释放锁。 */
static void gap_end(int host_index, CUstream stream, CUresult launch_ret) {
  if (launch_ret == CUDA_SUCCESS &&
      CUDA_INTERNAL_CALL(cuda_library_entry, __CUDA_API_PTSZ(cuEventRecord),
                         g_gap_end[host_index], stream) == CUDA_SUCCESS &&
      CUDA_INTERNAL_CALL(cuda_library_entry, cuEventSynchronize,
                         g_gap_end[host_index]) == CUDA_SUCCESS) {
    float gpu_ms = 0.0f;
    if (CUDA_INTERNAL_CALL(cuda_library_entry, cuEventElapsedTime, &gpu_ms,
                           g_gap_start[host_index], g_gap_end[host_index]) == CUDA_SUCCESS
        && gpu_ms > 0.0f) {
      int dc = g_gap_dc[host_index];                 /* begin 时锁内快照的目标占比 */
      double sleep_ms = (double)gpu_ms * (100.0 / (double)dc - 1.0);
      if (sleep_ms > GAP_MAX_SLEEP_MS) sleep_ms = GAP_MAX_SLEEP_MS;
      if (sleep_ms > 0.0) usleep((useconds_t)(sleep_ms * 1000.0));
    }
  }
  g_last_launch_ns[host_index] = monotonic_ns();
  pthread_mutex_unlock(&g_gap_lock[host_index]);
}
```

**Sleep 公式**：设实测忙时 `gpu_ms`、目标占比 `dc%`，周期 = `gpu_ms/(dc/100)`，空闲（sleep）= `gpu_ms·(100/dc − 1)`。
例：`dc=30`、kernel 跑 50ms → sleep `50·(3.333−1) ≈ 117ms`，周期内 GPU 占用 ~30%。

### 5.5 各启动点接入（统一三行包裹）

以 [`cuLaunchKernel`（line 1831）](../library/src/cuda_hook.c#L1831) 为范例：

```c
  int host_index = get_host_device_index_by_cuda_device(device);   // + 捕获索引
  rate_limiter(gridDimX * gridDimY * gridDimZ,
               blockDimX * blockDimY * blockDimZ, device);
  int gap = gap_begin(host_index, hStream);                        // +
  ret = CUDA_ENTRY_CHECK(cuda_library_entry, __CUDA_API_PTSZ(cuLaunchKernel), f, /*...*/);
  if (gap) gap_end(host_index, hStream, ret);                      // +
```

已接入的 9 个启动点及各自传入的 stream（`cuLaunch` / `cuLaunchGrid` / `cuLaunchGridAsync` 走 `goto CALL` 模式，需在函数顶部加 `int gap = 0;`）：

| 启动点 | stream |
|---|---|
| `cuLaunchKernel_ptsz` | `hStream` |
| `cuLaunchKernel` | `hStream` |
| `cuLaunchKernelEx` | `config->hStream` |
| `cuLaunchKernelEx_ptsz` | `config->hStream` |
| `cuLaunchCooperativeKernel_ptsz` | `hStream` |
| `cuLaunchCooperativeKernel` | `hStream` |
| `cuLaunch` | `CU_STREAM_LEGACY` |
| `cuLaunchGrid` | `CU_STREAM_LEGACY` |
| `cuLaunchGridAsync` | `hStream` |

无 init 函数:`g_gap_lock` 静态初始化,event 对在首次进入 GAP 路径时懒创建。

---

## 6. 协同行为矩阵

| 模式 / 状态 | watcher `up_limits` | `gap_effective_dc` | GAP 路径行为 |
|---|---|---|---|
| 不限速（core_limit=0） | — | 0 | 不介入 |
| 硬模式 | hard_core | hard_core | 严格守 hard_core |
| 软模式·独占·soft_core≥100 | soft_core(≥100) | ≥100 | 不节流，满速突发 ✓ |
| 软模式·独占·soft_core<100 | soft_core | soft_core | 守 soft_core（吃空闲）✓ |
| 软模式·竞争 | 在 hard_core↔soft_core 间弹性 | 当前弹性值 | 跟随 watcher 收紧/放宽 ✓ |

结论：GAP 路径完全继承 soft_core 的弹性语义 —— 空闲时放行突发，竞争时收紧，决策权始终在 watcher。

---

## 7. 正确性与边界

- **线程安全**：每设备 `g_gap_lock` + `trylock`；同设备并发只有一个线程测量，其余回退令牌桶，无 event 复用竞争。
- **begin/end 目标一致**：`dc` 在 `gap_begin` 锁内快照到 `g_gap_dc[]`，`gap_end` 复用，避免期间 watcher 改 `up_limits` 造成不一致。
- **懒创建 event**：失败降级为令牌桶，绝不崩溃。
- **CUDA 图捕获保护**：进入 GAP 路径前用 `cuStreamIsCapturing` 检查 stream;若正在捕获图(或查询失败),跳过事件注入回退令牌桶 —— 否则 `cuEventRecord` 会被捕获(延迟执行),后续 `cuEventSynchronize`/`ElapsedTime` 将失败甚至破坏捕获。该符号用 `CUDA_FIND_ENTRY` 守卫:缺失说明驱动早于 CUDA Graphs,无需保护。(正常捕获时启动是背靠背的、不触发 gap;仅捕获中途 >200ms 暂停才会命中此分支。)
- **dc 守卫**：`dc<=0`（不限速）与 `dc>=100`（满速突发）在任何 cuEvent 操作前短路。
- **首次启动**：`last==0` 视为 gap，立即开始计时。
- **启动失败**：`gap_end` 在 `launch_ret!=CUDA_SUCCESS` 时跳过测量，仍释放锁、刷新时间戳。
- **尖峰钳制**：`GAP_MAX_SLEEP_MS` 防止误测导致长时间卡死。
- **内存可见性**：跨线程共享量按职责分两类 ——（1）`up_limits`、`g_last_launch_ns` 用 `volatile`(watcher/launch 跨线程、对齐标量、无锁、与 `g_cur_cuda_cores` 同约定)；（2）`g_gap_dc`、`g_gap_evt_ready`、`g_gap_start/end` **始终在 `g_gap_lock` 内访问**,由互斥锁的 acquire/release 保证可见性,无需 `volatile`。详见上文与 §8。
- **fork() 子进程安全**:GAP 路径全局(`g_gap_evt_ready`/`g_gap_start/end`/`g_gap_lock`/`g_last_launch_ns`)以及外层的 `g_init_set` `pthread_once_t` 都通过 `pthread_atfork(NULL, NULL, child_after_fork)` 在子进程中复位,详见 §7.1。**注册位置**:`initialization()` 函数内(由 `pthread_once` 保证一次性),**故意不用 `__attribute__((constructor))`** —— 库加载期(`.init_array`)与 libvulkan/libGLX_nvidia/libcuda 的动态链接器初始化同窗口,在那里碰 pthread/glibc 内部锁是已知崩溃模式(参见 [HAMi-core PR #182](https://github.com/Project-HAMi/HAMi-core/pull/182) Step C 的 ICD init 崩溃,以及本仓 `check_no_constructors.sh` 在 CI 强制禁止)。这与 [HAMi-core PR #199](https://github.com/Project-HAMi/HAMi-core/pull/199) 处理的是同一类继承问题,我们的修复额外覆盖 GAP 路径特有的 cuEvent 句柄(父 CUcontext 失效)与 mutex 状态(父线程持锁瞬间 fork → 子永久 EBUSY)。回归用例:[test/test_fork_inherit.cu](../library/test/test_fork_inherit.cu)。

### 7.1 fork() 后的状态重置详解

`pthread_atfork` 的 child handler 在子进程里(此刻只有调用 fork 的那一个线程)执行:

| 重置项 | 父继承值 | 不重置的后果 | 重置后效果 |
|---|---|---|---|
| `g_init_set = PTHREAD_ONCE_INIT` | DONE | 子进程下次 launch 跳过 `initialization()` → watcher 线程不会重新 `pthread_create`(fork 只复制调用线程)→ 控制器永远不再运行 | 下次 launch 触发 `pthread_once` → 重跑 `initialization()` → 重新启 watcher |
| `g_gap_evt_ready[i] = 0` | 1 | `gap_events_ensure` 跳过懒创建 → 用父 CUcontext 失效的 CUevent 句柄 → `cuEventRecord` 失败 | 重新懒创建子进程自己的 event 对 |
| `g_gap_start/end[i] = NULL` | 父 CUcontext 句柄 | 同上 | 干净起点 |
| `pthread_mutex_init(&g_gap_lock[i])` | 任意(可能"已加锁") | 若 fork 时父线程正持锁 → 子永久 `EBUSY` → GAP 路径永久跳过(沉默退化) | 锁清零 |
| `g_last_launch_ns[i] = 0` | 父时间戳 | 仅 gap 检测时序略偏 | 无害,清掉求一致 |

不需要重置的:
- `g_sm_controller`(函数指针):父子地址空间布局同(同代码段),指针有效
- `g_sm_controller_kind` / AIMD 参数:从 env 读,父子同 env → 同值
- watcher 内部数组 `shares/sys_frees/up_limits/...`:子进程重启 watcher 时,watcher 自己 init 循环里全部清零

### 7.2 loader.c 的 fork 危害

`child_after_fork()` 还会调用 `loader_child_after_fork()`(定义在 loader.c)处理 4 个 loader 级 mutex 与一个线程键值缓存,它们都有相同的"父持锁瞬间 fork → 子永久 EBUSY"问题:

| 资源 | 用途 | 风险等级 |
|---|---|---|
| `init_config_mutex` | 保护 `load_controller_configuration` —— **每次 launch hook 入口都会跑到这里**(经 `load_necessary_data`) | **最高**:绝对会触及,父持锁瞬间 fork 必死锁 |
| `tid_dlsym_lock` | 保护 `tid_dlsyms` 缓存,每次 dlsym 拦截都要拿 | 高:worker 线程频繁触发 |
| `device_index_mutex` | 保护 `cuda↔nvml↔host` 设备索引查找,被多处调用 | 中:每次需要做索引转换都要拿 |
| `g_memory_node_lock` | 保护 vmem 节点账本 | 中:仅 vmem 路径触及 |
| `tid_dlsyms[]` 缓存 | 按 `pthread_t` 键值的 dlsym 递归保护表 | 低:陈旧条目仅是 cache miss,但理论上 `pthread_t` 复用可能假阳性匹配 |

`loader_child_after_fork()` 用 `pthread_mutex_init` 对 4 个 mutex 重新初始化,清空 `tid_dlsyms[]` 数组。被 `pthread_atfork` 通过 cuda_hook.c 的统一入口调用。

**注意不重置的 loader.c 内部状态**:
- `g_cuda_ver_init`/`g_cuda_lib_init`/`g_nvml_lib_init`/`init_nvml_host_index`(loader 的 4 个 `pthread_once_t`):它们守护的是**库加载/版本探测**这类系统级幂等结果,通过 mmap/dlopen 父子共享,跳过等价于"复用已有结果",**正确**。
- `init_config_changed_pid`(`static volatile pid_t`):`load_controller_configuration` 用它做 PID-比对去重(`init_config_changed_pid == getpid()`),fork 后子进程的 `getpid()` 自然不等于父的值 → 自动触发重 init,**设计上已 fork-safe**。
- signal/atexit handler:每次 init 重注册,父子各自处理自己的 PID 清理,**正确**。
- **与令牌桶共存**:GAP kernel 仍经 `rate_limiter` 扣减令牌(只是不阻塞);watcher 照常补充。`core_limit` 关闭时 GAP 路径在首行短路,BATCH 行为零变化。两条路径有清晰分工(见下"双重节流分析"):令牌桶 = 累积消费率控制;GAP = 单发占比控制。
- **双重节流分析**(单次启动是否会同时被 rate_limiter 睡眠 + GAP sleep):
  - **单线程稳态(常见):基本不会**。GAP 一旦触发,`gap_end` 戳 `last_launch_ns = now`,下一次启动若马上来,`now - last < 200ms` → BATCH 路径不再进 GAP。期间 sync + sleep(~百毫秒级)足够 watcher 把令牌补满,`rate_limiter` 也不睡。GAP 在"第一次空闲后"触发一次,后续靠令牌桶兜底,**两者时间维度上接力,不在同一次启动叠加**。
  - **多线程对抗(罕见):可能**。线程 A 抢先下大 kernel 把桶扣到极负、kernel 还在 GPU 跑;线程 B 进来 `rate_limiter` 见 `before<0` → 多个 10ms 周期 nanosleep,等 watcher 补回。期间 wall-clock 过去几百 ms,B 接着 `gap_begin`:`last_launch` 已 >200ms → 进 GAP 路径 → sync + sleep。B 这一次启动确实被双重节流。
  - **即使发生也不是 bug,只是延迟更高**:`rate_limiter` 的睡为的是把桶还到正常水位(代表 A 已经过度消费的份额);GAP 的睡为的是 B 自己这一发大 kernel 的未来占比平衡。两笔分别约束**过去的累积透支**和**未来的瞬时占比**,本来就应该叠加,不重复。GPU 在 B 的 sleep 期间仍能给别的进程/线程使用。
  - **稳态会自然消散**:`gap_end` 戳了 `last_launch`,B 后续启动落入 BATCH;B 实际只跑了限速后的少量 kernel,桶不再持续负。

---

## 8. 锁的作用域决策:线程锁 vs 进程锁

> 设计评审中提出过一个关键问题:`g_gap_lock` 是 `pthread_mutex`(线程级),在**多进程共享同卡**时跨不了进程;为什么不直接用 [`lock.c`](../library/src/lock.c) 里已实现的 `lock_gpu_device`(基于文件的进程锁)?以下是结论与依据,留档备查。

**结论:保留 `g_gap_lock`(线程锁),不在 GAP 临界区套进程锁。** 二者保护的不是同一对象,进程锁在这里既无法替代线程锁,也解决不了多进程算力协同,反而会损害性能。

### 8.1 `g_gap_lock` 保护的是进程私有的 CUDA 对象

GAP 临界区保护的共享状态是 `g_gap_start[]` / `g_gap_end[]` 这对 cuEvent 及 `g_gap_dc[]` 快照。cuEvent 是在**本进程自己的 CUDA context** 中由 `cuEventCreate` 创建的句柄,对其他进程没有意义(每个进程有独立 context、独立句柄)。

需要互斥的真实竞争是:**同一进程内两个线程**同时向同一对 event `cuEventRecord` → 句柄状态错乱。这是线程作用域问题。`lock_gpu_device` 是文件锁,**拦不住同进程的第二个线程**,因此它**无法替代** `g_gap_lock`;即使叠加它,线程锁仍不可省。

### 8.2 跨进程没有内存安全问题,无需任何进程锁

GAP 路径在跨进程之间**没有共享可变状态**(events、`g_gap_dc`、`g_last_launch_ns` 均为进程内 `static`),因此**不存在跨进程数据竞争**。进程锁不是为了安全,唯一可能的理由是协同算力占比 —— 而这恰恰用错了工具。

### 8.3 互斥锁会破坏 duty cycle,因为算力切分要"交错"而非"互斥"

- **锁跨整个 sleep 持有**:进程 A 占锁 → 跑 50ms → sleep 117ms → 释放;A 的 70% 空闲时间变成**全 GPU 死时间**,B 在 A 睡眠期间无法进入。时间片共享退化为"串行 + 空转",GPU 利用率塌陷,且每进程只得 `hard_core/N` 而非 `hard_core`。**严格更差。**
- **锁只跨 measure+launch、sleep 前释放**:把跨进程 kernel 执行串行化,破坏 GPU/MPS 本有的并发执行,徒增延迟,测出的 `gpu_ms` 也不再反映真实并发耗时,且仍无 duty cycle 协同。

用互斥去解比例共享,方向相反。

### 8.4 GAP 路径已具备收敛的跨进程感知(免费)

- **软模式**:`gap_effective_dc` 读 `up_limits`,而 watcher 靠 NVML 看到**整卡**利用率(`sys_current`/`sys_frees`),竞争时自动下压 `up_limits`,目标随之收紧。
- **任意模式**:竞争下本进程 kernel 变慢 → 实测 `gpu_ms` 变大 → sleep 更久 → **自我抑制更强**,收敛而非发散。
- **硬模式**:每进程各自卡在自身 `hard_core` 本就是硬限速的正确语义(per-process 保证,余量交 GPU 调度器分)。

故多进程下无正确性 bug,仅存在**精度**不足。

### 8.5 精确多进程协同应建在已有的共享内存账本上,而非 `lock_gpu_device`

阻塞版跨进程锁其实已存在:[`device_util_write_lock`](../library/src/lock.c#L137) 使用 `F_SETLKW`,作用在 mmap 共享文件 `device_util_t`(`g_device_util`,cuda_hook.c 已 `extern`)的**字节范围锁**上。这套"**共享内存 + 字节级阻塞锁**"正是 HAMi 商业版 `cudevshr.cache` 的同构物,也是 P2 的正确底座:各进程把 `(目标占比, 最近忙时)` 发布到共享区,据**聚合需求**各算 sleep —— 这是基于共享账本的**协同**,正是比例共享所需;而 `lock_gpu_device`(5s 超时的整卡互斥锁)是给"必须全局独占的短临界区"(如显存分配记账)设计的,不应包裹"kernel 执行 + 长 sleep"。

### 8.6 决策矩阵

| 关注点 | 采用 | 理由 |
|---|---|---|
| 保护进程内 cuEvent 对 | **`g_gap_lock`(线程锁)** | event 是进程私有 CUDA 对象,进程锁管不到 |
| 跨进程内存安全 | 不需要任何锁 | GAP 路径无跨进程共享可变状态 |
| 跨进程算力比例协同(P2) | `device_util_t` 共享内存 + `F_SETLKW` 字节锁做**账本协同** | 比例共享要交错,互斥会塌掉利用率 |

---

## 9. 验证计划

1. **限速关闭无副作用**：`core_limit=0` 时 GAP 路径在 `gap_begin` 首行即返回（仅几次比较），`gpu-burn` 基线无变化。
2. **硬模式·同步模式（核心目标）**：`N=8192` 矩阵乘 + `cuCtxSynchronize` 循环，`hard_core=30`，预期 SM 利用率稳定 ~30%（当前 ~100%）。
3. **软模式·独占**：`hard_core=30, soft_core=100`，单进程跑大 kernel，预期不节流、可吃满空闲。
4. **软模式·竞争**：两进程 `hard_core=30, soft_core=80`，预期各自随 watcher 在 30%↔80% 间弹性，合计不长期超卖。
5. **BATCH 不受影响**：大量微小 kernel、无 >200ms 间隔，不进入 GAP 路径，占用仍由令牌桶控制。
6. **多进程局限确认**：两进程各 `hard_core=30`，确认 P0 已知局限（合计可能偏低、各自自洽），为 P2 共享内存协同明确缺口。
7. **回归用例**：[test/test_runtime_launch_gap.cu](../library/test/test_runtime_launch_gap.cu) 刻意制造"大 kernel + >200ms 空闲 + sync"循环,**确定性地驱动 `gap_begin`→`gap_end`**(现有 `test_runtime_launch.cu` 背靠背启动停留在 BATCH 路径,覆盖不到此路径),校验结果数值正确、无崩溃/死锁/锁泄漏。需 GPU + `LD_PRELOAD`;不断言占比(那需配置 + NVML 采样)。

---

## 10. 可调参数

| 项 | 默认 | 说明 |
|---|---|---|
| GAP 判定阈值 `GAP_THRESHOLD_NS` | 200ms | 调小更激进，调大更保守 |
| 最大 sleep 钳制 `GAP_MAX_SLEEP_MS` | 500ms | 防尖峰卡死 |
| 门控 | `core_limit`（每设备） | 无环境变量开关；限速开启即生效 |

---

## 11. 后续路线

- **✅ 已落地 — AIMD watcher 控制器**(详见 [sm_controller_aimd.md](sm_controller_aimd.md)):与 GAP 路径**正交、互补** —— GAP 解大 kernel 同步模式下的瞬时偏差,AIMD 解 watcher 稳态围绕目标的高方差(参考 Midokura 实测数据:Stock MAE ~20% → AIMD MAE ~3%)。`CUDA_SM_CONTROLLER=aimd` 启用,默认 `delta`(stock)。
- **P1 — BATCH 路径**：对密集小 kernel 按批次摊销 event 计时，替代纯令牌桶，提升小 kernel 场景的占比精度。
- **P2 — 多进程协同**：复用已有的 `device_util_t` mmap 共享内存 + `F_SETLKW` 字节范围锁（参见 §8.5；对标 HAMi 商业版 `cudevshr.cache`），各进程发布 `(目标占比, 最近忙时)` 到共享账本，据聚合需求各算 sleep，使同卡合计 duty cycle 收敛到目标。注意：这是**账本协同**而非整卡互斥（§8.3）。
- **P3 — 反馈融合**：将 GAP 实测的真实 GPU 用时回灌给 watcher，减少其对 NVML 采样的依赖与抖动。
