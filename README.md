# Deterministic Layered-Dome Soft Renderer

纯 Python + NumPy 的离线软光栅验证组件。它读取一份穹顶场景 JSON，输出可逐像素核对的
RGBA 图、层归属图、逐灯投影落点清单、稳定统计和可续算快照。实现不使用 GPU、网络、
Docker、未播种随机数或时间戳。

## Requirements

- Python 3.10+
- NumPy 1.24+

```bash
python -m pip install -r requirements.txt
```

也可以直接在仓库根目录运行，无需安装：

```bash
python -m dome_render --version
```

## Quick Start

生成确定性本地样本：

```bash
python -m dome_render.sample_scene --out scenes/sample_scene.json
```

校验并渲染：

```bash
python -m dome_render validate --scene scenes/sample_scene.json
python -m dome_render render --scene scenes/sample_scene.json --out artifacts/sample
```

Windows PowerShell 使用相同的 `python ...` 命令。渲染过程中可按 `Ctrl+C`：程序会完成当前
分块、提交快照、写 `recovery.json`，并以退出码 `0` 结束。再次运行同一命令会续算。

## Outputs

| 文件 | 内容 |
| --- | --- |
| `rgba.png` | 16-bit RGBA。颜色为线性合成后经 `1-exp(-C*exposure)` 色调映射、再编码为 sRGB；Alpha 为直通 Alpha，RGB 为预乘颜色通道。 |
| `layer_map.png` | RGBA8 层归属图。`R = layer index + 1`（后墙为 255），`G = 1/2/3`（肋拱/星图板/内壁），`B = object id + 1` 的低 8 位，`A =` 获胜子样本占比。背景为全零。 |
| `projections.json` | 每盏灯、每个肋拱三角形质心到相邻外层星图板与内壁的落点：世界坐标、接收面 UV、屏幕坐标、是否越界、接收软阴影灰度。按几何量规范化排序，不依赖三角面书写顺序。 |
| `report.json` | 字段顺序固定的稳定统计：每层可见片元数、平均透射率、星点数、投影采样数、半影趋势、分块完成情况、图像摘要、恢复次数等。 |
| `snapshot.json` | 最近一次完整提交的累计进度快照，包含场景摘要、块布局、所有已完成块像素和校验摘要。原子覆盖写入。 |
| `recovery.json` | 中断、续算或快照丢弃的恢复报告。 |
| `differences.json` | 仅在已恢复块与确定性重算不一致时生成；包含分块编号、像素坐标、通道及两侧值。 |

## Scene JSON

完整有效样本见 `scenes/sample_scene.json`。下面仅展示字段骨架，网格顶点/三角形和灯数组做了
缩写（实际文件必须提供完整网格和恰好三盏灯）：

```json
{
  "schema_version": 1,
  "renderer": {
    "width": 320,
    "height": 200,
    "block_size": [64, 40],
    "sampling": {
      "supersample": 2,
      "shadow_samples": 16,
      "projection_density": 2
    },
    "exposure": 1.12,
    "background": [0.0, 0.0, 0.0, 0.0]
  },
  "camera": {
    "position": [0.0, -0.28, -5.8],
    "target": [0.0, 0.0, 3.3],
    "up": [0.0, 1.0, 0.0],
    "fov_y_deg": 58.0,
    "near": 0.05,
    "far": 40.0
  },
  "lights": [
    {
      "id": 0,
      "direction": [0.358906998, 0.219332054, 0.907237134],
      "irradiance": [1.08, 0.91, 0.72],
      "angular_radius_deg": 7.5,
      "source_distance": 5.5,
      "distance_softness": 0.003
    }
  ],
  "dome": {
    "inner_wall": {
      "plane": {"point": [0.0, 0.0, 5.35], "normal": [0.0, 0.0, -1.0]},
      "u_axis": [1.0, 0.0, 0.0],
      "v_axis": [0.0, 1.0, 0.0],
      "half_size": [4.4, 3.2],
      "resolution": [240, 180],
      "color": [0.016, 0.026, 0.052],
      "emission": [0.010, 0.016, 0.038],
      "shadow_floor": 0.28
    },
    "layers": [
      {
        "index": 0,
        "ribs": [
          {
            "id": 0,
            "mesh": {"vertices": [[0, 0, 0]], "triangles": [[0, 0, 0]]},
            "material": {
              "reflectance": 0.58,
              "anisotropy": 0.57,
              "roughness": 0.23,
              "base_color": [1.0, 0.73, 0.29]
            }
          }
        ],
        "star_boards": [
          {
            "id": 0,
            "mesh": {"vertices": [], "triangles": [], "uvs": []},
            "thickness": 0.36,
            "material": {
              "transmission": 0.80,
              "texture_seed": 10007,
              "resolution": [192, 128],
              "tint": [0.80, 0.88, 1.0]
            }
          }
        ]
      }
    ]
  }
}
```

协议约束：

- `dome.layers` 的 `index` 必须唯一且连续覆盖 `0..N-1`；每层至少一个肋拱和一个星图板。
- 同一层内肋拱 `id`、星图板 `id` 必须唯一且从 0 连续编号。
- 每层所有星图板必须共面；方向光不得平行于任何层平面。
- 方向灯必须恰好三盏，`direction` 必须是单位向量。
- 星图板 UV 必须在其平面上构成非退化仿射映射；省略 `uvs` 时由顶点平面坐标确定性派生。
- 肋拱是实体网格时会被闭合性检测；相机不能位于闭合几何内部。
- 层序号 0 为最内层，序号增大方向为外。方向光 `direction` 指向外侧接收面。
- `shadow_samples` 是投影边缘盘形采样数；`projection_density` 是投影栅格子纹素密度；
  `supersample` 是每像素每轴的固定子样本数。

## Deterministic Model

### Stars

每块星图板仅由 `texture_seed` 和 `resolution` 生成星点位置、半径与亮度。随机流使用
SHA-256 计数器，不依赖 NumPy RNG、层遍历顺序或 Python 全局随机状态。星点纹理量化为
`uint16` RGB 采样表，其摘要写入报告。局部透射率和星点发射均受该采样表调制。

### Layer Shading and Compositing

星图板透射率按入射角与板厚累积：

```text
tau_effective = transmission ^ (max(thickness, 0.1) / 0.1 / cos(theta))
tau_local     = tau_effective * (1 - 0.72 * star_brightness)
```

肋拱使用方向光漫反射与各向异性金色高光，最终统一色调映射，避免整片过曝。层 0 为最内层，
前向透射合成为：

```text
C = sum_k (product_{j<k}(1-alpha_j)) * C_k + product_all(1-alpha) * C_wall
```

每层的 `transmission_contribution`、`reflection_contribution` 及占比写入 `report.json`。
固定子像素位置和层内深度决胜键消除片元遍历顺序影响。

### Projection and Penumbra

内层肋拱三角形沿每盏灯方向投到相邻外层星图板平面与穹顶内壁平面，接收面 UV 中栅格化
遮挡覆盖，再以确定性盘形核柔化。半影模型为：

```text
penumbra_width = layer_gap * tan(angular_radius) + source_distance * distance_softness
```

因此层距与光源到肋拱距离增加时半影变宽。报告中的 `penumbra_trend` 对同一边缘给出多组
采样亮度、10%-90% 过渡宽度和灰度摘要。接收面之外的落点标记 `accepted: false` 且不参与
遮挡；软核零填充，不会把接收面边界直接切成硬黑边。

## Snapshot and Resume

每完成一块，程序在 `snapshot.json` 中原子覆盖写入当前全部已完成块、场景摘要、块布局、
块颜色摘要、载荷摘要与快照自校验值。

重启时：

1. 快照缺失则从零渲染。
2. 快照损坏、自校验失败、场景摘要/布局不一致或块偏移不符时，整体丢弃并从零渲染。
3. 合法部分快照中的块会被确定性重算并逐通道比对；一致才沿用快照像素，追加后续块。
4. 不一致时写 `differences.json` 并以退出码 `5` 结束。
5. `Ctrl+C` 只设置中断标志；当前块完成后提交快照并以 `0` 退出。

完整快照视为已完成输出缓存，不作为“部分续算”复用；重复运行会从零重算，从而保持统计字段稳定。

## Validation and Exit Codes

```text
0  success, complete or handled Ctrl+C
2  scene validation error
3  output I/O or incomplete-block error
5  resumed image/layer mismatch
```

校验错误输出到 stdout，形如：

```json
{
  "ok": false,
  "errors": [
    {
      "code": "E_MESH_INDEX_OUT_OF_RANGE",
      "location": "$.dome.layers[0].ribs[0].mesh.triangles[0][0]",
      "field": "triangles[0][0]",
      "message": "Triangle index is outside the vertex array.",
      "value": 99999
    }
  ]
}
```

覆盖的错误包括网格索引越界、退化三角形、材质范围、层/肋拱编号缺失或重复、方向光长度、
方向光与层平面平行、相机参数、相机位于闭合几何内部、墙面平面非法、星图板 UV 非法等。

## Tests

```bash
python -m unittest discover -s tests -v
```

测试包含真实 `SIGKILL` 重启续算、`SIGINT` 当前块收尾、快照损坏/场景不一致丢弃、可校验
快照篡改后的结构化差异、两次运行字节比较、层/三角面顺序反转、投影偏移和半影趋势。

实际验证记录见 `VALIDATION.md`。
