# AIMD 锯齿问题分析与优化路径

> 状态:**纯分析文档**,无配套代码。所有实验性改动应在 `experiment/aimd-sawtooth` 分支进行(基于 `fix/library` 派生)。
> 关联文档:[sm_controller_aimd.md](sm_controller_aimd.md) 描述 AIMD 控制器本身的设计。
> 触发来源:用户实测报告"开启 `CUDA_SM_CONTROLLER=aimd` 后,单 Pod 训练任务相同 `hard_core` 下耗时比 delta 算法高约 1/3,利用率刚冒头就立即跌、无法稳定在限制值附近"。

---

## 1. 现象与根因

### 1.1 实测现象

| | 单 Pod 训练任务耗时 | 利用率曲线 |
|---|---|---|
| `CUDA_SM_CONTROLLER=delta` | 基线 1.00× | 在 `hard_core` 附近窄幅波动 |
| `CUDA_SM_CONTROLLER=aimd` | 基线 1.33×(慢 ~1/3) | 利用率刚接近 `hard_core` → 立即回退 → 慢慢爬升 → 再回退,周而复始 |

### 1.2 这不是 bug —— 是 AIMD 算法形态的结构性代价

吞吐 = ∫ min(util, hard_core) dt。两种算法稳态形态对吞吐贡献:

| 算法 | 稳态形态 | util 时间均值 / hard_core |
|---|---|---|
| **delta**(对称 ±increment) | target 附近窄幅波动(±MIN_INCREMENT) | ≈ 1.00 |
| **AIMD `÷2`** | 锯齿:peak → 0.5×peak → 爬回 → 砍 | ≈ 0.75 |
| **AIMD `÷3`**(Midokura 默认) | 锯齿:peak → 0.33×peak → 爬回 → 砍 | ≈ 0.67 |

实测 `+1/3` 完美对应理论 `1 / 0.75 ≈ 1.33`,确认现象与算法形态吻合。**MAE 低 ≠ 吞吐高**:低 MAE 可以来自"持续低于目标 + 小波动"这种"被冤枉"的稳态,而吞吐取的是 `min(util, target)` 的积分,任何低于 target 的部分都直接损失。

### 1.3 AIMD 不可剥夺的优势:多 Pod 公平性

Midokura 的 ablation 文档强调 AIMD 在多 Pod 下的公平收敛特性,这是 delta **结构上做不到**的:

| 算法 | 多 Pod 行为 | 公平性保证 |
|---|---|---|
| **delta** | N 个 Pod 各自独立追 `hard_core` → N×`hard_core` 总占用 → **超卖** | 无 |
| **AIMD** | 一旦总占用超阈值,**所有人同时 MD**:占用大的绝对值减得多、小的减得少 → 数轮后趋向 `1/N × hard_core` | Chiu-Jain 1989 数学证明的收敛 |

→ 当前态势是 **"delta 高吞吐但单 Pod / AIMD 公平但单 Pod 低吞吐"** 的二选一,本文档要讨论的是如何摆脱这个二选一。

---

## 2. 为什么我们的 AIMD 锯齿特别难看 —— 3 个结构性放大器

具体到 [cuda_hook.c `aimd_controller`](../library/src/cuda_hook.c):

### 放大器 ①:NVML 整数 % 量化(根因,无法消除)

NVML 报告 `int %`。在 `hard_core=5` 时,util 从 4 跳到 5 就是 **25% 跳变**,跳变就触发 MD。**没有"稳定停在 5"的物理可能性** —— util 只能 4 / 5 / 6 这样整数跳。

小 setpoint 是重灾区,大 setpoint 不明显(80% 时 1% 跳变是 1.25%,可忽略)。

### 放大器 ②:每周期可以多次 MD(关键,可解)

重看代码:

```c
if (user_current > eff_limit) {
    share /= md_divisor;   /* 每个 watcher 周期都跑 */
}
```

**N 个连续超阈值的 watcher 周期 = N 次连续 MD**。NVML 反应有滞后(几十~上百 ms),期间 share 已经被 ÷2 → ÷4 → ÷8 → ÷16 砍到底。这就是用户看到的"利用率刚冒头就立即跌"。

**对比 TCP Reno 的"one halving per RTT"原则**:在一个往返时间内最多 MD 一次,等下次 RTT 才允许再 MD。我们的 AIMD **完全没有这个 cooldown**,是雪崩的根源。

### 放大器 ③:MD 后只有 `floor`,没有"快速恢复到上次稳态"

被砍后,share 从 `ai_step`(floor)起步,要靠 AI 累加几十个周期才能回到之前的水平。期间利用率一直在 0~低水位摇,这就是用户说"无法长期稳定在限制值"的直接原因。

**对比 TCP CUBIC**:记住 MD 前的 `W_max`,**快速恢复到 `0.85 × W_max`,然后平台期**,只在远高于 W_max 时才小步探测。CUBIC 把 Reno 的平均吞吐从 ~75% 推到了 ~95%。

---

## 3. 5 条优化路径(由轻到重)

### 方案 ①:hysteresis 死区 (deadband) —— 最便宜,立竿见影

```pseudo
设 low  = hard_core × 0.90
   high = hard_core × 1.00
util < low   → AI(和现在一样)
util > high  → MD(和现在一样)
low ≤ util ≤ high → 什么都不做(不动 share)
```

| 维度 | 评估 |
|---|---|
| 收益 | 消除"恰好在 hard_core 附近 4↔5 反复跳变 → 反复 MD"死循环;util 一旦进入死区就停在那里 |
| 代价 | 1 个新 env 参数(死区宽度),~5 行代码 |
| 行为变化 | 死区内 util 不严格收敛到 hard_core,会停在死区内任意位置(通常偏低 5-10%) |
| 适配场景 | 几乎所有 setpoint 都受益 |
| 实施难度 | ⭐ |

### 方案 ②:MD cooldown —— TCP Reno 同款防雪崩

```pseudo
记录 last_md_cycle
MD 后 N 个周期内,即使 util > eff_limit 也不再 MD
```

| 维度 | 评估 |
|---|---|
| 收益 | 杜绝 share 被连续 ÷2 ÷4 ÷8 砍到底。NVML 滞后反应期间不会再追加砍 |
| 代价 | 1 个状态字段 + 1 个 env 参数(N,典型 3~5 个周期 ≈ 250~400 ms,够 NVML 反应) |
| 适配场景 | 小 setpoint 收益最大(MD 雪崩最严重的地方) |
| 实施难度 | ⭐ |

### 方案 ③:EWMA 平滑 NVML 输入 —— 解量化

```pseudo
ewma_util = α × current + (1-α) × ewma_util   (α 取 0.3~0.5)
```

AIMD 读 `ewma_util`(浮点)而不是 NVML 原始 int %;`eff_limit` 比较也用浮点。

| 维度 | 评估 |
|---|---|
| 收益 | 小 setpoint 整数跳变(4 → 5)被滤成(4 → 4.3 → 4.7 → 5.1)缓变,延迟 MD 触发、减少触发频率 |
| 代价 | 1 个 float 状态 + 1 个 α 参数 |
| 适配场景 | 特别针对小 setpoint 痛点(`hard_core ≤ 10` 时收益显著) |
| 实施难度 | ⭐⭐ |

### 方案 ④:CUBIC 风格 W_max 记忆 —— TCP 工业级解法

```pseudo
状态:W_max = 上次 MD 前的 share

MD 时:
  W_max = share
  share /= md_divisor

AI 时,按 share 在 W_max 区间的位置选不同步长:
  if (share < 0.85 × W_max)  → 快速恢复(大步)
  else if (share < W_max)    → 平台期(小步,几乎不动)
  else                       → 探测期(超慢步)
```

| 维度 | 评估 |
|---|---|
| 收益 | 把 75% 吞吐推到 ~95%(TCP CUBIC 在 Reno 上的实测增益)。Pod 经过一轮 MD 后**快速回到接近 W_max 并停在那里**,锯齿被压扁成几乎平的 |
| 代价 | 多一个 `W_max[]` 状态 + 比较分支。算法复杂度上一个台阶,需要更细致的参数调(快恢复速率、平台速率、探测速率) |
| 适配场景 | 中大 setpoint(20%+)收益最大 |
| 实施难度 | ⭐⭐⭐⭐ |
| 风险 | CUBIC 本身在 TCP 圈被研究 15+ 年,参数选择有最佳实践可参考;但 vgpu 场景没有"丢包/RTT"概念,需要做映射 |

### 方案 ⑤:按 `sys_process_num` 自动路由 —— 架构级,推荐先做

```pseudo
watcher 已经知道 sys_process_num(当前 GPU 上的进程数)

sys_process_num == 1 → 用 delta   (单 Pod,要吞吐)
sys_process_num >= 2 → 用 AIMD    (多 Pod,要公平)
```

| 维度 | 评估 |
|---|---|
| 收益 | 两种工况下都拿到最优算法的最优行为。**用户报告的"单 Pod 训练慢 1/3" 直接消失** |
| 代价 | watcher 主循环加一个分支,函数指针动态切换;处理切换瞬间 share 状态过渡(两者输入同为 `g_cur_cuda_cores`,可直接交接) |
| 适配场景 | **通用,所有场景都正向收益** |
| 实施难度 | ⭐⭐ |
| 最大风险 | 多 Pod 进入/退出瞬间,sys_process_num 抖动导致算法乒乓切换 → 用 N 个周期一致才切换的简单去抖即可解决 |
| 为什么强烈推荐 | **利用已有信息的最聪明做法**,不需要新数据采集,不需要 ML 调参,行为可预测 |

---

## 4. 推荐组合策略

按"边际收益 / 实施复杂度"排序:

| 优先级 | 方案 | 主要收益 | 工作量 |
|---|---|---|---|
| **P0(强烈推荐)** | **⑤ sys_process_num 自动路由** | 直接解用户报告的"单 Pod 训练慢 1/3" | ⭐⭐ |
| **P1** | **① deadband + ② MD cooldown 一起做** | 即使留在 AIMD,锯齿幅度也明显收窄 | ⭐⭐(两个加一起) |
| **P2** | **③ EWMA 平滑** | 专攻小 setpoint | ⭐⭐ |
| **P3(长期)** | **④ CUBIC 风格 W_max** | 真正"工业级 AIMD" | ⭐⭐⭐⭐ |

**P0 + P1 加在一起就能解决 ~80% 的痛点**:单 Pod 自动走 delta(吞吐回归正常),多 Pod 走改进后的 AIMD(锯齿小很多 + 公平性保留)。

P3 是"如果未来发现 P0+P1 的多 Pod 体验还是不够好"的终极武器,先不急,等真实多 Pod 场景验证完 P0+P1 再决定要不要做。

---

## 5. 重要原则与已被排除的方向

### 5.1 改变算法形态 > 调参数

短期看,**改变 MD 触发频率与触发后的恢复形态**(P1 deadband + cooldown)比**调 `MD_DIVISOR / EFF_RATIO / AI_BASE_DIV`** 更有效。继续在 ÷2/÷3 之间调没用 —— 形态没变。

### 5.2 多 Pod 公平性如何保留

- **P0 自动路由**:多 Pod 自动走 AIMD,继续吃公平性红利。
- **P1 deadband + cooldown**:**不破坏公平性**。deadband 让所有 Pod 都"愿意停在略低于目标",cooldown 让所有 Pod 同步避免雪崩,反而**改善**公平收敛速度。
- **P3 CUBIC W_max**:保留 AIMD 公平内核(MD 步骤不变),只优化恢复轨迹。

### 5.3 已评估并排除的方向

| 想法 | 为何不做 |
|---|---|
| 把 GAP 路径实测的 `gpu_ms` 反馈进 AIMD | 用 GAP 数据派生的瞬时 util 永远≈目标 dc(因为 GAP sleep 就是为达成 dc 而注入的),反馈进 AIMD = 自循环,等于告诉 AIMD "目标达成了别动" → AIMD 摆烂。**架构上是反模式** |
| 用 cuEvent 自带的实测占比**替代** NVML | 只看到本进程,看不到整卡其他进程 → soft_core 模式的独占/竞争逻辑彻底失效(它依赖 `sys_current`)。**架构倒退** |
| 把 GAP 当 watcher 唤醒源(早一周期触发) | 收益小:watcher 本来 80ms 一周期,GAP 触发频率(>200ms 空闲)反而更慢,只有"大 kernel + sync"模式可能有微小改善。**ROI 低** |
| 继续微调 `MD_DIVISOR` | 已测 `÷2` vs `÷3`,模拟显示 ÷2 全 setpoint 更好但仍远不如 delta。**算法形态本身限制了上限,调参摸不到** |

---

## 6. 后续行动

1. **本文档**:存档供未来查证,不写代码。
2. **新分支**:`experiment/aimd-sawtooth` 从 `fix/library` 派生,在该分支上落地 P0 / P1 实验。
3. **`fix/library` 主分支**:保持现状(默认 `delta`,可选 `aimd`)。当前 AIMD 行为已经是 Midokura 算法的忠实移植,不在主分支做激进改动。
4. **决策路径**:
   - 用户在 `experiment/aimd-sawtooth` 上跑真实 GPU 工作负载验证 P0+P1 效果
   - 多 Pod 场景如能显著优于 delta 单 Pod,合并 P0 回 `fix/library`
   - P1 单独评估,决定是否合入 AIMD 默认行为
   - P3(CUBIC)看 P0+P1 落地后多 Pod 体验是否足够再决定

---

## 7. 参考

- **TCP Reno**:RFC 5681,对称 AIMD,"one halving per RTT"原则的最早工业实践。
- **TCP CUBIC**:Ha et al. 2008,CUBIC 函数 + W_max 记忆,Linux 默认拥塞控制。
- **Chiu-Jain 公平性证明**:Chiu, Jain 1989, "Analysis of the Increase and Decrease Algorithms for Congestion Avoidance in Computer Networks"。证明 AIMD 在共享资源上的公平收敛。
- **Midokura HAMi-core ablation**:[ablation/orig-aimd-v5 分支](https://github.com/midokura/HAMi-core/tree/ablation/orig-aimd-v5)。
