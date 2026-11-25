# 计划缓存首个可验收版本

本目录是 Go 1.22 模块 `plancache`。实现不依赖真实数据库业务实例：样本由固定种子生成器产出，检查点使用本地 SQLite 文件和纯 Go 驱动 `modernc.org/sqlite`。

## 快速验收

```bash
go version                 # 期望 go1.22+
bash scripts/verify.sh
```

脚本固定使用 `GOPROXY=off GOFLAGS=-mod=vendor`，会执行：

- `go test ./...`
- `go build -o artifacts/bin/plancache ./cmd/plancache`
- 生成三版样本到 `artifacts/samples/project.v1.json`、`project.v2.json`、`project.v3.json`
- 生成完整 JSON 报告 `artifacts/pipeline-report.json`
- 在临时 SQLite 文件上做检查点冒烟

也可以直接运行 CLI：

```bash
go run -mod=vendor ./cmd/plancache samples --dir artifacts/samples
go run -mod=vendor ./cmd/plancache report --out artifacts/pipeline-report.json
go run -mod=vendor ./cmd/plancache checkpoint --db /tmp/plancache.db
go run -mod=vendor ./cmd/plancache recover --db /tmp/plancache.db
```

## 代码地图

- `model.go`：规范工程、语句、子计划/节点、请求、容量、淘汰和预算模型。
- `generator.go`：固定种子 `DefaultSeed = 420261126` 的确定性样本和引用链。
- `schema.go`、`schema_v1.go`、`schema_v2.go`：三版 wire schema 渲染、装载和歧义检查。
- `canonical.go`：结构签名、指纹、规范节点顺序和工程校验。
- `key.go`：缓存键及模板、参数类型、节点结构、设置分项。
- `engine.go`：设置相容性、复用判定、每语句/总字节准入和确定性淘汰。
- `invalidation.go`：子计划引用图、失效闭包、传播代数、环/悬空/淘汰引用/指纹冲突报告。
- `runtime.go`：工人预算、每会话缓冲槽、背压码、按逻辑序号提交和会话耗时观测。
- `checkpoint.go`：`database/sql` + `modernc.org/sqlite`、两阶段检查点、恢复和版本重建。
- `cmd/plancache/main.go`：`samples`、`report`、`checkpoint`、`recover` 子命令。

## 三版 Schema 口径

| 版本 | 字段风格 | 节点表达 | 占位符 | 单位 | 指纹 |
| --- | --- | --- | --- | --- | --- |
| `plan-project/v1` | snake_case | `flat_nodes` + `subplan_roots` | `parameter_base` + `absolute_arguments`，模板为全局 `$N` | tick、字节 | `plan_fingerprint` |
| `plan-project/v2` | camelCase | `subplanTrees[].root/children` 嵌套树 | 每语句零基 `relativeArgs`，模板为 `$1..$n` | 毫秒、KiB | 不携带 |
| `plan-project/v3` | PascalCase | `SubplanTrees[].Root/Children` 嵌套树 | 每语句相对偏移 | 毫秒、KiB | 携带 `Fingerprint` |

装载后统一为：

- 占位符：每语句零基索引；模板统一为相对 `$1..$n`。
- 时间：`EstimatedCostNS`，V1 tick 按 `tick_duration_ns` 换算，V2/V3 毫秒乘 `1_000_000`。
- 容量：`MaxCacheBytes`，KiB 乘 `1024`。
- 节点：主 subplan 先序遍历优先，随后按声明顺序处理其他 subplan；装载阶段把节点切片规范化为先序。
- 指纹：缺失时计算 `fp_<sha256[:32]>`；显式指纹与规范结构不一致返回 `E_FINGERPRINT_CONFLICT`。

`TestThreeSchemaVersionsLoadEquivalent` 直接比较三版装载后的完整规范工程、遍历顺序和缓存键集合。

## 缓存键

键算法为带域分隔的 SHA-256：

```text
ck_<sha256("plancache.key/v1", template, param_types, structure_signature, deterministic_settings)[:40]>
```

`CacheKeyResult.Components` 输出：

- `template_text`、`template_hash`
- `param_type_seq`
- `node_structure`、`structure_hash`
- `settings`、`settings_hash`
- `algorithm`

语句 ID 不进入键，避免命名差异改变键。`TestCacheKeySetIndependentOfStatementOrder`、`TestCrossSchemaReuseConclusionsMatch` 和并发测试覆盖装载顺序、worker 数、请求提交顺序和批次差异。

## 复用、准入与淘汰

请求按全局 `seq` 排序后提交。相同缓存键的候选计划逐个做设置相容性检查：

- `exact`：必须完全相等。
- `ignore`：差异允许复用，但仍输出差异项。
- `one_of`：请求值必须在声明集合内。
- `numeric_at_least_entry`：请求数值必须不低于缓存计划所需值。

输出包括 `decision`、`hit_plan_id`、候选计划、`setting_diffs` 和拒绝码。主要决策值为 `REUSE_HIT`、`DERIVED_MISS`、`DERIVED_AFTER_SETTINGS_REJECT`。

条目字节数由模板长度、节点数、参数数和引用数确定性计算。每次写入输出：

- `occupied_bytes_before_write`
- `occupied_bytes_after_write`
- `occupied_bytes_final`
- `evictions[]`
- `per_statement_rule`
- `total_bytes_rule`

淘汰候选排序固定为：

```text
reuse_count ASC, last_used_tick_ms ASC, last_used_seq ASC, entry_id ASC
```

每语句数量规则是 `R-CAP-PER-STMT-RC-THEN-LRU-v1`，总字节规则是 `R-CAP-TOTAL-BYTES-RC-THEN-LRU-v1`。被淘汰条目标记并移出活动索引，后续请求不可能命中。

## 失效闭包

引用边定义为“消费计划 -> 被引用的语句/subplan”。失效事件从编辑语句出发，在反向依赖图上 BFS：

- generation `0`：被编辑语句自身的活动计划。
- generation `g+1`：直接引用第 `g` 层语句的消费计划。
- `propagation_depth` 包含第 0 代和最深传播代数。

每个条目输出 `event_id`、`plan_id`、`statement_id`、`generation`、`source` 和引用路径。固定样本的断言包括：

- 编辑 `S2-orders` 模板：`S2(g0) -> S3(g1) -> S4(g2)`，不误伤 `S6`、`S5`、`S1`。
- 编辑 `S1-users` 参数类型：`S1(g0) -> S2(g1) -> S3(g2)`。
- 编辑 `S4-inventory` 统计版本：`S4(g0) -> S6(g1)`。

## 并发与背压

`RunParallelPipeline` 并行派生键、并行求解各失效事件；请求 goroutine 按逻辑序号获得提交权后才占用 worker 进入引擎，因此 goroutine 调度不会造成乱序提交。

- worker 预算非法：`E_BUDGET_WORKERS_INVALID`
- 每会话缓冲非法：`E_BUDGET_BUFFER_INVALID`
- 缓冲槽耗尽：`E_BACKPRESSURE_BUFFER_FULL`
- worker 槽耗尽：`E_BACKPRESSURE_WORKERS_EXHAUSTED`

`SessionMetrics` 记录每会话请求数、总实际耗时和每次观测微秒值。墙钟耗时只用于诊断，不参与缓存键、淘汰或闭包。

## SQLite 检查点与强杀恢复

数据库表：

- `checkpoint_meta`：工程版本等元数据。
- `pending_entry_state`：阶段一的缓存条目状态。
- `pending_invalidation_progress`：阶段一的失效传播进度。
- `cache_entries`：已随请求提交的缓存条目。
- `request_cursor`：唯一全局请求序号游标。
- `committed_requests`：已提交请求和报告。
- `invalidation_progress`：失效传播进度。

提交顺序严格为：

1. `SavePendingEntryState(seq, entries, progress)`：先持久化缓存条目状态到 pending 表。
2. `CommitPendingRequest` 在一个 SQLite 事务内提升 pending 到 `cache_entries`、写 `committed_requests`、更新 `request_cursor.last_seq`、删除本序号 pending。

如果进程在阶段一之后、阶段二之前被 `SIGKILL`，旧的 `cache_entries` 和 cursor 仍指向上一请求。重启时 `Recover` 删除高于 cursor 的 pending，只返回已提交请求。测试 `TestCheckpointForceKillRecovery` 真的启动测试二进制子进程并发送 `SIGKILL`。

版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH`；`RecoverOrRebuild` 会删除旧 `.db/.db-wal/.db-shm` 并以当前工程版本新建空检查点，不会部分复用旧条目。截断或损坏文件返回 `E_CHECKPOINT_TRUNCATED` 或 `E_CHECKPOINT_CORRUPT`，错误对象带 `path` 和上下文。

## 稳定业务码

| 业务码 | 场景 |
| --- | --- |
| `E_SCHEMA_UNKNOWN_VERSION` | 无法识别 schema 版本 |
| `E_SCHEMA_FIELD_CONFLICT` | 同时出现无法消歧的字段组合或预算字段不合法 |
| `E_SCHEMA_MISSING_FIELD` | 必填字段缺失 |
| `E_NODE_TREE_MALFORMED` | 节点树缺根、子节点缺失、跨组、多父、环或不可达 |
| `E_PARAM_INDEX_OUT_OF_RANGE` | 占位符/参数编号越界或全局编号映射失败 |
| `E_CAPACITY_BYTES_ZERO` | 字节上限为 0 或负数 |
| `E_BUDGET_WORKERS_INVALID` | worker 预算越界 |
| `E_BUDGET_BUFFER_INVALID` | 会话缓冲预算越界 |
| `E_BACKPRESSURE_BUFFER_FULL` | 每会话缓冲耗尽 |
| `E_BACKPRESSURE_WORKERS_EXHAUSTED` | worker 槽耗尽 |
| `E_REFERENCE_CYCLE` | 子计划引用环 |
| `E_REFERENCE_DANGLING` | 引用不存在的语句/subplan/plan |
| `E_REFERENCE_TO_EVICTED_PLAN` | 显式 PlanID 指向已淘汰/已失效计划 |
| `E_FINGERPRINT_CONFLICT` | 显式或跨版本指纹与规范结构冲突 |
| `E_CHECKPOINT_TRUNCATED` | SQLite 文件截断或完整性检查失败 |
| `E_CHECKPOINT_VERSION_MISMATCH` | 检查点工程版本不匹配 |
| `E_CHECKPOINT_CORRUPT` | 其他检查点打开/迁移错误 |
| `E_REQUEST_NOT_FOUND` | 请求引用未声明语句 |

所有 `*BusinessError` 都包含 `code`、`path`、`message`，并可带 `context`。

## 离线 SQLite 驱动复现

依赖固定在 `go.mod`/`go.sum`：

- `modernc.org/sqlite v1.34.5`
- 间接运行时依赖包括 `modernc.org/libc v1.55.3`、`modernc.org/mathutil v1.6.0`、`modernc.org/memory v1.8.0`、`golang.org/x/sys v0.22.0`

仓库包含完整 `vendor/`，因此离线验收命令为：

```bash
GOPROXY=off GOFLAGS=-mod=vendor go test -count=1 ./...
GOPROXY=off GOFLAGS=-mod=vendor go build -mod=vendor ./cmd/plancache
```

若需要在有模块缓存或网络的机器上重新获取驱动：

```bash
go get modernc.org/sqlite@v1.34.5
go mod vendor
```

未使用 Docker、外部数据库、线上服务或账号。

## 实际验证记录

验证时间：2026-11-26 05:51 CST 之后。

```text
$ go version
go version go1.22.12 linux/amd64

$ GOPROXY=off GOFLAGS=-mod=vendor go vet ./...
VET_RC=0

$ GOPROXY=off GOFLAGS=-mod=vendor go test -count=1 ./...
ok      plancache       0.037s
?       plancache/cmd/plancache [no test files]

$ GOPROXY=off GOFLAGS=-mod=vendor go test -race -count=1 .
ok      plancache       1.290s
```

详细模式包含 23 个顶层测试和 11 个 schema 异常子测试，全部 PASS，覆盖：

- 三版 schema 等价和全局占位符归一
- 缓存键顺序无关
- 三版复用结论一致
- 并发度/反向提交/分批提交一致性
- 容量、淘汰和“淘汰后不可复用”
- 失效闭包代数和无误伤
- 环、悬空、淘汰引用、指纹冲突
- 背压码和会话指标
- SQLite 提交恢复、版本不匹配重建、截断错误码、真实 `SIGKILL` 恢复

CLI 强杀演练的实际结果：

```text
plancache checkpoint --db <tmp>/force.db --crash-at 2
CRASH_RC=137

plancache recover --db <tmp>/force.db
last_seq=1
request_seqs=[1]
discarded_pending=1
rebuilt=false
```
