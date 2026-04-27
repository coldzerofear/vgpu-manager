# Vulkan Implicit Layer 开发方案

> **状态**：设计已确认 / 未开发
> **参考**：[Project-HAMi/HAMi-core#182](https://github.com/Project-HAMi/HAMi-core/pull/182)
> **预计工作量**：~12-16 个工作日（library 5-7 + Go 部署 3-5 + 测试 2-3 + 文档 / 镜像 1-2）

---

## 1. 背景

vgpu-manager 当前的 vGPU 限额（内存、核数）通过 LD_PRELOAD 拦截 CUDA Driver API + NVML 实现。但 **Vulkan 应用**（Isaac Sim / Omniverse / 云游戏 / 部分仿真渲染管线 / Vulkan compute backends）的内存分配走的是 `vkAllocateMemory`，**完全独立于 CUDA driver**：

```
App ──[CUDA Driver API]──► libcuda.so   ← 我们当前 hook 这里
App ──[Vulkan API]────────► libvulkan.so ──► NVIDIA Vulkan ICD ──► /dev/nvidia*  ← 完全绕过我们
```

后果：vGPU 内存限制对 Vulkan workload **完全失效**。一个配置 4 GiB 显存上限的 Pod，跑 Isaac Sim 可以分配整张物理卡的内存。

HAMi-core 在 PR #182 引入了 Vulkan Implicit Layer 来填补这个洞，本文档将相同方案适配到 vgpu-manager。

## 2. 目标 / 非目标

### 目标
- Vulkan 应用 `vkAllocateMemory` 受同一份 vGPU 内存预算约束（与 CUDA 共享 `g_vgpu_config->devices[*].total_memory` 账本）
- `vkGetPhysicalDeviceMemoryProperties[2]` 返回 clamp 后的 heap size，让应用看到正确容量
- `vkQueueSubmit[2]` 接入现有 `rate_limiter` 实现核数节流
- `VkPhysicalDevice` 通过 deviceUUID 映射到我们的 `host_index`（与 CUDA / NVML 一致）
- CUDA-only 工作负载行为零变化（manifest `enable_environment` 控制）
- 同一份 `libvgpu-control.so` 同时承载 CUDA hook 与 Vulkan layer，部署不增加 mount point

### 非目标
- **不替代** CUDA hook，CUDA 路径继续工作
- **不**实现 OneAPI / ROCm / DirectX 等其它 GPU API 的拦截（本文档不涉及）
- **不**重写或抽象 `rate_limiter` 为通用 adapter（YAGNI；按需在 Vulkan 实现里直接调）
- **不**支持 Wayland / X11 上的 swapchain 内存计费（Vulkan 显示链路通常用 buffer 共享，不走 vkAllocateMemory）

## 3. 总体架构

### Implicit Layer 工作流

```
                     ┌────────────────────────────────────┐
应用启动              │                                    │
  │                  │ /etc/vulkan/implicit_layer.d/      │
  ▼                  │   vgpu_manager_implicit_layer.json │
libvulkan.so loader ─┤                                    │
  │                  │ enable_environment:                │
  │                  │   VGPU_VULKAN_ENABLE=1             │
  │                  │ library_path: libvgpu-control.so   │
  │                  └────────────────────────────────────┘
  │
  ▼
loader 调 vkNegotiateLoaderLayerInterfaceVersion
loader 调 vkGetInstanceProcAddr 拿我们各 hook 的指针
loader 串成 dispatch chain：
  app ── 我们 ── (其它 layer) ── NVIDIA ICD
  │
  ▼
应用调 vkAllocateMemory(...)
  │
  ▼
loader 路由到我们的 vk_AllocateMemory hook
  │
  ▼
查 g_vgpu_config 物理预算 (real_memory)  → 通过 ↓ / 拒绝 ↓
  used 视图通过 NVML 进程聚合获得，          │      │
  自动包含 Vulkan 已用的物理内存            │      └─► 返回 VK_ERROR_OUT_OF_DEVICE_MEMORY
                                            ▼
                              forward 到下一层（最终 ICD 物理分配）
                                            │
                                            ▼
                              成功后 NVML 自动反映新 used，
                              下次查询自动累加（不需要我们维护计数器）
```

### 关键设计点
- 不依赖 LD_PRELOAD：Vulkan loader 自己处理符号路由
- 不污染纯 CUDA 进程：env 控制启用，CUDA-only 应用 loader 不会加载我们的 layer 代码
- 单一 `.so` 双角色：`libvgpu-control.so` 同时被 CUDA LD_PRELOAD 和 Vulkan loader 加载，**两条路径独立、互不干扰**
- **物理-only 模型**：Vulkan 没有 CUDA `cuMemAllocManaged` / UVA 等价物，所有 `vkAllocateMemory` 走物理 heap。预算判断必须以 `real_memory`（物理切片）为上限，不能用 `total_memory`（CUDA oversold 视图）
- **used 视图天然共享**：vgpu-manager 的 `get_used_gpu_memory_by_device` 通过 NVML 同时收 compute + graphics 进程列表，**Vulkan 已分配的物理内存天然包含在 `used` 里**。Vulkan layer 不需要自己维护分配计数器

## 4. 可复用资产盘点

vgpu-manager 大部分 Vulkan layer 需要的设施**已经存在**，下表列出每项及其文件位置：

| 资产 | 位置 | Vulkan 用法 |
|---|---|---|
| `g_vgpu_config->devices[host_index].total_memory` 等预算字段 | [include/hook.h](../library/include/hook.h) | 单一账本，CUDA 与 Vulkan 共享，**无需新表** |
| `get_host_device_index_by_uuid(char *uuid_str, int *out)` | [src/loader.c:1670](../library/src/loader.c#L1670) | 接受 `"GPU-xxxx-..."` 字符串。`VkPhysicalDeviceIDProperties::deviceUUID` 是 16 字节二进制，需要新增一个 `_bytes` 变体（详见 Phase 0） |
| `get_host_device_index_by_nvml_device(nvmlDevice_t)` | [src/loader.c:1679](../library/src/loader.c#L1679) | 缓存型查表 + UUID fallback |
| `prepare_memory_allocation()` + `MEMORY_PATH_GPU/UVA/OOM` | [src/cuda_hook.c:93](../library/src/cuda_hook.c#L93) | 内存分配决策中心。**Vulkan 调用时必须强制走 GPU 路径**（`allow_uva=0`），UVA 仅 CUDA 适用 |
| `get_used_gpu_memory_by_device()` 通过 NVML 同时收 compute + graphics 进程 | [src/cuda_hook.c:807](../library/src/cuda_hook.c#L807) | **Vulkan 已分配的物理内存天然包含在 `used` 视图里** —— graphics 进程列表覆盖 Vulkan / OpenGL / DirectX。Vulkan layer **不需要自己维护分配计数器**（`compute/graphics` PID 去重已修复，commit `20e9519`）|
| `malloc_gpu_virt_memory` / `free_gpu_virt_memory` | src/cuda_hook.c | 虚拟内存账本，**Vulkan 不用** —— Vulkan 没有 UVA 等价物 |
| `rate_limiter(grids, blocks, device)` | [src/cuda_hook.c:308](../library/src/cuda_hook.c#L308) | `vkQueueSubmit` 节流复用。注意 `blocks` 参数当前 `(void)blocks` 未用 |
| `lib_control = dlopen("libvgpu-control.so")` | [src/loader.c:1082](../library/src/loader.c#L1082) | 已经把自己当 layer 用，与 Vulkan implicit layer 模型契合 |
| `nvmlDeviceGetUUID` / `nvmlDeviceGetHandleByUUID` | [src/loader.c:711, 764](../library/src/loader.c#L711) 派发表已注册 | 不需要扩 NVML 符号 |
| `load_necessary_data()` lazy idempotent 初始化 | [src/loader.c:2140](../library/src/loader.c#L2140) | Vulkan 入口直接调，重复调零成本 |
| `init_nvml_to_host_device_index()` | [src/loader.c:2093](../library/src/loader.c#L2093) | NVML init + 建 device 映射，已被 `load_necessary_data` 涵盖 |
| metrics / lock / VGPU_MANAGER_PATH / pthread 工具 | 全库 | 不动 |

**结论**：库侧大部分功能不需要新增，只需要写适配层把 Vulkan 的入参映射到现有 API。

## 5. 命名规范（早定下来）

| 项 | 名字 |
|---|---|
| 启用环境变量 | `VGPU_VULKAN_ENABLE=1` |
| 禁用环境变量 | `VGPU_VULKAN_DISABLE=1`（覆盖启用）|
| Manifest 文件名 | `vgpu_manager_implicit_layer.json` |
| Layer 名（manifest `name` 字段）| `VGPU_MANAGER_implicit_memory_budget` |
| 安装路径（容器内）| `/etc/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json` |
| `library_path`（manifest 字段）| `libvgpu-control.so`（相对名，依赖 ld.so.cache / LD_LIBRARY_PATH）|
| CMake 选项 | `BUILD_VULKAN_LAYER` (default `OFF`) |
| Layer 模块代码目录 | `library/src/vulkan/` |
| Layer 测试目录 | `library/test/vulkan/` |
| Manifest 源文件位置 | `library/deploy/vgpu_manager_implicit_layer.json` |

## 6. 阶段化开发

下面 10 个 phase 按依赖顺序排列，可分别提交独立 commit。每个 phase 都有验收标准。

**前置 bug 修复（已完成，独立于 Vulkan 立项）**：

| Phase | 内容 | 状态 |
|---|---|---|
| 0.5 | `get_used_gpu_memory_by_device` PID 去重，避免 CUDA + Vulkan 混合进程 used 翻倍 | ✅ 已修复 commit `20e9519` |

**Vulkan layer 阶段**：

| Phase | 内容 | 预估 |
|---|---|---|
| 0   | 接口前置铺垫（UUID binary 反查 + budget API 提取 + `vgpu_budget_kind_t`，**无悔重构**）| 0.5 天 |
| 1   | CMake / `BUILD_VULKAN_LAYER` 开关 + 空骨架 | 0.5 天 |
| 2   | Vulkan layer skeleton（`vkNegotiateLoaderLayerInterfaceVersion` + dispatch chain）| 1.5 天 |
| 3   | `VkPhysicalDevice` → `host_index` 映射 | 0.5 天 |
| 4   | Memory properties clamp 到 `real_memory`（不是 `total_memory`）| 0.5 天 |
| 5   | 内存分配预算（`vkAllocateMemory` / `vkFreeMemory`，`KIND_PHYSICAL`）| 1 天 |
| 5b  | CUDA-Vulkan interop import 路径检测（避免双重计费）| 0.5 天 |
| 6   | 队列节流（`vkQueueSubmit[2]`）| 1 天 |
| 7   | Manifest 与构建集成 | 0.5 天 |
| 8   | Go 侧（device-plugin / webhook / kubelet-plugin / 用户文档）| 3-5 天 |
| 9   | 测试（mock dispatch 单测 + 集成测）| 2-3 天 |

合计：library + 部署 ~10-13 天。

---

### Phase 0：接口前置铺垫（无悔重构）

**目标**：把后续 Vulkan layer 需要的两个公共能力从 file-static 提取出来，**不引入 Vulkan 依赖**，CUDA 路径行为不变。

**变更**：

1. **加 binary UUID 反查** — [src/loader.c](../library/src/loader.c) 新增：
   ```c
   /* binary 16-byte UUID -> host_index. Vulkan 的 VkPhysicalDeviceIDProperties
    * ::deviceUUID 是这个格式，NVML 的字符串 UUID 也是它的格式化呈现。 */
   void get_host_device_index_by_uuid_bytes(const uint8_t uuid[16],
                                            int *host_index);
   ```
   实现：先把 16 字节按 NVIDIA 标准格式化成 `GPU-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx` 字符串，复用现有 `get_host_device_index_by_uuid`。约 30 行。

2. **抽预算检查与计账接口** — 新增 [include/budget.h](../library/include/budget.h)：
   ```c
   /* 内部预算 API。CUDA 和 Vulkan hook 都通过这个接口调用,避免任一侧
    * 直接持有 cuda_hook.c 的 file-static 实现细节。所有函数线程安全。 */

   typedef enum {
     /* CUDA 视图:含 oversold UVA 容量,上限 g_vgpu_config[*].total_memory。
      * 允许的 caller:cuMemAlloc / cuMemAllocManaged / cuMemAllocAsync /
      * cuArrayCreate / cuMemCreate 等所有 CUDA 内存 API hook。 */
     VGPU_BUDGET_KIND_VIRTUAL = 0,

     /* 物理硬上限,上限 g_vgpu_config[*].real_memory。
      * 允许的 caller:vkAllocateMemory(无 UVA 概念)、未来其它纯物理 GPU
      * API。检查时 allow_uva 必须传 0。 */
     VGPU_BUDGET_KIND_PHYSICAL = 1,
   } vgpu_budget_kind_t;

   vgpu_path_t vgpu_check_alloc_budget(int host_index, size_t request_size,
                                       vgpu_budget_kind_t kind,
                                       int allow_uva,            /* 仅 VIRTUAL kind 有意义 */
                                       int *lock_fd_out);
   ```

   实现要点：
   - `prepare_memory_allocation` 与 `vgpu_check_alloc_budget` 在 cuda_hook.c 共用静态决策助手 `decide_path_with_used`，cap 选择 / oversold 判定逻辑只有一份
   - `vgpu_check_alloc_budget` 内部通过 `get_nvml_device_by_host_index`（host_index → uuid → `nvmlDeviceGetHandleByUUID`）取 nvmlDevice_t，**不**走 `nvml_to_host_device_index[]` 反查
   - **`PHYSICAL` 路径强制 `allow_uva = 0`**，永远不返回 `VGPU_PATH_UVA`
   - **不**新增 `vgpu_account_alloc_gpu/free` 等 metrics-hook 占位函数：`used` 视图来自 NVML 进程聚合，分配成功后驱动自动把 PID 暴露给 NVML，无需 hook 自己维护账本（与 HAMi PR #182 的 `mem_entry_t` 链表方案不同）

   约 80 行重构。

3. **同步 hack 检查** — 不影响 `check_cuda_hook_consistency.py`（只动私有函数可见性，不改派发表 / enum）。

**验收**：
- [ ] `make check` 通过（hook consistency / struct layout）
- [ ] CUDA 路径所有现有 wrapper 行为不变（gcc -fsyntax-only 通过 + `library/test/test_*.c` 全部链接成功）
- [ ] 一个独立 commit `library: extract budget API for cross-API reuse`

**预估**：0.5 天

---

### Phase 1：CMake / 构建系统

**目标**：加可选的 Vulkan-Headers 依赖、空目录骨架、配置开关。**不写 Vulkan 业务代码**。

**变更**：

1. [library/CMakeLists.txt](../library/CMakeLists.txt) 新增：
   ```cmake
   option(BUILD_VULKAN_LAYER "Build the Vulkan implicit layer module" OFF)

   if(BUILD_VULKAN_LAYER)
     find_package(Vulkan REQUIRED)
     # 仅需 headers,不链 libvulkan(layer 通过 dispatch chain 调下层)
     add_library(vgpu_vulkan_mod OBJECT
       src/vulkan/layer.c
       src/vulkan/dispatch.c
       src/vulkan/physdev_index.c
       src/vulkan/hooks_alloc.c
       src/vulkan/hooks_memory.c
       src/vulkan/hooks_submit.c)
     target_include_directories(vgpu_vulkan_mod
       PRIVATE ${Vulkan_INCLUDE_DIRS} ${CMAKE_SOURCE_DIR})
     target_compile_definitions(vgpu_vulkan_mod PRIVATE VK_USE_PLATFORM_XLIB_KHR=0)
     target_sources(vgpu-control PRIVATE $<TARGET_OBJECTS:vgpu_vulkan_mod>)
     target_compile_definitions(vgpu-control PRIVATE VGPU_VULKAN_LAYER_ENABLED)
   endif()
   ```

2. [library/Makefile](../library/Makefile) 加目标：
   ```makefile
   .PHONY: build-vulkan
   build-vulkan: ## Build with Vulkan implicit layer enabled.
       cd $(BUILD_DIR) && CUDA_HOME=$(CUDA_HOME) $(CMAKE) \
         -DCMAKE_BUILD_TYPE=$(CMAKE_BUILD_TYPE) \
         -DBUILD_VULKAN_LAYER=ON $(LIBRARY_DIR)
       $(MAKE) -C $(BUILD_DIR) -j$(J) vgpu-control
   ```

3. 创建空文件 `src/vulkan/{layer,dispatch,physdev_index,hooks_alloc,hooks_memory,hooks_submit}.c`，每个 stub 一个 `int __vgpu_vulkan_stub = 0;` 全局，让 OBJECT library 编得过。

4. README 加一段说明可选编译。

**验收**：
- [ ] `make build` 默认依然不需要 Vulkan-Headers（CUDA-only 用户零额外依赖）
- [ ] `make build-vulkan` 在带 Vulkan-Headers 的环境上能编出 `.so`
- [ ] 不带 Vulkan-Headers 时 `make build-vulkan` 给出清晰错误信息（`find_package(Vulkan REQUIRED)` 自带）
- [ ] `nm libvgpu-control.so` 在 `BUILD_VULKAN_LAYER=OFF` 时不含任何 `vk*` 符号

**预估**：0.5 天

---

### Phase 2：Vulkan layer 骨架

**目标**：实现 Vulkan loader interface 协议、dispatch chain、instance/device 创建拦截。**所有应用调用都 forward 到下一层**，业务逻辑零。这一阶段实现完应能加载我们的 layer 但行为透明。

**新文件**：

#### [library/src/vulkan/layer.h](../library/src/vulkan/layer.h)
```c
#ifndef VGPU_VULKAN_LAYER_H
#define VGPU_VULKAN_LAYER_H
#include <vulkan/vulkan.h>

/* Loader negotiate entry — 必须 export */
VK_LAYER_EXPORT VkResult VKAPI_CALL
vkNegotiateLoaderLayerInterfaceVersion(VkNegotiateLayerInterface *pVersionStruct);

/* Per-instance / per-device proc lookup */
VK_LAYER_EXPORT PFN_vkVoidFunction VKAPI_CALL
vk_layer_GetInstanceProcAddr(VkInstance instance, const char *pName);

VK_LAYER_EXPORT PFN_vkVoidFunction VKAPI_CALL
vk_layer_GetDeviceProcAddr(VkDevice device, const char *pName);

#endif
```

#### [library/src/vulkan/dispatch.h](../library/src/vulkan/dispatch.h)
```c
typedef struct {
  VkInstance                                       instance;
  PFN_vkGetInstanceProcAddr                        pfn_GetInstanceProcAddr;
  PFN_vkDestroyInstance                            pfn_DestroyInstance;
  PFN_vkEnumeratePhysicalDevices                   pfn_EnumeratePhysicalDevices;
  PFN_vkGetPhysicalDeviceMemoryProperties          pfn_GetPhysicalDeviceMemoryProperties;
  PFN_vkGetPhysicalDeviceMemoryProperties2         pfn_GetPhysicalDeviceMemoryProperties2;  /* nullable on 1.0 */
  PFN_vkGetPhysicalDeviceProperties2               pfn_GetPhysicalDeviceProperties2;        /* for ID props */
} vgpu_instance_dispatch_t;

typedef struct {
  VkDevice                                         device;
  VkPhysicalDevice                                 physical_device;
  PFN_vkGetDeviceProcAddr                          pfn_GetDeviceProcAddr;
  PFN_vkDestroyDevice                              pfn_DestroyDevice;
  PFN_vkAllocateMemory                             pfn_AllocateMemory;
  PFN_vkFreeMemory                                 pfn_FreeMemory;
  PFN_vkQueueSubmit                                pfn_QueueSubmit;
  PFN_vkQueueSubmit2                               pfn_QueueSubmit2;        /* nullable */
  PFN_vkQueueSubmit2KHR                            pfn_QueueSubmit2KHR;     /* nullable */
} vgpu_device_dispatch_t;

vgpu_instance_dispatch_t *vgpu_get_instance_dispatch(VkInstance instance);
vgpu_device_dispatch_t   *vgpu_get_device_dispatch  (VkDevice device);
void                      vgpu_register_instance(VkInstance, vgpu_instance_dispatch_t *);
void                      vgpu_register_device  (VkDevice,   vgpu_device_dispatch_t *);
void                      vgpu_remove_instance  (VkInstance);
void                      vgpu_remove_device    (VkDevice);
```

实现用 fixed-size hash table（开放寻址，进程内最多几十个 instance/device，完全够）。线程安全用 `pthread_rwlock_t`。

#### [library/src/vulkan/layer.c](../library/src/vulkan/layer.c)
- `vkNegotiateLoaderLayerInterfaceVersion`：协商版本，注册我们的 GetInstanceProcAddr
- `vk_layer_CreateInstance`：调链下一层 → 拿到 VkInstance → 填 dispatch table → 注册
- `vk_layer_DestroyInstance`：注销 + 转发
- `vk_layer_CreateDevice`：同样模式但 per-device
- `vk_layer_DestroyDevice`：同样
- `vk_layer_GetInstanceProcAddr`：返回我们 hook 的指针；其它转发
- `vk_layer_GetDeviceProcAddr`：同上

注意点：
- **必须用 `VK_LAYER_EXPORT`** 修饰所有 loader 入口（即 default visibility）
- **`vkCreateInstance` 入口调 `load_necessary_data()`**——Phase 3 开始 Vulkan 路径会读 `g_vgpu_config`（physdev UUID → host_index 解析），必须先初始化。`pthread_once` 保证幂等。原文档此处早期写"不调"是基于 Phase 2 不触达 vgpu 状态的假设，Phase 3 起改为必调。代价仅是纯 Vulkan 应用首次 `vkCreateInstance` 多 dlopen 一次 libcuda（~8 MB / ~10 ms），NVIDIA 系统上 libcuda 本来就在 host，可忽略
- **已知前提**：`VGPU_VULKAN_ENABLE=1` 隐含要求 `libcuda.so.1` 可在容器内 ld.so 路径上找到。vgpu-manager 的 device-plugin + NVIDIA Container Toolkit 在正常部署中保证这一点（device-plugin 把 env 跟 libcuda mount 一同交付）。**手动设 env 而 lib 缺失的情况下，`vk_layer_CreateInstance` 内部 `load_cuda_libraries` dlopen 失败会触发 `LOGGER(FATAL,...)` → `exit(1)`**，应用启动直接死。这个 fail-fast 行为是有意的（部署事故越早暴露越好），跟 HAMi PR #182 的 lazy 模式（lib 缺失时 layer 退化为 no-op）有意识地不同——HAMi 不假设 deployment 契约，我们假设 device-plugin 控制部署链。如果将来支持 vgpu-manager-外部的 Vulkan workload，需要重新评估这条假设。
- **dispatch table 的 chain info** 通过 `VkLayerInstanceCreateInfo` 的 `pNext` 链拿，必须严格按 Vulkan loader spec 处理

**验收**：
- [ ] `vkNegotiateLoaderLayerInterfaceVersion` 协议测试通过（mock loader struct）
- [ ] 把 manifest 拷到 `/etc/vulkan/implicit_layer.d/`、设 `VGPU_VULKAN_ENABLE=1`，跑任意简单 Vulkan demo（`vulkaninfo`）能正常输出，无 crash
- [ ] 不开 env，`vulkaninfo` 行为不变
- [ ] `vulkaninfo --validate` 不报 layer 自身错误

**预估**：1.5 天

---

### Phase 3：VkPhysicalDevice → host_index 映射

**目标**：通过 `VkPhysicalDeviceIDProperties::deviceUUID` 把 Vulkan 物理设备对应到我们的 `host_index`。**这一阶段所有 hook 内部都能拿到 host_index**，但仍不做 budget enforcement。

**新文件**：

#### [library/src/vulkan/physdev_index.h](../library/src/vulkan/physdev_index.h)
```c
/* 缓存型查询。失败返回 -1。线程安全。 */
int vgpu_vk_physdev_to_host_index(VkPhysicalDevice phys);
```

#### [library/src/vulkan/physdev_index.c](../library/src/vulkan/physdev_index.c)
- 维护 `VkPhysicalDevice` → `host_index` 哈希表
- 首次查询：调 dispatch table 的 `vkGetPhysicalDeviceProperties2` 拿 `VkPhysicalDeviceIDProperties::deviceUUID`
- 调 Phase 0 加的 `get_host_device_index_by_uuid_bytes`
- 写回缓存

**验收**：
- [ ] `vulkaninfo` 时（在带 NVIDIA 卡的机器上）日志能打出 `vk physical device 0xxxx => host device N`
- [ ] 多次查询同一 phys 命中缓存，`get_host_device_index_by_uuid_bytes` 只调一次

**预估**：0.5 天

---

### Phase 4：Memory properties clamp

**目标**：Hook `vkGetPhysicalDeviceMemoryProperties` / `_2`，把 device-local heap size clamp 到 vGPU 的物理可用上限。

**新文件**：[library/src/vulkan/hooks_memory.c](../library/src/vulkan/hooks_memory.c)

实现要点：
- 调下层拿原始 `VkPhysicalDeviceMemoryProperties`
- 判定哪些 heap 是 device-local（`VK_MEMORY_HEAP_DEVICE_LOCAL_BIT`）
- **对 device-local heap：`heap.size = min(heap.size, cap)`**，其中 `cap` 由 oversold 状态决定（与 `cuMemGetInfo` 路径形态对称）：
  - **oversold ON**：`cap = real_memory`（物理切片）—— Vulkan 没有 UVA 等价物，UVA 容量永远不可达
  - **oversold OFF**：`cap = total_memory`（按 `g_vgpu_config` invariant 等于 `real_memory`）—— 同 cuMemGetInfo 在非 oversold 路径的取值
- host-visible / staging heap 不动
- non-NVIDIA 设备（host_index < 0）/ memory_limit 未配（== 0）不动

**为什么这样选 cap**：

CUDA 的 `cuMemGetInfo` 在 oversold ON 时报 `total_memory`（含 UVA 容量），因为 `cuMemAllocManaged` 真能给出超物理容量。Vulkan 没有等价机制——`vkAllocateMemory` 总是物理分配——所以 oversold ON 时必须给 Vulkan 看 `real_memory`。oversold OFF 时 `total_memory == real_memory`（config 保证），picking 哪个都行；选 `total_memory` 是为了跟 cuMemGetInfo 那条 `actual_total = total_memory` 的非 oversold 分支结构一致，code review 时一眼看出"两条 hook 是一对"。

**验收**：
- [ ] 在 4 GiB 限额的 Pod 内跑 `vulkaninfo`，device-local heap size 显示 4 GiB（而不是物理 24 GiB）
- [ ] CPU 可见 heap 不被 clamp
- [ ] oversold ON 配置下 heap size 报 `real_memory`，oversold OFF 配置下报 `total_memory`（按 invariant 二者相等）
- [ ] non-NVIDIA 设备（如 CPU vendor）heap 不动

**预估**：0.5 天

---

### Phase 5：内存分配预算

**目标**：Hook `vkAllocateMemory` / `vkFreeMemory`，把 Vulkan 的设备内存分配纳入与 CUDA 共用的预算账本，并通过 Phase 5b 的 import 检测处理 CUDA-Vulkan interop 的 OOM 误判。

**新文件**：[library/src/vulkan/hooks_alloc.h](../library/src/vulkan/hooks_alloc.h) + [library/src/vulkan/hooks_alloc.c](../library/src/vulkan/hooks_alloc.c)

#### 5.1 Budget API 与决策共用

[include/budget.h](../library/include/budget.h) 新增 `vgpu_check_alloc_budget(host_index, size, kind, allow_uva, *lock_fd)`，返回 `vgpu_path_t` (GPU/UVA/OOM)。

实现位于 [src/cuda_hook.c](../library/src/cuda_hook.c)，与 `prepare_memory_allocation` 共用静态决策助手 `decide_path_with_used`，保证两条 API 路径在 cap 选择和 oversold 判定上绝对同步。差异仅在输入面：

| 函数 | 输入 | 用法 |
|---|---|---|
| `prepare_memory_allocation` | CUdevice | CUDA 11 个分配 hook 全部沿用，**签名/行为零变化** |
| `vgpu_check_alloc_budget` | host_index | Vulkan 路径专用，host_index 已由 Phase 3 的 deviceUUID 解析得到 |

`vgpu_check_alloc_budget` 内部通过新加的 [`get_nvml_device_by_host_index`](../library/src/loader.c)（host_index → `g_vgpu_config[*].uuid` 字符串 → `nvmlDeviceGetHandleByUUID`）取 nvmlDevice_t，**不依赖** `nvml_to_host_device_index[]` 反查表（该表只在 CUDA bootstrap 路径填充，纯 Vulkan 应用不会触达）。

#### 5.2 NVML 初始化前置

`load_necessary_data()` 只 dlsym，**不**调 `nvmlInit_v2`。真正的 NVML init 在 `init_nvml_to_host_device_index` 里，由 `init_devices_mapping()` 通过 pthread_once 触发。

`vk_layer_CreateInstance` 在 `next_create` **成功之后**追加调用 `init_devices_mapping()`：

```c
VkResult result = next_create(...);
if (result != VK_SUCCESS) return result;   // 失败不 init
... // dispatch 表 + physdev 缓存
init_devices_mapping();                    // 前置 NVML 准备
return VK_SUCCESS;
```

next_create 失败说明下层 NVIDIA ICD 没起来，这时跑 `init_devices_mapping` 会触发 `LOGGER(FATAL,...)` exit，不应进入。pthread_once 包装保证多次调用安全。

#### 5.3 vk_layer_AllocateMemory 流程

1. 查 `vgpu_device_dispatch_t`，拿到 `pfn_AllocateMemory` + `physical_device`
2. `host_index = vgpu_vk_physdev_to_host_index(physical_device)`
3. **import 短路**（Phase 5b）：`vk_alloc_is_import(pAllocateInfo)` → 直接 forward，不查预算
4. `vgpu_check_alloc_budget(host_index, allocationSize, VGPU_BUDGET_KIND_PHYSICAL, /*allow_uva*/ 0, &lock_fd)`
   - **kind 必须是 PHYSICAL**：cap 走 `real_memory`，与 Phase 4 报给应用的 heap size 一致
   - **`allow_uva` 必须是 0**：Vulkan 没有 UVA 概念。`vgpu_check_alloc_budget` 内部对 PHYSICAL 强制清零 allow_uva 作为防御
   - 不受 vgpu 管理的设备（host_index<0 / memory_limit==0 / NVML 解析失败）→ 返回 GPU + lock_fd=-1，自动跳过 enforcement
5. 路径判定：
   - OOM → `unlock_gpu_device` → `VK_ERROR_OUT_OF_DEVICE_MEMORY`
   - GPU → forward 到下一层 → `unlock_gpu_device` → 透传 `VkResult`

#### 5.4 vk_layer_FreeMemory 流程

直接 forward，不维护任何 per-allocation 状态。**这与 HAMi PR #182 的 `mem_entry_t` 链表方案不同**——HAMi 用手动计数器，必须 add/release 配对；vgpu 的 `used` 视图来自 NVML 进程聚合，free 后下次 NVML 查询自动反映释放，不需要 hook 维护账本。

> **不实现 `vgpu_account_alloc_gpu/free` 占位 hook**。早期方案讨论过为 metrics 预留接口，最终判定为 dead code（NVML 路径已自动覆盖），按 YAGNI 删除。

#### 5.5 注意点

- Vulkan 的 `VkDeviceMemory` 是 64-bit opaque handle，free 路径不需要 size 信息
- `pAllocator` 字段透传
- **CUDA + Vulkan 累加在 NVML 视图自动完成**，PID 去重已在 commit `20e9519` 修复，混合应用（Isaac Sim 等）的 `used` 不会被双重计数
- 唯一被双重计数的危险路径是 import（Phase 5b 处理）

#### 5.6 验收

- [ ] 单元测：mock dispatch，Vulkan 单次 alloc 5 GiB / 限额 4 GiB → `VK_ERROR_OUT_OF_DEVICE_MEMORY`
- [ ] 单元测：`pNext` 链含 `VkImportMemoryFdInfoKHR(fd>=0, handleType!=0)` → 短路 forward，未触及预算
- [ ] 单元测：`pNext` 链含 `VkImportMemoryFdInfoKHR(fd=-1)` → **不**短路，正常进预算检查（防探测）
- [ ] 集成测（带 GPU）：4 GiB Pod 跑 vkAllocateMemory loop，总量受限
- [ ] 集成测：CUDA cuMemAlloc 2 GiB → Vulkan 后续可再分配最多 2 GiB（PID 去重 + 预算共享生效）
- [ ] 集成测：CUDA cuMemAlloc 4 GiB + 导出 fd → Vulkan import 4 GiB → 不报 OOM（Phase 5b 生效）

**预估**：1 天

---

### Phase 5b：CUDA-Vulkan interop import 路径检测

**目标**：识别 `vkAllocateMemory` 的 `pNext` 链中"导入已存在外部内存"的结构，对这类调用短路 forward，避免预算公式中物理内存被算两次。

#### 为什么需要

CUDA-Vulkan interop 通过 `VK_KHR_external_memory` 跨 API 共享内存。Isaac Sim、Omniverse、视频渲染管线、`vk_video_*` sample 都走这套：

```
T0  CUDA 侧:cuMemAlloc(4 GiB)  +  cuMemExportToShareableHandle → fd
    ──► 物理 +4 GiB,NVML 视图反映 +4 GiB
T1  Vulkan 侧:vkAllocateMemory + VkImportMemoryFdInfoKHR(fd, 4 GiB)
    ──► 物理 +0,只是给 T0 那块物理内存多挂一个 VkDeviceMemory handle
```

预算公式 `(used + vmem_used + request_size) > cap`：
- `used`（NVML）= 4 GiB ✓ 正确
- `request_size` = 4 GiB（声明 import 4 GiB）
- 4 + 4 = 8 GiB → 若 cap=5 GiB 则误判 OOM

加号两侧实际是同一块物理内存（NVML used 反映底层、request_size 是上面新挂 handle 的声明大小），**这就是"算两次"**。普通 alloc 不会触发——hook 在真实分配**之前**跑，那时 NVML used 还没反映新分配。

#### 与 HAMi PR #182 的差异

HAMi PR #182 **没做** import 检测，每次 vkAllocateMemory 都加手动计数器。HAMi 也有等价的双计 bug（CUDA add 4 GiB + Vulkan import add 4 GiB = counter 8 GiB），他们暂未修复。vgpu 选择修复，因为 Isaac Sim / Omniverse 在 K8s 上是 vgpu 的核心场景之一。

#### 实现

[library/src/vulkan/hooks_alloc.c](../library/src/vulkan/hooks_alloc.c) 中的 `vk_alloc_is_import`：

```c
static int vk_alloc_is_import(const VkMemoryAllocateInfo *info) {
  if (info == NULL) return 0;
  for (const VkBaseInStructure *p = (const VkBaseInStructure *)info->pNext;
       p != NULL; p = p->pNext) {
    switch ((int)p->sType) {
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR: {
        const VkImportMemoryFdInfoKHR *imp = (const VkImportMemoryFdInfoKHR *)p;
        if (imp->fd >= 0 && imp->handleType != 0) return 1;
        break;
      }
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_HOST_POINTER_INFO_EXT: {
        const VkImportMemoryHostPointerInfoEXT *imp =
            (const VkImportMemoryHostPointerInfoEXT *)p;
        if (imp->pHostPointer != NULL && imp->handleType != 0) return 1;
        break;
      }
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_WIN32_HANDLE_INFO_KHR:
      case VK_STRUCTURE_TYPE_IMPORT_ANDROID_HARDWARE_BUFFER_INFO_ANDROID:
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_ZIRCON_HANDLE_INFO_FUCHSIA:
        return 1;     // 跨平台扩展,Linux 容器场景不会出现,仅作防御
      default: break;
    }
  }
  return 0;
}
```

`VkExportMemoryAllocateInfo` 是 export，仍是新物理分配，**不**在 import 列表里。

#### Bypass 不可能性论证（False-positive 安全分析）

恶意应用试图用伪造 import 绕过预算的所有路径：

| 攻击 | driver 行为 | 实际效果 |
|---|---|---|
| `fd = -1` 或 `handleType = 0` | spec 要求 driver 拒绝；同时我们的 fd>=0/handleType!=0 哨兵也会拦下，走正常预算 | 没有物理分配，没有 bypass |
| `fd` 来自不属于自己的 VkDeviceMemory | driver 验证 handleType / size / 权限不匹配，拒绝 | 没有物理分配，没有 bypass |
| 先 cuMemAlloc(1 KiB) 拿 fd，伪造 `allocationSize = 4 GiB` import | spec 要求 import 时 allocationSize 必须匹配源，driver 拒绝 | 没有物理分配，没有 bypass |
| 合法地 cuMemAlloc(4 GiB) → 重复 import N 次 | 全部短路（**正确**：物理只占 4 GiB） | 物理上仍然 4 GiB，没有 bypass |

根因：Vulkan loader/driver 强制 import 必须有合法外部 handle，**合法 handle 只能来自此前已计费的某次分配**（CUDA cuMemAlloc / Vulkan vkAllocateMemory）。验证责任在 driver。我们额外的 `fd>=0 / pHostPointer!=NULL` 哨兵把"看起来像 import 的探测请求"也挡到正常预算分支，避免它成为预算状态探测 oracle。

#### 验收

- [ ] 单元测：每种已识别 import sType（fd / host_pointer）合法值 → 短路
- [ ] 单元测：fd=-1 / handleType=0 / pHostPointer=NULL → 不短路
- [ ] 集成测（带 GPU）：CUDA cuMemAlloc 4 GiB → 导出 fd → Vulkan import 4 GiB → 后续 CUDA + Vulkan 总占用仍 4 GiB（限额=4 GiB 时 import 不报 OOM；限额=3 GiB 时 CUDA 那一步就报 OOM，与 import 无关）

**预估**：0.5 天

---

### Phase 6：队列节流

**目标**：Hook `vkQueueSubmit` / `vkQueueSubmit2` / `vkQueueSubmit2KHR`，每次提交前过现有的 SM rate_limiter，实现 Vulkan compute 工作负载的核数限流，与 CUDA 路径共享同一份 token bucket 与 watcher 线程。

**新文件**：
- [library/src/vulkan/queue_index.h](../library/src/vulkan/queue_index.h) / [.c](../library/src/vulkan/queue_index.c) — VkQueue → VkDevice 缓存
- [library/src/vulkan/hooks_submit.h](../library/src/vulkan/hooks_submit.h) / [.c](../library/src/vulkan/hooks_submit.c) — 5 个 hook：GetDeviceQueue / GetDeviceQueue2 / QueueSubmit / QueueSubmit2 / QueueSubmit2KHR（最后两个共用一个函数）

#### 6.1 阻塞点：纯 Vulkan 进程不会触发 `initialization()`

CUDA 侧 [`rate_limiter`](../library/src/cuda_hook.c) 依赖：
- `g_total_cuda_cores[host_index]`（容量上限）—— 由 `init_device_cuda_cores` 填，仅在 `initialization()` 内调用
- `utilization_watcher` 线程 —— 由 `initialization()` 经 `balance_batches` / `active_utilization_notifier` 启动
- `g_init_set` pthread_once 触发点 —— 仅在 CUDA bootstrap 入口（`cuInit` / `cuGetProcAddress[_v2]` / `cuDriverGetVersion`）

纯 Vulkan 进程从不触发以上任何入口，导致：
- `g_total_cuda_cores` 全 0，watcher 线程未起
- 第一次 vkQueueSubmit 调 rate_limiter → CAS 把 `g_cur_cuda_cores` 减到 -1
- 第二次提交在 `nanosleep` 重试循环中**永远阻塞**（watcher 永不存在，token 永不补充）

#### 6.2 解法：vk_layer_CreateInstance 显式触发 watcher 启动

[budget.h](../library/include/budget.h) 新增 `vgpu_ensure_sm_watcher_started()`：
```c
void vgpu_ensure_sm_watcher_started(void) {
  pthread_once(&g_init_set, initialization);
}
```

`vk_layer_CreateInstance` 在 next_create 成功后追加调用（顺序在 `init_devices_mapping()` 之后）：

```c
init_devices_mapping();              // Phase 5 添加（NVML 前置）
vgpu_ensure_sm_watcher_started();    // Phase 6 添加（CUDA + watcher 前置）
return VK_SUCCESS;
```

CUDA 路径仍然走原有的 `pthread_once(&g_init_set, initialization)`，由于 once 语义先到先 init、后到全 no-op，**CUDA 应用看到的行为完全不变**。

#### 6.3 CUDA 侧 rate_limiter 重构（CUDA 调用方零变化）

[cuda_hook.c](../library/src/cuda_hook.c) 抽出 host_index-keyed 核：

```c
// 新公开函数,声明在 budget.h
void vgpu_rate_limit_by_host_index(int kernel_size, int host_index) {
  if (host_index < 0 || host_index >= MAX_DEVICE_COUNT) return;
  if (!g_vgpu_config->devices[host_index].core_limit) return;
  /* ...原 rate_limiter 的 CAS 循环... */
}

// 旧静态包装,签名不变,11 个 CUDA 调用方零变化
static void rate_limiter(int grids, int blocks, CUdevice device) {
  (void)blocks;
  int host_index = get_host_device_index_by_cuda_device(device);
  if (host_index < 0) return;
  vgpu_rate_limit_by_host_index(grids, host_index);
}
```

#### 6.4 VkQueue → VkDevice 反查（queue_index）

vkQueueSubmit 接 VkQueue，但 host_index 是从 VkDevice → VkPhysicalDevice → host_index 链路解出来的。Vulkan 没给 layer 提供"VkQueue 属于哪个 VkDevice"的查询 API，所以必须在 vkGetDeviceQueue / vkGetDeviceQueue2 时记录 (queue, device) 对。

实现位置：[queue_index.c](../library/src/vulkan/queue_index.c) — 链表 + rwlock，与 [physdev_index.c](../library/src/vulkan/physdev_index.c) 同款模式。生命周期：
- vkGetDeviceQueue[2] 时 `vgpu_vk_register_queue(*pQueue, device)`（同 queue 重复注册去重）
- vk_layer_DestroyDevice 时 `vgpu_vk_unregister_queues_for_device(device)` 批量清理（顺序：清 queue → 清 dispatch → forward destroy，与 DestroyInstance 同款顺序保证并发安全）

#### 6.5 Submit hook 流程

```c
VkResult vgpu_vk_QueueSubmit(VkQueue queue, ...) {
  VkDevice dev = vgpu_vk_queue_to_device(queue);
  vgpu_device_dispatch_t *d = (dev != VK_NULL_HANDLE) ? vgpu_get_device_dispatch(dev) : NULL;
  if (d == NULL || d->pfn_QueueSubmit == NULL) return VK_ERROR_INITIALIZATION_FAILED;

  int host_index = vgpu_vk_physdev_to_host_index(d->physical_device);
  vgpu_rate_limit_by_host_index(/*kernel_size*/ 1, host_index);

  return d->pfn_QueueSubmit(queue, ...);
}
```

`kernel_size = 1`：与 HAMi PR #182 一致。Vulkan 提交的 cmdbuf / dispatch 数量任意且对工作量信号弱（真实 GPU 占用埋在 pipeline state 里），从 layer 视角廉价估算不可行。throttle 精度由 watcher 线程基于 NVML 利用率动态调整 share 决定，per-call 大小只控制公平性基线。

#### 6.6 注意点

- `vkQueueSubmit2` 和 `vkQueueSubmit2KHR` 签名/语义完全相同，**共用一个 hook 函数**
- next-layer 没暴露 `vkQueueSubmit2` 时回退到 `vkQueueSubmit2KHR`（pre-1.3 实例 + VK_KHR_synchronization2 扩展场景）
- 无 core_limit 配置：`vgpu_rate_limit_by_host_index` 内部 `core_limit == 0` 提前 return，per-submit 开销仅一次 host_index 查表 + 一次条件判断
- `cuInit` 失败的容错：当前实现**不**加 `g_total_cuda_cores == 0` 防御 guard。前提假设 vgpu-manager 部署环境必然有 libcuda（与 Phase 5 的 NVML 假设同源）；若 cuInit 真失败会 LOGGER ERROR 但不 exit，watcher 不起，第一次有 core_limit 的 vkQueueSubmit 仍会卡死。这是已知的 fail-fast 行为约定。

#### 6.7 与 HAMi PR #182 的差异

| | HAMi #182 | vgpu |
|---|---|---|
| rate_limiter signature | `(grids, blocks)` 全局 | `(kernel_size, host_index)` 按设备 |
| watcher bootstrap | budget.c 在首次 alloc 时 `pthread_once { cuInit(0); }` | vk_layer_CreateInstance 在 next_create 成功后 `vgpu_ensure_sm_watcher_started()` |
| 触发时机 | lazy（首次 Vulkan 调用） | eager（实例创建即就绪） |
| Submit token | 1 / submit | 1 / submit（一致） |

eager 选择与 Phase 5 的 `init_devices_mapping` 一致，可预测、首 submit 无延迟。

#### 6.8 验收

- [ ] 单元测：mock dispatch + queue，QueueSubmit 调用前 token bucket 被扣 1
- [ ] 单元测：core_limit=0 → vgpu_rate_limit_by_host_index 完全 no-op，submit 路径 hot-path 仅一次条件判断
- [ ] 单元测：QueueSubmit2 / QueueSubmit2KHR 走同一 hook 函数（同一 dispatch pfn 字段）
- [ ] 集成测（带 GPU）：50% core_limit，纯 Vulkan compute loop，NVML SM 利用率收敛到 ~50%
- [ ] 集成测：CUDA cuLaunchKernel + Vulkan vkQueueSubmit 同进程混合（公平共享同一 token bucket）
- [ ] 静态：`nm -D libvgpu-control.so | grep ' T '` 仅 `vkNegotiateLoaderLayerInterfaceVersion` 一个 Vk 相关符号

**预估**：1 天

---

### Phase 7：Manifest 与构建集成

**目标**：把 Vulkan implicit layer manifest 装入镜像，让 Vulkan loader 在 Pod 内能发现并加载 layer。

**Phase 7 不做的事**：
- ❌ 容器内 `/etc/vulkan/implicit_layer.d/` 挂载（Phase 8 device-plugin）
- ❌ Pod 注解开关、env 注入（Phase 8）
- ❌ NCT / kubelet-plugin 协调（Phase 8）

#### 7.1 Manifest 文件

[library/deploy/vgpu_manager_implicit_layer.json](../library/deploy/vgpu_manager_implicit_layer.json)：

```json
{
  "file_format_version": "1.2.0",
  "layer": {
    "name": "VK_LAYER_VGPU_MANAGER_vgpu",
    "type": "GLOBAL",
    "library_path": "/etc/vgpu-manager/driver/libvgpu-control.so",
    "api_version": "1.3.0",
    "implementation_version": "1",
    "description": "vgpu-manager Vulkan implicit layer: per-pod GPU memory budget, heap-size clamp, and SM rate limiting (mirrors the CUDA hook path).",
    "enable_environment":  { "VGPU_VULKAN_ENABLE":  "1" },
    "disable_environment": { "VGPU_VULKAN_DISABLE": "1" }
  }
}
```

字段决策依据：
- **`name`** = `VK_LAYER_VGPU_MANAGER_vgpu` —— 遵循 Vulkan `VK_LAYER_<vendor>_<purpose>` 约定，与 HAMi `VK_LAYER_HAMI_vgpu` 形式对齐
- **`library_path`** = 绝对路径 `/etc/vgpu-manager/driver/libvgpu-control.so` —— 和 device-plugin 的 [`ContVGPUControlFilePath`](../library/../pkg/deploy/vgpu/vnum_plugin.go#L433) 完全一致；零 ld.so 配置依赖
- **`api_version`** = `1.3.0` —— 覆盖到 `vkQueueSubmit2` 1.3 core
- **省略 `functions` 字段** —— v2 协议下 loader 按符号名探测 `vkNegotiateLoaderLayerInterfaceVersion`，`functions` 仅是 informational（与 HAMi 同款省略）
- **`enable_environment` 是 loader 的 load gate**：env 没设时 loader 根本不加载 .so。这意味着 ship 到所有镜像零额外条件分支，纯 CUDA Pod 完全无感

#### 7.2 CMake 集成

[library/CMakeLists.txt](../library/CMakeLists.txt) `BUILD_VULKAN_LAYER=ON` 时：

```cmake
# 配置时 COPYONLY 到 build dir,top-level Dockerfile 从此路径 COPY --from=builder
configure_file(
    ${CMAKE_CURRENT_SOURCE_DIR}/deploy/vgpu_manager_implicit_layer.json
    ${CMAKE_CURRENT_BINARY_DIR}/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json
    COPYONLY)

# CMake install rule (现有 Docker 流程不走 make install,但保留以便未来 packaging)
install(FILES
    ${CMAKE_CURRENT_SOURCE_DIR}/deploy/vgpu_manager_implicit_layer.json
    DESTINATION etc/vulkan/implicit_layer.d)
```

#### 7.3 build.sh 默认开 Vulkan

[library/build.sh](../library/build.sh) 默认 `-DBUILD_VULKAN_LAYER=ON`，可通过环境变量覆盖：

```sh
BUILD_VULKAN_LAYER=OFF ./build.sh   # 退回纯 CUDA 构建
```

理由：layer 的 `enable_environment` 已经是 loader 的 load gate，纯 CUDA Pod（不 set `VGPU_VULKAN_ENABLE`）完全不加载 .so 的 layer 部分。把 ON 设为默认 = 标准镜像直接 ship Vulkan 支持，零运行时副作用。

**注意**：若显式指定 `BUILD_VULKAN_LAYER=OFF`，build dir 不会产出 `build/vulkan/implicit_layer.d/`，顶层 Dockerfile 的 `COPY --from=builder` 会失败。要做 CUDA-only 镜像，需同步删掉顶层 Dockerfile 那行 COPY。

#### 7.4 Dockerfile 改动

[library/Dockerfile](../library/Dockerfile)（builder image）：
```diff
-apt-get -y install make cmake
+apt-get -y install make cmake libvulkan-dev
```

[Dockerfile](../Dockerfile)（顶层运行镜像）：
```diff
+# Vulkan implicit-layer manifest. Stays unconditional even on CUDA-only
+# Pods: the layer's enable_environment gate (VGPU_VULKAN_ENABLE=1) is
+# only set by device-plugin for Pods that opt in.
+COPY --from=builder /vgpu-controller/build/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json \
+                    /installed/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json
```

[scripts/install_files.sh](../scripts/install_files.sh) **零改动**：原本就是 `find /installed -type f` 递归复制到 `/etc/vgpu-manager/`，新增子目录自动处理。

#### 7.5 安装链路结果

```
源:    library/deploy/vgpu_manager_implicit_layer.json
   ↓ (CMake configure_file)
build: build/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json
   ↓ (top-level Dockerfile COPY --from=builder)
镜像: /installed/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json
   ↓ (init container 跑 install_files.sh)
Host: /etc/vgpu-manager/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json
   ↓ (Phase 8 device-plugin 在 Allocate 时挂载)
容器: /etc/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json   <-- Phase 8
```

#### 7.6 验收

- [x] `make build-vulkan` 后 `build/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json` 存在
- [x] `BUILD_VULKAN_LAYER=OFF bash build.sh` 不产出 manifest（CUDA-only 路径完整）
- [x] `nm -D libvgpu-control.so | grep ' T '` 仅 `vkNegotiateLoaderLayerInterfaceVersion` 一个 Vk 相关符号
- [ ] 镜像内 `cat /etc/vgpu-manager/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json` 有内容（init container 跑完后）
- [ ] Phase 8 完成后：容器内 `VK_LOADER_DEBUG=all vulkaninfo 2>&1 | grep VGPU` 能看到 `VK_LAYER_VGPU_MANAGER_vgpu` 被 loader 发现并加载

**预估**：0.5 天

---

### Phase 8：Go 侧（device-plugin / webhook / kubelet-plugin）

**目标**：让 Pod 能自动获得 Vulkan layer 的支持。**这是 library 之外的工作，不在 library/ 仓**。

**变更**（粗粒度，具体接口需要看 Go 代码再细化）：

1. **device-plugin** ([cmd/device-plugin](../cmd/device-plugin/)):
   - 给 `Allocate` 响应里的 `Mounts` 加 manifest 路径挂载，源是 host 上 `/usr/local/vgpu-manager/etc/vulkan/implicit_layer.d/`，目标是容器内 `/etc/vulkan/implicit_layer.d/`
   - 给 `Envs` 自动注入 `VGPU_VULKAN_ENABLE=1`
   - 加 Pod 注解开关：`vgpu-manager.io/vulkan: "true"` / `"false"` / 默认 true

2. **admission webhook** ([cmd/device-webhook](../cmd/device-webhook/)):
   - 检测开了 vGPU 资源且 `vulkan` 注解为 true 的 Pod
   - 注入 env / mount（如果 device-plugin 已经做了，webhook 这里可省，二选一）

3. **kubelet-plugin** ([cmd/kubelet-plugin](../cmd/kubelet-plugin/)):
   - 协调 NVIDIA Container Toolkit：NCT 已经处理 libvulkan / ICD 的挂载，**我们只补 layer manifest**
   - 验证 manifest 中 `library_path: libvgpu-control.so` 在容器内 ld.so 路径上能找到（NCT 会挂 `libvgpu-control.so`，需要确认默认路径在 `/usr/lib/x86_64-linux-gnu/` 或类似）

4. **scripts/install_files.sh**:
   - 把 manifest 安装到 host 路径

5. **deploy/charts** + 文档：
   - Helm values 加 `vulkanEnabled: true/false`
   - `docs/how_to_use_vulkan_workload.md` 写用户文档（启用方法、注解、常见问题）

**验收**：
- [ ] 普通 PyTorch Pod（CUDA-only）行为零变化
- [ ] 加了 `vgpu-manager.io/vulkan: "true"` 注解的 Pod 内 `env | grep VGPU_VULKAN` 有 `=1`
- [ ] 容器内 `/etc/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json` 存在
- [ ] e2e：在 4 GiB Pod 内跑 Isaac Sim mini-case 或一个 vkAllocateMemory smoke 程序，第二次大 alloc 返回 OOM

**预估**：3-5 天（Go 部分）

---

### Phase 9：测试

**目标**：单测覆盖（无 GPU 需求）+ 集成测试占位。**Phase 9.1 已落地**；Phase 9.2 集成测因需 GPU testbed 暂不实施。

#### 9.1 单元测（已实现，零 GPU/CUDA/NVML 依赖）

新增目录 [library/test/vulkan/](../library/test/vulkan/) 共 8 个测试 + 共享基建：

| # | 文件 | 关注点 | 单内 ok 数 |
|---|---|---|---|
| 1 | [test_layer_init.c](../library/test/vulkan/test_layer_init.c) | `vkNegotiateLoaderLayerInterfaceVersion` v2 协议 + 仅 1 个 ELF 导出 | 5 |
| 2 | [test_dispatch.c](../library/test/vulkan/test_dispatch.c) | instance/device dispatch 注册-查询-删除；NULL 防御 | 3 |
| 3 | [test_queue_index.c](../library/test/vulkan/test_queue_index.c) | VkQueue→VkDevice 缓存 + dedup + per-device unregister | 6 |
| 4 | [test_physdev_index.c](../library/test/vulkan/test_physdev_index.c) | mock `next_gipa` 喂入 deviceUUID，验证 UUID→host_index 映射 + per-instance unregister | 4 |
| 5 | [test_memprops_clamp.c](../library/test/vulkan/test_memprops_clamp.c) | DEVICE_LOCAL clamp + non-DEVICE_LOCAL 不动 + **oversold ON→real_memory / OFF→total_memory 分支** + memory_limit=0 跳过 + heap.size<cap 不动 + _2 entry pNext 扩展 untouched | 6 |
| 6 | [test_alloc_budget.c](../library/test/vulkan/test_alloc_budget.c) | GPU/OOM 路径分支；PHYSICAL/allow_uva=0 强制；lock_fd<0 不 unlock；Free 透传；缺 dispatch 返回 INIT_FAILED | 5 |
| 7 | [test_alloc_import.c](../library/test/vulkan/test_alloc_import.c) | **Phase 5b**：每种 import sType 短路 + fd=-1 / handleType=0 / pHostPointer=NULL 不短路（防御）+ 无 pNext / Export struct 走预算 | 7 |
| 8 | [test_submit_throttle.c](../library/test/vulkan/test_submit_throttle.c) | QueueSubmit / QueueSubmit2 throttle 然后 forward；GetDeviceQueue / _2 注册 (queue, device)；未注册 queue 返回 INIT_FAILED | 5 |

**38 个 `ok:` 断言全部通过**。

##### 链接策略

每个 test 二进制**直接编译**相关 `src/vulkan/*.c` 文件（不走 LD_PRELOAD），加 `stubs.c` 提供：
- `g_vgpu_config` 测试用静态实例（直接访问可控字段）
- `vgpu_check_alloc_budget` / `vgpu_rate_limit_by_host_index` 可控 mock（call_count、last_args、forced return）
- `lock_gpu_device` / `unlock_gpu_device` 计数 stub
- `get_host_device_index_by_uuid_bytes` UUID 字符串反查

测试基建：
- [test_helpers.h](../library/test/vulkan/test_helpers.h) — mock 控制 + `vgpu_test_pass()` 输出
- [stubs.c](../library/test/vulkan/stubs.c) — 所有外部依赖 stub
- [CMakeLists.txt](../library/test/vulkan/CMakeLists.txt) — `BUILD_VULKAN_LAYER=ON AND BUILD_TESTS=ON` 时启用，**独立于 CUDAToolkit 检测**（关键差异：让无 CUDA 工具链的 CI / devcontainer 也能跑）
- [run_unit_tests.sh](../library/test/vulkan/run_unit_tests.sh) — 顺序运行 8 个二进制 + 收集 PASS/FAIL

##### 触发方式

```sh
make test-vulkan-unit       # 编 + 跑 8 个单测,无 GPU 需要
make check-vulkan-layer     # 跑 hack/check_vulkan_layer.sh 静态检查
```

##### 静态检查脚本

[library/hack/check_vulkan_layer.sh](../library/hack/check_vulkan_layer.sh)：
1. `nm` 验证 `vkNegotiateLoaderLayerInterfaceVersion` 已导出
2. `nm` 验证**没有其他 vk* 符号**导出（防止 LD_PRELOAD 旁路 Vulkan loader）
3. python3 解析 manifest，验证 `name=VK_LAYER_VGPU_MANAGER_vgpu` + `library_path=/etc/vgpu-manager/driver/libvgpu-control.so` + 含 `enable_environment.VGPU_VULKAN_ENABLE`

#### 9.2 集成测（deferred）

需要带 NVIDIA GPU + Vulkan ICD 的机器，且需在容器内验证 manifest 实际加载效果。**等 Phase 8（Go 侧）先把 mount + env 注入跑通后再做**。

打算覆盖：
- `vulkan_alloc_loop.c` — 跑 vkAllocateMemory 循环直到 OOM，验证停在 budget 边界
- `vulkan_cuda_mixed.c` — 验证 CUDA + Vulkan 共享同一 budget 视图（NVML PID 去重生效）
- `vulkan_interop_import.c` — 验证 Phase 5b：CUDA cuMemAlloc → 导出 fd → Vulkan import 不报 OOM
- `VK_LOADER_DEBUG=all vulkaninfo` 输出含 `VK_LAYER_VGPU_MANAGER_vgpu` 被发现并加载

#### 9.3 已知限制

- `test_layer_init.c` 用 `dlopen(libvgpu-control.so, RTLD_LAZY)`：必须用 RTLD_LAZY（不是 RTLD_NOW）以避免 eagerly 解析 cuda_hook.c 引入的 `_dl_sym` glibc 私有符号。RTLD_LAZY 推迟到 actual call site，本测试只调 `vkNegotiateLoaderLayerInterfaceVersion`，不会触发 CUDA 路径
- 单元测**不替代** GPU integration test：mock dispatch chain 不验证真实 NVIDIA Vulkan ICD 行为
- HAMi PR #182 的 `test_throttle_adapter.c` **不移植**：vgpu 没有 throttle_adapter 中间层，`vgpu_rate_limit_by_host_index` 直接调用，等价测试已含在 `test_submit_throttle.c`

**预估**：2-3 天 → **9.1 实际 1 天落地**

---

## 7. 风险与开放问题

| 风险 | 等级 | 缓解 |
|---|---|---|
| ~~应用同时混用 CUDA + Vulkan,预算累计错位~~ | ~~中~~ | **已天然解决** —— `get_used_gpu_memory_by_device` 通过 NVML 进程聚合（compute + graphics 列表），同时覆盖 CUDA 和 Vulkan 物理用量 |
| ~~CUDA + Vulkan 混合进程 PID 双重计数~~ | ~~中~~ | **已修复（commit `20e9519`）** —— `get_used_gpu_memory_by_device` 现在 dedupe compute / graphics PID，避免 Isaac Sim / Omniverse 等混合应用的 used 视图被算两遍 |
| Vulkan loader 与我们 LD_PRELOAD 冲突（双重加载 `libvgpu-control.so`）| 中 | dlopen 同一个 .so 多次返回相同 handle，全局变量只一份。`pthread_once` 保证 init 幂等。需 **手动测一次**确认 |
| ICD 路径与 layer 路径不一致（容器内找不到 libvgpu-control.so）| 中 | NCT 已经挂 libvgpu，路径协调清楚后写到 `library_path`。e2e 集成测必须覆盖 |
| **CUDA-Vulkan interop import 路径双重计费** | 中 | Phase 5b 检测 `VkImportMemoryFdInfoKHR` 等 import 结构,识别为指向已存在物理内存的别名,不重复计费 |
| **应用配置 oversold 然后跑 Vulkan workload** | 中 | Phase 4 Vulkan heap clamp **永远用 `real_memory`,无视 oversold 配置**。CUDA 路径仍按 oversold 报 `total_memory`,两侧报告不同(预期);文档要写明 Vulkan 不参与 oversold |
| Vulkan headers 版本要求（`VkQueueSubmit2` 是 1.3.280+）| 低 | CMake `find_package(Vulkan REQUIRED)` 检查；旧版本通过 `#ifdef VK_VERSION_1_3` 优雅 fallback |
| Hopper 上 SM 对齐影响（如果将来加 green ctx）| 低 | 本设计不涉及 green ctx，纯 budget + rate_limiter |
| 用户绕过 `VGPU_VULKAN_ENABLE` 直接调 Vulkan 函数 | 低 | env 是 layer 加载条件，未加载就没 hook，应用走原生 Vulkan path（这是预期行为：opt-in）|
| `vkAllocateMemory` 在多线程并发 | 低 | `vgpu_check_alloc_budget` 内部已加锁（`lock_gpu_device`）|
| Wayland / X11 swapchain 内存不计费 | 低 | 不在目标内；swapchain 用的是 buffer 共享，`vkAllocateMemory` 只看 explicit allocation |
| NVML 视图更新延迟（一次大 alloc 后立刻再查可能还没体现）| 低 | `prepare_memory_allocation` 把 `request_size` 加进 OOM 判断,in-flight 请求自动算上;CUDA 路径同样行为,无新增风险 |

### 开放问题（实施前需要决策）

1. **Vulkan oversold 是否支持？**
   - Vulkan 没有 CUDA `cuMemAllocManaged` / UVA 等价物，`vkAllocateMemory` 永远是物理分配
   - 即使配置 `memory_oversold=true`，Vulkan 应用也用不到 oversold 容量
   - **决策：绝对不支持**。Vulkan layer 内部强制 `KIND_PHYSICAL` + `allow_uva=0` + heap clamp 用 `real_memory`，与 oversold 配置完全解耦
   - 文档对用户要写清楚："oversold 仅对 CUDA workload 生效；Vulkan workload 仍受物理切片硬上限约束"

2. **`vkQueueSubmit` 节流粒度？** 初版按 1 unit/submit 计算，简化。后续可考虑按 commandBuffer 数量、shader 复杂度估算。**决策建议：Phase 6 用最简单的，等真实压测数据再调**。

3. **manifest `api_version`?** 写 `1.3.280` 还是 `1.0.0`？前者明确支持 vkQueueSubmit2，后者最大兼容性。**决策建议：`1.3.280`，覆盖现代应用；不开 vkQueueSubmit2 hook 的版本下让 vkQueueSubmit 兜底**。

4. **device-plugin 注解默认 on/off？** Pod 没显式注解时，Vulkan layer 默认启用还是禁用？
   - 默认 on：透明覆盖所有 vGPU Pod，对 Vulkan 应用自动生效；CUDA-only 应用零开销（loader 不加载 layer）
   - 默认 off：用户显式 opt-in
   
   **决策建议：默认 on**——对 CUDA-only 的成本是零，且发现 Vulkan 应用时自然生效。

5. **失败模式：layer init 失败时是否阻塞 Vulkan app 启动？**
   - 严格模式：layer 出错 → Vulkan 函数返 error → app 启动失败
   - 宽松模式：layer 出错 → log + forward → app 正常启动但无 budget enforcement
   
   **决策建议：宽松模式**，避免 layer bug 把整个 GPU 应用栈拖死。Log + metrics counter 暴露问题。

6. **Vulkan 应用使用 oversold 配置 Pod 时的提示策略？**
   - 用户可能误以为 oversold 也对 Vulkan 生效
   - **决策建议**：Vulkan layer 启动时检测 `memory_oversold==true`，在 LOG_VERBOSE 打一条："Vulkan workload on oversold device; oversold capacity is **CUDA-only** and not visible to Vulkan"

## 8. 与 HAMi PR #182 的差异

| 项 | HAMi | 我们 |
|---|---|---|
| Env var | `HAMI_VULKAN_ENABLE` | `VGPU_VULKAN_ENABLE` |
| Manifest 文件名 | `hami.json` | `vgpu_manager_implicit_layer.json` |
| Layer name | (PR 内未明确) | `VGPU_MANAGER_implicit_memory_budget` |
| `library_path` | `libvgpu.so` | `libvgpu-control.so`（保持一致 .so）|
| Budget API 抽象 | 自带 `hami_budget_of` adapter | Phase 0 抽 `vgpu_check_alloc_budget` 含 `vgpu_budget_kind_t`，CUDA 走 VIRTUAL，Vulkan 走 PHYSICAL |
| **used 视图来源** | 自维护"已分配总量"计数器 | **完全来自 NVML 进程聚合** —— 不需要我们维护计数器，CUDA / Vulkan 自动累加 |
| **CUDA + Vulkan 同进程混合** | PR 内未提及去重 | 已修复 PID 去重 bug（commit `20e9519`），同进程 compute + graphics 不双重计数 |
| **Heap clamp 上限** | 单一 budget 字段（PR 内未区分 oversold） | 与 cuMemGetInfo 故意差异：CUDA 报 `total_memory`（含 oversold UVA），Vulkan 报 `real_memory`（物理硬上限） |
| **CUDA-Vulkan interop import 路径** | PR 内未明确处理 | Phase 5b 显式检测 `VkImportMemoryFdInfoKHR` 等 import 结构，避免双重计费 |
| 顺带的 CUDA bug 修复 | `cuMemFree` 未跟踪指针、`cuMemGetInfo_v2` NULL deref | **不需要**——我们设计本就 forward-friendly + 已有 `dca8e7a` 修复 |
| `throttle_adapter` | 抽 adapter 给 Vulkan submit 用 | **不抽**——直接调 `rate_limiter` |
| 测试目录 | `test/vulkan/` | `library/test/vulkan/` |

我们的实现可以**比 HAMi 更精简**——很多基础设施（UUID 反查、oversold UVA、cuMemGetInfo cap、NVML 进程聚合 + PID 去重）已经做了。**特别是 NVML 视图自动覆盖 Vulkan 物理用量，Vulkan layer 完全不需要自己维护分配计数器**——这是 vgpu-manager 架构上的优势。

## 9. 实施时间线

```
Week 1:
  Mon  Phase 0 (interface prep) + Phase 1 (CMake)
  Tue  Phase 2 (layer skeleton)
  Wed  Phase 3 (physdev mapping) + Phase 4 (memprops clamp)
  Thu  Phase 5 (alloc budget)
  Fri  Phase 6 (queue throttle) + Phase 7 (manifest)

Week 2:
  Mon-Wed  Phase 8 (Go 部分) — device-plugin / webhook / docs
  Thu      Phase 9 (testing)
  Fri      集成 / 修 bug / 文档完善 / 镜像构建
```

总计 **~10 工作日**（library 5 天 + Go 3 天 + 测试 2 天），略乐观。实际可能 12-16 天，看 Go 侧改动复杂度。

## 10. 后续

- Phase 9 完成后做 dogfooding：在内部测试 cluster 上跑一个 Isaac Sim 案例，验证生效
- 收集真实 Vulkan workload 内存模式数据，可能调整 Phase 6 的节流粒度
- 如果 ROCm / OneAPI 用户出现，**到时候再**抽 throttle/budget 通用 adapter（YAGNI 原则，**不在本计划内提前做**）
- 评估是否扩展到 DirectX 12 / OpenCL / Vulkan video extensions（视用户需求）

## 参考

- [HAMi-core PR #182: Vulkan implicit layer to enforce per-pod GPU memory budget](https://github.com/Project-HAMi/HAMi-core/pull/182)
- [NVIDIA Vulkan ICD documentation](https://docs.nvidia.com/vulkan/index.html)
- [Vulkan Layer Interface specification](https://github.com/KhronosGroup/Vulkan-Loader/blob/main/docs/LoaderLayerInterface.md)
- [Khronos Vulkan-Loader: layer manifest format](https://github.com/KhronosGroup/Vulkan-Loader/blob/main/docs/LoaderLayerInterface.md#layer-manifest-file-format)
