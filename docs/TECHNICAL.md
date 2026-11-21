# 技术说明

## 1. 模块与数据流

| 模块 | 职责 |
| --- | --- |
| `tactical_scene.scene` | 不可变单位、二进制读写、字段校验、内容哈希 |
| `tactical_scene.camera` | 行主序视图/投影矩阵、屏幕坐标、正交/透视相机 |
| `tactical_scene.projection` | 近远平面与视口裁剪、屏幕多边形、最近深度、排除原因 |
| `tactical_scene.index` | 不可变屏幕分桶，桶内单位编号固定排序 |
| `tactical_scene.query` | 预算规划、框选覆盖率、逐单位决策、悬停遮挡 |
| `tactical_scene.versioning` | 原子发布、发布清单、快照句柄、后台编辑互斥 |
| `tactical_scene.trace` | 无时间戳哈希链轨迹、按版本强制策略回放 |
| `tactical_scene.generator` | 确定性多规模/多相机样本与基线 |

一次查询绑定一个不可变 `Scene`。`QueryEngine` 在该场景上建立投影结果，并按网格尺寸惰性建立不可变分桶索引。`SceneEngine` 以 `(version, content_hash, camera_hash, configs)` 缓存 `QueryEngine`，同一相机可复用投影和索引。

## 2. 二进制场景格式

所有整数和小数均为小端序。文件末尾固定为 SHA-256，校验范围是校验和之前的所有字节。

```text
Header 28 bytes:
  magic[8]        = "BFSCENE1"
  format_version  u16 = 1
  header_size     u16 = 28
  scene_version   u64 >= 1
  unit_count      u32
  flags           u32 = 0

Each unit:
  unit_id         u32
  name_len        u16
  shape_type      u8       0=AABB, 1=Capsule
  visible         u8       0/1
  world_transform 16 * f64 row-major affine matrix
  geometry        7 * f64
                  AABB: min.xyz(3), max.xyz(3), unused
                  Capsule: a.xyz(3), b.xyz(3), radius
  name            name_len bytes UTF-8

Footer 32 bytes:
  SHA-256(header + units)
```

加载时校验：

- 魔数、格式版本、头长度、保留 flags、场景版本和记录数。
- 文件校验和、重复单位编号、UTF-8 名称、可见性位。
- 变换所有元素有限、末行为 `[0,0,0,1]`、行列式绝对值不低于 `1e-12`。
- AABB 三轴尺寸均为正；胶囊半径为正。
- 所有记录正好消耗主体，无截断或尾随字段。

内容哈希使用规范化单位 JSON，并包含场景版本，因此同一内容以新版本发布也会得到新哈希。

## 3. 相机、投影与裁剪

相机深度定义为 `-z_view`，屏幕原点在左上。AABB 使用 8 个角点和 12 条边；胶囊使用轴向采样、环向采样和两端纬度采样，并连接环边。局部采样点先经过世界变换，再进入相机空间。

分类顺序：

1. 可见性为假：`HIDDEN`，不进入候选。
2. 所有采样点都在近平面后方：`CAMERA_BEHIND`。
3. 所有采样点都超过远平面：`OUTSIDE_FRUSTUM`。
4. 对每条采样边与近/远平面求交，保留可见点和交点。
5. 投影到屏幕，取二维凸包，再用视口四边做 Sutherland-Hodgman 裁剪。
6. 裁剪后面积为零或全在视口外：`OUTSIDE_FRUSTUM`。
7. 发生近/远平面或视口边界裁剪：`VISIBLE_CROSSES_FRUSTUM`；否则 `VISIBLE_INSIDE`。

覆盖率使用裁剪后的屏幕多边形，因此跨视锥边界的单位分母也是实际可见的屏幕投影面积：

```text
coverage = area(polygon ∩ query_rect) / area(polygon)
```

`nearest_depth` 为可见裁剪几何的最大相机 `z` 对应深度；`farthest_depth` 同理。

## 4. 分桶与确定性

`ScreenBucketIndex` 将每个可框选单位的屏幕包围矩形登记到所有相交桶，桶内和查询结果均按 `unit_id` 排序。框选只遍历查询矩形覆盖的桶，再做包围盒预筛与可选多边形精化。

输出决策按 `unit_id` 排序；相交、覆盖率和面积在固定小数位规范化。候选插入顺序、分桶遍历顺序或场景单位输入顺序不参与最终排序，因此同一查询逐字段一致。

逐单位原因包括：

```text
HIDDEN
CAMERA_BEHIND
OUTSIDE_FRUSTUM
EMPTY_QUERY_RECT
BBOX_NO_OVERLAP
POLYGON_NO_OVERLAP
BELOW_THRESHOLD
INCLUDED
NOT_IN_SELECTION_BUCKETS
```

## 5. 预算规划与降级

规划器只建立一个实际所需的分桶索引，不通过实际查询去试探所有网格。它用保守模型估算索引建立成本、候选几何成本、逐单位决策成本和内存。

声明的精度/成本顺序：

```text
polygon_refine/64x64
polygon_refine/32x32
polygon_refine/16x16
polygon_refine/8x8
polygon_refine/1x1
fine_bucket_bbox/64x64
fine_bucket_bbox/32x32
fine_bucket_bbox/16x16
fine_bucket_bbox/8x8
fine_bucket_bbox/1x1
coarse_full_bbox
```

策略在 `0.95 * budget_ms` 内选择；预算进一步收紧时跳过前序计划，`degraded_stages` 记录被跳过的阶段。所有带索引计划仍超出预算时，若全量包围盒计划可行则选择它；否则返回 `E_BUDGET_EXCEEDED`，详情报价最低计划、估算耗时和内存。实测总耗时超过预算也会返回 `E_BUDGET_EXCEEDED`。

- `polygon_refine`：覆盖率误差上界 `0.0`。
- `fine_bucket_bbox` / `coarse_full_bbox`：用屏幕包围盒近似投影面，使用保守覆盖率误差上界 `1.0`，并在报告中明确策略。
- `total_elapsed_ms` 包含规划、索引建立、候选遍历和决策生成；`projection_prepare` 是相机视图预处理，作为独立阶段报告。
- 索引内存、投影/决策数组均计入内存配额；实测超配额返回 `E_BUDGET_EXCEEDED`。

## 6. 悬停遮挡

悬停先通过点所在桶及邻桶取候选，再以屏幕多边形和 `boundary_epsilon_px` 确认点覆盖。候选按 `(nearest_depth, unit_id)` 排序。

汇报字段：

- `nearest_visible_unit_id`：鼠标点下最前单位。
- `nearest_visible_face_depth`：其最近深度。
- `candidates`：完整深度栈、深度范围、点到边界距离。
- `occluders`：目标投影范围内、深度更近且投影实际相交的单位。
- `target_visible_screen_area`：目标多边形扣除前向遮挡后的扫描线积分面积。
- `visible_area_error_bound`：由目标包围盒宽度和扫描线步长给出的保守误差。

如果最前两项深度差不超过 `depth_epsilon`，或指定目标与其紧邻前向单位无法在精度内区分，返回 `E_OCCLUSION_AMBIGUOUS`，并附候选编号、深度和深度差，不静默选取。

## 7. 版本、发布与并发

`Scene` 不可变；编辑在独立单位结构上完成，由 `SceneRepository.publish` 在短临界区内整体替换当前引用。旧快照仍可被已开始的查询持有，但旧 `SnapshotHandle` 在新版本发布后会返回 `E_STALE_HANDLE`。

发布规则：

- 新版本必须严格大于当前版本。
- 同一版本不能对应两个内容哈希。
- 发布清单采用临时文件加 `os.replace` 原子替换，字段为 `version`、`content_hash`、`unit_count`、`scene_file`。
- 加载/查询的版本或哈希与清单或期望值不符返回 `E_SCENE_STALE`。
- `SceneEngine.submit_edit` 使用单线程执行器；同一场景重复提交立即返回 `E_BUSY`。
- 查询只在校验/取得快照引用时短暂加锁，实际投影、索引和查询不占用发布锁。
- `SceneEngine.max_publish_block_ms` 可用于观测发布临界区时长。

## 8. 轨迹格式与回放

轨迹为 JSON Lines，每个事件包含：

- `type`、`scene_version`、`scene_hash`、完整相机矩阵。
- 框选：屏幕矩形、阈值、策略、分桶、精化深度、阶段候选数、入选单位和覆盖率、误差上界、降级阶段。
- 悬停：屏幕点、目标、最近可见单位/深度、可见面积、候选栈、遮挡列表、深度与边界精度。

记录形如：

```text
{"schema":"tactical-trace-v1","seq":N,"prev_hash":"...","event":{...},"record_hash":"..."}
```

`record_hash = SHA-256(canonical(record without record_hash))`。首条 `prev_hash` 为 64 个零；后续必须等于前一条记录哈希。轨迹不含系统时间戳。加载时验证序号、链、JSON、场景版本和哈希；任何断链或缺失返回 `E_TRACE_INVALID`。

回放时从版本/哈希解析出历史场景，并按记录中的策略、网格、阈值和精度强制复算；结论逐字段不同则报 `E_TRACE_INVALID`。这样回放不受当前机器预算计时波动影响。

## 9. 样本与基线

生成器默认创建：

- `front_000064`、`oblique_000064`
- `front_002048`、`oblique_002048`
- `front_020000`
- `boundary_behind_00240`：包含相机后方、远平面和跨边界单位
- `occlusion_ambiguous_00048`：两个深度差约 `9e-7` 的重叠单位

每个案例目录含：

```text
scene.bfscene
manifest.json
camera.json
baseline.json
trace.jsonl
case.json
```

基线使用最细多边形精化策略，保存版本、哈希、策略、分桶、候选计数、入选集合和覆盖率。`verify` 会以基线策略重算并逐字段比较，再回放轨迹。

## 10. 已知精度边界

- AABB 使用角点/边裁剪后的屏幕凸包；胶囊使用声明采样数的表面点凸包。
- 胶囊投影和遮挡可见面积是带声明误差的离散近似；可提高 `ProjectionConfig` 和 `HoverConfig.area_samples` 获得更高精度。
- `coarse_full_bbox` 与 `fine_bucket_bbox` 使用包围盒覆盖率，误差上界声明为 `1.0`，不会冒充精确多边形结果。
- 投影预处理按“场景版本 + 相机”缓存；相机变化后需要重建该视图的投影。20,000 单位全量投影在当前机器约数秒，框选预算覆盖已准备视图上的规划、索引和查询阶段，并在结果中单列 `projection_prepare`。

## 11. 实际验证记录

验证日期：2026-11-22。环境：Python 3.11.18、NumPy 2.4.6、PySide6 6.12.0、Linux/WSL，NumPy 与 PySide6 均可直接导入。

- `python -m pytest -q`：`26 passed`。
- `python -m tactical_scene verify --suite artifacts/samples`：7 个案例全部通过基线复算和轨迹回放。
- 20000 单位正面样本：`scene_units=20000`、`projected_eligible=19596`、`selected=19498`。
- 20000 单位全量投影预处理约 3 秒；同一版本与相机的查询复用 `QueryEngine` 和已建 index。
- 遮挡歧义案例稳定报告 `E_OCCLUSION_AMBIGUOUS`，两个单位深度差约 `9e-7`，小于声明 `1e-4`。

预算基准（每档 5 次，`artifacts/benchmark.json`）：

| 预算 ms | 结果 | 策略/阶段 | 入选 | 报告耗时 ms | 墙钟中位数 ms | 覆盖率误差上界 |
| ---: | --- | --- | ---: | ---: | ---: | ---: |
| 5 | `E_BUDGET_EXCEEDED` | 最低估算 `coarse_full_bbox` 约 53.6 ms | - | - | - | - |
| 30 | `E_BUDGET_EXCEEDED` | 最低估算仍超预算 | - | - | - | - |
| 60 | `E_BUDGET_EXCEEDED` | 已降级到 `coarse_full_bbox`，实测 73.1 ms | 19498 | 73.1 | - | 1.0 |
| 100 | `E_BUDGET_EXCEEDED` | 已降级到 `fine_bucket_bbox/64x64`，实测 110.2 ms | 19498 | 110.2 | - | 1.0 |
| 150 | 成功 | `fine_bucket_bbox/64x64`，5 个多边形精化阶段被跳过 | 19498 | 66.4 | 68.8 | 1.0 |
| 250 | 成功 | `polygon_refine/64x64` | 19498 | 138.4 | 143.2 | 0.0 |
| 500 | 成功 | `polygon_refine/64x64` | 19498 | 139.1 | 142.2 | 0.0 |

表中 60/100 ms 两档证明“降级后仍超预算必须报错”的路径；150 ms 档证明预算收紧时包围盒降级路径可复现；250/500 ms 档证明多边形精化结果与误差上界为 0。不同机器和当前 CPU 负载会影响毫秒数值，但策略选择、候选计数、入选集合与覆盖率在固定声明配置下保持一致。
