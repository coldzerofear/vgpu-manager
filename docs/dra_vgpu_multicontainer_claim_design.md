# DRA vGPU 多容器共享同一 ResourceClaim 改造设计

## 1. 背景与问题定义

当前 `kubeletplugin` 在开启 `VGPUSupport` 并使用可消耗容量设备（`DRAConsumableCapacity`）时，已经可以为单个 `ResourceClaim` 完成 vGPU 份额分配并注入容器运行时配置。

但现状仍存在一个关键限制：

- 一个 `resourceapi.ResourceClaim` 实际只能稳定服务于 **1 个 Pod 的 1 个容器请求**。
- 当同一个 Pod 内多个容器共享同一个 `ResourceClaim`，并通过 `claims[].request` 引用该 Claim 内不同请求（`spec.devices.requests[].name`）时，当前实现无法正确区分并注入各自独立的 vGPU 配置。

典型失败场景：

- Claim 中有多个请求（例如 `single-vgpu` 与 `multi-vgpu`）。
- Pod 的容器 `ctr0` 绑定 `single-vgpu`，容器 `ctr1` 绑定 `multi-vgpu`。
- 期望：两个容器分别得到与其 request 对应的显存/算力限制及可见设备。
- 实际：配置按 Claim 粒度混合，容器侧区分失败或配置互相覆盖。


## 2. 根因分析（基于现有代码）

### 2.1 CDI 设备命名粒度过粗（Claim 粒度）

当前 CDI 名称由 `claimUID + canonicalDeviceName` 组成，未包含 request/share 维度：

- `pkg/kubeletplugin/cdi.go`：`GetClaimDeviceName(...)`
- `pkg/kubeletplugin/cdi.go`：`CreateClaimSpecFile(...)` 中 `dspec.Name`

后果：

- 同一 Claim 下，同一设备（如同一 vGPU 物理卡）被多个 allocation result（不同 request 或 share）复用时，CDI device name 会冲突或不可区分。

### 2.2 vGPU 容器编辑按 Claim 聚合，导致覆盖

`VGPUManager.GetCDIContainerEdits(claim, devices)` 是聚合式构造：

- 输入是 claim + 设备集合，不是单个 allocation result。
- `getConsumableCapacityMap` 以 `deviceName -> capacity` 建图，多个 result 命中同一设备时后写覆盖前写。

后果：

- `cores/memory` 等配额环境变量无法一一对应到容器 request。
- 多容器共享 claim 时，各容器看到的是混合或被覆盖后的配置。

### 2.3 `applySharingConfig` 分组应用加剧了“混合配置”

`applySharingConfig(...)` 按 config 分组处理 `results`，并一次性生成 group 级 `containerEdits`。

后果：

- 同组内不同 request 的结果共享一套 edits，无法表达“每个 request 单独配置”。

### 2.4 `ShareID` 的语义边界

在 `DRAConsumableCapacity` 下，`ResourceClaim.status.allocation.devices.results[].shareID` 官方定义用于区分同一设备上的不同并发份额。

经后续分析确认：

- `ShareID` 是 **allocation/share 级** 标识，不是容器级标识。
- 一个容器可能命中多个 `ShareID`（例如容器命中多个 request，或单 request 多份额）。

因此：

- `ShareID` 适合用于 CDI 条目唯一化；
- 不宜直接作为“容器挂载目录分组键”。


## 3. 设计目标与约束

### 3.1 目标

1. 支持同一 Pod 的多个容器通过同一个 `ResourceClaim` 引用不同 request，并得到各自独立的 vGPU 注入配置。
2. 在 consumable capacity 场景中，能区分同一设备上的多个份额分配（优先基于 `ShareID`）。
3. 保持 Prepare/Unprepare 语义不变，维持幂等特性。

### 3.2 非目标

1. 不改变调度器分配策略（只处理 kubelet plugin 的 prepare/unprepare 与 CDI 注入）。
2. 不重构非 vGPU 路径（GPU/MIG/VFIO 逻辑保持兼容）。
3. 不引入额外 CRD 或外部存储。

### 3.3 兼容约束

1. 旧 Claim/旧 checkpoint 数据可继续读取。
2. `VGPUSupport=false` 或非 vGPU 设备类型时行为不变化。
3. 失败时可回退到当前版本，不需要迁移集群对象。

### 3.4 业务侧校验前提（Webhook）

在本方案最终落地中，依赖 webhook 提供以下约束：

1. 同一个 Pod 内，一个容器最多命中一个 vGPU request。
2. 同一个 Pod 内，同一个 vGPU request 不允许 app-app 共享，不允许 init-init 共享；init 与 app 可重合。
3. 仅约束 vGPU request，非 vGPU 请求忽略。
4. claimRef 不写 request 时：若该 claim 下仅一个 vGPU request 则允许；若多个则拒绝。


## 4. 总体方案

核心思想：将“可注入单元”从 **Claim 粒度**下沉到 **allocation result 粒度**，同时将“挂载目录作用域”收敛为 **request 粒度**。

- 以 `result`（request + device + share）为基本注入对象。
- 对每个 result 生成唯一 `cdiDeviceID`（用于 CDI 唯一命名）。
- vGPU 容器 edits 拆分为：
  - 设备级 env（allocation/result 级）
  - request 级目录挂载（同 request 多设备共用）


## 5. 关键设计点

### 5.1 CDI DeviceID 设计

统一键生成规则（helper 函数）：

- 输入：`DeviceRequestAllocationResult` + 当前结果序号（`ordinal`）+ 设备 canonicalName
- 优先级：
  1. 有 `ShareID`：`<request>-<canonicalName>-share-<shareID>`
  2. 无 `ShareID`：`<request>-<canonicalName>-<ordinal>`

说明：

- `ShareID` 用于同 request + 同设备下的并发份额区分。
- 需对 `request/canonicalName/shareID` 做字符归一化（`[a-zA-Z0-9._-]`）。

### 5.2 PreparedDevice 扩展

在 `PreparedDevice` 增加与 allocation 绑定的元信息：

- `Request string`
- `CDIDeviceID string`

用途：

- 建立 result -> preparedDevice -> CDI 注入映射。
- 为 checkpoint 与调试提供稳定上下文。

### 5.3 CDI 命名改造

将 `GetClaimDeviceName` 和 `CreateClaimSpecFile` 从“claim + canonicalDeviceName”改为“claim + cdiDeviceID（仅 vGPU）”。

效果：

- 同一 Claim 中，同一设备的不同 share/request 可生成不同 CDI device 条目。

### 5.4 vGPU edits 结构拆分（修正版）

#### Claim 级公共 edits

仅保留真正公共且不随 request 变化的内容：

- 固定 mount（watcher、driver、preload、host proc 等）
- 公共环境变量

#### 设备级 env（allocation/result 级）

按 result 注入设备相关环境变量：

- 可见设备变量
- 核心算力限制（`cores`）
- 显存限制（`memory`）

#### request 级目录挂载

针对固定容器路径的挂载（`config`、`vgpu_lock`、`vmem_node`），按 request 分组：

- 目录路径：`claims/<claimUID>/<request>/...`
- 同 request 下多个设备/result 共用同一目录
- 不同 request 隔离目录

说明：

- 该策略依赖 webhook 保障“单容器最多一个 vGPU request”。
- 目录维护以容器侧映射路径为准，不额外维护冗余 host 目录树。

### 5.5 作用域隔离（保证不影响其他设备）

仅在以下条件启用新逻辑：

- `featuregates.VGPUSupport` 打开
- `allocatableDevice.Type() == VGpuDeviceType`

其余设备类型继续走当前路径。


## 6. 代码改造清单（按阶段）

### 阶段 A（Step 1）：唯一键与 CDI 命名改造（已完成）

1. `pkg/kubeletplugin/prepared.go`
   - 扩展 `PreparedDevice` 元字段（request/cdiDeviceID）
2. `pkg/kubeletplugin/device_state.go`
   - 在 `prepareDevices` 中为每个 result 生成 `cdiDeviceID`
   - 记录到 `PreparedDevice`
3. `pkg/kubeletplugin/cdi.go`
   - `GetClaimDeviceName(...)` 改签名为 `GetClaimDeviceName(claimUID, cdiDeviceID string)`
   - `CreateClaimSpecFile(...)` 按 `cdiDeviceID` 生成 `dspec.Name`

### 阶段 B（Step 2）：vGPU edits 拆分（已完成）

1. `pkg/kubeletplugin/vgpu.go`
   - 拆分公共 edits 与设备级 edits
2. `pkg/kubeletplugin/device_state.go`
   - `applySharingConfig` 不再聚合 vGPU claim 级 edits
3. `pkg/kubeletplugin/cdi.go`
   - 支持 device-level 叠加 edits

### 阶段 B.5（Step 2.5）：目录作用域收敛（待对齐）

目标：从“按 allocation 挂载目录”收敛为“按 request 挂载目录”。

1. `pkg/kubeletplugin/vgpu.go`
   - 将 `config` / `vgpu_lock` / `vmem_node` 的挂载键从 `allocationKey` 改为 `request`
2. `pkg/kubeletplugin/device_state.go`
   - 绑定 request 作用域的目录编辑

### 阶段 C（Step 3）：测试与回归（进行中）

补充测试覆盖：

1. cdiDeviceID 唯一性与稳定性
2. 同设备多 shareID 的 CDI 设备名唯一
3. request 级目录挂载路径正确
4. 同 request 多设备共用目录、不同 request 隔离
5. 非 vgpu 设备回归

建议新增的最小单测断言：

1. `buildCDIDeviceID` 在有 `ShareID` 与无 `ShareID` 时都可稳定生成唯一键（含字符归一化）。
2. `GetClaimDeviceName` 在传入 `cdiDeviceID` 时使用 result 级命名，未传入时回退 canonical 设备名。
3. 同一 request + 同一设备不同 `shareID` 对应的 `cdiDeviceID` 可生成不同 CDI 名称。
4. request 级挂载路径命中 `claims/<claimUID>/<request>/...`。
5. `GetAllocationEnvContainerEdits` 的 env 内容与 `ConsumedCapacity` 一致（cores/memory/visible）。
6. Unprepare 后 claim 目录与 CDI 临时文件可正常清理。


## 7. 兼容性与迁移策略

1. **Checkpoint 兼容**
   - 新增字段为可选 JSON 字段，旧数据可读。
   - 不强制 checkpoint 版本升级。
2. **灰度发布**
   - 优先在 `VGPUSupport` 开启集群小规模验证。
   - 通过多容器共享 claim 场景验证后再全量。
3. **回滚策略**
   - 回滚镜像即可恢复旧行为。
   - 旧 checkpoint 中多出的字段不会影响旧版本读取（未知字段忽略）。


## 8. 风险评估与缓解

### 风险 1：CDI 名称变化引入历史兼容问题

- 影响：已 prepare 但未 unprepare 的临时文件与新逻辑命名不一致。
- 缓解：
  - 保持 claim 级文件名不变，仅设备条目名变化。
  - 通过幂等 Prepare/Unprepare 验证切换安全性。

### 风险 2：固定容器路径重复 mount 导致冲突

- 影响：同容器多设备时，若仍按设备/share 级挂载，可能触发重复 destination mount 行为不确定。
- 缓解：
  - 将固定路径挂载收敛到 request 级作用域。
  - 依赖 webhook 保证单容器最多命中一个 vGPU request。

### 风险 3：Unprepare 清理不完整

- 影响：清理失败被吞掉可能造成目录残留。
- 缓解：
  - Unprepare 返回真实错误并记录日志。
  - 部分准备失败路径也纳入 vGPU 清理。


## 9. 验证方案

### 9.1 功能用例

1. 单容器单 request（现有能力回归）
2. 多容器共享同 claim，不同 request（目标场景）
3. 同 request 下多设备（count>1）
4. 同设备多 shareID（consumable capacity）
5. 非 vgpu 请求回归

### 9.2 关键断言

1. 每个容器仅收到其 request 对应 CDI Device
2. 环境变量中的 cores/memory/visible 不串扰
3. 同 device 不同 share 的 CDI 名称唯一
4. request 级目录挂载无冲突
5. Unprepare 后目录与 CDI 临时文件正常清理


## 10. 最终结论

本次改造应分阶段推进：

1. Step 1（cdiDeviceID + CDI 命名）建立正确映射基础；
2. Step 2（edits 拆分）解决配置串扰；
3. Step 2.5（目录作用域收敛到 request）解决固定路径挂载冲突；
4. Step 3 做回归收口。

该方案的优点是：

1. 精准解决多容器共享 Claim 的 vGPU request 隔离问题。
2. 保留 `ShareID` 在分配唯一性上的价值，同时避免将其误用为容器分组键。
3. 改动范围集中在 `kubeletplugin` 与 webhook 约束，不影响其他设备类型分配链路。
