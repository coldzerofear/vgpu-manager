# 算力切分控制器:可选 AIMD(对标 Midokura HAMi-core 消融研究)

> 作用范围:`library/`(LD_PRELOAD 运行时库)的 watcher 主循环内的份额更新算法。
> 关系:与 [GAP 路径节流](sm_core_limit_gap_throttle_design.md) **正交、互补** —— GAP 解大 kernel 同步模式下的瞬时绕过,AIMD 解 watcher 稳态围绕目标的高方差。两者可同时启用。
> 状态:已落地,**默认关闭**(env 切换),`CUDA_SM_CONTROLLER=aimd` 启用。

---

## 1. 问题:stock `delta()` 稳态精度差

现有 watcher 的份额更新算法(`delta()`,[cuda_hook.c:366](../library/src/cuda_hook.c#L366))是**对称按差比例**:`user_current < target` 加 `increment`,反之减同等量。该算法假设系统线性,但 NVML 采样有 ~80ms 滞后 + 噪声,导致 share 在目标附近震荡幅度大。

**实证数据**(来自 [midokura/HAMi-core 分支 `ablation/orig-aimd-v5`](https://github.com/midokura/HAMi-core/tree/ablation/orig-aimd-v5),`kenji-mido` 维护,2026-05-20 RTX 4080 重测):

| 算法 | MAE(实测 SM 利用率 vs `hard_core` 目标) |
|---|---|
| Stock(我们 `delta()` 同源) | 17.5% — 20.7% |
| AIMD v5(`÷3` MD,`7/8` 缓冲) | 2.2% — 2.8% |

误差降一个数量级。我们的 `delta()` 与 stock 同源,意味着我们的稳态精度大概率也是 ~20% 量级。

## 2. AIMD 算法

经典 **A**dditive **I**ncrease / **M**ultiplicative **D**ecrease 模式(TCP Reno 同款,具备多流公平性的理论收敛):

```c
eff_limit = up_limit * 875 / 1000;            // 87.5% 缓冲,提前回退
if (user_current <= eff_limit) {
  int gap = up_limit - user_current;          // 用真实目标,非 eff
  if (gap < 5) gap = 5;                       // 防止贴近目标时停滞
  ai_step = sm_num * max_thread * eff_limit / 400;
  share += ai_step * gap / 5;                 // AI:gap 成比例的慢增
  // 上界裁剪到 g_total_cuda_cores
} else {
  share /= 3;                                 // MD:超调一次砍 2/3
}
```

**为什么有效**:超调发生时,对称 delta 慢慢减回去(每周期减一个 increment),AIMD 一次砍 2/3 立刻回到安全区,下次重新慢增。`7/8` 缓冲让算法在到达真实目标前就开始减小步长,避免冲过头。

## 3. 是 `delta()` 的纯函数级替换 —— 零外溢影响

设计约束:**完全不改 watcher 的策略层与采样层**,只换内层算法。覆盖核对:

| 现有功能 | 是否被影响? | 原因 |
|---|---|---|
| `hard_limit` 模式 | ❌ | target=`hard_core` 由策略层设定,AIMD 收敛到它 |
| `soft` 模式·独占·burst 到 `soft_core` | ❌ | `up_limits=soft_core` 由策略层维护 |
| `soft` 模式·竞争·从 `hard_core` 弹性爬向 `soft_core` | ❌ | up_limits 由外层根据 sys_frees 调整,AIMD 跟随 |
| 新进程重置(`pre_sys_process_nums`) | ❌ | 策略层 |
| jitter init 特例 | ❌ | 走旁路直写 `g_cur_cuda_cores`,与 shares 路径互不冲突 |
| External SM Watcher(`EXTERNAL_SM_WATCHER_ENABLED=1`) | ❌ | 采样层切换,与算法层正交;4 种组合都成立 |
| GAP 路径([sm_core_limit_gap_throttle_design.md](sm_core_limit_gap_throttle_design.md)) | ❌ | `gap_effective_dc` 读 `up_limits` 不读 `shares` |
| `rate_limiter`/令牌桶 | ❌ | 仍按 `g_cur_cuda_cores` 决定睡不睡 |
| Metrics | ⚠️ 语义注意 | 见 §6 |

实现:[cuda_hook.c](../library/src/cuda_hook.c) watcher 主循环里 4 处 `delta(...)` 调用改成函数指针 `g_sm_controller(...)`,init 时按 env 指向 `delta` 或 `aimd_controller`。

## 4. Env 开关与参数

| Env | 默认 | 含义 |
|---|---|---|
| `CUDA_SM_CONTROLLER` | `delta` | `delta`(stock)或 `aimd` |
| `CUDA_SM_AIMD_MD_DIVISOR` | `3` | MD 因子,`share /= div`;最小 2 |
| `CUDA_SM_AIMD_EFF_RATIO` | `875` | 缓冲比例(千分制),875 = 87.5%;上界 1000 |
| `CUDA_SM_AIMD_AI_BASE_DIV` | `400` | AI 步长基数除数,越大步长越小 |

启动时一次性读取,**不支持运行时切换**(会涉及 share 历史状态重置的边界条件)。

启动日志会打出 controller 选择(与现有 `+ HardLimit` / `+ HardCoreSize` 行同款):

```
+ SmController   : delta (stock)
+ SmController   : aimd (md_div=3 eff_ratio=875/1000 ai_base_div=400)
```

## 5. 与 GAP 路径的组合

| 失效模式 | GAP 路径 | AIMD watcher | 合体 |
|---|---|---|---|
| 大 kernel + sync 循环绕过限速(瞬时大偏差) | ✅ | ❌(~80ms 采样仍慢) | ✅ |
| 稳态围绕 `hard_core` 高方差(持续误差) | ❌ | ✅(MAE 20% → 3%) | ✅ |
| 多 Pod 公平性 | ⚠️ P2 | 部分(AIMD 有理论收敛) | 待 P2 |

两者完全独立、可任意组合(`CUDA_SM_CONTROLLER` × GAP 路径默认开)。

## 6. Metrics 语义变化

`metric=kernel_rate_limit_hit` 现在带 `controller=delta|aimd` 标签:

```
metric=kernel_rate_limit_hit host_device=0 controller=aimd total=1024
```

**为什么重要**:AIMD 把 MAE 从 ~20% 压到 ~3%,代价是 `g_cur_cuda_cores` 在 0 附近"小步多次"波动,**`rate_limit_hit` 计数预期上升**(更频繁但每次更短)。如果运维看板用此指标做"算力被节流频繁度"告警,切到 AIMD 后会误报 —— controller 标签让看板可按算法分组,避免误读。

## 7. 必须主动提醒的参数标定问题

Midokura v5 的 `÷3` / `7/8` / `ai_base_div=400` 是在 **RTX 4080**(消费卡,SM=76,每 SM 1536 线程)上调出来的。我们的 A100(SM=108,2048/SM)/ H100(SM=132,2048/SM)的 SM 数量级与线程粒度不同:

- `÷3` 在大卡上**可能过激进**(一刀切 67% 算力,AI 爬回去需要更多 watcher 周期 → 短期欠用)。考虑 `÷2`。
- `ai_base_div=400` 对应的步长会随 `sm_num * max_thread_per_sm` 线性放大,大卡上**步长可能偏大** → 把 `ai_base_div` 调大(如 800-1600)收紧步长。

参数全部 env 暴露,正是为此 —— **上线前必须在目标 GPU 上至少跑一遍参数扫描**。Midokura 自己迭代了 v1→v5 五个版本才收敛。

## 8. 验证计划

1. **默认关闭无副作用**:不设 `CUDA_SM_CONTROLLER=aimd` 时为 no-op,所有现有 stock 行为零变化。
2. **AIMD 启用 + 硬模式**:`hard_core=30`,`gpu-burn` 或类似工作负载,采样 SM 利用率,计算 MAE。期望:相比 stock 显著降低。
3. **AIMD + 软模式·独占·`soft_core=80`**:期望 share 收敛到允许 ~80% 突发。
4. **AIMD + 软模式·竞争·两进程**:期望各进程随 watcher 在 hard_core↔soft_core 弹性,且 AIMD 不破坏弹性策略。
5. **AIMD + External SM Watcher 同时开**:期望 4 种组合中精度最佳。
6. **参数标定**:`MD_DIVISOR` 在 {2,3,4}、`EFF_RATIO` 在 {800,875,950}、`AI_BASE_DIV` 在 {200,400,800,1600} 上扫描,找各 GPU 型号的最优组合。
7. **看板**:确认 `controller=aimd` 标签出现在日志,运维看板新增按 controller 分组的面板。

## 9. 后续路线

> **重要补充**:实测发现 AIMD 在单 Pod 下耗时比 delta 高约 1/3,根因与解法全文档详见 [sm_controller_aimd_sawtooth_analysis.md](sm_controller_aimd_sawtooth_analysis.md)。下面列出的 P1/P2/P3 仅为原始路线图,真正的优化路径请优先参考 sawtooth 分析文档的 P0~P3 推荐(尤其 P0 sys_process_num 自动路由)。

- **P1 — 多 Pod 公平性测量**:借用 Midokura 的 `plot_multi_single.py` 等脚本(参见 [GAP 路径设计 §11](sm_core_limit_gap_throttle_design.md)),验证 AIMD 在 N Pod 间是否收敛到 `1/N · hard_core` 公平点。
- **P2 — 自动参数标定**:把参数扫描集成到 CI 跑,按 GPU 型号建一份推荐默认。
- ~~P3 — 与 GAP 路径联调~~:**已评估,不实施**。GAP 路径派生的瞬时 util 与 AIMD 形成自循环(GAP sleep 本就是为达成目标 dc 而注入,反馈进 AIMD 等于告诉它"目标达成了别动")。详见 [sawtooth 分析 §5.3](sm_controller_aimd_sawtooth_analysis.md)。
