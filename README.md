# 多租户共享集群配额账本

这是一个可独立集成的 Go 领域组件，覆盖三版配额工程装载、逐逻辑时刻账本推进、突发超卖防护、保底公平准入、定点精确换算、增量失效、并行结算、SQLite 检查点和本地故障注入演示。实现不依赖容器、网络服务或外部账号。

## 环境与快速开始

```bash
go version                 # 已用 go1.22.12 验证
go test ./...
go run ./cmd/quotademo -seed 42 -workers 4 -queue 64 -commit-stop 2
```

依赖只有标准库和纯本地 SQLite 驱动 `modernc.org/sqlite v1.29.10`。`go mod tidy` 已整理为直接依赖。

生成随机但按固定种子确定的样本并落盘：

```bash
go run ./cmd/quotademo -seed 42 -artifacts artifacts/demo
```

会写出 `v1.json`、`v2.json`、`v3.json`、`truth.json`。真值由 `gen` 包中的独立参考状态机构造。

## 目录

```text
quota/                  可集成领域组件
  model.go              统一租户、节点、事件和校验模型
  loader.go             v1/v2/v3 装载与单位/刻度/拓扑归一化
  fixed.go              有符号定点换算、溢出饱和与精度报告
  engine.go             账本推进、公平准入、等待队列、并行观测
  incremental.go        定时编辑、前缀复用、失效闭包
  checkpoint.go         SQLite 原子检查点
  checkpoint_run.go     逐时刻提交与中断恢复
gen/                    确定性生成器、三版编码、独立真值参考机
acceptance/             端到端验收测试
cmd/quotademo/          本地可重复演示与异常注入入口
```

## 三版 Schema

三个版本表达同一语义，装载后经 `Project.NormalizedClone` 比较：

| 维度 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 配额刻度 | 千分比整数 | 万分比整数 | 万分比整数 |
| 容量单位 | 核 | 毫核 | 毫核 |
| 时间单位 | 秒 | 毫秒 | 毫秒 |
| 拓扑 | 扁平节点表 + `rack_id` | 扁平节点表 + `rack_id` | `racks` 分组拓扑树 |
| 字段风格 | `tenant_id/quota_permille/...` | 长字段名与显式单位 | 紧凑字段名 |
| 突发上限 | 可选 `burst_permille` | 可选 `burst_basis_points` | 可选 `burst_bp` |
| 申请/释放量 | 核 | 毫核 | 毫核 |

装载器将缺失的突发上限映射为 `0`，并把所有事件规范化为 `(time_millis, seq)` 顺序。未知版本、未知字段组合、负配额、刻度越界和节点保底总和超容量都会返回带路径上下文的稳定业务码。

## 账本语义

内部所有容量、配额和时间均为 `int64` 毫核/毫秒。租户配额与保底均为万分比：

```text
node_quota = floor(node_capacity_mc * quota_bp / 10000)
node_weighted_quota = floor(node_quota * weight_permille / 1000)
tenant_hard_cap = Σ(eligible node quota + eligible node burst)
available = tenant_hard_cap - used
guarantee_remaining = Σ max(0, node_guarantee - tenant_usage_on_node)
```

逻辑时刻处理顺序固定为：

1. 应用权重事件；若新高权重会让已用量超出新硬上限，事件返回 `E_INVALID_EVENT`。
   权重取值范围为 `(0,4000]` 千分比；若加权后的节点保底总和超过容量，事件返回 `E_GUARANTEE_OVER_CAPACITY`。
2. 应用释放事件；释放不得超过当前占用。
3. 合并新申请与既有等待队列，统一按以下次序排序：
   - 先保底类申请，后突发类申请；
   - 同类别内按声明优先级从高到低；
   - 再按保底份额从高到低；
   - 再按租户 ID 和事件序号稳定决胜。
4. 每个候选依次检查租户硬上限、节点容量和其他租户未使用的保底预留。
5. 被拒绝的新申请进入该租户等待队列；队列中的请求在后续时刻继续重试。

约束编号：

- `C1_TENANT_HARD_LIMIT`：租户已用加申请超过配额加突发上限。
- `C2_NODE_CAPACITY`：节点已用加申请超过节点容量。
- `C3_GUARANTEE_RESERVE`：节点尚有容量，但允许突发会把其他租户未用的保底预留挤掉。

每个 `Decision` 同时输出 `constraint`、`order_basis` 和可读原因，因此拒绝原因可追溯到排序类别、优先级、保底份额和事件序号。

## 定点换算与精度

`ScaleRatio`、`ConvertUnit` 和 `QuotaForNode` 使用 `int64` 值与 `math/bits.Mul64` 做乘除前后的显式检查：

- 负数返回 `E_NEGATIVE_QUOTA`。
- 乘法高位非零或结果超出 `int64` 时标记 `Overflow=true`、`Saturated=true`、`ErrorCode=E_OVERFLOW`，结果饱和为 `MaxInt64`。
- `CheckPrecision` 输出真值、差值、声明容差（本组件为 0 毫核）、`ExceedsTolerance` 和 `Accepted`；溢出或超容差结果不会被沿用。
- `QuotaForNode` 记录中间值：`capacity*ratio`、比例基础配额、乘权重后的值、最终值。

v1 的千分比数值必须能被精确提升为万分比；生成器保证所有声明是 10 的倍数。三个版本装载后对同一节点和权重计算出的毫核配额完全相同。

## 增量失效

`ApplyEdit` 支持：

- `EditNodeCapacity`：修改节点容量。
- `EditTenantQuota`：修改租户配额。
- `EditTenantGuarantee`：修改租户保底。
- `EditWeightEvent`：按事件序号修改某时刻权重。

编辑生效时刻之前的时刻直接复用缓存；生效时刻及之后重放。由于等待队列和保底余量具有跨时刻传播，闭包显式延伸到当前时间线末尾：

```text
edit -> tenant/node ledger -> waiting queue + guarantee reserve
     -> later admission decisions -> global committed time
```

每次增量运行都会和“编辑在生效时刻切换”的全量参考重算逐时刻比较；不一致直接返回精度/业务错误，不允许带病结果通过。`IncrementalResult` 输出复用数、重算数、每个时刻的失效来源、依赖路径和闭包。

## 并行与背压

`Options.Workers` 控制候选排序与租户账本快照的 worker 池。准入写路径保持确定性串行，以避免并发改变节点竞争结果。每个时刻记录各租户实际耗时 `observed_latency_micros`，该观测值不参与正确性判定。

- worker 数或每租户队列上限配置越界：`E_INVALID_BUDGET`。
- 候选数超过 worker 预算：结果中记录 `E_BACKPRESSURE_WORKER`，排序和结算仍完整执行。
- 新申请被拒绝且该租户队列已满：`E_BACKPRESSURE_QUEUE`，不越界放行、不静默丢弃。

测试覆盖 worker 数 1/2/4/8 和同一时刻逆序提交，账本、准入集合、拒绝原因完全一致。

## SQLite 检查点

`RunWithCheckpoint` 每个完整逻辑时刻执行一个事务。事务内写入顺序固定为：

1. `tenant_ledger` 全量租户账本。
2. `node_occupancy` 各节点、各租户占用。
3. `wait_queue` 等待队列。
4. `commit_audit` 子状态提交审计。
5. 最后更新 `checkpoint_meta.committed_time_millis`。

只有事务提交成功才视为完整检查点。可处理退出信号在时刻边界停止，上一完整时刻已落盘。恢复时校验：

- 检查点格式版本 `quota-checkpoint-v1`；
- 工程 schema 版本；
- 规范化工程 SHA-256 指纹；
- 租户权重、节点容量、队列量以及账本与节点占用的重算一致性。

格式版本或工程/schema 版本不匹配返回 `E_VERSION_MISMATCH` / `E_CHECKPOINT_VERSION`；`RunWithCheckpoint` 会从起点重算并重建检查点。正常快照由节点占用重算租户账本，再与持久化账本核对，避免恢复时重复计入同一占用行。

同一租户可以在其多个允许节点上持有负载，但每个 `(node, tenant)` 占用只有一行；恢复后的租户已用量是这些唯一节点行的聚合，不会把一行重复计入两个节点。

## 稳定业务码

| 业务码 | 场景 |
| --- | --- |
| `E_SCHEMA_UNKNOWN` | 缺失/未知 schema 版本 |
| `E_SCHEMA_FIELD` | 必填字段、重复 ID、非法 JSON shape |
| `E_VERSION_MISMATCH` | 期望 schema 或检查点工程版本不匹配 |
| `E_NEGATIVE_QUOTA` | 负配额、保底、突发或换算输入 |
| `E_QUOTA_RANGE` | 比例超出 10000、保底大于配额 |
| `E_GUARANTEE_OVER_CAPACITY` | 节点可承载租户的保底总和超过容量 |
| `E_OVERFLOW` | 定点乘法/配额计算溢出并饱和 |
| `E_INVALID_BUDGET` | worker 或队列上限越界 |
| `E_BACKPRESSURE_WORKER` | worker 预算形成可观测排队 |
| `E_BACKPRESSURE_QUEUE` | 租户等待队列耗尽 |
| `E_INVALID_EVENT` | 非法事件或会导致账本越界的权重/释放 |
| `E_CHECKPOINT_MISSING` | 尚无完整检查点 |
| `E_CHECKPOINT_VERSION` | 检查点格式版本不匹配 |
| `E_CHECKPOINT_TRUNCATED` | SQLite 文件截断或无法识别 |
| `E_CHECKPOINT_CORRUPT` | 表结构/行内容损坏或账本无法核对 |
| `E_CHECKPOINT_INVARIANT` | 恢复状态违反占用/权重不变量 |
| `E_PRECISION_TOLERANCE` | 真值或增量/全量对照超出容差 |
| `E_EDIT_TARGET` | 增量编辑目标不存在 |

所有错误都实现 `quota.BusinessError`，包含 `Code`、`Op`、`Path`、租户/节点、逻辑时刻和上下文；业务码可用 `quota.ErrorCode(err)` 稳定提取。

## 异常注入

命令均只处理变异副本，不修改源工程字节，并验证已提交检查点仍可使用：

```bash
go run ./cmd/quotademo -inject negative-quota
go run ./cmd/quotademo -inject guarantee-over-capacity
go run ./cmd/quotademo -inject quota-scale-overflow
go run ./cmd/quotademo -inject unknown-schema
go run ./cmd/quotademo -inject invalid-budget
go run ./cmd/quotademo -inject queue-backpressure
go run ./cmd/quotademo -inject truncated-checkpoint
go run ./cmd/quotademo -inject version-mismatch
```

## 集成示例

```go
ctx := context.Background()
g, err := gen.Generate(42)
if err != nil { return err }
p, err := quota.Load(g.Documents.V3)
if err != nil { return err }

timeline, err := quota.Run(ctx, p, quota.Options{
    Workers: 4, QueuePerTenant: 64,
})
if err != nil { return err }
if err := g.Truth.Check(timeline); err != nil { return err }

incremental, err := quota.ApplyEdit(ctx, p, quota.Options{
    Workers: 4, QueuePerTenant: 64,
}, timeline, quota.Edit{
    Kind: quota.EditWeightEvent,
    TenantID: "t-gamma",
    EventSeq: 12,
    TimeMilli: 3000,
    Value: 2000,
})
```

## 实际验证结果

验证环境：Linux、`go1.22.12`、本地 SQLite 文件，无 Docker、无外部服务。
完整原始输出保存在 `artifacts/verification.log`，三版样本和真值保存在 `artifacts/demo/`。

```text
$ go test ./...
ok  quotaengine/acceptance
ok  quotaengine/gen

$ go test -race ./...
ok  quotaengine/acceptance
ok  quotaengine/gen

$ go vet ./...
（无输出，退出码 0）
```

演示关键结果：

```text
v1 tenants=4 nodes=3 racks=2 events=20
v2 tenants=4 nodes=3 racks=2 events=20
v3 tenants=4 nodes=3 racks=2 events=20
semantic_equal=true
truth_check=OK tolerance_millicores=0

first_run interrupted=true committed_steps=2
resumed=true from=1000ms suffix_steps=5 checkpoint_time=6000ms
reconciled_ledgers=4 final_equals_full=true
checkpoint_version_mismatch_code=E_VERSION_MISMATCH
committed_v2_ledger_still_usable=true

edit=node_capacity invalidated_from=0 reused=0 recomputed=7 full_equivalent=true
edit=tenant_quota invalidated_from=1000 reused=1 recomputed=6 full_equivalent=true
edit=weight_event invalidated_from=3000 reused=3 recomputed=4 full_equivalent=true
```

八类注入实测码：

```text
negative-quota          E_NEGATIVE_QUOTA
guarantee-over-capacity E_GUARANTEE_OVER_CAPACITY
quota-scale-overflow    E_QUOTA_RANGE
unknown-schema          E_SCHEMA_UNKNOWN
invalid-budget          E_INVALID_BUDGET
queue-backpressure      E_BACKPRESSURE_QUEUE
truncated-checkpoint    E_CHECKPOINT_TRUNCATED
version-mismatch        E_VERSION_MISMATCH
```

每个注入报告均包含 `source_project_unchanged=true` 和 `committed_ledger_usable=true`。
