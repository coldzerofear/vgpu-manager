# 容器 PID 归属判定：正确性审计与缓存方案

本文回答三件事：

1. `starttime` 能不能当 PID 复用的代标记（用户质疑，需量化而非断言）；
2. `check_device_pid_in_local_container_pid`（openKernelMode 兼容路径）是否合理、有没有逻辑 bug；
3. 在上述结论之上，缓存方案 A / B 的最终形态。

所有数字都是在本仓库 devcontainer（`3.10.0-1160.el7.x86_64`，`pid_max=32768`，`CLK_TCK=100`）上实测的，
复现程序见 §7。

---

## 1. 结论先行

| # | 发现 | 级别 | 位置 |
|---|------|------|------|
| F1 | `device_pid_in_same_container` 依赖 `/proc/<pid>/ns/cgroup` 存在。cgroup namespace 是 **Linux 4.6** 引入的，在更早的内核上该文件不存在，函数**对任何 PID（包括调用者自己）无条件返回 -1** → openKernelMode 识别恒失败 → 显存统计恒为 0 → 限额检查变成 no-op | **P0 静默失效** | `util.c:846` |
| F2 | 同一函数对**不同 uid** 的进程 `stat(/proc/<pid>/ns/mnt)` 得到 `EPERM` → 返回 -1。同容器内跨 uid 的 GPU 进程被漏算 | P1 | `util.c:846` |
| F3 | `library_exists_in_process_maps` 单次耗时随目标进程 VMA 数线性增长，实测 **20000 VMA → 43.8 ms**，60000 VMA → 134 ms。它运行在**持有设备锁的分配热路径**上，而锁的退避参数是按"临界区 1-3ms"设计的 | **P0 性能** | `util.c:817` |
| F4 | `strstr(libPath, "nvidia")` 对整条路径做子串匹配，`/tmp/nvidia-notes/data.bin` 这类无关映射会被判成"在用 GPU"（已实测命中） | P2 假阳性 | `util.c:834` |
| F5 | `char line[1024]` + `fgets`，maps 的 pathname 可达 PATH_MAX。超长行被截断后残段按新行解析 | P3 | `util.c:828` |
| F6 | 自身 `/proc/self/ns/*` 的 inode 在进程生命周期内不变，却对每个 PID 重复 `stat` 两次 | P3 浪费 | `util.c:846` |
| F7 | `starttime` 作为代标记是**充分的**，但理由不是"时间戳够精确"，而是"PID 回绕的下界远大于一个 tick"。见 §2 | — | — |

F1 和 F3 都在 `openKernelModules` **默认为 true**（`docs/how_to_use_deviceplugin_nodeconfig.md:23`）的路径上。

---

## 2. `starttime` 能否作为代标记

### 2.1 用户的质疑

> starttime 单位是 clock ticks, 他不是具体的时间戳, 所以复用pid有可能重复相同

质疑成立且重要：`starttime`（`/proc/<pid>/stat` 字段 22）的分辨率确实只有 `1/USER_HZ`，本机 `CLK_TCK=100` → **10 ms**。
同一个 10ms 跳内启动的两个进程，`starttime` 完全相同。所以"`starttime` 是时间戳、时间戳不会重复"这个论证是错的，我之前的表述站不住。

### 2.2 正确的论证：约束不在分辨率，在回绕下界

要让"PID n 被复用 **且** 新旧实例 `starttime` 相同"成立，必须同时满足：

- **(a)** PID 分配器从 n 走完一圈回到 n。Linux 的 `alloc_pid` 用 `idr_alloc_cyclic` 从 `last_pid+1` 递增、到 `pid_max` 回绕，所以这需要 **≈ pid_max 次进程创建**；
- **(b)** 这 ≈pid_max 次创建全部发生在**同一个 10 ms 跳内**。

(b) 要求的速率是 `pid_max / 10ms`。本机 `pid_max=32768` → **3,276,800 fork/s**。

实测本机纯 `fork()+_exit()` 上限：**2,856 fork/s**。差 **1,147 倍**。

直接测"同一 PID 相邻两次分配之间的真实时间间隔"更直观：30 万轮 fork、26.8 万次 PID 复用，
**最小间隔 7,896.7 ms**，而判定窗口是 10 ms —— 差 790 倍。间隔 <10ms 的复用次数：**0**。

另一个 20 万轮的实验直接记录 `(pid, starttime)`：**16.8 万次 PID 复用，starttime 相同 0 次**。

所以 `starttime` 够用，但要点必须写清楚：**它不是靠时间精度，而是靠 PID 回绕需要 ≈pid_max 次分配这个结构性下界。**

### 2.3 它真正会失效的边界（必须记进代码注释）

1. **`pid_max` 被调得很小**。若 `pid_max=512`，(b) 只需 51,200 fork/s —— 现代机器裸 fork 可达这个量级。
   k8s 节点不会这么配（kubelet/containerd 自身就要几千 PID），但这是该论证的真实前提，应当断言而非假设。
2. **TOCTOU**：读 `starttime` 与做判定不是原子的。进程在两步之间退出并被复用，会把新实例的属性配上旧实例的代标记。
   这与 tick 分辨率无关，是顺序问题 → 方案必须 **read-verify-read**（§5.3）。
3. **读取失败 / starttime 为 0** 必须视作"不可缓存"，不能当成匹配。

### 2.4 为什么不用 pidfd

`pidfd_open(2)` 拿到的 fd 绑定**进程实例**而非 PID 号，是内核保证的身份，严格强于 `starttime`。但：

- 需要 **Linux ≥ 5.3**。本仓库 devcontainer 内核 3.10，实测 `pidfd_open` 返回 `ENOSYS`。
  考虑到 F1 暴露出生产里存在 <4.6 的内核，pidfd 不能作为唯一机制；
- 每个缓存项占一个 fd，需要配 fd 上限 + 淘汰策略。

鉴于 §2.2 的量级（1147 倍余量），`starttime` + read-verify-read 已经够用，**不引入 pidfd**。
若将来要加强，它是正交的增量，不影响本方案结构。

---

## 3. `check_device_pid_in_local_container_pid` 正确性审计

```c
int check_device_pid_in_local_container_pid(unsigned int device_pid) {   // cuda_hook.c:2339
  int ret = -1;
  if (device_pid == 0) return ret;
  if (pid_exist(device_pid) != 0) return ret;                  // ① kill(pid,0)
  if (device_pid_in_same_container(device_pid) != 0) return ret; // ② mnt+cgroup ns inode 比对
  if (library_exists_in_process_maps("nvidia", device_pid) == 0) ret = 0;  // ③ 扫 maps
  return ret;
}
```

**三步的分工是合理的**，这点先确认：

- NVML 在某些驱动版本上返回的是**容器命名空间的 PID**，所以不能直接累加；
- 而 `hostPID=true` 或 `shareProcessNamespace=true` 时，我们自己的 PID 命名空间里**能看到**别的容器/宿主机的进程，
  `kill(pid,0)` 对它们也会成功 —— 所以 ① 单独不足，必须有 ②；
- ② 用 mnt ns 作主判据是对的：`hostPID=true` 下宿主进程 rootfs 不同、`shareProcessNamespace=true` 下
  同 Pod 兄弟容器各有自己的 rootfs，两者都会被 mnt inode 不等正确拒绝。**这是整条路径的承重墙**；
- ③ 防的是"NVML 给的 PID 其实是另一个 ns 的号，恰好撞上本容器内一个非 GPU 进程"。②无法区分这种串号，所以③不冗余。

问题出在实现，不在设计意图。

### 3.1 F1（P0）：cgroup ns 不存在 → 整条路径恒失效

```c
const char *ns_types[] = {"mnt", "cgroup", NULL};
...
if (stat(device_path, &device_st) != 0) return -1;   // ← cgroup ns 不存在时走这里
```

实测本机 `/proc/self/ns/` 内容：

```
ipc  mnt  net  pid  user  uts        ← 没有 cgroup
```

把 `device_pid_in_same_container` 原样移植出来，**用调用者自己的 PID** 测：

```
用自己的 pid (23018) 测 -- 自己当然属于自己的容器, 期望返回 0:
    mnt   : target ino=4026533416  self ino=4026533416  一致
    stat(/proc/23018/ns/cgroup) 失败: No such file or directory  => 函数返回 -1
  => 返回 -1  <<<< 对自己都判定为'不属于本容器'
```

cgroup namespace 由 Linux **4.6** 引入。在更早的内核（CentOS/RHEL 7 的 3.10、Ubuntu 16.04 的 4.4）上：

- `check_device_pid_in_local_container_pid` **恒返回 -1**；
- 当 openKernelMode 是唯一生效的识别策略（`accumulate_used_memory` 的最后一个 `else if`，`cuda_hook.c:2466`），
  `*used_memory` 恒为 0；
- `load_limited_memory_view` 拿到 used=0 → 预算检查恒通过 → **容器可以用满整张卡**；
- 全程**没有任何日志**。这正是 `启动失败优于静默降级` 要防的形态。

同一文件里 cgroupv1 路径对 cgroup namespace 是**有**显式防护的（`cuda_hook.c:2255` 那段注释讨论了
"容器在自己的 cgroup ns 里会读到 `/`"），说明作者清楚这个概念 —— 这里是漏了一处，不是认知问题。

**附带说明**：即便在 ≥4.6 的内核上，cgroup 这一维的判别力也可疑。containerd 默认
`cgroupns=private` 时每容器一个 cgroup ns（能判别）；`cgroupns=host` 时所有容器共享宿主 cgroup ns
（inode 相同，这一维恒成立，不提供任何判别力）。两种配置下 **mnt 都是唯一真正承重的判据**。

### 3.2 F2（P1）：跨 uid 的 `stat` 被拒

实测以 uid=1000 运行，对 uid=0 的进程：

| 文件 | 可读性 |
|---|---|
| `/proc/1/stat` | ok |
| `/proc/1/cgroup` | ok |
| `/proc/1/ns/mnt` | **EPERM** |
| `/proc/1/maps` | **EPERM** |

`/proc/<pid>/ns/*` 与 `/proc/<pid>/maps` 都要 `PTRACE_MODE_READ` 权限，而 `stat` 和 `cgroup` 是 world-readable。

后果分两面：

- **在 `hostPID=true` 下这恰好帮了正确性** —— 宿主 root 进程本就不该计入，被 EPERM 挡掉是想要的结果（但属于巧合，不是设计）；
- **真正的漏算**：同容器内以不同 uid 运行的 GPU 进程（`runAsUser` 切换、容器内降权子进程）会被判成"不属于本容器"。
  这与 `project_vgpu_nonroot_permissions` 记的非 root 权限模型议题同源。

### 3.3 F3（P0 性能）：maps 扫描在设备锁内

`library_exists_in_process_maps` 要读完目标进程的 `/proc/<pid>/maps`。内核生成这个文件时需持
目标进程的 `mmap_lock` 读锁遍历整棵 VMA 树，所以成本随 VMA 数线性增长，**而且会与目标进程自己的
`mmap`/`munmap` 争锁**。PyTorch 开 `expandable_segments:True` 走 VMM 路径时正是高频 map/unmap，VMA 数轻松上万。

实测（目标进程制造 N 个独立 VMA，父进程按 `util.c` 的写法完整扫一遍）：

| 造 VMA 数 | maps 行数 | maps 字节 | 单次扫描耗时 |
|---|---|---|---|
| 0 | 23 | 2.6 KB | 0.08 ms |
| 1,000 | 2,023 | 100 KB | 2.1 ms |
| 5,000 | 10,023 | 493 KB | 11.4 ms |
| 20,000 | 40,023 | 1.9 MB | **44.8 ms** |
| 60,000 | 120,023 | 5.9 MB | **134.8 ms** |

与另外两条路径对比（同一目标进程，VMA≈20000）：

| 操作 | 单次成本 | 相对 |
|---|---|---|
| `kill(pid, 0)` | 0.6 µs | 1× |
| `device_pid_in_same_container`（4× stat） | 8.5 µs | 14× |
| **`library_exists_in_process_maps`** | **43,789 µs** | **73,000×** |
| cgroupv2 路径：读 `/proc/<pid>/cgroup` | 15.6 µs | 26× |
| （候选缓存校验）读 `/proc/<pid>/stat` 取 starttime | 7.5 µs | 12× |
| （候选缓存校验）`stat(/proc/<pid>)` | 0.9 µs | 1.5× |

**openKernel 路径比 cgroupv2 路径贵 2,800 倍。** 这把优化优先级整个翻转过来了 ——
我之前把重点放在 cgroup 模式的 O(N) 上是搞错了对象：cgroup 路径加缓存只有 15.6 → 7.5 µs（2×），
而 openKernel 路径是 43,789 → 7.5 µs（**5,800×**）。

#### 故障链

调用链是：

```
cuMemAlloc 等内存钩子
  └─ load_limited_memory_view()            cuda_hook.c:363
       ├─ lock_gpu_device(*host_index)     ← 取设备锁（容器级，按设备分段）
       └─ get_used_gpu_memory()            cuda_hook.c:2656
            └─ get_used_gpu_memory_by_device()      :2604
                 ├─ accumulate_used_memory(compute_pids, ...)   :2650
                 └─ accumulate_used_memory(graphic_pids, ...)   :2651
                      └─ 每个 PID: check_device_pid_in_local_container_pid()
```

`lock.c:39` 的注释写明了设计假设：

> The critical section it guards is **~1-3ms** (two NVML process enumerations plus the real driver allocation)

退避参数 `SPIN_INTERVAL_MAX_US=1000`、`SPIN_AGING_MS=50` 都是按这个量级调的，注释里也说"the cap is
sized to the critical section (not 10x it)"。但按 F3 的实测，**设备上一个 2 万 VMA 的进程就让临界区变成 ~44ms**，
N 个进程 → N×44ms。这比设计假设高一到两个数量级，后果逐级放大：

1. 临界区从 1-3ms 涨到数百 ms，`SPIN_INTERVAL_MAX_US` 相对临界区过小 → 退避失准，等待者空转比例上升；
2. 同容器其他进程的**所有**显存分配被串行阻塞在这段时间上；
3. 极端情况（进程多 / VMA 多）累积超过 `LOCK_TIMEOUT_MS=10000` → `lock_gpu_device` 返回 -1；
4. 按 `lock.c:35` 的注释，此时**调用者无锁继续** —— "the budget check that this lock exists to serialise
   would run with no mutual exclusion at all" → 并发超卖。

这条链比 HAMi#3291 的 O(M²) 更严重：那个是 O(M²) 但常数极小，这个是 O(N) 但常数 44ms，而且在锁内。

### 3.4 F4（P2）：`strstr` 对整条路径做子串匹配

```c
char *libPath = GetNthMapsToken(line, 6);
char *p = strstr(libPath, libName);     // libName == "nvidia"
```

实测：`mmap` 一个纯数据文件 `/tmp/claude-1000/nvidia-notes/data.bin`，

```
命中 token6=[/tmp/claude-1000/nvidia-notes/data.bin]
  -> library_exists_in_process_maps("nvidia") 会返回 0(=在用GPU)
```

路径里任何位置含 `nvidia` 都算：`/home/nvidia/model.pt`、`/opt/conda/envs/nvidia-env/lib/libfoo.so`、
`/opt/nvidia-container-toolkit/...`。严重性有限（③ 只是串号兜底，前面已过 ②），但匹配确实过松。

更严的判据应当是 basename 前缀：`/dev/nvidia*`、`libnvidia-*`、`libcuda.so*`。

顺带确认两件解析正确的事（都已实测）：
- 字段号 6 是对的。maps 行是 `addr perms offset dev inode pathname`，`dev` 形如 `fd:03` 算一个 token；
  `strtok_r` 会合并连续分隔符，所以 pathname 前的对齐空格不影响取值；
- 匿名映射只有 5 个字段，`GetNthMapsToken` 返回 NULL → `continue`，正确。`[heap]`/`[stack]` 这类伪路径
  会作为字段 6 返回，不含 `nvidia`，无害。

### 3.5 F5（P3）：行缓冲 1024 < PATH_MAX

`char line[1024]`，maps 行固定部分约 73 字节，所以 pathname 超过约 950 字节即被 `fgets` 截断，
残段会被当作新行解析。对 `/dev/nvidia0` 这种短路径无影响，但深层挂载路径下可能漏判。
同理，pathname 含空格时（`/path/file (deleted)`、路径自带空格）`strtok_r` 只取到第一个空格前的部分。

### 3.6 F6（P3）：自身 ns 被重复 stat

`device_pid_in_same_container` 每次都 `stat("/proc/self/ns/mnt")` 和 `stat("/proc/self/ns/cgroup")`。
我们自己的 ns inode 在进程生命周期内不会变（除非自己 `unshare`，而我们不会），这是**每个 PID 2 次纯浪费的 syscall**。
缓存掉是无条件正确的。注意 fork 后子进程继承同一 ns，inode 不变，所以**不需要**在 fork handler 里失效这份缓存。

### 3.7 latch 逻辑复核：没有 bug

```c
} else if (compatibility_mode & CGROUPV2_COMPATIBILITY_MODE) {
    int matchCGroupV2Mode = 0;
    for (i = 0; i < size_on_device; i++) {
      if (!matchOpenKernel && check_device_pid_in_cgroupv2_container(pid) == 0) {
        matchCGroupV2Mode = 1; *used += ...;
      } else if (!matchCGroupV2Mode && openKernelMode && check_device_pid_in_local_container_pid(pid) == 0) {
        matchOpenKernel = 1; *used += ...;
      }
```

逐情形走过，两个 latch 自洽，且与 `cuda_hook.c:2366` 的注释所述意图一致（一旦一种识别策略在某个 list 上
证明自己，该 list 剩余部分不再尝试另一种）。`matchOpenKernel` 是函数局部，`accumulate_used_memory`
被 compute / graphics 各调一次，所以 latch 确实是 per-pass 的，符合注释。**这里没有发现 bug。**

需要记住的衍生后果：latch 意味着**第一个被判定的 PID 决定了整个 list 的识别策略**。
如果 openKernel 对某个不属于本容器的 PID 误判成功（F4 让这更可能），`matchOpenKernel` 置位后
整个 list 都走这条路，别人的显存会被计入 → 过算 → 本容器被误判 OOM。
这正是用户强调"不能单纯累加、必须特判"的具体后果，所以 §3.4 的收紧不只是洁癖。

---

## 4. 修正后的设计约束

用户已指出原约束 #1 是错的。重列：

| # | 约束 | 说明 |
|---|------|------|
| C1 | **答案绑定的是"进程实例"，不是 PID 号** | 原写法"答案在进程生命周期内不变"错在把 PID 号当身份。PID 会被复用，第二次扫到的 pid n 可能已是别的容器或宿主机的进程。缓存键必须是 `(pid, 代标记)`，不是 `pid` |
| C2 | 代标记取 `starttime`，理由是回绕下界而非时间精度 | §2.2。必须在注释里写明 `pid_max` 前提（C3），以及这不是"时间戳够精确" |
| C3 | `pid_max` 过小时该论证失效 | 启动时读 `/proc/sys/kernel/pid_max`，低于阈值则**不启用缓存**（退回逐次判定），而非静默照用 |
| C4 | 填充缓存必须 read-verify-read | §2.3(2) 的 TOCTOU。判定前后各读一次 `starttime`，不一致则本次结果不入缓存 |
| C5 | `starttime` 读失败/为 0 → 不可缓存 | 不能把读失败当成"匹配" |
| C6 | 否定结果也要缓存 | 「不属于本容器」同样是昂贵结论。只缓存肯定结果，则别人家的进程每次都要付 44ms |
| C7 | fork 后必须整表清空 | 子进程继承缓存，但它的 mnt/cgroup ns 虽相同、判定前提（自身 PID、session）可能变；且表内容是父进程视角。沿用 `loader_child_after_fork` 的既有做法 |
| C8 | 无门控 | 用户已定：不加 feature gate |
| C9 | 缓存不改变任何判定结果 | 缓存只能复现逐次判定会给出的同一答案。任何"顺带放宽/收紧"必须作为独立改动，不藏在缓存里 |
| C10 | 自身 ns inode 单独缓存，且不随 fork 失效 | §3.6。与 PID 缓存是两件事，不要混在一个失效域里 |

---

## 5. 方案

用户已定：**C 方案（复用 pids.config）否决** —— "cgroup路径本来就是兼容模式的一种, 如果真需要client可以启动client模式"。
A 倾向、B 保留。

### 5.1 方案 A：`(pid, starttime)` 代校验缓存

对两条路径统一加一层缓存，键 `pid`、值 `{starttime, verdict}`：

```
                        ┌─────────────────────────────────────┐
                        │ accumulate_used_memory 对每个 PID   │
                        └──────────────┬──────────────────────┘
                                       ▼
                        ┌──────────────────────────────────────┐
                        │ pid_verdict_lookup(pid)              │
                        │  查表拿到 cached{starttime, verdict}  │
                        └──────────────┬───────────────────────┘
                              未命中 ◄─┴─► 命中
                                 │          │
                                 │          ▼
                                 │   ┌────────────────────────────┐
                                 │   │ 读 /proc/<pid>/stat 字段22 │ 7.5 µs
                                 │   │        now_start          │
                                 │   └────────┬───────────────────┘
                                 │     读失败 │ 成功
                                 │        ┌───┴────┐
                                 │        ▼        ▼
                                 │   ┌─────────┐  now_start == cached.starttime ?
                                 │   │ 删表项  │        │           │
                                 │   │ 判定 -1 │       是          否(PID 被复用)
                                 │   │(已退出) │        │           │
                                 │   └─────────┘        ▼           ▼
                                 │              ┌───────────┐  ┌─────────┐
                                 │              │返回 cached│  │ 删表项  │
                                 │              │  .verdict │  │ 落到未  │
                                 │              │  【快路径】│  │ 命中流程│
                                 │              └───────────┘  └────┬────┘
                                 │                                  │
                                 ▼◄─────────────────────────────────┘
          ┌──────────────────────────────────────────────────┐
          │ 慢路径（C4 read-verify-read）                     │
          │  s1 = read_starttime(pid)                        │
          │  if s1 == 0: 返回 -1, 不入缓存        ── C5      │
          │                                                  │
          │  verdict = 逐次判定(pid)                          │
          │    ├ cgroupv2 模式: check_device_pid_in_         │
          │    │                cgroupv2_container  15.6µs  │
          │    └ openKernel 模式: check_device_pid_in_       │
          │                       local_container_pid 44ms  │
          │         ① kill(pid,0)                   0.6µs   │
          │         ② ns 比对(自身 inode 走 C10 缓存) 4.3µs   │
          │         ③ 扫 maps                        43.8ms  │
          │                                                  │
          │  s2 = read_starttime(pid)                        │
          │  if s2 != s1: 返回 verdict 但不入缓存  ── C4      │
          │  else: 入缓存 {pid -> (s1, verdict)}   ── C6      │
          │        (verdict 为 -1 也入)                       │
          └──────────────────────────────────────────────────┘
```

**代价与收益**

| 场景 | 现在 | 方案 A 命中 | 倍数 |
|---|---|---|---|
| openKernel，目标 2 万 VMA | 43.8 ms | 7.5 µs | **5,800×** |
| openKernel，目标 6 万 VMA | 134.8 ms | 7.5 µs | **18,000×** |
| cgroupv2 | 15.6 µs | 7.5 µs | 2× |

cgroup 路径收益有限（2×），**但仍然要上**：它和 openKernel 共用同一个 latch 循环，分开实现会出现
"同一个 PID 在两条路径上身份判定口径不同"，这比省下来的代码更危险（C9）。

**结构**：PID 数上限已有 `MAX_PIDS=1024`。表用定长开散列（桶数 256，侵入式单链），
零初始化即合法空表 —— 与 `vmem_table.h` 同样的理由：`hack/check_no_constructors.sh` 禁止
`__attribute__((constructor))`，而惰性初始化会给每次访问都加一道判断。容量上限按 `MAX_PIDS` 配，
满了就不再插入（退化为逐次判定，正确性不变）。

**纯函数化 + nogpu 可测**：按 `sm_delta.h` / `array_size.h` / `vmem_table.h` 的既有惯例，把表的机械部分
放 `include/pid_verdict_table.h`，`starttime` 读取与判定作为函数指针注入，于是
`test/nogpu/test_pid_verdict_table.c` 能直接构造"PID 复用"场景（同 pid、不同 starttime）而不需要真进程。
这是本方案唯一能在 CI 里证明 C1/C4 成立的办法。

**失效点**：
- fork → `loader_child_after_fork` 整表清空（C7）；
- `pid_max` 过小 → 初始化时禁用缓存（C3）；
- 表项在校验失败时立即删除，不靠 TTL。

### 5.2 方案 B（保留）：TTL 缓存

键 `pid`、值 `{verdict, deadline}`，`deadline` 过期即重判。

- **优点**：不读 `starttime`，命中成本 ~0；不依赖 `pid_max` 论证。
- **缺点**：正确性建立在"TTL 内 PID 不会被复用"这个**无法从任何前提推出**的假设上，
  而 TTL 的取值是个魔法值 —— 用户的判断"A的正确性更清晰没有基于一个不稳定的ttl魔法值"我认同。
  §2.2 实测最小复用间隔 7.9 s，意味着任何 >7.9s 的 TTL 在本机就已经不安全，而这个边界随机器和负载漂移。
- **保留理由**：若将来发现 `starttime` 读取本身在某些环境下成为瓶颈（7.5µs × N，N 大时不可忽略），
  B 可作为 A 的快路径补充（TTL 内免校验 + 超时走 A 的校验），而不是替代。

### 5.3 与 F1/F2/F4/F5/F6 的关系

缓存**不修**任何正确性问题，按 C9 它们必须分开处理。按我的判断排序：

1. **F1（P0）必须先修，且独立于缓存**。否则缓存的是一个恒为 -1 的错误答案 —— 缓存只会让它更快地错。
   修法有两个方向，需要你拍板：
   - **(i) 可用性探测 + 收窄判据**：初始化时探测 `/proc/self/ns/cgroup` 是否存在，不存在则 ns 列表只用 `mnt`。
     好处是老内核上这条路径真正可用；代价是判别力下降到只靠 mnt（但 §3.1 已说明 `cgroupns=host` 下
     cgroup 维本来就不提供判别力，所以实际损失小于看起来）。
   - **(ii) 启动失败**：检测不到 cgroup ns 且 openKernelMode 开启时，`LOGGER(FATAL)` 拒绝服务。
     符合 `启动失败优于静默降级`，但会让一批老内核节点直接起不来。

   我倾向 **(i) + 一条 WARNING**：这是"固定配置条件"（内核能力，不会中途变化），按你定的
   `启动失败优于静默降级` 里"固定配置条件才用 warning"的口径，正好落在 warning 一侧；
   而且 mnt 单独已经能挡住 hostPID / shareProcessNamespace 这两个真实威胁。
2. **F2（P1）**：跨 uid `EPERM` 无法从容器内绕过（需要 `CAP_SYS_PTRACE`）。可做的是把 `EPERM` 与
   `ENOENT` 区分开并记一条 VERBOSE，让漏算可观测，而不是静默。是否要进一步处理取决于你对
   「同容器跨 uid GPU 进程」这个场景的实际权重。
3. **F4 / F5（P2/P3）**：收紧为 basename 前缀匹配（`/dev/nvidia`、`libnvidia-`、`libcuda.so`）、
   行缓冲提到 `PATH_MAX + 128`。F4 的收紧会改变判定结果（C9），所以必须是独立 commit，并且
   需要你确认 `/dev/nvidia*` 映射在你们的真实驱动版本上确实出现在 maps 里（我没有 GPU，
   只实测了"设备文件映射会以完整路径出现在 maps 字段 6"这个一般性前提，用的是 `/dev/zero`）。
4. **F6（P3）**：自身 ns inode 缓存，无条件正确，可与缓存一起做（C10）。

---

## 6. 需要你拍板的点

1. **F1 的修法**：(i) 探测 + 只用 mnt + WARNING（我倾向），还是 (ii) FATAL 拒绝服务？
2. **F1 是否先于缓存单独出一个 PR**？我认为应该 —— 它是 P0 且与缓存正交，混在一起会让 review 变难。
3. **F4 收紧的 needle 集合**：`/dev/nvidia`、`libnvidia-`、`libcuda.so` 够不够？你们线上驱动版本里
   CUDA 进程的 maps 实际长什么样，我需要一份真机样本（`cat /proc/<pid>/maps | grep -i nvidia`）才能确认不漏判。
4. **F2 的处理深度**：只做可观测，还是要进一步？
5. **方案 A 是否就这么定**，B 留作后续补充？

## 7. 复现

实测程序在 `/tmp/claude-1000/-workspaces-my-workspace/e13f1321-0d3c-4ee7-9a6c-159f1169f61b/scratchpad/pidreuse/`：

| 文件 | 验证内容 | 关键输出 |
|---|---|---|
| `reuse.c` | 20 万轮 fork，记录 `(pid, starttime)` | 16.8 万次 PID 复用，starttime 相同 **0** 次 |
| `rate.c` | 纯 fork 速率 + 同 PID 相邻分配的最小间隔 | 2,856 fork/s；最小间隔 **7,896.7 ms**；<10ms 复用 **0** 次 |
| `maps.c` | maps 字段 6 解析；设备文件映射是否以路径出现；`strstr` 假阳性 | 解析正确；`/dev/zero` 以路径出现；`/tmp/.../nvidia-notes/data.bin` **误命中** |
| `mapscost.c` | maps 扫描成本 vs VMA 数 | 20000 VMA → **44.8 ms**；60000 → **134.8 ms** |
| `cmp.c` | 三条路径与两种代标记的单次成本 | 见 §3.3 表 |
| `pidfd.c` | pidfd 可用性 | `ENOSYS`（内核 3.10 < 5.3） |
| `perm.c` | 跨 uid 的 `/proc` 可读性 | `ns/mnt`、`maps` → EPERM；`stat`、`cgroup` → ok |
| `nscheck.c` | 原样移植 `device_pid_in_same_container` | **对调用者自己的 PID 返回 -1** |

---

## 8. 定稿（2026-10-26，用户已拍板）

拍板结论：§6 的 1 取**探测 + 只用 mnt + WARNING**；2 取 **F1 单独出 PR**；3 开放给更好的探测机制。
本节是 3 的答案，以及据此修正的实施顺序。

### 8.1 用 fd 的设备号取代 maps 的路径子串匹配

你提的「包含 `/dev/nvidia`、`libcuda`、`libnvidia` 关键字」可行，但它保留了两个性质：
仍是**路径字符串匹配**（`/home/libcuda-notes/x.bin` 一样会误命中），且仍要**扫完整个 maps**（F3 的 44ms 不变）。

更好的机制：**扫 `/proc/<pid>/fd`，按 `st_rdev` 比对设备号**。

```c
/* 初始化时一次性取得本容器可见的 NVIDIA 字符设备号 */
struct stat st;
if (stat("/dev/nvidiactl", &st) == 0 && S_ISCHR(st.st_mode)) want[n++] = st.st_rdev;
if (stat("/dev/nvidia-uvm", &st) == 0 && S_ISCHR(st.st_mode)) want[n++] = st.st_rdev;
/* /dev/nvidia0..N 同理 */

/* 每个 PID: 扫 fd 表, stat 跟随 symlink 到目标, 比设备号 */
DIR *d = opendir("/proc/<pid>/fd");
while ((e = readdir(d))) {
  if (!isdigit(e->d_name[0])) continue;
  if (stat("/proc/<pid>/fd/<n>", &st) != 0) continue;
  if (!S_ISCHR(st.st_mode)) continue;
  for (i = 0; i < n; i++) if (st.st_rdev == want[i]) return 0;   /* 在用 GPU */
}
```

**为什么语义更根本**：CUDA driver 要通过 `ioctl` 与 `/dev/nvidiactl` + `/dev/nvidia<N>` 持续通信，
这些 fd 在 `cuInit` 之后一直持有；maps 里的那些映射本身就建立在这些 fd 上。
**有 mmap 必有 fd**，反之不然 —— 所以 fd 是更靠前、更难绕过的判据。

**实测**（目标进程 45 个 fd，与 maps 扫描判定结果一致）：

| 造 VMA 数 | maps 行数 | maps 扫描 | fd+rdev 扫描 | 加速比 |
|---|---|---|---|---|
| 1,000 | 2,023 | 2,070 µs | **20.8 µs** | 99× |
| 20,000 | 40,023 | 44,237 µs | **21.0 µs** | 2,102× |
| 60,000 | 120,023 | 137,272 µs | **21.1 µs** | **6,507×** |

**成本与 VMA 数完全无关** —— 这是关键，它把 F3 的故障链（临界区 44ms → 退避失准 → 打穿
`LOCK_TIMEOUT_MS` → 无锁超卖）从根上切断，而不是靠缓存去摊销。

顺带解决：

- **F4 彻底消失**：比较的是 `st_rdev`（设备号），不是路径字符串。`/home/nvidia/model.pt`
  在数学上不可能匹配，不需要再讨论 needle 集合够不够严；
- **F5 消失**：没有行缓冲，不涉及 `PATH_MAX`；
- 不再依赖 `GetNthMapsToken` 的字段解析与 `strtok_r` 对空格的处理。

### 8.2 它的限制（必须记清，有一条是硬的）

1. **不改善 F2**。实测 `/proc/<pid>/fd` 与 `/proc/<pid>/maps` 权限完全一样 —— 跨 uid 都是 `EPERM`
   （两者都要 `PTRACE_MODE_READ`）。所以同容器跨 uid 的漏算问题，这个方案一分钱都不省。
2. **最坏情况是"未命中"**。命中时只需扫到那个 fd 就退出（实测扫 4 个 fd、32 µs）；
   未命中必须扫完整张 fd 表：

   | fd 总数 | 命中（低号 fd） | 未命中（扫完） |
   |---|---|---|
   | 55 | 32 µs | 114 µs |
   | 1,005 | 333 µs | 1,876 µs |
   | 10,005 | 389 µs | **24,049 µs** |

   好在 ③ 的输入都已经过 ② 筛选（确认是本容器进程），所以**命中才是常态**；
   未命中只发生在"本容器内的非 GPU 进程恰好与 NVML 的 PID 串号"这种罕见情形，
   且会被缓存按 C6 记下，最坏代价只付一次。
   
   我**不建议**用「只扫前 N 号 fd」来给上界封顶：nvidia fd 虽然通常在低号段（`cuInit` 早于业务 fd），
   但进程若在 `cuInit` 前已开了 >N 个 fd 就会漏判 → 漏算显存 → 配额逃逸。
   用缓存摊销比引入一个新的假阴性更划算。
3. **我没有 GPU，这是未验证的前提**。我只实测了一般性机制（用 `/dev/zero`、`/dev/null` 当替身，
   证明 `stat` 跟随 `/proc/<pid>/fd/<n>` 能拿到字符设备的 `st_rdev` 并正确比对）。
   **需要你给一份真机样本**，确认真实 CUDA 进程的 fd 表里有哪些 `/dev/nvidia*`：

   ```sh
   ls -l /proc/<某个CUDA进程pid>/fd | grep -i nvidia
   cat /proc/<同一个pid>/maps | grep -i nvidia
   ```

   两条都要，这样既能确认 fd 判据成立，也能对照出 maps 判据当前实际命中的是哪一行。
   MIG / vGPU / `nvidia-uvm` 的设备号是否需要一并纳入 `want[]`，取决于这份样本。

### 8.3 兜底：fd 探测未命中时回退 maps

按 C9（优化不得改变判定结果），fd 探测未命中时**回退到 maps 扫描**，这样新实现严格不弱于现状：

```
③ 是否在用 GPU
   ├─ fd+rdev 探测 ──命中──► 是 (21 µs, 常态)
   │        │
   │      未命中
   │        ▼
   └─ maps 扫描(收紧为 basename 前缀匹配) ──命中──► 是
            │
          未命中 ──► 否
```

代价是未命中路径变成 24ms + 44ms，但未命中罕见且被缓存。兜底的 maps 仍要按 §3.4 收紧
（basename 前缀 `/dev/nvidia`、`libnvidia-`、`libcuda.so`，按你的意见再加 `libcuda`），
因为它现在是兜底而不是主判据，假阳性的影响面反而更大 —— latch 一旦被误判置位，整个 list 都走错路径（§3.7）。

若 8.2(3) 的真机样本证明 fd 判据对你们的驱动版本完备，这层兜底可以在后续单独删掉，届时 F4/F5 一并消失。

### 8.4 修正后的实施顺序

缓存从「主要收益来源」降级为「次要优化」—— 8.1 已经把 openKernel 单次成本从 44ms 压到 21 µs（2000×），
缓存在此之上只能再省到 7.5 µs（约 3×）。这改变了排期：

| 序号 | 内容 | 为什么这个顺序 |
|---|---|---|
| **PR-1** | **F1**：探测 `/proc/self/ns/cgroup`，不存在则 ns 列表只用 `mnt` + 一条 WARNING | P0 静默失效，与其余全部正交。在它之前做任何性能优化都是在优化一个恒为 -1 的错误答案 |
| **PR-2** | **F3+F4+F5**：③ 改 fd+rdev 主判据、maps 收紧为兜底；**F6** 自身 ns inode 缓存（C10） | 单次成本 2000×，直接切断 F3 的锁故障链。F4/F5 作为副产品消失。需要 8.2(3) 的真机样本才能动手 |
| **PR-3** | **F2 可观测**：区分 `EPERM` / `ENOENT`，EPERM 记一条 VERBOSE | 让漏算可见。深度待定 |
| **PR-4** | **方案 A 缓存**（C1–C10） | 收益从 5800× 降到 3×，但仍有价值：它同时覆盖 cgroupv2 路径，且让 8.2(2) 的未命中最坏情况只付一次 |

PR-2 做完后，是否还值得做 PR-4 可以重新评估 —— 21 µs × N 个进程在设备锁内是否仍需进一步优化，
取决于你们真实的单卡进程数。如果 N 常态是个位数，PR-4 的优先级可以压很低。

---

## 9. 缓存方案的最终评估（2026-10-26，fd 探测落地后）

PR-1(F1)、PR-2(fd 主判据)、F6(自身 ns inode 缓存) 已实施，分支
`fix/library-container-pid-identity`，三个 commit。本节用改造后的真实成本重新评估
方案 A，结论与 §5 相反：**方案 A 原样不应实施**。

### 9.1 改造后的真实成本

| 步骤 | 改造前 | 改造后 |
|---|---|---|
| ① `pid_exist`（kill） | 0.6 µs | 0.30 µs |
| ② `device_pid_in_same_container` | 8.5 µs | **2.46 µs**（F6） |
| ③ 是否在用 GPU | **43,789 µs**（maps，20k VMA） | **~20 µs**（fd，命中）/ 66 µs（45 fd 全扫） |

两条路径的合计：

- **属于本容器的 GPU 进程**（走完三步，③ 命中）：约 **24 µs**
- **不属于本容器的进程**（② 就被拒，**根本走不到 ③**）：**2.77 µs**

第二行是关键，也是 §5 漏掉的：NVML 返回的是**整张卡**的进程列表，而高密度超卖下
设备上多数 PID 属于别的容器 —— 它们在 ② 一步就被便宜地挡掉了。

### 9.2 代标记的校验比它要替代的工作更贵

方案 A 的缓存命中路径是「读 `/proc/<pid>/stat` 校验 `starttime`」。同一轮测量、取多轮最小值：

| | 成本 |
|---|---|
| 缓存命中：读 `/proc/<pid>/stat` 解析第 22 字段 | **5.07 µs** |
| 它要替代的「别人家进程」判定：kill + ns 比对 | **2.77 µs** |
| （参考）`stat(/proc/<pid>)`，但不提供代标记 | 0.95 µs |

**对不属于本容器的 PID，缓存命中比直接重做判定贵 2.32 µs。** 这不是实现细节，
是结构性的：解析 `/proc/<pid>/stat` 到第 22 个字段，比两次 `stat()` 做的事更多。

净收益 = `k × 18.91µs − (N−k) × 2.32µs`（k = 本容器进程数，N = 整卡进程数）：

| 整卡 N | 本容器 k | 净收益 | |
|---|---|---|---|
| 8 | 4 | +66.3 µs | 有收益 |
| 16 | 4 | +47.8 µs | 有收益 |
| 32 | 4 | +10.6 µs | 几乎打平 |
| **64** | **4** | **−63.8 µs** | **反而更慢** |
| 64 | 16 | +191.0 µs | 有收益 |
| **128** | **8** | **−127.5 µs** | **反而更慢** |

临界点约 `k/N = 11%`。而 vGPU 的典型用途恰恰是高密度超卖 —— k/N 低正是目标场景，
方案 A 会在它最该起作用的地方变成负优化。

### 9.3 修正方案 A'：只缓存过了 ② 之后的那一步

原 C6「否定结果也要缓存」的前提已经消失：它成立于 maps 时代（否定结论也要付 44ms），
而现在否定结论多数由 ② 以 2.77 µs 给出。正确的做法是**只缓存昂贵的那一侧**——
把缓存放在 ② 之后，缓存的是第 ③ 步的结果而不是整个函数的结果：

```c
int check_device_pid_in_local_container_pid(unsigned int device_pid) {
  if (device_pid == 0) return -1;
  if (pid_exist(device_pid) != 0) return -1;
  if (device_pid_in_same_container(device_pid) != 0) return -1;   /* 廉价拒绝, 不碰缓存 */
  return gpu_in_use_cached(device_pid);   /* 只有这一步值得缓存 */
}
```

这样别人家的进程完全不进缓存（只多一次哈希查找，约 0.05 µs），净收益恒正：

- 本容器 GPU 进程：24 → 5.07 µs，省 **18.91 µs**
- 罕见的「过了 ② 但 ③ 未命中」（NVML 跨 namespace 串号撞上本容器非 GPU 进程）：省约 **61 µs**

### 9.4 建议：A' 也暂不实施

即便是 A'，收益是 `k × 18.91µs`。k=4 时 **76 µs**，k=16 时 **302 µs**。
对照 `lock.c` 记载的临界区基线「~1-3ms（两次 NVML 进程枚举加一次真实驱动分配）」，
这是 **2.5%–7.5%** 的量级。

而它要换来的复杂度是整套的：代标记、read-verify-read 的 TOCTOU 处理、
`pid_max` 门槛（C3）、fork 整表失效（C7）、定长开散列表、以及一套能在 CI 里证明
C1/C4 成立的 nogpu 测试。

fd 探测已经把这条路径上 2000 倍的改进拿到手了（44 ms → 20 µs）。剩下的 3 倍
不值得这套复杂度 —— 这与 §5 的判断相反，但那时的基线是 44 ms，现在是 24 µs，
同一个方案在两个基线下的价值本就不同。

**重新评估的触发条件**（任一出现即值得回头做 A'）：

1. 真实环境里本容器在设备上的进程数 k 常态超过 ~16（此时 A' 收益 >300 µs，
   开始接近临界区基线的 10%）；
2. 日志里出现大量「过了 ② 但 ③ 未命中」的 PID —— 说明 NVML 跨 namespace 串号频繁，
   这类 PID 每次分配都要付一次全量 fd 扫描（45 个描述符 66 µs，1 万个则是毫秒级）；
3. 真机 profile 显示 PID 判定在设备锁临界区里的占比超过 10%。

第 2 条目前无法观测，这恰好与 §8.4 的 PR-3（F2 可观测性）同属一类缺口 ——
把这条路径上的「廉价拒绝 / 昂贵否定 / 命中」三种结局分别计数，是决定要不要做 A'
的前置条件，也比直接实现缓存便宜得多。

### 9.5 仍然待办

| 项 | 状态 |
|---|---|
| F1 cgroup ns 探测 | 已实施（`fff3cb8`） |
| F3 fd 主判据 + maps 兜底 | 已实施（`5eeda90`） |
| F6 自身 ns inode 缓存 | 已实施（`7ba2d16`） |
| **F4/F5** maps 兜底的路径子串匹配与 1024 行缓冲 | **未做**。影响面已大幅缩小（只在 fd 未命中时才跑），但仍是已知缺陷 |
| **F2** 跨 uid `EPERM` 的可观测性 | **未做**，且现在它还兼有 9.4 第 2 条的判据价值 |
| 方案 A / A' 缓存 | **建议不做**，见 9.4 |

---

## 10. 三个遗留问题的回答（2026-10-26）

### 10.1 maps 兜底真的必要吗

**你的顾虑成立**：第 ③ 步未命中时现在要付 fd 全扫 + maps 全扫，45 个描述符的进程是
66 µs + 44 ms，比改造前的 44 ms **更慢**。

**我不能证明兜底必要。** 它是"没有 GPU 环境、无法穷举驱动行为"下的保守选择，不是针对
某个已确认的缺口。能想到的真实边界有四条，按可信度排：

1. **readlink 返回的是容器 mnt namespace 内的路径**。标准 nvidia-container-toolkit / CDI
   注入的就是 `/dev/nvidia*`，但若有人把设备节点挂到别的路径，前缀匹配就落空，而
   maps 的 `strstr(path, "nvidia")` 更宽松，仍可能命中。
2. **maps 命中的其实多半是库，不是设备节点**。注意 `libcuda.so.1` 的路径里**不含**
   "nvidia" —— 旧判据实际命中的是 `libnvidia-*.so` 或 `/dev/nvidia*` 的映射。
   也就是说 maps 提供的是"映射了 NVIDIA 用户态库"这个**另一个维度**的证据，
   与"持有设备描述符"不是同一件事。
3. **graphics 进程**（`nvmlDeviceGetGraphicsRunningProcesses`）：Vulkan/OpenGL 进程在
   走 DRM 的配置下是否一定持有 `/dev/nvidia*`，我无法确认；但它们一定映射了
   `libnvidia-glcore.so` 之类。
4. mmap 建立后 close 掉 fd：CUDA driver 不会这么做（要持续 ioctl），但第三方库难说。

**未命中实际多久发生一次**：第 ③ 步只在第 ② 步通过后才到达，即「本容器进程 + NVML 说它
在用 GPU + 却没有 nvidia 描述符」。在 openKernelMode 与驱动行为匹配的正确配置下，
NVML 给的就是本命名空间的 PID，这个组合几乎不出现；它真出现，要么说明 fd 判据漏了
（兜底救了命），要么说明模式配置与驱动行为不匹配（此时统计本身已经是错的，44 ms 是
次要问题）。

**建议：保留兜底，但给它加一个计数 + 限流日志**，专门记录「fd 未命中而 maps 命中」。

- 真机上该计数恒为 0 → 可以安全删掉兜底，未命中代价回落到 66 µs，F4/F5 一并消失；
- 非 0 → 兜底确实在救命，且日志会告诉我们是哪种场景。

代价是一个原子计数加一条一次性日志，比现在两头猜都强，也比我凭想象删掉它安全。
这件事和 §8.4 的 PR-3（F2 的 EPERM 可观测性）是同一类缺口，适合一起做。

### 10.2 不做缓存的数据

单步成本（本仓库 devcontainer，多轮取最小值）：

| 步骤 | 改造前 | 改造后 |
|---|---|---|
| ① `pid_exist`（kill） | 0.60 µs | **0.30 µs** |
| ② `device_pid_in_same_container` | 8.50 µs | **2.46 µs** |
| ③ 是否在用 GPU | **44,000 µs**（maps，20k VMA） | **~20 µs** 命中 / 66 µs 全扫 |

两类 PID 的全程成本 —— 关键在于第二行，**不属于本容器的 PID 在 ② 就被拒，根本走不到 ③**：

| | 改造前 | 改造后 | 缓存命中 |
|---|---|---|---|
| 本容器的 GPU 进程（走完三步） | 44,009 µs | **22.8 µs** | 5.07 µs |
| 别人家的进程（② 即拒） | 9.1 µs | **2.77 µs** | 5.07 µs ← **比不缓存更贵** |

缓存命中要读 `/proc/<pid>/stat` 并解析到第 22 个字段（`starttime`），5.07 µs；
而它对别人家的 PID 所替代的工作只有 kill 加一次 stat，2.77 µs。**这是结构性的，
不是实现问题。**

整卡 N 个进程、其中本容器 k 个，每次分配在设备锁内的总开销：

| N | k | 改造前 | 改造后 | 再加缓存 | 对照 `lock.c` 的 1–3 ms 基线 |
|---|---|---|---|---|---|
| 16 | 4 | 176.2 ms | **56 µs** | 54 µs | 改造后 1.9%–5.6% |
| 32 | 4 | 176.3 ms | **169 µs** | 98 µs | 改造后 5.6%–17% |
| 64 | 4 | 176.6 ms | **257 µs** | 186 µs | 改造后 8.6%–26% |
| 64 | 16 | 704.3 ms | **498 µs** | 214 µs | 改造后 17%–50% |
| 128 | 8 | 352.4 ms | **355 µs** | 373 µs ← **更慢** | 改造后 12%–36% |

改造前是基线的 **59–235 倍**（灾难）；改造后降到 **2%–26%**，已经不是瓶颈。
再加缓存只能把它压到 1.8%–19%，而在 k/N 低于约 11% 的格局下反而变慢。

`lock.c` 的那句注释是判据：临界区本体是「两次 NVML 进程枚举加一次真实驱动分配」，
1–3 ms。PID 判定从占主导（59–235 倍）变成占零头（<26%）之后，继续优化它收益递减，
而缓存要换来代标记、read-verify-read、`pid_max` 门槛、fork 整表失效、散列表和一套
nogpu 测试 —— 不划算。重新评估的触发条件见 §9.4。

### 10.3 HAMi #3291 的原计划还要不要继续

原计划其实是**两条独立的线**，现在状态不同：

**线一：HAMi#3291 本身对应的问题 —— 已完成。**
#3291 的根因是 hami-core allocator 的 `remove_chunk()` 每次 `cuMemFree` 线性扫一遍链表，
PyTorch OOM 重试时 `release_cached_blocks()` 一次性释放全部缓存块 → O(M²)。我们这边的
同源结构是 `g_memory_node` 链表（五个访问点里三个是按 dptr 的点查）。已在分支
`fix/library-memory-node-hashtable` 换成定长开散列表，A/B 实测 M=2k/10k/40k 时
**142× / 642× / 1420×**，确认了二次→线性。这条线按原计划走完了，等你合 PR。

**线二：PID 判定（方案 A/B/C）—— 改道了，而且已经够了。**
这条线不是 #3291 的内容，是追查 #3291 时我在本地代码里另外发现的。原计划是给 cgroup
路径加 O(N) PID 缓存（方案 A/B/C）。现在的结论与当初相反，原因是**瓶颈根本不在
cgroup 路径上**：

- 当初的方案 A/B/C 都围绕 cgroup 路径（一次 `/proc/<pid>/cgroup` 读，15.6 µs）；
- 实测发现真正昂贵的是 openKernelMode 路径的 maps 扫描（44 ms，**贵 2800 倍**），
  而且它跑在设备锁内、与 `lock.c` 的 1–3 ms 设计假设差两个数量级；
- 更关键的是 F1：那条路径在 Linux < 4.6 上**根本是失效的**（恒返回 -1，静默把限额
  变成 no-op）。先优化它等于在优化一个恒错的答案。

所以这条线的实际落点是 F1 + fd 主判据 + F6，而不是方案 A。按 §10.2 的数据，
PID 判定已从临界区的主导项降到零头，**目前的改造已经足够**，缓存不做。

**剩下的不是性能，是正确性与可观测性**，优先级比缓存高：

| 项 | 性质 | 建议 |
|---|---|---|
| F4/F5 | maps 兜底的路径子串匹配假阳性、1024 行缓冲截断 | 影响面已缩小（仅 fd 未命中时跑），但仍是已知缺陷 |
| F2 | 跨 uid `EPERM` 导致同容器跨 uid 进程漏算，且无日志 | 与 §10.1 的兜底计数一起做 |
| §10.1 | 「fd 未命中而 maps 命中」计数 | 决定能否删掉兜底的前置条件 |

---

## 11. HAMi-core#382：瓶颈推测与对我们的重新评估（2026-10-26）

HAMi#3291 已转到 HAMi-core#382（"vLLM TP=8 startup remains 3.2x slower with HAMi
control after disabling expandable_segments"）。新数据推翻了我们此前的主因判断。

### 11.1 #382 的新数据

| 配置 | health | profile/KV/warmup |
|---|---|---|
| HAMi on, `expandable_segments:True` | ~53 min | 2596.57 s |
| HAMi on, `expandable_segments:False` | 1384.07 s | 1210.64 s |
| `CUDA_DISABLE_CONTROL=true`, False | 433.29 s | 313.14 s |
| 裸 Docker（基线） | 463.03 s | 316.90 s |

**关键：禁用 `expandable_segments` 后仍慢 3.19×**（engine init 3.87×）。
这直接排除了"VMM 路径 / allocator 链表"作为主因 —— 那条路径在 `False` 下根本不走。

分阶段（TP0，`False`，enabled vs disabled）：权重加载 93.58 s vs 58.77 s（1.59×）、
首次图初始化 266 s vs 149 s（1.79×）、二次图初始化 68 s vs 28 s（2.43×）。
**权重加载是大块连续分配，它也慢 1.59×**，进一步说明问题不在数据结构的复杂度上。

环境：8×H100 80GB、driver 580.159.04、HAMi v2.10.0 经 `/etc/ld.so.preload` 注入、
vLLM 0.32.0 / PyTorch 2.13.0+cu130 / CUDA 13.0、内存上限 81559m（≈整卡）。

maintainer（moezdil）两个未经确认的假设及其实测：

1. 接近内存上限时失败的分配跨进程串行化 —— 8 进程重试 256 MB：**0.79 s vs 6.47 s（8.2×）**；
2. 每进程 libvgpu 初始化开销 × `nvrtc_compile` 子进程数 —— 3090 上每进程
   **~700 ms vs ~235 ms**（含 cuInit、建上下文、一次分配），即 **+465 ms/进程**；
   采样的 10 个 `nvrtc_compile` 子进程全部 mmap 了 HAMi 库并继承了注入。

采样到的等待通道：`uvm_gpu_retain_by_uuid`、`os_acquire_rwlock_write`。
**官方尚无确认的根因，也没有修复或时间线。**

### 11.2 从本地 HAMi-core 源码推测瓶颈

读 `cdb5260`（含 issue 中提到的 #374）。**HAMi-core 没有任何 constructor**，
和我们一样是 `pthread_once` 惰性初始化 —— 所以 465 ms 不是"库加载"开销，
而是 cuInit 路径上的开销。它落在 `postInit()`（`src/libvgpu.c:897`）：

```
cuInit()                                  libvgpu.c:980
  └─ pthread_once(pre_cuinit_flag, preInit)
  └─ ensure_post_init() -> pthread_once(post_cuinit_flag, postInit)
       ├─ allocator_init(); map_cuda_visible_devices();
       ├─ hostpid_fallback_lock_acquire_until()   ← 注释原文 "node-wide"
       ├─ lock_postinit()                          ← 第二把锁
       ├─ set_task_pid()                           utils.c:318
       │    └─ find_own_hostpid(nvml_probe, …, HOSTPID_DETECT_MAX_ATTEMPTS=8)
       │         每次 probe (utils.c:240):
       │           list_compute_processes(before)        ← NVML 枚举
       │           cuDevicePrimaryCtxRetain(&pctx, 0)    ← 建 primary context
       │           list_compute_processes(during)        ← NVML 枚举
       │           cuDevicePrimaryCtxRelease(0)
       │           list_compute_processes(after)         ← NVML 枚举
       └─ init_utilization_watcher()
```

即**单次探测 = 3 次 NVML 枚举 + 一次 primary context 建销，最多重试 8 次，
且整个过程在节点全局文件锁内串行**。H100 80GB 上建一次 primary context 本就是
百毫秒量级，这与 +465 ms/进程 的量级吻合。

再加上 `childReinitPostInit()`（`libvgpu.c:963`）在 fork 后把
`post_cuinit_flag` 重置回 `PTHREAD_ONCE_INIT` —— **每个 fork 出来且会调 cuInit 的
子进程都要重做一遍**。vLLM 在权重加载与图捕获期间持续派生 `nvrtc_compile` 子进程，
它们经 `/etc/ld.so.preload` 继承注入，于是 8 个 worker 与这一串子进程全部排在
同一把节点全局锁上 —— 这同时解释了 maintainer 的两个实测（单进程 +465 ms、
多进程 8.2× 串行化），也解释了为什么连权重加载都慢 1.59%（同期子进程在抢锁）。

**这正是用户在分析 #3291 时就指出的机制**："hami 有一个探测 host pid 的逻辑，
需要用节点全局文件锁去创建上下文循环探测 pid……他们的锁是节点全局的所有容器共享，
当多容器同时启动延迟变高。" 当时我把重点放在 allocator 上，方向偏了；#382 的数据
和上面的代码路径都指回这个判断。

### 11.3 对我们 library 的影响：结构上没有这个瓶颈

| 维度 | HAMi-core | 本仓库 |
|---|---|---|
| host PID 从哪来 | library 在容器内**探测**：建 primary context + NVML 枚举差分，最多 8 次 | **不探测**。manager 在容器外把 PID 列表写进 `pids.config`，library 只读文件 |
| 初始化期的锁 | **节点全局**文件锁（`hostpid_fallback_lock` + `lock_postinit`），所有容器共享 | 无。设备锁是**容器级**（`session_path(SESSION_LOCK_DIR)`，容器自己的 /tmp）且按设备分段（`vgpu_%d.lock`） |
| 初始化做什么 | 见 11.2 | `load_necessary_data()`：注册 atfork、读 `/proc/driver/nvidia/version`、dlopen NVML、dlopen CUDA、读配置、重置 index 映射。**不建上下文、不枚举 NVML、不重试、不抢全局锁** |
| constructor | 无（惰性） | 无（惰性），且由 `hack/check_no_constructors.sh` 强制 |
| 不碰 CUDA 的子进程 | 仍 mmap 库；若调 cuInit 则付全套 | 仅付库加载：**实测 0.125 ms/进程** |

实测对照（fork+execve `/bin/true`，400 次/轮 × 7 轮取最小）：
不预载 1.080 ms/进程，预载 `libvgpu-control.so` 1.205 ms/进程，**差 0.125 ms**。
`libvgpu-control.so` 4.4 MB，`INIT_ARRAYSZ = 8 bytes`（只有 GCC 的
`__frame_dummy_init_array_entry`，确认零自定义 constructor）。

所以 #382 的病我们**结构上就没有** —— 不是因为优化得好，而是因为 host PID 的来源
不同：我们把这件事放在了容器外的 manager 里。

### 11.4 新发现：我们在 CLIENT 模式下有一个同类但小两个量级的风险

`load_controller_configuration()`（`loader.c:2729`）末尾：

```c
if ((g_vgpu_config->compatibility_mode & CLIENT_COMPATIBILITY_MODE) == CLIENT_COMPATIBILITY_MODE) {
  register_to_remote_with_data(...);     /* loader.c:2833 */
}
```

而 `register_to_remote_with_data()`（`register.c:30`）是 **fork + execl 一个 RPC client
二进制，然后阻塞 waitpid**。

两个事实叠起来才是风险：

1. `g_controller_config_init` 在 `loader_child_after_fork()` 里被重置（`loader.c:2920`）
   —— 这与 HAMi 的 `childReinitPostInit` 重置 `post_cuinit_flag` 是**同一个模式**；
2. 函数开头的早退只覆盖 mmap（`if (g_vgpu_config == NULL)` 那一段），而**注册语句在该块之外**
   —— 本地 fork 的子进程会跳过 re-mmap，但**仍然执行注册**。

于是在 CLIENT 模式下，每个 fork 出来且会碰 CUDA 的子进程都要同步 exec 一次 RPC client。

**这是功能必需，不是疏漏**：子进程有新的 PID，manager 必须知道它属于本容器，
否则该进程的显存不会被计入 —— 漏算就是配额逃逸。

量级差异仍然悬殊：fork + exec + gRPC 往返 + 等待退出，粗估几毫秒到几十毫秒，
比 HAMi 的 465 ms 小一到两个数量级，而且**没有节点全局锁**把所有容器串起来。
另外 CLIENT 模式要显式设 `MANAGER_COMPATIBILITY_MODE=200`，不是默认值。

**建议**（未实施，等拍板）：如果"CLIENT 模式 + 大量短命 CUDA 子进程"是真实场景，
值得先量一下这个 exec 的真实耗时。可选的收敛方向：
- 把注册做成**按 PID 幂等**的，子进程若已在 manager 的列表里就跳过 exec；
- 或者让 manager 侧按 cgroup/ns 自行发现新 PID，去掉 library 侧的主动注册。

两者都要先有数据，否则是凭想象优化。

### 11.5 哈希表改造的价值重新定位

此前把 `fix/library-memory-node-hashtable` 当作 #3291 的解药，这个定位要修正：

- 它消除的 O(M²) 是**真实的**（A/B 实测 M=2k/10k/40k 时 142× / 642× / 1420×），
  防的是 `release_cached_blocks()` 那类"一次退役全部记录"的批量操作，值得保留；
- 但按 #382 的新数据，allocator 数据结构**不是** vLLM 启动慢的主因；
- 而 #382 真正指向的那个瓶颈，我们**结构上没有**。

结论：哈希表照常合，但不要把它当成 #382 的对策；#382 对我们的实际待办只有
11.4 那一条，而它需要先测数据再决定。

---

## 12. HAMi-core#382 第二轮追踪与双向性能评估（2026-10-27）

两个分支已合入 main（PR #115 `a967a29`、PR #116 `1e5e997`）。本节根据 #382 的
新诊断数据重做瓶颈推测，并双向评估两边的性能开销。

### 12.1 新证据推翻了 §11 的两个推测

10/26 的 `capture-20261026` 与 10/27 的 GDB 采样给出了关键数据：

| 指标 | enabled | bypass |
|---|---|---|
| health 成功 | 未达成（3649.01 s 人为停止） | 855.74 s |
| 单个 `nvrtc_compile` exec→exit 中位数 | **52.16 s** | 8.87 s |
| 同上 p95 | 88.49 s | 12.23 s |
| 最后一次 NVRTC 退出 | 578.49 s（**之后仍有 3070.52 s 慢时间**） | — |
| 非线程 clone/clone3 返回 | 92 | 2527 |
| 第二轮（无 strace）最后 NVRTC 活动 | 1217.75 s | 250.38 s |
| 第二轮 health | 1753.94 s | 579.70 s |
| 编译器退出后窗口的日志行数 | **1,291,988 行**（`LIBCUDA_LOG_LEVEL=1`） | — |

**推翻了 §11.2 的"子进程数 × 初始化开销"**：最后一个编译器退出后还慢 3070 s，
maintainer 原话是 "the ~3000 s gap after the last compiler exit is the bigger issue"。
初始化开销无法解释编译器退完之后的事。

**推翻了"锁竞争"**：15 个主线程采样里**没有任何一个**出现 futex / `sem_wait` /
pthread-wait 栈帧；4 个含 HAMi 栈帧的分别落在 `cuCtxSynchronize`、free 路径、分配路径。
maintainer 明确记为 "lock-contention hypothesis remains unproven"。

**官方至今没有确认根因，也没有修复或 PR。** 以下仍是推测，但这次有代码路径支撑。

### 12.2 推测：OOM 路径让临界区膨胀，再被单一全局信号量放大

读 `cdb5260`，每次分配的实际路径：

```
cuMemAlloc → add_chunk → reserve_device_memory          allocator.c:142
  ├─ lock_shrreg()                                       ← 见下
  ├─ oom_check_impl(dev, size, already_locked=1)         allocator.c:37
  │    ├─ get_gpu_memory_usage()                         memory_limit.c:373
  │    │    └─ seqlock 读 proc_num 个 slot（纯用户态原子，快）
  │    └─ if (new_allocated > limit):                    ← 接近上限时每次都进
  │         ├─ LOG_ERROR("Device %d OOM %lu / %lu")      ← **无门控**，见 12.2.1
  │         ├─ clear_proc_slot_nolock(1)                 memory_limit.c:1120
  │         │    └─ 遍历所有 slot，对每个活进程调 proc_alive()
  │         │         = fopen("/proc/<pid>/stat") + fscanf + fclose
  │         └─ 若 cleared>0 → **递归重做整个 oom_check**
  ├─ add_gpu_device_memory_usage(...)
  └─ unlock_shrreg()
```

三个要点：

**(1) `sem_t sem` 是共享区里唯一的一个信号量，不分设备**
（`multiprocess_memory_limit.h:106`，注释 "Only for process slot add/remove"）。
`lock_shrreg()` 用 `sem_timedwait`，而 `SEM_WAIT_TIME = 10` 是**秒**
（`get_timespec` 做 `tv_sec + seconds`），`SEM_WAIT_RETRY_TIMES = 30`。
所以 vLLM TP=8 的 8 个 worker 虽然各用一张卡，却都排在**同一把**信号量上。
这正是 maintainer 自己测到的 "8 进程重试 256 MB：0.79 s vs 6.47 s"（8.2×）。

**(2) `clear_proc_slot_nolock` 的注释与代码不符。** 注释写
"Limit to 10 checks per call to avoid holding lock too long"，但代码是
`if (do_clear > 0 && cleaned_dead < 10 && proc_alive(pid) == ...)` ——
`cleaned_dead` 只在**发现死进程**时递增，所以对**每个活着的** slot 都会调一次
`proc_alive()`，次数不受限。`proc_alive()` 是一次完整的
`fopen`+`fscanf`+`fclose`（栈上还各有两个 8 KB 缓冲），量级 10–20 µs。
`SHARED_REGION_MAX_PROCESS_NUM` 是 1024。

**(3) `nvrtc_compile` 子进程也会注册 slot**（它们调 cuInit → postInit）。
#382 记录了 **177 个 NVRTC PID**。于是 `proc_num` 可能从 8 涨到 185，
每次 OOM 检查在全局锁内要做 185 次 `/proc` 读 ≈ 2.8 ms，
8 个 worker 串行 → 每个 worker 等 ~22 ms，而 PyTorch 在 `utilization 0.92`
下会反复试探分配。这是雪崩。

这一条链解释了新数据的每一项：编译器进程慢 5.9×（它们也抢同一把锁）、
编译器退完仍慢 3000 s（slot 未必及时清理，`proc_num` 仍大，稳态分配继续走这条路）、
以及**为什么采样看不到 futex** —— 时间花在 `proc_alive` 的文件 I/O 和
`LOG_ERROR` 的 `fprintf` 上，不是花在等锁上。

#### 12.2.1 `LOG_ERROR` 没有级别门控

`src/include/log_utils.h`：`LOG_DEBUG/INFO/WARN/MSG` 都有 `if (g_log_level >= N)`，
而 `LOG_ERROR` **直接 `fprintf(stderr, ...)`，没有任何级别检查**。
所以 `LIBCUDA_LOG_LEVEL=1` 下那 1,291,988 行基本都是 LOG_ERROR，
其中 `oom_check_impl` 的那一行每次 OOM 检查失败都打。容器里 stderr 通向
kubelet 的日志管道，管道满时 `write(2)` 会阻塞——这笔开销也在全局锁内。

### 12.3 两边是两种记账架构，瓶颈位置因此不同

| | HAMi-core | 本仓库 |
|---|---|---|
| 当前用量从哪来 | **各进程自报到共享区**，读时 seqlock 求和 | **向驱动查询**：`nvmlDeviceGetComputeRunningProcesses` + `...Graphics...` 两次枚举 |
| 单次读的成本 | 纯用户态原子操作，**亚微秒** | **1–2 ms**（syscall + 驱动） |
| 一致性保障 | 单一全局信号量串行「检查+提交」 | 按设备分段的文件锁 `vgpu_<dev>.lock` |
| 并发度（8 卡 8 进程） | 1 把锁 → **全串行** | 8 把锁 → **全并行** |
| 死进程清理 | **必须做**（自报记账会留下死 slot），且在锁内读 `/proc` | **不需要**（NVML 是权威，死进程自然消失） |
| OOM 时的临界区 | 膨胀：`proc_num` 次 `/proc` 读 + 无门控日志 + 可能递归 | 不膨胀：`metrics_record_oom()` + 返回错误码 |
| 日志门控 | `LOG_ERROR` **无门控** | `LOGGER` 所有级别都过 `LOGGER_SHOULD_PRINT` |

### 12.4 我们的优势（针对 #382 的场景）

1. **锁粒度**：`lock_gpu_device(host_index)` 按设备分段，TP=8 的 8 个 worker 各用
   一把锁，零竞争。HAMi 的 8 个 worker 争同一把 `region->sem`。
2. **OOM 路径不膨胀**：我们 OOM 时只记一个 metric 并返回
   `CUDA_ERROR_OUT_OF_MEMORY`，不抢额外锁、不遍历进程、不读 `/proc`、不递归。
3. **不需要死进程清理**：显存用量来自 NVML，进程死了它就不在枚举里。
   HAMi 的自报记账必须清理死 slot，而清理逻辑落在 OOM 热路径的锁内。
4. **日志不会在 OOM 风暴下失控**：我们的 ERROR 也受级别门控。
5. **初始化不探测 host PID**（§11.3）：无节点全局锁、无建上下文、无 8 次重试。
6. 我们的 `check_cleanup_vmem_nodes_by_device` 有三层保护：
   `processes_size == 0` 无锁早退、feature gate 默认关闭时 `g_device_vmem == NULL`
   直接返回、锁按设备分段——而且它只在「`vmem_used > 0` 且还有空间」时才被调用，
   不是每次 OOM 都做。

### 12.5 我们的劣势（必须诚实记下）

**每次分配付一次 NVML 双枚举，约 1–2 ms，且没有任何结果缓存。**
`get_used_gpu_memory_by_device()` 每次调用都做两次完整枚举；
`lock.c:39` 把临界区记作 "~1-3 ms（两次 NVML 进程枚举加一次真实驱动分配）"。
HAMi 读同一个量只要亚微秒（共享区）。**单次成本上我们慢约三个数量级。**

付这笔钱的 hook 有 13 处，其中包括 **`cuMemCreate`**（`cuda_hook.c:3939`）——
也就是 PyTorch `expandable_segments:True` 走的 VMM 路径。按 2 MiB granularity，
49 GiB 权重约 25000 次 `cuMemCreate`，**仅 NVML 枚举就是 25–50 s**。
`expandable_segments:False` 时 caching allocator 以大块（百 MB 级）向驱动申请，
次数降到百量级，这笔开销可忽略——所以这个风险是**高频小粒度分配专有**的。

附带记录：`get_used_gpu_memory_by_device()` 里
`nvmlProcessInfo_t compute_pids[MAX_PIDS]` + `graphic_pids[MAX_PIDS]`
（`MAX_PIDS = 1024`）是约 64 KB 的栈分配，每次调用一次。栈分配本身很便宜，
只有被写入的页才会触发缺页，但它在分配热路径上，值得知道。

### 12.6 场景化结论

| 场景 | 更优的一方 | 原因 |
|---|---|---|
| 单进程、低频分配 | **HAMi** | 读用量亚微秒 vs 我们 1–2 ms |
| 多进程同容器多卡（vLLM TP=8） | **我们** | 按设备分段 vs 单一全局信号量 |
| 接近显存上限、OOM 频发 | **我们**（差距大） | 我们临界区不膨胀；HAMi 要在锁内读 N 次 `/proc` + 无门控日志 + 递归 |
| 高频小粒度分配（`expandable_segments:True`） | **都差，各有各的问题** | 我们是 25000 × NVML 枚举；HAMi 是 25000 × （全局锁 + 可能的 OOM 膨胀） |
| 大量短命 CUDA 子进程 | **我们** | 不探测 host PID（§11.3）；但 CLIENT 模式有 §11.4 那条 |
| 进程异常退出后的状态回收 | **我们** | NVML 是权威，无需清理死 slot |

**对「我们是否也受 #382 的性能影响」的回答：不受那条链的影响**——
它的三个放大器（单一全局信号量、OOM 路径在锁内读 `/proc`、无门控日志）我们都没有。
但我们有一条**自己的**、#382 里不存在的成本：每次分配一次 NVML 双枚举，
在 `expandable_segments:True` 这类高频小粒度分配下会累积到几十秒量级。

**建议（未实施，等拍板）**：给 `get_used_gpu_memory_by_device()` 加一个极短 TTL 的
结果缓存，只在同一把设备锁内复用。要点是它必须**在锁内**才安全——锁外复用会让两个
进程同时通过预算检查。这与 §9 否掉的那个 PID 缓存是两件不同的事：那个缓存的是
PID 归属（省 µs），这个缓存的是 NVML 枚举结果（省 ms），量级差三个数量级。
动手前应当先量一次真实的 NVML 枚举耗时与 `cuMemCreate` 频率，我没有 GPU 环境。

---

## 13. HAMi-core#382 第三轮：官方复现，前半段根因确认（2026-10-27 晚）

本节先更正 §12 的两处错误，再给结论。

### 13.1 两处更正

**更正一：§12.1 说"新证据推翻了 §11.2 的初始化开销推测"——这个判断是错的。**
正确的读法是：§11.2 那条链解释的是**编译器活动窗口**内的差异，它从来不声称解释
编译器退出之后的 3000 s。我把"不解释全部"误读成了"被推翻"。官方的新数据现在
**复现并确认了 §11.2 的机制**，见 13.2。

**更正二：§12.2 我提出的"OOM 路径在全局信号量内膨胀"推测，被 maintainer 的数据否了。**
moezdil 原话："no allocation failures in that window, and no futex/sem_wait in the
tp0 main thread samples, so lock contention in the allocator path looks unlikely."
那个窗口里**没有分配失败**，所以 `oom_check_impl` 的 OOM 分支根本没进，
`clear_proc_slot_nolock` 和无门控 `LOG_ERROR` 都不是这次的原因。
§12.2 作为代码事实仍然成立（那些放大器确实存在），但它**不是 #382 的机制**。

### 13.2 前半段根因已确认：postInit 的 host PID 探测在 byte-range 文件锁内

lixiaokun777 对**存活的 `nvrtc_compile` 子进程**采样，拿到了确切的等待栈：

```
postinit_file_lock_backoff(backoff_us=1000000, ...)
  → postinit_file_lock(lock_type=1)
  → lock_postinit
  → postInit
  → ensure_post_init
  → cuInit(Flags=0)                      # libvgpu.c:900
```

strace 同步显示：
`fcntl(3</usr/local/vgpu/CACHE_1.cache>, F_SETLK, ...) = -1 EAGAIN`，
**锁的持有者是另一个 `nvrtc_compile` 子进程**。

moezdil 在单张 3090 上复现，并给出线性标度：

| 并发进程数 | 有 HAMi | 无 HAMi |
|---|---|---|
| 8 | **5.6 s** | 1.0 s |
| 16 | **10.0 s** | 2.1 s |
| 32 | **21.5 s** | 4.7 s |

他的结论原话："the cost is the work under the lock: **set_task_pid runs at least
two host pid probes**"，并且降低 backoff "changed almost nothing"。

对照本地 `cdb5260` 的代码，这三件事全部对得上：

**(1) 为什么"至少两次 probe"**——`find_own_hostpid`（`src/utils.c:260`）的注释写明
"A candidate is accepted only after it matched two probes in a row"，代码里
`confirmed = n_known > 0` 在第一轮必然为 0，所以 `if (n == 1 && confirmed)`
最早只能在**第二轮**成立。每一轮 `nvml_probe`（`utils.c:240`）是：

```
list_compute_processes(before)        ← NVML 枚举
cuDevicePrimaryCtxRetain(&pctx, 0)    ← 建 primary context
list_compute_processes(during)        ← NVML 枚举
cuDevicePrimaryCtxRelease(0)
list_compute_processes(after)         ← NVML 枚举
```

即**锁内最少做 2×(3 次 NVML 枚举 + 一次 primary context 建销)**。两轮之间还有
`usleep(1000 + rand_r(&seed) % (2 * probe_us + 1000))`——**随机延迟最多两倍 probe
耗时，同样在锁内**。若 probe 约 150 ms，单次 postInit 持锁可达 ~600 ms；
32 进程串行即 ~19 s，与实测 21.5 s 吻合。

**(2) 锁是 byte-range 文件锁，不是我 §11.2 说的"节点全局"**——
`postinit_file_lock`（`multiprocess_memory_limit.c:751`）对
`region_info.fd` 的 `POSTINIT_FILE_LOCK_OFFSET`（0x40000000）处 1 字节下
`F_WRLCK`。文件是容器内的共享区 `CACHE_1.cache`，所以范围是**容器级**，
但容器内**所有**进程共享——包括那 177 个 `nvrtc_compile` 子进程。
我之前把 `hostpid_fallback_lock` 的 "node-wide" 注释套到了这条路径上，范围说错了；
但"所有进程排一把锁"这个结论不变，而且在 vLLM TP=8 这种单容器多进程场景下，
容器级和节点级的后果是一样的。

**(3) 为什么降低 backoff 没用**——`postinit_file_lock` 用非阻塞 `F_SETLK` 加指数退避
（翻倍到 `POSTINIT_FILE_LOCK_MAX_RETRY_US`，栈里 `backoff_us=1000000` 说明已到 1 秒上限），
且是 `for (;;)` **无超时的无限等待**（注释："A live owner is never bypassed: skipping
host PID detection also skips SM rate limiting"）。瓶颈在锁内工作量 × N，退避只是附加项。

**官方状态**：issue 仍 Open。前半段（编译器活动窗口，967.37 s 差异）机制已确认；
**编译器全部退出后的那 ~3000 s 仍未解释**，moezdil 明确说 "that part stays open"。
无 PR 合并；修复方向是 "deferring host pid detection from cuInit to first real gpu use"。

### 13.3 对我们的评估：这条已确认的瓶颈，我们结构上没有

§11.3 的结论现在价值最高，因为它对的正是官方刚确认的那条链：

| 已确认瓶颈的构成要素 | 本仓库 |
|---|---|
| 在 `cuInit` 里做 host PID 探测 | **不做**。host PID 由 manager 在容器外写入 `pids.config`，library 只读文件 |
| 探测要建销 primary context | 无此步骤 |
| 探测要连续两次 probe + 锁内随机 usleep | 无此步骤 |
| 全容器进程排一把 byte-range 文件锁 | 无此锁。设备锁按设备分段（`vgpu_<dev>.lock`）且只在分配路径，不在 `cuInit` |
| 短命子进程（`nvrtc_compile`）也要走一遍 | 不会：`load_necessary_data()` 只在 hook 命中 CUDA/NVML 符号时才跑，内容是 dlopen + 读配置，无锁无上下文 |

实测佐证（§11.3）：加载了库但不碰 CUDA 的子进程只付 **0.125 ms**。
moezdil 的修复方向是把探测**推迟**到首次真正用 GPU；我们的架构是**根本不探测**，
把这件事放在容器外的 manager 里——对这个问题是更彻底的解法。

**值得留意的是 moezdil 的线性标度本身**：8/16/32 进程 → 5.6/10.0/21.5 s。
它度量的是"N 个进程各做一次初始化、排一把锁"。我们对应的场景是 N 个进程各做一次
`load_necessary_data()`，彼此无锁、无上下文建销，所以这条曲线在我们这边是平的。

### 13.4 我们自己的那条成本仍然成立，且与 #382 无关

§12.5 记的劣势不受本轮更正影响，因为它与 #382 的机制无关：

**每次分配付一次 NVML 双枚举（约 1–2 ms），无结果缓存**，付这笔钱的 hook 有 13 处，
含 `cuMemCreate`（`cuda_hook.c:3939`，即 `expandable_segments:True` 的 VMM 路径）。
49 GiB 权重按 2 MiB granularity ≈ 25000 次 → 仅 NVML 枚举 25–50 s。
这是**高频小粒度分配专有**的风险，`expandable_segments:False` 下分配次数降到百量级即可忽略。

§12.6 的场景表仍然有效，只需把"接近上限、OOM 频发"那一行的依据改掉：
它原本引的是 §12.2 那条（已被否），但结论不变——我们 OOM 时只
`metrics_record_oom()` + 返回错误码，不抢额外锁、不遍历进程、不读 `/proc`、不递归，
这些代码事实本身没有被否。

### 13.5 仍然开放的问题

**编译器退出后的 ~3000 s，官方和我都没有解释。** 已知的排除项：
不是分配失败（那个窗口没有）、不是 allocator 路径的锁竞争（无 futex/sem_wait 采样）、
不是 postInit 锁（编译器已退出，不再有人抢）。

GDB 采样里 4 个含 HAMi 栈帧的样本落在 `cuCtxSynchronize`、free 路径、分配路径——
这指向**稳态 per-call hook 开销**，但缺少量化数据，而且 maintainer 也没给出结论。
我不打算在没有新证据的情况下再提一个推测：上一轮我提的 §12.2 就是这样被数据否掉的。

对我们的实际影响：这部分无论结论如何，都要等官方给出机制才能对照评估。
我们现在能做的只有 13.4 那条自己的成本，它独立于 #382 的结论。

---

## 14. 显存用量查询缓存：实施分析与阻塞点（2026-10-27）

§13.4 提的那条优化（给 `get_used_gpu_memory_by_device()` 加 TTL 结果缓存）做了完整
实施分析。结论：**设计可行，但有两个阻塞点，所以先只落了测量，缓存未实施。**
分支 `perf/library-memory-query-metrics`，commit `edc4bbd`。

### 14.1 调用点与已查证的约束

```
13 处内存 hook (cuMemAlloc / cuMemAllocManaged / cuMemAllocPitch /
  cuMemAllocAsync[_ptsz] / cuArrayCreate / cuArray3DCreate /
  cuMipmappedArrayCreate / cuMemCreate / cuMemAllocFromPoolAsync[_ptsz] /
  cuMemHostRegister / cuMemHostAlloc)
  └─ prepare_memory_allocation()                 cuda_hook.c:324
       └─ load_limited_memory_view()              cuda_hook.c:364
            ├─ lock_gpu_device(*host_index)       ← 全仓库唯一的取锁点
            ├─ get_used_gpu_memory()              ← 要缓存的对象
            │    └─ get_used_gpu_memory_by_device()  cuda_hook.c:2621
            │         ├─ _nvmlDeviceGetComputeRunningProcesses    ← 枚举 1
            │         ├─ _nvmlDeviceGetGraphicsRunningProcesses   ← 枚举 2
            │         └─ accumulate_used_memory() × 2（每 PID 判定归属）
            └─ get_used_gpu_virt_memory()         ← 读 mmap 区，不是同一量级
```

三条查证过的事实，决定了缓存的形态：

1. **`lock_gpu_device` 全仓库只在 `load_limited_memory_view` 一处调用** ——
   即只有**分配**路径取这把锁，**释放路径不取**。
2. **`used` 是本容器用量**，不是设备总用量：`accumulate_used_memory()` 只累加通过
   PID 归属判定的进程。
3. 释放路径**不维护任何用量计数器**（因为现在每次都重查 NVML，不需要账本）。

### 14.2 缓存必须解决的四个问题

| 问题 | 后果 | 必需的解法 |
|---|---|---|
| 同容器**其他进程**在 TTL 内分配，本进程的缓存看不到 | 漏算 → **超卖**（正确性） | 缓存必须放**容器内共享区**，在设备锁内读写。进程内缓存在这里是错的 |
| 本进程自己分配后未更新缓存 | 漏算 → 超卖 | 锁内 `delta += size` |
| 释放不走锁，缓存只增不减 | 偏大会累积 → 最终误判 OOM | TTL 过期重查 NVML 纠正 |
| 进程崩溃，它的 delta 残留在共享区 | 偏大 | 同上，由 TTL 纠正 |

所以正确的设计是：**容器内共享区存 `(stamp_ns, used_at_stamp, delta_since)`，
全部在设备锁内读写，TTL 过期查 NVML 重置。** 这比 HAMi 的架构安全
——NVML 仍是权威，共享区只是 TTL 内的加速，崩溃残留会自我纠正，
不需要 `clear_proc_slot_nolock` 那种在热路径锁内读 `/proc` 的清理逻辑（§13.2）。

### 14.3 两个阻塞点

**阻塞一：缺前提数据。** `1–2 ms` 是我从 `lock.c:39` 的注释推出来的，**不是实测**；
`cuMemCreate` 的真实频率同样没有数据。若 NVML 双枚举实际只要 0.2 ms，
25000 次也只有 5 s，不值得这个改造。我没有 GPU 环境，无法自测。

**阻塞二：需要新建一块共享区。** 不能把字段加进 `device_t` ——
`_Static_assert(sizeof(device_t) == CACHELINE_SIZE)` 加上 `offsetof` 断言和
Go 镜像（`pkg/config/vgpu/vgpu_config.go`），改它是 ABI 破坏。新建共享区要做：
文件创建、mmap、绕 umask 的 fchmod、session path 路由、fork 处理、崩溃恢复、测试。
这是中等工作量，而且新增一处共享状态——按 `metrics.h` 自己的话，
"This library's correctness lives in mmap'd shared regions, locks and atomics,
where a wrong offset is a silent memory error rather than a failed assertion"。

为一个未量化的收益引入超卖风险，不是好交易。所以先解除阻塞一。

### 14.4 已落地：测量（`edc4bbd`）

`metrics_memory_query_begin()` / `metrics_record_memory_query()`，插桩在
`load_limited_memory_view()`（有 `host_index`、在设备锁内、分配路径唯一一处），
按设备记 count / total / mean / max，用 `metrics.c` 既有的 powers-of-two 限流打 INFO。
门控沿用 `should_collect_metrics()`，关闭时不读时钟。

三处防护各有测试：0 作为"未测量"哨兵、`now <= start_ns` 的下溢丢弃、
索引越界校验。新增 `test/nogpu/test_memory_query_metrics.c`，它从 INFO 行里
解析 `mean_ns=` 回来断言——**第一版只断言"不崩溃"，而下溢恰恰不崩溃，
移除保护后测试仍 PASS，于是重写**；现在移除保护会报
`mean_ns=9223372036354783637` 并失败。

### 14.5 怎么用这组数据决定

真机跑一次工作负载，`LOGGER_LEVEL=3`，然后看日志里的

```
memory query metrics device=N count=... total_ns=... mean_ns=... max_ns=...
```

判据建议：

| 观察 | 结论 |
|---|---|
| `count` 只有几百、`mean_ns` 再大也无所谓 | 不做缓存。这是 `expandable_segments:False` 的常态 |
| `count` 上万 **且** `mean_ns × count` 占启动时间可观比例 | 值得做 §14.2 的共享区缓存 |
| `mean_ns` 远小于 1 ms（比如 0.2 ms） | 即使 count 上万也先别做，收益被高估了 |
| `max_ns` 比 `mean_ns` 高一两个数量级 | 长尾来自设备上进程数波动；此时缓存的收益主要在削峰，而削峰正是 `lock.c` 退避参数关心的 |

两边都要测：`expandable_segments` 开和关各跑一次，差异就是 VMM 路径的真实代价。

---

## 15. 真机实测：缓存方案正式否决（2026-10-27，RTX 2080 Ti）

§14 的阻塞一（缺前提数据）已解除。用户提供了一台 GPU 服务器，
在上面把需要的数字全部测了出来，结论是 **§12.5 / §13.4 / §14 里我给的
「25–50 秒」高估了一个数量级，缓存方案正式否决。**

环境：1×RTX 2080 Ti 11GB、驱动 595.84、CUDA 13.0、Ubuntu 24.04、内核 7.0.0-28。

### 15.1 NVML 双枚举的真实耗时

和 `get_used_gpu_memory_by_device()` 一样的调用方式（`MAX_PIDS=1024` 栈缓冲，
先 compute 后 graphics），1500–2000 次/轮 × 5–7 轮取最小：

| 设备上 compute 进程数 | Compute | Graphics | **双枚举合计** |
|---|---|---|---|
| 0 | 9.01 µs | 16.32 µs | **25.47 µs** |
| 1 | 19.40 µs | 19.23 µs | **38.79 µs** |
| 2 | 22.79 µs | 21.90 µs | 44.78 µs |
| 4 | 33.95 µs | 31.02 µs | 64.54 µs |
| 8 | 63.83 µs | 57.98 µs | **122.51 µs** |

斜率约 **12 µs/进程**，基线 ~25 µs。外推 32 进程约 410 µs、64 进程约 790 µs ——
**任何现实进程数下都远低于 1 ms。**

一次性成本：`nvmlInit_v2` 9.4 ms、`nvmlDeviceGetHandleByIndex_v2` 7.1 ms。

**所以 `lock.c:39` 的 "~1-3ms" 注释本身没错 —— 错的是我把 NVML 当成了其中的主项。**
那 1–3 ms 的主体是「真实驱动分配」，见 15.2。

### 15.2 驱动分配本身的耗时，以及 NVML 的占比

VMM granularity 实测 **minimum = recommended = 2048 KiB**，确认了 2 MiB 的假设。

| 操作 | 耗时 |
|---|---|
| `cuMemCreate`（2 MiB，**我们 hook 的**） | **9.75 µs** |
| `cuMemAddressReserve` | 0.56 µs |
| `cuMemMap` | 0.54 µs |
| `cuMemSetAccess` | 32.72 µs |
| VMM 四步合计 | 43.57 µs |

`cuMemAlloc` 随块大小的变化是决定性的：

| 块大小 | `cuMemAlloc` | `cuMemFree` | NVML 双枚举占 alloc |
|---|---|---|---|
| 2 MiB | 57.44 µs | 78.52 µs | **67.1%** |
| 8 MiB | 40.91 µs | 245.43 µs | 94.2% |
| 32 MiB | 48.40 µs | 874.40 µs | 79.6% |
| 128 MiB | 62.33 µs | 1297.17 µs | 61.8% |
| **512 MiB** | **1048.56 µs** | 2004.34 µs | **3.7%** |
| **1024 MiB** | **11801.50 µs** | 2158.02 µs | **0.3%** |

转折点在 512 MiB：小块时驱动几乎不干活（40–60 µs），NVML 占大头；
大块时驱动真正要做工作（1–12 ms），NVML 可忽略。
**PyTorch caching allocator 在 `expandable_segments:False` 下按大块申请，
正好落在 NVML 可忽略的那一侧。**

### 15.3 端到端：我们 library 的真实查询成本

把 `library/` 手工编译成 `.so`（远程没有 cmake，`gcc -shared -fPIC src/*.c` 即可），
`LD_PRELOAD` 跑 256 次 2 MiB 分配，读 §14.4 刚加的 metrics：

```
memory query metrics device=0 count=1   total_ns=5653328  mean_ns=5653328 max_ns=5653328
memory query metrics device=0 count=256 total_ns=44068501 mean_ns=172142  max_ns=5653328
```

- **首次 5.65 ms**（冷启动：NVML 初始化、dlopen、设备映射都算在第一次里）
- **稳态 ~153 µs**（后 128 次：(44068501−24473365)/128）

分解（同机，真实 CUDA 进程，**48 个 fd**）：

| 步骤 | 耗时 | 占稳态 153 µs |
|---|---|---|
| `kill(pid,0)` | 0.37 µs | 0.2% |
| ns 比对（自身 inode 已缓存，即 F6） | 2.10 µs | 1.4% |
| **fd 探测**（`gpu_device_fd_in_process`） | **19.50 µs** | 12.7% |
| 三步判定合计 | 22.33 µs | 14.6% |
| **NVML 双枚举** | **38.56 µs** | **25.2%** |
| 其余（设备索引映射含 mutex、handle 获取、两次 accumulate 遍历） | ~69 µs | ~45% |

两点值得记：

1. **本地无 GPU 时测的三步判定 22.8 µs，真机是 22.33 µs** —— 几乎一致，
   说明 §9 那套本地测量是可靠的，之前基于它做的判断不用重做。
2. **NVML 只占我们查询成本的 1/4。** 缓存 NVML 结果最多省 25%，
   剩下 3/4 是我们自己的代码（其中最大一块是设备索引映射和遍历，不是 PID 判定）。

### 15.4 结论：否决

49 GiB 权重在不同分配粒度下，我们的查询累计开销（按稳态 153 µs）：

| 分配粒度 | 次数 | 我们的累计开销 | 其中 NVML 部分 |
|---|---|---|---|
| 2 MiB（`expandable_segments:True`） | 25088 | **3.84 s** | 0.97 s |
| 8 MiB | 6272 | 0.96 s | 0.24 s |
| 128 MiB | 392 | 0.06 s | 0.015 s |
| 512 MiB（caching allocator 常态） | 98 | **0.015 s** | 0.004 s |

- 我原来说的 **25–50 秒 → 实际最坏 3.84 秒**，高估 6–13 倍；
- `expandable_segments:False`（#382 的实际配置）下是 **0.015–0.06 秒**，彻底可忽略；
- 而缓存只能解决其中 NVML 那 25%，即最坏场景下约 **0.97 秒**。

**为 0.97 秒的最坏收益，去新建一块共享区、并承担 §14.2 那个超卖风险，不是好交易。
缓存方案否决。** §14.3 的阻塞二（需新建共享区）因此也不必再评估。

### 15.5 顺带真机验证了两件此前标注为风险的事

**(1) fd 探测在真实 CUDA 进程上确实命中。** 这是 §8.2(3) / §10.1 一直标注的
「无 GPU 环境无法验证」的那一条。真实 CUDA 进程 48 个 fd，`fd_probe` 返回 0（命中），
VERBOSE 日志确认归属判定生效：

```
process id 4911 use gpu memory: 163577856
process id 1351 use gpu memory: 4677632
total used memory: 168255488
```

**(2) 整条限额链精确生效。** `CUDA_MEM_LIMIT_0=200MiB` 下分配到第 19 个
2 MiB 块时返回 OOM —— 因为 `used` 正确计入了 CUDA context 本身约 150 MB 的占用
（150 + 19×2 + 4.6 ≈ 193 MB，第 20 个会越界）。

**(3) §14.4 新加的 metrics 代码真机可用**，输出就是上面那两行。

### 15.6 调试时踩到的一个坑（非缺陷，但值得记）

`load_controller_configuration()` 在 `mmap_file_to_config_path()` 失败后会用环境变量
建配置并**写回** `vgpu.config`。所以第一次用不完整的环境变量跑过之后，
那个文件就固化了，后续再改环境变量**不生效** —— 我因此两次误判限额没工作。
生产上配置由 device-plugin 管理，这是预期行为；但手工调试时要记得先
`rm -f /etc/vgpu-manager/config/vgpu.config`。

### 15.7 仍然开放

#382 后半段那 ~3000 s 官方仍未解释（§13.5），与本节无关。
本节否决的只是「给显存用量查询加缓存」这一项优化。
