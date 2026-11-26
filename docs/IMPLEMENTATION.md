# 实现说明

## 端到端数据流

```text
固定 seed
  -> sample.Generate
       笔画胶囊栅格化 -> 参考掩码 -> 连通域/方向真值
       参考掩码 -> 确定性内缩、外溢、打洞 -> 候选掩码
       图标前景/近色渐变/半透明描边/噪声 -> icon.png
  -> schema.Export v1/v2/v3
  -> schema.Load -> 内部绝对坐标、万分比权重、SourceVersion
  -> engine.Run
       块规划 -> 内容哈希/参与笔/上游键 -> 缓存命中或并行栅格化
       alpha-over 积算 -> 全图拼装 -> 连通域/方向核验
       SQLite 笔画事务 -> 块事务 -> 运行摘要
  -> web.Server -> HTML/JSON/PNG
```

## 内部模型

`model.Stroke` 的规范表示：

- 绝对像素坐标 `X0/Y0/X1/Y1`。
- 万分比覆盖权重 `Weight`，合法范围 `0..10000`。
- 硬度 `0..100`、半径、工序优先级、合成次序、笔序号、方向、组标识。
- `SourceVersion` 保留加载来源。

`model.OrderedStrokes` 是唯一排序入口：

```text
(priority ASC, compositeOrder ASC, seq ASC, id ASC)
```

所有块参与笔列表、提交顺序和依赖图均使用该排序，声明输入切片的原始顺序不参与裁决。

## Schema 换算

### 坐标

v2 相对坐标以前一笔终点为锚点。加载时：

```text
absolute_start = relative_start + previous_absolute_end
absolute_end   = relative_end   + previous_absolute_end
previous_absolute_end = absolute_end
```

导出 v2 时反向使用 `safeSub(absolute, anchor)`。所有加减法都检查 `int64` 溢出；失败返回 `E_OVF`，错误中带 `field/path/objectId/operandA/operandB`。

### 权重

v1 千分比与内部万分比互转：

```text
up:   canonical = permille * 10
down: quotient, remainder = divmod(permyriad, 10)
      permille = quotient + (remainder*2 >= 10 ? 1 : 0)
```

采用显式 `nearest_half_up`。每一个换算点写入 `ConversionTrace`：字段、对象、源/目标版本、两个操作数、商、余数、结果、舍入方向和残差。`ConversionReport.Finish` 对残差序列做 SHA-256，作为缓存键中的“换算残差摘要”。

不可表达权重（非 10 的倍数）在 v1 中必然有残差。工程保证的是可表达格点上的严格往返；样本权重均为 10 的倍数，因此逐笔坐标、序号、权重完全一致。

## 默认补齐与降级

v1 加载时默认补齐：

- `hardness = 70`，来源 `schema:v1.default.hardness`。
- `direction = unknown`，来源 `schema:v1.default.direction`。
- `composite_order = 1`，来源 `schema:v1.default.compositeOrder`。

每条痕迹包含 `OldValue=<missing>`、`DefaultValue`、`DefaultSource` 和 `Handling=fill_default`。v2 缺失方向/合成次序时也按同样方式记录。

导出低版本时：

- 方向为 `E_LOSSY`，`handling=reset_to_unknown`。
- 分组为 `E_LOSSY`，`handling=flatten_group`。
- v1 硬度和非默认合成次序为 `E_LOSSY`，分别回退默认。
- 工件生成后立即调用对应版本 `Load` 自检身份、尺寸和可读性。

## 笔刷覆盖

对笔画线段上的最近点使用 1024 定点参数化，避免浮点累积：

```text
t = clamp(dot(P-S0, S1-S0) * 1024 / |S1-S0|², 0, 1024)
nearest = S0 + (S1-S0)*t/1024
d² = |P-nearest|²
```

覆盖率：

```text
inner = radius * hardness / 100
alphaWeight = nearest_half_up(weight * 255 / 10000)

d² <= inner²                  -> alphaWeight
d² >= radius²                 -> 0
inner < d < radius            -> alphaWeight * (radius-d)*256/(radius-inner) / 256
```

像素计算只读绝对坐标和声明参数，因此整图覆盖与任意分块切片的对应像素相同。`raster_test.go` 对整图、16×16 分块拼装、24 路并发哈希做一致性检查。

alpha-over：

```text
out = src + round(base*(255-src)/255)
```

## 块缓存与依赖

块输入内容哈希包含候选掩码块字节、块坐标和画布尺寸。逐笔上游键：

```text
componentKey = SHA256(
  "stroke-component",
  blockInputHash,
  strokeId,
  stroke.RasterSignature(),
  blockOrderVersion,
  conversionVersion,
  residualDigest,
  algorithmVersion
)
```

`RasterSignature` 只包含起止坐标、半径、硬度、权重。方向、分组、来源版本等非像素字段不触发覆盖重算。

块内 `blockOrderVersion` 是实际参与笔按唯一顺序排列后的标识序列哈希。块输出键再纳入全部 `componentKey`、块输入哈希、换算参数、残差、算法版本。

依赖图节点：

```text
conversion:<stroke> -> stroke:<stroke> -> block:<index>
order:composite     -> block:<index>
block:<index>       -> verify:topology -> verify:direction
```

依赖图随运行结果返回，页面以分层 SVG 展示。

## 精确失效

`ApplyStrokeEdit` 对旧/新工程分别计算：

1. 覆盖几何、半径、硬度、权重变化：旧包围框块集合 ∪ 新包围框块集合。
2. 优先级/合成次序/序号变化：分别建立编辑前后全序，检查所有相对次序翻转的笔画对；只有两笔包围框相交时才把交集包围框覆盖的块加入失效集。
3. 方向、分组等非像素字段变化：块不失效，拓扑/方向下游仍重算。

引擎在 SQLite 事务中删除失效块检查点，再运行增量路径；未失效块从内存或持久化结果复用。`InvalidationReport` 返回笔画标识、受影响笔、块号、原因、复用块与下游节点。

## 调度预算

`runScheduler` 使用：

- `MaxParallel` 个工作 goroutine。
- `QueueCapacity` 个待处理块槽位。
- 像素令牌池，容量为 `MaxParallel * PixelBufferPerWorker`；每块按像素面积获取令牌。
- 每线程像素缓冲至少覆盖一个完整块，否则配置返回 `E_PARA`。

队列满时提交方阻塞，并记录 `queue_budget` 与等待微秒；像素令牌不足时记录 `pixel_buffer_budget`。计算完成顺序只影响收集，不影响按块号排序后的积算、拓扑、方向与提交序列。

## 三路径与比较

- `ModeFull`：不读取块缓存，所有块重算。
- `ModeIncremental`：同引擎读取内存缓存与 SQLite 块。
- `ModeResume`：新引擎首次 `Restore`，从 SQLite 已提交块恢复。

`CompareRuns` 顺序：

1. 工程身份与分块提交序列。
2. 每个块缓存键、掩码长度、逐像素值。
3. 全图掩码。
4. 合成次序版本。
5. 域统计、边界段与方向摘要。

发现像素差异时返回块号和绝对坐标。

## SQLite 事务模型

`Store` 使用单个连接、`sync.Mutex` 和 SQLite 事务串行化写入。`CommitStroke` 在一个事务中：

1. Upsert 项目签章。
2. Upsert 笔画状态。
3. 删除该笔旧覆盖分量。
4. 按调用方给出的稳定块序遍历插入新分量。

`CommitBlock` 另以事务提交块掩码、摘要、参与笔、缓存键、版本与提交序号。数据库启用 WAL 与 `synchronous=FULL`。

`Restore` 校验项目内容哈希、尺寸、算法/换算版本、笔画版本、覆盖摘要、块字节摘要与块版本。任何失败清除该项目全部检查点并记录 `invalidation_log`。因此强杀进程后只会看到完整提交的项目状态。

## 拓扑与方向

- 对前景阈值 128 的掩码做 4 邻接标记。
- 域按 `(minY,minX)` 稳定排序并编号 `d001...`。
- 孔洞：包围框内不与边界连通的背景分量数量。
- 环宽：从域边界多源 BFS 的距离均值乘 2。
- 每条前景边界边产生一个外法向单位向量，并查找最近笔画。
- 一致性类别通过归一化点积比较法向与笔画行进方向，容差边界为 `|cos| <= 0.5` 视为垂直。
- 域方向取出现次数最多的类别；平票按 `perpendicular > co_directional > counter_directional > unknown` 固定裁决。

真值由参考掩码与同一几何笔画独立生成，编辑后可自然出现 `E_TOP`；三路径比较与真值容差是两条独立验收线。
