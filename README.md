# ShadowPipe 本地离线阴影管线

本项目在同一帧管线中实现场景二进制解析、级联阴影划分、光空间遮挡体深度金字塔、
保守遮挡查询、双重剔除、接收面/投射面级联选择、深度/法线偏移校正、软件离屏光栅、
误差摘要和确定性快照。核心库不访问网络，不依赖运行时外部服务。

## 构建

依赖：

- CMake 3.20+
- 支持 C++17 的 GCC/Clang/MSVC
- 已随仓库放在 `third_party/glm` 的 GLM 1.0.1 头文件

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j4
cmake --build build --target check
```

当前环境没有可用的窗口/GPU 开发头文件，因此交付的离屏后端是确定性 CPU 软件光栅器。
它提供与 OpenGL 3.3 相同输入的数学管线，但不受驱动浮点差异影响，适合逐字节验收；
不访问 OpenGL 或任何未授权目标。若需要 GPU 展示层，可直接复用核心库输出的级联表、
阴影贴图和快照，而 CPU 结果作为校准基线。

## 目录

- `include/shadowpipe/`：公共 C++17 API。
- `src/`：解析、级联、金字塔、光栅、渲染、快照实现。
- `apps/scene_gen.cpp`：确定性合成场景生成器。
- `apps/shadowpipe_cli.cpp`：脚本/参数驱动渲染入口。
- `tests/tests.cpp`：场景、级联、遮挡、偏移、并发、取消和确定性测试。
- `data/`：合成二进制场景、相机脚本和基线产物。
- `scripts/verify.sh`：完整构建、双渲染比对、快照版本与损坏检测。

## 二进制场景格式

所有整数和浮点字段均为小端；矩阵按 `mat4[column][row]` 顺序存储 16 个 `f32`。

| 区域 | 字段 |
| --- | --- |
| Header | `char magic[4]="RSCN"`, `u32 version=1`, `u32 materialCount`, `u32 meshCount`, `u32 nodeCount` |
| Material[] | `f32 albedo[3]`, `f32 roughness` |
| Mesh[] | `u32 materialSlot`, `u32 vertexCount`, `u32 indexCount`, `f32 localMin[3]`, `f32 localMax[3]`, `f32 vertices[vertexCount][3]`, `u32 indices[indexCount]` |
| Node[] | `u32 nameLength`, UTF-8 name bytes, `i32 parent`, `u32 meshIndex`, `f32 world[16]`, `f32 declaredWorldMin[3]`, `f32 declaredWorldMax[3]` |

`meshIndex = 0xffffffff` 表示组节点。解析器完整读取到临时 `Scene` 后一次校验：

- 材质槽与网格索引范围；
- 三角形索引范围和 `indexCount % 3 == 0`；
- 顶点局部包围盒包含关系；
- 世界矩阵有限且 `abs(det(M)) > 1e-12`；
- 父链、环和根节点；
- 节点声明世界包围盒包含自身网格与所有子节点内容。

任一项失败返回 `E_SCENE_INVALID`，同时写入节点索引和名称；函数返回空场景，不产生部分结果。

## 管线数值规则

### 级联

- 使用 uniform/log 混合分割，`lambda` 控制对数分布权重。
- 每级分辨率从基础分辨率逐级减半，最低 64；级联表记录近远距离、分辨率、
  光空间纹理世界尺寸、深度范围和量化误差。
- 光空间拟合中心按纹素对齐，边界统计输出相邻级纹理世界尺寸差和相对差。
- 接收面按视深度加法线斜率修正选择唯一级联，偏移只在该级施加一次，避免父子级联重复。

当前基线级联表：

| Cascade | Near | Far | Resolution | Texel world size |
| --- | ---: | ---: | ---: | ---: |
| 0 | 0.500000 | 4.565525 | 512 | 0.018418098 |
| 1 | 4.565525 | 10.145391 | 256 | 0.075621895 |
| 2 | 10.145391 | 20.389540 | 128 | 0.300458848 |
| 3 | 20.389540 | 45.000000 | 64 | 1.340682983 |

### 深度金字塔

- 每帧按当前光源方向重新光栅化遮挡体，不沿用旧层级。
- Level 0 为 24-bit 量化；每层使用更小的位宽，并记录 `depthMin/depthMax`、
  量化步长和最大量化误差。
- 每层还保存最近遮挡体节点 ID；查询某节点时排除自身纹素，避免把自遮挡误当外部遮挡。
- 金字塔重建报告逐层给出旧/新深度范围、差值、量化步长和是否变化。

### 遮挡判定

查询每个包围盒在各层的投影矩形，设量化误差为 `q`：

```text
marginLower = boxNear - maxOccluderDepth - q
marginUpper = boxFar  - minOccluderDepth + q
```

- 所有采样纹素均有位于包围盒前方的遮挡体：`OCCLUDED`。
- 所有遮挡体都在包围盒后方：`VISIBLE`。
- 余量区间跨过量化误差或遮挡覆盖不完整：`AMBIGUOUS`，对象状态为
  `E_OCCLUSION_AMBIGUOUS`，本轮保守保留并计入误差摘要。

### 偏移校正

对每个接收点使用唯一所选级联：

```text
cosTheta          = clamp(dot(N, -lightDirection), 0.2, 1)
tanTheta          = sqrt(1-cosTheta^2) / cosTheta
depthOffsetWorld  = (depthBias + slopeScale * tanTheta) * texelWorld
normalOffsetWorld = normalBias * sqrt(1-cosTheta^2) * texelWorld
```

深度偏移转换到该级光空间归一化深度后参与阴影比较。摘要输出每级代表偏移、
观测到的偏移范围、预估自阴影概率和阴影脱离距离。参数为负、非有限或总偏移超过
`maxBiasTexels` 时返回 `E_BIAS_INVALID`，并在光栅化前停止。

## 运行

```bash
# 生成 data/synthetic.rscene 及三个无效变体
./build/scene_gen --out-dir data

# 使用脚本渲染并输出基线产物
./build/shadowpipe \
  --scene data/synthetic.rscene \
  --config data/sample.script \
  --output output

# 载入旧快照；视点/级联变化会在摘要中标记并重新渲染
./build/shadowpipe \
  --scene data/synthetic.rscene \
  --config data/view_b.script \
  --output output_view_b \
  --load-snapshot output/snapshot.bin

# 直接覆盖相机、光源、偏移和容差
./build/shadowpipe --scene data/synthetic.rscene \
  --eye 0,6,16 --target 0,2,0 --light -0.42,-1,-0.31 \
  --depth-bias 1.5 --normal-bias 1.25 --switch-tolerance 0.05 \
  --output output_override
```

`--clear-output` 为默认行为，会清除已知旧渲染产物但保留当前快照；
`--no-clear` 可关闭该行为。`--help` 列出所有参数。

## 输出产物

- `shadow_factor.pgm`：8-bit 阴影因子图。
- `shadow_factor.f32`：逐像素 `f32` 阴影因子，用于精确比较。
- `visibility_mask.pgm`：`0` 无命中/被剔除，`128` 遮挡临界但保守保留，`255` 可见。
- `error_heatmap.ppm`：无偏参考与所选偏移、相邻级联偏差的热力图。
- `cascade_N_shadow_depth.f32/.pgm`：各级阴影深度图。
- `summary.json`：级联表、级联边界统计、金字塔层、所有遮挡查询与余量、
  偏移误差指标、切换事件、上界和产物哈希。
- `snapshot.bin`：视点矩阵、级联表、金字塔层级、偏移参数、场景/产物哈希。

摘要关键字段包括 `cascade_switch_count`、`occlusion_ambiguous_count`、
`bias_error_metrics`、`shadow_factor_delta_upper_bound`、
`old_new_level_range_difference_max` 和 `max_quantization_error`。

## 快照与并发

- 快照头部包含 payload 长度和 FNV-1a 64 内容哈希；文件被篡改返回
  `E_SNAPSHOT_MISMATCH`（退出码 13），不开始渲染。
- 载入有效快照后比较场景哈希、相机、光源、级联表、偏移参数；变化写入
  `snapshot_version.changed/view_changed/cascade_table_changed` 并执行重渲染。
- 场景哈希相同的并发任务只允许一个，重复提交即返回 `E_BUSY`（退出码 14）。
- 金字塔层、级联前后和提交前均有协作取消检查点；取消不提交新快照。
- 快照和目标文件使用临时文件后重命名；异常路径保留最后一次成功提交的快照。

## 错误码

| 业务码 | CLI 退出码 | 含义 |
| --- | ---: | --- |
| `E_OK` | 0 | 成功 |
| `E_SCENE_INVALID` | 10 | 场景解析或层级包围盒无效，摘要含节点索引 |
| `E_BIAS_INVALID` | 12 | 偏移参数为负/越界，光栅化前中止 |
| `E_SNAPSHOT_MISMATCH` | 13 | 快照头或内容哈希不符 |
| `E_BUSY` | 14 | 同场景已有任务 |
| `E_CANCELLED` | 15 | 协作取消 |
| `E_IO_ERROR` | 7 | I/O 失败 |
| `E_INVALID_ARGUMENT` | 2 | 脚本或参数非法 |

`E_OCCLUSION_AMBIGUOUS` 是对象级标记，不中止帧；对应物体保守保留。

## 实际验证结果

执行 `./scripts/verify.sh` 的实际结果：

- 场景哈希：`5ab48cbf36cdcc4f`
- 基线帧哈希：`806c8bc79dcd74c8`
- 视锥剔除 1，完全遮挡 1，临界保留 5，可见 2
- 级联切换计数 262，切换阴影因子偏差上界 1.0
- 金字塔最大量化误差 `8.1294e-05`
- 两次独立渲染的 `shadow_factor.pgm/.f32`、`visibility_mask.pgm`、
  `error_heatmap.ppm`、`summary.json`、`snapshot.bin` 全部逐字节一致
- 加载相同参数快照后再渲染，误差摘要仍逐字节一致
- 切换到 `data/view_b.script` 后识别 `view_changed=true`、
  `cascade_table_changed=true` 并重渲染
- 翻转快照正文一个字节后返回 `E_SNAPSHOT_MISMATCH`，退出码 13
- 单元测试覆盖无效索引/矩阵/包围盒、`E_BUSY`、取消保留快照、双渲染一致性

基线文件位于 `data/baseline/`，其 `SHA256SUMS` 可用于外部复核。

