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

### 2.4 未利用 `ShareID` 进行可消耗份额区分

在 `DRAConsumableCapacity` 下，`ResourceClaim.status.allocation.devices.results[].shareID` 官方定义即用于区分同一设备上的不同并发份额。

现状：项目中尚未显式使用 `ShareID` 参与键构造与注入映射。


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


## 4. 总体方案

核心思想：将“可注入单元”从 **Claim 粒度**下沉到 **allocation result 粒度**。

- 以 `result`（request + device + share）为基本注入对象。
- 对每个 result 生成唯一 `allocationKey`。
- CDI 设备命名使用 `claimUID + allocationKey`，避免冲突。
- vGPU 容器 edits 拆分为：
  - claim 级公共 edits（目录、挂载等）
  - allocation 级专属 edits（cores/memory/visible/env 等）


## 5. 关键设计点

### 5.1 AllocationKey 设计

新增统一键生成规则（建议新增 helper 函数）：

- 输入：`DeviceRequestAllocationResult` + 当前结果序号（`ordinal`）
- 优先级：
  1. 有 `ShareID`：`<device>-share-<shareID>`
  2. 无 `ShareID`：`<device>-req-<request>-<ordinal>`

说明：

- `ShareID` 是同设备并发份额的天然区分键，优先使用。
- 为了避免极端情况下 `request` 重名，保留 `ordinal` 兜底。
- 需要对 `device/request/shareID` 进行字符归一化（只保留 `[a-zA-Z0-9._-]`）。

### 5.2 PreparedDevice 扩展

在 `PreparedDevice` 增加与 allocation 绑定的元信息：

- `Request string`
- `ShareID string`（空字符串表示无 share）
- `AllocationKey string`

用途：

- 建立 result -> preparedDevice -> CDI 注入的一致映射。
- 为 checkpoint 持久化提供稳定上下文，便于重试和恢复。

### 5.3 CDI 命名改造

将 `GetClaimDeviceName` 和 `CreateClaimSpecFile` 从“claim + canonicalDeviceName”改为“claim + allocationKey”。

效果：

- 同一 Claim 中，即使是同一设备名，也可为不同 share/request 生成不同 CDI Device 条目。
- kubelet 按 request 给容器注入 CDI 时可自然完成隔离。

### 5.4 vGPU edits 结构拆分

#### Claim 级公共 edits

只包含与 claim 生命周期相关、与 allocation 无关的内容：

- 目录初始化（`claims/<uid>/...`）
- 固定 mount（watcher、driver、preload、host proc 等）

#### Allocation 级专属 edits

只包含该 result 对应份额应注入的信息：

- 可见设备变量
- 核心算力限制（`cores`）
- 显存限制（`memory`）
- 配置与状态目录挂载（`config`、`vgpu_lock`、`vmem_node`）

目录隔离策略：

- 目录路径使用 `claims/<claimUID>/<allocationKey>/...`。
- 每个 allocation 对应独立 host 子目录，再挂载到容器内固定路径。
- 这样即使多个容器共享同一个 Claim，也不会共享同一份 `config/lock/vmem` 状态文件。

这样避免了不同 request 的 env 混合覆盖。

### 5.5 作用域隔离（保证不影响其他设备）

仅在以下条件启用新逻辑：

- `featuregates.VGPUSupport` 打开
- `allocatableDevice.Type() == VGpuDeviceType`

其余设备类型继续走当前路径。


## 6. 代码改造清单（按阶段）

### 阶段 A（Step 1）：唯一键与 CDI 命名改造

目标：先解决“命名冲突与映射歧义”，不动 vGPU edits 聚合模型。

修改点：

1. `pkg/kubeletplugin/prepared.go`
   - 扩展 `PreparedDevice` 元字段（request/share/allocationKey）
2. `pkg/kubeletplugin/device_state.go`
   - 在 `prepareDevices` 中为每个 result 生成 `allocationKey`
   - 记录到 `PreparedDevice`
3. `pkg/kubeletplugin/cdi.go`
   - `GetClaimDeviceName(...)` 改签名为 `GetClaimDeviceName(claimUID, allocationKey string)`
   - `CreateClaimSpecFile(...)` 按 `allocationKey` 生成 `dspec.Name`

交付价值：

- 能稳定区分同 claim 内的多 allocation device entry。

### 阶段 B（Step 2）：vGPU edits 拆分

目标：解决多容器 request 配置串扰。

修改点：

1. `pkg/kubeletplugin/vgpu.go`
   - 新增 claim 级公共 edits 方法
   - 新增 allocation 级 edits 方法
   - claim 目录清理/创建只做一次（避免重复 RemoveAll）
2. `pkg/kubeletplugin/device_state.go`
   - `applySharingConfig` 不再对 vgpu 生成聚合 edits
   - per-result/per-device 绑定 allocation edits
3. `pkg/kubeletplugin/cdi.go`
   - spec-level 合并 claim common edits
   - device-level 追加 allocation edits

### 阶段 B.5（Step 2.5）：容器级路径隔离强化

目标：解决“不同容器引用同一 Claim 不同 request 时仍共享状态目录”的风险。

修改点：

1. `pkg/kubeletplugin/vgpu.go`
   - claim common edits 仅保留真正公共内容（driver/watcher/host proc/preload 等）
   - 将 `config` / `vgpu_lock` / `vmem_node` mount 下沉到 allocation edits
   - allocation mount host path 改为 `claims/<claimUID>/<allocationKey>/...`
2. `pkg/kubeletplugin/device_state.go`
   - 调用 allocation edits 时传入 `allocationKey`

交付价值：

- 同一 Claim 下不同 request 对应容器不再共享状态目录。
- 与 device-plugin 时代“每容器独立目录”的隔离语义保持一致。

交付价值：

- 不同 request 对应容器可获得独立 env 配置。

### 阶段 C（Step 3）：测试与回归

补充测试覆盖：

1. allocationKey 唯一性与稳定性
2. 同设备多 shareID 的 CDI 设备名唯一
3. 多容器共享 claim 的 request 隔离
4. 非 vgpu 设备回归

建议新增的最小单测断言：

1. `buildAllocationKey` 在有 `ShareID` 与无 `ShareID` 时都可稳定生成唯一键（含字符归一化）。
2. `GetClaimDeviceName` 在传入 `allocationKey` 时使用 allocation 级命名，未传入时回退 canonical 设备名。
3. 同一设备不同 `shareID` 对应的 `allocationKey` 可生成不同 CDI 名称。
4. `GetAllocationContainerEdits` 生成的 host mount 路径包含 `claims/<claimUID>/<allocationKey>/...`。
5. `GetClaimCommonContainerEdits` 不再包含 `config/vgpu_lock/vmem_node` 的状态目录挂载。
6. `GetAllocationContainerEdits` 的 env 内容与 `ConsumedCapacity` 一致（cores/memory/visible）。


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

### 风险 2：目录初始化时机变化导致竞态

- 影响：多个 result 并发准备时重复清理目录。
- 缓解：
  - 目录清理移动到 claim prepare 单点执行。
  - 复用现有 prepare/unprepare 锁与 checkpoint 锁。

### 风险 3：仅用 request 作为键不够唯一

- 影响：复合请求或子请求下可能碰撞。
- 缓解：
  - 优先 shareID。
  - 无 shareID 时追加 ordinal 兜底。


## 9. 验证方案

### 9.1 功能用例

1. 单容器单 request（现有能力回归）
2. 单容器多 request（回归）
3. 多容器共享同 claim，不同 request（目标场景）
4. 同设备多 shareID（consumable capacity）
5. ExactCount 多份额分配

### 9.2 关键断言

1. 每个容器仅收到其 request 对应 CDI Device
2. 环境变量中的 cores/memory/visible 不串扰
3. 同 device 不同 share 的 CDI 名称唯一
4. Unprepare 后目录与 CDI 临时文件正常清理


## 10. 最终结论

本次改造应分阶段推进，先完成 Step 1（allocationKey + CDI 命名）建立正确映射基础，再实施 Step 2（edits 拆分）与 Step 2.5（目录挂载隔离强化）解决配置串扰和状态共享风险，最后通过 Step 3 做回归收口。

该方案的优点是：

1. 精准解决多容器共享 Claim 的 vGPU request 隔离问题。
2. 充分利用 `ShareID`，与 DRA 可消耗容量模型语义一致。
3. 改动范围集中在 `kubeletplugin`，并可通过条件分支保证不影响现有非 vGPU 设备分配链路。
