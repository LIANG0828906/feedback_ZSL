# 皮影多层软光栅验证器

这是一个纯 CPU、离线、确定性的软光栅验证组件，用 Python 3.11+ 与 NumPy 实现。它读取一份 JSON 场景，生成：

- 预乘 Alpha 的逐像素 RGBA 图像；
- 逐像素皮革层归属图；
- 三盏油灯、各层镂空、相邻外层与幕布的组合投影落点清单；
- 稳定统计报告，包括逐层透射/反射占比、三灯重叠、半影宽度与边缘亮度采样；
- 原子提交的分块快照，可在中断或强制终止后续算。

不需要 GPU、容器、外部媒体或网络。

## 快速开始

环境要求：Python 3.11 或以上、NumPy 1.26 或以上。

```bash
py -3.11 -m pip install "numpy>=1.26,<2"
py -3.11 -m pip install -e .

python -m leather_raster sample --out examples/sample_scene.json
python -m leather_raster validate examples/sample_scene.json
python -m leather_raster render examples/sample_scene.json --out artifacts/sample_render
```

输出目录中主要文件：

| 文件 | 内容 |
| --- | --- |
| `rgba.bin` | `uint8`、`H x W x 4`、C 顺序、预乘 Alpha |
| `layer_map.bin` | `uint8`、`H x W`，`0..N-1` 为前向层序号，`255` 为背景/幕布 |
| `rgba.png` | 直接查看的预乘 RGBA 预览 |
| `layer_map.png` | 层归属预览 |
| `projection_points.json` | 每盏灯、每个镂空三角、每个接收面的投影顶点/中心/半影参数 |
| `stats.json` | 稳定图像统计、逐层能量占比、三灯重叠与半影趋势 |
| `manifest.json` | 场景摘要、分辨率、各输出 SHA-256 |
| `recovery_report.json` | 本次渲染与恢复状态 |
| `.snapshot/checkpoint.json` | 原子提交的分块快照 |

`rgba.npy` 与 `layer_map.npy` 也同时提供，便于 NumPy 直接加载。

## 中断与恢复

可处理中断会在当前块渲染完成后提交快照、写恢复报告并以退出码 `0` 结束：

```bash
python -m leather_raster render examples/sample_scene.json \
  --out artifacts/resume_demo --interrupt-after-blocks 4

python -m leather_raster resume examples/sample_scene.json \
  --out artifacts/resume_demo --verify-baseline artifacts/sample_render
```

默认 `render` 与 `resume` 都会加载已有的合法快照。`--fresh` 会丢弃快照并从零重算。快照校验失败、场景摘要不匹配或任一负载校验失败时，所有已存块都会整体丢弃，不会沿用旧块或旧偏移。

`--verify-baseline` 会比较实际输出与基准目录中的 `rgba.bin`、`layer_map.bin` 和 `projection_points.json`。不一致时写 `diff.json`，其中每条差异包含 `block_id`、`x`、`y`、`channel`、`actual`、`baseline`，进程以退出码 `4` 结束。

## 校验与退出码

```bash
python -m leather_raster validate scene.json
```

校验失败时输出结构化 JSON，示例：

```json
{
  "ok": false,
  "error": {
    "code": "SCENE_VALIDATION_FAILED",
    "message": "scene validation failed",
    "issues": [
      {
        "code": "MESH_INDEX_OUT_OF_RANGE",
        "path": "$.layers[2].mesh.triangles[17][1]",
        "message": "vertex index 900 is outside [0, 811]",
        "layer_index": 2
      }
    ]
  }
}
```

退出码：

- `0`：成功，或可处理中断已完成当前块并写出恢复报告；
- `2`：场景校验失败；
- `3`：内部错误；
- `4`：输出与基准不一致。

## 场景结构

核心字段如下；完整规则与算法见 [TECHNICAL.md](TECHNICAL.md)。

```json
{
  "schema_version": 1,
  "render": {
    "width": 192,
    "height": 144,
    "block_width": 48,
    "block_height": 36,
    "supersampling": 2,
    "projection_samples": 4,
    "exposure": 0.9,
    "ambient": 0.14,
    "background": [0.012, 0.009, 0.006, 1.0]
  },
  "camera": {
    "position": [0, 0, -3.8],
    "target": [0, 0, 0.1],
    "up": [0, 1, 0],
    "fov_degrees": 44,
    "near": 0.05,
    "far": 12
  },
  "screen": {
    "origin": [-1.9, -1.65, 0.95],
    "u_axis": [1, 0, 0],
    "v_axis": [0, 1, 0],
    "width": 3.8,
    "height": 3.3,
    "tint": [0.56, 0.42, 0.25],
    "fiber_seed": 8675309
  },
  "lights": [
    {
      "id": "lamp_left",
      "position": [-1.42, -0.48, -1.46],
      "intensity": 2.1,
      "color": [1.0, 0.66, 0.3],
      "radius": 0.11,
      "softness_gain": 1.0
    }
  ],
  "layers": [
    {
      "layer_index": 0,
      "mesh": {
        "vertices": [[0, 0, 0], [1, 0, 0], [0, 1, 0]],
        "triangles": [[0, 1, 2]]
      },
      "cutouts": [],
      "transmittance": 0.36,
      "tint": [0.74, 0.29, 0.1],
      "fiber_seed": 104729,
      "thickness": 0.011
    }
  ]
}
```

上例是字段片段，实际场景必须有三盏灯和至少一个有效皮革层。约定 `layer_index = 0` 为最靠近相机、最先参与前向合成的层；更高层序号朝向幕布。每层 `cutouts[].cutout_id` 在该层内必须从 `1` 开始连续编号，未列入任何镂空的三角形为实心皮革。

## 测试

```bash
python -m unittest discover -v
```

测试覆盖场景错误路径、纹理无状态确定性、同色邻层种子区分、层输入重排、分块大小无关、完整渲染重复性、三灯投影重叠、半影趋势、优雅中断、强制终止恢复、快照损坏、场景摘要不匹配及逐像素差异报告。实际运行结果见 [VALIDATION.md](VALIDATION.md)。

