# 多租户配额账本内核实现说明

本目录是仅依赖 Go 标准库的可验收内核。模块名为 `quotakernel`，领域代码在
`internal/kernel`，诊断工作台在 `cmd/quota-diag`。

## 快速运行

```bash
go version
go test ./...
go test -race ./...
go test ./internal/kernel -cover
go run ./cmd/quota-diag all --seed 42 --out samples --artifacts artifacts
go run ./cmd/quota-diag diagnose --seed 42
```

固定种子 `42` 会生成：

- `samples/project-v1.json`：扁平表、字节、tick、绝对额度。
- `samples/project-v2.json`：机架嵌套树、KiB、毫秒、千分比、项目级突发比例默认值。
- `samples/project-v3.json`：kebab-case、机架树、KiB、毫秒、万分比、突发 BP。
- `samples/truth.json`：顺序参考内核产出的逐时刻真值。
- `samples/critical-cases.json`：刻度/单位/溢出/容差临界用例。
- `artifacts/checkpoint.sqlite`：通过 Python `sqlite3` 校验的真实 SQLite 3 文件。
- `artifacts/verification-report.json`、`artifacts/diagnostic-report.json`：机器可读验收报告。

## 规范模型

三个 schema 均映射到 `internal/kernel/model.go` 中的 `Project`：

- 容量统一为 `Node.CapacityBytes int64`。
- 时刻统一为 `Event.Tick int64`，`Project.TickMS` 声明一个 tick 的毫秒数。
- 权重统一为万分比 `Tenant.WeightPermyriad`。
- 配额、保底、突发统一为字节整数。
- 租户硬上限为 `QuotaBytes + BurstLimitBytes`。

装载器在 `loader.go`、`loader_versions.go`、`loader_v3.go` 中手工遍历 JSON 对象，
每个对象都有允许字段清单；未知字段返回 `E_UNKNOWN_FIELD` 和 JSON 路径。互斥别名
同时出现时返回 `E_FIELD_COMBINATION_CONFLICT`。

## 账本与公平性

核心顺序实现在 `truth.go`：

1. 同一 tick 先应用权重事件和释放事件。
2. 将申请按保底层、配额层、突发层拆成顺序层。
3. 同层按声明优先级、有效权重、保底份额、租户 ID、申请 ID 做全序排序。
4. 节点可用量先扣除其他租户在该节点保底归属上的未使用保底余量。
5. 不足部分进入每租户等待队列，并记录：
   - `invader_tenant_id`：可能侵占保底的申请租户；
   - `protected_tenant_ids`：被保护的保底租户；
   - `constraint_id=C-GUARANTEE-FLOOR`；
   - `retry_after_tick`：后续释放时刻。
6. 超出 `Quota+Burst` 的部分直接以 `E_HARD_CAP_EXCEEDED` 拒绝。

排序键不依赖 map 迭代、租户切片顺序或 worker 完成顺序。测试会随机打乱租户、节点和
同 tick 事件顺序，并以 1/2/4/8 并发度比较终态。

## 闸门、延迟和并行

`gate.go` 提供显式 `Gate` 和保守预扣计算；`truth.go` 的批量调度器在每个排序后的
分层申请上执行同一规则。

- 精确路径估计成本为 3ms；预算不足时选择 1ms 的节点/硬上限快速预扣路径。
- 预扣额先检查 `used + held + hold <= Quota + Burst`，因此不会越过硬上限。
- 预扣随后转换为精确账本结算。
- `VerifyPathEquivalence` 对 exact、pre-deduct、parallel 三条路径比较全部时刻、
  账本、节点、队列、准入顺序和申请结论。
- `WorkerPool.TrySubmit` 在线程槽耗尽时返回
  `E_BACKPRESSURE_WORKER_EXHAUSTED/C-WORKER-BUDGET`。
- 每租户等待队列达到预算后，新申请按稳定公平顺序得到
  `E_BACKPRESSURE_QUEUE_EXHAUSTED/C-PER-TENANT-QUEUE-LIMIT`。

## 定点换算

`fixed.go` 全程使用 int64 定点整数：

- 千分比 ×10 → 万分比，反向使用四舍五入。
- KiB ×1024 → 字节；字节可转 Q16 KiB 定点值。
- tick × `TickMS` → 毫秒；毫秒转 tick 时显式检查对齐。
- 乘法使用 `CheckedMul`，临界 128 位路径使用 `bits.Mul64/Div64`。
- 溢出返回 `E_FIXED_POINT_OVERFLOW`，饱和函数显式返回 saturated 标记。

装载期完成一次单位/刻度换算；推进期只做整数加减，因此不会逐 tick 累积浮点误差。
长周期测试运行 300 个申请/释放周期并比较三条路径。

## 增量失效

`incremental.go` 支持四类编辑：

- `node_capacity`
- `tenant_quota`
- `tenant_guarantee`
- `weight_event`

每次编辑在克隆工程上校验；校验失败不会替换当前工程或缓存。成功后输出
`InvalidationReport`：

- `source_key`：失效来源；
- `closure`：来源、实体状态和 `tick:N` 的传播闭包；
- `provenance[tick:N]`：该结果依据的来源与前一时刻状态链；
- `recomputed_ticks` / `reused_ticks`；
- cache hit/miss 数。

前缀时刻直接复用 `ResultCache`，后缀从缓存中的前一时刻账本、节点占用和等待队列播种。
测试断言每类编辑的增量结果与编辑后一次性全量重算完全一致。循环因果引用由
`validate.go` 的 `CausalCycle` 输出 cycle 路径和 `E_CAUSAL_REFERENCE_CYCLE`。

## SQLite 检查点

Go 标准库没有 SQLite 驱动，而任务要求仅用标准库。因此 `sqlite_native.go` 实现了一个
最小 SQLite 3 写入器和 meta 页读取器：

- 65536 字节页；
- 合法 `SQLite format 3\0` 文件头；
- table leaf b-tree 页和 SQLite varint/record 编码；
- `meta`、`tenant_state`、`node_state`、`hold`、`queue_item` 表；
- `meta.bundle_json` 保存完整结构化检查点，供 Go 恢复。

生成文件已用 Python 内置 SQLite 验证：

```text
PRAGMA integrity_check => ok
```

`SaveCheckpoint` 先写同目录临时文件、`Sync`、再原子 `rename`。强制终止只会留下旧的
完整数据库或临时文件，不会把主路径写成半文件。逻辑提交顺序记录在 `CommitOrder` 和
`.sql` 旁路脚本中：

1. tenant states
2. node states
3. holds
4. queues
5. global committed tick

`LoadCheckpoint` 对短文件返回 `E_CHECKPOINT_TRUNCATED`，格式版本不匹配返回
`E_CHECKPOINT_VERSION_MISMATCH`。`RecoverOrRun` 遇到版本、工程指纹不匹配或损坏时从
tick 0 整段重算；匹配时从 `CommittedTick+1` 续算。测试遍历全部 tick，验证每个提交点
后的后缀都与全量真值一致。

`signals.go` 将 SIGINT/SIGTERM/SIGHUP/SIGQUIT 归类为可完成当前 tick 的优雅信号；
SIGKILL/SIGSTOP 无法被进程捕获，按强制终止模型处理，恢复只依赖已经原子落盘的检查点。

## 稳定业务码

主要异常码集中在 `codes.go`，诊断套件当前覆盖 17 组断言：

- 装载：`E_UNKNOWN_FIELD`、`E_SCHEMA_VERSION_UNSUPPORTED`、
  `E_FIELD_COMBINATION_CONFLICT`、`E_NEGATIVE_QUOTA`、
  `E_GUARANTEE_EXCEEDS_QUOTA`、`E_BURST_PERCENT_OUT_OF_RANGE`、
  `E_EVENT_TICK_OUT_OF_ORDER`、`E_BURST_EXPRESSION_CONFLICT`。
- 运行：`E_HARD_CAP_EXCEEDED`、`E_NODE_CAPACITY_EXHAUSTED`、
  `E_GUARANTEE_INVASION_BLOCKED`、`E_BACKPRESSURE_QUEUE_EXHAUSTED`、
  `E_BACKPRESSURE_WORKER_EXHAUSTED`。
- 参数/因果：`E_WORKER_BUDGET_INVALID`、`E_QUEUE_BUDGET_INVALID`、
  `E_CAUSAL_REFERENCE_CYCLE`。
- 定点：`E_FIXED_POINT_OVERFLOW`、`E_TOLERANCE_EXCEEDED`。
- 检查点：`E_CHECKPOINT_TRUNCATED`、`E_CHECKPOINT_VERSION_MISMATCH`、
  `E_PROJECT_FINGERPRINT_MISMATCH`。

所有 `BizError` 都可携带 JSON path、租户、节点、事件、约束号和结构化 details。
