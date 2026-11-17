# Scene Format and Validation

## Root Schema

```json
{
  "format": "mosaic-scene-v1",
  "seed": 232,
  "camera": {
    "position": [0.0, 3.35, 3.65],
    "target": [0.0, 0.14, 0.0],
    "up": [0.0, 1.0, 0.0],
    "fovYDeg": 44.0,
    "near": 0.01,
    "far": 30.0
  },
  "floor": {
    "vertices": [[-2.15, 0.0, -2.15], [2.15, 0.0, -2.15]],
    "triangles": [[0, 1, 2]]
  },
  "pieces": [
    {
      "layerIndex": 0,
      "pieceId": 0,
      "transmission": [0.54, 0.69, 0.76],
      "thickness": 0.014,
      "edgeIor": 1.46,
      "vertices": [[-0.5, 0.075, -0.5], [0.5, 0.075, -0.5]],
      "triangles": [[0, 1, 2], [0, 2, 3]]
    }
  ],
  "lights": [
    {
      "id": 0,
      "position": [-1.55, 3.08, 1.25],
      "color": [1.0, 0.84, 0.61],
      "intensity": 2.15
    }
  ],
  "settings": {
    "image": {
      "width": 320,
      "height": 240,
      "chunkWidth": 64,
      "chunkHeight": 48
    },
    "texture": {
      "bubblesPerPiece": 10,
      "flowBandsPerPiece": 3
    },
    "caustic": {
      "samplesPerEdge": 14,
      "penumbraBase": 0.0045,
      "penumbraThickness": 0.62,
      "penumbraLayerDistance": 0.105,
      "intensity": 0.23,
      "edgeBand": 0.12,
      "splatSupport": 2.8
    },
    "glare": {
      "reflectionScale": 0.68,
      "edgePower": 4.5
    },
    "compositionOrder": ["transmission", "reflection"]
  }
}
```

顶点和灯光位置使用同一世界坐标。`layerIndex` 从小到大定义由内向外合成顺序；同一层内按 `pieceId` 规范排序。每个嵌片的 `vertices`/`triangles` 是局部索引网格。

`floor` 应为闭合网格，才能可靠判定“点光源在地坪内部”。闭合边界上的位置也视为内部。地坪顶面高度取所有顶点的最大 `y`，作为焦散基底平面。

## Field Ranges

| Field | Rule |
|---|---|
| `seed` | 非负整数，最大 `2^53-1` |
| `camera.position` | 有限三维向量 |
| `camera.target` | 有限、且不等于相机位置 |
| `camera.up` | 有限、非零、不与视线平行 |
| `camera.fovYDeg` | `(0, 179)` |
| `camera.near/far` | `0 < near < far` |
| `piece.layerIndex` | `[0, 2^31-1]` |
| `piece.pieceId` | `[0, 2^31-1]`，与层号组合后唯一 |
| `piece.transmission` | 三个分量均在 `[0, 1]` |
| `piece.thickness` | `(0, 1]` |
| `piece.edgeIor` | `[1, 3]` |
| triangle index | 位于该嵌片顶点数组内 |
| triangle area | 叉积长度平方必须大于 `1e-24` |
| light.id | 非负且唯一 |
| light.color | 三个分量均在 `[0, 1]` |
| light.intensity | `(0, 100]` |
| `image.*` | 正整数；最大边长 8192；总像素不超过 64M |
| `texture.bubblesPerPiece` | `[0, 64]` |
| `texture.flowBandsPerPiece` | `[0, 32]` |
| `caustic.samplesPerEdge` | `[1, 256]` |
| `caustic.penumbraBase` | `[0, 1]` |
| `caustic.penumbraThickness` | `[0, 2]` |
| `caustic.penumbraLayerDistance` | `[0, 2]` |
| `caustic.intensity` | `(0, 10]` |
| `caustic.edgeBand` | `(0, 0.5]` |
| `caustic.splatSupport` | `[1, 8]` |
| `glare.reflectionScale` | `[0, 2]` |
| `glare.edgePower` | `[1, 32]` |
| `compositionOrder` | `transmission` 与 `reflection` 的排列 |

## Business Error Codes

语义错误统一输出为：

```json
{
  "ok": false,
  "errorCount": 1,
  "errors": [
    {
      "code": "MOSAIC_MESH_INDEX_OUT_OF_RANGE",
      "path": "scene.pieces[0].triangles[0][1]",
      "message": "vertex index is outside scene.pieces[].vertices"
    }
  ]
}
```

`render` 还会把同一内容写到 `<out>/validation_errors.json`，并以 `20` 退出。

| Code | Meaning |
|---|---|
| `MOSAIC_JSON_PARSE` | JSON 语法或数值范围错误 |
| `MOSAIC_FIELD_MISSING` | 必需字段缺失 |
| `MOSAIC_FIELD_TYPE` | 字段类型错误 |
| `MOSAIC_FORMAT_UNSUPPORTED` | 场景格式版本不支持 |
| `MOSAIC_MESH_INVALID` | 顶点/三角形结构错误 |
| `MOSAIC_MESH_INDEX_OUT_OF_RANGE` | 三角网格索引越界 |
| `MOSAIC_TRIANGLE_DEGENERATE` | 三角形退化 |
| `MOSAIC_TRANSMISSION_OUT_OF_RANGE` | 透射率越界 |
| `MOSAIC_IOR_OUT_OF_RANGE` | 边缘折射率越界 |
| `MOSAIC_THICKNESS_OUT_OF_RANGE` | 厚度越界 |
| `MOSAIC_PIECE_KEY_DUPLICATE` | `(layerIndex, pieceId)` 重复 |
| `MOSAIC_LIGHT_INSIDE_FLOOR` | 点光源落在地坪闭合体内 |
| `MOSAIC_LIGHT_ID_DUPLICATE` | 灯光 ID 重复 |
| `MOSAIC_CAMERA_INVALID` | 相机位置/目标/上方向/FOV/裁剪面非法 |
| `MOSAIC_COMPOSITION_ORDER_INVALID` | 合成顺序非法 |
| `MOSAIC_CAUSTIC_SETTING_OUT_OF_RANGE` | 焦散参数越界 |
| `MOSAIC_GLARE_SETTING_OUT_OF_RANGE` | 眩光参数越界 |

缺失字段和重复键均可在同一次解析中与其他错误一起报告。每个错误都带有从根对象开始的字段路径。
