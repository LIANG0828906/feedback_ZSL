# credrot

`credrot` 是一个零外部依赖的确定性凭据轮换工程。它从同一份内部毫秒模型出发，装载三个 schema 版本，求解逐逻辑时刻的有效期，计算悬置凭据的重载传播、批次调度、会话门控，并支持并行背压、增量缓存和崩溃安全检查点。

整个实现只使用 Go 标准库。样本、Oracle 真值和异常变体全部由固定种子在本地生成，不访问真实集群、密钥服务、容器或外部账号。

## 快速开始

```bash
go version
go test ./...
go test -race ./...
go vet ./...

go run ./cmd/credrot verify --seed 42
go run ./cmd/credrot gen --seed 42 --out ./samples/seed42
go run ./cmd/credrot analyze --project ./samples/seed42/project_v3.json --at-tick 6
```

`verify` 会把同一生成器样本依次用于三版收敛、Oracle 对拍、并行不变性、增量缓存、检查点崩溃点和异常码验证。相同命令与相同种子重复执行会得到逐字节一致的样本。

仓库中的 `samples/seed42/` 已保存一份生成结果，可直接用于阅读和 `analyze`；它可随时用上述 `gen` 命令重建并逐字节比对。

生成目录包含：

- `project_v1.json`：V1 扁平凭据表、逻辑 tick、秒级 TTL。
- `project_v2.json`：V2 按订阅面分组的凭据树、逻辑 tick、秒级 TTL，凭据版本与重载批次字段语义互换。
- `project_v3.json`：V3 扁平凭据表、毫秒时间基准、毫秒 TTL、显式轮换水位。
- `oracle.json`：独立参考实现计算的有效期、传播、调度和门控真值。
- `manifest.json`：样本清单。

## 代码结构

| 路径 | 职责 |
| --- | --- |
| `internal/project` | 三版 schema 装载、字段消歧、内部模型校验 |
| `internal/engine` | 有效期、传播、调度、门控、并行和增量求解 |
| `internal/generator` | 固定种子样本生成器和独立 Oracle |
| `internal/checkpoint` | 两阶段追加式本地检查点 |
| `internal/anomaly` | 异常注入；只克隆工程，不修改源工程 |
| `internal/verify` | 端到端验收编排 |
| `cmd/credrot` | `gen`、`verify`、`analyze`、`inject`、`checkpoint-verify` |

## 三版 Schema 映射

内部模型只使用 `int64` 毫秒：

```text
Credential:
  id, version, issued_at_ms, ttl_ms, reload_batch, supersedes
Subscription:
  id, active, depends_on[], refs[{credential_id, expected_version, role}]
```

| 语义 | V1 | V2 | V3 |
| --- | --- | --- | --- |
| 清单版本 | `manifest_version` | `manifest_revision` | `manifest_version` |
| 时间基准 | `time.unit=logical_tick` + `tick_ms` | `clock.basis=logical_tick` + `tick_ms` | `clock.basis=milliseconds` + `tick_ms` |
| 签发时间 | `issued_tick` | `issued_tick` | `issued_at_ms` |
| TTL | `ttl`，根级 `ttl_unit=seconds` | `ttl`，根级 `ttl_unit=seconds` | `ttl_ms`，根级 `ttl_unit=milliseconds` |
| 凭据版本 | `credential_version` | `batch`（历史字段，语义为版本） | `generation` |
| 重载批次 | `reload_batch` | `version`（历史字段，语义为批次） | `batch` |
| 凭据表达 | `credentials[]` 扁平表 | `credential_tree.<face>[]` 分组树 | `credentials[]` 扁平表 |
| 订阅面 | `subscriptions[]` | `faces{}` + 树键 | `subscribers[]` |
| 上次轮换水位 | `last_rotation_watermark_tick`，可省略 | 字段省略，由版本 1 凭据推导 | `last_rotation_watermark_ms`，可省略 |

装载规则：

1. 逻辑 tick 乘以 `tick_ms`，秒级 TTL 乘以 `1000`，全部转成定点整数毫秒。
2. V2 的 `version` 映射为 `reload_batch`，`batch` 映射为 `version`；这是显式声明的历史语义互换。
3. V2 分组树中同一凭据可重复出现，但所有定义必须一致，否则返回 `E_DUP_CREDENTIAL`。
4. 水位省略时，取版本 1 凭据的最早签发时刻；若没有版本 1 凭据，取全部凭据的最早签发时刻。
5. 同一字段同时以 tick/毫秒、秒/毫秒或两个别名出现，返回 `E_SCHEMA_AMBIGUOUS`，不会静默选一个。
6. 装载只读取输入字节，不修改原始 JSON。

## 有效期口径

对一个凭据 `c` 和逻辑时刻 `t`：

```text
now       = t * tick_ms
validFrom = max(c.issued_at_ms, last_rotation_watermark_ms)
expiresAt = c.issued_at_ms + c.ttl_ms
valid     = validFrom <= now < expiresAt
remaining = expiresAt - now
```

`last_rotation_watermark` 只约束信任起点，不延长 TTL。输出按 `(tick, subscription_id)` 排序，包含每个引用凭据的 `valid/not_yet_valid/expired` 状态与来源，以及所有可用凭据的剩余毫秒和剩余逻辑刻度。秒与毫秒表达在装载后得到完全相同的 `expiresAt`。

## 传播与调度

传播以每个 `rotation` 的目标凭据为起点：

1. 找到所有直接引用该凭据且处于 `active` 状态的订阅面。
2. 沿 `depends_on` 反向边传播到活跃后代。
3. 同一订阅面存在多条路径时，取最大深度，并用字典序最小的路径作为稳定路径。
4. `batch = target.reload_batch + depth`。因此到达集合在三个 schema 下归一后完全一致。
5. 非活跃订阅面不进入到达集合，也不作为继续传播的中继；增删订阅面后重新计算切面。

调度先按订阅面合并多条悬置凭据，取最晚目标批次，再执行批上限切分。超出上限的订阅面进入下一批，并输出：

```text
constraint = REL-CAP-001
reason     = batch capacity exceeded; moved to the next reload slice
```

重试口径是“最多尝试次数”。每次尝试带 `attempt`、逻辑时刻、毫秒时刻、失败集合和成功集合。全部尝试失败后输出最终失败时刻与触发来源：

```text
REL-RETRY-003  重试耗尽
REL-TOPO-002   上游订阅面最终失败导致下游失败
```

生成器固定样本会同时触发批上限延后、一次重试后成功、一次重试耗尽和跨批上游失败。每个批次的实际重载集合与最终失败集合合并后，必须逐项覆盖一次性全量重载集合；验收项为 `coverage_match`。

## 会话门控

门控严格按以下优先级给出唯一结论：

1. 请求凭据缺失：`E_MISSING_CREDENTIAL`。
2. 请求版本低于已提交版本：`E_VERSION_REGRESSION`。
3. 显式检查时批次序号跳号：`E_BATCH_GAP`。
4. 显式要求水位但不可用：`E_WATERMARK_MISSING`。
5. 新凭据已到达、已确认且在有效期内：`allow_new`。
6. 新凭据未到达、未确认或已过期，但已提交旧凭据仍有效：`fallback_old`。
7. 已提交旧凭据也失效：`E_GATE_NO_FALLBACK`，明确拒绝发起。

重载未确认期间绝不会以新凭据放行。每个结论都带 `source`，例如 `suspended:<id>;state=arrived_confirmed`、`committed:<id>;state=last_valid` 或 `committed:none_valid`。

“版本回退 + 批次跳号 + 水位缺失”同时出现时，上面的顺序保证版本回退优先，不会因后置异常改变结论。

## 并行与背压

`SolveParallel` 使用固定 worker 池推进有效期与传播任务：

- `workers <= 0` 返回 `E_WORKER_BUDGET`。
- `pending_buffer_per_subscription <= 0` 返回 `E_BUFFER_BUDGET`。
- 单订阅面待判定凭据超过缓冲上限时，按上限分块处理并输出 `credential_buffer` 背压事件。
- 所有 worker 都在忙且仍有任务时，输出 `worker_budget` 背压事件，然后等待槽位；不会静默丢弃任务。
- 结果按预分配索引回收，排序后计算 SHA-256 语义摘要。workers 为 `1/2/4/8`、缓冲为 `1/2/10` 时摘要和全部语义集合保持一致。

## 增量失效

`Snapshot` 缓存：

- 每个 `(tick, subscription)` 的有效期结果；
- 每个轮换凭据的重载传播；
- 调度结果。

编辑通过 `ApplyEdit` 在克隆工程上完成。凭据编辑只失效直接引用者及其后代的有效期区间；订阅面引用或拓扑编辑失效该订阅面及其后代，并触发受影响轮换传播；重载条件编辑只重算调度。

输出包含：

- `invalidated[]`：订阅面、tick 区间和失效来源；
- `reused[]`：复用缓存值的 tick 区间；
- `propagation_evidence[]`：每个轮换凭据是复用还是重算；
- `schedule_recomputed` 与 `schedule_source`。

增量结果与同工程全量重算逐项相等。

## 检查点与崩溃恢复

任务允许使用与 SQLite 等价的本地确定性存储。为避免引入需要联网获取的第三方纯 Go SQLite 驱动，本实现使用两阶段追加日志实现等价事务语义：

1. 先追加完整 `state` 记录，包含订阅面当前凭据、悬置队列和批次进度，并 `fsync`。
2. 再追加 `commit` 记录，引用上一条状态记录的序号与校验和，并携带全局已提交逻辑时刻，随后 `fsync`。
3. 每条记录都有序号、类型、payload 和 SHA-256 校验和。

恢复规则：

- 只有被 `commit` 引用的最新 `state` 才会被采纳。
- 崩溃发生在状态落盘后、提交落盘前：忽略悬空状态，继续使用上一提交。
- 崩溃发生在提交记录中途：非严格恢复会截断半行并回到上一提交；`checkpoint-verify` 严格模式返回 `E_CHECKPOINT_TRUNCATED`。
- 完整记录校验和不匹配返回 `E_CHECKPOINT_CORRUPT`。
- 清单版本不匹配返回 `E_MANIFEST_MISMATCH`；`OpenOrRebuild` 删除旧检查点并从空状态整段重建，不部分复用。

初始未提交状态使用 `committed_tick = -1`。

## 稳定业务码

| 业务码 | 触发 |
| --- | --- |
| `E_SCHEMA_UNSUPPORTED` | 未知 schema 或版本 |
| `E_SCHEMA_AMBIGUOUS` | 同一语义存在冲突字段/单位 |
| `E_SCHEMA_INVALID` | 缺字段、非法结构或非整数 |
| `E_DUP_SUBSCRIPTION` | 订阅面 ID 重复 |
| `E_DUP_CREDENTIAL` | 凭据 ID 重复或 V2 树定义冲突 |
| `E_DUP_REF` | 同一订阅面重复引用凭据 |
| `E_MISSING_CREDENTIAL` | 凭据引用缺失 |
| `E_MISSING_SUBSCRIPTION` | 拓扑依赖缺失 |
| `E_NEG_TTL` / `E_TTL_INVALID` | 负值/零值 TTL |
| `E_SUB_CYCLE` | 订阅面拓扑成环，含环路径 |
| `E_BATCH_LIMIT` | 批上限为零或越界 |
| `E_BATCH_GAP` | 门控显式检查时批次跳号 |
| `E_VERSION_REGRESSION` | 凭据版本回退 |
| `E_WATERMARK_MISSING` | 要求水位但不可用 |
| `E_GATE_NO_FALLBACK` | 新凭据不可用且旧凭据失效 |
| `E_WORKER_BUDGET` / `E_BUFFER_BUDGET` | 并行预算非法 |
| `E_CHECKPOINT_TRUNCATED` | 严格模式下检查点尾部不完整 |
| `E_CHECKPOINT_CORRUPT` | 校验和、序号或记录结构损坏 |
| `E_MANIFEST_MISMATCH` | 检查点清单版本不匹配 |
| `E_INVALID_ROTATION` | 轮换目标/版本不一致 |
| `E_NUMERIC_OVERFLOW` | 单位换算溢出 |

异常注入命令示例：

```bash
go run ./cmd/credrot inject \
  --project ./samples/seed42/project_v3.json \
  --kind duplicate_credential_ref
```

支持的 `--kind`：`duplicate_credential_ref`、`negative_ttl`、`batch_limit_zero`、`subscription_cycle`、`missing_credential`、`duplicate_credential`。注入过程不修改源工程。

## 生成器口径

生成器使用自带的确定性 PRNG，不依赖 `math/rand` 的跨版本实现细节。固定种子样本包含：

- 6 个订阅面：`api`、`billing`、`search`、`reports`、`archive`、`audit`；
- 拓扑边：`api -> billing/search`、`billing -> reports`、`search -> archive`；
- 4 至 5 条轮换链；
- 能触发批上限的拓扑汇聚；
- 一次第二尝试成功、一次重试耗尽；
- 门控结果同时覆盖 `allow_new`、`fallback_old`、`reject`；
- 显式/推导水位一致；
- 一个已过期旧凭据，用于验证明确拒绝。

Oracle 不调用 engine 求解函数，而是使用独立的逐时刻扫描、路径枚举、批切分/门控参考算法计算。

## 实际验证结果

当前工作区实际执行结果：

```text
$ go test ./...
ok  credrot/internal/anomaly
ok  credrot/internal/checkpoint
ok  credrot/internal/engine
ok  credrot/internal/project
ok  credrot/internal/verify

$ go test -race ./...
ok  credrot/internal/anomaly
ok  credrot/internal/checkpoint
ok  credrot/internal/engine
ok  credrot/internal/project
ok  credrot/internal/verify

$ go vet ./...
(no findings)

$ go run ./cmd/credrot verify --seed 42
[PASS] deterministic_generator
[PASS] three_schema_convergence
[PASS] validity_oracle
[PASS] propagation_oracle
[PASS] schedule_oracle
[PASS] schedule_coverage
[PASS] gating_oracle
[PASS] parallel_invariance
[PASS] parallel_backpressure
[PASS] incremental_equivalence
[PASS] incremental_reuse
[PASS] incremental_reload_conditions
[PASS] source_immutability
[PASS] checkpoint_recovery
[PASS] anomaly_codes
VERIFIED
```

集成测试还使用种子 `1`、`42`、`20261126` 重复执行同一验收流水线。

## 设计边界

- 不连接真实集群、凭据服务或网络。
- 检查点文件是本地确定性日志，不是 SQLite 二进制文件；选择该实现是为遵守“零联网外部依赖”，同时保留事务式的先状态后提交、校验和、截断恢复和清单原子重建语义。
- “重试上限”按最大尝试次数解释，包含首次尝试。
- 生成器故意把批上限设为 1，以保证每个种子都覆盖批容量延后路径；上限 > 1 的行为由同一通用切批算法处理。
