# 容器内多进程算力隔离：共享令牌桶设计

> 作用范围：`library/`（LD_PRELOAD 运行时库）为主；另涉及 `pkg/deviceplugin/vgpu`、`pkg/kubeletplugin`（新增 `/tmp/.sm_node` 按容器挂载 + 启动前清理，见 §4.5）。
> 目标：修复"同容器多进程各自持有私有令牌桶、瞬时叠加突破算力限额"的问题，做到**聚合限额严格**且**不引入锁 / 不串行化 kernel 发射**。
> 状态：设计稿（未实现；默认关闭，环境变量灰度开启）。
> 关联：[GAP 路径节流](./sm_core_limit_gap_throttle_design.md)、[AIMD 控制器](./sm_controller_aimd.md)。

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

`rate_limiter` 用 CAS 扣减（[cuda_hook.c#L588](../library/src/cuda_hook.c#L588)），watcher 每 ~80ms/设备用 NVML 采样、经 `delta()`/`aimd()` 反馈后用 `change_token()` 补充。控制器积分态同样是进程私有的 static：[`shares[]`](../library/src/cuda_hook.c#L1100)、[`up_limits[]`](../library/src/cuda_hook.c#L1122)、`is[]`、`avg_sys_frees[]`。

### 1.2 核心问题：N 个私有桶瞬时叠加突破限额

容器里启动 N 个计算进程时，每个进程有**自己的** `g_dev_hot[]`。同一时刻 N 个 `rate_limiter` 各自看自己的桶，都可能判定"令牌够，放行"，于是 N 份令牌被同时消费、N 批 kernel 被同时发射 —— GPU 上实际叠加，**瞬时利用率可达单进程限额的 ~N 倍**。

需要澄清一个**容易被夸大的点**：watcher 采样的 `user_current` 是**容器聚合利用率**（把容器内所有 PID 的 util 累加，见 `get_used_gpu_utilization` 里的 `check_device_pid_in_ordered_container_pids` 聚合逻辑）。这意味着 N 个控制器**观测的是同一个共享反馈信号**：聚合 util 一旦超限，每个进程的 `delta` 都会砍自己的 share，总吞吐随之下降。所以：

- **稳态均值仍收敛到限额**（不是"完全失效"）；
- 真正的损害是 **N 个相同控制器盯同一信号、同步涨同步砍 → 等效增益放大 ~N 倍**，叠加"N 个桶可被同时抽干"，表现为**瞬时突发放大 ~N 倍、限额附近振荡幅度 ~N 倍**。

准确结论：**多进程下限流"变松、变抖"，而非失效。** 这决定了本设计是"按需的严格化"，不是"救火"。

### 1.3 触发条件（决定要不要做）

- **单进程容器（ollama / llama-server / 单个训练进程）**：问题**不存在**，本设计**收益为零**。
- **多进程容器**（多路并发推理、DataLoader 多 worker 真正各开 CUDA context、多进程训练）：问题存在，程度取决于 N 与突发性。

**实施前必须先做 §7 的量化验证**，确认聚合超限是系统性的，才动手。

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

- **CAS 是 CPU 指令、与地址空间无关**：桶从 `static` 变成 `MAP_SHARED`，[rate_limiter 的 CAS](../library/src/cuda_hook.c#L588) **一个字都不用改**就变成跨进程原子扣减。这是本方案最省力、也最关键的支点。
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

复用仓库已有的跨进程共享范式（[`mmap_file_to_vmem_node`](../library/src/loader.c#L1372)：`open(O_CREAT)` + `ftruncate` + 首建者 `memset` + `mmap(MAP_SHARED)`），新增一个 SM 令牌桶共享区。

**这个结构体是一份 ABI**：它落在文件上、被多个进程映射、跨库版本存活（见 §4.5），宿主侧的 Go 代码也已有读取同类结构的先例（[container_lister.go#L205](../pkg/metrics/lister/container_lister.go#L205) 读 vmem_node）。因此字段一律用**定宽类型 + 显式 padding + `_Static_assert` 钉死布局**。

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

现有 [rate_limiter](../library/src/cuda_hook.c#L565) 的 CAS 循环逻辑**不变**，只把操作对象从 `g_dev_hot[host_index].cur_cuda_cores` 换成共享区 `g_sm_node->devices[host_index].cur_cuda_cores`（开启共享模式时）：

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

### 4.4 建区 / 重建：初始化路径用内核文件锁串行化

**不照抄 vmem 区的写法**。现有 [`mmap_file_to_vmem_node`](../library/src/loader.c#L1366) 有两个缺陷，本设计都不继承：

1. **TOCTOU 竞态**：`file_exist()` 判断在前、`open(O_CREAT)` 在后，两个进程可能双双认定 `created = 1`，双双 `ftruncate` + `memset` → **互相抹掉对方刚写的内容**。
2. **尺寸不符即报错退出**（[loader.c#L1399](../library/src/loader.c#L1399)）——正是本节要根治的行为，见 §4.5.4。

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

#### 4.4.3 建区/重建流程

```c
/* 每进程一次，在 pthread_once 之下调用；任何一步失败都 → 降级私有桶，绝不 exit */
fd = open(SM_NODE_FILE_PATH, O_RDWR | O_CREAT | O_CLOEXEC, 0644);  /* 无 TOCTOU */
if (fd < 0) return FALLBACK_PRIVATE;

ofd_fcntl(fd, /*wait=*/1, &(struct flock){.l_type = F_WRLCK, ...});  /* 整段串行化 */

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
| device plugin | [vnum_plugin.go#L846](../pkg/deviceplugin/vgpu/vnum_plugin.go#L846) `Allocate` 的 `response.Mounts` | 加挂 `sm_node` 目录 |
| device plugin | [vnum_plugin.go#L823](../pkg/deviceplugin/vgpu/vnum_plugin.go#L823) `EnsureDir` | 建 `sm_node` 目录 |
| DRA（CDI） | [vgpu.go#L288](../pkg/kubeletplugin/vgpu.go#L288) `GetPartitionMountContainerEdits` | 加 CDI mount |
| DRA（NRI） | [vgpu.go#L345](../pkg/kubeletplugin/vgpu.go#L345) `GetNRIPartitionInjection` | 加 NRI mount |
| DRA 两路共用 | [vgpu.go#L141](../pkg/kubeletplugin/vgpu.go#L141) `ensurePartitionDirectories` 的 `preparedDirs` | 建 `sm_node` 目录 |
| NRI 观测 | [nri/plugin.go#L73](../pkg/kubeletplugin/nri/plugin.go#L73) `mountDestsOfInterest` | 加 `/tmp/.sm_node`（仅日志高亮） |

#### 4.5.2 陈旧清理：复用现成的"每次启动前删缓存"钩子

**关键事实：这套机制已经在跑，不是新发明。** 两处都已经在删 `vmem_node.config`：

`PreStartContainer`（device plugin 路径，[vnum_plugin.go#L1094](../pkg/deviceplugin/vgpu/vnum_plugin.go#L1094)）：

```go
// Clean up old cache files before each startup
pidsConfigPath := filepath.Join(configDirPath, registry.PidsConfig)
vmemNodeConfigPath := filepath.Join(configDirPath, util.VMemNode, util.VMemNodeFile)
_ = os.RemoveAll(pidsConfigPath)
_ = os.RemoveAll(vmemNodeConfigPath)      // ← sm_node.config 加在这里
```

其可靠性由 [vnum_plugin.go#L228](../pkg/deviceplugin/vgpu/vnum_plugin.go#L228) 的 `PreStartRequired: true` 保证——kubelet 在**每次容器启动前**（含重启）调用它，代码注释 "before each startup" 正是此意。

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

**缺口成因**：DRA 非 NRI 路径的挂载由 CDI 注入，而 CDI spec 在 `NodePrepareResources`（**每 claim 一次，Pod 准入时**）生成；容器重启时运行时只是重新套用磁盘上已有的 spec，**没有任何插件代码运行**。`ensurePartitionDirectories`（[vgpu.go#L141](../pkg/kubeletplugin/vgpu.go#L141)）只 `EnsureDir` 不删除；会 `RemoveAll` 的 `ensureClaimDirectories`（[vgpu.go#L132](../pkg/kubeletplugin/vgpu.go#L132)）也只在 Prepare 时跑。

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

- **布局不符 → 重建，不是报错退出。** 这是本设计与现有 vmem 区的关键分歧：[loader.c#L1399](../library/src/loader.c#L1399) 在尺寸不符时 `LOGGER(ERROR)` + `return 1`，等于**库升级后容器直接不可用**。本区一律重建。
- **任何不可恢复的错误（`open`/`mmap` 失败）→ 优雅降级回进程私有桶**，等价于该进程没开这个特性。**绝不 `exit`/`abort`/让 CUDA 调用失败**——多进程算力隔离是一个**优化**，不是正确性前提，不值得为它牺牲可用性。
- `magic` **最后写、用 RELEASE 序**：重建中途若进程被杀，`magic` 仍是旧值/0 → 下一个进程照样判定不符 → 再次重建。**重建是幂等的、可中断的**，没有"半初始化"稳态。

#### 为什么就地重建不会踩到活着的旧版本读者

关键论证：**布局不符 ⟹ 该文件来自上一世容器 ⟹ 此刻没有任何进程正映射着旧布局。**

因为 `.so` 挂在容器内的**固定路径**（`ContVGPUControlFilePath`），宿主侧才是版本化的 `libvgpu-control.so.<version>`。**一个容器一生之内，所有进程加载的必然是同一个 `.so` 版本**——运行中的容器不会换库。因此同一时刻映射同一个区的进程，布局必然一致；布局不一致只可能发生在"上一世写的文件、这一世新库来读"，而那时上一世的进程全都不在了。

> **推论**：不需要 `unlink` + 重建新 inode，也不需要 `rename` 原子发布。那些手法是为了"新旧读者并存"而设计的，而这个前提在此**不成立**。就地 `memset` 严格更简单，且避免了 `rename` 竞态下"两个进程各自映射到不同 inode、共享桶退化成两个私有桶"的**静默失效**——那才是真正危险的失败模式。
>
> **唯一的例外**是宿主侧的 Go 读者（[container_lister.go#L205](../pkg/metrics/lister/container_lister.go#L205) 读 vmem_node 的先例）：它不在容器生命周期约束内。若将来给 `sm_node` 加宿主侧读取，**必须让 Go 侧也校验 `magic`/`layout_version` 并容忍读到重建中的区**（读到不符就当作"本周期无数据"，而不是报错）。

- 控制面刚删完文件 → 新建的区全 0 → 天然 `magic != MAGIC` → 走 `rebuild_region_locked`。**"控制面已清理"与"库内兜底重建"是同一条代码路径**，不是两套逻辑。
- **改结构体必须 bump `SM_NODE_LAYOUT_VERSION`** —— 本设计对未来维护者的硬约束，应写进结构体上方注释。冻结区的 4 个字段**永远不得改动**（§4.1）。

#### 附：这条规则的适用边界（重要，别改错地方）

`loader.c` 里 `file size mismatch` 出现在**三处**，但**只有一处**该改成重建。判据是**这个区归谁所有**：

| 函数 | 映射方式 | 数据所有者 | 尺寸不符时该怎么办 |
|---|---|---|---|
| [`mmap_file_to_config_path`](../library/src/loader.c#L1302)（`resource_data_t`） | `MAP_PRIVATE`/`PROT_READ` | **控制面**（manager 写 `vgpu.config`） | **保持报错**（见下） |
| [`mmap_file_to_util_path`](../library/src/loader.c#L1334)（`device_util_t`） | `MAP_PRIVATE`/`PROT_READ` | **外部 watcher** | **保持报错** |
| [`mmap_file_to_vmem_node`](../library/src/loader.c#L1366)（`device_vmemory_t`） | `MAP_SHARED`/读写 | **库自己** | 可以重建（但见下） |

**前两处必须保持报错，改成"重建"是错的**：它们是**只读消费**控制面产出的文件，库既无权也无力重建自己不拥有的数据——凭空造一份 `vgpu.config` 只会让容器带着错误的配额跑起来，比起不来更糟。而且 `resource_data_t` 的尺寸校验是**被设计成失败的**：[vnum_plugin.go#L1085](../pkg/deviceplugin/vgpu/vnum_plugin.go#L1085) 在 `Reschedule` 门控下调用 `CheckResourceDataSize`，注释写明"When a version upgrade causes a change in the configuration structure, the controller can reschedule these pods that cannot be started"——**升级后起不来是预期行为，由控制器重新调度兜底**。

> **本设计确立的规则应精确表述为**：*库自己拥有的共享区（`MAP_SHARED` 读写）布局不符 → 重建；只读消费控制面产出的区 → 保持报错，交给上层的重调度机制。* `sm_node` 属于前者。

**`vmem_node` 属于前者，但仍不在本设计范围内**。它确实有同样的缺陷（[loader.c#L1399](../library/src/loader.c#L1399) 尺寸不符即 `return 1`，DRA 非 NRI 路径下库升级会让容器起不来），但 **vmem 区是账本**（§4.5.4 的关键区别）：重建 = 丢失全部显存记账 = 已分配的显存变成"没人认领"，后果比令牌桶重建（几个控制周期收敛）严重得多。"重建 vs 报错 vs 交给重调度"哪个危害最小，需要单独评估，**不应捆绑进本设计**。此处仅记录问题与关联。

### 4.6 gap 检测的 `last_launch_ns`：**保持进程私有**

[GAP 路径](./sm_core_limit_gap_throttle_design.md) 的 `last_launch_ns`（[cuda_hook.c#L110](../library/src/cuda_hook.c#L110)）语义是"**本进程**上次发射到现在的空闲间隔"，用于判断"本进程是否刚从 >200ms 空闲醒来"。这是**进程本地**的时序，**不应共享**：

- 若共享 → A 频繁发射会一直刷新 `last_launch_ns` → B 即使真的空闲很久也检测不到自己的 gap，GAP 路径失效。
- 故 `last_launch_ns` 留在**进程私有的 `g_dev_hot[]`**，只把 `cur_cuda_cores` 及控制器态迁到共享区。

> 结论：`g_dev_hot[]` 拆成"共享的桶+积分态"和"私有的 gap 时序"两部分。

### 4.7 bypass 的 SET → 必须改（最易踩的坑）

现有防抖 bypass 是**直接赋值**（[cuda_hook.c#L1242](../library/src/cuda_hook.c#L1242)）：

```c
g_dev_hot[host_index].cur_cuda_cores = g_sm_controller(...);   // SET，非累加
```

共享桶下，一个进程的 SET 会**抹掉**并发消费者刚 CAS 扣掉的令牌 → 令牌凭空多出来 → 超发。**必须改**：

1. bypass 只在**补充选举赢家**里执行（和 §4.3 补充同属"赢家专属"块）；
2. 且改成**累加语义**（`change_token` 加）而非 SET，或用 CAS 把"目标值"安全地写入而不覆盖并发扣减。
   - 推荐：把 bypass 的"钳制到单步"语义重写为"补充到目标水位"的**增量**（`delta_tokens = target - current`，再 `change_token(delta_tokens)`），使其与消费者的 CAS 扣减可交换、不丢账。

> 这是共享化改造里语义最微妙的一处，需单独单测（并发扣减 + bypass 补充不丢令牌）。

### 4.8 change_token 的累加也要跨进程原子

现有 [`change_token`](../library/src/cuda_hook.c#L562) 已经是 CAS 循环（`before + delta`，钳制 `[0, total]`）。迁到共享区后 CAS 目标换成共享计数器即可，**逻辑不变**。补充者（选举赢家）用它累加，消费者用 rate_limiter 扣减，两者都是对同一 `cur_cuda_cores` 的 CAS → 天然并发安全。

### 4.9 反馈信号：无需改

`user_current` 已是**容器聚合** util（跨进程无关的量）。补充选举赢家用**它自己的**那次 NVML 采样即可，值与"哪个进程采的"无关。所以反馈侧零改动。

---

### 4.10 原子性：用 `__atomic_*` 内建，不用 `volatile`，也不用 `_Atomic`

**先澄清**：现有 `dev_hot_t` 上的 `volatile` **没有提供任何并发保护**——它不保证原子性、不保证跨线程顺序，只是禁止编译器缓存该变量。今天的正确性 **100% 来自 [`CAS` 宏](../library/include/hook.h#L168)**（`__sync_bool_compare_and_swap`，自带全序）。`volatile` 在此基本是装饰性的，且**有害**：它让人误以为存在它并不提供的保护。共享区**不带 `volatile`**。

**为什么也不用 `_Atomic`**（尽管它是"标准正确"的工具）：

1. **lock-free 是硬要求，否则跨进程静默失效**。`_Atomic T` 若非 lock-free，编译器退化为 libatomic 中**按地址索引的锁表**，而那张表**每进程一份** → 两个进程映射同一块共享内存会各自用**不同的锁** → 保护形同虚设、且不会报错。
2. **`_Atomic T` 的 size/alignment 允许与 `T` 不同**。本结构体是落盘的跨进程 ABI（§4.1），且宿主 Go 侧已有读同类结构的先例，多一层由编译器决定的布局变数是净负担。
3. **不合仓库既定习惯**。[hook.h#L63](../library/include/hook.h#L63) 已硬性要求 GCC/Clang + glibc，全库用的就是 GCC 内建（`CAS` = `__sync_bool_compare_and_swap`；`src/vulkan/` 用 `__atomic_*`；[cuda_hook.c#L583](../library/src/cuda_hook.c#L583) 用 `__atomic_store_n`）。

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
| 文件锁（`ofd_fcntl` 加/解） | **每进程一次** | 2 次 syscall |
| `open`/`ftruncate`/`fstat`/`mmap` | **每进程一次** | 4 次 syscall |
| 补充选举（`CAS(last_refill_ns)`） | 每 watcher 周期（~80ms）/设备 | 1 条 CAS |
| **`rate_limiter` 扣令牌** | **每次发射** | **1 条 CAS（= 现状，未增加）** |

> **必须守住的实现纪律**：守卫**只在初始化时校验一次**，把区指针缓存进 `g_sm_node`；**绝不允许**在 `rate_limiter` 里做 `magic` 校验、`NULL` 判断以外的任何检查。每次发射多一次分支都是不可接受的——这条路径的调用频率是 kernel 发射频率。

**唯一真实的新增开销**是共享桶的**跨进程 cacheline 弹跳**：单进程时 `cur_cuda_cores` 独占本核 L1；多进程时 N 个核争抢同一条 cacheline，CAS 延迟从 ~ns 升到 ~几十 ns。这正是 §8 阶段 2（本地批量取令牌）要压的对象，**且仅在 profiling 证明它是真开销时才做**。

> 注意此开销**只在开启共享桶时产生**（默认关闭，§5），且它换来的是限额从"N 倍松"变严——这个交换是否值得，由 §7 的阶段 0 数据决定，而不是先验假设。

### 4.13 与三种控制器的兼容性（delta / aimd / auto）

> **本节修正了本设计一个实质性缺陷。** 前几稿默认"只把令牌桶搬进共享区、控制算法不动"（§2.2 非目标）。核查代码后：**对 `delta` 成立，对 `aimd` 不成立，对 `auto` 也不成立**。必须把**全部跨周期控制状态**一起搬进共享区（§4.1 结构体已相应扩充）。

先明确一件被前几稿忽略的事实：控制器有**三种**，不是两种（[cuda_hook.c#L682](../library/src/cuda_hook.c#L682)）：

```c
enum { SM_CONTROLLER_DELTA = 0, SM_CONTROLLER_AIMD = 1, SM_CONTROLLER_AUTO = 2 };
```

`auto`（[auto_routed_controller](../library/src/cuda_hook.c#L985)）按排他性**逐设备逐周期**在两者间路由，所以它同时继承两者的约束。

#### 4.13.1 判据：什么状态必须共享

选举（§4.3）把"谁跑控制器"这件事**每周期换人**。于是：

> **凡是"只有选举赢家推进"的跨周期状态，都必须放进共享区。** 否则每个进程各存一份、各自只在自己赢的那 1/N 周期推进 → 语义碎裂成 N 份，且每份都以 ~1/N 的速率演进。

反之，**每周期从当周期观测重算的临时量**保持进程私有即可（`top_results[]` 采样、`sys_frees[]`（[写 L1206](../library/src/cuda_hook.c#L1206) → 同周期 [读 L1300](../library/src/cuda_hook.c#L1300)，是 scratch 不是积分态）、排他 memo `g_excl_memo_*`）。所有进程观测的是同一个容器聚合信号，重算结果一致。

#### 4.13.2 `delta`：无状态，**天然兼容**

[`delta()`](../library/src/cuda_hook.c#L592) 是**纯函数**：输出只取决于入参 `(up_limit, user_current, share)` + 设备几何（`g_sm_num`/`g_max_thread_per_sm`，各进程相同）+ `g_dynamic_config`（只读）。**没有任何跨周期自有状态。**

⟹ 只要 `share` / `up_limit` 在共享区（已在），谁来跑 `delta` 都得到同一个结果。**选举对 delta 完全透明，零额外改动。**

#### 4.13.3 `aimd`：有 `md_cooldown`，**不改会触发 MD 雪崩**

[`aimd_controller()`](../library/src/cuda_hook.c#L783) 持有一个跨周期积分态 [`g_aimd_md_cooldown[]`](../library/src/cuda_hook.c#L699)，其声明注释写明了它赖以成立的不变量：

> *"Per-device remaining cooldown counter. **Watcher-thread-only access** (each watcher thread owns a disjoint host_index slice via balance_batches). No volatile / atomics needed."*

**而选举恰好打破了这个不变量**——host_index 不再被某一个线程独占，而是每周期换一个**进程**来跑。后果是**最坏的那种**：

| 周期 | 赢家 | 该进程的 `md_cooldown` | 动作 |
|---|---|---|---|
| 1 | A | 0 | util 超限 → **MD 触发**，A 的 cooldown = 4 |
| 2 | B | **0**（B 自己那份从没被推进过） | util 仍超限 → **MD 再次触发** ← 本该被拦住 |
| 3 | C | **0** | → **MD 第三次触发** |

`share` 被连续砍成 `md_divisor^N`（默认 3^N；N=4 → **81 倍**）。而 cooldown 的存在**正是为了阻止这个**——[代码注释](../library/src/cuda_hook.c#L846)：

> *"NVML's ~80ms sample + share-take-effect lag (~200-400ms total) means consecutive MD cuts share by md_divisor^N before the first cut's effect surfaces, hence **"MD avalanche"**. Cooldown breaks the chain."*

**修复**：`md_cooldown` 移入共享区（§4.1 已加 `md_cooldown` 字段）。移入后只有赢家读写它，被选举串行化，语义与今天的单进程完全一致——cooldown 计的是**全局周期数**，而这正是它本来的语义（注释说的是 "time-based semantics"）。

#### 4.13.4 `auto`：还需要排他 FSM + 节流标志

`auto` 经 [`host_index_is_exclusive_debounced()`](../library/src/cuda_hook.c#L942) 路由，该 FSM 有三个跨周期字段（`g_is_exclusive_debounced` / `g_exclusive_pending_streak` / `g_lost_exclusivity_pending`），其注释同样声明依赖"watcher 线程独占"：

> *"every field below is written and read exclusively by the watcher thread that owns the corresponding host_index. No cross-thread read; no volatile needed."*

FSM 的三个调用点（soft burst 门、hard_limit jitter 门、auto 路由）**全都在赢家的控制块内**，所以把三个字段移入共享区后，FSM 每周期恰好被推进一次（由当周期赢家），**语义与单进程一致**——debounce 计的是全局周期数，正是其本意。

> `g_lost_exclusivity_pending` 尤其**不能**留私有：它是"true→false 翻转"时置位、由后续 reset 分支[消费清零](../library/src/cuda_hook.c#L1297)的**一次性标志**。若各进程各存一份，非赢家的标志会一直悬着，等它某个周期赢了才消费 → **迟到数个周期的、莫名其妙的 reset**。

**另一处必须共享的是 `throttled_since_watch`**（不属 FSM，但同类问题）。它由 `rate_limiter` 在节流时置位、watcher 每周期 [read-and-clear](../library/src/cuda_hook.c#L1210)，用于给防抖 bypass 把门（§4.7）。共享桶下：

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

> **可选的范围缩减**：若阶段 0 数据只支持在 `delta` 下使用共享桶，可以让共享模式**只对 `delta` 生效**，`aimd`/`auto` 下自动降级回私有桶（§5 的降级路径现成）。这样 §4.1 结构体可退回到只含 `share`/`up_limit`，改动面显著缩小。**此项需拍板**（§11）。

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

这条降级路径覆盖了一整类现实故障：**插件漏挂目录**（§4.5.1 的 5 处落点漏改任一处）、目录只读、`/tmp` 被业务覆盖挂载、老版本插件配新版本库。它们的后果统一是"退回今天的行为"，而**不是容器起不来**——这正是 §4.5.4 "永不致命"约定的落地点。

---

## 6. 正确性与 fork 边界

### 6.1 fork 语义（白送的好处）

- `MAP_SHARED` 映射**跨 fork 保留** → 子进程自动 attach 同一个桶，无需额外处理，天然参与聚合限流。
- 现有 [`child_after_fork`](../library/src/cuda_hook.c#L238) 重置 `g_dev_hot[].last_launch_ns=0`：迁移后 `last_launch_ns` 仍在私有 `g_dev_hot[]`，此重置**保持不变**（正确：子进程的 gap 时序应重新计）。
- 共享区里的 `cur_cuda_cores`/`share` **不应**在 child_after_fork 重置（那是全容器共享状态，子进程只是新加入的消费者/候选补充者）。

### 6.2 不新增锁 → 不新增 fork 死锁面

本方案**不引入任何用户态 mutex**（热路径全是 CAS + 选举；初始化用内核文件锁），因此**无需**动 [`loader_child_after_fork`](../library/src/loader.c#L2264) 的 mutex 重init 列表，规避了"父持锁 fork → 子死锁"这一整类隐患。这是相对 HAMi 锁方案的结构性优势。

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

## 7. 实施前必须做的量化验证（阶段 0）

**没有这一步不写实现代码。** 目的：证明问题对目标负载真实且系统性。

1. 起一个 **N 进程并发计算**的容器（如 N=4 个并发推理/训练进程），设 `hard_core`。
2. `LOGGER_LEVEL=5` + `nvidia-smi pmon`，记录：
   - **聚合 util**（容器内所有 PID 之和）相对 `hard_core` 的**超出幅度**与**振荡幅度**；
   - 对比单进程同负载的曲线。
3. 判据：
   - 若"均值贴 `hard_core`、仅瞬时冲高" → 现状够用，**不做**本设计；
   - 若"稳态系统性超出 ~N 倍" → 值得做。

> **我（设计者）无 GPU，无法执行本步，需要你在真机采集。**

---

## 8. 分阶段实施计划

| 阶段 | 内容 | 前置 |
|---|---|---|
| **0** | §7 量化验证，确认多进程系统性超限 | —— |
| **1a** | **Go 侧（可独立先行、风险低）**：`SMNode`/`SMNodeFile` 常量；5 处挂载/建目录落点（§4.5.1）；2 处启动前清理落点（§4.5.2）。此时库还没用这个目录 → **纯增量、对现网无影响**，可先合并验证挂载与清理是否按预期发生 | 阶段 0 通过 |
| **1b** | **库侧**：共享区建立/初始化/布局守卫（§4.1/4.4/4.5.4）；消费端切共享桶（§4.2）；补充选举（§4.3）；bypass 累加改造（§4.7）；env 开关 + **attach 失败优雅降级回私有桶**（§5） | 1a 已上 |
| **2** | 压争用：进程/线程本地**批量取令牌**（一次 CAS 取一批、本地花完再取），把 CAS 频率降 ~N 倍。**仅当 profiling 证明跨进程 cacheline 弹跳是真开销时做** | 1b 稳定 |

> **1a / 1b 拆分的价值**：Go 侧改动（挂载 + 清理）不依赖库侧，且在库未使用该目录前**完全无副作用**。先上 1a 可以真机确认"目录挂进去了、每次容器重启前文件确实被删了"，把 §4.5 的控制面假设**验证成事实**，再让库侧依赖它。

> 阶段 2 的方向，代码注释早已点名——[dev_hot_t 上方注释](../library/src/cuda_hook.c#L86)："**Fixing that needs thread-local token batching, tracked separately.**" 本设计与之一致。

---

## 9. 风险与待办清单

- **[高] 并发正确性无法静态穷尽**：CAS 选举、bypass 累加、建区/重置竞态，必须**真机多进程压测**（并发扣减 + 补充不丢账、N 进程聚合不超限、补充者崩溃自愈）。
- **[高] bypass 语义改写**（§4.7）：SET→增量累加，是最易引入超发/欠发的一处，需专门单测。
- **[高，新增] 控制状态块整体入共享区**（§4.13）：改动面比前几稿承诺的大——不只是令牌桶，`md_cooldown`、排他 FSM ×3、`throttled_since_watch` 都要搬。这些字段的注释目前都写着"watcher-thread-only, no atomics needed"，**搬动时必须同步改注释**，否则后人会按旧注释假设做出错误优化。`aimd` 的 MD 雪崩（`md_divisor^N`）是不改就必现的回归，需专门用例覆盖。
- **[中] 库升级后的布局错位**（§4.5.4）：宿主目录按 `<pod-uid>_<cont-name>` 跨容器重启存活，而 `.so` 按版本挂载 → 新库可能映射到老结构体。控制面清理覆盖了 device plugin / DRA+NRI 两路；**DRA 非 NRI 路径无钩子**，由 `magic`/`layout_version`/`region_size` 守卫兜底。**改结构体必须 bump `SM_NODE_LAYOUT_VERSION`**，需在 review 中把关。
- **[中] 挂载点漏改**（§4.5.1）：新目录要在 **5 处**并列落点（device plugin Allocate/EnsureDir、DRA CDI、DRA NRI、`ensurePartitionDirectories`）。漏改任一处 → 该路径下 `open` 失败 → 由 §5 的降级兜住（退回私有桶），**绝不可 fatal**。需专门测每条路径。
- **[中] 热路径不得被守卫污染**（§4.12）：布局校验只能发生在初始化。**任何在 `rate_limiter` 里增加的校验/分支都是性能回归**，需在 review 中明确把关。
- **[低] 冻结区被误改**（§4.1）：`magic`/`layout_version`/`region_size`/`device_count` 这 16 字节是永久 ABI，改动 = 守卫失效。已用 `_Static_assert` 钉死偏移，但语义靠 review。
- **[关联，范围外] vmem_node 的同类缺陷**（§4.5.4 附）：[loader.c#L1399](../library/src/loader.c#L1399) 尺寸不符即报错退出，DRA 非 NRI 路径下库升级会让容器起不来。**但 vmem 是账本、重建会丢显存记账**，取舍与令牌桶不同，须单独评估，不捆绑本设计。
- **[中] 补充周期与多 watcher 采样错峰**：`REFILL_PERIOD_NS` 选取需真机调，避免"抢权成功但采样过旧"。
- **[低] DRA 非 NRI 路径无清理钩子**（§4.5.3）：残留是良性的（自校正），故不补钩子；仅依赖库内守卫。若将来该路径出现非自校正的共享字段，此结论需重审。
- **[前提] 收益依赖真多进程**：单进程容器零收益，阶段 0 未通过则不实施。

---

## 10. 决策摘要

- **手段**：容器内 `MAP_SHARED` 共享令牌桶 + 消费端原有 CAS（不改）+ 补充端每周期 CAS 抢权。
- **不做**：锁串行化（钝、贵、死锁）、`sleeping` 协调（每进程语义、共享桶已天然覆盖）、Event 占空比（伤流水线，正交路线）。
- **陈旧清理交给控制面**（§4.5）：新增 `/tmp/.sm_node` 专用挂载（与 `/tmp/.vgpu_lock`、`/tmp/.vmem_node` 同构，**不用容器自己的 `/tmp`**——可能被覆盖挂载或只读）；插件在**每次容器启动前删缓存文件**，复用已在为 `vmem_node` 跑的现成钩子（`PreStartContainer` + NRI `CreateContainer`）→ 库 attach 时必然是全新零字节区。**因此删掉了库内 generation 与 ns `st_ino` 探测**：那是在重造一个已存在且更可靠的轮子。库内只留 `magic`/`layout_version`/`region_size` **布局守卫**作第二道防线，防**库升级后新库读老结构体**（`DRA 非 NRI` 路径无启动钩子，且清理本身 best-effort）。
- **不用 `volatile`、不用 `_Atomic`**（§4.10）：`volatile` 不提供并发保护（现有安全性全来自 CAS）；`_Atomic` 在跨进程共享内存下有 lock-free 退化（libatomic 锁表**每进程一份** → 静默失效）与 size/align ABI 风险。用**普通定宽类型 + `__atomic_*` 内建 + 显式内存序**，合仓库既定习惯（[hook.h#L63](../library/include/hook.h#L63) 已硬性要求 GCC/Clang+glibc）。
- **布局不符 → 重建，不是报错退出**（§4.5.4）：现有 vmem 区在尺寸不符时 `return 1`（[loader.c#L1399](../library/src/loader.c#L1399)），等于库升级后容器不可用。本区一律**就地 memset 重建**；文件尺寸恒定（`SM_NODE_FILE_SIZE`）使版本升级永不需要 resize。就地重建之所以安全：**布局不符 ⟹ 文件来自上一世容器 ⟹ 无活着的旧版本读者**（容器一生只加载一个 `.so` 版本），故不需要 unlink/rename，也就避开了 rename 竞态下"共享桶静默退化成两个私有桶"的失效模式。
- **永不致命**（§2.1/§5）：`open`/`mmap` 失败、插件漏挂目录、目录只读 → 一律**降级回进程私有桶**，绝不 `exit`/让 CUDA 调用失败。多进程隔离是优化，不是正确性前提。
- **不抬高工具链门槛**（§4.11）：只用仓库已在用的设施；OFD 锁复用 [`lock.c#L64`](../library/src/lock.c#L64) 的**运行时回退**封装（低于 3.15 的内核照样跑）；明确禁用 `memfd_create`/`O_TMPFILE`/`renameat2`/`statx`。
- **热路径开销不变**（§4.12）：仍是一条 CAS。守卫/文件锁/建区**全部每进程一次**，`pthread_once` 之下；唯一真实新增开销是共享 cacheline 弹跳，交给阶段 2 且**仅在 profiling 证明后**才压。
- **三种控制器都兼容，但代价是整个控制状态块入共享区**（§4.13）：`delta` 是纯函数、天然兼容；`aimd` 的 `md_cooldown` 不共享会触发**它自己存在的意义所要防的 MD 雪崩**（`md_divisor^N`）；`auto` 还额外需要排他 FSM ×3。三者共用的 `throttled_since_watch` 也必须共享，否则 bypass 会误放行。**算法逻辑一行不改，改的只是状态的存储位置。**
- **同时满足**"严格"（物理共享桶）与"低开销"（一条 CAS、不串行化），且崩溃自愈、fork 安全。
- **先量化再实施**；单进程场景不做。

---

## 11. 待拍板清单

按"卡住实施"的程度排序。**前两项不定，1b 不能开工。**

| # | 待定项 | 选项 | 我的建议 | 影响面 |
|---|---|---|---|---|
| **1** | **阶段 0 数据**：多进程聚合超限是否系统性？（§7） | 做 / 不做本设计 | **必须先测**——我无 GPU。单进程容器（ollama/llama-server）**收益为零**，测不出系统性超限就应当直接放弃 | **全部**。不通过则本设计作废 |
| **2** | **共享模式覆盖哪些控制器？**（§4.13.5） | (a) 三种全支持 (b) **只支持 `delta`**，`aimd`/`auto` 自动降级回私有桶 | 看第 1 项数据。若你现网主用 `delta`，选 **(b)**：结构体退回只含 `share`/`up_limit`，**改动面砍掉大半**，`md_cooldown`/FSM×3 全不用碰，风险显著下降 | 结构体规模、1b 工作量与风险 |
| 3 | `vmem_node` 是否一并加冻结区头 + 重建（§4.5.4 附） | 纳入 / 独立评估 | **独立**：vmem 是**账本**，重建 = 丢显存记账，取舍与令牌桶根本不同 | 是否扩大本次范围 |
| 4 | DRA 非 NRI 路径要不要补清理钩子（§4.5.3） | 补 / 不补 | **不补**：残留是自校正的，布局守卫已兜住真危险 | 若否决，需设计新钩子 |
| 5 | 目录名 `sm_node` / 文件 `sm_node.config`（§4.5.1） | —— | 与 `vmem_node` 对称，已否决 `shm_node`/`vgpu_shm`（按机制命名与 vmem_node 歧义） | 常量落点，好改 |
| 6 | `SM_NODE_FILE_SIZE = 8192`（2 页，当前用量 2176B） | —— | 够用且留 3.7x 余量；`_Static_assert` 兜底 | 一旦发版即冻结，改则需 bump 版本 |
| 7 | env 名 `CUDA_SM_SHARED_BUCKET`，默认 `0` | —— | 与现有 `CUDA_SM_*` 命名一致；默认关 = 零风险 | 好改 |
| 8 | `REFILL_PERIOD_NS` 取值（§4.3） | —— | 先取 = 现 watcher 单设备周期（~80–100ms），**真机调** | 需第 1 项数据后定 |
| 9 | 宿主 Go 侧要不要读 `sm_node`（做 metrics）（§4.5.4） | 要 / 不要 | **暂不要**。若要，Go 侧必须同样校验 `magic`/`layout_version`，且容忍读到重建中的区（当作"本周期无数据"而非报错） | 会把结构体变成跨语言 ABI，冻结约束更硬 |

**我可以自行决定、不必占用你时间的**：内存序选择（§4.10）、`ofd_fcntl` 复用（§4.4.2）、就地重建 vs unlink（§4.5.4）、冻结区字段集（§4.1）、1a/1b 拆分（§8）。这些都已在文档中给出理由，**若你不同意任何一条，直接打**。
