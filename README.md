# LockLab：事务隔离与锁等待领域组件

LockLab 是一个只依赖 Go 标准库的确定性事务隔离/锁等待实验组件。它同时提供：

- 可导入的 Go 领域 API；
- 固定种子的三 schema 事务工程生成器；
- 逐逻辑步锁判定、等待队列、超时/死锁、读可见性和最终写入真值；
- 增量失效、预算受限并行、原子检查点与异常注入；
- 本地 CLI 演示和可重复验收入口。

## 快速开始

要求 Go 1.21+。仓库不依赖第三方包、Docker 或外部 SQLite 驱动。

```bash
go test ./...
go vet ./...

go run ./cmd/locklab generate --seed 20261127 --out samples
go run ./cmd/locklab verify --dir samples

go run ./cmd/locklab demo --dir samples --checkpoint .demo-checkpoint
go run ./cmd/locklab incremental --dir samples --tx T8 --edit lock-mode
go run ./cmd/locklab parallel --dir samples --workers 2 --memory 262144 --batch 3 --seed 9
go run ./cmd/locklab faults --dir samples
```

中断与续算：

```bash
go run ./cmd/locklab demo --dir samples --checkpoint .demo-checkpoint \
  --stop-after-commits 3
go run ./cmd/locklab resume --dir samples --checkpoint .demo-checkpoint
go run ./cmd/locklab checkpoint-sql --checkpoint .demo-checkpoint \
  --out checkpoint.sql
```

`demo` 也处理 `SIGINT`/`SIGTERM`：退出时最后发布的提交标记始终对应最后一个完整提交点。

## 实际验证结果

固定种子 `20261127` 的样本包含 9 个事务、6 个数据项、44 个逻辑步。规范化摘要：

```text
8523a0fe2b3ac20f07b5d5e7b2da037beb38d61bbaa5c7bd288c07220878654d
```

`go run ./cmd/locklab verify --dir samples` 的实际结果：

```text
schema-loader: PASS (v1=v2=v3 ...)
truth: PASS (steps=44 commits=7 rollbacks=2 reads=10)
deadlock-victims: [T4]
rollback-reasons: [T4:deadlock,T7:explicit_rollback]
anomalies: dirty_read[T5 T6], non_repeatable_read[T2],
           nonserializable[T1 T2], nonserializable[T8 T9]
final-writes: accounts/A, accounts/B, accounts/C,
              inventory/I1, inventory/I2
incremental: PASS match=true prefix=36 recomputed=5 suffix=3 resync=41
checkpoint-resume: PASS last_commit=3 from_step=16 digest_match=true
parallel: PASS digest_match=true workers=4 observations=44
fault-matrix: PASS
overall: PASS
```

异常矩阵实际返回：

| 注入 | 业务码 | 定位 |
|---|---|---|
| 重复数据项 | `E_DUPLICATE_ITEM` | `$.items[6].id` |
| 线程预算越界 | `E_BACKPRESSURE_WORKERS` | `$.worker_budget` |
| 内存预算越界 | `E_BACKPRESSURE_MEMORY` | `$.memory_budget_bytes` |
| 失效等待图引用 | `E_WAITGRAPH_DANGLING` | `$.wait_graph[0].from` |
| 超时时刻倒序 | `E_TIMEOUT_REVERSED` | `$.transactions[0].operations[0].waitDeadlineSeconds` |
| 截断检查点 | `E_CHECKPOINT_TRUNCATED` | `$.checkpoint.commit` |
| 工程快照版本不匹配 | `E_PROJECT_VERSION_MISMATCH` | `$.checkpoint.project_id` |
| 检查点版本不匹配 | `E_CHECKPOINT_VERSION_MISMATCH` | `$.checkpoint.commit.store_version` |

所有注入只操作副本；矩阵会复跑基线并比较语义摘要，源工程和已提交锁/可见性结论均不改变。

另用本机 Python 内置 SQLite 3.46.1 对导出的 `checkpoint.sql` 做了独立导入验证：
6 张表均可查询，`checkpoint_meta` 记录提交序号与最后逻辑步；该验证不作为项目运行时依赖。

同一 seed 在两个目录重复生成后，`project-v1..3.json`、`truth.json`、`manifest.json`
均逐字节一致。

## 三版 Schema

同一规范化工程由生成器编码为三个版本：

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 字段命名 | `snake_case` | `camelCase` | `PascalCase` |
| 时间单位 | 毫秒 | 秒（小数，可精确换算为整数毫秒） | 毫秒 |
| 大小单位 | 字节 | KiB | 字节 |
| 等待超时 | 相对 `wait_timeout_ms` | 绝对 `waitDeadlineSeconds` | 相对 `WaitTimeoutMs` |
| 锁模式表达 | 顶层扁平 `lock_modes` 相容表 | 模式名直接引用 | 递归 `LockCompatibility` 相容树 |
| 死锁检测字段 | 必选 | `features` 内可选 | 顶层可选；缺省为 `false` |

无法消歧的输入不会猜测，例如重复 JSON 键、同时存在多个 schema 判别字段、同时存在相对/绝对超时、v3 相容树与规范矩阵不一致、秒值无法精确落为整数毫秒等。

## Go API

核心入口均位于模块根包 `locklab`：

```go
project := locklab.GenerateProject(20261127, locklab.DefaultGenerateOptions())

v1, _ := locklab.EncodeProject(project, 1)
loaded, _ := locklab.LoadProject(v1)

result, _ := locklab.Run(loaded, locklab.RunOptions{})
truth, _ := locklab.GenerateTruthForProject(loaded)
_ = locklab.VerifyTruth(loaded, truth)

baseline, _ := locklab.BuildBaseline(loaded)
incremental, _ := locklab.RunIncrementalWithBaseline(baseline, edited, "T8")

parallel, _ := locklab.RunParallel(loaded, locklab.ParallelOptions{
    Workers: 2, MemoryBudgetBytes: 262144, BatchSize: 3, ScheduleSeed: 9,
})

store, _ := locklab.OpenCheckpointStore(".checkpoint")
_, _ = locklab.Run(loaded, locklab.RunOptions{OnCommit: store.Observer()})
resumed, info, _ := locklab.ResumeOrRecompute(loaded, store, locklab.RunOptions{})
```

主要文件：

- `loader.go` / `encoder.go`：三版 schema、严格解析和归一化校验。
- `generator.go` / `truth.go`：固定种子样本和真值。
- `locks.go` / `engine.go` / `visibility.go`：锁矩阵、等待队列、执行、可见性和异常推导。
- `incremental.go`：缓存轨迹、失效闭包、状态汇合和后缀拼接。
- `parallel.go`：预算受限并行准备和确定性协调。
- `checkpoint.go`：原子检查点、恢复和 SQLite SQL 导出。
- `faults.go`：异常矩阵。
- `samples/`：已生成的三版样本、真值和清单。
- `docs/design.md`：详细协议和判定说明。

## 判定语义摘要

- 全局逻辑步按 `(AtMs, 事务声明顺序, local_seq)` 固定排序；阻塞事务的后续步骤进入延迟队列，解锁后按声明顺序继续。
- 锁资源覆盖表级/行级；模式覆盖 `IS`、`IX`、`S`、`SIX`、`X`；相容表同时处理同层模式和表级锁对子孙行锁的约束。
- 冲突请求进入每资源 FIFO 等待队列，并获得当前席位序号；`no_wait` 请求立即返回冲突拒绝。
- 等待超时统一换算为整数毫秒；同一绝对截止时刻的多个到期等待先全部冻结，再统一回滚和唤醒。
- 死锁检测由等待图环路驱动；环路内优先级数值最小者被回滚，平局按事务 ID 稳定裁决。无环不产生回滚。
- `READ_UNCOMMITTED` 可观察未提交版本；`READ_COMMITTED` 每次读取当前已提交快照，读锁在语句后释放；`REPEATABLE_READ`/`SERIALIZABLE` 使用事务开始快照并保留读锁到事务结束。
- 写版本在提交时获得全局提交序号；最终写入集合只包含已提交版本。脏读、非重复读和不可串行化均由实际读取/写入/提交事实推导。

## 增量、并行与检查点

- 增量：从首个失效逻辑步之前的缓存状态继续；编辑事务全部消费后，若运行状态指纹重新汇合且调度尾部一致，直接拼接旧后缀。报告包含直接来源、传播闭包、每步复用/重算理由和受影响等待队列区间。结果必须与一次性全量执行逐项一致。
- 并行：工作线程在预算内并行执行逐步准备；锁仲裁仍由单一日志流确定性协调。线程数、batch、shuffle seed 不影响语义，每步实际耗时单独作为诊断观测值。
- 检查点：先 fsync 不可变状态体，再原子发布包含版本、工程摘要、校验和、全局提交序号与最后提交步的 marker。恢复只接受版本/工程匹配的 marker；版本不匹配自动从起点重算，截断/损坏返回硬错误。

Go 标准库没有 SQLite 驱动，因此运行时采用上述本地崩溃安全格式；`checkpoint-sql` 会导出可直接交给 SQLite 的表结构、事务/锁状态、等待图、等待队列、已提交逻辑步和最后写入的 `checkpoint_meta` 提交行，且 `checkpoint_meta` 最后插入。

## 测试

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试覆盖三版装载/执行一致、锁矩阵、等待与死锁、超时表达、读可见性、增量三类编辑及重排、预算背压、检查点续算/截断/版本回退、异常矩阵和源不可变性。
