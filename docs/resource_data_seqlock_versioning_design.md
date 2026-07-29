# resource_data_t 运行时可变改造：冻结区版本化 + 每设备 seqlock 快照

> 状态：设计草案（未实施）
> 目标读者：library（C）+ pkg/config/vgpu（Go）维护者
> 基线：`main` @ `a2cce3d`
> 关联：[sm_multiproc_shared_bucket_design.md](sm_multiproc_shared_bucket_design.md)（seq/CAS 与冻结头范式来源）、HAMi-core PR #238（原子读写共享限额字段的同类问题）

---

## 0. 背景与决策依据

### 0.1 现状

`resource_data_t`（[library/include/hook.h:224](../library/include/hook.h#L224)）是容器内**每进程**从 `/etc/vgpu-manager/config/vgpu.config` **只读** mmap 的配置：

```c
*data = mmap(NULL, sb.st_size, PROT_READ, MAP_PRIVATE, fd, 0);   // loader.c:1521
```

其中 `device_t devices[16]`（[hook.h:208](../library/include/hook.h#L208)）承载每卡的显存/算力上限。今天它是**写死一次、运行时不变**的常量（结构体上方就写着 `// TODO No modifications allowed during runtime.`），所以 C 端到处 `g_vgpu_config->devices[i].xxx` 的**裸读**是安全的。

### 0.2 目标变化

将来需要一个 **Go 侧进程在运行时原子地修改某卡的 `device_t`**（读多写少）。一旦出现并发写方，现有裸读会踩两类问题：

1. **可见性**：`MAP_PRIVATE + PROT_READ` 的 reader 从不写页 → 永不触发 COW → 每页始终指向 page cache 共享页 → 外部**就地写**可见（已由 `device_util_t` 实测验证，它也是 `MAP_PRIVATE` + OFD 锁）。**这条不用改**，但要求 Go 写方必须 `MAP_SHARED` 就地写，**绝不 `rename`/`O_TRUNC` 重写整文件**。
2. **一致性/不撕裂**：`MAP_PRIVATE` 对此**零保证**。`device_t` 含 `size_t` 与多个联动字段（`total_memory`/`real_memory`/`memory_oversold` 一组、`hard_core`/`soft_core`/`core_limit`/`hard_limit` 一组），裸读会读到"新 total + 旧 oversold"这类撕裂组合，是真实逻辑 bug。这是本次改造要解决的核心。

### 0.3 方案选型（线路 B：seqlock）

在"OFD 字节锁（抄 `device_util`，每读 2~4 syscall）"与"seqlock（每读 2 次原子读 + fence，无 syscall）"之间，鉴于 **读多写少 + 要整块快照 + 开销尽量小**，选 **seqlock**，并保留一条 **OFD `F_RDLCK` 慢速兜底**用于对付"写方崩溃/长时间被抢占导致 seq 卡在奇数"。fast path 无系统调用；仅当自旋超限才退到一次 `F_RDLCK`。

---

## 1. 总体设计

四件事一起做（都是同一个 ABI 破坏窗口，合并为一次 `layout_version` 提升）：

1. **冻结区版本化**：给 `resource_data_t` 加 `magic / layout_version / region_size / device_count` 冻结头（对齐 `vmem_node`/`sm_node` 的范式），把 loader 里"仅按 `st_size` 校验"升级成"按 magic+版本+尺寸校验"。
2. **每设备 seqlock**：`device_t` 首字段加 `seq`，并整体补到 **一个 cache line（128B）**，消除相邻设备 seq 的伪共享。
3. **保留冗余空间**：`device_t` 内 28B、头部 72B、pod/meta 块 ~84B、以及文件级 `CONFIG_FILE_SIZE=8192` 的富余，未来加字段无需再破 ABI。
4. **快照读**：抽象 `device_t get_device_snapshot(int host_index)`，C 端所有读设备配置处替换为读快照（保证不撕裂）。

Go 侧同步：镜像新布局、加 magic/版本校验、写方写冻结头、并抽象 `func (r *ResourceDataT) ModifyDevice(deviceIndex int, mutation func(*DeviceT)) error`（seqlock 写序，当前先落地不启用）。

---

## 2. 详细内存布局

> 约束：`CACHELINE_SIZE = 128`（[hook.h:163](../library/include/hook.h#L163)）。跨语言/跨进程 ABI **必须用定宽类型**（`size_t → uint64_t`、`int → int32_t`），理由见 [hook.h:472](../library/include/hook.h#L472) 关于"禁用 `_Atomic` 以免 libatomic 每进程锁表降级"的既有注记——本设计沿用"普通定宽类型 + `__atomic_*` 显式内存序"。

### 2.1 新 `device_t`（128B，一个 cache line）

```c
#define DEVICE_T_RESERVED_I32 7

typedef struct {
  /* ---- seqlock 版本号：偶=稳定，奇=写入中。offset 0，供 C/Go 原子访问 ---- */
  uint32_t seq;                 /* @0  */
  uint32_t _seq_pad;            /* @4  保持 total_memory 8 字节对齐 */
  char     uuid[UUID_BUFFER_SIZE];  /* @8  (48) */
  uint64_t total_memory;        /* @56 (was size_t) */
  uint64_t real_memory;         /* @64 */
  int32_t  hard_core;           /* @72 */
  int32_t  soft_core;           /* @76 */
  int32_t  core_limit;          /* @80 */
  int32_t  hard_limit;          /* @84 */
  int32_t  memory_limit;        /* @88 */
  int32_t  memory_oversold;     /* @92 */
  int32_t  activate;            /* @96 */
  int32_t  reserved[DEVICE_T_RESERVED_I32]; /* @100..127 (28B 冗余) */
} __attribute__((aligned(CACHELINE_SIZE))) device_t;

_Static_assert(sizeof(device_t) == CACHELINE_SIZE, "device_t must be one cache line");
_Static_assert(_Alignof(device_t) == CACHELINE_SIZE, "device_t must be cache-line aligned");
_Static_assert(offsetof(device_t, seq) == 0, "seqlock word must stay at offset 0");
```

**为什么 seq 放 offset 0 且每设备独占一条 cache line**：写方 bump `devices[i].seq` 只会让本设备那条线失效，不牵连相邻设备的 reader（无伪共享）。写虽稀有，但这是零成本的干净布局，且冗余空间需求正好把 96B 撑到 128B。

### 2.2 新 `resource_data_t`

```c
#define CONFIG_MAGIC               0x56474346U   /* "VGCF" */
#define CONFIG_LAYOUT_VERSION      1U
#define CONFIG_FILE_SIZE           8192          /* 固定，与 sizeof 解耦，见 §4.3 */
#define DRIVER_VERSION_BUFFER_SIZE 32            /* NVIDIA 驱动串 "550.90.07" 等 */

typedef struct {
  /* ==== 冻结头：128B（一条 cache line），永久 ABI，magic@0 / layout_version@4 ==== */
  uint32_t  magic;                              /* @0  CONFIG_MAGIC */
  uint32_t  layout_version;                     /* @4  CONFIG_LAYOUT_VERSION */
  uint32_t  region_size;                        /* @8  = sizeof(resource_data_t) */
  uint32_t  device_count;                       /* @12 = MAX_DEVICE_COUNT */
  version_t cuda_version;                        /* @16 (8) 原 driver_version，见 §3 */
  char      driver_version[DRIVER_VERSION_BUFFER_SIZE]; /* @24 (32) NVIDIA 驱动串 */
  uint8_t   _hdr_reserved[CACHELINE_SIZE - 56]; /* @56..127 (72B 冗余) */

  /* ==== pod 身份 + 全局标志块：写一次、运行时不变，无需 seqlock ==== */
  char      pod_uid[UUID_BUFFER_SIZE];          /* @128 */
  char      pod_name[NAME_BUFFER_SIZE];
  char      pod_namespace[NAME_BUFFER_SIZE];
  char      container_name[NAME_BUFFER_SIZE];
  char      reg_uuid[UUID_BUFFER_SIZE];
  int32_t   compatibility_mode;
  int32_t   sm_watcher;
  int32_t   vmem_node;
  uint8_t   _meta_reserved[/* 补齐到 512 */];   /* ~84B 冗余，令 devices[] 落在 cache line */

  /* ==== 每设备配置，各 128B，seqlock 保护（seq 即 device_t[i].seq）==== */
  device_t  devices[MAX_DEVICE_COUNT];          /* @512 起，16*128=2048 */
} resource_data_t;

_Static_assert(offsetof(resource_data_t, magic) == 0, "magic@0");
_Static_assert(offsetof(resource_data_t, layout_version) == 4, "layout_version@4");
_Static_assert(offsetof(resource_data_t, devices) % CACHELINE_SIZE == 0,
               "devices[] must start on a cache line");
_Static_assert(sizeof(resource_data_t) <= CONFIG_FILE_SIZE,
               "config region must fit the permanently reserved file size");

/* 每设备 seq 字节锁偏移（供 F_RDLCK 慢速兜底 / 写方 F_WRLCK）。Go 侧 getConfigLockOffset 必须一致。 */
#define GET_CONFIG_LOCK_OFFSET(i) \
  (offsetof(resource_data_t, devices) + (size_t)(i) * sizeof(device_t) + offsetof(device_t, seq))
```

预计 `sizeof(resource_data_t) ≈ 2560`，`CONFIG_FILE_SIZE=8192` 留 ~5.6KB 文件级富余。**开销核算**：相对旧 1848B，磁盘/映射多几 KB，可忽略；读路径每次多一次 128B 结构拷贝 + 2 次 acquire 原子读，是"整块不撕裂"的最小代价。

### 2.3 对齐推荐小结（回答"具体大小怎么对齐"）

| 目标 | 推荐 | 理由 |
|---|---|---|
| 冻结头 | **128B（1 cache line）** | 与 vmem/sm 一致；magic@0/version@4 定死 |
| `device_t` | **128B（1 cache line）**，`aligned(128)` | 消除相邻设备 seq 伪共享；96→128 仅多 512B/16 卡 |
| `device_t` 冗余 | **28B（7×int32）** | 填满 cache line，够加未来 per-device 标志 |
| 头部冗余 | **72B** | 填满头 cache line |
| pod/meta 冗余 | **~84B** | 令 `devices[]` 对齐到 cache line |
| 文件尺寸 | **`CONFIG_FILE_SIZE=8192`（固定，与 sizeof 解耦）** | 允许未来长 struct 而不 `ftruncate` 旧映射（避免 SIGBUS） |

---

## 3. `driver_version` 字段：类型决策（回答第 7 点）

**事实**：现有 `resource_data_t.driver_version`（`version_t`）在 Go 侧是用 `CudaDriverVersion.MajorAndMinor()` 填的（[pkg/config/vgpu/vgpu_config.go](../pkg/config/vgpu/vgpu_config.go) `NewResourceDataT`，如 CUDA 12020 → 12.2）——**它承载的其实是 CUDA 版本，不是 NVIDIA 驱动串**。而 library 真正的 NVIDIA 驱动串（如 `550.90.07`）是另从 `/proc/driver/nvidia/version` 解析进全局 `char driver_version[FILENAME_MAX]`（[loader.c:1067](../library/src/loader.c#L1067)），用于拼 `libcuda.so.<ver>`，**并不来自本配置结构**。

**决策**：
- **新增** `char driver_version[DRIVER_VERSION_BUFFER_SIZE]` 到冻结头，语义 = NVIDIA 驱动串（与 `/proc` 一致；未来可让 library 直接取用、省去解析）。32B 对 `"535.129.03"` 这类 9~10 字符绰绰有余。
- **把现有 `version_t driver_version` 改名为 `cuda_version`（Go：`CudaVersion VersionT`）**，因为它就是 CUDA major.minor。
- **类型保持 `version_t`（`VersionT`）**，**不新造 `CudaVersion` 独立类型**：`version_t` 本就是通用 `{major,minor}`，另造类型只增churn 无收益。若想要语义标注，可加 `typedef version_t cuda_version_t;` 纯别名，但非必需。

> 回答"改为 CudaVersion 类型还是 VersionT"：**字段改名 `cuda_version`，类型仍用 `version_t`/`VersionT`**。

---

## 4. 冻结区版本化与兼容性

### 4.1 校验（替换现有仅比 `st_size`）

`mmap_file_to_config_path`（[loader.c:1501](../library/src/loader.c#L1501)）改造：映射仍 `PROT_READ, MAP_PRIVATE`（可见性 OK），但把 `sb.st_size != sizeof(resource_data_t)` 的单一校验换成：

1. `sb.st_size >= sizeof(resource_data_t)`（或固定 `== CONFIG_FILE_SIZE`，见 §4.3）；
2. `magic == CONFIG_MAGIC`；
3. `layout_version == CONFIG_LAYOUT_VERSION`；
4. `region_size == sizeof(resource_data_t)`；
5. `device_count == MAX_DEVICE_COUNT`。

任一不符 → **拒绝并明确报错**（对齐 vmem 的 `ErrUnknownLayout` "跳过、绝不重建"策略），而不是误读旧布局字节。

### 4.2 灰度/版本偏斜

配置文件是 **manager 在 Allocate 时按容器写一次**（`WriteVGPUConfigFile`，仅当不存在才写），随后注入容器供 library 读。滚动升级期 **写方(manager) 与 读方(注入的 library) 版本可能不一致** → 冻结头的 magic+version 正是为此：老库读到新配置、或新库读到老配置，都会因 `layout_version` 不符而**干净拒绝**（而非静默错读）。发布顺序建议：**先升 manager（写方），后升 library（读方）**，且同一 `layout_version` 内只允许"往 reserved 里加字段"的纯增量变更；任何改动既有字段类型/顺序/偏移 **必须 +1 `layout_version`** 并同步 Go 侧断言。

### 4.3 文件尺寸与 sizeof 解耦

固定 `CONFIG_FILE_SIZE=8192`，`ftruncate` 到该尺寸，`_Static_assert(sizeof <= CONFIG_FILE_SIZE)`。好处同 vmem：以后 `sizeof` 因加字段增大也不改文件尺寸，**老映射的尾部永不越过 EOF（不会 SIGBUS）**。校验用 `st_size == CONFIG_FILE_SIZE`。

### 4.4 需要同步改的"写方"清单

- **Go**：`writeResourceDataToDisk` / `NewResourceDataT` / `WriteVGPUConfigFile`（pkg/config/vgpu）——写冻结头 + `cuda_version` + `driver_version` 串 + 每设备 `seq=0`。
- **C**：library 内若仍有写回配置的路径（疑似 `setting_to_disk`，[loader.c:2006](../library/src/loader.c#L2006) `write(fd, data, sizeof(resource_data_t))`）——需确认是否仍在用；若在用，同样要写冻结头。**这是一个必须在实施期确认的点。**
- **Go 读方**：metrics lister（`pkg/metrics/lister/container_lister.go`）走 `MmapResourceData`（`MAP_SHARED` RW）读 → 加同样的 magic/版本校验；若它也读 `device_t` 字段，建议同样走"快照"读法（见 §6.2）。

---

## 5. `get_device_snapshot`：C 端读快照抽象

### 5.1 签名与语义

```c
/* 返回 device[host_index] 配置的不撕裂快照。绝不返回半更新组合：seqlock 重试到读到稳定副本。
 * 越界 / 配置未加载 → 返回全零（activate=0、memory_limit=0），调用方按"无限制"降级，与今日行为一致。 */
device_t get_device_snapshot(int host_index);
```

### 5.2 实现（fast path 无 syscall + 兜底）

```c
#define CONFIG_SEQ_SPIN_LIMIT 1024

static inline void cpu_relax(void) {
#if defined(__x86_64__)
  __builtin_ia32_pause();
#else
  __asm__ __volatile__("" ::: "memory");
#endif
}

device_t get_device_snapshot(int host_index) {
  device_t snap;
  if (unlikely(host_index < 0 || host_index >= MAX_DEVICE_COUNT || g_vgpu_config == NULL)) {
    memset(&snap, 0, sizeof(snap));
    return snap;
  }
  const device_t *d = &g_vgpu_config->devices[host_index];
  uint32_t s1, s2;
  unsigned spins = 0;
  for (;;) {
    s1 = __atomic_load_n(&d->seq, __ATOMIC_ACQUIRE);
    if (likely(!(s1 & 1u))) {
      snap = *d;                                   /* 整块普通拷贝 */
      __atomic_thread_fence(__ATOMIC_ACQUIRE);
      s2 = __atomic_load_n(&d->seq, __ATOMIC_ACQUIRE);
      if (likely(s1 == s2)) return snap;           /* 稳定副本 */
    }
    cpu_relax();
    if (unlikely(++spins >= CONFIG_SEQ_SPIN_LIMIT)) {
      /* 慢速兜底：写方崩溃(奇数残留)或被长时间抢占。取一次 F_RDLCK——写方持 F_WRLCK
       * 故这会阻塞到写方结束；写方进程若已死，其 OFD 锁随 fd 关闭自动释放，锁立即可得。
       * 拿到后再读一次即为一致快照。极其罕见，只在此付一次 syscall。 */
      int fd = config_device_read_lock(host_index);   /* 见 §5.3 */
      snap = *d;
      if (fd >= 0) config_device_unlock(fd);
      LOGGER(WARNING, "get_device_snapshot(%d): seqlock spin cap hit, took RDLCK fallback", host_index);
      return snap;
    }
  }
}
```

**内存序**：写方 release 发布、读方两次 acquire 读 seq + 中间 acquire fence，构成 happens-before；`s1==s2` 且偶数 ⇒ 拷贝期间无写入。字段用普通类型读（内核 seqlock 同款），在 amd64+gcc 上实践安全；严格消除 C11 数据竞争 UB 可把 `snap = *d` 换成逐字段 `__ATOMIC_RELAXED` 读或对整块 `memcpy`——不影响正确性，仅洁癖。

### 5.3 慢速兜底锁（复用现有 OFD 惯用法）

照 [lock.c:231](../library/src/lock.c#L231) `device_util_read_lock` 的写法,新增 `config_device_read_lock(i)` / `config_device_unlock(fd)`，锁 `GET_CONFIG_LOCK_OFFSET(i)` 一个字节（`F_RDLCK`）。写方在 seqlock 写序外层取同偏移 `F_WRLCK`。

---

## 6. C 端读点替换清单

`grep 'g_vgpu_config->' library/src` 共 **84 处**；其中 **头部字段(pod/flags) ~34 处不动**（不可变，且成员访问偏移自动重算，只需重编译），**需替换的是 `devices[...]` 字段读**，按文件：

### 6.1 需改为快照读的位置（约 50 处，按函数聚合，每函数取一次快照）

- **[cuda_hook.c:414-443](../library/src/cuda_hook.c#L414)** `prepare_memory_allocation`：一条表达式里 4 次读同卡 `total_memory/real_memory/memory_oversold/memory_limit` —— **最典型的撕裂风险点**，函数入口取一次 `device_t s = get_device_snapshot(*host_index);` 全程用 `s.*`。
- **[cuda_hook.c:719](../library/src/cuda_hook.c#L719)、4531** `core_limit` 判定。
- **[cuda_hook.c:1656-1880](../library/src/cuda_hook.c#L1656)** watcher 主循环：每卡每周期多次读 `hard_core/soft_core/core_limit/hard_limit`——**每卡每周期取一次快照**，替换循环体内所有 `g_vgpu_config->devices[host_index].*`。
- **[cuda_hook.c:1953](../library/src/cuda_hook.c#L1953)** `device_t *d = &g_vgpu_config->devices[host_index];` → 改为 `device_t d = get_device_snapshot(host_index);`（值拷贝，后续 `d.` 不变）。
- **[cuda_hook.c:3327/3398/3464/3538](../library/src/cuda_hook.c#L3327)** `memory_oversold` 判定（async free/capture 路径）。
- **[cuda_hook.c:3845-3899](../library/src/cuda_hook.c#L3845)** `nvmlDeviceGetMemoryInfo` 类：`memory_limit/total_memory/memory_oversold`。
- **[nvml_hook.c:66-122](../library/src/nvml_hook.c#L66)** 三处 `memory_limit/total_memory/core_limit`。
- **[loader.c:1812](../library/src/loader.c#L1812)** `hard_core`（初始化 up_limit）、**[loader.c:1976-1986](../library/src/loader.c#L1976)** 启动日志逐字段（低频，可取一次快照打印）、**[loader.c:2318](../library/src/loader.c#L2318)** `activate/uuid` 匹配。

### 6.2 替换范式

```c
/* 旧 */
if (g_vgpu_config->devices[i].memory_limit && ...) { size_t t = g_vgpu_config->devices[i].total_memory; ... }
/* 新 */
device_t s = get_device_snapshot(i);
if (s.memory_limit && ...) { uint64_t t = s.total_memory; ... }
```

要点：**同一逻辑单元只取一次快照**；循环内按迭代取；不要把 `get_device_snapshot` 塞进条件表达式里多次调用。

### 6.3 不动的位置

pod 身份/flags（`pod_uid/pod_name/.../compatibility_mode/sm_watcher/vmem_node/reg_uuid`，~34 处）不可变，保留裸读；`cuda_version/driver_version` 亦然。

---

## 7. Go 端改造

### 7.1 镜像新布局（`pkg/config/vgpu/vgpu_config.go`）

```go
const (
    ConfigMagic              uint32 = 0x56474346 // "VGCF"
    ConfigLayoutVersion      uint32 = 1
    ConfigFileSize           int64  = 8192
    DriverVersionBufferSize         = 32
    DeviceReservedI32               = 7
)

type DeviceT struct {           // 128B，Seq@0
    Seq            uint32
    _              uint32
    UUID           [UuidBufferSize]byte
    TotalMemory    uint64
    RealMemory     uint64
    HardCore       int32
    SoftCore       int32
    CoreLimit      int32
    HardLimit      int32
    MemoryLimit    int32
    MemoryOversold int32
    Activate       int32
    Reserved       [DeviceReservedI32]int32
}

type ResourceDataT struct {     // 冻结头 128B + pod 块 + devices[]
    Magic         uint32
    LayoutVersion uint32
    RegionSize    uint32
    DeviceCount   uint32
    CudaVersion   VersionT       // 原 DriverVersion
    DriverVersion [DriverVersionBufferSize]byte
    _             [CachelineSize - 56]byte
    PodUID        [UuidBufferSize]byte
    // pod_name / pod_namespace / container_name / reg_uuid / compat / sm_watcher / vmem_node
    // + _meta_reserved 补齐到 512
    Devices       [MaxDeviceCount]DeviceT
}
```

更新 `vgpu_config_test.go` 的 `sizeof` 断言（`DeviceT==128`、`ResourceDataT==计算值`、`offsetof(Devices)%128==0`、`offsetof(Magic)==0`、`offsetof(LayoutVersion)==4`）——与 C 的 `_Static_assert` **一一对应，锁死跨语言布局**。

### 7.2 写方与校验

- `NewResourceDataT`：填 `Magic/LayoutVersion/RegionSize/DeviceCount`、`CudaVersion`、`DriverVersion` 串、每设备 `Seq=0`。
- `writeResourceDataToDisk`：**首次创建**仍可 `os.WriteFile`（一次性、无并发读）；但需写满 `ConfigFileSize`（`Truncate`）。
- `MmapResourceData` / `CheckResourceDataSize`：加 magic/版本/region_size/device_count 校验（照 `pkg/config/vmem` 的 `ErrUnknownLayout` 范式）。

### 7.3 `ModifyDevice`（seqlock 写序抽象，先落地不启用）

```go
// ModifyDevice 在每设备 seqlock 下就地修改 devices[deviceIndex]，令并发的 C reader
// 要么看到整块新值、要么整块旧值。要求 r 由 MAP_SHARED 可写映射支撑（MmapResourceData），
// 绝不可走 os.WriteFile 整文件重写。写序外层建议持 GET_CONFIG_LOCK_OFFSET 的 F_WRLCK，
// 以便 reader 的 F_RDLCK 慢速兜底有效、并串行化多写方。
func (r *ResourceDataT) ModifyDevice(deviceIndex int, mutation func(*DeviceT)) error {
    if deviceIndex < 0 || deviceIndex >= MaxDeviceCount {
        return fmt.Errorf("device index %d out of range", deviceIndex)
    }
    d := &r.Devices[deviceIndex]
    seq := &d.Seq // uint32, 128 对齐 → atomic 安全
    atomic.AddUint32(seq, 1) // 偶→奇：开始写
    mutation(d)              // 就地改字段
    atomic.AddUint32(seq, 1) // 奇→偶：发布
    return nil
}
```

内存序：Go `sync/atomic` 为顺序一致（amd64 上 LOCK 前缀=全屏障），保证 `mutation` 的字段写不越过任一 seq 自增；与 C 侧 acquire 配对。**依赖 amd64**（本项目工具链即 amd64）。

---

## 8. 为什么这样对（内存序与跨语言正确性）

- **禁用 `_Atomic`**：沿用 [hook.h:472](../library/include/hook.h#L472) 既有决策——非 lock-free 的 `_Atomic` 会走 libatomic 每进程锁表，跨进程/跨语言直接失效。C 用 `__atomic_*(ACQUIRE/RELEASE)` + fence，Go 用 `sync/atomic`（seq-cst，更强，兼容）。
- **seq 宽度**：`uint32` 足够（回绕需 2^32 次写，写稀有，不可能在一次读窗口内回绕到同值）。4 字节对齐即可，`device_t` 128 对齐已满足。
- **可见性前提**：C reader 保持 `MAP_PRIVATE+PROT_READ`（`device_util` 已证），Go 写方**必须 `MAP_SHARED` 就地写**。二者缺一不可。

---

## 9. 工作量分析

| 模块 | 工作项 | 规模 | 风险 |
|---|---|---|---|
| C `hook.h` | 新 `device_t`/`resource_data_t` 布局 + magic/版本/尺寸常量 + `_Static_assert` + `GET_CONFIG_LOCK_OFFSET` | S | 中（偏移必须与 Go 精确对齐） |
| C `loader.c` | `mmap_file_to_config_path` 换版本校验；确认/改造 `setting_to_disk` 写方；`cuda_version` 改名波及初始化 | S | 中 |
| C 新增 | `get_device_snapshot` + `cpu_relax` + `config_device_read_lock/unlock` | S | 低 |
| C 读点替换 | ~50 处 `devices[...]` 裸读 → 快照读，约 20~30 个函数 | **L** | 中（机械但量大，watcher 循环与 4×表达式需仔细） |
| C 测试 | 并发写方压测证明不撕裂（见 §10） | M | 中 |
| Go `vgpu_config.go` | 镜像新布局 + magic/版本校验 + 写冻结头 + `ModifyDevice` | M | 中（sizeof 断言联动） |
| Go `vgpu_config_test.go` | 更新 sizeof/offset 断言 | S | 低 |
| Go 读方 | metrics lister 加校验/快照读 | S | 低 |
| 联调/灰度 | `layout_version` 提升、发布顺序、文档 | S | 中 |

**总体**：C 端读点替换是主体工作量（L）；其余多为 S/M。关键风险集中在 **C↔Go 布局精确对齐**（靠双侧断言锁死）与 **写方路径确认**（`setting_to_disk` 是否仍用）。建议实施顺序：① 布局+断言双侧落地并编译通过 → ② `get_device_snapshot`+`ModifyDevice` → ③ 批量替换读点 → ④ 并发压测 → ⑤ 灰度（先 manager 后 library）。

---

## 10. 测试计划

1. **布局断言**：C `_Static_assert` + Go `TestResourceDataStructLayout`，`sizeof`/`offsetof` 双侧一致（CI 必过）。
2. **版本守卫**：喂 magic 错/版本错/尺寸错的文件，`mmap_file_to_config_path` 与 Go `MmapResourceData` 均须干净拒绝。
3. **不撕裂并发压测**（核心）：一线程/进程 `ModifyDevice` 循环把某卡在两组**自洽但差异大**的配置间来回切（如 `{total=A, oversold=1}` ↔ `{total=B, oversold=0}`）；多线程狂调 `get_device_snapshot`，断言**永远只读到 A 组或 B 组、绝无交叉组合**。这是对 seqlock 正确性的直接证明（复用本仓 fork/seqlock 独立压测的做法）。
4. **兜底路径**：人为令写方在奇数 seq 处停顿超过自旋上限，验证 reader 走 `F_RDLCK` 兜底且最终一致。
5. **回归**：现有 library 测试（RC_SKIP 套件）全绿。

---

## 11. 非目标 / 未决

- **Go 侧尚不真正调用 `ModifyDevice`**：本次仅落地抽象与 seqlock 写序，具体"谁在什么时机改哪张卡"的上层逻辑不在范围内。
- **pod 身份/flags 运行时可变**：非目标，仍视为写一次不变；若将来也要可变，需各自的 seqlock 或整体 generation。
- **`setting_to_disk` 是否保留**：待实施期确认；若废弃则更省事，若保留必须同步写冻结头。
- **非 amd64**：本设计的 seqlock 依赖 amd64 内存模型与 `sync/atomic` seq-cst；跨架构需另评估 fence 强度。
