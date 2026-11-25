# 多版本可见性内核与诊断工作台

本交付是一个纯本地、确定性的 Go 领域组件与诊断工作台。固定种子 `424242` 生成三批事务工程；每批都有独立构造真值，覆盖可见版本、跳过原因、写入/冲突、提交序、可见性切面、影响读集合和增量 GC 决策。

## 快速运行

```bash
go mod init mvccworkbench          # 仓库若尚未初始化时执行；当前交付已含 go.mod
go get github.com/mattn/go-sqlite3@v1.14.24
make test                          # 或 GOPROXY=off go test -count=1 ./...
make verify                        # 三批 × canonical/v1/v2/v3 顺序真值验收
make verify-parallel               # 同样目标的有界并行验收
make serve                         # http://127.0.0.1:18080
go run ./cmd/mvccwb repl           # 交互式 REPL
```

环境记录：Go `1.22.12 linux/amd64`、SQLite CLI `3.50.2`、gcc 可用。SQLite 驱动使用 CGO 版 `github.com/mattn/go-sqlite3 v1.14.24`，模块依赖已在本地模块缓存中，验证时 `GOPROXY=off` 可通过。

## 包结构

- `internal/domain`：canonical 工程模型、稳定 `E_*` 错误码、输入校验。
- `internal/schema`：三套外部 schema 的 DTO、编码、检测与无损装载。
- `internal/generator`：固定种子样本与“构造期真值” oracle；真值不调用 `internal/engine`。
- `internal/engine`：快照、可见性、写追加、冲突、提交序、GC、顺序/并行执行器。
- `internal/checkpoint`：SQLite 检查点、原子保存、版本校验、截断/坏快照注入。
- `internal/incremental`：编辑后的依赖闭包、缓存复用/重算来源、与全量重算对比能力。
- `internal/diagnostics`：输入异常注入 API。
- `internal/workbench`：CLI/Web 共用应用服务、崩溃恢复与信号安全执行器。
- `cmd/mvccwb`：CLI、REPL 和 Web 服务入口。
- `tests`：跨包验收测试。

## 三版 Schema 映射

| 版本 | 命名 | 事务号基准 | 时间单位 | 链组织 | 删除事务号 |
| --- | --- | --- | --- | --- | --- |
| `mvcc.project/v1` | snake_case | 全局单调 `txn_id` | tick，`1 tick = 10ms` | 扁平 `versions` | 显式 `delete_txn`，`null`/`0` 归一为未定 |
| `mvcc.project/v2` | camelCase | `commitSequence`，未提交/回滚事务用稳定 alias | ms | `versionChainTrees` 分组树 | `removedBy` 相对提交序号/别名引用 |
| `mvcc.project/v3` | kebab-case | 全局单调 `txn-id` | ms | `version-chains` 分组树 | DTO 完全无删除字段，按后继节点 `create-txn` 恢复前驱删除标记 |

`schema.LoadDocument` 自动识别 `schema_version`/`schemaVersion`/`schema-version`。未知版本或已知版本中的不支持特性组合返回 `E_SCHEMA_UNKNOWN` / `E_SCHEMA_BAD_COMBINATION`，错误对象带 `path` 与稳定 `context`。

Canonical 模型统一使用逻辑毫秒、全局事务号和 `*uint64` 删除事务号；`nil` 是唯一的“未定删除”内部表示，绝不会被当成已删除。

## 可见性与快照

- `READ_COMMITTED`：每条读生成 `SNAP-R-<op>` 语句快照。
- `REPEATABLE_READ` / `SERIALIZABLE`：同一事务首次读生成 `SNAP-T-<txn>-SS`，后续读复用。
- 快照只包含声明逻辑流中该读之前已提交的事务集合，不读取挂钟，也不依赖 goroutine 实际调度顺序。
- 从链尾向链头扫描，跳过：
  - `SKIP_UNCOMMITTED`
  - `SKIP_ROLLED_BACK`
  - `SKIP_TXN_AFTER_SNAPSHOT`
  - 显式删除事务已在快照中时额外使用 `SKIP_DELETED_IN_SNAPSHOT`

## 写入、冲突与 GC

- 写操作按逻辑顺序追加版本，并把前驱 `DeleteTxn` 标记为当前写事务。
- 回滚事务的版本保留在链上，判定为 `SKIP_ROLLED_BACK`，不会被选中。
- `FIRST_COMMITTER_WINS` 规则下，重叠写事务按最终提交序决定 winner/loser；`ConflictDecision.CompetingTxns` 给出双方事务号。
- 同逻辑时刻提交按 `GLOBAL_TXN_ID_ASC` 或 `START_MS_ASC_THEN_TXN_ID` 导出唯一 `CommitSeq`。
- 每个提交输出有序已提交集合、各数据项可见头版本的 `VisibilityAspect` 和依赖该事务/版本的 `AffectedReadOps`。
- GC 步骤是操作流中的显式 `GC` 操作，逐步推进。每个决策输出 `OldestSnapshotID`、`ActiveSnapshotIDs`、`ProtectedVersions`、候选与实际回收版本。
- `SNAP-HORIZON` 是始终存在的最新已提交切面；长事务/语句快照存活时保护其可见版本。

## 并行与背压

`engine.RunParallel` 按数据项分片，worker 信号量限制实际并发；结果按声明的数据项与操作顺序合并。不同 worker 数（测试覆盖 `1/2/4/8`）下读、写、提交、冲突和 GC 决策均与真值逐项一致。

可观测背压码：

- `E_BACKPRESSURE_WORKER_BUDGET`：worker 槽位耗尽。
- `E_BACKPRESSURE_ITEM_BUFFER`：每数据项待处理缓冲满。
- `E_BACKPRESSURE_CHAIN_CAP`：每数据项链长度超过声明上限。

`Report.Metrics[itemID]` 记录每数据项实际耗时；该值只用于诊断，不进入任何比较或缓存指纹。

## SQLite 检查点与恢复

表：

- `checkpoint_meta`：工程/canonical/检查点版本、批次、seed、全局 `committed_op_seq`。
- `checkpoint_chains`：每数据项链 JSON 与回收 ID。
- `checkpoint_snapshots`：活跃快照 JSON，快照载荷自带版本。

`Store.Save` 在同一 SQLite 事务内先写 chains、再写 snapshots、最后 upsert `committed_op_seq`。SQLite 使用 `_synchronous=FULL`。进程强杀时未提交事务由 SQLite 回滚，因此不会发布半操作序号。

恢复策略：

- 版本匹配：`RecoveryResumed`，从 `committed_op_seq+1` 续算。
- meta 版本不匹配：`E_CHECKPOINT_VERSION_MISMATCH`，工作台 `Recover` 清空旧 canonical 链并整段重放。
- 快照载荷版本不匹配：`E_SNAPSHOT_VERSION_MISMATCH`；可调用 `QuarantineIncompatibleSnapshots` 隔离注入行，最后完整 chains/seq 仍可读。
- 截断 JSON/物理损坏：`E_CHECKPOINT_TRUNCATED` / `E_CHECKPOINT_CORRUPT`，可用源工程整段重放恢复服务。
- `workbench.RunCheckpointed` 每完成一个操作即落盘；context 被 SIGINT/SIGTERM 取消时用独立超时 context 再 flush 最后完整点，并返回 `E_RUN_INTERRUPTED`。

## 增量失效

`incremental.Service` 深拷贝当前工程后应用编辑，源 `domain.Project` 不被修改。事务开始时刻或隔离级别变更会传播到：

1. 该事务拥有的读；
2. 引用该事务创建版本的其他读；
3. 活跃集合包含相关快照 ID 的 GC 步骤。

每个结果标为 `REUSED_FROM_CACHE`、`RECOMPUTED_DEPENDENCY` 或 `NEW_OPERATION`，并重算结果带 `invalidated_by` 来源链。测试验证增量结果与对编辑后工程执行一次性 `engine.Run` 的全量结果逐项一致，同时确认前缀结果仍为缓存复用。

## 异常码

输入类：`E_SCHEMA_UNKNOWN`、`E_SCHEMA_BAD_COMBINATION`、`E_INPUT_DUPLICATE_ITEM`、`E_INPUT_DUPLICATE_TXN`、`E_INPUT_DUPLICATE_VERSION`、`E_INPUT_BAD_DELETE_REF`、`E_INPUT_BAD_CREATE_REF`、`E_INPUT_TIME_OUT_OF_RANGE`、`E_INPUT_INVALID_OPERATION`、`E_INPUT_INVALID_ISOLATION`、`E_INPUT_INVALID_REFERENCE`。

检查点/状态类：`E_CHECKPOINT_OPEN/READ/WRITE/CORRUPT/TRUNCATED/VERSION_MISMATCH`、`E_SNAPSHOT_VERSION_MISMATCH`、`E_RUN_INTERRUPTED`。

所有 `*domain.Error` 均含稳定 `code`、可读消息、`path` 和排序友好的 `context`。

## 诊断工作台

```bash
go run ./cmd/mvccwb serve --addr 127.0.0.1:18080
go run ./cmd/mvccwb repl
```

Web API：

- `GET /api/batches`
- `GET /api/verify?batch=...&schema=mvcc.project/v2&parallel=1`
- `GET /api/document?batch=...&version=mvcc.project/v3`
- `GET /api/inject?batch=...&kind=duplicate_item`
- `GET /api/crash-resume?batch=...`

CLI 还支持 `verify-all`、`truth`、`inject`、`checkpoint-demo`、`checkpoint-inject`、`run-safe` 和 `incremental-demo`。
