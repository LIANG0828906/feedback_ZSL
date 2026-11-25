# 网关日志聚合导出原型

本目录交付一个无外部服务依赖的 Go 1.22 原型。所有样本、schema 文档和观测真值均由固定种子 `20261126` 的确定性生成器产生；SQLite 使用纯 Go 驱动 `modernc.org/sqlite`，不需要 Docker、真实网关或网络账号。

## 快速验证

```bash
go version
bash scripts/verify.sh
```

脚本实际执行：

- `go mod verify`、`gofmt`、`go vet ./...`
- `go test ./... -count=1`
- `go test -race ./internal/gateway -count=1`
- 生成三份 schema 样本和 `truth-report.json`
- 分别用 v1、v2、v3 装载并运行
- 构造真实 `SIGKILL`：第 2 个导出批次提交后强杀，再重启跳过已提交批次并补齐剩余批次
- 验证检查点版本不匹配时整段重算
- 验证截断 SQLite 返回 `E_CHECKPOINT_CORRUPT`

最近一次完整输出保存在 `artifacts/verification-log.txt`。

常用命令：

```bash
go run . generate --out artifacts/sample
go run . validate --schema artifacts/sample/schema-v1.json
go run . validate --parallel --schema artifacts/sample/schema-v1.json
go run . run --schema artifacts/sample/schema-v1.json \
  --db artifacts/checkpoint.sqlite --export-dir artifacts/export
go run . inspect-checkpoint --db artifacts/checkpoint.sqlite
```

## 三版本装载口径

| 版本 | 字段风格 | 时刻 | 耗时 | 路由 | 水位 |
| --- | --- | --- | --- | --- | --- |
| v1 | snake_case | `event_time_ms` epoch 毫秒 | `latency_us` | 扁平 `route_list` | 显式 `watermark_ms` / `watermark_marks` |
| v2 | lowerCamelCase | `eventSeq` 相对序号，按 `originEpochMs + eventSeq * sequenceIntervalMs` 换算 | `latencyMs`，支持精确十进制 | `serviceTree[].routes` 嵌套树 | 省略水位，使用 `committedBoundaryMs` 推定 |
| v3 | OTel 风格嵌套字段 | `timing.observedAtEpochMs` 或 `timing.eventSequence` | `durationMicroseconds` / `durationMilliseconds` | 扁平 `routingRules` | `watermark.observedAtEpochMs` |

装载后统一为 `Project / Service / Route / Record / WindowConfig / ExportConfig / Budget`。同一工程的三个版本保证：

- 服务、路由和记录 ID 顺序一致；
- epoch 毫秒与相对事件序号换算后的 `EventTimeMS` 一致；
- 微秒和毫秒十进制换算后的 Q16.16 定点值一致；
- 每条记录派生出的 service、route、status 分组键集合一致。

同时提供 `event_time_ms` 与 `event_seq` 但二者换算不一致、或同时提供微秒/毫秒且换算不一致时，返回 `E_FIELD_AMBIGUOUS`，不会静默选择字段。

## 窗口与聚合语义

窗口以 `TimeOriginMS` 为对齐原点，使用半开区间：

```text
start(k) = origin + k * slide
end(k)   = start(k) + width
start(k) <= event_time < end(k)
```

整数边界编号的取值范围由 `floorDiv` 推导，支持原点之前的负编号窗口。每条记录同时写入三个层级：

- `service`：`Index + Level=service + ServiceID`
- `route`：再包含 `RouteID`
- `status`：再包含精确 `StatusCode`

每个窗口输出记录数、直方图桶计数、请求/响应字节、错误归类计数和排序后的记录 ID。校验规则：

- `width_ms == 0`：`E_WINDOW_WIDTH_INVALID`
- `width_ms < 0`：`E_WINDOW_WIDTH_INVALID`
- `slide_ms <= 0` 或 `slide_ms > width_ms`：`E_WINDOW_SLIDE_INVALID`

## 水位、晚到和丢弃

处理记录前推进所有 `BeforeArrivalSeq <= record.ArrivalSeq` 的水位标记。声明口径为：

```text
lateness = watermark - event_time
window_committable when watermark >= window.end + allowed_lateness
```

- `lateness <= 0`：正常聚合；
- `0 < lateness <= allowed_lateness`：窗口仍开放，更新聚合并记录 `late_record` 重算来源；
- `lateness > allowed_lateness`：不写入聚合，按候选窗口输出 `DroppedRecord`，包含窗口键、窗口编号和迟到量；
- 当水位达到 `end + allowed_lateness` 时窗口标记为 committed。

v2 缺少水位字段时，降级规则是使用 `committed_window_boundary_ms` 作为初始推定水位，输出 `watermark_source=presumed_committed_boundary`。测试用相同边界构造显式 `watermark_ms` 工程，两者的重算键和丢弃集合逐字一致。

## 定点直方图

耗时使用 64 位 Q16.16 微秒定点数：

```text
FixedDuration raw = microseconds * 65536
```

- 微秒输入走检查乘法；
- 毫秒整数先乘 1000，再转 Q16.16；
- 毫秒小数字段用纯整数十进制解析并舍入最后一个 Q16 bit，不经过 float；
- 溢出返回 `E_DURATION_OVERFLOW`，饱和策略返回 `saturated=true` 和 `math.MaxInt64`；
- 桶区间为半开区间 `[boundary[i-1], boundary[i])`，边界值归入右侧桶。

生成器为前三个边界各构造 `boundary-1 / boundary / boundary+1` 共 9 条临界记录。每条 trace 输出换算耗时、Q16 原始值、桶编号、增量 1、误差、声明容差和 `within_tolerance`。重复分类 100 次不会迁桶。

## 导出批次

候选窗口只包含 committed 且非空的聚合。固定排序为：

```text
(priority asc, end_ms asc, window_key asc)
```

装箱时保持窗口完整：

- 达到 `max_windows_per_batch` 即封批；
- 加入下一窗口会超过 `max_records_per_batch` 时先封批；
- 单个窗口记录数超过 `max_records_per_batch` 返回 `E_EXPORT_WINDOW_OVERSIZED`；
- 一个窗口在同一批次最多出现一次，且全局只属于一个批次，不会跨批拆分。

每批输出成员窗口、成员记录数、去重日志摘要、各窗口优先级编号、快照摘要和 SHA-256 digest。CLI 同时写 `batch-NNNN.json` 与逐批一行的 `export-sequence.jsonl`。重复执行 `BuildBatches` 或从检查点恢复后续传，批次身份完全一致。

## 增量失效与传播闭包

`IncrementalEngine` 保存窗口缓存、记录到候选窗口索引、路由到窗口索引、晚到/丢弃窗口索引和窗口到批次索引。支持：

- `error_rule`：失效该路由下 route/status 窗口，并向上传播 service 父窗口；
- `add_record` / `update_record` / `delete_record`：失效旧键与新键，传播所有层级父窗口；
- `allowed_lateness`：失效所有曾产生晚到/丢弃的窗口及父窗口，再重算接受/丢弃状态；
- `window_config`：窗口归属整体变化，失效全部窗口和批次；
- `watermark`：按晚到依赖闭包传播。

每次编辑返回 `DependencyReport`，包含来源、失效窗口、实际重算窗口、复用窗口、删除窗口、失效/重算/复用批次和可读传播边。测试对每种编辑再执行一次全量引擎，并逐窗口桶、字节、丢弃集合、重算键和批次 digest 比较。

## 并行与背压

`RunParallel` 按 service 分区，启动不超过 `budget.workers` 的固定 worker 池。每个 service 有显式容量的 `BoundedQueue[Record]`：

- 队列满时 `TryEnqueue` 返回 `E_BACKPRESSURE`，上下文带 `service_id/record_id/capacity/pending`；
- worker 槽耗尽时 `WorkerPool.TryAcquire` 返回 `E_BACKPRESSURE`，上下文带 `workers/active`；
- `workers <= 0`、超过 `max_workers`、buffer 非正或超过上限分别返回 `E_BUDGET_WORKERS_INVALID` / `E_BUDGET_BUFFER_INVALID`；
- 每个 service 记录 `elapsed_ns/elapsed_ms` 诊断值，但耗时不参与正确性比较。

各 service 的窗口键天然不相交，结果再经过固定排序和去重。测试覆盖 3 worker、1 worker、反转 service 声明顺序和 `-race`，窗口成员、桶、字节和丢弃集合均与顺序执行一致。

## SQLite 检查点与恢复

检查点表：

- `checkpoint_meta`：工程 ID、版本、指纹、`global_commit_seq`、`last_export_batch_no`
- `window_states`：窗口键、层级定位字段、提交序号和完整聚合 JSON
- `export_batches`：批次号、批次 ID、committed 状态、digest 和完整批次 JSON

窗口提交在同一个 SQLite 事务内执行：先 upsert 所有 `window_states`，最后更新 `global_commit_seq`。导出批次先原子写临时文件并 rename，再在事务内写 `export_batches` 和 `last_export_batch_no`。因此：

- 未提交事务不会被重启观察到；
- 半写的批次 JSON 只可能留在临时文件，不会成为正式批次；
- 已提交批次在恢复后跳过，未提交批次重新生成；
- `PRAGMA integrity_check` 失败或 SQLite 头截断返回 `E_CHECKPOINT_CORRUPT`；
- 检查点版本与输入不一致时 `LoadState` 返回 `E_CHECKPOINT_VERSION_MISMATCH`；CLI 的恢复策略是删除不匹配检查点并从起点整段重算。

`SIGINT` / `SIGTERM` 通过 `signal.NotifyContext` 在批次边界转换为 `E_SIGNAL_GRACEFUL`；`SIGKILL` 无法被捕获，恢复完全依赖已提交的 SQLite 状态。强杀可能发生在报告/manifest 写完之前，因此原型不承诺强杀后一定已有可读导出报告。

## 稳定错误码

| 错误码 | 触发条件 |
| --- | --- |
| `E_SCHEMA_UNSUPPORTED` | schema 版本无法识别 |
| `E_FIELD_REQUIRED` / `E_FIELD_TYPE_INVALID` | 必需字段缺失或类型不可解析 |
| `E_FIELD_AMBIGUOUS` | 冲突单位、时刻或重复表示无法消歧 |
| `E_NEGATIVE_BYTES` | 请求/响应字节为负 |
| `E_WINDOW_WIDTH_INVALID` | 窗口宽度为零或为负 |
| `E_WINDOW_SLIDE_INVALID` | 步长非法或大于宽度 |
| `E_BUCKET_BOUNDARY_INVALID` | 桶边界缺失、非正或非严格递增 |
| `E_BUCKET_SCALE_OUT_OF_RANGE` | 桶边界超出定点位宽 |
| `E_DURATION_OVERFLOW` / `E_DURATION_NEGATIVE` | 耗时换算溢出或为负 |
| `E_WATERMARK_BEFORE_COMMIT` | 水位早于已提交边界 |
| `E_BUDGET_WORKERS_INVALID` / `E_BUDGET_BUFFER_INVALID` | 并发或缓冲预算越界 |
| `E_BACKPRESSURE` | 有界队列或 worker 预算耗尽 |
| `E_EXPORT_BATCH_CONSTRAINT` / `E_EXPORT_WINDOW_OVERSIZED` | 批次参数或窗口大小违反原子装箱约束 |
| `E_CHECKPOINT_OPEN` / `E_CHECKPOINT_WRITE` | SQLite 打开、建表或写入失败 |
| `E_CHECKPOINT_CORRUPT` | 检查点头、integrity check 或 JSON payload 损坏/截断 |
| `E_CHECKPOINT_VERSION_MISMATCH` | 检查点工程版本与输入不匹配 |
| `E_SIGNAL_GRACEFUL` | 可处理退出信号到达安全停止点 |

## 代码地图

- `internal/gateway/sample.go`：固定种子样本和晚到/临界桶构造
- `internal/gateway/loader*.go`：v1/v2/v3 schema 装载
- `internal/gateway/histogram.go`：Q16.16 定点耗时和桶分类
- `internal/gateway/project.go`：校验、路由匹配、错误归类、窗口编号
- `internal/gateway/engine.go`：顺序/分区共用聚合与水位引擎
- `internal/gateway/export.go`：优先级排序和原子窗口装箱
- `internal/gateway/incremental.go`：依赖图、定向重算和传播闭包
- `internal/gateway/concurrent.go`：固定 worker、有界队列和诊断
- `internal/gateway/checkpoint.go`：SQLite 检查点与恢复状态
- `main.go`：`generate`、`validate`、`run`、`inspect-checkpoint`、`demo`
- `internal/gateway/*_test.go`：验收测试和异常码矩阵
