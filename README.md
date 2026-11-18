# veil-render

离线、确定性的多层纱幕与金属花格一致性验证渲染器。输入只来自本地 JSON；
纹理由整数种子生成，不读取外部模型、纹理、网络或未播种随机数。

## 快速开始

要求 Python 3.11+、NumPy、Pillow：

```bash
python3 -m pip install -r requirements.txt

# 1. 生成确定性样例
python3 -m veilrender generate-scene --output examples/courtyard_night.json

# 2. 分块渲染；每块提交完整快照
python3 -m veilrender render \
  --scene examples/courtyard_night.json \
  --output-dir examples/verified_output

# 3. 与另一份不中断结果逐像素核对
python3 -m veilrender compare \
  --expected examples/verified_output \
  --actual /path/to/resumed_output
```

也可使用 `python3 render.py ...`。安装为包后可使用 `veil-render` 命令。

## 主要输出

- `rgba.png`：预乘 Alpha RGBA8 图像，所有通道值满足 `RGB <= A`。
- `layer_ownership.png`：逐像素层归属图。`R,G` 为大端 16 位主归属
  ordinal，`B` 为命中层数，`A` 为是否有层命中；`layer_ownership.json`
  给出 ordinal 到场景层号/编号的映射。
- `projection_landings.json`：每个光源、花格层、接收面的投影源点、落点、
  距离、接收面内标记和半影宽度；另含两组单调性审计和边缘亮度剖面。
- `report.json`：稳定统计。含每层可见片元数、平均透射率、透射/反射贡献
  占比、纹理摘要、投影采样数、最终图像摘要和快照提交数。
- `snapshot/`：每块一个二进制 tile 与原子更新的完整 manifest。
- `run_metrics.json`：耗时、本次快照载入模式和本次提交块数；不稳定字段
  与 `report.json` 隔离。
- `differences.json`：仅在比较发现差异时产生，含块号、像素坐标和通道。

`examples/verified_output/` 是已生成并复核的 256x160 样例输出。

## 场景要点

```json
{
  "schema_version": "veil-scene/1",
  "image": {
    "width": 256,
    "height": 160,
    "tile_width": 64,
    "tile_height": 40,
    "background_rgba": [0.012, 0.017, 0.035, 1.0]
  },
  "camera": {
    "position": [0.0, 1.65, 6.3],
    "target": [0.0, 1.28, 0.0],
    "up": [0.0, 1.0, 0.0],
    "fov_y_deg": 43.0,
    "near": 0.03,
    "far": 20.0,
    "samples": 2
  },
  "layers": [
    {
      "id": "gauze-a",
      "layer": 1,
      "kind": "gauze",
      "mesh": {
        "vertices": [[-2.5, 0.4, 0.35], [2.5, 0.4, 0.35],
                     [2.5, 2.7, 0.35], [-2.5, 2.7, 0.35]],
        "triangles": [[0, 1, 2], [0, 2, 3]]
      },
      "transmittance": 0.56,
      "tint": [0.30, 0.78, 0.74],
      "weave_seed": 1103,
      "resolution": [64, 64],
      "texture_repeat": 8,
      "thickness": 0.005
    }
  ]
}
```

`layer` 是唯一正整数，按由内向外排序；同号或 `id` 重复会被拒绝。
花格层使用 `reflectance`、`width`、`metal_color`、`pattern.style`
(`grid`/`diamond`)、`pattern.pitch` 与 `pattern_seed`。点光源使用
`position`、`intensity`、`color`；地面使用 `y`、`x`/`z` 边界、`color`、
`reflectance`。完整字段与默认值见 `docs/TECHNICAL.md`。

## 校验与退出码

所有校验错误先聚合，再以 JSON 写到 stderr（若给出了输出目录，也写
`error.json`）。层相关错误带 `layer`、`layer_id`、`code` 与 `path`。

| 退出码 | 含义 |
|---:|---|
| 0 | 完成，或 Ctrl+C 已完成当前块并写出恢复报告 |
| 2 | 场景 JSON/几何/材质/相机校验失败 |
| 3 | 与基线存在逐像素或投影清单差异 |
| 4 | 快照、I/O 或其他运行时错误 |

主要业务错误码：`E_MESH_INDEX_RANGE`、`E_TRIANGLE_DEGENERATE`、
`E_TRANSMITTANCE_RANGE`、`E_REFLECTANCE_RANGE`、
`E_LAYER_ORDER_DUPLICATE`、`E_LAYER_ID_DUPLICATE`、
`E_LIGHT_INSIDE_LAYER`、`E_CAMERA_INVALID`。

## 快照与中断

每完成一块，先将 tile 原子写入，再原子替换包含全部已完成块及颜色摘要的
`snapshot/manifest.json`。重启时逐项校验场景摘要、算法版本、尺寸、文件
摘要、RGBA/归属摘要和 tile 形状；任一项损坏都会删除整个快照并从零重算。

Ctrl+C 只设置中断标记，当前块仍会计算并提交，随后写 `recovery.json` 并以
0 退出。SIGKILL 不保证退出报告；下次启动只读取已提交 manifest。

## 验证

```bash
python3 -m unittest discover -s tests -v
```

测试包括真实 SIGKILL 后续算、Ctrl+C、损坏快照丢弃、未中断/续算逐字节
比较、投影半影趋势、预乘 Alpha、纹理复算及所有指定非法输入。实际结果、
样例摘要与半影采样值记录在 `docs/VERIFICATION.md`。
