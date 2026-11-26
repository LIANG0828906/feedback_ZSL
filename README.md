# VPF Stack — 虚拟制片前景层栈合成迭代核

本地、确定性、仅依赖 Go 标准库的虚拟制片前景层栈领域组件与诊断应用。模块路径为 `vpfstack`，Go 版本声明为 1.21；已在 `go1.22.12 linux/amd64` 与 WSL Ubuntu 场景验证。

原始任务书保留在 `docs/task-spec.md`，实际命令结果记录在 `VALIDATION.md`。

## 快速验收

```bash
go test ./...
go test -race ./...

# 生成固定种子的三份工程与解析真值
go run ./cmd/vpfd generate

# 运行加载互操作、E_* 诊断、增量失效、三路径一致性与 SQLite 检查点验证
go run ./cmd/vpfd verify

# 本地诊断页面
go run ./cmd/vpfd serve -addr 127.0.0.1:18080
# 浏览器打开 http://127.0.0.1:18080
```

`verify` 默认写出：

- `artifacts/verification.json`：完整机器可读报告；
- `artifacts/vpf-checkpoints.sqlite`：可被标准 SQLite 读取的增量检查点库。

无 cgo 或无 `libsqlite3.so.0` 时，根包的 `database/sql` 驱动无关仓储仍可使用任意外部 SQLite 驱动；测试会自动跳过内置桥测试，文件/内存检查点与全部合成验证不受影响。

## 三版本确定性样本

固定种子：`20261127`。生成器位于 `generator.go`，产物位于 `samples/seed-20261127/`。

| 版本 | 字段风格 | 层级/可见性 | 透明通道 | 时间单位 |
| --- | --- | --- | --- | --- |
| v1 | `snake_case`，如 `layer_id`、`opacity_pct` | 平坦层表 + `visible_flag`，组层级降级为按序平坦化 | `color_rgba` 为预乘值 | `at_tick`，1 tick = 0.5 ms |
| v2 | 短字段，如 `node_id`、`opacity_bp` | 带 `parent_id` 的平坦节点表 + `disabled` 反向可见性 | `rgba` 为非预乘/straight alpha | `time_ms` |
| v3 | `camelCase`，如 `layerId`、`opacityPermille` | 递归 `layerTree.children` + `visible` 继承 | `color` 为 `ASSOCIATED`/预乘值 | `timeUs` |

加载入口：`vpf.LoadProject([]byte)`。加载后得到统一 `Project`：

- `Layers`：层定义；
- `Root`：保留组树；
- `Resolved`：深度优先、祖先可见性递归后的逐层结果与来源路径；
- `Stack`：实际参与合成的固定次序，文档顺序声明为自下而上；
- `Traces.Fields`：逐字段命名、重命名、默认值补齐、单位/透明约定转换痕迹；
- `Traces.Truncations`：整型除法余数与截断位置。

未知版本返回 `E_SCHEMA` 与 `$.version`/`$.format_version` 路径及来源版本；必填字段缺失返回包含 JSON 字段路径和来源版本的 `*vpf.Error`。

## 版本转换与 `E_LOSSY`

- `ExportProject(project, version)` 导出指定版本，并返回 `*LossReport`。
- v1 无法表达组树：处置为 `flatten_depth_first_and_bake_ancestor_visibility`。
- v1/v3 的预乘通道由内部 straight alpha 经整数 `floor` 转换：处置为 `canonical_straight_to_*_premultiplied_floor`。
- 非整除的不透明度/时间/通道换算会进入损失或截断记录。
- 导出的 v1/v2/v3 JSON 均立即被对应加载器读回；测试覆盖 v3 → v1 → v3 的颜色、透明度与时刻完全一致。

## 整数单位与合成

内部时间统一为纳秒，内部颜色统一为 straight RGBA8 与 basis-point 透明度。

- tick ↔ ns、ms ↔ ns、µs ↔ ns 全部使用 `int64` 检查乘法溢出；溢出为 `E_OVF`，包含字段路径与两个操作数。
- 预乘/非预乘通道使用 `c*a/255` 或 `p*255/a` 的整数除法，并记录非零余数。
- 距离和 gamma 线性化使用纯整数平方根，不使用浮点累加。
- 涟漪权重公式：
  `falloff = radius*10000 - decayBP*floor(sqrt(dx²+dy²))`；
  `weightBP = amplitudeBP * max(0, falloff) / (radius*10000)`。
- 混合模式编码：`normal`、`add`、`multiply`，在线性 Q16 空间确定求值。
- 帧尺寸和 RGBA 通道数严格来自 `Project.Width/Height/Channels`。同帧、同块、同参数重复渲染摘要一致；并行度、完成顺序和缓存命中不改变像素。

## 依赖图与精确失效

`BuildDependencyGraph(project, frame)` 显式建立参数、可见性、alpha 约定、涟漪源、图层块和最终块之间的依赖。

块缓存键由 genesis/算法版本/帧/块边界、上游键、图层 ID、图层内容哈希、图层参数版本、透明度/混合模式以及影响该块的涟漪源哈希/源参数版本串联：

```text
H("layer-chunk-v2" | upstream | layerID | contentHash | paramVersion | extras)
```

编辑操作包括 `SetLayerOpacity`、`SetGroupVisibility` 和 `EditRippleSource`。移动涟漪源时，验证器合并编辑前与编辑后的影响块，避免旧位置残留。样本为 96×64、32×32 分块，共 6 块：

- 徽章透明度 / `overlay_group`：仅失效 `[0]`；
- 左涟漪源：失效 `[0,1,3,4]`，复用 `[2,5]`。

## 检查点与三路径

`CheckpointStore` 是持久化接口，内置 `MemoryStore`、原子 rename 的 `FileStore`，以及基于 `database/sql` 的驱动无关 `SQLStore`（SQLite DDL 见 `SQLiteSchema`）。

每个块在单个事务中写入像素 BLOB、`pixel_digest`、`stack_snapshot_version`、`ripple_source_version`、`cache_key`、`content_hash`、`param_version`、`algorithm_version`、像素数、边界与串行 `commit_seq`。`SQLStore` 用互斥锁和 `SetMaxOpenConns(1)` 保证事务不交错。

`internal/sqlite` 是可选的标准库 cgo 桥，运行时通过 `dlopen("libsqlite3.so.0")` 调用系统 SQLite，并注册 `vpf_sqlite` 驱动；不引入第三方 Go 模块。

`VerifyThreePaths` 比较：

1. 4 worker、队列预算 1 的实时合成；
2. 先提交前两块再恢复的断点路径；
3. 单 worker 一次性全量重算。

比较内容包括完整帧像素、帧摘要和 `(chunk_index, commit_seq, cache_key, pixel_digest)` 提交序列。`CompareFrames` 返回首个不一致像素的全局坐标与分块索引。

## 调度预算与故障隔离

`BoundedScheduler` 支持最大 worker 数、每 worker 像素缓冲上限和待处理块队列预算。队列耗尽时生产者阻塞，并在 `QueueWait` 中记录块号、原因和等待纳秒；块超过像素缓冲上限返回 `E_BUDGET`。

单块永久失败时，其他成功块仍按规范顺序提交；失败块不写检查点。随后可只重试该块，再从持久化存储组装完整帧。`go test -race ./...` 已通过。

## 诊断页面

`vpfd serve` 提供组树/层栈、图层参数表、3×2 分块状态、短缓存键/摘要、三类失效传播节点链，以及 `/api/state` JSON 快照。

## 主要文件

- `model.go`：内部模型；
- `schema_*.go`：v1/v2/v3 加载与导出；
- `convert.go`：整数单位/通道换算；
- `tree.go`：组树展开与等价判定；
- `generator.go`、`truth.go`：固定种子样本与解析真值；
- `render.go`：线性化、混合、涟漪、分块渲染和像素比对；
- `dag.go`、`editor.go`：依赖图、失效传播和编辑；
- `scheduler.go`：有界并行调度；
- `checkpoint.go`、`filestore.go`、`sqlstore.go`：检查点抽象与持久化；
- `internal/sqlite/driver.go`：可选系统 SQLite `database/sql` 驱动桥；
- `pipeline.go`、`verify.go`：三路径、增量验证和总报告；
- `diagnostics.go`、`cmd/vpfd/main.go`：HTTP 诊断页与 CLI。
