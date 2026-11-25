# 节点维护滚动迁移编排内核 — 交付说明

本交付是一个可嵌入的 Go 领域组件，也是一个可重复运行的 `maintctl` 诊断 CLI。所有样本和真值均由固定种子 `424242` 在本地生成；不依赖 Docker、外部数据库、网络服务或 GPU。

## 1. 快速验证

```bash
go version
go env GOPROXY CGO_ENABLED

go run ./cmd/maintctl generate -dir samples -seed 424242
go run ./cmd/maintctl verify   -dir samples
go run ./cmd/maintctl analyze  -dir samples > /tmp/analysis.json

go test ./...
go test -race ./...
go vet ./...

go run ./cmd/maintctl recover-demo -state-dir .recovery-demo
go run ./cmd/maintctl incremental-demo -change lease
go run ./cmd/maintctl cases
go run ./cmd/maintctl fault -work .fault-work time_conflict
```

`verify` 会重新装载三个 schema，比较规范模型与三版分析结果，再逐时刻、逐节点、逐实例核对生成器写出的构造真值。

## 2. 代码结构

- `internal/domain`：规范工程模型、稳定 `E_xxx` 错误、字段路径和输入校验。
- `internal/schema`：v1/v2/v3 DTO、严格未知字段定位、逻辑时刻/相对偏移换算。
- `internal/generator`：固定种子样本和三版 JSON/真值文件输出。
- `internal/truth`：只由样本几何与参数构造的预期记录，不导入求解器。
- `internal/solver`：逐时刻可行性、目标选择、波次并发/时长/窗口/水位约束。
- `internal/lease`：租约生效、独占容量校验、到期/超限收回、水位降级。
- `internal/gate`：租约归还与实例回迁闸门、确定性推进序列。
- `internal/store`：`modernc.org/sqlite` 纯 Go SQLite 检查点。
- `internal/parallel`：有界线程信号量和每节点队列背压。
- `internal/incremental`：依赖图缓存、失效传播和变更补丁。
- `internal/app`：编排门面、真值核对、故障注入、恢复/增量演示。
- `cmd/maintctl`：生成、验证、分析、恢复、增量和故障码 CLI。
- `acceptance_test.go`：端到端验收测试。

## 3. 三版 Schema 统一口径

| 维度 | v1 | v2 | v3 | 规范模型 |
| --- | --- | --- | --- | --- |
| 节点 | `hosts[].host_id` | `nodes[].name` | `servers[].server_ref` | `Node.ID` |
| 容量 | 整核 `core_capacity` | 毫核 `capacity_millicores` | mCPU `capacity_mcpu` | 毫核 `CapacityMilli` |
| 水位 | `watermark_permille` | `watermark_basis_points` | `safe_fill.per_ten_thousand` | 万分之一基点 `WatermarkBasisPoints` |
| 时刻 | `logical_tick` | 相对 `base_tick` 的 `offset_ticks` | 全局 tick 或 offset | 全局逻辑时刻 `Tick` |
| 窗口 | 绝对 `start/finish_logical_tick` | `base_tick + length_ticks` | `starts_at_tick + duration_ticks` | 半开区间 `[Start,End)` |
| 批次 | `batch_schedule.flat_batches[]` | `wave_plan.waves[].batches[]` | `maintenance_tree[].work_orders[]` | `Batches + Waves` |
| 强制标记 | `force_marker` + 顶层武装位 | `force_flag` + `force.armed` | `mandatory_if_armed` + `force_gate.enabled` | `ForceDeclared` 与 `Forced` |

`750‰` 与 `7500‱` 都归一为 `7500` 基点。规范安全水位为：

```text
SafeLimitMilli = floor(CapacityMilli * WatermarkBasisPoints / 10000)
Headroom       = SafeLimitMilli - UsedMilli - ReservedMilli
```

严格解码会递归检查 DTO 字段；未知组合返回 `E_SCHEMA_UNKNOWN_FIELD_COMBO`，路径形如 `$.unrecognized_combination`。同一时刻同时给绝对 tick 和相对 offset 且换算不一致时返回 `E_TIME_BASE_CONFLICT`，路径定位到具体事件/租约字段。

## 4. 确定性样本几何

默认种子为 `424242`，生成 9 个节点、35 个实例、3 个声明维保波次和溢出求解波次。所有容量均为整核或整千毫核，因此 v1 整核表达与 v2/v3 毫核表达逐字节规范化后相同。

关键构造结果：

- 水位均为 `7500‱`，8 核节点的安全水位为 6 核。
- `m-block` 在时刻 10 可腾退 4 个实例；第 5 个实例因所有可硬容纳目标的水位余量不足而得到 `R_TARGET_WATERMARK_FULL / C-WATERMARK-NONDISRUPTION`。
- 波次 1 并发上限为 2：`i-fast-1 -> t-1`、`i-slot-1 -> t-2`，`i-fast-2` 因 `C-WAVE-CONCURRENCY-LIMIT` 顺延波次 2。
- 波次 2：`i-wave2-1 -> t-4`、`i-fast-2 -> t-3`。
- 波次 3/4 继续消化 `m-block`，最终保留两个无法迁出的实例，使闸门稳定为 blocked。
- `loan-fast-t3` 到期后，`t-3` 上仍有未完成回迁的 `i-fast-2`，因此在 tick 17 触发 `w-t3-degrade` 降级。
- `loan-wave2-over` 使 `t-over` 的有效借贷超过 `BorrowCapMilli=1000`，在 tick 13 按低优先级先收回规则收回。
- tick 22 时 `m-fast`、`m-slot`、`m-wave2` 闸门可调度；`m-block` 因两个实例未迁出而不可调度。

真值文件 `samples/truth.json` 由 `internal/truth.Construction` 按上述几何显式构造，不读取 `solver`、`lease` 或 `gate` 的处理结果。

## 5. 求解与裁决规则

- 候选实例按 `Priority` 升序、`DeclaredOrder` 升序、实例 ID 升序裁决；优先级数字越小越高。
- 目标选择使用 best-fit：选择能容纳实例且剩余水位余量最小的目标，平局按节点 ID。
- 硬容量不足返回 `R_TARGET_CAPACITY_INSUFFICIENT / C-CAPACITY-HARD-LIMIT`；硬容量足够但水位不足返回 `R_TARGET_WATERMARK_FULL / C-WATERMARK-NONDISRUPTION`。
- 波次为半开区间；下一波从 `max(end_tick)` 开始，因此波次不重叠。
- 迁移耗时在 `[start,end)` 内预留目标水位；完成后把用量从源节点切到目标节点。
- 超过策略最大搬迁时长返回 `R_MIGRATION_DURATION_EXCEEDS_LIMIT / C-MIGRATION-MAX-DURATION`；超出维护窗口由输入校验返回 `E_MIGRATION_EXCEEDS_WINDOW / C-MAINTENANCE-WINDOW`。

## 6. 借贷、降级与返回闸门

- 租约区间为半开 `[StartTick,ExpiryTick)`。
- 同一出借节点的有效租约总额不得超过其已腾退容量，否则返回 `E_LOAN_DOUBLE_BOOKED / C-LEASE-CAPACITY-EXCLUSIVE`。
- 同一借入节点的有效借贷不得超过 `BorrowCapMilli`；超限后按“低优先级、后声明次序优先收回”的规则产生 recall 事件。
- 租约到期或收回后，若当前用量加在途预留超过安全水位加有效借贷，按确定性顺序降级实例，直到恢复水位内。
- 返回闸门要求：未归还租约为零、非豁免实例全部回迁、未迁出实例为空。
- 回迁容量预算为：

```text
OutstandingLoans + ReturnReservations + CompletedReturns + NextSize <= FreedCapacity
```

这保证已借出的容量和待回迁实例不会重复占用同一份腾退容量。回迁目标只能是原节点，因此不会挤占其他节点已占用的水位。

## 7. SQLite 检查点与续算

驱动为纯 Go 的 `modernc.org/sqlite`，依赖已写入 `go.mod`。

提交检查点时在同一事务内按以下顺序执行：

1. upsert `node_states`；
2. upsert `lease_states`；
3. upsert `instance_states` 和回迁进度；
4. 写 `meta.state_version/project_id`；
5. 最后更新 `wave_cursor.committed_wave/tick`。

事务提交前强杀或回滚不会留下可见的半笔租约/波次游标。重开数据库后：

- 版本等于 `store.CurrentStateVersion=1`：从 `committed_wave`、实例当前节点和租约状态续做；
- 版本不匹配：返回 `E_STATE_VERSION_MISMATCH`，详情带 `full_recompute_required=true`，调用方必须整段重算；
- 截断/校验和失败的 envelope 或损坏库映射为 `E_CHECKPOINT_TRUNCATED`。

`go run ./cmd/maintctl recover-demo` 保存 wave 1 后重开 SQLite，并从 wave 2 续算；恢复出的 7 条完整迁移与一次性全量计划逐字段一致。

## 8. 并发背压与增量失效

- 线程预算范围：`1..64`；每节点队列范围：`1..4096`。
- 越界分别返回 `E_THREAD_BUDGET_INVALID`、`E_QUEUE_BUDGET_INVALID`。
- 线程信号量耗尽返回 `E_BACKPRESSURE_THREAD_EXHAUSTED`；指定节点队列满返回 `E_BACKPRESSURE_QUEUE_FULL`，不会放行或丢弃。
- 可行性任务按索引回收并比较 JSON；线程数 1、2、8 不改变结果。`go test -race ./...` 通过。
- `incremental.Engine` 维护 `依赖 -> 结果键` 的反向图。变更 `node:<id>`、`window:<id>` 或 `lease:<id>` 后沿依赖链传播；每个结果记录 `recomputed/reused`、失效来源和字段路径。

## 9. 稳定故障码

`go run ./cmd/maintctl cases` 列出全部注入用例；当前覆盖：

- `negative_watermark -> E_INPUT_NEGATIVE_WATERMARK`
- `time_conflict -> E_TIME_BASE_CONFLICT`
- `unknown_field -> E_SCHEMA_UNKNOWN_FIELD_COMBO`
- `migration_window -> E_MIGRATION_EXCEEDS_WINDOW`
- `loan_order -> E_LEASE_EXPIRY_BEFORE_START`
- `loan_maintenance -> E_LEASE_TARGET_MAINTENANCE_NODE`
- `thread_budget -> E_THREAD_BUDGET_INVALID`
- `queue_budget -> E_QUEUE_BUDGET_INVALID`
- `backpressure_queue -> E_BACKPRESSURE_QUEUE_FULL`
- `backpressure_thread -> E_BACKPRESSURE_THREAD_EXHAUSTED`
- `double_booking -> E_LOAN_DOUBLE_BOOKED`
- `truncated_checkpoint -> E_CHECKPOINT_TRUNCATED`
- `version_mismatch -> E_STATE_VERSION_MISMATCH`

## 10. 实际验证记录

最终一轮完整输出保存在 `docs/validation-output.txt`；摘要如下：

- `go version`: `go1.22.12 linux/amd64`
- `go test ./...`: 全部包 `ok`
- `go test -race ./...`: 根包与 `internal/store` 均通过
- `go vet ./...`: 无输出
- 相同 seed 在两个不同目录/工作目录生成的 `project.v1.json`、`project.v2.json`、`project.v3.json`、`truth.json` 均 `cmp` 逐字节一致
- `maintctl verify -dir samples`: `ok=true`，7 项 check 全为 true
- `maintctl recover-demo`: `matches_full_plan=true`，committed wave 1，恢复后 7 条迁移与全量计划一致

## 11. 边界与后续可增强点

当前样本刻意保持整核/整千毫核，以便 v1 整核 schema 与 v2/v3 毫核 schema 严格等价。可行性 API 已支持调用方传入任意时刻的 `Used/Reserved/CurrentNode/Pending`；门面中的样本真值使用构造侧基线几何，波次和租约时间线则完整按时刻推进。后续若接入真实 Prometheus/CMDB，只需替换 `domain.Project` 的来源和检查点仓储，不需要改求解裁决规则。
