# 容器内多进程算力隔离：共享令牌桶设计

> 作用范围：
> - `library/` —— 新增 `sm_node` 共享区；`vmem_node` 加冻结区头 + 重建语义（§10）。
> - `pkg/deviceplugin/vgpu`、`pkg/kubeletplugin` —— `/tmp/.sm_node` 按容器挂载 + 启动前清理（§4.5）。
> - `pkg/config/vmem`、`pkg/metrics/lister` —— 跟随 `vmem_node` 布局变更（§10.3）。**这是本设计唯一的跨语言 ABI 面**。
>
> 目标：修复"同容器多进程各自持有私有令牌桶、瞬时叠加突破算力限额"的问题，做到**聚合限额严格**且**热路径不引入锁 / 不串行化 kernel 发射**。
>
> 状态：**设计定稿**（已拍板，见 §12；未实现；默认关闭，环境变量灰度开启）。
> 基线：已对齐 `main` @ `2f234ef`（2026-07-24 同步，见 §0）。所有行号引用均按该基线校准。
> 关联：[GAP 路径节流](./sm_core_limit_gap_throttle_design.md)、[AIMD 控制器](./sm_controller_aimd.md)。

---

## 0. 与 `main` 的同步记录（2026-07-24，基线 `2f234ef`）

本文档初稿写于 `b73f1cb`。此后 `main` 前进了 37 个提交，本节记录**重新核对的结论**：哪些前提仍然成立、哪些需要改写。

### 0.1 结论：核心前提全部成立

`main` 的改动集中在**三块与本设计正交的区域**——显存记账（`cuMemAlloc*`/`cuMemFreeAsync`/graph capture）、`dlsym`/`cuGetProcAddress` 路由、`gpuallocator` NVLink 拓扑。逐一核对：

| 本设计依赖的事实 | 现状 |
|---|---|
| `dev_hot_t` / `g_dev_hot[]` 的形状与 `static` 存储（[L109-114](../library/src/cuda_hook.c#L114)） | **未变** |
| `rate_limiter` 的 CAS 循环（[L606](../library/src/cuda_hook.c#L606)） | **未变** |
| `change_token` 的 CAS 累加与钳制（[L567](../library/src/cuda_hook.c#L567)） | **未变** |
| 三种控制器与其跨周期积分态（`md_cooldown`、排他 FSM ×3、`throttled_since_watch`） | **未变**，§4.13 的全部论证原样成立 |
| watcher 单设备周期 ≈ 100ms（`100 / dev_count * MILLISEC` × `dev_count`） | **未变**，§12 第 8 项的 `REFILL_PERIOD_NS` 取值不需要改 |
| bypass 的 SET 语义（[L1260](../library/src/cuda_hook.c#L1260)） | **未变**，§4.7 仍是必改项 |
| `mmap_file_to_vmem_node` 的 TOCTOU + 尺寸不符报错（[L1563](../library/src/loader.c#L1563)/[L1597](../library/src/loader.c#L1597)） | **未变**，§10 的改造目标原样成立 |
| `device_vmemory_t` 无区头、`lock_byte` 偏移由 `offsetof` 推出 | **未变**，§10.2/§10.3 的改造点不变 |
| `ofd_fcntl` 的 OFD→经典锁运行时回退（[lock.c#L64](../library/src/lock.c#L64)） | **未变**，`lock.c` 零改动 |
| `.so` 按版本挂载 ⟹ 一个容器一生只加载一个版本（§4.5.4 的安全论证） | **成立，且两条路径都已核实**：device plugin [vnum_plugin.go#L492](../pkg/deviceplugin/vgpu/vnum_plugin.go#L492)、DRA [vgpu.go#L193-194](../pkg/kubeletplugin/vgpu.go#L193) 都把 `version.Get().Version` 拼进宿主路径 |

⟹ **§1–§9、§11 的技术判断无一被推翻。** 除行号外，仅需补充下面三处新事实。

### 0.2 需要补入的新事实

1. **`vmem_node` 现已 feature-gate 化**：新增 `util.VMemoryNode` 特性门控与 `VMEMORY_NODE_ENABLED` 环境变量，库侧由 `g_vgpu_config->vmem_node` 决定是否建区。这给本设计的开关（§5）提供了一个**可对照的既有范式**，也改变了 §10 的适用条件 —— 详见 §5.1 与 §10.6。
2. **库内新增了 `vmem_node` 的 PID 存活回收与退出/信号清理钩子**（`rm_vmem_node_by_non_existent_device_pid`、`check_cleanup_vmem_nodes*`、`atexit` + `SIGTERM/SIGINT/SIGHUP`）。这是第三条陈旧清理机制，本设计**一条都不需要**，但必须写清楚"为什么不需要"，否则后人会照抄 —— 详见 §4.5.5。
3. **`loader_child_after_fork` 现在会释放 fork 继承的显存记账链表**（[loader.c#L2635](../library/src/loader.c#L2635)）。§6.2 "不需要动 fork 处理器" 的结论不变，但理由需要精确化 —— 详见 §6.2。

---

## 1. 背景与问题

### 1.1 现状：令牌桶是进程私有的

运行时库通过 LD_PRELOAD 劫持 `cuLaunchKernel` 系列入口，对每次 kernel 下发做令牌桶节流：

```c
rate_limiter(grids, blocks, host_index);   // 扣令牌，桶为负则 nanosleep
ret = REAL_LAUNCH(...);
```

令牌桶与配套热态放在 [`g_dev_hot[]`](../library/src/cuda_hook.c#L114)：

```c
typedef struct {
  volatile int64_t cur_cuda_cores;  /* 令牌桶，每次发射 CAS 扣减 */
  volatile int64_t last_launch_ns;  /* gap 检测，每次发射打戳     */
} __attribute__((aligned(CACHELINE_SIZE))) dev_hot_t;

static dev_hot_t g_dev_hot[MAX_DEVICE_COUNT];   // ← static：每个进程一份
```

`rate_limiter` 用 CAS 扣减（[cuda_hook.c#L606](../library/src/cuda_hook.c#L606)），watcher 每 ~80ms/设备用 NVML 采样、经 `delta()`/`aimd()` 反馈后用 `change_token()` 补充。控制器积分态同样是进程私有的 static：[`shares[]`](../library/src/cuda_hook.c#L1118)、[`up_limits[]`](../library/src/cuda_hook.c#L1140)、`is[]`、`avg_sys_frees[]`。

### 1.2 核心问题：N 个私有桶瞬时叠加突破限额

容器里启动 N 个计算进程时，每个进程有**自己的** `g_dev_hot[]`。同一时刻 N 个 `rate_limiter` 各自看自己的桶，都可能判定"令牌够，放行"，于是 N 份令牌被同时消费、N 批 kernel 被同时发射 —— GPU 上实际叠加，**瞬时利用率可达单进程限额的 ~N 倍**。

需要澄清一个**容易被夸大的点**：watcher 采样的 `user_current` 是**容器聚合利用率**（把容器内所有 PID 的 util 累加，见 `get_used_gpu_utilization` 里的 `check_device_pid_in_ordered_container_pids` 聚合逻辑）。这意味着 N 个控制器**观测的是同一个共享反馈信号**：聚合 util 一旦超限，每个进程的 `delta` 都会砍自己的 share，总吞吐随之下降。所以：

- **稳态均值仍收敛到限额**（不是"完全失效"）；
- 真正的损害是 **N 个相同控制器盯同一信号、同步涨同步砍 → 等效增益放大 ~N 倍**，叠加"N 个桶可被同时抽干"，表现为**瞬时突发放大 ~N 倍、限额附近振荡幅度 ~N 倍**。

准确结论：**多进程下限流"变松、变抖"，而非失效。** 这决定了本设计是"按需的严格化"，不是"救火"。

### 1.3 适用场景

- **单进程容器（ollama / llama-server / 单个训练进程）**：问题**不存在**，本设计**收益为零**（但因默认关闭，也无代价）。
- **多进程容器**：问题存在，程度随 N 与突发性上升。典型场景：
  - **notebook 容器（Jupyter）** —— **本设计的首要驱动场景**。每个 kernel 一个独立进程、同容器多 kernel 并发是其常态用法，且用户随时新开/重启 kernel，进程数动态变化。这是最典型的多进程 CUDA 容器，**必然受影响**。
  - 多路并发推理服务、多进程训练、DataLoader 多 worker 各开 CUDA context。

> **关于"要不要做"**：本设计初稿曾把"阶段 0 量化验证"设为硬前置。该前置**已被撤销**（§12 第 1 项）：多进程竞争同一 GPU 必然产生超限与限制不准，notebook 场景更是必受影响——这是可从机制推出的结论，不需要先测。§7 的测量因此**从"准入门槛"降级为"验收基线"**：仍然要测，但目的是**验证修复效果**，而不是决定做不做。

---

## 2. 设计目标与非目标

### 2.1 目标

1. **聚合限额严格**：让"容器还能发多少 kernel"成为一个**物理不变量**（一个共享计数器），而非 N 个私有桶的统计平均。
2. **低开销**：**热路径（每次发射）维持"一条 CAS"的量级，不引入任何锁、不串行化发射**。初始化路径（每进程一次）允许用内核文件锁，见 §4.4。
3. **鲁棒**：任一进程崩溃不得卡住其它进程；热路径无临界区可持有；初始化锁由内核在进程死亡时自动释放。
4. **fork 安全**：不新增会"父持锁 fork → 子死锁"的隐患。
5. **永不致命**：共享区任何异常（建不出、映射失败、布局不符）都**不得报错退出**，一律**重建或优雅降级回进程私有桶**，见 §4.4/§4.5.4。
6. **兼容性**：不引入比仓库现有依赖更新的内核/编译器要求，见 §4.11。
7. **可灰度**：环境变量开关，默认关闭（保持现有进程私有桶行为）。

### 2.2 非目标

- **不做每进程独立限额**（HAMi 商业版 Event 模式那种"A、B 各限 50%、合计 100%"语义）。我们是**容器聚合**语义，见 §3.3 的对比。
- **不引入 Event/占空比模式**（cuEvent 计时 + duty-cycle）。那是另一条正交路线，需要同步、伤异步流水线，单独评估。
- 本设计**不改变控制算法**（delta/aimd 不动），只改"桶与积分态的存储位置 + 谁来补充"。

---

## 3. 方案：共享令牌桶 + CAS 消费 + 每周期选举补充

### 3.1 一句话

把 `g_dev_hot[]`（桶）与控制器积分态搬进**容器内 `MAP_SHARED` 共享内存**，消费端 CAS 扣减**保持不变**，补充端用**每周期 CAS 抢权**保证"每周期恰好一个进程补充"。

### 3.2 为什么这是对的手段

- **CAS 是 CPU 指令、与地址空间无关**：桶从 `static` 变成 `MAP_SHARED`，[rate_limiter 的 CAS](../library/src/cuda_hook.c#L606) **一个字都不用改**就变成跨进程原子扣减。这是本方案最省力、也最关键的支点。
- **"聚合限额"变成物理不变量**：N 个进程抢同一个 `cur_cuda_cores`，桶里有多少令牌就是全容器还能发多少 kernel。不需要瓜分限额、不需要回收空闲配额 —— 桶本身就是聚合。
- **无锁、不串行化**：消费仍是无阻塞 CAS；只有"桶为负"时各进程各自 `nanosleep`（现有逻辑），不是互相排队。

### 3.3 与 HAMi 商业版两种做法的对比

| 维度 | HAMi 锁串行化 | HAMi Event + `sleeping` 协调 | **本方案（共享桶 + CAS）** |
|---|---|---|---|
| 限额语义 | 聚合 | **每进程**（合计 = N×限额） | 聚合 |
| 每次发射开销 | 锁/解锁，争用陷内核 ~μs | 无（但要 cuEvent 同步） | **1 条 CAS（≈现状）** |
| 并行性 | **串行化，吞吐塌** | 并行 | 全并行 |
| 崩溃语义 | 持锁猝死 → **全容器死锁** | 标记残留 → 可自愈 | 无临界区 → **自愈** |
| 抗空转（配额回收） | 无 | 靠 `sleeping` 广播主动回收 | **共享桶天然回收**（A 不取、B 自然取走） |

关于 HAMi 的 `sleeping` 字段：它**不是锁、不串行化发射**，而是**抗空转**——每进程独立限额下，A 睡时 GPU 会空，于是广播"我睡了、你们上"让 B 错峰填充。**在聚合语义 + 共享桶下，这个协调是白送的**：A 不消费令牌，B 自然就消费走了，无需 `sleeping` 字段、无需扫描兄弟。故本方案**不移植** `sleeping`。

---

## 4. 详细设计

### 4.1 共享内存布局

复用仓库已有的跨进程共享范式（[`mmap_file_to_vmem_node`](../library/src/loader.c#L1563)：`open(O_CREAT)` + `ftruncate` + 首建者 `memset` + `mmap(MAP_SHARED)`），新增一个 SM 令牌桶共享区。

**这个结构体是一份 ABI**：它落在文件上、被多个进程映射、跨库版本存活（见 §4.5），宿主侧的 Go 代码也已有读取同类结构的先例（[container_lister.go#L206](../pkg/metrics/lister/container_lister.go#L206) 读 vmem_node）。因此字段一律用**定宽类型 + 显式 padding + `_Static_assert` 钉死布局**。

```c
/* 容器内路径 /tmp/.sm_node/sm_node.config。
 * 该目录不是容器自己的 /tmp，而是由 device plugin / DRA 驱动按容器挂入的
 * 专用读写目录（与 /tmp/.vgpu_lock、/tmp/.vmem_node 同构），见 §4.5。 */
#define SM_NODE_DIR       "/.sm_node"
#define SM_NODE_FILE_PATH (TMP_DIR SM_NODE_DIR "/sm_node.config")

/* 文件大小是一个【永久常量】，与 sizeof(region) 解耦：版本升级改结构体时
 * 文件尺寸不变 → 永远不需要 ftruncate 改大小 → 就地重建即可（§4.4）。
 * 当前用量 128 + 16*128 = 2176B，保留 8KiB（2 页）留足余量。 */
#define SM_NODE_FILE_SIZE 8192

#define SM_NODE_MAGIC          0x534D4E44U      /* "SMND" */
#define SM_NODE_LAYOUT_VERSION 1U               /* 改结构体必须 +1，见 §4.5.4 */

/* 无 volatile、无 _Atomic：全部访问走 __atomic_* / CAS 内建，见 §4.10。
 *
 * 【范围】这里放的是【全部跨周期控制状态】，不只是令牌桶。依据见 §4.13：
 * 凡是“只有选举赢家推进”的状态都必须共享，否则各进程各存一份 → 各自只在
 * 自己赢的周期推进 → 语义碎裂。每周期重算的临时量（top_results、sys_free、
 * 排他 memo）不在此列，保持进程私有。 */
typedef struct {
  /* 每设备一格，缓存行对齐防伪共享（沿用 dev_hot_t 的 128B 对齐约定）。 */
  int64_t  cur_cuda_cores;    /* 令牌桶：消费者 CAS 扣，补充者累加        */
  int64_t  total_cuda_cores;  /* g_total（= thread*sm*FACTOR），首建者写   */
  int64_t  last_refill_ns;    /* 补充选举戳：CAS 抢每周期补充权            */
  int64_t  share;             /* 对应现 shares[]                          */
  /* ↓ 控制器积分态：只有当周期选举赢家读写 → 被选举天然串行化            */
  int32_t  up_limit;          /* 现 up_limits[]（soft 弹性；GAP 路径跨线程读）*/
  int32_t  is_cnt;            /* 现 is[]（soft）                          */
  int32_t  avg_sys_free;      /* 现 avg_sys_frees[]（soft）               */
  int32_t  pre_external_proc; /* 现 pre_external_process_nums[]           */
  int32_t  md_cooldown;       /* 现 g_aimd_md_cooldown[] —— AIMD 必需(§4.13.2) */
  int32_t  excl_debounced;    /* 现 g_is_exclusive_debounced[]     ┐ 排他 FSM  */
  int32_t  excl_streak;       /* 现 g_exclusive_pending_streak[]   │ (§4.13.3) */
  int32_t  lost_excl_pending; /* 现 g_lost_exclusivity_pending[]   ┘           */
  /* ↓ 热路径写：rate_limiter 节流时置位，watcher 每周期 read-and-clear。
   *   共享后语义从“本进程是否节流”正确地变为“容器内是否有人节流”(§4.13.4) */
  int32_t  throttled_since_watch;
  uint8_t  _pad[CACHELINE_SIZE - 72];
} __attribute__((aligned(CACHELINE_SIZE))) sm_node_dev_t;

typedef struct {
  /* ┌── 冻结区：这 16 字节是【永久 ABI】，任何版本都不得改动其类型/顺序/偏移。
   *   │  布局守卫要在“还不知道对方是哪个版本”时读它们，所以它们必须先于
   *   │  一切版本差异而存在。改动它们 = 守卫失效 = 读到垃圾。          */
  uint32_t magic;             /* SM_NODE_MAGIC                            */
  uint32_t layout_version;    /* SM_NODE_LAYOUT_VERSION                   */
  uint32_t region_size;       /* sizeof(sm_node_region_t)                 */
  uint32_t device_count;      /* MAX_DEVICE_COUNT                         */
  /* └── 冻结区结束。以下字段随 layout_version 自由演进。                 */
  uint8_t  _pad[CACHELINE_SIZE - 16];
  sm_node_dev_t devices[MAX_DEVICE_COUNT];
} sm_node_region_t;

_Static_assert(sizeof(sm_node_dev_t) == CACHELINE_SIZE, "sm_node_dev_t must be one cacheline");
_Static_assert(offsetof(sm_node_region_t, devices) == CACHELINE_SIZE, "region header must be one cacheline");
/* 结构体永远不得超出保留尺寸；超了就是设计事故，编译期拦下。 */
_Static_assert(sizeof(sm_node_region_t) <= SM_NODE_FILE_SIZE, "region must fit the reserved file size");
/* 冻结区的偏移永久锁死。 */
_Static_assert(offsetof(sm_node_region_t, magic) == 0, "magic must stay at offset 0");
_Static_assert(offsetof(sm_node_region_t, layout_version) == 4, "frozen header ABI");
```

> `initialized` 三态字段已删除：初始化由 §4.4 的内核文件锁串行化，`magic` 本身就是"已初始化"的标记，不需要额外字段和自旋等待。

> **注意**：`last_launch_ns`（gap 检测）**不在**这里——它保持进程私有，见 §4.6。`total_cuda_cores` 各进程算出的值相同（由设备属性决定），放共享区只是为了"首建者算一次、其余读"，也避免各进程重复 NVML 查询。

### 4.2 消费端（rate_limiter）：几乎零改动

现有 [rate_limiter](../library/src/cuda_hook.c#L583) 的 CAS 循环逻辑**不变**，只把操作对象从 `g_dev_hot[host_index].cur_cuda_cores` 换成共享区 `g_sm_node->devices[host_index].cur_cuda_cores`（开启共享模式时）：

```c
before = g_sm_node->devices[host_index].cur_cuda_cores;  // 跨进程原子读
if (before < 0) { metrics_record_rate_limit_hit; nanosleep(&g_cycle); goto CHECK; }
after = before - kernel_size;
while (!CAS(&g_sm_node->devices[host_index].cur_cuda_cores, before, after));
```

- N 个进程并发 CAS 扣同一计数器 → **物理串行的原子扣减**，不会超发。
- 桶为负 → 各进程各自 `nanosleep` 重试（现有逻辑），**不是互相排队**。

### 4.3 补充端（watcher）：每周期 CAS 抢补充权（核心正确性）

**问题**：N 个进程各有一个 watcher，若都补充 → **N 倍过量供给 → 限额松 N 倍**，比现状更糟。

**解法**：不选 leader（要处理选举、失效检测、故障转移），而是**每周期靠 CAS 抢权**——谁抢到谁补充：

```c
/* watcher 每周期，对每个 host_index： */
int64_t now  = monotonic_ns();
int64_t last = region->last_refill_ns;
if (now - last >= REFILL_PERIOD_NS &&
    CAS(&region->last_refill_ns, last, now)) {
    /* 本周期补充权归我：读积分态 → 跑 delta/aimd → 累加 change_token */
    region->share = g_sm_controller(up_limit, user_current, region->share, host_index);
    change_token_shared(region, region->share);   // 累加到 cur_cuda_cores，见 §4.7
    /* up_limit/is_cnt/avg_sys_free 的 soft 弹性更新也在此块内 */
} else {
    /* 没抢到 → 本周期不补充，只做本进程自己的采样/日志 */
}
```

- **无 leader、无失效检测、自愈**：谁先到谁补；补充者本周期后崩溃，下周期 `now - last` 再次超阈值，别的进程自然抢到。
- **积分态只有赢家读写** → 天然被选举串行化，无需额外锁；仅需 acquire/release 内存序（`__atomic_load_n`/`__atomic_store_n` with `__ATOMIC_ACQUIRE`/`RELEASE`）。
- `REFILL_PERIOD_NS` ≈ 现有 watcher 单设备周期（~80–100ms）。多个 watcher 采样节奏可能错开，抢权只保证"每 period 至多补一次"，采样值用赢家自己的（聚合 util 与采样进程无关，见 §4.9）。

### 4.3.1 修正：采样权与补充权分离（实施期重大调整）

> **本节推翻了 §4.3 的一个隐含前提。** §4.3 只解决了"谁补令牌"，默认"每个进程各自采样"是可接受的。实施期评审指出这不成立，理由有二：

1. **N× NVML 开销**：每进程每设备每 ~100ms 调一次 `nvmlDeviceGetComputeRunningProcesses` + `nvmlDeviceGetProcessUtilization`。而 `get_gpu_process_from_local_nvml_driver` 里**既有注释自己就写着**"Frequent calls to nvmlDeviceGetProcessUtilization may result in the return of NVML_ERROR_NOT_FOUND, which is a normal phenomenon"——N 个进程恰好把这个已知会退化的调用频率乘以 N，而 N 最大的场景正是 §1.3 的首要目标 notebook。
2. **采样相位抖动**：赢家逐周期换进程，控制器输入的相位随之跳变。（澄清一个**没有**发生的问题：窗口是固定回看 1 秒的墙钟窗口 `now - 1s`，不是"自上次采样以来"的累积水位线，所以不存在"t1 看前 1 秒、t2 看前 2 秒"。窗口长度一致，只有相位差。）

#### 判据：锁管采样（软），CAS 管补充（硬）

| 机制 | 职责 | 强度 |
|---|---|---|
| `last_refill_ns` 的 CAS | **谁能补令牌** | **硬**，始终生效 |
| 每设备 1 字节的 `fcntl` 记录锁 | **谁去采样** | **软**，失效只退化成多采几次 |

**正确性绝不依赖锁**，这是本次修正最关键的设计取舍。因为锁有两个 CAS 没有的失效模式：持有者**活着但卡死**（锁不释放，内核帮不了你），以及 fork 共享（见下）。拆开之后：双持有者 → 仍不可能双补；持有者卡死 → 采样戳陈旧超 3 个周期，待机者自采自补（仍过 CAS 限速）；锁完全不可用 → 自动退回 §4.3 的每周期竞选。

#### 待机者的每周期成本

**1 次非阻塞 `fcntl` + 1 次共享内存读，零 NVML 调用。** 不阻塞、不自旋、不轮询睡眠。接管延迟 ≤1 周期（~100ms），与 §4.3 每周期竞选的最坏情况**完全相同**，没有退化。

> 为什么不用"待机者阻塞在 `F_OFD_SETLKW` 上、被内核唤醒"：`balance_batches` 让**一个 watcher 线程管一批设备**，线程不可能既阻塞等设备 1 的锁、又继续当设备 0 的 leader。真要做需要每设备一个专职待机线程。当前接管延迟已与旧方案持平，不值得。

#### 必须按设备竞选

各进程的 `CUDA_VISIBLE_DEVICES` 可能不相交（进程 a 只见 GPU0/1，进程 b 只见 GPU2/3），不存在"一个进程包揽所有设备"的可能。锁范围取 `l_start = host_index, l_len = 1`，每设备一字节、互相独立。这之所以成立，是因为 **`host_index` 由 UUID 解析**（`get_host_device_index_by_uuid`），与进程可见性无关——这同时也是共享区按 `host_index` 索引的正确性前提。

#### 采样主对补充权有优先级（实施期追加）

两把"钥匙"是分开发的：采样权靠**文件锁**（粘性），补充权靠 `last_refill_ns` 的 **CAS**（每周期重抢）。若两者完全对称，**谁补令牌就只取决于各进程 watcher 的相位**——某个待机者的周期恰好落在阈值之后，它就会拿走补充权，并用采样主**上一周期**发布的样本跑控制器；而采样主刚采到的新样本要等下一轮才被用上。

这从不导致错误（样本本身是 NVML 的 1 秒均值，且既有执行滞后已有 200–400ms），但它让"这次补充用的是哪份样本"取决于进程启动顺序，看 trace 时很难解释。

**做法**：给两者不同的抢占阈值。

```
采样主：now - last >= SM_REFILL_PERIOD_NS         (90ms)
待机者：now - last >= SM_REFILL_PERIOD_NS * 2     (180ms)
```

稳态因此变成"**谁采样谁立刻补充**"，待机者退回它本来的角色——只在采样主整整多错过一个周期时才兜底。**接管路径不受影响**：采样主若**死亡**则锁被内核释放，待机者在**同一周期内**先 `sm_sampling_claim` 拿到锁转正、随即以 1x 阈值抢占，所以 2x 只作用于"活着但卡死"和转正前的极窄窗口。

> **⚠️ 一个反直觉的陷阱（实施时踩到并修复）**：判据**不能**只写 `g_sm_sampling_mine[i] ? 1x : 2x`。当 `g_sm_lock_fd < 0`（锁文件不可用、降级模式）时**没有任何进程是采样主**，那样写会让**所有人都用 2x** → 全容器补充速率**减半** → 桶被饿死。正确判据是 `(g_sm_lock_fd < 0 || g_sm_sampling_mine[i])`：没有所有权概念时，退回全员 1x（即 §4.3 的原始每周期竞选）。用例 [5] 专门锁死这一点。

### 4.3.2 陈旧判定必须自适应，不能用固定阈值（实施期修正）

初版把待机者的陈旧阈值写死为 `3 × 90ms = 270ms`，**这个假设是错的**，因为它默认 watcher 的单设备周期恒为 ~100ms。

**watcher 的周期并不恒定。** 看 `utilization_watcher` 的节拍逻辑：它按绝对时间栅格 `clock_nanosleep(TIMER_ABSTIME)` 睡到 `next_wakeup`，但**一旦单次处理超出自己的时间片**，`remaining_ns < MIN_WATCHER_SLEEP_NS(10ms)` 就退化为固定睡 10ms。于是：

```
单次迭代耗时 = 处理耗时 P + 10ms
单设备周期  = dev_count × (P + 10ms)      ← 一个线程管一批设备
```

`dev_count=4`、NVML 采样慢到 `P=100ms` 时，**单设备周期变成 440ms**。

**固定 270ms 阈值的后果是灾难性的、且是正反馈**：待机者**每个周期都判定陈旧** → 全部退回自行调用 NVML → N 倍负载让 NVML 更慢 → 周期更长 → 更加陈旧。**集中化收益归零，而且恰好在机器已经吃紧时失效得最彻底。**

#### 解法：一切以"采样主的实测节拍"为单位，不用绝对毫秒

陈旧判定要回答的是"**采样主是否已经停了**"，而不是"采样主是否有我假设的那么快"。固定阈值把这两件事混为一谈。

先确立**一个**基准量，其余全部表述为它的倍数：

```c
/* watcher 的【设计】单设备周期。这不是拍脑袋：循环在每个设备前睡
   wait = 100ms/dev_count，一趟访问 dev_count 个设备，所以无论一批里有几个
   设备，单设备都是每 100ms 被回访一次。 */
#define SM_WATCHER_NOMINAL_PERIOD_NS (100ms)

owner_cadence = clamp(采样主发布的实测间隔, 1×nominal, 10×nominal)

采样主补充阈值 = 0.9 × nominal          // 略低于一拍，避免抖动吃掉一整个周期
待机者补充阈值 = 2   × owner_cadence    // "采样主漏了一整拍"
陈旧判定阈值   = 3   × owner_cadence    // "采样主漏了两拍"
```

**为什么下界取 nominal 而不是另一个常数**：采样主不可能比设计节拍更快，而"尚未发布过间隔"（值为 0）也落在同一分支——把未知节拍当成无限快，会让启动期的待机者一触即发。**一条规则同时处理了下界和冷启动。**

**为什么上界取 10×nominal**：超过这个倍数，watcher 已经把自己的设计契约违背了一个数量级；再继续按比例放宽，一次病态间隔（机器挂起、时钟异常、被调度器饿了几秒）就能把阈值顶到再也不会触发的地方。这个界**由 nominal 推导**，不是凭空的 5 秒。

> **顺带修掉一个同源的隐患**：待机者的**补充阈值**原本也是写死的 `2 × 90ms`。采样主若合法地以 440ms 节拍运行，待机者每 180ms 就会把补充权抢走——"谁采样谁补充"会**悄悄退化成只对快采样主成立**。现在它同样以 `owner_cadence` 为单位。

于是：健康但慢的采样主**被信任**；真正停摆的采样主**仍然会在它自己的 3 拍内被发现**。用例 [6] 锁死这一点，并显式断言"固定阈值会拒绝 440ms 的采样主、自适应阈值接受"。

#### 采样结果必须发布（否则引入新 bug）

集中采样后待机进程不再调 NVML，其 `top_results` 会永久陈旧 → **N-1 个进程的利用率 metrics 与 DETAIL 日志全部失真**。所以 leader 必须把 `user_current`/`sys_current`/`sys_process_num`/`external_process_num` 发布进共享区（§4.1 的 `s_*` 字段，`layout_version` 因此升到 2），待机者读它。

### 4.4 建区 / 重建：初始化路径用内核文件锁串行化

**不照抄 vmem 区的写法**。现有 [`mmap_file_to_vmem_node`](../library/src/loader.c#L1563) 有两个缺陷，本设计都不继承：

1. **TOCTOU 竞态**：`file_exist()` 判断在前、`open(O_CREAT)` 在后，两个进程可能双双认定 `created = 1`，双双 `ftruncate` + `memset` → **互相抹掉对方刚写的内容**。
2. **尺寸不符即报错退出**（[loader.c#L1597](../library/src/loader.c#L1597)）——正是本节要根治的行为，见 §4.5.4。

#### 4.4.1 为什么初始化路径可以用锁（而热路径绝不）

§2.1 的"无锁"约束**只针对热路径**。初始化每进程仅一次，用一把**内核文件锁**把"建/校验/重建"整段串行化，可以一次性消掉建区竞态、尺寸竞态、重建竞态**三类问题**，代价是 2 次 syscall / 进程。

**这与被否决的 HAMi 锁有本质区别**（§3.3）：

| | HAMi 的共享内存互斥锁 | 本设计的初始化文件锁 |
|---|---|---|
| 位置 | **每次 kernel 发射**（热路径） | 进程初始化，**一次** |
| 持锁者猝死 | 锁永久残留 → **全容器死锁** | **内核自动释放** → 无残留 |
| 对发射的影响 | 串行化，吞吐塌 | **零**（热路径不碰它） |

即"持锁猝死 → 死锁"这条否决 HAMi 的理由，**对内核文件锁不成立**——进程死亡时内核无条件回收其文件锁。

#### 4.4.2 复用仓库现成的 OFD 兼容范式

不新造原语。[`lock.c#L64`](../library/src/lock.c#L64) 已有一个**优先 OFD、运行时回退经典 POSIX 锁**的封装，直接复用：

```c
/* lock.c:64 现成 —— 优先 OFD 锁(Linux >= 3.15)，内核不支持则 EINVAL 回退经典锁 */
static int ofd_fcntl(int fd, int wait, struct flock *fl) {
  int ret = fcntl(fd, wait ? F_OFD_SETLKW : F_OFD_SETLK, fl);
  if (ret != -1 || errno != EINVAL) return ret;
  return fcntl(fd, wait ? F_SETLKW : F_SETLK, fl); /* legacy kernels */
}
```

回退到经典 POSIX 锁**对本用途完全够用**：经典锁的弱点是"同进程的 fd 之间不互斥"，而我们的初始化本就跑在 `pthread_once` 之下，进程内已经串行；跨进程互斥经典锁照样提供。

#### 4.4.2a 用【阻塞锁】而非非阻塞自旋（关键选择）

初始化锁用 **`F_OFD_SETLKW`（阻塞，`wait=1`）**，而**不是** `lock_gpu_device` 那种"非阻塞 + 退避自旋 + 超时"。这是刻意的，理由是**避免空转**：

> 首建者 A 正在建区时，后到的 B 应该**在内核里睡着**等 A 建完，被唤醒后拿到锁、发现 `magic` 已有效 → 不重建、直接解锁走人。全程 B **不消耗 CPU**。若用非阻塞自旋，B 会反复 `fcntl` + `nanosleep` 空转，纯属浪费。

**为什么这里能安全地阻塞（而 `lock_gpu_device` 不能）**——差别在"持锁者会不会卡住"：

| | `lock_gpu_device`（非阻塞自旋 + 10s 超时） | 初始化锁（纯阻塞，无超时） |
|---|---|---|
| 临界区里做什么 | 真实设备分配逻辑，**可能耗时/受外部影响** | 只有 `ftruncate`+`memset`+几个字段写，**微秒级** |
| 会不会无限卡住 | 有可能 → **必须有超时上限**兜底 | **不可能**——临界区内没有任何会无限阻塞的调用 |
| 争用热度 | 热、频繁 | 每进程一次，稳态无争用 |

初始化临界区里**没有 CUDA 调用、没有网络/管道 I/O、没有会永久阻塞的 syscall**（`ftruncate`/`memset`/字段写在一个几 KB 的文件上都是有界的），所以持锁者**不存在"卡住不放"的失败模式**。等待者最坏只等"一次建区的时长"（微秒级），因此**不需要超时**——加超时反而要把刚省掉的自旋逻辑重新引进来。

**阻塞不违反"永不致命"（§2.1 第 5 条）**：
- 降级回私有桶针对的是"**映射不出来**"（`open`/`mmap` 失败），是**硬错误**；阻塞等的是"另一个进程正在建区"，是**正常的短暂等待**，两回事。
- 持锁者若在临界区内**崩溃**，内核**无条件释放**其文件锁（§4.4.1）→ 阻塞的等待者立即被唤醒、接手重建（§4.5.4 的重建是幂等的）。所以阻塞**不会**变成永久卡死。
- 唯一能让阻塞变长的是"持锁者活着但卡住"，而上一段已论证这在初始化临界区内**不可能发生**。

> 对照仓库：`lock.c` 里 `ofd_fcntl(fd, 1, ...)`（阻塞）已用于 [vmem/sm-util 的记录锁](../library/src/lock.c#L245)（读者/写者应等待而非放弃），`ofd_fcntl(fd, 0, ...)`（非阻塞）用于 `lock_gpu_device` 的自旋。本设计的初始化锁归入前者，**与既有惯例一致**。

#### 4.4.2b 锁打在 config 文件本身，不新建锁文件、不锁目录

初始化锁的载体是 **`sm_node.config` 这个文件自身**——同一个 fd 既建区、又加锁、又 `mmap`。**不引入任何独立的 `.lock` 文件**（零额外 inode、零清理负担），与 `vmem_node.config` 的既有做法一致（记录锁打在共享文件内，无独立锁文件）。

**为什么不能拿父目录 `/tmp/.sm_node` 当锁**：仓库用的是 `fcntl` 记录锁（`ofd_fcntl`），而 `F_WRLCK`（排他）**要求 fd 可写打开**，目录无法以写方式打开（`open(dir, O_RDWR)` → `EISDIR`），只能 `O_RDONLY` + `F_RDLCK`（共享读锁，给不了互斥）。`flock(2)` 虽能锁 `O_RDONLY` 的目录 fd，但那是**新原语**（违背 §4.4.2 / §4.11 的"复用 `ofd_fcntl`、不引入新依赖"），且 flock 与 fcntl 混用是经典坑，换不来任何好处。

> **⚠️ 这条结论的适用边界（实施期补充，别外推）**：以上只对**用完即释放的初始化锁**成立。§4.3.1 的**采样权锁要持有整个进程生命周期**，取舍完全反过来，必须用**独立的 `sm_node.lock` 文件**，理由有二：
>
> 1. **经典 POSIX 记录锁会在进程关闭该文件的任意一个 fd 时被丢弃**，而 `map_sm_node_region` 初始化时正好 open 后 close 了 `sm_node.config`。共用一个文件 → leadership 可能**静默蒸发**，两个进程同时以为自己拥有采样权。OFD 锁没这个问题，但 §4.11 明确承诺经典锁回退可用，所以必须按最弱假设设计。
> 2. **锁文件绝不能进启动前清理**（§4.5.2）。删锁文件是破坏互斥的经典方式：新进程 `open(O_CREAT)` 拿到**新 inode**，锁是按 inode 的，两个进程在不同 inode 上持锁 = 完全不互斥。`sm_node.config` 可以删（我们要的就是全新区），`sm_node.lock` 不行。

**锁整个文件**（`l_start=0, l_len=0`）即可：`sm_node` 运行期是纯 CAS、**没有按设备的记录锁**（它没有 Go 读者，不像 vmem 需要一致快照），所以初始化时锁全文件不会与任何其它锁范围冲突。

#### 4.4.3 建区/重建流程

```c
/* 每进程一次，在 pthread_once 之下调用；任何一步失败都 → 降级私有桶，绝不 exit */
fd = open(SM_NODE_FILE_PATH, O_RDWR | O_CREAT | O_CLOEXEC, 0644);  /* 无 TOCTOU */
if (fd < 0) return FALLBACK_PRIVATE;

/* wait=1：阻塞锁。后到进程在内核里睡等首建者，不空转（§4.4.2a）。
 * 临界区只有 ftruncate+memset+字段写，微秒级、不会卡住，故无需超时。 */
ofd_fcntl(fd, /*wait=*/1, &(struct flock){.l_type = F_WRLCK, ...});

fstat(fd, &sb);
if (sb.st_size != SM_NODE_FILE_SIZE)          /* 新建(0) 或 异常尺寸 */
    ftruncate(fd, SM_NODE_FILE_SIZE);         /* 空洞读作 0 → magic 必然不符 → 下面重建 */

region = mmap(NULL, SM_NODE_FILE_SIZE, PROT_READ|PROT_WRITE, MAP_SHARED, fd, 0);
if (region == MAP_FAILED) { unlock; close; return FALLBACK_PRIVATE; }

if (!header_valid(region))                    /* 全新 / 老版本 / 损坏，见 §4.5.4 */
    rebuild_region_locked(region);            /* 就地重建，不删文件、不改尺寸 */

ofd_fcntl(fd, 1, &(struct flock){.l_type = F_UNLCK, ...});
close(fd);                                    /* mmap 在 close 后依然有效 */
```

要点：

- **`open(O_RDWR|O_CREAT)` 无条件调用**，不做 `file_exist` 预判 → 消除缺陷 1 的 TOCTOU。"谁是首建者"这个问题**根本不需要回答**：尺寸不对就 `ftruncate`，`magic` 不对就重建，两者都在锁内且幂等。
- **文件尺寸恒为 `SM_NODE_FILE_SIZE`**（§4.1）→ 版本升级不改尺寸 → **永远不需要为兼容而 resize**，也就没有"别的进程映射着旧尺寸被 SIGBUS"这一类问题。
- **`close(fd)` 不影响已建立的 mmap**（映射持有独立引用），所以锁的生命周期严格限制在初始化段内，不残留。
- 全零的新文件天然 `magic != SM_NODE_MAGIC` → 走 `rebuild_region_locked` → **首建与重建是同一条路径**，无需分支、无需 `created` 标志。

### 4.5 共享区的供给与陈旧清理：由控制面负责

> **本节两次推翻了先前设计。** 初稿用库内 `generation`（容器实例代号）清理残留；二稿改为库内布局守卫 + 可选 ns `st_ino` 探测。定稿是：**陈旧清理交给控制面**（插件在容器启动前删除缓存文件），库内只保留一层极廉价的布局守卫兜底。理由是控制面**本来就已经在为 `vmem_node` 这么做**——库内探测是在重造一个已经存在、且更可靠的轮子。

#### 4.5.1 目录供给：由插件按容器挂载，不用容器自己的 `/tmp`

共享区**不能**放在容器自己的 `/tmp`：那里可能被别的 hostPath 覆盖挂载、可能只读、可能被业务清理。沿用 `/tmp/.vgpu_lock`、`/tmp/.vmem_node` 的既有做法，由插件挂一个**专用读写目录**进去：

| | 容器内路径 | 宿主路径 |
|---|---|---|
| 既有 | `/tmp/.vgpu_lock` | `<host_manager_dir>/<pod-uid>_<cont-name>/vgpu_lock` |
| 既有 | `/tmp/.vmem_node` | `<host_manager_dir>/<pod-uid>_<cont-name>/vmem_node` |
| **新增** | **`/tmp/.sm_node`** | **`<host_manager_dir>/<pod-uid>_<cont-name>/sm_node`** |

**命名理由**：与 `vmem_node`（显存隔离的跨进程状态）**对称**——`sm_node` 即算力隔离的跨进程状态，文件 `sm_node.config` 对应 `vmem_node.config`。曾考虑按机制命名（`shm_node` / `vgpu_shm`），但 `vmem_node` 本身也是共享内存区，按机制命名会产生歧义；按**内容**命名才自解释。常量按既有惯例放置：

```go
// pkg/util/consts.go（与 VMemNode / VMemNodeFile 并列）
SMNode     = "sm_node"
SMNodeFile = "sm_node.config"
// pkg/deviceplugin/vgpu/vnum_plugin.go（与 ContVMemoryNodePath 并列）
ContSMNodePath = "/tmp/." + util.SMNode
```

需要落点的位置（均与 `vmem_node` 逐处并列）：

| 路径 | 位置 | 动作 |
|---|---|---|
| device plugin | [vnum_plugin.go#L853](../pkg/deviceplugin/vgpu/vnum_plugin.go#L853) `Allocate` 的 `response.Mounts` | 加挂 `sm_node` 目录 |
| device plugin | [vnum_plugin.go#L844](../pkg/deviceplugin/vgpu/vnum_plugin.go#L844) `EnsureDir` | 建 `sm_node` 目录 |
| DRA（CDI） | [vgpu.go#L289](../pkg/kubeletplugin/vgpu.go#L289) `GetPartitionMountContainerEdits` | 加 CDI mount |
| DRA（NRI） | [vgpu.go#L346](../pkg/kubeletplugin/vgpu.go#L346) `GetNRIPartitionInjection` | 加 NRI mount |
| DRA 两路共用 | [vgpu.go#L141](../pkg/kubeletplugin/vgpu.go#L141) `ensurePartitionDirectories` 的 `preparedDirs` | 建 `sm_node` 目录 |
| NRI 观测 | [nri/plugin.go#L73](../pkg/kubeletplugin/nri/plugin.go#L73) `mountDestsOfInterest` | 加 `/tmp/.sm_node`（仅日志高亮） |

> **`main` 同步补充**：上表 6 行落点（5 处功能 + 1 处观测）在同步后**逐处复核有效**，位置仅有行号漂移。`vnum_plugin.go` 的挂载块现在把 `vgpu.config` / `vgpu_lock` / `vmem_node` 三个 Mount 写在同一个 `append` 里（[L853-870](../pkg/deviceplugin/vgpu/vnum_plugin.go#L853)），`sm_node` 应作为第四项并列加入；对应的 `EnsureDir` 在 [L844](../pkg/deviceplugin/vgpu/vnum_plugin.go#L844) 旁边。DRA 两路（CDI [vgpu.go#L330](../pkg/kubeletplugin/vgpu.go#L330)、NRI [vgpu.go#L371](../pkg/kubeletplugin/vgpu.go#L371)）的 `util.VMemNode` 拼接点同理各加一项。

#### 4.5.2 陈旧清理：复用现成的"每次启动前删缓存"钩子

**关键事实：这套机制已经在跑，不是新发明。** 两处都已经在删 `vmem_node.config`：

`PreStartContainer`（device plugin 路径，[vnum_plugin.go#L1116](../pkg/deviceplugin/vgpu/vnum_plugin.go#L1116)）：

```go
// Clean up old cache files before each startup
pidsConfigPath := filepath.Join(configDirPath, registry.PidsConfig)
vmemNodeConfigPath := filepath.Join(configDirPath, util.VMemNode, util.VMemNodeFile)
_ = os.RemoveAll(pidsConfigPath)
_ = os.RemoveAll(vmemNodeConfigPath)      // ← sm_node.config 加在这里
```

其可靠性由 [vnum_plugin.go#L228](../pkg/deviceplugin/vgpu/vnum_plugin.go#L228) 的 `PreStartRequired: true` 保证——kubelet 在**每次容器启动前**（含重启）调用它，代码注释 "before each startup" 正是此意。

> **⚠️ 实施期发现：这条清理在 device plugin 路径上此前是失效的（已修复，commit `78e8efc`）。**
>
> 原代码把路径拼成 `filepath.Join(configDirPath, util.VMemNode, util.VMemNodeFile)`，即 `<cont-dir>/config/vmem_node/vmem_node.config`；而 `Allocate` 挂载的、metrics lister 读回的都是 `<cont-dir>/vmem_node/vmem_node.config`——**`vmem_node` 是 `config/` 的兄弟目录，不是子目录**。被命名的那个路径从不存在，`os.RemoveAll` 恒返回 `nil`，**清理是个静默空操作**。
>
> 成因是 `583df02` 复用了上一行 `pidsConfigPath` 的写法（那一行用 `configDirPath` 是对的，`pids.config` 确实在 `config/` 下）。NRI 路径因为显式做了 `strings.TrimSuffix(inj.ConfigDir, util.Config)` 而**没有**这个问题——这也是本设计原先只核对 NRI 侧就误以为两路都健康的原因。
>
> **对本设计的影响**：§4.5.3 的覆盖矩阵在修复前实际上是"两条路径都有陈旧风险"，而非表中所写的"仅 DRA 非 NRI 有缺口"。修复后矩阵恢复为表中所述。**这也反过来验证了 §4.5.4 保留库内布局守卫的决定是对的**——控制面清理不仅是 best-effort，它还可能**看起来在跑、实际没跑**，而且这种失效不产生任何日志。

> **这条教训应固化为实施纪律**：`sm_node` 的清理路径必须从 `contDir`（而非 `configDirPath`）拼起，且**必须与挂载路径来自同一个基址表达式**。阶段 1a 的落地已按此执行（两处清理均用 `filepath.Join(contDir, util.SMNode, util.SMNodeFile)` / `filepath.Join(basePath, util.SMNode, util.SMNodeFile)`）。

`CreateContainer`（DRA + NRI 路径，[nri/plugin.go#L387](../pkg/kubeletplugin/nri/plugin.go#L387)）：

```go
// Clean up old cache files (if any)
basePath := strings.TrimSuffix(inj.ConfigDir, util.Config)
vmemNodeConfigPath := filepath.Join(basePath, util.VMemNode, util.VMemNodeFile)
_ = os.RemoveAll(vmemNodeConfigPath)      // ← sm_node.config 加在这里
```

**这彻底改变了 §4.5 的性质**：容器每次启动前文件已被删除 → 库 attach 时必然是**全新的零字节区** → 走首建初始化 → **不存在陈旧状态**。二稿里的 ns `st_ino` 探测（best-effort、inum 会被 IDA 复用）因此**整个删除**——控制面给的是确定性保证，比库内探测严格得多，且零新增代码。

> 删文件而非清零内容是安全的：`RemoveAll` 只断开目录项，仍映射着旧 inode 的存活进程不受影响（但按下表，那一刻本就没有存活进程）。新容器 `open(O_CREAT)` 得到新 inode。

#### 4.5.3 覆盖矩阵：DRA 不开 NRI 是唯一缺口

逐路径核对"容器重启时谁来清"：

| 路径 | 每容器启动的钩子 | 清理 | 陈旧风险 |
|---|---|---|---|
| device plugin | `PreStartContainer`（`PreStartRequired: true`） | ✅ | 无 |
| DRA + NRI | `CreateContainer` | ✅ | 无 |
| **DRA 不开 NRI（纯 CDI）** | **无** | ❌ | **有** |

**缺口成因**：DRA 非 NRI 路径的挂载由 CDI 注入，而 CDI spec 在 `NodePrepareResources`（**每 claim 一次，Pod 准入时**）生成；容器重启时运行时只是重新套用磁盘上已有的 spec，**没有任何插件代码运行**。`ensurePartitionDirectories`（[vgpu.go#L141](../pkg/kubeletplugin/vgpu.go#L141)）只 `EnsureDir` 不删除；会 `RemoveAll` 的 `ensureClaimDirectories`（[vgpu.go#L129](../pkg/kubeletplugin/vgpu.go#L129)）也只在 Prepare 时跑。

**这个缺口不需要新机制来堵**，因为按 §4.5.4 残留是良性的（自校正），唯一的实际危害由布局守卫兜住。故：**不为 DRA 非 NRI 路径引入额外钩子**，只在文档与代码注释中记录该路径依赖库内守卫。

#### 4.5.4 库内兜底：布局守卫（保留，但降级为第二道防线）

控制面清理覆盖了两条主路径。库内仍保留**一层极廉价的守卫**，理由有二：一是 DRA 非 NRI 路径没有钩子（§4.5.3）；二是清理是 best-effort（`_ = os.RemoveAll(...)` 忽略错误，插件亦可能崩溃/降级）。

即便清理漏做，残留的是什么？`share` / `cur_cuda_cores` / `up_limit` / `is_cnt` / `avg_sys_free` **全是负反馈量**——控制器几个周期（~百毫秒级）就拉回收敛值；`total_cuda_cores` 是设备几何，同一次分配下**残留值本来就是对的**。

> **关键区别（初稿的判断失误）**：我把令牌桶当成了 vmem 那样的**账本**。账本的陈旧条目永不自愈、必须清理；令牌桶是**自校正的反馈量**。这个不对称是"控制面清理漏做也不致命"的根据，也是不给 DRA 非 NRI 路径补钩子的根据。

**唯一不自愈的残留是布局错位**：宿主目录跨容器重启存活，而 `.so` 按版本挂载（[vnum_plugin.go#L492](../pkg/deviceplugin/vgpu/vnum_plugin.go#L492)：`libvgpu-control.so.<version>`）→ 升级 library + DRA 非 NRI 路径容器重启 → **新库映射到老结构体的字节** → 字段错位 → 读出垃圾。守卫正是为它而留。

#### 守卫语义：重建，绝不报错退出

```c
/* 在 §4.4 的文件锁内调用。只读【冻结区】的 4 个字段——它们的偏移永久不变(§4.1)，
 * 所以“还不知道对方版本”时也能安全读。 */
static int header_valid(const sm_node_region_t *r) {
  return r->magic          == SM_NODE_MAGIC           &&
         r->layout_version == SM_NODE_LAYOUT_VERSION  &&
         r->region_size    == sizeof(sm_node_region_t) &&
         r->device_count   == MAX_DEVICE_COUNT;
}

static void rebuild_region_locked(sm_node_region_t *r) {
  LOGGER(WARN, "sm_node layout mismatch (magic=%#x ver=%u size=%u), rebuilding",
         r->magic, r->layout_version, r->region_size);
  memset(r, 0, SM_NODE_FILE_SIZE);        /* 尺寸恒定 → 就地清空即可 */
  for (int i = 0; i < MAX_DEVICE_COUNT; i++) {
    r->devices[i].total_cuda_cores = <本进程查到的 thread*sm*FACTOR>;
    r->devices[i].up_limit         = <hard_core>;
    /* share / cur_cuda_cores / last_refill_ns 归零即可，控制器会自己收敛 */
  }
  r->device_count   = MAX_DEVICE_COUNT;   /* 冻结区最后写 —— magic 是发布点 */
  r->region_size    = sizeof(sm_node_region_t);
  r->layout_version = SM_NODE_LAYOUT_VERSION;
  __atomic_store_n(&r->magic, SM_NODE_MAGIC, __ATOMIC_RELEASE);  /* 发布 */
}
```

**行为约定（本节的硬要求）**：

- **布局不符 → 重建，不是报错退出。** 这是本设计与现有 vmem 区的关键分歧：[loader.c#L1597](../library/src/loader.c#L1597) 在尺寸不符时 `LOGGER(ERROR)` + `return 1`，等于**库升级后容器直接不可用**。本区一律重建。
- **任何不可恢复的错误（`open`/`mmap` 失败）→ 优雅降级回进程私有桶**，等价于该进程没开这个特性。**绝不 `exit`/`abort`/让 CUDA 调用失败**——多进程算力隔离是一个**优化**，不是正确性前提，不值得为它牺牲可用性。
- `magic` **最后写、用 RELEASE 序**：重建中途若进程被杀，`magic` 仍是旧值/0 → 下一个进程照样判定不符 → 再次重建。**重建是幂等的、可中断的**，没有"半初始化"稳态。

#### 为什么就地重建不会踩到活着的旧版本读者

关键论证：**布局不符 ⟹ 该文件来自上一世容器 ⟹ 此刻没有任何进程正映射着旧布局。**

因为 `.so` 挂在容器内的**固定路径**（`ContVGPUControlFilePath`），宿主侧才是版本化的 `libvgpu-control.so.<version>`。**一个容器一生之内，所有进程加载的必然是同一个 `.so` 版本**——运行中的容器不会换库。因此同一时刻映射同一个区的进程，布局必然一致；布局不一致只可能发生在"上一世写的文件、这一世新库来读"，而那时上一世的进程全都不在了。

> **推论**：不需要 `unlink` + 重建新 inode，也不需要 `rename` 原子发布。那些手法是为了"新旧读者并存"而设计的，而这个前提在此**不成立**。就地 `memset` 严格更简单，且避免了 `rename` 竞态下"两个进程各自映射到不同 inode、共享桶退化成两个私有桶"的**静默失效**——那才是真正危险的失败模式。
>
> **唯一的例外**是宿主侧的 Go 读者（[container_lister.go#L206](../pkg/metrics/lister/container_lister.go#L206) 读 vmem_node 的先例）：它不在容器生命周期约束内。若将来给 `sm_node` 加宿主侧读取，**必须让 Go 侧也校验 `magic`/`layout_version` 并容忍读到重建中的区**（读到不符就当作"本周期无数据"，而不是报错）。

- 控制面刚删完文件 → 新建的区全 0 → 天然 `magic != MAGIC` → 走 `rebuild_region_locked`。**"控制面已清理"与"库内兜底重建"是同一条代码路径**，不是两套逻辑。
- **改结构体必须 bump `SM_NODE_LAYOUT_VERSION`** —— 本设计对未来维护者的硬约束，应写进结构体上方注释。冻结区的 4 个字段**永远不得改动**（§4.1）。

#### 附：这条规则的适用边界（重要，别改错地方）

`loader.c` 里 `file size mismatch` 出现在**三处**，但**只有一处**该改成重建。判据是**这个区归谁所有**：

| 函数 | 映射方式 | 数据所有者 | 尺寸不符时该怎么办 |
|---|---|---|---|
| [`mmap_file_to_config_path`](../library/src/loader.c#L1499)（`resource_data_t`） | `MAP_PRIVATE`/`PROT_READ` | **控制面**（manager 写 `vgpu.config`） | **保持报错**（见下） |
| [`mmap_file_to_util_path`](../library/src/loader.c#L1531)（`device_util_t`） | `MAP_PRIVATE`/`PROT_READ` | **外部 watcher** | **保持报错** |
| [`mmap_file_to_vmem_node`](../library/src/loader.c#L1563)（`device_vmemory_t`） | `MAP_SHARED`/读写 | **库自己** | 可以重建（但见下） |

**前两处必须保持报错，改成"重建"是错的**：它们是**只读消费**控制面产出的文件，库既无权也无力重建自己不拥有的数据——凭空造一份 `vgpu.config` 只会让容器带着错误的配额跑起来，比起不来更糟。而且 `resource_data_t` 的尺寸校验是**被设计成失败的**：[vnum_plugin.go#L1107](../pkg/deviceplugin/vgpu/vnum_plugin.go#L1107) 在 `Reschedule` 门控下调用 `CheckResourceDataSize`，注释写明"When a version upgrade causes a change in the configuration structure, the controller can reschedule these pods that cannot be started"——**升级后起不来是预期行为，由控制器重新调度兜底**。

> **本设计确立的规则应精确表述为**：*库自己拥有的共享区（`MAP_SHARED` 读写）布局不符 → 重建；只读消费控制面产出的区 → 保持报错，交给上层的重调度机制。* `sm_node` 属于前者。

**`vmem_node` 属于前者，但仍不在本设计范围内**。它确实有同样的缺陷（[loader.c#L1597](../library/src/loader.c#L1597) 尺寸不符即 `return 1`，DRA 非 NRI 路径下库升级会让容器起不来），但 **vmem 区是账本**（§4.5.4 的关键区别）：重建 = 丢失全部显存记账 = 已分配的显存变成"没人认领"，后果比令牌桶重建（几个控制周期收敛）严重得多。"重建 vs 报错 vs 交给重调度"哪个危害最小，需要单独评估，**不应捆绑进本设计**。此处仅记录问题与关联。

#### 4.5.5 为什么 `sm_node` 不需要 `vmem_node` 那三套回收机制（`main` 同步补充）

同步 `main` 后发现，库内已为 `vmem_node` 加了**三套**陈旧回收机制，都是本设计**明确不采用**的。这里逐条记录判据，避免后来者"照着 vmem 抄一遍"：

| `vmem_node` 现有机制 | 位置 | `sm_node` 是否需要 | 判据 |
|---|---|---|---|
| **PID 存活回收**：遍历记录，`pid_exist`/`is_zombie_proc` 不通过就踢出 | `rm_vmem_node_by_non_existent_device_pid`（[loader.c#L1825](../library/src/loader.c#L1825)） | **不需要** | `sm_node` **没有按 PID 的记录**。令牌桶是一个标量计数器，进程死亡不会在里面留下"属于它的条目" |
| **周期性体检**：watcher 每轮对本设备做一次回收 | `check_cleanup_vmem_nodes_by_device`（[loader.c#L1940](../library/src/loader.c#L1940)） | **不需要** | 同上；且它要拿**每设备写记录锁**，与本设计"热路径/watcher 路径不引入锁"的约束冲突 |
| **退出/信号清理**：`atexit` + `sigaction(SIGTERM/SIGINT/SIGHUP)` 归还本进程的记账 | 注册于 [loader.c#L2527](../library/src/loader.c#L2527) 附近（仅在 `vmem_node` 启用时） | **不需要** | 进程猝死时它**未消费的令牌本来就应该留在桶里**——那正是"聚合还能发多少"的正确答案。补充端每周期无条件把桶推向目标水位（§4.3/§4.7），任何偏差在 ~百毫秒内被抹平 |

**这三条其实是同一个判据的三种表现**，即 §4.5.4 已经点明的那个不对称：

> `vmem_node` 是**账本**（记"谁占了多少"，条目与 PID 绑定，陈旧条目永不自愈，所以必须有人来收尸）；
> `sm_node` 是**自校正的反馈量**（桶里的数字每周期被控制器重写，没有"归属"，因而没有尸体可收）。

**唯一的推论要写死**：将来若有人往 `sm_node` 里加**任何按 PID / 按进程归属的字段**，上面三条结论**同时失效**，必须重新评估 —— 因为那一刻 `sm_node` 就变成账本了。这是本设计对未来维护者的第二条硬约束（第一条是 §4.1 的"改结构体必须 bump `SM_NODE_LAYOUT_VERSION`"）。

### 4.6 gap 检测的 `last_launch_ns`：**保持进程私有**

[GAP 路径](./sm_core_limit_gap_throttle_design.md) 的 `last_launch_ns`（[cuda_hook.c#L110](../library/src/cuda_hook.c#L110)）语义是"**本进程**上次发射到现在的空闲间隔"，用于判断"本进程是否刚从 >200ms 空闲醒来"。这是**进程本地**的时序，**不应共享**：

- 若共享 → A 频繁发射会一直刷新 `last_launch_ns` → B 即使真的空闲很久也检测不到自己的 gap，GAP 路径失效。
- 故 `last_launch_ns` 留在**进程私有的 `g_dev_hot[]`**，只把 `cur_cuda_cores` 及控制器态迁到共享区。

> 结论：`g_dev_hot[]` 拆成"共享的桶+积分态"和"私有的 gap 时序"两部分。

### 4.7 bypass 的 SET → 必须改（最易踩的坑）

现有防抖 bypass 是**直接赋值**（[cuda_hook.c#L1260](../library/src/cuda_hook.c#L1260)）：

```c
g_dev_hot[host_index].cur_cuda_cores = g_sm_controller(...);   // SET，非累加
```

共享桶下，一个进程的 SET 会**抹掉**并发消费者刚 CAS 扣掉的令牌 → 令牌凭空多出来 → 超发。**必须改**：

1. bypass 只在**补充选举赢家**里执行（和 §4.3 补充同属"赢家专属"块）；
2. 且改成**累加语义**（`change_token` 加）而非 SET，或用 CAS 把"目标值"安全地写入而不覆盖并发扣减。
   - 推荐：把 bypass 的"钳制到单步"语义重写为"补充到目标水位"的**增量**（`delta_tokens = target - current`，再 `change_token(delta_tokens)`），使其与消费者的 CAS 扣减可交换、不丢账。

> 这是共享化改造里语义最微妙的一处，需单独单测（并发扣减 + bypass 补充不丢令牌）。

### 4.8 change_token 的累加也要跨进程原子

现有 [`change_token`](../library/src/cuda_hook.c#L567) 已经是 CAS 循环（`before + delta`，钳制 `[0, total]`）。迁到共享区后 CAS 目标换成共享计数器即可，**逻辑不变**。补充者（选举赢家）用它累加，消费者用 rate_limiter 扣减，两者都是对同一 `cur_cuda_cores` 的 CAS → 天然并发安全。

### 4.9 反馈信号：无需改

`user_current` 已是**容器聚合** util（跨进程无关的量）。补充选举赢家用**它自己的**那次 NVML 采样即可，值与"哪个进程采的"无关。所以反馈侧零改动。

---

### 4.10 原子性：用 `__atomic_*` 内建，不用 `volatile`，也不用 `_Atomic`

**先澄清**：现有 `dev_hot_t` 上的 `volatile` **没有提供任何并发保护**——它不保证原子性、不保证跨线程顺序，只是禁止编译器缓存该变量。今天的正确性 **100% 来自 [`CAS` 宏](../library/include/hook.h#L168)**（`__sync_bool_compare_and_swap`，自带全序）。`volatile` 在此基本是装饰性的，且**有害**：它让人误以为存在它并不提供的保护。共享区**不带 `volatile`**。

**为什么也不用 `_Atomic`**（尽管它是"标准正确"的工具）：

1. **lock-free 是硬要求，否则跨进程静默失效**。`_Atomic T` 若非 lock-free，编译器退化为 libatomic 中**按地址索引的锁表**，而那张表**每进程一份** → 两个进程映射同一块共享内存会各自用**不同的锁** → 保护形同虚设、且不会报错。
2. **`_Atomic T` 的 size/alignment 允许与 `T` 不同**。本结构体是落盘的跨进程 ABI（§4.1），且宿主 Go 侧已有读同类结构的先例，多一层由编译器决定的布局变数是净负担。
3. **不合仓库既定习惯**。[hook.h#L63](../library/include/hook.h#L63) 已硬性要求 GCC/Clang + glibc，全库用的就是 GCC 内建（`CAS` = `__sync_bool_compare_and_swap`；`src/vulkan/` 用 `__atomic_*`；[cuda_hook.c#L601](../library/src/cuda_hook.c#L601) 用 `__atomic_store_n`）。

**结论：普通定宽类型 + `__atomic_*` 内建 + 每处显式内存序。** 它作用在**普通类型**上 → 对共享结构体**零 ABI 歧义**；lock-free 由类型本身保证（`int64_t`/`int32_t` 在 x86-64/aarch64 上均是）；且每个访问点的内存序是显式写出来的，而不是藏在类型限定符里。

| 用途 | 写法 | 内存序 |
|---|---|---|
| 令牌桶扣减/补充 | `CAS(...)`（现有宏，不改） | 全序（`__sync_*` 自带） |
| 桶的探测性读 | `__atomic_load_n(&cur, __ATOMIC_RELAXED)` | relaxed（后面 CAS 会复核） |
| 补充选举 | `CAS(&last_refill_ns, last, now)` | 全序 |
| 积分态（赢家读写） | `__atomic_load_n(..., __ATOMIC_ACQUIRE)` / `__atomic_store_n(..., __ATOMIC_RELEASE)` | acq/rel 配对 |
| 区头 `magic` 发布 | `__atomic_store_n(..., __ATOMIC_RELEASE)`（§4.5.4） | release |

> 若将来仍想改用 `_Atomic`，必须同时加 `_Static_assert(ATOMIC_LLONG_LOCK_FREE == 2)` 与 size/alignment 断言；在收益为零（`__atomic_*` 已够）的前提下不建议。

### 4.11 兼容性约束：不抬高工具链/内核门槛

本设计**不引入任何比仓库现有依赖更新的要求**。[hook.h#L63](../library/include/hook.h#L63) 已硬性要求 GCC/Clang + glibc，除此之外不再加码。

**允许使用**（全部是仓库已在用、或古老到无兼容风险的设施）：

| 设施 | 最低要求 | 仓库现状 |
|---|---|---|
| `open`/`O_CREAT`/`ftruncate`/`fstat`/`mmap(MAP_SHARED)`/`memset` | 远古 POSIX | `mmap_file_to_vmem_node` 已在用 |
| `O_CLOEXEC` | Linux 2.6.23 (2007) | 已在用（loader.c、lock.c） |
| `fcntl` 经典 POSIX 记录锁 | 远古 POSIX | lock.c 已在用 |
| OFD 锁 `F_OFD_SETLKW` | Linux 3.15 (2014)，**且运行时回退** | [lock.c#L64](../library/src/lock.c#L64) 已封装好回退 |
| `__sync_*` 内建（`CAS` 宏） | GCC 4.1 (2006) | 已在用 |
| `__atomic_*` 内建 | GCC 4.7 (2012) | 已在用（cuda_hook.c、src/vulkan/） |
| `_Static_assert` | C11 / GCC 4.6 (2011) | [cuda_hook.c#L116](../library/src/cuda_hook.c#L116) 已在用 |
| `offsetof` | `<stddef.h>`，远古 | lock.c 已 include |

**禁止使用**（会抬高内核/glibc 门槛，且本设计并不需要）：

| 设施 | 门槛 | 本设计的替代 |
|---|---|---|
| `memfd_create` | Linux 3.17 / glibc 2.27 | 用普通文件 + `mmap`（必须落在插件挂载的目录里，本来就不能用匿名内存） |
| `O_TMPFILE` | Linux 3.11 + 文件系统支持 | 不需要临时文件：就地重建（§4.5.4） |
| `renameat2(RENAME_NOREPLACE)` | Linux 3.15 / glibc 2.28 | 不需要原子发布：文件锁已串行化（§4.4） |
| `statx` | Linux 4.11 / glibc 2.28 | `fstat` 足够 |
| `pthread_mutex` + `PTHREAD_PROCESS_SHARED` on shm | —— | **语义上就被否决**：持有者猝死 → 死锁（§4.4.1） |
| C11 `<stdatomic.h>` / `_Atomic` | —— | **语义上就被否决**：跨进程共享内存的 lock-free 退化风险（§4.10） |

> **注意 OFD 锁不是新增门槛**：`lock.c` 的 `ofd_fcntl` 在内核返回 `EINVAL` 时自动回退到经典 POSIX 锁，所以**低于 3.15 的内核照样能跑**；且经典锁对本用途够用（§4.4.2）。这也是本设计坚持复用它、而不是自己写锁的原因之一。

### 4.12 开销核算

**热路径（每次 kernel 发射）：与现状完全相同——一条 CAS。**

本设计新增的所有机制都**不在热路径上**：

| 机制 | 频率 | 开销 |
|---|---|---|
| 布局守卫（`header_valid`） | **每进程一次**（`pthread_once` 之下） | 4 次整数比较 |
| 文件锁（`ofd_fcntl` 加/解，**阻塞**§4.4.2a） | **每进程一次** | 2 次 syscall；稳态无争用→不睡；争用时睡等一次建区（微秒级，**不占 CPU**） |
| `open`/`ftruncate`/`fstat`/`mmap` | **每进程一次** | 4 次 syscall |
| 补充选举（`CAS(last_refill_ns)`） | 每 watcher 周期（~80ms）/设备 | 1 条 CAS |
| **`rate_limiter` 扣令牌** | **每次发射** | **1 条 CAS（= 现状，未增加）** |

> **阻塞锁不是 CPU 开销**：等待者在内核里睡，不空转（这正是选阻塞锁的原因，§4.4.2a）。且稳态下（容器已跑起来、没有并发新建进程）初始化锁**无争用**，`F_OFD_SETLKW` 首次尝试即成功、不睡。

> **必须守住的实现纪律**：守卫**只在初始化时校验一次**，把区指针缓存进 `g_sm_node`；**绝不允许**在 `rate_limiter` 里做 `magic` 校验、`NULL` 判断以外的任何检查。每次发射多一次分支都是不可接受的——这条路径的调用频率是 kernel 发射频率。

**唯一真实的新增开销**是共享桶的**跨进程 cacheline 弹跳**：单进程时 `cur_cuda_cores` 独占本核 L1；多进程时 N 个核争抢同一条 cacheline，CAS 延迟从 ~ns 升到 ~几十 ns。这正是 §8 阶段 3（本地批量取令牌）要压的对象，**且仅在 profiling 证明它是真开销时才做**。

> 注意此开销**只在开启共享桶时产生**（默认关闭，§5），且它换来的是限额从"N 倍松"变严。

### 4.13 与三种控制器的兼容性（delta / aimd / auto）

> **本节修正了本设计一个实质性缺陷。** 前几稿默认"只把令牌桶搬进共享区、控制算法不动"（§2.2 非目标）。核查代码后：**对 `delta` 成立，对 `aimd` 不成立，对 `auto` 也不成立**。必须把**全部跨周期控制状态**一起搬进共享区（§4.1 结构体已相应扩充）。

先明确一件被前几稿忽略的事实：控制器有**三种**，不是两种（[cuda_hook.c#L700](../library/src/cuda_hook.c#L700)）：

```c
enum { SM_CONTROLLER_DELTA = 0, SM_CONTROLLER_AIMD = 1, SM_CONTROLLER_AUTO = 2 };
```

`auto`（[auto_routed_controller](../library/src/cuda_hook.c#L1003)）按排他性**逐设备逐周期**在两者间路由，所以它同时继承两者的约束。

#### 4.13.1 判据：什么状态必须共享

选举（§4.3）把"谁跑控制器"这件事**每周期换人**。于是：

> **凡是"只有选举赢家推进"的跨周期状态，都必须放进共享区。** 否则每个进程各存一份、各自只在自己赢的那 1/N 周期推进 → 语义碎裂成 N 份，且每份都以 ~1/N 的速率演进。

反之，**每周期从当周期观测重算的临时量**保持进程私有即可（`top_results[]` 采样、`sys_frees[]`（[写 L1206](../library/src/cuda_hook.c#L1224) → 同周期 [读 L1300](../library/src/cuda_hook.c#L1318)，是 scratch 不是积分态）、排他 memo `g_excl_memo_*`）。所有进程观测的是同一个容器聚合信号，重算结果一致。

#### 4.13.2 `delta`：无状态，**天然兼容**

[`delta()`](../library/src/cuda_hook.c#L610) 是**纯函数**：输出只取决于入参 `(up_limit, user_current, share)` + 设备几何（`g_sm_num`/`g_max_thread_per_sm`，各进程相同）+ `g_dynamic_config`（只读）。**没有任何跨周期自有状态。**

⟹ 只要 `share` / `up_limit` 在共享区（已在），谁来跑 `delta` 都得到同一个结果。**选举对 delta 完全透明，零额外改动。**

#### 4.13.3 `aimd`：有 `md_cooldown`，**不改会触发 MD 雪崩**

[`aimd_controller()`](../library/src/cuda_hook.c#L801) 持有一个跨周期积分态 [`g_aimd_md_cooldown[]`](../library/src/cuda_hook.c#L717)，其声明注释写明了它赖以成立的不变量：

> *"Per-device remaining cooldown counter. **Watcher-thread-only access** (each watcher thread owns a disjoint host_index slice via balance_batches). No volatile / atomics needed."*

**而选举恰好打破了这个不变量**——host_index 不再被某一个线程独占，而是每周期换一个**进程**来跑。后果是**最坏的那种**：

| 周期 | 赢家 | 该进程的 `md_cooldown` | 动作 |
|---|---|---|---|
| 1 | A | 0 | util 超限 → **MD 触发**，A 的 cooldown = 4 |
| 2 | B | **0**（B 自己那份从没被推进过） | util 仍超限 → **MD 再次触发** ← 本该被拦住 |
| 3 | C | **0** | → **MD 第三次触发** |

`share` 被连续砍成 `md_divisor^N`（默认 3^N；N=4 → **81 倍**）。而 cooldown 的存在**正是为了阻止这个**——[代码注释](../library/src/cuda_hook.c#L864)：

> *"NVML's ~80ms sample + share-take-effect lag (~200-400ms total) means consecutive MD cuts share by md_divisor^N before the first cut's effect surfaces, hence **"MD avalanche"**. Cooldown breaks the chain."*

**修复**：`md_cooldown` 移入共享区（§4.1 已加 `md_cooldown` 字段）。移入后只有赢家读写它，被选举串行化，语义与今天的单进程完全一致——cooldown 计的是**全局周期数**，而这正是它本来的语义（注释说的是 "time-based semantics"）。

#### 4.13.4 `auto`：还需要排他 FSM + 节流标志

`auto` 经 [`host_index_is_exclusive_debounced()`](../library/src/cuda_hook.c#L960) 路由，该 FSM 有三个跨周期字段（`g_is_exclusive_debounced` / `g_exclusive_pending_streak` / `g_lost_exclusivity_pending`），其注释同样声明依赖"watcher 线程独占"：

> *"every field below is written and read exclusively by the watcher thread that owns the corresponding host_index. No cross-thread read; no volatile needed."*

FSM 的三个调用点（soft burst 门、hard_limit jitter 门、auto 路由）**全都在赢家的控制块内**，所以把三个字段移入共享区后，FSM 每周期恰好被推进一次（由当周期赢家），**语义与单进程一致**——debounce 计的是全局周期数，正是其本意。

> `g_lost_exclusivity_pending` 尤其**不能**留私有：它是"true→false 翻转"时置位、由后续 reset 分支[消费清零](../library/src/cuda_hook.c#L1315)的**一次性标志**。若各进程各存一份，非赢家的标志会一直悬着，等它某个周期赢了才消费 → **迟到数个周期的、莫名其妙的 reset**。

**另一处必须共享的是 `throttled_since_watch`**（不属 FSM，但同类问题）。它由 `rate_limiter` 在节流时置位、watcher 每周期 [read-and-clear](../library/src/cuda_hook.c#L1228)，用于给防抖 bypass 把门（§4.7）。共享桶下：

> 进程 A 撞节流 → 置 **A 的**标志；赢家 B 读**自己的**标志 = 0 → 判定"没人节流" → **放行 bypass** → bypass 的 SET 抹掉 A 正在扣的令牌。

移入共享区后，其语义从"**本进程**是否节流"正确地变成"**容器内是否有人**节流"——这恰好是共享桶下该门本来就该问的问题。

#### 4.13.5 小结

| 控制器 | 自有跨周期状态 | 选举下是否可用 | 需要的改动 |
|---|---|---|---|
| `delta` | **无**（纯函数） | ✅ 直接可用 | 无 |
| `aimd` | `md_cooldown` | ❌ MD 雪崩（`md_divisor^N`） | `md_cooldown` 入共享区 |
| `auto` | 上述 + 排他 FSM ×3 | ❌ 同上 + FSM 碎裂 | 再加 FSM ×3 入共享区 |
| 三者共用 | `throttled_since_watch` | ❌ bypass 误放行 | 入共享区 |

**结论：三种控制器都能兼容，但代价是"整个控制状态块入共享区"，而非前几稿说的"只搬令牌桶"。** §2.2 那条"不改变控制算法"仍然成立——**算法逻辑一行不改，改的只是状态的存储位置**；但改动面比前几稿承诺的大，风险评级相应上调（§9）。

> **已拍板：三种控制器全支持**（§12 第 2 项）。曾提出的范围缩减方案（只支持 `delta`、`aimd`/`auto` 降级回私有桶）**已被否决**。因此 §4.1 的结构体保持完整形态，`md_cooldown` 与排他 FSM ×3 都必须搬。
>
> 这个选择的直接后果，必须在实现时守住：**`md_cooldown` 和 FSM ×3 的现有注释都写着 "watcher-thread-only access / no volatile / no atomics needed"**（[cuda_hook.c#L717](../library/src/cuda_hook.c#L717)、[#L742](../library/src/cuda_hook.c#L757)）。搬进共享区后这些注释**全部失效且具误导性**——后人照旧注释做"这里不用原子操作"的优化就会引入难查的竞态。**改字段必须同时改注释**，这是 §9 的一条高风险项。

## 5. 开关与灰度

```c
CUDA_SM_SHARED_BUCKET = 0(默认，进程私有桶，现有行为) | 1(容器内共享桶)
```

- 默认 0：`g_dev_hot[]` 仍 static，行为与今日完全一致，风险为零。
- 开启 1：走共享区。集成进 `g_dynamic_config`（沿用 `CUDA_SM_DELTA_RAMP_FLOOR_DIVISOR` 等的 env→struct 加载模式，见现有 `sm_controller_init`），fork 边界自动继承（见 §6）。

**降级是开关的一部分，不是异常分支**（§2.1 第 5 条）：

```c
/* 初始化：任何一步失败 → g_sm_node = NULL → 全库自动回到私有桶 */
if (g_dynamic_config.sm_shared_bucket && map_sm_node_region(&g_sm_node) != 0) {
    LOGGER(WARN, "sm_node unavailable, falling back to per-process bucket");
    g_sm_node = NULL;         /* 不是错误，是降级 */
}
/* 热路径靠一次指针选择，不做任何校验（§4.12） */
static inline int64_t *bucket_of(int host_index) {
    return g_sm_node ? &g_sm_node->devices[host_index].cur_cuda_cores
                     : &g_dev_hot[host_index].cur_cuda_cores;
}
```

这条降级路径覆盖了一整类现实故障：**插件漏挂目录**（§4.5.1 的 5 处功能落点漏改任一处）、目录只读、`/tmp` 被业务覆盖挂载、老版本插件配新版本库。它们的后果统一是"退回今天的行为"，而**不是容器起不来**——这正是 §4.5.4 "永不致命"约定的落地点。

### 5.0 关闭时等价于旧行为：逐点审计（实施期验证记录）

§7.2 第 2 条要求"开关关闭时与基线逐点一致"。这不是靠自觉，而是靠**单一入口**：所有共享行为都经过 `g_sm_node`，关闭时它恒为 `NULL`。审计了它的**全部 10 处功能性使用**（`grep -n g_sm_node src/cuda_hook.c`，排除注释），每一处都有显式 NULL 分支：

| 使用点 | 关闭（`g_sm_node == NULL`）时 |
|---|---|
| `sm_bucket_of()` | → `&g_dev_hot[i].cur_cuda_cores`（私有桶）|
| `sm_throttled_of()` | → `&g_throttled_since_watch[i]` |
| `sm_try_claim_refill()` | `return 1` → 控制块**永远执行**，等于没有竞选 |
| `sm_ctl_load()` / `sm_ctl_publish()` | 立即 `return`，空操作 |
| `sm_sampling_claim()` | `return 1` → **每进程照常自采**，等于没有 leader |
| `sm_publish_sample()` | 立即 `return` |
| `sm_load_published_sample()` | `return 0`；且其所在 `else if` 分支在关闭时**不可达** |
| bypass（§4.7） | 走 `else` 分支 → **字面 SET**，与改造前逐字相同 |
| `gap_effective_dc()` | → 私有 `up_limits[]` |
| `initialization()` ×2 | 门控在 `g_dynamic_config.sm_shared_bucket`，整段跳过；锁 fd 因 `g_sm_node != NULL` 前置条件也跳过 |

`child_after_fork` 里的 `close(g_sm_lock_fd)` 在关闭时是 no-op（fd 恒为 -1）。

**唯一代价**：热路径多一个"对一个全局指针是否为 NULL"的分支。该分支高度可预测（进程生命周期内取值不变），且 `bucket` 指针**每次发射只解析一次、不在 CAS 重试循环内**。

> **给后来者的硬约束**：新增任何共享行为**必须**继续以 `g_sm_node` 为唯一开关入口。一旦出现绕过它的共享写入，上面这张表就失效，而 §7.2 第 2 条是**不会自动发现**这种回归的——它只测吞吐，不测代码路径。

### 5.1 为什么走 `g_dynamic_config` 环境变量，而不是 `resource_data_t` 字段（`main` 同步补充）

同步 `main` 后，仓库里出现了**第二种**开关范式，必须显式选边，否则实现时会摇摆：

| | 范式 A：`g_dynamic_config` + env（**本设计采用**） | 范式 B：`resource_data_t` 字段 + 控制面门控（`vmem_node` 采用） |
|---|---|---|
| 谁决定 | 容器内环境变量，`sm_controller_init` 里 env→struct 加载 | 控制面：`util.VMemoryNode` feature gate → `VMEMORY_NODE_ENABLED` env → `vgpu.config` 的 `vmem_node` 字段 |
| 已有同类 | `CUDA_SM_CONTROLLER`、`CUDA_SM_AIMD_*`、`CUDA_SM_DELTA_RAMP_FLOOR_DIVISOR`（[dynamic_config_t](../library/include/hook.h#L269)） | `sm_watcher`、`vmem_node`（[resource_data_t](../library/include/hook.h#L214)） |
| 落点数 | **0 处 Go 改动** | Go 侧至少 4 处：feature gate 定义、`vgpu_config.go` 组装、device plugin `Allocate`、DRA `GetClaimCommonContainerEdits` |
| ABI 影响 | 无 | **改 `resource_data_t` = 改 `vgpu.config` 的跨语言 ABI** |

**决定：采用范式 A。** 决定性理由是最后一行：`resource_data_t` 的尺寸被 [`CheckResourceDataSize`](../pkg/deviceplugin/vgpu/vnum_plugin.go#L1107) 在 `Reschedule` 门控下校验，**加字段 = 老容器起不来、等控制器重调度**（§4.5.4 附表已论证这是该文件被刻意设计成的行为）。为一个**默认关闭的性能优化**付出一次 `vgpu.config` ABI 变更，代价与收益完全不成比例。

**但要留一条明确的升级路径**：若将来需要让控制面统一开关（例如按 Pod 注解灰度），范式 B 的两个 env 注入点已经现成——[vnum_plugin.go#L823-824](../pkg/deviceplugin/vgpu/vnum_plugin.go#L823)（device plugin，feature gate 门控）与 [vgpu.go#L182](../pkg/kubeletplugin/vgpu.go#L182)（DRA，当前硬编码 `TRUE`）。届时**只需在这两处多注入一个 `CUDA_SM_SHARED_BUCKET=1`，库侧一行不改**——因为范式 A 读的就是环境变量。这正是选范式 A 的附带好处：它不排斥控制面接管，只是不强制。

> **顺带一条实现纪律**：`dynamic_config_t` 的注释（[hook.h#L228-267](../library/include/hook.h#L233) 的 "APPEND new fields to the tail"）写明"字段只能追加到尾部，不得重排"——`sm_shared_bucket` 必须**追加在末尾**。

---

## 6. 正确性与 fork 边界

### 6.1 fork 语义（白送的好处）

- `MAP_SHARED` 映射**跨 fork 保留** → 子进程自动 attach 同一个桶，无需额外处理，天然参与聚合限流。
- 现有 [`child_after_fork`](../library/src/cuda_hook.c#L238) 重置 `g_dev_hot[].last_launch_ns=0`：迁移后 `last_launch_ns` 仍在私有 `g_dev_hot[]`，此重置**保持不变**（正确：子进程的 gap 时序应重新计）。
- 共享区里的 `cur_cuda_cores`/`share` **不应**在 child_after_fork 重置（那是全容器共享状态，子进程只是新加入的消费者/候选补充者）。

### 6.2 不新增锁 → 不新增 fork 死锁面

本方案**不引入任何用户态 mutex**（热路径全是 CAS + 选举；初始化用内核文件锁），因此**无需**动 [`loader_child_after_fork`](../library/src/loader.c#L2635) 的 mutex 重init 列表，规避了"父持锁 fork → 子死锁"这一整类隐患。这是相对 HAMi 锁方案的结构性优势。

> **`main` 同步补充（结论不变，理由需精确化）**：`loader_child_after_fork` 现在除了重init 四把 mutex，还会**释放 fork 继承的显存记账链表**（`g_memory_node`）。这不是反例，恰恰是同一判据的另一面：那条链表是**进程私有的、按分配归属的账本**，父进程的条目在子进程里全是垃圾，所以必须丢弃。而 `sm_node` 的共享桶**没有归属**——子进程 fork 后自动 attach 同一个 `MAP_SHARED` 桶，它作为新增消费者参与聚合限流**正是我们要的语义**（§6.1）。
>
> ⟹ **在 `loader_child_after_fork` / `child_after_fork` 里对 `g_sm_node` 做任何重置或清理都是错的。** 唯一该保留的 fork 期重置仍是 `g_dev_hot[].last_launch_ns`（私有 gap 时序，§6.1）。这条要写进代码注释，因为紧邻的两个 fork 处理器现在都在"丢弃继承状态"，语义惯性会诱导后人把共享桶也一并清掉。

关于 §4.4 的初始化文件锁与 fork 的关系，逐条核对：

- **锁的生命周期完全包含在初始化段内**：`ofd_fcntl(F_WRLCK)` → 校验/重建 → `F_UNLCK` → `close(fd)`，全程无阻塞调用、无用户代码回调，且跑在 `pthread_once` 之下。**函数返回时锁已释放、fd 已关闭**，不存在"持锁状态被 fork 继承"的窗口。
- **即便真在持锁时被 fork**：文件锁附着在 fd / OFD 上，不是共享内存里的状态位。子进程 `fork` 后不会"继承一个死锁"；且经典 POSIX 锁根本不被子进程继承。
- **持有者猝死 → 内核无条件回收**（§4.4.1），不存在 vmem 那种"残留标记"或 pthread 那种"永久死锁"。

### 6.3 崩溃语义

- 消费者崩溃：无临界区、无持有物，桶计数器不受影响。
- 补充选举赢家崩溃：本周期没补上 → 下周期 `now-last` 超阈值 → 别人接手。最坏损失一个周期的补充（~80ms 少补一次），自愈。
- **初始化持锁者崩溃**：内核释放文件锁；若它死在 `rebuild_region_locked` 中途，`magic` 尚未发布（RELEASE 序最后写）→ 下一个进程判定不符 → 再次重建。**重建幂等可中断，无半初始化稳态**（§4.5.4）。
- 无 robust-futex / 一致性恢复负担（热路径根本没有锁；初始化锁由内核回收）。

### 6.4 内存序

见 §4.10 的逐字段内存序表。要点：桶计数器与选举戳走 `CAS`（`__sync_*` 自带全序）；积分态只有选举赢家读写、跨周期可能换进程 → `__ATOMIC_ACQUIRE`/`__ATOMIC_RELEASE` 配对，保证赢家看到上个赢家写的最新值。**不使用 `volatile`（不提供任何并发保护），也不使用 `_Atomic`（跨进程共享内存下的 lock-free 与 ABI 风险）。**

---

## 7. 验收基线与测试清单

> **定位已变更**：本节曾是"实施前的准入门槛"，现降级为**验收基线**（§12 第 1 项）。"要不要做"已由机制推理定论——多进程竞争同一 GPU 必然超限，notebook 场景必受影响。测量的目的因此从"决定做不做"变为"**证明修好了、且没修坏**"。

### 7.1 基线（改造前先采，作为对照）

起一个 **N 进程并发计算**的容器（notebook 多 kernel 最贴近真实场景；N=4 起），设 `hard_core`，`LOGGER_LEVEL=5` + `nvidia-smi pmon` 记录：

- **聚合 util**（容器内所有 PID 之和）相对 `hard_core` 的**超出幅度**与**振荡幅度**；
- 单进程同负载曲线作为对照。

### 7.2 验收判据（改造后）

1. **聚合限额收紧**：多进程聚合 util 的均值/峰值应显著向单进程曲线靠拢。
2. **无性能回归**：单进程场景开关关闭时，吞吐与基线**逐点一致**（默认关闭，理应零差异——这条是防止误伤的兜底）。
3. **AIMD 无雪崩**（§4.13.3）：开启共享桶 + `aimd`，观察 `metrics_record_aimd_event` 的 `MD_FIRED` 计数——**不得出现连续多个周期各触发一次 MD**。这是 `md_cooldown` 是否真正被共享的直接证据。
4. **降级路径可用**（§5）：故意不挂 `/tmp/.sm_node` 目录启动 → 应打印 WARN 并退回私有桶，**容器正常运行**。
5. **重建路径可用**（§4.5.4 / §10）：手工把 `sm_node.config` / `vmem_node.config` 的 magic 改坏 → 容器重启后应自动重建，**不得报错退出**。
6. **`vmem_node` 跨语言 ABI 一致**（§10.3）：Go 侧 metrics 报出的显存用量与容器内实际一致（验证 `getVmemoryLockOffset` 没算错——**这一处算错不会报错，只会给出错的数**）。
   > **前置条件（`main` 同步补充）**：必须显式打开 `util.VMemoryNode` feature gate（device plugin / device-monitor 均默认 `false`，Alpha），否则 `vmem_node` 区不会被创建，本条**看似通过实则未测**（§10.6）。
7. **`vmem_node` 硬失败不再致命**（§10.6）：故意让 `vmem_node` 目录不可写（或不挂载）并开启 gate 启动 → 应打印 WARNING 并关闭本进程的 vmem 记账，**容器正常运行**；改造前此路径会 `LOGGER(FATAL)` 杀死进程。这条专门验证外层 `FATAL` 已被降级。
8. **fork 后共享桶不被清空**（§6.2）：容器内 fork 出子进程 → 父子应共享同一个 `cur_cuda_cores`（子进程发射会扣父进程看得见的令牌），且聚合 util 仍不超 `hard_core`。这条同时验证"没有人顺手把 `g_sm_node` 加进 fork 重置列表"。

### 7.3 必须真机压测的并发用例

- N 进程并发扣令牌 + 补充，**不丢账**（总扣减 + 总补充 = 桶变化量）。
- N 进程聚合 util **不超** `hard_core`。
- 补充选举赢家**中途被 kill** → 下周期别的进程接手，无停摆。
- 初始化持锁者**中途被 kill** → 内核释放锁，下一个进程重建成功（§6.3）。

> **我（设计者）无 GPU，本节全部需要你在真机执行。**

---

## 8. 分阶段实施计划

| 阶段 | 内容 | 前置 |
|---|---|---|
| **1a** ✅ **已完成** | **Go 侧挂载 + 清理**：`SMNode`/`SMNodeFile` 常量；5 处功能落点 + 1 处 NRI 观测落点（§4.5.1）；2 处启动前清理落点（§4.5.2）；2 处测试守卫。此时库还没用这个目录 → **纯增量、对现网零影响** | —— |
| **1b** ✅ **已完成** | **库侧 `sm_node`**：建区/初始化/布局守卫（§4.1/4.4/4.5.4）；消费端切共享桶（§4.2）；补充选举（§4.3）；**全部控制状态入共享区**（§4.13）；bypass 累加改造（§4.7）；env 开关 + 降级（§5） | 1a 已上 |
| **2-pre** ✅ **已完成** | **外层 `FATAL` 降级**（§10.6），独立先行：`vmem_node` 映射失败 → WARNING + 关闭本进程记账，不再杀容器。与冻结区头正交、可独立回滚 | —— |
| **2** ✅ **已完成** | **`vmem_node` 冻结区头 + 重建**（§10）：C 侧结构体 + `mmap_file_to_vmem_node` 改造（**外层 `FATAL` 已由 2-pre 处理**）；**Go 侧结构体 + `getVmemoryLockOffset` 同步**（§10.3）。**C 与 Go 必须同一个 PR 合并**——分开合会让 fcntl 锁静默失配 | 与 1a/1b 正交，可并行 |
| **3** | 压争用：进程/线程本地**批量取令牌**（一次 CAS 取一批、本地花完再取），把 CAS 频率降 ~N 倍。**仅当 profiling 证明跨进程 cacheline 弹跳是真开销时做** | 1b 稳定 |

> **1a / 1b 拆分的价值**：Go 侧改动（挂载 + 清理）不依赖库侧，且在库未使用该目录前**完全无副作用**。先上 1a 可以真机确认"目录挂进去了、每次容器重启前文件确实被删了"，把 §4.5 的控制面假设**验证成事实**，再让库侧依赖它。

> **阶段 2 为什么必须 C+Go 同 PR**：`getVmemoryLockOffset` 算错**不会报错**，只会让 Go 与 C 锁在不重叠的字节范围上 → 互斥静默失效 → 撕裂读（§10.3）。分两个 PR 合并意味着中间必然存在一个**两侧不一致**的提交。

> 阶段 3 的方向，代码注释早已点名——[dev_hot_t 上方注释](../library/src/cuda_hook.c#L104)："**Fixing that needs thread-local token batching, tracked separately.**" 本设计与之一致。

---

## 9. 风险与待办清单

- **[高] 并发正确性无法静态穷尽**：CAS 选举、bypass 累加、建区/重置竞态，必须**真机多进程压测**（并发扣减 + 补充不丢账、N 进程聚合不超限、补充者崩溃自愈）。
- **[高] bypass 语义改写**（§4.7）：SET→增量累加，是最易引入超发/欠发的一处，需专门单测。
- **[高，新增] 控制状态块整体入共享区**（§4.13）：改动面比前几稿承诺的大——不只是令牌桶，`md_cooldown`、排他 FSM ×3、`throttled_since_watch` 都要搬。这些字段的注释目前都写着"watcher-thread-only, no atomics needed"，**搬动时必须同步改注释**，否则后人会按旧注释假设做出错误优化。`aimd` 的 MD 雪崩（`md_divisor^N`）是不改就必现的回归，需专门用例覆盖。
- **[中] 库升级后的布局错位**（§4.5.4）：宿主目录按 `<pod-uid>_<cont-name>` 跨容器重启存活，而 `.so` 按版本挂载 → 新库可能映射到老结构体。控制面清理覆盖了 device plugin / DRA+NRI 两路；**DRA 非 NRI 路径无钩子**，由 `magic`/`layout_version`/`region_size` 守卫兜底。**改结构体必须 bump `SM_NODE_LAYOUT_VERSION`**，需在 review 中把关。
- **[中，实施期修正] 挂载点漏改**（§4.5.1）：新目录要在 **5 处功能落点**并列（device plugin Allocate/EnsureDir、DRA CDI、DRA NRI、`ensurePartitionDirectories`），另加 1 处 NRI 观测落点。
  > **⚠️ 原先写的"漏改任一处 → `open` 失败 → 由 §5 降级兜住"是错的**（首个真实用户就撞上了）。`map_sm_node_region` 第一步就是 `mkdir(SM_NODE_PATH)`——挂载没生效时它会在**容器自己的 `/tmp`** 上把目录建出来，`open(O_CREAT)` 随即成功，于是打印 `sm_node attached`、一切看起来正常。**漏挂不会降级，会静默落到容器自己的 `/tmp`**，而那正是 §4.5.1 明确要避开的位置。
  >
  > 功能上多数仍可工作（同容器进程共享同一文件系统），真正丢掉的是：① 启动前清理（§4.5.2）删的是**宿主**路径，落在容器 `/tmp` 的区**永不被清理、跨重启存活**；② 宿主侧完全看不到该文件，排查时会误以为特性没生效。
  >
  > **已加检测**：`sm_node_dir_is_mounted()` 比较 `SM_NODE_PATH` 与 `TMP_DIR` 的 `st_dev`（bind mount 自成挂载点，`st_dev` 必然不同），不同即已挂载；相同则 `LOGGER(WARNING)` 明确指出"这个区在容器自己的 /tmp 里、不会被清理、宿主看不到"。必须在 `mkdir` **之前**判定，否则测的是我们自己刚建的目录。同时 `attached` 日志现在会打印**完整文件路径与尺寸**。
  >
  > `/tmp/.vmem_node` 有**完全相同**的静默回退问题（`mmap_file_to_vmem_node` 也是先 `mkdir`）。本轮未一并处理——那会改动既有特性的日志输出，应单独评估。
- **[中] 热路径不得被守卫污染**（§4.12）：布局校验只能发生在初始化。**任何在 `rate_limiter` 里增加的校验/分支都是性能回归**，需在 review 中明确把关。
- **[低] 冻结区被误改**（§4.1）：`magic`/`layout_version`/`region_size`/`device_count` 这 16 字节是永久 ABI，改动 = 守卫失效。已用 `_Static_assert` 钉死偏移，但语义靠 review。
- **[高，新增] `getVmemoryLockOffset` 漏改会静默失效**（§10.3）：这是全设计**唯一一处"错了不报错"**的改动。C 侧 `offsetof` 自动含头、无需改；**Go 侧必须手工加 `unsafe.Offsetof(DeviceVMemoryT{}.Devices)`**。漏改 → Go 与 C 锁在不重叠字节范围 → 互斥失效 → 撕裂读 → **错的 metrics，无任何报错**。缓解：C+Go 同 PR（§8 阶段 2）+ §7.2 第 6 条验收 + 建议加一个断言 `offsetof` 一致性的单测。
- **[中] `vmem_node` 布局变更的升级期表现**（§10.5）：manager 与容器内 `.so` 版本 skew 期间，该容器 **metrics 暂缺**直至重启。**非新增问题**（今天尺寸一变即如此），但升级公告应提及。
- **[中] 补充周期与多 watcher 采样错峰**：`REFILL_PERIOD_NS` 已定为现 watcher 单设备周期（~80–100ms，§12 第 8 项），仍需真机确认不会"抢权成功但采样过旧"。
- **[低] DRA 非 NRI 路径无清理钩子**（§4.5.3）：残留是良性的（自校正），故不补钩子；仅依赖库内守卫。若将来该路径出现非自校正的共享字段，此结论需重审。
- **[低] 收益依赖真多进程**：单进程容器零收益（但默认关闭 → 零代价）。首要目标场景是 notebook（§1.3）。
- **[高，`main` 同步新增] `vmem_node` 的 `FATAL` 是两层，只改一层等于没改**（§10.6）：`mmap_file_to_vmem_node` 内的 `return 1` 之外，`load_controller_configuration` 里还有一个 `LOGGER(FATAL, "mmap vmem nodes file failed")`。阶段 2 必须同时把外层降级为 WARNING + 关闭本进程记账，否则 `open`/`mmap` 硬失败仍会杀死容器。
- **[中，`main` 同步新增] 验收时必须显式开启 `util.VMemoryNode` feature gate**（§10.6）：该 gate 默认 `false`，不开则 `vmem_node` 区根本不建，§7.2 第 6 条会**假通过**。
- **[中，`main` 同步新增] 别照抄 `vmem_node` 的三套回收机制**（§4.5.5）：库内现有 PID 存活回收、watcher 周期体检、`atexit`/信号清理，全部只对账本成立。`sm_node` 一条都不需要；尤其 `check_cleanup_vmem_nodes_by_device` 要拿每设备写记录锁，抄进来会直接违反"watcher 路径不引入锁"。**若将来给 `sm_node` 加任何按 PID 归属的字段，这三条结论同时失效，必须重审。**
- **[中，`main` 同步新增] fork 处理器的语义惯性**（§6.2）：`loader_child_after_fork` 现在会释放 fork 继承的显存记账链表，`child_after_fork` 重置 `last_launch_ns`——两个处理器都在"丢弃继承状态"。**共享桶必须原样保留**（子进程作为新消费者参与聚合正是设计意图），需在代码注释里写死，防止后人顺手清掉。
- **[高，实施期新增] fork 会共享采样锁的 open file description**（§4.3.1）：OFD 锁属于 description 而非进程，而 fork **共享** description。两个方向都坏：① 子进程重跑 `initialization()` 会重新拉起自己的 watcher（`child_after_fork` 重置 `g_init_set` 的本意就是如此），于是父子都以为自己拥有采样权；② **更隐蔽**——父进程（持有者）退出后，只要子进程还持有继承的 fd，description 引用计数不为 0，**内核就不释放锁**，所有待机者永远等不到，桶饿死。这直接打穿"内核会在持有者死亡时释放锁"这条核心论据。**修复**：`child_after_fork` 必须 `close()` 继承的 fd 并清空 `g_sm_sampling_mine`，`initialization()` 用**独立于区映射的守卫**重开（折进 `g_sm_node == NULL` 那个守卫会让所有 fork 出来的子进程永久没有锁 fd）。已由 `test_sm_node_shared` 的用例 [4] 固化——该用例**断言这个 hazard 真实存在**，防止后人把 `close()` 当冗余删掉。
- **[中，实施期新增] 采样集中化会让非 leader 进程的 metrics 失真**（§4.3.1）：待机者不再调 NVML，`top_results` 会永久陈旧。必须由 leader 发布采样结果、待机者读取，否则是一个随集中化一起引入的新 bug。
- **[中，实施期新增] GAP 路径读进程私有的 `up_limits[]`**：粘性采样权下待机进程**永远不会**刷新私有副本 → soft 模式的 GAP 节流永久按初始值走。已修（`gap_effective_dc` 改读共享 `up_limit`，commit `ca53099`）。注意这在每周期竞选下**就已经**是 latent bug（值最多陈旧 N 个周期），只是被"人人轮得到"掩盖。
- **[低，`main` 同步新增] `dynamic_config_t` 只能尾部追加**（§5.1）：`sm_shared_bucket` 字段必须加在结构体末尾，`hook.h` 的注释已声明重排会移动偏移。

---

## 9.1 已知边界（不是缺陷，但接手前必须知道）

这些是实施期逐项审计后**确认存在、且决定不修**的边界，连同不修的理由。不写下来，后来者会把它们当成 bug 反复"修"。

1. **混合模式会破坏聚合限额。** 进程 A 映射成功、进程 B 失败（目录漏挂、只读、老版本插件），则 A 受共享桶约束而 B 用完整私有桶 → 聚合超限。这是"永不致命"降级（§2.1 第 5 条）的**固有代价**：要么容器起不来，要么限额在故障期变松，二者不可兼得。选后者。排查提示：日志里搜 `sm_node unavailable`。
2. **soft 模式的弹性余量语义改变（开启时）。** `up_limits` 的爬坡（`hard_core`↔`soft_core`）由每进程一份变成**容器一份**。这是设计意图——N 份私有爬坡意味着聚合可达 N×`soft_core`——但它确实是行为变化，升级公告应提及。**hard 模式无任何语义变化**：该分支只读 `up_limits`、从不写，限额始终取配置里的 `hard_core`。
3. **leader 采样持续失败会退化回 N× 采样。** leader 若 `get_used_gpu_utilization` 一直失败就不发布，待机者判定陈旧后各自采样。**不会 hang**，但集中化收益消失。未加"连续失败 K 次释放锁"，因为那会引入锁抖动（换个人多半一样失败），而"同容器同驱动下只有 leader 的 NVML 坏"基本不现实。
4. **`top_results.valid` 是粘性的**（既有语义，非本设计引入）：`get_used_gpu_utilization` 只置 1、从不清零，失败时提前 `return`。所以它实际含义是"是否**曾经**采到过有效样本"。待机者转正后若采样失败，会沿用 leader 发布的样本——比它自己陈旧的私有样本**更新**，不构成回归。
5. ~~`SM_SAMPLE_STALE_NS`（3 周期 ≈ 290ms）需真机确认~~ —— **已改为自适应，见 §4.3.2。** 固定阈值在 watcher 超时的情况下会必现误判。
7. **容器内删掉区文件会让共享桶静默裂开**（实施期新增，由真实使用触发）。挂载必须可写（库要建区），容器内是 root，所以 `rm -rf /tmp/.sm_node` 会删掉内容——只在挂载点本身上失败（`EBUSY`），于是目录仍在、里面空了，看起来像什么都没发生。
   > 已 mmap 的进程不受影响（inode 被映射引用着）。但**之后启动的进程** `open(O_CREAT)` 会得到**新 inode**、映射到**另一块区**，两组各自按自己的 `last_refill_ns` 补充 → **聚合限额变成 2 倍松**。这正是 §4.5.4 否决 `unlink`+`rename` 时要避开的静默失效，从容器内部被重新引入。
   >
   > **不能靠权限防**：区必须可写。**已加检测** `sm_node_check_identity()`：映射时记住 `st_ino`/`st_dev`，补充路径上每秒 stat 一次比对，被删或被替换即 `LOGGER(WARNING)` 指明"桶已裂开、需重启容器"。**不自动重映射**——老 inode 上还有进程在用，改映射只会多出第三个视图；进程内无法恢复，说清这一点才是有用的部分。
   >
   > `attached` 日志现在也打印 inode，便于比对。

8. **`vmem_node` / `vgpu_lock` 的同类暴露(实施期分析结论)**：三个区都是可写挂载，容器内 root 都能删。逐个判定过：
   > - **`vgpu_lock`：无需处理，且自愈。** `try_acquire_lock` **每次调用都按路径 `open(O_CREAT)`**，不持长期 fd，删掉后下次调用重建、所有加锁者自动收敛到最新 inode。仅"某进程正处临界区"那一瞬存在互斥失效窗口。
   > - **`vmem_node`：已加检测，但绝不自愈。** 后果比 `sm_node` 重——它是**账本**：分裂后每组只累加自己那份 charge，互相看不见对方的 oversold/UVA 分配，**显存限额被低估**，可能真的把物理卡打 OOM。**但重映射是有害的**：本进程先前记在旧区的 charge 会被孤立（PID 存活清理只扫自己映射的那个区），而后续 `free` 会去新区减一笔从未加过的账，被 `sub_gpu_virt_memory` 钳到 0 → **静默记账漂移**。§4.5.4 那条"账本 vs 自校正反馈量"的不对称，在这里给出的结论是：可以"布局不符就重建"，但**不能"被替换就重新 attach"**。
   > - **`sm_node`：也不自愈，理由与预期不同。** 桶是自校正的，技术上可以重映射（不 munmap 即无 use-after-free，~1 秒内收敛）。真正的阻碍是 `g_sm_node` 现有不变量——"watcher 启动前写一次、此后只读"，这正是 `sm_bucket_of()` 敢在热路径裸读它、不加任何同步的依据（§4.12 明令热路径不得增加检查）。改成运行期可变就得在**每次 kernel 发射**做原子读。为一个容器内手动 `rm` 的自伤场景动热路径地基，不值得。
   >
   > **Go 侧本来就是对的**：`util.MmapFile.NeedsReload()` 比较的正是 **dev+inode**，inode 被换会自动 reload。一直瞎的是 C 侧。

6. **`metrics_record_aimd_event` 现在只由每周期赢家记录。** 跨进程需求和。数值上比以前 N× 虚高**更准确**，但看板口径要跟着调，否则会误以为 MD 变少了。

## 10. `vmem_node` 冻结区头 + 重建改造（已拍板纳入）

> **已拍板纳入**（§12 第 3 项）。我先前建议"独立评估、不捆绑"，理由是"vmem 是账本、重建会丢显存记账"。**这个理由经不起推敲，我收回**——见 §10.1。

### 10.1 收回先前的反对意见

我曾主张 vmem 不该重建，因为它是**账本**而非自校正的反馈量。但把 §4.5.4 的安全论证套上去就会发现这个担心是空的：

> **布局不符 ⟹ 文件来自上一世容器 ⟹ 账本里记的全是【已死进程】的分配。**

因为 `.so` 挂在容器内固定路径、宿主侧才按版本命名，**一个容器一生只加载一个 `.so` 版本**。所以布局不符时，那份账本的每一条记录都属于上一世容器里早已消失的 PID——**它们没有任何价值，清掉不丢失任何真实信息**。

对比两种处置：

| | 现状：报错退出（[loader.c#L1597](../library/src/loader.c#L1597)） | 改后：重建 |
|---|---|---|
| 丢失的信息 | —— | **死 PID 的陈旧记录（零价值）** |
| 容器能否启动 | **不能**（DRA 非 NRI 路径下升级即挂） | 能 |

所以"重建"严格优于"报错"，**且不存在我先前担心的代价**。你的判断是对的。

### 10.2 C 侧改造

```c
#define VMEM_NODE_MAGIC          0x564D4E44U   /* "VMND" */
#define VMEM_NODE_LAYOUT_VERSION 1U
/* 当前用量 = 128(头) + 16 * 16392 = 262,400B ≈ 256.25KiB。保留 320KiB（~1.25x）。
 * 理由见 §10.4：固定尺寸是为了【永不 resize】，而不只是为了留余量。
 * 320KiB 已拍板（§12.1 第 12 项）：节点上容器密度高、page cache 敏感，
 * 不为用不上的余量付每容器 256KiB 的代价。
 * 【硬约束】此值一经发版即冻结；要改必须 bump VMEM_NODE_LAYOUT_VERSION。 */
#define VMEM_NODE_FILE_SIZE (320 * 1024)

typedef struct {
  /* ┌── 冻结区：16 字节永久 ABI，与 sm_node 同构（§4.1）。 */
  uint32_t magic;             /* VMEM_NODE_MAGIC          */
  uint32_t layout_version;    /* VMEM_NODE_LAYOUT_VERSION */
  uint32_t region_size;       /* sizeof(device_vmemory_t) */
  uint32_t device_count;      /* MAX_DEVICE_COUNT         */
  /* └── 冻结区结束。 */
  uint8_t  _pad[CACHELINE_SIZE - 16];
  device_vmem_used_t devices[MAX_DEVICE_COUNT];   /* ← 整体后移 128B */
} device_vmemory_t;

_Static_assert(sizeof(device_vmemory_t) <= VMEM_NODE_FILE_SIZE, "vmem region must fit reserve");
_Static_assert(offsetof(device_vmemory_t, magic) == 0, "frozen header ABI");
```

`mmap_file_to_vmem_node` 改造与 §4.4.3 完全同构：`open(O_RDWR|O_CREAT)`（去掉现有的 `file_exist` **TOCTOU 竞态**）→ `ofd_fcntl` 整段串行化 → 尺寸不符则 `ftruncate(VMEM_NODE_FILE_SIZE)` → `mmap` → `header_valid()` 不符则就地 `memset` + 重建 → 发布 `magic`（RELEASE 序，最后写）。

> **一个白送的好处**：`GET_VMEMORY_LOCK_OFFSET` 用的是 `offsetof(device_vmemory_t, devices[i].lock_byte)`（[lock.c#L33](../library/src/lock.c#L33)），**`offsetof` 会自动把 128B 头算进去**，所以 C 侧这个宏**一个字都不用改**。

### 10.3 Go 侧改造（本设计唯一的跨语言 ABI 面）

Go 侧 [`pkg/config/vmem/vmem_config.go`](../pkg/config/vmem/vmem_config.go) **逐字节镜像**着 C 的结构体，必须同步：

```go
type DeviceVMemoryT struct {
    Magic         uint32          // ← 新增，与 C 冻结区对齐
    LayoutVersion uint32
    RegionSize    uint32
    DeviceCount   uint32
    _             [112]byte       // 头补齐到 128B
    Devices       [util.MaxDeviceCount]DeviceVMemUsedT
}
```

**⚠️ 最容易漏、且漏了会静默出错的一处**——`getVmemoryLockOffset()`（[vmem_config.go#L116](../pkg/config/vmem/vmem_config.go#L116)）：

```go
// 现状：只算了 devices 数组【内部】的偏移，没有基址
return int64(deviceIndex)*deviceSize + lockByteOffset
// 改后：必须加上 devices 数组在结构体中的基址（= 128B 头）
return int64(unsafe.Offsetof(DeviceVMemoryT{}.Devices)) +
       int64(deviceIndex)*deviceSize + lockByteOffset
```

**漏改的后果是静默的**：Go 会在**错误的字节偏移**上加 fcntl 记录锁 → 与 C 侧锁的字节范围**不重叠** → 两边都以为自己拿到了锁 → **互斥彻底失效** → Go 读到撕裂的账本。不会报错，只会得到错的 metrics。C 侧因为 `offsetof` 自动正确（§10.2），**唯独 Go 这一处需要人工保证**，因此列为 §9 高风险项。

Go 侧的校验语义（与 C 侧**不同**）：

```go
if size != VMemNodeFileSize          { return ErrUnknownLayout }   // 跳过，非报错
if data.Magic != VMemNodeMagic ||
   data.LayoutVersion != VMemNodeLayoutVersion { return ErrUnknownLayout }
```

> **关键不对称：Go 侧只读、只跳过，绝不重建。** §4.5.4 的"重建"授权**只属于容器内的库**，其安全性来自"布局不符 ⟹ 无活着的旧版本读者"。而宿主侧的 Go manager **不在容器生命周期约束内**——它若重建，会抹掉一个**正在运行**的容器的账本。Go 遇到不认识的布局，正确动作是**当作"本容器暂无数据"静默跳过**（现有调用点已按 `os.IsNotExist` 风格在 V(4) 级别吞掉，[container_lister.go#L206](../pkg/metrics/lister/container_lister.go#L206)），等容器重启后自然恢复。

### 10.4 为什么必须固定尺寸（而非 `sizeof` 跟随）

对 `sm_node` 固定尺寸只是简化；对 `vmem_node` 它**防的是一个真实的崩溃**：

> Go manager 把文件 `mmap` 了 256KiB。容器重启、新库把结构体改小并 `ftruncate` 缩容 → **Go 那 256KiB 映射的尾部落到 EOF 之外 → 访问即 SIGBUS → manager 进程崩溃。**

固定 `VMEM_NODE_FILE_SIZE` ⟹ 文件尺寸永不改变 ⟹ 该 SIGBUS 类**根本不存在**。这正是你说的"可能也需要第 6 点的改造加余量"——**而且比"留余量"更强：是"永不 resize"**。

### 10.5 升级期的行为（两个方向都安全）

manager 与库**分别版本化**（`.so` 按 manager 版本挂载，但容器一旦启动就固定用那一份），所以运行期必然出现 skew：

| 场景 | 结果 |
|---|---|
| manager 新（有头）× 容器旧（无头） | 尺寸不符 → Go 跳过 → **该容器 metrics 暂缺**，容器重启后恢复 |
| manager 旧（无头）× 容器新（有头） | 旧 Go 的 `size != sizeof(DeviceVMemoryT)` 检查已存在 → 拒绝 → 同上 |

两个方向都**优雅降级为"metrics 暂缺"**，不会崩、不会读到错数据。

> **这不是新引入的问题**：今天只要 `device_vmemory_t` 尺寸变化就是同样表现（Go 的尺寸检查已在 [vmem_config.go#L133](../pkg/config/vmem/vmem_config.go#L133) 拒绝）。本改造只是**该行为的一次具体实例**，并额外用 magic 覆盖了"尺寸相同但布局不同"这个尺寸检查抓不到的盲区。
>
> **且鲁棒性是净增的**：现状的 `memset` 建区[无任何锁保护](../library/src/loader.c#L1610)，本改造把它放进 OFD 写锁内，反而收窄了与 Go 读者的竞态窗口。

### 10.6 `main` 同步补充：报错退出实为两层，且整个区已 feature-gate 化

同步 `main` 后，§10 的两个前提需要更精确的表述：

**(1) "尺寸不符即容器不可用"是两层，不是一层。** 前文只点了 `mmap_file_to_vmem_node` 内部的 `return 1`（[loader.c#L1597](../library/src/loader.c#L1597)），但真正杀死容器的是**调用方**：

```c
/* load_controller_configuration()，loader.c#L2522 附近 */
if (g_vgpu_config->vmem_node && g_device_vmem == NULL) {
  ret = mmap_file_to_vmem_node(&g_device_vmem);
  if (ret) {
    pthread_mutex_unlock(&init_config_mutex);
    LOGGER(FATAL, "mmap vmem nodes file failed");   /* ← 这一行才是致命点 */
  }
  ...
}
```

`LOGGER(FATAL, ...)` 会终止进程。所以 §10.2 的改造**必须两层都动**：内层从"尺寸不符 → `return 1`"改成"布局不符 → 就地重建"；外层的 `FATAL` 应降级为 `WARNING` + 关闭本进程的 vmem_node 记账（与 §5 对 `sm_node` 的降级语义一致）。**只改内层是不够的**——`open`/`mmap` 真失败时（目录只读、插件漏挂）外层照样 FATAL 掉容器。

> **✅ 外层降级已实施**（§12.1 第 15 项，独立先行）。实施前做了两项必须的核查，结论都支持降级：
>
> 1. **`g_device_vmem == NULL` 是全库已支持的状态**：所有解引用点（`cleanup_vmem_nodes`、`check_cleanup_vmem_nodes*`、`add/sub_gpu_virt_memory`、`get_used_gpu_virt_memory` 等）**无一例外**都在 `if (g_device_vmem != NULL)` 之下；两个未自带守卫的 `rm_vmem_node_by_*` helper 的全部 3 个调用点也都在守卫内。更强的证据是：**`VMemoryNode` 特性门控默认 `false`**，所以 `NULL` 恰恰是今天绝大多数节点的**默认运行状态**——降级等于落回默认配置，不是落进未测路径。
> 2. **显存限额不会因此失效**（这是降级前最该问的问题）。限额判据是 `used + vmem_used + request_size > total_memory`（[cuda_hook.c#L289](../library/src/cuda_hook.c#L289)），其中 `used` 来自 NVML、**与本共享区无关**；`vmem_node` 缺失只让 `vmem_used` 归零，而该项存在的意义仅是补上 **NVML 看不见的 oversold/UVA 分配**。⟹ 限额**依然强制执行**，只是不再计入观测不到的那部分。若这一条不成立（例如限额完全依赖该账本），`FATAL` 反而是更安全的选择，就不该降级。
>
> 实现要点：`ret` 显式清零（可选区缺失不算配置加载失败）；**不在降级分支里 `pthread_mutex_unlock`**（与被替换的 `FATAL` 不同，控制流现在会走到 `DONE:` 统一解锁，手工再解一次就是双重解锁）；`atexit`/`sigaction` 注册**移入成功分支**——那些处理器的职责是退出时归还本进程的账本记录，没有账本就无事可归还，凭空改动容器的信号处置是多余的副作用。
>
> 未一并改动 `sm_watcher` 分支相邻的同类 `FATAL`：那是另一个共享区、另一条判据，不在本次范围内，仅在此记录。

> 注意紧邻的 `sm_watcher` 分支（[loader.c#L2512](../library/src/loader.c#L2512) 附近）有同样的 `FATAL`，但它**紧接着就是一个 `g_device_util == NULL` 的 WARNING 回退分支**，说明作者本来就认可"外部共享区不可用应回退而非致命"。vmem 分支缺的正是这个回退。

**(2) `vmem_node` 区现在是 feature-gate 化的，不是无条件存在。** 门控链路：

```
util.VMemoryNode feature gate (device-plugin/device-monitor 默认 false, Alpha)
  → VMEMORY_NODE_ENABLED env（device plugin 按 gate 注入；DRA 路径硬编码 TRUE）
    → vgpu.config 的 resource_data_t.vmem_node 字段
      → 库侧 `if (g_vgpu_config->vmem_node)` 才建区
```

对 §10 的两点影响：

- **验收（§7.2 第 6 条）必须显式开启该 gate**，否则区根本不存在，"Go 侧 metrics 与容器内一致"这条无从验证，会得到一个**假通过**。
- **§10.5 的升级期矩阵多一格**：`gate=off` 时两侧都没有区，Go 侧走的是"文件不存在"路径而非"布局不符"路径。行为仍是"metrics 暂缺"，结论不变，但排障时要先确认 gate 状态再怀疑布局。

**(3) §10 与 §4.5.5 的关系**：`main` 新增的 PID 回收 / 退出钩子**全部属于 `vmem_node`**，本设计不为它们做任何改动——§10 只加"冻结区头 + 重建 + 降级"，回收逻辑原样保留。两者互不干涉：回收处理的是**区内条目**，重建处理的是**区本身的布局**。

---

### 10.7 实施记录与新发现（阶段 2）

**权威数值**（由 C 侧实测，Go 侧测试以字面量钉死）：

| 量 | 值 |
|---|---|
| `offsetof(device_vmemory_t, devices)` | **128**（新增冻结区头）|
| `sizeof(device_vmem_used_t)` | 16392 |
| `offsetof(device_vmem_used_t, lock_byte)` | 16388 |
| `sizeof(device_vmemory_t)` | 262400（256.25 KiB）|
| `GET_VMEMORY_LOCK_OFFSET(i)` | `128 + i*16392 + 16388`（i=0 → 16516，i=1 → 32908）|

#### 新发现 1：初始化锁应只锁**头部 1 个字节**，不是整个文件

§10.2 原文写"`ofd_fcntl` 整段串行化"，照字面理解会去锁整个文件——**那是错的**。本区有**按设备的记录锁**（`GET_VMEMORY_LOCK_OFFSET(i)`，最低 16516），整文件写锁会与每一个读者/写者争用，**包括宿主侧 Go manager 的读锁**。而初始化真正需要排斥的只是"另一个进程也在初始化"。

故改为锁 `l_start=0, l_len=1`：字节 0 落在冻结区头内，**永远不是任何按设备锁的目标**，零交互。这是与 `sm_node` 的实质差异（`sm_node` 没有按设备记录锁，所以锁全文件无妨，见 §4.4.2b）。

#### 新发现 2：一个既有测试把"无区头"布局写死了

`TestGetVmemoryLockOffset`（既有）用 `i*stride + lockByte` 复现 C 宏，**不含 devices 基址**——因为过去该基址恰好是 0。加了区头后它立刻失败。

> 这不是坏事，是**证据**：它精确证明了偏移确实移动了 128 字节，也正是本设计反复强调"Go 侧必须手工加基址"的那 128 字节。已更新该测试（保留其原有意图：守护每设备步长回归），并**新增** `TestVMemoryLayoutMatchesC` 以 **C 侧实测字面量**钉死四个关键数值——用 Go 结构体自推导只能证明"Go 和自己一致"，证明不了跨语言一致。

#### 新发现 3：Go 测试的反向对照第一次是无效的

把 `base +` 直接删掉时，测试确实红了，但红在 **`[build failed]`**——Go 编译器报 `base` 未使用，测试根本没跑。改成 `base := int64(0)`（保持可编译）后才是有效对照：测试报 `expected 16516, actual 16388`，**差值正好是 128**。

> 教训与 §7.3 并发测试那次同源：**反向对照本身也要验证它验证的是你以为的东西。** 一个"红了"的对照可能红在完全无关的原因上。

#### 兼容性与降级复核

- **Go 侧只跳过、绝不重建**：`NewMmapDeviceVMemory` 尺寸/magic/版本任一不符 → 返回 `ErrUnknownLayout`；调用点（[container_lister.go#L207](../pkg/metrics/lister/container_lister.go#L207)）因其非 `os.IsNotExist` 而在 V(4) 记一行后**不加入该容器** = "本轮无数据"。重建授权只属于容器内的库（§10.3）。
- **重建中途被读**：`magic` 最后发布（RELEASE），Go 侧看到 `magic == 0` → 判定不识别 → 跳过。这正是"无锁 Go 读者仍然安全"的依据。
- **外层 `FATAL` 已在 `f31457c`（阶段 2-pre）降级**，本阶段不再涉及。

## 11. 决策摘要

- **手段**：容器内 `MAP_SHARED` 共享令牌桶 + 消费端原有 CAS（不改）+ 补充端每周期 CAS 抢权。
- **不做**：锁串行化（钝、贵、死锁）、`sleeping` 协调（每进程语义、共享桶已天然覆盖）、Event 占空比（伤流水线，正交路线）。
- **陈旧清理交给控制面**（§4.5）：新增 `/tmp/.sm_node` 专用挂载（与 `/tmp/.vgpu_lock`、`/tmp/.vmem_node` 同构，**不用容器自己的 `/tmp`**——可能被覆盖挂载或只读）；插件在**每次容器启动前删缓存文件**，复用已在为 `vmem_node` 跑的现成钩子（`PreStartContainer` + NRI `CreateContainer`）→ 库 attach 时必然是全新零字节区。**因此删掉了库内 generation 与 ns `st_ino` 探测**：那是在重造一个已存在且更可靠的轮子。库内只留 `magic`/`layout_version`/`region_size` **布局守卫**作第二道防线，防**库升级后新库读老结构体**（`DRA 非 NRI` 路径无启动钩子，且清理本身 best-effort）。
- **不用 `volatile`、不用 `_Atomic`**（§4.10）：`volatile` 不提供并发保护（现有安全性全来自 CAS）；`_Atomic` 在跨进程共享内存下有 lock-free 退化（libatomic 锁表**每进程一份** → 静默失效）与 size/align ABI 风险。用**普通定宽类型 + `__atomic_*` 内建 + 显式内存序**，合仓库既定习惯（[hook.h#L63](../library/include/hook.h#L63) 已硬性要求 GCC/Clang+glibc）。
- **布局不符 → 重建，不是报错退出**（§4.5.4）：现有 vmem 区在尺寸不符时 `return 1`（[loader.c#L1597](../library/src/loader.c#L1597)），等于库升级后容器不可用。本区一律**就地 memset 重建**；文件尺寸恒定（`SM_NODE_FILE_SIZE`）使版本升级永不需要 resize。就地重建之所以安全：**布局不符 ⟹ 文件来自上一世容器 ⟹ 无活着的旧版本读者**（容器一生只加载一个 `.so` 版本），故不需要 unlink/rename，也就避开了 rename 竞态下"共享桶静默退化成两个私有桶"的失效模式。
- **永不致命**（§2.1/§5）：`open`/`mmap` 失败、插件漏挂目录、目录只读 → 一律**降级回进程私有桶**，绝不 `exit`/让 CUDA 调用失败。多进程隔离是优化，不是正确性前提。
- **不抬高工具链门槛**（§4.11）：只用仓库已在用的设施；OFD 锁复用 [`lock.c#L64`](../library/src/lock.c#L64) 的**运行时回退**封装（低于 3.15 的内核照样跑）；明确禁用 `memfd_create`/`O_TMPFILE`/`renameat2`/`statx`。
- **热路径开销不变**（§4.12）：仍是一条 CAS。守卫/文件锁/建区**全部每进程一次**，`pthread_once` 之下；初始化锁是**阻塞锁**（等待者内核睡等、不空转，§4.4.2a）；唯一真实新增开销是共享 cacheline 弹跳，交给阶段 3 且**仅在 profiling 证明后**才压。
- **三种控制器都兼容，但代价是整个控制状态块入共享区**（§4.13）：`delta` 是纯函数、天然兼容；`aimd` 的 `md_cooldown` 不共享会触发**它自己存在的意义所要防的 MD 雪崩**（`md_divisor^N`）；`auto` 还额外需要排他 FSM ×3。三者共用的 `throttled_since_watch` 也必须共享，否则 bypass 会误放行。**算法逻辑一行不改，改的只是状态的存储位置。**
- **`vmem_node` 一并加冻结区头 + 重建**（§10）：我先前"vmem 是账本、重建丢记账"的反对**已收回**——布局不符 ⟹ 账本里全是**已死进程**的记录 ⟹ 清掉零损失，而报错退出会让容器起不来。代价是**唯一的跨语言 ABI 面**：Go 侧结构体 + `getVmemoryLockOffset` 必须同步（漏改会**静默**让 fcntl 锁失效）。固定尺寸不只为余量，更是为**永不 resize**，避免缩容把 manager 的 mmap 打成 SIGBUS。
- **同时满足**"严格"（物理共享桶）与"低开销"（一条 CAS、不串行化），且崩溃自愈、fork 安全。
- **单进程场景零收益但零代价**（默认关闭）；notebook 等多进程容器是首要目标场景。

---

## 12. 拍板记录与剩余开放项

### 12.1 已拍板（本节为定稿依据）

| # | 决定 | 对设计的影响 |
|---|---|---|
| 1 | **做**。多进程竞争必然超限；notebook 容器必受影响 | **撤销阶段 0 硬前置**。§7 测量从"准入门槛"降级为"验收基线"（§1.3） |
| 2 | **三种控制器全支持**（delta/aimd/auto） | 结构体保持完整形态；`md_cooldown` + FSM×3 必须搬（§4.13）；范围缩减方案否决 |
| 3 | **`vmem_node` 一并纳入**，冻结区头 + 重建，含 Go 控制面改造，按需加余量 | 新增 §10；引入唯一跨语言 ABI 面；**我先前的反对已收回**（§10.1） |
| 4 | **DRA 非 NRI 路径不补清理钩子** | 残留自校正，布局守卫兜底（§4.5.3 结论不变） |
| 5 | 目录 `sm_node` / 文件 `sm_node.config` | §4.5.1 |
| 6 | `SM_NODE_FILE_SIZE = 8192` | §4.1；`vmem_node` 取 `320KiB`（见第 12 项） |
| 7 | env `CUDA_SM_SHARED_BUCKET`，默认 `0` | §5 |
| 8 | `REFILL_PERIOD_NS` = 现 watcher 单设备周期（~80–100ms） | §4.3 |
| 9 | Go 侧**暂不**读 `sm_node` | `sm_node` 不是跨语言 ABI，冻结约束仅对库内生效。**注意**：第 3 项使 `vmem_node` **是**跨语言 ABI（§10.3） |
| 10 | **开关走 `g_dynamic_config` + env，不改 `resource_data_t`**（`main` 同步后新增拍板） | §5.1。改 `resource_data_t` 会触发 `CheckResourceDataSize` 重调度；且范式 A 不排斥将来由控制面注入同一个 env |
| 11 | **不为 `sm_node` 引入任何 PID 回收 / 退出钩子**（`main` 同步后新增拍板） | §4.5.5。判据是"桶不是账本"；该结论绑定在"`sm_node` 无按 PID 归属字段"这一前提上 |
| 12 | **`VMEM_NODE_FILE_SIZE = 320KiB`**（~1.25x 余量），非 512KiB | §10.2。理由：节点容器密度高、page cache 敏感。**发版即冻结** |
| 13 | **`vmem_node` 与 `sm_node` 不合并** | 保持两个区。§12.2 第 2 项结论固化：生命周期与读者不同，合并会把 `sm_node` 拖进跨语言 ABI |
| 14 | **阶段 3（本地批量取令牌）暂不做** | §8。不是"永久不做"，而是**不进入本轮实施范围**；重启条件仍是 §4.12 的 profiling 证据 |
| 15 | **`vmem_node` 的 `FATAL` 降级拆成独立 PR 先行** | §10.6 / §12.2 第 4 项。与冻结区头正交，可独立回滚 |

### 12.2 剩余开放项（不阻塞开工，实现期定）

**§12.2 原有的 4 项已全部拍板结清**（见 §12.1 第 12–15 项）。当前剩余开放项：

~~1. 阶段 1b 的 `total_cuda_cores` 首建者取值~~ —— **实施期已解决，两个都不用选**。映射点落在 `initialization()` 内、`init_device_cuda_cores()` 与 `sm_controller_init()` 之后、`balance_batches()` 之前，此时设备几何、`hard_core`、env 开关**全部已知**，watcher 也还没启动。

更重要的是实现期改了一个判断：**`change_token` 的钳制上界仍读进程私有的 `g_total_cuda_cores[]`，而不是共享区的 `total_cuda_cores`。** 理由是消除排序风险——共享值若在某个进程读到的瞬间还是 0，桶会被钳到 0、整容器被节流；而该值由设备属性推出，每个进程算出的完全相同，本地读永远正确。共享区的 `total_cuda_cores` 因此降级为**纯观测字段**（ABI 保留，初始化后发布一次）。

`up_limit` 的初值则相反，必须**在建区时**写（`sm_node_rebuild_locked` 里写 `hard_core`）：若沿用"watcher 线程启动时初始化"，一个晚加入的进程每次启动 watcher 都会把容器**已收敛的** up_limit 打回 hard_core。这是共享化之后才出现的新失效模式，原设计未覆盖。

### 12.3 我已自行决定的（理由在文档内，不同意直接打）

内存序选择（§4.10）、`ofd_fcntl` 复用与回退（§4.4.2）、**初始化用阻塞锁而非自旋、且不设超时**（§4.4.2a，你已确认此方向）、就地重建 vs unlink/rename（§4.5.4）、冻结区字段集与永久 ABI 约束（§4.1）、Go 侧"只跳过不重建"的不对称授权（§10.3）、实施阶段拆分（§8）。
