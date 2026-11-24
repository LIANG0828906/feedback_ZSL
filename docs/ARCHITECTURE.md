# Block-minimal shading mesh 实现说明

本仓库把固定种子样本生成、schema 校验、单块自适应三角剖分、边界对齐归因、
依赖失效、并行调度和 SQLite 版本检查点串成一条本地可复现链路。

## 处理管线

1. `GenerateSample(seed)` 生成 2×2 共 4 块地形。相邻块共享边的栅格长度一致，
   但宽高、起伏特征、采样预算和容限不同。
2. `ParseProject` / `ValidateProject` 校验字段、路径、块编号、覆盖范围、
   采样预算、容限、共享边长度和约束引用。
3. `PlanAlignment` 对每条边做确定性一维误差细分，并合并两侧结点集合，
   生成双方逐点一致的强制顶点。
4. `BuildBlockMesh` 使用约束 Delaunay/Bowyer–Watson 增量剖分：
   - 四角、真值点和对齐结点为强制顶点；
   - 按当前三角形高程插值误差选择最差点；
   - 相同误差按 `(grid_y, grid_x)` 打破并列；
   - 顶点按 `(grid_y, grid_x)` 排序，三角形按顶点序号字典序排序；
   - 输出逐三角形最大高程误差及位置。
5. `ExtractBoundarySide` 提取共享边点列和相邻三角形法线；
   `AttributeBoundary` 输出最大折角、超限位置、残差和归因证据。
6. `DependencyGraph` 记录每条共享边的两条双向依赖。编辑一块时，
   `PlanInvalidation` 只失效该块和直接重对齐邻块。
7. `Service` 用 `BudgetMeter` 限制并行度、单线程顶点缓冲和总内存预算；
   成功结果按块编号稳定提交，SQLite 事务串行写入。

## 归因类别

- `ok`：折角未超过声明阈值。
- `undersample`：边界位置/顺序一致，但一侧或两侧高程误差超过容限，且折角超限。
- `alignment_mismatch`：点序反转、索引错位或直接配对残差超过严格对齐精度。
- `mixed`：同时存在对齐错位和欠采样证据。
- `natural_sharp_fold`：折角超限，但记录的高程误差仍在容限内。
- `alignment_unavailable`：最近点也超过对齐缓冲；返回 `E_ALIGN`，错误中带
  `block_a`、`block_b` 和 `measured_fold_angle_deg`。

生成器中的 `fault-undersample`、`fault-alignment-order` 和
`fault-align-unavailable` 是三个可核对的断裂样例。

## 确定性与一致性

- 所有输出排序规则固定，map 只用于查找，不决定顶点或三角形顺序。
- 同一工程、参数版本和算法版本下，串行、并行、草稿增量、断点恢复和全量路径
  使用同一个纯函数 `BuildBlockMesh`。
- `DiffMesh` / `CompareMeshMaps` 返回 `E_DIFF`，首个定位包含块编号、
  顶点序号或三角形序号以及字段名。
- 增量编辑后重新计算全局轻量对齐结点计划，但未失效块直接使用 SQLite/内存缓存；
  因此不会重剖无关块，同时强制结点与全量编译完全相同。

## SQLite 检查点

使用 vendored `modernc.org/sqlite`，设置 `CGO_ENABLED=0`，不依赖 gcc/mingw。

主要表：

- `snapshots`：快照、工程、算法版本和剖分参数版本。
- `blocks`：每块网格 JSON、顶点摘要、三角形摘要、内容哈希、版本和时间。
- `dependencies`：块—邻块—边界双向依赖。
- `commit_sequence`：稳定块序号；`commit_events` 保留追加式提交事件。
- `boundary_stats`：折角最大值、类别和完整归因 JSON。

每块的网格、摘要、依赖、边界统计和版本在一个事务中提交。服务层
`commitMu`、Store 写锁和 `SetMaxOpenConns(1)` 保证写入不交错。
快照算法/参数版本不匹配时，整个快照被删除并全量重剖。

## 本地 HTTP 诊断

`cmd/terrain-server` 默认监听 `127.0.0.1:18080`：

- `/`：Canvas 着色、线框、折角标注、失效传播、预算和排队面板。
- `GET /api/state`：完整状态 JSON。
- `POST /api/blocks/{n}/edit`：编辑高程、采样预算或容限并触发增量剖分。
- `POST /api/blocks/{n}/retry`：重试失败块。
- `POST /api/blocks/{n}/fault`：注入一次性异常以验证隔离。
- `GET /api/export`：导出工程、网格、边界报告和清单。

单块 panic/错误会被调度器捕获并只标记该块失败；成功块的 SQLite 事务和缓存
不被污染。进程终止后重新打开服务，只恢复哈希、摘要和版本均匹配的块。
