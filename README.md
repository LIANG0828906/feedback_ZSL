# 皮影屏风灯箱离线软光栅验证器

这是一个只依赖 Python、NumPy、Pillow 的确定性 CPU 验证组件。它从 JSON
读取五层剪纸屏风、材质、烛火、空气层和相机参数，输出可逐字节核对的
RGBA 图像、光学深度、统计报告、块级快照与恢复/差异报告。

当前仓库已包含可运行实现、23 项自动验收测试，以及一次完整实测产物：
`artifacts/demo/`。

## 快速开始

要求 Python 3.11 或以上：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt

.\.venv\Scripts\python -m shadowbox generate --out artifacts\demo\scene.json
.\.venv\Scripts\python -m shadowbox render --scene artifacts\demo\scene.json --out artifacts\check
.\.venv\Scripts\python -m shadowbox verify --scene artifacts\demo\scene.json --output artifacts\check
.\.venv\Scripts\python -m unittest discover -s tests -v
```

Linux/macOS 将解释器路径换成 `python3` 即可。完整演示和验收流程：

```bash
python scripts/run_demo.py
```

## 实际验证结果

完整样本为 `320x256`、`64x64` 分块、每像素 2 个确定性 Halton 子采样。
`python scripts/run_demo.py` 实测结果：

- 场景摘要：`07e40793529ee21178345a7d0635fd38dbc0f2729e15fff1db574c81bc60fcda`
- 不中断 RGBA 摘要：`79ff55bbb5407fd14d9a1e7ef605edfb9d6fbbbe9a6c098f68590435f6097093`
- 不中断光学深度摘要：`e3c6443dfd7fa2356e5986663884aded46e1cad85b858c4bcc62f9f0907034d6`
- 第 7 块后可控中断、重启续算：`recovery_count=1`，逐像素差异 `0`
- 独立 `verify` 复核：RGBA、光学深度、每块颜色摘要、每层统计和半影探针全部一致
- 半影宽度随相邻空气层增厚实测为 `5.25 -> 11.75 -> 21.0 -> 41.0 mm`
- 真实 SIGINT 实测：完成当前块、退出码 `0`、写出恢复报告；续算后哈希与不中断基线相同
- 可见像素中没有三通道均高于 `0.9` 的发白像素，最亮 1% 像素平均饱和度约 `0.69`

自动测试结果：`Ran 23 tests ... OK`。测试覆盖六类业务错误、无图像拒绝、
纹理确定性/顺序无关、同色层区分、投影随空气层移动、半影趋势、快照损坏丢弃、
场景摘要变化丢弃、恢复逐字节一致、差异报告字段及报告复算。

## 场景格式

顶层格式见 `artifacts/demo/scene.json`。关键约定：

- `format` 必须为 `shadowbox-scene-v1`。
- 层号必须完整且唯一，固定为 `0..4`；`0` 最靠近烛火。
- 每层网格是毫米单位的平面三角网格，顶点局部 `z=0`。
- 世界高度由 `stack.base_z_mm + sum(此前各层 air_gap_to_prev_mm)` 推导。
  因而修改空气层厚度会真实改变投影目标平面，而不是只改报告数字。
- 每层声明 `transmittance`、`scatter_color`、`texture_seed`、
  `shadow_samples_per_axis` 和 `projection_gain`。
- `texture.width/height` 是纤维与镂空表尺寸；
  `projection_width/height` 是外层投影图分辨率。
- 相机和点光源都使用世界毫米坐标。点光源必须位于最内层内侧，相机必须位于
  最外层外侧，且二者不得落在任何纸面三角形上。

```json
{
  "format": "shadowbox-scene-v1",
  "stack": {"base_z_mm": 24.0, "layer_count": 5},
  "camera": {
    "position_mm": [15.0, -11.0, 190.0],
    "target_mm": [0.0, 0.0, 38.0],
    "up": [0.0, 1.0, 0.0],
    "fov_y_deg": 47.0,
    "resolution": [320, 256],
    "block_size": [64, 64],
    "samples_per_pixel": 2
  },
  "light": {
    "position_mm": [-20.0, 14.0, 10.0],
    "intensity": 1.15,
    "color": [1.0, 0.79, 0.53],
    "reference_distance_mm": 100.0
  },
  "layers": [
    {
      "layer_index": 0,
      "name": "red",
      "air_gap_to_prev_mm": 0.0,
      "transmittance": 0.38,
      "scatter_color": [0.82, 0.075, 0.045],
      "texture_seed": 41017,
      "shadow_samples_per_axis": 1,
      "projection_gain": 1.0,
      "mesh": {"vertices": [], "uvs": [], "triangles": []}
    }
  ]
}
```

## 渲染模型

每层纹理使用 `SHA-256(texture_seed:layer_index:channel)` 派生独立种子，
因此结果不依赖层数组的遍历或输入顺序。纤维表同时调制
`T_eff = T * (0.82 + 0.34 * fiber)` 和
`C_eff = scatter_color * (0.86 + 0.26 * fiber)`。

纸面片元的透射项由点光源距离、入射余弦和有效透光率决定；散射颜色叠加漫反射。
高光使用按最大通道缩放的 Reinhard 映射，再转 sRGB，因此不会把红色内层逐通道
削波成白色。内部结果是预乘 Alpha，按层号 `0 -> 4` 使用 over 合成；输入列表倒序
不会改变结果。

相邻层投影通过点光源把外层采样点反投到内层。内层镂空掩码在
`shadow_samples_per_axis^2` 个核采样上取平均：

```text
kernel_radius = penumbra_base_mm + penumbra_per_gap_mm * air_gap_mm
```

反投或核采样落到纸面外时按“无遮挡”返回 1，不产生由纸面边界造成的黑边。
亮度可见性作为透射项的一部分参与外层着色。

## 输出

一次成功渲染在输出目录写出：

- `rgba.raw`：`H x W x 4` 的 `uint8` 预乘 sRGB 原始字节。
- `rgba.png`：同一预乘 RGBA 的 PNG 表示。
- `rgba_straight.png`：仅用于查看的直通 Alpha 预览。
- `optical_depth.f32`：`H x W` 小端 `float32`，每样本累加
  `-log(T_eff) * paper_coverage` 后按子采样平均。
- `report.json`：稳定字段顺序、无时间戳和绝对路径的统计报告。
- `checkpoint.json`：最后一个完整提交的块快照。
- `resume_report.json`：仅在可控中断时存在。
- `diff.json`：只在像素/深度比较失败时存在。

`report.json` 包含每层可见片元样本数、纸面/镂空片元数、平均有效透光率、
镂空连通域数、纤维/掩码摘要、软阴影采样点数、投影空气层、目标半影宽度、
每个块的坐标/颜色摘要/深度摘要、图像与深度摘要、恢复次数、以及 9 点半影
亮度比较。块统计可直接由 `rgba.raw` 的对应裁剪复算；层统计由场景、纹理和
相机确定性复算，`verify` 会做独立全量重渲核对。

## 命令行与退出码

```text
python -m shadowbox generate --out scene.json
python -m shadowbox validate --scene scene.json
python -m shadowbox render --scene scene.json --out OUT [--fresh]
python -m shadowbox render --scene scene.json --out OUT --interrupt-after-blocks 7
python -m shadowbox render --scene scene.json --out OUT --reference-dir BASELINE
python -m shadowbox verify --scene scene.json --output OUT
python -m shadowbox compare --scene scene.json --reference A --candidate B --report diff.json
```

- `0`：成功、可控中断成功，或比较一致。
- `2`：场景校验拒绝。stderr 输出结构化错误，不写图像。
- `3`：运行期 I/O 或其他可报告错误。
- `4`：图像/光学深度或复核不一致；写出差异报告。

可控中断包括 `--interrupt-after-blocks` 和 `SIGINT`/`SIGTERM`。终止请求只会在
当前块完成并提交快照后生效，随后写 `resume_report.json` 并退出 `0`。
`SIGKILL` 不保证清理或退出报告；重启只接受最后完整提交的快照。

## 结构化错误

校验错误示例：

```json
{
  "ok": false,
  "code": "SCENE_REJECTED",
  "errors": [
    {
      "code": "MESH_INDEX_OUT_OF_RANGE",
      "layer_index": 4,
      "message": "triangle 0 references out-of-range vertices",
      "details": {"triangle_index": 0, "vertex_indices": [0, 1, 500]}
    }
  ]
}
```

主要业务码：

| 业务码 | 含义 |
| --- | --- |
| `MESH_INDEX_OUT_OF_RANGE` | 三角形索引越界 |
| `DEGENERATE_TRIANGLE` | 重复索引或零面积三角形 |
| `TRANSMITTANCE_OUT_OF_RANGE` | 透光率不在 `(0, 1]` |
| `DUPLICATE_LAYER_INDEX` | 层号重复 |
| `MISSING_LAYER_INDEX` | 五层中缺少层号 |
| `LIGHT_INSIDE_LAYER_GEOMETRY` | 点光源位于某层纸面三角形上 |
| `LIGHT_SIDE_INVALID` | 点光源不在最内层内侧 |
| `CAMERA_SIDE_INVALID` | 相机不在最外层外侧 |
| `NON_PLANAR_LAYER` | 层网格局部顶点不是 `z=0` |
| `INVALID_CAMERA` / `INVALID_LIGHT` | 相机或光源参数非法 |

## 快照与恢复

每完成一块，完整快照先写同目录临时文件，`fsync` 后通过 `os.replace` 原子替换。
快照记录格式版本、引擎版本、场景摘要、块网格、完成块编号、每块 RGBA/深度
负载及其摘要、每层计数摘要、恢复计数和整文件校验和。

重启时：

1. 校验整文件 checksum、格式/引擎版本、场景摘要、块网格和每块负载摘要。
2. 块位置不取自快照，而是由当前场景网格重新导出。
3. 任一检查失败即删除整个快照，从第 0 块重算，不保留旧块或旧偏移。
4. 有效部分快照恢复时 `recovery_count` 加一；完整快照可幂等重建输出。

差异报告最多列出 64 个差异像素，但 `mismatch_pixels` 为完整数量。每个差异包含
`block_index`、`pixel: [x, y]`、`channels`（`R/G/B/A/DEPTH`）以及两侧值。

## 范围与限制

- 五层配置与层号 `0..4` 是任务明确业务约束。
- 纸面是零厚度平面三角网格，不建模纸张体积内部。
- 投影实现针对相邻层；多层累积遮挡仍是逐层相邻模型的组合。
- 当前实现只依赖 NumPy/Pillow 和标准库，无 GPU、网络、外部素材或容器依赖。
  实测环境为 Linux/WSL 的 Python 3.14；代码路径使用 `pathlib` 与原子
  `os.replace`，不包含 Unix 专用渲染逻辑。
