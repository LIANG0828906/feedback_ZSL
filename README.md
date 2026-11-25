# featurepipe：流式特征窗口内核

这是一个不依赖容器、外部服务或网络样本的 Go 1.22 工程。固定种子生成器同时产出 `v1`、`v2`、`v3` 三个 schema 文档；内核把它们规范化为同一特征、事件与窗口模型，并完成窗口归属、聚合、水位迟到处理、SQLite 检查点续算和增量失效。

## 快速开始

```bash
go mod download
go test ./...
go build ./...
go run ./cmd/featurepipe demo
go run ./cmd/featurepipe demo --json --out artifacts/demo-report.json
go run ./cmd/featurepipe generate --out artifacts/schemas
go run ./cmd/featurepipe anomalies
go run ./cmd/featurepipe run --version v2 --db /tmp/featurepipe.db
```

`demo` 会装载三版工程、逐窗口展示聚合值、路径、迟到丢弃、编辑失效传播、检查点续算对照以及异常码矩阵。`run` 安装 `SIGINT`/`SIGTERM` 处理；中断只发生在完整提交点之后。

## 三版 schema

| 版本 | 字段风格 | 时刻表达 | 窗口基准 | 权重刻度 | 水位 |
|---|---|---|---|---|---|
| `v1` | snake_case | epoch 毫秒 | 整点对齐 | 千分比 | 缺失，显式降级为未启用 |
| `v2` | camelCase | 相对事件 tick | 数据首点对齐 | 万分比 | 每事件水位 |
| `v3` | 紧凑字段 | epoch 毫秒 | 数据首点对齐 | 千分比 | 每事件水位 |

装载器入口：

```go
doc, err := pipeline.LoadJSON(data)
if err != nil {
    be, _ := pipeline.AsBusinessError(err)
    log.Printf("%s %v", be.Code, be.Context)
}
```

规范化保证：

- 特征 ID/名称、窗口宽度、步长、允许延迟、聚合函数与归一化权重一致；
- 事件逻辑序号、观测时刻、特征、通道、样本值一致；
- 整点对齐和数据首点对齐都还原为绝对毫秒边界；
- 同一工程的特征集合、事件序列和窗口边界在 `WindowPlans` 中一致；
- 冲突别名、版本与时刻/对齐组合冲突返回 `E_SCHEMA_AMBIGUOUS`；
- 水位缺失版本保留 `WatermarkEnabled=false` 和 `disabled_by_declaration` 标记。

## 窗口与聚合语义

窗口为 `[start, start+window_width_ms)`，起点按 `window_step_ms` 推进。每个观测事件只要满足 `start <= observed_at_ms < end`，就属于该窗口；窗口宽度大于步长时，一个事件会进入多个重叠窗口。

窗口集合从该特征最早观测所在的步长桶开始，到最晚观测所在的步长桶结束。工程要求 `window_width_ms >= window_step_ms`，避免出现未覆盖事件的采样空洞。

事件样本以 `ValueScale=1000` 保存（即样本 `1.234` 表示为 `1234`）。聚合权重统一归一化到 `1_000_000 = 1.0`。聚合定义：

- `sum`：原始样本和，保留 `ValueScale`；
- `count`：参与窗口的事件数；
- `avg`：`Σ(value * weight) / Σweight`，保留 `ValueScale`；
- `weighted_sum`：`Σ(value * weight) / 1_000_000`；
- `min` / `max`：原始样本极值；
- 空窗口输出 0，事件数为 0。

这些计算全部使用整数定点运算，验证容差为 0 个最小定点单位。

## 水位与迟到

水位启用时，对每个候选窗口独立判断：

- 当前水位 `< window_end + allowed_lateness_ms`：事件计入窗口；若事件在水位之后才到达，则窗口 `recompute_count` 增加；
- 当前水位 `>= window_end + allowed_lateness_ms`：该事件不再计入此窗口，写入丢弃记录，包含事件序号、特征、窗口编号、水位、迟到毫秒数和稳定 reason code；
- 同一事件可能在一个较早窗口被丢弃，同时仍被一个较晚窗口接收，这是重叠窗口水位语义的直接结果；
- 水位回退或低于已提交水位返回 `E_WATERMARK_BEFORE_COMMITTED`；
- `watermark_enabled=false` 时不做迟到门控，结果明确标记 `watermark_applied=false`。

## 遍历路径与并发

若 `window_width_ms > ratio_threshold * window_step_ms`，选择 `cumulative`：接收事件时直接累加到窗口状态，判定依据为 `P_CUMULATIVE_RATIO_GT_THRESHOLD`。否则选择 `rescan`：最终逐窗口扫描事件集合，判定依据为 `P_RESCAN_RATIO_LE_THRESHOLD`。

路径选择只影响执行策略，不改变窗口成员、聚合值、重算次数或丢弃集合。测试会同时强制两条路径并逐窗口比较。

每批事件按特征并行处理。结果先按特征独立计算，再按 `(feature_id,start_ms)` 排序；因此工作线程数、事件调度顺序和提交批次不会改变最终结果。已提交状态的退出只发生在完整批次之后。

预算规则：

- `workers > max_workers`：`E_THREAD_BUDGET_EXCEEDED`；
- `buffer > max_buffer`：`E_BUFFER_BUDGET_EXCEEDED`；
- 活跃未提交窗口数超过声明缓冲：`E_BACKPRESSURE_WINDOW_BUFFER`，附特征、窗口、事件序号、当前数量与上限；
- 预算参数非正：`E_INVALID_WORKERS` 或 `E_INVALID_WINDOW_BUFFER`。

## SQLite 检查点

纯 Go 驱动 `modernc.org/sqlite`，不依赖 CGO、容器或外部服务。

每次完整批次在一个事务内：

1. 先替换 `fp_windows` 中的窗口状态、参与事件序号和聚合值；
2. 先替换 `fp_discards` 中的迟到丢弃记录；
3. 最后更新 `fp_meta.last_event_seq` 与水位。

因此任何 COMMIT 前中断都只暴露上一完整提交点。测试还用回滚模拟强制终止，确认全局事件序号不前进。

恢复时校验引擎版本、规范化工程指纹和 schema 版本。版本/指纹不匹配时返回 `E_CHECKPOINT_VERSION_MISMATCH` 信息并清空检查点、从事件 1 整段重算。缺少必需元数据、事件序号越过快照、JSON 受损等返回 `E_CHECKPOINT_TRUNCATED`，不会静默接受半快照。

示例：

```go
store, _ := pipeline.OpenCheckpointStore("/tmp/featurepipe.db")
defer store.Close()
opts := pipeline.DefaultRunOptions()
opts.Checkpoint = store
opts.CommitEvery = 8
result, err := pipeline.Run(ctx, project, opts)
```

## 增量失效

`ApplyIncrement(ctx, previousProject, previousResult, nextProject, opts)`：

- 事件值、时刻、特征归属、通道、水位变化的失效从事件所属窗口开始；
- 特征窗口/步长/延迟变化使该特征全部新旧窗口失效；
- 聚合函数集合或权重变化使该特征全部窗口失效；
- 通过 `DerivedFrom` 依赖图递归传播到重叠的派生窗口；
- 未受影响窗口直接复制缓存结果；
- 输出 `EditClosure`（初始窗口与传播闭包）、`WindowInvalidation`（每窗口来源与依赖路径）、重算/复用窗口列表；
- 内部只对目标窗口重算；随后单独跑一次全量结果作为验证对照，并写入 `MatchesFull`，不把全量值混入增量结果。

事件流编辑需维持 1..N 的连续逻辑序号；删除中间事件时应重编号后续事件。

## 业务码

| 业务码 | 场景 |
|---|---|
| `E_SCHEMA_UNSUPPORTED` | 版本缺失/未知、JSON 结构错误 |
| `E_SCHEMA_AMBIGUOUS` | 同义字段冲突、版本与时刻/对齐组合冲突 |
| `E_TIME_BASIS_MISSING` | 相对 tick 缺少 origin/unit 或时间字段缺失 |
| `E_FEATURE_DUPLICATE` | 特征 ID 重复 |
| `E_FEATURE_WINDOW_WIDTH` | 负值/零窗口宽度 |
| `E_FEATURE_STEP_ZERO` | 非正步长或负允许延迟 |
| `E_FEATURE_WIDTH_BELOW_STEP` | 窗口宽度小于步长 |
| `E_FEATURE_UNKNOWN` | 派生依赖或事件引用未知特征 |
| `E_AGG_WEIGHT_OUT_OF_RANGE` | 权重非正或超过源刻度 |
| `E_AGG_FUNCTION_UNKNOWN` | 未知聚合函数 |
| `E_EVENT_SEQUENCE_INVALID` | 事件序号缺失、重复或非连续 |
| `E_EVENT_UNKNOWN_FEATURE` | 事件引用未知特征 |
| `E_WATERMARK_MISSING` | 声明启用但事件无水位 |
| `E_WATERMARK_BEFORE_COMMITTED` | 水位回退到已提交窗口之前 |
| `E_THREAD_BUDGET_EXCEEDED` | 线程预算越界 |
| `E_BUFFER_BUDGET_EXCEEDED` | 缓冲声明越界 |
| `E_BACKPRESSURE_WINDOW_BUFFER` | 运行时活跃窗口超过缓冲 |
| `E_CHECKPOINT_TRUNCATED` | 截断/损坏/不一致快照 |
| `E_CHECKPOINT_VERSION_MISMATCH` | 引擎/工程指纹/schema 版本不匹配 |
| `E_INTERRUPTED` | 可处理信号触发，已完成最后提交点 |
| `E_EDIT_TARGET_INVALID` | 增量编辑输入缺失 |

## 固定种子验证记录

环境：`go version go1.22.12 linux/amd64`，WSL Ubuntu，无容器。种子 `20241126`。

```text
$ go test ./...
ok  featurepipe/pipeline
$ go test -race ./...
ok  featurepipe/pipeline
$ go build ./...
(成功，无输出)
$ go vet ./...
(成功，无输出)
```

固定样本规模：4 个特征、33 个按逻辑顺序到达的观测事件、29 个窗口。实际 `artifacts/demo-report.json` 结果：

- `v1/v2/v3`：特征集合一致、事件序列一致、窗口边界一致，均 29 个窗口；
- `v2/v3`：水位启用，4 次迟到重算，4 条按窗口记录的丢弃记录；
- `v1`：水位字段缺失，显式关闭迟到门控，0 条丢弃；
- 强制 `cumulative` 与 `rescan` 两路径：聚合值、成员、重算次数和丢弃集合逐窗口相同；
- `workers=1` 与 `workers=4`、提交批次 1 与 9：窗口和丢弃集合相同；
- 增量事件值编辑：失效 3 个窗口、重算 3 个、复用 26 个，与全量重算一致；
- 检查点：第 5 个事件中断后从序号 5 恢复，最终 33 个事件，窗口与丢弃集合和全量运行一致；
- 物理截断 SQLite 文件与元数据缺失都会返回 `E_CHECKPOINT_TRUNCATED`；
- 异常矩阵均返回稳定业务码和定位上下文，且不修改源工程数据。

完整报告由命令重新生成：

```bash
go run ./cmd/featurepipe demo --out artifacts/demo-report.json --json
```

## 目录

```text
cmd/featurepipe/       本地演示、生成、运行与异常命令
pipeline/model.go      规范化模型、窗口/结果/路径类型
pipeline/schema.go     三版装载、消歧与定点权重归一化
pipeline/generator.go  固定种子样本与三版文档
pipeline/window.go     窗口边界与成员求解
pipeline/aggregate.go  定点聚合
pipeline/engine.go     并行批处理、水位迟到、双路径、检查点编排
pipeline/store.go      SQLite 原子提交与恢复校验
pipeline/incremental.go 编辑差异、依赖闭包与目标窗口重算
pipeline/demo.go      端到端演示报告
```
