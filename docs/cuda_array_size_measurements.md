# CUDA array 分配尺寸实测记录

`library/include/array_size.h` 要在 `cuArrayCreate` / `cuArray3DCreate` /
`cuMipmappedArrayCreate` 之前算出描述符请求多少字节。NVIDIA 对 CUDA 3.x 之后新增的
format **只给一行描述、不给任何尺寸规则**：不说每元素几字节，不说 `Width`/`Height` 按
texel 还是按 4×4 block，也不说哪些 `NumChannels` 合法。

本文记录向驱动实测得到的规则，以及得到它的过程。工具是
`library/tools/array_size_probe.c`（CUDA 侧）和 `library/tools/bc_block_probe.c`
（OpenGL 侧，只为 BC6H 而存在）。

## 测量环境

两台机器、两种 GPU 架构，驱动与工具链相同：

| | 第一轮 | 第二轮（复核） |
|---|---|---|
| GPU | GeForce RTX 2080 Ti（Turing，compute 7.5） | GeForce RTX 3090（Ampere，compute 8.6） |
| 驱动 | 595.84 | 595.84 |
| CUDA toolkit | 13.0 | 13.0 |
| OS | Ubuntu 24.04，kernel 7.0.0-28 | Ubuntu 24.04.3，kernel 7.0.0-28 |
| 预载污染 | 无（`/etc/ld.so.preload` 不存在，`LD_PRELOAD` 为空） | 同 |

**两轮结果逐位相同**：接受性矩阵（368 接受 / 1192 拒绝）、每个 format 接受的
`NumChannels` 集合、全部 49 个 format×通道组合在 1024×512 的 `exact_size`、块取整
的拐点、乃至我们**刻意不建模**的那几项填充参数（行距补 64 字节、高度补 128 行、
64 KiB 粒度），在 Turing 和 Ampere 上一个字节都没差。

因为两轮驱动版本相同，这组数据复核的是**架构相关性**，不是驱动相关性 —— 换驱动
版本仍未复核过。这也正是下面"按来源分类"那张表存在的原因：进代码的只有格式定义级
的常量，架构/驱动相关的量一律不进代码。

## 取数方法

### CUDA 侧

每个组合取两个独立来源，互相印证：

- **`cuArrayGetMemoryRequirements`**（权威）。需要数组带 `CUDA_ARRAY3D_DEFERRED_MAPPING`
  创建，`cuda.h` 明写不带就返回 `INVALID_VALUE`。368 个被接受的组合**全部**取到了值。
- **`cuMemGetInfo` 跨分配差值**（上界，被驱动次级分配器量化）。

**先用 8 个经典 format 校准方法本身**：48/48 个组合的
`exact_size / (bytes_per_channel × NumChannels × W × H)` **精确等于 1.0000**。说明在
2 的幂次维度下无填充，且 `exact_size` 可以直接用来反推规则。这一步同时在真机上确认了
`array_size.h` 已经在用的字节表（1/2/4）是对的。

### OpenGL 侧（只为 BC6H）

`cuArray3DCreate` 对 `BC6H_UF16` / `BC6H_SF16` **在任何形状、任何 `NumChannels` 下
全部拒绝**，CUDA 这条路问不出它的尺寸。但同一块卡、同一个驱动通过
`ARB_texture_compression_bptc` 暴露同样的格式，`GL_TEXTURE_COMPRESSED_IMAGE_SIZE`
直接给出压缩后的确切字节数（块数 × 每块字节，不含分配器粒度）。

`bc_block_probe` 因此**先校准再取数**：把 CUDA 能接受的 6 个 BC format 一起量，
只有它们全部复现 CUDA 实测的每块字节数，才认 BC6H 那两行；否则工具非零退出。
RTX 3090 / 595.84 上的结果（256×256 = 4096 个 4×4 块）：

| format | image_size | 每块字节 | 与 CUDA 实测 |
|---|---|---|---|
| BC1 (S3TC DXT1) | 32768 | 8 | ✅ 一致 |
| BC2 (S3TC DXT3) | 65536 | 16 | ✅ 一致 |
| BC3 (S3TC DXT5) | 65536 | 16 | ✅ 一致 |
| BC4 (RGTC1 RED) | 32768 | 8 | ✅ 一致 |
| BC5 (RGTC2 RG) | 65536 | 16 | ✅ 一致 |
| **BC6H_UF16** | **65536** | **16** | CUDA 拒绝该格式 |
| **BC6H_SF16** | **65536** | **16** | CUDA 拒绝该格式 |
| BC7 (BPTC UNORM) | 65536 | 16 | ✅ 一致 |

6/6 校准项一致，所以 BC6H = 16 字节/块是**实测值**而非推断。

同一个工具在**完全无关的实现**上跑出同一张表 —— 开发容器里的 Mesa 25.2.8 llvmpipe
（纯软件光栅化，跟 NVIDIA 没有关系）：8/16/16/8/16/**16**/**16**/16，逐项相同。
这正好是下面"来源分类"表里 BC 每块字节被归为**格式标准**而不是驱动行为的证据：
换厂商、换实现都不变。

**为什么非要给 BC6H 一个值**：把它留在未分类桶里不是保守，是危险。未分类桶按
`VGPU_ARRAY_UNKNOWN_FORMAT_BYTES` = 8 字节/通道收费，而 BC6H 真实成本是 1 字节/texel
（16 字节 ÷ 16 texel），即**超收 8~32 倍**，正是"凭空发明用量、把放得下的负载 OOM
掉"的方向。

## 接受性矩阵

1560 个探测（65 format × 4 形状 × 3 通道 × 2 宽度）：**368 接受 / 1192 拒绝**，
两种架构完全一致，被拒绝的全部是 `CUDA_ERROR_INVALID_VALUE`。

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

块压缩家族还**一律拒绝 1D 形状**（2D / 3D / 2Dlayered 接受），所以每个 BC format
是 6 行而不是 8 行。

### 32 个 format 被完全拒绝

- **30 个 YUV / planar / semi-planar / packed**：`NV12`、`P010`、`P016`、`NV16`、
  `P210`、`P216`、`YUY2`、`Y210`、`Y216`、`AYUV`、`Y410`、`Y416`、
  `Y444_PLANAR8/10`、`YUV444_8bit/16bit_SemiPlanar`、`UINT8_PACKED_422/444`、
  `UINT8/16_SEMIPLANAR_420/422/444`、`UINT8/16_PLANAR_420/422/444`
- **2 个 BC6H**：`BC6H_UF16`、`BC6H_SF16`（已由 OpenGL 补齐，见上）

每个都是 `NumChannels` ∈ {1,2,4} × 四种形状 × 两种宽度 **共 24 次尝试全部失败**。
那 30 个 YUV format 只能经外部内存导入 / NVDEC interop 取得，而那条路不经过我们
hook 的三个分配入口，也不由我们分配显存。

它们因此留在未分类桶里按 8 字节/通道收费。这件事的代价要说清楚：**如果未来某个驱动
开始接受它们，我们会超收**（`NV12` 真实 1.5 字节/texel）。仍然不猜的理由是
semi-planar 描述符的 `Height` 是否覆盖色度平面无从得知 —— 没有一个会接受它的驱动，
就没有办法测。现在的做法是收上界 + 由 `vgpu_array_format_is_known()` 把该格式打进
日志，等真有驱动接受的那天再测。

## 尺寸规则

### 非块压缩：每通道字节 × NumChannels × W × H × D

零维度按 1 计（`cuda.h` 原文："the CUDA array is one-dimensional if height is 0"；
"A 1D array is allocated if Height and Depth extents are both zero. A 2D array is
allocated if only Depth extent is zero."）。

| format | 每通道字节 | 实测（1024×512，两种架构相同） | 公式 |
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
| `BC6H_UF16` / `BC6H_SF16` | 16 | —（CUDA 拒绝，经 OpenGL 实测每块 16） | — |

**向上取整已验证**（`BC1`，H=4096 固定）：

| W | 块宽 ceil(W/4) | 实测 exact | 说明 |
|---|---|---|---|
| 4096 | 1024 | 8388608 | 行距 8192B，已 64 对齐 |
| 4097 | 1025 | 8454144 | 行距 1025×8=8200 → 填充到 8256 |
| 4098/4099/4100 | 1025 | 8454144 | 同一块列数、同一行距 |
| 4101/4104 | 1026 | 8454144 | 块列数涨到 1026，但行距仍落在同一个 8256 档 |

我们算 `ceil(W/4)×8`，驱动在此之上再补行距，所以恒为下界。

### Flags：Depth 就是切片数，不需要特判

`CUDA_ARRAY3D_LAYERED` 把 Depth 变成层数，`CUDA_ARRAY3D_CUBEMAP` 要求 Depth 为 6
（layered 时为 6 的倍数）。层和立方体面各自都是一整片，所以乘进去就对。实测于
RTX 3090，前三行来自 `array_size_probe --extra`，后三行来自全量 sweep：

| 描述符 | 驱动 exact | 关系 |
|---|---|---|
| `FLOAT` ch=4 2D 64×64 | 65536 | 一个面 |
| `FLOAT` ch=4 **CUBEMAP** 64×64×6 | 393216 | **= 6 × 面** ✓ |
| `FLOAT` ch=4 **CUBEMAP\|LAYERED** 64×64×12 | 786432 | **= 12 × 面** ✓ |
| `BC1` ch=4 2048×1024 2D | 1048576 | 512×256 块 |
| `BC1` ch=4 2048×256×4 3D | 1048576 | **= 同上**，Depth 线性 ✓ |
| `BC1` ch=4 2048×256×4 2Dlayered | 1048576 | **= 同上**，LAYERED 对尺寸无影响 ✓ |

### Mipmap：我们只收 base level，欠收有上界

`cuMipmappedArrayCreate` 的 `numMipmapLevels` 我们不建模。实测（2D 1024×1024，
`cuMipmappedArrayGetMemoryRequirements`）：

| format | L1 | L2 | L4 | L8 | L12 | L20 |
|---|---|---|---|---|---|---|
| `UNSIGNED_INT8` ch=1 | 1.000× | 1.250× | 1.375× | 1.375× | 1.375× | 1.375× |
| `UNSIGNED_INT8` ch=4 | 1.000× | 1.250× | 1.328× | 1.344× | 1.344× | 1.344× |
| `FLOAT` ch=1 | 1.000× | 1.250× | 1.328× | 1.344× | 1.344× | 1.344× |
| `BC1_UNORM` ch=4 | 1.000× | 1.250× | 1.375× | 1.375× | 1.375× | 1.375× |

关键在于**比值会饱和**：请求再多层也不超过 1.375×（超过实际 mip 级数的请求被驱动截
断）。所以"只收 base level"是一个**有界**的欠收，而不是开口的。换成去猜 mip 尾部，
就会把有界欠收换成无界超收 —— 方向反了。

## 驱动额外的填充（我们刻意不建模）

`exact_size` 恒为 **65536 的倍数**，`alignment` 恒为 **65536** —— 驱动按 64 KiB 粒度
向上取整。小数组因此有下限：`UNSIGNED_INT8` ch=1 的 1D 数组（W=2048，逻辑 2048 字节）
实测 `exact` = 65536。

此外实测到四项（Turing / Ampere 数值相同）：

- **行距填充到 64 字节**：`UNSIGNED_INT8` ch=1，W=4097,H=4096 → 实测 17039360 =
  4160×4096，行距从 4097 填到 4160 = ceil(4097/64)×64。ch=2（8194→8256）、
  ch=4（16388→16448）同规律。
- **高度填充到 128 行**：W=4096,H=4097 → 实测 17301504 = 4096×4224，4097 填到
  4224 = ceil(4097/128)×128。三种通道数的行数都填到 4224。
- **3D 的 Depth 补到下一个 2 的幂**：`UNSIGNED_INT8` ch=4，512×512 每片 1 MiB，
  D=1/2/3/4/5/6/7/8/9/10/16/17 实测为 1/2/4/4/8/8/8/8/16/16/16/32 片。
  **2Dlayered 没有这个填充**（D=3 就是 3 片），同样的 extent 只差一个 flag。
- **1D 的每层 stride 被放大（实测 8×）+ 64 KiB 总下限**：`UNSIGNED_INT8` ch=1
  layered，D=4 时 W=256→16384/层、W=1024→16384/层、W=4096→32768/层、
  W=16384→131072/层、W=32768→262144/层。

> **一个被测掉的假设**（留个记录，免得后人重走）：最后一项一度被读成"1D layered
> 层数补到 64"。在 W=2048,D=8 这一个数据点上，"层数补到 64"和"每层 stride 放大 8 倍"
> 给出的是同一个 131072，根本区分不开。加测宽度扫描后可以判定：W=16384,D=4 若按
> 层数补到 64 应为 1048576，实测 524288；W=16384,D=65 实测 8519680 = 65×131072。
> **层数没有被补齐**，被放大的是每层 stride。

这些全是架构/驱动相关的 tiling 与分配器参数，**没有在其它驱动版本上复核过**，所以
`array_size.h` 只算逻辑尺寸、不建模填充。后果是我们的数字是**下界**：4097×4097 的
`UNSIGNED_INT8` 我们算 16785409、驱动实际 17301504（低 3%）。这个方向是对的 ——
预算检查绝不能凭空发明用量，而差额由 NVML 实时读回的真实用量在下一次分配时纠正
（我们不维护账本，见 `array_size.h` 文件注释）。

## 这些数值换 OS / 换卡还成立吗

单一环境的数据不能直接当普适规则用，所以按**来源**分类，而不是按"测到了就当真"：

| 进了代码的常量 | 来源 | 跨 OS / GPU |
|---|---|---|
| 每通道字节 1 / 2 / 4 | **CUDA API 的格式定义**（"Unsigned 8-bit integers"） | ✅ 定义，不可能变 |
| BC 每块 8 / 16 字节 | **BC 压缩格式标准**（D3D / Vulkan / OpenGL 通用）；在 Turing + Ampere 的 CUDA 路径、以及 NVIDIA GL 与 Mesa llvmpipe 两个无关实现上都复现 | ✅ 压缩数据尺寸由格式定死，否则纹理文件无法跨 API 互换 |
| 块几何 4×4 | 同上 | ✅ |
| Depth 线性相乘（含 cubemap / layered） | `cuda.h` 对 Flags 的定义 + 实测 | ✅ |

| 刻意**没有**进代码的 | 来源 | 跨 OS / GPU |
|---|---|---|
| 64 KiB 分配粒度 | 驱动分配器 | ❌ 架构相关 |
| 行距补到 64 字节 | 驱动 tiling | ❌ 架构相关 |
| 高度补到 128 行 | 驱动 tiling | ❌ 架构相关 |
| 3D Depth 补到 2 的幂 | 驱动 tiling | ❌ 架构相关 |
| 1D 每层 stride 放大 8× | 驱动 tiling | ❌ 架构相关 |
| mip 链 ≤1.375× 的尾部 | 驱动逐级取整 | ❌ 架构相关 |

后六项全部落在"我们算下界、不建模填充"这一桶里。它们只会让我们**偏小**，而偏小由
NVML 实时读回的真实用量在下一次分配时纠正；要是把它们写死进代码，换一代架构就会变成
**偏大**，也就是凭空发明用量、把本来放得下的负载 OOM 掉。

### 一处因此做了加固的地方

实测到的"`NumChannels` 必须等于 format 名里的通道数"（`UNORM_INT8X4` 只收 4）是一条
**驱动接受性规则**，不是格式定义 —— 别的驱动 / GPU 代 / OS 完全可能放宽。原先的公式是
`每通道字节 × 描述符里的 NumChannels`，一旦某个驱动接受 `UNORM_INT8X4` 配 `ch=1`，
我们就会按 1 字节/texel 收费而真实是 4 —— **少收 4 倍，配额静默被绕过**。

`vgpu_array_format_channels()` 因此取"描述符的 NumChannels"与"format 名里固定的通道数"
**两者较大值**。在驱动强制二者一致的平台上结果与实测完全相同；在放宽的平台上仍然按完整
元素收费。格式名本身就是依据：*"4 channel unsigned 8-bit normalized integer"* 说的就是
一个 4 字节元素。块压缩路径本来就不看 `NumChannels`，天然免疫。

`test_implied_channels_cannot_be_underpaid` 把这条钉住：实测值不变，且任何
`NumChannels` 都不能把它压低。

## 端到端验证结果

`library/tools/array_size_verify.c` 把探测 CSV 里**每一条**被驱动接受的描述符喂进
出厂的 `include/array_size.h`，和驱动的 `exact_size` 逐条比对。两个必须为零的指标：

- **OVER** —— 我们收得比驱动实际保留的多，等于凭空发明用量、把本来放得下的负载 OOM 掉；
- **UNSIZABLE** —— 驱动接受但我们算不出来，等于那次分配跳过预算检查。

RTX 3090 / 595.84（最终一轮，三份数据集）：

| 数据集 | 被接受 | 精确相等 | 偏小（安全） | **OVER** | **UNSIZABLE** |
|---|---|---|---|---|---|
| 对齐几何（全量 sweep） | 368 | 294 | 74 | **0** | **0** |
| 非对齐几何（8 种：4097/4100/1023/777 等） | 392 | 162 | 230 | **0** | **0** |
| `--extra`（cubemap / 3D depth / 1D layered） | 8 | 5 | 3 | **0** | **0** |
| **合计** | **768** | **461** | **307** | **0** | **0** |

RTX 2080 Ti 的对齐几何一轮同为 368 / 294 / 74 / 0 / 0。

偏小的成因都是上面刻意不建模的驱动行为：

- **对齐几何的 74 条全部是 1D**（2D/3D/layered 无一例外精确相等）。最差比值 32×：
  `UNSIGNED_INT8` ch=1 的 1D 数组逻辑 2048 字节，驱动按 64 KiB 粒度给到 65536。
- **非对齐几何**是 2D tiling 填充，比值 1.002–1.52（最差是 777×333×3）。
- **`--extra` 的 3 条**：3D Depth 补 2 的幂（1.33×）与 1D 每层 stride 放大（8×）。

## 复现

```bash
# CUDA 侧探测（需要 GPU + 驱动；工具只有一个 .c 文件，不需要整个仓库）
gcc -O2 -o array_size_probe library/tools/array_size_probe.c \
    -I/usr/local/cuda/include -l:libcuda.so.1 -ldl

./array_size_probe 0 --csv                    # 全量 sweep，1560 行
./array_size_probe 0 --csv --geom 1024,512,0  # 单一几何，用于钉某一条规则
./array_size_probe 0 --csv --geom 4097,4096,0 # 非 4 倍数宽度，钉块取整与行填充
./array_size_probe 0 --extra                  # Flags / Depth / mip 规则
./array_size_probe 0 --csv --extra > extra.csv 2>mip.txt  # CSV 可喂给 verify，
                                                          # mip 表走 stderr

# BC6H 的每块字节（OpenGL；自带校准，校准不过就非零退出）
gcc -O2 -o bc_block_probe library/tools/bc_block_probe.c -ldl
./bc_block_probe

# 回放比对（无需 GPU，无需 CUDA toolkit）
gcc -O2 -I library -I library/include -o array_size_verify \
    library/tools/array_size_verify.c
cat sweep.csv geom_*.csv extra.csv | ./array_size_verify
```

**必须在裸 GPU 上跑，不要在 vGPU 容器里** —— 带着 `libvgpu-control.so` 时
`cuMemGetInfo` 报的是容器配额、测的是我们自己的记账。工具会检查 `/proc/self/maps`
并在发现时告警。
