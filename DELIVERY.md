# 时序降采样与聚合窗口重算内核

本仓库在空目录上交付了一个无外部服务依赖的 Go 领域组件和本地演示。样本、三版 schema 文档、权重临界用例与对照真值均由固定种子 `20261126` 确定性生成；SQLite 使用纯 Go 驱动 `modernc.org/sqlite`。

## 快速运行

```bash
go version
go test ./...
go test -race ./...
go run ./cmd/demo --out artifacts/demo --verify
go run ./cmd/demo --inject list
go run ./cmd/demo --inject function_cycle --out artifacts/demo
go build -o /tmp/downsample-demo ./cmd/demo
/tmp/downsample-demo --signal-demo --out /tmp/downsample-signal
```

完整演示会生成：

- `artifacts/demo/project-v1.json`、`project-v2.json`、`project-v3.json`
- `artifacts/demo/windows.json`、`weights.json`
- `artifacts/demo/demo-report.json`
- `artifacts/demo/report.md`
- `artifacts/demo/demo-checkpoint.sqlite`
- `artifacts/demo/demo-crash-order.sqlite`
- `artifacts/demo/injections/*.sqlite`

## 可集成 API

根包名是 `downsample`。典型流程：

```go
bundle, _ := downsample.GenerateBundle()
project, err := downsample.LoadProject(bundle.Documents[downsample.SchemaV2])
if coded := downsample.AsError(err); coded != nil {
    // coded.Code / coded.Version / coded.FieldPath / coded.Context
}

engine := downsample.NewEngine(project, downsample.Budget{
    MaxWorkers: 4,
    MaxWindowsPerMetric: 64,
})
report, err := engine.RecomputeAll(downsample.InvalidationSource{Kind: "bootstrap"})
window, coded := engine.QueryWindow("cpu.usage", startMS)

editReport, coded := engine.ApplyConfigEdit(downsample.ConfigEdit{
    MetricID: "cpu.usage",
    Field:    "functions",
    Functions: []downsample.Func{
        downsample.FuncAvg, downsample.FuncSum,
        downsample.FuncMin, downsample.FuncMax,
        downsample.FuncWeightedAvg,
    },
})

session, coded := downsample.NewSession(project, downsample.DefaultBudget(), "checkpoint.sqlite")
appendReport, coded := session.AppendAndCheckpoint(ctx, point)
_ = session.PersistConfigReport(editReport)
_ = session.Close()

restarted, recovery, coded := downsample.RecoverOrInitialize(
    project, downsample.DefaultBudget(), "checkpoint.sqlite",
)
```

## Schema 版本映射

| 能力 | v1 | v2 | v3 |
|---|---|---|---|
| 字段风格 | `snake_case` | `camelCase` | `PascalCase` |
| 时刻 | `timestamp_ms` 绝对毫秒 | `windowIndex` + `offsetWithinWindowMs`，由 `anchorTimestampMs + index*width + offset` 还原 | `TimestampUnixMilli` 绝对毫秒 |
| 对齐基准 | `integer_minute_epoch` | `first_data_point` | `epoch_grid` |
| 权重刻度 | `permille_1000` | `permyriad_10000` | `permyriad_10000` |
| 函数表 | `function_table.constants` 扁平常量 | `functionTree.groups` 按指标组嵌套 | `FunctionCatalog.Functions` 扁平目录 |
| 最近值填充 | 字段缺失，按 `degradation` 声明默认策略降级 | `lastValueFill` 显式存在 | `FillPolicy` 显式存在 |

生成器把每个原始指标的首点放在基准网格原点，因此整点基准与首点基准得到完全相同的窗口边界。装载测试逐指标比较 ID、顺序、点序号、还原时间戳和窗口起点。

严格解析器拒绝未知字段，并返回：

- `E_SCHEMA_UNRECOGNIZED_FIELD`
- `E_SCHEMA_FIELD_COMBINATION_INVALID`
- `E_SCHEMA_VERSION_UNSUPPORTED`
- 错误中的 `schema_version` 与 JSON 字段路径可直接定位，例如 `$.metrics[0].points[0].offsetWithinWindowMs`。

## 窗口与填充口径

- 边界区间固定为左闭右开：`[start_ms, end_ms)`；恰好落在右边界的点归入下一窗口。
- 整点/锚点网格使用 `WindowStart(ts, width, anchor)`，负时间差使用 floor division。
- 相对偏移只接受 `0 <= offset < width`；右边界必须表达为下一窗口的 `offset=0`。
- 空窗口仍输出。填充策略从当前窗口向前查找声明的 `maxLookbackWindows`，只沿用真实 `OK` 窗口，不把填充窗口当作新的真实源。
- 填充结果的 `point_count=0`；`filled=true`，并在 `fill` 中给出来源窗口和距离。
- 回溯范围内无真实值时为 `EMPTY` / 未填充。
- v1 的 `fill_field_present=false`、`fill_degraded=true`，并通过 `fill_annotation=FILL_FIELD_MISSING_DEGRADED_DEFAULT_APPLIED__FILL_FIELD_NOT_PROVIDED` 标明“填充字段未提供但按声明默认规则降级”；无值窗口标注 `UNFILLED_NO_VALID_SOURCE_WITHIN_DECLARED_LOOKBACK`。
- 对齐基准变更的边界裁决由 `AnchorChangeDecision` 明确给出；配置编辑会令相关闭包失效，避免同一窗口混用两种基准。

## 权重定点换算

内部规范权重刻度为万分比 `10000`。`ConvertWeight` 全程使用声明位宽：

1. 检查源整数是否超出源位宽；
2. 用 `bits.Mul64` 检查 `raw * target_scale` 的 64 位中间乘法溢出；
3. 按源刻度做四舍五入整数除法；
4. 检查目标位宽，超界时饱和到该位宽最大值并返回 `E_WEIGHT_FIXED_OVERFLOW`；
5. 输出精确有理真值、差值 `delta`、声明容差 `0.5` 目标单位以及 `within_tolerance`。

窗口输出 `weighted_sum_raw = Σ value_i * weight_i`、`weight_total_raw = Σ weight_i`、`weighted_mean = weighted_sum_raw / weight_total_raw`。千分比 v1 装载后会定点转换为同一规范万分比，因此三版的权重总量和均值一致。

`StableRoundTrip` 证明 `12345/10000 -> 1235/1000 -> 12350/10000` 后到达固定点，后续反复换算不再漂移。

## 失效传播

- 单点追加只直接失效其时间戳所属窗口。
- 填充依赖会向后传播至多 `maxLookbackWindows`，遇到新的真实 `OK` 窗口即停止。
- 派生指标通过 `Derivation.Inputs` 引用上游窗口聚合；窗口级 BFS 会加入对应 lag 的派生窗口。
- 配置的窗口宽度、对齐基准、函数集合、权重刻度编辑会展开“指标依赖闭包”，并重算闭包内的全部窗口；闭包外窗口保留缓存。
- 每份 `WindowResult.Invalidation` 记录 `initial`、`append`、`config`、`upstream`、`fill_dependency` 或 `checkpoint` 来源。
- `RecomputeReport.Closure` 输出闭包指标、跨指标边和显式边界，例如 `cpu.usage -> cpu.scaled via avg` 与 `terminal_derived_metric:cpu.scaled`。
- 测试把增量结果与新引擎全量重算结果逐窗口比较，包括状态、点数、函数值、填充和权重统计。

## 并行预算与背压

`Budget{MaxWorkers, MaxWindowsPerMetric}` 的声明范围分别是 `[1,64]` 和 `[1,100000]`。`Gate` 使用 worker 信号量和每指标缓冲信号量：

- 非法预算：`E_BUDGET_WORKERS_OUT_OF_RANGE` 或 `E_BUDGET_BUFFER_OUT_OF_RANGE`
- 非阻塞获取不到 worker：`E_BACKPRESSURE_WORKER_BUDGET_EXHAUSTED`
- 非阻塞获取不到窗口缓冲：`E_BACKPRESSURE_WINDOW_BUFFER_EXHAUSTED`

正常全量重算使用有界 `Acquire` 并行推进多指标；注入入口通过持有确定数量的租约稳定复现背压码，不存在越界切片分配或静默丢窗口。

## SQLite 检查点

数据库包含：

- `checkpoint_meta(global_seq, config_version)`
- `window_states(metric_id,start_ms,window_index,config_version,commit_seq,result_json,ready)`
- `samples(seq,metric_id,timestamp_ms,value,weight,point_json,commit_seq)`

提交顺序为：

1. 阶段一事务写入采样点和全部窗口状态，`ready=1`；
2. 阶段二事务更新 `global_seq` 和 `config_version`。

如果进程在两阶段之间被强杀，阶段一可能留下 `commit_seq > global_seq` 的行；恢复只读取 `commit_seq <= global_seq` 的窗口和样本，并报告 `orphan_rows`，所以不会读到未完成提交。恢复还执行 `PRAGMA integrity_check`，截断/损坏文件返回 `E_CHECKPOINT_TRUNCATED`。配置版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH`；`RecoverOrInitialize` 随后清空检查点并整段重算。

## 稳定业务码

| 场景 | 业务码 |
|---|---|
| 不支持的 schema 版本 | `E_SCHEMA_VERSION_UNSUPPORTED` |
| 未知字段 | `E_SCHEMA_UNRECOGNIZED_FIELD` |
| 非法字段组合 | `E_SCHEMA_FIELD_COMBINATION_INVALID` |
| 窗口宽度为 0/负 | `E_WINDOW_WIDTH_ZERO` |
| 相对偏移越界 | `E_RELATIVE_OFFSET_OUT_OF_RANGE` |
| 权重刻度非法 | `E_WEIGHT_SCALE_OUT_OF_RANGE` |
| 定点位宽溢出/饱和 | `E_WEIGHT_FIXED_OVERFLOW` |
| 函数引用成环 | `E_FUNCTION_TABLE_CYCLE` |
| 线程预算越界 | `E_BUDGET_WORKERS_OUT_OF_RANGE` |
| 缓冲预算越界 | `E_BUDGET_BUFFER_OUT_OF_RANGE` |
| worker 背压 | `E_BACKPRESSURE_WORKER_BUDGET_EXHAUSTED` |
| 缓冲背压 | `E_BACKPRESSURE_WINDOW_BUFFER_EXHAUSTED` |
| 检查点截断/损坏 | `E_CHECKPOINT_TRUNCATED` |
| 检查点版本不匹配 | `E_CHECKPOINT_VERSION_MISMATCH` |
| 流被退出信号打断 | `E_STREAM_INTERRUPTED` |

所有公开错误尽量使用 `*downsample.Error`，可通过 `downsample.AsError(err)` 读取稳定码、版本、字段路径和上下文。

## 异常注入

```bash
go run ./cmd/demo --inject list
```

支持：`window_width_zero`、`relative_offset_out_of_range`、`weight_scale_out_of_range`、`function_cycle`、`worker_budget_out_of_range`、`buffer_budget_out_of_range`、`worker_backpressure`、`buffer_backpressure`、`checkpoint_truncated`、`checkpoint_version_mismatch`、`unknown_field`、`stream_interrupt`。

注入只修改临时解析对象或 `--out` 下的临时 SQLite 文件；测试和演示都会校验三版源文档字节哈希未变。被拒绝的编辑不会修改引擎配置，已提交窗口仍可查询。
