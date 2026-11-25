# SRE 多源时间轴复盘领域组件与诊断工作台

本交付是一个可集成的 Go 领域组件和本地 CLI 工作台。所有样本、三版 schema 工程和核对真值都由固定种子 `42` 的确定性生成器产出；不访问监控系统、外部数据库、Docker 或网络。SQLite 驱动使用纯 Go 的 `modernc.org/sqlite`，依赖已写入 `go.mod`/`go.sum` 并放入 `vendor/`，可用 `GOPROXY=off -mod=vendor` 离线验证。

## 快速运行

```bash
go version                 # 验收环境：go1.22.12 linux/amd64
bash scripts/demo.sh

# 或分步执行
go test -mod=vendor -count=1 ./...
go run -mod=vendor ./cmd/sre-workbench generate --seed 42 --dir out/samples
go run -mod=vendor ./cmd/sre-workbench verify --seed 42 --dir artifacts/verify
go run -mod=vendor ./cmd/sre-workbench repl --workers 4 --buffer 4096
```

交互工作台支持：

```text
load v1|v2|v3
timeline
intervals [service]
review [topN]
candidates [interval-id]
propagate <service> [generation]
diagnostics
edit threshold <service> <basis-points>
edit event-time <event-id> <relative-seconds>
edit add-config <service> <event-id> <relative-seconds> [weight-bp]
edge add <caller> <callee>
edge delete <caller> <callee>
diff
checkpoint save [round]|load|recover|mismatch|truncate
inject time-reversal|negative-threshold|cycle|scale|unknown-field
inject backpressure-buffer|backpressure-workers|truncated-checkpoint
```

`go run ./cmd/sre-workbench demo` 会非交互执行上述主要链路，并把完整输出写到 `artifacts/demo-output.txt`。

## 代码地图

- `internal/model`：统一 canonical 工程、服务、边、事件、策略和真值结构。
- `internal/bizerr`：稳定 `E_xxx` 业务码、可定位 path 和 context。
- `internal/sample`：固定种子样本、三版工程写出和确定性真值编排。
- `internal/schemaio`：v1/v2/v3 DTO、严格未知字段解码、版本探测、编码和校验。
- `internal/timeline`：统一时刻、来源/服务归属、容差去重。
- `internal/intervals`：观测点聚合、不健康区间、合并裁决和稳态基线段。
- `internal/fixedpoint`：位宽 64、scale `1,000,000` 的定点运算与溢出/饱和检测。
- `internal/scoring`：时序、可达性、变更类型因子，贡献分项和固定 tie-break 排序。
- `internal/topology`：调用方向、最短依赖代数、反向影响闭包、衰减叠加、成环检测和代数上限。
- `internal/concurrency`：有界 worker pool、固定容量服务缓冲和背压错误。
- `internal/engine`：并行分析、区间/评分缓存、编辑失效闭包、增量重算和快照替换。
- `internal/checkpoint`：`database/sql` + modernc SQLite 检查点事务与版本恢复。
- `internal/workbench`：`generate`、`verify`、`demo`、REPL 和异常注入链路。
- `test/acceptance_test.go`：跨版本、真值、传播、增量、检查点、并发和错误码验收测试。

## 三版 Schema 归一口径

| 版本 | 字段风格 | 时刻基准 | 时长/评分单位 | 拓扑 | 真值字段 |
| --- | --- | --- | --- | --- | --- |
| `v1` | `snake_case`，如 `occurred_at_epoch_ms` | epoch 毫秒 | lag/TTL 为秒；评分为万分比 `10000` | 扁平 `dependencies` 边表 | 有 `truth` |
| `v2` | `camelCase`，如 `atTick`、`schemaVersion` | `originEpochMs + windowStartTick*tickIntervalMs + atTick*tickIntervalMs`；tick=`5000ms` | lag/TTL 为毫秒；阈值/指标/权重为千分比 `1000`，整数乘 `10` 转 BP | `dependencyTree` 递归嵌套树 | 无 |
| `v3` | 短 `snake_case`，如 `at_rel_ms`、`schema` | `origin_epoch_ms + at_rel_ms` | lag 为毫秒、TTL 为秒；评分为万分比 `10000` | `topology_edges` 扁平边表 | 无 |

内部统一使用 epoch 整型毫秒；canonical 哈希忽略来源 schema、可选 truth 和 v2 tick 表示元数据。当前样本三版装载后的哈希均为：

```text
07a7e30819a8d524e9ce683466180025020cda438f084ecad6702a3816d8972f
```

严格 JSON 解码启用 `DisallowUnknownFields`。无法识别的版本标记返回 `E_SCHEMA_VERSION`；已识别版本中的未声明字段返回 `E_SCHEMA_UNKNOWN_FIELD`，错误包含 `$.field` 路径。

## 时间轴、区间和评分语义

去重键为 `source | service | fingerprint`。同键事件与首条保留事件的绝对时间差不超过 `dedup_tolerance_ms=10000` 时，按逻辑提交顺序保留第一条，其余合并到 `merged_ids`；超出容差则另立一组。报告输出每个原始事件的归一时刻、来源、服务、是否保留、归属事件和去重数量。样本有 43 条原始事件、3 条重复上报，因此保留 40 个候选事件。

区间推导把同一毫秒的健康/容量点合并为一个时间状态；任一指标达到阈值即不健康。区间采用半开口径：

```text
[首个不健康观测时刻, 首个恢复/健康观测时刻)
```

起点闭、终点开；没有健康收尾点时，用最后一个不健康观测的 `alive_ms` 推导 TTL 终点并受窗口末端约束。原子区间之间的健康间隔 `gap <= merge_gap_ms=60000` 时合并；更大的间隔不跨越。生成样本得到 5 个区间，其中 inventory 的 `[60s,120s)` 与 `[150s,210s)` 因 30 秒间隔合并为 `[60s,210s)`，而 `[300s,360s)` 与前一段间隔 90 秒，保持独立。每个区间输出证据点、触发点、关闭点、指标、阈值和峰值；窗口补集输出稳态基线段及声明基线。

评分为声明 64 位定点整数，内部 scale=`1,000,000`：

```text
timing: 区间内=1.0；区间前 lookback=600000ms 内线性衰减；过旧或区间后=0
reach:  同服务=1.0；沿 caller -> callee 边到达依赖的第 d 代 = 1 / 2^d
type:   deploy=1.0, db_migration=.95, config=.90, flag=.80, alert=.45；异常 capacity=.40、异常 health=.30；健康基线证据与 recovery=0
score = timing * reach * type
```

加法使用饱和累加并标记 `saturated`；乘法用 `math/big` 仅做 int64 边界核验，结果仍是 int64，溢出返回 `E_FIXED_OVERFLOW`。候选按总分降序、逻辑序号升序、事件 ID 升序裁决。样本第一根因候选稳定为 `chg-pay-deploy`，总分为 `1600000` raw，参与 `I-0002/I-0003/I-0004`。

## 传播与增量闭包

边方向声明为 `caller -> callee`，即调用方依赖被调用方。故障从 callee 向其 callers 反向传播；默认每代衰减 `0.5`，最多 `propagation_gen=3`。多路径/环回贡献按代叠加，输出 source、target、最短 generation、raw impact、饱和标志、可枚举 closure 和 `generation_capped`。成环拓扑默认由装载/编辑校验返回 `E_TOPOLOGY_CYCLE`；直接调用 `Topology.Propagate` 时也会在代数上限内有界执行并报告 `cycle_detected=true`。v1/v3 扁平边表与 v2 嵌套树收敛为同一个边集合，测试用 `reflect.DeepEqual` 比较传播结果。

增量缓存分为：

- 服务区间缓存：键为 service ID，指纹包含阈值、该服务事件、窗口和合并策略。
- 区间评分缓存：键为稳定的 `service@start:end`，内容为该区间的贡献分项。

阈值编辑只删除该服务的区间缓存，并按反向传播闭包删除闭包内服务的评分缓存；事件补录/修正同时考虑旧/新服务闭包，健康或容量事件还会失效对应区间推导；边增删通过编辑前后完整可达性差异精确找出受影响的调用方服务，所有区间推导均复用。全局候选排序每次从缓存/新贡献重新聚合。`diff` 命令和 `TestIncrementalEditsMatchFullRecompute` 会把增量引擎结果与同快照新建全量引擎逐项比较。快照 revision、canonical hash 或 schema variant 改变时清空全部缓存。

## 并发、耗时与背压

`RunOrdered` 使用固定 worker budget，服务 ID 排序后输出，内部用 map 收集再排序，因此 worker 数和调度顺序不改变区间、分数和排名。每个服务有固定容量 channel buffer；事件数超过 `--buffer` 时返回：

```text
E_BACKPRESSURE_BUFFER at service_buffer
context: service, limit, event_count
```

worker pool 使用固定 worker 和有界队列；队列饱和时 `TrySubmit` 返回 `E_BACKPRESSURE_WORKERS`。`diagnostics` 命令和 verification JSON 记录每服务、每区间的实际纳秒耗时以及缓存复用/重算键。

## SQLite 检查点与恢复

数据库包含 `checkpoint_meta`、`review_rounds`、`service_states`、`contribution_cache`。`SaveRound` 在一个事务内按以下顺序提交：

1. 写/替换 `service_states`（服务与区间状态）；
2. 写/替换 `contribution_cache`（贡献分缓存）；
3. 写完整 `review_rounds`；
4. 最后更新 `checkpoint_meta.latest_complete_round` 全局轮次指针；
5. `COMMIT`。

因此强杀时未提交事务由 SQLite 回滚，读端只能通过最后指针看到完整轮次。SIGINT/SIGTERM 由 `signal.NotifyContext` 捕获，REPL 会关闭已打开的 store；已 `COMMIT` 的最后一轮可恢复。SIGKILL 无法捕获，只承诺已持久化事务继续可用。恢复时若 `review_rounds.project_version != 当前 canonical hash`，返回 `E_CHECKPOINT_VERSION`，不装载/复用子状态；REPL 的 `checkpoint recover` 会明确丢弃部分复用、新建引擎做一次性全量重算，再物化新一轮。截断到 32 字节的检查点在打开前返回 `E_CHECKPOINT_TRUNCATED`；其他 SQLite 损坏映射为 `E_CHECKPOINT_CORRUPT`。

## 稳定错误码

| Code | 触发场景 |
| --- | --- |
| `E_SCHEMA_VERSION` | 缺少/无法识别 schema 版本组合 |
| `E_SCHEMA_FIELD` / `E_SCHEMA_UNKNOWN_FIELD` | 字段类型错误、空 ID、严格模式发现未声明字段 |
| `E_TIME_ORDER` | 窗口倒置、相对时刻越界、显式逻辑流时刻倒序注入 |
| `E_THRESHOLD_NEGATIVE` | 负错误率/饱和度阈值 |
| `E_TOPOLOGY_CYCLE` | 依赖边形成环，context 给出 cycle 和问题边 |
| `E_SCALE_OUT_OF_RANGE` | 评分刻度不是声明的 1000/10000，或 tick 非法 |
| `E_FIXED_OVERFLOW` | 定点乘法/加法超出 int64 |
| `E_BACKPRESSURE_BUFFER` / `E_BACKPRESSURE_WORKERS` | 服务缓冲或 worker 队列耗尽 |
| `E_CHECKPOINT_VERSION` / `E_CHECKPOINT_TRUNCATED` / `E_CHECKPOINT_CORRUPT` | 版本不匹配、文件截断、其他数据库损坏 |
| `E_EDIT_TARGET` / `E_NOT_FOUND` / `E_INVALID_INPUT` | 编辑目标不存在或参数不合法 |

所有错误均为 JSON 可序列化的 `bizerr.Error`，包含 `code`、`message`、`path` 和 `context`。注入只修改内存克隆或 JSON map，不修改生成器和源工程结构。

## 实际验证结果

验收环境：

```text
go version go1.22.12 linux/amd64
```

离线、禁用测试缓存执行：

```text
$ GOPROXY=off go test -mod=vendor -count=1 ./...
?       sre-timeline/cmd/sre-workbench [no test files]
?       sre-timeline/internal/bizerr [no test files]
?       sre-timeline/internal/checkpoint [no test files]
?       sre-timeline/internal/concurrency [no test files]
?       sre-timeline/internal/engine [no test files]
?       sre-timeline/internal/fixedpoint [no test files]
?       sre-timeline/internal/intervals [no test files]
?       sre-timeline/internal/model [no test files]
?       sre-timeline/internal/sample [no test files]
?       sre-timeline/internal/schemaio [no test files]
?       sre-timeline/internal/scoring [no test files]
?       sre-timeline/internal/timeline [no test files]
?       sre-timeline/internal/topology [no test files]
?       sre-timeline/internal/workbench [no test files]
ok      sre-timeline/test       0.035s
```

并发路径另通过：

```text
$ GOPROXY=off go test -race -mod=vendor -count=1 ./test
ok      sre-timeline/test       1.186s
```

CLI 端到端验收：

```text
$ go run -mod=vendor ./cmd/sre-workbench verify --dir artifacts/verify
[PASS] generate_and_load_three_schemas
[PASS] cross_version_normalized_equivalence
[PASS] parallel_interval_and_score_analysis
[PASS] truth_alignment_interval_candidate
[PASS] incremental_threshold_matches_full
[PASS] checkpoint_commit_and_version_recovery
[PASS] stable_injection_codes

seed=42 go=go1.22.12 intervals=5 top=chg-pay-deploy
report=artifacts/verify/verification-report.json
VERIFY PASS
```

测试覆盖：

- `TestSchemaVariantsNormalizeToSameProject`
- `TestAlignmentIntervalsAndScoresMatchTruth`
- `TestPropagationFlatAndNestedTreeMatch`
- `TestIncrementalEditsMatchFullRecompute`
- `TestCheckpointVersionAndTruncation`
- `TestBackpressureAndDeterministicConcurrency`
- `TestStableInputErrorCodes`
- `TestFixedPointOverflowAndSaturation`
- `TestSnapshotReplacementInvalidatesCache`

完整脚本化工作台输出保存在 `artifacts/demo-output.txt`，机器可读验收报告保存在 `artifacts/verify/verification-report.json`，三版工程样本保存在 `out/samples/`。
