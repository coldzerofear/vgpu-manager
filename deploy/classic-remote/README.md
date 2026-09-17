# deploy/classic-remote：远程 GPU（调度器扩展器 + 设备插件）直铺部署

与 `deploy/classic-local/` 同风格的直接 `kubectl apply` 部署集，跑的是**经典路径**
（scheduler-extender + device-plugin）的远程 vGPU：Pod 在没有 GPU 的消费节点上运行，
CUDA 调用经 lupine 数据面落到 GPU 服务器节点的卡上。集群**不需要 DRA**（`resource.k8s.io`），
1.28 这样的老集群也能用；需要 DRA 版本的同一能力见 `deploy/dra-remote/`。

设计与实现细节见 `docs/deviceplugin_remote_gpu_integration_analysis.md`（§16 为准）。

## 组件与拓扑

| 文件 | 组件 | 部署位置 | 作用 |
|---|---|---|---|
| `vgpu-manager-scheduler.yaml` | kube-scheduler + device-scheduler 扩展器 | 控制面 | 远程 Pod 在**服务器节点**上完成筛选/分配，`predicate-node` = 服务器节点，再把结果映射回消费节点 |
| `vgpu-manager-remote-gpu-server.yaml` | remote-agent + lupine-server + device-monitor（一个 DaemonSet 三容器） | GPU 节点（`vgpu-manager=remote-server`） | 会话物化/回收 + EnsureSession/FetchClientBundle gRPC(:14834)、远程 GPU 数据面(:14833)、指标(:3456，远程容器按会话 PID 归账) |
| `vgpu-manager-deviceplugin-server.yaml` | device-plugin `--remote-server` | GPU 节点（同上标签） | 发布节点设备注册、`remote-server` 角色标签与 `remote-endpoints` 注解、注册 `vgpu-number`；**拒绝一切 Allocate**（本地 Pod 不该来这里） |
| `vgpu-manager-deviceplugin-consumer.yaml` | device-plugin `--remote-consumer` | 消费节点（`vgpu-manager=remote-consumer`） | 注册 `vgpu-number` 槽位、发布 `remote-consumer` 标签；Allocate 时建会话、备客户端 shim、注入环境与挂载 |
| `vgpu-manager-webhook.yaml` | device-webhook | 控制面 | 校验 `vgpu-access-mode` 注解等准入规则（需 cert-manager） |

一个进程同时承担两种角色也是支持的：GPU 节点想顺带消费自己的卡，就给同一个 DaemonSet 加上
`--remote-server --remote-consumer`（此时 `vgpu-number` 数量取 `max(本地槽位, --remote-consumer-number)`），
**不要**在同一节点再起一个消费者 DaemonSet——kubelet 对同名扩展资源只保留最后注册的那个插件。

### 节点标签：运维打的 vs 插件发布的

| 标签 | 谁写 | 用途 |
|---|---|---|
| `vgpu-manager=remote-server` / `vgpu-manager=remote-consumer` | **运维手动打** | 本目录各 DaemonSet 的 `nodeSelector` |
| `nvidia.com/remote-server=true` | 服务者插件发布，退出时删 | 调度器识别 GPU 服务器；同时让本地 Pod 避开该节点 |
| `nvidia.com/remote-consumer=true` | 消费者插件发布，退出时删 | 调度器识别可运行远程 Pod 的节点（还要求 `vgpu-number` 可分配 > 0） |
| `nvidia.com/remote-endpoints`（注解） | 服务者插件发布，每 5s 刷新 | agent / lupine-server 地址与服务器 CUDA 版本；服务不可用时发布 `{}`，调度器随即跳过该节点 |

**不要**把 `nvidia.com/remote-*` 当 DaemonSet 的 `nodeSelector`：它们由插件自己发布，插件没起来时并不存在。

## 部署步骤

```bash
# 1. 打标签
kubectl label node <gpu-node>      vgpu-manager=remote-server
kubectl label node <consumer-node> vgpu-manager=remote-consumer
# GPU 节点也想跑远程 Pod（自产自销）时：不要打上面的 consumer 标签，而是给服务者 DaemonSet
# 同时加 --remote-consumer（见上文"一个进程两种角色"）

# 2. 按下表改完参数后 apply
kubectl apply -f vgpu-manager-scheduler.yaml          # 已部署 classic-local 的调度器则跳过
kubectl apply -f vgpu-manager-remote-gpu-server.yaml
kubectl apply -f vgpu-manager-deviceplugin-server.yaml
kubectl apply -f vgpu-manager-deviceplugin-consumer.yaml
kubectl apply -f vgpu-manager-webhook.yaml            # 可选，需 cert-manager；已部署过则跳过

# 3. 校验
kubectl get node <gpu-node> -o jsonpath='{.metadata.labels.nvidia\.com/remote-server}{"\n"}{.metadata.annotations.nvidia\.com/remote-endpoints}{"\n"}'
#   期望：true 与一段 JSON（serverEndpoint/agentEndpoint/serverCudaVersion）；
#   若是 {} 说明 agent 探不到 lupine-server，调度器会跳过这台服务器
kubectl get node <consumer-node> -o jsonpath='{.status.allocatable.nvidia\.com/vgpu-number}{"\n"}'
```

## 使用示例

远程 Pod = 普通 vGPU Pod + 一条访问模式注解；它调度到消费节点，卡来自某台服务器节点。

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: remote-gpu-pod
  namespace: default
  annotations:
    nvidia.com/vgpu-access-mode: remote   # ← 唯一的远程开关（缺省 local）
spec:
  schedulerName: vgpu-scheduler
  terminationGracePeriodSeconds: 0
  containers:
    - name: default
      image: nvidia/cuda:12.4.1-devel-ubuntu20.04   # glibc 镜像；不要自带真 libcuda.so.1
      command: ["sleep", "9999999"]
      resources:
        limits:
          nvidia.com/vgpu-number: 1     # 远程卡数
          nvidia.com/vgpu-cores: 10     # 算力 %（消费节点不上报这两项，kubelet 准入时会丢掉，
          nvidia.com/vgpu-memory: 1024  #  扩展器仍按它们在服务器侧分配）
```

容器里 `nvidia-smi` 看到的是远程会话视图（新版 client 制品自带 `nvidia-smi`，插件会只读挂到
`/usr/bin/nvidia-smi`）；`LUPINE_SERVER` / `LUPINE_SESSION` 由插件注入，不要自己写。

## 需要自行修改的部署参数

| 参数 | 位置 | 默认/占位值 | 说明 |
|---|---|---|---|
| **vgpu-manager 镜像** | 四个文件所有 `coldzerofear/vgpu-manager:latest` | latest | 换成内网 registry / 钉版本；GPU 服务器那份要求镜像内含 `remote-agent` 二进制 |
| **lupine-server 镜像** | `vgpu-manager-remote-gpu-server.yaml` → 容器 `lupine-server` | `ghcr.io/coldzerofear/lupine-server:cuda-13.3.1-ubuntu24.04` | 静态镜像只依赖 glibc：**镜像 CUDA 版本必须 ≤ 节点驱动支持的 CUDA**（13.3 需驱动 ≥ 580，老驱动换 12.9.1 / 11.8.0）；正式环境用 release tag 或 `@sha256` |
| **客户端 shim 制品** | 自动：消费插件在 `Allocate` 里经 agent 的 `FetchClientBundle` 拉取 lupine-server 内嵌的同构建 bundle（校验 etag/digest 后落盘 `<CUDA 版本>/` 并记 `.etag`），server 换构建后自动重拉。可选预铺：`vgpu-manager-deviceplugin-consumer.yaml` 的 initContainers 注释块 | `ghcr.io/coldzerofear/lupine-client-static:cuda-13.3.1` | 预铺目录必须与 server 同一 release（没有 `.etag`，插件按运维背书原样使用）；目录名必须是版本号（选择规则 = 取 ≤ 服务器 CUDA 版本的最高版本） |
| **消费槽位数** | `vgpu-manager-deviceplugin-consumer.yaml` `--remote-consumer-number` | `1000` | 本节点同时能跑多少个远程 vGPU（上报为 `vgpu-number`）。纯 CPU 节点给足够大的虚数即可，真正的容量约束在服务器侧 |
| **切分参数** | `vgpu-manager-deviceplugin-server.yaml` `--device-split-count` / `--device-memory-scaling` / `--device-cores-scaling` + ConfigMap `nodeConfig.json` | 10 / 1 / 1 | 与本地路径含义相同，作用在服务器节点的卡上；ConfigMap 按节点名覆盖（示例里的 `gpu-node-a` 要改成真节点名） |
| **cgroup driver** | 两个设备插件的 `CGROUP_DRIVER` | `auto` | 探测顺序：kubelet 配置 → kubeadm flags → kubelet 进程 → cgroup 布局；探测失败会退出，此时显式写 `systemd` / `cgroupfs`（消费者 DaemonSet 也因此挂了 `kubelet-root` 与 `cgroup-root`，显式指定后可去掉） |
| **服务端 endpoint** | `remote-gpu-server.yaml`：`LUPINE_PORT` + agent 的 `REMOTE_SERVER_ENDPOINT`（探测地址，默认 `:14833` 即 127.0.0.1）；可选 `ADVERTISE_SERVER_ENDPOINT`（运维指定的对外地址，URL 形态） | `:14833` | 对外地址由 agent 自动发现（探测地址是回环时，在本机地址里找一个 server 同样应答的，优先节点 InternalIP），经服务者插件发布成节点注解；改端口只改这两处 |
| **agent endpoint** | `remote-gpu-server.yaml` `LISTEN_SERVER_ENDPOINT`；`deviceplugin-server.yaml` `--remote-agent-endpoint` | `grpc://:14834` + `unix:///etc/vgpu-manager/agent.sock` | 消费节点按注解里的 `agentEndpoint` 调 agent（不用配）；同节点的服务者插件走 unix 套接字。两处要一致 |
| **会话归属** | `remote-gpu-server.yaml` `SESSION_OWNER` | `pod` | 经典路径 = `pod`；DRA 路径 = `claim`；`auto` = 两种都服务（DRA 与设备插件混布时用，集群没有 DRA API 时只打日志跳过 claim，并需要放开该文件 RBAC 里注释的 `resource.k8s.io` 权限） |
| **SM watcher** | `deviceplugin-server.yaml` 与 `remote-gpu-server.yaml`（agent、monitor）三处 `FEATURE_GATES` / `--feature-gates` | 均开启 | 联动开关：**设备插件是写方**（节点级 SM 采样写到 `/etc/vgpu-manager/watcher/sm_util.config`），agent 把会话标记为使用它，monitor 读它。关就三处一起关 |
| **monitor 端口** | `remote-gpu-server.yaml` `--server-bind-port` | `3456` | hostNetwork，与节点上其他进程冲突时改（Service targetPort 联动） |
| **客户端 etag 校验** | `deviceplugin-consumer.yaml` `IGNORE_CLIENT_SHIM_ETAG` 注释块 | 关闭（即校验） | 打开后不注入 `LUPINE_CLIENT_ETAG`/`LUPINE_CLIENT_PLATFORM`，服务端无法校验 client 构建，不匹配会延后成运行时错误 |
| **命名空间** | 全部文件 | `kube-system` | 整体替换时注意 webhook 证书 dnsNames 联动 |
| **资源域名** | 各组件 `--domain`（未设 = `nvidia.com`） | `nvidia.com` | 改它会同时改掉资源名与所有注解/标签前缀，所有组件必须一起改 |

## 端口一览（GPU 节点 hostNetwork）

| 端口 | 进程 | 用途 |
|---|---|---|
| 14833 | lupine-server | 远程 GPU 数据面（消费 Pod 的 `LUPINE_SERVER` 直连）；同端口也答 HTTP/1.x（版本探测、client bundle 下载） |
| 14834 | remote-agent | EnsureSession / ReleaseSessions / FetchClientBundle gRPC（消费节点的设备插件在 `Allocate` 内同步调用） |
| 3456 | device-monitor | Prometheus 指标（`/metrics`，含 `container_vgpu_*` 远程归账；`/healthz`、`/readyz`） |

## 已知边界

- **每个节点只能有一个 `vgpu-number` 注册者**：本地 vGPU 插件、消费者插件、服务者插件三者互斥。
  同节点既服务又消费用"一个进程两个开关"，不要叠 DaemonSet。
- **服务器节点不接本地 Pod**：调度器对本地 Pod 直接以 `NodeIsRemoteServer` 拒绝该节点（服务器的卡上
  记着别的节点 Pod 的用量，本地 NodeInfo 统计不到）。服务者插件的 `Allocate` 也会拒绝。
- **污点语义**：集群自己打的硬污点（cordon/drain、not-ready、unreachable、out-of-service、资源压力、
  云侧 shutdown，即 `node.kubernetes.io/*` 与 `node.cloudprovider.kubernetes.io/*` 的
  `NoSchedule`/`NoExecute`）未被 Pod 容忍时，该服务器不再接**新**会话，已有会话不受影响；
  运维为隔离打的业务污点（如 `nvidia.com/gpu=true:NoSchedule`）**不影响**远程服务——远程 Pod 本来
  就不在服务器上运行。
- **`vgpu-cores` / `vgpu-memory` 在消费节点不上报**：`RemoteGPUSupport` 与
  `GPUCoreResourcePlugin`/`GPUMemoryResourcePlugin` 互斥。Pod 照常可以写这两项：kube-scheduler 侧
  `ignoredByScheduler: true`，扩展器按它们在服务器侧分配，kubelet 准入时会把节点未上报的扩展资源丢掉。
- **首个远程 Pod 的冷启动**：节点上没有可用 client 制品（或 etag 过期）时，`Allocate` 内会先下载
  bundle（上限 40s）再建会话（5s）。kubelet 准入是串行的，这段时间同节点其他 Pod 的准入排队；
  想避免就用 initContainers 预铺制品。
- **会话随 lupine-server 重启作废**：连接态不可恢复，应用层需自行重试/重启。因此设备插件与
  数据面刻意分成两个 DaemonSet——滚动升级设备插件不会打断在跑的会话。
- **消费镜像约束**：glibc-only（musl/alpine 不支持）；镜像不得自带真 `libcuda.so.1`。
- **monitor 只部署在 GPU 服务器节点**：远程容器的实时用量只能在持有会话的服务器上采集
  （容器级指标 `node=<服务器>`、`pod_node=<消费节点>`、`access_mode=remote`）。纯 CPU 消费节点
  没有 GPU，装了也起不来（NVML 初始化失败），也没有可采的东西。
- **hostPID / hostNetwork 是硬要求**（GPU 服务器那个 Pod）：SESSION 记账要求 `pids.config` 里的
  PID 与 NVML 返回的宿主 PID 对得上；endpoint 要是节点地址而不是 Pod IP。
- **remote-agent 的 gRPC(:14834) 以会话 token 为凭证**：token = `sha256(<podUID>_<容器名>)` 前 32 位，
  agent 会回查 Pod（仍在用本节点的卡、容器名在预分配里）才放行；它挡的是网络访问者，不是集群内
  有 Pod 读权限的人。更强的边界：让 agent 只监听 unix 套接字（则消费节点无法远程建会话，只适合
  自产自销的单节点形态），或等 TLS 方案。
- **明文传输**：`LUPINE_SESSION` 与数据面都是明文，多租户/跨信任域前需要先落 TLS。
- **会话目录固定在 `/etc/vgpu-manager/remote-sessions`**（agent / lupine-server / monitor 经
  manager-root hostPath 共享）；SM watcher 共享缓存靠 agent 启动时把 `<会话根>/watcher` 软链到
  `../watcher` 桥接，所以会话根必须直接位于 manager 目录下（默认布局即满足）。
