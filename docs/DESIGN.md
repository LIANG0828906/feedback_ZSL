# Design

## 1. Module boundaries

| 模块 | 职责 |
| --- | --- |
| `scene` | 小端二进制解析、实体索引/边界校验、规范编码 |
| `region` | 区域状态、内容哈希、不可变发布快照、句柄生命周期 |
| `bvh` | 确定性 AABB BVH 与射线粗筛 |
| `camera` | 相机校验、屏幕反投影、点投影 |
| `query` | 单位/掩体/地面最近命中、框选多单位、快照完整性复核 |
| `placement` | 多边形拓扑校验、网格可通行、单位/掩体冲突 |
| `trace` | 位精确轨迹、SHA-256 哈希链、版本快照回放 |
| `injection` | 公共输入入口的故障构造，不进入查询/版本实现 |
| `sample` | 确定性场景、操作序列、基线和实验执行器 |

## 2. Scene format

文件采用 little-endian、显式长度和固定魔数 `BTSSCN01`。格式版本为 `1`。

```text
magic[8] "BTSSCN01"
u32 format_version
u32 flags
u32 grid_width, grid_levels, grid_depth
f32 cell_size
f32 origin[3]
u32 walkable_count
u8  walkable[walkable_count]
u32 unit_count
repeat unit_count:
  u32 id
  u32 record_index
  u8  shape (0=AABB, 1=Capsule), u8 reserved[3]
  AABB:    f32 min[3], f32 max[3]
  Capsule: f32 a[3], f32 b[3], f32 radius
u32 cover_count
repeat cover_count:
  u32 id
  u32 record_index
  f32 a[3], f32 b[3], f32 c[3]
u32 footer 0x4E4F5442
```

加载器拒绝非有限值、零尺寸 AABB、零半径/零长度胶囊、退化三角形、重复 ID、越界网格标记、非 0/1 标记、非连续记录索引、截断内容和尾随字节。错误定位格式如 `scene.unit.aabb`、`scene.cover.index`，并携带 `EntityRef`。

## 3. Region snapshot and versioning

`RegionState` 包含：

- 单调递增 `version`；
- 3D 网格及可通行标记；
- 单位位置与包围体；
- 掩体三角形与 `present` 状态；
- 规范内容哈希 `content_hash`。

规范哈希覆盖版本和全部区域内容，采用固定字段顺序、little-endian 和原始浮点位模式。`PublishedRegion` 在状态之外冻结：

- 单位 BVH；
- 掩体 BVH；
- 加速结构哈希。

发布流程：

1. 编辑线程复制当前状态并应用一次变更。
2. 版本必须恰好为当前版本 + 1。
3. 锁外校验几何、计算内容哈希、构建两棵 BVH、计算加速结构哈希。
4. 短暂持有发布锁，整体替换 `shared_ptr<const PublishedRegion>`。
5. 新快照进入版本归档，供轨迹回放使用。

查询线程通过 `SnapshotHandle` 读取。句柄在注册表中有效时返回同一个不可变 `shared_ptr`；显式 `release` 后再次读取返回 `E_SNAPSHOT_STALE`。旧句柄不会因新版本发布而失效，因此进行中的查询可以完整结束，但永远看不到半更新状态。

## 4. Pick pipeline

1. 解析句柄并检查版本/内容哈希/加速结构哈希。
2. 校验期望版本与期望哈希，不匹配返回 `E_REGION_STALE`。
3. 校验相机位置、朝向、上向量、FOV、宽高比、视口、近远平面。
4. 检查屏幕坐标有限且位于视口闭区间。
5. 计算正交基：

```text
forward = normalize(camera.forward)
right   = normalize(cross(forward, camera.up))
up      = normalize(cross(right, forward))
ndcX    = 2*sx/width - 1
ndcY    = 1 - 2*sy/height
dir     = normalize(forward + right*ndcX*aspect*tan(fov/2) + up*ndcY*tan(fov/2))
```

6. 在同一个快照上查询单位 BVH、掩体 BVH 和有限地面平面。
7. 使用精确浮点 `t` 选择最近命中；完全相等时依次按 `unit < cover < ground`、实体 ID 决定。

相交算法：

- AABB：三轴 slab，返回进入面法向；原点位于盒内时 `t=0`。
- 胶囊：有限圆柱侧面二次方程 + 两端半球限定。
- 掩体：Moller-Trumbore 双面三角形，法向翻转为朝向射线。
- 地面：`y=ground_y` 平面，且交点必须位于网格 XZ 范围。

`PickResult` 带区域版本、内容哈希、实际射线、命中实体、点、法向和距离。轨迹使用这些值的位模式，不使用重新格式化的小数。

## 5. Box selection

框选使用同一快照和同一相机校验。每个单位的世界包围盒 8 个角点投影到屏幕；至少一个角点位于近平面之前时，计算屏幕包围矩形，与拖拽矩形相交则选中。结果按单位 ID 排序并带版本/哈希。

## 6. Draft placement

草拟区域是多边形顶点数组，当前放置平面固定为 `grid.ground_y()`。

拓扑校验顺序：

1. 顶点数 `3..256`；
2. 所有坐标有限；
3. 全部顶点位于地面平面；
4. 全部顶点位于网格 XZ 边界；
5. 无零长度边；
6. 非零面积；
7. 非相邻边无交点（包括端点接触/共线重叠）。

拓扑错误返回 `E_REGION_INVALID`，`vertex_index`/`edge_index` 和 `injection_point` 定位失败位置。

规则校验：

1. 计算多边形 AABB 覆盖的网格单元。
2. 对与多边形相交的每个单元检查 `walkable_at(x, 0, z)`。
3. AABB 单位按 XZ 投影矩形、胶囊按 XZ 胶囊投影检查占地重叠。
4. 存在的掩体三角形按 XZ 投影检查与多边形的包含、边相交和顶点落点。

规则拒绝返回 `Outcome::success`，但 `legal=false`，并给出 `PlacementReason`、`failed_vertex`、失败单元和按类型/ID 排序的冲突实体。

## 7. Trace format

轨迹为确定性行日志：

```text
PICK|seq=...|version=...|region_hash=...|prev_hash=...|screen=...|camera=...|ray=...|hit=...|point=...|normal=...|distance=...|status=...|error=...|injection=...|record_hash=...
BOX|...|rect=...|camera=...|units=...|...
DRAFT|...|vertices=...|legal=...|reason=...|failed_vertex=...|failed_cell=...|conflicts=...|...
```

浮点字段均为逗号分隔的 8 位十六进制 IEEE-754 位模式。`record_hash = SHA256(line_without_record_hash)`；`prev_hash` 必须等于上一条 `record_hash`，首条为 64 个零。加载器要求：

- `record_hash` 是最后字段且长度 64；
- `seq` 从 0 连续；
- 每行存在非零 `version`、64 位 `region_hash`、`prev_hash`、`record_hash`；
- 拒绝任何 `time`/`timestamp` 键；
- 任何哈希不符或断链返回 `E_TRACE_INVALID`，并带行号定位。

回放时按版本和内容哈希读取 `.region` 快照，重建查询，逐位比较射线、命中实体、命中点、法向、距离、框选 ID 或草拟合法性与冲突。

## 8. Fault injection

`InjectionGate` 是独立公共边界层：

| 注入 | 返回码 | 定位 |
| --- | --- | --- |
| 退化单位包围体 | `E_SCENE_INVALID` | `injection.degenerate_bounds` + 单位实体 |
| 零长度射线 | `E_RAY_INVALID` | `injection.zero_length_ray.*` |
| 越界屏幕坐标 | `E_RAY_INVALID` | `injection.out_of_bounds_screen` |
| 自交草拟区域 | `E_REGION_INVALID` | `injection.self_intersecting_draft` |
| 损坏区域快照 | `E_SNAPSHOT_STALE` | `injection.corrupt_snapshot` |

门层只构造输入/临时快照，查询和版本模块不读取 `InjectionKind`。所有故障在返回任何命中结果前被拦截。

## 9. Determinism

- 编译使用 `-ffp-contract=off -fno-fast-math` 或 MSVC `/fp:precise`。
- 场景实体与冲突列表稳定排序。
- BVH 使用确定性稳定排序和固定叶子大小。
- 查询不使用并行归约，不使用随机数。
- 样本 PRNG 为 SplitMix64，浮点由 24 位整数映射产生。
- 日志不使用时间戳、局部化数字格式或实现相关 `%f` 输出。

