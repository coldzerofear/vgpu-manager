# deploy/dra-remote：远程 GPU（DRA）直铺部署

与 `deploy/dra-local/` 同风格的直接 `kubectl apply` 部署集，部署 vgpu-manager 远程 GPU
（lupine 数据面）的全部 k8s 组件。设计背景见
`docs/remote_gpu_k8s_integration_design.md`（v2.x 统一设备模型：远程不是新资源池，
是既有设备的 `accessMode=remote` 发布属性 + pool nodeSelector 放宽）。

## 组件与拓扑

| 文件 | 组件 | 部署位置 | 作用 |
|---|---|---|---|
| `vgpu-manager-dra-gpu-server.yaml` | remote-agent + lupine-server + device-monitor（一个 DaemonSet 三容器） | GPU 节点（`vgpu-manager.io/remote-server=true`） | 会话物化/EnsureSession gRPC(:14834)、远程 GPU 数据面(:14833)、指标（远程会话按 PID 归账） |
| `vgpu-manager-dra-remote-server.yaml` | kubelet-plugin `--plugin-mode=server` | GPU 节点（同上标签） | **只发布不分配**：设备叠加 `accessMode=remote`/`endpoint` 属性、pool nodeSelector 放宽；不向 kubelet 注册 DRA 服务 |
| `vgpu-manager-dra-remote-inject.yaml` | kubelet-plugin `--plugin-mode=inject` + （可选）client 制品 init 容器 + 远程 DeviceClass | 消费节点 **及 GPU 节点**（`vgpu-manager.io/remote-inject=true`） | 节点上唯一注册的 DRA 插件：令牌/EnsureSession 屏障/env+CDI 注入；铺 lupine-client 版本目录 |
| `vgpu-manager-dra-gpu-server-networkpolicy.yaml` | NetworkPolicy（可选） | GPU 节点的服务端 Pod | 只在 `hostNetwork: false` **且**命名空间有 default-deny 时才需要，见"按域名寻址" |
| `vgpu-manager-dra-webhook.yaml` | device-webhook | 控制面节点 | 准入 + 资源声明→DRA 转换（转到 `vgpu-manager` class） |

关键拓扑约束（v2.1 设计）：GPU 节点上 server 插件只发布、inject 插件独占 kubelet 注册；
pod 即使调度到 GPU 节点本机，也经 lupine 环回消费。因此 **GPU 节点必须同时打两个标签、
也必须铺 client 制品**（inject DaemonSet 覆盖它即可满足）。

## 部署步骤

```bash
# 1. 打标签：GPU 节点两个都打；纯消费节点只打 remote-inject
kubectl label node <gpu-node> vgpu-manager=dra-remote vgpu-manager.io/remote-server=true vgpu-manager.io/remote-inject=true
kubectl label node <consumer-node> vgpu-manager.io/remote-inject=true

# 2. 按需修改下表参数后 apply（webhook 若集群已有本地版部署则跳过，见文件头注释）
kubectl apply -f vgpu-manager-dra-gpu-server.yaml -f vgpu-manager-dra-remote-server.yaml -f vgpu-manager-dra-remote-inject.yaml
kubectl apply -f vgpu-manager-dra-webhook.yaml -f vgpu-manager-deviceclass.yaml

# 3. 消费：pod 直接写引用 vgpu-manager 的 ResourceClaim/Template；
#    或走 webhook 转换（资源声明 + 注解 nvidia.com/vgpu-access-mode: remote）
```

验证：`kubectl get resourceslice` 应看到 GPU 节点的 slice 带 `nodeSelector` 与
`accessMode: remote`/`endpoint` 属性；无 GPU 节点上的消费 pod 能跑通 CUDA。

## 需要自行修改的部署参数

| 参数 | 位置 | 默认/占位值 | 说明 |
|---|---|---|---|
| **lupine-server 镜像** | `vgpu-manager-dra-gpu-server.yaml` → 容器 `lupine-server` `image` | `ghcr.io/coldzerofear/lupine-server-static:cuda-13.3.1`（fork 自产静态镜像） | 只依赖 glibc，不带 cuda-compat：**镜像 CUDA 版本必须 ≤ 节点驱动支持的 CUDA**（13.3 需驱动 ≥ 580，老驱动换 12.9.1 / 11.8.0）；隔离库不用打进镜像（见下一行）；正式环境改用 release tag 或 `@sha256` digest，并与 client 制品同一 release |
| **隔离库 .so 路径** | 同上 `LD_PRELOAD` / `LUPINE_CHECKPOINT_LIBRARY` | `/etc/vgpu-manager/driver/libvgpu-control.so` | init-install 容器把它从 vgpu-manager 镜像落盘到节点 hostPath，server 容器挂载即得；两个变量指向同一个 .so（既是 hook 库又是 checkpoint provider），一般不用改 |
| **lupine-client 制品** | 自动：节点上没有可用版本目录时，inject 在 NodePrepare 内通过 agent 的 `FetchClientBundle` 从 lupine-server 拉取其内嵌的 client bundle（校验 etag / content-digest / manifest sha256）落盘为 `<floor CUDA 版本>/` 并记录 `.etag`；server 换构建后 etag 变化会自动重新拉取。可选预铺：`vgpu-manager-dra-remote-inject.yaml` → initContainers | `ghcr.io/coldzerofear/lupine-client-static:cuda-13.3.1` / `cuda-12.9.1` | 预铺目录**必须与 server 镜像来自同一 release**（没有 `.etag`，inject 无法校验，按运维背书原样使用）；自动拉取的目录带 `.etag`，并向 pod 注入 `LUPINE_CLIENT_ETAG`/`LUPINE_CLIENT_PLATFORM`，server 会对不一致的 client 直接回 426；`/artifacts` 载体镜像（静态 client 的 `libcuda.so.1`/`libnvidia-ml.so.1`）；每个 CUDA 版本一个 init 容器，落盘目录名必须是版本号（选择规则 = 取 ≤ server CUDA 上限的最高版本）；增删版本 = 增删 init 容器后滚动；新制品镜像还带 `nvidia-smi`，inject 会把它只读挂到 pod 的 `/usr/bin/nvidia-smi`（单文件 bind，不覆盖镜像目录；旧制品没有就跳过），pod 里跑它看到的是远程会话视图 |
| **server 状态（版本 / endpoint）** | 自动：remote-agent 每 5s GET `http://<REMOTE_SERVER_ENDPOINT>/` 读响应头 `x-lupine-cuda-version`；dra-server 只向 agent 的 `ServerInfo` gRPC 取结果（5s 一次直到首次成功，之后 60s） | — | dra-server / inject **不再直接访问 lupine-server**，只需知道 agent 地址。发布为设备属性 `serverCudaVersion`（inject 选制品按 **min(驱动上限, server 版本)** 取 ≤ 的最高版本）与 `serverEndpoint`；版本或地址变化都会自动重发 slice。agent 探测地址是回环时，会在本机地址里找一个 server 同样应答的（优先节点 InternalIP，物理网卡优先于 docker/cni/flannel 等虚拟网卡）作为对外 endpoint，并粘住直到它不再应答 |
| **vgpu-manager 镜像** | 四个文件所有 `coldzerofear/vgpu-manager-dra:latest` | latest | 换成内网 registry / 钉版本；remote-server 的 agent 容器要求镜像内含 `remote-agent` 二进制 |
| **可达域 selector** | `vgpu-manager-dra-remote-server.yaml` → `REMOTE_NODE_SELECTOR` | `vgpu-manager.io/remote-inject=true` | 标准 label selector 语法（`k=v,k2 in (a,b),!k3`）；决定 pool 可调度到哪些节点。**要允许本机消费必须覆盖 GPU 节点自身**（默认值配合上面打标签方式已覆盖） |
| **服务端端口** | `vgpu-manager-dra-gpu-server.yaml` 的 `LUPINE_PORT`（agent 与 lupine-server 两个容器各一份，一起改）；agent 的 `REMOTE_SERVER_ENDPOINT` / `ADVERTISE_SERVER_ENDPOINT` 都从它展开 | `14833` | **dra-server 不再配置它**：对外地址由 agent 报告（自动发现或 advertise），发布为设备属性 `serverEndpoint`；inject 在 EnsureSession 回包里也拿到它，所以即使属性还没发布出来（或调度器忽略了污点）也能正确注入。改端口只改 `LUPINE_PORT` 那两处（同一个值，两个容器） |
| **agent 端口** | `vgpu-manager-dra-gpu-server.yaml` 的 `LISTEN_PORT`（`LISTEN_SERVER_ENDPOINT` 与 `ADVERTISE_AGENT_ENDPOINT` 从它展开；套接字仍写在 `LISTEN_SERVER_ENDPOINT` 里）、`vgpu-manager-dra-remote-server.yaml` `REMOTE_AGENT_ENDPOINT`（本机怎么连 agent：grpc:// 留空 host = 节点 InternalIP，或 unix://） | `:14834` | agent 自己报告对外可达的 `grpc://<可路由 host>:<TCP 端口>`，发布为设备属性 `agentEndpoint`，inject 按它调 EnsureSession（dra-inject 无需再配端口）。unix 套接字只供同节点组件，不会被发布；agent 只监听 unix 时没有 agentEndpoint，设备保持污点 |
| **monitor 端口** | `vgpu-manager-dra-gpu-server.yaml` `--server-bind-port` | `3456` | hostNetwork，与节点上其他进程冲突时修改（Service targetPort 联动） |
| **SM watcher** | `vgpu-manager-dra-remote-server.yaml` 与 `vgpu-manager-dra-gpu-server.yaml` 两处 `FEATURE_GATES` 的 `SharedSMUtilizationWatcher` | 均开启 | 联动开关：dra-server 写节点级采样缓存，agent 把会话标记为使用它。关闭时两处同时关 |
| **webhook DRA class** | `vgpu-manager-dra-webhook.yaml` `--vgpu-device-class-name`，与 `vgpu-manager-deviceclass.yaml` 里的 class 名一致 | `vgpu-manager` | webhook 目前只转换一个 class。集群同时有 dra-local 时两边的 class 同名同选择器，claim 可能拿到本地卡——要区分就给远程 class 换个名字并打开 deviceclass 里注释的 accessMode 选择器 |
| **整卡远程 class** | `vgpu-manager-deviceclass.yaml` 再加一个 `type == 'gpu'` 的 class（本地版见 `deploy/dra-local` 的 `gpu-manager`） | 未提供 | dra-server 关掉 `VGPUSupport` 改发布 `type=gpu` 时才需要 |
| **NRI 按容器会话** | `vgpu-manager-dra-remote-inject.yaml` `FEATURE_GATES` 加 `NRISupport=true` + 放开 nri-root 挂载注释 | 关闭 | 开启后同 claim 不同容器各自独立会话记账（需 containerd NRI 开启） |
| **命名空间** | 全部文件 | `kube-system` | 整体替换时注意 webhook 证书 dnsNames 联动 |

## 按域名寻址（集群禁止 hostNetwork 时）

默认形态是 `hostNetwork: true`：节点 IP 天生稳定，数据面也不经 CNI 封装。集群策略禁止
hostNetwork（PSS `baseline` 连 hostPort 一起禁）时，服务端 Pod 每次重建都换 IP，而 inject
注入给消费容器的 `LUPINE_SERVER` 只在 NodePrepare 时写一次、容器重启不会重写——连着旧 IP
的容器恢复不了。解决办法是给每个节点的服务端 Pod 一个稳定域名：

```
<spec.hostname>.<headless svc>.<namespace>.svc.<cluster domain>
```

DaemonSet 自己做不到：它只有一份 Pod 模板，`spec.hostname` 也不支持字段引用。所以由
device-webhook 在准入时按目标节点名生成（本目录的 webhook 清单已含 `/pods/hostname` 入口）。

**切换步骤**（都在 `vgpu-manager-dra-gpu-server.yaml` 里，按注释打开）：

1. Pod 模板标签打开 `vgpu-manager.io/node-hostname: "true"`；
2. `hostNetwork: false`、`dnsPolicy: ClusterFirst`、`subdomain: vgpu-manager-dra-gpu-server-headless`；
3. remote-agent 打开 `POD_NAMESPACE` / `HEADLESS_SERVICE_NAME` / `CLUSTER_DOMAIN` /
   `ADVERTISE_SERVER_ENDPOINT` / `ADVERTISE_AGENT_ENDPOINT` 这一组环境变量。

几个要点：

- **两个 ADVERTISE 都要开**：`serverEndpoint` 与 `agentEndpoint` 都是 dra-server 从 agent 的
  `ServerInfo` 取来发布成设备属性的。只开 server 那条时，`agentEndpoint` 仍是 Pod IP——Pod 重建后
  要等一轮重发才恢复，窗口内 inject 的 EnsureSession 会失败。
- **节点名 → hostname 的转换**：节点名是 DNS subdomain（可带点、最长 253），`spec.hostname` 必须是
  DNS label。需要改写时（大写、下划线、点、超长）会追加节点名摘要，否则 `a.b` 与 `a-b` 会撞成同一个
  域名，客户端可能被解析到另一台 GPU 服务器。
- **headless Service 的 `publishNotReadyAddresses: true`** 是刻意的：记录只要 Pod 有 IP 就存在，不随
  Pod 就绪状态抖动（服务端是否可用由设备属性与 `remote-unavailable` 污点表达），也避免重建期间的
  NXDOMAIN 被 CoreDNS 否定缓存住（默认 30s）。
- **消费侧必须能解析集群 DNS**：默认 `ClusterFirst` 即可，`hostNetwork: true` 的业务 Pod 必须写
  `dnsPolicy: ClusterFirstWithHostNet`。校验 webhook 会拒绝 `vgpu-access-mode: remote` 且
  `dnsPolicy: Default`、或 `hostNetwork` + `ClusterFirst` 的 Pod；`None` 要求自带 `dnsConfig.nameservers`。
- **代价**：数据面改走 CNI，overlay 封装与 MTU 会吃掉一部分 H2D/D2H 带宽；能用 hostNetwork 时仍然推荐
  hostNetwork。headless Service 在 hostNetwork 模式下也可以照常部署，那时记录解析到节点 IP。
- **命名空间有 default-deny 时**再部署 `vgpu-manager-dra-gpu-server-networkpolicy.yaml`：它给出服务端入站的
  最小集合（14833 来自所有命名空间的 Pod——远程负载是任意业务 Pod；14834 来自 dra-inject；3456 按需打开），
  只声明 Ingress（出站要放行 API server 与 DNS，地址随集群而变，模板在文件末尾）。没有 default-deny 时
  不要部署它：那是收紧而不是放行。kubelet 探针来自节点而非 Pod，只能用 `ipBlock` 表达（Calico / Cilium
  默认放行 host → 本机 Pod，通常不必加）；策略由 CNI 执行，Flannel 这类没有策略插件的 CNI 会直接忽略。

## 端口一览（GPU 节点 hostNetwork）

| 端口 | 进程 | 用途 |
|---|---|---|
| 14833 | lupine-server | 远程 GPU 数据面（消费 pod 的 `LUPINE_SERVER` 直连）；同端口也答 HTTP/1.x（版本探测、client bundle 下载 `/.well-known/lupine/client/v1/<platform>`） |
| 14834 | remote-agent | EnsureSession gRPC（dra-inject 在 NodePrepare 内同步调用） |
| 3456 | device-monitor | Prometheus 指标（`/metrics`，含 `container_vgpu_*` 远程归账） |

## 已知边界

- **K1 明文传输**：`LUPINE_SESSION` 令牌以 HTTP/2 头明文传输，多租户/跨信任域前必须
  先落 TLS 方案（设计 D5/§6.1）。
- **remote-agent 的 gRPC（:14834）以 session token 为凭证**：EnsureSession / ReleaseSessions / FetchClientBundle 都要求
  token 已登记在 claim 当前分配上，否则 PermissionDenied；ReleaseSessions 必须给出 token（只给 claim UID 不再释放）。
  token 写在 claim 注解里，集群内有 claim 读权限者可见，所以挡的是网络访问者而非集群内读者（EnsureSession 幂等，
  重放无害）。更强的边界：只监听 unix 套接字（`LISTEN_SERVER_ENDPOINT=unix:///etc/vgpu-manager/agent.sock`），
  或等 D5 的 TLS/身份。
- **首个远程 pod 的冷启动**：节点上没有可用 client 制品时 NodePrepare 会先从 agent 拉 bundle（几十 MB，单次
  60s 超时），期间本节点其他 prepare 串行等待；要避免这段延迟就用 init 容器预铺。被替换的旧目录以 `.stale-*`
  保留给仍在用的 pod，不会自动删除。
- **NodePrepare 串行**：dra-inject 以 `kubeletplugin.Serialize` 串行处理本节点的 Prepare/Unprepare，单次 EnsureSession
  超时 5s；一个失联的 agent 最多让本节点其他 pod 的 prepare 等 5s × 该 claim 跨的 agent 数。
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
