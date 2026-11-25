# 采集端到聚合端背压推演：技术说明

本模块是一个不依赖真实日志平台、容器或外部服务的确定性参考实现。它内置固定种子样本生成器，覆盖三个 schema 版本装载、键归属与拆批、代理缓冲、退避、分区确认/积压/水位、背压丢弃、增量失效、并行调度和 SQLite 检查点恢复。

## 1. 代码结构

- `bpderr/`：稳定业务错误类型与 `E_xxx` 码。
- `model/`：与 schema 无关的工程、代理、分区、规则、批次、时刻报告模型。
- `schema/`：`collector.v1`、`collector.v2`、`collector.v3` DTO、装载器、反向渲染和单位归一化。
- `planner/`：分区键唯一归属、批内条数/字节求解和拆批序号。
- `engine/`：逐 tick 推演、有界并行调度、缓冲/退避/队列/积压/丢弃规则、内存快照。
- `incremental/`：编辑后的依赖闭包、前缀缓存复用和后缀快照重算。
- `checkpoint/`：`database/sql` + `modernc.org/sqlite` 纯 Go 驱动的 SQLite 检查点。
- `sample/`：固定种子 `42` 的确定性工程与生成真值。
- `cmd/bpctl/`：生成本地样本、运行推演、崩溃/信号演练的 CLI。

## 2. 三版本归一化

内部模型统一使用：

- 字节：`BufferMaxBytes`、`RateBytesPerTick`、`BacklogLimitBytes`。
- 时间：逻辑 tick；`TickMillis` 声明一个 tick 的毫秒数。
- 速率/退避比例：归一到万分比对应的实际字节/tick 或 tick 数。
- 规则：扁平 `[]KeyRule{ID,Tenant,Key,PartitionID,ParentID,Priority}`。

版本差异处理：

| 差异 | v1 | v2 | v3 | 归一化 |
| --- | --- | --- | --- | --- |
| 命名 | snake_case | camelCase | 领域嵌套命名 | 同一 `model.Project` |
| 缓冲/积压 | `send_buffer_bytes`、`backlog_limit_bytes` | `bufferEquivMessages`、`backlogLimitEquivMessages` 与 `equivalentBytesPerMessage` | `bufferLimitBytes`、`backlogCeilingBytes` | 等价条数乘等价字节数；两种表达冲突返回歧义码 |
| 比例 | 千分比 `permille` | 万分比 `permyriad` | 万分比 | `base * value / denominator`，要求精确换算 |
| 时间 | `arrival_tick`、`ack_delay_ticks` | `arrivalMs`、`ackDelayMs` | `arrivalAtMs`、`acknowledgementDelayMs` | 毫秒必须是 `TickIntervalMs` 的整数倍 |
| 规则 | 扁平 `key_rules` | 扁平 `keyRules` | `tenantKeyTree` 嵌套树 | 深度优先拍平并保留 `ParentID` |
| 消费水位 | 无字段 | 无字段 | `consumerWatermarkBytes` | 缺失时使用 `derived_rate`，存在时使用 `declared` / `declared_update` |

当两种表达同时出现但换算结果不一致、时间不能整除、比例不能精确归一、规则成环或匹配不唯一时，装载器返回稳定业务码，不静默选择默认值。

## 3. 推演口径

### 3.1 拆批与归属

`planner.BuildChunks` 按批次输入顺序处理消息：

- 同批所有 chunk 具有相同分区归属。
- 达到 `MaxBatchMessages` 或加入下一条消息会超过 `MaxBatchBytes` 时封口。
- 每个 chunk 输出 `SplitIndex/SplitCount`、`Count`、`Bytes`。
- 单条消息超过 `MaxBatchBytes` 返回 `E_BATCH_TOO_LARGE`。

规则匹配按租户/键精确度与显式优先级排序；最高分若映射到不同分区，返回 `E_KEY_RULE_AMBIGUOUS`。

### 3.2 每 tick 状态推进

每个逻辑时刻按以下确定顺序执行：

1. 分区侧按 `RateBytesPerTick` 确认已到 `WriteTick + AckDelayTicks <= CurrentTick` 的可确认字节。
2. 校验静态/更新消费水位：水位大于已确认位置返回 `E_WATERMARK_AHEAD_ACK`；严格模式下水位小于已确认位置返回 `E_WATERMARK_BEFORE_ACK`。
3. 根据分区累计 offset 释放代理中已完整确认的在途 chunk。
4. 并行执行各代理的接收、队列准入、背压挂起、退避判断和缓冲发送。
5. 按代理 ID、chunk 顺序确定性追加分区 offset 和 segment。
6. 并行求解分区积压、水位来源和压力状态。
7. 对达到水位的分区显式丢弃一个最新且本 tick 未发送的挂起批次；没有可丢批次时记录挂起信号。
8. 重排挂起位次、生成快照并在配置 SQLite sink 时提交检查点。

核心公式：

- `BacklogBytes = WrittenBytes - AckedBytes`
- `AckedBytes(t) = min(EligibleWrittenBytes(t), AckedBytes(t-1) + RateBytesPerTick)`
- 缺失消费水位时：`EffectiveWatermark = AckedBytes`，来源为 `derived_rate`
- `WatermarkLagBytes = max(0, AckedBytes - EffectiveWatermark)`
- `WatermarkBacklogBytes = max(0, WrittenBytes - EffectiveWatermark)`

### 3.3 缓冲、退避和丢弃规则

使用的规则编号：

- `R_AGENT_BUFFER_BYTES`：在途字节加队首 chunk 会超过 `BufferMaxBytes`。
- `R_AGENT_FIFO_HEAD_BLOCKED`：队首被阻塞后，后续 chunk 保持 FIFO 挂起。
- `R_AGENT_BACKOFF_SCALE`：退避窗口内禁止发送。
- `R_AGENT_PENDING_QUEUE_LIMIT`：每代理挂起队列预算不足；默认策略显式丢弃新批次并记录 `E_AGENT_QUEUE_FULL`。
- `R_BUFFER_EXHAUST_DURATION`：缓冲连续阻塞达到声明时长，生成 `E_BUFFER_EXHAUSTED_SLOWDOWN`，退避窗口落在归一化后的 `BackoffTicks`。
- `R_PARTITION_BACKLOG_WATERMARK`：`BacklogBytes >= BacklogLimitBytes`。
- `R_CONSUMER_WATERMARK_LAG`：声明水位导致的 `WatermarkBacklogBytes >= BacklogLimitBytes`。
- `R_PARTITION_PRESSURE_SUSPEND`：分区压力仍在但当前没有可显式丢弃的挂起批次。

每条 `DropRecord` 包含 tick、batch/chunk、agent、partition、触发源、规则号、稳定码和原因。测试逐 tick 校验同一 batch/chunk 不会同时出现在发送集合和丢弃集合。

## 4. 增量失效

`incremental.Cache` 保存首次完整运行和每 tick 快照。支持：

- `EditAgentBuffer`
- `EditPartitionBacklogLimit`
- `EditPartitionRate`
- `EditConsumerWatermark`
- `EditKeyRule`

依赖图是代理与分区之间的二分图：只要某 chunk 连接代理 `a` 和分区 `p`，二者就有双向边。编辑分区会传播到写入该分区的代理，再传播到这些代理通过 FIFO/共享缓冲影响的其他分区，反之亦然。键规则编辑同时把旧分区和新分区加入闭包。

时间区间从直接依赖对象最早出现的 chunk tick 开始；该 tick 之前的报告标记为 `reused:...`，之后从 `fromTick-1` 的 SQLite 可序列化内存快照恢复并重算。每 tick 的 `Invalidation.Sources` 和 `PropagationClosure` 可定位失效来源。测试证明多种编辑下增量结果与编辑后工程的一次性全量重算逐字段语义一致。

## 5. 并行与预算

`engine.Budget` 声明：

- `Workers`：并行 worker，合法范围 `[1,64]`。
- `PerAgentQueue`：每代理挂起队列硬预算，合法范围 `[1,100000]`。
- `SchedulerQueue`：有界调度准入队列。

越界预算返回 `E_BUDGET_OUT_OF_RANGE`。调度准入队列耗尽时 `TrySubmit` 返回 `E_WORKER_BUDGET_EXHAUSTED`，不会无界提交。代理和分区任务分别操作独立状态，结果按下标和排序 ID 合并；worker 数、重复运行和代理声明顺序均有确定性测试。每代理每 tick 的实际耗时写入 `AgentReport.ElapsedNanoseconds`。

## 6. SQLite 检查点

检查点表：

- `agent_checkpoints`：tick、agent、在途字节、挂起数量、完整状态 JSON。
- `partition_checkpoints`：tick、partition、written/acked/backlog、完整 segment 状态。
- `commits`：每个完整 tick 的提交历史。
- `global_cursor`：唯一全局已提交 tick 和工程/ schema 版本。

`Store.SaveSnapshot` 在一个事务中按以下顺序执行：

1. 写所有 agent 状态；
2. 写所有 partition 状态；
3. 写 `commits`；
4. 最后 upsert `global_cursor.last_committed_tick`。

事务提交后进程被 `SIGKILL` 也只能看到完整提交点。恢复时校验 `project_id/project_version`；版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH`，高层流程可 `Reset` 后从 tick 0 全量重算。截断或 SQLite integrity 失败返回 `E_CHECKPOINT_TRUNCATED` / `E_CHECKPOINT_CORRUPT`。

## 7. 常用命令

```bash
go version
go test ./...
go test -race ./...
go test -cover ./...

go run ./cmd/bpctl sample --dir ./samples
go run ./cmd/bpctl run --schema collector.v3 --db ./checkpoint.db

# 模拟 tick 2 事务提交后立即硬退出（CLI 退出码 137）
go run ./cmd/bpctl run --schema collector.v2 \
  --db ./crash.db --crash-after-tick 2

# 版本匹配时从最近完整 tick 恢复
go run ./cmd/bpctl run --schema collector.v2 --db ./crash.db

# 信号演练：另一个终端向 bpctl 发送 SIGINT
go run ./cmd/bpctl run --schema collector.v1 \
  --db ./signal.db --tick-delay 40ms
```

## 8. 稳定异常码与注入入口

| 注入/异常 | 稳定码 | 主要测试 |
| --- | --- | --- |
| 缓冲上限为 0 | `E_BUFFER_LIMIT_ZERO` | `engine/engine_test.go` |
| 水位超前已确认位置 | `E_WATERMARK_AHEAD_ACK` | `TestStableFaultCodes` |
| 严格参数下水位早于已确认位置 | `E_WATERMARK_BEFORE_ACK` | `TestStableFaultCodes` |
| 键规则父图成环 | `E_KEY_RULE_CYCLE` | schema/engine/incremental tests |
| 键规则匹配歧义 | `E_KEY_RULE_AMBIGUOUS` | `planner.AssignPartition` |
| 越界 worker/queue budget | `E_BUDGET_OUT_OF_RANGE` | `TestStableFaultCodes` |
| 代理队列预算耗尽且策略为 error | `E_AGENT_QUEUE_FULL` | `TestStableFaultCodes` |
| worker 准入预算耗尽 | `E_WORKER_BUDGET_EXHAUSTED` | `TestStableFaultCodes` |
| 截断 SQLite 文件 | `E_CHECKPOINT_TRUNCATED` | `TestTruncatedCheckpointReturnsStableCode` |
| 检查点版本不匹配 | `E_CHECKPOINT_VERSION_MISMATCH` | `TestVersionMismatchRequiresFullRecompute` |
| 可处理退出信号 | `E_SIGNAL_INTERRUPTED` | `TestCancellationPersistsNearestCompleteTick` |

所有 `bpderr.Error` 都带 `Field` 和 `Context`，例如 agent/partition/batch/tick、期望值与实际值、规则环路径或数据库路径。

## 9. 实际验证记录

验证环境：

```text
$ go version
go version go1.22.12 linux/amd64
```

普通测试：

```text
$ GOTOOLCHAIN=local go test ./...
ok  github.com/liang/collector-backpressure/checkpoint
ok  github.com/liang/collector-backpressure/engine
ok  github.com/liang/collector-backpressure/incremental
ok  github.com/liang/collector-backpressure/planner
ok  github.com/liang/collector-backpressure/schema
```

竞态与静态检查：

```text
$ GOTOOLCHAIN=local go vet ./...
$ GOTOOLCHAIN=local go test -race ./...
ok  github.com/liang/collector-backpressure/checkpoint
ok  github.com/liang/collector-backpressure/engine
ok  github.com/liang/collector-backpressure/incremental
ok  github.com/liang/collector-backpressure/planner
ok  github.com/liang/collector-backpressure/schema
```

覆盖率（最近一次）：

```text
checkpoint   63.3%
engine       79.0%
incremental  73.4%
planner      57.0%
schema       71.5%
```

CLI 硬退出与恢复实测：

```text
crash_exit=137
resumed_from_tick=2
ticks_returned=11
last_tick=13
checkpoint_tick=13
```

真实 `kill -9` 演练（`--tick-delay 1s`，在 tick 0 提交后强杀）得到等待状态 `137`；重启输出 `resumed_from_tick=0`、`ticks_returned=13`、`last_tick=13`。

CLI `SIGINT` 演练实测：

```json
{
  "ok": false,
  "code": "E_SIGNAL_INTERRUPTED",
  "ticks_returned": 2,
  "last_tick": 1,
  "checkpoint_tick": 1
}
```

确定性样本的一次完整运行包含 14 个 tick、6 条显式丢弃记录和 9 条背压/降速信号；逐 tick 校验缓冲上限、FIFO 位次、发送/丢弃互斥和三版本 chunk 等价。耗时数值是诊断观测值，不参与语义相等比较。
