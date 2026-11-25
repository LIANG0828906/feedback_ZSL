# 战斗技能排队内核设计说明

## 1. 模块与入口

模块名：`battlekernel`，Go directive 为 `go 1.22`。SQLite 驱动使用纯 Go 的 `modernc.org/sqlite v1.29.10`，交叉编译 Windows/amd64 时设置 `CGO_ENABLED=0` 即可，不需要 GCC 或 Docker。

主要包：

- `internal/errs`：`E_SCHEMA`、`E_OVF`、`E_ORDER`、`E_STATE`、`E_DECODE`、`E_FLAG`、`E_COOL`、`E_CHECKPOINT` 和带字段、回合、单位、技能、两个取值的错误结构。
- `internal/model`：统一编排模型、回合结果、真值、窗口、失效项和依赖描述。
- `internal/convert`：纯整型回合/毫秒换算，支持 floor/ceil 声明方向和溢出检查。
- `internal/loader`：三套发行格式的解码、字段路径校验、语义互换映射、跨格式一致性检查。
- `internal/sample`：固定种子样本生成器，以及生成器自有的参考真值构造。
- `internal/engine`：排队裁决、冷却资源结算、真值比对、依赖图和窗口摘要。
- `internal/checkpoint`：SQLite schema、串行/原子窗口提交、失败回合、哈希与版本校验、整体作废。
- `internal/pipeline`：实时、断点恢复、全量重算、路径比对和增量失效。
- `internal/review`：不导入 `engine`、`pipeline`、`checkpoint` 结算路径的独立复核器，直接读 SQLite 和真值指纹。
- `internal/sched`：有界 worker pool、每 worker 缓冲、阻塞等待记录。
- `internal/app`：一键验收编排。
- `internal/web`：标准库 `net/http` 页面和 JSON API。
- `cmd/battlekernel`：`verify`、`generate`、`serve`、`review` 命令。
- `cmd/review`：独立复核可执行文件。

## 2. 三套发行格式

同一场战斗生成三份文档和一份真值：

| 格式号 | 源标识 | 回合基准 | 整数字段互换 | 列表语义互换 | 条件标志 |
|---:|---|---|---|---|---|
| 1 | `tick-weight-v1` | `timeline[].round` 回合序号 | `units[].priority` = 行动权重；`units[].recast` = 初始冷却剩余 | `lists.alpha` = 技能释放序列；`lists.beta` = 行动者排队序列 | `0/1` 整数 |
| 2 | `chrono-swapped-v2` | `timePoints[].atMilliseconds` 毫秒时间戳 | `unitRecords[].priority` = 冷却剩余；`unitRecords[].recast` = 行动权重 | `sequenceBox.alpha` = 行动者序列；`sequenceBox.beta` = 技能序列 | JSON bool，可带冗余 bit |
| 3 | `compact-legacy-v3` | `frames[].r` 回合序号，短字段命名 | `actors[].w` = 权重；`actors[].cd` = 冷却 | `lists.skill_seq` / `lists.actor_seq` | `"Y"/"N"` 字符串，可带冗余 bit |

加载后统一为 `model.Project`，其中：

- `BaseWeight` 始终是行动权重；
- `InitialCooldown` 始终是冷却剩余；
- `SkillSequence` / `ActorSequence` 始终是两个规范化列表；
- `RoundSpec.Round` 始终是内部回合单位，`TimestampMs` 是规范化到回合边界的整型毫秒；
- `SourceFormat` / `FormatNo` 保留来源格式标识。

未知格式号返回 `E_SCHEMA`，字段为 `format` 或 `fmt`。必填字段缺失返回 `E_SCHEMA`，字段路径形如 `units[2].priority`、`timePoints[0].commands[2].conditionFlag`。JSON 语法损坏返回 `E_DECODE`。

## 3. 整型换算

一个内部回合长度由 `tick_ms` 声明，样本固定为 `1000ms`。换算只使用 `int64`：

- 回合序号 → 毫秒：`epoch_ms + (round - start_round) * tick_ms`，乘法和加法均做溢出检查。
- 毫秒 → 回合序号：`start_round + floor_div(timestamp_ms - epoch_ms, tick_ms)`；也支持声明的 ceil 方向。
- 非对齐时间戳按声明方向落入回合桶，再规范化到该回合起点。核心保证的是排队回合和出手次序不变；生成器产出的时间戳均对齐，因此原始毫秒也精确往返。
- 溢出返回 `E_OVF`，错误中包含字段路径和参与运算的两个值。

测试 `TestChronoTickChronoRoundTripPreservesOrder` 执行格式 2 → 格式 1 → 格式 2，并逐回合比较内容哈希。

## 4. 排队与结算规则

每回合：

1. 校验所有单位冷却、资源边界。冷却协议上限为 `99`；负数或超过上限返回 `E_COOL`，错误带回合、单位和技能。
2. 条件技能标志归一化为真才进入候选；标志为假进入 `unqueued`，原因 `flag_false`，并且该单位冷却本回合冻结，不执行 `-1`。
3. 冷却剩余大于零的技能进入 `unqueued`，原因 `cooldown_wait`，普通等待冷却在回合末 `-1`。
4. 候选按以下稳定键排序：
   `weight desc → tie_ordinal asc → unit_id asc → skill_id asc`。
   每条 `QueueEntry.Basis` 记录完整裁决键。
5. 按队列顺序结算：
   - 资源小于技能消耗时写 `decision=skipped_resource` 和解释，不扣资源、不进入冷却；
   - 资源足够时扣资源、造成伤害，并把冷却设为技能完整冷却；
   - 回合末所有单位获得 `regen`，但不超过 `max_resource`；
   - 非条件不满足、且本回合成功施放的单位保留完整冷却值；其他非冻结单位冷却大于零时减一。

真值由 `internal/sample/reference.go` 独立构造，不复用 `internal/engine`。`engine.VerifyTruth` 逐回合逐位比较队列、实际出手、未入队原因、冷却和资源；不一致分别返回 `E_ORDER` 或 `E_STATE`，并带回合、单位和技能标识。

## 5. 重排生效边界与失效传播

`ReorderInstruction` 含 `offset`、`unit_id`、`after_round`、`new_tie_ordinal`。规则是严格作用于 `round > after_round`，只改变目标单位的确定性 tie ordinal。权重编辑使用 `WeightEdit.FromRound`，作用于 `round >= from_round`。

`engine.BuildDependencies` 显式输出：

- 行动权重 → 目标单位及同权重排名邻居；
- 冷却状态 → 后续回合排队和状态；
- 条件标志 → 当前回合计队与冷却是否冻结；
- 重排指令 → 指定回合之后的 tie ordinal 和同权重邻居。

增量更新时，`pipeline.ApplyIncremental` 从声明边界做影子比对：

- 没有任何队列、冷却或资源差异：`NoOp=true`，不删除、不重写窗口，全部使用持久化结果。
- 发现首个真实差异：输出 `Invalidation{Round,UnitID,Field,Reason}`，随后把发生排名/状态交换的单位及其后续回合标记为 `future_round_propagation`。
- 原子窗口包含首个差异时，整个窗口一起删除并重提；窗口内位于差异点之前的对等回合仍使用旧快照值，报告中列入 `atomic_peer_rounds`。
- 早于窗口起点的回合保留在 SQLite 中，不重算、不重写。

样本中的验证结果：

- 把 `u4` 权重改为 `99` 且从第 3 回合生效：首个差异回合为 3，复用第 1、2 回合，重算第 3–6 回合。
- 插入让 `u2` 在第 3 回合后使用 `tie=0` 的重排：第 4 回合改变 `u2/u3` 次序，复用第 1–3 回合。
- 相同权重的无效编辑：无失效项、无窗口重写。

## 6. SQLite 检查点与断点续算

数据库表：

- `meta`：持久化工程信封、语义哈希、格式版本、算法版本。
- `windows`：窗口序号、起止回合、窗口哈希、格式/算法版本、重排 offset 列表、状态。
- `rounds`：每回合 JSON 结果、内容哈希、`committed/failed` 状态和错误码/信息。
- `consumed_reorders`：已消费重排指令 offset、所属窗口和截止回合。

`Store` 使用 `sync.Mutex`、`SetMaxOpenConns(1)` 和单个 SQLite 事务提交一个窗口内的 `windows + rounds + consumed_reorders`，因此写入串行、窗口原子、窗口序号和回合顺序稳定。

恢复时：

1. 读取已提交回合和窗口。
2. 重算并比对每个回合内容哈希及数据库存储哈希。
3. 比对 `format_version` 和 `algorithm_version`。
4. 任一不匹配返回 `E_CHECKPOINT`，随后删除该工程的全部 `windows/rounds/consumed_reorders/meta`，从第 1 回合重算，不混用旧窗口。
5. 单回合异常通过 `MarkRoundFailed` 写 `failed` 行；重试成功后以同一主键覆盖为 `committed`。已提交的前序窗口不受污染。

## 7. 三路径与独立复核

`pipeline.CompareReports` 比较：

- 回合数和回合号；
- 每回合 queue ID 序列；
- 实际 action order ID 序列；
- 每单位冷却和资源；
- 每回合内容哈希；
- 窗口数、窗口序号和窗口哈希。

验收命令对每场战斗运行：

1. 一次性 `FullRecompute`；
2. 写 SQLite 的实时 `RunRealtime`；
3. 提交一个窗口后关闭、重开并 `RunResume`；
4. 第 3 回合注入失败后重试；
5. 篡改算法版本/内容哈希后整体作废并恢复；
6. `cmd/review` 独立复核。

`internal/review` 不导入 `internal/engine`、`internal/pipeline` 或 `internal/checkpoint`。它直接执行 SQL，读取持久化的工程信封、回合快照和 `out/truth-fingerprints.json`，用自带的第三套排序/结算代码重算，并：

- 校验快照内容哈希；
- 比对重算结果与快照的队列、出手、冷却、资源；
- 构造真值回合哈希，与生成器给出的真值指纹逐回合比较；
- 输出首个违规回合和字段名。

## 8. 并发与资源边界

`sched.Pool` 启动固定数量 worker，每个 worker 有容量为 `bufferPerWorker` 的任务 channel。提交时选择最短队列；所有 worker 缓冲满后，`Submit` 阻塞在 channel 发送上。等待结束后记录：

- `reason`；
- `worker`；
- `wait_ms`；
- 当前缓冲长度。

验收中有一个 `1 worker + buffer 1` 的确定性压测：占住 worker、填满缓冲、再提交第三个任务，观察到一次约 30ms 的等待记录，不丢弃任务。

同一工程的回合有状态依赖，因此回合内部按确定顺序提交；不同战斗工程在验收中由 worker pool 并行核验。SQLite 写入再由 store mutex 和单连接串行化，兼顾并行计算与稳定提交顺序。

## 9. 本地命令

Linux/WSL：

```bash
go mod tidy
go vet ./...
go test ./...
go build ./...
go run ./cmd/battlekernel verify
go run ./cmd/review --db out/data/battle-001.realtime.db \
  --truth out/truth-fingerprints.json --project-id battle-001
go run ./cmd/battlekernel serve --addr 127.0.0.1:8080
```

Windows PowerShell：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\verify-windows.ps1
```

WSL 中验证 Windows 纯 Go 交叉编译：

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o out/windows/battlekernel.exe ./cmd/battlekernel
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o out/windows/review.exe ./cmd/review
```
