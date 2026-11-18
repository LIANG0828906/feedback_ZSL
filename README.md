# 宋代青瓷叠盘离线软光栅预览

这是一个完全离线、确定性的 Python/NumPy 软光栅组件，用任务代码构造三层材质、三只叠放青瓷盘、顶部点光源与相机，并输出可逐像素核对的 RGBA、光学深度、投影图和稳定统计报告。运行时不读取外部图片、不访问网络、不使用容器。

## 快速开始

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r requirements.txt
# 可选：安装为命令行程序 celadon-museum
# python -m pip install -e .

python -m celadon_museum make-scene --out examples/scene.json
python -m celadon_museum validate --scene examples/scene.json
python -m celadon_museum render --scene examples/scene.json --out out
```

也可以使用便捷入口：

```bash
python render.py render --scene examples/scene.json --out out
python render.py --scene examples/scene.json --out out
```

默认场景为 `384x256`、`64x64` 分块、3 只盘、每只盘 1 层胎体 + 2 层釉面、2 个顶部点光源。`examples/scene.json` 由 `make-scene` 确定性生成，可随时删除后重建。

## 场景格式

以下为字段结构节选；实际网格数组由 `make-scene` 生成，节选中的网格仅示意字段位置。

```json
{
  "schema": "celadon-scene-v1",
  "image": {
    "width": 384,
    "height": 256,
    "tile_size": 64,
    "background": [0.011, 0.017, 0.021]
  },
  "camera": {
    "position": [1.95, -2.55, 2.05],
    "target": [0.0, 0.0, 0.19],
    "up": [0.0, 0.0, 1.0],
    "vfov_deg": 35.0,
    "near": 0.02,
    "far": 50.0
  },
  "lights": [
    {
      "id": "warm-key",
      "position": [-1.45, -0.95, 4.10],
      "intensity": 5.6,
      "color": [1.0, 0.92, 0.78],
      "radius": 0.20
    }
  ],
  "plates": [
    {
      "plate_id": 0,
      "center": [0.0, 0.0, 0.0],
      "orientation": [1.0, 0.0, 0.0, 0.0],
      "radius": 0.80
    }
  ],
  "layers": [
    {
      "plate_id": 0,
      "layer_id": 0,
      "kind": "body",
      "seed": 20261118,
      "transmittance": 0.13,
      "scatter_color": [0.22, 0.39, 0.36],
      "texture_resolution": [128, 128],
      "carve_depth": 1.0,
      "vertices": [[0.0, 0.0, -0.02]],
      "triangles": [[0, 1, 2]]
    }
  ],
  "render": {
    "tile_size": 64,
    "ambient": 0.31,
    "exposure": 0.94,
    "max_display": 0.94,
    "projection_strength": 0.62,
    "soft_shadow_samples": 12,
    "projection_threshold": 0.57,
    "depth_scale": 8.0
  }
}
```

约定：

- `orientation` 为归一化四元数 `[w, x, y, z]`；网格顶点为盘局部坐标，世界坐标为 `R @ local + center`。
- 每只盘必须且只能有 `layer_id` 0、1、2 各一层，其中恰好一层 `body`、两层 `glaze`。0 为胎体内层，1 为内釉，2 为外釉。
- 同一像素的多层片元按 `(plate_id, layer_id)` 升序由内向外合成，不依赖 JSON 中 `plates` 或 `layers` 的列表顺序。
- `soft_shadow_samples` 是投影边缘与半影比较共同使用且记录在报告中的采样密度。
- `transmittance` 必须在 `[0, 1]`，`scatter_color` 和光源颜色各通道必须在 `[0, 1]`。

## 实现要点

**校验先于输出。** 读入 JSON 后先做完整业务校验和场景摘要，再创建输出目录或快照。任何校验失败只输出结构化 JSON 错误，不产生 `rgba.png` 等部分结果。

**确定性纹理。** 冰裂纹使用整数哈希驱动的 Worley F1/F2 边界表；三邻域汇合点由 F1/F2/F3 距离共同判定，并用确定性的 3x3 邻域混合压低交界峰值。`sample_crackle(seed, resolution, x, y)` 可脱离纹理上下文逐点复算同一值。刻花表由种子、半径、角度和分层值噪声生成，同时调制透射率、散射色调与表面明暗。

**软光栅着色。** 每层三角形先做近/远裁剪、透视投影和屏幕空间光栅化；片元属性使用透视正确插值。每层每像素保留最近片元，再按固定盘号/层号由内向外以预乘 Alpha 合成。着色包含入射角透射、散射色漫反射、低强度宽高光与 Reinhard + sRGB 色调映射，顶灯下的盘沿不会截断成 255。

**盘间投影。** 对下层盘片元、每个更高盘、每个点光源，将光线投影到上层盘局部纹理平面，采样同一套刻花/镂空表。光源半径内的低差异采样形成半影；落点超出上层盘时按无遮挡处理。渲染只增加透射光，不通过减法制造盘沿黑边。

**分块快照。** 每块完成后先原子写入 RGBA/深度/投影数组，再整体替换 `.state/snapshot.json`。快照保存场景摘要、图像几何、已完成块编号、块颜色/深度/投影摘要、逐块统计与恢复次数。恢复时校验快照自身摘要、场景摘要及所有块摘要；任一项失败就删除整个状态目录并从第 0 块重算。

**恢复后自检。** 使用快照续算时默认重新扫描全部块，与已提交块逐字节比较。差异会写入 `resume_diff.json`，并在 CLI 输出中包含块号、像素坐标和通道，退出码为 3。

## 输出

`render --out out` 生成：

- `rgba.png`：无损 RGBA8 预览图。
- `rgba.npy`：与 PNG 对应的精确 RGBA 数组。
- `optical_depth.npy`：`float32` 逐像素光学深度。
- `optical_depth.png`：按 `depth_scale` 归一化的 16 位深度可视化。
- `projection_map.npy` / `projection_map.png`：盘间透射投影能量图。
- `report.json`：稳定字段顺序的统计报告。
- `resume_report.json`：块完成状态、下一块编号、颜色摘要与恢复次数。
- `.state/`：原子快照与已提交块。

`report.json` 包含每层可见片元数、平均透光率、裂纹交界处理采样点数、纹理摘要、投影光斑数及质心、软阴影采样总数、三组层叠间距下的半影宽度与亮度采样、分块完成情况、RGBA/深度摘要、恢复次数和续算自检结果。报告中不含时间戳或绝对路径。

## 中断与恢复

```bash
# 完成 7 块后写出中断报告，退出码 0
python -m celadon_museum render --scene examples/scene.json --out out --stop-after-tiles 7

# 续算；默认重扫全部块并核对逐字节一致性
python -m celadon_museum render --scene examples/scene.json --out out

# 丢弃旧快照，从零开始
python -m celadon_museum render --scene examples/scene.json --out out --reset
```

`SIGINT`/`SIGTERM` 只设置停止标记，当前块完成并提交后才写中断报告，进程退出码 0。`SIGKILL` 可能留下最后一次已提交快照；重启后只使用该快照。

## 结构化业务码

校验错误以如下形式输出并返回非零退出码：

```json
{
  "ok": false,
  "error": {
    "code": "E_DEGENERATE_TRIANGLE",
    "plate_id": 0,
    "layer_id": 1,
    "triangle_index": 4,
    "reason": "triangle has zero area or repeated vertices"
  }
}
```

主要错误码：

- `E_MESH_INDEX_OUT_OF_RANGE`、`E_MESH_INDEX_INVALID`、`E_DEGENERATE_TRIANGLE`
- `E_TRANSMITTANCE_OUT_OF_RANGE`、`E_INVALID_COLOR`
- `E_DUPLICATE_PLATE_ID`、`E_MISSING_PLATE_ID`、`E_DUPLICATE_LAYER`、`E_MISSING_PLATE`、`E_MISSING_LAYER`
- `E_LIGHT_INSIDE_LAYER`
- `E_INVALID_CAMERA`、`E_IMAGE_CONFIG`、`E_PROJECTION_CONFIG`
- `E_RESUME_MISMATCH`（续算自检不一致，退出码 3）

## 自动测试

```bash
python -m unittest discover -s tests -v
```

测试覆盖：索引越界、退化三角形、透光率越界、重复/缺失盘号与层号、光源位于层内、纹理逐字节复现、裂纹交界独立复算、输入顺序无关、透射/散射观感约束、投影随层距偏移、半影随层距变宽、快照损坏丢弃、场景摘要变化重算、中断续算逐字节一致，以及故障注入下的结构化差异报告。

## 本次实际验证

环境：Python 3.14.4、NumPy 2.5.5、Pillow 13.0.0。

- `python -m unittest discover -s tests -v`：20 项测试全部通过，耗时约 11 秒。
- 默认场景摘要：`sha256:12227421bb0aab2d71ee4c8aa773fbdd8e3c85bd50972ec5ec56fa69e361fb05`。
- 默认输出 RGBA 摘要：`sha256:f89c51c0e5bc527bf3a3414f9ced6d83c6e1d956650c8f7b7600984516fc5630`。
- 默认输出光学深度摘要：`sha256:96987c2932fd02a2bdbce63feb7fa6824b76efed4737bf96738ff2338278a9cb`。
- 24/24 块完成；19427 个非背景像素；RGB 最大值 167（未截断为白色），非背景平均 RGB 为 `[117.84, 157.10, 147.31]`，保持青绿层次。
- 投影光斑 8 个；软阴影采样总数 2,710,392。
- 层叠间距 `0.19 / 0.285 / 0.38` 下的 10%-90% 半影宽度为 `0.16813 / 0.17314 / 0.17903`，严格递增。
- 在 `192x128` 验证场景中，盘距从 `0.19` 增至 `0.32` 后，主投影光斑质心移动约 `4.65` 像素。
- 同一验证场景中，两盏顶灯同时降低 `1.0` 后，主投影光斑质心移动约 `1.29` 像素。
- 真实 `SIGTERM` 测试：第 37/70 块完成后返回退出码 0，并写出 `resume_report.json`。
- 真实 `SIGKILL` 测试：已提交 27 块后强制终止，续算完成的 `rgba.npy` 与 `optical_depth.npy` 均与不中断完整渲染逐字节一致，自检差异数为 0。
