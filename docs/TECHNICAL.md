# OcclusionLab 技术说明

## 1. 确定性工程

`cmd/scene-gen` 以 `(seed, quantization, tolerance)` 为输入生成固定布局：

- 15 个流式分块，5 列 × 3 行。
- 两段墙体和一个中央柱体作为遮挡体。
- 6 个逻辑时刻，相机位置、基向量、近远平面、视锥切线与屏幕参数均为整型。
- 每窗 15 个真值分块和 15 个已知锚点投影，共 90 个投影校验点。

所有整数和真值都写入 `project.json`、`world.json`、`truth.json`、`manifest.json`。相同参数重复生成得到相同 `ContentHash`。生成器不读取操作系统区域设置、时间或随机环境；伪随机抖动只来自固定 `seed`。

真值路径使用 `math/big.Rat`：

```text
camera_x = dot(world - position, right) / 2^basis_bits
camera_y = dot(world - position, up) / 2^basis_bits
depth    = dot(world - position, forward) / 2^basis_bits
screen_x = center_x + focal_x * camera_x / depth
screen_y = center_y - focal_y * camera_y / depth
```

真值可见性定义为“分块锚点在屏幕内、深度在近远平面之间，且不被任一引用遮挡体的精确投影矩形覆盖”。遮挡体必须严格比锚点更近。该规则固定写在 `truth.json` 的 `occlusion_rule` 中。

## 2. 定点评估器

### 2.1 操作与残差

`internal/fixed` 使用 `big.Int` 做精确整数乘除，再按声明的 2 次幂位宽舍入。每一步输出：

- 操作名。
- 精确分子、除数。
- 定点商。
- 整数余数。
- 输入/输出位宽。
- 余数换算后的屏幕量子残差。

虽然内部用 `big.Int` 避免中间乘法溢出，结果始终是声明精度的整型商，不使用浮点参与几何判定。

### 2.2 包围体视锥

`projectBounds` 投影包围体 8 个顶点，累计每个顶点的屏幕除法残差。`planeMargins` 对每个顶点计算 6 个平面余量：

- 所有顶点都在某平面外侧：包围体 `outside`。
- 所有顶点都在所有平面内侧：包围体 `inside`。
- 任一顶点落入平面误差带：包围体 `boundary`。
- 其他：`intersecting`。

锚点另有 `AnchorStatus`，用于与生成器锚点真值对齐并输出三态结果。界面与导出都同时保留包围体状态和锚点状态。

### 2.3 投影误差推导

设：

```text
W = 2^-world_frac_bits
B = 2^-basis_frac_bits
R = |anchor - camera|_1 + bbox_span/2
S = |right|_1 (或 up/forward 对应基向量)
```

点积链路误差：

```text
E_dot = R * B/2 + S * W/2 + W*B/2 + 3*W + 舍入残差
```

其中 `3*W` 是显式 `dot_product_terms` 乘积步骤的保守余量。相机 x/z 误差相同上界，屏幕误差：

```text
r       = camera_x / depth
den     = depth - E_dot
E_ratio = (E_dot + r*E_dot) / den
focal_q = 量化焦距
E_focal = focal_q * tan_step / tan_q
E_px    = focal_q * E_ratio + r * E_focal
        + screen_step/2
        + screen_div_residual
        + projection_ladder_ops
```

所有量均由报告中的 observed step、bbox span、camera distance、focal、tan 和残差计算，不读取生成器真值。

### 2.4 遮挡栅格

对当前分块引用的遮挡体：

1. 投影遮挡体 8 个顶点，得到屏幕包围矩形和深度范围。
2. 将矩形映射到 `raster_resolution × raster_resolution` 栅格。
3. 每个单元保存最近遮挡体的深度范围与 ID。
4. 以锚点投影为中心，查询半径 `projection_error + one_cell` 内的所有候选单元。
5. 分类候选单元：
   - 无覆盖。
   - 更近且深度明确：遮挡。
   - 更远且深度明确：可见。
   - 深度落入 `depth_compare_error`：不确定。

最终三态：

- 所有候选遮挡，且无可见/不确定候选：`occluded`。
- 所有候选可见，且无遮挡/不确定候选：`visible`。
- 其他：`uncertain`。

`uncertain` 同时输出可见性上下界 `[0, 65536]`、深度上下界、候选单元数、覆盖候选数、参考遮挡体和原因。

### 2.5 累计上界

```text
depth_px = focal * depth_error / anchor_depth
plane_px = focal * plane_error / anchor_depth
upper    = projection_error + raster_cell
         + depth_px + plane_px + screen_step
```

该 `upper` 仅用于诊断，不与真值误差做比较；`cmd/verify` 另行检查真实观测误差不超过报告上界。

## 3. 流式状态机

```text
pending -> culling -> loading -> committed
   |          |          |
   +----------+----------+--> failed -> pending
```

- `pending`：窗口等待剔除。
- `culling`：工作池执行分块投影与遮挡查询。
- `loading`：计算驻留目标、加载/卸载集合与成本。
- `committed`：冻结结果并持久化。
- `failed`：单窗失败；后续窗口不越过失败窗口提交，避免检查点乱序。

非法迁移调用 `Window.Transition` 返回 `E_STATE`，不修改 `State` 或历史。

驻留目标为 `visible ∪ uncertain`。加载和卸载均按分块索引排序，保证三路径序列一致。超过 `max_resident` 或 `load_cost_budget` 时窗口失败，不静默截断。

相机平移与栅格/容差调整只允许作用于未提交窗口。已提交窗口的相机消费偏移、结果哈希和驻留集合不允许被覆盖。未提交相机覆盖属于运行期诊断输入，不写回生成器工程文件；重启时仅从检查点恢复已提交窗口。

## 4. 并行与预算

`Pool.EvaluateChunks` 为每个分块启动结果槽，通过容量为 `max_workers` 的 token channel 限流：

1. 尝试立即获取 token。
2. 失败时写入 `worker_slot_wait` 事件，说明任务将阻塞排队而不会丢弃。
3. 阻塞等待 token 或 context 取消。
4. 每线程查询使用独立栅格缓冲，缓冲上限由 `raster_buffer_cells` 检查。
5. 所有结果按分块编号排序后进入窗口结果，消除调度顺序影响。

## 5. SQLite 检查点

驱动：`modernc.org/sqlite`，纯 Go，无需 CGO。

连接设置：

```text
MaxOpenConns(1)
busy_timeout=5000
journal_mode=WAL
synchronous=FULL
```

每次 `AppendSnapshot`：

1. 获取进程内互斥锁。
2. 校验窗口编号、场景哈希、结果哈希、可见集合摘要、参数版本。
3. 开始 SQLite 事务，检查 `checkpoint_meta` 的整体版本。
4. 查询已有窗口数和最大索引，期望新窗口号等于窗口数。
5. 取前序 `content_hash`，计算链式哈希。
6. 插入并提交。

加载 `LoadAll` 逐行校验：

- 窗口 0..N-1 连续。
- `scene_hash` 与头一致。
- `previous_hash` 与上一行 `content_hash` 一致。
- 结果 JSON 可解码。
- `ResultHash` 与冻结帧重新计算结果一致。
- `VisibleDigest` 与可见集合一致。
- 参数哈希/版本与帧字段一致。

任一失败都会在事务中删除全部检查点并返回 `Invalidated=true`，由服务执行全量重放。

## 6. 三路径

`internal/verify` 在独立临时 SQLite 上执行：

1. **realtime**：顺序提交并每窗写检查点。
2. **resume**：提交前 2 窗，关闭数据库，重新打开、校验、恢复，再继续。
3. **replay**：全新引擎、不写检查点，一次跑完。

比较内容：

- `ResultHash`
- 可见集合、不确定集合
- 驻留集合
- 加载/卸载序列
- 四项误差上界与累计上界

`ResultHash` 对完整冻结窗口结果（含逐分块证据、参考遮挡体、误差字段）做 canonical SHA-256，因此任意数值差异都会导致路径比对失败。

## 7. 界面状态

前端是原生 JavaScript、CSS 与 Canvas，无 npm 依赖：

- `sceneCanvas`：世界俯视分块、视锥体、遮挡体、驻留描边。
- `screenCanvas`：当前窗口的屏幕空间分块和遮挡体投影。
- `errorCanvas`：逐窗投影/覆盖/累计上界曲线。
- 窗口矩阵：点击选择窗口。
- 分块证据表：状态、锚点像素、参考遮挡体、误差。
- 槽位面板：驻留与新增加载。
- 三路径卡片：路径耗时、检查点数量、差异数、所有验收检查项。
- 失效窗口、资源排队、日志。

Wails 与 HTTP 模式共用 `internal/workbench.Service`。前端在 Wails 下调用 `window.go.main.App.*`，在 HTTP 下调用 `/api/*`，返回的数据结构与状态完全一致。
