# DRA 本地模式下 VGPUSupport 与 DynamicMIG 协作分析

分析基线：`main` @ `6e12e5f`，依赖 `k8s.io/dynamic-resource-allocation v0.37.1`。
范围：kubelet-plugin 本地路径（不含 `RemoteGPUSupport`）。第 3 节描述基线上的问题，处理结果见第 7 节。
全部结论都没有上真机。

## 1. 结论

两个门控可以同时开启，协作模型是**按物理卡互斥**：同一张卡某一时刻要么处于 vGPU 共享状态，要么处于 MIG
切分状态，随分配情况动态切换。这不是"vGPU 跑在 MIG 实例里"，该能力没有实现。

| 层面 | 状态 |
| --- | --- |
| 门控校验 | 互斥检查已注释掉（`featuregates.go:334`），可同时启用 |
| 调度互斥 | K8s ≥ 1.36.2 成立，已用真实 allocator 验证；**低于此版本有缺口**（见 3.1） |
| Prepare/Unprepare 主链路 | 正常路径成立；节点侧没有卡级冲突兜底（见 3.3） |
| 健康监控 | 整卡 XID 在 vGPU 卡上被丢弃（见 3.2） |
| 真机验证 | **未做**，见第 6 节 |

各卡型行为：

| 卡型 | 启动时 MIG 状态 | 发布的设备 | 能否动态切换 |
| --- | --- | --- | --- |
| Hopper 及以上（可免复位切换） | 任意 | `vgpu-N` + 全部 `gpu-N-mig-*` | 能 |
| Ampere（A100） | MIG 关 | 仅 `vgpu-N`（`isDynamicMIGCapable=false`） | 不能 |
| Ampere（A100） | MIG 开 | 仅 `gpu-N-mig-*`（`nvlib.go:293`） | 不能 |
| 不支持 MIG 的卡 | - | 仅 `vgpu-N` | 不涉及 |

开启 `VGPUSupport` 后整卡设备 `gpu-N` 不再发布，整卡需求走 100% 的 vGPU，或走 7g 的 MIG。

## 2. 协作机制

一张 Hopper 卡发布到同一个 ResourceSlice 的形态（仿真实测输出，H100 形态夹具）：

```text
slice-0 (仅 SharedCounters): gpu-0-counter-set, gpu-1-counter-set
slice-1: gpu-0-mig-1g10gb-19-0 ... gpu-0-mig-7g80gb-0-0, vgpu-0[allowMultipleAllocations]
slice-2: gpu-1-mig-1g10gb-19-0 ... gpu-1-mig-7g80gb-0-0, vgpu-1[allowMultipleAllocations]
```

- `vgpu-N` 消耗该卡**全部** counter（`partitions.go:113-122`），任一 MIG 分配后计数不足，首个 vGPU share 进不来。
- 首个 vGPU share 扣满 counter 后 MIG 进不来；后续 share 经 `skipCounterCheck` 跳过，不重复扣。
- 代价是一个 `cores=1` 的 vGPU 也会锁死整卡的 MIG。这是物理约束决定的，卡开 MIG 后 parent 不能再当共享整卡用。
- 卡的状态靠分配驱动：`createMigDevice` 按需 `SetMigMode(ENABLE)`，`deleteMigDevice` 删到最后一个 MIG 时
  `maybeDisableMigMode` 关闭。

已逐项核对、确认没有问题的点：

- counter set 命名：vGPU 与 MIG 都走 `GpuDeviceInfo.GetSharedCounterSetName()`，两者一致。
- `getConfigResultsMap`：`GpuConfig` 匹配 vGPU，`MigDeviceConfig` 匹配 MIG。
- `validateNoOverlappingPreparedDevices` 跳过 vGPU，共享不会被误拒；MIG 同名重叠仍会拒绝。
- `resolveVGPUClaimPartitions` 对纯 MIG claim 提前返回，不访问 API（`claimresolve/partitions.go:106`）。
- 回滚的名称解析 `^gpu-` 不匹配 `vgpu-N`，不会误删。
- vGPU 的 LD_PRELOAD 等 CDI edits 只进 vGPU 的配置组，纯 MIG 容器不受影响。
- SM watcher 每次按实时状态跳过 MIG 卡（`watcher.go` 的 `IsMigEnabled`）。
- DaemonSet 已是 privileged + hostPID，具备 `SetMigMode` 权限。
- kubelet 明确先 Unprepare 再写终态 Status（`kubelet.go:2441` 注释），正常路径下 MIG 拆完才释放分配，
  不存在"MIG 未拆完卡就被重新分配"的竞争。

## 3. 问题清单

### 3.1 【P0】K8s < 1.36.2：已持久化的 vGPU 不占 counter，MIG 会分到正在跑 vGPU 的卡上

调度器把带 ShareID 的分配记入 `AllocatedSharedDeviceIDs`。旧版本计算可用 counter 时只看 `AllocatedDevices`：

```go
// v1.34 / v1.35.x / v1.36.0 / v1.36.1
if !alloc.allocatedState.AllocatedDevices.Has(deviceID) { continue }
// v1.36.2 起
if !internal.IsDeviceAllocated(deviceID, &alloc.allocatedState) { continue }
```

版本核对（直接取自 kubernetes 各 tag 的源码）：

| 版本 | ConsumableCapacity 使用的 allocator | 计入持久化 share | 同周期 share 不重复扣 |
| --- | --- | --- | --- |
| 1.34.x（查 .0/.3/.5/.8） | experimental | 否 | **否** |
| 1.35.x（查 .0/.3/.6/.9） | experimental | 否 | 是 |
| 1.36.0、1.36.1 | incubating | 否 | 是 |
| 1.36.2 及以后 | incubating | **是** | 是 |

后果：vGPU claim 落定后，下一个调度周期 MIG 看到的 counter 是满的，于是被分到同一张卡。

- vGPU 有活跃进程时：`SetMigMode` 报 IN_USE，MIG Pod 反复 Prepare 失败卡在 ContainerCreating。
- vGPU 尚未打开 GPU 时：MIG 模式打开成功，随后 vGPU 容器看不到可用设备。

1.34 还多一个问题：同一调度周期内多个 share 落到同一张卡会重复扣 counter，第二个失败。

插件无法在节点侧修正调度结果。另外 1.36 之前 `DRAPartitionableDevices` 与 `DRAConsumableCapacity` 默认关闭，
必须显式开启；可用 `kubectl get resourceslice -o yaml` 确认 `consumesCounters`、`allowMultipleAllocations` 没被丢弃。

建议：两个门控同开且节点有可动态 MIG 的卡时，检测 API server 版本，低于 1.36.2 直接启动失败。
`shouldUseSplitResourceSlices` 已经取到版本，改动很小。这属于配置不兼容，放行的后果是分配到错误的卡。
严格说起作用的是 kube-scheduler 版本，API server 版本只是近似，文档需同时写明。

### 3.2 【P1】整卡 XID 在 vGPU 卡上被丢弃（与 DynamicMIG 无关，当前版本已存在）

`resolveDeviceByEventAddress` 对整卡事件调用 `GetGPUDeviceByUUID`（`device_health.go:339`），该函数只匹配
`GpuDeviceType`（`allocatable.go:423-430`）。`VGPUSupport` 下 parent 的类型是 `vgpu`，返回 nil，事件走
"unexpected device" 分支后被忽略。仿真已复现。

后果：Chart 默认开启 `NVMLDeviceHealthCheck`，但致命 XID 不会给 vGPU 打污点，调度器继续把新 Pod 放到坏卡上。
`GPU_LOST` 走全量批量路径，不受影响。

DynamicMIG 下还有第二层：污点只打在单个设备上，修好上面的查找后 `vgpu-N` 被污染，同卡的 `gpu-N-mig-*`
仍可调度。整卡级事件应扇出到该 PCI 下的全部设备（`DeviceHealthEvent.Devices` 已支持批量）。

### 3.3 【P1】节点侧没有卡级冲突兜底

`Prepare` 的重叠校验只按设备名比较，看不出 `vgpu-0` 与 `gpu-0-mig-*` 是同一张卡（仿真验证：父卡已有 prepared 的
MIG 时，vGPU 校验返回 nil）。vGPU 分支也不检查父卡实时 MIG 状态，直接把 parent 交给容器。

能走到这里的路径：

1. 3.1 的旧版本调度结果。
2. 强删 Pod：Pod 对象消失后分配被释放，kubelet 还没 Unprepare（`device_state.go:376-378` 注释提到的场景）。
3. 启动清理失败：`DestroyUnknownMIGDevices` 是 best-effort（`device_state.go:537`），只打日志，卡可能留在 MIG 模式，
   但 `vgpu-N` 照常发布。

建议在 `prepareDevices` 增加按 PCI 的互斥校验：vGPU 与其他已 prepared 的 MIG 冲突时返回错误，反之亦然；
vGPU 分支再读一次实时 MIG 状态，开启且无 MIG 设备时先尝试关闭，失败则报错。kubelet 会重试，错误比静默拿到不可用设备好排查。

### 3.4 【P2】`SetMigMode` 返回值变量名写反，API 级错误被吞

go-nvml 的返回顺序是 `(activationStatus, ret)`，而 `nvlib.go:612`、`nvlib.go:798` 写成
`ret, activationStatus := ...SetMigMode(...)`。两个分支的效果不同：

- 激活失败（如 IN_USE）：落在变量 `ret` 里，被正确拦截。
- API 调用失败（如 NO_PERMISSION）：`activationStatus` 保持零值 SUCCESS，判断通过，错误被吞。

随后 `maybeDisableMigMode` 把 `gpu.MigEnabled` 置 false，但硬件仍在 MIG 模式。下次 `createMigDevice` 读到实时已开启，
走 `else` 分支不回写标志；`GetMigDeviceInfos` 看到 `MigEnabled=false` 返回 nil（`nvlib.go:171`），
最终报 `error getting migInfo`，且一直持续到插件重启。

建议同时判断两个返回值，并在 `createMigDevice` 的 `else` 分支补 `gpu.MigEnabled = true`。上游写法相同，可一并回馈。

### 3.5 【P2】启动时对每张卡都调用 `SetMigMode(DISABLE)`

`obliterateStaleMIGDevices` 对所有 GPU 无条件调用 `maybeDisableMigMode`（`nvlib.go:573`），后者不判断当前 MIG 状态：
`GetMigDeviceInfos` 对未开 MIG 的卡返回 nil，随后对 Hopper 及以上直接 `SetMigMode(DISABLE)`。

因此插件每次重启，都会对**正在跑 vGPU 业务**的卡发一次该调用。NVML 文档说明此 API 可能解绑或复位设备；模式未变时
是否无副作用，代码层面无法确认。建议加 `if !gpu.MigEnabled { return nil }` 守卫，成本极低。

### 3.6 【P2，待真机确认】启动时机：拓扑与内存在 MIG 清理之前读取

`NewDeviceState` 在 `DestroyUnknownMIGDevices`（`driver.go:137`）之前完成拓扑发现和 `Memory.Total` 读取，
且都只读一次。若插件重启时卡处于 MIG 模式：

- MIG 下不支持 GPU 间 P2P，若 NVML 此时报告链路不可用，该卡的 `nvlinkDomain` 会缺失，直到下次重启，
  `link-strict` 请求一直不匹配。
- `vgpu-N` 的 memory 容量取自 MIG 模式下的父卡内存读数，可能与非 MIG 模式不同。

### 3.7 【P3】较小问题

- `docs/feature_gates.md:129` 写 `DynamicMIG ⊗ NVMLDeviceHealthCheck`，代码和测试都已允许，文档过期；同文件
  `:292` 写健康检查默认开启，与默认值 `false` 不符。
- 没有覆盖 `VGPUSupport + DynamicMIG` 组合的测试。
- DRA 路径不上报 MIG 指标（`dra_gpu.go:144`），切到 MIG 的卡上的容器监控为空。
- 一个容器同时请求 vGPU 与 MIG 时没有 webhook 拒绝，`libvgpu-control.so` 不感知 MIG，行为未定义。
- 时间片复位在其他 vGPU claim 还在用这张卡时也会执行：`isGpuUUIDInUseByOtherClaims` 只看 `dev.Gpu`，不看 `dev.VGpu`。
- `partitions.go:233`：DynamicMIG 开启、卡不可动态 MIG 但已开 MIG（仅 vGPU guest）时会 panic，场景很窄，记录不修。
- 调度是 first-fit，不感知"卡的模式"，混合负载下可能把多张卡各占一点。需要固定比例时，用节点分组或 selector 约束。

## 4. 建议改造顺序

1. 3.2 XID 查找修复与整卡扇出：独立于本特性，当前已影响线上 vGPU。
2. 3.1 版本门槛：启动校验 + 文档写明最低版本与所需 k8s 门控。
3. 3.3 节点侧按卡互斥校验。
4. 3.4、3.5 两处小修复，附单测。
5. 把第 5 节仿真转成正式测试，修正 3.7 文档，再上真机走第 6 节。

## 5. 测试数据

### 5.1 方法

用 `go test -overlay` 注入测试文件，仓库文件不变。切片由插件真实的 `GenerateDriverResources` 生成，分配用真实的
`k8s.io/dynamic-resource-allocation/structured` v0.37.1（incubating），features 为
`ConsumableCapacity + PartitionableDevices + PrioritizedList`。已分配状态按调度器 `foreachAllocatedDevice` 的语义构造：
有 ShareID 进 `AllocatedSharedDeviceIDs` + `AggregatedCapacity`，否则进 `AllocatedDevices`。

夹具为 H100 形态：8 个 memory slice，132 SM，7 个 1g 位置 + 1 个 7g。

### 5.2 结果（v0.37.1，切片模式 split）

| 场景 | 结果 |
| --- | --- |
| 单卡：vGPU#1、vGPU#2 | 都落到 `vgpu-0` |
| 单卡：已持久化 vGPU 后请求 MIG | 拒绝 |
| 单卡：两个 1g 后请求 vGPU | 拒绝 |
| 单卡：释放一个 MIG 后请求 vGPU | 仍拒绝 |
| 单卡：MIG 全部释放后请求 vGPU | 成功 |
| 单卡：7g 后请求 1g、vGPU | 都拒绝 |
| 双卡：vGPU×2 + 1g×2 + 再一个 vGPU | vGPU 全在 `vgpu-0`，MIG 全在 `gpu-1-*` |
| 单卡：同一周期 vGPU + MIG | 拒绝 |
| 单卡：同一周期 3 个 vGPU share | 成功，counter 只扣一次 |
| 双卡：同一周期 vGPU + MIG + vGPU | `vgpu-0`、`gpu-1-mig-1g10gb-19-0`、`vgpu-0` |
| 整卡 XID 落在 vGPU parent | 解析为 nil，事件被丢弃（复现 3.2） |
| 父卡已有 prepared MIG 时校验 vGPU | 返回 nil，未检测到冲突（复现 3.3） |

现有测试基线：`go test ./pkg/kubeletplugin/...` 全部通过（Go 1.26.9）。

### 5.3 否定结果与限制

- 1.34～1.36.1 的结论来自对各 tag 源码的逐行比对，**没有实际运行**旧版 allocator。
- 1.34 专用的 combined 切片（同一 slice 同时含 devices 与 counters）没有验证：1.37 API 校验已禁止这种形态，
  在 1.37 allocator 上首个 vGPU 就分配失败，失败的具体分支未深入，不作为产品问题。
- 仿真早期两次失败都是夹具错误，非产品问题：7g 与 1g 共用 profile 名，导致 1g 选择器命中 7g；jpeg counter
  总量设成 1，导致第二个 1g 计数不足。修正后全部通过。第二次失败也说明 counter 约束确实生效。
- NVML、MIG 模式切换、容器内 CUDA 行为全部没有真机验证。

## 6. 真机验证清单

需要 Hopper 及以上卡，K8s ≥ 1.36.2：

1. vGPU 与 MIG 轮替：同卡 vGPU → 全释放 → MIG → 全释放 → vGPU，确认模式切换和容器内 CUDA 可用。
2. SM watcher 持续轮询时，`SetMigMode` 是否报 IN_USE；monitor 每次采集的短 NVML 会话是否会造成 Unprepare 反复重试。
3. 运行中的 vGPU 业务在插件重启时，`SetMigMode(DISABLE)` 是否有影响（3.5）。
4. MIG 模式下重启插件，确认 `nvlinkDomain`、`Memory.Total` 是否变化（3.6）。
5. 致命 XID 注入，确认修复后污点行为（3.2）。
6. 强删 Pod 后立即调度另一模式的 Pod（3.3）。

## 7. 修复记录（分支 `fix/dra-vgpu-dynamic-mig`）

| 章节 | 处理 |
| --- | --- |
| 3.1 | 同一张卡同时发布 vGPU 和 MIG 时，API server < 1.36.2 启动失败。校验放在 `DestroyUnknownMIGDevices` 之前 |
| 3.2 | `GetGPUDeviceByUUID` 识别 vGPU；整卡事件扇出到该卡全部 Dynamic MIG 设备 |
| 3.3 | 新增 `validateNoGPUModeConflict`（按 PCI）和 vGPU 分支的 `ensureMigDisabled`（读实时模式） |
| 3.4 | 新增 `setMigMode` 同时检查两个返回值；`createMigDevice` 回写 `MigEnabled` |
| 3.5 | `maybeDisableMigMode` 先读实时模式，不在 MIG 模式的卡不再收到 `SetMigMode` |
| 3.6 | **未处理**，需要真机确认后再改 |
| 3.7 | 修正文档，补组合测试，修正时间片的 in-use 判断。MIG 指标、混合容器拒绝、vGPU guest panic、调度策略未处理 |

### 7.1 健康检查对 SM watcher 的影响

存在影响，基线上被 3.2 掩盖，修 3.2 后才会真正触发，所以一并处理：

- `smWatcherSingleDevice` 遇到 `!Healthy` 直接返回，`LastSeenTimeStamp` 停止刷新。
- 基线下 `Healthy` 由 `IsHealthy(taints)` 计算，且污点不恢复，一次致命 XID 会让该卡 watcher 永久停摆。
- NoSchedule 不驱逐，业务继续运行。读方前 5 秒拿到冻结样本，之后每个容器退回本地 NVML 轮询（限流仍然有效，但
  O(1) 退化为 O(N)）。

修复：`Healthy` 在 DRA 路径只有 watcher 使用，改为仅 `GPU_LOST` 才置 false。另外
`healthDeviceMap` 原先在 goroutine 里填充，同时被健康事件循环读取，存在 map 并发读写，已改为启动 goroutine 前同步填好。

仍存在的限制：

- NVML 的 `GPU_LOST` 通常分不清是哪张卡，会标记全节点，此时所有卡的 watcher 都停，直到插件重启，容器走本地轮询。
- `Healthy` 是无同步的 bool，读写之间有良性数据竞争。
- device-plugin 路径的 `Healthy` 同样让 watcher 跳过，但语义不同（设备会从 kubelet 摘除），本次未改。
- device-monitor 不读这份共享文件，不受影响。

### 7.2 校正

3.7 初稿写"时间片复位不覆盖 vGPU"有误：`unprepareDevices` 调用的是 `PreparedDeviceList.GpuUUIDs`，包含 vGPU。
实际问题是 in-use 判断漏掉 vGPU，已更正。3.4 初稿的推测"非 MIG 卡 SetMigMode 失败会中断清理"也不成立，
变量写反使该错误被吞，现在由 `isMigModeEnabled` 的 NOT_SUPPORTED 分支提前返回。

### 7.3 修复后的测试数据

`go test -race` 覆盖 `pkg/kubeletplugin/...`、`pkg/device/...`、`pkg/metrics/...`、`cmd/kubelet-plugin`，全部通过（Go 1.26.9）。
第 5 节仿真已转成 `pkg/kubeletplugin/vgpu_dynamic_mig_test.go`。

变异检查（用 `-overlay` 逐项破坏修复，确认测试能发现）：

| 破坏项 | 结果 |
| --- | --- |
| `GetGPUDeviceByUUID` 不识别 vGPU | 被 `TestFullGPUXIDAffectsWholeCard` 等发现 |
| 整卡事件不扇出 | 被 `TestFullGPUXIDAffectsWholeCard` 发现 |
| XID 也让 watcher 停止 | 被 `TestUpdateSMWatcherHealth` 发现 |
| 最低版本放宽到 1.36.1 | 被 `TestValidateVGPUDynamicMIGSupport` 发现 |
| 不读实时 MIG 状态 | 被 `TestMaybeDisableMigMode` 发现 |
| 卡级 XID 被 MIG 销毁清掉 | 被 `TestClearDynamicMIGXIDTaintKeepsCardLevelXID` 发现 |
| 去掉按卡互斥 | 被 `TestValidateNoGPUModeConflict` 发现 |
| in-use 判断漏 vGPU | 被 `TestIsGpuUUIDInUseByOtherClaimsCountsVGPU` 发现 |
| `SetMigMode` 接收顺序对调 | 未发现，属等价变异：`setMigMode` 两个值都检查，顺序不影响判定 |

两个既有测试的夹具做了调整：`TestDeviceLibDeleteMigDevice` 和回滚测试原本靠父卡 `MigEnabled=false` 这个与现实不符的状态跳过 MIG 枚举；
`TestClearDynamicMIGXIDTaint` 的 `MigSpec` 没有 Parent。生产路径上这些状态都不会出现，所以改夹具，没有加防御代码。

`ensureMigDisabled` 串入 `prepareDevices` 的调用点、`NewDriver` 里的版本校验调用点只有单元级覆盖，没有端到端测试。
