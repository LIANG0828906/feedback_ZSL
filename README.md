# SVOL Budgeted Volume Renderer

可集成的本地三维标量体渲染组件，包含：

- `SVOL v1` 自定义二进制头与原始体素加载、严格校验、标量/掩码归一化。
- 分段线性颜色/不透明度传递函数、连续 LUT、区间误差上界、事务式编辑。
- 固定采样/内存预算下的 NumPy 光线投射，按声明顺序执行
  `increase_step -> lower_early_term -> reduce_batch`，逐步记录预算用量。
- 相对充足预算基线的像素误差、累积不透明度误差上界和确定性内容哈希。
- 原子版本化快照、过期字段识别、单任务并发约束、协作取消与恢复日志。
- 五类外部异常注入、确定性样本生成器、PySide6 示例应用和脚本化演示。

## 环境

本机实际验证环境：

| 组件 | 版本 |
|---|---|
| Python | 3.11.18 |
| NumPy | 2.4.6 |
| PySide6 | 6.12.0 |
| PyOpenGL | 3.1.10 |
| pytest | 9.2.0 |

本项目已使用 `uv` 安装 Python 3.11，并在仓库内创建 `.venv`：

```bash
uv python install 3.11
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python pip setuptools wheel
.venv/bin/python -m pip install -r requirements.txt
```

备用等价安装方式：

```bash
.venv/bin/python -m pip install numpy PySide6 PyOpenGL pytest
```

## 快速运行

```bash
# 生成 64x64x64 与 512x512x256 两级样本及基线
.venv/bin/python scripts/generate_samples.py --tiers small,large --seed 20261121

# 31 项回归测试
.venv/bin/python -m pytest

# 小样本/大样本预算轨迹
.venv/bin/python scripts/run_demo.py \
  --volume artifacts/samples/small/volume.svol \
  --output artifacts/demo_small --tier small
.venv/bin/python scripts/run_demo.py \
  --volume artifacts/samples/large/volume.svol \
  --output artifacts/demo_large --tier large

# 五类异常注入；每个目录只写 trace.jsonl，不写图像
.venv/bin/python scripts/run_demo.py \
  --volume artifacts/samples/small/volume.svol \
  --output artifacts --all-injections

# 校验既有产物；--rerun-determinism 会重跑两级样本各两次并逐字节比较
.venv/bin/python scripts/verify_artifacts.py --rerun-determinism

# OpenGL/离屏能力探测
.venv/bin/python -m volrender.app --probe-only

# 桌面示例
.venv/bin/python -m volrender.app \
  --volume artifacts/samples/small/volume.svol

# 无窗口脚本入口
.venv/bin/python -m volrender.app --headless \
  --volume artifacts/samples/large/volume.svol \
  --output /tmp/volrender_headless --tier large
```

本机探测到 Qt OpenGL 离屏上下文可用：
`llvmpipe (LLVM 21.1.8)`、`OpenGL 4.5 Compatibility Profile (Mesa 26.0.8)`。
示例的确定性像素路径仍由 NumPy 光栅后端执行，离屏上下文用于宿主能力探测与集成展示。
若桌面/EGL 驱动不可用，示例自动使用同一 NumPy 路径，加载校验、预算、哈希、快照与
异常业务码完全不变。

## 库接口

```python
from volrender import (
    BudgetConfig, CameraView, RayCaster, TransferFunction,
    load_volume, save_snapshot,
)

volume = load_volume("artifacts/samples/small/volume.svol")
transfer = TransferFunction()
renderer = RayCaster(volume, transfer)
view = CameraView(yaw_deg=35.0, pitch_deg=24.0)

result = renderer.render(
    view,
    width=256,
    height=256,
    step_size=1.0,
    budget=BudgetConfig(max_samples=1_000_000, max_memory_bytes=600_000_000),
)

print(result.step_size, result.degradation_stages)
print(result.usage, result.error_summary, result.content_hash)
save_snapshot("snapshot.json", result)
```

核心异常均继承 `BusinessError`，可直接调用 `to_dict()` 写入轨迹：

| 业务码 | 含义 |
|---|---|
| `E_VOLUME_INVALID` | 头字段、类型、间距、窗宽窗位、长度或体素截断错误 |
| `E_TF_INVALID` | 控制点越界、非有限、非单调或 LUT 配置错误 |
| `E_BUDGET_EXCEEDED` | 降级序列耗尽仍超采样/内存预算 |
| `E_SNAPSHOT_MISMATCH` | 快照缺字段、版本、摘要、视角哈希或内容不匹配 |
| `E_BUSY` | 同一体数据同一视角已有渲染任务 |
| `E_CANCELLED` | 光线批/采样/降级阶段检查点收到取消 |

## SVOL v1 格式

头部固定 128 字节，little endian：

| 偏移 | 大小 | 字段 |
|---|---:|---|
| 0 | 4 | magic `SVOL` |
| 4 | 2 | version，当前为 1 |
| 6 | 2 | header size，当前为 128 |
| 8 | 12 | `dim_x, dim_y, dim_z`，各 uint32 |
| 20 | 1 | 标量类型代码 |
| 21 | 1 | 掩码类型代码 |
| 22 | 2 | flags/reserved |
| 24 | 24 | `spacing_x, spacing_y, spacing_z`，float64 |
| 48 | 8 | window width，float64 |
| 56 | 8 | window level，float64 |
| 64 | 8 | mask offset，uint64，文件绝对偏移 |
| 72 | 4 | mask component count |
| 76 | 12 | reserved |
| 88 | 8 | 标量归一化参考最大值 |
| 96 | 32 | reserved |

类型代码：`1=uint8`、`2=uint16`、`3=uint32`、`4=int8`、`5=int16`、
`6=float32`、`7=float64`。

标量平面从 `header_size` 开始，掩码平面从 `mask_offset` 开始。加载器要求：

- 维度乘积、标量/掩码字节宽与完整文件长度一致，不允许静默接受尾部多余字节。
- 间距为有限正数，窗宽为有限正数，窗位有限。
- `mask_offset` 不与标量平面重叠。
- 标量按 `window_low = level - width/2`、`window_high = level + width/2`
  裁剪并归一化到 `[0,1]`，归一化来源记录为
  `header.window_width/window_level`。
- 掩码按实际 `max(mask)` 归一化到 `[0,1]`，来源记录为 `data.max(mask)`。
- 加载失败信息包含具体 `field` 和 `offset`，例如
  `window_width@48`、`scalar_type@20`、`mask_offset@64`、`voxel_data@<实际长度>`。

`load_volume(..., window_override=(width, level))` 可在不修改文件的情况下重建预览窗，
归一化来源会改为 `render.window_override`。

## 传递函数

每个控制点为 `[x, r, g, b, opacity]`：

- `x/r/g/b/opacity` 必须有限。
- `x` 和四通道均在 `[0,1]`；`x` 严格递增。
- 相邻控制点之间线性插值；LUT 默认 256 项，颜色索引连续。
- 每个区间输出 `opacity_error_bound = abs(Δopacity) * 2/255`。
- 编辑先校验并构建候选 LUT，全部成功后才原子替换；任何 `E_TF_INVALID`
  都不会改变上一份 LUT 或摘要。

## 预算与降级

一次渲染开始前，先根据当前视角、输出尺寸、步长、提前终止阈值和光线批大小
估算采样数与工作内存。若超预算，严格按配置序列执行：

1. `increase_step`：步长乘 `increase_step_factor`。
2. `lower_early_term`：提前终止阈值从 `early_term_alpha` 降到
   `degraded_early_term_alpha`。
3. `reduce_batch`：并行光线批从 `ray_batch` 降到 `reduced_ray_batch`。

每次触发都写入：

```json
{
  "stage": "increase_step",
  "reason": "sample_budget",
  "samples_before": 1401716,
  "samples_after": 386652,
  "memory_before_bytes": 987136,
  "memory_after_bytes": 987136
}
```

渲染结果固定包含最终 `step_size`、`early_term_alpha`、`ray_batch`、
`degradation_stages`、`stage_usage`、采样/内存用量和内容哈希。降级序列耗尽时
抛出 `E_BUDGET_EXCEEDED`，其 `usage` 同时带预算、最终估算和已执行阶段。

`memory_used_bytes` 计量单次渲染的帧缓冲、光线批工作数组和 LUT，即热路径工作集；
已常驻体数组单独在 `resident_volume_bytes` 中报告，避免把可共享的只读体数据
重复计入每次渲染。

每次降级渲染都与“同视角、同 TF、同初始步长、充足预算”的基线比较，输出：

- `max_pixel_deviation`
- `mean_pixel_deviation`
- `opacity_deviation_bound`

内容哈希只覆盖像素内容与影响像素的元数据，不包含时间、路径、文件大小或预算数字。

## 快照、并发与恢复

快照字段包括：

`version`、`view_matrix`、`view_hash`、`step_size`、`control_points`、
`precision_mode`、`budget`、`degradation_stages`、`content_hash`、
`snapshot_digest`。

- 保存写入同目录临时文件，`fsync` 后 `os.replace`，失败时清理临时文件。
- 载入校验必需字段、版本、快照摘要、4x4 有限视角矩阵、视角哈希、
  控制点合法性、预算结构、精度模式、内容哈希格式和降级阶段名。
- 字段缺失/损坏统一 `E_SNAPSHOT_MISMATCH`，验证失败前不会更新
  `SnapshotManager.current_result`。
- 视角、LUT、预算、步长、精度或降级阶段变化由 `stale_fields()` 返回
  `view`、`lut`、`budget`、`step`、`precision`、`degradation`，
  调用方据此触发重渲染。

`RenderCoordinator` 以 `(volume_sha256, view_hash)` 为键，非阻塞占用；
同键并发提交返回 `E_BUSY`。光线批、采样步和降级阶段都有取消检查点。
取消或异常后释放内部资源，最后成功提交的快照和结果保持不变。

`ChunkJournal` 逐块 `fsync` 已完成光线批记录；`recover_from_disk()` 只读取
已提交快照和完成分块记录，不承诺强制终止时生成退出报告。

## 异常注入

注入由 `volrender.injection` 在加载/配置入口修改私有字节副本或候选配置完成，
不修改加载器、TF 或渲染器源码。CLI：

```bash
.venv/bin/python scripts/run_demo.py \
  --volume artifacts/samples/small/volume.svol \
  --output artifacts --all-injections
```

实测结果：

| 注入 | 业务码 | 定位 |
|---|---|---|
| `dim_mismatch` | `E_VOLUME_INVALID / DIM_LENGTH_MISMATCH` | `data_length@782464` |
| `window_nonfinite` | `E_VOLUME_INVALID / WINDOW_NON_FINITE` | `window_width@48` |
| `tf_out_of_range` | `E_TF_INVALID / CONTROL_POINT_OUT_OF_RANGE` | `control_points[5]@5` |
| `truncated_voxels` | `E_VOLUME_INVALID / DATA_TRUNCATED` | `voxel_data@524402` |
| `budget_overflow` | `E_BUDGET_EXCEEDED / DEGRADATION_EXHAUSTED` | `config_entry:budget_overflow` |

五类注入目录均只包含 `trace.jsonl`，图像写出发生在渲染成功之后，因此异常路径
不会留下部分图像。

## 实际验收结果

完整机器可读结果见
`artifacts/reports/validation.json`。31 项 pytest 全部通过。

### 64×64×64

- 体数据 SHA-256：`57c80163d69ba061026162350f7a2e2e5208da433c9ec34a224c03bf35df1e65`
- 基线：`256×256`，步长 `1.0`，内容哈希
  `eabee12638ed4a722510c8b8bb0da63bf84c91061dcc5a5575c384a643e7bf8e`
- 降级轨迹：充足预算 → 步长 `4.0` → 提前终止 `0.70` → 批大小 `1024`
- 最终采样 `256258`，工作内存 `528384` B，最大像素偏差 `122`，
  均值偏差 `16.423009236653645`，不透明度误差上界 `0.8393462846931306`
- 最终内容哈希：`2c2082c1ee0feda6dc6e9a12cec06a9ddfaa214ce831a4b466b7f84b4c4baa25`

### 512×512×256

- 体数据 SHA-256：`2affef33565f4e75b3eaeb5c22628c128e0f8a4d08f4043203a6fb765e215f96`
- 基线：`192×192`，步长 `0.7`，内容哈希
  `237dd263e2fbfa0f2225476f4768e7729acff3098e7172b100627e584b3d0cf0`
- 降级轨迹：充足预算 → 步长 `2.8` → 提前终止 `0.70` → 批大小 `1024`
- 最终采样 `1028456`，工作内存 `327680` B，最大像素偏差 `122`，
  均值偏差 `13.677110460069445`，不透明度误差上界 `0.8393462846931306`
- 最终内容哈希：`5571bafe0b5de8877eaaf826a46368e24aeff8d6f627e2b891fb75c4a49c1cc8`

### 确定性

`scripts/verify_artifacts.py --rerun-determinism` 对两级样本分别重跑两次，
并将结果与交付产物逐字节比较。以下文件三次 SHA-256 完全一致：

| 层级 | 文件 | SHA-256 |
|---|---|---|
| small | `trace.jsonl` | `e5f145bf46962c362586e73cbb116d52da0f0b90e6ffedd29688992040ce548f` |
| small | `render.png` | `676201bcfdcdf89409163d2470ac2faa507e6f5a7f448dbb7a9de978e363b6c0` |
| small | `error_heatmap.png` | `6bb25fd8ca65dc4cbc93f105f375bb8ed1d19bb73d863353c9721dd4ed4bd228` |
| small | `snapshot.json` | `6962dbf19a6129797c962c0eb5b7720f4a850c2fbb98828d3ac783db27351db3` |
| large | `trace.jsonl` | `4ae6e161637fb2133eeb05870a12c1400b2f04b17660bc2abc1dd119e40b7fd1` |
| large | `render.png` | `b9afebb56a447900a91b26d6a574a37e0ec1b5ee18a83b52ce40eda4ebd57687` |
| large | `error_heatmap.png` | `0cf5f8e16fbe3e6a725483d69622f1f510abe738739e4c560d49e2c6aafb8e90` |
| large | `snapshot.json` | `e799724efc600ca19d6757b2868c52d2a3249e1022af0d12172dc0b9b3459ed7` |

同一预算与视角下连续渲染的像素缓冲、透明度缓冲和摘要也逐字节一致。
轨迹与摘要没有时间戳、文件大小等非内容字段。

## 产物索引

| 路径 | 内容 |
|---|---|
| `artifacts/samples/small/volume.svol` | 64×64×64 合成样本 |
| `artifacts/samples/large/volume.svol` | 512×512×256 合成样本 |
| `artifacts/samples/*/baseline.png` | 充足预算基线图像 |
| `artifacts/samples/*/baseline_hash.json` | 基线内容哈希、体积哈希与归一化来源 |
| `artifacts/demo_small/trace.jsonl` | 小样本降级、误差、重复性与快照轨迹 |
| `artifacts/demo_large/trace.jsonl` | 大样本降级、误差、重复性与快照轨迹 |
| `artifacts/demo_*/render.png` | 降级最终图像 |
| `artifacts/demo_*/error_heatmap.png` | 相对基线的误差热力图 |
| `artifacts/demo_*/snapshot.json` | 原子快照 |
| `artifacts/demo_inj_*/trace.jsonl` | 五类注入错误轨迹 |
| `artifacts/reports/validation.json` | 自动验收报告 |
| `artifacts/reports/pytest.txt` | 31 项测试的实际运行结果 |

## 代码结构

```text
volrender/
  volume.py       SVOL 头解析、长度/字段校验、归一化
  transfer.py     控制点、LUT、区间误差上界、事务编辑
  budget.py       预算规划与逐级降级记录
  renderer.py     NumPy 光线投射、采样/内存计量、误差摘要、哈希
  snapshot.py     原子快照、摘要与过期字段识别
  coordinator.py  E_BUSY、协作取消、完成分块日志与恢复
  injection.py    外部输入/配置异常注入
  samples.py      确定性两级合成样本和基线元数据
  demo.py         脚本化轨迹与异常演示
  app.py          PySide6 示例、离屏探测与 headless 入口
scripts/
  generate_samples.py
  run_demo.py
  verify_artifacts.py
tests/
  test_volume.py
  test_transfer.py
  test_render_budget.py
  test_snapshot.py
  test_injection_demo.py
```

