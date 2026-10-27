# CUDA array 分配尺寸实测记录

`library/include/array_size.h` 要在 `cuArrayCreate` / `cuArray3DCreate` /
`cuMipmappedArrayCreate` 之前算出描述符请求多少字节。NVIDIA 对 CUDA 3.x 之后新增的
format **只给一行描述、不给任何尺寸规则**：不说每元素几字节，不说 `Width`/`Height` 按
texel 还是按 4×4 block，也不说哪些 `NumChannels` 合法。

本文记录向驱动实测得到的规则，以及得到它的过程。工具是
`library/tools/array_size_probe.c`。

## 测量环境

| | |
|---|---|
| GPU | NVIDIA GeForce RTX 2080 Ti（Turing，compute 7.5） |
| 驱动 | 595.84 |
| CUDA toolkit | 13.0 |
| OS | Ubuntu 24.04，kernel 7.0.0-28 |
| 预载污染 | 无（`/etc/ld.so.preload` 不存在，`LD_PRELOAD` 为空） |

**单一硬件/驱动的数据点。** 行填充与高度填充是架构相关的 tiling 参数（见下），
逻辑尺寸规则本身预期是架构无关的，但没有在第二种架构上复核过。

## 取数方法

每个组合取两个独立来源，互相印证：

- **`cuArrayGetMemoryRequirements`**（权威）。需要数组带 `CUDA_ARRAY3D_DEFERRED_MAPPING`
  创建，`cuda.h` 明写不带就返回 `INVALID_VALUE`。本次 368 个被接受的组合**全部**取到了值。
- **`cuMemGetInfo` 跨分配差值**（上界，被驱动次级分配器量化）。

**先用 8 个经典 format 校准方法本身**：48/48 个组合的
`exact_size / (bytes_per_channel × NumChannels × W × H)` **精确等于 1.0000**。说明在
2 的幂次维度下无填充，且 `exact_size` 可以直接用来反推规则。这一步同时在真机上确认了
`array_size.h` 已经在用的字节表（1/2/4）是对的。

## 接受性矩阵

1560 个探测（65 format × 4 形状 × 3 通道 × 2 宽度）：**368 接受 / 1192 拒绝**。

### `NumChannels` 必须精确匹配 format 自带的通道数

这是原先卡住 P1 的问题 —— `UNORM_INT8X4` 的描述是 "4 channel unsigned 8-bit"，
那 `NumChannels` 该填 1 还是 4？两种读法差 4 倍。实测答案：

| format 家族 | 接受的 `NumChannels` |
|---|---|
| 8 个经典 format | 1、2、4 都接受 |
| `*INT8X1` / `*INT16X1` | **仅 1** |
| `*INT8X2` / `*INT16X2` | **仅 2** |
| `*INT8X4` / `*INT16X4` | **仅 4** |
| `UNORM_INT_101010_2` | **仅 4** |
| `BC1/BC2/BC3/BC7`（含 SRGB） | **仅 4** |
| `BC4_UNORM/SNORM` | **仅 1** |
| `BC5_UNORM/SNORM` | **仅 2** |

### 32 个 YUV / planar / packed format 被全部拒绝

`NV12`、`P010`、`P016`、`NV16`、`P210`、`P216`、`YUY2`、`Y210`、`Y216`、`AYUV`、
`Y410`、`Y416`、`Y444_PLANAR8/10`、`YUV444_8bit/16bit_SemiPlanar`、
`UINT8/16_PACKED_*`、`UINT8/16_SEMIPLANAR_*`、`UINT8/16_PLANAR_*`

在 `NumChannels` ∈ {1,2,4} × 四种形状 × 两种宽度 **共 24 次尝试全部返回
`CUDA_ERROR_INVALID_VALUE`**。它们只能经外部内存导入 / NVDEC interop 取得，而那条路
不经过我们 hook 的三个分配入口，也不由我们分配显存。

`BC6H_UF16` / `BC6H_SF16` 同样被全部拒绝，因此**没有 BC6H 的实测数据**。

## 尺寸规则

### 非块压缩：每通道字节 × NumChannels × W × H × D

零维度按 1 计（`cuda.h` 原文："the CUDA array is one-dimensional if height is 0"；
"A 1D array is allocated if Height and Depth extents are both zero. A 2D array is
allocated if only Depth extent is zero."）。

| format | 每通道字节 | 实测（1024×512） | 公式 |
|---|---|---|---|
| `UNORM_INT8X1` ch=1 | 1 | 524288 | 1×1×1024×512 ✓ |
| `UNORM_INT8X2` ch=2 | 1 | 1048576 | 1×2×1024×512 ✓ |
| `UNORM_INT8X4` ch=4 | 1 | 2097152 | 1×4×1024×512 ✓ |
| `UNORM_INT16X1` ch=1 | 2 | 1048576 | 2×1×1024×512 ✓ |
| `UNORM_INT16X2` ch=2 | 2 | 2097152 | 2×2×1024×512 ✓ |
| `UNORM_INT16X4` ch=4 | 2 | 4194304 | 2×4×1024×512 ✓ |
| `SNORM_INT8X{1,2,4}` | 1 | 同上 | ✓ |
| `SNORM_INT16X{1,2,4}` | 2 | 同上 | ✓ |
| `UNORM_INT_101010_2` ch=4 | 1 | 2097152 | 1×4×1024×512 ✓（打包 32 位 = 4 字节/texel） |

也就是说这 13 个 format **不需要新分支**，把每通道字节填进现有表即可。

### 块压缩：ceil(W/4) × ceil(H/4) × 每块字节 × D

**`NumChannels` 不参与**（每块字节已经把通道算进去了）。

| format | 每块字节 | 实测（1024×512） | 公式 |
|---|---|---|---|
| `BC1_UNORM`(+SRGB) | 8 | 262144 | 256×128×8 ✓ |
| `BC4_UNORM` / `BC4_SNORM` | 8 | 262144 | 256×128×8 ✓ |
| `BC2_UNORM`(+SRGB) | 16 | 524288 | 256×128×16 ✓ |
| `BC3_UNORM`(+SRGB) | 16 | 524288 | 256×128×16 ✓ |
| `BC5_UNORM` / `BC5_SNORM` | 16 | 524288 | 256×128×16 ✓ |
| `BC7_UNORM`(+SRGB) | 16 | 524288 | 256×128×16 ✓ |

**向上取整已验证**（`BC1`，H=4096 固定）：

| W | 块宽 ceil(W/4) | 实测 exact | 说明 |
|---|---|---|---|
| 4096 | 1024 | 8388608 | 行距 8192B，已 64 对齐 |
| 4097 | 1025 | 8454144 | 行距 1025×8=8200 → 填充到 8256 |
| 4098/4099/4100 | 1025 | 8454144 | 同一块列数、同一行距 |

**Depth 线性相乘**（`BC1`，2048 宽）：2D(H=1024,D=0)、3D(H=256,D=4)、
2Dlayered(H=256,D=4) 三者 `exact` 全为 1048576 = 512×64×8×4。`CUDA_ARRAY3D_LAYERED`
对尺寸无影响。

## 驱动额外的填充（我们刻意不建模）

`exact_size` 恒为 **65536 的倍数**，`alignment` 恒为 **65536** —— 驱动按 64 KiB 粒度
向上取整。小数组因此有下限：`UNSIGNED_INT8` ch=1 的 1D 数组（W=2048，逻辑 2048 字节）
实测 `exact` = 65536。

二维还有 tiling 填充，Turing 上测得：

- **行距填充到 64 字节**：`UNSIGNED_INT8` ch=1，W=4097 → 实测 17039360 = 4160×4096，
  行距从 4097 填到 4160 = ceil(4097/64)×64。ch=2（8194→8256）、ch=4（16388→16448）同规律。
- **高度填充到 128 行**：W=4096,H=4097 → 实测 17301504 = 4096×4224，4097 填到
  4224 = ceil(4097/128)×128。

这两个是架构相关的 tiling 参数，**没有在其它架构上复核**，所以
`array_size.h` 只算逻辑尺寸、不建模填充。后果是我们的数字是**下界**：4097×4097 的
`UNSIGNED_INT8` 我们算 16785409、驱动实际 17301504（低 3%）。这个方向是对的 ——
预算检查绝不能凭空发明用量，而差额由 NVML 实时读回的真实用量在下一次分配时纠正
（我们不维护账本，见 `array_size.h` 文件注释）。

## 端到端验证结果

`library/tools/array_size_verify.c` 把探测 CSV 里**每一条**被驱动接受的描述符喂进
出厂的 `include/array_size.h`，和驱动的 `exact_size` 逐条比对。两个必须为零的指标：

- **OVER** —— 我们收得比驱动实际保留的多，等于凭空发明用量、把本来放得下的负载 OOM 掉；
- **UNSIZABLE** —— 驱动接受但我们算不出来，等于那次分配跳过预算检查。

| 数据集 | 被接受 | 精确相等 | 偏小（安全） | **OVER** | **UNSIZABLE** |
|---|---|---|---|---|---|
| 对齐几何（全量 sweep） | 368 | 294 | 74 | **0** | **0** |
| 非对齐几何（4097/4100/1023/777 等） | 294 | 61 | 233 | **0** | **0** |

偏小的两种成因，都是刻意不建模的驱动行为：

- **对齐几何的 74 条全部是 1D**（2D/3D/layered 无一例外精确相等）。最差比值 32×：
  `UNSIGNED_INT8` ch=1 的 1D 数组逻辑 2048 字节，驱动按 64 KiB 粒度给到 65536。
- **非对齐几何**是 2D tiling 填充，比值 1.002–1.52（最差是 777×333×3）。

## 复现

```bash
gcc -O2 -o array_size_probe library/tools/array_size_probe.c \
    -I/usr/local/cuda/include -l:libcuda.so.1 -ldl

./array_size_probe 0 --csv                    # 全量 sweep，1560 行
./array_size_probe 0 --csv --geom 1024,512,0  # 单一几何，用于钉某一条规则
./array_size_probe 0 --csv --geom 4097,4096,0 # 非 4 倍数宽度，钉块取整与行填充

# 回放比对（无需 GPU）
gcc -O2 -I library -I library/include -o array_size_verify \
    library/tools/array_size_verify.c
./array_size_verify < probe.csv
```

**必须在裸 GPU 上跑，不要在 vGPU 容器里** —— 带着 `libvgpu-control.so` 时
`cuMemGetInfo` 报的是容器配额、测的是我们自己的记账。工具会检查 `/proc/self/maps`
并在发现时告警。
