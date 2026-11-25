# ReplayBench 设计说明

ReplayBench 是一个无外部服务依赖的 Go 领域组件与本地演示。它把三个发行格式的回放映射到同一内部回合模型，执行确定性结算、依赖失效、增量重放、SQLite 检查点恢复和诊断输出。

## 包结构

- `internal/eerr`：`E_SCHEMA`、`E_OVF`、`E_CONFLICT`、`E_DIFF`、`E_CHECKPOINT`、`E_CANCELED` 与字段路径、操作数、争用集合。
- `internal/fixed`：定点整型生命/资源。`PointScale=1000`，安全边界 `MaxWhole=1_000_000_000`，原始点上限 `10^12`；所有累加使用检查运算，不使用浮点。
- `internal/model`：统一回放、技能、单位、编辑、裁决、回合快照、窗口和解析真值。
- `internal/timeconv`：回合序号与毫秒时间戳的纯整型 `floor/ceil` 换算、往返和溢出错误。
- `internal/loader`：三格式 JSON 加载、必填字段校验、语义槽位互换、schema 路径和格式互转。
- `internal/generator`：固定种子 `20261126` 的确定性样本；真值由生成器侧显式构造，不读取引擎输出。
- `internal/engine`：无 map 遍历顺序依赖的逐回合状态机、资源/冷却仲裁、条件技能、定点结算和胜负判定。
- `internal/graph`：规则 DAG、内容哈希、节点 ID、参数版本和上游键联合缓存键、精确失效区间。
- `internal/diff`：增量/全量快照与窗口提交序列自动比对，返回首个回合和字段路径。
- `internal/checkpoint`：纯 Go `modernc.org/sqlite` 驱动；窗口、单位摘要和节点缓存表；单事务提交与版本后缀作废。
- `internal/runtime`：恢复最后有效窗口、续算窗口并提交。
- `internal/scheduler`：有界 worker pool、每 worker 缓冲、总队列预算、阻塞等待记录和取消。
- `internal/diagnostics`：`net/http` JSON API 与诊断首页。
- `cmd/replaydemo`：一键验证、样本落盘、HTTP 服务和 SIGKILL 恢复演练。

## 三格式语义槽位

物理字段 `units[].primary_int` 与 `frames[].primary_list` 在不同格式中改变解释：

| 格式号 | 时间字段 | `primary_int` | `secondary_int` | `primary_list` | `secondary_list` |
| ---: | --- | --- | --- | --- | --- |
| 1 | `round` | 单位行动权重 | 技能冷却剩余回合 | 技能释放序列 | 行动者排队序列 |
| 2 | `round` | 技能冷却剩余回合 | 单位行动权重 | 行动者排队序列 | 技能释放序列 |
| 3 | `timestamp_ms` | 同格式 1 | 同格式 1 | 同格式 1 | 同格式 1 |

格式 3 使用：

```text
timestamp_ms = epoch_ms + (round - 1) * ms_per_round
round        = floor((timestamp_ms - epoch_ms) / ms_per_round) + 1
```

声明为 `ceil` 时使用整数向上除法。对齐时间戳可高格式 → 低格式 → 高格式往返；溢出返回 `E_OVF`，错误包含字段路径和两个操作数。

## 确定性仲裁

每回合从释放序列构造意图，条件标志为假的技能只记录 `condition_flag/skipped`，不参与效果。可裁决顺序为：

1. 行动权重降序；
2. 行动者排队位次升序；
3. 技能优先级降序；
4. 单位 ID 升序；
5. 技能 ID 升序；
6. 序列下标升序。

显式 `precedence` 边存在时，用 Kahn 拓扑排序并在可执行节点中应用上述键。若优先级约束成环，返回 `E_CONFLICT` 并携带排序后的争用技能集合。

同一单位的正消耗技能共享资源池；相同 `cooldown_group` 的技能共享冷却组，`cooldown_scope=global` 时跨单位共享。先通过仲裁顺序者结算，后续资源不足或冷却未结束的意图被拒绝。每条意图都有裁决明细：回合、单位、技能、争用类型、结果、原因、争用集合和可解释排序键。

## DAG 与缓存键

显式节点包括：

- `input:replay`
- `rule:weights`
- `rule:cooldown_reduction`
- `rule:flags`
- `rule:formation`
- `edit:weight:<unit>@<round>`
- `edit:flag:<flag>@<round>`
- `edit:reorder@<round>`
- `content:round:<r>`
- `snapshot:round:<r>`

回合快照依赖回合内容、四类规则节点、已生效编辑节点和上一回合快照。缓存键材料为：

```text
sha256(JSON({
  input_hash,
  node_id,
  params_version,
  dep_keys[]
}))
```

编辑 `beta` 权重并从第 2 回合生效时，第 1 回合键不变，第 2 回合键改变；由于第 3 回合依赖第 2 回合快照键，失效区间为 `[2,3]`。未失效回合从 SQLite `node_cache` 读取并重建运行时状态，失效回合按回合拓扑顺序重算，随后在一个节点缓存事务中原子替换。

## SQLite 检查点

`checkpoint_window` 每个窗口一行，`unit_digest` 在同一事务写入该窗口末快照的所有单位摘要。窗口索引从 1 连续递增；`Store` 使用互斥锁和 `SetMaxOpenConns(1)` 防止写入交错。

窗口行保存：

- 窗口和首尾回合；
- 末状态摘要；
- 已消费事件 offset 与 next offset；
- 内容哈希；
- 源格式版本；
- 参数版本；
- 算法版本；
- 前一窗口摘要和当前窗口摘要；
- 完整窗口快照 JSON。

恢复时按窗口顺序校验索引、前向摘要链、内容哈希和三个版本。第一个不匹配窗口及其后所有窗口在单事务中删除，只返回最后有效窗口。

## 调度预算和终止边界

`scheduler.Pool` 限制：

- `MaxWorkers`：最大并行 worker；
- `WorkerBuffer`：每 worker mailbox/状态缓冲上限；
- `QueueBudget`：排队加运行任务总预算。

预算耗尽时提交者阻塞等待，并记录 `queue_budget_exhausted`、任务 ID 和等待毫秒；worker 缓冲满时记录 `worker_buffer_full`。取消会中断预算或 worker 等待；已经进入窗口事务的任务只在回合边界响应取消，未完成窗口不会提交。

`SIGKILL`/宿主机强制终止无法生成退出报告，但 SQLite 的 FULL synchronous + 单事务窗口保证只有已提交且版本匹配的窗口可用于恢复。自动化测试 `TestForceKillCheckpointRecoveryBoundary` 会启动测试子进程、在窗口 1 提交后给自己发送 `SIGKILL`，父进程随后验证只能从窗口 1 续算且结果等于全量。

## 常用命令

```bash
go test -count=1 ./...
go test -race -count=1 . ./internal/checkpoint ./internal/scheduler
go run ./cmd/replaydemo -samples-dir artifacts/samples
go run ./cmd/replaydemo -serve -addr 127.0.0.1:18080

# 真实强杀恢复演练
go build -o artifacts/replaydemo ./cmd/replaydemo
./artifacts/replaydemo -db /tmp/rb.db -crash-after-window=1
./artifacts/replaydemo -db /tmp/rb.db -recover-only
```

依赖已 vendored，可使用：

```bash
GOPROXY=off GOFLAGS=-mod=vendor CGO_ENABLED=0 go test ./...
```
