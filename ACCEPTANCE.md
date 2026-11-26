# 查询优化内核验收说明

本目录是独立、可运行的 Go 模块 `queryopt`。样本由固定种子 `20261127` 的确定性生成器产出，
持久化使用 `database/sql` + `modernc.org/sqlite`，不依赖 Docker、真实数据库或线上服务。

## 验证命令与实际结果

以下结果已于 2026-11-27（Asia/Shanghai，Go `go1.22.12 linux/amd64`）实际执行：

```text
$ gofmt -l .
# 无输出

$ go vet ./...
# 通过，无输出

$ go test ./... -count=1
?       queryopt/cmd/qo [no test files]
ok      queryopt        0.022s

$ CGO_ENABLED=1 go test -race ./... -count=1
?       queryopt/cmd/qo [no test files]
ok      queryopt        1.149s
```

覆盖的顶层测试包括：

- `TestThreeSchemaVersionsLoadSameSetsAndOrders`
- `TestSchemaUnitScaleAndPlaceholderMapping`
- `TestSelectivityMatchesFixedPointTruth`
- `TestFixedOverflowAndSaturationAreExplicit`
- `TestJoinOrderDegenerateRulesAndRulingBasis`
- `TestParameterSensitivityAndRelativeExpressions`
- `TestHistogramEditRecomputesOnlyDependencyChain`
- `TestPredicateAndRowEditsPropagateAndMatchFull`
- `TestParallelBudgetAndCandidateBackpressure`
- `TestSQLiteStateBeforeCursorAndRecovery`
- `TestFaultInjectionStableCodes`
- `TestProcessingDoesNotMutateSourceProject`

CLI 冒烟也已实际执行：

```text
$ go run ./cmd/qo -mode persist -db /tmp/qo/accept.db
{"committed_ordinal":5,"loaded_queries":5,"db":"/tmp/qo/accept.db"}

$ go run ./cmd/qo -mode crash-demo -db /tmp/qo/crash.db
{"after_query":"q2_account_txn","crash_injected":true,...}

$ go run ./cmd/qo -mode recover -db /tmp/qo/crash.db
{"committed_ordinal":2,"loaded_queries":2,"recovered":[2],...}
```

异常注入 CLI 均返回稳定码：

```text
E_STAT_HISTOGRAM_ORDER
E_STAT_SELECTIVITY_SCALE
E_SCHEMA_TABLE_NOT_FOUND
E_STAT_ROW_COUNT_NEGATIVE
E_OPT_BUDGET_INVALID
E_OPT_BUDGET_INVALID
E_SCHEMA_VERSION_MISMATCH
E_STORE_CHECKPOINT_TRUNCATED
```

## 需求与实现对照

| 需求 | 主要实现 | 验收测试 |
| --- | --- | --- |
| 三 schema 装载、字段/布局/编号/刻度/单位映射 | `wire.go`, `mappers.go`, `validate.go` | `TestThreeSchemaVersionsLoadSameSetsAndOrders`, `TestSchemaUnitScaleAndPlaceholderMapping` |
| Q32 定点选择率、溢出/饱和、真值容差 | `fixed.go`, `estimate.go`, `reference.go`, `truth.go` | `TestSelectivityMatchesFixedPointTruth`, `TestFixedOverflowAndSaturationAreExplicit` |
| 候选排列、代价、淘汰原因、裁决编号 | `optimizer.go` | `TestJoinOrderDegenerateRulesAndRulingBasis` |
| 环、笛卡尔边、不可达谓词、无顺序错误码 | `optimizer.go`, `errors.go` | `TestJoinOrderDegenerateRulesAndRulingBasis` |
| 参数绑定、顺序/访问路径敏感性、两种占位符换算 | `binding.go`, `sensitivity.go` | `TestParameterSensitivityAndRelativeExpressions` |
| 增量依赖闭包、缓存复用、与全量一致 | `incremental.go` | `TestHistogramEditRecomputesOnlyDependencyChain`, `TestPredicateAndRowEditsPropagateAndMatchFull` |
| 并行预算、候选缓冲、背压码 | `parallel.go` | `TestParallelBudgetAndCandidateBackpressure` |
| SQLite 状态先落、水位后推进、崩溃恢复 | `store.go` | `TestSQLiteStateBeforeCursorAndRecovery` |
| 八类异常注入与上下文 | `faults.go`, `validate.go`, `store.go` | `TestFaultInjectionStableCodes` |

## 三版 Schema 口径

- `queryopt/v1-flat`：短蛇形字段；直方图为全局扁平 `hist_flat`；占位符为 1-based 全局绝对 `gpid`；
  选择率/权重/空值率使用千分比；延迟为 100ns tick（`10000 tick = 1 ms`）；容量为字节。
- `queryopt/v2-nested`：camelCase 字段；直方图嵌入 `columns[].statistics.histogram.buckets`；
  占位符为每语句 0-based 相对下标，`global = placeholder_base + relative + 1`；
  选择率/权重/空值率使用万分比；延迟为毫秒；容量为 KiB（`1 KiB = 1024 B`）。
- `queryopt/v3-current`：当前规范化 JSON，Q32 定点选择率、万分比权重、毫秒和字节。

装载后统一为 `Project`。测试逐项比较三版的表 ID、谓词 ID、连接边 ID，并对全部查询比较最终顺序和业务错误码。

## 定点估算与真值

内部小数为 signed Q32.32：`FixedScale = 2^32`。乘法使用 `math/bits` 的 64×64→128 位中间结果，
行计数乘法显式检查溢出并饱和到 `math.MaxInt64`，同时在 `PredicateEstimate.Saturated` 标记。

- 等值：命中桶时为 `bucket_rows / (bucket_distinct * table_rows)`，未命中时回退 `1 / NDV`。
- 范围：按桶交集宽度加权；区间规范化为半开 `[lo, hi)`，精确边界归入右侧桶，避免双计。
- 桶边界：`boundary` 谓词显式定位 `Lower/Upper` 相等边界并输出 `boundary_bucket_id`。
- 嵌套：AND 为选择率乘积，OR 为 `1 - Π(1-sel)`，带谓词环检测。

`reference.go` 用 `math/big.Rat` 独立计算真值。声明容差为 64 个 Q32 raw unit；
行数容差按 `base_rows * 64 / 2^32 + 1` 换算，因此深层笛卡尔基表上的绝对行数容差也符合同一定点误差口径。

## 代价与裁决

列裁剪宽度取查询显式 `required_column_ids` 与谓词/连接引用列的并集。设表行数 `R_t`、
局部过滤后行数 `F_t`、裁剪宽度 `W_t`、前缀宽度 `PW_k`，连接递推为：

```text
I_1 = F_first
I_k = mulDivRound(I_{k-1}, F_t, max(NDV(left), NDV(right)))
```

代价明细包含：

```text
ScanBytes        = Σ R_t * W_t
IntermediateRows = Σ I_k
IntermediateBytes= Σ I_k * PW_k
TotalCost        = round((Σ R_tW_t*w_scan + Σ I_k*w_row + Σ I_kPW_k*w_width) / 10000)
```

每个候选都输出 `feasible`、`reject_code`、`reject_reason`、定位边/步骤和分阶段代价。
最小代价唯一时给 `RB_MIN_COST#<candidate>`；代价差在声明的
`rules.tie_relative_permyriad` 内时，按候选声明次序选择并给
`RB_TIE_ORDER#<candidate>#ORDnn`。

## 退化图规则

- 环：只在非笛卡尔等值边中做 DFS 找环；环内按 `break_priority` 降序、`declared_order` 升序、
  edge ID 升序选一条裁剪，输出 `R_CYCLE_EDGE_CUT` 和 `cut_edge_id`。
- 笛卡尔边：`cartesian:true` 或缺少 `predicate_id` 的边不参与断环；候选若只能通过该边接入新表，
  返回 `R_CARTESIAN_EDGE_FORBIDDEN`，上下文含 edge ID 和失败步骤。
- 不可达谓词：从查询声明的第一张表遍历未裁剪等值边，未覆盖表上的局部谓词标记
  `R_UNREACHABLE_PREDICATE`。
- 无完整连通顺序：查询计划返回稳定 `E_PLAN_DISCONNECTED`，上下文含表、裁剪边和不可达谓词。

## 增量失效

依赖图节点形如 `hist:<column>`、`table:<table>`、`pred:<predicate>`、
`candidate:<query>/<candidate>`、`decision:<query>`、`plan:<query>`、`query:<query>`。
边的方向是“被依赖对象 → 依赖它的结果”，一次 BFS 即得到传播闭包。

编辑直方图、谓词或表行数时：

1. 深拷贝工作工程并校验；非法输入直接返回，不替换旧工程。
2. 从来源节点求传播闭包和脏谓词/脏查询集合。
3. 只重算闭包内谓词与查询计划。
4. 未受影响查询保留原 `*QueryPlan` 指针并标记 `cache_hit:true`。
5. 测试将增量结果与编辑后工程的一次性 `FullRecompute` 逐项 `reflect.DeepEqual`。

## 并行与背压

声明边界为 `1 <= workers <= 64`、`1 <= candidate_buffer <= 4096`。

- `RunParallel` 按 worker 预算阻塞获取 slot，结果按查询 ordinal 排序，不丢查询。
- `BoundedScheduler.TryPlan` 在 worker 预算瞬时耗尽时立即返回
  `E_OPT_WORKER_BACKPRESSURE` 和 `admitted:false`，由调用方重试，不扩张 goroutine。
- 候选排列数 `n!` 大于每查询缓冲时，在分配候选切片前返回
  `E_OPT_CANDIDATE_BUFFER_EXHAUSTED`，上下文含 `candidate_count` 与 `candidate_buffer`。
- 越界线程/缓冲预算统一返回 `E_OPT_BUDGET_INVALID`。

## SQLite 提交与恢复

表：

- `query_state(ordinal PRIMARY KEY, query_id UNIQUE, status, selected_order_json, plan_json, estimates_json, checksum)`
- `global_cursor(id=1, committed_ordinal)`
- `checkpoints(kind PRIMARY KEY, format_version, engineering_version, payload, payload_len, checksum)`

提交顺序：

1. 一个事务写入完整 query state、plan、estimate cache 和 SHA-256 checksum，并提交。
2. 随后单独推进 `global_cursor.committed_ordinal`。

读取端只查询 `ordinal <= committed_ordinal`，因此在步骤 1 后、步骤 2 前被杀掉时不会读到未来查询。
重启后 `Recover` 按连续 ordinal 校验 checksum/JSON，并幂等推进水位。测试用提交状态后立即关闭 DB
模拟 `kill -9`，验证崩溃后直接读取只有 q1，恢复后得到 q1、q2。

## 稳定错误码

| Code | 触发 |
| --- | --- |
| `E_INPUT_INVALID` | 通用结构/参数错误、谓词环、空参数等 |
| `E_SCHEMA_VERSION_UNSUPPORTED` | 未知 schema 版本 |
| `E_SCHEMA_VERSION_MISMATCH` | 工程快照版本与期望版本不一致 |
| `E_STORE_CHECKPOINT_TRUNCATED` | 检查点长度或 checksum 不符，含截断上下文 |
| `E_STAT_HISTOGRAM_ORDER` | 桶边界倒序/单调关系破坏 |
| `E_STAT_SELECTIVITY_SCALE` | 千分比/万分比或定点选择率越界 |
| `E_SCHEMA_TABLE_NOT_FOUND` | 边、列或编辑目标指向缺失表 |
| `E_STAT_ROW_COUNT_NEGATIVE` | 表行数为负 |
| `E_OPT_BUDGET_INVALID` | worker 或候选缓冲越界 |
| `E_OPT_WORKER_BACKPRESSURE` | 运行时 worker slot 耗尽 |
| `E_OPT_CANDIDATE_BUFFER_EXHAUSTED` | 候选排列数超过缓冲上限 |
| `E_PLAN_DISCONNECTED` | 裁剪/禁笛卡尔边后无完整连接顺序 |
| `E_PRED_PLACEHOLDER_UNBOUND` | 占位符未绑定 |
| `E_STORE_WRITE_FAILED` / `E_STORE_CORRUPT` | SQLite 写入或已提交数据损坏 |

## CLI

```bash
go run ./cmd/qo -mode verify
go run ./cmd/qo -mode persist -db /tmp/qo.db
go run ./cmd/qo -mode crash-demo -db /tmp/qo-crash.db
go run ./cmd/qo -mode recover -db /tmp/qo-crash.db
go run ./cmd/qo -mode inject -kind histogram_boundary_reversed
go run ./cmd/qo -mode inject -kind truncated_checkpoint -db /tmp/qo-fault.db
```

`-kind` 支持：`histogram_boundary_reversed`、`selectivity_scale_out_of_range`、
`join_edge_missing_table`、`negative_table_rows`、`worker_budget_out_of_range`、
`buffer_budget_out_of_range`、`truncated_checkpoint`、`snapshot_version_mismatch`。
