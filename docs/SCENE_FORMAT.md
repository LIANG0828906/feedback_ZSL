# 场景格式与校验

单位约定：几何长度为米，角度为度，颜色分量为 `[0,1]`，图像尺寸和分块尺寸为像素。样本生成器为 `tools/generate_samples.py`。

## 顶层结构

```json
{
  "schema_version": 1,
  "seed": 20261118,
  "image": {
    "width": 480,
    "height": 360,
    "tile_size": 40,
    "aa_samples": 2,
    "background": [0.018, 0.013, 0.010]
  },
  "camera": {
    "position": [0.16, 1.06, 3.92],
    "target": [0.0, 3.42, 0.0],
    "up": [0.0, 1.0, 0.0],
    "vertical_fov_degrees": 56.0,
    "near": 0.08,
    "far": 30.0
  },
  "ambient": [0.040, 0.029, 0.019],
  "layers": [],
  "lamps": [],
  "dougong": [],
  "shadow": {
    "samples_per_axis": 6,
    "probe_samples": 65,
    "probe_half_span": 2.4
  }
}
```

约束：

- `schema_version` 必须为 `1`。
- 图像宽高、分块尺寸和 AA 倍数必须为正。AA 倍数为每轴子样本数，范围为 `1..4`。
- 相机位置与目标不能相同，上向量不能为零且不能与视线平行，`0 < near < far`。
- `seed` 为有符号 64 位整数。

## 层

每层有唯一 `layer_id`（`1..254`）和四象限网格：

```json
{
  "layer_id": 1,
  "wood_color": [0.88, 0.30, 0.15],
  "transmittance": 0.60,
  "reflectance": 0.10,
  "lattice_width": 0.060,
  "quadrants": [
    {
      "quadrant": 0,
      "grid": {"columns": 7, "rows": 4},
      "vertices": [[0.2, 3.12, 0.0]],
      "uvs": [[0.0, 0.0]],
      "indices": [0, 1, 2]
    }
  ]
}
```

- `layer_id` 重复报 `E_LAYER_DUPLICATE`。
- `transmittance` 和 `reflectance` 必须在 `[0,1]`，分别报 `E_TRANSMITTANCE_RANGE`、`E_REFLECTANCE_RANGE`。
- `lattice_width` 为木格条宽占一个格元比例，范围 `(0.001,0.49)`。
- 象限号必须在 `[0,3]` 且同一层内唯一。
- 也允许省略 `quadrants`，把一个带可选 `"quadrant"` 的网格直接放在层对象上。
- 层序号越小表示越靠内、越接近相机；合成时按 `layer_id` 升序由内向外进行。

### 网格

- `grid.columns`、`grid.rows` 为正整数，用于木格/薄纱材质分区和纹样映射。
- `vertices` 为三维点数组；`indices` 为三角形索引数组，长度必须为 3 的非零倍数。
- `uvs` 可选，若缺省则由网格的 X/Z 包围范围生成。
- 索引越界报 `E_GEOMETRY_INDEX`，附带 `layer_id` 与 `quadrant`。
- 零面积三角形报 `E_TRIANGLE_DEGENERATE`。
- 网格必须包含至少一个有效三角形。
- 面法线按三角形几何计算并支持双面着色。

## 油灯

必须恰好三盏：

```json
{
  "id": "lamp_a",
  "position": [-0.56, 1.62, -0.34],
  "color": [1.0, 0.61, 0.28],
  "intensity": 6.4,
  "radius": 0.12
}
```

- `id` 唯一。
- `radius` 表示面积光球形采样半径，必须大于 0。
- 油灯若与任一层三角形共面且落在三角形内部，报 `E_LAMP_INSIDE_LAYER`，附带层号、象限和灯 id。

## 斗拱

```json
{
  "id": "dougong_deep",
  "color": [0.24, 0.12, 0.055],
  "grid": {"columns": 2, "rows": 2},
  "vertices": [[0, 0, 0]],
  "indices": [0, 1, 2]
}
```

- 至少一个斗拱；`id` 唯一。
- 网格索引和退化规则与层网格相同。
- 斗拱同时参与相机遮挡和油灯到彩画的阴影射线遮挡。

## 软阴影

- `samples_per_axis`：每盏面积灯每轴采样数，实际采样数为平方，范围 `2..16`。
- `probe_samples`：报告剖面的采样点数，必须为 `[5,257]` 内奇数。
- `probe_half_span`：剖面沿受影面半宽，单位米。

## 业务错误码

| 代码 | 条件 |
|---|---|
| `E_GEOMETRY_INDEX` | 三角形索引越界 |
| `E_TRIANGLE_DEGENERATE` | 三角形零面积 |
| `E_TRANSMITTANCE_RANGE` | 透光率不在 `[0,1]` |
| `E_REFLECTANCE_RANGE` | 反射率不在 `[0,1]` |
| `E_LAYER_DUPLICATE` | 层序号重复 |
| `E_LAMP_INSIDE_LAYER` | 油灯落在层三角形内部 |
| `E_LAMP_COUNT` | 油灯不是恰好三盏 |
| `E_GEOMETRY_EMPTY` | 网格或索引为空 |
| `E_CAMERA_PARAM` | 相机参数非法 |
| `E_JSON_PARSE` | JSON 语法错误 |
| `E_SCENE_IO` | 场景不可读 |

错误输出示例：

```json
{"error":{"code":"E_GEOMETRY_INDEX","message":"triangle index is out of range","layer_id":1,"quadrant":0}}
```

所有校验在创建最终图像前完成；错误只写 stderr，不写部分 PNG。

## 场景摘要

摘要不是输入文件的字节哈希，而是对语义规范化后的场景计算 SHA-256：

- 层按 `layer_id` 排序，象限按象限号排序；
- 油灯和斗拱按 `id` 排序；
- 数值使用稳定十进制格式；
- 包含材质、相机、图像参数、全部网格与阴影参数。

因此仅调整 JSON 数组顺序或对象字段顺序不会改变摘要，也不会改变渲染输出。快照中的摘要不匹配时会删除整个旧快照并从头渲染。

