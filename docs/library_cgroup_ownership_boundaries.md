# library cgroup 归属判定：以 k8s 容器形态为准绳的边界梳理

本文记录 `check_device_pid_in_proc_cgroup_path()`（`library/src/cuda_hook.c`）与
`cgroup_path_is_ours()`（`library/include/cgroup_match.h`）这条判据的真机实测行为。

**判定准绳**：library 运行在 **k8s 容器**里，不是裸容器。一个形状要不要在代码里
处理，先问"标准 k8s 容器能不能产生它"。裸 docker 在特权 / 可写 cgroupfs / 手工
`unshare` 下能造出一批形状，多数在 k8s 下不可达 —— 这些**只在本文记录，不进代码**，
避免为大而全的兼容性堆判据复杂度。

实测环境：Ubuntu 24.04 / kernel 7.0.0-28 / Docker 29.1，无 GPU（只验证 cgroup
判据本身，不涉及 NVML）。三种 cgroup 布局均实测过：
cgroup v2 unified（默认）、systemd hybrid（`systemd.unified_cgroup_hierarchy=0`）、
legacy v1（再加 `systemd.legacy_systemd_cgroup_controller=yes`）。

## 1 判据在什么时候被调用

两个入口，共用同一个函数：

| 入口 | 读哪个 proc | 适用模式 | 语义 |
|---|---|---|---|
| `check_device_pid_in_host_cgroup_container()` | `.host_proc/<pid>/cgroup` | CGROUPV1 / CGROUPV2 | NVML 返回宿主命名空间 pid（CUDA 13 之前） |
| `check_device_pid_in_local_container_pid()` 的 UNKNOWN 回退 | 容器本地 `/proc/<pid>/cgroup` | OPEN_KERNEL | NVML 返回容器命名空间 pid（CUDA 13 之后） |

第二个入口只在 `device_pid_in_same_container()` 返回 UNKNOWN 时走到，即
`/proc/<pid>/ns/*` 被 `PTRACE_MODE_READ` 拒绝（目标 pid 属于另一个 uid）。它在
CLIENT / SESSION / CGROUPV1 / CGROUPV2 各分支里都作为 openKernel 回退存在
（`accumulate_used_memory()` 与 `get_used_gpu_memory_by_device()` 各四处，
例如 `cuda_hook.c` CLIENT 分支的 `!matchClientMode && openKernelMode && ...`）。

## 2 k8s 容器里实际会出现的形状

k8s 侧的约束（决定了判据只需要覆盖很窄的一组形状）：

- **kubelet 把一个容器的全部进程放在同一个 cgroup**，不在容器 cgroup 下再建子 cgroup。
  路径形如 `/kubepods.slice/kubepods-<qos>.slice/kubepods-<qos>-pod<uid>.slice/cri-containerd-<cid>.scope`
  （systemd driver）或 `/kubepods/<qos>/pod<uid>/<cid>`（cgroupfs driver）。
- **容器内 `/sys/fs/cgroup` 默认 ro**（实测非特权容器建子 cgroup 与迁移都失败），
  可写需 privileged；k8s 没有 cgroup delegation 的受支持开关。
- **cgroupns 由 CRI 决定，Pod spec 里没有开关**：containerd 在 cgroup v2 上为**每个
  容器**unshare 一个 cgroup namespace，ns 根 = 该容器自己的 cgroup（**不是 Pod 级**，
  已实测，见第 7 节）；在 v1 上用 host。
- **没有业务进程跑在根 cgroup 里**。

于是 k8s 容器里只有两种形态，且**本容器的每个进程与 self 读到完全相同的字符串**：

```
cgroup v2 + cgroupns=private
  self / 本容器进程     "/"
  同 Pod 其他容器       "/../<other-cid>.scope"
  其他 Pod / 宿主进程   "/../../..."

cgroup v1 + cgroupns=host
  self / 本容器进程     "/kubepods.slice/.../cri-containerd-<cid>.scope"
  其他容器 / 宿主进程   不同路径
```

**现行判据在这两种形态下全部正确**，包括 `..` 修复之前的版本。
`hostPID=true`、`shareProcessNamespace=true`、`hostIPC=true` 的各种组合都实测过
（24 种容器配置 / 144 次判定，0 不符）：它们只改变容器能看见哪些 pid，而判据靠
cgroup，所以宿主进程与兄弟容器在全部组合下都被正确排除。

## 3 内核渲染规则（实测）

`/proc/<pid>/cgroup` 第三字段由 `proc_cgroup_show()` 相对**读取者**的 cgroup 命名
空间根渲染，与读的是哪个 procfs 挂载无关。越出读取者 ns 根时内核用 `..` 段表示
向上的层数。两条可当硬事实用的性质：

- **`..` 段不可伪造**：`mkdir ".."` 不可能，所以恰好等于 `..` 的路径段的唯一生产者
  就是内核的越界渲染。（`..foo` 是合法 cgroup 名，判据必须逐段比较而非子串搜索。）
- **cgroupns=host 下任何路径都不会出现 `..`**，因为没有边界可越。所以"看到 `..`"是
  private 的正证据，"没看到"什么都不能证明。

裸容器下另外采到的形状（k8s 不产生，仅备查）：
`/nested`（ns 根下的子 cgroup）、`/..`（嵌套 cgroupns 的外层）、
`/../../outside`（被搬出容器 cgroup 的 self）、`/`（cgroupns=host 下宿主根 cgroup 进程）。

## 4 cgroup v1 的三种布局（实测，决定 key 顺序是否有意义）

`read_cgroup` 按 key 列表依次尝试，**命中即停**。key `""` 匹配 cgroup2 的
`0::<path>` 行，`"memory"` 匹配 v1 memory 层级。

| 布局 | 有 `0::` 行? | 容器的 `0::` 行内容 | `GetCompatibilityMode` 归类 |
|---|---|---|---|
| v2 unified | 有，唯一一行 | 容器自己的路径 | CGROUPV2 |
| systemd hybrid | **有** | **每容器独立**：`0::/docker/<id>`（runc 在 unified 层级也建组） | **CGROUPV2**（`IsCgroup2HybridMode` 分支） |
| legacy v1（cgroup2 从未挂载） | **没有**（实测 0 条） | — | CGROUPV1 |

关键后果：**key 顺序在任何真实布局下都不影响结果**。hybrid 的 `0::` 行是每容器
独立的，所以先读它是对的；而 CGROUPV1 位只会在 legacy 布局下被置上，那里压根没有
`0::` 行，`""` 必然落空、自动落到 `"memory"`。

内核机制：`proc_cgroup_show()` 对 cgroup2 默认根有 `if (root == &cgrp_dfl_root &&
!cgrp_dfl_visible) continue;`，而 `cgrp_dfl_visible` 只在 cgroup2 被挂载时置位。

`a7a50f7`（CGROUPV1 下改用 `{"memory", ""}`）在上表的归类下曾是死码：CGROUPV1 位只在
legacy 布局置上，而那里没有 `0::` 行。**`c76b450` 把 hybrid 从 v2 归类里摘掉之后它就生效了**
（`GetCompatibilityMode` 与 `GetClaimCommonContainerEdits` 两处都删了 `IsCgroup2HybridMode`
分支，hybrid 落到 `default: CGroupv1Mode`）。于是 hybrid 节点上先读 memory 层级。

**key 顺序还有一项与正确性无关的实测收益：少一次整文件扫空。** `read_cgroup` 每次
调用都 `rewind` 并重扫，key `""` 在 v1 形状（14 行，无 `0::` 行）上要扫完整个文件才失败，
然后 `"memory"` 再扫一遍。实测（legacy v1 形状，memory 在第 2 行，-O2，50 万次）：

| key 顺序 | 每 pid 解析耗时 |
|---|---|
| `{"memory", ""}` | **297 ns** |
| `{"", "memory"}` | **1660 ns**（+1363 ns，5.6×） |

那次扫空的整文件遍历约 1360 ns，而每 pid 的 `open + read + 解析` 总共 3800–3900 ns，
所以这一项省掉约 **35%** 的 per-pid cgroup 开销 —— 比判据函数本身的差异（几十 ns）
大一个数量级。hybrid 上 `0::` 行通常在文件末尾，memory 在靠前位置，同样是 memory 优先更省；
v2 unified 只有一行，`""` 立即命中，不受影响。

构造出过算（而非只是多扫一遍）需要人为把容器进程迁进共享的 unified 组或 unified 根 ——
在 k8s 下不可达；但省下的那次重扫是无条件的。

## 5 已修缺陷

| 编号 | 方向 | 形状 | k8s 可达 | 状态 |
|---|---|---|---|---|
| F-a | 漏算 | cgroupns=private 下 `self == "/"`，同容器进程在子 cgroup（`/nested`）被当成外人 | **否**（kubelet 不建子 cgroup，cgroupfs ro） | 已修（ns 根上改用"无 `..` 段"判定） |
| F-b | 漏算 | `strstr(path, "/..")` 把合法命名 `..foo` 误判为逃逸 | 否（cgroup 名是 hex id / kubepods-*） | 已修（逐段比较）。这是 F-a 代码自身引入的 bug |
| F-c | 过算 | cgroup v1 下 key 顺序让 unified 行决定归属 | 否（构造才能触发） | 已提交 `a7a50f7`；`c76b450` 让它在 hybrid 上生效，**保留** —— 另有实测的 5.6× 解析收益（见第 4 节） |

F-a / F-b 虽然 k8s 不可达，但改动已在、无额外复杂度、且 F-b 是 F-a 的配套修正，保留。
`c76b450` 另外把实测最快的 `strcmp(self, pid) == 0` 快路径加进了 `cgroup_path_is_ours`
—— k8s 下这是唯一会出现的形状，6.8 ns，比改动前的 10.6 ns 还快。

## 6 记录但不修的边界

### B1 过算：cgroupns=host 下位于宿主**根 cgroup** 的进程
函数开头的短路 `pid_cgroup == "/"` 即判为自己的；cgroupns=host 下宿主根 cgroup 的
进程正好渲染成 `/`。cgroup v1 上更容易触发（未被放进任何 memory cgroup 的进程读出来
就是 `2:memory:/`）。cgroupns=private 下**不存在**（渲染为 `/../..`，被正确排除）。
**k8s 不可达**：kubelet 把所有 Pod 放进 `kubepods.slice`，根 cgroup 里不跑业务进程。

### B2 漏算：self 被放进子 cgroup，同容器进程在兄弟子 cgroup
`self = /a`、同容器 GPU 进程 = `/b`，后代判定两边都不满足 → 排除。v2 / v1 均实测重现。
需要容器内 cgroupfs 可写（privileged）**且**有东西在容器内建子 cgroup 并分散进程
（容器内跑 systemd、DinD、自建 cgroup 细分的框架）。**k8s 不可达**。
注：cgroupns=host 下此形状**无法**用 cgroup 判据修复 —— 把祖先也算进来会把宿主
cgroup 一起算进来，是过算。

### B3 漏算：self 已逃逸出容器 cgroup，同容器进程在子 cgroup
需 B2 的条件**叠加**外力：实测容器自己迁不出 ns 根（`nsdelegate` 拦住，返回 ENOENT，
特权也不行），只有宿主 root 从外部搬运、或把宿主 `/sys/fs/cgroup` 以 rw 绑挂进容器
（绕过 nsdelegate，因为它是挂载实例级选项）才能做到。**k8s 不可达**。
相邻的两个形状实测不成问题：self 逃逸而同容器进程留在容器根时靠短路判对；宿主 root
把**整个容器**搬走时 self 与 pid 同步变化、相等，靠后代规则判对（子进程继承 cgroup）。

### B4 漏算：容器内自己 `unshare --cgroup`（嵌套 cgroup 命名空间）
嵌套 ns 内 self 渲染为 `/`，外层同容器进程渲染为 `/..`。需特权 + 刻意 unshare，自伤。

### B5 无解：GPU 进程被刻意迁出容器 cgroup 子树
任何基于 cgroup 的判据都认不回来。但拿到跨 delegation 边界 cgroup 写权限的容器，本来
就能直接写 `memory.max` 或摘掉 `LD_PRELOAD`，**不构成新的绕过面**。

## 7 B6 / B7：两条 k8s 相关的前提

### B6 NVML 返回的 pid 属于哪个命名空间（部分已知，一项待验）

按项目方的说明：

- **CUDA 13 之前**：返回**宿主命名空间**下的全部 pid。
- **CUDA 13 之后**：`nvmlDeviceGetComputeRunningProcesses` 等显存接口返回**当前容器
  命名空间**的 pid，并**过滤掉**非本容器的条目。
- **进程利用率接口**（`nvmlDeviceGetProcessUtilization`）不同：CUDA 13 之后会把 pid
  **转换**为当前容器命名空间的 pid，但**不过滤**非本容器的条目。

前两条与现有的双入口设计吻合（`.host_proc` 对应宿主 ns 语义，本地 `/proc` 对应容器 ns 语义）。

第三条意味着利用率路径（`get_used_gpu_memory_by_device()` 里的 `processes_sample`
循环）会收到不属于本容器的条目，必须靠归属判定剔除。**同 Pod 且 `shareProcessNamespace`
的情形已实测正确**（见 B7）：兄弟容器的进程在本地 pid ns 里有合法 pid，归属判定把它排除。

**待验的一项**：对**不在**本容器 pid namespace 里的进程（例如非本 Pod 的容器），
利用率接口在 `pid` 字段里填什么值。
- 若填 `0` —— 安全：`check_device_pid_in_local_container_pid()`、
  `check_device_pid_in_ordered_container_pids()`、`check_device_pid_in_proc_cgroup_path()`
  都有 `device_pid == 0` 的早退。
- 若原样保留宿主 pid —— 有风险：该数值可能与本容器某个本地 pid 数值相同，于是被判成
  自己的进程，`user_current` 多算 → SM 限流误触发（方向是**过算/误限流**，不是配额逃逸）。
  私有 pid ns 下本容器 pid 是小数值、宿主 pid 通常是几千，碰撞需要容器长期存活、pid
  号增长到重叠区间。

这一条需要 GPU 节点实测（本文档的机器无 GPU；原 GPU 节点凭据已失效）。

### B7 containerd 的 cgroupns 根是容器还是 Pod —— **已实测：容器级**

在测试机上装 crictl 并走**真实 CRI 路径**（containerd 2.2.1 的 CRI 插件 +
kubelet 风格 `cgroup_parent = /kubepods/besteffort/pod<uid>`），建一个 Pod 沙箱 + 两个
业务容器，`namespace_options.pid = POD`（即 `shareProcessNamespace: true` 的形态）。

宿主视角：

```
c1       0::/kubepods/besteffort/podb7uid0001/<c1-id>
c2       0::/kubepods/besteffort/podb7uid0001/<c2-id>
sandbox  0::/kubepods/besteffort/podb7uid0001/<sandbox-id>
```

c1 容器内视角：

```
self                 0::/
cgroup.type 存在?    是
兄弟容器 c2          0::/../<c2-id>
sandbox (pause)      0::/../<sandbox-id>
宿主 init            0::/../../../../init.scope     (四层: kubepods/besteffort/pod/container)
```

**ns 根就是容器自己的 cgroup**，兄弟容器与 pause 都落在 ns 根之外（带 `..`），被判据
正确排除；自己被正确计入。第 2 节形状表的前提成立，**Pod 级 ns 根带来的过算风险不存在**。

同一轮顺带实测了生产里 cgroup 回退真正存在的理由：让 c1 以 `run_as_user: 1000` 运行、
c2 以 root 运行，共享 pid ns。c1 看得到 c2 的本地 pid，而 `stat()` 读
`/proc/<c2-local-pid>/ns/pid` 返回 **EACCES (errno 13)** → `PID_IN_CONTAINER_UNKNOWN`
→ 落到 cgroup 回退读本地 `/proc`，渲染为 `/../<c2-id>` → 正确排除。经 `.host_proc` 判
同一个容器也得到同样结论。

### B7 续：`shareProcessNamespace: true` 下三个容器互不串账（已实测）

同一个 Pod 沙箱里起三个容器、`namespace_options.pid = POD`，其中 a / b 以 root 运行、
c 以 `run_as_user: 1000` 运行。pid namespace 确认共享（三者 `pid:[4026532438]` 相同），
而 mnt / cgroup namespace **每容器独立**：

```
a  pid:[4026532438]  mnt:[4026532439]  cgroup:[4026532440]  uid 0
b  pid:[4026532438]  mnt:[4026532441]  cgroup:[4026532442]  uid 0
c  pid:[4026532438]  mnt:[4026532443]  cgroup:[4026532444]  uid 1000
```

因为 pid ns 共享，每个容器的 `/proc` 里都能看到另外两个容器的主进程（本地 pid
a=8 / b=15 / c=22）。用原样复现的 `device_pid_in_same_container()` 逐对判定：

| 判定方 | 目标 a (pid 8, root) | 目标 b (pid 15, root) | 目标 c (pid 22, uid 1000) |
|---|---|---|---|
| 在 a 内（uid 0） | **YES** mnt 同 + cgroup 同 | **NO** mnt 异 | UNKNOWN (EACCES) |
| 在 b 内（uid 0） | **NO** mnt 异 | **YES** mnt 同 + cgroup 同 | UNKNOWN (EACCES) |
| 在 c 内（uid 1000） | UNKNOWN (EACCES) | UNKNOWN (EACCES) | **YES** mnt 同 + cgroup 同 |

- **同 uid 的兄弟容器被 mnt 腿直接判掉**（`mnt` 是 `g_ns_names` 的第一条，也是承重的那条）：
  共享 pid ns 不改变 rootfs，mnt inode 必然不同 → NO → 不计入。
- **跨 uid 的兄弟容器落到 UNKNOWN**，由 cgroup 回退决定；回退读容器本地 `/proc`，
  兄弟容器渲染为 `/../<other-cid>`（带 `..`）→ 排除。同一轮已实测（c1 uid 1000 判
  root 的 c2，经本地 `/proc` 与经 `.host_proc` 都得到排除）。
- 每个容器判自己的进程都是 YES。

结论：**`shareProcessNamespace: true` 下三个容器各计各的，不会串账。** 两条路径
（ns 比对 / cgroup 回退）都验到了，没有一格落在"算成别人的"或"把别人算成自己的"。

一处测量更正：`stat(1)` 命令默认是 **lstat**，量到的是 procfs 符号链接自身的 inode
（每个 pid 不同），不是 nsfs 的命名空间 inode。用 C 的 `stat()` 系统调用（跟随符号链接）
实测：同容器内 `self` / `pid 1` / 子进程的 `ns/pid` 均为 `4026532439`、`ns/mnt` 均为
`4026532436`，完全一致。库用的是 `stat()`，判定正确；是 shell 探针误导了我。

## 8 评估过但不实施的判据改造

曾考虑把判据换成"规范化 `..` + 以 cgroup 命名空间根为锚"：

```
规范化两边路径为 (up, names[])   —— ".." 逐段抵消, 抵消不掉的记入 up
1. up(self) == up(pid) 且 names(self) 是 names(pid) 的段前缀   -> 是自己的
2. 已证实处于 private cgroupns 且 up(pid) == 0                 -> 是自己的
3. 否则                                                        -> 不是
```

规则 2 的 private 证据只取免费来源（self 自己的渲染里有 `..`；本进程此前读到过任何
带 `..` 的 pid 路径 —— CGROUPV1/V2 模式读 `.host_proc` 时天然提供），拿不到证据时沿用
今天的行为，因此零回退、零新增 syscall。

**结论：不实施。** 它修的是 B1 / B2 / B3，而这三条在标准 k8s 容器里都不可达；换来的是
判据从 18 行变成约 60 行、多一个入参、多一处进程级状态。按 k8s 优先的取向，这笔复杂度
买不到正确性。

实测开销（同机，-O2，200 万次），留档供将来需要时参考：

| 判据 | 混合形状 | self == pid（k8s 下的唯一形状） |
|---|---|---|
| 现行 | 8.6 – 10.6 ns | 10.6 ns |
| 规范化（数组） | 54 – 57 ns | 161 ns |
| 规范化（流式无数组） | 104 – 106 ns | — |
| 规范化 + `strcmp` 快路径 | 39 ns | 6.8 ns |

对照：这条链路里**每个 pid 本来就要做一次** `open + read + 解析 /proc/<pid>/cgroup`，
实测 3800 – 3900 ns，所以判据本身无论哪个版本都在噪声里。流式实现比数组版慢一倍
（多走一遍字符串 + 逐字节比对），已否决。若另起探测判 private 需 3.6 µs/进程一次。

## 9 全量真机矩阵（2026-10-28，930 条判定）

### 9.1 规模与方法

| 维度 | 取值 | 怎么变 |
|---|---|---|
| cgroup 布局 | v2 unified / systemd hybrid v1 / legacy v1 | grub `systemd.unified_cgroup_hierarchy=0` [+ `legacy_systemd_cgroup_controller=yes`] + 重启 |
| 容器入口 | **真实 CRI（containerd 2.2.1 CRI 插件）** / Docker 29.1 | 两种运行时都跑，判据不依赖任一运行时 |
| cgroup driver | systemd / cgroupfs | containerd `SystemdCgroup` + 对应的 `cgroup_parent` 形态 |
| cgroupns | private / host | CRI 不暴露此开关（containerd 自己决定：v2→private、v1→host），用 Docker `--cgroupns` 显式扫 |
| pid ns | CONTAINER（默认）/ POD（`shareProcessNamespace`）/ NODE（`hostPID`） | CRI `namespace_options.pid`；Docker `--pid` |
| ipc ns | POD / NODE（`hostIPC`） | CRI `namespace_options.ipc`；Docker `--ipc` |
| 子 cgroup 嵌套 | 判定方在容器 cgroup 根 / 在子 cgroup | 特权容器内 `mkdir` + 迁移 |
| 嵌套容器 | DinD（`docker:dind` 内再起容器） | 特权容器 |

判定目标（每个配置都判）：本容器同 uid 进程、本容器跨 uid 进程、自己、同 Pod 兄弟容器、
另一个 Pod 的容器、sandbox(pause)、宿主普通进程、宿主**根 cgroup** 进程、子 cgroup 里的
同容器进程、DinD 内层容器进程。判定方分别以 root 和 uid 1000 各跑一遍。

探针用仓库原文 `include/cgroup_match.h`，`read_cgroup()` / `device_pid_in_same_container()` /
`check_device_pid_in_proc_cgroup_path()` 按 `util.c` / `cuda_hook.c` 原样复现（含 `c76b450`
的 key 顺序与 `strcmp` 快路径）。每条记录同时给出三种口径：**本地腿**（openKernelMode：
ns 比对，UNKNOWN 落 cgroup 回退）、**`.host_proc` 腿**（CGROUPV1/V2 模式的主判据）、
**合成**（主判据不成立则落 openKernel 兜底）。

### 9.2 总体结果

| cgroup 布局 | 判定记录 | 三口径判定数 | 不符 |
|---|---|---|---|
| v2 unified | 366 | 924 | 30 |
| systemd hybrid v1 | 377 | 938 | 86 |
| legacy v1 | 187 | 476 | 50 |
| **合计** | **930** | **2338** | **166** |

166 条不符只有**两个成因**：

| 成因 | 条数 | 方向 |
|---|---|---|
| 宿主**根 cgroup** 进程被 `pid == "/"` 短路认领（B1） | 162 | 过算 |
| DinD 内层容器进程被排除 | 4 | 漏算 |

**本容器自己的进程（含跨 uid、含子 cgroup 里的）在全部 930 条里 0 漏算**；
**兄弟容器 / 其他 Pod / sandbox / 宿主普通进程在全部 930 条里 0 过算**。

### 9.3 按入口与目标类拆解

| cgroup | 入口 | 目标类 | 本地腿 | `.host_proc` 腿 | 合成 |
|---|---|---|---|---|---|
| v2 | **CRI** | 本容器（应计入） | 72/72 | 48/48 | 72/72 |
| v2 | **CRI** | 外部（须排除） | 72/72 | 120/120 | 120/120 |
| v2 | Docker | 本容器 | 72/72 | 48/48 | 72/72 |
| v2 | Docker | 外部 | 20/24 | 84/96 | 84/96 |
| v1 | **CRI** | 本容器 | 72/72 | 48/48 | 72/72 |
| v1 | **CRI** | 外部 | 64/72 | 96/120 | 96/120 |
| v1 | Docker | 本容器 | 72/72 | 48/48 | 72/72 |
| v1 | Docker | 外部 | 20/24 | 84/96 | 84/96 |

v2 的 CRI 入口（= 真实 k8s 路径）**全部干净**；v1 的 CRI 入口出现过算，因为
**containerd 在 cgroup v1 上用 cgroupns=host**，B1 于是可达。

### 9.4 四个维度不参与判定（数据证明）

| cgroup | 维度 | 取值 | 符合 | 过算 | 漏算 |
|---|---|---|---|---|---|
| v2 | **cgroupns** | private | 86 | **0** | 0 |
| v2 | **cgroupns** | host | 74 | **12** | 0 |
| v2 | pid | CONTAINER / POD / NODE | 64 / 64 / 64 | 0 / 0 / 0 | 0 |
| v2 | ipc | POD / NODE | 96 / 96 | 0 / 0 | 0 |
| v2 | driver | cgroupfs / systemd | 96 / 96 | 0 / 0 | 0 |
| v1 | **cgroupns** | private | 89 | **0** | 0 |
| v1 | **cgroupns** | host | 77 | **12** | 0 |
| v1 | pid | CONTAINER / POD / NODE | 56 / 56 / 56 | 8 / 8 / 8 | 0 |
| v1 | ipc | POD / NODE | 84 / 84 | 12 / 12 | 0 |
| v1 | driver | cgroupfs / systemd | 84 / 84 | 12 / 12 | 0 |

过算数**只随 cgroupns 变化**；在 pid / ipc / driver 的各个取值之间**完全均匀**，说明
`hostPID`、`shareProcessNamespace`、`hostIPC`、cgroup driver 这四个维度不进入判据 ——
它们只决定容器能看见哪些 pid，归属靠 cgroup 与 mnt/cgroup namespace。

### 9.5 子 cgroup 嵌套：B2 的触发条件被收窄

| 判定方位置 | cgroupns | self / 目标渲染 | cgroup 腿 | 本地腿(合成) |
|---|---|---|---|---|
| 容器 cgroup 根 | private | `/` 对 `/y` | **1 正确** | 1 正确 |
| 容器 cgroup 根 | host(v1) | `/docker/<id>` 对 `/docker/<id>/y` | **1 正确** | 1 正确 |
| **子 cgroup `/x`** | private | `/x` 对 `/y` | **0 漏算** | **1 正确**（ns 腿先答 YES） |
| **子 cgroup `/x`** | host(v1) | `.../x` 对 `.../y` | **0 漏算** | **1 正确**（同上） |

两条新结论：

1. **判定方留在容器 cgroup 根时，子 cgroup 里的同容器进程被正确计入**（`cgroup_path_is_ours("/", "/y")`
   在 ns 根上只查 `..`，`/docker/<id>` 对 `/docker/<id>/y` 走前缀+边界）。所以 B2 只在
   **判定方自己被挪进子 cgroup** 时才咬。
2. 在 openKernelMode 链里，同容器同 uid 的目标 **ns 腿先返回 YES**，cgroup 比较根本不会被执行，
   所以 B2 只对 **CGROUPV1/V2 模式的主判据** 有影响。

### 9.6 DinD：唯一的新发现

| cgroup | dind 自身 cgroup | 内层容器渲染 | ns 腿 | cgroup 腿 | 结果 |
|---|---|---|---|---|---|
| v2 | **`/init`**（v2 "no internal process" 规则逼 dockerd 迁出根） | `/docker/<inner>` | NO | **0** | **两条腿都漏算** |
| hybrid v1 | `/` | `/docker/<inner>` | NO | **1** | cgroup 腿答对 |
| legacy v1 | `/` | `/docker/<inner>` | NO | **1** | cgroup 腿答对 |

宿主视角内层容器在 `/system.slice/docker-<dind>.scope/docker/<inner>`，**位于外层容器的
cgroup 子树内**，所以宿主按外层容器计费，而库若不计入就会少算。两个独立成因：

- **`ns = NO` 在 openKernelMode 链里是终态**：内层容器 mnt ns 不同 → NO → 直接排除，
  不会落到 cgroup 回退。cgroup 腿即便答对也用不上。
- **v2 上 cgroup 腿也失手**：dockerd 把自己迁进 `/init`，于是 self 与内层容器成了兄弟
  cgroup —— 这正是 B2 的形状。v1 上 dockerd 留在 `/`，cgroup 腿答对。

因此：**CGROUPV1/V2 模式在 v1 上判对、在 v2 上漏算；纯 openKernelMode / CLIENT 回退在两者上都漏算。**

可达性：需要 privileged（DinD 的前提），且内层容器真的拿到 GPU 设备并用起来。
CI/构建类特权 Pod 会跑 DinD，但内层容器默认不会被注入 library、也拿不到 GPU 设备，
需要显式在内层再配 GPU 运行时。属"特权 + 刻意配置"，不属标准 k8s GPU 容器。

### 9.7 本轮顺带确认的两件事

- **CAP_SYS_PTRACE 不在容器默认 capability 集里**：实测容器内 root 读另一个 uid 的
  `/proc/<pid>/ns/*` 同样 **EACCES**（`as0/own-uid1000` 一列 `ns=UNK`）。所以
  "UNKNOWN → cgroup 回退"不是边缘路径 —— **任何跑多 uid 进程的容器都会常态走它**。
- **两条腿看到的 cgroup 字符串完全相同**：`/proc/<pid>/cgroup` 的渲染只取决于读取者的
  cgroup namespace，与读的是本地 `/proc` 还是 `.host_proc` 无关（同一行记录里 `selfL/pidL`
  与 `selfH/pidH` 逐字相等）。两条腿的差别只在用哪个 pid 号。

## 10 B1 的修法、开销与收益（实测）

### 10.1 删掉 `pid_cgroup == "/"` 短路的开销

短路存在的意义就是省掉 `/proc/self/cgroup` 的那一次 open+read：pid 的 cgroup 读出来是
`/` 就直接判为自己的，不去读 self。cgroupns=private 下**本容器每个进程都会命中它**，
所以照字面删掉，等于给每个自己的 pid 加一次文件读。

实测（同一台机，-O2，12 万次；v2 形状在 `--cgroupns=private` 容器内量，pid 的 cgroup
确实是 `/`；v1 形状用 14 行层级的构造文件，key 顺序 `{"memory",""}`）：

| 路径 | v2 形状 | v1 形状 |
|---|---|---|
| A 现状（短路命中） | 4524 ns/pid | 2924 ns/pid |
| B 照字面删短路（每 pid 多读一次 self） | **9155 ns/pid（+4632 ns，+102%）** | **5771 ns/pid（+2847 ns，+97%）** |
| C 删短路 **+ self 的 cgroup 提到 per-pid 循环之外** | **4523 ns/pid（±0）** | **2849 ns/pid（−75 ns）** |

结论：**照字面删会让这条判定翻倍；把 self 的 cgroup 读提到循环外则零成本。**

而且提升 self 的读取**本身就是净收益，与 B1 无关**：今天对**不属于本容器**的 pid，短路
不命中，self 已经是每 pid 读一次（即路径 B 的 9155 / 5771 ns）。提到循环外之后它们也降到
路径 C。举例：整卡 8 个进程、其中 2 个是我们的 —— 今天 `2×4524 + 6×9155 = 64.0 µs`，
提升后 `8×4523 + 一次 4.6 µs self 读 = 40.8 µs`，**−36%**。

落点：`accumulate_used_memory()`（以及 `get_used_gpu_memory_by_device()` 的利用率循环）
在循环前读一次 self 的 cgroup，往下传给 `check_device_pid_in_proc_cgroup_path()`；
后者不再自己 `fopen(PID_SELF_CGROUP_PATH)`。剩下的 ~4.5 µs 主体是
`/proc/<pid>/cgroup` 的 open+read+close，无法再降（结果缓存已在 §8 被否决）。

### 10.2 收益：离线重算全量矩阵

把 930 条记录里的 self/pid 原始字符串取出来，按"删掉短路"的判据重算（短路命中的行里
self 原值从同配置、同判定身份的其它行恢复）：

| | 判定数 | 过算 | 漏算 |
|---|---|---|---|
| 现状 | 2338 | **162** | 4 |
| 删掉短路 | 2322 | **0** | 4 |

- **162 条过算全部消失**，且在全部 930 条记录里**没有引入任何新的漏算**。
- 剩下的 4 条漏算全是 DinD 内层容器（§9.6），与本修改无关。
- 2322 vs 2338：16 条因为同组所有行都命中短路、self 原值无法恢复，重算时记为不适用。

**理论上仍存在的回退**（矩阵里没有这一格，故未被计入）：判定方自己位于子 cgroup、
而目标恰在容器 cgroup 根时，`cgroup_path_is_ours("/x", "/")` 前缀不匹配 → 漏算，
而今天的短路会侥幸判对。这需要容器内可写 cgroupfs（privileged），属 §6 B2 的同一前提。

### 10.3 修完之后还剩什么

| 残留 | 方向 | 触发条件 | 处置 |
|---|---|---|---|
| DinD 内层容器 | 漏算 | privileged + 内层容器真配到 GPU | 接受；拟在 Go 侧用 `nvmlDeviceSetMemoryLimits_v1` 对容器 cgroup 施加 GPU 显存上限兜底（只能限显存、限不住 cores），另议 |
| B2（判定方在子 cgroup） | 漏算 | privileged + 容器内建 cgroup 并迁移；且只影响 CGROUPV1/V2 主判据（openKernel 链里 ns 腿先答 YES） | 记录 |
| B3 / B4 / B5 | 漏算 | 宿主侧搬运 / rw 绑挂宿主 cgroupfs / 刻意 unshare / 刻意迁出子树，均需特权 | 记录 |
| NVML 利用率接口不过滤非本容器条目 | 过算风险 | CUDA 13+；取决于它对不可翻译的 pid 填什么值 | 待 GPU 节点实测（§7 B6） |
