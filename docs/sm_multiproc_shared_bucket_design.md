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
2. **低开销**：热路径（每次发射）维持"一条 CAS"的量级，**不引入锁、不串行化发射**。
3. **鲁棒**：任一进程崩溃不得卡住其它进程；无临界区可持有。
4. **fork 安全**：不新增会"父持锁 fork → 子死锁"的隐患。
5. **可灰度**：环境变量开关，默认关闭（保持现有进程私有桶行为）。

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

#define SM_NODE_MAGIC          0x534D4E44U      /* "SMND" */
#define SM_NODE_LAYOUT_VERSION 1U               /* 改结构体必须 +1，见 §4.5 */

/* 无 volatile、无 _Atomic：全部访问走 __atomic_* / CAS 内建，见 §4.10。 */
typedef struct {
  /* 每设备一格，缓存行对齐防伪共享（沿用 dev_hot_t 的 128B 对齐约定）。 */
  int64_t  cur_cuda_cores;    /* 令牌桶：消费者 CAS 扣，补充者累加        */
  int64_t  total_cuda_cores;  /* g_total（= thread*sm*FACTOR），首建者写   */
  int64_t  last_refill_ns;    /* 补充选举戳：CAS 抢每周期补充权            */
  /* 控制器积分态（只有当周期补充选举赢家读写 → 天然被选举串行化）：       */
  int64_t  share;             /* 对应现 shares[]                          */
  int32_t  up_limit;          /* 对应现 up_limits[]（soft 弹性）           */
  int32_t  is_cnt;            /* 对应现 is[]                              */
  int32_t  avg_sys_free;      /* 对应现 avg_sys_frees[]                   */
  int32_t  initialized;       /* 三态：0 未建 / 1 建设中 / 2 完成，见 §4.4 */
  uint8_t  _pad[CACHELINE_SIZE - 48];
} __attribute__((aligned(CACHELINE_SIZE))) sm_node_dev_t;

typedef struct {
  /* 区头：布局守卫，取代原设计的 generation，见 §4.5。 */
  uint32_t magic;             /* SM_NODE_MAGIC                          */
  uint32_t layout_version;    /* SM_NODE_LAYOUT_VERSION                 */
  uint32_t region_size;       /* sizeof(sm_node_region_t)               */
  uint32_t device_count;      /* MAX_DEVICE_COUNT                         */
  uint8_t  _pad[CACHELINE_SIZE - 16];
  sm_node_dev_t devices[MAX_DEVICE_COUNT];
} sm_node_region_t;

_Static_assert(sizeof(sm_node_dev_t) == CACHELINE_SIZE, "sm_node_dev_t must be one cacheline");
_Static_assert(offsetof(sm_node_region_t, devices) == CACHELINE_SIZE, "region header must be one cacheline");
```

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

### 4.4 建区竞争（谁建文件、谁初始化）

沿用 vmem 区的处理，但要防"文件已建、内容未初始化"的窗口：

1. `open(O_CREAT)`：多进程并发，内核保证至多一个真正创建。
2. 首建者 `ftruncate(sizeof(region))`（`ftruncate` 出的空洞读作 0）。
3. `mmap(MAP_SHARED)`。
4. **初始化用 CAS 选举 + `initialized` 标志**：
   ```c
   if (CAS(&region->devices[i].initialized, 0, 1) 抢到初始化权) {
       region->devices[i].total_cuda_cores = thread*sm*FACTOR;
       region->devices[i].share = 0;
       region->devices[i].up_limit = hard_core;
       ...
       __atomic_store_n(&region->devices[i].initialized, 2, __ATOMIC_RELEASE); // 2=完成
   } else {
       while (__atomic_load_n(&region->devices[i].initialized, __ATOMIC_ACQUIRE) != 2)
           sched_yield();   // 等首建者初始化完成
   }
   ```
   > 用三态 `initialized`（0 未建 / 1 建设中 / 2 完成）避免"抢到 CAS 但内容还没写完就被别人读"的窗口。

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

**唯一不自愈的残留是布局错位**：宿主目录跨容器重启存活，而 `.so` 按版本挂载（[vnum_plugin.go#L492](../pkg/deviceplugin/vgpu/vnum_plugin.go#L492)：`libvgpu-control.so.<version>`）→ 升级 library + DRA 非 NRI 路径容器重启 → **新库映射到老结构体的字节** → 字段错位 → 读出垃圾。守卫正是为它而留：

```c
/* attach 后，任何字段读取之前： */
if (region->magic          != SM_NODE_MAGIC           ||
    region->layout_version != SM_NODE_LAYOUT_VERSION  ||
    region->region_size    != sizeof(sm_node_region_t) ||
    region->device_count   != MAX_DEVICE_COUNT) {
    /* 老版本 / 异版本 / 未初始化(全 0) → 整区重整（CAS 抢重置权，同 §4.4 三态） */
    reinit_region(region);
}
/* 设备指纹兜底：total 与本进程新查的设备几何不符 → 该格重整 */
if (region->devices[i].total_cuda_cores != thread*sm*FACTOR)
    reinit_device_slot(region, i);
```

- 控制面刚删完文件 → 新建的区全 0 → 天然 `magic != MAGIC` → 走首建初始化。**"控制面已清理"与"库内兜底重整"是同一条代码路径**，不是两套逻辑，也无需额外分支。
- **改结构体必须 bump `SM_NODE_LAYOUT_VERSION`** —— 本设计对未来维护者的硬约束，应写进结构体上方注释。

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
| `initialized` 三态 | 同上 acq/rel（§4.4） | acq/rel 配对 |

> 若将来仍想改用 `_Atomic`，必须同时加 `_Static_assert(ATOMIC_LLONG_LOCK_FREE == 2)` 与 size/alignment 断言；在收益为零（`__atomic_*` 已够）的前提下不建议。

## 5. 开关与灰度

```c
CUDA_SM_SHARED_BUCKET = 0(默认，进程私有桶，现有行为) | 1(容器内共享桶)
```

- 默认 0：`g_dev_hot[]` 仍 static，行为与今日完全一致，风险为零。
- 开启 1：走共享区。集成进 `g_dynamic_config`（沿用 `CUDA_SM_DELTA_RAMP_FLOOR_DIVISOR` 等的 env→struct 加载模式，见现有 `sm_controller_init`），fork 边界自动继承（见 §6）。

---

## 6. 正确性与 fork 边界

### 6.1 fork 语义（白送的好处）

- `MAP_SHARED` 映射**跨 fork 保留** → 子进程自动 attach 同一个桶，无需额外处理，天然参与聚合限流。
- 现有 [`child_after_fork`](../library/src/cuda_hook.c#L238) 重置 `g_dev_hot[].last_launch_ns=0`：迁移后 `last_launch_ns` 仍在私有 `g_dev_hot[]`，此重置**保持不变**（正确：子进程的 gap 时序应重新计）。
- 共享区里的 `cur_cuda_cores`/`share` **不应**在 child_after_fork 重置（那是全容器共享状态，子进程只是新加入的消费者/候选补充者）。

### 6.2 不新增锁 → 不新增 fork 死锁面

本方案**刻意不引入任何 mutex**（全用 CAS + 选举），因此**无需**动 [`loader_child_after_fork`](../library/src/loader.c#L2264) 的 mutex 重init 列表，规避了"父持锁 fork → 子死锁"这一整类隐患。这是相对 HAMi 锁方案的结构性优势。

### 6.3 崩溃语义

- 消费者崩溃：无临界区、无持有物，桶计数器不受影响。
- 补充选举赢家崩溃：本周期没补上 → 下周期 `now-last` 超阈值 → 别人接手。最坏损失一个周期的补充（~80ms 少补一次），自愈。
- 无 robust-futex/一致性恢复负担（因为根本没有锁）。

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
- **[中] 库升级后的布局错位**（§4.5.4）：宿主目录按 `<pod-uid>_<cont-name>` 跨容器重启存活，而 `.so` 按版本挂载 → 新库可能映射到老结构体。控制面清理覆盖了 device plugin / DRA+NRI 两路；**DRA 非 NRI 路径无钩子**，由 `magic`/`layout_version`/`region_size` 守卫兜底。**改结构体必须 bump `SM_NODE_LAYOUT_VERSION`**，需在 review 中把关。
- **[中] 挂载点漏改**（§4.5.1）：新目录要在 **5 处**并列落点（device plugin Allocate/EnsureDir、DRA CDI、DRA NRI、`ensurePartitionDirectories`）。漏改任一处 → 该路径下 `open` 失败 → 必须**优雅降级回进程私有桶**（等价于关掉开关），绝不可 fatal。需专门测每条路径。
- **[中] 补充周期与多 watcher 采样错峰**：`REFILL_PERIOD_NS` 选取需真机调，避免"抢权成功但采样过旧"。
- **[低] DRA 非 NRI 路径无清理钩子**（§4.5.3）：残留是良性的（自校正），故不补钩子；仅依赖库内守卫。若将来该路径出现非自校正的共享字段，此结论需重审。
- **[前提] 收益依赖真多进程**：单进程容器零收益，阶段 0 未通过则不实施。

---

## 10. 决策摘要

- **手段**：容器内 `MAP_SHARED` 共享令牌桶 + 消费端原有 CAS（不改）+ 补充端每周期 CAS 抢权。
- **不做**：锁串行化（钝、贵、死锁）、`sleeping` 协调（每进程语义、共享桶已天然覆盖）、Event 占空比（伤流水线，正交路线）。
- **陈旧清理交给控制面**（§4.5）：新增 `/tmp/.sm_node` 专用挂载（与 `/tmp/.vgpu_lock`、`/tmp/.vmem_node` 同构，**不用容器自己的 `/tmp`**——可能被覆盖挂载或只读）；插件在**每次容器启动前删缓存文件**，复用已在为 `vmem_node` 跑的现成钩子（`PreStartContainer` + NRI `CreateContainer`）→ 库 attach 时必然是全新零字节区。**因此删掉了库内 generation 与 ns `st_ino` 探测**：那是在重造一个已存在且更可靠的轮子。库内只留 `magic`/`layout_version`/`region_size` **布局守卫**作第二道防线，防**库升级后新库读老结构体**（`DRA 非 NRI` 路径无启动钩子，且清理本身 best-effort）。
- **不用 `volatile`、不用 `_Atomic`**（§4.10）：`volatile` 不提供并发保护（现有安全性全来自 CAS）；`_Atomic` 在跨进程共享内存下有 lock-free 退化（libatomic 锁表**每进程一份** → 静默失效）与 size/align ABI 风险。用**普通定宽类型 + `__atomic_*` 内建 + 显式内存序**，合仓库既定习惯（[hook.h#L63](../library/include/hook.h#L63) 已硬性要求 GCC/Clang+glibc）。
- **同时满足**"严格"（物理共享桶）与"低开销"（一条 CAS、不串行化），且崩溃自愈、fork 安全。
- **先量化再实施**；单进程场景不做。
