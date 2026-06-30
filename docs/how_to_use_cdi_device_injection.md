# CDI 设备注入 (CDI Device Injection)

## 1. 背景

vgpu-manager 的设备插件原先只支持两种把「设备列表」传递给底层容器运行时的方式，通过
`--device-list-strategy` 选择：

- `envvar`：设置 `NVIDIA_VISIBLE_DEVICES=<uuid,...>` 环境变量；
- `volume-mounts`：在 `/var/run/nvidia-container-devices/<uuid>` 下挂载占位卷。

两者都依赖 NVIDIA 容器运行时读取该信号后注入 GPU 驱动库。本特性新增对
[Container Device Interface (CDI)](https://github.com/cncf-tags/container-device-interface)
的支持，对齐 NVIDIA k8s-device-plugin 的实现：

- `cdi-annotations`：在 `Allocate` 响应里写入 CDI 容器注解，运行时据此注入设备；
- `cdi-cri`：在 `Allocate` 响应的 `CDIDevices` 字段返回受限定的设备名，由 kubelet/CRI 传给运行时。

`--device-list-strategy` 现在支持**多个策略组合**（逗号分隔，或在 NodeConfig 中写成列表），
例如 `envvar,cdi-annotations`。

## 2. 关键设计

### 2.1 与 NVIDIA 插件的差异

在 NVIDIA 插件里，`device-list-strategy` 决定**整个**注入过程（开启 CDI 时会提前返回、
跳过 `passDeviceSpecs`）。vgpu-manager 是 HAMi 风格的 vGPU 插件，它的注入分成两部分：

1. **始终无条件**注入的内容（与策略无关）：`/dev/nvidia*` 设备节点（`PassDeviceSpecs`）、
   `libvgpu-control.so` 与 `ld.so.preload`、`vgpu.config`、显存/算力限制环境变量等；
2. **由策略决定**的内容：仅「如何把 GPU 列表告知运行时以注入驱动库」这一层。

因此本特性的设计原则是：**CDI 只替换第 2 部分**（驱动库注入通道），第 1 部分保持原样。
即使开启 CDI，`PassDeviceSpecs` 仍会无条件注入设备节点 —— 这与 CDI 规范里的
`deviceNodes` 冗余但幂等、安全，且最大限度不改变既有行为。

### 2.2 CDI 规范文件由插件自生成

插件在启动时使用 `nvidia-container-toolkit` 的 `nvcdi` 库生成节点的 CDI 规范文件，
写入宿主机的 CDI 目录（默认 `/var/run/cdi`），描述每个限定设备
`k8s.device-plugin.nvidia.com/gpu=<uuid>`。这一步复用设备管理器已有的 NVML/nvlib 句柄
（`DeviceManager.DeviceLib` 同时实现 nvml / device / info 三个接口），不会重复初始化 NVML。

当未启用任何 CDI 策略时，构造的是一个 **null（空操作）handler**，`CreateSpecFile` 不写任何
文件，`Allocate` 也不会写 CDI 注解/设备 —— 既有部署行为零变化。

### 2.3 涉及的代码

| 文件 | 作用 |
|------|------|
| `pkg/util/strategy.go` | `DeviceListStrategies` 类型：`Includes`/`AnyCDIEnabled`/`AllCDIEnabled`/`Validate`，以及标量或列表的 JSON/YAML 双向序列化（单值仍序列化成字符串，向后兼容） |
| `pkg/util/consts.go` | CDI vendor/class/默认 hook 路径等常量 |
| `pkg/deviceplugin/cdi/` | `Handler` 接口、`null` 空实现、基于 `nvcdi` 的真实实现（`New`/`CreateSpecFile`/`QualifiedName`/`GetDeviceAnnotations`） |
| `pkg/config/node/node_config.go` | `deviceListStrategy` 改为多策略类型并接入校验 |
| `cmd/device-plugin/options/options.go` | `--device-list-strategy` 改为可多值；新增 CDI 相关 flag |
| `cmd/device-plugin/main.go` | 启动时构建 CDI handler 并生成规范文件 |
| `pkg/deviceplugin/vgpu/vnum_plugin.go` | `UpdateResponseForNodeConfig` 改为多策略；新增 `UpdateResponseForCDI` |
| `pkg/deviceplugin/mig/mig_plugin.go` | MIG 插件同样接入 CDI，避免「选了 CDI 时 MIG 容器拿不到驱动库」的隐患 |

## 3. 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--device-list-strategy` | `envvar` | 设备列表策略，可多值（逗号分隔）。支持：`envvar` / `volume-mounts` / `cdi-annotations` / `cdi-cri` |
| `--cdi-annotation-prefix` | `cdi.k8s.io/` | CDI 容器注解 key 前缀（仅 `cdi-annotations` 使用） |

> NVIDIA CDI hook 的路径**不暴露为参数**：二进制内置在镜像中、由 init 容器安装到宿主机
> `$HOST_MANAGER_DIR/nvidia-cdi-hook`，插件据此自动写入 CDI 规范。
| `--driver-root` | `/` | 生成 CDI 规范时插件视角的驱动根目录 |
| `--dev-root` | （同 driver-root） | 插件视角的设备节点根目录 |
| `--target-driver-root` | `/` | 写入规范的宿主机驱动根目录 |
| `--target-dev-root` | （同 target-driver-root） | 写入规范的宿主机设备节点根目录 |

> 驱动安装在宿主机的常见场景，使用默认值 `/` 即可；GPU-Operator 驱动容器场景需要
> 设置 `--driver-root` / `--target-driver-root`。

## 4. 运行时前提

1. **容器运行时已启用 CDI**：containerd ≥ 1.7 或 CRI-O（开启 CDI），或 NVIDIA 运行时的 cdi 模式；
2. **`/var/run/cdi` 从宿主机挂载进设备插件容器**：插件把生成的规范写到这里，运行时才能读到；
   - `deploy/vgpu-manager-deviceplugin.yaml` 已新增 `cdi-root` 卷与挂载；
   - Helm chart 在 `deviceListStrategy` 含 `cdi` 时自动挂载（host 路径由 `cdiRoot` 控制）。
3. **CDI hook 二进制存在于宿主机**：CDI 规范里的 hook 由**宿主机容器运行时**在拉起业务容器时执行，
   因此该二进制必须在宿主机上对应路径存在（生成规范本身不需要它，插件也不会执行它）。
   - `nvidia-cdi-hook` 已内置在设备插件镜像中（`Dockerfile` 从 `TOOLKIT_CONTAINER_IMAGE`
     拷贝到 `/installed/`），并由 `install` init 容器经 `install_files.sh` 安装到宿主机
     `$HOST_MANAGER_DIR/nvidia-cdi-hook`（默认 `/etc/vgpu-manager/nvidia-cdi-hook`）；
     插件自动按该路径写入 CDI 规范，**无需任何配置**。
   - 构建时需提供 `TOOLKIT_CONTAINER_IMAGE`（`make` 已通过 `versions.mk` 的
     `scripts/toolkit-container-image.sh` 自动推导）。

## 5. 使用示例

### 5.1 通过命令行 / Helm

`values.yaml`：

```yaml
devicePlugin:
  devicePlugin:
    commands:
      # 单独使用 CDI，或与 envvar 组合
      deviceListStrategy: "cdi-annotations"
      # 可选：自定义 CDI 选项
      cdiRoot: "/var/run/cdi"
      # driverRoot: "/run/nvidia/driver"        # GPU-Operator 驱动容器场景
      # targetDriverRoot: "/run/nvidia/driver"
```

### 5.2 通过 NodeConfig（差异化配置）

JSON（单值，向后兼容）：

```json
[
  { "nodeName": "node-a", "deviceListStrategy": "cdi-cri" }
]
```

JSON（多策略组合，列表形式）：

```json
[
  { "nodeName": "node-b", "deviceListStrategy": ["envvar", "cdi-annotations"] }
]
```

YAML（列表形式）：

```yaml
version: v1
configs:
  - nodeName: node-b
    deviceListStrategy:
      - envvar
      - cdi-annotations
```

## 6. 校验与排错

- 非法的策略值会在启动时被 `DeviceListStrategies.Validate()` 拒绝并退出；
- 若开启 CDI 但容器起不来 / 找不到驱动库，先确认：
  1. `/var/run/cdi/*.json` 是否在**宿主机**上生成（检查插件是否挂载了 `/var/run/cdi`）；
  2. 容器运行时是否启用了 CDI；
  3. `--driver-root` / `--target-driver-root` 是否与实际驱动位置一致。
