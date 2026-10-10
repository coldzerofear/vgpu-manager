# lupine 环境变量参考（供 vgpu-manager 远程 GPU 设计使用）

> 来源：`D:\WorkSpace\GoCode\src\lupine` 源码逐处核对（含 README 已记录与代码内未记录项）。
> 用途：为 vgpu-manager 的远程 GPU 注入层、lupine-server 部署、测试与排查提供权威配置语义。
> 关联：设计文档 `docs/remote_gpu_pool_research_design.md`（§2.2 lupine 概览、§6.2.1 会话判别、§7 版本分发）。

---

## 0. 约定

- **作用域**：`client`=lupine CUDA/NVML shim（客户端进程）；`server`=`lupine_driver_server`；
  `python`=`python/lupine` 包；`deploy`=镜像/脚本的部署约定（shim 进程不读取）。
- **默认值**以代码实测为准（不只看 README）。
- 以 `LUPINE_` 开头的**编译期宏**（`LUPINE_CUDA_VERSION`、`LUPINE_RPC_*`、`LUPINE_ROUTE_*`、`LUPINE_LOG_*`
  等）**不是环境变量**，见 §4。

---

## 1. 连接与会话

### `LUPINE_SERVER`
| | |
|---|---|
| 作用域 | client（CUDA shim `client.cpp:8215` 与 NVML shim `nvml_client.cpp:104`） |
| 默认 | 无（未设置则连接失败） |
| 语义 | 逗号分隔的 `host[:port]` 端点列表（最多 16 个）。可选 `https://`（TLS，经终止代理，默认端口 443）
  或 `http://`（默认端口 14833）。设备按 server 顺序平铺成虚拟序号。连接在**首次 CUDA/NVML 初始化时**建立，
  进程内后续改动不生效。 |

**对设计的意义**：注入层（device-plugin/DRA Allocate）必须给远程 pod 设置 `LUPINE_SERVER=<gpu-node>:14833`；
多节点资源池时可用逗号列表。

### `LUPINE_PORT`
| | |
|---|---|
| 作用域 | server（`server.cpp:545`） |
| 默认 | `14833` |
| 语义 | server 监听端口。值须为 1–65535（非法直接退出，防 atoi 回退到 0）。绑定 `INADDR_ANY`。 |

### `LUPINE_SESSION`
| | |
|---|---|
| 作用域 | client（`h2.cpp:711`）→ server（`h2.cpp:525, 850-857`） |
| 默认 | 无（连接不携带会话头） |
| 语义 | 稳定的连接标识。客户端作为 HTTP/2 请求头 `x-lupine-session` 发送；server 每连接解析进各自 transport 的
  `session_id`，子进程经 `rpc_http2_session_id(&conn)` 读取。**目前 lupine 仅用于 checkpoint 恢复**
  （`server.cpp:421-429`），不参与任何记账/过滤。 |

**对设计的意义（核心）**：这是方案 C 的**容器判别基础**——注入层给容器所有进程设同一 `LUPINE_SESSION`，
server 子进程据此派生 `VGPU_CONFIG_PATH`（设计 §6.2.1）。**session id 客户端可控，必须消毒 + 控制面签发令牌 + fail-closed。**

### `LUPINE_CHECKPOINT_LIBRARY`
| | |
|---|---|
| 作用域 | server（`server_checkpoint.cpp:80`） |
| 默认 | `liblupinecr.so.0` → `liblupinecr.so`（找不到则 checkpoint 禁用，server 正常排空退出） |
| 语义 | 覆盖 checkpoint provider 库路径。provider 是实现 `lupine_checkpoint_provider_v1`（`checkpoint_provider.h`，
  符号 `lupinecr_get_lupine_provider_v1`）的外部插件，lupine 仅 `dlopen` 它（`server_checkpoint.cpp:78-126`）。 |
| 制品 | **lupine 仓库不构建、不发布 `liblupinecr` 编译制品**（CI workflow 均无此产出；`CMakeLists.txt:251-257`
  只把 `test/test_checkpoint_provider.c` 编成测试用的 no-op provider）。需要 checkpoint 能力的部署方须**自研/自建**
  provider 并放到 `LD_LIBRARY_PATH` 或经本变量指定。 |

---

## 2. 路由与本地 GPU

### `LUPINE_DISABLE_LOCAL`
| | |
|---|---|
| 作用域 | client（`client.cpp:651`） |
| 默认 | 未设置 = 启用本地 GPU（若检测到真 libcuda 则加入设备表，且排在远程之前） |
| 语义 | 置为任意非 `0`/`false`/`no` 值即**禁用本地 GPU 探测**，设备表只含远程设备。 |
| 语义细节 | 由 `lupine_env_enabled` 判定（`client.cpp:641-645`）。 |

**对设计的意义（关键）**：**所有远程验收/生产客户端必须设 `LUPINE_DISABLE_LOCAL=1`**。否则同机/有 GPU 的客户端
会把设备 0 路由到本地（`routing.cpp:226-244`），CUDA 调用根本不经过 server（设计 §4.3.2 实测教训）。

### `LUPINE_REAL_LIBCUDA`
| | |
|---|---|
| 作用域 | client（`client.cpp:654`） |
| 默认 | 依次尝试 `/usr/lib/x86_64-linux-gnu/libcuda.so.1`、aarch64 变体、`/usr/lib64/libcuda.so.1`、
  `/usr/lib/wsl/lib/libcuda.so.1` |
| 语义 | 覆盖"本地真 libcuda"路径，供 lupine-client 探测本地 GPU / 本地路由用。vgpu-manager 纯远程场景通常不设。 |

### `LUPINE_DRIVER_VERSION_OVERRIDE`
| | |
|---|---|
| 作用域 | client（codegen `gen_client.cpp:97, 112`） |
| 默认 | 无（返回 server 真驱动版本） |
| 语义 | 若设置，`cuDriverGetVersion` 返回 `atoi(value)`，覆盖 server 驱动版本。用于蒙混 cudart 的最小驱动版本校验。 |
| 风险 | 伪造版本可能让 cudart 走不存在的 API 路径，慎用。 |

---

## 3. 符号面 / 日志 / 统计 / Python

### 符号面（默认值以代码为准）

| 变量 | 默认 | 语义 | 位置 |
|---|---|---|---|
| `LUPINE_STUB_MISSING` | **启用**（未设置即启用；设 `0` 才禁用） | `dlsym` 遇到未知 `cu*` 时返回生成的报错 stub 而非 NULL | `client.cpp:160-166` |
| `LUPINE_STUB_PRIVATE_EXPORTS` | 禁用（需显式设非 `0`） | 启用 stub 私有导出仿真 | `client.cpp:2608-2614` |
| `LUPINE_REMOTE_PRIVATE_EXPORTS` | **启用**（设 `0` 才禁用） | 启用远程私有导出表 | `client.cpp:2616-2622` |
| `LUPINE_PRIVATE_EXPORT_TABLES` | 无 | 逗号分隔 `uuid:tableid`，配置私有导出表映射 | `client.cpp:2860-2874` |

> 这组变量面向 `cuGetExportTable` 等私有/调试面，vgpu-manager 场景保持默认即可。

### 日志 / 调试

| 变量 | 默认 | 语义 | 位置 |
|---|---|---|---|
| `LUPINE_LOG_LEVEL` | `debug` | `none`/`0`、`error`/`1`、`debug`/`2`；控制 `LUPINE_LOG_ERROR/DEBUG`（stderr） | `lupine_log.h:22` |
| `LUPINE_TRACE` | 关 | `0`/空=关；`1`=stdout；`2`=stderr；其他非空=文件路径（追加）。**client/server 共用**；`LUPINE_SERVER_TRACE` 已废弃 | `lupine_log.h:44` |
| `LUPINE_DEBUG` | 关 | 启用 HTTP/2 层调试；trace 开启时自动启用 | `h2.cpp:430-435` |

### 统计

| 变量 | 默认 | 语义 | 位置 |
|---|---|---|---|
| `LUPINE_RPC_STATS` | 无 | 设置后把每 RPC op 的 `op<TAB>count<TAB>wait_ns` dump 到该文件 | `rpc.cpp:362-380` |

### Python 适配器（可选）

| 变量 | 默认 | 语义 | 位置 |
|---|---|---|---|
| `LUPINE_LIBCUDA` | 仓库内 `../build/libcuda.so.1` | `lupine.connect()` 用 `ctypes.CDLL` 提前加载的 shim 路径 | `python/lupine/__init__.py:95` |
| `LUPINE_LIB` | `/opt/lupine/lib/libcuda.so.1`（镜像内） | **部署约定**，供镜像/test 脚本引用 shim 路径；C shim 进程本身不读取 | `Dockerfile:118-119`、`local.sh:220` 等 |

---

## 4. 易混淆项：编译期宏 ≠ 环境变量

以下以 `LUPINE_` 开头但**不是环境变量**，勿在部署时设置：

- `LUPINE_CUDA_VERSION`（编译期注入的 CUDA 版本，作为 `x-lupine-cuda-version` 响应头发给客户端）
- `LUPINE_RPC_*`（RPC op id，CRC32 哈希）
- `LUPINE_ROUTE_LOCAL/REMOTE/INVALID/UNKNOWN_DEVICE`（路由枚举）
- `LUPINE_LOG_LEVEL_NONE/ERROR/DEBUG`、`LUPINE_LOG_AT` 等（日志枚举/宏）
- `LUPINE_COMPRESS_BLOCK_BYTES`、`LUPINE_EVENT_QUERY_BATCH_MAX`、`LUPINE_DEVICE_SNAPSHOT_NAME_BYTES` 等（常量）

---

## 5. 对 vgpu-manager 远程 GPU 设计的使用矩阵

| 设计环节 | 需要设置/依赖的变量 | 说明 |
|---|---|---|
| 客户端注入（DRA/device-plugin Allocate） | `LUPINE_SERVER`、`LUPINE_SESSION`、`LUPINE_DISABLE_LOCAL=1` | 三者缺一不可：连哪、是谁、强制远程 |
| 容器判别（方案 C §6.2.1） | `LUPINE_SESSION` | server 子进程据其派生 `VGPU_CONFIG_PATH`；消毒+令牌+fail-closed |
| server 部署（GPU 节点） | `LUPINE_PORT` | 默认 14833，节点网络放行 |
| 验收测试（§4.3.2） | `LUPINE_DISABLE_LOCAL=1` | 无 GPU 客户端或强制远程，否则远程路径不执行 |
| cudart 版本校验兜底 | `LUPINE_DRIVER_VERSION_OVERRIDE`（慎用） | 一般依赖 server 驱动版本 ≥ pod cudart（§7.2） |
| 排查/性能 | `LUPINE_TRACE=2`、`LUPINE_LOG_LEVEL=debug`、`LUPINE_RPC_STATS=<file>` | 追踪链路、RPC 耗时 |
| 混合本地+远程 | `LUPINE_REAL_LIBCUDA`、不设 `LUPINE_DISABLE_LOCAL` | 设备表本地在前、远程在后 |
| checkpoint（不用） | `LUPINE_CHECKPOINT_LIBRARY` | vgpu-manager 不依赖 checkpoint |

> 注：`LUPINE_SESSION` 的取值在我们设计中应由**控制面签发不可预测令牌**（设计 §6.2.1），而非裸 pod UID，
> 因为 lupine 对 session id 完全不做校验。
