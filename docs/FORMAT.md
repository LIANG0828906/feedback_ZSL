# 格式与确定性规范

## 1. 场景二进制容器

所有整数和浮点均为小端序。文件由以下连续部分组成：

| 偏移 | 长度 | 内容 |
|---|---:|---|
| 0 | 8 | magic：`b"VOLSCN1\0"` |
| 8 | 4 | `header_length`，`u32` |
| 12 | 4 | `payload_length`，`u32` |
| 16 | `header_length` | UTF-8、规范化 JSON 场景头 |
| 16 + `header_length` | `payload_length` | 密度值，连续 `<f4` |
| 末尾 | 32 | `SHA-256(header || payload)` |

头部 JSON 必须满足：

- `format == "volrender-scene"`
- `format_version == 1`
- 字段序列化稳定：`sort_keys=True`，紧凑分隔符，不使用 NaN/Inf。

加载器同时校验 magic、精确总长度和尾部校验和。截断、额外尾随数据或校验错误均无法进入几何构建。

## 2. 场景头字段

生成样本位于 `artifacts/demo/sample/scene.volbin`，其结构如下：

```json
{
  "format": "volrender-scene",
  "format_version": 1,
  "scene_id": "smoke_glass_demo",
  "camera": {
    "width": 48,
    "height": 32,
    "K": [[81.6, 0, 24], [0, 81.6, 16], [0, 0, 1]],
    "world_to_camera": [[1, 0, 0, 0], [0, 1, 0, 0], [0, 0, 1, 5], [0, 0, 0, 1]]
  },
  "light": {
    "direction": [0.45, 0.82, -0.55],
    "color": [1.0, 0.92, 0.80],
    "intensity": 6.0
  },
  "background": [0.018, 0.024, 0.040],
  "media": [],
  "density_blocks": []
}
```

### 相机

- `width`、`height` 为正整数，预览上限为 4096。
- `K` 为 3x3 内参，必须有限且满秩。
- `world_to_camera` 为 4x4 外参，必须有限且可逆。
- 像素中心使用 `(x + 0.5, y + 0.5, 1)`；世界射线方向由 `K^-1` 和外参逆矩阵计算后归一化。

### 方向光

- `direction` 表示从介质点指向光源的方向，加载时归一化。
- `color * intensity` 是散射使用的辐射亮度。

### 介质

AABB：

```json
{
  "id": "smoke_plume",
  "type": "aabb",
  "min": [-0.55, -0.55, 1.72],
  "max": [0.55, 0.55, 3.18],
  "density_block": 0,
  "sigma_s": 0.95,
  "sigma_a": 0.18,
  "phase_g": 0.28
}
```

球体：

```json
{
  "id": "glass_shell",
  "type": "sphere",
  "center": [0, 0, 2.45],
  "radius": 1.62,
  "density_block": 1,
  "sigma_s": 0.10,
  "sigma_a": 0.035,
  "phase_g": 0.15
}
```

校验规则：

- 介质 ID 唯一且非空。
- AABB 三个轴均满足 `max > min`；球体半径有限且大于 0。
- `sigma_s >= 0`、`sigma_a >= 0`，且消光 `sigma_s + sigma_a > 0`。
- `-0.95 <= phase_g <= 0.95`。
- 任意两个介质要么空间不相交，要么一个完全包含另一个。部分重叠返回 `E_SCENE_INVALID`，字段为 `media.intervals`。
- 每个介质必须引用存在且 `medium_id` 匹配的密度块；所有密度块都必须被介质引用。

## 3. 密度块

```json
{
  "block_id": 0,
  "medium_id": "smoke_plume",
  "dims": [6, 6, 6],
  "bounds": [min_x, min_y, min_z, max_x, max_y, max_z],
  "offset": 0,
  "count": 216,
  "version": 1
}
```

- `count == dims[0] * dims[1] * dims[2]`。
- `offset` 以浮点元素计数，不是字节数。
- 有效字节区间为 `[offset*4, (offset+count)*4)`，必须完全位于 payload 内。
- 密度值必须有限且非负。
- 内存排布为 `[z, y, x]`，采样时做三线性插值；块外密度为 0。
- `version` 参与 `density_version` 哈希，便于报告与快照核对。

## 4. 渲染配置

```json
{
  "tile_width": 16,
  "tile_height": 16,
  "max_workers": 2,
  "sample_budget": 12000,
  "base_step": 0.22,
  "shadow_samples": 4,
  "shadow_distance": 3.4,
  "max_distance": 14.0,
  "seam_tolerance": 1e-9,
  "degradation_sequence": [
    {"name": "full", "step_scale": 1.0},
    {"name": "balanced", "step_scale": 1.45},
    {"name": "draft", "step_scale": 2.05}
  ],
  "precision_paths": ["float64", "fixed"],
  "fixed_scale": 1048576
}
```

约束：

- 分块尺寸、并行度、预算、步长、阴影参数、距离和容差必须为正/非负且有限。
- 降级阶段名唯一，`step_scale >= 1.0` 且序列非递减。
- `max_workers` 直接作为线程池上限，运行期不得提高。
- 配置解析后为 frozen dataclass；没有运行时改写预算的接口。

## 5. 预算计数

单像素采样量：

```text
samples_per_pixel = view_samples * (1 + shadow_samples)
```

单块预检量是块内全部像素计数之和。调度器在启动线程池前按声明阶段逐级计算：

1. 当前阶段预检量 `<= sample_budget`：接受并记录。
2. 否则进入下一阶段。
3. 所有阶段均超限：进入等待队列轨迹，最终以 `E_BUDGET_EXCEEDED` 拒绝，不启动该块。

实际渲染使用与预检完全相同的遍历生成器；实际用量和预检不一致会直接报错，避免静默改变步长或计数。

## 6. 采样相位与接缝确定性

每个像素的相位只依赖整屏坐标，不依赖分块局部坐标：

```text
phase = frac(
  x * 0.7548776662466927
  + y * 0.5698402909980532
  + segment_index * 0.3819660112501051
  + 0.5
)
```

因此同一 `(x, y)` 无论被单块真值渲染还是被某个分块渲染，初始采样偏移相同。步长会在分段内按局部消光做 0.5x 到 2.0x 的确定性自适应，并量化到 0.25x。

接缝验证使用相同相位、相同阶段映射的整屏单块渲染作为真值。正常实现应得到逐位相等结果，实测颜色/透射率接缝偏差均为 0。
