# 隔离与恢复编排原型

这是一个可从空 Go 模块独立构建、测试和人工核验的隔离编排原型。输入完全由仓库内的固定种子生成器产出，不依赖外部集群、容器或未授权目标；SQLite 使用纯 Go 驱动 `modernc.org/sqlite v1.34.5`。

## 快速验收

```bash
go version
go env GOPATH
go build ./...
go test ./...
go test -race ./...

go run ./cmd/orchestrator gen -seed 424242 -out samples
go run ./cmd/orchestrator verify -seed 424242 -out artifacts/verification.json
go run ./cmd/orchestrator run -input samples/cluster-v2.json -out artifacts/run-v2.json
go run ./cmd/orchestrator execute -input samples/cluster-v1.json -db /tmp/isolation.db
go run ./cmd/orchestrator execute -input samples/cluster-v1.json -db /tmp/isolation.db -resume
```

本地实际环境为 `go1.22.12 linux/amd64`，`go env GOPATH` 可正常解析并用于模块缓存。`samples/` 下的三个 schema 文件、规范模型和真值均由生成器在本地写出；`artifacts/verification.json` 是最近一次验收结果。

## 代码结构

- `loader.go`、`model.go`、`units.go`：三版本线格式装载、单位换算、规范排序和稳定业务码。
- `gen.go`：固定种子生成同一工程的 v1/v2/v3 表示及稀疏逐 tick 真值。
- `engine.go`、`propagation.go`：逐 tick 故障域、冻结队列、恢复门槛、超时升级和多源传播。
- `incremental.go`：依赖边、域边界和恢复事件的增量失效与缓存窗口复用。
- `checkpoint.go`、`store.go`、`executor.go`：SQLite 提交点、事务回滚、优雅中断和从最后完整提交续做。
- `cycles.go`：依赖强连通分量和可定位闭环报告。
- `cmd/orchestrator/main.go`：生成、求解和完整验收命令。

## 三版本映射

生成器描述同一个 9 节点、6 条依赖、4 个隔离域、55 个逻辑 tick 的工程，但三份线格式刻意使用不同表达：

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 版本字段 | `cluster.v1` | `cluster/v2` | `cluster.v3` |
| 节点表 | 扁平 `nodes` | `topology.racks[].nodes` | 扁平 `nodes` + 嵌套 `zone` |
| 隔离域 | `zone_id_abs` 绝对编号 | `zone_base + zone_offset` | `zone.base + zone.offset` |
| 事件时间 | `at_ms` | `at_tick` | `at_ms` |
| 超时 | 可选 `isolation_timeout_ms` | 可选 `isolation_timeout_ticks` | 可选 `timeout_ms` |
| 容量 | 字节 | KiB | KiB |
| 事件/依赖字段 | `events`/`edges` | `timeline`/`dependencies` | `log`/`links` |

装载后的节点、工作负载、依赖边、事件按稳定主键排序。显式超时和缺省超时都归一为精确 `ticks` 有理数，因此不同字段名、单位、绝对/相对编号和可选字段存在性不会改变内部模型。未知 schema 或版本组合返回稳定业务码 `E_UNSUPPORTED_SCHEMA`。

## 逐 tick 语义

- `probe` 事件把节点加入故障集，并按节点所属 zone 聚合隔离域；同一 zone 的多个根节点合并到一个域。
- 依赖边表示为 `From depends on To`。隔离传播从 `To` 反向扩散到依赖它的 `From`，使用多源 BFS，域内节点为第 0 代。
- 隔离域内节点以及传播影响节点上的工作负载进入冻结；新请求进入等待队列并记录 `domain:<id>`、`edge:<from>-><to>` 等冻结来源。
- 冻结请求在 `commit` 提交前不放行。提交 tick 内先完成域状态更新，再解冻并按到达顺序重新检查预算；超出上限的释放请求标记为拒绝，不存在越界放行。
- `recover` 是恢复意图。节点自身所属域仍活跃，或其依赖仍故障/隔离时不能恢复，并输出依赖链和传播代数；已阻塞的恢复意图在后续 tick 自动重试。
- 超时使用归一化 tick 有理数做纯整数边界比较。边界包含等号：`elapsed >= timeout` 才升级；若同一 tick 已提交隔离，则提交优先。升级只记录一次，包含升级 tick 与精确依据。
- 所有输出数组使用节点 ID、域 ID、事件 `(tick, seq)` 等确定性顺序。

## 增量与并发

增量引擎把缓存保存到每个 tick 的完整求解状态之后。编辑依赖边或域边界时，以新旧依赖图的弱连通分量为影响范围，从该范围最早探测 tick 开始，并按缓存中的静止 tick 切成窗口；只重算会经过被编辑对象的窗口，窗口间无故障、无冻结、无等待的基线区间直接复用。追加恢复事件只扩展事件 tick 之后的窗口。每个重算结果带 `invalidated_by`，报告同时列出 `recomputed_ticks` 与 `reused_ticks`。快照 ID/修订变化会清空整个缓存；同版本但内容变化返回 `E_SNAPSHOT_CHANGED`。

传播按 BFS 层把 frontier 切成不超过 `per_domain_pending_limit` 的独立任务，并由 `worker_budget` 个 worker 并行展开。任务结果按父节点、子节点排序后合并，因此调度顺序不改变传播链。frontier 超过 `workers * per_domain_pending_limit` 时输出确定性的 `BackpressureSignal`，但仍分批处理全部节点，不跳过、不越界分配。

## SQLite 提交与恢复

一次 tick 的提交在单个事务中按以下顺序写盘：

1. 节点隔离/故障/恢复状态；
2. 活跃隔离域；
3. 工作负载预算、冻结集合与等待队列；
4. 提交历史；
5. 最后更新全局 tick 的单行 `checkpoint_meta`。

任意阶段注入错误都会回滚到上一完整 tick。恢复器只读取 `checkpoint_meta` 指向的已提交状态，重建故障集、活跃域、冻结队列、等待预算和待恢复意图，再从 `tick+1` 继续。`context` 取消表示可处理退出信号：只返回最后一个完整提交点并停止，不产生半 tick 报告；强制终止则没有退出报告承诺。

## 已执行验证

```text
go build ./...                  PASS
go test ./... -count=1          PASS
go test -race ./... -count=1    PASS
go test ./... -cover -count=1   PASS (主包 81.0%)
go mod verify                   PASS
```

固定种子 `424242` 的验收报告：

```json
{
  "seed": 424242,
  "schemas_equivalent": true,
  "truth_match": true,
  "cross_schema_identical": true,
  "concurrency_invariant": true,
  "backpressure_observed": true,
  "incremental_matches_full": true,
  "snapshot_cache_dropped": true,
  "crash_rollback_held": true,
  "resume_matches_full": true,
  "completed_ticks": 55,
  "incremental_sources": ["edge:b:[]"],
  "incremental_recomputed_ticks": 19,
  "incremental_reused_ticks": 36,
  "crash_last_committed_tick": 14,
  "final_checkpoint_tick": 54
}
```

测试还覆盖：三版本逐 tick 结果完全一致、8 tick 超时在 tick 20 精确升级、恢复依赖链阻塞、预算拒绝、依赖环报告、不同 worker 预算/域上限下语义一致、SQLite 提交前故障回滚、等待队列持久化、优雅取消与强制中断后续跑。

这是可独立核验的原型，不包含旧脚本兼容层、真实集群适配器或生产级鉴权/网络控制。
