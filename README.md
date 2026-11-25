# 列批增量重写、依赖求解与扫描失效重建

这是一个可独立集成的 Go 1.22+ 领域组件与本地可重复演示。它不连接真实对象存储、远端缓存或线上服务；全部样本、三版 schema、逐操作真值和扫描真值均由固定种子 `20261126` 在本地生成。

## 快速开始

```bash
go version                 # 需要 Go 1.22+
go test ./...
go run ./cmd/columnar-demo --out artifacts/demo
```

SQLite 使用纯 Go 驱动 `modernc.org/sqlite`，检查点写入本地文件，不需要 CGO 或外部数据库服务。
默认输出包含 `v1.json`、`v2.json`、`v3.json` 与生成器逐操作/逐扫描真值 `truth.json`。

查看单个异常注入：

```bash
go run ./cmd/columnar-demo --inject patch_range
go run ./cmd/columnar-demo --inject dependency_cycle
go run ./cmd/columnar-demo --inject buffer_budget
```

## 组件边界

| 包 | 职责 |
|---|---|
| `domain` | 归一化列批/操作/扫描模型、稳定 `E_xxx` 业务错误、定宽摘要与饱和整数运算 |
| `generator` | 固定种子样本、逐操作参考真值、逐扫描缓存真值、三版 schema 编码 |
| `loader` | 严格解析 v1/v2/v3、单位换算、字段映射、跨版本结构冲突检测 |
| `engine` | 更新推进、依赖图、摘要校验、扫描失效重建、增量编辑重放、预算调度 |
| `checkpoint` | SQLite 原子检查点、版本/内容指纹校验、截断检测、崩溃恢复 |
| `anomaly` | 可重复的异常注入用例及预期业务码 |
| `cmd/columnar-demo` | 三版装载、逐操作/逐扫描展示、增量差异、恢复对照、异常演示 |

## 三版 schema 映射

同一逻辑工程包含 3 个列族、8 个初始列批、11 个按逻辑顺序执行的操作和 8 个扫描请求。

| 表达 | v1 | v2 | v3 |
|---|---|---|---|
| 列批布局 | 按列族分组的扁平批列表 | 按列族分组的嵌套 `segments` 树 | 全局扁平 `batch_table` |
| 时间 | tick | ms | tick |
| 容量 | byte | KiB | byte |
| 区间补丁 | 无；以整批替换表达同一逻辑结果 | `mutations[].range` | `change_log[].row_window` |
| 数据更新 | `replace` / `delete` / `compact` | `replace` / `patch` / `delete` / `compact` | 同 v2 |
| 可选失效标记 | `invalidated` | 字段不存在，默认 false | `stale` |

装载后统一为：

- 时间统一为毫秒，容量统一为字节；
- 列族顺序、列批序号、行区间、有效行数、编码、压缩字节数、摘要、来源编号和依赖集合一致；
- v1 的整批替换与 v2/v3 的区间补丁在状态效果上等价，引擎会逐操作比较最终批量状态；
- 未知版本、未知字段、缺失必填字段、单位与字段组合不符均返回 `E_SCHEMA_*` / `E_UNIT_CONFLICT`，不会静默忽略。

跨版本装载可用：

```go
projects, err := loader.LoadAll(v1JSON, v2JSON, v3JSON)
results := make([]*domain.RunResult, 0, 3)
for _, p := range projects {
    r, err := engine.Run(p, engine.Options{})
    // ...
    results = append(results, r)
}
err = engine.VerifyVersionEquivalence(results)
```

## 更新语义

操作严格按 `Index` 递增提交：

- `replace`：整批替换有效行数、压缩字节数和摘要；保留列批身份。
- `patch`：半开区间 `[From,To)` 必须落在当前有效行数内；更新 `Rows += RowDelta`、`Bytes += BytesDelta`，摘要使用定宽混合运算；越界、结果行数为负、字节下溢有不同错误码。
- `delete`：列批状态变为 `deleted`，行数/字节/摘要归零；此后打补丁返回 `E_BATCH_DELETED`。
- `compact`：源列批变为 `superseded`，新列批继承源依赖并追加源编号，记录 `SourceID`；摘要必须等于确定性重写摘要，否则返回 `E_COMPACT_DRIFT`。

摘要和行数始终以声明位宽运算：

- 摘要每步 `MaskWidth` 到声明位宽；
- 字节/行数使用显式溢出与饱和检测；
- 每个操作结果包含 `DigestCheck`，记录期望/实际摘要、Hamming 距离、行数偏差、是否在容差内，以及超差归因；
- 扫描缓存失效判定依据列批版本和依赖闭包，不使用可能累计偏差的浮点近似，因此多次补丁/压缩不会改变失效集合。

## 依赖与扫描

依赖图逐操作保存。每条边包含前驱列批、列族、状态、来源编号和反向依赖者。初始批次的列族前驱可由 `engine.DeriveFamilyPredecessors` 推导；压缩新批继承源依赖并增加源边。依赖环、缺失前驱、指向已删除列批的依赖分别返回：

- `E_DEP_CYCLE`
- `E_BATCH_UNKNOWN`
- `E_DEP_DELETED`

扫描按列裁剪、列族和半开行谓词选批：

- 只有 `active` 且与谓词有行交集的列批可读；
- 操作改写列批后，该列批及其传递依赖者进入缓存失效闭包；
- 扫描只重建失效列批，未失效列批复用；
- 输出 `ReadBatches`、`DecodedRows`、`RebuiltBatches`、`ReusedBatches`、`InvalidSources`、冷缓存/命中标志；
- `InvalidSources` 使用操作序号，`0` 表示冷缓存。

样本扫描真值示例：

```text
scan-1 read=[100 101 102 200 201] rows=342 invalid=[0 1]
scan-2 read=[100 101 200 201] rows=474 invalid=[2 3]
scan-6 read=[200] rows=100 cache_hit=true
scan-8 read=[102 901 903] rows=242 rebuilt=[901] invalid=[11]
```

## 增量编辑

`engine.Editor` 与 `engine.RunIncremental` 支持：

- 修改补丁区间与编码；
- 修改批次初始编码；
- 移动列批到其他列族；
- 增加或删除扫描；
- 修改操作依赖。

只重放依赖失效闭包内的操作和扫描，返回：

- `ClosureBatchIDs`：失效传播闭包边界；
- `RecomputedOps` / `ReusedOps`；
- `RecomputedScans` / `ReusedScans` / `AddedScans` / `RemovedScans`；
- 每处结果来自重算还是缓存复用。

测试和演示均把增量结果与一次性全量 `Run` 做逐操作、逐扫描比较。

## 并发、预算与背压

每次扫描的失效列批解码使用真实 goroutine 并行。调度先按确定性的“波次 + 字节容量”装箱，再启动不超过 worker 预算的并发任务：

- worker 为 0 返回 `E_BUDGET_WORKERS`；
- 单批所需缓冲超过单批上限或总容量返回 `E_BUDGET_BUFFER`；
- 无法加入当前波次的列批产生可观测 `BackpressureEvents` / `BackpressuredBatches`，不会丢批或越界分配；
- `engine.SolvePatches` 对互不相同的列批并行求解补丁，共享同一预算与背压语义；逻辑提交仍按操作序号串行以保持事务顺序。

## SQLite 检查点

每次操作提交和扫描后可写入本地 SQLite。一个 `Save` 事务内严格按以下顺序执行：

1. 清空旧快照；
2. 写入全部列批状态；
3. 写入扫描缓存索引；
4. 最后更新 `committed_op`、schema/engine 版本和内容指纹；
5. 提交事务。

因此任意提交点被强制终止时，SQLite 只暴露上一个完整事务。测试会启动子进程，在“列批已写、全局序号未写”处直接 `os.Exit(23)`，重启后仍只能读到上一个已提交操作。

恢复校验包括：

- `PRAGMA quick_check`；
- schema 版本、engine 版本和内容指纹；
- SQLite 头、文件截断和损坏；
- 不完整提交元数据。

对应错误码：`E_CHECKPOINT_TRUNCATED`、`E_CHECKPOINT_VERSION`、`E_CHECKPOINT_CORRUPT`、`E_CHECKPOINT_WRITE`。

## 异常注入

演示内置以下稳定用例：

| 用例 | 预期业务码 |
|---|---|
| 重复列批序号 | `E_BATCH_DUPLICATE` |
| 补丁区间越界 | `E_PATCH_RANGE` |
| 单位/字段组合冲突 | `E_UNIT_CONFLICT` |
| 依赖成环 | `E_DEP_CYCLE` |
| 依赖已删除列批 | `E_DEP_DELETED` |
| 压缩重写摘要漂移 | `E_COMPACT_DRIFT` |
| 越界线程预算 | `E_BUDGET_WORKERS` |
| 越界缓冲预算 | `E_BUDGET_BUFFER` |
| 截断检查点 | `E_CHECKPOINT_TRUNCATED` |
| 检查点版本不匹配 | `E_CHECKPOINT_VERSION` |

所有异常路径都在项目/检查点副本上运行；源工程不被修改，失败事务不会让上一提交列批不可读。

## 实际验证结果

在 Linux + Go `1.22.12` 下实际执行：

```text
$ go test ./...
?    example.com/columnar/anomaly       [no test files]
?    example.com/columnar/cmd/columnar-demo [no test files]
ok   example.com/columnar/checkpoint
ok   example.com/columnar/domain
ok   example.com/columnar/engine
ok   example.com/columnar/generator
ok   example.com/columnar/loader
```

演示实际输出摘要：

```text
version-equivalence: OK (operations, digests, dependencies and scans)
closure=[901] recomputed_ops=[11] reused_ops=[1 2 3 4 5 6 7 8 9 10]
recomputed_scans=[scan-8] reused_scans=[scan-1 scan-2 scan-3 scan-4 scan-5 scan-6 scan-7]
added_scans=[scan-9]
incremental-vs-full: OK
checkpoints_written=22 committed_op=11 recovered_batches=11 scan_cache=8 integrity=ok
rollback-kept-committed-op=11 batches-readable=11
version-mismatch=E_CHECKPOINT_VERSION
truncated-checkpoint=E_CHECKPOINT_TRUNCATED
```

## 当前边界

这是首个可验收的本地领域实现，不包含真实对象存储适配、分布式缓存、多进程调度或生产级 SQLite 调优。正式接入时可用同一 `domain.Project` / `RunResult` / `CheckpointData` 边界替换存储和缓存实现，确定性样本与参考真值可继续作为兼容性回归基线。
