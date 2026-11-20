# DUSTVOL1 场景格式

场景文件由固定 52 字节头、UTF-8 JSON 元数据和连续密度负载组成。所有多字节整数和
`float32` 均为小端序。

## 文件头

| 偏移 | 长度 | 含义 |
| ---: | ---: | --- |
| 0 | 8 | 固定魔数 `DUSTVOL1` |
| 8 | 4 | 容器版本，当前为 `1` |
| 12 | 4 | JSON 元数据字节数 `metadata_length` |
| 16 | 4 | 密度负载字节数 `payload_length` |
| 20 | 32 | 密度负载的 SHA-256 原始字节 |
| 52 | `metadata_length` | JSON 元数据 |
| `52 + metadata_length` | `payload_length` | 密度块 `float32` 负载 |

解析器要求文件总长度严格等于 `52 + metadata_length + payload_length`，并校验负载
SHA-256。块 `offset` 是从负载起点计算的字节偏移，因此必须 4 字节对齐。

## 元数据

必需顶层字段：

- `schema`: 当前为 `1`
- `scene_id`
- `screen.width`、`screen.height`
- `camera.intrinsics`、`camera.world_to_camera`、`camera.near`、`camera.far`
- `ambient`
- `light.direction`、`light.color`、`light.intensity`
- `density_fields`
- `media`

相机使用 `3x3` 内参和 `4x4` world-to-camera 矩阵。光线方向按
`R^T * (u, v, -1)` 转到世界空间，其中 `u=(x+0.5-cx)/fx`、
`v=(y+0.5-cy)/fy`。这与容器样本的相机约定一致。

## 密度场

密度场字段包括：

- `name`
- `grid = [nx, ny, nz]`
- `bounds_min`、`bounds_max`
- `default`
- `version`
- `blocks`

每个块包含 `index`、`origin`、`shape`、`offset`、`count` 和 `hash`。`origin` 是
全局网格索引，`count` 必须等于 `shape` 三个分量之积，`hash` 是块原始
`float32` 小端字节的 SHA-256。块不得越出网格、重叠负载区间，索引和密度值必须
合法。物理坐标到网格坐标采用线性映射，采样使用三线性插值。

## 介质

支持的形状：

```json
{"type": "sphere", "center": [0, 0, 0], "radius": 1.5}
```

```json
{"type": "box", "min": [-1, -1, -1], "max": [1, 1, 1]}
```

盒体也接受 `center` 加 `half_size`/`half_extents`。介质字段包括 `id`、
`shape`、`scattering`、`absorption`、`phase_g`、`ior`、可选 `parent`，以及可选
`density = {field, scale, bias}`。`parent` 必须引用已存在且包含子介质的介质；
层级不得成环。

## 配置

渲染配置示例：

```json
{
  "chunk_width": 12,
  "chunk_height": 9,
  "parallelism": 3,
  "task_budget": 100000,
  "budget_policy": "degrade",
  "base_step": 0.12,
  "min_step": 0.04,
  "max_step": 0.8,
  "density_adapt_gain": 1.25,
  "phase": 0.5,
  "shadow_samples": 2,
  "shadow_distance": 8.0,
  "precision": "float",
  "compare_precision": false,
  "seam_validation": true,
  "seam_tolerance": 0.00002
}
```

`budget_policy=degrade` 会在 `max_step` 内增大全局有效步长；无法满足预算时返回
`E_BUDGET_EXCEEDED`。`budget_policy=reject` 会拒绝超预算分块并保留已完成分块。
