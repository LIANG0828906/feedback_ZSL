# 三版本格式说明

三个版本描述同一套视觉语义，但序列化能力不同。内部模型统一使用：

- 4 通道 RGBA；
- 16 位线性色彩；
- 预乘 alpha 作为规范工作状态；
- `tick` 时间基，默认 `90000 tick/s`；
- 图层顺序为从底到顶，先出现的图层先合成。

## 版本能力

| 版本 | 字段命名 | 层级表达 | 可见性 | alpha 约定 | 时间单位 |
| --- | --- | --- | --- | --- | --- |
| v1 | `snake_case` | 平坦图层表，无组节点 | `enabled` + `visible_until_ms` | straight | ms |
| v2 | `camelCase` | 平坦 `nodes` 表，`parentId` 连成组树 | `visible` + `visibleUntilTick` | premultiplied | tick |
| v3 | `kebab-case` | `tree` 内嵌套 `children` | `visibility.visible` + `visibility.until-tick` | premultiplied | tick |

## v1：legacy flat / straight / ms

```json
{
  "format": "vp-foreground",
  "schema_version": 1,
  "project_id": "virtual-stage-layer-stack",
  "frame": {"width": 64, "height": 48, "channels": 4},
  "timebase": {"unit": "ms", "tick_rate": 90000, "rounding": "half-up"},
  "window_size": 4,
  "layers": [
    {
      "layer_id": "plate-backdrop",
      "name": "Stage Backdrop",
      "content_sha256": "...",
      "opacity": 65535,
      "blend_mode": "normal",
      "enabled": true,
      "visible_until_ms": 1100
    }
  ],
  "instructions": [],
  "frames": [
    {
      "index": 0,
      "time_ms": 0,
      "sample_points": [{"x": 0, "y": 0}]
    }
  ],
  "truth": {"tolerance": 0, "frames": []}
}
```

v1 不声明 `alpha_mode` 时按声明默认值 `straight` 加载，并在
`Project.ConversionTraces` 中记录来源默认值。v1 无组节点，导出时嵌套组被
`flatten-preorder`，会产生 `E_LOSSY` 损失项。

## v2：broadcast flat groups / premultiplied / tick

```json
{
  "format": "vp-foreground",
  "schemaVersion": 2,
  "projectId": "virtual-stage-layer-stack",
  "frameSize": {"width": 64, "height": 48, "channels": 4},
  "timebase": {"unit": "tick", "tickRate": 90000, "rounding": "half-up"},
  "windowSize": 4,
  "nodes": [
    {
      "nodeId": "grp-plate",
      "kind": "group",
      "name": "Plate"
    },
    {
      "nodeId": "plate-backdrop",
      "kind": "layer",
      "parentId": "grp-plate",
      "layerId": "plate-backdrop",
      "contentSha256": "...",
      "opacity": 65535,
      "blendMode": "normal",
      "visible": true,
      "visibleUntilTick": 99000000,
      "alphaMode": "premultiplied"
    }
  ],
  "instructions": [],
  "frames": [{"index": 0, "timeTick": 0, "sample_points": [{"x": 0, "y": 0}]}],
  "truth": {"tolerance": 0, "frames": []}
}
```

`parentId` 必须引用同表中的节点。成环时加载器返回 `E_CYCLE`，并在
`EngineError.LayerIDs` 中列出参与环的节点 ID。

## v3：studio nested tree / premultiplied / tick

```json
{
  "format": "vp-foreground",
  "schema-version": 3,
  "project-id": "virtual-stage-layer-stack",
  "frame-size": {"width": 64, "height": 48, "channels": 4},
  "timebase": {"unit": "tick", "tick-rate": 90000, "rounding": "half-up"},
  "window-size": 4,
  "tree": [
    {
      "node-id": "grp-plate",
      "kind": "group",
      "name": "Plate",
      "children": [
        {
          "node-id": "plate-backdrop",
          "kind": "layer",
          "parent-id": "grp-plate",
          "layer-id": "plate-backdrop",
          "content": {"sha256": "..."},
          "opacity": 65535,
          "blend-mode": "normal",
          "visibility": {"visible": true, "until-tick": 99000000},
          "alpha": {"mode": "premultiplied"}
        }
      ]
    }
  ],
  "instructions": [],
  "frames": [{"index": 0, "time-tick": 0, "sample_points": [{"x": 0, "y": 0}]}],
  "truth": {"tolerance": 0, "frames": []}
}
```

## 合成指令

指令在 `at` 时刻及之后生效，按数组顺序应用：

| 动作 | v1 字段 | v2/v3 字段 |
| --- | --- | --- |
| `set_opacity` | `opacity` | `opacity` |
| `set_visible` | `enabled` | `visible` |
| `set_visible_until` | `visible_until_ms` 或 `visibleUntilTick`/`until-tick` | 与版本一致 |
| `set_blend_mode` | `blend_mode` | `blendMode`/`blend-mode` |

支持混合模式：`normal`、`add`、`multiply`、`screen`。

## 真值

每个工程内嵌同构 `truth` 对象：

```json
{
  "tolerance": 0,
  "frames": [
    {
      "index": 0,
      "timeTick": 0,
      "layerEnds": {"plate-backdrop": 99000000},
      "samplePixels": [
        {"x": 8, "y": 7, "rgba": [1, 2, 3, 4]}
      ]
    }
  ]
}
```

`timeTick` 与 `layerEnds` 统一写成内部 tick，便于三版本直接比对。真值不调用
领域合成器；`sample/generator.go` 中有独立的整数解析合成路径生成这些像素。

## 舍入与溢出

- 支持 `down`、`up`、`half-up`、`floor`、`ceil`、`nearest`；
- `ms -> tick`：`ms * tickRate / 1000`；
- `tick -> ms`：`tick * 1000 / tickRate`；
- 所有中间乘法先检查 64 位溢出，失败返回 `E_OVF`，包含字段路径与两个操作数；
- alpha 预乘/反预乘均使用 `uint32` 乘积和显式半入舍入，不使用浮点累积。

