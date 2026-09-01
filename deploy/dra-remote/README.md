# deploy/dra-remote：远程 GPU（DRA）直铺部署

与 `deploy/dra/` 同风格的直接 `kubectl apply` 部署集，部署 vgpu-manager 远程 GPU
（lupine 数据面）的全部 k8s 组件。设计背景见
`docs/remote_gpu_k8s_integration_design.md`（v2.x 统一设备模型：远程不是新资源池，
是既有设备的 `accessMode=remote` 发布属性 + pool nodeSelector 放宽）。

## 组件与拓扑

| 文件 | 组件 | 部署位置 | 作用 |
|---|---|---|---|
| `remote-server.yaml` | remote-agent + lupine-server + device-monitor（一个 DaemonSet 三容器） | GPU 节点（`vgpu-manager.io/remote-server=true`） | 会话物化/EnsureSession gRPC(:14834)、远程 GPU 数据面(:14833)、指标（远程会话按 PID 归账） |
| `dra-server.yaml` | kubelet-plugin `--plugin-mode=server` | GPU 节点（同上标签） | **只发布不分配**：设备叠加 `accessMode=remote`/`endpoint` 属性、pool nodeSelector 放宽；不向 kubelet 注册 DRA 服务 |
| `dra-inject.yaml` | kubelet-plugin `--plugin-mode=inject` + client 制品 init 容器 + 远程 DeviceClass | 消费节点 **及 GPU 节点**（`vgpu-manager.io/remote-inject=true`） | 节点上唯一注册的 DRA 插件：令牌/EnsureSession 屏障/env+CDI 注入；铺 lupine-client 版本目录 |
| `dra-webhook.yaml` | device-webhook | 控制面节点 | 准入 + 资源声明→DRA 转换（转到 `remote-vgpu-manager` class） |

关键拓扑约束（v2.1 设计）：GPU 节点上 server 插件只发布、inject 插件独占 kubelet 注册；
pod 即使调度到 GPU 节点本机，也经 lupine 环回消费。因此 **GPU 节点必须同时打两个标签、
也必须铺 client 制品**（inject DaemonSet 覆盖它即可满足）。

## 部署步骤

```bash
# 1. 打标签：GPU 节点两个都打；纯消费节点只打 remote-inject
kubectl label node <gpu-node> vgpu-manager=dra-driver-remote vgpu-manager.io/remote-server=true vgpu-manager.io/remote-inject=true
kubectl label node <consumer-node> vgpu-manager.io/remote-inject=true

# 2. 按需修改下表参数后 apply（webhook 若集群已有本地版部署则跳过，见文件头注释）
kubectl apply -f remote-server.yaml -f dra-server.yaml -f dra-inject.yaml
kubectl apply -f dra-webhook.yaml

# 3. 消费：pod 直接写引用 remote-vgpu-manager 的 ResourceClaim/Template；
#    或走 webhook 转换（资源声明 + 注解 nvidia.com/vgpu-access-mode: remote）
```

验证：`kubectl get resourceslice` 应看到 GPU 节点的 slice 带 `nodeSelector` 与
`accessMode: remote`/`endpoint` 属性；无 GPU 节点上的消费 pod 能跑通 CUDA。

## 需要自行修改的部署参数

| 参数 | 位置 | 默认/占位值 | 说明 |
|---|---|---|---|
| **lupine-server 镜像** | `remote-server.yaml` → 容器 `lupine-server` `image` | `ghcr.io/coldzerofear/lupine-server-static:cuda-13.3.1`（fork 自产静态镜像） | 只依赖 glibc，不带 cuda-compat：**镜像 CUDA 版本必须 ≤ 节点驱动支持的 CUDA**（13.3 需驱动 ≥ 580，老驱动换 12.9.1 / 11.8.0）；隔离库不用打进镜像（见下一行）；正式环境改用 release tag 或 `@sha256` digest，并与 client 制品同一 release |
| **隔离库 .so 路径** | 同上 `LD_PRELOAD` / `LUPINE_CHECKPOINT_LIBRARY` | `/etc/vgpu-manager/driver/libvgpu-control.so` | init-install 容器把它从 vgpu-manager 镜像落盘到节点 hostPath，server 容器挂载即得；两个变量指向同一个 .so（既是 hook 库又是 checkpoint provider），一般不用改 |
| **lupine-client 制品镜像** | `dra-inject.yaml` → initContainers | `ghcr.io/coldzerofear/lupine-client-static:cuda-13.3.1` / `cuda-12.9.1` | **必须与 server 镜像来自同一 release**（lupine RPC 协议没有版本号也没有校验，混用会在运行时报未知 opcode）；`/artifacts` 载体镜像（静态 client 的 `libcuda.so.1`/`libnvidia-ml.so.1`）；每个 CUDA 版本一个 init 容器，落盘目录名必须是版本号（选择规则 = 取 ≤ server CUDA 上限的最高版本）；增删版本 = 增删 init 容器后滚动 |
| **server CUDA 版本探测** | 自动：dra-server 定期 GET `http://<endpoint>/`，读响应头 `x-lupine-cuda-version` | 5s 一次直到首次成功，之后 60s | 发布为设备属性 `serverCudaVersion`；inject 选制品按 **min(驱动上限, server 版本)** 取 ≤ 的最高版本。server 比 dra-server 晚起也没关系，探到后自动重发 slice。remote-agent 也用同一个 GET 判断 server 是否就绪 |
| **vgpu-manager 镜像** | 四个文件所有 `coldzerofear/vgpu-manager-dra:latest` | latest | 换成内网 registry / 钉版本；remote-server 的 agent 容器要求镜像内含 `remote-agent` 二进制 |
| **可达域 selector** | `dra-server.yaml` → `REMOTE_NODE_SELECTOR` | `vgpu-manager.io/remote-inject=true` | 标准 label selector 语法（`k=v,k2 in (a,b),!k3`）；决定 pool 可调度到哪些节点。**要允许本机消费必须覆盖 GPU 节点自身**（默认值配合上面打标签方式已覆盖） |
| **服务端 endpoint** | `remote-server.yaml` `LUPINE_PORT`、`dra-server.yaml` `REMOTE_SERVER_ENDPOINT` | `:14833`（host 留空 = 自动取节点 InternalIP） | URL 形态（可带 `https://`、域名、路径前缀，为将来 DNS + 网关路由预留）；改端口时与 `LUPINE_PORT` 联动；发布为设备属性 `serverEndpoint` |
| **agent endpoint** | `remote-server.yaml` `LISTEN_SERVER_ENDPOINT`、`dra-server.yaml` `REMOTE_AGENT_ENDPOINT` | `:14834`（host 留空 = 同服务端 host） | 两处端口必须一致（hostNetwork）；发布为设备属性 `agentEndpoint`，inject 按它调 EnsureSession（dra-inject 无需再配端口） |
| **monitor 端口** | `remote-server.yaml` `--server-bind-port` | `3456` | hostNetwork，与节点上其他进程冲突时修改（Service targetPort 联动） |
| **SM watcher** | `dra-server.yaml` 与 `remote-server.yaml` 两处 `FEATURE_GATES` 的 `SharedSMUtilizationWatcher` | 均开启 | 联动开关：dra-server 写节点级采样缓存，agent 把会话标记为使用它。关闭时两处同时关 |
| **webhook DRA class** | `dra-webhook.yaml` `--vgpu-device-class-name` | `remote-vgpu-manager` | 集群同时有本地 vGPU 时按主要路径取舍（webhook 目前单 class 转换） |
| **整卡远程 class** | `dra-inject.yaml` 末尾注释块 | 注释 | dra-server 关 `VGPUSupport` 发布 `type=gpu` 时启用 `remote-gpu-manager` |
| **NRI 按容器会话** | `dra-inject.yaml` `FEATURE_GATES` 加 `NRISupport=true` + 放开 nri-root 挂载注释 | 关闭 | 开启后同 claim 不同容器各自独立会话记账（需 containerd NRI 开启） |
| **命名空间** | 全部文件 | `kube-system` | 整体替换时注意 webhook 证书 dnsNames 联动 |

## 端口一览（GPU 节点 hostNetwork）

| 端口 | 进程 | 用途 |
|---|---|---|
| 14833 | lupine-server | 远程 GPU 数据面（消费 pod 的 `LUPINE_SERVER` 直连）；同端口也答 HTTP/1.x（版本探测、client bundle 下载 `/.well-known/lupine/client/v1/<platform>`） |
| 14834 | remote-agent | EnsureSession gRPC（dra-inject 在 NodePrepare 内同步调用） |
| 3456 | device-monitor | Prometheus 指标（`/metrics`，含 `container_vgpu_*` 远程归账） |

## 已知边界

- **K1 明文传输**：`LUPINE_SESSION` 令牌以 HTTP/2 头明文传输，多租户/跨信任域前必须
  先落 TLS 方案（设计 D5/§6.1）。
- **会话随 lupine-server 重启作废**：连接态不可恢复，应用层需自行重试/重启（设计固有约束）。
- **消费镜像约束**：glibc-only（musl/alpine 不支持）；镜像不得自带真 `libcuda.so.1`。
- `dra-server` 开启 RemoteGPUSupport 后禁止 `--http-endpoint`/`--healthcheck-port`
  （启动校验拦截），故该 DaemonSet 无探针；健康观测走 remote-server 的 monitor。
- 会话目录固定在节点 `/etc/vgpu-manager/remote-sessions`（agent/lupine-server/monitor
  三容器经 manager-root hostPath 共享）；agent 的就绪文件在 pod 级 emptyDir
  （`/run/vgpu/ready`），避免 hostPath 上的陈旧文件破坏启动排序。
- **SM watcher 共享缓存的路径桥接**：库在会话模式下从 `<会话根>/watcher/sm_util.config` 读共享采样缓存，
  而写入方（dra-server 的 watcher 线程）写在 `/etc/vgpu-manager/watcher/`。agent 启动时会把
  `<会话根>/watcher` 建成指向 `../watcher` 的软链接完成桥接——因此会话根必须直接位于 manager 目录下
  （默认布局即满足），不要单独改动其一。
- **探测要求 server 会答 HTTP/1.x**（lupine #660 之后的构建，本 fork 所有镜像都满足）：agent 用它判断 server
  就绪，dra-server 用它读 server CUDA 版本；更老的 server 会一直被判为未就绪。TLS 前置代理（D5）需同时透传
  h2c 与 HTTP/1.1。
- **消费侧内存镜像（identity-VA/DSM）的运行时约束**：client 进程会预留 1 TiB 虚拟地址（PROT_NONE + NORESERVE，
  8 个槽位）、自装 SIGSEGV 处理器、按页 mprotect。严格 overcommit（`vm.overcommit_memory=2`）或 `ulimit -v` 会拒绝
  预留；大量 pinned/managed host 内存且写入分散的负载可能撞 `vm.max_map_count`（默认 65530，需节点 sysctl）；
  后装且不链式转发 SIGSEGV 的运行时（部分 JVM 配置）需实测。
