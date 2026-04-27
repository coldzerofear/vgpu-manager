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

   int  vgpu_check_alloc_budget(int host_index, size_t request_size,
                                vgpu_budget_kind_t kind,
                                int allow_uva,            /* 仅 VIRTUAL kind 有意义 */
                                int *path_out, int *lock_fd_out);
   void vgpu_account_alloc_gpu(int host_index, void *opaque, size_t size);
   void vgpu_account_alloc_uva(int host_index, void *opaque, size_t size);
   void vgpu_account_free(int host_index, void *opaque);
   ```

   实现要点：
   - 把 `prepare_memory_allocation` / `malloc_gpu_virt_memory` / `free_gpu_virt_memory` 从 cuda_hook.c 的 file-static 改成 extern
   - `prepare_memory_allocation` 内部根据 `kind` 选择上限字段（`VIRTUAL` → `total_memory`、`PHYSICAL` → `real_memory`）
   - **`PHYSICAL` 路径强制 `allow_uva = 0`**，永远不返回 `MEMORY_PATH_UVA`
   - **重要**：`vgpu_account_alloc_gpu` 在 Vulkan 路径其实是个 no-op —— `used` 视图来自 NVML 进程聚合，分配成功后驱动自然把 PID 暴露给 NVML，下次查询自动反映。保留这个函数是为了 API 对称（CUDA 路径仍然有用：metrics / 调试 hook 可能需要）

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

**目标**：Hook `vkGetPhysicalDeviceMemoryProperties` / `_2`，把 device-local heap size clamp 到 `g_vgpu_config->devices[host_index].real_memory`（**物理切片**，不是 oversold 的 `total_memory`）。

**新文件**：[library/src/vulkan/hooks_memory.c](../library/src/vulkan/hooks_memory.c)

实现要点：
- 调下层拿原始 `VkPhysicalDeviceMemoryProperties`
- 判定哪些 heap 是 device-local（`VK_MEMORY_HEAP_DEVICE_LOCAL_BIT`）
- 对 device-local heap：`heap.size = min(heap.size, g_vgpu_config->devices[host_index].real_memory)`
- host-visible / staging heap 不动
- **不要按 oversold 状态 gate**：跟 cuMemGetInfo 不同，Vulkan 没有 UVA 等价物，oversold 容量在 Vulkan 端**永远不可达**。即使用户配置了 oversold，Vulkan 应用看到的物理上限仍然只能是 `real_memory`——告诉它更多反而误导，让它以为还有空间最后却 OOM
- non-NVIDIA 设备（host_index < 0）不动

**为什么是 `real_memory` 而不是 `total_memory`**：

CUDA 的 `cuMemGetInfo` 在 oversold 时报 `total_memory`（含 UVA 容量），是因为 CUDA 的 `cuMemAllocManaged` 真的能给出超物理容量（驱动按页 swap）。Vulkan 没有等价机制——`vkAllocateMemory` 总是物理分配，告诉它一个超物理的 heap size 应用一旦真去用最后那部分就会失败。所以 Vulkan 这条路径**只看物理切片**。

**验收**：
- [ ] 在 4 GiB 限额的 Pod 内跑 `vulkaninfo`，device-local heap size 显示 4 GiB（而不是物理 24 GiB）
- [ ] CPU 可见 heap 不被 clamp
- [ ] oversold 配置下 heap size 仍然显示 `real_memory`（**不**显示 `total_memory`）—— 这是与 cuMemGetInfo 的故意差异
- [ ] non-NVIDIA 设备（如 CPU vendor）heap 不动

**预估**：0.5 天

---

### Phase 5：内存分配预算

**目标**：Hook `vkAllocateMemory` / `vkFreeMemory`，接 `vgpu_check_alloc_budget` / `vgpu_account_*`。

**新文件**：[library/src/vulkan/hooks_alloc.c](../library/src/vulkan/hooks_alloc.c)

`vk_layer_AllocateMemory`：
1. 取 `host_index = vgpu_vk_physdev_to_host_index(device's physdev)`
2. 如果 host_index < 0（非 NVIDIA、未配置）：直接 forward
3. 取 `request_size = pAllocateInfo->allocationSize`
4. **扫 `pAllocateInfo->pNext` 链识别 import 路径**（详见 Phase 5b）—— 如果这次 alloc 是导入已存在的外部内存（CUDA 已经分配过的、或者跨进程共享的），**短路 forward 到下一层、不查预算、不计账**
5. 否则调 `vgpu_check_alloc_budget(host_index, request_size, VGPU_BUDGET_KIND_PHYSICAL, /*allow_uva*/ 0, &path, &lock_fd)`
   - **kind 必须是 PHYSICAL**：上限走 `real_memory`，跟 Phase 4 报给应用的 heap size 一致
   - **`allow_uva` 必须是 0**：Vulkan 没有 UVA 概念，绝不能进 `MEMORY_PATH_UVA` 分支
6. 如果返回 OOM：返回 `VK_ERROR_OUT_OF_DEVICE_MEMORY`，`unlock_gpu_device(lock_fd)` 后退出
7. forward 到下一层
8. 成功后调 `vgpu_account_alloc_gpu(host_index, *pMemory, request_size)`，`*pMemory` 当 opaque key
   - 如 §4 所述，这一步对 Vulkan 路径是**保留 hook 给 metrics 用**，**真正的 used 视图来自 NVML 进程聚合**，不依赖这个调用
9. 失败后只 `unlock_gpu_device`，不计账

`vk_layer_FreeMemory`：
1. 取 host_index
2. forward 到下一层（先 free 再清账，跟 cuMemFree 一致）
3. 成功后 `vgpu_account_free(host_index, memory)`（同样主要是 metrics hook）

**注意点**：
- Vulkan 的 `VkDeviceMemory` 是 64-bit 不透明 handle，可以直接当 key 存
- `pAllocator` 字段（应用自定义内存分配器）forward 时透传
- **CUDA + Vulkan 累加在 NVML 视图自动完成**，不需要 Vulkan layer 自己维护"已分配总量"
- **PID 去重已在 commit `20e9519` 修复**，CUDA + Vulkan 同进程混合应用（Isaac Sim 等）的 `used` 不会被双重计数

**验收**：
- [ ] 单元测：mock dispatch，模拟 Vulkan app 一次 alloc 5 GiB，限额 4 GiB → 返回 `VK_ERROR_OUT_OF_DEVICE_MEMORY`
- [ ] 单元测：模拟 import path（`pNext` 链含 `VkImportMemoryFdInfoKHR`）→ 不查预算，直接 forward
- [ ] 集成测：在 4 GiB Pod 内跑一个 vkAllocateMemory loop，分配总量受限
- [ ] CUDA + Vulkan 同进程同时分配（mocked Isaac Sim 风格）：CUDA app 在容器内分配 2 GiB，Vulkan app 后续只能再分配最多 2 GiB（确认 PID 去重生效，不会被算成 4 GiB）

**预估**：1 天

---

### Phase 5b：CUDA-Vulkan interop import 路径检测

**目标**：识别 `vkAllocateMemory` 的 `pNext` 链中表示**导入已存在外部内存**的结构，对这类调用**短路**——不查预算、不计账。

**为什么需要**：CUDA-Vulkan interop 通过 `VK_KHR_external_memory` 跨 API 共享内存。典型流程（Isaac Sim、视频渲染应用、Vulkan-CUDA 互操作示例都用这套）：

```
1. CUDA 侧:cuMemAlloc + cuMemGetMemoryFd  分配 + 导出 fd
   ──► 已经被我们的 cuMemAlloc hook 计入 NVML 视图
2. 同进程 Vulkan 侧:vkAllocateMemory + VkImportMemoryFdInfoKHR(fd)
   ──► 这个 alloc 不是新物理分配,是**指向同一块物理内存的另一个 handle**
```

如果我们对 import path 也走预算检查，会**双重计费**——同一块物理内存被算两次。NVML 那边只会统计一次（同一物理内存），但我们 OOM 判断会按"CUDA used 2 GiB + Vulkan 请求 2 GiB = 4 GiB"判，错。

**新增** [library/src/vulkan/hooks_alloc.c](../library/src/vulkan/hooks_alloc.c)：

```c
/* 扫 pAllocateInfo->pNext 链,识别 "import 既存内存" 类型的结构。
 * 找到任一种 import 结构都意味着这次 alloc 不是新物理分配,应短路。 */
static int vk_alloc_is_import(const VkMemoryAllocateInfo *info) {
  for (const VkBaseInStructure *p = (const VkBaseInStructure *)info->pNext;
       p != NULL; p = p->pNext) {
    switch (p->sType) {
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR:                 /* Linux fd 导入 */
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_WIN32_HANDLE_INFO_KHR:       /* Windows handle */
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_HOST_POINTER_INFO_EXT:       /* 已 mmap 的 host ptr */
      case VK_STRUCTURE_TYPE_IMPORT_ANDROID_HARDWARE_BUFFER_INFO_ANDROID:
      case VK_STRUCTURE_TYPE_IMPORT_MEMORY_ZIRCON_HANDLE_INFO_FUCHSIA:
      /* 必要时再扩。注意:VkExportMemoryAllocateInfo 是导出,
       * 不是导入,导出仍然是新物理分配,要计费 */
        return 1;
      default:
        break;
    }
  }
  return 0;
}
```

调用点（Phase 5 第 4 步）：
```c
if (vk_alloc_is_import(pAllocateInfo)) {
  LOGGER(DETAIL, "vkAllocateMemory import path, skip budget check");
  return dispatch->pfn_AllocateMemory(device, pAllocateInfo, pAllocator, pMemory);
}
```

**验收**：
- [ ] 单元测覆盖每种已识别 import sType
- [ ] 集成测（带 GPU）：CUDA cuMemAlloc 4 GiB → 导出 fd → Vulkan import 4 GiB → 总占用仍是 4 GiB（不是 8 GiB），后续 CUDA / Vulkan 都还能再分配 0 GiB（因为限额 4 GiB 已用完）

**预估**：0.5 天

---

### Phase 6：队列节流

**目标**：Hook `vkQueueSubmit` / `_2` / `_2KHR`，每次提交前过 `rate_limiter`。

**新文件**：[library/src/vulkan/hooks_submit.c](../library/src/vulkan/hooks_submit.c)

实现：
- 取 host_index
- 估计提交规模（command buffer 数 × 估计 SM 占用？或者简单按提交次数算 1 unit）
- 调 `rate_limiter(estimated_grids, /*blocks*/ 0, host_index)`——blocks 当前未用，传 0
- forward 到下一层

**讨论**：CUDA 那边 `rate_limiter(grids, blocks, device)` 的 `grids` 是 `gridDimX*Y*Z`，可比较直接。Vulkan 的提交粒度不一样，初版可以保守简单计 1 unit per submit。后续可以基于 `VkSubmitInfo` 的 commandBufferCount 做比例放大。

**验收**：
- [ ] 配 50% 核数限制，跑 Vulkan compute kernel loop，观察利用率被限制到 ~50%
- [ ] 不开 core_limit 时无任何节流开销

**预估**：1 天

---

### Phase 7：Manifest 与构建集成

**目标**：生成 layer manifest JSON，配置安装规则，让 layer 能被 Vulkan loader 找到。

**新文件**：

#### [library/deploy/vgpu_manager_implicit_layer.json](../library/deploy/vgpu_manager_implicit_layer.json)
```json
{
  "file_format_version": "1.2.0",
  "layer": {
    "name": "VGPU_MANAGER_implicit_memory_budget",
    "type": "GLOBAL",
    "library_path": "libvgpu-control.so",
    "api_version": "1.3.280",
    "implementation_version": "1",
    "description": "vgpu-manager Vulkan memory budget enforcement",
    "functions": {
      "vkNegotiateLoaderLayerInterfaceVersion": "vkNegotiateLoaderLayerInterfaceVersion"
    },
    "enable_environment": {
      "VGPU_VULKAN_ENABLE": "1"
    },
    "disable_environment": {
      "VGPU_VULKAN_DISABLE": "1"
    }
  }
}
```

#### CMake 安装规则（[library/CMakeLists.txt](../library/CMakeLists.txt)）
```cmake
if(BUILD_VULKAN_LAYER)
  install(FILES deploy/vgpu_manager_implicit_layer.json
    DESTINATION etc/vulkan/implicit_layer.d/)
endif()
```

#### Dockerfile 调整（[library/Dockerfile](../library/Dockerfile)）
- 把 manifest 拷到镜像 `/etc/vulkan/implicit_layer.d/`
- 安装阶段决定（构建 image 时编 BUILD_VULKAN_LAYER=ON 还是 OFF）

**验收**：
- [ ] `make install` 后 manifest 在正确位置
- [ ] 镜像内 `cat /etc/vulkan/implicit_layer.d/vgpu_manager_implicit_layer.json` 有内容
- [ ] 启用 layer 时 `VK_LOADER_DEBUG=all vulkaninfo 2>&1 | grep VGPU` 能看到 layer 被发现 / 加载

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

**目标**：单测覆盖 + 集成测试。

#### 9.1 单元测（无 GPU 需求，mock dispatch）

新增 `library/test/vulkan/`：
- `test_layer_init.c` — 验证 `vkNegotiateLoaderLayerInterfaceVersion` 协议正确
- `test_dispatch.c` — 验证 dispatch table 注册 / 查询 / 删除
- `test_alloc_budget.c` — mock dispatch 模拟分配，验证预算限制
- `test_memprops_clamp.c` — mock dispatch 模拟 vkGetPhysicalDeviceMemoryProperties，验证 clamp
- `test_physdev_index.c` — UUID 反查正确性

新增 `library/test/CMakeLists.txt` 分支：当 `BUILD_VULKAN_LAYER=ON AND BUILD_TESTS=ON`，编 vulkan 测试。

#### 9.2 集成测（需要带 NVIDIA GPU + Vulkan ICD 的机器）

`library/test/vulkan/integration/`:
- `vulkan_alloc_loop.c` — 不停 vkAllocateMemory 直到 OOM，应该停在 budget 边界
- `vulkan_cuda_mixed.c` — 验证 CUDA + Vulkan 共享 budget 累加
- 跑一个 minimal Isaac Sim / Omniverse demo container（如果资源允许）

#### 9.3 hack 脚本扩展

更新 `library/hack/check_cuda_hook_consistency.py`：不需要——Vulkan 跟 CUDA 派发表无关。

新增 `library/hack/check_vulkan_layer.sh`（可选）：
- 验证 `nm libvgpu-control.so` 在 `BUILD_VULKAN_LAYER=ON` 时确实 export 了 `vkNegotiateLoaderLayerInterfaceVersion`
- 验证 manifest JSON 格式

**验收**：
- [ ] 单测全部通过（CI 可在无 GPU 机器跑）
- [ ] 集成测在 dev box 上手动跑通
- [ ] `make test` 集成 vulkan 测试

**预估**：2-3 天

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
