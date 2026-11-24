# Data and Algorithm Contract

## Region Input

`Region` JSON 的必填结构：

```text
seed?                         generator seed metadata
schema_version
region_id
bounds {min_x,min_y,max_x,max_y}
boundary[]                    closed simple polygon
terrain_types[]               raster type index -> name
terrain_raster {
  cell_size, cols, rows,
  values[]                    row-major terrain indexes
}
zones[] {
  id, name, polygon[],
  terrain_types[],
  density_limit, rarity_mix,
  sample_count_limit?,       0/omitted = no zone-local cap
  depends_on[]
}
no_collect[]? { id,name,polygon[] }
sampling_config {
  grid_spacing, max_samples, min_per_stratum,
  per_thread_sample_buffer, max_parallel,
  density_step, rarity_step,
  density_tolerance, count_tolerance, heatmap_tolerance,
  bias_threshold, heatmap_cell_size, max_points_per_zone?
}
sample_sites[] {
  id,x,y,terrain_type?,
  observed_density,
  rarity_counts? {rarity: count}
}
truth {
  zone_truth {zone_id: {density, rarity_mix}},
  points[] {zone_id,x,y,terrain_type,rarity}
}
```

Validation rules:

- 多边形至少有 3 个顶点和一个闭合顶点，首尾坐标一致。
- 多边形不得有重复顶点、零长边或自交，面积必须为正。
- 地带互不重叠，但允许共享边界。
- 地带和禁采区必须位于区域边界内。
- 栅格 `cols × cell_size == bounds width`，`rows × cell_size == bounds height`。
- 所有栅格类型索引必须映射到 `terrain_types`。
- `sample_sites` 数量不得超过 `sampling_config.max_samples`。
- 每个样本点必须在边界内、地带内、允许地形内，且不能位于禁采区。
- 每个地带必须有隐藏真值，真值清单不得为空。

## Editable State

```text
WorkspaceState {
  zone_params {
    zone_id: {
      density_limit,
      rarity_mix_override?
    }
  },
  no_collect[] {id,name,polygon[]}
}
```

不可变内容哈希排除：

- `truth`
- `no_collect`
- `zones[].density_limit`
- `zones[].rarity_mix`

因此编辑可编辑参数不会伪造“内容变化”，但每个 affected zone 的参数版本会改变。

## Deterministic Sorting

- zones/no-collect/sample-sites 按 ID 排序。
- 依赖列表按 ID 排序。
- 地形层按名称排序。
- 稀有度 map 的所有求和/归一化/取整都按 key 排序。
- 输出拓扑序用稳定的 Kahn 算法，每步按 ID 排序选取 ready 节点。

## Sampling Formula

有效面积：

```text
A_eff(zone) = Σ cell_area,
  cell center ∈ zone
  ∧ terrain(cell) ∈ zone.terrain_types
  ∧ cell center ∉ no_collect
```

层均值与标准误：

```text
mean_s = Σ density_i / n_s
se_s   = sample_stddev_s / sqrt(n_s)
```

地带估计：

```text
density = Σ A_s * mean_s / Σ A_s
se      = sqrt(Σ (A_s * se_s)^2) / Σ A_s
```

最后应用 `density = min(density, density_limit)`。

稀有度：

```text
p_r = count_r / Σ count
quantized_r = largest-remainder(p_r, rarity_step)
residual = Σ |p_r - quantized_r|
```

## Reconstruction Formula

```text
target_count = round(density * A_eff)
```

每个候选坐标使用确定性哈希派生的两个 53-bit 浮点数映射到地带包围盒，再拒绝无效点。每个已接受点按稀有度权重做确定选择。点按 `(zone_id, x, y, id)` 排序。

热力面单元中心：

```text
heat[cell] =
  zone.density   if cell center ∈ zone
                  and terrain allowed
                  and not no_collect
  0              otherwise
```

## Bias Bounds

逐地带上界：

```text
quantization_upper = density_step / 2
se_upper           = observed standard error
rarity_upper       = rarity_step * rarity_count / 2
heat_upper         = density_step / 2
                   + 1.96 * observed standard error
                   + rarity_step
                   + |raw_density - capped_density|
```

四项在拓扑序上累积。`E_BIAS` 在第一个累积值超过 `bias_threshold` 的地带返回。

## SQLite Tables

```sql
snapshot_meta(
  snapshot_id, region_id, base_hash,
  algorithm_version, state_json, state_hash, updated_at
)

zone_checkpoints(
  id, snapshot_id, zone_id, topology_order,
  generation, commit_seq,
  content_hash, param_version, algorithm_version,
  sample_indices, estimate_json,
  point_summary, heat_summary, result_json,
  is_current, created_at
)
```

`zone_checkpoints` 是追加式历史表。`is_current=1` 每个 `(snapshot_id, zone_id)` 最多一行。

## Invariants

- 相同 region 内容 + state 参数 + algorithm version 产生相同样本索引、密度、稀有度、点集合、热力面和拓扑提交序。
- 线程数、worker ID、goroutine 完成顺序和并行/串行模式不改变结果。
- 增量路径只重算失效地带；未失效地带保留原代次。
- 全量比较失败必须返回 `E_DIVERGE` 并识别首个地带与数据项。
- 快照部分版本不匹配必须整体失效。
- 隐藏真值不得进入 baseline hash、采样排序、密度估计、重建、热力面或偏差上界。
