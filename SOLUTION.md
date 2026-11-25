# 告警规则内核交付说明

本目录是一个可集成的 Go 领域组件与本地演示程序。样本由固定种子 `20261126` 确定性生成，不访问真实监控系统、外部账号或网络服务。

## 快速运行

```bash
# Go 1.22+；首次拉包如遇代理 EOF，可使用：
# GOPROXY=https://goproxy.cn,direct go mod tidy

make verify          # 生成三版工程并执行全部本地验收
make test            # go test ./...
make race            # 关键包竞态检查
make cgo0            # CGO_ENABLED=0 构建，验证纯 Go SQLite

go run ./cmd/alertdemo generate --out artifacts/demo
go run ./cmd/alertdemo timeline
go run ./cmd/alertdemo run --schema v2 --db artifacts/demo/checkpoints/v2.db
```

强制终止与恢复对照：

```bash
go build -o bin/alertdemo ./cmd/alertdemo
./bin/alertdemo crash-run --db /tmp/kill.db --crash-after 2; echo exit=$?
./bin/alertdemo resume --db /tmp/kill.db
```

`crash-run` 在 t=2 的 SQLite 事务提交成功后立即 `exit(137)`，不执行 `Close`；重开数据库后从 `cursor.last_t=2` 恢复。也可以运行：

```bash
./bin/alertdemo run --db /tmp/signal.db --step-sleep 1s
# 另一个终端 kill -TERM <pid>
```

SIGINT/SIGTERM 后，执行器停在上一个完整提交点，不会把半步骤写入全局游标。

## 包结构

- `model`：规范工程、规则、指标、标签、接收方、路由、抑制和静默模型。
- `errs`：稳定 `E_xxx` 业务错误码与可定位 JSON 路径。
- `fixed`：声明位宽定点整数边界、受检加法、溢出检测与饱和策略。
- `schema`：`v1`、`v2`、`v3` DTO、严格解析、版本编码和规范化校验。
- `engine`：并行规则求值、来源追踪、状态机、联合压制裁决、路由和通知去重。
- `incremental`：编辑失效区间、依赖闭包、增量重算和全量对照。
- `checkpoint`：`modernc.org/sqlite` 检查点、版本校验、损坏识别和恢复。
- `sample`：固定种子样本、三版 JSON 与确定性真值。
- `demo`：验收器、逐时刻文本轨迹和注入检查。
- `cmd/alertdemo`：`all/verify/generate/timeline/run/crash-run/resume` 入口。

## 三版 Schema 映射

| 版本 | 字段风格 | 时刻 | 时长 | 标签 | 规则组织 | 静默字段 |
| --- | --- | --- | --- | --- | --- | --- |
| `v1` | `snake_case` | epoch 毫秒 + `at_ms` | 秒 | 扁平 `labels` | 扁平 `rules` | 无该字段，按无静默处理 |
| `v2` | `camelCase` | `sampleIndex` 相对序号 + `timeOriginMs` | 毫秒 | 递归 `labelTrees`，序列引用 `labelSetId` | `ruleGroups` 一层分组 | `silenceWindows` |
| `v3` | `kebab-case` 嵌套结构 | epoch 毫秒 | 毫秒 | 扁平 `tag-set` | 递归 `rule-tree.groups` | `silence-windows` |

规范化阶段统一完成：

- 相对序号按 `time_origin + index * step_ms` 转 epoch 毫秒。
- 秒乘 `1000` 转毫秒。
- 标签树沿路径合并为完整标签集合。
- 递归规则组展开并保留组路径。
- 规则按 `priority DESC, id ASC` 排序并写入稳定 `order`。
- 计算规范 JSON SHA-256 版本号 `rv_...`，用于检查点版本匹配。

无法识别的 `schema/schemaVersion/schema`、错误的时刻字段组合、严格 DTO 未知字段等返回 `E_SCHEMA_UNKNOWN` 或 `E_SCHEMA_COMBO_UNRECOGNIZED`，不会静默降级。

## 求值与状态口径

- 逻辑时刻为 `t=0..9`，步长 `15s`；所有持续时间只使用逻辑时间，不使用挂钟推进状态。
- 每个规则选择同指标和匹配标签的序列，取 `sample_index <= t` 的最后一个采样。
- 采样年龄超过 `freshness_ms` 判为 `stale_sample_gap`；指标完全不存在输出 `missing_metric`；均按无数据处理，绝不把缺失值当 `0`。
- 定点数值使用工程声明的 `bits` 与 `scale`；受检运算溢出可按声明策略返回 `E_FIXED_OVERFLOW` 或饱和到该位宽上下界，并在观测中记录 `overflow_detected/saturated`。
- 谓词命中的序列经标签改写后按规范标签键聚合为实例；每个实例保留 `SourceRef`（序列、采样序号、采样时刻、原始定点值）。

状态迁移：

- `for_ms > 0`：首次命中从 `inactive` 进入 `pending`；连续命中且 `now - pending_since >= for_ms` 时进入 `firing`。
- `for_ms = 0`：当次命中直接 `firing`。
- `for_ms < 0` 或 `keep_firing_ms < 0`：装载/编辑校验返回 `E_DURATION_NEGATIVE`。
- `keep_firing_ms >= 0`：最后一次命中后，在 `now - last_match <= keep_firing_ms` 内保持 `firing`；超过边界的首次求值转 `inactive`。
- `pending` 遇到不满足或无数据立即回到 `inactive`。
- 每次迁移分配稳定、规范顺序的 `B0001...` 依据编号，并分别记录 `fire_basis_*` 与 `resolve_basis_*`。

## 压制、静默与通知

- 抑制规则包含 `source_matchers`、`target_matchers` 和 `equal_labels`；源必须是内部 `firing` 状态。
- 确定性事件窗口 `SEV-INH-DB-05` 限定 `INH-DB` 在 t=5..7 生效；即使没有事件窗口，抑制关系也可按全逻辑区间生效。
- 静默按标签选择器和 `[start_t,end_t]` 闭区间判断。
- 内部状态始终保留；只有存在主压制原因的实例从 `visible` 集合移除。
- 同时命中抑制和静默时，按 `policy.reason_priority` 给出唯一 `primary_reason`，同时保留 `suppressed/silenced` 两个布尔归因。样本声明抑制优先，因此 t=7 为 `suppressed`，t=8 为 `silenced`。
- 通知只在实例当次迁移到 `firing` 且对外可见时产生；接收方由路由匹配或规则直连决定，输出 `route:route-id` / `rule:<id>.receiver` 依据。
- 去重键为 `receiver|<group_by 标签有序值>`；同一逻辑时刻同键成员聚合为一个批次。持续 firing 但状态未变化不会再次通知。
- 路由或规则引用未声明接收方时返回 `E_RECEIVER_NOT_FOUND`，不会静默丢批。

## 并发与缓冲

- `budget.workers` 合法范围 `1..32`，`budget.per_rule_buffer` 合法范围 `1..4096`；越界分别返回 `E_BUDGET_WORKERS_OOR`、`E_BUDGET_BUFFER_OOR`。
- 规则在 worker 池中并行执行，结果写入按规则顺序固定的槽位；最终状态、可见集合和通知仍按规范顺序生成。
- 每规则序列缓冲容量为 `per_rule_buffer`；缓冲满时先 flush 再复用，并累加 `backpressure_events/buffer_chunks`，不做无界追加或跳过规则。
- `RuleScheduling.duration_ns` 仅作诊断，不参与正确性比较。

## SQLite 检查点

默认演示数据库：`artifacts/demo/checkpoints/alertkernel.db`。

表：

- `cursor(id=1,last_t,basis_seq,rule_version,...)`：全局完整提交序号。
- `instance_state`：每规则每实例当前状态 JSON。
- `adjudication_cache`：每时刻压制/静默判定 JSON。
- `time_result`：完整时刻结果 JSON，便于重启后对照。
- `meta`：扩展元数据。

提交事务中的语句顺序为：

1. upsert 全部实例状态；
2. 写该时刻压制/静默缓存和完整结果；
3. 最后 update `cursor.last_t/basis_seq/rule_version`；
4. commit。

SQLite DSN 使用 `synchronous=FULL`。事务原子性保证强杀后要么读到旧游标+旧完整时刻，要么读到新完整时刻；状态先于游标的写入顺序也满足崩溃恢复约定。

恢复时校验 `cursor.rule_version`：

- 版本相同：载入状态和 `basis_seq`，从 `last_t+1` 续算。
- 版本不同：`LoadRecovery` 返回 `E_CHECKPOINT_VERSION_MISMATCH`；`LoadMatchingOrReset` 会清空检查点并从 t=0 整段重算，不复用旧状态。
- 截断/损坏数据库：返回 `E_CHECKPOINT_CORRUPT`，演示中通过截断有效 SQLite 文件验证。

## 增量失效闭包

`incremental.Manager` 缓存上一份完整 `RunReport`。编辑先深拷贝，再修改并重新规范化，避免失败编辑污染当前版本。

每次编辑输出：

- `direct_from_t`：直接数据/事件生效点。
- `closure_from_t/closure_to_t`：实际重算区间；当前后缀闭包统一到最后逻辑时刻。
- `backward_lookback_ms/forward_keep_ms`：工程中的最大 `for/keep_firing_for` 跨时刻传导边界。
- `dependency_sources`：从被编辑对象到谓词、状态链、压制裁决、路由和通知去重的来源链。
- `reused_times/recomputed_times`：复用与重算时刻。
- 每个重算的 `TimeResult.invalidation_sources` 也携带同一来源链。

后缀闭包到时间轴末端是刻意的安全边界：`for/keep_firing_for` 会跨时刻传播状态，通知去重链也要求确认后缀没有重复 firing 迁移。`closure_from_t-1` 的完整状态和 `basis_seq` 作为续算种子，因此前缀直接复用。

已验证编辑：

- 覆盖指标采样：复用 t=0..1，重算 t=2..9。
- 追加指标采样：复用 t=0..2，重算 t=3..9。
- 删除静默：复用 t=0..6，重算 t=7..9。
- 新增静默：从 t=3 重算。
- 删除抑制事件：从 t=5 重算。
- 新增抑制关系、修改谓词、修改 `for`、修改标签改写：从 t=0 全区间重算。

所有增量报告与编辑后工程的一次性全量重算逐时刻语义一致（耗时观测已排除）。

## 稳定错误码

| 错误码 | 触发场景 |
| --- | --- |
| `E_SCHEMA_UNKNOWN` | 三版 schema 标识无法识别 |
| `E_SCHEMA_COMBO_UNRECOGNIZED` | 版本字段组合、时刻字段或 DTO 严格解析失败 |
| `E_PREDICATE_MALFORMED` | 操作符、模板或谓词结构非法 |
| `E_DURATION_NEGATIVE` | `for` / `keep_firing_for` 为负 |
| `E_FIXED_OVERFLOW` | 定点整数超出声明位宽或受检运算溢出且策略为 error |
| `E_BUDGET_WORKERS_OOR` / `E_BUDGET_BUFFER_OOR` | 并发或缓冲预算越界 |
| `E_RECEIVER_NOT_FOUND` | 路由/规则引用未声明接收方 |
| `E_EDIT_INVALID` | 不支持的编辑或目标不存在 |
| `E_CHECKPOINT_VERSION_MISMATCH` | 检查点规则版本不匹配 |
| `E_CHECKPOINT_CORRUPT` | SQLite 截断、损坏或 integrity check 失败 |
| `E_INTERRUPTED` | 上下文/信号在逻辑步骤边界触发 |

## 本环境实测结果

以下结果来自当前工作区：

- `go version`：`go1.22.12 linux/amd64`。
- `go test ./...`：`demo`、`engine`、`schema` 测试通过，其余包无测试文件。
- `go test -race ./engine ./schema ./demo`：通过。
- `CGO_ENABLED=0 go build ./...`：通过，确认 `modernc.org/sqlite` 可纯 Go 构建。
- `go run ./cmd/alertdemo all --out artifacts/demo`：`ok=true`；三版核心模型和逐时刻报告一致；18/18 关键真值检查通过；9 个增量编辑均与全量重算一致；`per_rule_buffer=1` 时确定性记录 20 次背压 flush；SQLite 恢复、版本不匹配和截断检查通过；全部注入错误码检查通过。
- 直接二进制 `crash-run --crash-after 2` 退出码为 `137`，随后 `resume` 从 t=2 恢复到 t=9。
- 真实 `kill -TERM` 测试中，`run --step-sleep 1s` 在 t=0 提交后收到信号，输出 `checkpoint is durable`，退出码为 0；随后 resume 正常完成。

生成的可重复检查材料位于：

- `artifacts/demo/project-v1.json`
- `artifacts/demo/project-v2.json`
- `artifacts/demo/project-v3.json`
- `artifacts/demo/events.json`
- `artifacts/demo/truth.json`
- `artifacts/demo/timeline.txt`
- `artifacts/demo/verification-report.json`
- `artifacts/demo/incremental-*.json`
- `artifacts/demo/checkpoints/`
